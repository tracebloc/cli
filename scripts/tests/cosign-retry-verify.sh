#!/usr/bin/env bash
# =============================================================================
#  cosign-retry-verify.sh — pin the three properties of scripts/cosign-retry.sh
#  (backend#2379): BOUNDED, CLASSIFIED, FAIL-CLOSED.
#
#  Driven the same way install-verify.sh drives install.sh: the REAL script is
#  executed as a subprocess with `cosign` replaced by a PATH shim, so every
#  assertion goes through the production classifier and the production retry
#  loop. No network, no real cosign, no copy of the rule.
#
#  The failure texts below are written HERE, independently of the pattern list
#  in cosign-retry.sh — the first is the verbatim message from the 2026-08-23
#  darwin/arm64 leg. A harness that iterated the script's own pattern list would
#  agree with itself and detect nothing.
#
#  Each case asserts the exit status AND the number of cosign invocations, so a
#  test cannot pass by failing for a different reason than its name claims.
# =============================================================================
# pipefail so a failing producer in a pipeline is not masked. Deliberately NO
# -e: this harness counts its own pass/fail and must survive a failed assertion.
set -uo pipefail

SELF_DIR="$(cd "$(dirname "$0")" && pwd)"
UNDER_TEST="$SELF_DIR/../cosign-retry.sh"

[ -x "$UNDER_TEST" ] || { printf 'cosign-retry-verify: %s missing or not executable — refusing to report clean\n' "$UNDER_TEST" >&2; exit 2; }

PASS=0
FAIL=0
ok()  { printf '  ok   %s\n' "$1"; PASS=$((PASS+1)); }
bad() { printf '  FAIL %s\n' "$1"; FAIL=$((FAIL+1)); }

# The verbatim sigstore TUF CDN reset from run 32624602531 (the incident).
TRANSIENT_TUF_RESET='Error: signing tracebloc-v0.10.10-rc.2-darwin-arm64: getting key from Fulcio: getting CTFE public keys: updating local metadata and targets: error updating to TUF remote mirror: tuf: failed to download 10.root.json: Get "https://tuf-repo-cdn.sigstore.dev/10.root.json": read tcp 10.1.0.138:59750->34.117.62.14:443: read: connection reset by peer'
# A second, differently-shaped transient: Rekor upload stalling mid-request.
TRANSIENT_REKOR_TIMEOUT='Error: signing blob: uploading to rekor: Post "https://rekor.sigstore.dev/api/v1/log/entries": dial tcp 34.120.11.7:443: i/o timeout'
# A GENUINE refusal: the OIDC token the workflow presented was rejected. No
# number of retries fixes a permissions problem, and retrying it would convert
# a clear red into a slow one.
GENUINE_OIDC_REFUSAL='Error: getting signer: getting key from Fulcio: retrieving cert: POST https://fulcio.sigstore.dev/api/v2/signingCert: 401 Unauthorized: invalid identity token'
# A GENUINE refusal: cosign was pointed at a file that is not there.
GENUINE_MISSING_FILE='Error: open tracebloc-v9.9.9-linux-amd64: no such file or directory'

# ---------------------------------------------------------------------------
# sandbox: a temp dir with a fake `cosign` on PATH and a call counter.
#   $1 = shell body for the fake cosign; it may read $ATTEMPT (1-based) and
#        write to $CERT / $SIG. Every invocation appends to $COUNTER first.
# ---------------------------------------------------------------------------
make_sandbox() {
  SBX="$(mktemp -d)"
  BIN="$SBX/bin"; mkdir -p "$BIN"
  COUNTER="$SBX/calls"; : > "$COUNTER"
  CERT="$SBX/blob.cert"; SIG="$SBX/blob.sig"
  printf 'binary-bytes\n' > "$SBX/blob"
  {
    printf '#!/usr/bin/env bash\n'
    printf 'echo call >> "%s"\n' "$COUNTER"
    printf 'ATTEMPT=$(wc -l < "%s" | tr -d " ")\n' "$COUNTER"
    printf 'CERT="%s"; SIG="%s"\n' "$CERT" "$SIG"
    printf '%s\n' "$1"
  } > "$BIN/cosign"
  chmod +x "$BIN/cosign"
}

