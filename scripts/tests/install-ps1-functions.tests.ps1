# =============================================================================
#  install-ps1-functions.tests.ps1 — behavioural tests for install.ps1's
#  verification helpers (RFC-0001 R8, backend#2078).
#
#  install.ps1 is a `irm | iex` entrypoint that ends in Windows-registry PATH
#  writes, so it cannot be driven end-to-end on the Linux runner the way
#  install-verify.sh drives install.sh. What CAN be driven — and is where the
#  security decisions actually live — are its pure helpers.
#
#  They are EXTRACTED FROM THE REAL FILE by AST and evaluated here. Nothing
#  below re-implements a rule from install.ps1: if a helper changes, this test
#  runs the changed helper. A copy would pass while production broke, which is
#  the failure mode CLAUDE.md rule 9 names.
#
#  No Pester: this repo has no Pester tier, and standing one up to assert four
#  things costs more than it returns. Exit code is the contract.
# =============================================================================
Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$script:Pass = 0
$script:Fail = 0
function ok  ([string]$m) { Write-Host "  ok   $m";   $script:Pass++ }
function bad ([string]$m) { Write-Host "  FAIL $m";   $script:Fail++ }
function is  ([string]$m, $got, $want) {
    if ($got -eq $want) { ok $m } else { bad "$m (got '$got', want '$want')" }
}

$installer = Join-Path $PSScriptRoot '..' 'install.ps1' | Resolve-Path

# ── extract, never restate ──────────────────────────────────────────────────
$errs = $null
$ast  = [System.Management.Automation.Language.Parser]::ParseFile(
            "$installer", [ref]$null, [ref]$errs)
if ($errs) {
    # A parse error is a finding, not a skip: we cannot tell whether the
    # helpers are correct, and "cannot tell" never passes (CLAUDE.md rule 3).
    $errs | ForEach-Object { Write-Host "  FAIL parse: $($_.Message)" }
    exit 1
}
ok 'install.ps1 parses'

function Get-Fn([string]$Name) {
    $hit = $ast.FindAll({
        param($n)
        $n -is [System.Management.Automation.Language.FunctionDefinitionAst] -and
        $n.Name -eq $Name
    }, $true)
    if ($hit.Count -ne 1) {
        bad "expected exactly one definition of $Name, found $($hit.Count)"
        return $null
    }
    return $hit[0].Extent.Text
}

foreach ($n in 'Get-Sha256', 'Copy-StreamBounded', 'Save-BoundedFile', 'Resolve-Cosign', 'Test-CosignRuns') {
    $src = Get-Fn $n
    if (-not $src) { Write-Host "install-ps1-functions: $script:Pass passed, $script:Fail failed"; exit 1 }
    Invoke-Expression $src
}
ok 'Get-Sha256 / Copy-StreamBounded / Save-BoundedFile / Resolve-Cosign / Test-CosignRuns extracted'

# ── 1. $AllowUnverified: only the literal '1' opts out ──────────────────────
# The bypass is the single thing standing between a user and an unverified
# binary, so its parsing gets a truth table rather than a spot check.
#
# '0' is the case that matters. [bool]'0' is $true in PowerShell — any
# non-empty string is — so the natural-looking `[bool]$env:...` turns
# TRACEBLOC_ALLOW_UNVERIFIED=0 into "verification off". Caught here before it
# shipped; this test is why it stays caught.
$assign = $ast.FindAll({
    param($n)
    $n -is [System.Management.Automation.Language.AssignmentStatementAst] -and
    $n.Left -is [System.Management.Automation.Language.VariableExpressionAst] -and
    $n.Left.VariablePath.UserPath -eq 'AllowUnverified'
}, $true)
if ($assign.Count -ne 1) {
    bad "expected one \$AllowUnverified assignment, found $($assign.Count)"
} else {
    $expr = $assign[0].Right.Extent.Text
    # Written down independently of the expression under test — the values come
    # from what a user might plausibly set, not from reading the matcher
    # (CLAUDE.md rule 9's "never test a list against itself").
    $cases = @(
        @{ v = $null;    want = $false; why = 'unset'      },
        @{ v = '';       want = $false; why = 'empty'      },
        @{ v = '0';      want = $false; why = "the string 0" },
        @{ v = 'false';  want = $false; why = "'false'"    },
        @{ v = 'no';     want = $false; why = "'no'"       },
        @{ v = 'true';   want = $false; why = "'true' is not the documented opt-in" },
        @{ v = '1';      want = $true;  why = 'the documented opt-in' }
    )
    foreach ($c in $cases) {
        if ($null -eq $c.v) { Remove-Item Env:TRACEBLOC_ALLOW_UNVERIFIED -ErrorAction SilentlyContinue }
        else                { $env:TRACEBLOC_ALLOW_UNVERIFIED = $c.v }
        is "AllowUnverified is $($c.want) for $($c.why)" (Invoke-Expression $expr) $c.want
    }
    Remove-Item Env:TRACEBLOC_ALLOW_UNVERIFIED -ErrorAction SilentlyContinue
}

