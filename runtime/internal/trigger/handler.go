// Package trigger 实现 Phase 1.5 Trigger 服务(CLAUDE.md §12.5):
// GitLab pipeline webhook(验签、去重)→ 拉 bundle → 登记 artifacts → 启动 DeviceTestWorkflow。
package trigger

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"github.com/rs/zerolog"

	"hermes-devops/runtime/internal/store"
	wf "hermes-devops/runtime/internal/workflow"
)

// BundleFetcher 按 (projectID, shortSHA, pipelineGlobalID) 定位并下载
// bundle-g{sha}-p{pipelineGlobalID}.json。
// found=false 表示该 pipeline 没有发布 bundle(如 MR 构建),不是错误。
type BundleFetcher interface {
	FetchBundle(ctx context.Context, projectID int64, shortSHA string, pipelineGlobalID int64) (raw []byte, found bool, err error)
}

// WorkflowStarter 启动 DeviceTestWorkflow。
// started=false 表示同 ID workflow 已存在(重复投递),视为幂等成功。
type WorkflowStarter interface {
	StartDeviceTest(ctx context.Context, in wf.DeviceTestInput) (workflowID string, started bool, err error)
}

// Config 是 Trigger 服务配置。
type Config struct {
	WebhookSecret string
	Refs          []string        // 触发的分支白名单;空 = 不过滤
	Logger        *zerolog.Logger // 缺省 Nop
	// GitLabBaseURL 用于校验 /kick 载荷中的产物 URL 必须指向本 GitLab
	// (形如 https://gitlab.example,空 = 不校验,仅开发)。
	GitLabBaseURL string
	// PipelineWebhookDisabled 关闭 pipeline success webhook 的触发语义
	// (变体级 /kick 上线后,避免同一变体被 bundle workflow 与 kick
	// workflow 双跑;webhook 仍接收并 204,仅作记录)。缺省 false = 启用。
	PipelineWebhookDisabled bool
}

// Handler 处理 GitLab webhook 与 CI 直发 /kick。
type Handler struct {
	cfg     Config
	fetcher BundleFetcher
	store   store.ArtifactStore
	starter WorkflowStarter
	log     zerolog.Logger
	// Prober 复核 /kick 产物存在性;nil = 跳过复核(仅开发)。
	Prober PackageProber
}

func New(cfg Config, fetcher BundleFetcher, st store.ArtifactStore, starter WorkflowStarter) (*Handler, error) {
	if cfg.WebhookSecret == "" {
		return nil, errors.New("webhook secret is required")
	}
	logger := zerolog.Nop()
	if cfg.Logger != nil {
		logger = *cfg.Logger
	}
	return &Handler{cfg: cfg, fetcher: fetcher, store: st, starter: starter, log: logger}, nil
}

// pipelineEvent 是 GitLab 13.8 Pipeline Hook payload 的消费子集。
type pipelineEvent struct {
	ObjectKind       string `json:"object_kind"`
	ObjectAttributes struct {
		ID     int64  `json:"id"` // GitLab 13.8 全局 ID,等于 CI_PIPELINE_ID(非 IID),对应 bundle.pipeline_global_id
		Ref    string `json:"ref"`
		Tag    bool   `json:"tag"`
		SHA    string `json:"sha"` // 全长 40 位
		Status string `json:"status"`
	} `json:"object_attributes"`
	Project struct {
		ID                int64  `json:"id"`
		PathWithNamespace string `json:"path_with_namespace"`
	} `json:"project"`
}

const shortSHALen = 8 // CI_COMMIT_SHORT_SHA 固定 8 位,bundle 文件名以此编码

var fullSHARegexp = regexp.MustCompile(`^[0-9a-f]{40}$`)

