#!/usr/bin/env bash
# Blocking deadcode gate (cli#281). Fail on any function unreachable from the
# CLI entrypoint that is not explicitly declared in deadcode-allowlist.txt.
#
# The x/tools deadcode command always exits 0, even with findings — so the
# gate keys on OUTPUT: normalize each finding to "<file>: <func>" (strip
# :line:col so ordinary edits don't red the gate), subtract the allowlist,
# and fail loudly on whatever's left. Stale allowlist entries (allowlisted
# funcs deadcode no longer reports) warn but don't fail — that's cleanup,
# not breakage.
#
# Usage: scripts/deadcode-check.sh   (run from the repo root)
# DEADCODE_VERSION overrides the pinned tool version (Makefile passes it,
# keeping the single source of truth there — lockstep with build.yml).
#
# Portable to bash 3.2 (macOS default).
set -euo pipefail

DEADCODE_VERSION="${DEADCODE_VERSION:-v0.48.0}"
allowlist_file="$(dirname "$0")/deadcode-allowlist.txt"

raw="$(go run "golang.org/x/tools/cmd/deadcode@${DEADCODE_VERSION}" ./cmd/tracebloc)"

# "path/file.go:12:6: unreachable func: Name" -> "path/file.go: Name"
findings="$(printf '%s' "$raw" | sed -E 's/^([^:]+):[0-9]+:[0-9]+: unreachable func: /\1: /')"

# Allowlist minus comments/blanks.
allowed="$(grep -v '^[[:space:]]*#' "$allowlist_file" | grep -v '^[[:space:]]*$' || true)"

unexpected=""
if [ -n "$findings" ]; then
  unexpected="$(printf '%s\n' "$findings" | grep -vxF -e dummy-never-matches -f <(printf '%s\n' "$allowed") || true)"
fi

stale=""
if [ -n "$allowed" ]; then
  stale="$(printf '%s\n' "$allowed" | { grep -vxF -f <(printf '%s\n' "$findings") || true; })"
fi

if [ -n "$stale" ]; then
  echo "==> deadcode: stale allowlist entries (no longer reported — consider pruning $allowlist_file):"
  printf '%s\n' "$stale" | sed 's/^/    /'
fi

if [ -n "$unexpected" ]; then
  echo "==> deadcode: unreachable from ./cmd/tracebloc and NOT allowlisted:"
  printf '%s\n' "$unexpected" | sed 's/^/    /'
  echo "==> delete the code, or (deliberately, with a reason) add it to $allowlist_file"
  exit 1
fi

echo "==> deadcode: clean (allowlist: $(printf '%s\n' "$allowed" | grep -c . ) entries)"
