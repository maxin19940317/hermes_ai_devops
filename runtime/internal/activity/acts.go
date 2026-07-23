package activity

import (
	"context"
	"net/http"
	"time"

	"github.com/rs/zerolog"

	"hermes-devops/runtime/internal/hermesclient"
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
}

// Config is activity runtime parameters (§10 defaults + external endpoints).
type Config struct {
	LeaseSeconds      int    // task lease; default 120
	QuarantineAfter   int    // consecutive INFRA quarantine threshold; default 3
	CallbackBaseURL   string // base URL given to Client for callbacks (§8.1)
	ArtifactAuthType  string // bearer | job_token
	ArtifactAuthToken string
	FeishuWebhookURL  string // empty → Notify logs only (dev mode)
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
	HTTP    *http.Client // for Dispatch/CancelTask/Notify (Task 3)
	SpecCfg *SpecConfig
	Log     *zerolog.Logger     // optional; nil-safe (tests may leave unset)
	Hermes  hermesclient.Client // Phase 2 Analyzer;nil = 禁用,规则引擎保底(§12)
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
