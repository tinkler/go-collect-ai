# freshcheck W1 smoke (ASCII only, no Chinese, to avoid Windows PowerShell 5.1 encoding issues)
#
# Run: env FRESHCHECK_TEST_PG_DSN=... powershell -NoProfile -ExecutionPolicy Bypass -File e2e/freshcheck_smoke.ps1
#
# Pre-req: PG (5432) + cube-agent-server (8088) + collect-ai (8089) all running
# Exit: 0 = all PASS, 1 = at least one FAIL

$ErrorActionPreference = 'Stop'
$collectAIBase = 'http://127.0.0.1:8089/api/v1'

$Script:pass = 0
$Script:fail = 0
$Script:total = 0

function Test-Case {
    param([string]$Name, [scriptblock]$Block)
    $Script:total++
    try {
        & $Block
        Write-Host "[PASS] $Name" -ForegroundColor Green
        $Script:pass++
    } catch {
        Write-Host "[FAIL] $Name : $_" -ForegroundColor Red
        $Script:fail++
    }
}

Write-Host "================================================" -ForegroundColor Cyan
Write-Host "freshcheck W1 smoke" -ForegroundColor Cyan
Write-Host "================================================" -ForegroundColor Cyan
Write-Host ""

# 1) collect-ai health
Write-Host "=== [1] collect-ai health ===" -ForegroundColor Yellow

Test-Case "1.1 /api/v1/freshcheck/health 13 tables OK" {
    $token = (Invoke-RestMethod -Method POST 'http://127.0.0.1:8089/api/v1/auth/dev-login?as_user=u_owner').access_token
    $Script:ownerToken = $token
    $r = Invoke-RestMethod -Headers @{Authorization="Bearer $token"} "$collectAIBase/freshcheck/health"
    if (-not $r.ok) { throw "ok=false" }
    if ($r.count -ne 13) { throw "count=$($r.count), want 13" }
}

Test-Case "1.2 thresholds list (expect >= 11)" {
    $r = Invoke-RestMethod -Headers @{Authorization="Bearer $Script:ownerToken"} "$collectAIBase/freshcheck/config/thresholds"
    if ($r.count -lt 11) { throw "count=$($r.count), want >= 11" }
}

Test-Case "1.3 category-tracks list (expect >= 5)" {
    $r = Invoke-RestMethod -Headers @{Authorization="Bearer $Script:ownerToken"} "$collectAIBase/freshcheck/config/category-tracks"
    if ($r.count -lt 5) { throw "count=$($r.count), want >= 5" }
}

Test-Case "1.4 pool-codes list (empty OK, not configured yet)" {
    $r = Invoke-RestMethod -Headers @{Authorization="Bearer $Script:ownerToken"} "$collectAIBase/freshcheck/config/pool-codes"
    if ($null -eq $r.items) { throw "items missing" }
}

Test-Case "1.5 sku-map list (empty OK, not configured yet)" {
    $r = Invoke-RestMethod -Headers @{Authorization="Bearer $Script:ownerToken"} "$collectAIBase/freshcheck/config/sku-map"
    if ($null -eq $r.items) { throw "items missing" }
}

# 2) Auth + RBAC
Write-Host ""
Write-Host "=== [2] Auth + RBAC ===" -ForegroundColor Yellow

Test-Case "2.1 dev-login u_owner" {
    $r = Invoke-RestMethod -Method POST 'http://127.0.0.1:8089/api/v1/auth/dev-login?as_user=u_owner'
    if ([string]::IsNullOrEmpty($r.access_token)) { throw "no access_token" }
    $Script:ownerToken = $r.access_token
}

Test-Case "2.2 dev-login u_cashier (no freshcheck perm)" {
    $r = Invoke-RestMethod -Method POST 'http://127.0.0.1:8089/api/v1/auth/dev-login?as_user=u_cashier'
    if ([string]::IsNullOrEmpty($r.access_token)) { throw "no access_token" }
    $Script:cashierToken = $r.access_token
}

Test-Case "2.3 u_owner read thresholds OK" {
    $r = Invoke-RestMethod -Headers @{Authorization="Bearer $Script:ownerToken"} "$collectAIBase/freshcheck/config/thresholds"
    if ($r.count -lt 11) { throw "count=$($r.count)" }
}

