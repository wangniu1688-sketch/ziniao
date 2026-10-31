# 紫鸟浏览器仿制 · Phase 0 地基

跨境电商多账号安全管理系统（Anti-Detect Browser + 多店铺管理 SaaS + 团队协作平台）的 Phase 0 交付：
**monorepo 骨架 + 数据库 schema + 事件总线 + iam.PolicyEngine + 多租户隔离 + identity + 审计写入管道 + 通知中心雏形**。

验收口径：**能建租户 → 建用户 → 授角色 → 跑通一次带审计的写操作**。

---

## 1. 一条命令启动

### 方案 A：零依赖（推荐，本机无需 Docker / PostgreSQL）
$env:Path = "C:\Users\Administrator\tools\go\bin;" + $env:Path
cd g:\ziniao; go run ./cmd/server -store memory -seed




```powershell
# Windows PowerShell
cd g:\ziniao
$env:Path = "C:\Users\Administrator\tools\go\bin;" + $env:Path   # 若 go 不在 PATH
go run ./cmd/server -store memory -seed
```

```bash
# macOS / Linux
make dev-memory     # 等价于：go run ./cmd/server -store memory -seed
```

启动后：

- 服务地址：`http://127.0.0.1:8080`
- OpenAPI：`http://127.0.0.1:8080/openapi.json`
- 健康检查：`http://127.0.0.1:8080/healthz`
- 演示账号（`Zn@12345678`）：`demo`（老板）/ `finance`（财务）/ `operator`（运营）

### 方案 B：PostgreSQL（生产形态，含 RLS）

```bash
docker compose up -d          # 起 postgres + redis
make migrate                  # 执行 migrations/0001_phase0_core.up.sql
make dev                      # go run ./cmd/server -store postgres -seed
```

> 说明：本机（Windows）未安装 Docker 与 PostgreSQL，方案 B 的 SQL 与 pg 驱动为**编译期验证**，
> 集成测试待有 PG 环境时补充，见 `docs/TODO-phase0.md`。

---

## 2. 验收演示（30 秒）

```bash
./scripts/demo.sh          # macOS / Linux
pwsh ./scripts/demo.ps1    # Windows
```

手工等价流程：

```bash
# 1) 注册团队（含主账号 + owner 角色 + 订阅）
curl -X POST localhost:8080/api/v1/auth/register -H 'Content-Type: application/json' \
  -d '{"name":"验收团队","slug":"acme","plan":"pro","username":"boss","password":"Zn@12345678","nickname":"老板老王"}'

# 2) 登录拿 token
TOKEN=$(curl -s -X POST localhost:8080/api/v1/auth/login -H 'Content-Type: application/json' \
  -d '{"username":"boss","password":"Zn@12345678"}' | python -c "import sys,json;print(json.load(sys.stdin)['token'])")

# 3) 一次写操作：新建店铺（权限裁决 → 配额校验 → 事务写 + 计量 + 事件投递）
curl -X POST localhost:8080/api/v1/shops -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -d '{"name":"亚马逊美国站","platform":"Amazon","site":"US","url":"/shops"}'

# 4) 审计日志：这次写操作必然落一条带哈希链的记录
curl "localhost:8080/api/v1/audit/logs?action=shop.create" -H "Authorization: Bearer $TOKEN"

# 5) 哈希链校验（篡改/删除任一条都会失败）
curl localhost:8080/api/v1/audit/verify -H "Authorization: Bearer $TOKEN"

# 6) 财务角色看店铺：profit/cost/balance 返回 ***（服务端已遮蔽，不只是前端隐藏）
curl localhost:8080/api/v1/shops -H "Authorization: Bearer $FINANCE_TOKEN"

# 7) DOM 注入层策略（字段遮蔽 + 元素屏蔽 + 水印）
curl "localhost:8080/api/v1/iam/dom-policy?path=/finance" -H "Authorization: Bearer $FINANCE_TOKEN"
```

---

## 3. 目录结构（模块边界即目录边界）

