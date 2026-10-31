# 桌面客户端（浏览器）安装包构建指南

## 已实跑验证（2026-10-31，本机）

在这台 Windows 10 1909 机器上**已经真实构建出安装包**，产物：

```
client\src-tauri\target\release\ziniao-client.exe                      4.5 MB  单文件可执行
client\src-tauri\target\release\bundle\nsis\紫鸟浏览器_0.2.0_x64-setup.exe  1.7 MB  安装程序
```

双击安装程序即可安装到本机，桌面/开始菜单出现「紫鸟浏览器」。
客户端本身不含业务逻辑，运行时连接 `http://127.0.0.1:8080` 的控制台（可在顶部地址栏改，
或用环境变量 `ZINIAO_SERVER` 指定，连不上会给出启动提示）。

## 本机工具链位置（全部装在 D 盘，C 盘零写入）

| 组件 | 位置 | 说明 |
|---|---|---|
| Rust 1.99.1 | `D:\rust-home` | `RUSTUP_HOME=D:\rust-home\rustup`，`CARGO_HOME=D:\rust-home\cargo` |
| VS 2019 Build Tools | `D:\VS2019` | MSVC 14.29 + Windows 10 SDK 10.0.19041 |
| Node 22 / npm 10 | 系统已有 | 用于静态资源生成与 Tauri CLI |

## 为什么用 VS 2019 而不是 VS 2022

本机是 **Windows 10 1909（build 18363）**，VS 2022 最新引导器（17.14）在该版本上
**不安装 MSVC 工具集**（`.NET 4.8.1`、Win11 SDK 等组件判定 OS 不支持直接跳过，
最终 cl.exe 不落地）。VS 2019 Build Tools 支持 Win10 1909，MSVC 链接器正常可用，
Rust `x86_64-pc-windows-msvc` 工具链链接无问题。

## 为什么安装包是 NSIS(.exe) 而不是 MSI

WiX 的 MSI 数据库使用代码页 1252，**不支持中文产品名**（`LGHT0311` 报错）。
NSIS 格式原生支持 Unicode，因此打包目标选择 `nsis`。如确实需要 MSI，
把 `tauri.conf.json` 的 `productName` 改成纯 ASCII 即可。

## 重新构建

```bat
scripts\build-client-win.bat
```

脚本内容：先 `call D:\VS2019\VC\Auxiliary\Build\vcvars64.bat` 进入 MSVC 编译环境，
再设置 Rust 环境变量并执行 `npm run tauri build`。
首次构建约 10 分钟（编译依赖），增量重建约 3 分钟。

## 客户端红线（与服务端一致）

- 客户端只做执行，**不做权限决策**（L3/L6）：先问服务端，拿到审批/策略裁决才执行；
- TLS 指纹不在客户端构造，走 Go 侧 **uTLS 前置代理**（L2，致命项同源）；
- 凭证明文不落盘、不进日志（L4）。

## 已知边界

| 项 | 状态 |
|---|---|
| Tauri 2 外壳（窗口 / 独立安装包） | ✅ 已构建通过并可运行（WebView2 环境正常） |
| JS/CDP 指纹注入脚本 | 已交付（`client/inject/fingerprint.js`） |
| uTLS 前置代理 sidecar | 服务端已实现（`network`），客户端侧预留命令入口 |
| Chromium fork（C++ 层注入） | P1/P2 TODO，本机无构建环境（见 TODO-phase2.md 第 1 条） |
