package activity

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/rs/zerolog"

	"hermes-devops/runtime/internal/feishu"
	"hermes-devops/runtime/internal/hermesclient"
	"hermes-devops/runtime/internal/store"
	wf "hermes-devops/runtime/internal/workflow"
)

// Store is the subset of persistence layer dependencies for activities;
// both store.MemStore and (later) PGStore satisfy this interface.
type Store interface {
	AcquireDevice(ctx context.Context, sel wf.DeviceSelector, taskID string, leaseSeconds int) (*wf.Lease, error)
	ReleaseDevice(ctx context.Context, deviceID, taskID string, scope wf.FailScope, quarantineAfter int) error
	CreateTask(ctx context.Context, row wf.TaskRow) error
	FinishTask(ctx context.Context, req wf.FinishRequest) error
	SaveDecision(ctx context.Context, row wf.DecisionRow) error
	HasCapableDevice(ctx context.Context, sel wf.DeviceSelector) (bool, error)
	ListFleet(ctx context.Context) ([]store.FleetDevice, error)
	GetResult(ctx context.Context, taskID string) (*wf.ResultRecord, error)
	GetLeaseExpiry(ctx context.Context, taskID string) (*time.Time, error)
	SaveEvidenceSnapshot(ctx context.Context, snap store.EvidenceSnapshot) error
	GetEvidenceSnapshot(ctx context.Context, evidenceID string) (*store.EvidenceSnapshot, error)
	HasDecision(ctx context.Context, taskID, actor string) (bool, error)
	RecordWorkflowRun(ctx context.Context, run store.WorkflowRun) error
	SaveMetrics(ctx context.Context, points []store.MetricPoint) error
	Baseline(ctx context.Context, project, variant, suite, metricName string, n int) (*store.MetricBaseline, error)
	GetClientVersion(ctx context.Context, clientID string) (string, error)
	WriteAudit(ctx context.Context, entry store.AuditEntry) error
	ListExpiredTaskIDs(ctx context.Context, maxAgePassed, maxAgeFailed time.Duration) ([]store.ExpiredTask, error)
}

// Config is activity runtime parameters (§10 defaults + external endpoints).
type Config struct {
	LeaseSeconds      int    // task lease; default 120
	QuarantineAfter   int    // consecutive INFRA quarantine threshold; default 3
	CallbackBaseURL   string // base URL given to Client for callbacks (§8.1)
	ArtifactAuthType  string // bearer | job_token | basic(Deploy Token,原则 5)
	ArtifactAuthToken string
	// ArtifactAuthUsername 仅 basic 使用(Deploy Token 用户名);空 = 非 basic。
	ArtifactAuthUsername string
	FeishuWebhookURL     string // empty → 无 webhook 兜底(dev mode 可全空)
	// 飞书企业自建应用(三件套齐全时优先于 webhook;缺任一项回退 webhook)。
	FeishuAppID         string
	FeishuAppSecret     string
	FeishuReceiveID     string // 接收方:open_id(个人单聊)或 chat_id(群)
	FeishuReceiveIDType string // chat_id|open_id;空 → chat_id
	// FeishuCmdWhitelist 指令 listener 白名单(逗号分隔 open_id);
	// 空 = listener 不启动。
	FeishuCmdWhitelist string
	// DevOps → PM 升级通道(docs/superpowers/specs/2026-07-30 §8):
	// Endpoint 空 = 升级禁用(现状)。
	EscalationEndpoint      string
	EscalationToken         string
	EscalationMinConfidence float64 // 缺省 0.7
	// MinIO 预签名直传(§3.7);Endpoint 或凭据为空即禁用,优雅降级为空 presigned_uploads。
	MinIOEndpoint       string // 集群内 endpoint(如 minio:9000);兼作启用开关
	MinIOPublicEndpoint string // 预签名 URL 的 host,须 Client 可达(签名覆盖 Host)
	MinIOAccessKey      string
	MinIOSecretKey      string
	MinIOBucket         string        // 缺省 hermes-evidence
	MinIOPresignTTL     time.Duration // 缺省 1h
	// UploadMaxFiles 是按需签发端点单次请求的文件数上限(差距 #8);缺省 64。
	// 这里只做配置透传落地(callbacks.Handler 才是消费方),Acts 本身不用它。
	UploadMaxFiles int
	// Phase 2 Analyzer(§12):复用 q-uat hermes-agent 平台(§4)。
	// HermesEndpoint 为空即禁用 Analyzer(优雅降级,verdict 由规则引擎保底)。
	HermesEndpoint  string
	HermesAuthToken string
	HermesModel     string // 可选透传;模型主体由平台配置
	// HermesExpressModel 是表述层(Express)专用模型(设计文档 §4.3 评审定稿):
	// 表述在交互路径上对延迟最敏感,独立配置;空 = 回落 HermesModel。
	HermesExpressModel string
	// CmdAPIToken 是受控命令接口(cmdapi POST /api/v1/cmd)的 Bearer 共享密钥,
	// 供 MCP bridge(hermes-agent 平台侧)调用。空 = 接口未启用(401)。
	// 与飞书 open_id 白名单正交:这是机器对机器的结构化指令通道,无 LLM 参与。
	CmdAPIToken string
	// WorkflowBridgeURL/Token 是测试结果回填 hermes workflow_runtime 的桥
	// (2026-08-07 方案 B):workflow 结束后 SyncWorkflowRuns 活动调用它,
	// 让 workflow-assets 排行榜反映真实测试次数。空 = 跳过(开发模式)。
	WorkflowBridgeURL   string
	WorkflowBridgeToken string
	HermesTimeout   time.Duration
	// §12 Phase 2 / 设计文档 §3.1:飞书指令层自然语言翻译旁路总开关(缺省关,灰度)。
	// 启用需三者合取:FeishuCmdNL && HermesEndpoint 非空 && 指令 listener 本身已启用
	// (FeishuCmdWhitelist 非空)——装配逻辑见 cmd/worker/main.go。
	FeishuCmdNL bool
	// FeishuCmdNLTimeout 是 /translate 调用超时,不复用 HermesTimeout:bridge 实测
	// -t "" 冷/热约 76s/13s,这是人在飞书里等回复的交互路径,需单独调(缺省 60s)。
	FeishuCmdNLTimeout time.Duration
	// MinAgentVersion 是最低允许的 Agent 版本号(Phase 3 版本门禁)。
	// 空 = 不限(缺省);非空时 AcquireDevice 拒绝低于此版本的 Client。
	// 版本号用语义化版本比较(如 "0.1.0" < "0.2.0" < "1.0.0")。
	MinAgentVersion string
	// Phase 3 mTLS(§12):三件套齐全时启用双向证书认证。
	// 空 = mTLS 未配置,(全部 HTTP 流量与 Phase 2 无异)。
	MTLSCAFile   string
	MTLSCertFile string
	MTLSKeyFile  string
}