# run_under_test — invoke the real script against the sandbox. Extra env is
# taken from the caller's ATTEMPTS/DELAYS/TMO vars (each optional).
run_under_test() {
  OUT="$SBX/out"
  PATH="$BIN:$PATH" \
  COSIGN_RETRY_ATTEMPTS="${ATTEMPTS:-3}" \
  COSIGN_RETRY_DELAYS="${DELAYS:-0 0}" \
  COSIGN_RETRY_TIMEOUT="${TMO:-120}" \
    bash "$UNDER_TEST" sign-blob \
      --output-certificate "$CERT" --output-signature "$SIG" "$SBX/blob" \
      > "$OUT" 2>&1
  RC=$?
  CALLS=$(wc -l < "$COUNTER" | tr -d ' ')
}

check() { # check <name> <expected_rc> <expected_calls> [needle]
  local name="$1" want_rc="$2" want_calls="$3" needle="${4:-}"
  local why=''
  [ "$RC" = "$want_rc" ]       || why="$why rc=$RC(want $want_rc)"
  [ "$CALLS" = "$want_calls" ] || why="$why calls=$CALLS(want $want_calls)"
  if [ -n "$needle" ] && ! grep -qF "$needle" "$OUT"; then
    why="$why missing-output:'$needle'"
  fi
  if [ -z "$why" ]; then ok "$name"; else bad "$name —$why"; fi
  [ -z "$why" ] || { printf '       ---- captured output ----\n'; sed 's/^/       /' "$OUT"; }
}

sign_ok='printf "signed\n"; printf "CERTDATA\n" > "$CERT"; printf "SIGDATA\n" > "$SIG"; exit 0'

echo "== BOUNDED / CLASSIFIED / FAIL-CLOSED: scripts/cosign-retry.sh"

# 1. The incident itself: one transient CDN reset, then success. This is the
#    case that had to become a non-event.
make_sandbox "if [ \"\$ATTEMPT\" = 1 ]; then printf '%s\n' '$TRANSIENT_TUF_RESET' >&2; exit 1; fi; $sign_ok"
ATTEMPTS=3 DELAYS='0 0' run_under_test
check "the 2026-08-23 TUF CDN reset is retried and succeeds on attempt 2" 0 2 "signed on attempt 2/3"

# 2. A differently-shaped transient (Rekor i/o timeout) must also be retried —
#    the vocabulary is not one string wide.
make_sandbox "if [ \"\$ATTEMPT\" = 1 ]; then printf '%s\n' '$TRANSIENT_REKOR_TIMEOUT' >&2; exit 1; fi; $sign_ok"
ATTEMPTS=3 DELAYS='0 0' run_under_test
check "a Rekor i/o timeout is retried and succeeds on attempt 2" 0 2 "signed on attempt 2/3"

# 3. CLASSIFIED: a rejected OIDC token is a real refusal. Exactly ONE call.
make_sandbox "printf '%s\n' '$GENUINE_OIDC_REFUSAL' >&2; exit 1"
ATTEMPTS=3 DELAYS='0 0' run_under_test
check "a 401 from Fulcio is NOT retried (genuine refusal)" 1 1 "NOT retrying"

# 4. CLASSIFIED: a missing input file is a real refusal. Exactly ONE call.
make_sandbox "printf '%s\n' '$GENUINE_MISSING_FILE' >&2; exit 1"
ATTEMPTS=3 DELAYS='0 0' run_under_test
check "a missing blob is NOT retried (genuine refusal)" 1 1 "NOT retrying"

# 5. BOUNDED: a transient that never clears must stop at the budget and FAIL,
#    so the aggregate gate holds. Never an unbounded loop, never a slow green.
make_sandbox "printf '%s\n' '$TRANSIENT_TUF_RESET' >&2; exit 1"
ATTEMPTS=3 DELAYS='0 0' run_under_test
check "a transient that never clears fails after exactly 3 attempts" 1 3 "exhausted 3 attempts"

# 6. BOUNDED: the budget is the declared one, not a constant.
make_sandbox "printf '%s\n' '$TRANSIENT_TUF_RESET' >&2; exit 1"
ATTEMPTS=2 DELAYS='0' run_under_test
check "COSIGN_RETRY_ATTEMPTS=2 means exactly 2 attempts" 1 2 "exhausted 2 attempts"

