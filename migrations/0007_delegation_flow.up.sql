-- Phase 2 补齐：代理机构委托审批（docs/TODO-phase2.md 第 5 条「仍缺」项）
--
-- 与 approval_tickets 的区别：审批工单是「同一租户内的高危动作二次审批」；
-- 委托审批是**跨租户**的两方流程——受托方（代理机构，同为系统内租户）申请
-- 代管委托方（店铺主团队）的指定店铺，由委托方 owner/ops_manager 裁决。
-- 表名沿用规划中的 approval_flows（见 approval/router.go 的历史注释）。
--
-- 行归属 tenant_id = 委托方；但受托方必须能看到自己提交的申请状态
--（否则申请提交后石沉大海），所以 RLS 是双主体策略（OR 受托方租户）。
-- 写路径的显式谓词仍是防线（tenant_id / agency_tenant_id 双列带全），RLS 只是第二道。
-- 可回滚：见 0007_delegation_flow.down.sql

CREATE TABLE IF NOT EXISTS approval_flows (
    id               text PRIMARY KEY,
    tenant_id        text NOT NULL REFERENCES tenants(id) ON DELETE CASCADE, -- 委托方（店铺主）
    agency_tenant_id text NOT NULL REFERENCES tenants(id) ON DELETE CASCADE, -- 受托方（代理机构）
    agency_name      text NOT NULL DEFAULT '',   -- 受托方名称快照（展示用，改名不回溯）
    grantee_id       text NOT NULL,              -- 受托方申请人（批准后获得访问的账号）
    grantee_name     text NOT NULL DEFAULT '',
    scope            jsonb NOT NULL DEFAULT '{}', -- {"shop_ids":[...]}，审批前由服务端校验归属
    reason           text,                        -- 受托方申请理由
    comment          text,                        -- 委托方审批意见
    status           text NOT NULL DEFAULT 'pending',
                     -- pending|active|rejected|cancelled|revoked|expired
                     -- active = 已批准且授权生效中；expired 同时覆盖「申请超期未决」与「委托到期」
    detail           jsonb NOT NULL DEFAULT '{}', -- 批准时写入 grants/role_id 便于回收溯源
    approver_id      text,                        -- 委托方审批人（裁决时写入）
    approver_name    text,
    expires_at       timestamptz NOT NULL,        -- 申请单有效期（复用审批单 TTL）
    valid_until      timestamptz,                 -- 委托有效期（批准时写入；授权到期即回收）
    created_at       timestamptz NOT NULL DEFAULT now(),
    decided_at       timestamptz,
    revoked_at       timestamptz
);

CREATE INDEX IF NOT EXISTS idx_approval_flows_tenant_status ON approval_flows(tenant_id, status);
CREATE INDEX IF NOT EXISTS idx_approval_flows_agency_status ON approval_flows(agency_tenant_id, status);
-- 委托到期回收扫描：只扫 active 且已过期的行
CREATE INDEX IF NOT EXISTS idx_approval_flows_valid_until ON approval_flows(valid_until) WHERE status = 'active';

ALTER TABLE approval_flows ENABLE ROW LEVEL SECURITY;
ALTER TABLE approval_flows FORCE ROW LEVEL SECURITY;
-- 双主体：委托方看收到的申请，受托方看自己发出的申请。
-- 除这两列外任何租户上下文都看不到行（fail-closed）。
CREATE POLICY tenant_isolation ON approval_flows
    USING (tenant_id = current_tenant() OR agency_tenant_id = current_tenant())
    WITH CHECK (tenant_id = current_tenant() OR agency_tenant_id = current_tenant());
