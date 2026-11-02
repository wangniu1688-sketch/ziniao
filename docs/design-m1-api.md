# M1 前置设计（二）：接口定稿

> 状态：**已批复（2026-11-01）**，21 个接口与权限码定稿。
> **里程碑纪律**：仅实现当期所需接口，后续里程碑的接口**不得提前开发、不得在前端暴露入口**
> （§3 总表的「里程碑」列即交付边界）。M1 只交付 #1 `#2`。
> 敏感字段遮蔽（§5）对全部里程碑生效：代理口令、上游口令、注入脚本正文不得出现在
> 任何 HTTP 响应、前端页面、日志与审计 detail；代理只回 `has_password` 布尔。
> 配套：`docs/design-m1-schema.md`（数据模型）、`docs/design-m1-browser-manager.md`（BrowserManager 状态机）。
> 命名与现状核对依据：`gateway/server.go:98-137`（routes）、`gateway/phase1.go:24-61`（Phase 1 路由）、`gateway/docs.go`（OpenAPI 登记，CI 有覆盖）。

---

## 1. 全局约定

| 项 | 约定 |
|---|---|
| 前缀 | `/api/v1`（与现状一致） |
| 鉴权 | 全部走 `s.authed(...)`：`Authorization: Bearer <token>` → `identity.Authenticate` → `ctx = store.WithTenant(ctx, principal.TenantID)` |
| 授权 | `s.authorize(r, "<permission>", "<path>")`；页面级 `page:*`、功能级 `*:action` 两层不变 |
| 限流 | 复用 `rateLimiter`（默认 600/min，按 UserID） |
| 列表响应 | `{ "items": [...], "total": N }`（与 `/api/v1/shops`、`/api/v1/audit/logs` 同形） |
| 分页 | `?limit=50&offset=0`，`limit` 上限 200 |
| 错误响应 | `{ "code": "...", "message": "...", "request_id": "..." }`，code 取自 `platform/apierr` 既有 11 个，**不新增 code** |
| 文档登记 | 每个新接口必须在 `gateway/docs.go` 的 `ops` 里加一行，否则 `/openapi.json` 缺项会被 CI 测试打回 |
| 本地回环 | 服务只绑 `127.0.0.1:8080`（`ZINIAO_HTTP_ADDR` 默认值由 `:8080` 改为 `127.0.0.1:8080`） |

### 错误码使用口径

| 场景 | HTTP | code |
|---|---|---|
| 未带 / 失效 token | 401 | `unauthorized` |
| 权限不足（含数据范围不匹配） | 403 | `forbidden` |
| 环境 / 分组 / 代理不存在 | 404 | `not_found` |
| 非空分组删除、被引用代理删除、环境已停用却要启动、同环境重复启动 | 409 | `conflict` |
| 参数不合法（协议不在枚举、端口越界、name 为空、分页越界） | 400 | `validation_failed` |
| 出口 IP 分配失败、Chrome 拉起失败、CDP 超时（已回滚） | 500 | `internal_error` |
| 触发限流 | 429 | `rate_limited` |

> 说明：`internal_error` 的 `message` 必须对用户可读（如「浏览器启动失败：CDP 连接超时（20s）」），详细原因写服务端日志与 `env.launch_failed` 事件，不回传堆栈。

---

## 2. 需新增的权限码

登记到 `iam/permissions.go` 的目录（`AllKeys()` 自动覆盖 `owner`），并**显式补进 `BuiltinRoles()` 中非 owner 角色的清单**（`iam/roles.go:22-84`，owner 走 `AllKeys()`，其它角色是硬编码清单，不写就永远拿不到）。

| 权限码 | Layer | Group | Risk | 说明 |
|---|---|---|---|---|
| `group:read` | function | 店铺 | low | 查看分组 |
| `group:manage` | function | 店铺 | medium | 分组 CRUD |
| `proxy:read` | function | 网络 | low | 查看手工代理台账（不含口令） |
| `proxy:manage` | function | 网络 | high | 代理 CRUD / 改凭据 / 拨测 |
| `env:read` | function | 环境 | low | 环境聚合列表与详情 |
| `env:update` | function | 环境 | medium | 改环境配置（绑定代理 / 起始 URL / 国家 / 编号） |
| `page:groups` | page | 店铺 | — | 分组页 |
| `page:proxies` | page | 网络 | — | 代理台账页 |
| `page:logs` | page | 安全 | — | 日志中心页 |

