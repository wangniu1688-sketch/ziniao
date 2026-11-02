-- =============================================================================
-- 回滚 0013_proxy_upstream：只删 0013 新增的列与约束，不碰 0012 的既有结构。
--
-- 为什么敢 DROP COLUMN：这八列全是 0013 引入的派生/台账附加信息，
-- 删掉只丢「最近一次拨测结果」，不丢代理地址与凭据（password_enc 属 0012）。
-- =============================================================================

ALTER TABLE proxies DROP CONSTRAINT IF EXISTS proxies_check_pair_chk;
ALTER TABLE proxies DROP CONSTRAINT IF EXISTS proxies_check_status_chk;
ALTER TABLE proxies DROP CONSTRAINT IF EXISTS proxies_type_chk;

DROP INDEX IF EXISTS idx_proxies_tenant_check;

ALTER TABLE proxies DROP COLUMN IF EXISTS last_error;
ALTER TABLE proxies DROP COLUMN IF EXISTS last_checked_at;
ALTER TABLE proxies DROP COLUMN IF EXISTS latency_ms;
ALTER TABLE proxies DROP COLUMN IF EXISTS exit_country;
ALTER TABLE proxies DROP COLUMN IF EXISTS exit_ip;
ALTER TABLE proxies DROP COLUMN IF EXISTS check_status;
ALTER TABLE proxies DROP COLUMN IF EXISTS city;
ALTER TABLE proxies DROP COLUMN IF EXISTS type;
