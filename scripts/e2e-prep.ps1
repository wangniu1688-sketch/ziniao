# e2e 准备：登录 → 取店 → 确保环境 → 落盘 token/shop
$ErrorActionPreference = 'Stop'
$login = Invoke-RestMethod -Method Post -Uri http://127.0.0.1:8080/api/v1/auth/login -ContentType 'application/json' -Body '{"username":"demo","password":"Zn@12345678","url":"/client"}'
$h = @{ Authorization = 'Bearer ' + $login.token }
$shops = (Invoke-RestMethod -Uri 'http://127.0.0.1:8080/api/v1/shops?limit=5' -Headers $h).items
$s = $shops[0]
Write-Output ('SHOP ' + $s.id + ' ' + $s.name)
try {
  Invoke-RestMethod -Method Post -Uri ('http://127.0.0.1:8080/api/v1/shops/' + $s.id + '/environment') -Headers $h -ContentType 'application/json' -Body '{"country":"US","url":"/client"}' | ConvertTo-Json -Compress
} catch {
  Write-Output ('ENV_CREATE: ' + $_.Exception.Message)
}
[IO.File]::WriteAllText('g:\ziniao\.e2e-token', $login.token)
[IO.File]::WriteAllText('g:\ziniao\.e2e-shop', $s.id)
Write-Output 'READY'