复用既有、不新增：`env:launch` / `env:stop` / `env:create` / `env:delete` / `env:check`（runtime 视图用 `env:launch`）、`device:read`（设备 runtime 视图）、`audit:read`（聚合日志）、`shop:read` / `shop:create` / `shop:update`。

---

## 3. 接口清单总表

| # | 方法 | 路径 | 权限 | 里程碑 | 说明 |
|---|---|---|---|---|---|
| 1 | GET | `/api/v1/system/info` | `-`（登录即可） | M1 | 启动器自描述：版本/数据目录/浏览器探测/能力 |
| 2 | GET | `/api/v1/system/stats` | `-` | M1 | 首页静态统计 |
| 3 | GET | `/api/v1/groups` | `group:read` | M2 | 分组列表（含 environment_count） |
| 4 | POST | `/api/v1/groups` | `group:manage` | M2 | 新建分组 |
| 5 | PUT | `/api/v1/groups/{id}` | `group:manage` | M2 | 改名 / 排序 |
| 6 | DELETE | `/api/v1/groups/{id}` | `group:manage` | M2 | 空分组才能删 |
| 7 | GET | `/api/v1/proxies` | `proxy:read` | M2 | 代理台账 |
| 8 | POST | `/api/v1/proxies` | `proxy:manage` | M2 | 新建 |
| 9 | PUT | `/api/v1/proxies/{id}` | `proxy:manage` | M2 | 改（含改口令） |
| 10 | DELETE | `/api/v1/proxies/{id}` | `proxy:manage` | M2 | 被环境引用则 409 |
| 11 | POST | `/api/v1/proxies/{id}/test` | `proxy:manage` | M2 | 服务端连通性拨测 |
| 12 | PUT | `/api/v1/shops/{id}/environment/proxy` | `env:update` | M2 | 环境绑定 / 解绑手工代理 |
| 13 | GET | `/api/v1/environments` | `env:read` | M2 | 环境聚合列表（跨店铺） |
| 14 | GET | `/api/v1/environments/{id}` | `env:read` | M2 | 环境详情（含 runtime 态） |
| 15 | PATCH | `/api/v1/environments/{id}` | `env:update` | M2 | 改起始 URL / 国家 / 编号 |
| 16 | POST | `/api/v1/environments/{id}/launch` | `env:launch` | M3 | 真拉起 Chrome（新路径） |
| 17 | POST | `/api/v1/environments/{id}/stop` | `env:stop` | M3 | 优雅停止 |
| 18 | GET | `/api/v1/environments/runtime` | `env:launch` | M3 | 本机运行时注册表快照（诊断） |
| 19 | GET | `/api/v1/devices/runtime` | `device:read` | M2 | 设备页 runtime 视图（列表） |
| 20 | GET | `/api/v1/devices/{id}/runtime` | `device:read` | M2 | 单设备 runtime 视图 |
| 21 | GET | `/api/v1/logs` | `audit:read` | M2 | 聚合日志 |

> 现有 `/api/v1/shops/{id}/environment/launch|stop|deactivate|activate|delete` **全部保留不动**（既有 e2e 依赖），M3 后内部改为转发到同一 service；响应体剔除 `upstream`（见 §5）。

---

## 4. 接口详情

### 4.1 `GET /api/v1/system/info`（M1）

响应 200：

```json
{
  "service": { "name": "ziniao-gateway", "env": "dev", "uptime_sec": 1234 },
  "store":   { "driver": "memory" },
  "data": {
    "dir":      "G:\\ziniao\\data",
    "profiles": "G:\\ziniao\\data\\profiles",
    "runtime":  "G:\\ziniao\\data\\runtime",
    "ca_dir":   "G:\\ziniao\\data\\ca",
    "ca_ready": true
  },
  "browser": {
    "installed": true,
    "kind":      "chrome",
    "path":      "C:\\Program Files\\Google\\Chrome\\Application\\chrome.exe",
    "version":   "131.0.6778.86",
    "major":     131,
    "candidates": [
      { "kind": "chrome", "path": "...", "version": "131.0.6778.86" },
      { "kind": "edge",   "path": "...", "version": "131.0.2903.86" }
    ]
  },
  "capabilities": {
    "launch": true, "cdp": true, "local_proxy": true, "mitm": true, "sidepanel": false
  },
  "listen": { "http": "127.0.0.1:8080" },
  "limits": { "env_reclaim_hours": 12, "api_rate_per_minute": 600 }
}
```

