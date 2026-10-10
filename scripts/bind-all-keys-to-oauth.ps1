# Bind every plugin Key to all current OAuth accounts via the credit-manager management API.
#
# Why the management API (not the SQLite DB directly): only the running host can
# enumerate OAuth accounts and report the exact account IDs the scheduler matches
# bindings against. Reading the auth files off disk cannot reproduce those IDs.
#
# Usage:
#   $env:CPA_MANAGEMENT_SECRET = "<host management secret>"
#   powershell -ExecutionPolicy Bypass -File .\scripts\bind-all-keys-to-oauth.ps1
#   ... -BaseUrl http://127.0.0.1:8317 -Secret <secret> -WhatIf
#   ... -Replace            # overwrite each key's bindings with exactly the OAuth set
#   ... -IncludeDisabledKeys
param(
    [string]$BaseUrl = "http://127.0.0.1:8317",
    [string]$Secret = $env:CPA_MANAGEMENT_SECRET,
    [switch]$Replace,            # default merges onto existing bindings; -Replace overwrites
    [switch]$IncludeDisabledKeys,
    [switch]$WhatIf,             # list what would change, write nothing
    [switch]$SelfTest            # run the offline merge self-check and exit
)

$ErrorActionPreference = "Stop"

# mergeBindings is the one piece of non-trivial logic: union existing + oauth on
# the (provider, auth_id) pair, case-insensitive, order-stable.
function Merge-Bindings {
    param([object[]]$Existing, [object[]]$OAuth, [switch]$Overwrite)
    $out = [System.Collections.ArrayList]::new()
    $seen = @{}
    $add = {
        param($items)
        foreach ($b in $items) {
            $p = ("$($b.provider)").Trim(); $a = ("$($b.auth_id)").Trim()
            if (-not $p -or -not $a) { continue }
            $k = $p.ToLower() + "`0" + $a.ToLower()
            if ($seen.ContainsKey($k)) { continue }
            $seen[$k] = $true
            [void]$out.Add([ordered]@{ provider = $p; auth_id = $a })
        }
    }
    if (-not $Overwrite) { & $add $Existing }
    & $add $OAuth
    , $out.ToArray()
}

if ($SelfTest) {
    $existing = @(@{ provider = "codex"; auth_id = "a" }, @{ provider = "xai"; auth_id = "x" })
    $oauth = @(@{ provider = "Codex"; auth_id = "A" }, @{ provider = "codex"; auth_id = "b" })
    $merged = Merge-Bindings -Existing $existing -OAuth $oauth
    if ($merged.Count -ne 3) { throw "merge: expected 3 unique, got $($merged.Count)" }
    $over = Merge-Bindings -Existing $existing -OAuth $oauth -Overwrite
    if ($over.Count -ne 2) { throw "overwrite: expected 2, got $($over.Count)" }
    if ($over[0].provider -ne "Codex" -or $over[1].auth_id -ne "b") { throw "overwrite: wrong set $($over | ConvertTo-Json -Compress)" }
    Write-Host "self-test ok"
    return
}

if (-not $Secret) { throw "management secret required: set -Secret or `$env:CPA_MANAGEMENT_SECRET" }
$base = $BaseUrl.TrimEnd('/')
$headers = @{ Authorization = "Bearer $Secret"; Accept = "application/json" }

function Invoke-Mgmt {
    param([string]$Method, [string]$Path, [object]$Body)
    $url = "$base/v0/management/$($Path.TrimStart('/'))"
    $args = @{ Method = $Method; Uri = $url; Headers = $headers }
    if ($PSBoundParameters.ContainsKey('Body')) {
        $args.ContentType = "application/json"
        $args.Body = ($Body | ConvertTo-Json -Depth 10 -Compress)
    }
    try { Invoke-RestMethod @args }
    catch { throw "$Method $url failed: $($_.Exception.Message)" }
}

# 1. Current OAuth accounts (enabled, oauth-backed only).
$accounts = (Invoke-Mgmt GET "credit-manager/auth-accounts").items
$oauthBindings = @(
    $accounts | Where-Object { $_.oauth -and -not $_.disabled } |
        ForEach-Object { @{ provider = $_.provider; auth_id = $_.id } }
)
if ($oauthBindings.Count -eq 0) { throw "no enabled OAuth accounts reported by $base" }
Write-Host "OAuth accounts to bind ($($oauthBindings.Count)):"
$oauthBindings | ForEach-Object { Write-Host "  [$($_.provider)] $($_.auth_id)" }

# 2. Walk every page of keys.
$keys = [System.Collections.ArrayList]::new()
$page = 1
do {
    $resp = Invoke-Mgmt GET "credit-manager/keys?page=$page&page_size=100$(if ($IncludeDisabledKeys) { '' } else { '&active_only=1' })"
    foreach ($k in $resp.items) { [void]$keys.Add($k) }
    $totalPages = [int]$resp.total_pages
    $page++
} while ($page -le $totalPages)
Write-Host "keys found: $($keys.Count)"

# 3. Bind.
$changed = 0; $skipped = 0; $failed = 0
foreach ($k in $keys) {
    $existing = @($k.auth_bindings)
    $target = Merge-Bindings -Existing $existing -OAuth $oauthBindings -Overwrite:$Replace
    $before = ($existing | ForEach-Object { "$($_.provider)/$($_.auth_id)" } | Sort-Object) -join ','
    $after = ($target | ForEach-Object { "$($_.provider)/$($_.auth_id)" } | Sort-Object) -join ','
    if ($before -eq $after) { $skipped++; continue }
    $label = if ($k.label) { $k.label } else { $k.id }
    if ($WhatIf) {
        Write-Host "WOULD bind $label ($($k.id)): $($target.Count) accounts"
        $changed++
        continue
    }
    try {
        [void](Invoke-Mgmt POST "credit-manager/keys/update" @{ id = $k.id; auth_bindings = $target })
        Write-Host "bound $label ($($k.id)): $($target.Count) accounts"
        $changed++
    } catch {
        Write-Warning "FAILED $label ($($k.id)): $($_.Exception.Message)"
        $failed++
    }
}

Write-Host ""
Write-Host "done. changed=$changed skipped(up-to-date)=$skipped failed=$failed$(if ($WhatIf) { '  [dry-run]' })"
if ($failed -gt 0) { exit 1 }
