// Package rerun 是文本 rerun 指令与卡片重试按钮共用的只读解析层。
// 抽出来是为了让两个入口共享一份业务语义:复制必然漂移,而"文本 rerun 回复逐字不变"
// 是本轮的验收项(设计 §4)。本包只做只读解析(取权威 run → 校验已关闭 → 定位
// 目标变体集 → 解析 artifact);attempt 分配与 workflow 启动留在调用方
// (runtime/internal/feishucmd.Executor.rerun),因为那两步会产生副作用,
// 卡片按钮与文本指令各自的幂等/审计要求可能不同,不适合在只读层里做决定。
package rerun

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"hermes-devops/runtime/internal/rules"
	"hermes-devops/runtime/internal/store"
	wf "hermes-devops/runtime/internal/workflow"
)

// Store 是 Resolver 依赖的持久层子集,窄接口以避免反向依赖
// runtime/internal/feishucmd(其 Store 接口字段更多,是执行层的超集)。
type Store interface {
	GetWorkflowRun(ctx context.Context, workflowID string) (*store.WorkflowRun, error)
	ListArtifacts(ctx context.Context, project, commitSHA string, pipelineID int) ([]store.Artifact, error)
}

// WorkflowLookup 是 Resolver 依赖的 Temporal 只读查询子集
// (trigger.TemporalStarter 满足;定义在这里而非引用 feishucmd.WorkflowStarter,
// 避免本包反向依赖 feishucmd)。
type WorkflowLookup interface {
	WorkflowClosed(ctx context.Context, workflowID string) (bool, error)
	WorkflowResult(ctx context.Context, workflowID string) (*wf.DeviceTestOutput, error)
}

// Resolver 把一个源 workflow ID(+可选 variant)解析成可直接拿去起
// DeviceTestWorkflow 的目标变体集与 artifact 引用。
type Resolver struct {
	Store   Store
	Starter WorkflowLookup
}

// FailureRun 是 ResolveFailureRun 的结果:权威 run 加上它失败/待重试的变体集。
// ignore 类动作只需要这一层,不需要 artifact(见 ResolveFailureRun 的 doc)。
type FailureRun struct {
	Run     store.WorkflowRun
	Targets []string
}

// Resolution 是 ResolveRetry 的结果:在 FailureRun 之上多出可直接派单的
// artifact 引用与本次 retry 的 scope(explicit variant 时为该 variant,
// 否则沿用源 run 的 scope)。
type Resolution struct {
	Run      store.WorkflowRun
	Targets  []string
	Packages []wf.PackageRef
	Scope    string
}

// RejectReason 是解析被拒绝的原因,executor 按 Code 渲染既有文本 rerun 的逐字
// 回复(见 feishucmd.Executor.rerun)。取值:
// NotAuthoritative / StillRunning / CheckFailed / ResultUnreadable /
// NoFailedVariants / VariantNotMember / ArtifactMissing。
//
// CONTRACT-ISSUE: 原 executor.rerun 对 WorkflowClosed 探测失败(不是"运行中",
// 是探测本身出错,如 Temporal Describe 报错)有独立文案"检查 workflow 状态失败",
// 与 WorkflowResult 读取失败的"读取 workflow 结果失败"不是一回事;这里补一个
// 单独的 CheckFailed 分类,而不是像本任务 brief 骨架示例那样把两者都塞进
// ResultUnreadable(那样会丢失"探测失败"与"结果读取失败"的既有文案区分)。
type RejectReason struct {
	Code       string
	WorkflowID string
	Variant    string
	Count      int
	// Err 携带触发本次拒绝的底层错误,仅 CheckFailed(WorkflowClosed 探测失败)
	// 与 ResultUnreadable(WorkflowResult 读取失败)两种 Code 非空。原
	// executor.rerun 对这两种情况的回复都是 "...失败: %v"(err 原文,例如
	// "context deadline exceeded"),丢弃它会让运维无法从回复区分超时/网络/
	// 权限等不同故障——这是"文案逐字不变"约束的一部分,不是可选项。
	Err error
}

// Error 满足 error 接口;不是 executor 渲染回复用的文案(那由 Code 驱动),
// 只用于日志/兜底场景下人可读的诊断串。
func (e *RejectReason) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("rerun rejected: %s (workflow=%s variant=%s count=%d): %v",
			e.Code, e.WorkflowID, e.Variant, e.Count, e.Err)
	}
	return fmt.Sprintf("rerun rejected: %s (workflow=%s variant=%s count=%d)",
		e.Code, e.WorkflowID, e.Variant, e.Count)
}

// Unwrap 暴露 Err,让 errors.Is/errors.As 能穿透 RejectReason 找到底层原因
// (例如上游想单独判断是不是 context.DeadlineExceeded),同时 Code 仍留给
// executor 做分支渲染,两者不冲突。
func (e *RejectReason) Unwrap() error {
	return e.Err
}

