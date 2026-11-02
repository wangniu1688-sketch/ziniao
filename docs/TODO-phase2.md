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
- **~~仍缺：代理机构委托审批~~ → ✅ 已实现（`delegation/`，`approval_flows` 表，migration 0007）**：
  - **模型**：受托方（代理机构，同为系统内租户）申请代管委托方（店铺主团队）的指定店铺，
    委托方 owner/ops_manager 裁决。与审批工单（同租户高危动作二次审批）互补，是**跨租户**两方流程；
    `approval_flows` 行归属委托方，RLS 为**双主体策略**（`tenant_id = current_tenant()
    OR agency_tenant_id = current_tenant()`）——受托方必须能看到自己申请的状态，否则提交后石沉大海。
  - **授权随委托生灭**：批准 = 成员身份 + 内置 `agency_ops` 角色（最小权限：查看 + 环境开关，
    无审批/删除/凭证/财务）+ 每店一条**带到期时间**的店铺级 DataGrant（scope=`delegation:<flowID>` 可溯源），
    与单据状态**同一事务**落地——授权写不进去就整体回滚，不允许「单据已批准、实际没授权」的假生效。
    撤销/到期整批回收（DataGrant 删除 + 角色撤销），读路径带时间判定兜底，巡检 `delegation.expire`
    （30 分钟）负责清行、改状态与通知双方。
  - **闸门**：受托方不能批自己的申请（受托方 owner 也有 delegation:approve，必须按单据内容再拦）；
    申请范围混入非委托方店铺整体拒绝（不悄悄缩小范围）；同对（委托方,受托方）同时只允许一张 pending；
    申请单复用审批单 TTL 超期自动失效；委托期 1..365 天封顶。
  - **数据级放行接入店铺链路**：own 数据范围下，店铺列表/详情/状态迁移/更新按有效 DataGrant
    放行被托管店铺（到期行随读即失效）。
  - **API**：`GET/POST /api/v1/delegations`（party=agency 查自己发出的）、
    `POST /api/v1/delegations/{id}/approve|reject|cancel|revoke`；权限 `delegation:apply/read/approve/revoke`
    （apply 给 owner/ops_manager；客服没有）。通知：`iam.delegation.*` 6 个模板，待办定向委托方审批人、
    结果定向申请人。
  - **实测**：真库（ZINIAO_PG_DSN）验证迁移 0007 + 双主体 RLS（第三方读不到、受托方可读可撤回）；
    e2e 验证批准后受托方以委托方团队登录**真的**能看到被托管店铺、撤销后 403。
  - **已知边界（记档，后续轮次）**：成员花名册会多出受托方申请人一行（角色回收后为空权限，不构成
    权限面）；~~环境启动/停止等入口暂无数据级店铺校验~~ → ✅ 已统一收口（见第 17 条）；
    委托单无运营后台界面，暂以 API 供客户端使用。

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
- **~~仍缺：企微/飞书/钉钉专属卡片~~ → ❌ 判定为不做（决策留档，不要再排）**：
  收件人是**租户团队内部的运营人员**（events 携带 tenant_id，收件人=操作人本人/owner/ops_manager/
  定向的 `approver_ids`；全仓不存在「外部客户/buyer」身份）。这些人已经被站内信 + 邮件 + 短信覆盖，
  通用 Webhook（JSON + HMAC）也已能对接 Slack/Discord/自建中台。缺的只剩「对方机器人强制加签」
  这一格边际场景——让用户关掉加签或加一层中转，成本远低于我们维护三套卡片格式 + 三套测试 + 长期升级。
- **本轮补齐（收件侧闭环）**：
  - **每人通知偏好**（`notification_prefs` 表，migration 0008）：`GET/PUT /api/v1/notifications/preferences`。
    约束：站内信**不可关**（是可回看的事实记录），关掉全部外发 = 只要站内信；Webhook **不进个人偏好**
    （服务端集成通道，不能让「这条告警进没进 SIEM」取决于个人勾选）；未设置 = 默认全发，老租户升级行为不变；
    未知通道名直接 400（拼错却静默生效会让用户以为自己关掉了）。
  - 生效点在**落库之前**：`Channels` 写进表里就是重投任务的输入，若按全通道落库再靠重投过滤，
    用户刚关掉的通道会在下一轮重投复活（重投同样按**当下**偏好再收敛一次）。
  - **客户端通知中心**：运营每天待在桌面端开环境，审批待办/委托申请只在后台控制台可见必然漏办。
    现在 `client/index.html` 有未读角标 + 抽屉列表 + 单条/全部已读 + 通知设置（按 `available` 动态渲染开关）。
    角标取服务端 `unread`（SQL COUNT 真值），不是"拉 50 条数一数"——后者在超出一页时会给出假的未读数。

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
- ~~多代理商路由（`egress_bindings.provider` 已留字段，选择策略未实现）~~ → ✅ 已交付：
  `network/router.go` 提供路由表（主备 Priority / 权重 Weight / 可售国家 / 并发名额 / 手工摘除 /
  连续失败熔断冷却），`Allocator` 改为按路由选代理商并**跨家故障转移**；
  归还、重启复用、到期 sweep 全部按 `egress_bindings.provider` 分派（不再拿 A 家的 ID 去 B 家退）；
  配置走 `ZINIAO_IP_VENDORS` + 每家 `ZINIAO_IP_VENDOR_<NAME>_*`（凭据只进环境变量，半配置即报错）；
  只读可见性 `GET /api/v1/egress/vendors`（`egress:read`：老板 + 运营主管）与控制台路由表卡片。
  判定不做：按实时质量打分选路——代理商质量是采购决策，应写进配置，
  让运行时去猜只会得到一条没人能解释的路由。
- **~~仍缺：绑定历史留档~~ → ✅ 已交付（`egress_binding_history`，migration 0009，见第 15 条）**
- **~~仍缺：设备申购绕过 Allocator~~ → ✅ 已交付（设备出口并入分配链，见第 16 条）**
- **~~仍缺：重启后 `Router.inflight` 从 0 起算~~ → ✅ 已交付（启动期按库回填，见第 16 条）**
- **仍缺**：环境停用/删除尚无服务端入口，因此 `Allocator.Release`（解绑 + 归还）目前只有
  「到期 sweep」这一条实际触发路径——建了入口后应在那里调用它，别等租约到期。
  设备到期回收目前只归还出口、不回退 `device_count` 计量（与设备删除/回收站一致，
  要减配额得先确定「过期设备算不算占用配额」的产品口径）。

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
## 13. ~~短信通道~~ → ✅ 已交付（`notify/sms.go`，阿里云云市场 AppKey+AppSecret 签名）

