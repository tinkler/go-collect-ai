# freshcheck W1 smoke 脚本
# 跑法:
#   1) 启 PG (127.0.0.1:5432, db=collectai, user=postgres, pwd=postgres)
#   2) 启 cube-agent-server (8088)
#   3) 启 collect-ai (8089)
#   4) 跑本脚本
#
# 输出: PASS/FAIL 表格 + 详细 log
# 退出码: 0 = 全过, 1 = 有失败

$ErrorActionPreference = 'Stop'
$collectAIBase = 'http://127.0.0.1:8089/api/v1'
$cubeBase = 'http://127.0.0.1:8088'

# 计数器
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
Write-Host "freshcheck W1 smoke 脚本" -ForegroundColor Cyan
Write-Host "================================================" -ForegroundColor Cyan
Write-Host ""

# ============== 1. collect-ai 启动检查 ==============
Write-Host "=== [1] collect-ai 健康检查 ===" -ForegroundColor Yellow

Test-Case "1.1 /api/v1/freshcheck/health 13 张表齐" {
    $r = Invoke-RestMethod "$collectAIBase/freshcheck/health"
    if (-not $r.ok) { throw "ok=false" }
    if ($r.count -ne 13) { throw "count=$($r.count), want 13" }
}

Test-Case "1.2 thresholds 列表 (期望 12 行)" {
    $r = Invoke-RestMethod "$collectAIBase/freshcheck/config/thresholds"
    if ($r.count -lt 12) { throw "count=$($r.count), want >= 12" }
}

Test-Case "1.3 category-tracks 列表 (期望 5 行)" {
    $r = Invoke-RestMethod "$collectAIBase/freshcheck/config/category-tracks"
    if ($r.count -lt 5) { throw "count=$($r.count), want >= 5" }
}

Test-Case "1.4 pool-codes 列表 (空 OK, 还没配)" {
    $r = Invoke-RestMethod "$collectAIBase/freshcheck/config/pool-codes"
    if ($null -eq $r.items) { throw "items missing" }
}

Test-Case "1.5 sku-map 列表 (空 OK, 还没建账)" {
    $r = Invoke-RestMethod "$collectAIBase/freshcheck/config/sku-map"
    if ($null -eq $r.items) { throw "items missing" }
}

# ============== 2. dev-login 鉴权 ==============
Write-Host ""
Write-Host "=== [2] 鉴权 + RBAC ===" -ForegroundColor Yellow

# u_owner 拿全部 perm
Test-Case "2.1 dev-login u_owner" {
    $r = Invoke-RestMethod -Method POST "$collectAIBase/auth/dev-login?as_user=u_owner"
    if ([string]::IsNullOrEmpty($r.access_token)) { throw "no access_token" }
    $Script:ownerToken = $r.access_token
}

# u_floor 只拿 pool:write
Test-Case "2.2 dev-login u_floor" {
    $r = Invoke-RestMethod -Method POST "$collectAIBase/auth/dev-login?as_user=u_floor"
    if ([string]::IsNullOrEmpty($r.access_token)) { throw "no access_token" }
    $Script:floorToken = $r.access_token
}

Test-Case "2.3 u_owner 读 thresholds OK" {
    $r = Invoke-RestMethod -Headers @{Authorization="Bearer $Script:ownerToken"} "$collectAIBase/freshcheck/config/thresholds"
    if ($r.count -lt 12) { throw "count=$($r.count)" }
}

Test-Case "2.4 u_floor 读 thresholds 应该 403" {
    try {
        Invoke-RestMethod -Headers @{Authorization="Bearer $Script:floorToken"} "$collectAIBase/freshcheck/config/thresholds" | Out-Null
        throw "u_floor 不应有 freshcheck:settle:read perm"
    } catch {
        if ($_.Exception.Response.StatusCode -ne 403) {
            throw "期望 403, 实际 $($_.Exception.Response.StatusCode)"
        }
    }
}

Test-Case "2.5 u_floor PUT 特价码 (pool:write 不够, 应 403)" {
    $body = @{pool_name="1元/斤"; pricing_mode="weight"; unit_price=1.0; is_active=$true} | ConvertTo-Json
    try {
        Invoke-RestMethod -Method PUT -Headers @{Authorization="Bearer $Script:floorToken"} `
            -ContentType "application/json" -Body $body `
            "$collectAIBase/freshcheck/config/pool-codes/POOL001" | Out-Null
        throw "u_floor 不应有 freshcheck:config:write perm"
    } catch {
        if ($_.Exception.Response.StatusCode -ne 403) {
            throw "期望 403, 实际 $($_.Exception.Response.StatusCode)"
        }
    }
}

# ============== 3. CRUD 写入测试 ==============
Write-Host ""
Write-Host "=== [3] 配置 CRUD 端到端 ===" -ForegroundColor Yellow

Test-Case "3.1 PUT 特价码 (u_owner)" {
    $body = @{pool_name="1元/斤测试"; pricing_mode="weight"; unit_price=1.0; priority_rank=0; is_active=$true} | ConvertTo-Json
    $r = Invoke-RestMethod -Method PUT -Headers @{Authorization="Bearer $Script:ownerToken"} `
        -ContentType "application/json" -Body $body `
        "$collectAIBase/freshcheck/config/pool-codes/SMOKE001"
    if (-not $r.ok) { throw "ok=false" }
}