// resolveRun 取权威 run 并确认它已终结:两个入口(ResolveFailureRun 的隐式路径、
// ResolveRetry 的显式路径)共用这一段,顺序固定为 GetWorkflowRun → WorkflowClosed。
func (r *Resolver) resolveRun(ctx context.Context, workflowID string) (*store.WorkflowRun, error) {
	run, err := r.Store.GetWorkflowRun(ctx, workflowID)
	if errors.Is(err, store.ErrWorkflowRunNotFound) {
		return nil, &RejectReason{Code: "NotAuthoritative", WorkflowID: workflowID}
	}
	if err != nil {
		return nil, fmt.Errorf("get workflow run %s: %w", workflowID, err)
	}
	closed, err := r.Starter.WorkflowClosed(ctx, workflowID)
	if err != nil {
		return nil, &RejectReason{Code: "CheckFailed", WorkflowID: workflowID, Err: err}
	}
	if !closed {
		return nil, &RejectReason{Code: "StillRunning", WorkflowID: workflowID}
	}
	return run, nil
}

// ResolveFailureRun 只做:权威 run + 已关闭 + 失败 summary 集合。
// ignore 只需要这一层——它是纯记录动作,不应因 artifact 缺失而失败
// (TestResolveFailureRunIgnoresArtifacts)。
func (r *Resolver) ResolveFailureRun(ctx context.Context, workflowID string) (*FailureRun, error) {
	run, err := r.resolveRun(ctx, workflowID)
	if err != nil {
		return nil, err
	}
	out, err := r.Starter.WorkflowResult(ctx, workflowID)
	if err != nil {
		return nil, &RejectReason{Code: "ResultUnreadable", WorkflowID: workflowID, Err: err}
	}
	seen := map[string]struct{}{}
	for _, s := range out.Tasks {
		if s.Variant == "" || s.Verdict == string(rules.VerdictPassed) || s.Verdict == wf.VerdictSkipped {
			continue
		}
		seen[s.Variant] = struct{}{}
	}
	targets := make([]string, 0, len(seen))
	for v := range seen {
		targets = append(targets, v)
	}
	sort.Strings(targets)
	if len(targets) == 0 {
		return nil, &RejectReason{Code: "NoFailedVariants", WorkflowID: workflowID}
	}
	return &FailureRun{Run: *run, Targets: targets}, nil
}

// resolvePackages 校验每个目标变体属于源 run 的 Variants 集合,再在
// artifacts 表里为它定位恰好一条记录(四元组 project/commit/pipeline/variant)。
func (r *Resolver) resolvePackages(
	ctx context.Context, run *store.WorkflowRun, targets []string,
) ([]wf.PackageRef, error) {
	arts, err := r.Store.ListArtifacts(ctx, run.Project, run.CommitSHA, run.PipelineID)
	if err != nil {
		return nil, err
	}
	sourceVariants := make(map[string]struct{}, len(run.Variants))
	for _, variant := range run.Variants {
		sourceVariants[variant] = struct{}{}
	}
	byVariant := make(map[string][]store.Artifact, len(arts))
	for _, a := range arts {
		byVariant[a.Variant] = append(byVariant[a.Variant], a)
	}
	packages := make([]wf.PackageRef, 0, len(targets))
	for _, variant := range targets {
		if _, ok := sourceVariants[variant]; !ok {
			return nil, &RejectReason{Code: "VariantNotMember", WorkflowID: run.WorkflowID, Variant: variant}
		}
		matches := byVariant[variant]
		if len(matches) != 1 {
			return nil, &RejectReason{
				Code: "ArtifactMissing", WorkflowID: run.WorkflowID, Variant: variant, Count: len(matches),
			}
		}
		packages = append(packages, pkgRef(matches[0]))
	}
	return packages, nil
}

// ResolveRetry 解析一次 rerun(文本指令或卡片按钮共用):variant 非空是用户的
// 明确选择,只校验成员关系与 artifact,不读 Temporal output(因此允许重跑
// PASSED/SKIPPED);variant 为空则调用 ResolveFailureRun 取失败集,再解析
// artifact。两条路径最终都产出可直接派单的 Resolution。
func (r *Resolver) ResolveRetry(ctx context.Context, workflowID, variant string) (*Resolution, error) {
	if variant != "" {
		run, err := r.resolveRun(ctx, workflowID)
		if err != nil {
			return nil, err
		}
		targets := []string{variant}
		packages, err := r.resolvePackages(ctx, run, targets)
		if err != nil {
			return nil, err
		}
		return &Resolution{Run: *run, Targets: targets, Packages: packages, Scope: variant}, nil
	}

	fr, err := r.ResolveFailureRun(ctx, workflowID)
	if err != nil {
		return nil, err
	}
	packages, err := r.resolvePackages(ctx, &fr.Run, fr.Targets)
	if err != nil {
		return nil, err
	}
	return &Resolution{Run: fr.Run, Targets: fr.Targets, Packages: packages, Scope: fr.Run.Scope}, nil
}

func pkgRef(a store.Artifact) wf.PackageRef {
	return wf.PackageRef{
		Variant: a.Variant, URL: a.URL, SHA256: a.SHA256,
		Size: a.Size, ManifestDigest: a.ManifestDigest,
	}
}
