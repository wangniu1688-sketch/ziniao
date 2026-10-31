# Phase 0 acceptance demo script (Windows PowerShell 5+ compatible, ASCII only to avoid codepage issues)
# Usage: powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\demo.ps1 -Go "go"
param(
    [string]$Go = "go",
    [int]$Port = 18099
)

$ErrorActionPreference = "Stop"
$root = Split-Path -Parent (Split-Path -Parent $MyInvocation.MyCommand.Path)
Push-Location $root
$base = "http://127.0.0.1:$Port"

function Step($msg) { Write-Host ""; Write-Host "=== $msg ===" }
function Post($path, $body) {
    $json = $body | ConvertTo-Json -Compress
    $bytes = [System.Text.Encoding]::UTF8.GetBytes($json)
    return Invoke-RestMethod -Uri "$base$path" -Method Post -ContentType "application/json; charset=utf-8" -Body $bytes
}
function PostAuth($path, $token, $bodyObj) {
    $json = $bodyObj | ConvertTo-Json -Compress
    $bytes = [System.Text.Encoding]::UTF8.GetBytes($json)
    return Invoke-RestMethod -Uri "$base$path" -Method Post -ContentType "application/json; charset=utf-8" `
        -Headers @{ Authorization = "Bearer $token" } -Body $bytes
}
function PutAuth($path, $token, $bodyObj) {
    $json = $bodyObj | ConvertTo-Json -Compress
    $bytes = [System.Text.Encoding]::UTF8.GetBytes($json)
    return Invoke-RestMethod -Uri "$base$path" -Method Put -ContentType "application/json; charset=utf-8" `
        -Headers @{ Authorization = "Bearer $token" } -Body $bytes
}
function GetJson($path, $token) {
    return Invoke-RestMethod -Uri "$base$path" -Method Get -Headers @{ Authorization = "Bearer $token" }
}
# StatusOf 只取状态码：PowerShell 5 没有 -SkipHttpErrorCheck，用 try/catch 兜住非 2xx
function StatusOf($method, $path, $token, $body) {
    try {
        $params = @{ Uri = "$base$path"; Method = $method; Headers = @{ Authorization = "Bearer $token" }; UseBasicParsing = $true }
        if ($body) { $params.ContentType = "application/json"; $params.Body = $body }
        $r = Invoke-WebRequest @params
        return $r.StatusCode
    } catch {
        if ($_.Exception.Response) { return [int]$_.Exception.Response.StatusCode }
        return -1
    }
}

