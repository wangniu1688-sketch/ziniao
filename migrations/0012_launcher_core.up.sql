-- =============================================================================
-- schema version: m1-launcher-core v1
-- 启动器内核（M1）：手工代理台账 + 显式分组 + 店铺/环境扩展列
--
-- 本期只做加法：新增 proxies / groups 两张表，给 shops、environments 各加若干列。
-- 后端地基（指纹画像与注入、TLS/JA3/JA4、MITM CA、代理 Provider/Router/Allocator、
-- 出口绑定与历史、安全告警、事件总线、审计、通知、身份权限、租户隔离、0001–0011）
-- 一律不动。
--
-- 沿用 0001–0011 的规范：
--   - 表名复数 snake_case，主键 id text（platform/ids.New()，26 字符无前缀）；
--   - 时间列 timestamptz NOT NULL DEFAULT now()，updated_at 由应用层维护（全库 0 trigger）；
--   - 租户域表必须 tenant_id NOT NULL + RLS（只 ENABLE 不 FORCE，策略名 tenant_isolation）；
--   - 索引命名 idx_{表}_{语义} / uq_{表}_{语义}，统一 IF NOT EXISTS，复合索引首列 tenant_id；
--   - 枚举用 CHECK 收口（Go 侧再判一次：mem 驱动没有 CHECK）；
--   - 整个文件在单事务内执行 → 禁止 CREATE INDEX CONCURRENTLY / VACUUM。
--
-- 可回滚：见 0012_launcher_core.down.sql
-- =============================================================================

-- ---------- 手工静态代理台账 ----------
-- 与 devices.proxy_* 的区别：devices 是「有采购/续费/健康分」的网络资产，
-- proxies 是运营手工录入的静态出口，只有地址、凭据与启停，没有生命周期。
-- password_enc 存信封加密密文（v<ver>.<wrapped_dek>.<ct>），明文只存在于
-- Go 进程内存：不进 API 响应（只回 has_password 布尔）、不进审计 detail、不进日志。
CREATE TABLE IF NOT EXISTS proxies (
    id           text PRIMARY KEY,
    tenant_id    text NOT NULL,
    name         text NOT NULL,
    protocol     text NOT NULL DEFAULT 'http',   -- http|https|socks5
    host         text NOT NULL,
    port         integer NOT NULL,
    username     text,                           -- 用户名不是机密，可与 host 一并展示
    password_enc text,                           -- L4：信封加密密文；NULL/'' = 无口令
    key_version  integer NOT NULL DEFAULT 1,     -- 与 shop_credentials.key_version 同口径
    country      text,
    note         text,
    status       text NOT NULL DEFAULT 'active', -- active|disabled
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);

-- 同一租户下代理名不可重复：列表页靠 name 认人，两个「美国-01」等于台账失效。
CREATE UNIQUE INDEX IF NOT EXISTS uq_proxies_tenant_name
    ON proxies (tenant_id, name);
CREATE INDEX IF NOT EXISTS idx_proxies_tenant_status
    ON proxies (tenant_id, status, created_at DESC);

