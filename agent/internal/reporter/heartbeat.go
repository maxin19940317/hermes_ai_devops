package reporter

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
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
// store 中的进行中任务(租约续期依据)。LEASE_NOT_OWNED 处置见 once。
type Heartbeat struct {
	Runner adb.Runner   // 设备发现与探测(可注入 fake)
	Store  *store.Store // active_task_ids 与 BUSY 判定来源
	Client *Client
	Logf   func(format string, args ...any) // nil → 静默

	// StopTask 在心跳应答带 LEASE_NOT_OWNED 时调用(租约易主/失效,
	// 立即停止本地执行;§10/差距 #15)。nil = 只记日志不停任务(仅测试)。
	StopTask func(taskID string)

	ClientID     string
	AgentVersion string
	BaseURL      string // 本 Agent 的 API 基地址,Runtime 派单用

	Interval           time.Duration       // 心跳周期;0 → DefaultHeartbeatInterval
	MaxWait            time.Duration       // 失败后退避上限;0 → DefaultHeartbeatMaxWait
	DeviceWorkdir      string              // df 探测路径;空 → DefaultDeviceWorkdir
	SOCAliases         map[string]string   // 平台代号 → SoC 型号(如 trinket→QCM6125)
	Capabilities       []string            // 旧版单设备能力声明
	DeviceCapabilities map[string][]string // serial/SoC → 能力

	// lastDevices 记录上次心跳的设备 serial 集合,用于 diff 日志(设备上下线感知)。
	// nil 表示首次心跳,输出当前可寻址设备作为基线。
	lastDevices map[string]bool
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
	return &Prober{Runner: h.Runner, Logf: h.Logf, DeviceWorkdir: h.deviceWorkdir(), SOCAliases: h.SOCAliases, Capabilities: h.Capabilities, DeviceCapabilities: h.DeviceCapabilities}
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
// 拖住循环。应答中的 not_owned(LEASE_NOT_OWNED)逐项停任务:
// 租约已易主/失效,继续操作设备会干扰新持有者(§10 防线)。
func (h *Heartbeat) once(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, h.interval())
	defer cancel()

	activeIDs, busySerials := h.inflight(ctx)
	devices := h.prober().ProbeDevices(ctx, busySerials)

	// 设备变化 diff 日志:与上次心跳对比,输出新增/移除的设备 serial。
	// 首次心跳(lastDevices == nil)输出当前可寻址设备并初始化基线。
	h.diffDevices(devices)

	req := HeartbeatRequest{
		ClientID:      h.ClientID,
		BaseURL:       h.BaseURL,
		AgentVersion:  h.AgentVersion,
		Ts:            utcNowMs(),
		Devices:       devices,
		ActiveTaskIDs: activeIDs,
	}
	ack, err := h.Client.Heartbeat(ctx, req)
	if err != nil {
		return err
	}
	for _, no := range ack.NotOwned {
		// 停止路径与 DELETE /api/v1/tasks 取消一致(executor.Cancel →
		// CANCELED 终态 → 自动离开 inflight 集合,下轮心跳不再上报)
		h.logf("heartbeat: %s not owned (%s), stopping local execution", no.TaskID, no.Code)
		if h.StopTask != nil {
			h.StopTask(no.TaskID)
		}
	}
	return nil
}

// inflight 从 store 取进行中任务:返回逐任务混合格式的任务清单与占用中的
// 设备 serial 集合(由 dispatch_json 的 device_serial 解析;解析失败只丢
// BUSY 判定,任务仍上报)。
//
// 混合格式的滚动升级兼容推理(差距 #15):
//   - 旧 Runtime(只认字符串)派发的任务不带凭据 → 全部字符串 → 与旧格式
//     逐字节等价,Runtime 行为不变(仅续 client 心跳);
//   - 新 Runtime 派发的任务带 lease 凭据 → 对象格式 → 条件续租生效;
//   - 升级窗口内两种任务并存时各按各的格式上报,契约 oneOf 两种都合法。
//
// 因此 agent 与 runtime 的任何部署顺序都安全。
func (h *Heartbeat) inflight(ctx context.Context) ([]any, map[string]bool) {
	ids := []any{}
	busy := map[string]bool{}
	inf, err := h.Store.LoadInflight(ctx)
	if err != nil {
		h.logf("heartbeat: load inflight: %v", err)
		return ids, busy
	}
	for _, t := range inf.Tasks {
		if t.LeaseID != "" {
			ids = append(ids, ActiveTask{
				TaskID:          t.TaskID,
				Attempt:         attemptFromTaskID(t.TaskID, t.Attempt),
				LeaseID:         t.LeaseID,
				LeaseGeneration: t.LeaseGeneration,
			})
		} else {
			ids = append(ids, t.TaskID) // 无凭据:旧字符串格式
		}
		var d struct {
			DeviceSerial string `json:"device_serial"`
		}
		if err := json.Unmarshal([]byte(t.DispatchJSON), &d); err == nil && d.DeviceSerial != "" {
			busy[d.DeviceSerial] = true
		}
	}
	return ids, busy
}

// attemptFromTaskID 从 task_id 的 :a{N} 后缀解析 attempt
// (task_id = {workflow_id}:{test_id}:a{attempt},差距 #14);
// 解析失败回退 dispatch 载荷里的 attempt 字段。
func attemptFromTaskID(taskID string, fallback int) int {
	i := strings.LastIndex(taskID, ":a")
	if i < 0 {
		return fallback
	}
	n, err := strconv.Atoi(taskID[i+2:])
	if err != nil || n < 1 {
		return fallback
	}
	return n
}

// diffDevices 对比本次探测到的设备与上次心跳的基线，输出新增/移除日志。
// 首次调用（lastDevices == nil）输出当前设备列表作为基线确认。
func (h *Heartbeat) diffDevices(devices []DeviceInfo) {
	now := make(map[string]bool, len(devices))
	for _, d := range devices {
		now[d.Serial] = true
	}

	if h.lastDevices == nil {
		// 首次心跳：输出当前设备列表，让用户确认设备已发现
		if len(devices) == 0 {
			h.logf("device probe: no devices found")
		} else {
			serials := make([]string, 0, len(devices))
			for _, d := range devices {
				serials = append(serials, d.Serial)
			}
			h.logf("device probe: %d device(s) online: %s", len(devices), strings.Join(serials, ", "))
		}
		h.lastDevices = now
		return
	}

	// 检查新增设备
	for serial := range now {
		if !h.lastDevices[serial] {
			h.logf("device added: %s", serial)
		}
	}

	// 检查移除设备
	for serial := range h.lastDevices {
		if !now[serial] {
			h.logf("device removed: %s", serial)
		}
	}

	h.lastDevices = now
}