- **绝不返回**：DSN、JWT secret、任何上游代理账号口令、`ZINIAO_IP_TOKEN`；
- `browser.installed=false` 时前端首页引导卡显示「未检测到 Chrome/Edge」，`launch` 置 false。

### 4.2 `GET /api/v1/system/stats`（M1，首页静态统计）

响应 200：

```json
{
  "shops":    { "total": 12, "running_envs": 3 },
  "devices":  { "total": 5 },
  "proxies":  { "total": 8, "active": 7 },
  "groups":   { "total": 3 },
  "approvals":{ "pending": 2 },
  "generated_at": "2026-11-01T10:00:00Z"
}
```

`running_envs` 取 `environments.status='running'` 计数（权威在库，不采样本机进程）；M1 是静态快照，前端不轮询。

### 4.3 分组

**`POST /api/v1/groups`**
```json
// request
{ "name": "亚马逊-美国", "sort": 10 }
// response 201
{ "id": "01H...", "tenant_id": "01H...", "name": "亚马逊-美国", "sort": 10,
  "environment_count": 0, "created_at": "...", "updated_at": "..." }
```
400：`name` 为空 / 超 64 字符；409：同租户同名。

**`GET /api/v1/groups`** → `{ "items": [ {...,"environment_count": N}, ... ], "total": N }`，按 `sort, created_at` 排序，`environment_count` 一次 GROUP BY 带出。

**`PUT /api/v1/groups/{id}`** → 200 返回整行；改名后成员店铺的 `group_name` 同事务同步（兼容旧字段）。

**`DELETE /api/v1/groups/{id}`**
- 空分组 → 200 `{ "id": "...", "deleted": true }`
- 非空 → **409** `{ "code":"conflict", "message":"分组「亚马逊-美国」下仍有 7 个店铺，请先移出或改组" }`（判据：`GroupRepo.CountMembers` 实时计数，不用缓存）

### 4.4 代理台账

**`POST /api/v1/proxies`**
```json
// request
{ "name": "美国住宅-01", "protocol": "http", "host": "1.2.3.4", "port": 8000,
  "username": "u1", "password": "p1", "country": "US", "note": "", "status": "active" }
// response 201
{ "id":"01H...", "name":"美国住宅-01", "protocol":"http", "host":"1.2.3.4", "port":8000,
  "username":"u1", "has_password": true, "country":"US", "note":"", "status":"active",
  "key_version": 1, "bound_env_count": 0, "created_at":"...", "updated_at":"..." }
```
- `password` 入参**只进不出**：服务端 `cipher.Seal()` 后落 `password_enc`，响应与后续所有 GET 均不返回；
- 400：协议不在 `http|https|socks5`、端口越界、host 为空、name 为空/重复；
- 审计事件 `proxy.created` 的 `data` 只含 `has_password`，不含用户名与口令。

**`GET /api/v1/proxies`** → `{ items: [...], total }`，支持 `?status=active&keyword=&limit=&offset=`；每项含 `has_password`、`bound_env_count`（被多少环境引用，用于删除前置校验提示）。

**`PUT /api/v1/proxies/{id}`** → 200 整行（同样不返回口令）。`password` 传空串表示**清空口令**，传 `null`/缺省表示**不改**（与常见的「缺省即清空」区分开，避免误删）。

**`DELETE /api/v1/proxies/{id}`**
- 未被引用 → 200 `{ "id":"...", "deleted": true }`
- 被引用 → 409 `{ "message": "该代理仍被 3 个环境引用，请先解绑" }`

**`POST /api/v1/proxies/{id}/test`**（可选，M2 尾）
```json
// request  { "timeout_ms": 5000, "target": "https://www.example.com" }
// response 200
{ "ok": true, "elapsed_ms": 812, "exit_ip": "1.2.3.4", "exit_country": "US", "error": "" }
```
服务端拨测（复用 `network.NewDialer`），不返回任何凭据；`ok=false` 时 200 返回（拨测失败不是接口错误），只有参数非法才 400。出网总开关 `ZINIAO_PROXY_TEST`（默认 `on`，`off` 时返回「拨测已禁用」且不出网）与可配置目标 `ZINIAO_PROXY_TEST_TARGET`（默认 `https://api.ipify.org`）按 Q3 拍板于 M2 收尾落地（T12）；目标只接受 http/https，响应不回显上游口令。

