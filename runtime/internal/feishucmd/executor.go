package feishucmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/rs/zerolog"

	"hermes-devops/runtime/internal/feishu"
	"hermes-devops/runtime/internal/hermesclient"
	"hermes-devops/runtime/internal/rules"
	"hermes-devops/runtime/internal/store"
	wf "hermes-devops/runtime/internal/workflow"
)

// Store 是指令执行依赖的持久层子集(MemStore/PGStore 均满足)。
type Store interface {
	FleetOverview(ctx context.Context) (*store.FleetOverview, error)
	UnquarantineDevice(ctx context.Context, deviceID string) (bool, error)
	// QuarantineDevice 手动隔离设备(与 UnquarantineDevice 对称,飞书指令
	// quarantine):BUSY 运行中不隔离;设备不存在返回 (false, nil)。
	QuarantineDevice(ctx context.Context, deviceID string) (bool, error)
	ListArtifacts(ctx context.Context, project, commitSHA string, pipelineID int) ([]store.Artifact, error)
	// MetricsForVariant 返回指定变体最近 limit 条指标点(created_at 倒序,跨
	// project/suite/metric)。供 metrics 指令展示性能概况。
	MetricsForVariant(ctx context.Context, variant string, limit int) ([]store.MetricPoint, error)
	// LatestArtifactForVariant 返回指定变体最近一次构建的 artifact(按 created_at
	// 取最新,不限 project);无记录返回 nil,nil。供 test 命令缺省 commit 时定位
	// "最近构建"——project 从 artifact 自身带出,不依赖 workflow run。
	LatestArtifactForVariant(ctx context.Context, variant string) (*store.Artifact, error)
	// ListArtifactsForVariant 返回指定变体最近 limit 条构建(created_at 倒序,
	// 不限 project)。供 artifacts 指令查构建历史。
	ListArtifactsForVariant(ctx context.Context, variant string, limit int) ([]store.Artifact, error)
	NextWorkflowAttempt(ctx context.Context, project, commitSHA string, pipelineID int, variant string) (int, error)
	CurrentWorkflowAttempt(ctx context.Context, project, commitSHA string, pipelineID int, variant string) (int, error)
	NextWorkflowAttemptAll(ctx context.Context, project, commitSHA string, pipelineID int) (int, error)
	GetWorkflowRun(ctx context.Context, workflowID string) (*store.WorkflowRun, error)
	GetTask(ctx context.Context, taskID string) (*wf.TaskRow, error)
	GetResult(ctx context.Context, taskID string) (*wf.ResultRecord, error)
	LatestTaskIDForVariant(ctx context.Context, workflowID, variant string) (string, error)
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
	// TerminateWorkflow 取消运行中的 workflow(Temporal Terminate;已终态幂等)。
	TerminateWorkflow(ctx context.Context, workflowID, reason string) error
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
	// Planner 非 nil 时启用自然语言规划(Phase 2 Planner v1);
	// nil = 未启用,plan 命令返回"未启用"。
	Planner hermesclient.Planner
	// Variants 是合法变体名单(来自 specCfg),供 plan 上下文快照使用。
	Variants []string
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

func (e *Executor) log() *zerolog.Logger {
	if e.Log != nil {
		return e.Log
	}
	nop := zerolog.Nop()
	return &nop
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
		return e.devices(ctx, cmd.Args)
	case "test":
		return e.testCmd(ctx, cmd.Args)
	case "rerun":
		return e.rerun(ctx, cmd.Args)
	case "unquarantine":
		return e.unquarantine(ctx, cmd.Args)
	case "quarantine":
		return e.quarantine(ctx, cmd.Args)
	case "runs":
		return e.runs(ctx, cmd.Args)
	case "result":
		return e.resultCmd(ctx, cmd.Args)
	case "metrics":
		return e.metrics(ctx, cmd.Args)
	case "artifacts":
		return e.artifacts(ctx, cmd.Args)
	case "cancel":
		return e.cancel(ctx, cmd.Args)
	case "plan":
		return e.planCmd(ctx, cmd.Args)
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
		fmt.Fprintf(&b, "\n  %s serial=%s soc=%s %s fail_streak=%d client=%s client_fail=%d",
			deviceDisplayName(d), d.Serial, d.SOC, d.Status, d.FailStreak, d.ClientID, d.ClientFailStreak)
		if d.LeaseTaskID != "" {
			fmt.Fprintf(&b, " lease=%s", d.LeaseTaskID)
		}
	}
	return b.String(), nil
}

