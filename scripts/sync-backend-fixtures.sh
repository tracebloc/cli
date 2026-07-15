#!/usr/bin/env bash
# Sync the CLI's vendored backend response-shape fixtures from
# tracebloc/backend (metaApi/tests/contracts/cli/*.json) into
# internal/api/testdata/.
#
# The fixtures are REAL serialized responses for the nine endpoints
# internal/api/client.go decodes, plus the load-bearing error bodies (426
# min_version, RFC 8628 device-flow error enums, 403, 409 conflicts). They are
# generated and shape-asserted by backend CI
# (metaApi/tests/test_cli_response_contracts.py); the Go contract test in
# internal/api replays the vendored copies through the CLI's real decode
# paths, so a renamed backend field can't silently decode to a zero value
# (backend#1106 WS-D.2, cli#291).
#
# Run this script when bumping the backend pin. CI invokes it in check-mode
# (`--check`) to fail builds whose vendored copies have drifted from the
# pinned upstream.
#
# Usage:
#   scripts/sync-backend-fixtures.sh            # write into internal/api/testdata/
#   scripts/sync-backend-fixtures.sh --check    # verify in-tree copies match upstream; exit non-zero on drift
#
# Env knobs:
#   BACKEND_REF               override the backend ref (default: the pinned
#                             SHA in scripts/.backend-ref, else develop)
#   BACKEND_CONTRACTS_TOKEN   token used to read tracebloc/backend (PRIVATE
#                             repo). Falls back to GH_TOKEN, then GITHUB_TOKEN,
#                             then `gh auth token` — note a workflow's default
#                             GITHUB_TOKEN is repo-scoped to the CLI and canNOT
#                             read backend; CI needs a real cross-repo token.
#
# The ref is PINNED (scripts/.backend-ref), not a floating branch, so an
# unrelated backend commit doesn't red every open CLI PR — adopting upstream
# is a deliberate SHA bump + re-sync (backend#1009 pattern, mirrors
# scripts/sync-schema.sh).

set -euo pipefail
cd "$(dirname "$0")/.."

# The pinned backend ref: first non-comment, non-blank line of the ref file
# (a full commit SHA), overridable via BACKEND_REF, falling back to develop
# if the file is somehow absent.
REF_FILE="scripts/.backend-ref"
readonly REF_FILE
_pinned_ref="$(grep -vE '^[[:space:]]*(#|$)' "$REF_FILE" 2>/dev/null | head -1 | tr -d '[:space:]' || true)"
BACKEND_REF="${BACKEND_REF:-${_pinned_ref:-develop}}"

# The ref is interpolated into an API URL, so validate it before use (same
# guard as sync-schema.sh): allow only a SHA / branch / tag shape — alnum
# start, then alnum . _ - / and no ".." component.
if ! printf '%s' "$BACKEND_REF" | grep -qE '^[A-Za-z0-9][A-Za-z0-9._/-]*$' \
   || printf '%s' "$BACKEND_REF" | grep -q '\.\.'; then
  echo "error: invalid backend ref '$BACKEND_REF' — expected a commit SHA, branch, or tag" >&2
  echo "(set it in scripts/.backend-ref or via BACKEND_REF)" >&2
  exit 2
fi

# tracebloc/backend is PRIVATE — fetch via the authenticated contents API
# (Accept: raw), not raw.githubusercontent.com.
TOKEN="${BACKEND_CONTRACTS_TOKEN:-${GH_TOKEN:-${GITHUB_TOKEN:-}}}"
if [[ -z "$TOKEN" ]] && command -v gh >/dev/null 2>&1; then
  TOKEN="$(gh auth token 2>/dev/null || true)"
fi
if [[ -z "$TOKEN" ]]; then
  echo "error: no token to read tracebloc/backend (a private repo)." >&2
  echo "Set BACKEND_CONTRACTS_TOKEN (or GH_TOKEN/GITHUB_TOKEN), or sign in with \`gh auth login\`." >&2
  exit 2
fi

readonly UPSTREAM_DIR="metaApi/tests/contracts/cli"
readonly API_BASE="https://api.github.com/repos/tracebloc/backend/contents/${UPSTREAM_DIR}"
readonly OUT_DIR="internal/api/testdata"

# The full fixture manifest — keep in lock-step with the backend test module
# (metaApi/tests/test_cli_response_contracts.py) AND internal/api's contract
# test, which fails if a file here is missing or an in-tree fixture is not
# covered by an assertion.
FIXTURES=(
  auth_revoke.json
  device_code.json
  device_token_error_access_denied.json
  device_token_error_authorization_pending.json
  device_token_error_expired_token.json
  device_token_error_slow_down.json
  device_token_success.json
  edge_device_admins.json
  edge_device_adopt.json
  edge_device_create.json
  edge_device_list.json
  edge_device_patch_cluster_id.json
  edge_device_revoke.json
  error_403_client_write.json
  error_409_cluster_conflict.json
  error_409_cluster_in_use.json
  error_426_upgrade_required.json
  userinfo.json
)

