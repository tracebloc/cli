#!/usr/bin/env bats
#
# Tests for scripts/install.sh — the rc-file mutation introduced by cli#61
# (PATH-append on the ~/.local/bin fallback) and the hygiene added by #741
# (uninstall + managed-rc safety).
#
# These tests source install.sh with the TRACEBLOC_INSTALL_SH_SOURCE_ONLY
# gate so the pure shell-rc functions can be exercised without any network
# download. The end-to-end "--uninstall" tests run the real script (the
# uninstall path takes no network).
#
# Run locally:  bats scripts/tests/install.bats
# Requires:     bats-core (brew install bats-core / apt install bats).
#
# NOTE: there is currently no CI step that runs this file — build.yml only
# exercises the Go code. A shell-lint + bats job is proposed in the PR.

SCRIPT="${BATS_TEST_DIRNAME}/../install.sh"

setup() {
    # Isolated fake HOME per test so rc files never touch the real one.
    TEST_HOME="$(mktemp -d)"
    export HOME="$TEST_HOME"

    # Source the functions only — the gate returns before any download /
    # install / uninstall side effect. Clear positional params first: a
    # sourced script inherits the caller's $@, which install.sh's arg
    # parser would otherwise try to consume.
    set --
    export TRACEBLOC_INSTALL_SH_SOURCE_ONLY=1
    export SHELL="/bin/bash"
    # shellcheck disable=SC1090
    . "$SCRIPT"
    # detect_os ran during sourcing and set OS to the host. Pin it to a
    # deterministic value so bash routes to ~/.bashrc on every CI host
    # (Linux runner or macOS dev box alike).
    OS="linux"

    # The fallback dir the installer uses; not on PATH inside the test, so
    # append_path_to_rc actually fires.
    PREFIX="$HOME/.local/bin"
}

teardown() {
    [ -n "${TEST_HOME:-}" ] && rm -rf "$TEST_HOME"
}

# --- helpers ---------------------------------------------------------

# Path to the bash rc for our pinned OS/SHELL.
rc_path() { echo "$HOME/.bashrc"; }

count_markers() {
    grep -cF "$RC_MARKER" "$1" 2>/dev/null || true
}

# =====================================================================
# Round-trip: append then uninstall restores the file byte-identical.
# =====================================================================

@test "append -> strip round-trips an rc with trailing newline (byte-identical)" {
    rc="$(rc_path)"
    printf '# my rc\nalias ll="ls -la"\nexport EDITOR=vim\n' > "$rc"
    cp "$rc" "$HOME/orig"

    append_path_to_rc
    [ "$(count_markers "$rc")" -eq 1 ]

    strip_rc_block "$rc"
    [ "$(count_markers "$rc")" -eq 0 ]

    # The whole point of #741: the file comes back exactly as it was.
    run diff "$HOME/orig" "$rc"
    [ "$status" -eq 0 ]
}

@test "round-trip is byte-identical when the rc already ends in a blank line" {
    rc="$(rc_path)"
    printf '# rc\nexport EDITOR=vim\n\n' > "$rc"
    cp "$rc" "$HOME/orig"

    append_path_to_rc
    strip_rc_block "$rc"

    run diff "$HOME/orig" "$rc"
    [ "$status" -eq 0 ]
}

@test "round-trip is byte-identical for an empty rc" {
    rc="$(rc_path)"
    : > "$rc"
    cp "$rc" "$HOME/orig"

    append_path_to_rc
    strip_rc_block "$rc"

    run diff "$HOME/orig" "$rc"
    [ "$status" -eq 0 ]
}

@test "an rc with no trailing newline gains one (only non-identical case, documented)" {
    rc="$(rc_path)"
    printf 'export FOO=bar' > "$rc"   # deliberately NO trailing newline

    append_path_to_rc
    strip_rc_block "$rc"

    # Content is preserved; the file is now newline-terminated (POSIX-correct).
    run cat "$rc"
    [ "$output" = "export FOO=bar" ]
    # exactly one line, properly terminated
    [ "$(wc -l < "$rc" | tr -d ' ')" = "1" ]
}

