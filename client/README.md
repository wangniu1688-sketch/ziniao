# 客户端骨架（Tauri 2 + Rust）

> ⚠️ **状态：骨架，未编译验证**。本机（Windows）未安装 Rust / Node 工具链，
> 因此本目录只交付「目录结构 + 关键实现思路 + 可直接落地的注入脚本」，
> 不含构建产物。安装 Rust 后按文末命令即可构建。

## 职责边界（必须与服务端对齐）

| 能力 | 服务端（Go） | 客户端（Rust/JS） |
|---|---|---|
| 画像采样与自洽性体检 | ✅ `fingerprint` 包 | 只接收结果 |
| 注入脚本生成 | ✅ `fingerprint.BuildInjectionPlan` | 在文档创建前执行 |
| TLS 指纹 | ✅ uTLS 前置代理（`network.TLSProxy`） | 进程内启动 / 旁挂 sidecar |
| 代理出口与纯净度 | ✅ `network` 包 | 上报观测值 |
| 环境自检 | ✅ `shop.EnvService.Check` | 采集 `navigator` 观测值上报 |
| 离线启动校验 | ✅ 基线哈希比对（L8） | 启动前上报 `baseline_hash` |
| 凭证明文 | ✅ 信封加密，按需下发 | **绝不落盘** |
| 权限三层 | ✅ 服务端裁决 | 第三层 DOM 注入按 `/api/v1/iam/dom-policy` 执行 |

铁律：**客户端是执行器，不是决策器**。任何"客户端自己判断能不能做"的实现都是安全漏洞（L3/L6）。

## 目录

```
client/
├── index.html                 # 最小界面：登录 → 环境列表 → 启动
├── src/main.js                # 前端逻辑（调用云端 API）
├── inject/fingerprint.js      # 注入脚本（在文档创建前执行，服务端下发为准）
└── src-tauri/
    ├── Cargo.toml
    ├── tauri.conf.json
    └── src/main.rs            # Rust 外壳：拉注入计划、管本地代理、上报自检
```

## 关键实现点

### 1. 注入时机（决定指纹能不能被绕过）
`inject/fingerprint.js` 必须在**文档创建前**通过 `Page.addScriptToEvaluateOnNewDocument`（CDP）
或 Tauri 的 `initialization_script` 执行，否则 `navigator.userAgent` 已被页面读取，
后出现"到底该信哪个"的自相矛盾（对标文档 M0.1）。

### 2. 代理与指纹同源（L2 / 致命项）
Rust 侧只负责启动本地前置代理（`ziniao-proxy` sidecar，Go 编译），
**不自己构造 TLS**——保证"浏览器声称的内核版本" 与 "实际 TLS 指纹" 出自同一份基线，
避免 TLS 与声称版本脱钩。

### 3. 自检数据回传
启动环境后采集 `navigator.*`（UA / platform / 时区 / 语言 / 屏幕 / WebGL / Canvas / Audio 噪声）
作为 `observed` 上报 `POST /api/v1/shops/{id}/environment/check`，
服务端计算自洽性、漂移与基线校验，返回体检报告。

## 构建（需先安装 Rust 1.77+ / Node 18+）

```bash
cd client
npm install
npm run tauri build
```
