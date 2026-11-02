$ErrorActionPreference = 'Stop'
$token = [IO.File]::ReadAllText('g:\ziniao\.e2e-token').Trim()
$shop = [IO.File]::ReadAllText('g:\ziniao\.e2e-shop').Trim()
$r = Invoke-WebRequest -UseBasicParsing -Uri ('http://127.0.0.1:8080/api/v1/shops/' + $shop + '/environment') -Headers @{ Authorization = ('Bearer ' + $token) }
$j = $r.Content | ConvertFrom-Json
Write-Output ('keys: ' + ($j.PSObject.Properties.Name -join ','))
Write-Output ('status: ' + $j.environment.status)
