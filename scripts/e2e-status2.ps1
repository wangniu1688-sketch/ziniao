$ErrorActionPreference = 'Stop'
$token = [IO.File]::ReadAllText('g:\ziniao\.e2e-token').Trim()
$shop = [IO.File]::ReadAllText('g:\ziniao\.e2e-shop').Trim()
$raw = curl.exe -s -H ('Authorization: Bearer ' + $token) ('http://127.0.0.1:8080/api/v1/shops/' + $shop + '/environment')
$m = [regex]::Matches($raw, '"status":"[a-z]+"')
Write-Output ($m | ForEach-Object { $_.Value })