Test-Case "2.4 u_cashier read thresholds should 403 (no freshcheck perm)" {
    try {
        Invoke-RestMethod -Headers @{Authorization="Bearer $Script:cashierToken"} "$collectAIBase/freshcheck/config/thresholds" | Out-Null
        throw "u_cashier should not have freshcheck:settle:read perm"
    } catch {
        if ($_.Exception.Response.StatusCode -ne 403) {
            throw "expect 403, got $($_.Exception.Response.StatusCode)"
        }
    }
}

Test-Case "2.5 u_cashier PUT pool-code should 403 (no freshcheck:config:write perm)" {
    $body = @{pool_name="1.00/jin test"; pricing_mode="weight"; unit_price=1.0; is_active=$true} | ConvertTo-Json
    try {
        Invoke-RestMethod -Method PUT -Headers @{Authorization="Bearer $Script:cashierToken"} `
            -ContentType "application/json" -Body $body `
            "$collectAIBase/freshcheck/config/pool-codes/POOL001" | Out-Null
        throw "u_cashier should not have freshcheck:config:write perm"
    } catch {
        if ($_.Exception.Response.StatusCode -ne 403) {
            throw "expect 403, got $($_.Exception.Response.StatusCode)"
        }
    }
}

# 3) Config CRUD
Write-Host ""
Write-Host "=== [3] Config CRUD end-to-end ===" -ForegroundColor Yellow

Test-Case "3.1 PUT pool-code (u_owner)" {
    $body = @{pool_name="1.00/jin test"; pricing_mode="weight"; unit_price=1.0; priority_rank=0; is_active=$true} | ConvertTo-Json
    $r = Invoke-RestMethod -Method PUT -Headers @{Authorization="Bearer $Script:ownerToken"} `
        -ContentType "application/json" -Body $body `
        "$collectAIBase/freshcheck/config/pool-codes/SMOKE001"
    if (-not $r.ok) { throw "ok=false" }
}

Test-Case "3.2 GET pool-code (verify write)" {
    $r = Invoke-RestMethod -Headers @{Authorization="Bearer $Script:ownerToken"} "$collectAIBase/freshcheck/config/pool-codes/SMOKE001?branch_no=0001"
    if ($r.pool_code -ne "SMOKE001") { throw "got $($r.pool_code)" }
    if ($r.unit_price -ne 1.0) { throw "unit_price=$($r.unit_price)" }
}

Test-Case "3.3 PUT sku-map (raw SKU + category)" {
    $body = @{item_name="test spinach"; fresh_category="leaf"; turnover_class="fast"; shelf_life_days=5; is_active=$true} | ConvertTo-Json
    $r = Invoke-RestMethod -Method PUT -Headers @{Authorization="Bearer $Script:ownerToken"} `
        -ContentType "application/json" -Body $body `
        "$collectAIBase/freshcheck/config/sku-map/SMOKE0001"
    if (-not $r.ok) { throw "ok=false" }
}

Test-Case "3.4 GET sku-map filter by leaf" {
    $r = Invoke-RestMethod -Headers @{Authorization="Bearer $Script:ownerToken"} "$collectAIBase/freshcheck/config/sku-map?fresh_category=leaf"
    if ($r.count -lt 1) { throw "count=$($r.count)" }
}

Test-Case "3.5 PUT threshold (change c1_pool_saturation_pct)" {
    $body = @{value=18.0} | ConvertTo-Json
    $r = Invoke-RestMethod -Method PUT -Headers @{Authorization="Bearer $Script:ownerToken"} `
        -ContentType "application/json" -Body $body `
        "$collectAIBase/freshcheck/config/thresholds/c1_pool_saturation_pct"
    if (-not $r.ok) { throw "ok=false" }
    if ($r.value -ne 18.0) { throw "value=$($r.value)" }
}

Test-Case "3.6 GET threshold confirm c1=18.0" {
    $r = Invoke-RestMethod -Headers @{Authorization="Bearer $Script:ownerToken"} "$collectAIBase/freshcheck/config/thresholds"
    $c1 = $r.items | Where-Object { $_.key -eq "c1_pool_saturation_pct" }
    if ($c1.value -ne 18.0) { throw "c1=$($c1.value), want 18.0" }
}

