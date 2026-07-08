#!/usr/bin/env bash
# Fail if a load-bearing package's statement coverage drops below its floor.
#
# internal/cli (the money path — submit → classify → JSON → reclaim) and
# internal/submit (the jobs-manager orchestration) were historically the
# thinnest-tested, highest-stakes code in the CLI (backend#1009). A bare
# `go test -cover` prints the number and asserts nothing, so coverage could
# silently rot. This gate makes a regression loud.
#
# The floors are a RATCHET: set just under the current numbers, and bumped UP
# as coverage improves — never silently down. Lowering a floor must be a
# deliberate, reviewed edit here (with a reason), not a side effect of deleting
# tests. Current (develop): internal/cli ~70%, internal/submit ~75%.
#
# Usage: scripts/coverage-floor.sh   (run from the repo root)
#
# Portable to bash 3.2 (macOS default): no associative arrays.
set -euo pipefail

# "package:floor" entries. Keep floors integers; coverage is compared as a
# float against them.
FLOORS="
internal/cli:68
internal/submit:72
"

status=0
for entry in $FLOORS; do
  pkg="${entry%%:*}"
  min="${entry##*:}"
  line="$(go test -cover "./$pkg/" 2>/dev/null | grep -E 'coverage: [0-9]' || true)"
  pct="$(printf '%s\n' "$line" | sed -nE 's/.*coverage: ([0-9]+(\.[0-9]+)?)% of statements.*/\1/p' | head -1)"
  if [ -z "$pct" ]; then
    echo "::error::could not read coverage for ./$pkg/ (did any test run?)" >&2
    status=1
    continue
  fi
  # awk exits 0 when pct < min (i.e. below the floor → failure).
  if awk "BEGIN{exit !($pct < $min)}"; then
    echo "::error::./$pkg/ coverage ${pct}% is below the floor ${min}% — add tests, or (with a reason) lower the floor in scripts/coverage-floor.sh" >&2
    status=1
  else
    echo "ok: ./$pkg/ ${pct}% >= ${min}%"
  fi
done

exit "$status"
