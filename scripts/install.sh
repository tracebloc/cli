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
# If neither is available, refuse to install — running an unverified
# binary from the internet is exactly what this script exists to
# prevent. Bugbot PR #11 caught the previous "warn + continue + still
# print ✓ matches" branch as both a security regression AND a
# misleading-log issue. Almost every modern Linux distro ships coreutils
# (sha256sum) by default; macOS ships /usr/bin/shasum as part of the
# base Perl install. A host with neither is unusual enough that
# erroring out is the right call — the customer can install coreutils
# / xcode-select / similar and re-run.
if command -v sha256sum >/dev/null 2>&1; then
    actual="$(sha256sum "$TMP/$BINARY_FILE" | awk '{print $1}')"
elif command -v shasum >/dev/null 2>&1; then
    actual="$(shasum -a 256 "$TMP/$BINARY_FILE" | awk '{print $1}')"
else
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

echo ""
echo "✓ tracebloc CLI installed: $PREFIX/$BINARY_NAME"
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
echo "  $BINARY_NAME cluster info        # confirm CLI can reach your cluster"
echo "  $BINARY_NAME dataset push --help # see the dominant flow"
