# e2e 状态检查：客户端进程在不在 + 服务端环境状态
$ErrorActionPreference = 'Stop'
$pidText = [IO.File]::ReadAllText('g:\ziniao\.e2e-pid').Trim()
$proc = Get-Process -Id $pidText -ErrorAction SilentlyContinue
if ($proc) { Write-Output ('CLIENT alive, WS=' + $proc.MainWindowTitle) } else { Write-Output 'CLIENT dead' }
$token = [IO.File]::ReadAllText('g:\ziniao\.e2e-token').Trim()
$shop = [IO.File]::ReadAllText('g:\ziniao\.e2e-shop').Trim()
$h = @{ Authorization = 'Bearer ' + $token }
$env1 = Invoke-RestMethod -Uri ('http://127.0.0.1:8080/api/v1/shops/' + $shop + '/environment') -Headers $h
Write-Output ('ENV status=' + $env1.environment.status)
# 客户端窗口列表（环境窗口应有 title）
Get-Process | Where-Object { $_.MainWindowTitle -like '*环境*' } | ForEach-Object { Write-Output ('WIN: ' + $_.MainWindowTitle) }
