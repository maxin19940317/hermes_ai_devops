package reporter

import (
	"context"
	"encoding/json"
	"time"

	"hermes-devops/agent/internal/adb"
	"hermes-devops/agent/internal/store"
)

// Heartbeat 默认值(设计文档 §3.3 / §10:周期 10s,退避上限 60s)。
const (
	DefaultHeartbeatInterval = 10 * time.Second
	DefaultHeartbeatMaxWait  = 60 * time.Second
	// DefaultDeviceWorkdir 是 workdir_free_mb 的探测路径(设备端临时工作根)。
	DefaultDeviceWorkdir = "/data/local/tmp"
)

// Heartbeat 周期上报心跳:adb 设备清单 + getprop/df 组装的设备状态 +
// store 中的进行中任务(租约续期依据)。只做上报,不触碰任务执行。
type Heartbeat struct {
	Runner adb.Runner   // 设备发现与探测(可注入 fake)
	Store  *store.Store // active_task_ids 与 BUSY 判定来源
	Client *Client
	Logf   func(format string, args ...any) // nil → 静默

	ClientID     string
	AgentVersion string
	BaseURL      string // 本 Agent 的 API 基地址,Runtime 派单用

	Interval      time.Duration     // 心跳周期;0 → DefaultHeartbeatInterval
	MaxWait       time.Duration     // 失败后退避上限;0 → DefaultHeartbeatMaxWait
	DeviceWorkdir string            // df 探测路径;空 → DefaultDeviceWorkdir
	SOCAliases    map[string]string // 平台代号 → SoC 型号(如 trinket→QCM6125)
}

func (h *Heartbeat) interval() time.Duration {
	if h.Interval > 0 {
		return h.Interval
	}
	return DefaultHeartbeatInterval
}

func (h *Heartbeat) maxWait() time.Duration {
	if h.MaxWait > 0 {
		return h.MaxWait
	}
	return DefaultHeartbeatMaxWait
}

func (h *Heartbeat) deviceWorkdir() string {
	if h.DeviceWorkdir != "" {
		return h.DeviceWorkdir
	}
	return DefaultDeviceWorkdir
}

func (h *Heartbeat) logf(format string, args ...any) {
	if h.Logf != nil {
		h.Logf(format, args...)
	}
}

// prober 组装共享设备探测器(与 server 的 /api/v1/devices 同一逻辑)。
func (h *Heartbeat) prober() *Prober {
	return &Prober{Runner: h.Runner, Logf: h.Logf, DeviceWorkdir: h.deviceWorkdir(), SOCAliases: h.SOCAliases}
}

// Run 启动心跳循环,阻塞至 ctx 取消(返回 nil,属正常停止)。
// 立即发第一次,之后按 Interval 周期发送;连续失败按
// Interval×2ⁿ 指数退避(上限 MaxWait),成功后复位。永不因失败退出。
func (h *Heartbeat) Run(ctx context.Context) error {
	fails := 0
	for {
		if err := h.once(ctx); err != nil {
			fails++
			h.logf("heartbeat: %v (consecutive failures: %d)", err, fails)
		} else {
			fails = 0
		}
		wait := h.interval()
		if fails > 0 {
			// 指数退避:Interval << (fails-1),封顶 MaxWait
			for i := 1; i < fails && wait < h.maxWait(); i++ {
				wait *= 2
			}
			if wait > h.maxWait() {
				wait = h.maxWait()
			}
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}

// once 组装并发送一次心跳。单次探测整体限时一个周期,避免设备挂死
// 拖住循环。
func (h *Heartbeat) once(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, h.interval())
	defer cancel()

	activeIDs, busySerials := h.inflight(ctx)
	devices := h.prober().ProbeDevices(ctx, busySerials)
	req := HeartbeatRequest{
		ClientID:      h.ClientID,
		BaseURL:       h.BaseURL,
		AgentVersion:  h.AgentVersion,
		Ts:            utcNowMs(),
		Devices:       devices,
		ActiveTaskIDs: activeIDs,
	}
	if _, err := h.Client.Heartbeat(ctx, req); err != nil {
		return err
	}
	return nil
}

// inflight 从 store 取进行中任务:返回任务 ID 列表与占用中的设备 serial
// 集合(由 dispatch_json 的 device_serial 解析;解析失败只丢 BUSY 判定,
// 任务 ID 仍上报)。
func (h *Heartbeat) inflight(ctx context.Context) ([]string, map[string]bool) {
	ids := []string{}
	busy := map[string]bool{}
	inf, err := h.Store.LoadInflight(ctx)
	if err != nil {
		h.logf("heartbeat: load inflight: %v", err)
		return ids, busy
	}
	for _, t := range inf.Tasks {
		ids = append(ids, t.TaskID)
		var d struct {
			DeviceSerial string `json:"device_serial"`
		}
		if err := json.Unmarshal([]byte(t.DispatchJSON), &d); err == nil && d.DeviceSerial != "" {
			busy[d.DeviceSerial] = true
		}
	}
	return ids, busy
}
