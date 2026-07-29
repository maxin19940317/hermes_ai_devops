-- outbox 积压视图(第四批:backlog/失败监控)。
-- Relay 定期把同样的数字打进日志;这个视图是人工排查入口:
--   SELECT * FROM outbox_backlog;
-- stuck 的判定阈值在 Relay 侧可配(RELAY_STUCK_ATTEMPTS),视图固定用 3——
-- 视图是给人看的粗筛,精确阈值以 Relay 日志为准。
--
-- 幂等:CREATE OR REPLACE VIEW,重复执行无副作用。只读视图,不改任何表结构。

BEGIN;

CREATE OR REPLACE VIEW outbox_backlog AS
SELECT count(*)                                            AS pending,
       count(*) FILTER (WHERE attempts >= 3)               AS stuck,
       coalesce(EXTRACT(EPOCH FROM (now() - min(created_at))), 0)::bigint
                                                           AS oldest_age_sec,
       max(attempts)                                       AS max_attempts
FROM outbox
WHERE published_at IS NULL;

COMMIT;