- **先确认凭据属于哪套 API**（这一步选错的代价是整条通道稳定 403）：
  `ck_…/cs_…` 前缀的 Consumer Key/Secret 是**阿里云云市场 API 网关**的 AppKey/AppSecret，
  不是 dysmsapi 的 `LTAI…` AccessKey。两者签名算法完全不同：
  - 云市场：`GET /singleSendSms`（`sms.market.alicloudapi.com`），
    `X-Ca-Key/Timestamp/Nonce/Signature` + `HMAC-SHA256(StringToSign)`；
  - dysmsapi：RPC 风格 `Signature=HMAC-SHA1(GET&%2F&sortedParams)`。
  代码里用 `ZINIAO_SMS_PROVIDER`（默认 `aliyun_market`）显式区分，填成别的值装配阶段直接报错，
  而不是"看起来发成功了"。
- **`StringToSign` 逐行拼接**：method / Accept / Content-MD5 / Content-Type / Date /
  规范化头 / 规范化 URL。两个坑都写进测试：
  - GET 无正文时 MD5 与 Content-Type 是**空行而不是省略**——少一行就是另一个签名；
  - URL 只含 `path + 字典序 query`，**不含 host**（把 host 也算进去是最常见的 403 来源）。
- **三个与邮件/Webhook 不同的口径**（都不是实现细节，是成本问题）：
  - **默认不重试**（`ZINIAO_SMS_MAX_RETRY=0`）：短信"失败"常常是超时，而对方其实已收下，
    再发一次就是同一条告警发两遍——重复扣费 + 把人烦到静音。漏发交给 5 分钟后的重投任务，
    它有次数上限且只补未送达通道。只有**限流**类业务错误值得当场重试。
  - **只认 2xx 不算完**：网关把「模板不存在 / 签名未审核 / 余额不足」放在 200 里
    （`success:false`）。只看状态码会把每一条都记成已送达，控制台一片"已外发"而用户一条没收到。
  - **号码问题按终态处理**：未填手机号或格式非法统一包装 `ErrNoRecipient`。
    1 个填错的号码不该让一条 high 告警反复失败，最后把真告警一起淹没。
- **号码规范化**（`NormalizeMobile`）：忽略空格/`-`/`()`；`+86`、`0086`、裸 `86` 开头都能收；
  **带 `+` 或 `00` 前缀却不是 86 的一律拒发**——`+1-380-013-8000` 去掉符号正好是 11 位
  "看起来合法"的国内号，放行就等于把短信发到不相干的人手上。错误文案里号码脱敏（前 3 后 4）。
- **内容**：标题 + 正文按**字符**截断到 120 字写入模板变量（默认变量名 `content`，
  可配 `ZINIAO_SMS_TEMPLATE_KEY`）。超长会被运营商拆条、乱序到达且重复计费。
- **凭据口径同邮件**：只经环境变量，不进 argv、不进日志、不进 API 响应；
  `Health()` **不做"真发一条"**（短信没有免费探测方式，任何真实调用都计费并打扰收件人），
  只校验配置完整性与端点域名可解析。真机校验走 `go run ./cmd/ziniao-sms -to <手机号>`。
- **实测**（`notify/sms_test.go`，自起 httptest 网关而非打桩 Send）：
  签名由测试**独立重算**比对（不复用被测实现，避免"两边一起错"）；查询串字段与字典序正确；
  4xx 只请求 1 次、5xx 按 MaxRetry 重试到成功、默认 0 次重试、限流重试而非限流不重试；
  `success:false` 算失败；半配置（缺任一项）装配即报错；AppSecret 不出现在错误/请求头/查询串；
  号码规范化表驱动（含 `+1-…` 拒发）；超长内容被截断。
## 14. ~~环境只有"启动"没有"关闭"~~ → ✅ 已交付（生命周期闭环）

- **缺口**：`Launch` 把 `env.Status` 写成 `running` 之后，**全仓库没有任何路径把它改回去**
  （grep 不到 `env.stopped` / `"stopped"` / 任何 stop 接口）。后果不是"少一个接口"这么轻：
  运营看到的是一屏永远在跑的环境，窗口其实早关了，代理进程与端口也没回收——
  「正在用」和「上次启动过」完全分不开，环境列表这条信息等于作废。
- **服务端** `shop.StopEnv` + `POST /api/v1/shops/{id}/environment/stop`（权限 `env:stop`）：
  - **幂等是硬要求**：客户端做不到"恰好上报一次"（关窗口报一次、进程退出再一次、
    网络失败重投还可能再来一次）。已经不是 `running` 就原样返回，不报错、不重复写审计——
    重复审计会让"今天启停了几次"这种问题没法回答（测试锁住：第二次调用不刷新 `UpdatedAt`）。
  - **不动出口绑定**：`egress_bindings` 的意义正是"重启不换 IP"，关一次窗口就换 IP
    等于把稳定出口变成每次都变的风险信号。
- **客户端**（`client/src-tauri/src/env_window.rs`）：
  - 维护「已打开环境」登记表（label → env/shop/端口/凭据），窗口 `Destroyed` 时上报关闭；
  - **代理进程句柄从丢弃改为登记**：`ensure_proxy` 原来 `.spawn()` 后不保存 `Child`，
    一个环境一个代理进程会越积越多；现在按端口 kill + wait（顺便消掉僵尸进程）；
  - 进程退出（`RunEvent::Exit`）时逐个上报——尽力而为，退出时异步任务未必跑得完；
  - 前端 `index.html`：环境列显示「运行中 / 空闲」，运行时多一个「关闭环境」按钮。
- **兜底巡检 `env.reclaim`**（30 分钟一轮，`ZINIAO_LIMIT_ENV_RECLAIM_HOURS`，默认 12）：
  客户端崩溃 / 断电时那条上报永远不会来。判据只有"多久没人动过它"，因此是启发式：
  误判只把状态显示成空闲，**不删 profile、不放出口绑定**，正在开着的窗口不受影响，
  下次启动会重新置 running。
- **实测**：`shop/environment_test.go`（关闭→idle、重复关闭不重写、无权限拒绝、
  无环境不假装成功、回收只收超阈值的、阈值 0 不启用）、`jobs/env_test.go`、
  `gateway/env_stop_e2e_test.go`（真 httptest：建店→建环境→启动→关闭→重复关闭→
  不存在的店铺不返回 200；跨租户不能关别人的环境）。
- **~~仍缺：企微/飞书/钉钉专属卡片~~ → ❌ 不做（见第 6 条的决策留档：收件人是租户内运营人员，
  站内信/邮件/短信已覆盖，通用 Webhook 兜底足够）**
- **仍缺**：投递幂等目前依赖对方按 `id` 去重（同一通知每个通道至多成功一次，但跨进程故障切换时
  存在「投递成功而状态未回写」的窗口，此时会重发一次——这也是 `delivered_channels`
  之所以要落库的原因；接收侧的幂等表仍未建）。

## 15. ~~出口绑定历史留档~~ → ✅ 已交付（`egress_binding_history`，migration 0009）

- **缺口**：`egress_bindings` 只有「当前绑定」，一次释放 / 一次换绑就把过去抹掉。
  而风控真正要回答的是「这两个店铺**先后**有没有用过同一 ASN / 同一出口」——
  时间错开的共用，当前表永远看不见（那时另一个环境早已解绑）。
