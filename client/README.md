# 紫鸟 Windows 客户端（Tauri 2）

当前客户端不是已验收的正式产品，但仓库已有 Tauri 2 / Rust 桌面外壳、店铺与环境管理界面、环境窗口生命周期处理，以及 Go 代理 sidecar 的启动入口。**是否能在干净 Windows 环境安装并完成真实代理链路，必须以 Windows 构建和端到端验收结果为准。**

## 首版交付范围

- 登录并连接 Go 服务端。
- 加载店铺及环境列表。
- 创建、启动、关闭、启用、停用和删除环境。
- 为每个环境使用独立 WebView2 profile。
- 启动环境时尝试拉起同环境代理配置的 `ziniao-proxy.exe`。
- 展示服务端返回的环境状态及站内通知。

首版暂不以 Chromium 内核深度定制、复杂趋势报表和高级指纹能力作为发布门槛。任何代理链路未真实验证的能力，不得在产品页面宣称已可用。

## Windows 构建

GitHub Actions 已增加 Windows 安装包构建工作流：

- 工作流：`.github/workflows/windows-client.yml`
- 触发：推送或 PR 修改客户端、代理 sidecar 或构建工作流；也支持手动触发。
- 构建步骤：Windows runner → Go 编译 `cmd/ziniao-proxy` → `npm ci` → 前端静态资源构建 → Tauri NSIS 安装包。
- 成功产物：Actions Artifact `ziniao-windows-nsis`（保留 14 天）。

查看 [Windows Client Build 工作流](../actions/workflows/windows-client.yml)。

本机手动构建需要 Windows C++ Build Tools、Rust MSVC 工具链、Node.js 22、Go。构建前先生成 sidecar：

```powershell
New-Item -ItemType Directory -Force client/src-tauri/sidecar
go build -trimpath -o client/src-tauri/sidecar/ziniao-proxy.exe ./cmd/ziniao-proxy
cd client
npm ci
npm run build
npm run tauri build
```

安装包默认输出到 `client/src-tauri/target/release/bundle/nsis/`。

## 关键文件

| 文件 | 职责 |
|---|---|
| `client/index.html` | 登录、店铺/环境列表、通知中心 |
| `client/src-tauri/src/lib.rs` | Tauri 命令注册、应用入口 |
| `client/src-tauri/src/env_window.rs` | 环境窗口、profile、代理 sidecar 与生命周期 |
| `client/src-tauri/tauri.conf.json` | 桌面窗口、CSP、NSIS 与 sidecar 资源配置 |
| `cmd/ziniao-proxy/main.go` | 本地代理 sidecar 入口 |
| `client/scripts/build-static.mjs` | 生成 Tauri 前端静态目录 |

## 发布前必须通过的 Windows 验收

1. Windows 10/11 干净环境安装、启动、卸载；确认 WebView2 依赖提示清晰。
2. 登录真实测试服务端，验证错误密码、服务端不可达、会话过期时的提示。
3. 创建环境并启动两个不同店铺，确认窗口与 profile 隔离。
4. 确认安装包内 sidecar 确实存在并可启动；分别用真实 HTTP、HTTPS、SOCKS5 上游代理验证出口 IP。
5. 关闭窗口后确认代理子进程退出、服务端环境状态回到 idle。
6. 验证通知加载、刷新环境、删除环境及本机 profile 清理。
7. 不使用仓库内演示账号作为正式默认凭据；测试账号和服务地址必须由部署环境提供。

**注意：** CI 构建成功只证明 Windows 安装包能编译出来，不等于上述运行验收已通过，更不等于已可向卖家正式收费。
