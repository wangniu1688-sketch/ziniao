-- Phase 2B：环境启动（真实浏览器窗口的能力源）
--
-- 注入脚本由服务端在建环境时一次性生成并落库，客户端启动时原样取走注入。
-- 客户端禁止自行随机化任何字段——随机化等于与画像库脱钩（L2 致命项）。

ALTER TABLE environments ADD COLUMN IF NOT EXISTS inject_script text;

COMMENT ON COLUMN environments.inject_script IS '文档创建前注入的 JS（服务端生成，客户端原样执行）';