**P3 变更（上游住宅/ISP 代理接入）**：

- 拨测结果**落库**：`check_status`（`unknown`/`ok`/`fail`）、`exit_ip`、`exit_country`、`latency_ms`、`last_checked_at`、`last_error`。原因是「能不能绑」必须来自库里的事实，而不是某次前端调用的返回值。
- 台账新增 `type`（`isp`/`static_residential`/`residential`/`datacenter`）与 `city`；`status` 仍只表启停（`active`/`disabled`），**拨测健康单独一列**，两者禁止互相覆盖。
- `off` 语义更新：返回 `skipped: true` + `ok: false`。`off` 是「部署侧摘掉了这条出网路径」，不是「代理不通」——没有这个标记，绑定校验会把「没验证过」当成「验证过且失败」。
- 可选的国家实测 `ZINIAO_PROXY_TEST_COUNTRY`（默认空 = 不实测）：配了才额外取一次 `country`，失败不致命，只留空并记 WARN。响应新增 `country_measured` 标注来源。
- 新增 `GET /api/v1/proxies/{id}`：唯一带 `password_mask`（掩码）的接口；列表为不做 N 次解密只给 `has_password`。

### 4.5 环境绑定代理

**`PUT /api/v1/shops/{id}/environment/proxy`**
```json
// request  { "proxy_id": "01H..." }   // null = 解绑，回落到 Allocator
// response 200
{ "env_id":"01H...", "shop_id":"01H...", "proxy_id":"01H...", "exit_mode":"manual" }
```
- `exit_mode`：`manual`（手工代理）/ `pool`（代理商池）/ `direct`（直连，仍走本地 TLSProxy）；
- 解绑后 `exit_mode` 回落 `pool`；
- 404：代理不存在或不属于本租户；400：代理 `status='disabled'`；409：环境正在运行（先停止）；
- 事件 `env.proxy_bound`（含 proxy_id，不含凭据）。

**P3 变更（上游住宅/ISP 代理接入）**：

- `ZINIAO_PROXY_TEST=on`（默认）时，绑定前置条件增加 `check_status=ok`；`off` 时不强制（内网部署不能被自己的开关卡死），但在审计里留 `probe_skipped=true`。
- 响应新增 `warnings: string[]`（软校验，**不阻断**）：同一代理被多个环境共用（一号一 IP）、出口国家与环境指纹国家不一致。
- 环境视图新增 `proxy_check_status` / `proxy_exit_ip` / `proxy_exit_country`：来自最近一次拨测，不是本次启动的实测值。
- 启动链路：阶段 1（`EnvService.Launch`）解析绑定代理后先做**启动前出口拨测**，`fail` 直接返回结构化错误，不进入浏览器拉起阶段；此刻本机没有任何资源，不需要逆序回滚，但状态复位仍走既有的 `MarkLaunchFailed` 语义，禁止悬空 `running`。失败**不退回直连**（直连 = 本机单 IP，比用一条不通的代理更糟）。

### 4.6 环境聚合列表与详情

**`GET /api/v1/environments`**
```
?group_id=&status=&country=&keyword=&exit_mode=&limit=50&offset=0
```
响应项（`EnvironmentView` 聚合视图，**不含注入脚本正文、不含上游口令**）：

```json
{
  "id":"01H...", "shop_id":"01H...", "shop_name":"亚马逊A店", "code":"AMZ-001",
  "group_id":"01H...", "group_name":"亚马逊-美国",
  "status":"running", "country":"US", "kernel_major":131, "tls_hello_id":"HelloChrome_131",
  "baseline_hash":"...", "start_url":"https://sellercentral.amazon.com",
  "exit_mode":"manual", "proxy_id":"01H...", "proxy_name":"美国住宅-01",
  "device_id":"01H...", "last_started_at":"2026-11-01T09:58:12Z",
  "thumbnail_path": null,
  "runtime": { "present": true, "pid": 12345, "cdp_port": 53127,
               "local_proxy_port": 53128, "proc_state": "running",
               "started_at": "2026-11-01T09:58:12Z" }
}
```

