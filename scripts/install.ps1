# tracebloc CLI installer for Windows (PowerShell 5.1+).
#
# Usage:
#   irm https://github.com/tracebloc/cli/releases/latest/download/install.ps1 | iex
#   # Or pin a version:
#   $env:RELEASE_VERSION='v0.1.0'; irm <url> | iex
#
# What it does:
#   1. Detects arch (amd64 or arm64 on Windows)
#   2. Resolves the latest release tag (or honors $env:RELEASE_VERSION)
#   3. Downloads tracebloc-<tag>-windows-amd64.exe + SHA256SUMS
#   4. Verifies SHA256
#   5. (Optional) Verifies cosign signature if cosign.exe is on PATH
#   6. Installs to $env:USERPROFILE\AppData\Local\Programs\tracebloc\tracebloc.exe
#      and PATH-adds it via user-scope env var
#
# PowerShell 5.1 is the floor (ships with Windows 10 21H1+). PS7+ also
# works. Older PS versions miss `Invoke-WebRequest -UseBasicParsing`'s
# default, but the script forces it.

# Strict mode + halt on errors — the customer sees a clear error if
# anything fails, not a half-installed binary.
Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

# Fail prints a clean error message + exits. With Stop preference,
# Write-Error throws a terminating error BEFORE any subsequent exit
# runs, surfacing as a verbose PowerShell error record with stack
# trace + line numbers — confusing for a customer-facing installer.
# Bugbot PR #11 r3 flagged the original Write-Error + exit 1
# patterns as both dead code (exit never reached) AND ugly UX.
# Fail's Write-Host + explicit exit produces a one-line red error
# the customer can actually act on.
function Fail([string]$msg) {
    Write-Host "Error: $msg" -ForegroundColor Red
    exit 1
}

# ---------------------------------------------------------------------
# Config knobs (env-overridable, mirrors install.sh).
# ---------------------------------------------------------------------
$ReleaseVersion = if ($env:RELEASE_VERSION) { $env:RELEASE_VERSION } else { 'latest' }
$InstallPrefix  = if ($env:INSTALL_PREFIX) { $env:INSTALL_PREFIX } `
                  else { Join-Path $env:LOCALAPPDATA 'Programs\tracebloc' }
$GitHubRepo     = 'tracebloc/cli'
$BinaryName     = 'tracebloc.exe'

# ---------------------------------------------------------------------
# Detect arch.
# ---------------------------------------------------------------------
function Get-Arch {
    # PROCESSOR_ARCHITECTURE is the canonical Windows arch env var:
    #   AMD64 → x64 binary
    #   ARM64 → Windows on ARM
    # Caveat: a 32-bit / x64-emulated process on an ARM64 host reports
    # the PROCESS arch in PROCESSOR_ARCHITECTURE but the NATIVE arch in
    # PROCESSOR_ARCHITEW6432. Prefer the latter when set so an x64
    # PowerShell on Windows-on-ARM still installs the native arm64 build.
    $proc = if ($env:PROCESSOR_ARCHITEW6432) { $env:PROCESSOR_ARCHITEW6432 } else { $env:PROCESSOR_ARCHITECTURE }
    switch ($proc) {
        'AMD64' { return 'amd64' }
        'ARM64' { return 'arm64' }
        default {
            Fail "Unsupported processor architecture: $proc"
        }
    }
}

$arch = Get-Arch

# ---------------------------------------------------------------------
# Resolve the release tag if "latest".
# ---------------------------------------------------------------------
function Resolve-Tag {
    if ($script:ReleaseVersion -ne 'latest') {
        return $script:ReleaseVersion
    }
    # Follow the /releases/latest redirect to find the tag, same trick
    # install.sh uses. -MaximumRedirection 0 makes Invoke-WebRequest
    # surface the Location header instead of following.
    try {
        $resp = Invoke-WebRequest `
            -Uri "https://github.com/$script:GitHubRepo/releases/latest" `
            -MaximumRedirection 0 `
            -UseBasicParsing `
            -ErrorAction SilentlyContinue
    } catch {
        # PowerShell treats 3xx as an error when MaximumRedirection=0.
        # The Exception.Response carries the redirect we want.
        $resp = $_.Exception.Response
    }
    $loc = $null
    if ($resp -and $resp.Headers) {
        # PS5.1: Headers is a Dictionary[string,string]; PS7: a
        # HttpResponseHeaders that needs different access. Try both.
        try { $loc = $resp.Headers['Location'] } catch {}
        if (-not $loc) { try { $loc = $resp.Headers.Location } catch {} }
    }
    if (-not $loc) {
        Fail "Couldn't resolve the 'latest' release tag from GitHub. Pass `$env:RELEASE_VERSION explicitly."
    }
    # Location: https://github.com/tracebloc/cli/releases/tag/vX.Y.Z
    return Split-Path -Leaf $loc
}

