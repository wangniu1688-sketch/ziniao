-- Phase 2：审批工单（高危动作二次审批，反例 8 / L3 职责分离）
-- 可回滚：见 0003_phase2_approval.down.sql

CREATE TABLE IF NOT EXISTS approval_tickets (
    id             text PRIMARY KEY,
    tenant_id      text NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    type           text NOT NULL,          -- audit.export.plain|shop.credential.reveal|...
    subject_type   text,                   -- shop|device|tenant|audit
    subject_id     text,
    requester_id   text NOT NULL,          -- 申请人（必须与审批人不同）
    requester_name text,
    approver_id    text,                   -- 审批人（审批时写入）
    approver_name  text,
    reason         text,
    status         text NOT NULL DEFAULT 'pending', -- pending|approved|rejected|expired|cancelled
    detail         jsonb NOT NULL DEFAULT '{}',
    expires_at     timestamptz NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now(),
    decided_at     timestamptz
);

CREATE INDEX IF NOT EXISTS idx_approval_tenant_status ON approval_tickets(tenant_id, status);
CREATE INDEX IF NOT EXISTS idx_approval_expires ON approval_tickets(expires_at) WHERE status = 'pending';

-- 定时任务需要在无租户上下文时遍历团队（运营后台同理）
DROP POLICY IF EXISTS admin_list ON tenants;
CREATE POLICY admin_list ON tenants FOR SELECT
    USING (current_tenant() IS NULL);

ALTER TABLE approval_tickets ENABLE ROW LEVEL SECURITY;
ALTER TABLE approval_tickets FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON approval_tickets
    USING (tenant_id = current_tenant()) WITH CHECK (tenant_id = current_tenant());