- 数据级收口与现状一致：`own` 范围只返回本人（或被委托）店铺的环境；
- `runtime` 由本机注册表带出，`present=false` 时字段缺省（`runtime: {"present": false}`）——**权威状态仍是 `status` 字段**，注册表缺失不代表环境没在跑（可能是另一台机器/启动器重启过），前端只把它当作「本机是否可见」提示；
- 列表不返回 `cdp_port` 之外的任何本地细节。

**`GET /api/v1/environments/{id}`** → 200 同上单条（`runtime` 完整）。404 不存在或无数据范围。

**`PATCH /api/v1/environments/{id}`**
```json
// request（全部可选）
{ "start_url": "https://...", "country": "US", "code": "AMZ-001" }
// response 200 整行
```
`country` 变更**不自动重算指纹**（指纹由画像库决定），仅影响出口选择与列表过滤；这点要在响应注释/文档里写明，避免误以为改了国家就换了指纹。

### 4.7 环境生命周期（M3）

**`POST /api/v1/environments/{id}/launch`**
```json
// request  { "open_url": "https://..." }   // 省略则用 environments.start_url
// response 200
{ "env_id":"01H...", "shop_id":"01H...", "status":"running",
  "pid": 12345, "cdp_port": 53127, "local_proxy_port": 53128,
  "profile_dir": "G:\\ziniao\\data\\profiles\\01H...",
  "kernel_major": 131, "baseline_hash": "...", "script_bytes": 18234,
  "opened_url": "https://...", "exit_mode": "manual", "started_at": "..." }
```
**关键约束（批复硬性）**：

- 响应**不含** `upstream` 任何字段（既有的 `LaunchSpec.Upstream.Username/Password` 从响应体中剔除，仅保留在 Go 内存里喂给本地 TLSProxy）；
- `status=running` 只在 Chrome 进程起来 **且** CDP 连通 **之后** 才置位（事务边界见 BrowserManager 文档 §3）；
- 409：环境已停用 / 该环境已在本机 running（重复启动）；
- 500 + `internal_error`：任一阶段失败且**已完整回滚**（端口释放、profile 半成状态清理、状态回到 idle）。

**`POST /api/v1/environments/{id}/stop`** → 200 `{ "env_id":"...", "status":"idle", "stopped_at":"...", "purge": {"profile_kept": true} }`；幂等（非 running 原样返回）。

**`GET /api/v1/environments/runtime`** → `{ "items":[...], "total":N, "generated_at":"..." }`，本机注册表快照，供排障与「设备 runtime 视图」共用；**不返回**任何上游凭据与注入脚本。

### 4.8 设备 runtime 视图（M2）

`GET /api/v1/devices/runtime`、`GET /api/v1/devices/{id}/runtime`，权限 `device:read`。

```json
{
  "device": { "id":"01H...", "name":"深圳住宅-03", "type":"cloud_network",
              "status":"active", "country":"CN", "asn_type":"residential",
              "health_score": 92, "purity_score": 88,
              "bound_accounts": 6, "concurrent_users": 2, "max_concurrent": 6 },
  "runtime": {
    "online": true, "last_seen_at": "...", "exit_ip": "1.2.3.4",
    "active_envs": [ { "env_id":"01H...", "shop_name":"亚马逊A店", "status":"running",
                       "proc_state":"running", "started_at":"..." } ],
    "active_users": [ { "user_id":"01H...", "nickname":"张三" } ],
    "proxy": { "protocol":"http", "host":"1.2.3.4", "port":8000, "username":"u1",
               "has_password": true }
  }
}
```

**边界声明**：`types.Device` 是网络资产台账，**不等于浏览器运行时**；runtime 段是「此刻的运行情况」投影，不写回 `devices` 表，也不参与设备 CRUD 的字段校验。

### 4.9 聚合日志（M2）

`GET /api/v1/logs`

```
?sources=audit,security,egress
&category=&level=&shop_id=&env_id=&actor_id=&action=
&from=2026-10-01T00:00:00Z&to=2026-11-01T00:00:00Z&keyword=
&include_detail=0&limit=50&offset=0
```

