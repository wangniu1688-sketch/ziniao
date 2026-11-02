-- =============================================================================
-- 回滚 0012_launcher_core：按 up 的逆序撤销（删列 → 删表 → 删列）。
-- up 只做加法（新增表 + 新增列），因此 down 之后 0001–0011 的语义完全恢复。
-- uq_shops_tenant_code / idx_shops_tenant_group 随列自动消失；
-- uq_proxies_tenant_name / idx_proxies_tenant_status 随表消失。
-- =============================================================================

ALTER TABLE environments DROP COLUMN IF EXISTS thumbnail_path;
ALTER TABLE environments DROP COLUMN IF EXISTS last_started_at;
ALTER TABLE environments DROP COLUMN IF EXISTS start_url;
ALTER TABLE environments DROP COLUMN IF EXISTS country;
ALTER TABLE environments DROP COLUMN IF EXISTS proxy_id;

DROP TABLE IF EXISTS proxies;
DROP TABLE IF EXISTS groups;

ALTER TABLE shops DROP COLUMN IF EXISTS group_id;
ALTER TABLE shops DROP COLUMN IF EXISTS code;
