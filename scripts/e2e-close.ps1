$ErrorActionPreference = 'SilentlyContinue'
$pidText = [IO.File]::ReadAllText('g:\ziniao\.e2e-pid').Trim()
taskkill /PID $pidText | Out-Null
Start-Sleep 6
# 若还没退出（窗口被吞掉 WM_CLOSE 的情况）再强杀
taskkill /PID $pidText /F 2>&1 | Out-Null
Write-Output 'closed'
