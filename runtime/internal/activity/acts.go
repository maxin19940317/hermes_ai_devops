package activity

import (
	"context"
	"net/http"
	"time"

	"github.com/rs/zerolog"

	wf "hermes-devops/runtime/internal/workflow"
)

// Store is the subset of persistence layer dependencies for activities;
// both store.MemStore and (later) PGStore satisfy this interface.
type Store interface {
	AcquireDevice(ctx context.Context, sel wf.DeviceSelector, taskID string, leaseSeconds int) (*wf.Lease, error)
	ReleaseDevice(ctx context.Context, deviceID, taskID string, infraFail bool, quarantineAfter int) error
	CreateTask(ctx context.Context, row wf.TaskRow) error
	FinishTask(ctx context.Context, req wf.FinishRequest) error
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
}

// Acts carries all activities; method names are the activity name strings referenced in workflow.
type Acts struct {
	Store   Store
	Cfg     Config
	HTTP    *http.Client // for Dispatch/CancelTask/Notify (Task 3)
	SpecCfg *SpecConfig
	Log     *zerolog.Logger // optional; nil-safe (tests may leave unset)
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
