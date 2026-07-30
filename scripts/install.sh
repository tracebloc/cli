#!/usr/bin/env sh
# tracebloc CLI installer for Linux + macOS.
#
# Usage:
#   curl -fsSL https://github.com/tracebloc/cli/releases/latest/download/install.sh | sh
#   curl -fsSL https://github.com/tracebloc/cli/releases/latest/download/install.sh | sh -s -- --version v0.1.0
#
# What it does:
#   1. Detects OS (linux/darwin) + arch (amd64/arm64) of the host
#   2. Resolves the latest release tag (or honors --version)
#   3. Downloads tracebloc-<tag>-<os>-<arch> from the GitHub Release
#   4. Verifies SHA256 against the release's SHA256SUMS file
#   5. Verifies the cosign signature — MANDATORY (RFC-0001 R8). If cosign isn't
#      installed it bootstraps a pinned, checksum-verified one; if it can't, it
#      FAILS CLOSED (never silently skips, never trusts the same-channel SHA256
#      alone). Override only with TRACEBLOC_ALLOW_UNVERIFIED=1.
#   6. Installs to /usr/local/bin/tracebloc (falls back to $HOME/.local/bin
#      with PATH advice if /usr/local/bin isn't writable)
#
# Why /bin/sh + POSIX-only constructs:
#   The customer's distro might not have bash. /bin/sh is POSIX-mandated.
#   No bashisms (no [[ ]], no <(), no ${var/...}). Tested against dash,
#   busybox sh, and bash.

set -eu

# --------------------------------------------------------------------
# Configuration knobs (override via env or args).
# --------------------------------------------------------------------
INSTALL_PREFIX="${INSTALL_PREFIX:-/usr/local/bin}"
RELEASE_VERSION="${RELEASE_VERSION:-latest}"
GITHUB_REPO="tracebloc/cli"
BINARY_NAME="tracebloc"

# Cosign signature verification is MANDATORY on the default path (RFC-0001 R8,
# backend#889). The previous build silently SKIPPED it when cosign was absent —
# the default on a fresh box — degrading to a SHA256 fetched over the same
# channel as the binary, which an on-path attacker also controls. We now require
# a signature: if cosign isn't present we bootstrap a pinned, checksum-verified
# one; if we can't, we FAIL CLOSED. This explicit opt-out is the only way past,
# for the genuinely-constrained operator, and it shouts.
ALLOW_UNVERIFIED="${TRACEBLOC_ALLOW_UNVERIFIED:-0}"
# Pin kept in lockstep with the release workflow's cosign-installer and the
# client installer's COSIGN_VERSION.
COSIGN_VERSION="${COSIGN_VERSION:-v2.4.1}"
COSIGN_BIN=""

usage() {
    cat <<EOF
tracebloc CLI installer

Usage:
  install.sh [--version <tag>] [--prefix <dir>] [--help]

Options:
  --version <tag>   Install a specific version (e.g. v0.1.0). Default: latest.
  --prefix <dir>    Install directory. Default: /usr/local/bin (falls back to
                    \$HOME/.local/bin if not writable).
  --help            Show this help.

Environment overrides:
  RELEASE_VERSION   Same as --version.
  INSTALL_PREFIX    Same as --prefix.
EOF
}

# --------------------------------------------------------------------
# Arg parsing — minimal POSIX-shell loop, no getopt (not portable).
# --------------------------------------------------------------------
while [ $# -gt 0 ]; do
    case "$1" in
        --version)
            RELEASE_VERSION="$2"
            shift 2
            ;;
        --prefix)
            INSTALL_PREFIX="$2"
            shift 2
            ;;
        --help|-h)
            usage
            exit 0
            ;;
        *)
            echo "Error: unknown argument: $1" >&2
            usage >&2
            exit 1
            ;;
    esac
done

# --------------------------------------------------------------------
# Detect OS + arch.
# --------------------------------------------------------------------
detect_os() {
    uname_s="$(uname -s)"
    case "$uname_s" in
        Linux)  echo "linux" ;;
        Darwin) echo "darwin" ;;
        *)
            echo "Error: unsupported OS: $uname_s" >&2
            echo "tracebloc CLI is released for linux + darwin via this script;" >&2
            echo "Windows users can run install.ps1 from a PowerShell prompt." >&2
            exit 1
            ;;
    esac
}

detect_arch() {
    uname_m="$(uname -m)"
    case "$uname_m" in
        x86_64|amd64)                   echo "amd64" ;;
        arm64|aarch64)                  echo "arm64" ;;
        i386|i686)                      echo "386" ;;
        armv6l|armv7l|armv8l|armhf|arm) echo "arm" ;;
        *)
            echo "Error: unsupported arch: $uname_m" >&2
            echo "tracebloc CLI ships linux binaries for amd64, arm64, 386, and arm;" >&2
            echo "if you need another arch, please file an issue at github.com/tracebloc/cli." >&2
            exit 1
            ;;
    esac
}

OS="$(detect_os)"
ARCH="$(detect_arch)"

# --------------------------------------------------------------------
# sha256 helper (coreutils sha256sum on Linux, shasum -a 256 on macOS).
# Echoes the digest, or returns non-zero if neither tool is present.
# --------------------------------------------------------------------
sha256_of() {
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$1" | awk '{print $1}'
    elif command -v shasum >/dev/null 2>&1; then
        shasum -a 256 "$1" | awk '{print $1}'
    else
        return 1
    fi
}