Test-Case "3.2 GET 特价码 (验证写入)" {
    $r = Invoke-RestMethod "$collectAIBase/freshcheck/config/pool-codes/SMOKE001?branch_no=0001"
    if ($r.pool_code -ne "SMOKE001") { throw "got $($r.pool_code)" }
    if ($r.unit_price -ne 1.0) { throw "unit_price=$($r.unit_price)" }
}

Test-Case "3.3 PUT SKU 映射 (原 SKU + 子类)" {
    $body = @{item_name="测试菠菜"; fresh_category="leaf"; turnover_class="fast"; shelf_life_days=5; is_active=$true} | ConvertTo-Json
    $r = Invoke-RestMethod -Method PUT -Headers @{Authorization="Bearer $Script:ownerToken"} `
        -ContentType "application/json" -Body $body `
        "$collectAIBase/freshcheck/config/sku-map/SMOKE0001"
    if (-not $r.ok) { throw "ok=false" }
}

Test-Case "3.4 GET SKU 映射按 leaf 类过滤" {
    $r = Invoke-RestMethod "$collectAIBase/freshcheck/config/sku-map?fresh_category=leaf"
    if ($r.count -lt 1) { throw "count=$($r.count)" }
}

Test-Case "3.5 PUT 阈值 (改 c1_pool_saturation_pct)" {
    $body = @{value=18.0} | ConvertTo-Json
    $r = Invoke-RestMethod -Method PUT -Headers @{Authorization="Bearer $Script:ownerToken"} `
        -ContentType "application/json" -Body $body `
        "$collectAIBase/freshcheck/config/thresholds/c1_pool_saturation_pct"
    if (-not $r.ok) { throw "ok=false" }
    if ($r.value -ne 18.0) { throw "value=$($r.value)" }
}

Test-Case "3.6 GET 阈值 确认 c1=18.0" {
    $r = Invoke-RestMethod "$collectAIBase/freshcheck/config/thresholds"
    $c1 = $r.items | Where-Object { $_.key -eq "c1_pool_saturation_pct" }
    if ($c1.value -ne 18.0) { throw "c1=$($c1.value), want 18.0" }
}

# 还原
Test-Case "3.7 PUT 阈值还原 c1=15.00 (snap 审计)" {
    $body = @{value=15.00} | ConvertTo-Json
    Invoke-RestMethod -Method PUT -Headers @{Authorization="Bearer $Script:ownerToken"} `
        -ContentType "application/json" -Body $body `
        "$collectAIBase/freshcheck/config/thresholds/c1_pool_saturation_pct" | Out-Null
}

# ============== 4. C7 同步校验 ==============
Write-Host ""
Write-Host "=== [4] C7 同步校验 ===" -ForegroundColor Yellow

Test-Case "4.1 手动触发 C7 同步校验" {
    $r = Invoke-RestMethod -Method POST -Headers @{Authorization="Bearer $Script:ownerToken"} `
        -ContentType "application/json" -Body '{}' `
        "$collectAIBase/freshcheck/admin/sync-check"
    if ($null -eq $r.cube_value) { throw "cube_value missing" }
    if ($null -eq $r.source_value) { throw "source_value missing" }
    if ($null -eq $r.diff_pct) { throw "diff_pct missing" }
    Write-Host "    cube_value=$($r.cube_value) source_value=$($r.source_value) diff_pct=$($r.diff_pct)%" -ForegroundColor Gray
}

Test-Case "4.2 u_floor 触发 C7 应 403 (缺 override perm)" {
    try {
        Invoke-RestMethod -Method POST -Headers @{Authorization="Bearer $Script:floorToken"} `
            -ContentType "application/json" -Body '{}' `
            "$collectAIBase/freshcheck/admin/sync-check" | Out-Null
        throw "u_floor 不应有 freshcheck:override perm"
    } catch {
        if ($_.Exception.Response.StatusCode -ne 403) {
            throw "期望 403, 实际 $($_.Exception.Response.StatusCode)"
        }
    }
}

# ============== 5. 集成测试 (W1.7) ==============
Write-Host ""
Write-Host "=== [5] 集成测试 (需 PG 在线) ===" -ForegroundColor Yellow

Test-Case "5.1 跑 12 个 store_pg_test" {
    $env:FRESHCHECK_TEST_PG_DSN = "postgres://postgres:postgres@127.0.0.1:5432/collectai?sslmode=disable"
    Push-Location (Split-Path $PSScriptRoot)
    $out = go test -count=1 ./internal/freshcheck/... 2>&1 | Out-String
    Pop-Location
    if ($LASTEXITCODE -ne 0) {
        Write-Host "    test output:" -ForegroundColor Gray
        Write-Host "    $out" -ForegroundColor Gray
        throw "go test 退出码 $LASTEXITCODE"
    }
    if ($out -notmatch "ok\s+github.com/tinkler/collect-ai/internal/freshcheck") {
        throw "test 失败: $out"
    }
}

# ============== 总结 ==============
Write-Host ""
Write-Host "================================================" -ForegroundColor Cyan
Write-Host "总结: $($Script:pass)/$($Script:total) PASS, $($Script:fail) FAIL" -ForegroundColor Cyan
Write-Host "================================================" -ForegroundColor Cyan

if ($Script:fail -gt 0) {
    exit 1
}
exit 0
