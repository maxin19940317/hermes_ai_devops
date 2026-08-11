-- 2026-08-11: devices 表补充 disk_total_mb / disk_free_mb 列
-- (workdir 所在文件系统的总/可用空间,MB)。Agent 从 adb shell df -k 探测上报
-- (probe.go probeDisk),供飞书设备列表与 Hermes 文本展示。
-- 展示信息,非调度必要条件;旧 Agent/探测失败 → NULL。
ALTER TABLE devices ADD COLUMN IF NOT EXISTS disk_total_mb bigint;
ALTER TABLE devices ADD COLUMN IF NOT EXISTS disk_free_mb bigint;