$proc = Start-Process -FilePath $Go -ArgumentList "run", "./cmd/server", "-addr", ":$Port", "-seed" -PassThru -WindowStyle Hidden
try {
    Step "waiting for server"
    $ready = $false
    for ($i = 0; $i -lt 60; $i++) {
        Start-Sleep -Milliseconds 500
        try { $null = Invoke-RestMethod -Uri "$base/healthz" -Method Get; $ready = $true; break } catch { }
    }
    if (-not $ready) { throw "server did not start; check that '$Go' is on PATH" }

    Step "1) login as owner (seed: demo / Zn@12345678)"
    $owner = Post "/api/v1/auth/login" @{ username = "demo"; password = "Zn@12345678" }
    $ownerToken = $owner.token
    Write-Host ("tenant={0} token_len={1}" -f $owner.tenant_id, $ownerToken.Length)

    Step "2) write operation: create shop"
    $shop = PostAuth "/api/v1/shops" $ownerToken @{ name = "demo-shop-amazon-us"; platform = "Amazon"; site = "US"; url = "/shops" }
    Write-Host ("shop id={0} status={1}" -f $shop.id, $shop.status)

    Start-Sleep -Seconds 1

    Step "3) audit log: this write must be recorded (actor -> shop -> terminal -> ip)"
    $logs = GetJson "/api/v1/audit/logs?action=shop.create" $ownerToken
    Write-Host ("total={0} action={1} shop_id={2} ip={3}" -f $logs.total, $logs.items[0].action, $logs.items[0].shop_id, $logs.items[0].terminal_ip)
    Write-Host ("hash={0}" -f $logs.items[0].hash)

    Step "4) hash chain verification"
    $vf = GetJson "/api/v1/audit/verify" $ownerToken
    Write-Host ("ok={0} checked={1}" -f $vf.ok, $vf.checked)

    Step "5) finance role: profit/cost/balance masked server-side (not just hidden in UI)"
    $fin = Post "/api/v1/auth/login" @{ username = "finance"; password = "Zn@12345678" }
    $shops = GetJson "/api/v1/shops" $fin.token
    Write-Host ("masked_fields={0} profit={1}" -f ($shops.items[0].masked_fields -join ","), $shops.items[0].profit)

    Step "6) DOM injection layer policy (3rd permission layer)"
    $dp = GetJson "/api/v1/iam/dom-policy?path=/finance" $fin.token
    Write-Host ("deny_selectors={0}" -f ($dp.deny_selectors -join " "))
    Write-Host ("mask_selectors={0}" -f ($dp.mask_selectors -join " "))

    Step "7) plaintext export requires 2nd approval (expect HTTP 202 + csv body)"
    $resp = Invoke-WebRequest -Uri "$base/api/v1/audit/logs/export" -Method Get -Headers @{ Authorization = "Bearer $ownerToken" } -UseBasicParsing
    Write-Host ("http_status={0} content_type={1}" -f $resp.StatusCode, $resp.Headers["Content-Type"])

    Step "8) quota: free plan cannot exceed shop limit (server-side, not a greyed button)"
    $free = Post "/api/v1/auth/register" @{
        name = "quota-team"; slug = "quota-team"; plan = "free";
        username = "quotaboss"; password = "Zn@12345678"; nickname = "quota"
    }
    Write-Host ("free tenant created: {0}" -f $free.tenant.id)

    # ---------------- Phase 1 ----------------
    Step "9) TLS baseline: JA3/JA4 computed from real uTLS ClientHello"
    $bl = GetJson "/api/v1/fingerprint/tls-baselines" $ownerToken
    Write-Host ("baseline_hash={0} computed={1} items={2}" -f $bl.baseline_hash, $bl.computed, $bl.items.Count)

    Step "10) create shop environment (profile + kernel + TLS baseline)"
    $env = PostAuth "/api/v1/shops/$($shop.id)/environment" $ownerToken @{ country = "US"; url = "/shops" }
    $bh = $env.environment.baseline_hash
    Write-Host ("env_id={0} kernel_major={1} baseline_hash={2}" -f $env.environment.id, $env.environment.kernel_major, $bh)

    Step "11) environment self-check: right baseline passes, tampered baseline is rejected"
    $ok = PostAuth "/api/v1/shops/$($shop.id)/environment/check" $ownerToken @{ baseline_hash = $bh }
    Write-Host ("right baseline -> passed={0} score={1}" -f $ok.passed, $ok.score)
    $bad = PostAuth "/api/v1/shops/$($shop.id)/environment/check" $ownerToken @{ baseline_hash = "tampered" }
    Write-Host ("tampered baseline -> passed={0} note={1}" -f $bad.passed, $bad.baseline_note)

    Step "12) credential vault: envelope encryption, plaintext never in list response"
    $null = PutAuth "/api/v1/shops/$($shop.id)/credentials" $ownerToken @{ kind = "password"; username = "seller1"; secret = "Zn@DemoSecret2026"; url = "/shops" }
    $kinds = GetJson "/api/v1/shops/$($shop.id)/credentials" $ownerToken
    Write-Host ("kinds={0}" -f ($kinds.kinds -join ","))

    Step "13) access policy: pre-action interception denies high risk element"
    $rule = @(@{ id = "r1"; type = "element"; pattern = '[data-el="shop.delete_button"]'; action = "deny"; severity = "high" })
    $null = PostAuth "/api/v1/security/policies" $ownerToken @{ name = "high risk guard"; scope = "shop"; scope_id = $shop.id; enabled = $true; rules = $rule; url = "/security" }
    $ic = PostAuth "/api/v1/security/intercept" $ownerToken @{ shop_id = $shop.id; element = '[data-el="shop.delete_button"]'; url = "/shops" }
    Write-Host ("allowed={0} action={1} rule={2}" -f $ic.allowed, $ic.action, $ic.rule_id)
    $ev = GetJson "/api/v1/security/events?limit=5" $ownerToken
    Write-Host ("security_events_total={0}" -f $ev.total)

    # ---------------- Phase 2 ----------------
    Step "14) approval ticket: submit -> self-approval rejected -> approved by another person"
    $null = PutAuth "/api/v1/shops/$($shop.id)/credentials" $ownerToken @{ kind = "password"; username = "seller1"; secret = "Zn@DemoSecret2026"; url = "/shops" }
    $op = Post "/api/v1/auth/login" @{ username = "operator"; password = "Zn@12345678" }
    $tk = PostAuth "/api/v1/approvals" $ownerToken @{ type = "shop.credential.reveal"; subject_type = "shop"; subject_id = $shop.id; reason = "demo"; url = "/approvals" }
    Write-Host ("ticket={0} status={1}" -f $tk.id, $tk.status)
    $selfStatus = StatusOf "POST" "/api/v1/approvals/$($tk.id)/approve" $ownerToken '{}'
    Write-Host ("self_approve_status={0} (expect 403: requester cannot approve own ticket)" -f $selfStatus)
    $ok = PostAuth "/api/v1/approvals/$($tk.id)/approve" $op.token @{ comment = "ok" }
    Write-Host ("approved_by={0} status={1}" -f $ok.approver_id, $ok.status)

    Step "15) credential reveal with ticket is one-time use"
    $sec = GetJson "/api/v1/shops/$($shop.id)/credentials/password?ticket=$($tk.id)" $ownerToken
    Write-Host ("secret={0}" -f $sec.secret)
    $reuseStatus = StatusOf "GET" "/api/v1/shops/$($shop.id)/credentials/password?ticket=$($tk.id)" $ownerToken $null
    Write-Host ("reuse_status={0} (expect 202: ticket consumed, a new approval is required)" -f $reuseStatus)

    Step "16) batch import: partial success returns 207"
    $imp = PostAuth "/api/v1/shops/batch/import" $ownerToken @{
        rows = @(
            @{ name = "batch-shop-1"; platform = "Amazon"; site = "US" },
            @{ name = "batch-shop-2"; platform = "Shopee"; site = "MY" },
            @{ name = ""; platform = "Amazon"; site = "US" }
        )
        url = "/shops"
    }
    Write-Host ("total={0} created={1} failed={2} reason={3}" -f $imp.total, $imp.created, $imp.failed, $imp.failures[0].reason)

    Step "17) run scheduled jobs manually (device inspect / trash purge / approval expiry)"
    $jr = PostAuth "/api/v1/jobs/run" $ownerToken @{}
    Write-Host ("job_results={0}" -f $jr.results.Count)
    foreach ($r in $jr.results) { Write-Host ("  {0}: scanned={1} affected={2}" -f $r.job, $r.scanned, $r.affected) }

    Step "demo finished"
}
finally {
    # /T 同时结束 go run 派生的子进程，否则端口会被遗留的 server 进程占用
    if ($proc -and -not $proc.HasExited) { & taskkill /PID $proc.Id /T /F 2>$null | Out-Null }
    Pop-Location
}
