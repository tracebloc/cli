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

# write_stub <path> <seen-file> <mode>   mode: clean | drift | explode
#
# The two formatters get INDEPENDENT stubs so each propagation path can be pinned
# on its own. A single shared stub that fails both only proves "at least one call
# site still exits", which is or-coverage masquerading as per-formatter coverage
# (@LukasWodka on #550, who mutation-proved it: swallowing ONLY the gofmt call
# site left the suite green, because the goimports one still carried the script to
# a non-zero exit). One call-site shape being wrong is how the original bug
# arrived, so that is the case that has to redden.
write_stub() {
  local path="$1" seen="$2" mode="$3"
  cat > "$path" <<STUB
#!/usr/bin/env bash
# Record the argv, so which files reached a formatter is checkable.
for a in "\$@"; do case "\$a" in *.go) echo "\$a" >> "${seen}" ;; esac; done
case "${mode}" in
  clean)   exit 0 ;;
  drift)   for a in "\$@"; do case "\$a" in *.go) echo "\$a" ;; esac; done; exit 0 ;;
  explode) echo "stub: simulated formatter failure" >&2; exit 3 ;;
esac
STUB
  chmod +x "$path"
}

# A throwaway git repo with one tracked + one UNTRACKED misformatted .go file.
# $1 = behaviour: clean | drift | explode | explode-gofmt | explode-goimports
make_fixture() {
  local behaviour="$1" dir gofmt_mode goimports_mode
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

  case "$behaviour" in
    clean)              gofmt_mode=clean;   goimports_mode=clean   ;;
    drift)              gofmt_mode=drift;   goimports_mode=drift   ;;
    explode)            gofmt_mode=explode; goimports_mode=explode ;;
    explode-gofmt)      gofmt_mode=explode; goimports_mode=clean   ;;
    explode-goimports)  gofmt_mode=clean;   goimports_mode=explode ;;
    *) echo "make_fixture: unknown behaviour '$behaviour'" >&2; exit 2 ;;
  esac

  # `gofmt` is called bare; the goimports path goes through $GO, so stubbing `go`
  # means `go run` never fetches or builds anything.
  write_stub "$dir/bin/gofmt" "$dir/seen.txt" "$gofmt_mode"
  write_stub "$dir/bin/go"    "$dir/seen.txt" "$goimports_mode"
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

# 1b. EACH propagation path, pinned on its own. With both formatters failing, one
# surviving `|| exit 2` is enough to carry the script to non-zero — so the case
# above cannot tell WHICH path works. These can: exactly one formatter fails, so
# only that call site can produce the non-zero exit.
for which in gofmt goimports; do
  dir="$(make_fixture "explode-${which}")"
  run_case "$dir" --check; rc=$CASE_RC
  if [ "$rc" -eq 0 ]; then
    bad "a failing ${which} alone exited 0 — that call site swallows failures"
  elif printf '%s' "$CASE_OUT" | grep -q "clean"; then
    bad "a failing ${which} alone printed 'clean' (rc=$rc)"
  else
    ok "a failing ${which} ALONE fails check mode (rc=$rc)"
  fi
  rm -rf "$dir"
done

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
