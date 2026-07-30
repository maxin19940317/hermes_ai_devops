package feishucmd

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"hermes-devops/runtime/internal/feishu"
	"hermes-devops/runtime/internal/store"
	wf "hermes-devops/runtime/internal/workflow"
)

// Store 是指令执行依赖的持久层子集(MemStore/PGStore 均满足)。
type Store interface {
	FleetOverview(ctx context.Context) (*store.FleetOverview, error)
	UnquarantineDevice(ctx context.Context, deviceID string) (bool, error)
	ListArtifacts(ctx context.Context, project, commitSHA string, pipelineID int) ([]store.Artifact, error)
	NextWorkflowAttempt(ctx context.Context, project, commitSHA string, pipelineID int, variant string) (int, error)
	NextWorkflowAttemptAll(ctx context.Context, project, commitSHA string, pipelineID int) (int, error)
	GetWorkflowRun(ctx context.Context, workflowID string) (*store.WorkflowRun, error)
	// 以下三个供意图翻译层使用(设计文档 §3.1)
	RecentRuns(ctx context.Context, limit int) ([]store.RecentRun, error)
	SaveCommandTranslation(ctx context.Context, row store.CommandTranslation) error
	ListCommandTranslations(ctx context.Context, openID string, limit int) ([]store.CommandTranslation, error)
}

// WorkflowStarter 启动 DeviceTestWorkflow(trigger.TemporalStarter 满足)。
type WorkflowStarter interface {
	StartDeviceTest(ctx context.Context, in wf.DeviceTestInput) (workflowID string, started bool, err error)
	WorkflowClosed(ctx context.Context, workflowID string) (bool, error)
	WorkflowResult(ctx context.Context, workflowID string) (*wf.DeviceTestOutput, error)
}

// Executor 是指令执行体:鉴权(白名单)→ 解析 → 执行 → 文本回复。
// 全部依赖为接口/函数值,单测可 fake。
type Executor struct {
	Store     Store
	Starter   WorkflowStarter
	Sender    feishu.Sender   // 回复通道;nil = 只执行不回复(测试)
	Log       *zerolog.Logger // 可选;nil 用 Nop
	Whitelist map[string]bool

	// Translator 非 nil 时启用自然语言翻译旁路(设计文档 §3.1);
	// nil = 未启用,未知输入回 usage(改动前的行为)。
	Translator *Translator
	// Now 可注入,便于测试待确认 TTL;nil 用 time.Now().UTC()。
	Now func() time.Time

	pendingMu sync.Mutex
	pending   map[string]pendingCmd
}

// confirmTTL 是待确认态存活时长(设计文档 §5.2)。过期后回 y 视同未理解。
const confirmTTL = 120 * time.Second

// pendingCmd 是一条等待用户确认的副作用指令。存内存而非落库:worker 重启丢失
// 待确认项,代价只是用户重说一遍,绝不会误执行一个跨重启的陈旧 rerun。
type pendingCmd struct {
	cmd      Command
	rendered string
	expires  time.Time
}

func (e *Executor) nowFn() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now().UTC()
}

func (e *Executor) log() zerolog.Logger {
	if e.Log != nil {
		return *e.Log
	}
	return zerolog.Nop()
}