- **只追加不改写**：5 种动作 `bound / rebound / released / expired / dropped`。
  枚举在 DB 有 CHECK、在 Go 侧也判一次——mem 驱动没有 CHECK，
  两份驱动给出不同结论就是「单测全绿、生产脏数据」（PG 那轮踩过的同一类坑）。
  事件按 `id` 幂等（回收任务重投、启动重试不会让「这个 ASN 用过几次」虚高）。
- **写入点（分配器，5 条生命周期）**：
  - 首次绑定 `bound`；
  - 换绑 `rebound`，带 `prev_exit`（从哪个出口变到哪个）与「原绑定为何作废」的成因；
  - 主动释放 `released`（内存模式下也留档）；
  - 到期 sweep `expired`（行被 DELETE 前留档，否则证据永久消失）；
  - 绑定作废 `dropped`（代理商下架 / 不支持按 ID 取回 / 取回失败 / 已过期）。
  **写历史失败只告警**：历史是可观测性，不该把一次库抖动放大成全站起不来（有测试锁住）。
- **读取**：`GET /api/v1/egress/history`（env_id / provider / asn / host / action / from / to + 分页）、
  `GET /api/v1/egress/asn-sharing`（`GROUP BY asn HAVING COUNT(DISTINCT env_id) >= n`）。
  权限沿用 `egress:read`（老板 + 运营主管；普通运营/代运营 403、匿名 401）。
- **「共用 ASN」按不同环境计数**：一个店自己在同一 ASN 下换绑多次不算共用，
  否则每个大客户都会被自己刷成告警。判定放在上层：这是**线索**，不是判决。
- **控制台**：系统管理页新增历史时间线（最近 20 条）与共用 ASN 线索卡片。
- **顺手修掉的两个进程内缺陷**（都是这轮用例逼出来的，都不是新代码引入的）：
  - `resources` / `provOf` 仍以**裸 resource ID** 作键：两家代理商都签发 `res-1` 时
    （各家订单号互不相干，这是常态），第二条会把第一条覆盖 ⇒ 归还时可能退掉
    **另一家正在用的线路**、名额记到别人头上。现统一为 `(provider,resource_id)` 复合键，
    与库侧唯一索引同口径；`TestSameResourceIDFromTwoVendorsStaysSeparate` 锁住。
  - 归还时无条件 `Router.Done`：重启后按库 `Attach` 取回的绑定**从未 Select 过**，
    归还它扣掉的是别的环境的名额 ⇒ `in_use` 出现假空位 ⇒ 继续往已满额的代理商派单。
    现按 `holds`（本进程真正拿过名额的资源）归还；`TestReleaseOfRestoredBindingKeepsOtherSlots` 锁住。
- **PG 实跑**（`platform/store/pg/egress_history_test.go`，默认 skip）：迁移 0009 应用、
  幂等重投、`array_agg`/`text[]` 扫描、时间窗收口、CHECK 拒绝脏动作、跨租户读不到。
  踩坑留档：最初给这张表加了 **FORCE RLS**，于是裸 `DELETE` 清理「执行成功但零行」——
  残留行让主键冲突，新事件被 `ON CONFLICT DO NOTHING` 全部吞掉，表现为「一条都没写进去」。
  现与 `egress_bindings` 同口径（只 ENABLE，不 FORCE）。
- **仍缺**：
  - 历史保留期（现在不清理；要裁剪就按 `idx_egress_history_tenant_time` 做区间删除，别加触发器）；
  - 单环境时间线的界面入口（API 支持 `env_id`，控制台只展示全局最近 20 条）；
  - ~~重启后 `Router.inflight` 从 0 起算~~ → ✅ 已交付（启动期按库回填，见第 16 条）

## 16. ~~设备出口并入分配链~~ → ✅ 已交付 + ~~重启后名额回填~~ → ✅ 已交付

**为什么这条优先级最高**：设备申购此前**直接调 Provider**，是系统里唯一绕过出口不变量的
入口——无互斥判重、无落库（重启必换 IP）、无归还路径（按量计费的线路一直烧钱）、
无历史留档。设备是长期资产（按月租），它比环境更不能承受「重启换出口」：
一台设备上绑着多个店铺账号，出口一变就是一批账号同时跳 IP。

- **并入方式**：`device.Service` 持有 `*network.Allocator`（与环境同一个实例，由 `App.SetIPPool`
  一并注入），不再持有裸 Provider。设备在绑定表里的键是 `dev:<deviceID>`——
  **同一张 `egress_bindings` 就是互斥边界**：设备的出口不能和环境撞到同一个 IP，
  前缀让「这条绑定是环境的还是设备的」在历史里一眼可辨。
- **生命周期**（全部复用分配器已有能力，不另写一套）：
  - 申购：逐台 `pool.Acquire`（独占 + 落库 + 留 bound 历史）；
    批量申购里第 N 台失败会**归还前 N-1 台**的出口（否则设备没建出来、线路已经开始计费）；
    出口租约短于购买周期时设备到期时间取更早的那个（不要出现「活着的设备没有出口」）。
  - 取回：`Service.Egress`（重启后靠 `egress_bindings` + 代理商 Attach 拿回活凭据，
    而不是读设备行里那份申购时刻的快照）；只对「已在库里有绑定」的设备取回，
    不给自有设备/紫鸟盒子凭空派一条出口。
  - 退役：`Service.Retire` + `POST /api/v1/devices/{id}/retire`（新权限 `device:retire`，
    与申购同级高风险，只给老板与运营主管）。顺序是**先归还出口再改状态**——
    反过来的话，归还失败就会留下一台「已停用但线路仍在计费」的设备，且没有任何提示。
  - 到期：`Service.RetireExpired` + 新定时任务 `device.retire_expired`（6 小时一轮）。
    设备按月租、出口按量计费，没人点退役时这是唯一的归还路径。
- **启动时名额回填**（顺带解决第 15 条留下的洞）：
  `Router.Seed` + `Allocator.RestoreInflight(ctx, tenants)`，按租户遍历 `ListByTenant`
  把库里每条既存绑定回填成一格 `inflight`，并登记进 `holds`（记账），
  这样后续归还（主动释放 / 到期 sweep / 绑定作废）才有据可依地把坑让出来——
  **只加计数不还，一次重启就会变成永久性的配额泄漏**。
  半回填比不回填更危险（看起来回填过了，实际只有一部分准），所以读失败即 fail-closed。
- **为什么不做跨租户全表扫描**：仓储只暴露带租户上下文的读口，
  不给「绕过 RLS 的全局读」开先例——省一次 SQL 换一个安全口子不划算。
- **顺手修**：`MockProvider` 字面量构造时 `issued` 为 nil，第一次 Acquire 直接 panic。
  改为 Acquire 内兜底初始化（零值可用的结构体才不容易被用错）。
