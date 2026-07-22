// Package reporter 实现 Client → Runtime 的三类回调(设计文档 §3.3,
// 契约 contracts/callbacks-api.openapi.yaml):
//
//   - Heartbeat:每 10s 上报心跳 + 设备状态 + 进行中任务(租约续期),
//     失败指数退避,不阻塞任务执行;
//   - EventReporter:挂 executor 状态迁移钩子,经 store 单事务落盘取 seq 后
//     即发 task-events,未确认事件由后台循环按 (task_id, seq) 有序补报;
//   - ResultReporter:终态后组装 result.json v1(过 contracts/result.schema.json
//     嵌入副本校验)上报 results;500 重发,400 不重发。
//
// 所有回调幂等:Runtime 按 idempotency_key + seq(事件)/ task_id(结果)
// 去重,重发安全(§4)。
package reporter

import "time"

// tsLayout 是回调时间戳格式:UTC ISO-8601 毫秒精度(契约要求;
// 与 store 落盘格式一致)。
const tsLayout = "2006-01-02T15:04:05.000Z"

// utcNowMs 返回当前时刻的 UTC 毫秒精度时间戳。
func utcNowMs() string {
	return time.Now().UTC().Format(tsLayout)
}

// formatTS 将 t 格式化为 UTC 毫秒精度时间戳。
func formatTS(t time.Time) string {
	return t.UTC().Format(tsLayout)
}
