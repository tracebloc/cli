#!/usr/bin/env bash
# Sync the location zone list (ZONE_CHOICES) from tracebloc/backend into the
# CLI's vendored internal/zones/zones.json, so `client create` validates a
# location against exactly what the backend's ChoiceField accepts.
#
# The backend (metaApi/models/zone_choices.py) is the source of truth — it is
# what the API validates against. It lives in a PRIVATE repo, so unlike
# sync-schema.sh (which curls a public URL) this reads a local sibling checkout;
# point ZONES_SOURCE at zone_choices.py if your layout differs.
#
# Usage:
#   scripts/sync-zones.sh            regenerate zones.json
#   scripts/sync-zones.sh --check    fail if zones.json has drifted (CI)
#
# Env:
#   ZONES_SOURCE  path to zone_choices.py
#                 (default: ../backend/metaApi/models/zone_choices.py)
#   ZONES_OUT     destination (default: internal/zones/zones.json)
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ZONES_SOURCE="${ZONES_SOURCE:-$repo_root/../backend/metaApi/models/zone_choices.py}"
ZONES_OUT="${ZONES_OUT:-$repo_root/internal/zones/zones.json}"

if [[ ! -f "$ZONES_SOURCE" ]]; then
  echo "error: zone source not found: $ZONES_SOURCE" >&2
  echo "       set ZONES_SOURCE to the backend's metaApi/models/zone_choices.py" >&2
  exit 1
fi

tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT

# zone_choices.py holds ZONE_CHOICES = [("CODE", "Name"), ...]. Extract the
# (code, name) pairs by regex (no eval of the source) and emit a sorted
# {code: name} JSON object. Zone names contain no embedded double quotes.
python3 - "$ZONES_SOURCE" >"$tmp" <<'PY'
import json, re, sys

with open(sys.argv[1], encoding="utf-8") as f:
    content = f.read()
pairs = re.findall(r'\(\s*"([^"]+)"\s*,\s*"([^"]+)"\s*\)', content)
if not pairs:
    sys.exit("no zone tuples found in " + sys.argv[1])
json.dump(dict(pairs), sys.stdout, indent=2, sort_keys=True, ensure_ascii=False)
sys.stdout.write("\n")
PY

if [[ "${1:-}" == "--check" ]]; then
  if [[ ! -f "$ZONES_OUT" ]] || ! diff -q "$tmp" "$ZONES_OUT" >/dev/null; then
    echo "error: $ZONES_OUT has drifted from the backend — run scripts/sync-zones.sh" >&2
    diff -u "$ZONES_OUT" "$tmp" 2>/dev/null | head -40 >&2 || true
    exit 1
  fi
  echo "==> zones.json matches the backend — no drift"
  exit 0
fi

mkdir -p "$(dirname "$ZONES_OUT")"
cp "$tmp" "$ZONES_OUT"
echo "==> wrote $ZONES_OUT ($(grep -c '": "' "$ZONES_OUT") zones)"
