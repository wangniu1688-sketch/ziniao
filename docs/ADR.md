# 架构决策记录（ADR）· Phase 0

每条记录：**决策 / 理由 / 备选 / 影响与 TODO**。

---

## ADR-0001 客户端壳层选 Tauri 2（Rust）+ Chromium 内核定制

- **决策**：`client/` 用 Tauri 2；管理后台面由 WebView 承载 React 应用（与产品形态一致：后台以标签页运行在浏览器壳内）；
  店铺运行面为独立 Chromium 进程，由 Rust 侧做进程隔离与 CDP 控制。
- **理由**：多店铺环境并发开启时，体积与内存是硬指标（Tauri ~10MB vs Electron ~120MB）；Rust 侧便于做
  本地凭证加密（DPAPI / Keychain）与进程树隔离。
- **备选**：Electron（生态成熟、插件 API 齐全，但体积与内存劣势，Node 侧插件沙箱更难）。
- **影响**：需要 Rust 能力；`kernel-patch/` 的构建链路需单独搭建（见 ADR-0003）。
- **TODO**：Phase 1 产出 Tauri 壳与标签页/地址栏/书签栏骨架。

## ADR-0002 后端主语言 Go 1.22

- **决策**：后端 Go（单二进制部署）；网关、计量、CDP 代理、事件派发统一在 Go 内实现。
- **理由**：环境启动/CDP 代理是长连接高并发场景；单二进制降低交付与运维成本。
- **备选**：Java Spring Boot 3（计费/工作流生态成熟，但部署重）。
- **TODO**：若团队主力是 Java，可在 Phase 2 前切换（模块已按目录边界隔离，替换成本可控）。

## ADR-0003 事件总线 = 事务发件箱 + 进程内派发（Kafka 后置）

- **决策**：写路径只做「业务数据 + 事件入 `event_outbox`」**同一事务**，由派发器异步投递给
  audit / notify /（后续）BI、billing 处理器；`Claim` 用 `FOR UPDATE SKIP LOCKED` 支持多实例并发。
- **理由**：Phase 0 目标是「一条命令起」，引入 Kafka/RabbitMQ 会显著抬高启动门槛；
  事务发件箱保证「写了必有事件」这一铁律不依赖外部中间件，后续可平滑替换为 Kafka（处理器接口不变）。
- **备选**：直接上 Kafka（吞吐高、天然多消费者，但本地启动重）。
- **TODO**：Phase 2 引入 Kafka/RabbitMQ 作为传输层，保留 `Handler` 接口不变。

## ADR-0004 口令哈希用 PBKDF2-HMAC-SHA256（stdlib 实现）

- **决策**：`platform/crypto` 自行实现 PBKDF2（210000 迭代、16 字节盐、32 字节输出），
  编码格式 `pbkdf2-sha256$iter$salt$hash`，`VerifyPassword` 返回 `needsRehash` 以支持平滑升级。
- **理由**：保持零外部依赖、可离线运行；算法与参数记录在密文中，便于后续迁移。
- **备选**：argon2id（`golang.org/x/crypto/argon2`，抗 GPU 更强，为标准推荐）。
- **TODO(P1)**：接入 argon2id，登录成功且 `needsRehash` 时静默重哈希。

## ADR-0005 多租户 = 共享库 + 行级 `tenant_id` + PostgreSQL RLS

- **决策**：默认共享库；所有租户域表带 `tenant_id`；开启 RLS 策略，
  会话内 `SELECT set_config('app.tenant_id', $1, true)`；`current_tenant()` 为空时返回零行（fail-closed）。
  企业版保留 `tenants.schema_name` 作为独立 schema 逃生舱（字段已建，路由未实现）。
- **理由**：独立 schema 会导致迁移与运维爆炸；RLS 把「忘了写 `where tenant_id`」从越权漏洞变成运行时报错。
- **备选**：独立 schema（隔离更强，运维成本高）；仅应用层过滤（不可靠）。
- **例外**：登录阶段租户未知，允许 `memberships` 按 `user_id` 查本人所属团队（策略 `login_lookup`，
  仅 SELECT 且驱动强制带 `user_id` 谓词）。
