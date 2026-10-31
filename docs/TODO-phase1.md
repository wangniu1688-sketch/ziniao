# Phase 1 未实现项清单（逐条登记，无静默省略）

> 原则：**不能做的必须写明，不能靠"省略"混过去**。以下每条都给出影响、原因与解锁条件。

## 1. Chromium fork（C++ 指纹注入）与 BoringSSL 改造 —— 未实现

- **状态**：P1/P2，本阶段不编码
- **影响**：无法覆盖极少数深入内核的检测（C++ 侧 `navigator` 实现、SSL 层细节）
- **原因**：本机无构建环境（Chromium 源码树约 100GB+，需 Linux + 数小时编译）
- **解锁**：Linux 构建机 + depot_tools；预估 20+ 人日
- **当前替代**：JS/CDP 注入（已实现）+ uTLS 前置代理（已实现）

## 2. Tauri/Rust 客户端未编译验证 —— 骨架

- **状态**：目录结构与关键实现已交付，未编译
- **影响**：不能实际启动桌面端
- **原因**：本机未安装 Rust / Node 工具链
- **解锁**：`rustup install` + `npm install` → `npm run tauri build`

## 3. TLS 基线未与真实 Chrome 校准 —— 已标记 `unverified`

- **状态**：基线数值来自公开指纹库参考值，JA3/JA4 由 uTLS 真实 ClientHello 计算（同源可比），
  但**未与真实 Chrome 抓包逐项比对**
- **影响**：`consistency` 报告中的 `unverified: ["ja3-match"]` 即此项；生产前必须校准
- **解锁**：采集真实 Chrome 115/120/131 的 ClientHello 覆盖 `tls_baseline.json`，
  并把 `source` 改为 `verified`

## 4. PostgreSQL 集成测试 —— 未跑

- **状态**：`migrations/0002_phase1_device_env`（含 RLS 与回滚脚本）与 pg 驱动均为编译期验证
- **影响**：生产 SQL 未经实跑
- **原因**：本机无 Docker / PostgreSQL
- **解锁**：`docker compose up postgres` → `go run ./cmd/server -store postgres -migrate`

## 5. 未接入真实设备代理商 API

- **状态**：`network/providers.go` 提供 `Provider` 接口 + `MockProvider`，真实代理商适配为 TODO
- **影响**：购买/续费/回收走本地模拟（返回住宅 IP 与 30 天有效期）
- **解锁**：按代理商文档实现 `Acquire/Renew/Recycle`，配置项注入凭据

## 6. 云手机 / 云号（M3.3）—— 仅数据模型

- **状态**：`DeviceType` 已定义（cloud_phone / cloud_number），无购买与调度实现
- **原因**：依赖外部供应商能力，需先确认合作方

## 7. 未实现的功能点（明确登记）

| 模块 | 未实现 | 说明 |
|---|---|---|
| M2.1 | 店铺回收站**定时清理** | 恢复/列表已实现，自动清理策略待定（留存期需业务确认） |
| M3.2 | 设备健康**自动巡检** | 评分已实现，定时巡检器待 scheduler 模块 |
| M14 | ~~审批工单流（二次审批）~~ → ✅ Phase 2 已落地 | 工单 CRUD + 审批人路由 + 会签 + 定向待办均已实现，见 `docs/TODO-phase2.md` 第 5/6 条 |
| M0.3 | 环境自检**页面** | 报告 API 已实现，前端页面属 Phase 2 |
| Q10 | KMS 接入 | 本地 KEK 已实现，云 KMS 待环境 |
| — | 事件总线 Kafka 传输层 | 当前进程内派发（事务发件箱已就位） |
| — | argon2id 替换 PBKDF2 | 待引入 `golang.org/x/crypto` |
