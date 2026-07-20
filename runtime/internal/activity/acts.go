package activity

import (
	"context"
	"net/http"

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
}

// Acts carries all activities; method names are the activity name strings referenced in workflow.
type Acts struct {
	Store   Store
	Cfg     Config
	HTTP    *http.Client // for Dispatch/CancelTask/Notify (Task 3)
	SpecCfg *SpecConfig
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
