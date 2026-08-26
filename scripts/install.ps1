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
#   5. Verifies the cosign signature — MANDATORY (RFC-0001 R8). If cosign
#      isn't on PATH it bootstraps a pinned, checksum-verified copy; if it
#      can't, the install FAILS CLOSED rather than trusting the same-channel
#      SHA256 alone. TRACEBLOC_ALLOW_UNVERIFIED=1 is the one (loud) escape.
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

# Pinned verifier. Keep in lockstep with tracebloc/client's install.sh /
# install.ps1 COSIGN_VERSION and release.yml's cosign-installer pin.
$CosignVersion  = 'v2.4.1'

# The ONE escape from mandatory verification, for a genuinely constrained
# environment. Loud, and never the default (RFC-0001 R8).
#
# Compare against '1' explicitly. NOT [bool]$env:... — PowerShell casts any
# non-empty string to $true, so TRACEBLOC_ALLOW_UNVERIFIED=0 would have
# switched the bypass ON. Matches install.sh's `[ "$ALLOW_UNVERIFIED" = "1" ]`.
$AllowUnverified = ($env:TRACEBLOC_ALLOW_UNVERIFIED -eq '1')

# TLS 1.2 floor. PowerShell 5.1 defaults to SSL3/TLS1.0 on older Windows, and
# every fetch below carries either the binary we are about to run or the
# verifier that authenticates it — neither may negotiate down. PS7+ already
# defaults higher; setting it is harmless there.
#
# ASSIGN Tls12, do NOT -bor onto the default: OR-ing Tls12 on LEAVES SSL3/TLS1.0/1.1
# advertised, so a downgrade stays on the table — the exact thing this floor exists
# to remove. Tls12 alone is the floor and is always negotiable on any host that can
# reach us. We deliberately do NOT also add Tls13: [Enum]::IsDefined is true on
# .NET 4.8 even where Schannel cannot negotiate 1.3 (Win10 21H1, Server 2019), and
# assigning it then THROWS — the empty catch would swallow that and leave the floor
# unset, so a `Tls12 -bor Tls13` attempt can defeat the very floor it decorates
# (cli#528 Bugbot). 1.2 is secure for these fetches; 1.3 is not worth that risk.
try {
    [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
} catch { }

# ---------------------------------------------------------------------
# cosign bootstrap (RFC-0001 R8).
# ---------------------------------------------------------------------

function Get-Sha256([string]$Path) {
    return (Get-FileHash -Algorithm SHA256 -Path $Path).Hash.ToLower()
}

# Bounded download: cap BOTH the time a fetch may take AND the bytes it may write.
# Invoke-WebRequest has -TimeoutSec but no portable max-size knob
# (-MaximumSizeToDownload-style parameters are not in the PS 5.1 floor this
# installer targets), so we stream the body and count it ourselves. This matters
# most for the cosign bootstrap below: cosign is the supply-chain trust root,
# fetched before anything has authenticated it, so a MITM or a broken mirror must
# not be able to wedge the install on an unbounded, never-ending body
# (backend#2199). Mirrors install.sh's single shared `dl()` download profile.
#
# Split so the security-critical bit — the byte ceiling — is a pure function the
# sibling install-ps1-functions.tests.ps1 can drive against a MemoryStream with no
# network, rather than a rule restated in a test that could drift from it.
function Copy-StreamBounded {
    param([IO.Stream]$Source, [IO.Stream]$Dest, [long]$MaxBytes)
    # A server that under-declares or omits Content-Length cannot slip a runaway
    # body past this: the cap is enforced on bytes actually read, not on what the
    # server claimed. Abort the moment writing the next chunk would cross it.
    $buf   = New-Object byte[] 65536
    $total = 0L
    while (($n = $Source.Read($buf, 0, $buf.Length)) -gt 0) {
        $total += $n
        if ($total -gt $MaxBytes) { throw "download exceeded its $MaxBytes-byte cap" }
        $Dest.Write($buf, 0, $n)
    }
}

# Fetch $Uri to $OutFile under a size ceiling and a timeout, or throw.
#
# HttpWebRequest.Timeout bounds connect + response-headers; ReadWriteTimeout bounds
# each individual read — a STALL, not a wall-clock cap on the whole transfer. That
# is deliberate and matches install.sh (review #426): a slow-but-alive link must be
# allowed to finish, while a dead connection still aborts instead of hanging the
# install. The TLS 1.2 floor assigned above via ServicePointManager is inherited.
function Save-BoundedFile {
    param(
        [Parameter(Mandatory)][string]$Uri,
        [Parameter(Mandatory)][string]$OutFile,
        [Parameter(Mandatory)][long]$MaxBytes,
        [int]$TimeoutSec = 120
    )
    $req = [System.Net.WebRequest]::Create($Uri)
    $req.Timeout = $TimeoutSec * 1000
    # HTTP-only knobs; a non-HTTP scheme (e.g. a test file:// URI) skips them.
    if ($req -is [System.Net.HttpWebRequest]) {
        $req.ReadWriteTimeout  = $TimeoutSec * 1000
        $req.AllowAutoRedirect = $true
        $req.UserAgent         = 'tracebloc-cli-installer'
    }
    $resp = $req.GetResponse()
    try {
        # Cheap early refusal when the server is honest about an oversized body,
        # before a single body byte is read. Copy-StreamBounded is the authority
        # for a server that is not.
        if ($resp.ContentLength -gt $MaxBytes) {
            throw "refusing ${Uri}: server-declared size $($resp.ContentLength) B exceeds the $MaxBytes B cap"
        }
        $in  = $resp.GetResponseStream()
        $out = [System.IO.File]::Create($OutFile)
        $ok  = $false
        try     { Copy-StreamBounded -Source $in -Dest $out -MaxBytes $MaxBytes; $ok = $true }
        finally {
            $out.Dispose(); $in.Dispose()
            # On the throw path (cap tripped / read error) drop the partial file so a
            # rejected fetch doesn't leave up to MaxBytes of litter behind; handles are
            # disposed first so the delete succeeds on Windows.
            if (-not $ok) { Remove-Item -LiteralPath $OutFile -Force -ErrorAction SilentlyContinue }
        }
    } finally {
        # WebResponse.Dispose() is an explicit IDisposable impl — NOT callable as
        # $resp.Dispose() on Windows PowerShell 5.1 (the installer floor), where it
        # throws "does not contain a method named 'Dispose'"; Close() is public on
        # both 5.1 and pwsh 7 (Bugbot #582; pwsh-7 tests can't see the 5.1 break).
        $resp.Close()
    }
}

# Resolve a cosign we can vouch for: one already on PATH, else a pinned build
# fetched and checked against sigstore's own published checksums. Returns the
# path, or $null when it cannot be obtained — the caller decides what that means.
#
# A cosign we cannot vouch for is no better than no cosign, so a checksum
# mismatch returns $null rather than a usable path.
function Resolve-Cosign([string]$TmpDir) {
    $onPath = Get-Command cosign -ErrorAction SilentlyContinue
    if ($onPath) { return $onPath.Source }

    # BOTH architectures fetch the amd64 build, deliberately.
    #
    # Sigstore has never published a Windows arm64 cosign — not at $CosignVersion,
    # not at any release. `cosign-windows-amd64.exe` is the only Windows asset
    # there is, so asking for a per-arch name 404s and blocks Windows-on-ARM
    # permanently (tracebloc/client#734, fixed there the same way).
    #
    # Running it under Windows-on-ARM's x64 emulation costs nothing that matters:
    # cosign verifies a signature over BYTES, so the instruction set it was
    # compiled for cannot change the verdict, and the artifact we hand it is
    # still the native arm64 binary. It is checksum-verified below exactly as on
    # amd64, so the trust chain is identical.
    #
    # Do not "fix" this to $arch. There is nothing on the other end — and
    # since the asset is arch-independent there is nothing to branch on
    # either; whether it RUNS here is Test-CosignRuns' question, not ours.
    $base  = "https://github.com/sigstore/cosign/releases/download/$CosignVersion"
    $asset = 'cosign-windows-amd64.exe'
    $bin   = Join-Path $TmpDir 'cosign.exe'
    $sums  = Join-Path $TmpDir 'cosign_checksums.txt'

    Write-Host "  cosign not found — downloading pinned cosign $CosignVersion (~180 MB) to verify the signature..."
    try {
        # Bounded (backend#2199). cosign-windows-amd64.exe is ~178 MB at the pinned
        # v2.4.1 (186,998,456 B) — the Windows build is far larger than the other
        # platforms' ~105 MB. 300 MB leaves headroom for growth while still refusing a
        # runaway; REVISIT this cap whenever $CosignVersion is bumped to a larger build.
        # cosign_checksums.txt is a few KB, so it gets a tight 1 MB / 60 s.
        Save-BoundedFile -Uri "$base/$asset"                -OutFile $bin  -MaxBytes 300MB
        Save-BoundedFile -Uri "$base/cosign_checksums.txt"  -OutFile $sums -MaxBytes 1MB -TimeoutSec 60
    } catch {
        Write-Host "  ⚠ couldn't download cosign: $($_.Exception.Message)"
        return $null
    }

    # cosign_checksums.txt lines: "<sha256>  <asset>".
    $want = $null
    foreach ($line in Get-Content -LiteralPath $sums) {
        $parts = @($line -split '\s+' | Where-Object { $_ -ne '' })
        if ($parts.Count -ge 2 -and $parts[-1] -eq $asset) { $want = $parts[0].ToLower(); break }
    }
    if (-not $want) { return $null }
    if ((Get-Sha256 $bin) -ne $want) {
        Write-Host "  Bootstrapped cosign failed its own checksum — not using it." -ForegroundColor Red
        return $null
    }
    Write-Host "  ✓ cosign $CosignVersion downloaded and checksum-verified"
    return $bin
}

# Can this cosign actually EXECUTE here? A trivial `cosign version`.
#
# A binary that will not start reports through the same channel as a signature
# that did not verify, and those warrant opposite reactions — only one of them
# means the artifact may be tampered with. Windows-on-ARM makes it real: the
# amd64 build needs x64 emulation, and where that is absent cosign never runs.
# The 255 preset means a binary that never starts cannot leave a stale 0 behind.
function Test-CosignRuns([string]$Cosign) {
    $global:LASTEXITCODE = 255
    $prev = $ErrorActionPreference
    try {
        $ErrorActionPreference = 'Continue'
        & $Cosign version 2>&1 | Out-Null
    } catch {
        return $false
    } finally {
        $ErrorActionPreference = $prev
    }
    return ($LASTEXITCODE -eq 0)
}

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
    # Bounded, exactly like the cosign bootstrap above (backend#2544 finishes
    # what backend#2199 started). These artifacts are SHA256- and cosign-verified
    # AFTER download, so a corrupted or oversized body is rejected before use —
    # but an unbounded one can still hang the install or exhaust disk BEFORE that
    # verification runs, the same DoS the bootstrap fetch closed. So every fetch
    # goes through Save-BoundedFile (size ceiling + timeout) rather than a raw
    # Invoke-WebRequest. Caps are sized against the real published assets with
    # headroom for growth; REVISIT the binary cap as the CLI grows:
    #   binary      ~50 MB today (windows-amd64 is 49.9 MB at v0.10.x) → 200 MB
    #   SHA256SUMS  <1 KB → a tight 1 MB / 60 s, mirroring cosign_checksums.txt
    # Wrapped so a bounded-fetch throw — cap tripped, timeout, 404, oversized
    # body — surfaces as Fail's clean one-line error, not the raw PowerShell
    # error record + stack trace that an uncaught throw under `Stop` produces.
    # These two are mandatory (unlike cosign, there is no fallback), so the
    # right response is a clean hard failure, not the bootstrap's warn-and-continue.
    try {
        Write-Host "Downloading binary..."
        Save-BoundedFile -Uri "$baseUrl/$binaryFile" -OutFile (Join-Path $tmpDir $binaryFile) -MaxBytes 200MB

        Write-Host "Downloading SHA256SUMS..."
        Save-BoundedFile -Uri "$baseUrl/SHA256SUMS" -OutFile (Join-Path $tmpDir 'SHA256SUMS') -MaxBytes 1MB -TimeoutSec 60
    } catch {
        Fail "couldn't download the release artifacts for ${tag}: $($_.Exception.Message)"
    }

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
    # Cosign signature verification — MANDATORY (RFC-0001 R8).
    #
    # The SHA256 above is same-channel: it comes from the same GitHub
    # release as the binary, so whoever could swap the binary could swap
    # SHA256SUMS with it. It proves the download completed, not who built
    # it. The cosign signature is the independent, Sigstore-rooted proof
    # that tracebloc's release workflow produced these bytes.
    #
    # So this no longer skips when cosign is absent — it bootstraps a
    # pinned, checksum-verified cosign, and FAILS CLOSED when it cannot.
    # This mirrors install.sh exactly; Windows was the one platform still
    # installing on the checksum alone (backend#2078).
    #
    # TRACEBLOC_ALLOW_UNVERIFIED=1 covers "cannot verify" — no cosign, no
    # .sig/.cert. It deliberately does NOT cover a verification that ran
    # and FAILED: that is evidence of tampering, and no env var overrides
    # it.
    # -------------------------------------------------------------
    $cosign = Resolve-Cosign $tmpDir

    if ($cosign -and -not (Test-CosignRuns $cosign)) {
        # Distinct from "no cosign": we have one, it just won't start here.
        # On Windows-on-ARM that means x64 emulation is missing or blocked;
        # it can also be SmartScreen/AV quarantine or a policy block. Saying
        # "install cosign" here would be useless advice — one is installed.
        if ($AllowUnverified) {
            Write-Host "  WARNING: cosign is present but won't run here — signature NOT" -ForegroundColor Yellow
            Write-Host "  verified (TRACEBLOC_ALLOW_UNVERIFIED=1)." -ForegroundColor Yellow
            $cosign = $null
        } else {
            Write-Host "Error: cosign was found but won't execute on this machine, so the" -ForegroundColor Red
            Write-Host "       signature can't be verified (RFC-0001 R8)." -ForegroundColor Red
            Write-Host "       On Windows-on-ARM this usually means x64 emulation is" -ForegroundColor Red
            Write-Host "       unavailable; it can also be a quarantine or policy block." -ForegroundColor Red
            Write-Host "       Fix that, or for a constrained environment re-run with" -ForegroundColor Red
            Write-Host "       TRACEBLOC_ALLOW_UNVERIFIED=1." -ForegroundColor Red
            exit 1
        }
    }

    if (-not $cosign) {
        if (-not $AllowUnverified) {
            Write-Host "Error: cosign is required to verify the binary's signature and" -ForegroundColor Red
            Write-Host "       could not be found or bootstrapped — refusing to install on" -ForegroundColor Red
            Write-Host "       an unauthenticated, same-channel checksum alone (RFC-0001 R8)." -ForegroundColor Red
            Write-Host "       Fix: install cosign and re-run —" -ForegroundColor Red
            Write-Host "         https://docs.sigstore.dev/cosign/system_config/installation/" -ForegroundColor Red
            Write-Host "       or for a constrained environment re-run with" -ForegroundColor Red
            Write-Host "       TRACEBLOC_ALLOW_UNVERIFIED=1." -ForegroundColor Red
            exit 1
        }
        Write-Host "  WARNING: cosign unavailable and couldn't be bootstrapped —" -ForegroundColor Yellow
        Write-Host "  signature NOT verified (TRACEBLOC_ALLOW_UNVERIFIED=1). The SHA256" -ForegroundColor Yellow
        Write-Host "  above is same-channel only; do not use this path in production." -ForegroundColor Yellow
    } else {
        Write-Host "Verifying cosign signature..."

        # "download .sig/.cert" and "verify the downloaded sig" must stay
        # separate. With $ErrorActionPreference = 'Stop', a Write-Error
        # inside the try-block is thrown and caught by the same catch that
        # handles a missing sig — so a FAILED verify silently downgrades to
        # "skip + continue" (Bugbot, PR #11). The verify below therefore
        # runs OUTSIDE any try/catch: & invokes cosign as an external
        # process, whose non-zero $LASTEXITCODE cannot be caught anyway.
        $sigDownloaded = $false
        try {
            # Bounded too (backend#2544). Both are tiny — .sig ~96 B, .cert ~3.2 KB
            # — so a 1 MB / 60 s profile bounds a runaway with vast headroom. A
            # Save-BoundedFile throw (cap tripped, timeout, 404, oversized body)
            # lands in the same catch that already handles a missing .sig/.cert:
            # fail closed unless TRACEBLOC_ALLOW_UNVERIFIED=1.
            Save-BoundedFile -Uri "$baseUrl/$binaryFile.sig"  -OutFile (Join-Path $tmpDir "$binaryFile.sig")  -MaxBytes 1MB -TimeoutSec 60
            Save-BoundedFile -Uri "$baseUrl/$binaryFile.cert" -OutFile (Join-Path $tmpDir "$binaryFile.cert") -MaxBytes 1MB -TimeoutSec 60
            $sigDownloaded = $true
        } catch {
            if (-not $AllowUnverified) {
                Write-Host "Error: couldn't download $binaryFile.sig / .cert for $tag — the" -ForegroundColor Red
                Write-Host "       release is unsigned or incomplete. Every supported release" -ForegroundColor Red
                Write-Host "       is cosign-signed; refusing to install unverified (RFC-0001 R8)." -ForegroundColor Red
                Write-Host '       Pin a signed $env:RELEASE_VERSION, or re-run with TRACEBLOC_ALLOW_UNVERIFIED=1.' -ForegroundColor Red
                exit 1
            }
            Write-Host "  WARNING: .sig/.cert not published for $tag — signature NOT" -ForegroundColor Yellow
            Write-Host "  verified (TRACEBLOC_ALLOW_UNVERIFIED=1)." -ForegroundColor Yellow
        }

        if ($sigDownloaded) {
            # PRESET 255, exactly as Test-CosignRuns does — and for the same reason,
            # which this call site was missing (Bugbot, HIGH, cli#528).
            #
            # $LASTEXITCODE persists from the PREVIOUS command. Test-CosignRuns runs
            # `cosign version` immediately before this, so a shim that exits 0 there
            # and then never sets an exit code on verify-blob leaves $LASTEXITCODE at
            # 0 — and the `-ne 0` check below reads that stale success as a valid
            # signature. The installer then prints "cosign signature valid" and
            # installs a binary nothing verified.
            #
            # 255 means "no verdict yet": a verifier that never runs cannot inherit a
            # pass. Only cosign actually completing can bring it back to 0. That is
            # the whole guarantee behind RFC-0001 R8, and it was one line away.
            $global:LASTEXITCODE = 255
            & $cosign verify-blob `
                --certificate-identity-regexp "https://github.com/$GitHubRepo/.github/workflows/release.yml@refs/tags/v.*" `
                --certificate-oidc-issuer 'https://token.actions.githubusercontent.com' `
                --certificate (Join-Path $tmpDir "$binaryFile.cert") `
                --signature   (Join-Path $tmpDir "$binaryFile.sig") `
                (Join-Path $tmpDir $binaryFile) 2>$null
            if ($LASTEXITCODE -ne 0) {
                # No TRACEBLOC_ALLOW_UNVERIFIED branch here, deliberately.
                # Verification RAN and said no.
                Write-Host "Error: cosign signature verification FAILED — refusing to install." -ForegroundColor Red
                exit 1
            }
            Write-Host "  ✓ cosign signature valid"
        }
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