```json
{
  "items": [
    { "id":"01H...", "source":"audit",    "occurred_at":"...", "category":"env",
      "level":"info", "actor":"张三", "shop_id":"01H...", "env_id":"01H...",
      "action":"env.launched", "summary":"启动店铺环境（亚马逊A店）" },
    { "id":"01H...", "source":"security", "occurred_at":"...", "category":"risk",
      "level":"high", "shop_id":"01H...", "action":"env.exit_ip_rotated",
      "summary":"出口 IP 发生轮换" },
    { "id":"01H...", "source":"egress",   "occurred_at":"...", "category":"egress",
      "level":"info", "env_id":"01H...", "action":"rebound", "summary":"出口重新绑定" }
  ],
  "total": 128, "limit": 50, "offset": 0, "next_offset": 50
}
```

规则：

- 权限：`audit:read` 起；`sources` 含 `security` 时额外要求 `security:event.read`，含 `egress` 时额外要求 `egress:read`（缺权直接 403，不静默过滤——静默过滤会让「为什么少了一半日志」无从解释）；
- 三源统一按 `occurred_at DESC` 归并分页（各源分别查 `limit+offset` 后在内存归并，或各源查同一窗口再归并；本期用「同窗口归并 + 内存排序」，`limit` 上限 200）；
- `detail` **默认不返回**：`include_detail=1` 才带，且服务端按既有字段遮蔽规则过滤（财务字段打码、凭据字段剔除）；
- 不新增独立日志表，只做读侧聚合（写侧仍是 `audit_log` / `security_events` / `egress_binding_history`）。

---

## 5. 敏感字段遮蔽规则（聚合视图一律适用）

| 字段 | 处理 |
|---|---|
| `proxies.password` / `password_enc` | 永不出现在任何响应；统一以 `has_password: bool` 表示 |
| 上游代理商账号口令（`LaunchSpec.Upstream`） | 只存在 Go 进程内存与本地 TLSProxy 配置；**不进 API 响应、不进 argv、不进日志、不进审计 detail** |
| `environments.inject_script` | 聚合视图与列表**不返回正文**，只返回 `script_bytes`；详情接口也不返回（M3 注入由 Go 直接喂给 CDP，前端不需要） |
| `devices.proxy_secret_enc` | 现状即 `json:"-"`，保持不变 |
| 店铺财务字段 `profit/cost/balance` | 沿用 IAM `MaskFields` 裁决结果 |
| CDP 端口 / 本地代理端口 | 仅对持 `env:launch` 的调用方、且仅在详情与 runtime 接口返回；两者都只监听 `127.0.0.1` |
| `system/info` | 不返回 DSN / JWT secret / 代理商 token |

---

## 6. M1 / M2 / M3 的接口交付映射

| 里程碑 | 交付接口 | 前端形态 |
|---|---|---|
| M1 | #1 system/info、#2 system/stats；前端只用这两个 + 现有登录 | 浅色启动器骨架、7 个导航、首页静态统计卡 + 引导卡；未批准页面统一空态 |
| M2 | #3–#12、#13–#15、#19–#21 | 店铺列表/新建、代理台账与绑定、分组、日志聚合、设备 runtime 视图 |
| M3 | #16–#18 | 两窗口闭环（真 Chrome），launch 返回真实 pid/cdp_port |

### M1 的 7 个导航（草案，待确认）

1. 工作台（首页：统计卡 + 引导卡）
2. 店铺管理
3. 代理管理
4. 分组管理
5. 设备与网络（runtime 视图）
6. 日志中心
7. 系统设置（`/api/v1/system/info` 的只读展示）

> 现状 `gateway/web/app.js` 的 `SECTIONS` 有 10 项（工作台/店铺/审批/委托/设备/安全/审计/通知/团队/系统）。M1 的重画按上面 7 项收敛，审批/委托/安全/审计/通知/团队暂不进启动器导航（它们是管理台能力，不是「启动器」日常操作面）。**这一点请确认**：是要 7 项里的这 7 个，还是保留部分管理台入口。

---

## 7. 落地注意事项（写代码时必须遵守）

