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
# Uninstall:
#   curl -fsSL .../install.sh | sh -s -- --uninstall
#   ...removes the tracebloc binary from the install prefix and strips the
#   marked PATH block (the "# Added by the tracebloc CLI installer" line
#   plus the export/fish_add_path line right after it) that the installer
#   appended to your shell rc. It touches ONLY that two-line block and
#   leaves the rest of the file byte-identical. If the rc is a symlink or
#   read-only (managed dotfiles, chezmoi, Nix home-manager) it advises you
#   what to remove by hand instead of writing through it.
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
DO_UNINSTALL=0

# The exact marker line the installer writes above the PATH line. The
# uninstall path keys off this string, so install and uninstall MUST agree
# on it — keep them in lock-step. (Used by the append block far below too.)
RC_MARKER="# Added by the tracebloc CLI installer"

usage() {
    cat <<EOF
tracebloc CLI installer

Usage:
  install.sh [--version <tag>] [--prefix <dir>] [--help]
  install.sh --uninstall [--prefix <dir>]

Options:
  --version <tag>   Install a specific version (e.g. v0.1.0). Default: latest.
  --prefix <dir>    Install directory. Default: /usr/local/bin (falls back to
                    \$HOME/.local/bin if not writable). On --uninstall, the
                    prefix to remove the binary from (both the default prefix
                    and the \$HOME/.local/bin fallback are checked anyway).
  --uninstall       Remove the tracebloc binary and strip the marked PATH
                    block the installer added to your shell rc. No download.
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
        --uninstall)
            DO_UNINSTALL=1
            shift
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
# Shell-rc routing — shared by the install (append) and --uninstall
# (strip) paths so they always target the same file.
#
# Returns the rc path for the user's $SHELL on stdout, OR exits non-zero
# when $SHELL is unset/unknown/exotic (csh, tcsh, nu, …). A non-zero
# return means "we can't safely guess a single rc" — callers fall back to
# printing advice rather than mutating the wrong file. (#61 silently
# defaulted these to ~/.profile, which a non-login bash never reads — see
# #741.)
# --------------------------------------------------------------------
rc_for_shell() {
    _shell_name="$(basename "${SHELL:-}")"
    case "$_shell_name" in
        zsh)  echo "$HOME/.zshrc" ;;
        bash)
            # macOS Terminal opens a login shell (reads .bash_profile);
            # Linux terminals are interactive non-login (read .bashrc).
            if [ "$OS" = "darwin" ]; then echo "$HOME/.bash_profile"; else echo "$HOME/.bashrc"; fi
            ;;
        fish) echo "$HOME/.config/fish/config.fish" ;;
        # $SHELL unset, or a shell whose rc syntax we don't emit
        # (csh/tcsh use `setenv`, nu uses its own config) — refuse to
        # guess. Caller advises instead.
        *) return 1 ;;
    esac
}

# The PATH line to add for a given shell. fish has its own helper;
# everything else gets a POSIX `export`.
path_line_for_shell() {
    _shell_name="$(basename "${SHELL:-}")"
    if [ "$_shell_name" = "fish" ]; then
        echo "fish_add_path $PREFIX"
    else
        echo "export PATH=\"$PREFIX:\$PATH\""
    fi
}

# True (0) when we must NOT write through to $1: it's a symlink, or it
# exists but isn't writable, or we can't create it in its parent dir.
# Symlinks are the chezmoi / dotfiles-repo / Nix home-manager case —
# appending would mutate the link target, polluting version control. In
# all of these we advise instead of writing (#741).
#
# For the not-yet-existing case we mkdir -p the parent first (preserving
# #61's behavior of creating e.g. ~/.config/fish), and only call it
# "managed" if even that can't be made writable.
rc_is_managed() {
    _rc="$1"
    if [ -L "$_rc" ]; then
        return 0   # symlink → managed; never write through it
    fi
    if [ -e "$_rc" ]; then
        [ -w "$_rc" ] && return 1 || return 0   # exists: writable?
    fi
    # Doesn't exist yet — create the parent dir if needed, then check we
    # can actually write into it.
    _dir="$(dirname "$_rc")"
    mkdir -p "$_dir" 2>/dev/null || true
    [ -d "$_dir" ] && [ -w "$_dir" ] && return 1 || return 0
}