// HandleMessage 处理一条单聊文本消息。安全红线:非白名单 open_id 静默忽略
// (不回复,防探测;翻译层永远看不到非白名单消息),记 info 日志。
func (e *Executor) HandleMessage(ctx context.Context, openID, text string) {
	log := e.log()
	if !e.Whitelist[openID] {
		log.Info().Str("open_id", openID).Msg("feishu cmd from non-whitelist sender, ignored")
		return
	}
	trimmed := strings.TrimSpace(text)
	prefix := "" // 待确认被取消时,前置到最终回复里

	// superseded 承载"default 分支旧项该判 declined 还是 expired"的延迟决策,
	// 详见 disposeSuperseded 的 doc comment(CONTRACT-ISSUE)。
	var superseded *pendingCmd

	// 待确认态检查必须在 Parse 之前:否则用户打的 y 会被当成未知指令(设计文档 §5.2)
	if pend, ok, expired := e.takePending(openID); ok {
		switch strings.ToLower(trimmed) {
		case "y", "yes":
			e.audit(ctx, openID, trimmed, pend.rendered, store.OutcomeConfirmed)
			reply, err := e.execute(ctx, pend.cmd)
			if err != nil {
				log.Error().Err(err).Str("cmd", pend.cmd.Name).Msg("feishu cmd failed")
				reply = fmt.Sprintf("指令执行失败: %v", err)
			}
			e.reply(ctx, reply)
			return
		case "n", "no":
			// 与 y/yes 对称:确认问句给两个答案,不是一个答案加一堆非答案。
			// 顺带省掉一次必然落 rejected_none 的 LLM 调用。
			e.audit(ctx, openID, trimmed, pend.rendered, store.OutcomeDeclined)
			e.reply(ctx, "已取消: "+pend.rendered)
			return
		default:
			superseded = &pend
			prefix = "已取消上一条待确认(" + pend.rendered + ")\n"
		}
	} else if expired {
		// 设计文档 §4.3:expired 由 TTL 到期或被新翻译覆盖两个独立触发共同定义,
		// 覆盖侧走 superseded/putPending 的 expired 审计,这里补上 TTL 到期侧——
		// 二者都必须留痕,否则 120s 后一个 y 会静默清掉一条待确认而零审计。
		e.audit(ctx, openID, trimmed, pend.rendered, store.OutcomeExpired)
	}

	cmd := Parse(trimmed)
	if cmd.Name == "help" && trimmed != "" && e.Translator != nil {
		e.reply(ctx, prefix+e.handleTranslated(ctx, openID, trimmed, superseded))
		return
	}
	// 新消息本身就是已知指令,不会再产生新的待确认:旧项(若有)算放弃。
	e.disposeSuperseded(ctx, openID, trimmed, superseded)
	reply, err := e.execute(ctx, cmd)
	if err != nil {
		log.Error().Err(err).Str("cmd", cmd.Name).Msg("feishu cmd failed")
		reply = fmt.Sprintf("指令执行失败: %v", err)
	}
	log.Info().Str("open_id", openID).Str("cmd", cmd.Name).Msg("feishu cmd executed")
	e.reply(ctx, prefix+reply)
}

// disposeSuperseded 落 superseded(fallthrough 取代的旧待确认,可能为 nil)的
// declined 审计——用于确认新消息最终没有产生替代它的新待确认的各条路径。
//
// CONTRACT-ISSUE: brief 原文对 HandleMessage "default" 分支给的字面代码是无条件
// `e.audit(..., store.OutcomeDeclined)`。但 takePending 在函数最开始就已经把
// 槽位取空(delete),所以当 fallthrough 继续处理的新消息本身又翻译出一个需确认
// 指令时(TestNewTranslationSupersedesPending 这种"再放一次"场景),
// putPending 里"覆盖旧项落 expired"的分支永远看不到旧项(已经被删了)——
// 单槽覆盖(§5.2 规则 4)在时序上必然落空,只会落 declined,与该测试断言的
// "被覆盖应为 expired"矛盾。
// 解决:把 default 分支的旧项处理推迟到知道下一步结果之后 ——
// 如果新消息立刻翻译出另一个需确认指令,旧项算被"取代"(expired,规则 4,见
// handleTranslated);否则旧项算被"放弃"(declined,规则 3,此函数)。用
// superseded 承载这个延迟决策,putPending 里的"覆盖落 expired"逻辑保留作为
// 并发场景的兜底(两个 goroutine 同时处理同一 openID 消息时的防御,非本次改动
// 重点)。
func (e *Executor) disposeSuperseded(ctx context.Context, openID, rawText string, superseded *pendingCmd) {
	if superseded == nil {
		return
	}
	e.audit(ctx, openID, rawText, superseded.rendered, store.OutcomeDeclined)
}