1. **新接口必须同步登记 `gateway/docs.go`**，否则 CI 的 OpenAPI 覆盖测试失败；
2. `POST /api/v1/shops/{id}/environment/launch` 现有响应体含 `upstream`，M3 改造后**必须剔除**；需同步检查 `gateway/env_stop_e2e_test.go`、`gateway/lifecycle2_test.go` 等既有断言是否依赖该字段（当前 grep 显示测试未直接断言 `upstream`，但需在改造时复跑确认）；
3. 所有新 handler 用 `store.WithTenant(r.Context(), p.TenantID)` 访问仓储，禁止裸 `r.Context()`（RLS fail-closed）；
4. mem 驱动也要实现 `ProxyRepo` / `GroupRepo`，否则 `make test` 的 mem 路径直接崩；
5. `PUT /api/v1/shops/{id}/group`（旧）与 `PUT /api/v1/shops/{id}/environment/proxy`（新）并存期间，前者必须双写 `group_id` 与 `group_name`。

---

## 8. 待确认项 → 批复结论（2026-11-01）

| # | 批复 |
|---|---|
| 1 | ✅ M1 导航确定为 7 项：工作台 / 店铺管理 / 代理管理 / 分组管理 / 设备与网络 / 日志中心 / 系统设置。审批、委托、团队、通知中心、独立安全中心、独立审计页**保留后端接口与数据、不删除**，但不进入启动器导航；审计日志与安全事件统一聚合进「日志中心」 |
| 2 | `POST /api/v1/proxies/{id}/test` 归 **M2 尾**（可选），涉及服务端出网，部署侧可禁用；Q3 拍板后于 M2 收尾补齐 `ZINIAO_PROXY_TEST`（默认 on）与可配置目标（T12） |
| 3 | 聚合日志**不做导出**，复用 `audit:export` 的既有能力留在管理台侧 |
| 4 | 环境详情**只返回 `script_bytes`**，不返回注入脚本正文，也不额外返回哈希 |

---

## 9. M2 实施增补与口径差异（2026-11-01 落地记录）

本章记录 M2 相对第 4 章的**新增接口**与**已识别歧义**，供人工确认。

### 9.1 新增接口（M2 批复第 3 节）

| # | 接口 | 权限 | 说明 |
|---|---|---|---|
| 1 | `POST /api/v1/environments` | `shop:create` + `env:create` | 原子创建店铺 + 环境 + profile 元数据，**同一事务**；本期不拉起 Chrome |
| 2 | `GET /api/v1/environments?group_id=&platform=&status=&country=&keyword=&limit=&offset=` | `env:read` | 环境聚合列表（跨店铺，受数据范围约束） |
| 3 | `GET /api/v1/environments/{id}` | `env:read` | 单项聚合视图 |
| 4 | `PUT /api/v1/environments/{id}/proxy` | `env:update` | 绑定 / 解绑手工代理；运行中禁止换出口（409） |
| 5 | `PUT /api/v1/environments/{id}/group` | `env:update` | 环境移动分组；双写 `shops.group_id` + `group_name` |
| 6 | `GET /api/v1/environments/runtimes` | `env:read` | 设备与网络只读台账（**仅数据库侧**，无 PID/CDP/运行时长） |
| 7 | `GET /api/v1/logs?sources=&type=&level=&shop_id=&env_id=&actor_id=&action=&keyword=&from=&to=&include_detail=&limit=&offset=` | `audit:read`（叠加 `security:event.read` / `egress:read`） | 三源聚合 |

### 9.2 已识别歧义（**待人工决策，未自行扩需求**）

| # | 设计文档口径 | M2 批复口径 | 本次实现 |
|---|---|---|---|
| A | `GET /api/v1/devices/runtime`（#19/#20，M2，`device:read`） | 未列出该项，改为 `GET /api/v1/environments/runtimes` | **按 M2 批复实现** `environments/runtimes`（`env:read`）；`devices/runtime` 本期未交付 |
| B | `PUT /api/v1/shops/{id}/environment/proxy`（#16）、`PUT /api/v1/shops/{id}/group`（#12） | 环境中心化路径 | **按 M2 批复实现**环境中心化路径；两个旧接口保持原样可用（并存期双写 `group_id`/`group_name`） |
| C | `PATCH /api/v1/environments/{id}`（#15，改 start_url / country / code） | 未列出 | **本期未实现**（M2 批复「只做以下内容」不含该项） |
| D | 日志过滤用 `sources/category/...` | `env_id / level / type / from / to` | 取并集：`sources` + `category`（`type` 为同义别名）+ `shop_id/env_id/actor_id/action/keyword/from/to/include_detail` |