# Print the "add this by hand" advice block. $1 = rc path (for the
# message), $2 = the PATH line to add.
advise_path_manual() {
    _rc="$1"; _line="$2"
    echo ""
    echo "Note: $PREFIX is not on \$PATH and the installer did not modify your"
    echo "shell config (it looks managed — a symlink, read-only, or an unknown"
    echo "shell). Add this line to ${_rc:-your shell rc}, then open a new terminal:"
    echo ""
    echo "  $_line"
    echo ""
}

# Persist $PREFIX onto PATH by appending our marked block to the user's
# shell rc — the install-side counterpart to strip_rc_block. install.ps1
# persists user PATH on Windows (SetEnvironmentVariable, User scope); this
# brings Unix to parity. The old print-only advice (#61's predecessor)
# silently failed on Ubuntu: ~/.profile adds ~/.local/bin only at *login*
# and only if it already existed, but the installer creates it mid-session,
# so a new (non-login) terminal reading ~/.bashrc never picks it up.
#
# Reads $PREFIX. No-ops (with a friendly message) when $PREFIX is already
# on PATH. Never fatal: anything it can't safely do becomes printed advice.
#
# Note: the block starts with a leading '\n' separator, so append→uninstall
# round-trips byte-identically for any rc that ends in a newline (the
# universal real-world case). An rc whose last line lacks a trailing newline
# gains one — POSIX-correct, harmless for an rc, and the only non-identical
# case (asserted in scripts/tests/install.bats).
append_path_to_rc() {
    case ":$PATH:" in
        *":$PREFIX:"*) return 0 ;;  # already on PATH — nothing to do
    esac

    _line="$(path_line_for_shell)"

    # Route to the rc the user's shell actually reads. If $SHELL is unset
    # or exotic (csh, nu, …) rc_for_shell fails — we refuse to guess a
    # wrong file and just advise instead (#741).
    if _rc="$(rc_for_shell)"; then
        if grep -qsF "$PREFIX" "$_rc" 2>/dev/null; then
            # rc already references it — leave it alone (idempotent).
            echo ""
            echo "$PREFIX is already referenced in $_rc; left it as-is."
            echo "Open a new terminal — or load it now:  . \"$_rc\""
            echo ""
        elif rc_is_managed "$_rc"; then
            # Symlink / read-only / managed dotfiles — writing through
            # would mutate a tracked target or fail. Advise instead.
            advise_path_manual "$_rc" "$_line"
        elif printf '\n%s\n%s\n' "$RC_MARKER" "$_line" >> "$_rc" 2>/dev/null; then
            echo ""
            echo "Added $PREFIX to your PATH in $_rc."
            echo "Open a new terminal — or load it now:  . \"$_rc\""
            echo "(Undo any time with:  install.sh --uninstall)"
            echo ""
        else
            # Last-resort: the writability probe passed but the append
            # still failed (race, odd FS). Don't fail the install — advise.
            advise_path_manual "$_rc" "$_line"
        fi
    else
        advise_path_manual "" "$_line"
    fi
}

# --------------------------------------------------------------------
# Uninstall — strip ONLY our marked block from the rc and remove the
# binary. No network. Idempotent: running it on an already-clean system
# is a no-op that still exits 0.
#
# The block the installer wrote is (see append_path_to_rc):
#
#     <blank line>                            ← separator we added
#     # Added by the tracebloc CLI installer  ← RC_MARKER
#     export PATH="<prefix>:$PATH"            (or: fish_add_path <prefix>)
#
# To round-trip the file to byte-identical, we drop all three: the marker,
# the line right after it, AND the single blank separator line immediately
# before it (only if it IS blank — a non-blank preceding line is real
# content and is kept). Removing just the two visible lines would orphan
# the separator newline (caught by the round-trip bats test).
#
# Portable editing only — write a temp file and mv it over (the repo's
# sync-schema.sh convention); no `sed -i`, whose -i semantics differ
# between GNU and BSD/macOS.
# --------------------------------------------------------------------
strip_rc_block() {
    _rc="$1"

    [ -f "$_rc" ] || return 0          # nothing to strip
    grep -qF "$RC_MARKER" "$_rc" 2>/dev/null || return 0   # no block → no-op

    # Managed rc (symlink/read-only): don't write through it — tell the
    # user what to delete. Still "succeeds" (non-fatal).
    if rc_is_managed "$_rc"; then
        echo "Note: $_rc looks managed (symlink or read-only); not editing it."
        echo "Remove this block by hand (the marker line, the line after it,"
        echo "and the blank line just before it):"
        echo ""
        echo "  $RC_MARKER"
        echo ""
        return 0
    fi

    # One-line lookbehind: each line is held and only printed on the NEXT
    # iteration, so when we reach the marker we can still suppress a held
    # blank separator. `skip` drops the PATH line right after the marker.
    # We write to a temp file in the rc's own dir (same filesystem → mv is
    # atomic) and only swap it in on success.
    _dir="$(dirname "$_rc")"
    _tmp="$(mktemp "$_dir/.tracebloc-rc.XXXXXX")" || {
        echo "Warning: couldn't create a temp file next to $_rc; left it untouched." >&2
        return 0
    }
    if awk -v marker="$RC_MARKER" '
        skip == 1 { skip = 0; next }            # PATH line after marker — drop
        $0 == marker {
            # Drop the marker. Discard a held blank separator; flush a
            # held non-blank (real content) so we keep it.
            if (have_held && held != "") print held
            have_held = 0
            skip = 1
            next
        }
        { if (have_held) print held; held = $0; have_held = 1 }  # delayed print
        END { if (have_held) print held }
    ' "$_rc" > "$_tmp" 2>/dev/null && mv "$_tmp" "$_rc" 2>/dev/null; then
        echo "Removed the tracebloc PATH block from $_rc."
    else
        rm -f "$_tmp" 2>/dev/null || true
        echo "Warning: couldn't rewrite $_rc; left it untouched." >&2
    fi
}

