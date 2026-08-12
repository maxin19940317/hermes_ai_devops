package trigger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"hermes-devops/runtime/internal/store"
	wf "hermes-devops/runtime/internal/workflow"
)

// kickPayload 是 CI build job 在产物上传 Registry 后直发的变体级触发载荷
// (§6.3:一个包编好即触发,不再等全部 8 个包)。字段与 ci/write_meta.py
// 输出的 meta JSON 一致,build job 原样透传。
type kickPayload struct {
	Variant          string `json:"variant"`
	PackageFile      string `json:"package_file"`
	URL              string `json:"url"`
	SHA256           string `json:"sha256"`
	Size             int64  `json:"size"`
	ManifestDigest   string `json:"manifest_digest"`
	Version          string `json:"version"`
	Project          string `json:"project"`
	Commit           string `json:"commit"` // short sha
	PipelineID       int    `json:"pipeline_id"`
	PipelineGlobalID int64  `json:"pipeline_global_id"`
	// RuleVersion 规则引擎版本(契约只加不删,缺省 verdict-rules-v1,差距 #7)。
	RuleVersion string `json:"rule_version"`
	// Retry 显式重试语义(差距 #11):true 时 workflow ID 加 -r{N} 后缀起新 run
	// (N 取自 artifacts.workflow_attempt 原子递增);缺省 false = 普通触发,
	// 同 ID 已存在一律幂等不再启动(无论上次成败)。
	Retry bool `json:"retry"`
	// Requirements / FailureSignatures 是业务仓库 variants.yaml 声明的设备
	// 调度约束与失败签名(2026-08-12 解耦:单变体触发自包含,不依赖 artifact)。
	Requirements      *wf.VariantRequirements `json:"requirements,omitempty"`
	FailureSignatures []wf.VariantSignature   `json:"failure_signatures,omitempty"`
}

var (
	kickVariantRegexp = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)
	kickProjectRegexp = regexp.MustCompile(
		`^[A-Za-z0-9_][A-Za-z0-9_.-]*(?:/[A-Za-z0-9_][A-Za-z0-9_.-]*)*$`,
	)
	kickCommitRegexp = regexp.MustCompile(`^[0-9a-f]{8,40}$`)
	kickSHA256Regexp = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// validate 校验载荷形态与自洽性;gitlabBase 非空时要求产物 URL 指向
// 本 GitLab(防伪造 meta 把 Trigger 当任意 URL 的拉取代理)。
func (p *kickPayload) validate(gitlabBase string) error {
	switch {
	case !kickVariantRegexp.MatchString(p.Variant):
		return errors.New("bad variant")
	case len(p.Project) > 256 || !kickProjectRegexp.MatchString(p.Project) ||
		strings.Contains(p.Project, ".."):
		return errors.New("bad project")
	case !kickCommitRegexp.MatchString(p.Commit):
		return errors.New("bad commit")
	case !kickSHA256Regexp.MatchString(p.SHA256):
		return errors.New("bad sha256")
	case p.Size <= 0:
		return errors.New("bad size")
	case p.ManifestDigest == "":
		return errors.New("bad manifest_digest")
	case p.Version == "":
		return errors.New("bad version")
	case p.PipelineID <= 0 || p.PipelineGlobalID <= 0:
		return errors.New("bad pipeline identity")
	case p.PackageFile == "":
		return errors.New("bad package_file")
	}
	if gitlabBase != "" &&
		!strings.HasPrefix(p.URL, strings.TrimRight(gitlabBase, "/")+"/api/v4/projects/") {
		return errors.New("url not under gitlab base")
	}
	return nil
}

// PackageProber 复核 kick 声明的产物确实存在于 Registry
// (防伪造 meta 直发;CI 凭据本身不证明产物已上传)。
type PackageProber interface {
	PackageExists(ctx context.Context, url string) error
}