```
client/            Tauri 2 桌面客户端骨架（Rust 外壳 + JS 注入脚本；本机无 Rust 链，未编译验证）
fingerprint/       设备画像模板库 + 加权采样 + 自洽性体检 + 漂移监控 + TLS 基线库 + 注入计划
network/           代理拨号 + uTLS 前置代理 + IP 纯净度四维检测 + 健康评分 + 资源调度 + 代理商适配
approval/          审批工单：高危动作二次审批，职责分离 + TTL + 一次性核销
jobs/              定时任务：设备巡检 / 回收站清理 / 审批单过期（系统身份 + 留审计）
device/            设备池/自有设备/紫鸟盒子/云手机、绑定与占用（配额模型 L9）、续费
security/          访问策略（页面/元素/动作）事前拦截 + 事中告警 + 事后追溯
gateway/           API 网关：鉴权、租户隔离、限流、路由、OpenAPI
identity/          身份认证、会话、登录保护
iam/               权限引擎：功能/页面/元素/数据/资源 五层裁决
tenant/            多租户与团队：成员、角色授予、临时授权
billing/           计量（Meter）与配额（Entitlement）
eventbus/          统一事件总线（事务发件箱 + 派发器）
audit/             六大日志 + 哈希链 + 差异化留存 + 导出管控
notify/            通知中心（站内 + 通道驱动）
shop/              店铺实体 + 生命周期状态机 + 浏览器环境 + 账密托管（信封加密）
platform/          共享内核：config / apierr / crypto(信封加密) / ids / store / openapi / migrate
app/               组合根：装配全部模块（服务端与测试共用）
migrations/        SQL 迁移（0001 核心 / 0002 设备与环境 / 0003 审批工单 / 0004 环境启动，均可回滚）
docs/              ADR、权限矩阵、计费规则、未实现项清单
```

---

## 4. 关键设计（一句话版）

| 铁律 | 落地方式 |
|---|---|
| L3 自动化不得成为权限旁路 | API/RPA/CLI/Agent 都走 `identity.Login` + `iam.PolicyEngine`，以**发起人**身份裁决 |
| L4 敏感数据不落明文 | 口令 PBKDF2 密文 + `key_version`；TOTP 种子不进 API 响应；错误响应不含内部细节 |
| L5 所有写操作落审计 | 业务写 + 事件入发件箱**同一事务**，异步派发到 audit/notify sink |
| L6 权限三层同时执行 | ① 网关 `authorize` ② 服务层 `PolicyEngine.Check` ③ `GET /iam/dom-policy` 下发 DOM 注入策略 |
| L9 设备是配额不是互斥锁 | `device.max_concurrent_users=6`（配置中心），未实现调度器时已预留配置项 |
| L10 不臆造数值 | 所有限值集中在 `platform/config`（6 人 / 180 天 / 50000 行 / 68 元 等） |
| L12 多租户隔离 | `tenant_id` 全表覆盖 + PostgreSQL RLS（`app.tenant_id` 未设置 → 零行，fail-closed） |

---

## 5. 环境变量

| 变量 | 默认值 | 说明 |
|---|---|---|
| `ZINIAO_STORE_DRIVER` | `memory` | `memory` \| `postgres` |
| `ZINIAO_POSTGRES_DSN` | `postgres://ziniao:ziniao@localhost:5432/ziniao?sslmode=disable` | PG 连接串 |
| `ZINIAO_HTTP_ADDR` | `:8080` | 监听地址 |
| `ZINIAO_JWT_SECRET` | dev-only | 生产必须覆盖 |
| `ZINIAO_LIMIT_DEVICE_CONCURRENT_USERS` | `6` | 设备同时使用人数软阈值 |
| `ZINIAO_LIMIT_AUDIT_RETENTION_DAYS` | `180` | 浏览类审计留存 |
| `ZINIAO_LIMIT_EXPORT_MAX_ROWS` | `50000` | 导出行上限 |

---

## 6. 测试

```bash
go test ./...        # 单测 + HTTP 端到端（httptest，无需外部依赖）
go vet ./...
```

覆盖：权限矩阵（表驱动）、字段级遮蔽、跨租户隔离、配额与 entitlement、哈希链防篡改、
口令/JWT/TOTP、一次写操作必产生审计记录的端到端链路、OpenAPI 完整性。

---

## 8. Phase 1：指纹 / 设备 / 环境 / 安全中心

### 8.1 指纹三层对照（谁负责什么）

| 层 | 手段 | 可检测性 | 本仓库实现 |
|---|---|---|---|
| JS 层 | 覆盖 `navigator/canvas/webgl/audio/fonts` | low（易被反作弊读取 getter） | `fingerprint.InjectionScript` + `client/inject/fingerprint.js` |
| CDP 层 | `Page.addScriptToEvaluateOnNewDocument`、网络与请求头改写 | medium | 注入计划 `plan.script`（服务端生成，客户端执行） |
| 协议层 | uTLS 前置代理（JA3/JA4/HTTP2 与内核基线同源） | high（改不了就必死） | `network.TLSProxy` + `fingerprint/data/tls_baseline.json` |

