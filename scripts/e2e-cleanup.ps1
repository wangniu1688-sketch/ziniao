$ErrorActionPreference = 'SilentlyContinue'
Stop-Process -Name 'ziniao-server-test' -Force
Get-Process node | Where-Object { $_.CommandLine -match 'dev-server' } | Stop-Process -Force
Start-Sleep 1
Remove-Item 'g:\ziniao\.e2e-*' -Force
Remove-Item 'g:\ziniao\ziniao-server-test.exe' -Force
Write-Output 'cleaned'
