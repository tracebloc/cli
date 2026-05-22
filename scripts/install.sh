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
#   5. (Optional) Verifies cosign signature if cosign is on PATH
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
        x86_64|amd64)        echo "amd64" ;;
        arm64|aarch64)       echo "arm64" ;;
        *)
            echo "Error: unsupported arch: $uname_m" >&2
            echo "tracebloc CLI is released for amd64 + arm64; if you need" >&2
            echo "another arch, please file an issue at github.com/tracebloc/cli." >&2
            exit 1
            ;;
    esac
}

OS="$(detect_os)"
ARCH="$(detect_arch)"

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
    redirect_url="$(curl -fsSI \
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

TAG="$(resolve_tag)"
echo "Installing tracebloc CLI $TAG ($OS/$ARCH)..."

# --------------------------------------------------------------------
# Download binary + SHA256SUMS + (optional) cosign sig/cert.
# --------------------------------------------------------------------
BINARY_FILE="${BINARY_NAME}-${TAG}-${OS}-${ARCH}"
BASE_URL="https://github.com/${GITHUB_REPO}/releases/download/${TAG}"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT INT TERM

echo "Downloading binary..."
if ! curl -fsSL "$BASE_URL/$BINARY_FILE" -o "$TMP/$BINARY_FILE"; then
    echo "Error: failed to download $BASE_URL/$BINARY_FILE" >&2
    exit 1
fi

echo "Downloading SHA256SUMS..."
if ! curl -fsSL "$BASE_URL/SHA256SUMS" -o "$TMP/SHA256SUMS"; then
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
# sha256sum (GNU coreutils) vs shasum -a 256 (macOS): detect which is on PATH.
if command -v sha256sum >/dev/null 2>&1; then
    actual="$(sha256sum "$TMP/$BINARY_FILE" | awk '{print $1}')"
elif command -v shasum >/dev/null 2>&1; then
    actual="$(shasum -a 256 "$TMP/$BINARY_FILE" | awk '{print $1}')"
else
    echo "Warning: no sha256sum / shasum on PATH — skipping checksum verify." >&2
    actual=""
fi
if [ -n "$actual" ] && [ "$actual" != "$expected" ]; then
    echo "Error: SHA256 mismatch!" >&2
    echo "  expected: $expected" >&2
    echo "  actual:   $actual" >&2
    echo "  refusing to install." >&2
    exit 1
fi
echo "  ✓ checksum matches"

# --------------------------------------------------------------------
# Verify cosign signature if cosign is on PATH (optional).
# --------------------------------------------------------------------
if command -v cosign >/dev/null 2>&1; then
    echo "Verifying cosign signature..."
    if ! curl -fsSL "$BASE_URL/$BINARY_FILE.sig" -o "$TMP/$BINARY_FILE.sig"; then
        echo "  ⚠ couldn't download .sig — release may pre-date signing." >&2
    elif ! curl -fsSL "$BASE_URL/$BINARY_FILE.cert" -o "$TMP/$BINARY_FILE.cert"; then
        echo "  ⚠ couldn't download .cert — release may pre-date signing." >&2
    elif ! cosign verify-blob \
            --certificate-identity-regexp \
              "https://github.com/${GITHUB_REPO}/.github/workflows/release.yml@.*" \
            --certificate-oidc-issuer 'https://token.actions.githubusercontent.com' \
            --certificate "$TMP/$BINARY_FILE.cert" \
            --signature "$TMP/$BINARY_FILE.sig" \
            "$TMP/$BINARY_FILE" 2>/dev/null; then
        echo "Error: cosign signature verification FAILED — refusing to install." >&2
        exit 1
    else
        echo "  ✓ cosign signature valid"
    fi
else
    # Not having cosign isn't fatal — the SHA256 check above is the
    # baseline. Recommend cosign for higher-trust installs.
    echo "  (cosign not installed; SHA256 verified, signature skipped)"
fi

# --------------------------------------------------------------------
# Install to a writable prefix.
# --------------------------------------------------------------------
PREFIX="$INSTALL_PREFIX"
if [ ! -w "$(dirname "$PREFIX/$BINARY_NAME" 2>/dev/null || true)" ] && [ ! -w "$PREFIX" ] 2>/dev/null; then
    # /usr/local/bin needs sudo on most distros. Fall back to per-user.
    FALLBACK="$HOME/.local/bin"
    echo "Note: $PREFIX is not writable; falling back to $FALLBACK"
    mkdir -p "$FALLBACK"
    PREFIX="$FALLBACK"
fi

mkdir -p "$PREFIX"
chmod +x "$TMP/$BINARY_FILE"
mv "$TMP/$BINARY_FILE" "$PREFIX/$BINARY_NAME"

echo ""
echo "✓ tracebloc CLI installed: $PREFIX/$BINARY_NAME"
echo ""
echo "Verify with:"
echo "  $PREFIX/$BINARY_NAME version"
echo ""

# PATH guidance for the fallback case.
case ":$PATH:" in
    *":$PREFIX:"*) ;;  # already on PATH
    *)
        echo "Note: $PREFIX is not on \$PATH. Add this to your shell rc file:"
        echo "  export PATH=\"\$PATH:$PREFIX\""
        echo ""
        ;;
esac

echo "First steps:"
echo "  $BINARY_NAME cluster info        # confirm CLI can reach your cluster"
echo "  $BINARY_NAME dataset push --help # see the dominant flow"
