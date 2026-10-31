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
- **仍缺**：仅站内 + 控制台两通道；邮件/短信/Webhook 需新增 Sender 实现。

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
- **买真代理前的硬前置**：绑定关系目前在**分配器内存**里，服务重启后环境会拿到
  新出口 IP（有告警，但没解决"不换"）。上真账号前必须落库持久化
  （建议 shop_lookup 表 + memory/PG 双实现），否则每次发布/deploy 都会让店铺换 IP。
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
| — | 临时授权到期自动回收（定时任务） | 数据模型已支持，任务未注册 |
