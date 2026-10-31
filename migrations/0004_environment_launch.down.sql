-- 回滚 Phase 2B：环境启动

ALTER TABLE environments DROP COLUMN IF EXISTS inject_script;
