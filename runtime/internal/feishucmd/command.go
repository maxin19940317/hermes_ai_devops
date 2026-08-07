// Package feishucmd 实现飞书指令 listener:企业自建应用长连接(WebSocket)
// 接收单聊消息 → 白名单鉴权 → 封闭枚举指令解析 → 执行并文本回复。
// 指令面只含只读查询与既有定义动作(status/devices/rerun/unquarantine),
// 不接自由文本,不放大任何能力(安全红线:只响应 FEISHU_CMD_WHITELIST
// 的 open_id,非白名单静默忽略)。
package feishucmd

import (
	"fmt"
	"strings"
)

// Command 是一条解析后的指令(封闭枚举;Name 未知时恒为 help)。
type Command struct {
	Name string   // status | devices | rerun | unquarantine | test | help
	Args []string // rerun: <source_workflow_id> [variant];test: <variant> [commit]
}

// usage 是帮助文本(空/未知指令的应答)。
const usage = `可用指令:
  status                        运行中 workflow / 设备状态 / 活跃租约
  devices                       设备列表(serial/soc/status/fail_streak)
  test <variant> [commit]       测试指定变体(最近构建或指定 commit)
  rerun <source_workflow_id> [variant]  重跑权威终态运行的失败变体(可指定一个变体)
  unquarantine [device_id]      解除设备隔离(多台时需指定 id)
  plan <需求描述>                自然语言规划:生成可执行的测试计划 Plan DSL`

// Parse 解析一条消息文本:trim 后按空白切分,命令名大小写不敏感;
// 空文本/未知命令 → help。plan 取第一条空白后的全部文本作为参数。
func Parse(text string) Command {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return Command{Name: "help"}
	}
	parts := strings.SplitN(trimmed, " ", 2)
	name := strings.ToLower(parts[0])
	switch name {
	case "status", "devices", "rerun", "unquarantine", "test":
		args := []string{}
		if len(parts) > 1 {
			args = strings.Fields(parts[1])
		}
		return Command{Name: name, Args: args}
	case "plan":
		raw := ""
		if len(parts) > 1 {
			raw = strings.TrimSpace(parts[1])
		}
		return Command{Name: "plan", Args: []string{raw}}
	default:
		return Command{Name: "help"}
	}
}

// ParseWhitelist 解析逗号分隔的 open_id 白名单(FEISHU_CMD_WHITELIST);
// 空串 → 空集合(listener 不启动,见 cmd/worker)。
func ParseWhitelist(raw string) map[string]bool {
	out := map[string]bool{}
	for _, id := range strings.Split(raw, ",") {
		if id = strings.TrimSpace(id); id != "" {
			out[id] = true
		}
	}
	return out
}

// validateSHA:rerun 的 commit 形态(7-40 位小写 hex,与 bundle 契约一致)。
func validateSHA(s string) error {
	if len(s) < 7 || len(s) > 40 {
		return fmt.Errorf("sha 长度 %d 非法(7-40 位 hex)", len(s))
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return fmt.Errorf("sha 含非法字符 %q", r)
		}
	}
	return nil
}