# =====================================================================
# Idempotency.
# =====================================================================

@test "append twice produces exactly one block" {
    rc="$(rc_path)"
    printf '# rc\n' > "$rc"

    append_path_to_rc
    append_path_to_rc

    [ "$(count_markers "$rc")" -eq 1 ]
    # ...and exactly one PATH line for our prefix.
    run grep -cF "$PREFIX" "$rc"
    [ "$output" -eq 1 ]
}

@test "strip twice is a clean no-op the second time (no error, exit 0)" {
    rc="$(rc_path)"
    printf '# rc\nexport EDITOR=vim\n' > "$rc"
    append_path_to_rc

    run strip_rc_block "$rc"
    [ "$status" -eq 0 ]

    run strip_rc_block "$rc"
    [ "$status" -eq 0 ]
    [ "$(count_markers "$rc")" -eq 0 ]
}

@test "strip on a never-touched rc is a no-op and leaves it identical" {
    rc="$(rc_path)"
    printf '# pristine\nexport X=1\n' > "$rc"
    cp "$rc" "$HOME/orig"

    run strip_rc_block "$rc"
    [ "$status" -eq 0 ]
    run diff "$HOME/orig" "$rc"
    [ "$status" -eq 0 ]
}

# =====================================================================
# Read-only rc -> advise, never write, never fail.
# =====================================================================

@test "read-only rc: append advises and does NOT modify the file" {
    rc="$(rc_path)"
    printf '# read only\n' > "$rc"
    cp "$rc" "$HOME/orig"
    chmod 0444 "$rc"

    run append_path_to_rc
    [ "$status" -eq 0 ]                       # non-fatal
    [[ "$output" == *"did not modify your"* ]]
    [[ "$output" == *"$PREFIX"* ]]           # prints the exact line to add

    chmod 0644 "$rc"
    run diff "$HOME/orig" "$rc"
    [ "$status" -eq 0 ]                       # untouched
}

# =====================================================================
# Symlinked rc (chezmoi / dotfiles repo / Nix home-manager) -> advise,
# never write THROUGH the link.
# =====================================================================

@test "symlinked rc: append advises and does NOT write through the symlink" {
    real="$HOME/dotfiles_bashrc"
    printf '# managed by dotfiles repo\nexport KEEP=1\n' > "$real"
    cp "$real" "$HOME/orig"
    rc="$(rc_path)"
    ln -s "$real" "$rc"

    run append_path_to_rc
    [ "$status" -eq 0 ]
    [[ "$output" == *"did not modify your"* ]]

    # link target untouched, and it's still a symlink (not clobbered).
    run diff "$HOME/orig" "$real"
    [ "$status" -eq 0 ]
    [ -L "$rc" ]
}

@test "symlinked rc containing our block: strip advises, does NOT edit the target" {
    real="$HOME/dotfiles_bashrc"
    printf '# managed\n\n%s\nexport PATH="/x:$PATH"\n' "$RC_MARKER" > "$real"
    cp "$real" "$HOME/orig"
    rc="$(rc_path)"
    ln -s "$real" "$rc"

    run strip_rc_block "$rc"
    [ "$status" -eq 0 ]
    [[ "$output" == *"looks managed"* ]]

    run diff "$HOME/orig" "$real"
    [ "$status" -eq 0 ]
    [ -L "$rc" ]
}

# =====================================================================
# Unset / exotic $SHELL -> advise generically rather than guess a wrong rc.
# =====================================================================

@test "unset \$SHELL: append falls back to generic advice (no wrong-file write)" {
    unset SHELL
    run append_path_to_rc
    [ "$status" -eq 0 ]
    [[ "$output" == *"your shell rc"* ]]
    # It must not have created ~/.profile behind our back.
    [ ! -e "$HOME/.profile" ]
}

