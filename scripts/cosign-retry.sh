#!/usr/bin/env bash
# =============================================================================
#  cosign-retry.sh — run ONE cosign invocation with a bounded, classified retry.
#
#  Why this exists (backend#2379). On 2026-08-23 tag v0.10.10-rc.2 was cut, and
#  one of release.yml's eight matrix legs — darwin/arm64 — died on:
#
#    signing tracebloc-v0.10.10-rc.2-darwin-arm64: getting key from Fulcio:
#    getting CTFE public keys: updating local metadata and targets:
#    error updating to TUF remote mirror: tuf: failed to download 10.root.json:
#    Get "https://tuf-repo-cdn.sigstore.dev/10.root.json": read tcp ...:
#    read: connection reset by peer
#
#  A sigstore CDN reset. The tag is the workflow's TRIGGER, so it already
#  existed; the release object is created by the `publish` job, gated on all
#  eight legs. One flaky leg therefore meant a git tag with no GitHub Release
#  and no assets, for 6h04m, until a manual re-run. fr-assist caught it, not
#  the build.
#
#  The gate is NOT the bug and must not be weakened: a release missing
#  darwin/arm64 is worse than no release. The unretried network call is the bug,
#  and it is retried HERE — one cosign call — not by re-running the job, which
#  would redo seven good legs and widen the window.
#
#  THREE PROPERTIES, each pinned by scripts/tests/cosign-retry-verify.sh:
#
#   1. BOUNDED. At most COSIGN_RETRY_ATTEMPTS attempts (default 3), with a
#      declared backoff (default 5s then 15s) and a per-attempt wall-clock cap
#      (default 120s, enforced in bash so it is verifiable on every host) so a
#      cosign that hangs against a degraded CDN is killed and retried rather
#      than eating the job budget. Worst case with the shipped defaults is
#      3x120s + 5s + 15s = 6m20s, inside release.yml's timeout-minutes: 20 —
#      so the retry can never be the reason a leg is killed.
#
#   2. CLASSIFIED. Only failures whose output matches a known transient
#      sigstore/network signature are retried. A genuine signing refusal — a
#      missing file, a rejected OIDC token, a bad flag — fails on the FIRST
#      attempt. An indiscriminate retry would turn a real signing failure into a
#      slow red (or worse, hide it behind noise); "retry everything" is the
#      failure mode this script is written to avoid.
#
#   3. FAIL CLOSED. cosign exiting 0 is not evidence that it signed anything.
#      Every --output-* file named on the command line must exist and be
#      non-empty afterwards, or this script fails — and does NOT retry, because
#      a zero exit with no artifact is not a network symptom. "Cannot tell" is a
#      finding, never a pass. Those files are removed BEFORE each attempt, so
#      the check reads only what the current attempt wrote — a leftover from an
#      earlier failed attempt can never stand in as a signature (cli#568).
#
#  Usage (arguments are passed to cosign verbatim):
#    scripts/cosign-retry.sh sign-blob \
#      --output-certificate "$BIN.cert" --output-signature "$BIN.sig" "$BIN"
#
#  Exit codes: 0 signed · 1 cosign failed (classified, or budget exhausted)
#              2 usage/config error · 3 cosign exited 0 but produced no artifact
#
#  Env (all optional): COSIGN_BIN COSIGN_RETRY_ATTEMPTS COSIGN_RETRY_DELAYS
#                      COSIGN_RETRY_TIMEOUT
# =============================================================================
set -uo pipefail

COSIGN_BIN="${COSIGN_BIN:-cosign}"
ATTEMPTS="${COSIGN_RETRY_ATTEMPTS:-3}"
DELAYS="${COSIGN_RETRY_DELAYS:-5 15}"
PER_ATTEMPT_TIMEOUT="${COSIGN_RETRY_TIMEOUT:-120}"

die() { printf 'cosign-retry: %s\n' "$1" >&2; exit "${2:-2}"; }

