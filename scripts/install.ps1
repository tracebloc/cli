# tracebloc CLI installer for Windows (PowerShell 5.1+).
#
# Usage:
#   irm https://github.com/tracebloc/cli/releases/latest/download/install.ps1 | iex
#   # Or pin a version:
#   $env:RELEASE_VERSION='v0.1.0'; irm <url> | iex
#
# What it does:
#   1. Detects arch (amd64 only on Windows; arm64 not yet shipped)
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
    #   AMD64 → x64 binary needed
    #   ARM64 → Windows on ARM (not yet shipped — see below)
    $proc = $env:PROCESSOR_ARCHITECTURE
    switch ($proc) {
        'AMD64' { return 'amd64' }
        'ARM64' {
            Write-Error "Windows ARM64 binary isn't released yet. File an issue at https://github.com/tracebloc/cli if you need it."
            exit 1
        }
        default {
            Write-Error "Unsupported PROCESSOR_ARCHITECTURE: $proc"
            exit 1
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
        Write-Error "Couldn't resolve the 'latest' release tag from GitHub. Pass `$env:RELEASE_VERSION explicitly."
        exit 1
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
        Write-Error "SHA256SUMS doesn't contain an entry for $binaryFile — release artifacts may be incomplete."
        exit 1
    }
    $actual = (Get-FileHash -Algorithm SHA256 -Path (Join-Path $tmpDir $binaryFile)).Hash.ToLower()
    if ($actual -ne $expected) {
        Write-Error "SHA256 mismatch!`n  expected: $expected`n  actual:   $actual`n  refusing to install."
        exit 1
    }
    Write-Host "  ✓ checksum matches"

    # -------------------------------------------------------------
    # Cosign signature verification (optional).
    # -------------------------------------------------------------
    if (Get-Command cosign -ErrorAction SilentlyContinue) {
        Write-Host "Verifying cosign signature..."
        try {
            Invoke-WebRequest -Uri "$baseUrl/$binaryFile.sig"  -OutFile (Join-Path $tmpDir "$binaryFile.sig")  -UseBasicParsing
            Invoke-WebRequest -Uri "$baseUrl/$binaryFile.cert" -OutFile (Join-Path $tmpDir "$binaryFile.cert") -UseBasicParsing
            & cosign verify-blob `
                --certificate-identity-regexp "https://github.com/$GitHubRepo/.github/workflows/release.yml@.*" `
                --certificate-oidc-issuer 'https://token.actions.githubusercontent.com' `
                --certificate (Join-Path $tmpDir "$binaryFile.cert") `
                --signature   (Join-Path $tmpDir "$binaryFile.sig") `
                (Join-Path $tmpDir $binaryFile) 2>$null
            if ($LASTEXITCODE -ne 0) {
                Write-Error "cosign signature verification FAILED — refusing to install."
                exit 1
            }
            Write-Host "  ✓ cosign signature valid"
        } catch {
            Write-Host "  ⚠ couldn't download .sig/.cert — release may pre-date signing."
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

    Write-Host ""
    Write-Host "✓ tracebloc CLI installed: $target"
    Write-Host ""
    Write-Host "Verify with:"
    Write-Host "  $target version"

    # PATH advice. User-scope PATH edit so this survives reboots.
    $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
    if (-not ($userPath -split ';' | Where-Object { $_ -eq $InstallPrefix })) {
        Write-Host ""
        Write-Host "Note: $InstallPrefix is not on \$env:Path. Adding it for your user:"
        [Environment]::SetEnvironmentVariable('Path', "$userPath;$InstallPrefix", 'User')
        Write-Host "  (open a new PowerShell window to pick up the change)"
    }

    Write-Host ""
    Write-Host "First steps:"
    Write-Host "  tracebloc cluster info        # confirm CLI can reach your cluster"
    Write-Host "  tracebloc dataset push --help # see the dominant flow"
}
finally {
    # Always clean up the temp dir, even on early exit / Ctrl-C.
    Remove-Item -Recurse -Force -Path $tmpDir -ErrorAction SilentlyContinue
}
