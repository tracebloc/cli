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
    # --tlsv1.2 floor for the cosign bootstrap fetch, matching the client
    # installer's curls — never negotiate below TLS 1.2 to pull the verifier we
    # then trust to authenticate the release.
    if ! curl -fsSL --tlsv1.2 "$cbase/$casset" -o "$cbin" 2>/dev/null; then return 1; fi
    if ! curl -fsSL --tlsv1.2 "$cbase/cosign_checksums.txt" -o "$csums" 2>/dev/null; then return 1; fi

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
    redirect_url="$(curl -fsSI --tlsv1.2 \
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

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT INT TERM

echo "Downloading binary..."
if ! curl -fsSL --tlsv1.2 "$BASE_URL/$BINARY_FILE" -o "$TMP/$BINARY_FILE"; then
    echo "Error: failed to download $BASE_URL/$BINARY_FILE" >&2
    exit 1
fi

echo "Downloading SHA256SUMS..."
if ! curl -fsSL --tlsv1.2 "$BASE_URL/SHA256SUMS" -o "$TMP/SHA256SUMS"; then
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
    if ! curl -fsSL --tlsv1.2 "$BASE_URL/$BINARY_FILE.sig" -o "$TMP/$BINARY_FILE.sig" 2>/dev/null \
       || ! curl -fsSL --tlsv1.2 "$BASE_URL/$BINARY_FILE.cert" -o "$TMP/$BINARY_FILE.cert" 2>/dev/null; then
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
              "https://github.com/${GITHUB_REPO}/.github/workflows/release.yml@.*" \
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
    # parent, /usr/local/bin without sudo, etc.). Fall back to a
    # per-user dir; the PATH-advice block below tells them how to
    # pick it up.
    FALLBACK="$HOME/.local/bin"
    echo "Note: $PREFIX isn't writable (couldn't mkdir or no -w); falling back to $FALLBACK"
    mkdir -p "$FALLBACK"
    PREFIX="$FALLBACK"
fi

chmod +x "$TMP/$BINARY_FILE"
mv "$TMP/$BINARY_FILE" "$PREFIX/$BINARY_NAME"

# Short alias: `tb` — the kubectl→k pattern for the most-typed word in the
# product. A symlink next to the binary, created only when the name is free
# (or already ours): never clobber an unrelated tool the user has as `tb`.
if [ ! -e "$PREFIX/tb" ] || [ "$(readlink "$PREFIX/tb" 2>/dev/null)" = "$PREFIX/$BINARY_NAME" ]; then
    ln -sf "$PREFIX/$BINARY_NAME" "$PREFIX/tb"
    TB_ALIAS_NOTE=" (short alias: tb)"
else
    echo "Note: $PREFIX/tb already exists and isn't ours — skipping the tb alias."
    TB_ALIAS_NOTE=""
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
    *) case ":$PATH:" in *":$PREFIX:"*) ;; *) persist=yes ;; esac ;;
esac

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
        path_line="fish_add_path $PREFIX"
    else
        path_line="export PATH=\"$PREFIX:\$PATH\""
    fi

    # Track three outcomes precisely so the message can neither over- nor
    # under-claim: already configured / freshly added / couldn't write.
    state=failed
    mkdir -p "$(dirname "$rc")" 2>/dev/null || true
    # Idempotency: only an actual, non-comment PATH op that references $PREFIX
    # counts as "already configured" — a bare comment or an unrelated line that
    # merely mentions the dir must NOT pass, or we'd claim success while a new
    # shell still can't find the binary (#61). Match PATH= / PATH+= /
    # fish_add_path / zsh's path+=() (case-insensitive); the [^A-Za-z_] guard
    # keeps PYTHONPATH=/MYPATH= out.
    if grep -v '^[[:space:]]*#' "$rc" 2>/dev/null \
         | grep -iE '(^|[^A-Za-z_])path[+]?=|fish_add_path' \
         | grep -qF "$PREFIX"; then
        state=present   # rc already persists it — leave it alone
    # Group the append so the redirection-open error (e.g. a read-only rc, or
    # an unwritable parent dir) is suppressed too: `cmd >> "$rc" 2>/dev/null`
    # leaks the shell's "Permission denied" because the >> open is attempted
    # before 2>/dev/null applies. Wrapping in { ... } 2>/dev/null puts the
    # stderr redirect in scope first.
    elif { printf '\n# Added by the tracebloc CLI installer\n%s\n' "$path_line" >> "$rc"; } 2>/dev/null; then
        state=added
    fi

    echo ""
    case "$state" in
        added)
            echo "Added $PREFIX to your PATH in $rc."
            echo "Open a new terminal — or load it now:  . \"$rc\""
            ;;
        present)
            echo "$PREFIX is already in your PATH config ($rc) — nothing to add."
            echo "If a new terminal can't find it yet, open one — or load it now:  . \"$rc\""
            ;;
        *)
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
