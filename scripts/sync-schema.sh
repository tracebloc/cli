#!/usr/bin/env bash
# Sync ingest.v1.json from tracebloc/data-ingestors into the CLI's
# embedded copy at internal/schema/ingest.v1.json.
#
# The CLI validates locally using this schema. Drift between the
# CLI's copy and data-ingestors' canonical source is a real
# correctness hazard — a customer's YAML could pass `tracebloc ingest
# validate` locally but be rejected by jobs-manager (or vice versa).
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
#   SCHEMA_SOURCE_URL   override the upstream URL (default: built from the
#                       pinned ref below)
#   DATA_INGESTORS_REF  override the data-ingestors ref (default: the pinned
#                       SHA in scripts/.data-ingestors-ref, else master)
#   SCHEMA_OUT          override the in-tree destination (default: internal/schema/ingest.v1.json)
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

readonly DEFAULT_URL="https://raw.githubusercontent.com/tracebloc/data-ingestors/${DATA_INGESTORS_REF}/tracebloc_ingestor/schema/ingest.v1.json"
readonly DEFAULT_OUT="internal/schema/ingest.v1.json"

SCHEMA_SOURCE_URL="${SCHEMA_SOURCE_URL:-$DEFAULT_URL}"
SCHEMA_OUT="${SCHEMA_OUT:-$DEFAULT_OUT}"

CHECK_MODE=false
if [[ "${1:-}" == "--check" ]]; then
  CHECK_MODE=true
fi

# Stage the fetched schema in a temp file so a half-failed curl doesn't
# leave a truncated file in the repo.
tmp=$(mktemp)
trap 'rm -f "$tmp"' EXIT

echo "==> fetching $SCHEMA_SOURCE_URL"
curl -fsSL "$SCHEMA_SOURCE_URL" -o "$tmp"

# Make sure what came back is valid JSON before we trust it.
if ! python3 -m json.tool < "$tmp" > /dev/null 2>&1; then
  echo "error: upstream response is not valid JSON" >&2
  echo "first 200 bytes of response:" >&2
  head -c 200 "$tmp" >&2
  exit 2
fi

mkdir -p "$(dirname "$SCHEMA_OUT")"

if $CHECK_MODE; then
  if [[ ! -f "$SCHEMA_OUT" ]]; then
    echo "error: $SCHEMA_OUT does not exist" >&2
    echo "run \`scripts/sync-schema.sh\` (without --check) to seed it." >&2
    exit 1
  fi
  if ! diff -q "$tmp" "$SCHEMA_OUT" >/dev/null; then
    echo "error: $SCHEMA_OUT has drifted from upstream." >&2
    echo "diff (upstream → in-tree):" >&2
    diff -u "$SCHEMA_OUT" "$tmp" | head -40 >&2 || true
    echo >&2
    echo "to fix, run \`scripts/sync-schema.sh\` and commit the result." >&2
    exit 1
  fi
  echo "==> $SCHEMA_OUT matches upstream — no drift"
  exit 0
fi

# Write mode. Only touch the destination if the content actually
# changed, so re-running the script on an already-current schema
# produces no file-mtime churn.
if [[ -f "$SCHEMA_OUT" ]] && diff -q "$tmp" "$SCHEMA_OUT" >/dev/null; then
  echo "==> $SCHEMA_OUT already matches upstream — no change"
  exit 0
fi

mv "$tmp" "$SCHEMA_OUT"
trap - EXIT  # the temp file is now in place; don't try to rm it
echo "==> wrote $SCHEMA_OUT ($(wc -c < "$SCHEMA_OUT" | tr -d ' ') bytes)"
