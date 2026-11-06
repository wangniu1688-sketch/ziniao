-- 回滚 0014：只删除拨测历史，不影响代理台账与最新一次拨测摘要。
DROP TABLE IF EXISTS proxy_probe_history;
DROP INDEX IF EXISTS uq_proxies_tenant_id;
