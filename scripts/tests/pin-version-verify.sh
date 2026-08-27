#!/usr/bin/env bash
# =============================================================================
#  pin-version-verify.sh — the properties scripts/check-pin-version.sh must not
#  lose (backend#2704).
#
#  check-pin-version.sh is a fail-closed watcher: it must redden on a contract
#  VERSION bump and on every "cannot evaluate" shape, and stay green ONLY when
#  the pinned version equals HEAD's. An inert checker and a working one look
#  identical in a green scheduled log, so this harness mutation-proves the two
#  that matter — DRIFT reddens (exit 1), and each cannot-evaluate reddens
#  (exit 2) — the way backend#2704 asks for.
#
#  HERMETIC AND FAST: every fetch is a `file://` URL into a temp dir, the pin
#  and default branch are injected via env, so nothing here touches the network
#  or needs `gh`. That means these cases assert check-pin-version.sh's own
#  logic — version compare, fail-closed, shape guard — not data-ingestors'
#  behaviour. The live end-to-end check is the scheduled workflow itself.
#
#  Usage: bash scripts/tests/pin-version-verify.sh
# =============================================================================
set -uo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
CHECKER="${REPO_ROOT}/scripts/check-pin-version.sh"
[ -f "$CHECKER" ] || { echo "pin-version-verify: no scripts/check-pin-version.sh — refusing to report pass" >&2; exit 2; }

pass=0
fail=0
ok()  { echo "  ok:   $1"; pass=$((pass + 1)); }
bad() { echo "  FAIL: $1" >&2; fail=$((fail + 1)); }

FIX="$(mktemp -d "${TMPDIR:-/tmp}/pinver.XXXXXX")" || exit 2
trap 'rm -rf "$FIX"' EXIT INT TERM

printf '{"tasks":{},"version":"2"}\n'  > "$FIX/v2.json"
printf '{"tasks":{},"version":"3"}\n'  > "$FIX/v3.json"
printf '{"tasks":{}}\n'                > "$FIX/noversion.json"
printf '<html>not json</html>\n'       > "$FIX/notjson.txt"

# run_case <name> <expected-exit> <extra env assignments...>
# Runs the checker with URL/ref overrides and asserts the exit code. A dummy
# DATA_INGESTORS_REF of valid shape stands in for the pin. EVERY case below also
# sets HEAD_LAYOUT_URL, so the checker never takes the default-branch path — the
# bogus DATA_INGESTORS_DEFAULT_BRANCH only backstops the `gh api` lookup. It is
# NOT a full offline guarantee: dropping a case's HEAD_LAYOUT_URL would curl
# raw.githubusercontent.com with this value, so keep every case's HEAD_LAYOUT_URL
# set. That, not this env var, is what keeps the harness hermetic.
run_case() {
  local name="$1" want="$2"; shift 2
  local out rc
  out="$(env "$@" \
    DATA_INGESTORS_DEFAULT_BRANCH="unused-in-tests" \
    "$CHECKER" 2>&1)"
  rc=$?
  if [ "$rc" -eq "$want" ]; then
    ok "$name (exit $rc)"
  else
    bad "$name — expected exit $want, got $rc"
    echo "$out" | sed 's/^/        /' >&2
  fi
}

echo "check-pin-version.sh — fail-closed contract-version watcher"

# 0 — IN SYNC: pin version == HEAD version.
run_case "in-sync: v2 == v2 -> pass" 0 \
  DATA_INGESTORS_REF="0123456789abcdef0123456789abcdef01234567" \
  PIN_LAYOUT_URL="file://$FIX/v2.json" \
  HEAD_LAYOUT_URL="file://$FIX/v2.json"

# 1 — DRIFT: the mutation-proof. pin v2, HEAD v3 MUST redden (exit 1). If the
#     version comparison is ever made inert, this is the case that fails.
run_case "drift: pin v2 vs HEAD v3 -> exit 1" 1 \
  DATA_INGESTORS_REF="0123456789abcdef0123456789abcdef01234567" \
  PIN_LAYOUT_URL="file://$FIX/v2.json" \
  HEAD_LAYOUT_URL="file://$FIX/v3.json"

# 2 — FAIL CLOSED: HEAD URL unreachable (moved path / dead raw URL).
run_case "fail-closed: unreachable HEAD url -> exit 2" 2 \
  DATA_INGESTORS_REF="0123456789abcdef0123456789abcdef01234567" \
  PIN_LAYOUT_URL="file://$FIX/v2.json" \
  HEAD_LAYOUT_URL="file://$FIX/does-not-exist.json"

# 3 — FAIL CLOSED: contract JSON is missing the top-level "version".
run_case "fail-closed: missing version field -> exit 2" 2 \
  DATA_INGESTORS_REF="0123456789abcdef0123456789abcdef01234567" \
  PIN_LAYOUT_URL="file://$FIX/noversion.json" \
  HEAD_LAYOUT_URL="file://$FIX/v3.json"

# 4 — FAIL CLOSED: response is not JSON.
run_case "fail-closed: non-JSON body -> exit 2" 2 \
  DATA_INGESTORS_REF="0123456789abcdef0123456789abcdef01234567" \
  PIN_LAYOUT_URL="file://$FIX/notjson.txt" \
  HEAD_LAYOUT_URL="file://$FIX/v3.json"

# 5 — FAIL CLOSED: pin file has no ref (comment-only). Must exit 2 before any
#     fetch — URL overrides are set only so nothing touches the network if the
#     resolution order ever regresses.
printf '# just a comment, no ref\n' > "$FIX/empty-ref"
run_case "fail-closed: empty pin (comment-only ref file) -> exit 2" 2 \
  REF_FILE="$FIX/empty-ref" \
  PIN_LAYOUT_URL="file://$FIX/v2.json" \
  HEAD_LAYOUT_URL="file://$FIX/v3.json"

# 6 — FAIL CLOSED: pin ref has a traversal-shaped value ("..").
printf 'main/../../etc\n' > "$FIX/bad-shape-ref"
run_case "fail-closed: malformed pin shape ('..') -> exit 2" 2 \
  REF_FILE="$FIX/bad-shape-ref" \
  PIN_LAYOUT_URL="file://$FIX/v2.json" \
  HEAD_LAYOUT_URL="file://$FIX/v3.json"

echo
echo "pin-version-verify: $pass passed, $fail failed"
[ "$fail" -eq 0 ] || exit 1
