# M1 前置设计（一）：数据模型与 0012 迁移 DDL 草案

> 状态：**已批复（2026-11-01）**。**数据模型 4 项拍板全部批准**，本文可作为编码依据。
> 批复补充的强制边界已并入正文：`groups` 必须携带 `tenant_id` 并纳入 RLS，
> `environments.group_id` 为应用层外键，**禁止出现跨租户可见分组**；
> 分组本期不参与分组级数据范围（不做成员级 ACL / 不做分组级授权）。
> 配套文档：`docs/design-m1-api.md`（接口定稿）、`docs/design-m1-browser-manager.md`（BrowserManager 状态机）。
> 本文所有「现状」结论均来自对本仓库实际代码的核对（迁移 0001–0011、`platform/types`、`shop/environment.go`、`network/*`），不依赖推断。

---

## 1. 范围与原则

本期只做**加法**：新增两张表（手工代理台账、显式分组）+ 两张既有表扩展列（shops、environments）。
后端地基（指纹画像与注入、TLS/JA3/JA4、请求头对齐、MITM CA、代理 Provider/Router/Allocator、出口绑定与历史、安全告警、事件总线、审计、通知、身份权限、租户隔离、0001–0011 迁移）**一律不动**。

沿用既有规范（已从 0001–0011 归纳）：

- 表名复数 snake_case；列名 snake_case；主键 `id text`（生成用 `platform/ids.New()`，26 字符可排序字符串，**无前缀**）；
- 时间列 `timestamptz NOT NULL DEFAULT now()`，**不建触发器**，`updated_at` 由应用层维护（全库 0 个 trigger）；
- 租户域表必须 `tenant_id text NOT NULL`；RLS **只 `ENABLE` 不 `FORCE`**（0009/0010/0011 的近期口径，FORCE 会让维护 SQL「执行成功但零行」），策略名统一 `tenant_isolation`；
- 索引新派命名 `idx_{表}_{语义}` / `uq_{表}_{语义}`，统一 `IF NOT EXISTS`，复合索引首列 `tenant_id`；
- 枚举在 DB 侧用 `ALTER TABLE ... ADD CONSTRAINT ... CHECK`（Go 侧再判一次，因为 mem 驱动没有 CHECK）；
- 整个 up 脚本在**单事务**内执行 → 禁止 `CREATE INDEX CONCURRENTLY` / `VACUUM`；
- 必须同时提交同名 `.down.sql`（`platform/migrate` 的 `Down()` 读不到就报「迁移文件缺失」）。

---

## 2. 现状基线（核对结论，作为设计约束）

| 事实 | 位置 | 对本期的影响 |
|---|---|---|
| `shops` 已有 `group_name text`（0002 加的**文本**分组，无实体）、`tags text[]`、`device_id` | `0002:198-202` | 分组升格为实体后需回填，见 §5 |
| `environments` 字段：id/tenant_id/shop_id/device_id/profile_id/baseline_hash/kernel_major/tls_hello_id/status/snapshot/consistency/created_at/updated_at + `inject_script`（0004） | `0002:253-267`、`0004` | 本期只加列，`inject_script` 语义不变（仍由服务端生成） |
| 环境以 `shop_id` 为唯一键（`GetByShop`），**一店铺一环境** | `platform/store/store.go:295-303` | 分组的 `environment_count` 等价于「分组下店铺数」 |
| `devices` 已自带代理字段 `protocol/proxy_host/proxy_port/proxy_user/proxy_secret_enc` | `0002:205-234` | 与新增 `proxies` 表的边界见 §3.1 |
| 代理凭据流转现状：`LaunchSpec.Upstream{Username,Password}` 随启动单下发给客户端 | `shop/environment.go:170-205` | **本期改**：口令只留 Go 内存，响应体剔除（详见接口文档 §5） |
| `types.Device.ProxySecretEnc` 是 `json:"-"`，API 永不返回密文 | `platform/types/types.go:213` | `proxies.password_enc` 同口径，且 API 只返回 `has_password` 布尔 |
| 信封加密 API：`crypto.EnvelopeCipher.Seal/Open/Rekey/CurrentVersion` | `platform/crypto/envelope.go:114-173` | 手工代理口令复用它，落库字段 `password_enc` + `key_version` |
| 审计钩子**不是 DB 触发器**，是「写业务 + 事件入 `event_outbox`」同事务，由 Sink 落 `audit_log` | `audit/audit.go:34-68` | 新增实体沿用：不发新 trigger，只发事件 |
| 双存储驱动 `mem` / `pg` 必须同时实现（ADR-0007） | `platform/store` | 新增 Repo 需两个驱动各写一遍 |

---

## 3. 新增表

