# Phase 2 未实现项清单（逐条登记，无静默省略）

> 原则：**不能做的必须写明，不能靠"省略"混过去**。以下每条都给出影响、原因与解锁条件。

## 1. Chromium fork（C++ 指纹注入）与 BoringSSL 改造 —— 未实现

- **状态**：P1/P2，本阶段仍未编码（沿用 JS/CDP + uTLS 前置代理替代方案，ADR-0009）
- **影响**：无法覆盖极少数深入内核的检测
- **原因**：本机无构建环境（Chromium 源码树 + Linux + 数小时编译）
- **解锁**：Linux 构建机 + depot_tools；预估 20+ 人日

## 2. ~~Tauri/Rust 客户端未编译验证~~ → ✅ 已编译并可运行（Phase 2B）

- **状态**：已在 Win10 1909 本机用 VS2019 Build Tools + Rust 1.99.1 构建出
  NSIS 安装包并实跑通过；环境窗口（隔离 profile + 文档前注入）已验证——
  两个不同环境 UA/时区/屏幕/WebGL/Canvas 全不相同，见 `docs/build-client.md`。

## 2B. ~~uTLS 本地前置代理 sidecar~~ → ✅ 已交付（`cmd/ziniao-proxy`，MITM 架构）

- **状态**：每环境一个独立进程，**本地 CA + MITM**——对客户端动态签发目标证书
  （伪装成目标站点），对目标用 uTLS 重建 ClientHello（按环境声称的内核版本）。
  实测：环境窗口经 sidecar → 上游代理 → 真实互联网（百度完整加载）；
  curl 经 sidecar 访问 baidu = 200。
- **关键教训（反例归档）**：第一版"对上游先 uTLS 握手再透传客户端字节"是
  **双重 TLS**（客户端 ClientHello 被塞进已建立的 TLS 连接再加密一次），
  目标端必然 `ERR_SSL_PROTOCOL_ERROR`。TLS 隧道里替换 ClientHello 只有
  终结 TLS 一条路 ⇒ 本地根证书 + 动态签发是指纹浏览器的标准形态。
- **凭据处理**：上游代理口令不进 argv（wmic 可读）、不落盘，走 `ZINIAO_UPSTREAM_*`
  环境变量由客户端透传给 sidecar。
- **仍缺**：真实住宅/ISP 代理商接入（`network.Provider` 接口就绪，缺 API 实现）——
  现在可以买任意 HTTP/SOCKS5 代理填 `ZINIAO_UPSTREAM_*` 立即用；
  CA 自动安装已用 `certutil -user`（用户级），其它平台 keychain/NSS TODO。

## 2E. ~~UA-CH（sec-ch-ua）伪装~~ → ✅ 已补齐（`network/header_rewrite.go` + 注入脚本）

- **JS 层**：注入脚本原先只在 `navigator.userAgentData` **不存在**时才伪造，而现代 Chromium
  本来就有该值 ⇒ 保留了 WebView2 真实版本 156，与伪装 UA(120) 自相矛盾。已改为无条件覆盖，
  并补齐 `getHighEntropyValues` / `toJSON`（brands 与 UA 同源）。
- **网络层**：请求头里的 sec-ch-ua 是浏览器自动加的，JS 碰不到。新增 `network/header_rewrite.go`，
  在 MITM 的明文段改写 `User-Agent` 与 sec-ch-ua 三件套（成套补齐，缺一件本身就是异常信号）。
  身份由 `ZINIAO_ENV_UA` 注入；keep-alive 多请求、Content-Length/chunked 分帧均有测试覆盖。
- **已知限制（写入此处的都是实测确认的）**：
  - 只对目标协商 **HTTP/1.1**（`TLSProxyConfig.HTTP2`，默认 false）。
    h2 的头部是 HPACK 二进制帧，无法字节改写；且在 Verified 之前先把 h2 站点打通优先。
  - 要同时保留 HTTP/2 指纹 + 改写请求头，需做 HPACK 层解码/编码，或 fork Chromium（第 8 条）。
- **实测**：`httpbin.org` 返回的 UA 为 `Chrome/120.0.6099.109`；本地 h2 目标站点 `code=000 → 200`。