// checkToken 恒定时间比对共享密钥(GitLab 13.8 webhook secret;
// /kick 复用同一密钥与请求头,CI 变量下发)。
func (h *Handler) checkToken(r *http.Request) bool {
	return subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Gitlab-Token")), []byte(h.cfg.WebhookSecret)) == 1
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// GitLab webhook secret token 是共享密钥比对(13.8 无 HMAC 签名),恒定时间比较
	if !h.checkToken(r) {
		http.Error(w, "bad token", http.StatusUnauthorized)
		return
	}

	var ev pipelineEvent
	if err := json.NewDecoder(r.Body).Decode(&ev); err != nil {
		http.Error(w, "bad payload", http.StatusBadRequest)
		return
	}
	if ev.ObjectKind == "pipeline" && h.cfg.PipelineWebhookDisabled {
		// 变体级 /kick 已接管触发;webhook 仅记录,不再起完整 bundle workflow(防双跑)
		h.log.Info().Str("project", ev.Project.PathWithNamespace).
			Int64("pipeline", ev.ObjectAttributes.ID).
			Str("status", ev.ObjectAttributes.Status).
			Msg("pipeline webhook received but disabled (kick mode)")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if ev.ObjectKind != "pipeline" || ev.ObjectAttributes.Status != "success" ||
		!h.refAllowed(ev.ObjectAttributes.Ref, ev.ObjectAttributes.Tag) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if ev.Project.ID <= 0 || ev.ObjectAttributes.ID <= 0 {
		http.Error(w, "bad pipeline identity", http.StatusBadRequest)
		return
	}
	if !fullSHARegexp.MatchString(ev.ObjectAttributes.SHA) {
		http.Error(w, "bad sha", http.StatusBadRequest)
		return
	}
	shortSHA := ev.ObjectAttributes.SHA[:shortSHALen]
	log := h.log.With().Str("project", ev.Project.PathWithNamespace).
		Int64("pipeline", ev.ObjectAttributes.ID).Str("sha", shortSHA).Logger()

	// ---- 拉 bundle(Trigger 只认 bundle,§6.3) ----
	raw, found, err := h.fetcher.FetchBundle(r.Context(), ev.Project.ID, shortSHA, ev.ObjectAttributes.ID)
	if err != nil {
		log.Error().Err(err).Msg("fetch bundle")
		http.Error(w, "bundle fetch failed", http.StatusBadGateway)
		return
	}
	if !found {
		log.Info().Msg("no bundle for pipeline, skipping")
		writeJSON(w, http.StatusOK, map[string]any{"skipped": "no bundle"})
		return
	}
	b, err := ParseBundle(raw)
	if err != nil {
		log.Error().Err(err).Msg("invalid bundle")
		http.Error(w, "invalid bundle: "+err.Error(), http.StatusUnprocessableEntity)
		return
	}
	if b.Project != ev.Project.PathWithNamespace {
		log.Error().Str("bundle_project", b.Project).
			Str("event_project", ev.Project.PathWithNamespace).Msg("bundle project mismatch")
		http.Error(w, "bundle project mismatch", http.StatusUnprocessableEntity)
		return
	}
	if b.PipelineGlobalID != ev.ObjectAttributes.ID {
		log.Error().Int64("bundle_pipeline_global_id", b.PipelineGlobalID).
			Int64("event_pipeline_global_id", ev.ObjectAttributes.ID).Msg("bundle pipeline mismatch")
		http.Error(w, "bundle pipeline mismatch", http.StatusUnprocessableEntity)
		return
	}
	// bundle.commit(short)必须是事件 sha 的前缀,防串包
	if !strings.HasPrefix(ev.ObjectAttributes.SHA, b.Commit) {
		log.Error().Str("bundle_commit", b.Commit).Msg("bundle commit mismatch")
		http.Error(w, "bundle commit mismatch", http.StatusUnprocessableEntity)
		return
	}

	// ---- 登记 artifacts(幂等 upsert) ----
	arts := make([]store.Artifact, 0, len(b.Packages))
	for _, p := range b.Packages {
		arts = append(arts, store.Artifact{
			Project: b.Project, CommitSHA: b.Commit, PipelineID: b.PipelineID,
			Variant: p.Variant, BuildType: "Release", // 见 store.Artifact 的 CONTRACT-ISSUE
			URL: p.URL, SHA256: p.SHA256, Size: p.Size, ManifestDigest: p.ManifestDigest,
		})
	}
	if err := h.store.RegisterArtifacts(r.Context(), arts); err != nil {
		log.Error().Err(err).Msg("register artifacts")
		http.Error(w, "register artifacts failed", http.StatusInternalServerError)
		return
	}

	// ---- 启动 workflow(ID 确定性,重复投递由 Temporal 去重) ----
	in := wf.DeviceTestInput{
		Project: b.Project, Commit: b.Commit, PipelineID: b.PipelineID,
		Version: b.Version, Packages: b.Packages,
	}
	wfID, started, err := h.starter.StartDeviceTest(r.Context(), in)
	if err != nil {
		log.Error().Err(err).Msg("start workflow")
		http.Error(w, "start workflow failed", http.StatusBadGateway)
		return
	}
	log.Info().Str("workflow_id", wfID).Bool("started", started).Msg("device test triggered")
	writeJSON(w, http.StatusAccepted, map[string]any{"workflow_id": wfID, "started": started})
}

func (h *Handler) refAllowed(ref string, isTag bool) bool {
	if len(h.cfg.Refs) == 0 {
		return true
	}
	return isTag || slices.Contains(h.cfg.Refs, ref)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