### 3.1 `proxies` —— 手工静态代理台账

**与 `devices.proxy_*` 的边界（避免两张表互相越界）**：

- `devices` = **网络资产台账**（云网络设备 / 紫鸟盒子 / VPS），带采购、续费、健康分、纯度分、配额，出口由 `Allocator` 从代理商分发；
- `proxies` = **运营手工录入的静态代理**，无采购生命周期，只做「地址 + 凭据 + 备注 + 启停」，由环境直接引用成出口。

一个环境的出口来源三选一，互斥且显式：

1. `environments.proxy_id` 非空 → 用手工代理（本地 TLSProxy 上游 = 该代理）；
2. 否则 → `Allocator.AllocateEx()` 从代理商池分配（现有链路，不变）；
3. 显式「直连」→ 本地 TLSProxy 上游策略为 `direct`（仍走本地代理，链路形态统一）。

```sql
-- ---------- 手工静态代理台账 ----------
-- 与 devices.proxy_* 的区别：devices 是「有采购/续费/健康分」的网络资产，
-- proxies 是运营手工录入的静态出口，只有地址、凭据与启停，没有生命周期。
-- password_enc 存信封加密密文（v<ver>.<wrapped_dek>.<ct>），明文只存在于
-- Go 进程内存，不进 API 响应、不进审计 detail、不进日志（L4）。
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

-- 同一租户下代理名不可重复：列表页靠 name 认人，重复两个「美国-01」等于台账失效
CREATE UNIQUE INDEX IF NOT EXISTS uq_proxies_tenant_name
    ON proxies (tenant_id, name);
CREATE INDEX IF NOT EXISTS idx_proxies_tenant_status
    ON proxies (tenant_id, status, created_at DESC);
```

### 3.2 `groups` —— 显式分组实体

`environment_count` **采用实时统计，不落缓存列**。理由：

- 缓存列的漂移点是「店铺改组 / 店铺软删 / 环境删除」三条写路径，任何一条漏更新就会让「空分组才能删除」这个硬约束失效（把还有店铺的组删了，或反过来删不掉）；
- 本期分组规模是运营手维护的几十条量级，一次 `GROUP BY` 的成本可忽略；
- mem 驱动的 `WithTx` 不支持回滚，缓存列在 mem 上更容易与实际不一致。

若后续分组量上千且列表页出现性能问题，再加缓存列 + 与 `shops.group_id` 同事务维护（本文不预建该列）。

「空」的判定：该分组下 `deleted_at IS NULL` 的店铺数为 0（一店铺一环境，无店铺即无环境；环境删除不影响分组归属）。

```sql
-- ---------- 分组（把 shops.group_name 的文本分组升格为实体） ----------
-- 为什么实体化：文本分组做不到「改名同步所有成员」「空分组才能删除」「按组做数据范围」，
-- 而这三件事是店铺网格页的日常操作。
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
```

实时计数查询（列表接口一次 GROUP BY 带出，避免 N+1）：

```sql
SELECT g.id,
       count(s.id) FILTER (WHERE s.deleted_at IS NULL) AS environment_count
FROM groups g
LEFT JOIN shops s ON s.group_id = g.id AND s.tenant_id = g.tenant_id
WHERE g.tenant_id = current_tenant()
GROUP BY g.id;
```

---

## 4. 既有表扩展

### 4.1 `shops`

```sql
-- 自定义编号：运营侧的「店铺编号」，与系统 id 分离，允许人工可读（如 AMZ-001）
ALTER TABLE shops ADD COLUMN IF NOT EXISTS code text;
-- 分组外键（应用层外键，不加 DB FK：与全局约定一致，shops.group_id 由服务层保证有效）
ALTER TABLE shops ADD COLUMN IF NOT EXISTS group_id text;

-- 部分唯一索引：code 可以整列为空（历史数据），但一旦填了同租户内不可重复
CREATE UNIQUE INDEX IF NOT EXISTS uq_shops_tenant_code
    ON shops (tenant_id, code) WHERE code IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_shops_tenant_group
    ON shops (tenant_id, group_id) WHERE deleted_at IS NULL;
```

`group_name` **保留不动**：现有 `PUT /api/v1/shops/{id}/group` 与既有 e2e 测试依赖它；服务层写 `group_id` 时同步写 `group_name`（双写，读路径以 `group_id` 为准），等前端全部切到 `/api/v1/groups` 后再考虑下线。

**不改 shops 加 `country`**：国家是指纹属性，权威值在 `environments.country`；店铺列表页需要的国家由聚合查询 join 带出，避免两处可写、互相漂移。

### 4.2 `environments`

