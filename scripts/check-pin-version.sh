#!/usr/bin/env bash
# =============================================================================
#  check-pin-version.sh — is the pinned data-ingestors contract still the
#  CURRENT contract version?  (backend#2704)
#
#  scripts/sync-schema.sh --check answers one question well: "does my vendored
#  copy match the SHA I pinned?" It never answers the second: "is the SHA I
#  pinned still current?" So the pin can sit six weeks / a whole contract
#  VERSION behind while every drift gate stays green — which is exactly what
#  happened (pin on layout contract v2, data-ingestors@develop on v3).
#
#  The pin is deliberate and must stay (backend#1009: a floating ref reds every
#  open CLI PR on unrelated upstream commits). This script does NOT float it. It
#  is the watcher for the pin ITSELF: resolve the pin, resolve data-ingestors'
#  current default branch, fetch the dataset-layout contract at BOTH refs, and
#  compare the CONTRACT VERSION — the top-level "version" string in
#  tracebloc_ingestor/schema/layout.v1.json.
#
#  VERSION, not commit distance. 180 unrelated upstream commits that never touch
#  the version are fine and stay green; a single version bump is the finding.
#  This is why it is a SCHEDULED job, never a PR gate: gating PRs on live
#  upstream is the backend#1009 failure this repo already removed.
#
#  FAIL CLOSED. "Cannot tell" is a finding, never a pass — the guard already has
#  one shape of "cannot tell reads as fine" (the pinned check going stale) and
#  must not gain a second. An absent/malformed pin, a moved path, an unreachable
#  raw URL, a non-JSON body, or a missing "version" all exit non-zero.
#
#  Exit codes:
#    0  IN SYNC   — the pin is on the current contract version.
#    1  DRIFT     — the pin's contract version differs from HEAD's. Re-pin
#                   (scripts/.data-ingestors-ref) and re-run scripts/sync-schema.sh.
#    2  FAIL CLOSED — could not evaluate (bad/absent pin, moved path, unreachable
#                   URL, non-JSON, missing version, or an unparseable
#                   sync-schema.sh). Never reported as agreement.
#
#  Env knobs (defaults are the production values; overrides exist for the
#  hermetic test harness, scripts/tests/pin-version-verify.sh):
#    REF_FILE                       pin file (default: scripts/.data-ingestors-ref)
#    SYNC_SCHEMA_SH                 source of the vendored layout path
#                                   (default: scripts/sync-schema.sh)
#    DATA_INGESTORS_REF             override the pin ref (default: from REF_FILE)
#    DATA_INGESTORS_DEFAULT_BRANCH  override the upstream default branch
#                                   (default: resolved via `gh api`)
#    PIN_LAYOUT_URL / HEAD_LAYOUT_URL  full URL overrides for the two fetches
#                                   (default: built from the refs above)
# =============================================================================
set -uo pipefail

readonly UPSTREAM_BASE="https://raw.githubusercontent.com/tracebloc/data-ingestors"

_here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REF_FILE="${REF_FILE:-${_here}/.data-ingestors-ref}"
# sync-schema.sh is the single source of truth for the vendored layout path;
# see derive_layout_subpath. SYNC_SCHEMA_SH lets the test harness point at a
# fixture.
SYNC_SCHEMA_SH="${SYNC_SCHEMA_SH:-${_here}/sync-schema.sh}"

die_closed() { echo "FAIL CLOSED: $*" >&2; exit 2; }

# A ref is interpolated into a raw.githubusercontent URL, so validate its shape
# before use — same guard as sync-schema.sh / chart-drift.yml: alnum start, then
# alnum . _ - / and no ".." component. Blocks path-traversal / extra segments.
valid_ref_shape() {
  grep -qE '^[A-Za-z0-9][A-Za-z0-9._/-]*$' <<<"$1" && ! grep -q '\.\.' <<<"$1"
}

# Derive the upstream layout path (tracebloc_ingestor/schema/layout.v1.json)
# from sync-schema.sh instead of holding a second copy: sync-schema.sh is the
# single source of truth for the vendored path, so a moved schema dir there
# changes both in one place, and the old "keep in lockstep" comment becomes an
# actual check (LukasWodka on cli#595; pin-version-verify.sh asserts the derived
# value). Returns non-zero — the caller fail-closes — if either declaration it
# parses is gone. It parses these two sync-schema.sh lines:
#   UPSTREAM_BASE="...:${DATA_INGESTORS_REF}/<dir>"
#   "${UPSTREAM_BASE}/<file>|internal/schema/layout.v1.json"
derive_layout_subpath() {
  [[ -f "$SYNC_SCHEMA_SH" ]] || { echo "  no sync-schema.sh at $SYNC_SCHEMA_SH" >&2; return 1; }
  local dir file
  dir="$(grep -oE 'DATA_INGESTORS_REF\}/[A-Za-z0-9._/-]+' "$SYNC_SCHEMA_SH" | head -1 | sed -E 's#^DATA_INGESTORS_REF\}/##')"
  file="$(grep -oE 'UPSTREAM_BASE\}/[A-Za-z0-9._-]+\.json\|internal/schema/layout\.v1\.json' "$SYNC_SCHEMA_SH" | head -1 | sed -E 's#^UPSTREAM_BASE\}/##; s#\|.*$##')"
  [[ -n "$dir" && -n "$file" ]] || { echo "  could not parse the layout path from $SYNC_SCHEMA_SH" >&2; return 1; }
  printf '%s/%s' "$dir" "$file"
}

