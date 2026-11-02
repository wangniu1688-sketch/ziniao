# 环境生命周期闭环 e2e：启动 → 拒绝停用（窗口还在本机）→ 关闭上报 → 停用 → 启用 → 删除 + 清本机 profile
#
# 为什么单独一个脚本：这条链里三个动作（停用 / 启用 / 删除）此前只有服务端接口与单测，
# 客户端侧一个入口都没有；删除还牵扯「本机 profile 目录」这件服务端删不到的事，
# 只能真跑一遍才知道有没有漏。
#
# 依赖（缺一不可）：
#   1) %TEMP%\ziniao-e2e-server.exe（go build -o %TEMP%\ziniao-e2e-server.exe ./cmd/server）
#   2) dev-server 在 :1420 托管 client/dist（debug 构建走 devUrl，不起 dev server 会 404）
#   3) 已构建的 debug 客户端 client\src-tauri\target\debug\ziniao-client.exe
param(
    [string]$ServerExe = (Join-Path $env:TEMP 'ziniao-e2e-server.exe'),
    [string]$Client = 'g:\ziniao\client\src-tauri\target\debug\ziniao-client.exe',
    [int]$Port = 8080
)
$ErrorActionPreference = 'Stop'
$base = "http://127.0.0.1:$Port"
$profRoot = Join-Path $env:LOCALAPPDATA 'com.ziniao.browser\profiles'
$global:e2eFail = 0
$run = 0

