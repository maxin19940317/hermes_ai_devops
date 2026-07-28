package feishucmd

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/rs/zerolog"

	"hermes-devops/runtime/internal/feishu"
	"hermes-devops/runtime/internal/store"
	wf "hermes-devops/runtime/internal/workflow"
)

// Store 是指令执行依赖的持久层子集(MemStore/PGStore 均满足)。
type Store interface {
	FleetOverview(ctx context.Context) (*store.FleetOverview, error)
	UnquarantineDevice(ctx context.Context, deviceID string) (bool, error)
	ListArtifacts(ctx context.Context, commitSHA string, pipelineID int) ([]store.Artifact, error)
	NextWorkflowAttempt(ctx context.Context, commitSHA string, pipelineID int, variant string) (int, error)
	NextWorkflowAttemptAll(ctx context.Context, commitSHA string, pipelineID int) (int, error)
	// 以下三个供意图翻译层使用(设计文档 §3.1)
	RecentRuns(ctx context.Context, limit int) ([]store.RecentRun, error)
	SaveCommandTranslation(ctx context.Context, row store.CommandTranslation) error
	ListCommandTranslations(ctx context.Context, openID string, limit int) ([]store.CommandTranslation, error)
}

// WorkflowStarter 启动 DeviceTestWorkflow(trigger.TemporalStarter 满足)。
type WorkflowStarter interface {
	StartDeviceTest(ctx context.Context, in wf.DeviceTestInput) (workflowID string, started bool, err error)
}

// Executor 是指令执行体:鉴权(白名单)→ 解析 → 执行 → 文本回复。
// 全部依赖为接口/函数值,单测可 fake。
type Executor struct {
	Store     Store
	Starter   WorkflowStarter
	Sender    feishu.Sender   // 回复通道;nil = 只执行不回复(测试)
	Log       *zerolog.Logger // 可选;nil 用 Nop
	Whitelist map[string]bool
	// ExpectedVariants 是 bundle 全量变体数(variants.yaml 声明数),
	// rerun 无 variant 时的包齐整性判据;0 = 不校验齐整性。
	ExpectedVariants int
}

func (e *Executor) log() zerolog.Logger {
	if e.Log != nil {
		return *e.Log
	}
	return zerolog.Nop()
}

// HandleMessage 处理一条单聊文本消息。安全红线:非白名单 open_id 静默忽略
// (不回复,防探测),记 info 日志。
func (e *Executor) HandleMessage(ctx context.Context, openID, text string) {
	log := e.log()
	if !e.Whitelist[openID] {
		log.Info().Str("open_id", openID).Msg("feishu cmd from non-whitelist sender, ignored")
		return
	}
	cmd := Parse(text)
	reply, err := e.execute(ctx, cmd)
	if err != nil {
		log.Error().Err(err).Str("cmd", cmd.Name).Msg("feishu cmd failed")
		reply = fmt.Sprintf("指令执行失败: %v", err)
	}
	log.Info().Str("open_id", openID).Str("cmd", cmd.Name).Msg("feishu cmd executed")
	if e.Sender != nil {
		if err := e.Sender.SendText(ctx, reply); err != nil {
			log.Error().Err(err).Msg("feishu cmd reply failed")
		}
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
		fmt.Fprintf(&b, "\n  %s %s %s fail_streak=%d", d.Serial, d.SOC, d.Status, d.FailStreak)
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
		fmt.Fprintf(&b, "%s  soc=%s status=%s fail_streak=%d\n", d.Serial, d.SOC, d.Status, d.FailStreak)
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// rerun <sha8> <pipeline_iid> [variant]:用 artifacts 表重建 DeviceTestInput
// 显式启动(-r{N} 后缀来自 workflow_attempt 递增,差距 #11)。
// 查无记录/包不齐/参数非法 → 明确报错文本(不返回 error,不算执行失败)。
func (e *Executor) rerun(ctx context.Context, args []string) (string, error) {
	if len(args) < 2 || len(args) > 3 {
		return "用法: rerun <sha8> <pipeline_iid> [variant]", nil
	}
	sha := strings.ToLower(args[0])
	if err := validateSHA(sha); err != nil {
		return fmt.Sprintf("非法 sha: %v", err), nil
	}
	iid, err := strconv.Atoi(args[1])
	if err != nil || iid <= 0 {
		return fmt.Sprintf("非法 pipeline_iid %q(正整数)", args[1]), nil
	}
	arts, err := e.Store.ListArtifacts(ctx, sha, iid)
	if err != nil {
		return "", err
	}
	if len(arts) == 0 {
		return fmt.Sprintf("查无记录: g%s p%d 未登记任何 artifact", sha, iid), nil
	}
	in := wf.DeviceTestInput{
		Project: arts[0].Project, Commit: sha, PipelineID: iid,
	}
	if len(args) == 3 {
		// 单变体重跑:Scope=variant,ID 含变体后缀
		variant := args[2]
		var art *store.Artifact
		for i := range arts {
			if arts[i].Variant == variant {
				art = &arts[i]
				break
			}
		}
		if art == nil {
			avail := make([]string, 0, len(arts))
			for _, a := range arts {
				avail = append(avail, a.Variant)
			}
			return fmt.Sprintf("变体 %s 无记录;已登记: %s", variant, strings.Join(avail, ", ")), nil
		}
		n, err := e.Store.NextWorkflowAttempt(ctx, sha, iid, variant)
		if err != nil {
			return "", err
		}
		in.Scope = variant
		in.Attempt = n
		in.Packages = []wf.PackageRef{pkgRef(*art)}
	} else {
		// 全量重跑:包不齐拒绝(被 interruptible 打断的残缺构建不得重跑)
		if e.ExpectedVariants > 0 && len(arts) < e.ExpectedVariants {
			return fmt.Sprintf("包不齐: g%s p%d 仅 %d/%d 个变体有 artifact,请补齐后重试",
				sha, iid, len(arts), e.ExpectedVariants), nil
		}
		n, err := e.Store.NextWorkflowAttemptAll(ctx, sha, iid)
		if err != nil {
			return "", err
		}
		in.Attempt = n
		for _, a := range arts {
			in.Packages = append(in.Packages, pkgRef(a))
		}
	}
	id, started, err := e.Starter.StartDeviceTest(ctx, in)
	if err != nil {
		return "", fmt.Errorf("start workflow: %w", err)
	}
	if !started {
		return fmt.Sprintf("workflow 已存在(幂等,未重启): %s", id), nil
	}
	return fmt.Sprintf("已启动: %s(%d 个变体)", id, len(in.Packages)), nil
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
