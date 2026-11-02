# e2e：无人值守启动客户端并打开环境窗口，读回客户端日志
$ErrorActionPreference = 'Stop'
$token = [IO.File]::ReadAllText('g:\ziniao\.e2e-token').Trim()
$shop = [IO.File]::ReadAllText('g:\ziniao\.e2e-shop').Trim()
$p = Start-Process 'g:\ziniao\client\src-tauri\target\debug\ziniao-client.exe' -ArgumentList '--server','http://127.0.0.1:8080','--token',$token,'--launch-env',$shop -PassThru -WindowStyle Hidden -RedirectStandardOutput 'g:\ziniao\.e2e-client.out' -RedirectStandardError 'g:\ziniao\.e2e-client.err'
[IO.File]::WriteAllText('g:\ziniao\.e2e-pid', [string]$p.Id)
Start-Sleep 12
Write-Output '--- client stderr ---'
Get-Content 'g:\ziniao\.e2e-client.err'
Write-Output '--- client stdout ---'
Get-Content 'g:\ziniao\.e2e-client.out'