- **测试**：`device/device_egress_test.go`（逐台独占 + 落库 + 留档、退役归还且幂等、
  到期巡检归还、无池降级、无权限拒绝）、`network/allocator_restore_test.go`
  （回填后不再超卖、释放真的让出名额、代理商下架不计数、读失败 fail-closed）、
  `gateway/device_retire_test.go`（401 / 200 / 幂等 / 状态 recycled）。
- **仍缺**：设备到期回收不回退 `device_count` 计量（口径未定）；设备删除入口仍无。

## 17. ~~环境/凭证入口无数据级店铺校验~~ → ✅ 已统一收口（`shop/access.go`）

**为什么是它**：这是本地清单里唯一一条「安全性靠调用点自觉」的缺口——
own 数据范围的判定原先散在各入口，且只有店铺读/改路径（Get/Transition/SetGroup）接入；
**环境四入口（建/启/停/查/自检）与凭证四入口（写/列/揭示/轮换）完全没有数据级校验**，
持有 `env:launch` / `shop:credential.manage` 的 own 范围成员可以操作租户内**任何**店铺的
环境与凭证。内置角色矩阵恰好把这个洞藏住了（有凭证权限的都是 all 范围），但自定义角色
一组合就穿透，agency_ops 的设计前提（"环境开关要配合 DataGrant 放行"）也没有落地。

- **收口方式**：`shop/access.go` 的 `checkShopAccess`，判定口径只有一份：
  all/team 放行 → own 范围本人店铺放行 → 其余必须持有该店铺的有效店铺级 DataGrant
  （到期行随读即失效）→ 授权查询失败一律拒绝（fail-closed，查不到就当没有）。
  原先散落三处的三段式判断（含 `hasDelegatedShop`）全部删除，改走同一入口；
  Delete/Restore 也一并补上（内置角色不可达，但自定义 own 角色持有
  `shop:delete` 时不再能删全租户店铺）。
- **接入的入口**（共 12 个）：
  - 环境：`CreateEnv` / `Launch` / `StopEnv` / `EnvService.Get` / `EnvService.Check`
    —— 后两个签名从 `tenantID` 改为 `actor`，服务层补完整裁决
    （与 Put/Reveal 的"网关一次、服务再核一次"同口径）；
  - 凭证：`Put` / `Reveal`（拒绝同样落 denied 高危审计）/ `ListKinds` / `RotateKey`
    —— 后两个原先**服务端零校验**（RotateKey 甚至没有任何调用方，属于"写好但没接线"
    的裸 API），现在功能权限 + 数据级校验都在服务层；
  - 店铺：`Get` / `Transition` / `SetGroup` / `Delete` / `Restore`。
- **顺序细节**：StopEnv 的数据级判定放在幂等早退**之前**——无权就是无权，
  与窗口当前是不是 running 无关；Reveal 的数据级拒绝同样走 `record(denied)`
  高危审计，明文从未离开密文层。
- **为什么不沿用"店铺行可缺失就跳过判定"**：环境本就归属店铺，店铺行取不到
  说明数据已经不自洽；对 own 范围成员 fail-closed 比放行安全。
  `seedEnv` 测试助手因此改为连店铺行一起种（它们本来就是一体的）。
- **实测**：
  - `shop/access_test.go`：own 范围成员对他人店铺的环境四入口/凭证四入口全部拒绝；
    持有效 DataGrant 的那家店全链路放行（含 Reveal 拿到真明文、RotateKey 轮换）；
    到期授权随读失效；授权查询故障时 fail-closed（fail 校验失败 → 拒绝而非放行）。
  - `gateway/delegation_e2e_test.go` 8b：受托方以委托方团队身份登录后，
    启动/关闭**被托管**店铺环境放行（200），**未托管**店铺 403——
    锁住「委托授权 = 店铺级、不是租户级」这条边界。
- **顺手修**：ListKinds/RotateKey 服务端零校验的裸入口（见上）。

## 18. ~~审批路由规则硬编码~~ → ✅ 已可配置（`approval_route_rules`，migration 0010）

**为什么是它**：审批级别（谁有资格批这张单、要不要会签）此前写在 `approval/router.go`
的 `rulesOf` 里——运营把「批量删除从运营主管提到老板」或「申购阈值从 5 万降到 3 万」
必须改代码发版。审批级别是**运营策略**而不是代码不变量；配置不支持热更新
（`config` 只在启动时 Load 一次），所以必须落库。

- **表**：`approval_route_rules`（`approval_flows` 已被委托审批占用，故另起名）。
  列：ticket_type / min_amount / roles / min_approvers / reason / priority / enabled。
  CHECK 三道（单据类型、金额非负、会签 1..10）+ Go 侧再判一次（mem 无 CHECK，两份驱动
  给出不同结论就是「单测全绿、生产脏数据」）。
- **判定口径**（决定了老租户升级行为不变）：
  租户规则按 priority 升序取**第一条命中** → 命中不了则**回落内置规则** → 再兜底。
  回落而不是直接兜底，是两件事的平衡：只配了「≥5 万双人复核」就来一张 1 千的单，
  直接兜底会变成「谁都能批」（配了一半反而降级）；而配半套就死锁同样不可接受。
- **不重复读配置**：网关 `purchaseNeedsApproval`（申购要不要持证）改走
  `Approvals.NeedsTicketForAmount`，与 `RequiredFor` **同一数据源**——
  两边各读一次阈值就是「网关说不用持证、单据却要求双人签署」这种看起来像随机抽风的错
  （与第 5 条里那个阈值被读两次的坑同源）。
- **写入期校验而非运行期猜**：角色码必须真实存在（拼错一个角色 = 这条规则永远匹配不到
  审批人 = 单据卡在 pending 谁也批不了）；会签人数上限 10（配 99 = 永远批不出来）。
- **可追溯**：规则变更发 `iam.approval.rule.updated`（high 级）落审计；
  `Requirement` 新增 `Source`/`RuleID`，审批单事件里记下「这条单为什么是这个级别」。
- **权限**：新增 `approval:route.read/manage`（只给老板 + 运营主管）——
  改审批级别等于改控制本身，普通运营能改就等于审批是一道可绕过的门。
- **API**：`GET|POST /api/v1/approvals/rules`、`DELETE /api/v1/approvals/rules/{id}`；
  GET 同时返回 `builtin`（运营要能看到「删掉这条会变成什么样」）。
  控制台：审批页顶部新增规则卡片（无权限 403 → 不渲染）。
- **实测**：
  - `approval/rules_test.go`：配置覆盖内置、金额门槛可配（含「配了规则但金额没命中 → 回落」）、
    停用行不参与、priority 取最小、租户隔离、四类非法输入被拒、局部更新不误停用、
    变更落审计（单测里显式接 audit.Sink + DispatchOnce）、配置出来的会签人数真的驱动会签引擎。
  - `gateway/approval_rules_test.go`（真 httptest）：运营主管建规则 → 自己批批量删除被拒
    （内置规则下他能批）→ 老板可批 → 删除后回落；普通运营读写均 403；重复删除不假成功。
  - `platform/store/pg/approval_rules_test.go`（真库）：迁移 0010 应用、
    `numeric(14,2)`↔`float64` 等值（靠 `min_amount::float8`）、nil `[]string` 不写成 NULL、
    CHECK 拒绝未知类型与会签 99、跨租户读不到也删不掉、更新与删除（含重复删除报错）。