// handleTranslated 走翻译旁路并返回回复文本(不含 prefix)。superseded 非 nil
// 时表示本次消息取代了一个尚未确认的旧指令(见 disposeSuperseded 的
// CONTRACT-ISSUE 注释):若本次翻译又产生一个需确认指令,旧项按"被取代"落
// expired;否则按"被放弃"落 declined。
func (e *Executor) handleTranslated(ctx context.Context, openID, text string, superseded *pendingCmd) string {
	log := e.log()
	res := e.Translator.Translate(ctx, openID, text)
	log.Info().Str("open_id", openID).Str("outcome", res.Outcome).
		Str("rendered", res.Rendered).Msg("feishu cmd translated")
	if res.OK && res.NeedsConfirm {
		e.putPending(ctx, openID, pendingCmd{
			cmd: res.Cmd, rendered: res.Rendered,
			expires: e.nowFn().Add(confirmTTL),
		})
		if superseded != nil {
			e.audit(ctx, openID, text, superseded.rendered, store.OutcomeExpired)
		}
		msg := fmt.Sprintf("将执行: %s", res.Rendered)
		if res.Reason != "" {
			msg += "\n(依据: " + res.Reason + ")"
		}
		return msg + fmt.Sprintf("\n回复 y 确认,n 取消,%d 秒后自动失效", int(confirmTTL.Seconds()))
	}
	e.disposeSuperseded(ctx, openID, text, superseded)
	if !res.OK {
		return res.Reply
	}
	reply, err := e.execute(ctx, res.Cmd)
	if err != nil {
		log.Error().Err(err).Str("cmd", res.Cmd.Name).Msg("feishu cmd failed")
		return fmt.Sprintf("指令执行失败: %v", err)
	}
	// 带上"已理解为 X":用户下次可以直接打 X,翻译层因此是自我消解的
	return fmt.Sprintf("(已理解为: %s)\n%s", res.Rendered, reply)
}

// takePending 取出并清空某用户的待确认项。三种结果:
//   - ok=true:存在且未过期,pend 可用于 y/n/fallthrough 分支。
//   - ok=false, expired=true:存在但已过 TTL——调用方需为其落一行 expired 审计
//     (设计文档 §4.3 的 TTL 触发侧;pend.rendered 仍有效,供审计用)。
//   - ok=false, expired=false:本来就没有待确认项,无事可做。
//
// 注:该待确认项对应的 pending_confirm 行不是这里写的,而是 Translator.Translate
// 产生需确认指令时写的(Task 6,translate.go);takePending/putPending 只负责
// 这条项之后的确认/取消/过期/覆盖这几种终态审计。
func (e *Executor) takePending(openID string) (pend pendingCmd, ok bool, expired bool) {
	e.pendingMu.Lock()
	defer e.pendingMu.Unlock()
	pend, had := e.pending[openID]
	if !had {
		return pendingCmd{}, false, false
	}
	delete(e.pending, openID)
	if e.nowFn().After(pend.expires) {
		return pend, false, true
	}
	return pend, true, false
}

// putPending 放入待确认项(单槽:覆盖旧项)。这里的"had"覆盖审计是并发场景的
// 兜底(两个 goroutine 同时处理同一 openID 的消息、都在 takePending 时读到空槽);
// 顺序场景下的"取代"审计已经由 HandleMessage/handleTranslated 的 superseded
// 机制在语义上更准确地处理(见 HandleMessage 里的 CONTRACT-ISSUE 注释)。
func (e *Executor) putPending(ctx context.Context, openID string, pend pendingCmd) {
	e.pendingMu.Lock()
	if e.pending == nil {
		e.pending = map[string]pendingCmd{}
	}
	old, had := e.pending[openID]
	e.pending[openID] = pend
	e.pendingMu.Unlock()
	if had {
		e.audit(ctx, openID, "", old.rendered, store.OutcomeExpired)
	}
}

// audit 追加一行翻译审计(确认/取消/过期这些非翻译事件也留痕,设计文档 §4.3)。
// 落库失败只记 error 日志,不阻断(设计文档 §6:审计落库失败 → 记日志,不阻断执行)。
func (e *Executor) audit(ctx context.Context, openID, rawText, rendered, outcome string) {
	if e.Store == nil {
		return
	}
	if err := e.Store.SaveCommandTranslation(ctx, store.CommandTranslation{
		OpenID: openID, RawText: rawText, Rendered: rendered, Outcome: outcome,
	}); err != nil {
		log := e.log()
		log.Error().Err(err).Str("open_id", openID).Str("outcome", outcome).
			Msg("save command translation audit failed")
	}
}