# Resolve the pin: first non-comment, non-blank line of REF_FILE (the rule
# sync-schema.sh uses), overridable via DATA_INGESTORS_REF.
resolve_pin() {
  if [[ -n "${DATA_INGESTORS_REF:-}" ]]; then
    printf '%s' "$DATA_INGESTORS_REF"; return 0
  fi
  [[ -f "$REF_FILE" ]] || return 1
  grep -vE '^[[:space:]]*(#|$)' "$REF_FILE" 2>/dev/null | head -1 | tr -d '[:space:]'
}

# Resolve data-ingestors' CURRENT default branch, overridable for tests. Uses
# `gh api` (needs GH_TOKEN in CI); a public repo's metadata reads with the
# default workflow token.
resolve_default_branch() {
  if [[ -n "${DATA_INGESTORS_DEFAULT_BRANCH:-}" ]]; then
    printf '%s' "$DATA_INGESTORS_DEFAULT_BRANCH"; return 0
  fi
  local b
  b="$(gh api repos/tracebloc/data-ingestors --jq .default_branch 2>/dev/null)"
  # gh emits nothing on failure and the literal "null" when the field is absent
  # (a 200 with an unexpected body). Treat both as unresolved so main's -n check
  # fail-closes on the real cause instead of curling a `.../null/...` URL.
  [[ -n "$b" && "$b" != "null" ]] || return 1
  printf '%s' "$b"
}

# fetch_version <url> <label> — echo the top-level "version" string on success;
# any failure (fetch, non-JSON, missing/empty/non-scalar version) returns
# non-zero so the caller fail-closes. The body is captured into a variable, not
# a temp file: fetch_version is called as `$(fetch_version ...)`, i.e. in a
# subshell where an EXIT trap is reset and a `_tmpfiles+=` would be invisible to
# the parent — so a temp file here would leak. No temp file, nothing to clean.
fetch_version() {
  local url="$1" label="$2" body ver
  # -f: HTTP errors (404 moved-path) are failures; time bounds keep a hung
  # endpoint from wedging the run. Mirrors sync-schema.sh's curl.
  if ! body="$(curl -fsSL --tlsv1.2 --connect-timeout 10 --max-time 60 "$url")"; then
    echo "  could not fetch the $label contract: $url" >&2
    return 1
  fi
  # python3 exits non-zero on a non-JSON body or a missing/empty/non-scalar
  # "version"; pipefail carries that exit to the `if !`.
  # shellcheck disable=SC2016  # the $-expressions are Python, not shell
  if ! ver="$(printf '%s' "$body" | python3 -c '
import json, sys
try:
    data = json.load(sys.stdin)
except Exception:
    sys.exit(1)
v = data.get("version")
if not isinstance(v, (str, int)) or isinstance(v, bool) or str(v).strip() == "":
    sys.exit(1)
print(str(v).strip())
')"; then
    echo "  the $label contract is not JSON, or has no top-level \"version\": $url" >&2
    return 1
  fi
  printf '%s' "$ver"
}

main() {
  local pin default_branch pin_url head_url pin_ver head_ver layout_subpath=""

  pin="$(resolve_pin)"
  [[ -n "$pin" ]] || die_closed "no ref in ${REF_FILE} (first non-comment line must be a commit SHA)"
  valid_ref_shape "$pin" || die_closed "invalid pin ref shape: '$pin'"

  # Derive the layout path only when we actually build a URL from it — the test
  # harness overrides both URLs and then needs neither sync-schema.sh nor a
  # network fetch.
  if [[ -z "${PIN_LAYOUT_URL:-}" || -z "${HEAD_LAYOUT_URL:-}" ]]; then
    layout_subpath="$(derive_layout_subpath)" \
      || die_closed "could not derive the layout path from ${SYNC_SCHEMA_SH} (its format changed?)"
  fi

  # Build the pin URL (overridable). The pin comes from the ref file above.
  pin_url="${PIN_LAYOUT_URL:-${UPSTREAM_BASE}/${pin}/${layout_subpath}}"

  # Build the HEAD URL (overridable). Only resolve the default branch when the
  # URL isn't overridden, so the test harness needs no `gh` and no network.
  if [[ -n "${HEAD_LAYOUT_URL:-}" ]]; then
    head_url="$HEAD_LAYOUT_URL"
    default_branch="(HEAD_LAYOUT_URL override)"
  else
    default_branch="$(resolve_default_branch)"
    [[ -n "$default_branch" ]] || die_closed "could not resolve data-ingestors' default branch (gh api)"
    valid_ref_shape "$default_branch" || die_closed "invalid default-branch shape: '$default_branch'"
    head_url="${UPSTREAM_BASE}/${default_branch}/${layout_subpath}"
  fi

  pin_ver="$(fetch_version "$pin_url" "pinned")"  || die_closed "could not read the pinned contract version"
  head_ver="$(fetch_version "$head_url" "HEAD")" || die_closed "could not read the HEAD contract version"

  echo "data-ingestors dataset-layout contract version"
  echo "  pin            ${pin}  ->  v${pin_ver}"
  echo "  default branch ${default_branch}  ->  v${head_ver}"

  if [[ "$pin_ver" == "$head_ver" ]]; then
    echo "IN SYNC: the pin is on the current contract version (v${pin_ver})."
    return 0
  fi

  echo "DRIFT: the pinned contract is v${pin_ver}, but data-ingestors HEAD is v${head_ver}." >&2
  echo "The CLI's vendored layout/sidecar/help are derived from a stale contract version." >&2
  echo "Fix: bump scripts/.data-ingestors-ref, run scripts/sync-schema.sh, commit the regenerated files (cli#286 shape)." >&2
  return 1
}

# Run main only when executed directly, not when sourced — so the test harness
# can source this file and call derive_layout_subpath in isolation.
if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
  main "$@"
fi