Test-Case "3.7 PUT threshold restore c1=15.00 (snap audit)" {
    $body = @{value=15.00} | ConvertTo-Json
    Invoke-RestMethod -Method PUT -Headers @{Authorization="Bearer $Script:ownerToken"} `
        -ContentType "application/json" -Body $body `
        "$collectAIBase/freshcheck/config/thresholds/c1_pool_saturation_pct" | Out-Null
}

# 4) Integration test
Write-Host ""
Write-Host "=== [4] Integration test (PG required) ===" -ForegroundColor Yellow

Test-Case "4.1 run 12 store_pg_test" {
    if (-not $env:FRESHCHECK_TEST_PG_DSN) {
        throw "FRESHCHECK_TEST_PG_DSN not set, cannot run integration test"
    }
    Push-Location (Split-Path $PSScriptRoot)
    try {
        $out = go test -count=1 -v ./internal/freshcheck/... 2>&1 | Out-String
    } finally {
        Pop-Location
    }
    if ($LASTEXITCODE -ne 0) {
        Write-Host "    test output:" -ForegroundColor Gray
        Write-Host "    $out" -ForegroundColor Gray
        throw "go test exit code $LASTEXITCODE"
    }
    if ($out -notmatch "ok\s+github.com/tinkler/collect-ai/internal/freshcheck") {
        throw "test failed: $out"
    }
}

# ============== [5] W2 池事件端到端 ==============
Write-Host ""
Write-Host "=== [5] W2 pool events (PG required) ===" -ForegroundColor Yellow

# 5.1 seed W2 测试数据 (pool + sku)
Test-Case "5.1 seed W2 pool + sku" {
    Push-Location (Split-Path $PSScriptRoot)
    $out = go run ./scripts/tmp/seed_w2_smoke.go 2>&1 | Out-String
    Pop-Location
    if ($out -notmatch "seed OK") {
        throw "seed failed: $out"
    }
}

Test-Case "5.2 POST /pool/events/in 201" {
    $r = Invoke-RestMethod -Method POST -Headers @{Authorization="Bearer $Script:ownerToken"} `
        -ContentType "application/json" `
        -Body '{"pool_code":"W2TEST1","item_no":"W2SKU1","weight_kg":1.5,"operator":"u_floor"}' `
        "$collectAIBase/freshcheck/pool/events/in"
    if ($r.id -le 0) { throw "id should be set, got $($r.id)" }
    if ($r.pool_state_after.in_total_kg -lt 1.0) { throw "in_total_kg should >= 1.0" }
}

Test-Case "5.3 GET /pool/state 200" {
    $r = Invoke-RestMethod -Headers @{Authorization="Bearer $Script:ownerToken"} `
        "$collectAIBase/freshcheck/pool/state?pool_code=W2TEST1"
    if ($r.items.Count -lt 1) { throw "items count should >= 1" }
}

Test-Case "5.4 POST /pool/events/out 触发 missing_in" {
    # W2SKU2 之前没录 in, 现在录 out → missing
    $body = '{"pool_code":"W2TEST1","operator":"u_floor","items":[{"item_no":"W2SKU2","weight_kg":0.3,"out_destination":"sold_out"}]}'
    $r = Invoke-RestMethod -Method POST -Headers @{Authorization="Bearer $Script:ownerToken"} `
        -ContentType "application/json" -Body $body `
        "$collectAIBase/freshcheck/pool/events/out"
    if ($r.events_created -ne 1) { throw "events_created should 1" }
    if ($r.missing_in_records.Count -ne 1) { throw "missing_in should detect W2SKU2" }
    if ($r.missing_in_records[0].item_no -ne "W2SKU2") { throw "missing item should W2SKU2" }
}

