-- 失败归因分离(差距 #10)。clients 加 client 级计数;
-- devices.fail_streak 保留原名,语义收窄为"设备级"。
-- 幂等:IF NOT EXISTS + 幂等 UPDATE,重复执行无副作用。

BEGIN;

ALTER TABLE clients ADD COLUMN IF NOT EXISTS fail_streak INTEGER NOT NULL DEFAULT 0;

-- 历史值按旧(错误)语义累计:client 侧失败、超时、Runtime 自身故障都记在设备头上。
-- 语义既然收窄为"设备级",旧值就不该带进新语义——归零重新开始计。
UPDATE devices SET fail_streak = 0;

COMMIT;