## 2D. TLS 基线校准（部分完成，仍标 unverified）

- **已做的真实校准**：本机真实 Chromium（Edge 87）对本地 raw 探针握手 vs uTLS
  `HelloChrome_120` 发出的字节 → JA3 四个字段**完全一致**
  （version 771 / 15 套件 / 14 扩展 `23,65281,10,11,35,16,5,13,18,51,45,43,27,21` / 曲线 29-23-24），
  逐连接差异仅来自随机 GREASE ⇒ 静态构成与真实 Chromium 可确认对齐，JA3 逐连接变化、以 JA4 为准。
- **仍未闭环**：本机没有真实 Chrome **120 / 131** 可对照，这两条对真实版本的偏差仍 `unverified`。
  升级 uTLS 或抓真实版本后必须重新校准（ADR-0006）。
- **顺手修掉的致命不自洽**：uTLS v1.6.7 无 `HelloChrome_131`，131 只能复用 `HelloChrome_120_PQ`；
  此前 UA 仍声称 `Chrome/131` 而握手发出的是 120 ⇒ 平台交叉比对 UA 与 JA4 即可识破。
  现由 `shop.CreateEnv` 用 `ParseChromeMajorFromHelloID` 认出实际版本，
  并把 UA 的 Chrome 版本一并对齐（**宁可用低版本但完全自洽，也不要高版本混搭**），
  `fingerprint/tls_test.go` 固化了「回落条目必须有真实条目可对齐」的数据不变量。

## 2C. ~~账号隔离实测~~ → ✅ 已验证（`/demo/*` 隔离实测站点，`-demo-site` 开启）

- **方法**：同域名同路径下，两个环境分别登录不同账号 → 各自重开 → 读回服务端
  真实收到的 Cookie；反向对照（同一环境换账号立即变化）证明检测方法灵敏。
- **结果**：T1(US)=seller_a、T2(GB)=seller_b，重启客户端后各自保持；
  同环境切换立即生效（T3/T4），另一环境不受影响（T5）。
  Cookie/localStorage 物理隔离（每环境独立 `profiles/<env_id>`）成立。

## 3. TLS 基线未与真实 Chrome 抓包校准 —— 已标记 `unverified`

- **状态**：JA3/JA4 由 uTLS 真实 ClientHello 计算（与代理同源可比），但未与真实 Chrome 逐项比对
- **暴露方式**：自检报告 `consistency.unverified: ["ja3-match"]`、基线 `source=unverified`
- **解锁**：采集真实 Chrome 115/120/131 覆盖 `fingerprint/data/tls_baseline.json`

## 4. ~~PostgreSQL 未实跑（含新增的 0003 迁移）~~ → ✅ 已实跑（PostgreSQL 16.15）

- **实跑方式**：本机安装 PostgreSQL **16.15**（与 `docker-compose.yml` 的 `postgres:16-alpine`
  对齐，方便以后换成容器不用改 expectations）→
  `go run ./cmd/server -store postgres -migrate -seed`
