#!/usr/bin/env bash
# Sync the CLI's embedded contract files from tracebloc/data-ingestors:
#
#   - ingest.v1.json  — the ingest-config JSON Schema the CLI validates against
#   - layout.v1.json  — the per-task dataset-layout contract (data-ingestors
#                        #347/#353) the CLI mirrors for discovery + staging
#
# both under internal/schema/.
#
# The CLI validates locally using these files. Drift between the
# CLI's copy and data-ingestors' canonical source is a real
# correctness hazard — a customer's YAML could pass `tracebloc ingest
# validate` locally but be rejected by jobs-manager (or vice versa), or the
# CLI could stage a layout the ingestor rejects.
#
# Run this script when bumping the schema version. CI invokes it in
# check-mode (`--check`) to fail builds that have drifted without
# running the sync first.
#
# Usage:
#   scripts/sync-schema.sh                # write into internal/schema/
#   scripts/sync-schema.sh --check        # verify in-tree copy matches upstream; exit non-zero on drift
#
# Env knobs:
#   SCHEMA_SOURCE_URL   override the upstream URL for ingest.v1.json (default:
#                       built from the pinned ref below)
#   DATA_INGESTORS_REF  override the data-ingestors ref (default: the pinned
#                       SHA in scripts/.data-ingestors-ref, else master)
#   SCHEMA_OUT          override the in-tree destination for ingest.v1.json
#                       (default: internal/schema/ingest.v1.json)
#
# The ref is PINNED (scripts/.data-ingestors-ref), not a floating branch, so an
# unrelated upstream commit doesn't red every open CLI PR — adopting upstream
# is a deliberate SHA bump + re-sync (backend#1009).
#
# Future: when we cut a v2 schema, this script will need to learn
# about multiple versions (e.g. embed v1 AND v2 side-by-side, picked
# at runtime based on the ingest.yaml's apiVersion). Keeping the path
# explicit in SCHEMA_OUT makes that extension easy.

set -euo pipefail

# The pinned data-ingestors ref: first non-comment, non-blank line of the ref
# file (a full commit SHA), overridable via DATA_INGESTORS_REF, falling back to
# master if the file is somehow absent.
REF_FILE="$(cd "$(dirname "$0")" && pwd)/.data-ingestors-ref"
readonly REF_FILE
_pinned_ref="$(grep -vE '^[[:space:]]*(#|$)' "$REF_FILE" 2>/dev/null | head -1 | tr -d '[:space:]' || true)"
DATA_INGESTORS_REF="${DATA_INGESTORS_REF:-${_pinned_ref:-master}}"

# The ref is interpolated into a download URL, so validate it before use
# (like scripts/install.sh does for its release tag): a crafted ref — most
# plausibly via the DATA_INGESTORS_REF override — could otherwise inject path
# traversal ("../..") or extra segments into the raw.githubusercontent path.
# Allow only a SHA / branch / tag shape: alnum start, then alnum . _ - / and
# no ".." component.
if ! printf '%s' "$DATA_INGESTORS_REF" | grep -qE '^[A-Za-z0-9][A-Za-z0-9._/-]*$' \
   || printf '%s' "$DATA_INGESTORS_REF" | grep -q '\.\.'; then
  echo "error: invalid data-ingestors ref '$DATA_INGESTORS_REF' — expected a commit SHA, branch, or tag" >&2
  echo "(set it in scripts/.data-ingestors-ref or via DATA_INGESTORS_REF)" >&2
  exit 2
fi

readonly UPSTREAM_BASE="https://raw.githubusercontent.com/tracebloc/data-ingestors/${DATA_INGESTORS_REF}/tracebloc_ingestor/schema"

readonly DEFAULT_URL="${UPSTREAM_BASE}/ingest.v1.json"
readonly DEFAULT_OUT="internal/schema/ingest.v1.json"

# ingest.v1.json keeps its historical env overrides; layout.v1.json is derived
# from the pinned ref. Each entry is "URL|OUT".
SCHEMA_SOURCE_URL="${SCHEMA_SOURCE_URL:-$DEFAULT_URL}"
SCHEMA_OUT="${SCHEMA_OUT:-$DEFAULT_OUT}"
FILES=(
  "${SCHEMA_SOURCE_URL}|${SCHEMA_OUT}"
  "${UPSTREAM_BASE}/layout.v1.json|internal/schema/layout.v1.json"
)

CHECK_MODE=false
if [[ "${1:-}" == "--check" ]]; then
  CHECK_MODE=true
fi

# sync_one fetches one upstream file and either checks it against the in-tree
# copy (--check) or writes it. Returns non-zero on drift / missing file in
# check mode. Each fetch is staged in its own temp file so a half-failed curl
# never leaves a truncated file in the repo.
sync_one() {
  local url="$1" out="$2"
  local tmp
  tmp=$(mktemp)
  trap 'rm -f "$tmp"' RETURN

  echo "==> fetching $url"
  curl -fsSL "$url" -o "$tmp"

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
      echo "run \`scripts/sync-schema.sh\` (without --check) to seed it." >&2
      return 1
    fi
    if ! diff -q "$tmp" "$out" >/dev/null; then
      echo "error: $out has drifted from upstream." >&2
      echo "diff (upstream → in-tree):" >&2
      diff -u "$out" "$tmp" | head -40 >&2 || true
      echo >&2
      echo "to fix, bump scripts/.data-ingestors-ref if needed, run \`scripts/sync-schema.sh\`, and commit the result." >&2
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

  cp "$tmp" "$out"
  echo "==> wrote $out ($(wc -c < "$out" | tr -d ' ') bytes)"
  return 0
}

rc=0
for entry in "${FILES[@]}"; do
  url="${entry%%|*}"
  out="${entry##*|}"
  if ! sync_one "$url" "$out"; then
    rc=1
  fi
done
exit "$rc"