自洽性体检规则 9 条（`fingerprint/consistency.go`）：UA↔OS、platform↔OS、时区↔国家、
语言↔国家、GPU↔OS、DPR↔OS、核数↔内存档位、字体↔OS 标志、插件一致性。
**内置模板全部通过体检；任一关键字段被篡改都会被表驱动测试检出**。

TLS 基线由 uTLS 真实构造的 ClientHello 计算后回写（`app.wirePhase1`），
因此"服务端基线"与"代理实际发出的指纹"同源可比。

**UA↔TLS 自洽闸门（关键）**：uTLS v1.6.7 没有 `HelloChrome_131`，131 只能复用
`HelloChrome_120_PQ`——即实际发出的是 120 的握手。若 UA 仍声称 `Chrome/131`，
平台把 UA 与 JA4 交叉一比就能看穿。因此 `shop.CreateEnv` 会用
`ParseChromeMajorFromHelloID` 认出 hello_id 实际代表的版本，并把 UA 的 Chrome 版本
一并对齐（原则：**宁可用低版本但完全自洽，也不要高版本混搭**）。

| 基线 `source` | 含义 |
|---|---|
| `fallback` | 该版本没有独立 uTLS 基线，复用低版本的 ClientHello（131→120），UA 已同步对齐 |
| `unverified` | uTLS 构造已与本机真实 Chromium 静态构成对齐实测一致，但缺真实同版本 Chrome 对照 |

本机实测：真实 Chromium（Edge 87）对 raw 探针的 ClientHello 与 uTLS `HelloChrome_120`
发出的字节在 JA3 四字段上完全一致，逐连接差异仅来自随机 GREASE ⇒ **JA3 会逐连接变化，
一致性判定以 JA4 为准**。真实 120/131 的偏差待抓真实版本后校准（见 TODO 2D）。

### 8.2 设备是配额不是互斥锁（L9）

```go
// 一设备可绑多账号 → 配额；同一用户开多店 → 只占 1 个并发槽；
// 超过软阈值（6）允许但告警；超过硬阈值才拒绝并引导切换设备。
bound >= cfg.DeviceMaxBoundAccounts   → 402 quota_exceeded
concurrent >= cfg.DeviceHardLimitUsers → 409 conflict
```

### 8.3 环境自检报告（M0.3）

`POST /api/v1/shops/{id}/environment/check` 返回：

```json
{ "consistency": {"score":100,"threshold":85,"issues":[],"unverified":["ja3-match"]},
  "drift": {"score":100,"risk":"low","items":[]},
  "baseline_checked": true, "baseline_match": true,
  "tls_match": true, "score": 100, "passed": true }
```

离线启动必须上报 `baseline_hash`，不一致即禁止启动（L8）。

### 8.4 账密托管（Q10 信封加密）

KEK（按版本）→ DEK（每条数据一次）→ 数据密文，密文格式 `v<ver>.<wrapped_dek>.<ct>`：

```go
cipher.Seal(plain)  // 写入
cipher.Open(enc)    // 按需解密（需 shop:credential.reveal，审批义务 + 高危告警）
cipher.Rekey(enc)   // 密钥轮换：只换封皮，明文不动
```
审计只记元信息（kind / key_version / username），**绝不明文或密文**。

### 8.5 安全中心三层（M14）

- **事前**：访问策略（页面级 / 元素级 / 高危动作），模板一键套用，`POST /security/intercept` 判定；
- **事中**：命中即写 `security_events` 并经事件总线告警（软阈值超限、凭证明文查看、设备健康恶化）；
- **事后**：`GET /security/events` + 审计日志同源追溯。

## 8.5 Web 控制台（无需 Node，服务端直接托管）

启动后浏览器打开 `http://127.0.0.1:8080`（不再是 404），登录 `demo / Zn@12345678`：

| 菜单 | 能力 |
|---|---|
| 工作台 | 店铺/设备/待审批/安全事件统计 + 审计哈希链校验 |
| 店铺管理 | 建店、状态机迁移、回收站/恢复/彻底删除、**批量导入（207 部分成功）**、凭证托管（写密文 + 审批后看明文） |
| 审批工单 | 提交 / 通过 / 拒绝 / 撤销，自己批自己会被 403（职责分离） |
| 设备与网络 | 购买云网络设备、绑定店铺（配额模型）、续费 |
| 安全中心 | 策略模板一键套用、安全事件追溯 |
| 审计日志 | 查询、哈希链校验、**带水印 CSV 导出（审批联动）** |
| 团队与成员 | 邀请成员、授予角色、权限矩阵 |
| 系统管理 | 定时任务手动触发、TLS 基线实算一致性、OpenAPI 入口 |

