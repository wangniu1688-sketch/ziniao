DROP POLICY IF EXISTS tenant_isolation ON notification_prefs;
DROP INDEX IF EXISTS idx_notification_prefs_user;
DROP TABLE IF EXISTS notification_prefs;