func (e *Executor) devices(ctx context.Context, args []string) (string, error) {
	scope := "online"
	if len(args) > 1 {
		return "用法: devices [online|all|offline|quarantined]", nil
	}
	if len(args) == 1 {
		switch args[0] {
		case "online", "all", "offline", "quarantined":
			scope = args[0]
		default:
			return "用法: devices [online|all|offline|quarantined]", nil
		}
	}
	ov, err := e.Store.FleetOverview(ctx)
	if err != nil {
		return "", err
	}
	var matched []store.DeviceStatus
	for _, d := range ov.Devices {
		switch scope {
		case "all":
			matched = append(matched, d)
		case "online":
			if d.Status == store.DeviceIdle || d.Status == store.DeviceBusy {
				matched = append(matched, d)
			}
		case "offline":
			if d.Status == store.DeviceOffline {
				matched = append(matched, d)
			}
		case "quarantined":
			if d.Status == store.DeviceQuarantined {
				matched = append(matched, d)
			}
		}
	}
	if len(matched) == 0 {
		return fmt.Sprintf("无 %s 设备", scope), nil
	}
	var b strings.Builder
	for _, d := range matched {
		fmt.Fprintf(&b, "%s  serial=%s soc=%s status=%s fail_streak=%d client=%s client_fail=%d\n",
			deviceDisplayName(d), d.Serial, d.SOC, d.Status, d.FailStreak, d.ClientID, d.ClientFailStreak)
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

func deviceDisplayName(d store.DeviceStatus) string {
	if d.DisplayName != "" {
		return d.DisplayName
	}
	if d.SOC != "" {
		return strings.ToUpper(d.SOC) + "-" + d.Serial
	}
	return "UNKNOWN-" + d.Serial
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
	for _, arg := range args {
		if !validRerunArg(arg) {
			return "rerun 参数必须无空白且单项不超过 512 字符", nil
		}
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
			if summary.Verdict == string(rules.VerdictPassed) || summary.Verdict == wf.VerdictSkipped ||
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
		if id := e.retryInFlight(ctx, source, variant); id != "" {
			return fmt.Sprintf("重试正在进行中: %s", id), nil
		}
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

// testCmd 实现 test <variant> [commit]:校验变体名 → 解析 artifact(指定 commit
// 或该变体最近构建)→ 启动单变体 workflow(scope=variant)。与 rerun 同链路
// (NextWorkflowAttempt + StartDeviceTest)。设计见
// docs/superpowers/specs/2026-08-07-feishu-test-command-design.md。
func (e *Executor) testCmd(ctx context.Context, args []string) (string, error) {
	if len(args) < 1 || len(args) > 2 {
		return "用法: test <variant> [commit]", nil
	}
	variant := args[0]
	valid := false
	for _, v := range e.Variants {
		if v == variant {
			valid = true
			break
		}
	}
	if !valid {
		return fmt.Sprintf("未知变体 %s。可用变体: %s", variant, strings.Join(e.Variants, ", ")), nil
	}
	commit := ""
	if len(args) == 2 {
		if validateSHA(args[1]) != nil {
			return fmt.Sprintf("commit 形态不合法(7-40 位小写 hex): %s", args[1]), nil
		}
		commit = args[1]
	}

	var art *store.Artifact
	if commit != "" {
		// 指定 commit:从最近 runs 找含该 commit 且含该变体的 run,取其 artifact。
		runs, err := e.Store.RecentRuns(ctx, 50)
		if err != nil {
			return "", err
		}
		for _, r := range runs {
			if r.Commit != commit || !containsVariant(r, variant) {
				continue
			}
			arts, err := e.Store.ListArtifacts(ctx, r.Project, r.Commit, r.PipelineID)
			if err != nil {
				return "", err
			}
			for _, a := range arts {
				if a.Variant == variant {
					art = &a
					break
				}
			}
			if art != nil {
				break
			}
		}
		if art == nil {
			return fmt.Sprintf("commit %s 无变体 %s 的构建记录", commit, variant), nil
		}
	} else {
		// 缺省 commit:该变体最近一次构建(project 从 artifact 自身带出)。
		latest, err := e.Store.LatestArtifactForVariant(ctx, variant)
		if err != nil {
			return "", err
		}
		if latest == nil {
			return fmt.Sprintf("变体 %s 暂无构建记录", variant), nil
		}
		art = latest
	}

	// 防连点:该变体最新 attempt 的 workflow 仍在运行则拒绝(与 rerun 的
	// retryInFlight 同语义,避免用户连发 test 命令启动多个重复测试)。
	if id := e.testInFlight(ctx, art, variant); id != "" {
		return fmt.Sprintf("测试正在进行中: %s", id), nil
	}

	// 启动;遇到已存在 ID 时区分两种情况:已终态(如手动启动的历史残留)
	// 自动推进到下一 attempt 重试;运行中(并发启动的竞态)则提示,不重复派发。
	maxAdvance := 5
	for {
		n, err := e.Store.NextWorkflowAttempt(ctx, art.Project, art.CommitSHA, art.PipelineID, variant)
		if err != nil {
			return "", fmt.Errorf("next workflow attempt: %w", err)
		}
		in := wf.DeviceTestInput{
			Project: art.Project, Commit: art.CommitSHA, PipelineID: art.PipelineID,
			// Version 来自 artifact 登记的包版本(bundle/kick 写入;workflow_runs 必填)。
			Version:     art.Version,
			Packages:    []wf.PackageRef{pkgRef(*art)},
			Scope:       variant,
			RuleVersion: rules.DefaultVersion,
			Attempt:     n,
		}
		id, started, err := e.Starter.StartDeviceTest(ctx, in)
		if err != nil {
			return "", fmt.Errorf("start workflow: %w", err)
		}
		if started {
			return fmt.Sprintf("已启动: %s\n变体 %s (%s g%s p%d)", id, variant, art.BuildType,
				art.CommitSHA, art.PipelineID), nil
		}
		closed, err := e.Starter.WorkflowClosed(ctx, id)
		if err != nil {
			return "", fmt.Errorf("workflow closed check %s: %w", id, err)
		}
		if !closed {
			return fmt.Sprintf("测试正在进行中: %s", id), nil
		}
		maxAdvance--
		if maxAdvance <= 0 {
			return fmt.Sprintf("workflow 已存在且均为终态,推进超限,请稍后重试: %s", id), nil
		}
	}
}

// containsVariant 判断某 run 是否就是指定变体(RecentRun 是单变体行)。
func containsVariant(r store.RecentRun, variant string) bool {
	return r.Variant == variant
}

func isPositiveInt(s string) bool {
	n, err := strconv.Atoi(s)
	return err == nil && n > 0
}

func validRerunArg(s string) bool {
	return s != "" && utf8.ValidString(s) && utf8.RuneCountInString(s) <= 512 &&
		strings.IndexFunc(s, unicode.IsSpace) < 0
}

func pkgRef(a store.Artifact) wf.PackageRef {
	return wf.PackageRef{
		Variant: a.Variant, URL: a.URL, SHA256: a.SHA256,
		Size: a.Size, ManifestDigest: a.ManifestDigest,
	}
}

// retryInFlight 防连点认领:同一源运行+变体已有未关闭的重试 workflow 时
// 返回其 ID,否则返回 ""。查询失败按"无"处理——重试是显式人工动作,
// Temporal 抖动不应阻塞;此时退化为改动前行为(attempt 原子递增,
// 连点只造成排队重复执行,无数据错乱)。
func (e *Executor) retryInFlight(ctx context.Context, source *store.WorkflowRun, variant string) string {
	cur, err := e.Store.CurrentWorkflowAttempt(
		ctx, source.Project, source.CommitSHA, source.PipelineID, variant)
	if err != nil || cur < 1 {
		return ""
	}
	latestID := wf.DeviceTestInput{
		Project: source.Project, Commit: source.CommitSHA, PipelineID: source.PipelineID,
		Scope: variant, Attempt: cur,
	}.WorkflowID()
	closed, err := e.Starter.WorkflowClosed(ctx, latestID)
	if err != nil || closed {
		return ""
	}
	return latestID
}

// testInFlight 防连点认领(test 命令用):指定变体最新 attempt 的 workflow
// 未关闭时返回其 ID,否则返回 ""。查询失败按"无"处理——test 是显式人工
// 动作,宁可放行也不阻塞用户(与 retryInFlight 一致)。
func (e *Executor) testInFlight(ctx context.Context, art *store.Artifact, variant string) string {
	cur, err := e.Store.CurrentWorkflowAttempt(
		ctx, art.Project, art.CommitSHA, art.PipelineID, variant)
	if err != nil || cur < 1 {
		return ""
	}
	latestID := wf.DeviceTestInput{
		Project: art.Project, Commit: art.CommitSHA, PipelineID: art.PipelineID,
		Scope: variant, Attempt: cur,
	}.WorkflowID()
	closed, err := e.Starter.WorkflowClosed(ctx, latestID)
	if err != nil || closed {
		return ""
	}
	return latestID
}

// retryVariant 从已校验的 workflow run 与 variant 启动单变体重试。
// 步骤:查 artifact → 取 attempt 号 → 构造 DeviceTestInput → StartDeviceTest。
// source 不能 nil,已由调用方校验。
func (e *Executor) retryVariant(
	ctx context.Context, source *store.WorkflowRun, variant string,
) (string, error) {
	arts, err := e.Store.ListArtifacts(ctx, source.Project, source.CommitSHA, source.PipelineID)
	if err != nil {
		return "", err
	}
	var ref wf.PackageRef
	found := false
	for _, art := range arts {
		if art.Variant == variant {
			ref = pkgRef(art)
			found = true
			break
		}
	}
	if !found {
		return fmt.Sprintf("变体 %s 的 artifact 不存在", variant), nil
	}
	if id := e.retryInFlight(ctx, source, variant); id != "" {
		return fmt.Sprintf("重试正在进行中: %s", id), nil
	}
	n, err := e.Store.NextWorkflowAttempt(
		ctx, source.Project, source.CommitSHA, source.PipelineID, variant,
	)
	if err != nil {
		return "", err
	}
	in := wf.DeviceTestInput{
		Project: source.Project, Commit: source.CommitSHA, PipelineID: source.PipelineID,
		Version:     source.Version,
		Scope:      variant,
		Attempt:    n,
		Packages:   []wf.PackageRef{ref},
	}
	id, started, err := e.Starter.StartDeviceTest(ctx, in)
	if err != nil {
		return "", fmt.Errorf("start workflow: %w", err)
	}
	if !started {
		return fmt.Sprintf("workflow 已存在: %s", id), nil
	}
	return fmt.Sprintf("已启动重试 %s: %s", variant, id), nil
}

// HandleCardAction 处理飞书交互卡片按钮点击(card.action.trigger 经 WS 送达)。
// 返回值:(回复文本, toast 类型)。toastType 为空时不弹 toast(静默记录)。
func (e *Executor) HandleCardAction(
	ctx context.Context, value wf.ButtonValue, openID string,
) (text, toastType string, err error) {
	if !e.Whitelist[openID] {
		e.log().Info().Str("open_id", openID).Msg("card action from non-whitelist sender, ignored")
		return "", "", nil
	}

	// 参数防御:action/源 workflow/variant 三者缺一不可
	if value.Action == "" || value.SourceWorkflowID == "" || value.Variant == "" {
		return "按钮载荷不完整", "error", nil
	}
	if !validRerunArg(value.SourceWorkflowID) || !validRerunArg(value.Variant) {
		return "按钮载荷非法字符", "error", nil
	}

	source, err := e.Store.GetWorkflowRun(ctx, value.SourceWorkflowID)
	if errors.Is(err, store.ErrWorkflowRunNotFound) {
		return "该次运行记录已过期或不存在", "info", nil
	}
	if err != nil {
		return "", "", fmt.Errorf("get workflow run %s: %w", value.SourceWorkflowID, err)
	}

	switch value.Action {
	case "retry":
		closed, cerr := e.Starter.WorkflowClosed(ctx, value.SourceWorkflowID)
		if cerr != nil {
			return fmt.Sprintf("检查 workflow 状态失败: %v", cerr), "error", nil
		}
		if !closed {
			return "workflow 尚未结束，无法重试", "info", nil
		}
		text, err = e.retryVariant(ctx, source, value.Variant)
		if err != nil {
			return "", "", err
		}
		return text, "success", nil

	case "ignore":
		// 记录人工忽略裁决:decisions 表 actor="human",output 存按钮载荷。
		// decisions.task_id 有 FK 指向 tasks,必须落该变体最新任务的真实
		// task_id,不能直接写 workflow_id(实测违反 decisions_task_id_fkey)。
		taskID, terr := e.Store.LatestTaskIDForVariant(ctx, source.WorkflowID, value.Variant)
		if terr != nil {
			return "", "", fmt.Errorf("lookup task for %s/%s: %w", source.WorkflowID, value.Variant, terr)
		}
		if taskID == "" {
			return "该变体在此运行中没有任务记录", "info", nil
		}
		output, _ := json.Marshal(value)
		dec := wf.DecisionRow{
			TaskID: taskID, Actor: "human",
			Output: output,
		}
		if err := e.saveDecision(ctx, dec); err != nil {
			return "", "", err
		}
		e.log().Info().
			Str("open_id", openID).
			Str("workflow", value.SourceWorkflowID).
			Str("variant", value.Variant).
			Msg("card action: ignore")
		return fmt.Sprintf("已忽略 %s", value.Variant), "success", nil

	default:
		return fmt.Sprintf("未知操作: %s", value.Action), "error", nil
	}
}

// saveDecision 落 decisions 表(actor="human" 的卡片操作)。
// 重复插入(相同 task_id+actor)静默成功(幂等)。
func (e *Executor) saveDecision(ctx context.Context, row wf.DecisionRow) error {
	// Store 接口里没有 SaveDecision,需要通过类型断言;如果 store 不支持则降级记日志。
	type decisionSaver interface {
		SaveDecision(ctx context.Context, row wf.DecisionRow) error
	}
	if ds, ok := e.Store.(decisionSaver); ok {
		return ds.SaveDecision(ctx, row)
	}
	e.log().Warn().Str("task_id", row.TaskID).Msg("store does not support SaveDecision, card action unrecorded")
	return nil
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

// planCmd 调用 Planner 将自然语言需求翻译成 Plan DSL,
// 输出格式化的计划摘要供用户确认。
func (e *Executor) planCmd(ctx context.Context, args []string) (string, error) {
	raw := ""
	if len(args) > 0 {
		raw = strings.TrimSpace(args[0])
	}
	if raw == "" {
		return "用法: plan <需求描述>\n例如: plan 测一下master的SNPE 2.21", nil
	}
	if e.Planner == nil {
		return "规划器未启用(HERMES_ENDPOINT 未配置)", nil
	}

	// 组装上下文快照:变体名单 + 设备状态
	variants := e.Variants
	if variants == nil {
		variants = []string{}
	}
	ctxSnap := map[string]any{
		"now":      e.nowFn().UTC().Format(time.RFC3339),
		"variants": variants,
		"devices":  []map[string]string{},
	}
	if ov, err := e.Store.FleetOverview(ctx); err == nil {
		devs := make([]map[string]string, 0, len(ov.Devices))
		for _, d := range ov.Devices {
			devs = append(devs, map[string]string{
				"device_id": d.DeviceID, "serial": d.Serial, "status": d.Status, "soc": d.SOC,
			})
		}
		ctxSnap["devices"] = devs
	}
	ctxJSON, _ := json.Marshal(ctxSnap)

	planRaw, err := e.Planner.Plan(ctx, hermesclient.PlanRequest{
		RawText: raw,
		Context: ctxJSON,
	})
	if err != nil {
		e.log().Warn().Err(err).Msg("planner failed")
		if errors.Is(err, hermesclient.ErrSchemaInvalid) {
			return "规划服务返回的数据格式不合法(已重试多次),请换个说法再试", nil
		}
		return fmt.Sprintf("规划服务暂时不可用,请稍后重试\n(%v)", err), nil
	}

	// 格式化计划摘要
	var plan struct {
		PlanID      string `json:"plan_id"`
		GoalSummary string `json:"goal_summary"`
		Build       struct {
			Project   string   `json:"project"`
			Ref       string   `json:"ref"`
			Targets   []string `json:"targets"`
			BuildType string   `json:"build_type"`
		} `json:"build"`
		Tests []struct {
			TestID string `json:"test_id"`
			Device struct {
				SOC          []string `json:"soc"`
				Capabilities []string `json:"capabilities"`
			} `json:"device_selector"`
		} `json:"tests"`
	}
	if err := json.Unmarshal(planRaw, &plan); err != nil {
		return fmt.Sprintf("规划成功但解析失败: %v\n原始输出:\n%s", err, truncStr(string(planRaw), 500)), nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "📋 测试计划 %s\n", plan.PlanID)
	fmt.Fprintf(&b, "  目标: %s\n", plan.GoalSummary)
	bt := plan.Build.BuildType
	if bt == "" {
		bt = "Release"
	}
	fmt.Fprintf(&b, "  构建: %s %s %s\n", plan.Build.Project, plan.Build.Ref, bt)
	fmt.Fprintf(&b, "  变体(%d):\n", len(plan.Build.Targets))
	for _, t := range plan.Build.Targets {
		fmt.Fprintf(&b, "    · %s\n", t)
	}
	fmt.Fprintf(&b, "  测试项(%d):\n", len(plan.Tests))
	for _, tk := range plan.Tests {
		soc := ""
		if len(tk.Device.SOC) > 0 {
			soc = "(" + strings.Join(tk.Device.SOC, ",") + ")"
		}
		caps := ""
		if len(tk.Device.Capabilities) > 0 {
			caps = " [" + strings.Join(tk.Device.Capabilities, ",") + "]"
		}
		fmt.Fprintf(&b, "    · %s %s%s\n", tk.TestID, soc, caps)
	}
	b.WriteString("\n⏳ 规划阶段不执行测试(实现计划为 Phase 2b);可复制 plan_id 用于后续触发。")
	return b.String(), nil
}

func truncStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// quarantine 与 unquarantine 对称:手动隔离设备(BUSY 运行中不隔离)。
func (e *Executor) quarantine(ctx context.Context, args []string) (string, error) {
	if len(args) > 1 {
		return "用法: quarantine [device_id]", nil
	}
	if len(args) == 1 {
		ok, err := e.Store.QuarantineDevice(ctx, args[0])
		if err != nil {
			return "", err
		}
		if !ok {
			return fmt.Sprintf("无法隔离 %s(不存在或运行中)", args[0]), nil
		}
		return fmt.Sprintf("已隔离: %s", args[0]), nil
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
		ok, err := e.Store.QuarantineDevice(ctx, d.DeviceID)
		if err != nil {
			return "", err
		}
		if !ok {
			return fmt.Sprintf("无法隔离 %s(运行中)", d.DeviceID), nil
		}
		return fmt.Sprintf("已隔离: %s(唯一设备,自动选定)", d.DeviceID), nil
	default:
		var b strings.Builder
		b.WriteString("多台设备,请指定 id: quarantine <device_id>\n")
		for _, d := range ov.Devices {
			fmt.Fprintf(&b, "  %s status=%s\n", d.DeviceID, d.Status)
		}
		return strings.TrimRight(b.String(), "\n"), nil
	}
}

// runs [n] 展示最近运行历史(RecentRuns 权威优先)。
func (e *Executor) runs(ctx context.Context, args []string) (string, error) {
	limit := 5
	if len(args) > 1 {
		return "用法: runs [n]", nil
	}
	if len(args) == 1 {
		n, err := strconv.Atoi(args[0])
		if err != nil || n < 1 || n > 20 {
			return "用法: runs [n],n 为 1-20 的整数", nil
		}
		limit = n
	}
	runs, err := e.Store.RecentRuns(ctx, limit)
	if err != nil {
		return "", err
	}
	if len(runs) == 0 {
		return "暂无运行记录", nil
	}
	var b strings.Builder
	for _, r := range runs {
		mark := "?"
		switch r.Verdict {
		case "PASSED":
			mark = "✅"
		case "TEST_FAILED", "INFRA_ERROR", "TIMEOUT":
			mark = "❌"
		case wf.VerdictSkipped:
			mark = "⏭"
		}
		authority := ""
		if r.Authoritative {
			authority = "*"
		}
		verdict := r.Verdict
		if verdict == "" {
			verdict = "运行中"
		}
		fmt.Fprintf(&b, "%s%s %s %s g%s p%d %s %s\n",
			mark, authority, r.Variant, verdict, r.Commit, r.PipelineID,
			r.WorkflowID, formatEnded(r.EndedAt, e.nowFn()))
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// result <workflow_id> 展示单次运行各变体结论(task verdict + 指标)。
func (e *Executor) resultCmd(ctx context.Context, args []string) (string, error) {
	if len(args) != 1 {
		return "用法: result <workflow_id>", nil
	}
	wfid := args[0]
	run, err := e.Store.GetWorkflowRun(ctx, wfid)
	if errors.Is(err, store.ErrWorkflowRunNotFound) {
		return fmt.Sprintf("查无 workflow: %s", wfid), nil
	}
	if err != nil {
		return "", err
	}
	if run == nil {
		return fmt.Sprintf("查无 workflow: %s", wfid), nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s\ng%s p%d v%s %s", run.WorkflowID, run.CommitSHA, run.PipelineID,
		run.Version, run.RuleVersion)
	for _, v := range run.Variants {
		taskID, err := e.Store.LatestTaskIDForVariant(ctx, wfid, v)
		if err != nil {
			return "", err
		}
		if taskID == "" {
			fmt.Fprintf(&b, "\n  %s 无任务", v)
			continue
		}
		task, err := e.Store.GetTask(ctx, taskID)
		if err != nil {
			return "", err
		}
		status := "?"
		if task != nil {
			status = task.Status
		}
		fmt.Fprintf(&b, "\n  %s [%s]", v, status)
		if rec, err := e.Store.GetResult(ctx, taskID); err != nil {
			return "", err
		} else if rec != nil {
			rs := rec.Result
			fmt.Fprintf(&b, " exit=%d dur=%.0fs cases=%d/%d", rs.ExitCode, rs.DurationSec,
				rs.CasesTotal-rs.CasesFailed, rs.CasesTotal)
			if len(rs.Metrics) > 0 {
				names := make([]string, 0, len(rs.Metrics))
				for m := range rs.Metrics {
					names = append(names, m)
				}
				sort.Strings(names)
				fmt.Fprintf(&b, " |")
				for _, m := range names {
					fmt.Fprintf(&b, " %s=%.1f", shortMetric(m), rs.Metrics[m])
				}
			}
			if len(rs.Attachments) > 0 {
				fmt.Fprintf(&b, " | 附件 %d", len(rs.Attachments))
			}
		}
	}
	return b.String(), nil
}

// metrics <variant> 展示某变体最近性能指标(metrics 表,§9 基线数据源)。
func (e *Executor) metrics(ctx context.Context, args []string) (string, error) {
	if len(args) != 1 {
		return "用法: metrics <variant>", nil
	}
	points, err := e.Store.MetricsForVariant(ctx, args[0], 30)
	if err != nil {
		return "", err
	}
	if len(points) == 0 {
		return fmt.Sprintf("变体 %s 暂无指标记录", args[0]), nil
	}
	// 按 metric 聚合为"最近值 + 均值",按 metric 名排序。注意 MetricsForVariant
	// 从最新往旧排,latest 只在首次(最新)赋值。
	latest := map[string]float64{}
	sum := map[string]float64{}
	count := map[string]int{}
	order := []string{}
	seen := map[string]bool{}
	for _, p := range points {
		if !seen[p.MetricName] {
			seen[p.MetricName] = true
			order = append(order, p.MetricName)
			latest[p.MetricName] = p.Value
		}
		sum[p.MetricName] += p.Value
		count[p.MetricName]++
	}
	sort.Strings(order)
	var b strings.Builder
	fmt.Fprintf(&b, "%s 最近 %d 条指标:", args[0], len(points))
	for _, m := range order {
		avg := sum[m] / float64(count[m])
		fmt.Fprintf(&b, "\n  %s: 最新 %.1f / 均值 %.1f (n=%d)", shortMetric(m), latest[m], avg, count[m])
	}
	return b.String(), nil
}

// artifacts <variant> 展示某变体最近构建历史(最近 10 条)。
func (e *Executor) artifacts(ctx context.Context, args []string) (string, error) {
	if len(args) != 1 {
		return "用法: artifacts <variant>", nil
	}
	arts, err := e.Store.ListArtifactsForVariant(ctx, args[0], 10)
	if err != nil {
		return "", err
	}
	if len(arts) == 0 {
		return fmt.Sprintf("变体 %s 暂无构建记录", args[0]), nil
	}
	var b strings.Builder
	for _, a := range arts {
		fmt.Fprintf(&b, "%s v%s g%s p%d\n", a.Project, a.Version, a.CommitSHA, a.PipelineID)
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// cancel <workflow_id> 取消运行中的 workflow(Temporal Terminate;已终态幂等)。
func (e *Executor) cancel(ctx context.Context, args []string) (string, error) {
	if len(args) != 1 {
		return "用法: cancel <workflow_id>", nil
	}
	closed, err := e.Starter.WorkflowClosed(ctx, args[0])
	if err != nil {
		return "", err
	}
	if closed {
		return fmt.Sprintf("workflow 已终态,无需取消: %s", args[0]), nil
	}
	if err := e.Starter.TerminateWorkflow(ctx, args[0], "feishu cancel command"); err != nil {
		return "", err
	}
	return fmt.Sprintf("已取消: %s", args[0]), nil
}

// shortMetric 截断过长的 metric 名(如 xxx_test.inference_ms_avg → 保留前 24 字符)。
func shortMetric(m string) string {
	if len(m) <= 24 {
		return m
	}
	return m[:21] + "..."
}

// formatEnded 格式化 UTC 时间,零值返回 "—"。
func formatEnded(t time.Time, now time.Time) string {
	if t.IsZero() {
		return "—"
	}
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%.0fs前", d.Seconds())
	case d < time.Hour:
		return fmt.Sprintf("%.0fm前", d.Minutes())
	case d < 24*time.Hour:
		return fmt.Sprintf("%.0fh前", d.Hours())
	default:
		return fmt.Sprintf("%.0fd前", d.Hours()/24)
	}
}
