-- 设备 OS 维度(Phase 4 轮次 A):devices 表加 os 列,支持 OS 级调度约束。
-- 纯加列,非停写窗口型迁移;旧代码不读该列,不影响运行中服务。
-- 存量设备默认 'android'(该列上线前 fleet 中仅有 Android 设备)。
-- 幂等,可重复执行。
-- 执行:docker exec -i hermes-runtime-postgres-1 psql -U hermes_runtime -d hermes_runtime -v ON_ERROR_STOP=1 < 本文件

BEGIN;

ALTER TABLE devices ADD COLUMN IF NOT EXISTS os TEXT NOT NULL DEFAULT 'android';

COMMIT;