### 9.3 M1 补查结论（批复第 2 节）

1. **RLS**：`migrations/0012_launcher_core.up.sql` 第 132-142 行为 `proxies` / `groups` 建了 `tenant_isolation` 策略（USING + WITH CHECK，`current_tenant()` 取空即零行 fail-closed）；`groups` 表自带 `tenant_id`；环境分组路径为 `environments → shops.group_id`，服务层 `Groups().Get` 带租户上下文，跨租户分组 ID 一律 404。
2. **`system/info`**：新增 `product_version` / `data_dir` / `server_addr` / `browser{name,path,version}` / `profile_root` / `extension_root` / `ca_ready` 七个扁平字段（原有嵌套 `data.*` / `listen.*` 保留，不破坏 M1 前端）。
3. **`system/stats`**：新增 `environment_total` / `running` / `offline` / `recent[]`（`recent` 按 `last_started_at` → `updated_at` 倒序，不含进程态）。
4. **`browser/argv.go`**：已补参数清单注释，明确上游账号口令只存 Go 进程内存，不进 argv / 日志 / 审计 / 响应。
5. **主题**：`gateway/web/style.css` 为浅色蓝白（`#f4f8fd` / `--brand:#2f6fdb`），全目录仅 `fp-test.html` 保留 `#0b0f1a` 深色（M0 决定原样保留）；`app.js` 的 `SECTIONS` 与批复 7 项逐字一致。

### 9.4 第 9 章与实际接口复核（M2 终审第 3.4 项，2026-11-01）

复核方式：逐条比对 `gateway/server.go` 路由、`gateway/{proxy,group,env,logs}.go` 的 `s.authorize`
与 `gateway/docs.go` 的 OpenAPI 登记。

| 结论 | 说明 |
|---|---|
| 9.1 表中 7 项接口全部已实现，路径与方法一致 | — |
| **偏差 1：已修正** | 9.1 #1 写 `shop:create + env:create`，实现只挂了 `env:create`。已在 `handleEnvCreateShop` 改用 `s.authorizeAll(r, "/environments", "shop:create", "env:create")`，`docs.go` 同步为 `shop:create + env:create`（建环境会顺带建店铺，两个码缺一不可） |
| **偏差 2：已补齐登记** | 9.1 只列了环境/日志，未列代理与分组台账。实际已实现且权限如下：`GET /proxies`=`proxy:read`；`POST/PUT/DELETE /proxies/{id}`、`POST /proxies/{id}/test`=`proxy:manage`；`GET /groups`=`group:read`；`POST/PUT/DELETE /groups/{id}`=`group:manage`；`PUT /environments/{id}/group`=`env:update`；`GET /logs`=`audit:read`（含 security 叠加 `security:event.read`，含 egress 叠加 `egress:read`）。与 `docs.go` 第 112-132 行逐条一致 |
| **偏差 3：已补齐登记（T15）** | `iam.RequiredPage` 补齐 `/proxies`→`page:proxies`、`/groups`→`page:groups`、`/logs`→`page:logs`、`/environments`→`page:environments`（新增 `page:environments` 权限码）。双层口径见 `docs/permission-model.md` 第 2 节：资源接口由资源码拦截，`page:*` 只管导航可见性与纯页面导航（Action 为空的裁决） |
| 9.2 歧义 A/B/C/D | 与现状一致，无变化（`devices/runtime` 未交付、`PATCH /environments/{id}` 未实现、日志过滤取并集） |
| 9.3 补查结论 | 与现状一致，无变化 |

新增测试（本次随复核提交）：`iam/page_nav_test.go`（页面登记 / 双层口径 / 导航可见性）、
`gateway/m2_test.go:TestM2PagePermLayerDoesNotOverrideResourceCode`（cs 无 `page:environments` 仍 200、
finance 403、`/api/v1/me` 的 `nav[]` 随资源码隐藏）。

顺带修掉一个**测试替身与真实依赖语义不一致**的隐患：`platform/store/mem` 的 `Roles().GetByCode`
原先一次遍历并用 `|| TenantID == ""` 兜底，命中哪一条取决于 map 迭代顺序（同一用例反复跑随机失败）；
PG 侧是「先租户自定义、再系统内置」。mem 已改为同样的两趟优先顺序。
