package feishucmd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"hermes-devops/runtime/internal/hermesclient"
	"hermes-devops/runtime/internal/store"
)

// minConfidence 是执行门限:低于此值一律不执行,只回翻译结果供人工判断。
const minConfidence = 0.75

// recentRunsLimit 是上下文快照携带的历史运行条数上限(设计文档 §4.2)。
const recentRunsLimit = 10

// sideEffect 标记需要二次确认的指令:LLM 猜错参数的代价是白跑一轮设备测试。
var sideEffect = map[string]bool{"rerun": true, "unquarantine": true}

// TranslateResult 是一次翻译的完整结论。OK=false 时 Reply 即最终回复文本。
type TranslateResult struct {
	Cmd          Command
	Rendered     string // 渲染出的那行指令(可直接展示给用户照打)
	Reason       string // LLM 给出的依据
	NeedsConfirm bool
	Outcome      string
	Reply        string
	OK           bool
}

// Translator 把自然语言翻译成一行既有指令文本(设计文档 §3.1)。
// 它不执行指令,只产出"可执行的 Command"或"不可执行 + 原因"。
type Translator struct {
	Client   hermesclient.Translator
	Store    Store
	Variants []string // 合法变体名单(来自 specCfg)
	Model    string
	Now      func() time.Time // 可注入,便于测试
	// Log 可选;nil 用 Nop(镜像 Executor.Log 的处理)。
	Log *zerolog.Logger
}

func (t *Translator) now() time.Time {
	if t.Now != nil {
		return t.Now()
	}
	return time.Now().UTC()
}

func (t *Translator) log() zerolog.Logger {
	if t.Log != nil {
		return *t.Log
	}
	return zerolog.Nop()
}

// render 把翻译结果拼成一行指令文本。args 各项已由 command.schema.json 保证不含
// 空白字符,故 strings.Fields 可无损切回(方案 1 封闭性的实现基础)。
func render(cmd string, args []string) string {
	if len(args) == 0 {
		return cmd
	}
	return cmd + " " + strings.Join(args, " ")
}

// snapshot 是发给平台的只读上下文(设计文档 §4.2)。
type snapshot struct {
	Now        string        `json:"now"`
	Variants   []string      `json:"variants"`
	RecentRuns []snapshotRun `json:"recent_runs"`
	Devices    []snapshotDev `json:"devices"`
}

type snapshotRun struct {
	WorkflowID    string `json:"workflow_id,omitempty"`
	Commit        string `json:"commit"`
	PipelineIID   int    `json:"pipeline_iid"`
	Version       string `json:"version,omitempty"`
	RuleVersion   string `json:"rule_version,omitempty"`
	Variant       string `json:"variant"`
	Verdict       string `json:"verdict,omitempty"`
	EndedAt       string `json:"ended_at,omitempty"`
	Authoritative bool   `json:"authoritative"`
}

type snapshotDev struct {
	DeviceID string `json:"device_id"`
	Serial   string `json:"serial"`
	Status   string `json:"status"`
}

// buildSnapshot 组装上下文快照。查库失败时降级为只含 now 的空快照——
// 快照缺失只会让 LLM 返回 none,是安全降级,不该阻断(设计文档 §6)。
func (t *Translator) buildSnapshot(ctx context.Context) snapshot {
	snap := snapshot{
		Now:        t.now().UTC().Format(time.RFC3339),
		Variants:   t.Variants,
		RecentRuns: []snapshotRun{},
		Devices:    []snapshotDev{},
	}
	if snap.Variants == nil {
		snap.Variants = []string{}
	}
	if runs, err := t.Store.RecentRuns(ctx, recentRunsLimit); err == nil {
		for _, r := range runs {
			sr := snapshotRun{
				WorkflowID: r.WorkflowID, Commit: r.Commit, PipelineIID: r.PipelineID,
				Version: r.Version, RuleVersion: r.RuleVersion, Variant: r.Variant,
				Verdict: r.Verdict, Authoritative: r.Authoritative,
			}
			if !r.EndedAt.IsZero() {
				sr.EndedAt = r.EndedAt.UTC().Format(time.RFC3339)
			}
			snap.RecentRuns = append(snap.RecentRuns, sr)
		}
	}
	if ov, err := t.Store.FleetOverview(ctx); err == nil {
		for _, d := range ov.Devices {
			snap.Devices = append(snap.Devices, snapshotDev{
				DeviceID: d.DeviceID, Serial: d.Serial, Status: d.Status,
			})
		}
	}
	return snap
}

