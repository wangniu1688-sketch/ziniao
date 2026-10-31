-- 回滚 Phase 2 审批工单
DROP POLICY IF EXISTS admin_list ON tenants;
DROP POLICY IF EXISTS tenant_isolation ON approval_tickets;
DROP INDEX IF EXISTS idx_approval_expires;
DROP INDEX IF EXISTS idx_approval_tenant_status;
DROP TABLE IF EXISTS approval_tickets;
