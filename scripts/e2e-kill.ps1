$ErrorActionPreference = 'SilentlyContinue'
$pidText = [IO.File]::ReadAllText('g:\ziniao\.e2e-pid').Trim()
Stop-Process -Id $pidText -Force
Start-Sleep 1
Write-Output ('killed ' + $pidText)
