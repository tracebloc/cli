#!/usr/bin/env bash
# =============================================================================
#  format-verify.sh — the properties scripts/format.sh must not lose (cli#549)
#
#  Every case here is a way the gate could report CLEAN while checking nothing,
#  which is the backend#1729 class and the reason format.sh exists. The one that
#  shipped and had to be caught in review (Bugbot High + @LukasWodka on #550) is
#  FORMATTER-FAILS-CHECK-MODE: `run_formatter` used to `exit 2` from inside a
#  command substitution, which ends only the subshell — the caller then read an
#  empty capture, found no drift, and printed "clean" with exit 0.
#
#  HERMETIC AND FAST: the formatters are STUBS on PATH (and via $GO), so nothing
#  here builds a tool or touches the network. That means these cases assert
#  format.sh's own logic — scoping, propagation, fail-closed — not gofmt's
#  behaviour. Real end-to-end formatting is covered by `make fmt-check` itself.
#
#  Usage: bash scripts/tests/format-verify.sh    (make fmt-selftest)
# =============================================================================
set -uo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
FORMAT_SH="${REPO_ROOT}/scripts/format.sh"
[ -f "$FORMAT_SH" ] || { echo "format-verify: no scripts/format.sh — refusing to report pass" >&2; exit 2; }

pass=0
fail=0

ok()   { echo "  ok:   $1"; pass=$((pass + 1)); }
bad()  { echo "  FAIL: $1" >&2; fail=$((fail + 1)); }

# A throwaway git repo with one tracked + one UNTRACKED misformatted .go file.
# $1 = stub behaviour: "clean" | "drift" | "explode"
make_fixture() {
  local behaviour="$1" dir
  dir="$(mktemp -d "${TMPDIR:-/tmp}/fmtverify.XXXXXX")" || exit 2
  mkdir -p "$dir/scripts" "$dir/bin" "$dir/.claude/worktrees/session/internal"
  cp "$FORMAT_SH" "$dir/scripts/format.sh"
  chmod +x "$dir/scripts/format.sh"

  ( cd "$dir" \
    && git init -q . \
    && git config user.email t@example.com \
    && git config user.name t \
    && printf 'package p\nfunc  F(){}\n' > tracked.go \
    && git add tracked.go scripts/format.sh \
    && git commit -qm init ) >/dev/null 2>&1

  # Untracked, and deliberately misformatted: the thing that must never be seen.
  printf 'package p\nfunc  Untracked(){}\n' > "$dir/.claude/worktrees/session/internal/scratch.go"

  # gofmt stub. Records the argv it was handed so scoping is checkable, then
  # behaves as asked. `go` is stubbed too, so the goimports path never runs
  # `go run` — same stub serves both via the trailing file arguments.
  cat > "$dir/bin/gofmt" <<STUB
#!/usr/bin/env bash
for a in "\$@"; do case "\$a" in *.go) echo "\$a" >> "$dir/seen.txt" ;; esac; done
case "$behaviour" in
  clean)   exit 0 ;;
  drift)   for a in "\$@"; do case "\$a" in *.go) echo "\$a" ;; esac; done; exit 0 ;;
  explode) echo "stub: simulated formatter failure" >&2; exit 3 ;;
esac
STUB
  cp "$dir/bin/gofmt" "$dir/bin/go"
  chmod +x "$dir/bin/gofmt" "$dir/bin/go"
  echo "$dir"
}

# Sets CASE_OUT and CASE_RC. Call it DIRECTLY, never as `$(run_case ...)`: a
# function's assignments to globals do not survive a command substitution, which
# is the same subshell trap as the defect under test (it bit this harness once).
CASE_OUT=""
CASE_RC=0
run_case() {  # run_case <dir> <mode>
  local dir="$1" mode="$2"
  CASE_OUT="$(cd "$dir" && PATH="$dir/bin:$PATH" GO="$dir/bin/go" ./scripts/format.sh "$mode" 2>/dev/null)"
  CASE_RC=$?
}

