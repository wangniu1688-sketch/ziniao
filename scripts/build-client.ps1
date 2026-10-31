# 一键构建桌面客户端安装包（Tauri 2 → .msi/.exe）
# 用法（在有 Rust + Node 的机器上）：
#   powershell -ExecutionPolicy Bypass -File scripts\build-client.ps1
# 产物位置：client\src-tauri\target\release\bundle\msi\*.msi

$ErrorActionPreference = "Stop"
$root = Split-Path -Parent $PSScriptRoot
Set-Location (Join-Path $root "client")

function Need($cmd, $hint) {
    if (-not (Get-Command $cmd -ErrorAction SilentlyContinue)) {
        Write-Host "缺少 $cmd。安装方式：$hint" -ForegroundColor Red
        exit 1
    }
}
Need node  "winget install OpenJS.NodeJS.LTS"
Need npm   "随 Node.js 一起安装"
Need cargo "winget install Rustlang.Rustup（然后 rustup default stable-msvc）"

# Windows 下 Tauri 还需要 WebView2（Win11 自带）与 VS C++ Build Tools：
#   winget install Microsoft.VisualStudio.2022.BuildTools --override "--add Microsoft.VisualStudio.Workload.VCTools"

Write-Host "==> 安装前端依赖" -ForegroundColor Cyan
npm install

Write-Host "==> 构建 Tauri 安装包（首次需编译 Rust，约 5-15 分钟）" -ForegroundColor Cyan
npm run tauri build

Write-Host ""
Write-Host "构建完成。安装包在：client\src-tauri\target\release\bundle\ 下（.msi / .exe）" -ForegroundColor Green
Write-Host "客户端默认连接 http://127.0.0.1:8080 ，可用环境变量 ZINIAO_SERVER 修改服务端地址。"
