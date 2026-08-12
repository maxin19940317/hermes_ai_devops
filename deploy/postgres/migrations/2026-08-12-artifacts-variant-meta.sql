-- 2026-08-12:artifacts 表补充 variant_requirements / variant_signatures
-- (jsonb,业务仓库 variants.yaml 声明的设备调度约束与失败签名)。
-- 解耦(长期方案):Runtime 不再维护自己的变体配置副本,触发端(webhook/kick)
-- 登记 artifact 时写入,workflow 据此做设备匹配与证据提取。
-- 旧行(改动前登记)→ NULL,触发端兼容:缺字段时按既有行为降级(见代码注释)。
ALTER TABLE artifacts ADD COLUMN IF NOT EXISTS variant_requirements jsonb;
ALTER TABLE artifacts ADD COLUMN IF NOT EXISTS variant_signatures jsonb;