- **已知边界**：金额语义目前只有 `device.purchase` 用得上（其它类型配 min_amount 不会命中）；
  规则变更没有通知外发（只落审计 + 站内事件），需要告警的话接 notify 模板即可。

## 19. ~~接收侧投递无幂等 / 历史无限增长 / 环境与设备没有删除入口~~ → ✅ 已交付（migration 0011 + 定时裁剪 + 生命周期入口）

**为什么是这四件**：都是第 14/16/17 条留档的「仍缺」项，不含探索性工作——
收尾阶段把它们一次清掉。

### 19a. 接收侧投递幂等表（`event_consumptions`，migration 0011）

- **修的是发件箱的固有窗口**：outbox 只能保证 at-least-once——Handle 成功到 Ack 之间
  崩溃（或同事件里另一个处理器失败把整条打回 pending），整条事件会重投。没有登记表时
  重投的后果：notify 再生成一整份站内通知（同一事件两封信）、audit 撞审计主键
  （`entry.ID = event.ID`），事件卡在 pending 直到重试耗尽，同租户哈希链跟着停摆。
- **用法（强约束写进接口注释）**：`OutboxRepo.Consumed / MarkConsumed`；
  登记必须与 Handler 的业务写在**同一事务**（audit/notify 两个 Sink 均如此实现），
  否则「业务写已提交、登记没提交」的窗口原样复现。派发器重投时按
  `(event_id, handler)` 跳过已消费的处理器——**部分成功的进度得以保留**。
- **notify 的粒度**：逐收件人登记（键 `notify:<userID>`）且与该收件人的通知落库
  同事务——中途崩溃重投时，已通知的人不重复、没通知的人补上；
  全部处理完再登记整体键（bus 层也会登记，双保险幂等）。
- **fail-closed**：幂等查询失败按「未知」处理，整条事件等待重试——
  吞掉 err 当 false 等于没做幂等。
- **边界（不掩盖）**：外发通道自身的 at-least-once 窗口（Send 成功但状态回写前崩溃）
  不由这张表解决——那半步在对方系统里，接收方按通知 id 去重是契约的一部分（第 14 条）。
  这张表收窄的是我们自己能收窄的部分。
- **PG 实跑教训**：RLS 只 ENABLE 不 FORCE 时**表主连接不受策略约束**——
  `Consumed` 必须显式带 `tenant_id` 谓词（跨租户读不到也要靠仓储注入，不是靠 RLS；
  与 `egress_binding_history` 同口径，L12 老坑）。测试先用 `octx` 读到了 `true` 才暴露。

### 19b. 出口绑定历史保留期裁剪（`network.egress_history_prune` 任务）

- **表**：`EgressBindingRepo.PruneEvents`（CTE `DELETE ... RETURNING` 计数，PG 一条语句；
  mem 原位过滤）。裁剪是**运营决策**（保留多久），不是数据不变量——所以走定时任务 +
  配置（`egress.history_retention_days`，默认 180，环境变量
  `ZINIAO_LIMIT_EGRESS_HISTORY_RETENTION_DAYS`），不走触发器。
- **宁少勿多**：严格 `occurred_at < before`（边界上那条留给明天），
  且只删本租户的行；`retentionDays <= 0` 表示不裁剪（历史是资产，
  共用 ASN 的证据链怕断不怕长，想永远留着就配 0）。
- 24 小时一轮，逐租户出结果（jobs.Result 口径不变）。

### 19c. 单环境出口时间线（控制台）

- 后端 `GET /api/v1/egress/history?env_id=` 本就支持（第 14 条埋的口子），
  补的是控制台入口：系统页历史卡顶部加环境 ID 过滤框——非空时卡片切为
  「单环境出口时间线」，回答「这个店铺的出口换过几次、何时换的」这个排障问题
  （全局最近 20 条回答不了它）。

### 19d. 环境停用/启用/删除入口（`Allocator.Release` 的第二、三个触发点）

- **新增状态** `stopped`：环境退出服务（不参与启动），介于 idle 与删除之间；
  `Launch` 明确拒绝 stopped 环境（状态拦在启动前，不靠客户端自觉）。
- **停用** `POST /api/v1/shops/{id}/environment/deactivate`（幂等，权限 `env:deactivate`）：
  **先归还出口再改状态**（与设备退役同口径）——归还失败**整体失败**（fail-closed）：
  反过来会留下「已停用但线路仍在计费」的环境且无提示；环境还是可用的，用户可重试。
  running 必须先关闭（409）——窗口还开着就停用，客户端的关闭上报会落空。
- **启用** `POST .../environment/activate`：stopped → idle（幂等；非停用状态原样返回）。
  低风险事件 `env.activated`；再启用后首次启动可能换 IP，那条告警由启动链路的
  `env.exit_ip_rotated` 负责，不在这里重复造一遍。
- **删除** `DELETE /api/v1/shops/{id}/environment`（终态，权限 `env:delete`）：
  归还出口（**失败只告警不阻断**——与停用相反，因为删除不可逆，卡住状态机只会堆满
  删不掉的环境；漏还的线路走 `security.Raise`（`env.egress_release_failed`，medium）留痕）
  + 物理删行 + `env.deleted`（high）落审计。
  profile 在客户端本机，服务端删不到：事件带 `env_id`，客户端据此清理（尽力而为）。
  重复删除：店铺已无环境 → 404，**不假装成功**。
- **权限设计**：`env:deactivate` / `env:delete` 只给老板 + 运营主管（与 `device:retire` 同级）——
  停用会释放出口绑定（再启用可能换 IP，等于动所有环境的线路），删除不可逆。
- **顺带修**：mem `envRepo.List` 的排序比较器写错（`out[i].CreatedAt.After(out[i].CreatedAt)`
  恒 false），列表顺序一直是创建时间升序的巧合值。

### 19e. 设备删除入口 + device_count 计量口径定案

- **口径定案**：**过期/退役不回退计量，删除才回退**。理由：`Renew` 会把 expired 拉回
  active，若到期即回退就必须在续费时补计量（还要重新过配额校验，续费可能因配额不足被卡），
  链路多一对对称操作就多一对出错机会。口径定为「**台账占用**」：删除 = 台账出清 =
  释放配额，一次操作一个语义；想腾配额就删掉退役设备——这正是删除入口存在的意义。
  （已被否掉的另一版：到期即回退 + 续费补计量，被 Renew 路径的对称性否决。）
