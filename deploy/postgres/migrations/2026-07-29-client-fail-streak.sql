-- 失败归因分离(差距 #10)。clients 加 client 级计数;
-- devices.fail_streak 保留原名,语义收窄为"设备级"。
-- 幂等:IF NOT EXISTS + 幂等 UPDATE,重复执行无副作用。

BEGIN;

ALTER TABLE clients ADD COLUMN IF NOT EXISTS fail_streak INTEGER NOT NULL DEFAULT 0;

-- 历史值按旧(错误)语义累计:client 侧失败、超时、Runtime 自身故障都记在设备头上。
-- 语义既然收窄为"设备级",旧值就不该带进新语义——归零重新开始计。
UPDATE devices SET fail_streak = 0;

-- 旧隔离全部是误伤(该项目三次实际隔离:两次 SNPE 测试挂起属工作负载问题,
-- 一次 Runtime 配置 bug),且新语义下 device 无信号源、隔离不再触发:
-- 遗留的 QUARANTINED 会永久悬挂(HasCapableDevice 不看 status,SelectTestSpecs
-- 不会跳过该变体,每个 workflow 都要空等到 no device available),一并归位。
UPDATE devices SET status = 'IDLE' WHERE status = 'QUARANTINED';

COMMIT;