# --------------------------------------------------------------------
# Resolve a usable cosign into $COSIGN_BIN. Prefer one already on PATH;
# otherwise download the pinned release binary for this OS/arch and verify it
# against cosign's own published checksums before trusting it (a cosign we
# can't vouch for is no better than none). Returns non-zero if cosign can be
# neither found nor safely bootstrapped — the caller then fails closed.
# --------------------------------------------------------------------
ensure_cosign() {
    if command -v cosign >/dev/null 2>&1; then
        COSIGN_BIN="cosign"
        return 0
    fi

    # cosign publishes assets named cosign-<os>-<arch> (arch in amd64/arm64);
    # 386/arm have no official cosign build, so bootstrapping isn't possible there.
    cosign_arch=""
    case "$ARCH" in
        amd64) cosign_arch="amd64" ;;
        arm64) cosign_arch="arm64" ;;
        *) return 1 ;;
    esac

    # COSIGN_VERSION is env-overridable and gets interpolated into the Sigstore
    # download URL, so it needs the same semver + path-traversal gate as the
    # release tag — a crafted value must not redirect which release path we fetch.
    validate_version_tag "$COSIGN_VERSION" "cosign version" \
        "Set COSIGN_VERSION to a published cosign release tag (e.g. v2.4.1)."

    cbase="https://github.com/sigstore/cosign/releases/download/${COSIGN_VERSION}"
    casset="cosign-${OS}-${cosign_arch}"
    cbin="$TMP/cosign"
    csums="$TMP/cosign_checksums.txt"

    echo "  cosign not found — bootstrapping pinned ${COSIGN_VERSION} to verify the signature..."
    # dl() carries the TLS 1.2 floor + stall-based bounding (see the helper) —
    # never negotiate below TLS 1.2 to pull the verifier we then trust to
    # authenticate the release, and never let a dead endpoint wedge the
    # install. No wall-clock cap: the ~90MB cosign binary must be allowed to
    # finish on slow links (review #426).
    if ! dl "$cbase/$casset" "$cbin" 2>/dev/null; then return 1; fi
    if ! dl "$cbase/cosign_checksums.txt" "$csums" 2>/dev/null; then return 1; fi

    cwant="$(grep " ${casset}\$" "$csums" | awk '{print $1}' | head -1)"
    [ -n "$cwant" ] || return 1
    cgot="$(sha256_of "$cbin")" || return 1
    if [ "$cwant" != "$cgot" ]; then
        echo "Error: bootstrapped cosign failed its own checksum — not using it." >&2
        return 1
    fi
    chmod +x "$cbin"
    COSIGN_BIN="$cbin"
    return 0
}

# --------------------------------------------------------------------
# Resolve the release tag if "latest".
# --------------------------------------------------------------------
resolve_tag() {
    if [ "$RELEASE_VERSION" != "latest" ]; then
        echo "$RELEASE_VERSION"
        return
    fi
    # Use the redirect-trail of /releases/latest to learn the tag —
    # avoids hitting the rate-limited /api/repos endpoint for the
    # zero-auth one-liner case.
    redirect_url="$(curl -fsSI --tlsv1.2 --connect-timeout 30 --max-time 30 \
        "https://github.com/${GITHUB_REPO}/releases/latest" \
        | awk '/^[Ll]ocation:/ { print $2 }' \
        | tr -d '\r')"
    if [ -z "$redirect_url" ]; then
        echo "Error: couldn't resolve the 'latest' release tag from GitHub." >&2
        echo "Pass --version <tag> to install a specific release." >&2
        exit 1
    fi
    # The redirect URL ends in /tag/<vX.Y.Z>; basename gives us the tag.
    basename "$redirect_url"
}