- **TODO**：应用连接需使用 `NOBYPASSRLS` 的独立数据库角色（当前 DSN 未区分 owner/app 角色）。

## ADR-0006 指纹与 TLS 指纹：JS/CDP 注入 + uTLS 代理（P0–P1）

- **决策**：Phase 0–1 不做 Chromium 编译；指纹字段先用 CDP `Page.addScriptToEvaluateOnNewDocument`
  注入（文档创建前），JA3/JA4 用 Go + uTLS 前置代理重建 ClientHello。
- **理由**：真实 Chromium fork 需 depot_tools + 100GB 源码 + 数小时编译，本机不具备；
  uTLS 方案对「服务端可检测的 TLS 指纹」等效，且代理出口本就是产品的一部分。
- **备选**：硬改 BoringSSL（最真实，成本极高，列 P2）。
- **TODO**：`kernel-patch/` 出 patch 集与字段→注入层对照表（见主提示词 Q3 表格）；TLS 基线库
  `fingerprint/tls-baseline/` 按 Chromium 版本维护 JA3/JA4/HTTP2 SETTINGS，客户端启动校验 `baseline_hash`。

## ADR-0007 双存储驱动 memory / postgres

- **决策**：`platform/store` 定义 Repository 契约，`mem` 与 `pg` 两个驱动实现同一套接口；
  `mem` 用于本地启动、单测与演示（本机无 Docker/PG）。
- **理由**：保证「一条命令起」可在任何机器跑通，同时生产路径保持真实 SQL 与 RLS。
- **限制**：`mem` 的 `WithTx` 只提供串行化，不支持回滚；生产必须用 `postgres`。
- **TODO**：补 PG 集成测试（需 PG 环境）。

## ADR-0008 权限裁决结果携带「义务」（obligations）而非仅 allow/deny

- **决策**：`Decision` 除 `Allow` 外返回 `MaskFields`、`DenyElements`、`DataScope`、`Obligations`
  （`require_approval` / `require_mfa` / `watermark`）。
- **理由**：「能看但必须打码」「能做但必须审批」是紫鸟类产品的常态，布尔值不足以表达；
  同时让 DOM 注入层（第三层）与服务端（第二层）用同一份裁决结果，避免两层策略漂移。
- **TODO**：审批工单与水印渲染在 Phase 3 落地（当前导出未携带审批单号返回 202 + `approval_required`）。

---

## ADR-0009 指纹三层：JS 注入 + CDP 注入 + uTLS 前置代理（Chromium fork 后置）

- **状态**：Accepted（Phase 1）
- **决策**：
  1. JS 层覆盖 `navigator / screen / canvas / webgl / audio / fonts`（`fingerprint.InjectionScript`）；
  2. CDP 层在**文档创建前**注入同一份脚本，并改写网络层请求头；
  3. 协议层由 Go 侧 uTLS 前置代理发出与内核版本严格对应的 ClientHello（JA3/JA4/HTTP2）；
  4. Chromium C++ patch 与 BoringSSL 改造列为 P1/P2（TODO），本阶段不实现。
- **理由**：协议层指纹（TLS）是「改不了就必死」的致命项，必须优先保证与声称内核同源；
  而 JS/CDP 层足以覆盖绝大多数平台侧检测，投入产出比高于 fork 内核。
- **代价**：无法覆盖极少数深入内核的检测（如 C++ 侧 navigator 实现细节）。
- **红线**：注入值必须与服务端画像**逐字一致**，客户端禁止"随机化"——随机化即与画像库脱钩。

## ADR-0010 设备并发用「双向配额」而非「独占互斥锁」

