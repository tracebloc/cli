#!/usr/bin/env bash
# Regenerate (or --check) the validator-parity goldens against the REAL
# data-ingestors validators. Mirrors sync-schema.sh's shape.
#
#   DATA_INGESTORS_DIR=~/repos/data-ingestors scripts/sync-validator-goldens.sh [--check]
#
# Needs a data-ingestors checkout with pandas + Pillow importable — its own
# .venv works: point PYTHON at it, e.g.
#   PYTHON="$DATA_INGESTORS_DIR/.venv/bin/python" scripts/sync-validator-goldens.sh --check
set -euo pipefail
cd "$(dirname "$0")/.."
GOLDENS="internal/push/testdata/parity/goldens.json"
PYTHON="${PYTHON:-python3}"
if [[ "${1:-}" == "--check" ]]; then
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' EXIT
  cp "$GOLDENS" "$tmp/committed.json"
  "$PYTHON" scripts/gen-validator-goldens.py >/dev/null
  # Compare VERDICTS only — error text may drift harmlessly (and embeds
  # fixture paths); verdicts may not. VALUE-level goldens (resolved label +
  # row count + class set) carry no paths, so compare them too — a value-only
  # drift (the data-ingestors #340 class: verdict unchanged, stored labels
  # change) must fail the check, not slip through.
  if ! "$PYTHON" -c "
import json,sys
a=json.load(open('$tmp/committed.json'))['verdicts']
b=json.load(open('$GOLDENS'))['verdicts']
def view(d): return {k:(v['verdict'], v.get('values')) for k,v in d.items()}
sys.exit(0 if view(a)==view(b) else 1)
"; then
    cp "$tmp/committed.json" "$GOLDENS"   # restore — check must not mutate
    echo "DRIFT: the ingestor's validator verdicts or read-path VALUES changed. Re-run" >&2
    echo "the generator, commit the new goldens, and update cases.json (+ the Go preview)" >&2
    echo "consciously." >&2
    exit 1
  fi
  cp "$tmp/committed.json" "$GOLDENS"   # keep the committed copy (paths etc. unchanged)
  echo "validator goldens in sync"
else
  "$PYTHON" scripts/gen-validator-goldens.py
fi