# --------------------------------------------------------------------
# Validate the resolved tag before it flows into a download URL.
#
# --version / RELEASE_VERSION is returned by resolve_tag verbatim and then
# interpolated into BASE_URL=.../releases/download/${TAG}. An unvalidated value
# such as 'v1.2.3-../../heads/main' would let curl collapse the '..' and fetch
# from a path other than the intended release — a path-traversal lever in the
# most security-sensitive download in the installer. Constrain it to a release
# tag shape and refuse any '/' or '..' (RFC-0001 R8, backend#889). Matches the
# client bootstrap's gate (^v[0-9]+\.[0-9]+\.[0-9]+([.-][A-Za-z0-9.]+)?$).
# validate_version_tag <value> <label> <hint>: refuse any value not shaped like
# a release tag before it is interpolated into a download URL. <label> names the
# thing in error messages and <hint> is the corrective suggestion.
validate_version_tag() {
    _vt_val="$1"; _vt_label="$2"; _vt_hint="$3"
    # Path-traversal belt: no separators, no parent-dir tokens.
    case "$_vt_val" in
        */*|*..*)
            echo "Error: $_vt_label '$_vt_val' contains a path separator or '..' —" >&2
            echo "       refusing to build a download URL from it (RFC-0001 R8)." >&2
            exit 1
            ;;
    esac
    # Shape: vMAJOR.MINOR.PATCH with an optional [.-]alnum/dot suffix. grep -E is
    # POSIX and already relied on elsewhere in this script; -q keeps it quiet.
    if ! printf '%s\n' "$_vt_val" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+([.-][A-Za-z0-9.]+)?$'; then
        echo "Error: '$_vt_val' is not a valid $_vt_label (expected vX.Y.Z, e.g. v0.1.0)." >&2
        echo "       $_vt_hint" >&2
        exit 1
    fi
}

validate_tag() {
    validate_version_tag "$1" "release tag" "Pass --version with a published release tag."
}

TAG="$(resolve_tag)"
validate_tag "$TAG"
echo "Installing tracebloc CLI $TAG ($OS/$ARCH)..."

# --------------------------------------------------------------------
# Download binary + SHA256SUMS + (optional) cosign sig/cert.
# --------------------------------------------------------------------
BINARY_FILE="${BINARY_NAME}-${TAG}-${OS}-${ARCH}"
BASE_URL="https://github.com/${GITHUB_REPO}/releases/download/${TAG}"

# Shared download profile for every body fetch in this script (review: #426).
# Stall-based bounding instead of a wall-clock cap: --max-time 300 made the
# ~50MB binary fail under ~1.4 Mbps and the ~90MB cosign bootstrap under
# ~2.6 Mbps — links that are slow but alive must be allowed to finish, while
# a dead connection (under 1 KiB/s for 60s straight) still aborts instead of
# wedging the install. TLS floor stays 1.2. Retune here, once.
# usage: dl <url> <dest>
dl() {
    curl -fsSL --tlsv1.2 --connect-timeout 30 --speed-limit 1024 --speed-time 60 "$1" -o "$2"
}

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT INT TERM

echo "Downloading binary..."
if ! dl "$BASE_URL/$BINARY_FILE" "$TMP/$BINARY_FILE"; then
    echo "Error: failed to download $BASE_URL/$BINARY_FILE" >&2
    exit 1
fi

echo "Downloading SHA256SUMS..."
if ! dl "$BASE_URL/SHA256SUMS" "$TMP/SHA256SUMS"; then
    echo "Error: failed to download SHA256SUMS — release may be malformed" >&2
    exit 1
fi

# --------------------------------------------------------------------
# Verify SHA256.
# --------------------------------------------------------------------
echo "Verifying SHA256..."
expected="$(grep " $BINARY_FILE$" "$TMP/SHA256SUMS" | awk '{print $1}')"
if [ -z "$expected" ]; then
    echo "Error: SHA256SUMS doesn't contain an entry for $BINARY_FILE" >&2
    echo "       — release artifacts may be incomplete." >&2
    exit 1
fi
# sha256sum (GNU coreutils) vs shasum -a 256 (macOS): sha256_of picks one.
# If neither is available, refuse to install — running an unverified
# binary from the internet is exactly what this script exists to
# prevent. Bugbot PR #11 caught the previous "warn + continue + still
# print ✓ matches" branch as both a security regression AND a
# misleading-log issue. Almost every modern Linux distro ships coreutils
# (sha256sum) by default; macOS ships /usr/bin/shasum as part of the
# base Perl install. A host with neither is unusual enough that
# erroring out is the right call — the customer can install coreutils
# / xcode-select / similar and re-run.
if ! actual="$(sha256_of "$TMP/$BINARY_FILE")"; then
    echo "Error: neither sha256sum nor shasum is on PATH — can't verify the" >&2
    echo "       downloaded binary's integrity. Install one of:" >&2
    echo "         apt install coreutils       # Debian/Ubuntu" >&2
    echo "         dnf install coreutils       # Fedora/RHEL" >&2
    echo "         apk add coreutils           # Alpine" >&2
    echo "         (macOS ships /usr/bin/shasum by default — PATH issue?)" >&2
    echo "       and re-run." >&2
    exit 1
fi
if [ "$actual" != "$expected" ]; then
    echo "Error: SHA256 mismatch!" >&2
    echo "  expected: $expected" >&2
    echo "  actual:   $actual" >&2
    echo "  refusing to install." >&2
    exit 1
fi
echo "  ✓ checksum matches"

# --------------------------------------------------------------------
# Verify the cosign signature — MANDATORY on the default path (RFC-0001 R8).
#
# The SHA256 check above proves the binary matches SHA256SUMS, but SHA256SUMS is
# fetched over the SAME channel as the binary — an on-path attacker who can swap
# the binary can swap the sums too. The cosign signature is the independent,
# Sigstore-rooted proof that tracebloc's release workflow produced these bytes.
# So we no longer "skip when cosign is absent": we require a verifier, bootstrap
# a pinned+checksummed cosign if one isn't installed, and FAIL CLOSED otherwise.
# The only escape is an explicit, loud TRACEBLOC_ALLOW_UNVERIFIED=1.
# --------------------------------------------------------------------
verify_cosign_signature() {
    if ! ensure_cosign; then
        if [ "$ALLOW_UNVERIFIED" = "1" ]; then
            echo "  WARNING: cosign unavailable and couldn't be bootstrapped —" >&2
            echo "  signature NOT verified (TRACEBLOC_ALLOW_UNVERIFIED=1). The SHA256" >&2
            echo "  above is same-channel only; do not use this path in production." >&2
            return 0
        fi
        echo "Error: cosign is required to verify the binary's signature and could" >&2
        echo "       not be found or bootstrapped — refusing to install on an" >&2
        echo "       unauthenticated, same-channel checksum alone (RFC-0001 R8)." >&2
        echo "       Fix: install cosign and re-run —" >&2
        echo "         https://docs.sigstore.dev/cosign/system_config/installation/" >&2
        echo "       (brew install cosign / apt / the released binary), or for a" >&2
        echo "       constrained environment re-run with TRACEBLOC_ALLOW_UNVERIFIED=1." >&2
        exit 1
    fi

    echo "Verifying cosign signature..."
    if ! dl "$BASE_URL/$BINARY_FILE.sig" "$TMP/$BINARY_FILE.sig" 2>/dev/null \
       || ! dl "$BASE_URL/$BINARY_FILE.cert" "$TMP/$BINARY_FILE.cert" 2>/dev/null; then
        if [ "$ALLOW_UNVERIFIED" = "1" ]; then
            echo "  WARNING: .sig/.cert not published for $TAG — signature NOT verified" >&2
            echo "  (TRACEBLOC_ALLOW_UNVERIFIED=1)." >&2
            return 0
        fi
        echo "Error: couldn't download $BINARY_FILE.sig / .cert for $TAG — the" >&2
        echo "       release is unsigned or incomplete. Every supported release is" >&2
        echo "       cosign-signed; refusing to install unverified (RFC-0001 R8)." >&2
        echo "       Pin a signed --version, or re-run with TRACEBLOC_ALLOW_UNVERIFIED=1." >&2
        exit 1
    fi

    if "$COSIGN_BIN" verify-blob \
            --certificate-identity-regexp \
              "https://github.com/${GITHUB_REPO}/.github/workflows/release.yml@refs/tags/v.*" \
            --certificate-oidc-issuer 'https://token.actions.githubusercontent.com' \
            --certificate "$TMP/$BINARY_FILE.cert" \
            --signature "$TMP/$BINARY_FILE.sig" \
            "$TMP/$BINARY_FILE" >/dev/null 2>&1; then
        echo "  ✓ cosign signature valid"
    else
        echo "Error: cosign signature verification FAILED — refusing to install." >&2
        exit 1
    fi
}
verify_cosign_signature

# --------------------------------------------------------------------
# Install to a writable prefix.
#
# Bugbot PR #11 r2 caught a UX bug in the previous flow: POSIX
# `test -w` is false for paths that don't exist, so a custom
# --prefix /opt/tracebloc (legitimate, just not created yet) was
# silently overridden with the ~/.local/bin fallback. The right
# semantic: try to mkdir the customer's chosen prefix; if THAT
# fails (no write perms on parent), THEN fall back.
# --------------------------------------------------------------------
PREFIX="$INSTALL_PREFIX"
if ! mkdir -p "$PREFIX" 2>/dev/null || [ ! -w "$PREFIX" ]; then
    # The customer's chosen prefix isn't usable (no write perms on
    # parent, /usr/local/bin without sudo, etc.). Prefer ~/bin when it
    # already exists on $PATH and is writable: the binary is then usable
    # in THIS shell (and every new one) with NO rc edit and no new
    # terminal — the least-privilege install's B2 goal (RFC 0001). We
    # only consider the conventional general-purpose ~/bin, never a
    # language-specific dir that merely happens to be on PATH (~/.cargo/bin,
    # ~/go/bin, …). Otherwise fall back to ~/.local/bin, which the
    # PATH-advice block below wires into the shell rc.
    home_bin="${HOME%/}/bin"
    home_bin_on_path=no
    # Match both "$home_bin" and a trailing-slash "$home_bin/" PATH entry — some
    # users have "$HOME/bin/" (with the slash) on PATH, which a bare
    # ":$home_bin:" pattern would miss and wrongly fall back to ~/.local/bin
    # (Bugbot #392).
    case ":$PATH:" in *":$home_bin:"*|*":$home_bin/:"*) home_bin_on_path=yes ;; esac
    # Guard HOME="" or "/": "${HOME%/}/bin" collapses to "/bin", so a root process
    # (HOME=/) with /bin writable + on PATH would drop the CLI into /bin. Require a
    # real, non-root $HOME before preferring ~/bin (Bugbot #392 r3).
    if [ -n "${HOME:-}" ] && [ "$HOME" != "/" ] \
       && [ "$home_bin_on_path" = yes ] && [ -d "$home_bin" ] && [ -w "$home_bin" ]; then
        echo "Note: $PREFIX isn't writable; installing to $home_bin (already on your PATH)."
        PREFIX="$home_bin"
    else
        FALLBACK="$HOME/.local/bin"
        echo "Note: $PREFIX isn't writable (couldn't mkdir or no -w); falling back to $FALLBACK"
        mkdir -p "$FALLBACK"
        PREFIX="$FALLBACK"
    fi
fi

chmod +x "$TMP/$BINARY_FILE"
mv "$TMP/$BINARY_FILE" "$PREFIX/$BINARY_NAME"

# Short alias: `tb` — the kubectl→k pattern for the most-typed word in the
# product. A symlink next to the binary, created only when the name is free
# (or already ours): never clobber an unrelated tool the user has as `tb`.
TB_ALIAS_NOTE=""
if command -v ln >/dev/null 2>&1; then
    if [ ! -e "$PREFIX/tb" ] || [ "$(readlink "$PREFIX/tb" 2>/dev/null)" = "$PREFIX/$BINARY_NAME" ]; then
        # Best-effort: an alias must never be able to fail the install.
        if ln -sf "$PREFIX/$BINARY_NAME" "$PREFIX/tb" 2>/dev/null; then
            TB_ALIAS_NOTE=" (short alias: tb)"
        fi
    else
        echo "Note: $PREFIX/tb already exists and isn't ours — skipping the tb alias."
    fi
fi

echo ""
echo "✓ tracebloc CLI installed: $PREFIX/$BINARY_NAME$TB_ALIAS_NOTE"
echo ""
echo "Verify with:"
echo "  $PREFIX/$BINARY_NAME version"
echo ""

# PATH handling. install.ps1 persists the PATH entry on Windows
# (SetEnvironmentVariable, User scope) — do the same on Unix by writing to
# the rc file the user's shell actually reads. The old print-only advice
# silently failed on Ubuntu: ~/.profile adds ~/.local/bin only at *login*
# and only if it already existed, but the installer creates it mid-session,
# so a new (non-login) terminal reading ~/.bashrc never picks it up.
#
# Decide whether to persist a PATH entry to the user's shell rc:
#   - a user-local prefix (under $HOME — the unprivileged `curl | sh` fallback)
#     ALWAYS needs one, even when a one-off `export` already put it on $PATH for
#     this shell (that won't survive into a new terminal);
#   - a non-$HOME prefix that ISN'T on $PATH (e.g. `--prefix /opt/tracebloc`)
#     also needs one;
#   - a non-$HOME prefix already on $PATH (e.g. the default /usr/local/bin) is
#     on PATH for every shell and needs nothing.
# ($HOME is stripped of a trailing slash first so a HOME like "/home/u/" can't
# misclassify "/home/u/.local/bin" via a "/home/u//*" pattern it won't match.)
home_dir="${HOME%/}"
persist=no
case "$PREFIX" in
    "$home_dir"/*) persist=yes ;;
    *) case ":$PATH:" in *":$PREFIX:"*|*":$PREFIX/:"*) ;; *) persist=yes ;; esac ;;
esac

# Is $PREFIX on the CURRENT shell's PATH? If so the binary is usable right now
# (this covers the ~/bin-already-on-PATH case). We still persist a $HOME prefix
# to the rc (a current-$PATH hit may be session-only — a one-off export, direnv —
# that new terminals won't have; Bugbot #392 r2), but the message must say
# "ready now" instead of nagging "open a new terminal" for a dir that IS on PATH.
on_path=no
# Trailing-slash tolerant, as above (Bugbot #392): a "$PREFIX/" PATH entry still
# means the binary is usable now, so don't nag "open a new terminal".
case ":$PATH:" in *":$PREFIX:"*|*":$PREFIX/:"*) on_path=yes ;; esac

# --------------------------------------------------------------------
# The ONE PATH block this installer owns.
#
# Before #433 the append was guarded only by "does the rc mention $PREFIX", so
# every install with a DIFFERENT prefix appended another block and the profile
# grew without bound: the v0.10.1 validation box collected TEN, each pointing at
# a temp dir that no longer existed. We now tag our block with $PATH_MARKER and
# a re-run REPLACES it instead of stacking a second one.
#
# Keep $PATH_MARKER byte-stable — matching it is exactly what lets a re-run find
# and clean up the blocks older installers left behind.
# --------------------------------------------------------------------
PATH_MARKER="# Added by the tracebloc CLI installer"

# The marker line we WRITE also records the directory, so a later run can prove
# it wrote both halves of the block:
#
#   # Added by the tracebloc CLI installer (prefix: /opt/tracebloc)
#   export PATH="/opt/tracebloc:$PATH"
#
# When the recorded prefix and the directory on the PATH line agree, the block is
# unambiguously ours and can be reclaimed even if that directory has since been
# deleted — which is what keeps the #433 cleanup working. $PATH_MARKER stays the
# stable *beginning* of the line (matched literally, never as a whole line) so
# blocks written by older installers are still recognised.
PATH_MARKER_LINE="$PATH_MARKER (prefix: $PREFIX)"

# _tb_owns_dir <recorded> <dir>: 0 if the block naming <dir>, under a marker that
# recorded <recorded> ("-" when it recorded nothing), is one we wrote.
#
# The marker alone is NOT proof. A marker left dangling by a hand-edit can end up
# directly above the USER's own PATH line, and taking that line with it would
# silently delete a PATH entry we never added — unrecoverable, and the one outcome
# this whole change exists to avoid (Bugbot on #434). So we require positive
# evidence, in order:
#   * a '$' anywhere in the dir -> never ours. We always write a literal,
#     already-expanded path; "$HOME/mytools" is the user's own idiom.
#   * the dir we are installing to right now -> safe to replace whoever wrote it,
#     since we are about to write that very line.
#   * the marker recorded this same dir -> we wrote the comment AND the line.
#     Existence is irrelevant here: a recorded prefix that has been deleted is
#     exactly the stale cruft #433 reported.
#   * a legacy marker (recorded nothing) -> the only proof left is a tracebloc
#     binary still sitting in the dir. A legacy marker over a VANISHED dir is
#     deliberately NOT claimed: it is indistinguishable from the user's own entry
#     for a directory they haven't created yet, so we keep their line and accept
#     that one pre-#433 dead entry may survive.
_tb_owns_dir() {
    _tow_rec="$1"
    _tow_dir="$2"
    case "$_tow_dir" in
        ''|-|*'$'*) return 1 ;;
    esac
    if [ "$_tow_dir" = "$PREFIX" ]; then return 0; fi
    if [ "$_tow_rec" != "-" ] && [ "$_tow_rec" = "$_tow_dir" ]; then return 0; fi
    if [ "$_tow_rec" = "-" ] && [ -d "$_tow_dir" ] && [ -e "$_tow_dir/$BINARY_NAME" ]; then return 0; fi
    return 1
}

# strip_tb_path_block <file>: echo <file> minus every block we can PROVE we wrote
# — the marker line, the PATH op on the line after it, and the blank separator on
# the line before, but only when _tb_owns_dir vouches for the directory named.
#
# A block we cannot claim is left completely intact, comment and all. Two reasons:
# we must never delete a PATH line we didn't write, and a pre-#433 block we can't
# claim is far more useful to the user still labelled "Added by the tracebloc CLI
# installer" (so they can see what it is and delete it) than reduced to a bare,
# unexplained PATH line. Every other line is passed through unchanged and in
# order: this is a file we do NOT own.
strip_tb_path_block() {
    _stb_file="$1"
    _stb_owned=""
    _stb_tab="$(printf '\t')"

    # Emit one record per marker: its line number, the prefix that marker recorded
    # ("-" for a legacy marker that recorded none), and the directory named on the
    # line below it ("-" when that line isn't a PATH op shaped like one we write).
    # Via a temp file rather than a pipe so the ownership verdicts land in THIS
    # shell, and rather than a heredoc so the awk program needs no
    # nested-expansion escaping.
    #
    # A marker is any line STARTING with $PATH_MARKER (index(...) == 1 is a
    # literal test, so a path full of regex metacharacters can't misfire).
    _stb_pairs="$TMP/tb.pairs"
    awk -v marker="$PATH_MARKER" '
        function recorded_prefix(line,   rest) {
            rest = substr(line, length(marker) + 1)
            # " (prefix: <dir>)" -> <dir>; anything else (including the bare
            # legacy marker) records nothing.
            if (rest ~ /^ \(prefix: .*\)$/) return substr(rest, 11, length(rest) - 11)
            return "-"
        }
        NR > 1 && prev_is_marker {
            dir = "-"
            if ($0 ~ /^[[:space:]]*export[[:space:]]+PATH="[^"]+:\$PATH"[[:space:]]*$/) {
                dir = $0
                sub(/^[^"]*"/, "", dir)
                sub(/:\$PATH".*$/, "", dir)
            } else if ($0 ~ /^[[:space:]]*fish_add_path[[:space:]]/) {
                dir = $0
                sub(/^[[:space:]]*fish_add_path[[:space:]]+/, "", dir)
                sub(/[[:space:]]+$/, "", dir)
                gsub(/^["\047]|["\047]$/, "", dir)
            }
            printf "%d\t%s\t%s\n", NR - 1, prev_rec, dir
        }
        {
            prev_is_marker = (index($0, marker) == 1)
            prev_rec = prev_is_marker ? recorded_prefix($0) : "-"
        }
        END { if (NR >= 1 && prev_is_marker) printf "%d\t%s\t-\n", NR, prev_rec }
    ' "$_stb_file" > "$_stb_pairs"

    while IFS="$_stb_tab" read -r _stb_no _stb_rec _stb_dir; do
        [ -n "$_stb_no" ] || continue
        if _tb_owns_dir "$_stb_rec" "$_stb_dir"; then
            _stb_owned="$_stb_owned,$_stb_no"
        fi
    done < "$_stb_pairs"

    awk -v owned="$_stb_owned" '
        BEGIN {
            n = split(owned, a, ","); for (i = 1; i <= n; i++) if (a[i] != "") own[a[i] + 0] = 1
        }
        { line[NR] = $0 }
        END {
            for (i = 1; i <= NR; i++) {
                if (!(i in own)) continue
                drop[i] = 1          # the marker
                drop[i + 1] = 1      # the PATH op under it (vouched for)
                if (i > 1 && line[i - 1] == "") drop[i - 1] = 1   # our blank separator
            }
            for (i = 1; i <= NR; i++) if (!(i in drop)) print line[i]
        }
    ' "$_stb_file"
}

# rc_lists_dir <dir> <file>: 0 if a non-comment PATH op in <file> already puts
# <dir> on PATH. Only an actual PATH op counts — a bare comment or an unrelated
# line that merely mentions the dir must NOT pass, or we'd claim success while a
# new shell still can't find the binary (#61). Handles PATH= / PATH+= / zsh's
# path+=() / fish_add_path.
#
# It compares whole path COMPONENTS. The old test was `grep -F "$PREFIX"`, a
# substring match: installing --prefix /opt/tb after /opt/tb2 matched the
# /opt/tb2 line and reported "already in your PATH config" for a directory that
# was on nobody's PATH (#433). Quotes and trailing slashes are tolerated on
# either side.
rc_lists_dir() {
    awk -v want="$1" '
        # exit 1, not a bare exit. A bare exit in BEGIN still runs END, so
        # the status would be the END rule below -- 1 when nothing was found --
        # but saying it outright means "no component to look for" can never be
        # read as "already listed" by a later reader or a stricter awk.
        BEGIN { sub(/\/+$/, "", want); if (want == "") exit 1 }
        /^[[:space:]]*#/ { next }
        {
            n = 0
            if ($0 ~ /(^|[^A-Za-z_])fish_add_path([^A-Za-z_]|$)/) {
                # Parse quotes instead of field-splitting. We WRITE
                # fish_add_path "$PREFIX", so a prefix containing spaces became
                # two whitespace fields here and never matched -- the installer
                # then appended a second block and claimed it had added a PATH
                # entry that was already present. The sibling reader further up
                # already keeps the remainder intact; this now agrees with it.
                rest = $0
                sub(/^.*fish_add_path[[:space:]]*/, "", rest)
                while (rest ~ /^-/) { sub(/^-[^[:space:]]*[[:space:]]*/, "", rest) }
                sub(/[[:space:]]+$/, "", rest)
                q = substr(rest, 1, 1)
                if (q == "\"" || q == "\047") {
                    # Quoted: everything up to the closing quote is ONE path,
                    # spaces included.
                    body = substr(rest, 2)
                    close_at = index(body, q)
                    cand[++n] = (close_at > 0) ? substr(body, 1, close_at - 1) : body
                } else {
                    # Unquoted: several bare paths may share the line.
                    n = split(rest, cand, /[[:space:]]+/)
                }
            } else if ($0 ~ /(^|[^A-Za-z_])[Pp][Aa][Tt][Hh][+]?=/) {
                value = $0
                sub(/^[^=]*=/, "", value)
                n = split(value, cand, ":")
            } else {
                next
            }
            for (i = 1; i <= n; i++) {
                dir = cand[i]
                gsub(/[\047"()]/, "", dir)
                sub(/\/+$/, "", dir)
                if (dir == want) { found = 1; exit }
            }
        }
        END { exit(found ? 0 : 1) }
    ' "$2"
}

