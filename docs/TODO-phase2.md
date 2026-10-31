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

## 4. PostgreSQL 未实跑（含新增的 0003 迁移）

- **状态**：0001/0002/0003 迁移与 pg 驱动均为编译期验证；`tenantRepo.List` 依赖
  `admin_list` 策略（`current_tenant() IS NULL`）放行定时任务与运营后台
- **原因**：本机无 Docker / PostgreSQL
- **解锁**：`docker compose up postgres` → `go run ./cmd/server -store postgres -migrate`

## 5. 审批工单的「审批人路由规则」—— 简化实现

- **状态**：当前只要有 `approval:approve` 权限且不是申请人即可审批
- **未做**：按金额/风险等级路由到不同审批人（如提现 > 阈值需老板审批）、多人会签、代理机构
- **原因**：需要组织架构与审批流配置能力，属运营后台范围
- **解锁**：引入 `approval_flows` 表 + 规则引擎

## 6. 审批工单的「待办通知」—— 仅站内 + 控制台

- **状态**：审批事件走事件总线，通知 Sink 只订阅 `risk.*` 等类型，审批待办未定向推送给审批人
- **解锁**：Notify Sink 增加 `iam.approval.*` 订阅 + 按角色解析审批人

## 7. 导出水印仅覆盖 CSV

- **状态**：明文导出的水印以 CSV 注释行承载（导出人/审批单/时间）
- **未做**：PDF / Excel 每页水印渲染
- **解锁**：引入表格与 PDF 渲染库（`excelize` / `gopdf`）

## 8. 未接入真实设备代理商 API

- **状态**：`network.Provider` 接口 + `MockProvider`
- **解锁**：按代理商文档实现 `Acquire/Renew/Recycle`

## 9. 其它登记项

| 模块 | 未实现 | 说明 |
|---|---|---|
| M3.3 | 云手机 / 云号购买与调度 | 仅数据模型 |
| M19 | Agent 人工确认闸门 | `AgentConfirmTTLSeconds` 已登记，执行器未接 |
| — | 事件总线 Kafka 传输层 | 当前进程内派发 |
| — | argon2id 替换 PBKDF2 | 待引入 `golang.org/x/crypto` |
| — | 临时授权到期自动回收（定时任务） | 数据模型已支持，任务未注册 |