// reply 发送回复;Sender 为 nil 时只执行不回复(测试)。
func (e *Executor) reply(ctx context.Context, text string) {
	if e.Sender == nil {
		return
	}
	if err := e.Sender.SendText(ctx, text); err != nil {
		log := e.log()
		log.Error().Err(err).Msg("feishu cmd reply failed")
	}
}

// execute 执行指令并返回回复文本;返回 error 时由 HandleMessage 包装通报。
func (e *Executor) execute(ctx context.Context, cmd Command) (string, error) {
	switch cmd.Name {
	case "status":
		return e.status(ctx)
	case "devices":
		return e.devices(ctx)
	case "rerun":
		return e.rerun(ctx, cmd.Args)
	case "unquarantine":
		return e.unquarantine(ctx, cmd.Args)
	default:
		return usage, nil
	}
}

func (e *Executor) status(ctx context.Context) (string, error) {
	ov, err := e.Store.FleetOverview(ctx)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "运行中 workflow: %d\n活跃租约: %d\n设备(%d):", ov.InflightWorkflows, ov.ActiveLeases, len(ov.Devices))
	for _, d := range ov.Devices {
		fmt.Fprintf(&b, "\n  %s %s %s fail_streak=%d client=%s client_fail=%d",
			d.Serial, d.SOC, d.Status, d.FailStreak, d.ClientID, d.ClientFailStreak)
		if d.LeaseTaskID != "" {
			fmt.Fprintf(&b, " lease=%s", d.LeaseTaskID)
		}
	}
	return b.String(), nil
}

