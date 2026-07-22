package server

import (
	"context"
	"net/http"
	"time"

	"hermes-devops/agent/internal/reporter"
)

// devicesProbeTimeout 是设备清单探测的整体上限(与心跳单次探测同尺度)。
const devicesProbeTimeout = 15 * time.Second

// listDevices 实现 GET /api/v1/devices:adb 发现 + 属性/空间探测
// (与心跳共用 reporter.Prober);store 中非终态任务占用的设备记 BUSY。
func (s *Server) listDevices(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), devicesProbeTimeout)
	defer cancel()

	busy := reporter.BusySerials(ctx, s.cfg.Store, s.cfg.Logf)
	prober := &reporter.Prober{
		Runner:        s.cfg.Runner,
		Logf:          s.cfg.Logf,
		DeviceWorkdir: s.cfg.DeviceWorkdir,
		SOCAliases:    s.cfg.SOCAliases,
		Capabilities:  s.cfg.Capabilities,
	}
	devices := prober.ProbeDevices(ctx, busy)
	writeJSON(w, http.StatusOK, devices)
}
