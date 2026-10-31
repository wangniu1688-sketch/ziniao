-- 回滚脚本：phase1-device-env v1
DROP POLICY IF EXISTS tenant_isolation ON shop_credentials;
DROP POLICY IF EXISTS tenant_isolation ON security_events;
DROP POLICY IF EXISTS tenant_isolation ON access_policies;
DROP POLICY IF EXISTS tenant_isolation ON environments;
DROP POLICY IF EXISTS tenant_isolation ON device_bindings;
DROP POLICY IF EXISTS tenant_isolation ON devices;

DROP TABLE IF EXISTS shop_credentials;
DROP TABLE IF EXISTS security_events;
DROP TABLE IF EXISTS access_policies;
DROP TABLE IF EXISTS environments;
DROP TABLE IF EXISTS device_bindings;
DROP TABLE IF EXISTS devices;

DROP INDEX IF EXISTS shops_device_idx;
DROP INDEX IF EXISTS shops_group_idx;
ALTER TABLE shops DROP COLUMN IF EXISTS device_id;
ALTER TABLE shops DROP COLUMN IF EXISTS tags;
ALTER TABLE shops DROP COLUMN IF EXISTS group_name;