@test "exotic \$SHELL (csh): append advises rather than emitting export syntax" {
    SHELL="/usr/bin/csh"
    run append_path_to_rc
    [ "$status" -eq 0 ]
    [[ "$output" == *"your shell rc"* ]]
    [ ! -e "$HOME/.profile" ]
}

@test "rc_for_shell returns non-zero for unset and exotic shells" {
    ( unset SHELL; run rc_for_shell; [ "$status" -ne 0 ] )
    ( SHELL=/usr/bin/csh; run rc_for_shell; [ "$status" -ne 0 ] )
    ( SHELL=/usr/bin/nu;  run rc_for_shell; [ "$status" -ne 0 ] )
}

@test "rc_for_shell routes the known shells correctly" {
    SHELL=/bin/zsh  OS=linux  run rc_for_shell
    [ "$output" = "$HOME/.zshrc" ]

    SHELL=/bin/bash OS=linux  run rc_for_shell
    [ "$output" = "$HOME/.bashrc" ]

    SHELL=/bin/bash OS=darwin run rc_for_shell
    [ "$output" = "$HOME/.bash_profile" ]

    SHELL=/usr/bin/fish OS=linux run rc_for_shell
    [ "$output" = "$HOME/.config/fish/config.fish" ]
}

# =====================================================================
# End-to-end: real `install.sh --uninstall` (no network) removes both the
# binary and the rc block, and is idempotent.
# =====================================================================

@test "--uninstall removes the binary and strips the rc block (end to end)" {
    # Simulate a prior install: binary in the fallback dir + appended block.
    mkdir -p "$HOME/.local/bin"
    printf '#!/bin/sh\necho hi\n' > "$HOME/.local/bin/tracebloc"
    chmod +x "$HOME/.local/bin/tracebloc"

    rc="$(rc_path)"
    printf '# rc\nexport EDITOR=vim\n\n%s\nexport PATH="%s/.local/bin:$PATH"\n' \
        "$RC_MARKER" "$HOME" > "$rc"

    # Run the REAL script (executed, not sourced) — unset the source-only
    # test hook for this subprocess so it runs the actual uninstall path.
    run env -u TRACEBLOC_INSTALL_SH_SOURCE_ONLY HOME="$HOME" SHELL=/bin/bash \
        sh "$SCRIPT" --uninstall --prefix "$HOME/.local/bin"
    [ "$status" -eq 0 ]

    [ ! -e "$HOME/.local/bin/tracebloc" ]
    [ "$(count_markers "$rc")" -eq 0 ]

    printf '# rc\nexport EDITOR=vim\n' > "$HOME/expected"
    run diff "$HOME/expected" "$rc"
    [ "$status" -eq 0 ]
}

@test "--uninstall on a clean system is a harmless no-op (exit 0)" {
    run env -u TRACEBLOC_INSTALL_SH_SOURCE_ONLY HOME="$HOME" SHELL=/bin/bash \
        sh "$SCRIPT" --uninstall --prefix "$HOME/.local/bin"
    [ "$status" -eq 0 ]
    [[ "$output" == *"No tracebloc binary found"* ]]
}

@test "--uninstall finds the block even when it lives in a non-routed rc" {
    # Block in ~/.zshrc but uninstall runs under bash — the multi-rc scan
    # must still find and remove it.
    printf '# zrc\n\n%s\nexport PATH="%s/.local/bin:$PATH"\n' \
        "$RC_MARKER" "$HOME" > "$HOME/.zshrc"
    cp "$HOME/.zshrc" "$HOME/before"

    run env -u TRACEBLOC_INSTALL_SH_SOURCE_ONLY HOME="$HOME" SHELL=/bin/bash \
        sh "$SCRIPT" --uninstall --prefix "$HOME/.local/bin"
    [ "$status" -eq 0 ]
    [ "$(count_markers "$HOME/.zshrc")" -eq 0 ]

    printf '# zrc\n' > "$HOME/expected"
    run diff "$HOME/expected" "$HOME/.zshrc"
    [ "$status" -eq 0 ]
}