- **状态**：Accepted（Phase 1）
- **决策**：一台设备绑定账号数上限 `max_bound_accounts`（默认 20）；同时使用人数上限
  `max_concurrent`（默认 6，**按 user_id 去重，一人多店只占 1 个槽**）；
  软阈值命中 = 允许 + 告警 + 审计；硬阈值命中 = 拒绝 + 引导切换备用设备。
- **理由**：紫鸟类产品的典型场景是「一店多人」与「一人多店」，互斥锁会把正常协作卡死（反例 4）。
- **替代方案**：设备级互斥锁（已否决）、无限并发（已否决，会导致 IP 关联封号）。

## ADR-0011 密钥管理：本地 KEK（SOPS/age）→ 云 KMS，信封加密 + 版本化

- **状态**：Accepted（Phase 1）
- **决策**：`platform/crypto` 定义 `KeyManager` 抽象（`Wrap`/`Unwrap`），默认实现 `LocalKEK`
  （32 字节主密钥，按 `key_version` 管理）；凭证走信封加密：`v<ver>.<wrapped_dek>.<ct>`。
  dev 环境允许随机 KEK，但必须打 WARN 日志（重启后历史密文不可解）。
- **理由**：直接用单一密钥加密业务数据无法实现「轮换不动业务」；信封加密让轮换只换封皮。
- **TODO**：生产接入 KMS（env 注入 `ZINIAO_KEK_V1`）与 SOPS/age 密钥文件；替换为 argond KMS 客户端时
  业务代码零改动（只依赖 `Wrap/Unwrap`）。

## ADR-0012 TLS 基线由 uTLS 真实 ClientHello 计算并回写，未校准标记 `unverified`

- **状态**：Accepted（Phase 1）
- **决策**：基线库 `fingerprint/data/tls_baseline.json` 保存「目标值」；服务启动用 uTLS
  按 `HelloID` 构造真实 ClientHello 计算 JA3/JA4 并 `RegisterComputed` 回写，
  `GET /api/v1/fingerprint/tls-baselines` 同时暴露 `computed` 与 `source`。
- **理由**：L10 禁止臆造数值——不能声称"我们的 JA3 等于 Chrome 131"却拿不出计算依据；
  「计算值」与「目标值」是否一致必须可验证、可追溯。
- **TODO**：接入真实 Chrome 采集器校准（`source: verified`），当前为 `unverified`。

## ADR-0013 高危动作走审批工单，且「申请人与审批人必须不同人」

- **状态**：Accepted（Phase 2）
- **决策**：`audit.export.plain` / `shop.credential.reveal` / `shop.bulk_delete` / `device.purchase`
  四类动作必须持有**已通过**的审批单；审批单带 TTL（默认 1800s）、一次性核销（`used`）、
  校验「类型 + 申请人 + 租户 + 未过期」四要素；审批人 ≠ 申请人（服务层硬校验，不只是前端灰按钮）。
- **理由**：反例 8「只灰按钮不算控制」——前端灰掉按钮，绕过 UI 直接调 API 就拿到了明文；
  自己批自己等于没有审批（L3 职责分离）。
- **代价**：增加一次往返，需运营配置审批人路由（TODO 第 5 条）。
- **红线**：审批单核销后不可复用；换人使用、类型不匹配一律拒绝。

## ADR-0014 定时任务以「系统身份」执行但必须留审计 + 可单步触发

- **状态**：Accepted（Phase 2）
- **决策**：`jobs` 包提供 `Job{Name/Interval/Run}` 与内存调度器；任务按 `Interval` 节流；
  `RunOnce` 供单测/CLI/手动触发；设备巡检、回收站清理、审批单过期三类任务以 `system` 身份写审计，
  并带 `Terminal.Mode=job` 标记来源。
- **理由**：自动化同样受 L5 约束（"自动化不是权限旁路"是 L3 的延伸）——定时任务绕过权限引擎，
  但绕过审计是不允许的；只等定时触发导致无法自测，因此必须可单步执行。
- **代价**：内存调度器不具备分布式能力（多实例会重复执行）。
- **解锁**：替换为分布式调度（如 `robfig/cron` + 选主）时接口不变。