```sql
-- 手工代理绑定：非空时该环境出口走此代理，否则走 Allocator（代理商池）/ direct
ALTER TABLE environments ADD COLUMN IF NOT EXISTS proxy_id text;
-- 指纹国家（快照里已有 country，这里提列是为了列表筛选与排序）
ALTER TABLE environments ADD COLUMN IF NOT EXISTS country text;
-- 起始 URL：启动后自动打开的首个页面（为空则打开 about:blank）
ALTER TABLE environments ADD COLUMN IF NOT EXISTS start_url text;
-- 最近启动时间：与 status 分离，status 归 idle 后仍能回答「这个店上次什么时候开的」
ALTER TABLE environments ADD COLUMN IF NOT EXISTS last_started_at timestamptz;
-- 缩略图占位（本期可空）：存 data/ 下的相对路径，M3 之后再落真实截帧
ALTER TABLE environments ADD COLUMN IF NOT EXISTS thumbnail_path text;

CREATE INDEX IF NOT EXISTS idx_environments_tenant_status
    ON environments (tenant_id, status);
CREATE INDEX IF NOT EXISTS idx_environments_tenant_last
    ON environments (tenant_id, last_started_at DESC);
CREATE INDEX IF NOT EXISTS idx_environments_tenant_proxy
    ON environments (tenant_id, proxy_id) WHERE proxy_id IS NOT NULL;
```

`last_started_at` 与 `updated_at` 的区别要在代码注释里写清楚：`updated_at` 每次写都动（含停用、改代理），`last_started_at` 只在**真正拉起成功**时写（见 BrowserManager 文档 §3，CDP 连通后置位）。

---

## 5. 数据回填与字段迁移方式

0012 是「加列 + 建新表 + 回填」，**不删任何既有列**，因此可回滚、可重放。

```sql
-- 回填：把历史 shops.group_name（自由文本）升格为显式分组行。
-- 同租户同名只建一行；id 用 md5 确定性生成（SQL 里拿不到 ids.New()，且需要幂等）。
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
```

- `code`、`country`、`start_url`、`last_started_at`、`thumbnail_path` 全部**可空、不回填**（历史环境 `country` 可从 `snapshot->>'country'` 补，但快照与列会形成两份真相，本期不补，等新环境创建时自然填充）；
- 旧 Tauri 时期的测试 profile / 本地测试账号**不迁移**（见批复 3.4），新环境统一落到 `data/profiles/<env-id>/`；
- 数据库**不记录易失 PID/端口**作为长期事实（批复 3.6）：PID / CDP 端口 / 本地代理端口只进 `data/runtime/registry.json`。

---

## 6. 约束、索引、RLS 汇总（0012 全部 DDL 的收口段）

```sql
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
ALTER TABLE proxies ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON proxies
    USING (tenant_id = current_tenant())
    WITH CHECK (tenant_id = current_tenant());

ALTER TABLE groups ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON groups
    USING (tenant_id = current_tenant())
    WITH CHECK (tenant_id = current_tenant());
```

### 审计钩子（不发 DB 触发器，只发事件）

沿 `shop.EnvService` 的模式：`st.WithTx(tctx, func(tx){ 写仓储; bus.Emit(tctx, tx, event) })`，事件与业务写同事务，由 `audit.Sink` 落 `audit_log`。

| 事件 type | 触发 | Data（不含任何口令 / 脚本正文） |
|---|---|---|
| `proxy.created` | 新建手工代理 | proxy_id、protocol、host、port、has_password、country |
| `proxy.updated` | 改地址/凭据/启停 | proxy_id、变更字段列表（**字段名**，不是值）、has_password |
| `proxy.deleted` | 删除（被引用时拒绝） | proxy_id、name |
| `env.proxy_bound` | 环境绑定/解绑手工代理 | env_id、shop_id、proxy_id（null=解绑） |
| `group.created` / `group.updated` / `group.deleted` | 分组 CRUD | group_id、name（删除时记录 member_count=0 的校验结果） |
| `env.launch_failed` | 启动任一阶段失败回滚（新增，见 BrowserManager 文档 §4） | env_id、stage、reason（**不含** upstream 地址与端口） |
| `env.reconciled` | 启动器重启后残留进程对账 | env_id、pid、action（closed/reset）、reason |

---

## 7. 本地运行时注册表（非数据库）

`data/runtime/registry.json` 只做**本机进程态缓存**，权威状态永远以 PostgreSQL `environments.status` 为准（批复 3.6）。

