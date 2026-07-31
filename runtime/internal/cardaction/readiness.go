package cardaction

import "sync/atomic"

// ReadinessConfig contains the four deployment-time card action readiness
// factors. WebSocket liveness is maintained separately through SetWS.
type ReadinessConfig struct {
	Enabled           bool
	WhitelistNonEmpty bool
	SenderIsApp       bool
	HandlerWired      bool
}

// Readiness gates both button rendering and callback acceptance.
type Readiness struct {
	enabled           bool
	whitelistNonEmpty bool
	senderIsApp       bool
	handlerWired      bool
	wsUp              atomic.Bool
}

// NewReadiness creates a readiness gate whose WebSocket starts down.
func NewReadiness(cfg ReadinessConfig) *Readiness {
	return &Readiness{
		enabled:           cfg.Enabled,
		whitelistNonEmpty: cfg.WhitelistNonEmpty,
		senderIsApp:       cfg.SenderIsApp,
		handlerWired:      cfg.HandlerWired,
	}
}

// Ready reports the conjunction of all five readiness factors.
func (r *Readiness) Ready() bool {
	return r != nil &&
		r.enabled &&
		r.whitelistNonEmpty &&
		r.senderIsApp &&
		r.handlerWired &&
		r.wsUp.Load()
}

// SetWS updates the current WebSocket connection state.
func (r *Readiness) SetWS(up bool) {
	if r != nil {
		r.wsUp.Store(up)
	}
}