- **实现**：`DELETE /api/v1/devices/{id}`（权限 `device:delete`，老板 + 运营主管）。
  服务层强校验：**只有 expired/recycled 可删**（active 先退役——归还出口有专门路径，
  防「删了设备、线路还在计费」这种只有对账才能发现的漏）。删除与计量回退**同一事务**；
  回退前先查后减并在 0 时跳过（计量与台账历史不同步时钳 0 比减成负数好——
  负数会让后续申购凭空多出额度）。事件 `device.deleted`（high）带 `quota_released`。
- **软删除**：设备行走 `deleted_at`（列本来就有、没有任何代码在用——这就是「删除入口仍无」
  的实锤），与 shops 回收站同口径；列表默认过滤，审计与出口历史保留。

### 19f. 测试与验证

- `eventbus/bus_test.go`：A 成功 + B 失败 → 重投时 A 不再执行（部分进度保留）；
  重复派发不重复执行处理器。
- `notify/idempotency_test.go`：重投不产生第二份通知/外发；
  audit 与 notify 的登记键位互不干扰；逐人键位与整体键位分开。
- `device/device_delete_test.go`：删除回退计量（active 拒删 409、退役不回退、删除回退到 0、
  列表不再可见）、无权限拒绝。
- `shop/env_lifecycle_test.go`：停用幂等 + 拒绝启动 + 启用回来；running 拒停；
  删除后 GetByShop 无幽灵行、重复删除 404。
- `jobs/egress_prune_test.go`：只删本租户且严格早于边界的行；retention 0 不裁剪。
  （反例归档：首版用例给 t2 也放了 200 天前的数据，被 t2 自己那轮裁掉——
  跨租户隔离的断言要用「另一家在保留期内」的数据才能成立。）
- `gateway/lifecycle2_test.go`（真 httptest）：停用幂等 → 停用后拿不到启动单 → 启用回 idle
  → 删除 → 查询 404 → 重复删除 404；running 停用/删除 409；
  设备删除：active 409 → 退役 → 删 → 列表不见 → 未认证 401。
- `platform/store/pg/phase2d_test.go`（真库）：迁移 0011 应用；消费登记幂等 + 键位隔离 +
  跨租户读不到 + 事务回滚不残留；PruneEvents 计数正确且幂等；设备软删幂等 + 列表过滤；
  计量负 delta 等值读回；环境物理删除。
- gofmt 空、vet 干净、`go test ./...` 全绿（含 PG）；`node --check` 通过。
- **已知边界（已在本轮解决，见第 20 条）**：内置角色矩阵在 `EnsureBuiltinRoles` 里
  「已存在即跳过」——老部署的运营主管不会自动拿到新增权限码。已改为并集同步。

## 20. ~~内置角色权限不随版本同步~~ → ✅ 已并集同步（`iam.EnsureBuiltinRoles` + `RoleRepo.AddPermissions`）

**为什么是它**：内置角色在库里只写一次（`existing != nil { continue }`），新版本新增的
权限码老部署**永远拿不到**——运营主管在控制台看不到删除按钮、调用才 403，升级日志里
还没有任何痕迹（`device:retire`、`approval:route.*`、`device:delete`、`env:*` 四条同源）。
产品口径（用户拍板）：**要同步**。

- **同步方式：并集，不是全量对齐**（`RoleRepo.AddPermissions`，靠
  `(role_id, permission_key)` 主键 `ON CONFLICT DO NOTHING`，返回真正新增条数）：
  - 已有的不动、缺的补上、**多出来的保留**——全量 `SetPermissions` 会抹掉部署侧
    额外加的码，那是功能倒退；
  - 自定义角色（`Builtin=false`，含与内置同码的租户级角色）**一个码都不动**；
  - 租户级收紧的正道是**自定义角色**而不是改内置角色——内置角色是系统级共享行
    （`tenant_id` 为空），改它影响所有租户。
- **前提核实**：`RoleRepo.SetPermissions` 在业务层**没有任何调用方**——内置角色权限
  没有运行时修改入口，所以并集同步不存在「覆盖租户自定义改动」的实际风险
  （唯一能改它的路径是直接改库，那属于绕过系统）。
- **代价（明说）**：若有人绕过系统直接改库摘掉了某个内置权限码，升级会把它补回来。
  这是刻意的——内置角色以代码定义为准，不允许长期停在半同步态。
- **可观测**：`EnsureBuiltinRolesWithReport` 返回 `{角色码: 新增条数}`，
  `app.New` 在启动时 `log.Info("内置角色权限已同步", "added", ...)` 打出来——
  「升级后运营主管能用删除入口了」这件事在日志里没有痕迹的话，
  排查「为什么这台机器不行」只能靠猜部署时间。`EnsureBuiltinRoles` 保持原签名
  （十余处测试调用不受影响），只是转调带报告版本。
- **只同步 permissions**：`DataScope` / `MaskFields` / `DenyElement` 仍为首次写入；
  它们变更频率远低于权限码，需要同步时另开条目（同一套并集思路即可）。
- **弃用码不做回收**：并集只增不减，某个权限码从 `iam.Catalog` 移除后仍会留在
  角色行里（无害：功能权限判定按请求的 action 比对，多余码不产生放行）。
- **测试**：`iam/roles_sync_test.go`（老部署升级补齐 device:delete/env:*/approval:route.*、
  原有码不丢、二次同步报告 0、部署侧额外码保留、自定义角色不被污染）；
  `platform/store/pg/phase2d_test.go` 第 4 段（真库：`ON CONFLICT` 重复补齐返回 0 影响行、
  已有码不重复计、补齐后条数正确——mem 的 map 天然幂等，测不出 SQL 写错时每次启动都插一遍）。

## 21. 浏览器壳形态定案 + 多 webview 原型验证（✅ 已探路，落地待下一条）

**为什么先探路**：紫鸟/AdsPower 那种「环境窗口里带地址栏」的形态，在 Tauri 2 下只有一条路——
**空窗口 + 两个子 webview**（顶部工具条 / 网页区）。网页从「窗口自带 webview」搬进「子 webview」
之后，防关联的四条命脉（UA、注入脚本、profile 目录、代理）还生不生效，**只能真跑才知道**；
猜错就是把最致命的那层拆了。

### 21a. API 层结论（docs.rs，tauri 2.12.5，逐条核对过）

- `WebviewBuilder` 上 **`user_agent` / `initialization_script` / `data_directory` / `proxy_url`
  四个方法全在**（外加 `auto_resize`、`on_navigation`、`on_page_load`、
  `on_document_title_changed`、`on_download`、`on_new_window`）——子 webview **不丢**致命能力，
  这是敢走这条路的前提。
- `Webview::navigate()` 明确会**重跑 initialization_script**——地址栏导航后指纹脚本仍然生效
  （这条若不成立，带地址栏的浏览器壳就是伪需求）。
- `Window::builder`（空窗口）与 `Window::add_child(builder, position, size)` 需要
  **`unstable` feature，且仅 desktop**；`Webview::set_bounds/set_size/set_position/set_auto_resize`
  用于跟随窗口 resize。