# rc_same <fileA> <fileB>: 0 if the two files hold the same text. Used to skip
# the write entirely when the rc already says what we were going to say —
# `tracebloc upgrade` re-execs this installer, so the same prefix comes back
# around on every upgrade and a file we don't own must not be rewritten to no
# effect.
#
# Deliberately not cmp(1): cmp ships in diffutils, which a minimal container
# image can lack, and a missing tool would silently turn "leave the file alone"
# into "rewrite it every run". $(...) strips trailing newlines on both sides
# equally, which is harmless here — a missing block is a far bigger difference
# than a final newline.
rc_same() {
    [ "$(cat "$1")" = "$(cat "$2")" ]
}

# replace_rc <file>: make $rc's contents exactly <file>'s. Truncates the
# existing path rather than mv'ing a temp over it, so the inode, mode and owner
# survive — and so an rc that is a SYMLINK into a dotfiles repo is written
# THROUGH instead of being silently replaced by a regular file.
replace_rc() {
    if { cat "$1" > "$rc"; } 2>/dev/null; then
        return 0
    fi
    # The redirection truncates before cat writes, so a write that dies partway
    # (no space left, a vanishing mount) would leave the user's rc in pieces.
    # Put the original contents back — best effort, but far better than leaving a
    # file we don't own half-written. The caller then reports `failed` and prints
    # the line to add by hand.
    { cat "$rc_now" > "$rc"; } 2>/dev/null || true
    return 1
}