[ "$#" -gt 0 ] || die "no cosign arguments given"

case "$ATTEMPTS" in
  ''|*[!0-9]*) die "COSIGN_RETRY_ATTEMPTS must be a positive integer, got '$ATTEMPTS'" ;;
esac
[ "$ATTEMPTS" -ge 1 ] || die "COSIGN_RETRY_ATTEMPTS must be >= 1, got '$ATTEMPTS'"

case "$PER_ATTEMPT_TIMEOUT" in
  ''|*[!0-9]*) die "COSIGN_RETRY_TIMEOUT must be a positive integer (seconds), got '$PER_ATTEMPT_TIMEOUT'" ;;
esac
[ "$PER_ATTEMPT_TIMEOUT" -ge 1 ] || die "COSIGN_RETRY_TIMEOUT must be >= 1, got '$PER_ATTEMPT_TIMEOUT'"

# The backoff schedule must cover every retry the attempt budget permits.
# Fail closed on a short schedule rather than silently retrying with no wait —
# a zero-delay retry against a resetting CDN is the same request twice.
read -r -a _delays <<< "$DELAYS"
if [ "${#_delays[@]}" -lt "$((ATTEMPTS - 1))" ]; then
  die "COSIGN_RETRY_DELAYS ('$DELAYS') declares ${#_delays[@]} delays but $ATTEMPTS attempts need $((ATTEMPTS - 1))"
fi

# ---------------------------------------------------------------------------
# The transient vocabulary: sigstore/TUF/CDN and transport-level symptoms that
# a second attempt can plausibly clear. Everything NOT matched here is treated
# as a genuine refusal and fails immediately.
#
# Written as ONE list, consulted by ONE function, so the classifier the retry
# loop uses is the same one the harness drives. The harness supplies its own
# independently written failure texts (the real 2026-08-23 message among them)
# rather than iterating this list — a list checked against itself is blind.
# ---------------------------------------------------------------------------
COSIGN_TRANSIENT_PATTERNS='tuf: failed to download
error updating to TUF remote mirror
connection reset by peer
connection refused
broken pipe
i/o timeout
TLS handshake timeout
context deadline exceeded
no such host
temporary failure in name resolution
unexpected EOF
502 Bad Gateway
503 Service Unavailable
504 Gateway Time-?out
429 Too Many Requests
StatusCode: 5[0-9][0-9]'

# is_transient <file> — true when the captured cosign output carries a known
# transient signature. Exported behaviour: the harness calls this via the
# script itself, never a copy.
is_transient() {
  printf '%s\n' "$COSIGN_TRANSIENT_PATTERNS" | grep -q '[^[:space:]]' || return 1
  grep -qiE -f <(printf '%s\n' "$COSIGN_TRANSIENT_PATTERNS") "$1"
}

# The output files cosign was told to write, derived from the ACTUAL argument
# list rather than a second hardcoded naming convention. Any --output-<x> flag
# counts, so a new cosign output flag is covered without editing this script.
outputs_from_args() {
  local prev='' a
  for a in "$@"; do
    # Space form: the token AFTER an --output-* flag is its path.
    case "$prev" in --output-*) printf '%s\n' "$a" ;; esac
    # Equals form: --output-<x>=<path> carries its own path. Clear prev so the
    # NEXT token is not mis-read as this flag's space-form argument — otherwise
    # --output-<x>=<path> still matches --output-* and swallows the arg after it.
    case "$a" in
      --output-*=*) printf '%s\n' "${a#*=}"; prev=''; continue ;;
    esac
    prev="$a"
  done
}