// Acts carries all activities; method names are the activity name strings referenced in workflow.
type Acts struct {
	Store   Store
	Cfg     Config
	HTTP    *http.Client // for Dispatch/CancelTask (Task 3)
	SpecCfg *SpecConfig
	Log     *zerolog.Logger     // optional; nil-safe (tests may leave unset)
	Hermes  hermesclient.Client // Phase 2 Analyzer;nil = 禁用,规则引擎保底(§12)
	// Feishu 通知发送方(feishu.NewSender 构造:app 优先,webhook 兜底);
	// nil = 未配置,Notify 静默成功(开发模式)。
	Feishu feishu.Sender
}

// writeAudit writes an audit_log row; failures are logged but never returned
// (audit is fire-and-forget, never blocks the main flow).
func (a *Acts) writeAudit(ctx context.Context, entry store.AuditEntry) {
	if a.Store == nil {
		return
	}
	if err := a.Store.WriteAudit(ctx, entry); err != nil {
		a.warnf("audit write failed: %v: actor=%s action=%s target=%s",
			err, entry.Actor, entry.Action, entry.Target)
	}
}

func (a *Acts) AcquireDevice(ctx context.Context, req wf.AcquireRequest) (*wf.Lease, error) {
	lease, err := a.Store.AcquireDevice(ctx, req.Selector, req.TaskID, a.Cfg.LeaseSeconds)
	if err != nil || lease == nil {
		return lease, err
	}
	// Phase 3 版本门禁:拒绝低于最低版本的 Client Agent。
	// 空 MinAgentVersion = 不限(缺省兼容旧部署)。
	if a.Cfg.MinAgentVersion != "" {
		cv, err := a.Store.GetClientVersion(ctx, lease.ClientID)
		if err != nil {
			a.warnf("acquire device: get client version %s failed: %v; releasing lease", lease.ClientID, err)
			_ = a.Store.ReleaseDevice(ctx, lease.DeviceID, req.TaskID, wf.FailScopeNone, a.Cfg.QuarantineAfter)
			return nil, fmt.Errorf("acquire device: client %s version unknown: %w", lease.ClientID, err)
		}
		if compareVersion(cv, a.Cfg.MinAgentVersion) < 0 {
			a.warnf("acquire device: client %s version %s below minimum %s; releasing lease",
				lease.ClientID, cv, a.Cfg.MinAgentVersion)
			_ = a.Store.ReleaseDevice(ctx, lease.DeviceID, req.TaskID, wf.FailScopeNone, a.Cfg.QuarantineAfter)
			return nil, fmt.Errorf("acquire device: client %s version %s below minimum %s",
				lease.ClientID, cv, a.Cfg.MinAgentVersion)
		}
	}
	// Phase 3 审计:设备租约获取成功 → 落 audit_log
	a.writeAudit(ctx, store.AuditEntry{
		Actor:  "activity:acquire_device",
		Action: "device_leased",
		Target: lease.DeviceID,
	})
	return lease, nil
}