uninstall() {
    echo "Uninstalling tracebloc CLI..."

    # 1) Remove the binary from the chosen prefix AND the fallback dir,
    #    so an uninstall cleans up regardless of which path install took.
    _removed_bin=0
    for _dir in "$INSTALL_PREFIX" "$HOME/.local/bin"; do
        _bin="$_dir/$BINARY_NAME"
        if [ -f "$_bin" ] || [ -L "$_bin" ]; then
            if rm -f "$_bin" 2>/dev/null; then
                echo "Removed $_bin"
                _removed_bin=1
            else
                echo "Note: couldn't remove $_bin (permission?). Remove it manually:" >&2
                echo "  sudo rm -f \"$_bin\"" >&2
            fi
        fi
    done
    [ "$_removed_bin" = "0" ] && echo "No tracebloc binary found in $INSTALL_PREFIX or $HOME/.local/bin."

    # 2) Strip our marked PATH block. We scan EVERY rc the installer could
    #    plausibly have written to — not just the one $SHELL/$OS routes to
    #    right now. The user may have installed under a different shell, or
    #    changed shells since, so the block can live in a file other than
    #    today's route. strip_rc_block no-ops on any file lacking the
    #    marker, so over-scanning is safe and idempotent.
    _stripped_any=0
    for _cand in \
        "$HOME/.zshrc" \
        "$HOME/.bashrc" \
        "$HOME/.bash_profile" \
        "$HOME/.config/fish/config.fish" \
        "$HOME/.profile"
    do
        if [ -f "$_cand" ] && grep -qF "$RC_MARKER" "$_cand" 2>/dev/null; then
            strip_rc_block "$_cand"
            _stripped_any=1
        fi
    done
    if [ "$_stripped_any" = "0" ]; then
        echo "No tracebloc PATH block found in your shell rc files."
        # $SHELL unset/exotic means we never managed an rc — say so so the
        # user knows to check any hand-added line themselves.
        if ! rc_for_shell >/dev/null 2>&1; then
            echo "(Shell '${SHELL:-unset}' has no rc the installer manages; remove any"
            echo " tracebloc PATH line you added by hand.)"
        fi
    fi

    echo ""
    echo "✓ tracebloc CLI uninstalled."
    echo "  Open a new terminal so the PATH change takes effect."
}

# Test hook: when sourced with TRACEBLOC_INSTALL_SH_SOURCE_ONLY=1, every
# function above is now defined — return before any download / install /
# uninstall side effect so the bats suite can exercise them in isolation.
# (POSIX sh has no $BASH_SOURCE, so an explicit env gate is the portable
# way to make this script sourceable for testing.)
if [ "${TRACEBLOC_INSTALL_SH_SOURCE_ONLY:-0}" = "1" ]; then
    # Intended to be *sourced* by the bats suite, where `return` is valid.
    # (The suite unsets this var before executing the script for real, so
    # this branch is never hit in a normal run.)
    return 0
fi

if [ "$DO_UNINSTALL" = "1" ]; then
    uninstall
    exit 0
fi

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