$tag = Resolve-Tag
Write-Host "Installing tracebloc CLI $tag (windows/$arch)..."

# ---------------------------------------------------------------------
# Download artifacts.
# ---------------------------------------------------------------------
$binaryFile  = "tracebloc-$tag-windows-$arch.exe"
$baseUrl     = "https://github.com/$GitHubRepo/releases/download/$tag"
$tmpDir      = New-Item -ItemType Directory -Path (Join-Path $env:TEMP "tracebloc-install-$tag-$([guid]::NewGuid())") -Force

try {
    Write-Host "Downloading binary..."
    Invoke-WebRequest -Uri "$baseUrl/$binaryFile" -OutFile (Join-Path $tmpDir $binaryFile) -UseBasicParsing

    Write-Host "Downloading SHA256SUMS..."
    Invoke-WebRequest -Uri "$baseUrl/SHA256SUMS" -OutFile (Join-Path $tmpDir 'SHA256SUMS') -UseBasicParsing

    # -------------------------------------------------------------
    # Verify SHA256.
    # -------------------------------------------------------------
    Write-Host "Verifying SHA256..."
    $sumsContent = Get-Content (Join-Path $tmpDir 'SHA256SUMS')
    $expected = ($sumsContent | Where-Object { $_ -match " $([regex]::Escape($binaryFile))$" } |
                 Select-Object -First 1) -replace ' .*$',''
    if (-not $expected) {
        Fail "SHA256SUMS doesn't contain an entry for $binaryFile — release artifacts may be incomplete."
    }
    $actual = (Get-FileHash -Algorithm SHA256 -Path (Join-Path $tmpDir $binaryFile)).Hash.ToLower()
    if ($actual -ne $expected) {
        Fail "SHA256 mismatch!`n  expected: $expected`n  actual:   $actual`n  refusing to install."
    }
    Write-Host "  ✓ checksum matches"

    # -------------------------------------------------------------
    # Cosign signature verification (optional).
    # -------------------------------------------------------------
    if (Get-Command cosign -ErrorAction SilentlyContinue) {
        Write-Host "Verifying cosign signature..."
        # Separate "download .sig/.cert" (recoverable if absent — old
        # releases predate signing) from "verify the downloaded sig"
        # (NOT recoverable — a failed verification means the binary
        # is potentially tampered, refuse to install). Bugbot PR #11
        # caught the prior structure: with $ErrorActionPreference =
        # 'Stop', Write-Error inside the try-block was thrown and
        # caught by the same catch that handled missing-sig, so a
        # failed verify silently downgraded to "skip + continue."
        $sigDownloaded = $false
        try {
            Invoke-WebRequest -Uri "$baseUrl/$binaryFile.sig"  -OutFile (Join-Path $tmpDir "$binaryFile.sig")  -UseBasicParsing
            Invoke-WebRequest -Uri "$baseUrl/$binaryFile.cert" -OutFile (Join-Path $tmpDir "$binaryFile.cert") -UseBasicParsing
            $sigDownloaded = $true
        } catch {
            Write-Host "  ⚠ couldn't download .sig/.cert — release may pre-date signing."
        }
        if ($sigDownloaded) {
            # Verify OUTSIDE the try/catch: a non-zero $LASTEXITCODE
            # from cosign is a hard refusal, not a swallowed
            # exception. & invokes cosign as an external process,
            # which doesn't interact with $ErrorActionPreference.
            & cosign verify-blob `
                --certificate-identity-regexp "https://github.com/$GitHubRepo/.github/workflows/release.yml@.*" `
                --certificate-oidc-issuer 'https://token.actions.githubusercontent.com' `
                --certificate (Join-Path $tmpDir "$binaryFile.cert") `
                --signature   (Join-Path $tmpDir "$binaryFile.sig") `
                (Join-Path $tmpDir $binaryFile) 2>$null
            if ($LASTEXITCODE -ne 0) {
                Write-Host "Error: cosign signature verification FAILED — refusing to install." -ForegroundColor Red
                exit 1
            }
            Write-Host "  ✓ cosign signature valid"
        }
    } else {
        Write-Host "  (cosign not installed; SHA256 verified, signature skipped)"
    }

    # -------------------------------------------------------------
    # Install to $InstallPrefix.
    # -------------------------------------------------------------
    New-Item -ItemType Directory -Path $InstallPrefix -Force | Out-Null
    $target = Join-Path $InstallPrefix $BinaryName
    Move-Item -Path (Join-Path $tmpDir $binaryFile) -Destination $target -Force

    # Short alias: `tb` — a cmd shim next to the exe (symlinks need admin or
    # dev-mode on Windows). Best-effort: an alias must never fail the install
    # (mirrors install.sh), and we never clobber an unrelated tb.cmd — "ours"
    # means it invokes exactly our binary path, not merely mentions the name.
    $tbShim = Join-Path $InstallPrefix 'tb.cmd'
    $shimBody = "@echo off`r`n`"$target`" %*`r`n"
    $tbNote = ''
    $tbExisting = if (Test-Path $tbShim) { Get-Content $tbShim -Raw -ErrorAction SilentlyContinue } else { $null }
    if (-not (Test-Path $tbShim) -or ($tbExisting -like ('*"' + $target + '"*'))) {
        try {
            Set-Content -Path $tbShim -Value $shimBody -Encoding ascii -ErrorAction Stop
            $tbNote = ' (short alias: tb)'
        } catch {
            Write-Host "Note: couldn't create the tb alias ($($_.Exception.Message)) — skipping."
        }
    } else {
        Write-Host "Note: $tbShim already exists and isn't ours — skipping the tb alias."
    }

    Write-Host ""
    Write-Host "✓ tracebloc CLI installed: $target$tbNote"
    Write-Host ""
    Write-Host "Verify with:"
    Write-Host "  $target version"

    # PATH advice. User-scope PATH edit so this survives reboots.
    #
    # Null-guard the existing $userPath before concatenation. On
    # fresh Windows installs (or accounts that never set user-scope
    # PATH), GetEnvironmentVariable returns $null. The naive
    # `"$userPath;$InstallPrefix"` interpolation would then produce
    # `";C:\..."` — a leading semicolon = empty PATH entry, which on
    # Windows resolves to the CURRENT WORKING DIRECTORY. That's a
    # well-known PATH-injection vector (binary planted in cwd runs
    # ahead of real ones). Bugbot PR #11 r3 flagged the security
    # concern.
    $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
    $existingEntries = if ($userPath) { $userPath -split ';' } else { @() }
    if ($existingEntries -notcontains $InstallPrefix) {
        Write-Host ""
        Write-Host "Note: $InstallPrefix is not on `$env:Path. Adding it for your user:"
        $newPath = if ($userPath) { "$userPath;$InstallPrefix" } else { $InstallPrefix }
        [Environment]::SetEnvironmentVariable('Path', $newPath, 'User')
        Write-Host "  (open a new PowerShell window to pick up the change)"
    }

    Write-Host ""
    Write-Host "First steps:"
    Write-Host "  tracebloc cluster info        # confirm the CLI can reach your cluster"
    Write-Host "  tracebloc data ingest --help  # stage a dataset onto the cluster"
    Write-Host ""
    Write-Host "Short alias: tb works everywhere tracebloc does (tb data ingest .\data)"
}
finally {
    # Always clean up the temp dir, even on early exit / Ctrl-C.
    Remove-Item -Recurse -Force -Path $tmpDir -ErrorAction SilentlyContinue
}
