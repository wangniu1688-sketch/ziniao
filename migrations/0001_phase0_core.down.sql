-- 回滚脚本：phase0-core v1（必须可回滚）
DROP POLICY IF EXISTS tenant_isolation ON shops;
DROP POLICY IF EXISTS tenant_isolation ON notifications;
DROP POLICY IF EXISTS tenant_isolation ON audit_log;
DROP POLICY IF EXISTS tenant_isolation ON event_outbox;
DROP POLICY IF EXISTS tenant_isolation ON usage_counters;
DROP POLICY IF EXISTS tenant_isolation ON subscriptions;
DROP POLICY IF EXISTS tenant_isolation ON sessions;
DROP POLICY IF EXISTS tenant_isolation ON data_grants;
DROP POLICY IF EXISTS tenant_isolation ON user_roles;
DROP POLICY IF EXISTS tenant_isolation ON roles;
DROP POLICY IF EXISTS tenant_isolation ON memberships;
DROP POLICY IF EXISTS login_lookup ON memberships;

DROP FUNCTION IF EXISTS current_tenant();

DROP TABLE IF EXISTS shops;
DROP TABLE IF EXISTS notifications;
DROP TABLE IF EXISTS audit_log;
DROP TABLE IF EXISTS event_outbox;
DROP TABLE IF EXISTS usage_counters;
DROP TABLE IF EXISTS subscriptions;
DROP TABLE IF EXISTS data_grants;
DROP TABLE IF EXISTS user_roles;
DROP TABLE IF EXISTS role_permissions;
DROP TABLE IF EXISTS roles;
DROP TABLE IF EXISTS sessions;
DROP TABLE IF EXISTS credentials;
DROP TABLE IF EXISTS users;
DROP TABLE IF EXISTS memberships;
DROP TABLE IF EXISTS tenants;
DROP TABLE IF EXISTS schema_migrations;
