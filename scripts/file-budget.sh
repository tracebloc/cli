#!/usr/bin/env bash
# Fail if a budgeted file grows past its line ceiling.
#
# data.go reached 1539 lines before the WS-B split (backend#1106); a
# 1000+-line file is where the next one quietly grows. This gate makes the
# growth loud at PR time instead of in the next audit. It's a shell ratchet
# rather than a golangci funlen/lll rule per the #6 standalone-tools
# decision (same reasoning as scripts/coverage-floor.sh, whose shape this
# clones).
#
# The ceilings are a RATCHET: set just above the current numbers, and only
# ever lowered as files shrink — never silently raised. Raising one must be
# a deliberate, reviewed edit to this checked-in file (with a reason), not
# a side effect of a big PR. Current (develop, post cli#282/#283 split):
# preflight.go ~1655, client.go ~1027, home.go ~849, data.go ~240.
#
# Usage: scripts/file-budget.sh   (run from the repo root)
#
# Portable to bash 3.2 (macOS default): no associative arrays.
set -euo pipefail

# "path:max_lines" entries. Keep ceilings integers.
BUDGETS="
internal/push/preflight.go:1700
internal/cli/data.go:500
internal/cli/client.go:1050
internal/cli/home.go:855
"

status=0
for entry in $BUDGETS; do
  path="${entry%%:*}"
  max="${entry##*:}"
  # A malformed entry (no ":max", or a non-integer ceiling) must fail
  # loudly, not slip through as a silent no-op for that file — same guard,
  # same reason as coverage-floor.sh.
  if [ "$path" = "$entry" ] || ! printf '%s' "$max" | grep -qE '^[0-9]+$'; then
    echo "::error::malformed BUDGETS entry '$entry' (want 'path:INT') — fix scripts/file-budget.sh" >&2
    status=1
    continue
  fi
  if [ ! -f "$path" ]; then
    # A budgeted file that vanished (moved/renamed) means this list is
    # stale — update it in the same PR, as a reviewed diff, so the budget
    # follows the file instead of evaporating.
    echo "::error::budgeted file '$path' not found — update scripts/file-budget.sh in the same PR" >&2
    status=1
    continue
  fi
  lines="$(wc -l < "$path" | tr -d '[:space:]')"
  if [ "$lines" -gt "$max" ]; then
    echo "::error::$path is $lines lines, over its $max-line budget — split it (cli#282 is the pattern), or (with a reason) raise the ceiling in scripts/file-budget.sh" >&2
    status=1
  else
    echo "ok: $path $lines <= $max"
  fi
done

exit "$status"