# run_cosign <logfile> <cosign args...> — one attempt, with its own wall-clock
# cap enforced in bash. Deliberately NOT coreutils `timeout`: that is absent on
# macOS, which would leave the cap's behaviour unverifiable for anyone running
# the harness locally, and an untestable branch is how a cap stops being real.
# Returns cosign's exit status, or 124 when the attempt was killed at the cap
# (the same code `timeout` uses, so the caller reads identically).
run_cosign() {
  local log="$1"; shift
  "$COSIGN_BIN" "$@" >"$log" 2>&1 &
  local pid=$! waited=0
  while kill -0 "$pid" 2>/dev/null; do
    if [ "$waited" -ge "$PER_ATTEMPT_TIMEOUT" ]; then
      kill -TERM "$pid" 2>/dev/null
      sleep 1
      kill -KILL "$pid" 2>/dev/null
      wait "$pid" 2>/dev/null
      return 124
    fi
    sleep 1
    waited=$((waited + 1))
  done
  wait "$pid"
}

LOG="$(mktemp)"
# shellcheck disable=SC2317  # reached via the EXIT trap
cleanup() { rm -f "$LOG"; }
trap cleanup EXIT

attempt=1
while : ; do
  printf 'cosign-retry: attempt %d/%d: %s %s\n' "$attempt" "$ATTEMPTS" "$COSIGN_BIN" "$*"

  # Clear every --output-* file BEFORE the attempt runs. Without this a
  # non-empty .cert/.sig left by an EARLIER failed attempt survives, and the
  # fail-closed check below — which only tests [ -s ] — reads that stale
  # artifact as proof that a later `exit 0` (which wrote nothing) signed. That
  # is the harness reporting a signature that does not exist: the one property
  # it exists to prevent. Wiping the slate makes [ -s ] a claim about THIS
  # attempt alone (backend#2379, cli#568).
  while IFS= read -r out; do
    [ -n "$out" ] && rm -f "$out"
  done < <(outputs_from_args "$@")

  run_cosign "$LOG" "$@"
  rc=$?
  cat "$LOG"

  if [ "$rc" -eq 0 ]; then
    # Fail closed: a zero exit is a claim, the artifact is the evidence — and
    # after the pre-attempt wipe above, the evidence is THIS attempt's alone.
    missing=''
    while IFS= read -r out; do
      [ -n "$out" ] || continue
      [ -s "$out" ] || missing="$missing $out"
    done < <(outputs_from_args "$@")
    if [ -n "$missing" ]; then
      printf 'cosign-retry: cosign exited 0 but produced no signature artifact:%s\n' "$missing" >&2
      printf 'cosign-retry: refusing to report a signed binary on an absent signature (not retried — a zero exit with no output is not a network symptom)\n' >&2
      exit 3
    fi
    printf 'cosign-retry: signed on attempt %d/%d\n' "$attempt" "$ATTEMPTS"
    exit 0
  fi

  # Exit 124 is the per-attempt cap killing a hung attempt — a stalled network call by
  # construction, so it is transient without needing to match any text.
  if [ "$rc" -eq 124 ]; then
    printf 'cosign-retry: attempt %d timed out after %ss (treated as transient)\n' "$attempt" "$PER_ATTEMPT_TIMEOUT" >&2
  elif is_transient "$LOG"; then
    printf 'cosign-retry: attempt %d failed (rc=%d) on a known transient sigstore/network signature\n' "$attempt" "$rc" >&2
  else
    printf 'cosign-retry: attempt %d failed (rc=%d) and the output matches no transient sigstore/network signature.\n' "$attempt" "$rc" >&2
    printf 'cosign-retry: this is a genuine signing refusal, not a flake — NOT retrying.\n' >&2
    exit "$rc"
  fi

  if [ "$attempt" -ge "$ATTEMPTS" ]; then
    printf 'cosign-retry: exhausted %d attempts; the transient failure did not clear. Failing so the aggregate gate holds and no partially-signed release is published.\n' "$ATTEMPTS" >&2
    exit "$rc"
  fi

  delay="${_delays[$((attempt - 1))]}"
  printf 'cosign-retry: retrying in %ss\n' "$delay" >&2
  sleep "$delay"
  attempt=$((attempt + 1))
done