echo "== format.sh properties =="

# 1. THE REGRESSION. A formatter that fails must NEVER produce exit 0 / "clean".
dir="$(make_fixture explode)"
run_case "$dir" --check; rc=$CASE_RC
if [ "$rc" -eq 0 ]; then
  bad "a failing formatter exited 0 (check mode swallowed it — the #550 defect)"
elif printf '%s' "$CASE_OUT" | grep -q "clean"; then
  bad "a failing formatter printed 'clean' (rc=$rc)"
else
  ok "a failing formatter fails check mode (rc=$rc, no 'clean' claimed)"
fi

# ...and in write mode, which was never affected but must stay that way.
run_case "$dir" --write; rc=$CASE_RC
if [ "$rc" -ne 0 ]; then
  ok "a failing formatter fails write mode (rc=$rc)"
else
  bad "a failing formatter exited 0 in write mode"
fi
rm -rf "$dir"

# 2. Scoping: the untracked misformatted file must never reach a formatter.
dir="$(make_fixture clean)"
run_case "$dir" --check; rc=$CASE_RC
if [ "$rc" -ne 0 ]; then
  bad "clean fixture did not pass (rc=$rc)"
elif grep -q "scratch.go" "$dir/seen.txt" 2>/dev/null; then
  bad "an UNTRACKED file was passed to the formatter: $(grep scratch.go "$dir/seen.txt")"
elif ! grep -q "tracked.go" "$dir/seen.txt" 2>/dev/null; then
  bad "the TRACKED file was never passed to the formatter — nothing was checked"
else
  ok "scoped to tracked files (tracked.go seen, untracked scratch.go never)"
fi
rm -rf "$dir"

# 3. Drift on a tracked file is still reported, and exits 1 (not 2).
dir="$(make_fixture drift)"
run_case "$dir" --check; rc=$CASE_RC
if [ "$rc" -eq 1 ] && printf '%s' "$CASE_OUT" | grep -q "tracked.go"; then
  ok "tracked drift is reported and exits 1"
else
  bad "tracked drift not reported as exit 1 (rc=$rc)"
fi
rm -rf "$dir"

# 4. Fail closed: empty tracked-.go list, and outside a work tree.
dir="$(mktemp -d "${TMPDIR:-/tmp}/fmtverify.XXXXXX")"
mkdir -p "$dir/scripts" && cp "$FORMAT_SH" "$dir/scripts/" && chmod +x "$dir/scripts/format.sh"
( cd "$dir" && git init -q . && git config user.email t@example.com && git config user.name t \
  && git commit -q --allow-empty -m init ) >/dev/null 2>&1
printf 'package p\nfunc  F(){}\n' > "$dir/untracked.go"
out="$(cd "$dir" && ./scripts/format.sh --check 2>&1)"; rc=$?
if [ "$rc" -eq 2 ]; then
  ok "empty tracked list fails closed (rc=2)"
else
  bad "empty tracked list did not fail closed (rc=$rc): $out"
fi
rm -rf "$dir/.git"
out="$(cd "$dir" && ./scripts/format.sh --check 2>&1)"; rc=$?
if [ "$rc" -eq 2 ]; then
  ok "outside a git work tree fails closed (rc=2)"
else
  bad "outside a work tree did not fail closed (rc=$rc): $out"
fi
rm -rf "$dir"

# 5. Bad usage is an error, not a silent pass.
out="$("$FORMAT_SH" 2>&1)"; rc=$?
if [ "$rc" -eq 2 ]; then ok "no mode argument exits 2"; else bad "no mode argument exited $rc"; fi
out="$("$FORMAT_SH" --bogus 2>&1)"; rc=$?
if [ "$rc" -eq 2 ]; then ok "an unknown mode exits 2"; else bad "unknown mode exited $rc"; fi

echo
if [ "$fail" -ne 0 ]; then
  echo "format-verify: ${fail} FAILED, ${pass} passed" >&2
  exit 1
fi
echo "format-verify: ${pass} properties hold"