实现：`gateway/web/`（原生 HTML/JS/CSS，`go:embed` 打进二进制），权限全部由服务端裁决，
无权限的操作点击即收到 403——前端只做展示分组，不做权限判断（与反例 8 同一原则）。

---

## 9. Phase 2：审批工单 / 定时任务 / 批量导入

### 9.1 二次审批不是"灰按钮"（反例 8）

```
GET /api/v1/shops/{id}/credentials/password            → 202 approval_required（服务端硬拦）
POST /api/v1/approvals        {type:"shop.credential.reveal", ...}   → 201 pending
POST /api/v1/approvals/{id}/approve  （申请人本人）      → 403 职责分离
POST /api/v1/approvals/{id}/approve  （他人）            → 200 approved
GET  ...?ticket=<id>                                    → 200 明文（同时核销）
GET  ...?ticket=<id>  （第二次）                          → 202 已核销，需重新申请
```

审批单校验四要素：**类型 + 申请人本人 + 同租户 + 未过期（TTL 默认 1800s）**，
通过后**一次性核销**（`used`）。需要审批的动作由配置中心
`Limits.ApprovalRequiredActions` 决定，不硬编码（L10）。

### 9.2 导出水印

审计明文导出在 CSV 头部写入水印注释行：

```
# 本文件为审计日志导出，含水印，禁止外传
# 租户=t1 导出人=demo(u1) 审批单=01H...  导出时间=2026-10-30T16:00:00Z
id,category,actor,action,result,shop_id,ip,url,occurred_at,hash
```

### 9.3 定时任务（自动化同样受 L5 约束）

| 任务 | 周期 | 行为 |
|---|---|---|
| `device.inspect` | 6h | 临期（≤7d）high 告警 + 健康分 <60 medium 告警 |
| `shop.trash_purge` | 24h | 回收站超留存期（30d）彻底删除 |
| `approval.expire` | 10m | 超时未决审批单置为 `expired` |

任务以 `system` 身份写审计，`Terminal.Mode=job` 标记来源；`POST /api/v1/jobs/run`
可手动触发（不必等定时），`Jobs` 按 `Interval` 节流避免重复告警。

### 9.4 批量导入 / 批量删除

**导入**（`POST /api/v1/shops/batch/import`）支持两种提交方式：

| 方式 | 用法 |
|---|---|
| 上传 CSV | `multipart/form-data`，字段名 `file`（表头 `name,platform,site,group`，顺序任意；支持无表头文件；兼容 Excel 导出的 BOM） |
| JSON | `{"rows":[{...}]}`（脚本调用用这个） |

```http
GET  /api/v1/shops/batch/template            → CSV 模板（带 BOM，Excel 打开不乱码）
POST /api/v1/shops/batch/import?dry_run=1    → 只体检：报出脏行，不写任何数据
POST /api/v1/shops/batch/import              → 逐行走权限 + 配额 + 审计；部分成功返回 **207**
```

行级失败不牵连整批——客户给的表从来就不干净（空行、缺名称、文件内重名），
"有一行脏数据就让整份文件作废"是最容易被运营骂的实现。失败行计入 `total`，
不会让人以为那些行被静默丢掉了。单次上限 1000 行，超了整份拒绝。

**删除**（`POST /api/v1/shops/batch/delete`）：一次勾删几十家时人工逐个确认已失去意义，
能真正拦住误操作的是**事前那张审批单**——须持已通过的 `shop.bulk_delete` 单据，
执行后单据即核销，一张单不能清两次库。

---

### 9.5 桌面客户端（浏览器）✅ 已可运行

双击 `client\src-tauri\target\release\bundle\nsis\紫鸟浏览器_0.2.0_x64-setup.exe` 安装，
或直接运行 `client\src-tauri\target\release\ziniao-client.exe`。

**这就是"浏览器"本体**：登录服务端 → 店铺列表 → 点「启动环境」→
弹出一个**独立浏览器窗口**，与其它环境完全隔离：

