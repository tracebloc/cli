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
# tests. Current (develop, 2026-07-14): internal/cli 82.9%, internal/submit
# 80.4%, internal/push 89.0%, internal/cluster 78.5%.
#
# Usage: scripts/coverage-floor.sh   (run from the repo root)
#
# Portable to bash 3.2 (macOS default): no associative arrays.
set -euo pipefail

# "package:floor" entries. Keep floors integers; coverage is compared as a
# float against them.
FLOORS="
internal/cli:80
internal/submit:78
internal/push:87
internal/cluster:75
"

status=0
for entry in $FLOORS; do
  pkg="${entry%%:*}"
  min="${entry##*:}"
  # A malformed entry (no ":floor", or a non-integer floor) must fail loudly,
  # not slip through. Without this, a dropped colon leaves min="$entry" (the
  # whole token); the awk comparison below then errors on that as bare source
  # and exits non-zero — which `if awk` reads as "not below floor", prints a
  # bogus "ok", and turns the ratchet into a silent no-op for that package.
  if [ "$pkg" = "$entry" ] || ! printf '%s' "$min" | grep -qE '^[0-9]+$'; then
    echo "::error::malformed FLOORS entry '$entry' (want 'package:INT') — fix scripts/coverage-floor.sh" >&2
    status=1
    continue
  fi
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