- **已验证**：迁移 0001/0002/**0003/0004** 全部执行成功；种子数据写入；登录后
  建单/审批/核销/批量删除/审计导出（含水印）/通知/定时任务手动触发全链路跑通；
  `tenantRepo.List` 依赖的 `admin_list` 策略正常放行定时任务。
- **本次因此修掉的 4 个 pg 路径缺陷**（mem 实现全部测不出来）。区分「怎么发现的」，
  是因为前两个是实跑撞出来的、后两个是接上库之前的代码审查先排掉的，不该混为一谈：

  | 缺陷 | 怎么发现的 | 现象 / 根因 |
  |---|---|---|
  | 内置角色写不进去 | **实跑撞出** | `not-null violation: roles.mask_fields`。Go 的 nil 切片被编成 SQL NULL，而列的 `DEFAULT '{}'` 只在**省略该列**时生效；除 finance 外所有内置角色的 MaskFields 都是 nil |
  | **任何授权都失败** | **实跑撞出** | seed 报「角色不存在: finance」。pg 的 `roles.GetByCode` 用租户完全相等匹配，取不到系统级角色，而 mem 允许回退 ⇒ 单测全绿、生产完全不可用 |
  | 审批列表分页错误 | **接库前审查发现，随即实跑验证** | 分页误用了占位符计数器 `n`，无筛选条件时会拼出 `LIMIT 2 OFFSET 3` 且多出两个无占位符的参数 |
  | 其它 `text[] NOT NULL` 列 | **防御性归一** | `shops.tags` / `notifications.channels` 同属「nil 切片变 NULL」，与第一条同源，一并加 `strArr` 归一化 |

- **结论**：`mem` 与 `pg` 的语义差异是本类缺陷的温床。凡是「租户级 + 系统级」混合的数据
  （角色、策略模板），两边的取值口径必须逐条对齐。

## 5. ~~审批工单的「审批人路由规则」~~ → ✅ 已实现（`approval/router.go`）

- **规则表驱动**：不同单据要求不同审批级别，而不是"有权限就行"：

  | 单据 | 要求 | 理由 |
  |---|---|---|
  | 明文导出全量审计 | owner | 一次性带出**全团队**操作记录，影响面最大 |
  | 揭示店铺主账号口令 | owner / ops_manager | 泄露即账号被接管，无回滚余地 |
  | 批量删除 | owner / ops_manager | 影响不可逆 |
  | 设备申购 ≥ 阈值 | **owner 双人签署** | 大额申购是最容易滋生套利的地方 |
  | 其它 | owner / ops_manager | 管理员级兜底 |

- **金额路由**：申购金额超阈值上收老板并触发双人复核。单据需给出 `Detail.amount`，缺失按低档处理。
- **会签引擎**：已实现且**已接上规则**（大额申购 quorum=2）。第一人签署后仍 pending，
  达到法定人数才 approved；同一人重复签署不累计票数（防一人刷够票）。
- **为什么不把「揭示口令」收成 owner-only**：老板自己申请时若只允许老板批，团队里没人能批，
  流程直接死锁。故保留 ops_manager，把最高级别留给影响全团队的审计导出。
- **实测**：普通运营（有 approval:approve 权限）尝试批准揭示口令 → 403；运营主管 → 通过。
- **补齐生产入口（本轮）**：以上规则此前只是「写好但没人调用」——没有任何接口去 Verify
  `shop.bulk_delete` / `device.purchase` 两种单据。现已接上：
  - `POST /api/v1/shops/batch/delete`（须持 `shop.bulk_delete` 单据，单次核销）
  - `POST /api/v1/devices/purchase`：新增 `count` 支持批量申购，金额由服务端按
    套餐单价 × 月数 × 台数重算（**不能听客户端报**），超阈值必须持证；
    客户端只要带了单据就一律校验，防止拿已核销/已拒绝的单据混过去。
- **补齐控制台界面（本轮）**：以上 API 此前没有对应界面，只有 curl 能用：
  - 店铺页：勾选 ≡ 批量删除，无证立刻被告知缺 `shop.bulk_delete` 单据并可一键申请；
  - 设备页：申购填**台数**不填金额，超阈值提示需双人复核单据。
- **金额一律不信客户端报价（本轮）**：审批单的 `detail.amount` 此前直接采信报文，
  于是"申购 198 台（5 万+）把 amount 写成 100"就能走单人审批，双人复核形同虚设。
  现由 `overridePurchaseDetail` 按套餐单价重算并覆盖，与申购接口共用同一口径。
- **阈值不再借用财务（本轮）**：此前 `approval/router.go` 与 `gateway/phase1.go`
  各自读 `finance.withdraw_approval_threshold_cny` 并各自兜底 50000 —— 典型的
  「两边口径不一致」，改配置只有一半生效，而两边单测都过（与 PG 那次同类）。
  现统一走 `Limits.PurchaseOwnerThresholdCNY()`（默认值仍 50000，真实值由运营定）。
- **202 不是成功（本轮，调用方规范）**：所有审批闸门返回 HTTP **202 +
  `code=approval_required`**（`apierr.Approval`）。谁把它当 2xx 放行，谁就会拿到
  「下载了一个装着错误 JSON 的 audit-export.csv」。前端统一由 `needsApproval()` 判别。
- **仍缺**：代理机构委托审批，需 `approval_flows` 表承载，属运营后台范围。

## 6. ~~审批工单的「待办通知」~~ → ✅ 已实现（notify Sink 定向推送）

- Notify Sink 新增 `iam.approval.` 订阅 + 6 个审批模板（待办/已签署/通过/驳回/过期/核销）。
- **定向推送**：`approval.ResolveApprovers` 在服务侧算出合格审批人名单放进事件 `approver_ids`，
  Sink 据此定向投递。**不是全团队广播**——否则大多数人点进去才发现无权处理。
- 停用成员（membership 非 active）不推送。
- **实测**：普通运营 0 条待办、运营主管收到待办、申请人只收到结果通知。
- **补齐「过期」那一半（本轮）**：`iam.approval.expired` 事件此前只有常量和模板、
  从来没有地方发出，`ExpireSweep` 只改状态 ⇒ 申请人的单据悄悄失效了自己却不知道。
  现已补上事件投递（**风险等级必须是 medium**，notify Sink 会丢弃 low），
  并对申请人定向通知；审批人不被这条打扰。
- **补齐外发通道（本轮）**：邮件（SMTP）与 Webhook 两个 Sender 已实装，含投递留痕、
  失败重投任务与控制台状态展示——详见第 12 条。
- **仍缺**：短信通道；企微/飞书/钉钉目前走通用 Webhook 载荷（JSON + HMAC 签名头），
  没有按各家自己的卡片格式与加签协议单独适配。

## 7. 导出水印仅覆盖 CSV

- **状态**：明文导出的水印以 CSV 注释行承载（导出人/审批单/时间）
- **未做**：PDF / Excel 每页水印渲染
- **解锁**：引入表格与 PDF 渲染库（`excelize` / `gopdf`）

## 8. ~~未接入真实设备代理商 API~~ → ✅ 已交付（`network/providers_http.go` + `network/allocator.go`）

- **通用 REST 适配器**：厂商差异（接口路径 / 鉴权方式 / 字段名）收敛成 `VendorConfig`
  + 宽松字段映射（host/ip/ip_address × port/http_port/socks_port…），
  买任意一家填配置即可接入，不必为每家厂商改代码。
- **逐环境独立出口（关键）**：此前所有环境共用客户端进程的一组 `ZINIAO_UPSTREAM_*`
  环境变量，也就是**所有店铺同一个出口 IP**——平台据此可直接把账号串起来。
  现由服务端资源池按环境分配、凭据随启动单下发，实测两个窗口流量零交叉
  （出口A baidu=20/httpbin=0，出口B baidu=0/httpbin=4）。
- **不变量**：同一出口同一时刻只绑一个环境；厂商返回重复资源会继续申请，
  始终重复则 `ErrNoExclusiveResource`（宁可起不来也不能共 IP）；
  同一环境重复启动返回同一出口（粘性——出口 IP 频繁跳变本身也是关联信号）。
- **fail-closed**：分配失败直接拒绝启动，**不退回直连**（共用本机公网 IP 更糟）。
- **代理商密钥**只经 `ZINIAO_IP_TOKEN` 环境变量注入，不进 argv（wmic 可读命令行）。
- **轮换可观测** ✅：同一环境出口 IP 变了会 Raise `env.exit_ip_rotated`（high 级）
  安全事件，detail 带 prev_exit/current_exit。实测：12s 租约到期后再 launch，
  出口 39101→39102 且事件正确落库。同一账号换个 IP 登录是平台眼里的强风险信号，
  必须让运营看见而不是静默发生。
- **买真代理前的硬前置 ✅（已解决）**：绑定关系原先只在**分配器内存**里，服务重启后
  环境会拿到新出口 IP（有告警，但没解决"不换"）。现已落库到 `egress_bindings`，
  重启/发布后走同一条绑定取回同一出口——细节见第 11 条。
- **仍缺**：厂商的**质量元数据**（ASN 真实类型、rDNS/TTL 一致性、黑名单历史）
  目前按 residential 假设，纯净度打分缺真实输入；`ProxyConfig` 也未长期落库沉淀，
  无法做「历史共用同一 ASN」的关联检测（见第 9 条）。

## 9. 其它登记项

| 模块 | 未实现 | 说明 |
|---|---|---|
| M3.3 | 云手机 / 云号购买与调度 | 仅数据模型 |
| M19 | Agent 人工确认闸门 | `AgentConfirmTTLSeconds` 已登记，执行器未接 |
| — | 事件总线 Kafka 传输层 | 当前进程内派发 |
| — | argon2id 替换 PBKDF2 | 待引入 `golang.org/x/crypto` |

## 10. ~~临时授权到期自动回收~~ → ✅ 已注册为定时任务（本轮）

- **分工**：到期**即刻失效**由读路径负责（`identity` 拿着 `expires_at` 现场判定，
  不等定时任务，否则会留下最长一个执行周期的裸奔窗口）；
  `iam.temp_grant_revoke`（30m）负责**清行 + 连带撤销 `user_roles` + 定向通知本人**。
- **本轮撞出的连带缺陷**：`DataGrantRepo.ListByUser` 在 SQL/内存层把过期行提前过滤了，
  而 `identity` 判断「角色是否因临时授权到期而失效」恰恰依赖这些过期记录存在 ——
  两者相消的结果是**临时授权到期后权限照旧**。现在仓储返回全量，过期判定由策略层做。
- **mem 踩坑记录**：给 `grantRepo` 的方法自行加锁会当场死锁——`mem.WithTx` 已经持有
  同一把不可重入的 `sync.Mutex`，仓储层不负责并发控制（曾导致测试卡 10 分钟超时）。
- **PG 16.15 实跑已验证（本轮）**：手工构造三种输入跑 `iam.temp_grant_revoke`——
  ① 到期临时角色（连带撤销 `user_roles`）② 到期但同角色另有正式授权（正式角色**保留**）
  ③ 未到期/正式授权（一行未动）。`affected=2`、两条定向给当事人的通知入库，
  实跑数据随后已清理。`DELETE ... RETURNING` 这条 SQL 此前从未在真库上跑过，
  mem 全绿不代表它对。
- **已知限制**：`user_roles` 上 `(tenant_id,user_id,role_id)` 唯一约束，导致同一人的
  「正式」与「临时」授权共用一行；回收时靠比对 `granted_at` 与授权 `created_at`
  决定是否连带撤销，属间接证据。根治需给 `user_roles` 增加 `source` 列
  （`permanent`/`temporary`），届时改为显式字段判断。测试
  `TestRevokeKeepsEarlierPermanentRole` 锁住了这条判据。

## 11. ~~出口绑定持久化~~ → ✅ 已落库（`egress_bindings`，migration 0005）

- **问题**：绑定关系原先只活在 `network.Allocator` 的四张 map 里，进程重启即空 ⇒
  每个环境被重新分配一个新出口。对平台而言「同一账号忽然换登录 IP」是最强的关联信号之一，
  等于**每次 deploy 都主动给全部店铺做一次 IP 跳变**。docs 此前把它标为「上真账号前必须落库」。
- **落库什么**：`env_id / tenant_id / provider / resource_id / protocol / host / port /
  country / region / city / asn / asn_type / region_code / last_exit / bound_at / expires_at`。
  **不落凭据**（L4）：username/password 只随启动单下发；重启后由分配器调
  `Provider.(Attacher).Attach(resource_id)` 向代理商**取回**，`-ip-attach-path` 配置路径
  （支持 `{id}` 占位）。厂商不支持取回时只能重新申请，此时出口必变 ⇒ 仍会触发
  `env.exit_ip_rotated`（high），并在启动日志与运行日志里说明原因，绝不静默换。
- **两条互斥锁都在数据库里**（进程内 map 只对一个进程有效，滚动发布时新旧进程会同时申请）：
  ① `(provider, resource_id)` ② `(provider, host, port)`。后者容易被漏掉——平台看到的是
  **出口地址**，不是我们的订单号；同一 IP:PORT 被重复下发了两个订单号时，只看资源 ID 会放过
  「两店同 IP」。PG 侧用不带推断目标的 `ON CONFLICT DO NOTHING`，撞任一条唯一索引都算
  「资源已被占用」，交给分配器换下一个候选，而不是把 23505 冒成 500。
- **fail-closed**：绑定写不进库就**拒绝下发出口**（`ErrBindingUnavailable`），并把刚申请到的
  资源归还厂商。发一个「重启后必然换 IP」的出口比这次起不来贵得多。
  缺租户上下文同样拒绝——不降级成内存绑定。
- **回收**：`network.egress_sweep`（15m）按租户删到期绑定并**向厂商归还资源**。
  重启后内存是空的，只有库知道这台机器还占着哪些资源；不归还等于持续计费 + 占着代理商名额
  （新环境申请不到独占 IP，表现为莫名的 `ErrNoExclusiveResource`）。
  与 `network` 池内的 10m 内存巡检分工：后者管进程内过期，前者管库里跨重启的残留。
- **轮换判定跨重启**：`last_exit` 随绑定一起落库，重启后拿它当对照值；
  没有这一列，重启导致的 IP 跳变在告警里会变成「首次分配」，运营看不到。
- **实测**：
  - 单测（`network/egress_test.go`）：重启复用同出口且不重复向厂商申请、厂商不支持 Attach 时
    正确报轮换并归还旧资源、落库失败 fail-closed、跨实例抢同一出口被数据库挡住、
    到期 sweep 归还资源、过期绑定被替换并带出旧出口。
  - PG 16.15 实跑（`platform/store/pg/egress_test.go`，`ZINIAO_PG_DSN` 存在时执行）：
    迁移 0005 应用成功；两条唯一索引各自的冲突都返回「占用」且不落库；
    `DELETE ... RETURNING` 只删到期行；同一环境换绑只剩一行；Release 幂等。
    另有 `TestAllocatorRestartOnPG` 用真库 + 真分配器验证「重启后取回同一条绑定、申请次数仍为 1」。
    实跑数据按租户前缀清理。
- **仍缺**：多代理商路由（`egress_bindings.provider` 已留字段，选择策略未实现）；
  绑定历史留档（当前只有「当前绑定」，做「历史共用同一 ASN」关联检测需要事件化）；
  环境停用/删除尚无服务端入口，因此 `Allocator.Release`（解绑 + 归还）目前只有
  「到期 sweep」这一条实际触发路径——建了入口后应在那里调用它，别等租约到期。

## 12. ~~通知只有站内 + 控制台两通道~~ → ✅ 已交付 SMTP 邮件 + Webhook（含投递留痕与重投）

- **为什么先做它**：上一轮的痛点是「出口轮换这种 high 级事件没人盯着控制台就看不见」。
  站内通知解决的是「写进了数据库」，不解决「人收到了」——中间这一公里以前完全空白。
- **两个 Sender**（`notify/smtp.go` / `notify/webhook.go`）：
  - 邮件：`STARTTLS` 协商失败**直接报错**，不降级明文（告警正文带租户/站点/操作结果；
    且标准库 `PlainAuth` 本身也拒绝明文发口令）。主题 RFC 2047 + 正文 base64，
    保证中文告警穿过老旧的 7bit 中继。
  - Webhook：JSON 载荷 + `X-Ziniao-Signature: sha256=<HMAC(ts+"."+body)>`。
    签名**带时间戳**是为了让接收方能拒重放——一条被重放的「出口 IP 已轮换」
    足以把运营引到完全错误的处置路径上。5xx/429/连接错误重试并退避，
    4xx 不重试（再试多少次都一样）。
- **SSRF 拦在 dial 层，不是拦 URL 字面量**：只看 URL 挡不住「域名先解析到公网、
  真正连接时解析到 127.0.0.1」的 DNS rebinding，也挡不住配错到云元数据地址
  （169.254.169.254）。`newSafeTransport` 在每次建立连接前逐个检查解析结果，
  内网/链路本地/组播/CGNAT 一律拒绝；本地自测由 `ZINIAO_WEBHOOK_ALLOW_LOOPBACK` 显式放开。
- **投递留痕（`notifications` 加列，migration 0006）**：
  `delivery_status`（none/pending/sent/failed）+ `delivery_note` + `attempts` +
  `delivered_channels` + `last_delivery_at`。没这张表状态，「这条 high 告警到底发没发到」
  就永远只能靠翻日志猜。
  - **Handle 对外发失败不返回 error**：通知已入库，返回 error 会让 outbox 重投**整条事件**、
    于是再生成一条一模一样的站内通知（测试 `TestDeliveryFailureDoesNotDuplicateNotification`
    锁住了这条）。失败的正确归宿是这一行的 `failed`，交给重投任务。
  - **DeliveredChannels 的语义是「已终结」而非「已成功」**：用户没填邮箱时邮件通道
    被跳过（`ErrNoRecipient`），这种情况必须记进来，否则重投任务会把「没邮箱的人」
    当成「投递失败的人」，每轮重试一次直到耗尽次数，最后还是假的 failed——
    这类假失败会把真正需要人看的告警淹没掉。
- **重投任务 `notify.delivery_retry`（5m）**：只发「所需通道 ∩ 已装配通道 − 已终结通道」的差集。
  邮件已经到了对方邮箱，却因为 Webhook 抖动再发一封，是最伤用户信任的做法。
  `attempts` 达 `MaxAttempts`（默认 3）后置 `failed` 并停手，不再无限增长。
- **默认只放行 high**（`ZINIAO_NOTIFY_MIN_RISK`）：medium（用量提醒等）量大且不需要立刻处理，
  全推向邮箱只会让人把整个通道静音——那等于连 high 告警一起失去。
- **配置口径**：`platform/config` 的 `Notify` 段 + `AttemptsLimit()` 单一取值方法。
  环境变量：`ZINIAO_SMTP_HOST/PORT/USER/PASS/FROM/TLS_MODE/TIMEOUT`、
  `ZINIAO_WEBHOOK_URL/SECRET/TIMEOUT/MAX_RETRY/ALLOW_LOOPBACK`、
  `ZINIAO_NOTIFY_MAX_ATTEMPTS/MIN_RISK`。**口令与签名密钥只走环境变量**，
  不进 argv（wmic 可读命令行）、不进日志、不在任何 API 响应里出现。
- **实测**：
  - `notify/smtp_test.go`：自写了最小 SMTP 服务器（不是 mock Send）——中文主题确认为 RFC 2047
    编码、正文 base64 可解、RCPT/MAIL FROM 正确；无收件人返回 `ErrNoRecipient`；
    明文连接带口令被拒且不泄露口令；STARTTLS 协商失败不降级明文；Health 真的 dial 一次。
  - `notify/webhook_test.go`：签名可被接收方独立验算；503 重试到第 3 次成功；
    404 只请求 1 次；429 重试但带退避；默认拦截 loopback 目标；超时受控。
  - `notify/dispatch_test.go`：high 事件落到全部外发通道且状态为 sent；medium 不外发；
    失败标记 failed 且不重复生成通知；无邮箱时邮件跳过、整体仍 sent；
    重投只补未送达通道；attempts 耗尽后停手。
  - **端到端** `app/notify_delivery_test.go`（真 `app.New` + 真 `config.Load()` + httptest）：
    环境变量 → BuildSenders → Sink → Webhook 500 → 留下 failed → 恢复后 `RunJobsOnce`
    补投成功 → 状态转 sent 且再跑一轮不重复投递。这条用例专门防「每处都对、拼起来不对」。
  - **PG 16.15 实跑** `platform/store/pg/notif_test.go`（默认 skip）：迁移 0006 应用；
    nil/空 `[]string` 写入 `text[] NOT NULL` 不触发 23502（即 egress 那次踩过的 NULL 坑，
    这里靠 `strArr` 兜底且由真库验证）；非法 `delivery_status` 被 CHECK 拒绝且不落库；
    `ListPendingDelivery` 只取 pending/failed 且按 `created_at` 升序；回写不改标题正文。
- **控制台**：通知中心新增「外发」一列（已外发 / 投递中 / 外发失败 + 失败原因 tooltip / 仅站内），
  把「站内已通知」与「人真的收到了」区分开。
- **仍缺**：短信通道；企微/飞书/钉钉按各家的卡片格式与加签协议单独适配
  （当前是通用 JSON 载荷 + HMAC 签名头，机器人接收方需自行解析）；
  投递幂等目前依赖对方按 `id` 去重（同一通知每个通道至多成功一次，但跨进程故障切换时
  存在「投递成功而状态未回写」的窗口，此时会重发一次——这也是 `delivered_channels`
  之所以要落库的原因；接收侧的幂等表仍未建）。