func (a *Acts) CreateTask(ctx context.Context, row wf.TaskRow) error {
	return a.Store.CreateTask(ctx, row)
}

func (a *Acts) FinishTask(ctx context.Context, req wf.FinishRequest) error {
	return a.Store.FinishTask(ctx, req)
}

// ReleaseDevice 归还设备并按归因记账。空 FailScope 表示载荷来自改动前的
// workflow(重放场景,设计文档 §5):按旧语义翻译,保持当初的记账行为。
func (a *Acts) ReleaseDevice(ctx context.Context, req wf.ReleaseRequest) error {
	scope := req.FailScope
	if scope == "" {
		scope = wf.FailScopeOK
		if req.InfraFail {
			scope = wf.FailScopeDevice
		}
	}
	if err := a.Store.ReleaseDevice(ctx, req.DeviceID, req.TaskID, scope, a.Cfg.QuarantineAfter); err != nil {
		return err
	}
	// Phase 3 审计:设备释放 → 落 audit_log(成功与失败分支都释放,归因见 scope)
	a.writeAudit(ctx, store.AuditEntry{
		Actor:  "activity:release_device",
		Action: "device_released",
		Target: req.DeviceID,
	})
	return nil
}

// SaveDecision 落 decisions 表(§11):规则引擎与 LLM 的每次裁决都落表,可回放。
func (a *Acts) SaveDecision(ctx context.Context, row wf.DecisionRow) error {
	return a.Store.SaveDecision(ctx, row)
}

// LoadResult 按 task_id 回读 results 表的权威结果(原则 3 + 差距清单 #2:
// signal 只作唤醒提示,结果本体以数据库为准)。无记录返回 (nil, nil),
// 由 workflow 按 INFRA 处理(结果未随 signal 落库说明 outbox 链路异常)。
func (a *Acts) LoadResult(ctx context.Context, req wf.LoadResultRequest) (*wf.ResultRecord, error) {
	return a.Store.GetResult(ctx, req.TaskID)
}

// SaveMetrics 落 PASSED 任务的性能指标点(metrics 表,§9 基线数据源,
// schema.sql 既定写入路径)。由 workflow 在 PASSED 分支调用(ExtractEvidence
// 只在非 PASSED 路径运行,承担不了);workflow 侧 fire-and-forget 不重试,
// 无需幂等键。
func (a *Acts) SaveMetrics(ctx context.Context, req wf.SaveMetricsRequest) error {
	if len(req.Metrics) == 0 {
		return nil
	}
	points := make([]store.MetricPoint, 0, len(req.Metrics))
	for name, val := range req.Metrics {
		points = append(points, store.MetricPoint{
			Project: req.Project, Variant: req.Variant, Suite: "smoke",
			MetricName: name, Value: val, TaskID: req.TaskID,
		})
	}
	return a.Store.SaveMetrics(ctx, points)
}

// CheckLease 读库返回 task 当前租约的到期时刻(原则 6:由 workflow 的租约
// 到期 Durable Timer 触发,非轮询);租约不存在/已释放返回 (nil, nil),
// workflow 据此进入 INFRA 处理。
func (a *Acts) CheckLease(ctx context.Context, req wf.CheckLeaseRequest) (*time.Time, error) {
	return a.Store.GetLeaseExpiry(ctx, req.TaskID)
}

// ListExpiredTaskIDs 列出过期任务 ID
func (a *Acts) ListExpiredTaskIDs(ctx context.Context, maxAgePassed, maxAgeFailed time.Duration) ([]store.ExpiredTask, error) {
	return a.Store.ListExpiredTaskIDs(ctx, maxAgePassed, maxAgeFailed)
}