| 能力 | 实现 |
|---|---|
| 指纹注入 | 服务端下发启动单（UA/时区/Canvas/WebGL/字体/插件），窗口在**文档创建前**原样注入 |
| 账号隔离 | 每环境独立 `profiles/<env_id>` 目录（Cookie/localStorage/IndexedDB 物理隔离） |
| 内核一致性 | 窗口 UA 与注入画像同源，标题栏显示 内核版本/地区/代理状态 |
| 出口通道 | 每 environment 拉起一个独立 `ziniao-proxy` 进程：HTTP CONNECT → **MITM**（本地 CA 动态签发目标证书）+ 对目标 **uTLS 重建 ClientHello**；找不到该进程如实显示"直连"而不伪造 TLS |
| HTTP/2 | ⚠️ 现阶段**只对目标协商 HTTP/1.1**：h2 头部是 HPACK 二进制帧，无法字节改写且会导致链路不可用。留 `TLSProxyConfig.HTTP2` 开关，真正的 h2 支持需 HPACK 层改写或 fork Chromium |
| **出口 IP 隔离** ✅ | 服务端资源池按环境分配**独立**住宅出口，凭据随**启动单**逐环境下发（`-ip-base-url` + `ZINIAO_IP_TOKEN`）。实测两个窗口流量零交叉；分配失败直接拒绝启动，不退回直连 |

### 接入住宅代理商（多账号防关联的命脉）

```bat
set ZINIAO_IP_TOKEN=你的代理商密钥          :: 不进 argv，wmic 读不到
ziniao-server.exe -ip-base-url https://api.你的代理商.com ^
                  -ip-acquire-path /api/v1/proxies/acquire ^
                  -ip-release-path /api/v1/proxies/release ^
                  -ip-auth bearer -ip-protocol socks5
```

厂商差异（接口路径 / 鉴权 / 字段名 host|ip|ip_address、port|http_port|socks_port…）
由 `network.VendorConfig` + 宽松解析兜住，换厂商改参数即可。
**不购买也行**：不配 `-ip-base-url` 时环境仍可启动，但所有店铺共用本机公网出口，
不具备多账号隔离能力——服务启动日志会对此告警。
| 版本自洽 | 环境声称的 Chrome 版本、TLS 基线、`navigator.userAgent`、**请求头 sec-ch-ua** 四者同源：uTLS 缺位的版本会把 UA 一并回落（`shop.CreateEnv`），请求头由 sidecar 在明文处改写（`network/header_rewrite.go`，身份来自 `ZINIAO_ENV_UA`） |
| HTTP/2 | ⚠️ 现阶段**只对目标协商 HTTP/1.1**：h2 头部是 HPACK 二进制帧，无法字节改写且会导致链路不可用。留 `TLSProxyConfig.HTTP2` 开关，真正的 h2 支持需 HPACK 层改写或 fork Chromium |
| 上网 IP | ⚠️ 本机只有一个公网出口，**多个环境共用同一 IP** ⇒ 平台仍可据此关联账号。IP 隔离必须接入住宅/ISP 代理（填 `--upstream-host/port/user/pass`，口令走环境变量 `ZINIAO_UPSTREAM_*` 而非 argv） |
| 权限裁决 | `env:launch` 由服务端裁决（L3），启动写审计 `env.launched`（L5） |

肉眼验证：环境窗口默认打开 `/fp-test` 指纹实测页——开两个不同店铺的环境，
UA/时区/屏幕/WebGL/Canvas 哈希应互不相同；同一环境反复开关应完全一致（确定性，非随机）。

无人值守验证（自动化用）：
`ziniao-client.exe --server http://127.0.0.1:8080 --token <jwt> --launch-env <shopID>`（可重复）

一键重建：`scripts\build-client-win.bat`。
工具链全装在 D 盘（Rust `D:\rust-home`、VS2019 Build Tools `D:\VS2019`），
详见 [`docs/build-client.md`](docs/build-client.md)。

---

## 10. 已知限制

见 [`docs/TODO-phase0.md`](docs/TODO-phase0.md)、[`docs/TODO-phase1.md`](docs/TODO-phase1.md)
与 [`docs/TODO-phase2.md`](docs/TODO-phase2.md)。最重要三条：

1. **Chromium 内核与 TLS 指纹未实现**：本阶段按你的选择走「JS/CDP 注入 + uTLS 代理」路线，
   代码骨架与配置项已就位，C++ patch 与 BoringSSL 改造列为 Phase 1/2 TODO。
2. **PostgreSQL 驱动未做集成测试**：本机无 PG/Docker，SQL 与驱动为编译期验证。
3. **内存驱动不支持事务回滚**：仅 dev/test 使用，生产必须走 `postgres`。