if [ "$persist" = "yes" ]; then
    shell_name="$(basename "${SHELL:-sh}")"
    case "$shell_name" in
        zsh)  rc="$HOME/.zshrc" ;;
        bash)
            # macOS Terminal opens a login shell (reads .bash_profile);
            # Linux terminals are interactive non-login (read .bashrc).
            if [ "$OS" = "darwin" ]; then rc="$HOME/.bash_profile"; else rc="$HOME/.bashrc"; fi
            ;;
        fish) rc="$HOME/.config/fish/config.fish" ;;
        *)    rc="$HOME/.profile" ;;
    esac

    if [ "$shell_name" = "fish" ]; then
        # Quoted, like the client installer's fish hint: a --prefix containing a
        # space must survive into the rc as one argument.
        path_line="fish_add_path \"$PREFIX\""
    else
        path_line="export PATH=\"$PREFIX:\$PATH\""
    fi

    # fish's rc lives in ~/.config/fish/, which may not exist yet; the others sit
    # directly in $HOME. Create the parent or the write below has nowhere to go.
    mkdir -p "$(dirname "$rc")" 2>/dev/null || true

    # Read the rc as it stands (it may not exist yet) and compute what it looks
    # like with our block removed. Deciding from the STRIPPED copy is what makes
    # this idempotent: whatever we wrote on an earlier run is out of the way, so
    # "is $PREFIX already handled?" is answered by the user's own lines only, and
    # our block gets rewritten rather than duplicated.
    rc_now="$TMP/rc.current"
    rc_next="$TMP/rc.next"
    rc_want="$TMP/rc.want"
    : > "$rc_now"
    if [ -f "$rc" ]; then cat "$rc" > "$rc_now" 2>/dev/null || : > "$rc_now"; fi
    strip_tb_path_block "$rc_now" > "$rc_next"

    # Build the contents the rc SHOULD have. If a line we don't own already puts
    # $PREFIX on PATH we add nothing — the stripped copy is already the answer,
    # and any block of ours it removed was redundant.
    cat "$rc_next" > "$rc_want"
    if rc_lists_dir "$PREFIX" "$rc_next"; then
        persisted=yes
    else
        persisted=no
        printf '\n%s\n%s\n' "$PATH_MARKER_LINE" "$path_line" >> "$rc_want"
    fi

    # Six outcomes, tracked precisely so the closing message can neither over- nor
    # under-claim. The two failure states are distinct on purpose: only one of them
    # means the user's PATH is actually wrong (Bugbot on #434).
    #   present     — the rc already says this; nothing written
    #   added       — our block appended to an rc that had none of ours
    #   replaced    — our stale block rewritten to name $PREFIX
    #   tidied      — $PREFIX was already persisted by the user; we removed a
    #                 redundant block of ours
    #   tidy_failed — as `tidied`, but the rewrite failed. PATH is still correct,
    #                 so this is cosmetic and must NOT ask for a manual edit.
    #   failed      — $PREFIX is not persisted and we could not write. The only
    #                 case that warrants manual instructions.
    state=failed
    if rc_same "$rc_want" "$rc_now"; then
        # Never touch a file we don't own to no effect — the re-install and
        # `tracebloc upgrade` path lands here.
        state=present
    elif rc_same "$rc_next" "$rc_now"; then
        # Nothing of ours was stripped, so the only difference is our new block:
        # append rather than rewriting the whole file. (Reachable only when
        # $persisted is no — otherwise rc_want would equal rc_now above.)
        #
        # Group the append so the redirection-open error (e.g. a read-only rc, or
        # an unwritable parent dir) is suppressed too: `cmd >> "$rc" 2>/dev/null`
        # leaks the shell's "Permission denied" because the >> open is attempted
        # before 2>/dev/null applies. Wrapping in { ... } 2>/dev/null puts the
        # stderr redirect in scope first.
        if { printf '\n%s\n%s\n' "$PATH_MARKER_LINE" "$path_line" >> "$rc"; } 2>/dev/null; then
            state=added
        fi
    elif replace_rc "$rc_want"; then
        if [ "$persisted" = yes ]; then state=tidied; else state=replaced; fi
    elif [ "$persisted" = yes ]; then
        state=tidy_failed
    fi

    echo ""
    case "$state" in
        added)
            if [ "$on_path" = yes ]; then
                # Usable in THIS shell already; the rc line is just so new
                # terminals find it too (covers a session-only $PATH hit).
                echo "tracebloc is ready to use now."
                echo "Also added $PREFIX to $rc so new terminals find it too."
            else
                echo "Added $PREFIX to your PATH in $rc."
                echo "Open a new terminal — or load it now:  . \"$rc\""
            fi
            ;;
        replaced)
            # An earlier install left a PATH line for a different prefix. We
            # updated that one line in place instead of stacking another (#433).
            echo "Updated the tracebloc PATH entry in $rc to $PREFIX."
            if [ "$on_path" = yes ]; then
                echo "tracebloc is ready to use now."
            else
                echo "Open a new terminal — or load it now:  . \"$rc\""
            fi
            ;;
        present|tidied)
            # `tidied` differs only in that we also dropped a redundant block of
            # ours; either way the rc already persists $PREFIX, so the user has
            # nothing to do.
            if [ "$on_path" = yes ]; then
                echo "tracebloc is ready to use now ($PREFIX is on your PATH)."
            else
                echo "$PREFIX is already in your PATH config ($rc) — nothing to add."
                echo "If a new terminal can't find it yet, open one — or load it now:  . \"$rc\""
            fi
            ;;
        tidy_failed)
            # $PREFIX IS persisted — by a line we don't own — and all we failed to
            # do is remove a now-redundant block of ours. Telling the user to add a
            # PATH line by hand here would be plain wrong: their PATH is correct.
            # Say what actually happened and leave it at that (Bugbot on #434).
            if [ "$on_path" = yes ]; then
                echo "tracebloc is ready to use now ($PREFIX is on your PATH)."
            else
                echo "$PREFIX is already in your PATH config ($rc) — nothing to add."
                echo "If a new terminal can't find it yet, open one — or load it now:  . \"$rc\""
            fi
            echo "Note: couldn't remove a leftover tracebloc PATH line from $rc"
            echo "      (not writable). Harmless — your PATH is already correct."
            ;;
        *)
            # Usable in THIS shell already ($PREFIX on PATH) — say so even though
            # we couldn't persist it for new terminals (Bugbot #392 r3).
            [ "$on_path" = yes ] && echo "tracebloc is ready to use now ($PREFIX is on your PATH)."
            echo "Note: the installer couldn't update your shell config ($rc)."
            echo "Add this line to it (or your shell's startup file), then open a new terminal:"
            echo "  $path_line"
            ;;
    esac
    echo ""
fi

echo "First steps:"
echo "  $BINARY_NAME cluster info        # confirm the CLI can reach your cluster"
echo "  $BINARY_NAME data ingest --help  # stage a dataset onto the cluster"
echo ""
echo "Short alias: tb works everywhere tracebloc does (tb data ingest ./data)"