// PackageExists 实现 PackageProber:GET Range bytes=0-0,2xx/206 即存在。
// 只取首字节,不下载整包。
func (g *GitLabClient) PackageExists(ctx context.Context, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	header := g.TokenHeader
	if header == "" {
		header = "PRIVATE-TOKEN"
	}
	req.Header.Set(header, g.Token)
	req.Header.Set("Range", "bytes=0-0")
	resp, err := g.http().Do(req)
	if err != nil {
		return fmt.Errorf("probe package: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusPartialContent {
		return nil
	}
	return fmt.Errorf("probe package: status %d", resp.StatusCode)
}

// HandleKick 处理 POST /kick(变体级触发,§6.3):校验 → Registry 复核 →
// 登记 artifacts → 起单变体 workflow(ID 含 variant,重复 kick 由 Temporal 去重)。
func (h *Handler) HandleKick(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !h.checkToken(r) {
		http.Error(w, "bad token", http.StatusUnauthorized)
		return
	}
	var p kickPayload
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		http.Error(w, "bad payload", http.StatusBadRequest)
		return
	}
	if err := p.validate(h.cfg.GitLabBaseURL); err != nil {
		http.Error(w, "invalid kick: "+err.Error(), http.StatusUnprocessableEntity)
		return
	}
	log := h.log.With().Str("project", p.Project).Str("variant", p.Variant).
		Str("sha", p.Commit).Int("pipeline_iid", p.PipelineID).Logger()

	if h.Prober != nil {
		if err := h.Prober.PackageExists(r.Context(), p.URL); err != nil {
			log.Error().Err(err).Msg("package probe failed")
			http.Error(w, "package probe failed", http.StatusBadGateway)
			return
		}
	}

	art := store.Artifact{
		Project: p.Project, CommitSHA: p.Commit, PipelineID: p.PipelineID,
		Variant: p.Variant, BuildType: "Release", // 见 store.Artifact 的 CONTRACT-ISSUE
		Version: p.Version, URL: p.URL, SHA256: p.SHA256, Size: p.Size,
		ManifestDigest: p.ManifestDigest,
		// 2026-08-12 解耦:单变体触发自包含调度约束与签名。
		VariantRequirements: p.Requirements,
		VariantSignatures:   p.FailureSignatures,
	}
	if err := h.store.RegisterArtifacts(r.Context(), []store.Artifact{art}); err != nil {
		log.Error().Err(err).Msg("register artifacts")
		http.Error(w, "register artifacts failed", http.StatusInternalServerError)
		return
	}

	in := wf.DeviceTestInput{
		Project: p.Project, Commit: p.Commit, PipelineID: p.PipelineID,
		Version: p.Version, Scope: p.Variant,
		RuleVersion: ruleVersionOr(p.RuleVersion),
		Packages: []wf.PackageRef{{
			Variant: p.Variant, PackageFile: p.PackageFile, URL: p.URL,
			SHA256: p.SHA256, Size: p.Size, ManifestDigest: p.ManifestDigest,
			Requirements: p.Requirements, FailureSignatures: p.FailureSignatures,
		}},
	}
	if p.Retry {
		// 显式 retry(差距 #11):同一逻辑键(commit,pipeline,variant)原子递增
		// workflow_attempt 得 N,workflow ID 加 -r{N} 起新 run;普通重放永不走这里。
		n, err := h.store.NextWorkflowAttempt(
			r.Context(), p.Project, p.Commit, p.PipelineID, p.Variant,
		)
		if err != nil {
			log.Error().Err(err).Msg("next workflow attempt")
			http.Error(w, "retry attempt failed", http.StatusInternalServerError)
			return
		}
		in.Attempt = n
	}
	wfID, started, err := h.starter.StartDeviceTest(r.Context(), in)
	if err != nil {
		log.Error().Err(err).Msg("start workflow")
		http.Error(w, "start workflow failed", http.StatusBadGateway)
		return
	}
	log.Info().Str("workflow_id", wfID).Bool("started", started).Msg("device test kicked")
	writeJSON(w, http.StatusAccepted, map[string]any{"workflow_id": wfID, "started": started})
}