// Translate 执行一次翻译。无论成败都落一行审计(设计文档 §4.3)。
func (t *Translator) Translate(ctx context.Context, openID, rawText string) TranslateResult {
	snap := t.buildSnapshot(ctx)
	snapJSON, err := json.Marshal(snap)
	if err != nil {
		snapJSON = []byte(`{}`)
	}
	sum := sha256.Sum256(snapJSON)
	digest := hex.EncodeToString(sum[:])

	audit := store.CommandTranslation{
		OpenID: openID, RawText: rawText,
		PromptVersion: hermesclient.PromptVersionTranslate,
		Model:         t.Model, ContextDigest: digest,
	}

	tr, err := t.Client.Translate(ctx, hermesclient.TranslateRequest{
		RawText: rawText, Context: snapJSON, Model: t.Model,
	})
	if err != nil {
		audit.Output = []byte(fmt.Sprintf(`{"error":%q}`, err.Error()))
		if errors.Is(err, hermesclient.ErrSchemaInvalid) {
			// 平台答复不符合 command.schema.json:prompt 需要迭代的信号,
			// 与网络/超时/非 2xx 等基础设施问题在审计里必须能分辨(见 CONTRACT 评审)。
			audit.Outcome = store.OutcomeRejectedSchema
			t.save(ctx, audit)
			return TranslateResult{Outcome: audit.Outcome, Reply: "没理解这句话。\n" + usage}
		}
		audit.Outcome = store.OutcomeTranslatorError
		t.save(ctx, audit)
		return TranslateResult{Outcome: audit.Outcome,
			Reply: "翻译服务暂时不可用。\n" + usage}
	}
	if raw, mErr := json.Marshal(tr); mErr == nil {
		audit.Output = raw
	}

	if tr.Command == "none" {
		audit.Outcome = store.OutcomeRejectedNone
		t.save(ctx, audit)
		reply := "没理解这句话。"
		if tr.Reason != "" {
			reply += "(" + tr.Reason + ")"
		}
		return TranslateResult{Outcome: audit.Outcome, Reason: tr.Reason, Reply: reply + "\n" + usage}
	}

	rendered := render(tr.Command, tr.Args)
	audit.Rendered = rendered
	cmd := Parse(rendered)
	if cmd.Name == "help" {
		// 渲染后回灌未命中四指令:值域被破坏,拒绝
		audit.Outcome = store.OutcomeRejectedArgs
		t.save(ctx, audit)
		return TranslateResult{Outcome: audit.Outcome, Rendered: rendered, Reply: "没理解这句话。\n" + usage}
	}
	if why := t.checkArgs(cmd, snap.RecentRuns, snap.Devices); why != "" {
		audit.Outcome = store.OutcomeRejectedArgs
		t.save(ctx, audit)
		return TranslateResult{Outcome: audit.Outcome, Rendered: rendered,
			Reply: fmt.Sprintf("你是想说 `%s` 吗?该指令参数不合法: %s\n%s", rendered, why, usage)}
	}
	if tr.Confidence < minConfidence {
		audit.Outcome = store.OutcomeRejectedLowConfidence
		t.save(ctx, audit)
		return TranslateResult{Outcome: audit.Outcome, Rendered: rendered,
			Reply: fmt.Sprintf("不太确定,你是想说 `%s` 吗?确认请直接发这行。", rendered)}
	}

	res := TranslateResult{
		Cmd: cmd, Rendered: rendered, Reason: tr.Reason, OK: true,
		NeedsConfirm: sideEffect[cmd.Name],
	}
	if res.NeedsConfirm {
		res.Outcome = store.OutcomePendingConfirm
	} else {
		res.Outcome = store.OutcomeExecuted
	}
	audit.Outcome = res.Outcome
	t.save(ctx, audit)
	return res
}

// checkArgs 复用既有参数校验(设计文档 §5.3);返回空串表示通过。
// workflow/变体/设备存在性按快照成员判定;execute 内部仍会独立查库,两层都保留。
// devices 是本次快照里的设备列表,用于 unquarantine 的 device_id 存在性判定。
func (t *Translator) checkArgs(cmd Command, runs []snapshotRun, devices []snapshotDev) string {
	switch cmd.Name {
	case "rerun":
		if len(cmd.Args) < 1 || len(cmd.Args) > 2 {
			return "rerun 需要 <source_workflow_id> [variant]"
		}
		workflowID := cmd.Args[0]
		variant := ""
		if len(cmd.Args) == 2 {
			variant = cmd.Args[1]
		}
		for _, run := range runs {
			if !run.Authoritative || run.WorkflowID != workflowID {
				continue
			}
			if variant == "" || run.Variant == variant {
				return ""
			}
		}
		if variant != "" {
			return fmt.Sprintf("变体 %s 不属于权威运行 %s", variant, workflowID)
		}
		return fmt.Sprintf("workflow %s 不在权威运行快照内", workflowID)
	case "unquarantine":
		if len(cmd.Args) > 1 {
			return "unquarantine 最多一个 device_id"
		}
		if len(cmd.Args) == 1 && !containsDevice(devices, cmd.Args[0]) {
			return fmt.Sprintf("设备 %s 不在快照设备名单内", cmd.Args[0])
		}
	case "status", "devices":
		if len(cmd.Args) != 0 {
			return cmd.Name + " 不接受参数"
		}
	}
	return ""
}

// containsDevice 判定 device_id 是否在快照设备列表内(unquarantine 的存在性校验)。
func containsDevice(devices []snapshotDev, deviceID string) bool {
	for _, d := range devices {
		if d.DeviceID == deviceID {
			return true
		}
	}
	return false
}

// save 落审计;失败只记 error 日志不阻断(与 persistEvidenceSnapshot 的降级一致)。
// Store 是必填依赖(buildSnapshot 已无条件解引用它),这里不再重复判空。
func (t *Translator) save(ctx context.Context, row store.CommandTranslation) {
	if err := t.Store.SaveCommandTranslation(ctx, row); err != nil {
		log := t.log()
		log.Error().Err(err).Str("open_id", row.OpenID).Str("outcome", row.Outcome).
			Msg("save command translation audit failed")
	}
}