func (e *Executor) devices(ctx context.Context) (string, error) {
	ov, err := e.Store.FleetOverview(ctx)
	if err != nil {
		return "", err
	}
	if len(ov.Devices) == 0 {
		return "fleet 无注册设备", nil
	}
	var b strings.Builder
	for _, d := range ov.Devices {
		fmt.Fprintf(&b, "%s  soc=%s status=%s fail_streak=%d client=%s client_fail=%d\n",
			d.Serial, d.SOC, d.Status, d.FailStreak, d.ClientID, d.ClientFailStreak)
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// rerun <source_workflow_id> [variant] 从 workflow_runs 与 Temporal 终态输出
// 重建精确输入。无 variant 时只重试该次运行输出里的失败变体。
func (e *Executor) rerun(ctx context.Context, args []string) (string, error) {
	if len(args) == 3 ||
		(len(args) == 2 && validateSHA(strings.ToLower(args[0])) == nil &&
			isPositiveInt(args[1])) {
		return "旧 rerun 语法已停用，请使用 rerun <source_workflow_id> [variant]", nil
	}
	if len(args) < 1 || len(args) > 2 {
		return "用法: rerun <source_workflow_id> [variant]", nil
	}

	workflowID := args[0]
	source, err := e.Store.GetWorkflowRun(ctx, workflowID)
	if errors.Is(err, store.ErrWorkflowRunNotFound) {
		return fmt.Sprintf("查无权威 workflow 运行记录: %s", workflowID), nil
	}
	if err != nil {
		return "", fmt.Errorf("get workflow run %s: %w", workflowID, err)
	}
	closed, err := e.Starter.WorkflowClosed(ctx, workflowID)
	if err != nil {
		return fmt.Sprintf("检查 workflow 状态失败: %v", err), nil
	}
	if !closed {
		return fmt.Sprintf("workflow 尚未结束: %s", workflowID), nil
	}

	explicit := len(args) == 2
	targets := []string{}
	if explicit {
		targets = append(targets, args[1])
	} else {
		out, err := e.Starter.WorkflowResult(ctx, workflowID)
		if err != nil {
			return fmt.Sprintf("读取 workflow 结果失败: %v", err), nil
		}
		seen := make(map[string]struct{})
		for _, summary := range out.Tasks {
			if summary.Verdict == "PASSED" || summary.Verdict == wf.VerdictSkipped ||
				summary.Variant == "" {
				continue
			}
			seen[summary.Variant] = struct{}{}
		}
		for variant := range seen {
			targets = append(targets, variant)
		}
		sort.Strings(targets)
		if len(targets) == 0 {
			return fmt.Sprintf("workflow 没有失败变体: %s", workflowID), nil
		}
	}

	arts, err := e.Store.ListArtifacts(ctx, source.Project, source.CommitSHA, source.PipelineID)
	if err != nil {
		return "", err
	}
	sourceVariants := make(map[string]struct{}, len(source.Variants))
	for _, variant := range source.Variants {
		sourceVariants[variant] = struct{}{}
	}
	byVariant := make(map[string][]store.Artifact, len(arts))
	for _, art := range arts {
		byVariant[art.Variant] = append(byVariant[art.Variant], art)
	}
	packages := make([]wf.PackageRef, 0, len(targets))
	for _, variant := range targets {
		if _, ok := sourceVariants[variant]; !ok {
			return fmt.Sprintf("变体 %s 不属于源 workflow %s", variant, workflowID), nil
		}
		matches := byVariant[variant]
		if len(matches) != 1 {
			return fmt.Sprintf("变体 %s 的 artifact 数量为 %d，要求恰好 1 个", variant, len(matches)), nil
		}
		packages = append(packages, pkgRef(matches[0]))
	}

	in := wf.DeviceTestInput{
		Project: source.Project, Commit: source.CommitSHA, PipelineID: source.PipelineID,
		Version: source.Version, RuleVersion: source.RuleVersion,
		SourceWorkflowID: source.WorkflowID, Packages: packages,
	}
	if explicit {
		variant := targets[0]
		n, err := e.Store.NextWorkflowAttempt(
			ctx, source.Project, source.CommitSHA, source.PipelineID, variant,
		)
		if err != nil {
			return "", err
		}
		in.Scope = variant
		in.Attempt = n
	} else {
		n, err := e.Store.NextWorkflowAttemptAll(
			ctx, source.Project, source.CommitSHA, source.PipelineID,
		)
		if err != nil {
			return "", err
		}
		in.Scope = source.Scope
		in.Attempt = n
	}
	id, started, err := e.Starter.StartDeviceTest(ctx, in)
	if err != nil {
		return "", fmt.Errorf("start workflow: %w", err)
	}
	if !started {
		return fmt.Sprintf("workflow 已存在，本次 attempt 未启动: %s", id), nil
	}
	return fmt.Sprintf("已启动: %s(%d 个变体)", id, len(in.Packages)), nil
}

func isPositiveInt(s string) bool {
	n, err := strconv.Atoi(s)
	return err == nil && n > 0
}

func pkgRef(a store.Artifact) wf.PackageRef {
	return wf.PackageRef{
		Variant: a.Variant, URL: a.URL, SHA256: a.SHA256,
		Size: a.Size, ManifestDigest: a.ManifestDigest,
	}
}

// unquarantine [device_id]:不带 id 时单台直接操作,多台列出要求指定。
func (e *Executor) unquarantine(ctx context.Context, args []string) (string, error) {
	if len(args) > 1 {
		return "用法: unquarantine [device_id]", nil
	}
	if len(args) == 1 {
		ok, err := e.Store.UnquarantineDevice(ctx, args[0])
		if err != nil {
			return "", err
		}
		if !ok {
			return fmt.Sprintf("无此设备: %s", args[0]), nil
		}
		return fmt.Sprintf("已解隔离: %s", args[0]), nil
	}
	ov, err := e.Store.FleetOverview(ctx)
	if err != nil {
		return "", err
	}
	switch len(ov.Devices) {
	case 0:
		return "fleet 无注册设备", nil
	case 1:
		d := ov.Devices[0]
		ok, err := e.Store.UnquarantineDevice(ctx, d.DeviceID)
		if err != nil {
			return "", err
		}
		if !ok {
			return fmt.Sprintf("无此设备: %s", d.DeviceID), nil
		}
		return fmt.Sprintf("已解隔离: %s(唯一设备,自动选定)", d.DeviceID), nil
	default:
		var b strings.Builder
		b.WriteString("多台设备,请指定 id: unquarantine <device_id>\n")
		for _, d := range ov.Devices {
			fmt.Fprintf(&b, "  %s status=%s\n", d.DeviceID, d.Status)
		}
		return strings.TrimRight(b.String(), "\n"), nil
	}
}
