-- =============================================================================
-- schema version: proxy-probe-history v1
-- 代理拨测不可变历史：保留每次拨测结果，供成功率、延迟趋势与异常规则使用。
-- =============================================================================

-- 代理主键本身全局唯一；复合唯一键让历史表同时用 tenant_id 约束归属，
-- 避免错误租户把自己的历史挂到别的租户代理上。
CREATE UNIQUE INDEX IF NOT EXISTS uq_proxies_tenant_id
    ON proxies (tenant_id, id);

CREATE TABLE IF NOT EXISTS proxy_probe_history (
    id            text PRIMARY KEY,
    tenant_id     text NOT NULL,
    proxy_id      text NOT NULL,
    check_status  text NOT NULL CHECK (check_status IN ('ok', 'fail')),
    status_code   integer NOT NULL DEFAULT 0 CHECK (status_code BETWEEN 0 AND 599),
    latency_ms    integer NOT NULL CHECK (latency_ms >= 0),
    error_message text,
    checked_at    timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT fk_proxy_probe_history_proxy
        FOREIGN KEY (tenant_id, proxy_id)
        REFERENCES proxies (tenant_id, id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_proxy_probe_history_tenant_proxy_time
    ON proxy_probe_history (tenant_id, proxy_id, checked_at DESC, id DESC);

ALTER TABLE proxy_probe_history ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON proxy_probe_history
    USING (tenant_id = current_tenant())
    WITH CHECK (tenant_id = current_tenant());