# 7. FAIL CLOSED: cosign exits 0 and writes nothing. A zero exit is a claim;
#    the artifact is the evidence. Must fail, and must NOT retry.
make_sandbox 'printf "signed\n"; exit 0'
ATTEMPTS=3 DELAYS='0 0' run_under_test
check "cosign exit 0 with no artifacts fails closed, unretried" 3 1 "produced no signature artifact"

# 8. FAIL CLOSED: a half-written signature (cert present, sig empty) is not a
#    signed binary either.
make_sandbox 'printf "signed\n"; printf "CERTDATA\n" > "$CERT"; : > "$SIG"; exit 0'
ATTEMPTS=3 DELAYS='0 0' run_under_test
check "an empty .sig alongside a good .cert fails closed" 3 1 "produced no signature artifact"

# 9. BOUNDED in wall-clock too: a hung attempt is killed and retried, so a
#    stalled CDN connection cannot consume the whole job budget.
make_sandbox "if [ \"\$ATTEMPT\" = 1 ]; then sleep 30; fi; $sign_ok"
ATTEMPTS=3 DELAYS='0 0' TMO=1 run_under_test
check "a hung attempt is killed at the per-attempt cap and retried" 0 2 "timed out after 1s"

# 10. The backoff schedule is indexed, not a constant: the first retry waits
#     the FIRST declared delay.
make_sandbox "if [ \"\$ATTEMPT\" = 1 ]; then printf '%s\n' '$TRANSIENT_TUF_RESET' >&2; exit 1; fi; $sign_ok"
ATTEMPTS=3 DELAYS='1 0' run_under_test
check "the first retry waits the first declared delay" 0 2 "retrying in 1s"

# 11. Config fails closed: a backoff schedule too short for the attempt budget
#     is refused BEFORE cosign is called, not silently retried with no wait.
make_sandbox "$sign_ok"
ATTEMPTS=3 DELAYS='5' run_under_test
check "a backoff schedule shorter than the budget is refused (0 calls)" 2 0 "declares 1 delays but 3 attempts need 2"

# 12. Config fails closed: a non-numeric budget is refused before any call.
make_sandbox "$sign_ok"
ATTEMPTS='lots' DELAYS='0 0' run_under_test
check "a non-numeric attempt budget is refused (0 calls)" 2 0 "must be a positive integer"

# 13. Config fails closed: a non-numeric per-attempt cap is refused before any
#     call, so the cap can never be silently absent.
make_sandbox "$sign_ok"
ATTEMPTS=3 DELAYS='0 0' TMO='soon' run_under_test
check "a non-numeric per-attempt cap is refused (0 calls)" 2 0 "COSIGN_RETRY_TIMEOUT must be a positive integer"

# 14. THE SHIPPED DEFAULTS, with no env overrides at all. Every case above sets
#     COSIGN_RETRY_ATTEMPTS explicitly, so none of them would notice the default
#     budget being changed to 1 — and the default is what release.yml runs.
#     This case deliberately pays the real first backoff (5s) to assert that an
#     unconfigured invocation retries the incident's failure and recovers.
make_sandbox "if [ \"\$ATTEMPT\" = 1 ]; then printf '%s\n' '$TRANSIENT_TUF_RESET' >&2; exit 1; fi; $sign_ok"
OUT="$SBX/out"
PATH="$BIN:$PATH" bash "$UNDER_TEST" sign-blob \
  --output-certificate "$CERT" --output-signature "$SIG" "$SBX/blob" > "$OUT" 2>&1
RC=$?
CALLS=$(wc -l < "$COUNTER" | tr -d ' ')
check "the shipped defaults retry the incident and succeed (3 attempts, 5s first backoff)" 0 2 "signed on attempt 2/3"
grep -qF 'retrying in 5s' "$OUT" \
  && ok "the shipped default backoff is 5s on the first retry" \
  || bad "the shipped default backoff is 5s on the first retry"

printf '\n%d passed, %d failed\n' "$PASS" "$FAIL"
[ "$FAIL" -eq 0 ] || exit 1