function Step($m) { Write-Output ''; Write-Output ('=== ' + $m + ' ===') }
function Check($ok, $msg) {
    if ($ok) { Write-Output ('PASS ' + $msg) } else { Write-Output ('FAIL ' + $msg); $global:e2eFail++ }
}
function WaitHealth($u) {
    for ($i = 0; $i -lt 60; $i++) {
        Start-Sleep -Milliseconds 400
        try { $null = Invoke-RestMethod -Uri $u; return $true } catch { }
    }
    return $false
}
# EnvRow 环境行；404 返回 $null（删除后才应该走到这里）
function EnvRow($shopId, $h) {
    try { return Invoke-RestMethod -Uri "$base/api/v1/shops/$shopId/environment" -Headers $h } catch { return $null }
}
# RunClient 起一个客户端实例跑 CLI 动作，等窗口起来后读 stderr（客户端日志都走 eprintln）
function RunClient([string[]]$extraArgs, [string]$tag, [int]$wait = 14) {
    # 文件名单调递增而不是先删后建：这台机器上的删除操作会被安全钩子拦成 fail-closed，
    # 而且「上一次的日志还在」本身也不影响判定（每次都是新文件名）。
    $script:run++
    $out = Join-Path $env:TEMP "ziniao-$tag-$($script:run).out"
    $err = Join-Path $env:TEMP "ziniao-$tag-$($script:run).err"
    $p = Start-Process $Client -ArgumentList $extraArgs -PassThru `
        -RedirectStandardOutput $out -RedirectStandardError $err
    Start-Sleep -Seconds $wait
    $title = ''
    try { $title = (Get-Process -Id $p.Id).MainWindowTitle } catch { }
    # 必须显式按 UTF-8 读：客户端日志是 UTF-8，PowerShell 5 的 Get-Content 默认按 ANSI(GBK)
    # 解码，中文全成乱码，`-match '已打开环境窗口'` 也就永远匹配不上（用例假失败）。
    $text = ''
    try { $text = (Get-Content $err -Raw -Encoding UTF8) } catch { }
    return [pscustomobject]@{ Id = $p.Id; Title = $title; Err = $text; Proc = $p }
}
function CloseClient($c) {
    try {
        $p = Get-Process -Id $c.Id -ErrorAction Stop
        $null = $p.CloseMainWindow()
        if (-not $p.WaitForExit(15000)) { $p.Kill() }
    } catch { }
}

$srv = $null
try {
    Step '0) 起服务端（内存存储 + seed）'
    $srv = Start-Process $ServerExe -ArgumentList '-addr', ":$Port", '-store', 'memory', '-seed' -PassThru -WindowStyle Hidden `
        -RedirectStandardOutput (Join-Path $env:TEMP 'ziniao-e2e-server.out') `
        -RedirectStandardError (Join-Path $env:TEMP 'ziniao-e2e-server.err')
    if (-not (WaitHealth "$base/healthz")) { throw '服务端未就绪' }
    Write-Output 'server ready'

    $login = Invoke-RestMethod -Method Post -Uri "$base/api/v1/auth/login" -ContentType 'application/json' `
        -Body '{"username":"demo","password":"Zn@12345678","url":"/client"}'
    $h = @{ Authorization = 'Bearer ' + $login.token }
    $shop = (Invoke-RestMethod -Uri "$base/api/v1/shops?limit=5" -Headers $h).items[0]
    if (-not $shop) { throw 'seed 里没有店铺' }
    Write-Output ('SHOP ' + $shop.id + ' ' + $shop.name)

    $envRow = EnvRow $shop.id $h
    if (-not $envRow) {
        $null = Invoke-RestMethod -Method Post -Uri "$base/api/v1/shops/$($shop.id)/environment" -Headers $h `
            -ContentType 'application/json' -Body '{"country":"US","url":"/client"}'
        $envRow = EnvRow $shop.id $h
    }
    $envId = $envRow.id
    $profDir = Join-Path $profRoot $envId
    Write-Output ('ENV ' + $envId + ' status=' + $envRow.status + ' profile=' + $profDir)
    Check ($envRow.status -ne 'running') '起始状态不是 running（不会误判后面的用例）'

    Step '1) 启动：壳窗口打开 + 服务端 running + 本机 profile 落地'
    $a = RunClient @('--server', $base, '--token', $login.token, '--launch-env', $shop.id) 'launch'
    Write-Output ('client stderr: ' + ($a.Err -replace "`r?`n", ' | '))
    Write-Output ('window title: ' + $a.Title)
    Check ($a.Err -match '已打开环境窗口') '客户端报告已打开环境窗口'
    Check ($a.Title -match '环境') '壳窗口标题带环境标识'
    Check ((EnvRow $shop.id $h).status -eq 'running') '服务端状态 running'
    Check (Test-Path $profDir) '本机 profile 目录已创建'

    Step '2) 关闭上报：窗口关掉后服务端必须回到 idle'
    CloseClient $a
    Start-Sleep -Seconds 4
    Check ((EnvRow $shop.id $h).status -eq 'idle') '关闭后状态回到 idle（Destroyed 上报闭环）'

    Step '3) 停用：stopped + 服务端归还出口（无出口池时只改状态）'
    $b = RunClient @('--server', $base, '--token', $login.token, '--deactivate-env', $shop.id) 'deactivate'
    Write-Output ('client stderr: ' + ($b.Err -replace "`r?`n", ' | '))
    CloseClient $b
    Check ($b.Err -match '停用环境成功') '客户端停用成功'
    Check ((EnvRow $shop.id $h).status -eq 'stopped') '服务端状态 stopped'

    Step '4) 停用后不能启动（服务端拒绝），启用后回到 idle'
    $launchSt = 0
    try {
        $null = Invoke-RestMethod -Method Post -Uri "$base/api/v1/shops/$($shop.id)/environment/launch" -Headers $h `
            -ContentType 'application/json' -Body '{"open_url":""}'
        $launchSt = 200
    } catch { $launchSt = [int]$_.Exception.Response.StatusCode }
    Check ($launchSt -ne 200) ('停用的环境拿不到启动单（HTTP ' + $launchSt + '）')
    $null = Invoke-RestMethod -Method Post -Uri "$base/api/v1/shops/$($shop.id)/environment/activate" -Headers $h `
        -ContentType 'application/json' -Body '{"url":"/client"}'
    Check ((EnvRow $shop.id $h).status -eq 'idle') '启用后回到 idle'

    Step '5) 同进程「窗口还开着就停用」必须被客户端自己拦住'
    $c = RunClient @('--server', $base, '--token', $login.token, '--launch-env', $shop.id, '--deactivate-env', $shop.id) 'guard'
    Write-Output ('client stderr: ' + ($c.Err -replace "`r?`n", ' | '))
    Check ($c.Err -match '请先关闭窗口再停用') '本机窗口开着时停用被拒绝（不留下孤儿窗口/代理进程）'
    Check ((EnvRow $shop.id $h).status -eq 'running') '被拒绝后服务端仍是 running（没有半途改状态）'
    CloseClient $c
    Start-Sleep -Seconds 4

    Step '6) 删除：先收窗口 → 删服务端行 → 清本机 profile'
    $d = RunClient @('--server', $base, '--token', $login.token, '--launch-env', $shop.id, '--delete-env', $shop.id) 'delete' 16
    Write-Output ('client stderr: ' + ($d.Err -replace "`r?`n", ' | '))
    CloseClient $d
    Start-Sleep -Seconds 2
    Check (($d.Err -match '删除环境') -and ($d.Err -match 'profile_purged=true')) '删除成功且 profile 已清理'
    Check ((EnvRow $shop.id $h) -eq $null) '服务端环境行已不存在（GET 404）'
    Check (-not (Test-Path $profDir)) '本机 profile 目录已删除'

    Step '7) 删除幂等性：再删一次应报「还没有环境」而不是静默成功'
    $e = RunClient @('--server', $base, '--token', $login.token, '--delete-env', $shop.id) 'delete-again' 12
    Write-Output ('client stderr: ' + ($e.Err -replace "`r?`n", ' | '))
    CloseClient $e
    Check ($e.Err -match '还没有环境') '重复删除如实报错（不假装成功）'
}
finally {
    foreach ($p in (Get-Process ziniao-client -ErrorAction SilentlyContinue)) { try { $p.Kill() } catch { } }
    if ($srv -and -not $srv.HasExited) { & taskkill /PID $srv.Id /T /F 2>$null | Out-Null }
}
Write-Output ''
if ($global:e2eFail -eq 0) { Write-Output 'E2E_RESULT ALL_PASS' } else { Write-Output ('E2E_RESULT FAIL ' + $global:e2eFail) }
exit $global:e2eFail