-- ---------- 分组（把 shops.group_name 的文本分组升格为实体） ----------
-- 实体化的理由：文本分组做不到「改名同步所有成员」「空分组才能删除」「按组筛选」，
-- 而这三件事是店铺网格页的日常操作。
-- environment_count 不落缓存列：缓存列的漂移点是「改组 / 软删 / 删环境」三条写路径，
-- 漏更新就会让「空分组才能删除」这条硬约束失效。本期规模下实时 COUNT（一次 GROUP BY）。
CREATE TABLE IF NOT EXISTS groups (
    id         text PRIMARY KEY,
    tenant_id  text NOT NULL,
    name       text NOT NULL,
    sort       integer NOT NULL DEFAULT 0,      -- 侧栏排序，默认按 created_at
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX IF NOT EXISTS uq_groups_tenant_name
    ON groups (tenant_id, name);
CREATE INDEX IF NOT EXISTS idx_groups_tenant_created
    ON groups (tenant_id, created_at DESC);

-- ---------- shops 扩展列 ----------
-- 自定义编号：运营侧的「店铺编号」，与系统 id 分离，允许人工可读（如 AMZ-001）。
ALTER TABLE shops ADD COLUMN IF NOT EXISTS code text;
-- 分组外键（应用层外键，不加 DB FK：与全局约定一致，有效性由服务层保证）。
ALTER TABLE shops ADD COLUMN IF NOT EXISTS group_id text;

-- 部分唯一索引：code 可以整列为空（历史数据），一旦填了同租户内不可重复。
CREATE UNIQUE INDEX IF NOT EXISTS uq_shops_tenant_code
    ON shops (tenant_id, code) WHERE code IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_shops_tenant_group
    ON shops (tenant_id, group_id) WHERE deleted_at IS NULL;

-- group_name 保留不动：现有 PUT /api/v1/shops/{id}/group 与既有 e2e 依赖它。
-- 服务层写 group_id 时同步写 group_name（双写），读路径以 group_id 为准。

-- ---------- environments 扩展列 ----------
ALTER TABLE environments ADD COLUMN IF NOT EXISTS proxy_id text;          -- 手工代理绑定；非空即该环境出口走它
ALTER TABLE environments ADD COLUMN IF NOT EXISTS country text;           -- 指纹国家（列表筛选用，快照里的 country 仍是指纹权威）
ALTER TABLE environments ADD COLUMN IF NOT EXISTS start_url text;         -- 启动后自动打开的首个页面
ALTER TABLE environments ADD COLUMN IF NOT EXISTS last_started_at timestamptz; -- 只在真正拉起成功时写（CDP 连通后）
ALTER TABLE environments ADD COLUMN IF NOT EXISTS thumbnail_path text;    -- 缩略图占位（本期可空，M3 之后落真实截帧）

CREATE INDEX IF NOT EXISTS idx_environments_tenant_status
    ON environments (tenant_id, status);
CREATE INDEX IF NOT EXISTS idx_environments_tenant_last
    ON environments (tenant_id, last_started_at DESC);
CREATE INDEX IF NOT EXISTS idx_environments_tenant_proxy
    ON environments (tenant_id, proxy_id) WHERE proxy_id IS NOT NULL;

-- ---------- 历史数据回填 ----------
-- 0012 是「加列 + 建新表 + 回填」，不删任何既有列，因此可回滚、可重放。
-- 把 shops.group_name（自由文本）升格为显式分组行：同租户同名只建一行，
-- id 用 md5 确定性生成（SQL 里拿不到 ids.New()，且需要幂等）。
INSERT INTO groups (id, tenant_id, name, created_at, updated_at)
SELECT 'grp_' || md5(s.tenant_id || ':' || s.group_name),
       s.tenant_id, s.group_name, now(), now()
FROM (SELECT DISTINCT tenant_id, group_name
      FROM shops
      WHERE group_name IS NOT NULL AND group_name <> '' AND deleted_at IS NULL) s
ON CONFLICT DO NOTHING;

UPDATE shops s
SET group_id = 'grp_' || md5(s.tenant_id || ':' || s.group_name)
WHERE s.group_id IS NULL
  AND s.group_name IS NOT NULL
  AND s.group_name <> '';

-- code / country / start_url / last_started_at / thumbnail_path 全部可空、不回填：
-- 历史环境的 country 虽可从 snapshot->>'country' 补，但那会形成两份真相，
-- 本期不补，等新环境创建时自然填充。

-- ---------- CHECK（DB 侧收口；Go 侧必须再判一次，mem 驱动没有 CHECK） ----------
ALTER TABLE proxies DROP CONSTRAINT IF EXISTS proxies_protocol_chk;
ALTER TABLE proxies ADD CONSTRAINT proxies_protocol_chk
    CHECK (protocol IN ('http', 'https', 'socks5'));

ALTER TABLE proxies DROP CONSTRAINT IF EXISTS proxies_port_chk;
ALTER TABLE proxies ADD CONSTRAINT proxies_port_chk
    CHECK (port BETWEEN 1 AND 65535);

ALTER TABLE proxies DROP CONSTRAINT IF EXISTS proxies_status_chk;
ALTER TABLE proxies ADD CONSTRAINT proxies_status_chk
    CHECK (status IN ('active', 'disabled'));

-- ---------- 行级安全（与 0009/0010/0011 同口径：只 ENABLE，不 FORCE） ----------
-- 分组天然跟随租户隔离：禁止出现跨租户可见分组。
ALTER TABLE proxies ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON proxies
    USING (tenant_id = current_tenant())
    WITH CHECK (tenant_id = current_tenant());

ALTER TABLE groups ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON groups
    USING (tenant_id = current_tenant())
    WITH CHECK (tenant_id = current_tenant());