Test-Case "5.5 GET /pool/diff 算 box_loss" {
    $r = Invoke-RestMethod -Headers @{Authorization="Bearer $Script:ownerToken"} `
        "$collectAIBase/freshcheck/pool/diff?pool_code=W2TEST1&pos_sales_kg=1.0"
    if ($r.in_total_kg -lt 1.0) { throw "in_total_kg should >= 1.0" }
}

Test-Case "5.6 RBAC: u_cashier 录池事件 403" {
    $body = '{"pool_code":"W2TEST1","item_no":"W2SKU1","weight_kg":0.1,"operator":"u_cashier"}' | ConvertTo-Json
    try {
        Invoke-RestMethod -Method POST -Headers @{Authorization="Bearer $Script:cashierToken"} `
            -ContentType "application/json" -Body $body `
            "$collectAIBase/freshcheck/pool/events/in" | Out-Null
        throw "u_cashier should not have freshcheck:pool:write perm"
    } catch {
        if ($_.Exception.Response.StatusCode -ne 403) {
            throw "expect 403, got $($_.Exception.Response.StatusCode)"
        }
    }
}

Test-Case "5.7 cleanup W2 seed data" {
    Push-Location (Split-Path $PSScriptRoot)
    $out = go run ./scripts/tmp/seed_w2_smoke.go -cleanup 2>&1 | Out-String
    Pop-Location
    if ($out -notmatch "cleanup OK") {
        throw "cleanup failed: $out"
    }
}

# ============== [6] W3.1 周期盘点端到端 ==============
Write-Host ""
Write-Host "=== [6] W3.1 period stock ===" -ForegroundColor Yellow
$w3Period = 20990101

Test-Case "6.0 seed W3.1 sku+pool idempotent" {
    Push-Location (Split-Path $PSScriptRoot)
    $out = go run ./scripts/tmp/seed_w3_smoke.go 2>&1 | Out-String
    Pop-Location
    if ($out -notmatch "seed OK") { throw "seed failed: $out" }
}

Test-Case "6.1 POST /period/stock 创建 (SKU 合法)" {
    $body = @{
        period_id = $w3Period
        item_no   = "W3SKU1"
        item_name = "smoke cabbage"
        qty       = 12.5
        unit      = "kg"
        operator  = "u_floor"
        note      = "w3.1 smoke"
    } | ConvertTo-Json
    $r = Invoke-RestMethod -Method POST -Headers @{Authorization="Bearer $Script:ownerToken"} `
        -ContentType "application/json" -Body $body `
        "$collectAIBase/freshcheck/period/stock"
    if ($r.id -le 0) { throw "id should be set, got $($r.id)" }
    if ($r.item_no -ne "W3SKU1") { throw "item_no = $($r.item_no)" }
    if ($r.qty -ne 12.5) { throw "qty = $($r.qty)" }
    if ($r.confidence -ne "high") { throw "confidence = $($r.confidence), want high" }
    $Script:w3StockId = $r.id
}

Test-Case "6.1b POST /period/stock C8 reject pool_code" {
    $body = @{
        period_id = $w3Period
        item_no   = "W3POOL1"
        qty       = 1.0
        unit      = "kg"
        operator  = "u_floor"
    } | ConvertTo-Json
    try {
        Invoke-RestMethod -Method POST -Headers @{Authorization="Bearer $Script:ownerToken"} `
            -ContentType "application/json" -Body $body `
            "$collectAIBase/freshcheck/period/stock" | Out-Null
        throw "C8 校验应拒特价码 W3POOL1, 但成功录入"
    } catch {
        if ($_.Exception.Response.StatusCode -ne 400) {
            throw "expect 400, got $($_.Exception.Response.StatusCode)"
        }
    }
}

Test-Case "6.2 GET /period/stock list by period" {
    $r = Invoke-RestMethod -Headers @{Authorization="Bearer $Script:ownerToken"} `
        "$collectAIBase/freshcheck/period/stock?period_id=$w3Period"
    if ($r.count -lt 1) { throw "count = $($r.count), want >= 1" }
    if ($r.items[0].item_no -ne "W3SKU1") { throw "first item = $($r.items[0].item_no)" }
    $Script:w3StockId = $r.items[0].id
}

