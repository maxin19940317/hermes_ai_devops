package activity

import (
	"context"
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
	ReleaseDevice(ctx context.Context, deviceID, taskID string, infraFail bool, quarantineAfter int) error
	CreateTask(ctx context.Context, row wf.TaskRow) error
	FinishTask(ctx context.Context, req wf.FinishRequest) error
	SaveDecision(ctx context.Context, row wf.DecisionRow) error
	HasCapableDevice(ctx context.Context, sel wf.DeviceSelector) (bool, error)
	GetResult(ctx context.Context, taskID string) (*wf.ResultRecord, error)
	GetLeaseExpiry(ctx context.Context, taskID string) (*time.Time, error)
	SaveEvidenceSnapshot(ctx context.Context, snap store.EvidenceSnapshot) error
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
	// MinIO 预签名直传(§3.7);Endpoint 或凭据为空即禁用,优雅降级为空 presigned_uploads。
	MinIOEndpoint       string        // 集群内 endpoint(如 minio:9000);兼作启用开关
	MinIOPublicEndpoint string        // 预签名 URL 的 host,须 Client 可达(签名覆盖 Host)
	MinIOAccessKey      string
	MinIOSecretKey      string
	MinIOBucket         string        // 缺省 hermes-evidence
	MinIOPresignTTL     time.Duration // 缺省 1h
	// Phase 2 Analyzer(§12):复用 q-uat hermes-agent 平台(§4)。
	// HermesEndpoint 为空即禁用 Analyzer(优雅降级,verdict 由规则引擎保底)。
	HermesEndpoint  string
	HermesAuthToken string
	HermesModel     string // 可选透传;模型主体由平台配置
	HermesTimeout   time.Duration
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

func (a *Acts) AcquireDevice(ctx context.Context, req wf.AcquireRequest) (*wf.Lease, error) {
	return a.Store.AcquireDevice(ctx, req.Selector, req.TaskID, a.Cfg.LeaseSeconds)
}

func (a *Acts) CreateTask(ctx context.Context, row wf.TaskRow) error {
	return a.Store.CreateTask(ctx, row)
}

func (a *Acts) FinishTask(ctx context.Context, req wf.FinishRequest) error {
	return a.Store.FinishTask(ctx, req)
}

func (a *Acts) ReleaseDevice(ctx context.Context, req wf.ReleaseRequest) error {
	return a.Store.ReleaseDevice(ctx, req.DeviceID, req.TaskID, req.InfraFail, a.Cfg.QuarantineAfter)
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

// CheckLease 读库返回 task 当前租约的到期时刻(原则 6:由 workflow 的租约
// 到期 Durable Timer 触发,非轮询);租约不存在/已释放返回 (nil, nil),
// workflow 据此进入 INFRA 处理。
func (a *Acts) CheckLease(ctx context.Context, req wf.CheckLeaseRequest) (*time.Time, error) {
	return a.Store.GetLeaseExpiry(ctx, req.TaskID)
}
