# Phase 0 未实现项清单（不允许静默省略）

> 每条含：**状态 / 原因 / 何时做 / 预估工作量**。全部为显式 TODO，代码中已标注同名注释。

## P0 已实现

- [x] monorepo 骨架与模块目录边界
- [x] PostgreSQL schema + 可回滚迁移（`migrations/0001_phase0_core.up.sql` / `.down.sql`）
- [x] 事件总线（事务发件箱 + 派发器）→ 审计、通知同源派生
- [x] `iam.PolicyEngine` 五层裁决（功能/页面/元素/数据/资源）+ 义务（审批/MFA/水印）
- [x] 多租户隔离（tenant_id 全覆盖 + RLS fail-closed）
- [x] identity（注册/登录/登出/会话/登录保护/新终端识别）
- [x] 审计六大日志 + 哈希链 + 校验接口 + 导出管控（行数上限 + 二次审批）
- [x] 通知中心雏形（站内 + console 通道）
- [x] billing.Meter 统一计量 + Entitlement 配额/功能开关
- [x] OpenAPI（`/openapi.json`）、健康检查、限流
- [x] 单测 + HTTP 端到端（httptest，无需外部依赖）
- [x] 交付物：README / ADR / 权限矩阵 / 计费规则 / 演示脚本

## 未实现（按优先级）

| # | 项 | 原因 | 计划 | 预估 |
|---|---|---|---|---|
| 1 | Chromium fork 编译与 C++ 层指纹注入 | 需 depot_tools + 100GB 源码 + 数小时编译，本机不具备构建条件 | Phase 1 出 `kernel-patch/` patch 集与构建脚本（在有构建机的环境编译验证） | 15–25 人日 |
| 2 | TLS JA3/JA4 基线库与 uTLS 前置代理实现 | 按你的决策走 P0–P1 路线，本阶段未编码 | Phase 1：`fingerprint/tls-baseline/` + Go uTLS 代理 + 基线哈希校验 | 5–8 人日 |
| 3 | 设备画像模板库与自洽性引擎 | 属 Phase 1 核心（M0.3/环境自检页） | Phase 1：模板采样 + 自洽性评分 + 体检报告 | 10–15 人日 |
| 4 | PostgreSQL 集成测试 | 本机无 Docker / PostgreSQL，SQL 与驱动仅编译期验证 | 有 PG 环境后补 `store/pg` 集成测试与迁移回放测试 | 2 人日 |
| 5 | 应用数据库角色分离（NOBYPASSRLS） | 当前 DSN 用超级用户，RLS 可被 owner 绕过 | 随部署脚本一起交付（`app_rls` 角色） | 0.5 人日 |
| 6 | 云 KMS / 信封加密与密钥轮换 | 本阶段口令用 PBKDF2，凭证加密尚未接入 KMS | Phase 2（M2 账密托管）：SOPS/age 起步，P6 接云 KMS | 5 人日 |
| 7 | argon2id 替换 PBKDF2 | 零依赖优先于最优算法（ADR-0004） | P1，登录时静默重哈希 | 1 人日 |
| 8 | Kafka/RabbitMQ 作为事件传输层 | 本地一条命令起优先（ADR-0003） | Phase 2 替换传输层，Handler 接口不变 | 3 人日 |
| 9 | 审批工单与水印渲染（导出二次审批闭环） | 当前返回 202 + `approval_required`，工单系统未建 | Phase 3（M12 审批中心） | 5 人日 |
| 10 | 审计留存清理任务（差异化留存执行器） | 配置项已就位，调度器未建 | Phase 2（scheduler 模块） | 2 人日 |
| 11 | 审计全文检索（Elasticsearch） | 未引入 ES | Phase 2 | 5 人日 |
| 12 | 设备/网络/店铺/成员等模块的正式实现 | Phase 0 只建地基 + 最小店铺实体 | Phase 2（M2/M3） | 按模块 |
| 13 | 客户端壳层（Tauri）/ 移动端 / 小程序 | Phase 0 不含 | Phase 1 起 | 大 |
| 14 | 店铺生命周期状态机、账密托管、TOTP/Passkey 落地 | 属 M2/M3 | Phase 2 | 中 |
| 15 | 限流分布式化（Redis） | 当前为进程内令牌桶 | Phase 2 | 1 人日 |

## 明确的“假实现 / 降级实现”提示

- `mem` 驱动 `WithTx` **不支持回滚**（串行化 + 无撤销），仅 dev/test 使用。
- 通知外发当前只有 `console` 通道；邮件/短信/企微/飞书/钉钉为 TODO。
- 登录失败次数锁定未实现（需 Redis 计数），当前只做时段限制 + 新终端告警。
- 店铺为 Phase 0 最小实体（无生命周期状态机、无设备绑定），M2 在 Phase 2 实现。