- 坑（已写进代码注释）：
  1. **Windows 上在同步 command / 事件回调里创建 webview 会死锁**（WebView2 已知问题）
     → 一律在 async 任务 / 独立线程里建；
  2. `WebviewUrl::App` **不能带 query**（`probe.html?role=bar` 被当路径的一部分 → 404）；
  3. **debug 构建走 `devUrl`（:1420），release 才用嵌入的 dist**——不起 dev server 时页面 404，
     而 404 页的 `document.title` 恰好是 `localhost`，**极易被误读成"验证通过"**。

### 21b. 实测结果（`--probe-shell`，报告落 `%TEMP%/ziniao-probe.log`）

```
window ok（空窗口，无自带 webview）
bar  webview ok / page webview ok          ← 两个子 webview 都挂上了
page OK PROBE:{"role":"page","ua":"...Chrome/120.0.0.0 Safari/537.36 ZiniaoProbe/1.0",
               "injected":"yes","localStorage":"kept"}
bar  OK PROBE:{"role":"bar","ua":"...Chrome/156.0.0.0 Safari/537.36 Edg/156.0.0.0",
               "injected":"(未注入)","localStorage":"kept"}
set_bounds ok
resized 1000x700 → 重排 ok
profile dir entries: EBWebView
```

- **UA 生效**：page 拿到我们设的 `Chrome/120 … ZiniaoProbe/1.0`；对照组 bar 是本机真实内核
  `Chrome/156 Edg/156`——差异证明不是巧合。
- **注入脚本生效**：`injected: yes`（文档创建后、页面脚本前执行）。
- **profile 目录（data_directory）生效且被真正使用**：localStorage 可写可读，且指定目录下出现了
  WebView2 的 `EBWebView`——多账号 Cookie 隔离的前提成立。
- **布局可控**：手动 `set_bounds` 成功；窗口 resize 事件里重排成功（子 webview 默认**不**自动跟随）。
- **尚未验证**：`proxy_url`（要真起代理进程才有意义，留给真机联调）、release 打包下的
  `tauri://` 协议（能力是 webview 级的，与协议无关，但打包后应再跑一遍原型）。

### 21c. 代价与下一步

- **代价**：`tauri` 必须启用 **`unstable` feature**（实验性 API，随版本可能变）。
  已在 `Cargo.toml` 注明原因；原型 `--probe-shell` 保留在仓库里，升级 Tauri 后一键复跑即可
  验证 API 是否还成立——这比写在文档里可靠。
- **下一步（落地）**：`env_window.rs` 启动链路改成这套形态——空窗口 + 顶部工具条
  （`browser.html`：后退/前进/刷新/地址栏 + 环境信息条：店铺名、环境尾 8 位、内核、国家、代理状态）
  + 网页 webview（沿用现有 UA/注入/profile/代理）；**失败回退现在的裸窗口形态**
  （不因实验 API 把"启动环境"这个功能搞挂）；补 resize 重排与窗口关闭上报。
- **顺带修**：主控页「创建环境」按钮在无环境时被 `disabled = !hasEnv` 禁用——客户端永远建不了
  环境，只能回控制台建；已放开，并加国家输入 + 建完自动刷新列表（前端改动需重新打包才生效）。

## 22. ~~浏览器壳未落地~~ → ✅ 已交付（空窗口 + 工具条 webview + 网页 webview，失败回退裸窗口）

按第 21 条的定案落地 `client/src-tauri/src/env_window.rs`，形态就是原型验证过的那套。

### 22a. 结构（`env_window.rs`）

- **`build_shell()`**：`Window::builder`（空窗口）→ `add_child` 挂工具条 webview
  （`browser.html`，总高 `BAR_H = 72`：导航行 46 + 环境信息条 26，与 CSS 严格一致）→
  `add_child` 挂网页 webview（`WebviewUrl::External`）。防关联四件套（UA / 注入脚本 /
  profile 目录 / `proxy_url`）**一件不少全挂在网页 webview 上**，与裸窗口形态逐字段相同。
- **工具条与 Rust 的分工**：环境信息（店铺名 / 环境尾 8 位 / 内核 / 国家 / 代理状态）
  经注入脚本 `window.__ZINIAO_ENV__` 带入（`WebviewUrl::App` 不能带 query 的老坑）；
  后退/前进/刷新/地址栏经 `invoke('env_nav')` 交给 Rust 侧，只操作**该窗口自己的
  `-page` webview**（label 前缀强校验，地址栏导航不可能落到别的环境的 profile 上）。
- **地址栏回显**：网页 webview 的 `on_navigation` + `on_page_load(Finished)` 把当前 URL
  回写给工具条（`window.__ZINIAO_SET_URL__`）；地址栏导航用 `Webview::navigate()`——
  它会**重跑 initialization_script**（第 21a 条核对过的语义），指纹脚本换页不丢。
- **resize 重排**：子 webview 默认不跟随窗口，`Resized` 事件里按 scale_factor 换算逻辑像素，
  `set_bounds` 重排两个子 webview（与原型同口径）。
- **回退**：`build_shell` 任一步 Err → 打日志 → 收掉半成品窗口 → 原样走裸窗口
  `WebviewWindowBuilder` 路径。「启动环境」主功能不陪 experimental API 埋葬；
  `register_open` 在建窗口前完成，两种形态共用同一套 Destroyed → 上报关闭 + kill 代理。
- **重复启动 / stop / 退出上报**：查找从 `get_webview_window` 改为 `get_window`
  （壳窗口不是 WebviewWindow），导航优先打 `-page` webview；`close_env_window` 兼容两种形态。
- **CLI 坑**：`--launch-env` 只认「两个独立参数」，`--launch-env=<id>` 会被解析器静默跳过
  （无人值守启动第一次就栽在这，表现为"客户端起来了但什么都没发生"）。

### 22b. IPC 能力（本轮撞出的存量缺陷）

全工程**没有任何 capability 文件**（`gen/schemas/capabilities.json = "{}"`）——Tauri 2 下这等于
**对所有窗口全量拒绝 IPC**，探针原型为此才用 document.title 回传。新增
`src-tauri/capabilities/default.json`：`core:default` 授给 `main` 窗口与 `env-*-bar` 工具条
webview（应用自定义命令默认放行）；**网页 webview（`env-*-page`，指向外部站点）刻意不授予
任何能力**。顺带把 `index.html` / `browser.html` 的 CSP `connect-src` 放行 `ipc: http://ipc.localhost`
（Windows 上 invoke 的真实通道，缺了照样发不出去）。

### 22c. 实测（全部真跑，非推断）

- `cargo build`（debug）通过，capabilities 经 tauri-build 校验无告警。
- **真机 e2e**（`scripts/e2e-*.ps1`，内存存储 + seed）：登录 demo → 建环境 →
  客户端 `--launch-env` 启动 → **壳窗口正常打开**（无回退日志，窗口标题
  「亚马逊美国站01 · 环境 7D6CNDDZ · Chrome 120 · US · 代理已启用」）→ uTLS 前置代理就绪
  （listen=127.0.0.1:24072）→ 服务端 `status=running` → 任务栏优雅关闭 →
  服务端 `status=idle`（关闭上报闭环成立）。
