@echo off
rem Desktop client build script (Windows / MSVC).
rem Steps: 1) build uTLS proxy sidecar  2) build frontend dist  3) build Tauri NSIS bundle.
rem Output: client\src-tauri\target\release\bundle\nsis\*-setup.exe
call "D:\VS2019\VC\Auxiliary\Build\vcvars64.bat" >nul
set RUSTUP_HOME=D:\rust-home\rustup
set CARGO_HOME=D:\rust-home\cargo
set PATH=D:\rust-home\cargo\bin;C:\Users\Administrator\tools\go\bin;%PATH%

cd /d %~dp0..
echo [1/3] building ziniao-proxy (uTLS sidecar)
if not exist client\src-tauri\sidecar mkdir client\src-tauri\sidecar
go build -o client\src-tauri\sidecar\ziniao-proxy.exe .\cmd\ziniao-proxy
if errorlevel 1 (
  echo BUILD_EXIT=%ERRORLEVEL%
  exit /b %ERRORLEVEL%
)

echo [2/3] building frontend dist
cd /d %~dp0..\client
call npm run build

echo [3/3] building tauri bundle
call npm run tauri build
echo BUILD_EXIT=%ERRORLEVEL%
