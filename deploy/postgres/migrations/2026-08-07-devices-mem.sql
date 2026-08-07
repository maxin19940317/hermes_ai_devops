-- 2026-08-07: devices 表补充 mem_total_mb 列(设备物理内存总量,MB)。
-- Agent 从 /proc/meminfo 探测上报(probe.go probeMemTotal),供飞书设备列表展示。
-- 展示信息,非调度必要条件;旧 Agent/探测失败 → NULL。
ALTER TABLE devices ADD COLUMN IF NOT EXISTS mem_total_mb bigint;