- `--probe-shell` 复跑全绿（UA / 注入 / profile / set_bounds / resize 重排）。
- **待真机复验**：地址栏回显与按钮操作需人眼确认（无头环境只能验证到窗口出现与状态闭环）；
  release 打包（`tauri://` 协议）下的壳形态待打包轮一起验（前端改动尚有未打包的）。

## 23. ~~客户端环境生命周期缺口：停用/启用/删除无入口 + 本机 profile 无人清理~~ → ✅ 已交付

**为什么是它**：第 19d 给了服务端三个入口（deactivate / activate / DELETE），客户端一个都没有；
而且那一条里明写了「profile 在客户端本机，服务端删不到：`env.deleted` 事件带 env_id，
客户端据此清理」——**客户端从来没实现过**这句承诺。三个具体后果：

1. **停用即死路**：`stopped` 在客户端的环境列里被渲染成「空闲」（只有 running/其它 两分支），
   运营点「启动环境」只会拿到服务端一句「环境已停用，请先启用再启动」，而客户端没有启用按钮
   ——一旦环境被停用，在客户端里就永远回不来。
2. 停用/删除只能 curl（控制台也没有入口，见「仍缺」）。
3. 每删一个环境，本机留下一个几十 MB 的 WebView2 profile 目录（`EBWebView`），只增不减。

### 23a. 客户端入口（`env_window.rs` + `index.html`）

- **状态分档**：`running` 只有「关闭环境」；`stopped` 只有「启用 + 删除」；
  `created`/`idle` 是「启动 + 停用 + 删除」；`stopped` 单独用告警色标签，不再冒充「空闲」。
- **删除必须二次确认**，文案写明三件事：环境行物理删除、出口交还代理商、**本机 profile 一并清理**
  ——这类操作不能让运营在「手滑」的代价里学会谨慎。
- **停用的本地守卫**：本机还开着该店铺的窗口时直接拒绝。服务端只按 `status == running` 判 409，
  看不见「状态已被巡检收回 idle，但客户端窗口与前置代理还在跑」这种组合；放过去就是
  「环境已停用、窗口和代理进程还在跑」的孤儿。**已知边界**：这份登记表是进程内的，
  多开客户端实例时看不到别的实例的窗口，跨实例只有服务端 409 兜底。

### 23b. 删除链的顺序与路径安全

- **顺序**：先收本机窗口（关窗口那条路径会上报 stop，running→idle）→ 删服务端行 → 清本机目录。
  反过来就是「本机数据没了、服务端还挂着一个环境」，两边对不上；而 running 时服务端本来就 409。
- **env_id 只从服务端取**：`GET /environment` → `id` 就是 profile 目录名。让 UI 传「要删哪个目录」，
  等于把「按任意字符串删本机目录」的能力开放给前端层。
- **两道路径防线**：`is_safe_env_id`（仅字母数字 + `-`/`_`、长度 ≤64、非 `.`/`..`）
  + `canonicalize()` 后必须仍在 `profiles/` 根目录下。启动链路也加了同一条校验——
  env_id 会拼进 profile 路径，服务端虽然可信，但一个带 `../` 的 id 会让建目录/删目录跑到应用数据目录之外。
- **清目录带重试（5 × 250ms）**：WebView2 关窗后目录句柄晚几百毫秒才释放，第一次 `remove_dir_all`
  基本必失败；不重试的表现就是「环境删了、目录还在」，而且没人会注意到。
- **清理失败不阻塞删除**（删除本身已不可逆），但结果如实回传：`profile_purged` + `note`，
  UI 侧未清理时用红色显示，不静默。

### 23c. 顺带修掉一个打包期才会炸的隐患

`tauri.conf.json` 的 CSP `connect-src` 缺 `ipc: http://ipc.localhost`——第 22b 条只补了
`index.html` / `browser.html` 的 meta，配置里那份没动。dev 模式（devUrl）下生效的是 meta，
所以看不出来；release（`tauri://` 协议 + 配置 CSP 头）下主控页与工具条的 `invoke` 会全哑，
和第二轮撞的坑同一个成因，只是藏得更深。已补齐。

### 23d. 实测（真跑，非推断）

- Rust 单测（`cargo test --lib`）：env_id 目录名安全（`../`、分隔符、65 字符、`.`,`..` 全拒）、
  profile 目录永不越出根、地址栏缺 scheme 时补 https。
- `cargo build` 通过（含新命令 `deactivate_env` / `delete_env` 与 capabilities 校验）。
- **新增 `scripts/e2e-lifecycle.ps1`**（内存存储 + seed，真客户端进程），15 条断言全 PASS：
  启动（壳窗口标题 + 服务端 running + 本机 profile 落地）→ 关闭上报回 idle → 停用 stopped →
  停用后拿不到启动单（409）→ 启用回 idle → **同进程「窗口还开着就停用」被客户端拒绝** →
  删除（`profile_purged=true`、GET 404、目录真的不存在）→ 重复删除如实报「该店铺还没有环境」。
- CLI 补 `--deactivate-env` / `--delete-env`（与 `--launch-env` 同一套解析，仍然只认两个独立参数）。

### 23e. harness 自己踩的两个坑（都写进脚本注释，避免下次再花时间）

1. **PowerShell 5 的 `Get-Content` 默认按 ANSI(GBK) 解码**：客户端日志是 UTF-8，中文全成乱码，
   `-match '已打开环境窗口'` 永远不成立 ⇒ 5 条用例假失败。要 `-Encoding UTF8`。
2. `[IO.File]::ReadAllText` 读 `RedirectStandardError` 正在写的文件会撞共享冲突、被 `catch`
   吞成空串——表现为「客户端一句日志都没有」，比乱码更容易误导。`Get-Content` 带读共享，能读。
   （另：函数里的 `$script:fail++` 没进到脚本作用域的计数里，末尾照样打印 `ALL_PASS`——
   这条差点把 5 个 FAIL 报成全绿，比用例本身更值得记；改用 `$global:e2eFail`。）

### 23f. 仍缺（登记）

- **控制台（`gateway/web`）同样没有环境生命周期入口**，只有 API——与客户端这一轮同源，
  下次一起补（`env:deactivate` / `env:delete` 只有老板与运营主管有，控制台的展示位置要先定）。
- **`env.deleted` 事件仍无客户端消费**：现在只有「本机自己点删除」才清 profile；
  别的管理员在控制台/别的机器删掉环境时，本机 profile 仍残留。闭环需要客户端订阅
  通知/事件流（目前客户端只轮询通知，不消费事件），属于新能力，单列。
- 多开客户端实例时本机窗口登记不共享（见 23a 的边界）。