Test-Case "6.3 GET /period/stock/coverage C8 check" {
    $r = Invoke-RestMethod -Headers @{Authorization="Bearer $Script:ownerToken"} `
        "$collectAIBase/freshcheck/period/stock/coverage?period_id=$w3Period"
    if ($r.period_id -ne $w3Period) { throw "period_id = $($r.period_id)" }
    if ($r.counted -lt 1) { throw "counted = $($r.counted), want >= 1" }
    if ($r.total -lt 1) { throw "total = $($r.total), want >= 1" }
    if ($null -eq $r.missing) { throw "missing 字段缺失" }
    # counted == 1 of total, 但 3.x 残留的 SMOKE0001 也算 active, 所以 total >= 2
    # coverage_pct = counted/total*100, meets_c8 = coverage_pct >= 100 (默认阈值)
    if ($r.counted -gt $r.total) { throw "counted > total: $($r.counted) > $($r.total)" }
}

Test-Case "6.4 GET /period/stock/:id by id" {
    $r = Invoke-RestMethod -Headers @{Authorization="Bearer $Script:ownerToken"} `
        "$collectAIBase/freshcheck/period/stock/$Script:w3StockId"
    if ($r.id -ne $Script:w3StockId) { throw "id = $($r.id), want $Script:w3StockId" }
}

Test-Case "6.5 PUT /period/stock/:id update qty+note" {
    $body = @{ item_name = "renamed"; qty = 20.0; unit = "kg"; note = "corrected" } | ConvertTo-Json
    $r = Invoke-RestMethod -Method PUT -Headers @{Authorization="Bearer $Script:ownerToken"} `
        -ContentType "application/json" -Body $body `
        "$collectAIBase/freshcheck/period/stock/$Script:w3StockId"
    if (-not $r.ok) { throw "ok=false" }
    $g = Invoke-RestMethod -Headers @{Authorization="Bearer $Script:ownerToken"} `
        "$collectAIBase/freshcheck/period/stock/$Script:w3StockId"
    if ($g.qty -ne 20.0) { throw "after update qty = $($g.qty), want 20.0" }
    if ($g.note -ne "corrected") { throw "note = $($g.note)" }
}

Test-Case "6.6 RBAC u_cashier POST 403" {
    $body = @{ period_id = $w3Period; item_no = "W3SKU1"; qty = 1.0; unit = "kg"; operator = "u_cashier" } | ConvertTo-Json
    try {
        Invoke-RestMethod -Method POST -Headers @{Authorization="Bearer $Script:cashierToken"} `
            -ContentType "application/json" -Body $body `
            "$collectAIBase/freshcheck/period/stock" | Out-Null
        throw "u_cashier 应 403"
    } catch {
        if ($_.Exception.Response.StatusCode -ne 403) {
            throw "expect 403, got $($_.Exception.Response.StatusCode)"
        }
    }
}

Test-Case "6.7 DELETE /period/stock/:id cleanup" {
    $r = Invoke-RestMethod -Method DELETE -Headers @{Authorization="Bearer $Script:ownerToken"} `
        "$collectAIBase/freshcheck/period/stock/$Script:w3StockId"
    if (-not $r.ok) { throw "ok=false" }
    try {
        Invoke-RestMethod -Headers @{Authorization="Bearer $Script:ownerToken"} `
            "$collectAIBase/freshcheck/period/stock/$Script:w3StockId" | Out-Null
        throw "删后 GET 应 404"
    } catch {
        if ($_.Exception.Response.StatusCode -ne 404) {
            throw "expect 404, got $($_.Exception.Response.StatusCode)"
        }
    }
}

Test-Case "6.8 cleanup W3.1 seed data" {
    Push-Location (Split-Path $PSScriptRoot)
    $out = go run ./scripts/tmp/seed_w3_smoke.go -cleanup 2>&1 | Out-String
    Pop-Location
    if ($out -notmatch "cleanup OK") { throw "cleanup failed: $out" }
}

# Summary
Write-Host ""
Write-Host "================================================" -ForegroundColor Cyan
Write-Host "Summary: $($Script:pass)/$($Script:total) PASS, $($Script:fail) FAIL" -ForegroundColor Cyan
Write-Host "================================================" -ForegroundColor Cyan

if ($Script:fail -gt 0) {
    exit 1
}
exit 0
