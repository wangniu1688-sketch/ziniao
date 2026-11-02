DROP POLICY IF EXISTS tenant_isolation ON approval_flows;
DROP INDEX IF EXISTS idx_approval_flows_valid_until;
DROP INDEX IF EXISTS idx_approval_flows_agency_status;
DROP INDEX IF EXISTS idx_approval_flows_tenant_status;
DROP TABLE IF EXISTS approval_flows;