```json
{
  "schema": 1,
  "updated_at": "2026-11-01T10:00:00Z",
  "entries": [
    {
      "env_id": "01H...",
      "shop_id": "01H...",
      "tenant_id": "01H...",
      "pid": 12345,
      "cdp_port": 53127,
      "local_proxy_port": 53128,
      "profile_dir": "G:\\ziniao\\data\\profiles\\01H...",
      "chrome_path": "C:\\Program Files\\Google\\Chrome\\Application\\chrome.exe",
      "started_at": "2026-11-01T09:58:12Z",
      "proc_state": "running"
    }
  ]
}
```

- 写入时机：注册表变更（启动成功 / 停止完成 / 对账修正）后异步落盘；进程崩溃时文件会停留，靠下次启动的 `reconcile` 纠正；
- `proc_state` 是**进程态**（`starting|running|stopping|unknown`），与业务状态 `created/running/idle/stopped` 严格分离，不写回数据库。

---

## 8. 数据目录（`ZINIAO_DATA_DIR`）

新增配置项（当前代码里**不存在** `ZINIAO_DATA_DIR`，需新加）：

| 配置 | 默认 | 说明 |
|---|---|---|
| `ZINIAO_DATA_DIR` | `./data` | 启动后统一转绝对路径（`filepath.Abs` + `Clean`） |
| `ZINIAO_HTTP_ADDR` 默认值变更 | `:8080` → `127.0.0.1:8080` | 批复 3.5：禁止监听 `0.0.0.0` |
| `ZINIAO_CA_DIR` | `$DATA_DIR/ca` | 复用 `network.LoadOrCreateCA`；未设置时退回现有 `%APPDATA%\ziniao\ca` 口径 |

目录结构（启动时 `MkdirAll`）：

```
data/
  profiles/<env-id>/        Chrome --user-data-dir，含 DevToolsActivePort
  extensions/sidepanel/     侧栏扩展源码（本期默认不加载，见 BrowserManager 文档 §11）
  runtime/registry.json     本机进程态缓存
  logs/                     启动器/浏览器相关本地日志
  ca/                       MITM 根证书与私钥
```

---

## 9. `0012_launcher_core.down.sql`

```sql
ALTER TABLE environments DROP COLUMN IF EXISTS thumbnail_path;
ALTER TABLE environments DROP COLUMN IF EXISTS last_started_at;
ALTER TABLE environments DROP COLUMN IF EXISTS start_url;
ALTER TABLE environments DROP COLUMN IF EXISTS country;
ALTER TABLE environments DROP COLUMN IF EXISTS proxy_id;

DROP TABLE IF EXISTS proxies;
DROP TABLE IF EXISTS groups;

ALTER TABLE shops DROP COLUMN IF EXISTS group_id;
ALTER TABLE shops DROP COLUMN IF EXISTS code;
```

（`uq_shops_tenant_code` / `idx_shops_tenant_group` 随列自动消失；`uq_proxies_tenant_name` 随表消失。）

---

## 10. 仓储层接口草案（Go）

新增到 `platform/store/store.go`，并同时实现 `pg` 与 `mem` 两个驱动（ADR-0007）：

```go
type ProxyRepo interface {
    Create(ctx, *types.Proxy) error
    Get(ctx, id string) (*types.Proxy, error)
    List(ctx, ProxyFilter) ([]*types.Proxy, int, error)  // status/keyword/limit/offset
    Update(ctx, *types.Proxy) error
    Delete(ctx, id string) error
}

type GroupRepo interface {
    Create(ctx, *types.Group) error
    Get(ctx, id string) (*types.Group, error)
    List(ctx) ([]*types.Group, error)                    // 带 environment_count
    Update(ctx, *types.Group) error
    Delete(ctx, id string) error
    CountMembers(ctx, id string) (int, error)            // 空分组才能删除的唯一判据
}
```

`types.Proxy` 的 `PasswordEnc` 字段必须 `json:"-"`（与 `types.Device.ProxySecretEnc` 同口径），API 层只输出 `has_password bool`。

---

## 11. 待确认项 → 批复结论（2026-11-01）

| # | 本文方案 | 批复 |
|---|---|---|
| 1 | `environment_count` 实时统计，不建缓存列 | ✅ 批准。本期规模下实时 `COUNT`，不引入缓存列与一致性维护 |
| 2 | `country` 只落 `environments`，`shops` 不加冗余列 | ✅ 批准。国家属于环境指纹属性，店铺侧由聚合查询 join 带出 |
| 3 | `code` 只落 `shops` | ✅ 批准 |
| 4 | 分组本期不参与分组级数据范围 | ✅ 批准，附强制边界：不做成员级 ACL / 不做分组级授权，但 `groups` 必须带 `tenant_id` 并纳入 RLS，`environments.group_id` 外键关联分组，禁止跨租户可见分组 |

> 0012 迁移已按本文落地：`migrations/0012_launcher_core.up.sql` / `.down.sql`。