# ── 2. Test-CosignRuns distinguishes "won't start" from "ran and said no" ───
# This is the Windows-on-ARM case in miniature: a cosign that cannot execute
# reports through the same channel as a signature that failed to verify, and
# those two warrant opposite messages. /bin/true and /bin/false stand in for a
# cosign that starts and one that doesn't.
$tmp = Join-Path ([IO.Path]::GetTempPath()) ("tb-ps1-" + [Guid]::NewGuid())
New-Item -ItemType Directory -Path $tmp -Force | Out-Null
try {
    # Resolved, not hardcoded: /bin/true on Linux, /usr/bin/true on macOS.
    $trueBin  = (Get-Command true  -CommandType Application).Source | Select-Object -First 1
    $falseBin = (Get-Command false -CommandType Application).Source | Select-Object -First 1
    is 'Test-CosignRuns true for a binary that exits 0'    (Test-CosignRuns $trueBin)  $true
    is 'Test-CosignRuns false for a binary that exits 1'   (Test-CosignRuns $falseBin) $false

    # The one that actually reproduces Windows-on-ARM without emulation: the
    # file is there, it just cannot be executed. Must be $false, not a throw —
    # a throw would escape into the installer's `Stop` preference and surface
    # as a stack trace instead of the actionable message.
    $noexec = Join-Path $tmp 'not-executable'
    Set-Content -LiteralPath $noexec -Value 'not a binary'
    is 'Test-CosignRuns false for a non-executable file'   (Test-CosignRuns $noexec) $false
    is 'Test-CosignRuns false for a path that is absent'   (Test-CosignRuns (Join-Path $tmp 'nope')) $false

    # A stale success must not leak through, and this is the input that proves
    # it. An absent path throws and returns early, so it never reads
    # $LASTEXITCODE — a "stale 0" assertion built on one is vacuous (it was, and
    # the mutation caught it). The reachable case is a PowerShell SHIM: scoop and
    # chocolatey install tools as cosign.ps1, Get-Command finds .ps1 on PATH
    # whatever PATHEXT says, and `&` dispatches it IN-PROCESS — setting no
    # $LASTEXITCODE at all. Without the 255 preset the helper then reads the 0
    # left by the last successful command and reports that a shim which did
    # nothing is a working cosign.
    $shim = Join-Path $tmp 'cosign.ps1'
    Set-Content -LiteralPath $shim -Value 'Write-Output "shim: nothing to do"'
    & $trueBin                      # arm a stale 0, exactly as the happy path does
    is 'Test-CosignRuns false for a PowerShell shim that sets no exit code' `
        (Test-CosignRuns $shim) $false

    # ── 2b. the bootstrap fetch is bounded by SIZE, not just time (backend#2199) ─
    # cosign is the supply-chain trust root, fetched before anything authenticates
    # it, so an unbounded body must not be able to hang the install or exhaust disk.
    # Copy-StreamBounded is the hard ceiling: it caps the bytes it actually reads, so
    # a server that lies about (or omits) Content-Length can't slip a runaway body
    # through. Driven with MemoryStreams — the cap is what's under test, not the wire.
    $under = [System.IO.MemoryStream]::new([byte[]]::new(4096))
    $sink  = [System.IO.MemoryStream]::new()
    Copy-StreamBounded -Source $under -Dest $sink -MaxBytes 1MB
    is 'Copy-StreamBounded passes a body under the cap through intact' $sink.Length 4096L

    $over  = [System.IO.MemoryStream]::new([byte[]]::new(2048))
    $sink2 = [System.IO.MemoryStream]::new()
    $capped = $false
    try { Copy-StreamBounded -Source $over -Dest $sink2 -MaxBytes 1024 } catch { $capped = $true }
    is 'Copy-StreamBounded throws when the body exceeds the cap' $capped $true

    # End-to-end through Save-BoundedFile over a file:// URI — the real request path
    # (Content-Length early-reject + the streamed copy), still hermetic, no network.
    $small = Join-Path $tmp 'bounded-small.bin'
    [System.IO.File]::WriteAllBytes($small, [byte[]]::new(2048))
    $big   = Join-Path $tmp 'bounded-big.bin'
    [System.IO.File]::WriteAllBytes($big, [byte[]]::new(3 * 1024 * 1024))

    Save-BoundedFile -Uri ([System.Uri]::new($small).AbsoluteUri) `
        -OutFile (Join-Path $tmp 'bounded-out') -MaxBytes 1MB -TimeoutSec 10
    is 'Save-BoundedFile writes a file under the cap' (Get-Item (Join-Path $tmp 'bounded-out')).Length 2048L

    $rejected = $false
    try {
        Save-BoundedFile -Uri ([System.Uri]::new($big).AbsoluteUri) `
            -OutFile (Join-Path $tmp 'bounded-out-big') -MaxBytes 1MB -TimeoutSec 10
    } catch { $rejected = $true }
    is 'Save-BoundedFile refuses a file over the cap' $rejected $true

    # ── 3. Resolve-Cosign returns the one on PATH without a download ─────────
    $onpath = Join-Path $tmp 'pathbin'
    New-Item -ItemType Directory -Path $onpath -Force | Out-Null
    $fake = Join-Path $onpath 'cosign'
    Set-Content -LiteralPath $fake -Value "#!/bin/sh`nexit 0"
    & chmod +x $fake
    $savedPath = $env:PATH
    try {
        $env:PATH = "${onpath}:${savedPath}"
        $got = Resolve-Cosign $tmp
        if ($got -and (Resolve-Path $got).Path -eq (Resolve-Path $fake).Path) {
            ok 'Resolve-Cosign short-circuits to the cosign already on PATH'
        } else {
            bad "Resolve-Cosign short-circuits to the cosign already on PATH (got '$got')"
        }
    } finally { $env:PATH = $savedPath }

    # ── 4. a bootstrapped cosign that fails its own checksum is refused ──────
    # The security-critical branch. A verifier we cannot vouch for is worth no
    # more than no verifier, so Resolve-Cosign must return $null — NOT a usable
    # path — and let the caller's fail-closed logic run.
    #
    # Shadowing the helper: a function defined here wins over the extracted
    # Save-BoundedFile for the duration, so the extracted Resolve-Cosign hits this
    # instead of the network. $CosignVersion is what the real file pins.
    $CosignVersion = 'v0.0.0-test'
    function Save-BoundedFile {
        param($Uri, $OutFile, $MaxBytes, $TimeoutSec)
        if ($Uri -match 'checksums') {
            # A well-formed checksums file naming the right asset — with the
            # WRONG digest. Nothing about the transfer failed; the bytes are
            # simply not the bytes sigstore published.
            Set-Content -LiteralPath $OutFile -Value ("{0}  cosign-windows-amd64.exe" -f ('0' * 64))
        } else {
            Set-Content -LiteralPath $OutFile -Value 'pretend-cosign-bytes'
        }
    }
    $bootstrapDir = Join-Path $tmp 'boot'
    New-Item -ItemType Directory -Path $bootstrapDir -Force | Out-Null
    is 'Resolve-Cosign returns $null when the bootstrap fails its checksum' `
        (Resolve-Cosign $bootstrapDir) $null

    # …and returns a path when the digest DOES match, so the check above is
    # failing for the reason it claims and not because the mock never worked.
    # Without this pair, a Resolve-Cosign that always returned $null would look
    # perfectly healthy (CLAUDE.md rule 5: assert the anchor applied).
    $good = Join-Path $tmp 'good'
    New-Item -ItemType Directory -Path $good -Force | Out-Null
    $payload = 'pretend-cosign-bytes'
    function Save-BoundedFile {
        param($Uri, $OutFile, $MaxBytes, $TimeoutSec)
        if ($Uri -match 'checksums') {
            $probe = Join-Path ([IO.Path]::GetTempPath()) ([Guid]::NewGuid())
            Set-Content -LiteralPath $probe -Value 'pretend-cosign-bytes' -NoNewline
            # Match how Set-Content writes the payload below, byte for byte.
            $h = (Get-FileHash -Algorithm SHA256 -Path $probe).Hash.ToLower()
            Remove-Item $probe -Force
            Set-Content -LiteralPath $OutFile -Value "$h  cosign-windows-amd64.exe"
        } else {
            Set-Content -LiteralPath $OutFile -Value 'pretend-cosign-bytes' -NoNewline
        }
    }
    $gotGood = Resolve-Cosign $good
    if ($gotGood -and (Test-Path $gotGood)) {
        ok 'Resolve-Cosign returns the bootstrapped path when the checksum matches'
    } else {
        bad "Resolve-Cosign returns the bootstrapped path when the checksum matches (got '$gotGood')"
    }
} finally {
    Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
}

# ── 5. the verification that RAN and failed has no escape hatch ─────────────
# TRACEBLOC_ALLOW_UNVERIFIED covers "cannot verify" — no cosign, no .sig/.cert.
# It must NOT cover "verified and FAILED": that is evidence of tampering, and no
# env var overrides it. Asserted against the AST rather than by reading the
# file, so it holds however the branch is reformatted.
$verifyCall = $ast.FindAll({
    param($n)
    $n -is [System.Management.Automation.Language.CommandAst] -and
    $n.Extent.Text -match 'verify-blob'
}, $true)
if ($verifyCall.Count -ne 1) {
    bad "expected one cosign verify-blob call, found $($verifyCall.Count)"
} else {
    ok 'exactly one cosign verify-blob call'
    # The refusal is the `if ($LASTEXITCODE -ne 0)` that follows it.
    $guard = $ast.FindAll({
        param($n)
        $n -is [System.Management.Automation.Language.IfStatementAst] -and
        $n.Extent.StartOffset -gt $verifyCall[0].Extent.EndOffset -and
        $n.Clauses[0].Item1.Extent.Text -match 'LASTEXITCODE'
    }, $true) | Sort-Object { $_.Extent.StartOffset } | Select-Object -First 1

    if (-not $guard) {
        bad 'no $LASTEXITCODE guard follows the verify-blob call'
    } else {
        $body = $guard.Clauses[0].Item2.Extent.Text
        if ($body -match '\bexit\b') { ok 'a failed verification exits' }
        else                         { bad 'a failed verification does not exit' }
        if ($body -match 'AllowUnverified') {
            bad 'the failed-verification branch has a TRACEBLOC_ALLOW_UNVERIFIED escape'
        } else {
            ok 'no TRACEBLOC_ALLOW_UNVERIFIED escape from a FAILED verification'
        }
    }
}

# ── 6. every "cannot verify" path is a refusal by default ───────────────────
# Each branch that gives up on verifying must sit under an $AllowUnverified
# test AND exit when that test is false. Counted from the AST: three such
# branches exist (no cosign, cosign won't run, no .sig/.cert). A fourth added
# later without a refusal shows up here as a count change rather than passing
# silently.
$escapes = $ast.FindAll({
    param($n)
    $n -is [System.Management.Automation.Language.IfStatementAst] -and
    $n.Clauses[0].Item1.Extent.Text -match 'AllowUnverified'
}, $true)
$withExit = @($escapes | Where-Object { $_.Extent.Text -match '(?m)^\s*exit 1\s*$' })
if ($escapes.Count -ge 3 -and $withExit.Count -eq $escapes.Count) {
    ok "all $($escapes.Count) cannot-verify branches refuse unless opted out"
} else {
    bad ("cannot-verify branches: $($escapes.Count) found, " +
         "$($withExit.Count) refuse by default (want >=3, all refusing)")
}

# ── 7. no user-facing message loses text to a bash-shaped escape ────────────
# `\$` is not an escape in PowerShell — the escape character is a backtick. In a
# double-quoted string it renders as a literal backslash followed by the
# EXPANDED variable, so "Pin a signed \$env:RELEASE_VERSION" prints
# "Pin a signed ," silently dropping the one thing the user needed. Written
# during this change and caught by rendering the messages; asserted here so the
# next person reaching for bash muscle memory gets told.
$backslashDollar = $ast.FindAll({
    param($n)
    $n -is [System.Management.Automation.Language.ExpandableStringExpressionAst] -and
    $n.Value -match '\\\$'
}, $true)
if ($backslashDollar.Count -eq 0) {
    ok 'no double-quoted message uses a bash-style \$ escape'
} else {
    $backslashDollar | ForEach-Object { bad "bash-style \$ escape in: $($_.Extent.Text)" }
}

Write-Host ''
Write-Host "install-ps1-functions: $script:Pass passed, $script:Fail failed"
if ($script:Fail -gt 0) { exit 1 }
exit 0