CHECK_MODE=false
if [[ "${1:-}" == "--check" ]]; then
  CHECK_MODE=true
fi

# Track every temp file we stage so a single top-level trap can clean them all
# up — on normal exit AND on a signal-driven one (same rationale as
# sync-schema.sh: a per-function trap would leak the temp file when the run is
# killed mid-fetch).
_tmpfiles=()
cleanup_tmpfiles() {
  # Guard the expansion: under `set -u`, "${arr[@]}" on an empty array is an
  # unbound-variable error in bash < 4.4 (macOS ships 3.2).
  [[ ${#_tmpfiles[@]} -eq 0 ]] && return 0
  local f
  for f in "${_tmpfiles[@]}"; do
    rm -f "$f"
  done
}
trap cleanup_tmpfiles EXIT INT TERM

# sync_one fetches one upstream fixture and either checks it against the
# in-tree copy (--check) or writes it. Returns non-zero on drift / missing
# file in check mode. Each fetch is staged in its own temp file so a
# half-failed curl never leaves a truncated file in the repo.
sync_one() {
  local name="$1"
  local url="${API_BASE}/${name}?ref=${BACKEND_REF}"
  local out="${OUT_DIR}/${name}"
  local tmp
  tmp=$(mktemp)
  _tmpfiles+=("$tmp")

  echo "==> fetching ${UPSTREAM_DIR}/${name} @ ${BACKEND_REF}"
  # sync_one is called as `if ! sync_one ...`, which suspends `set -e` for the
  # whole body — so check curl's exit explicitly and report the real fetch
  # failure instead of misdiagnosing an empty temp file as "not valid JSON".
  # --tlsv1.2 matches every other curl in the repo (scripts/install.sh).
  curl -fsSL --tlsv1.2 \
    -H "Authorization: Bearer ${TOKEN}" \
    -H "Accept: application/vnd.github.raw+json" \
    -H "X-GitHub-Api-Version: 2022-11-28" \
    "$url" -o "$tmp"
  local curl_rc=$?
  if [[ $curl_rc -ne 0 ]]; then
    echo "error: failed to fetch $url (curl exited $curl_rc)" >&2
    echo "(is the token authorized to read tracebloc/backend, and does the pinned ref carry the fixtures?)" >&2
    return "$curl_rc"
  fi

  # Make sure what came back is valid JSON before we trust it.
  if ! python3 -m json.tool < "$tmp" > /dev/null 2>&1; then
    echo "error: upstream response is not valid JSON ($url)" >&2
    echo "first 200 bytes of response:" >&2
    head -c 200 "$tmp" >&2
    return 2
  fi

  mkdir -p "$(dirname "$out")"

  if $CHECK_MODE; then
    if [[ ! -f "$out" ]]; then
      echo "error: $out does not exist" >&2
      echo "run \`scripts/sync-backend-fixtures.sh\` (without --check) to seed it." >&2
      return 1
    fi
    if ! diff -q "$tmp" "$out" >/dev/null; then
      echo "error: $out has drifted from upstream." >&2
      echo "diff (upstream → in-tree):" >&2
      diff -u "$out" "$tmp" | head -40 >&2 || true
      echo >&2
      echo "to fix, bump scripts/.backend-ref if needed, run \`scripts/sync-backend-fixtures.sh\`, and commit the result." >&2
      return 1
    fi
    echo "==> $out matches upstream — no drift"
    return 0
  fi

  # Write mode. Only touch the destination if the content actually changed, so
  # re-running the script on an already-current file produces no mtime churn.
  if [[ -f "$out" ]] && diff -q "$tmp" "$out" >/dev/null; then
    echo "==> $out already matches upstream — no change"
    return 0
  fi

  # Check the write explicitly (same rationale as sync-schema.sh: the `if !`
  # caller suspends `set -e`, so a failed cp must not read as success).
  if ! cp "$tmp" "$out"; then
    echo "error: failed to write $out (check directory permissions / disk space)" >&2
    return 1
  fi
  echo "==> wrote $out ($(wc -c < "$out" | tr -d ' ') bytes)"
  return 0
}

rc=0
for name in "${FIXTURES[@]}"; do
  if ! sync_one "$name"; then
    rc=1
  fi
done
exit "$rc"
