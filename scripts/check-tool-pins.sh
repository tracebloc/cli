#!/usr/bin/env bash
# =============================================================================
#  check-tool-pins.sh — one declaration per pinned tool version (backend#1972)
#
#  govulncheck's version used to live in THREE places: build.yml's job,
#  vulncheck.yml's job, and GOVULNCHECK_VERSION in the Makefile — kept in step by
#  a comment reading "keep the job in lockstep". Two copies held in sync by a
#  request is not a mechanism; all three happened to read v1.1.4, which is
#  exactly what made it look fine.
#
#  Both workflows now run `make vulncheck`, so the Makefile is the declaration.
#  This guard exists so that stays true: it PARSES the Makefile for the tools it
#  covers and fails if a workflow hardcodes a version for one of them.
#
#  DERIVED, NOT RESTATED: the version to look for is read from the Makefile. This
#  guard holds no version of its own, so it cannot agree with itself while
#  disagreeing with reality.
#
#  Runs in CI (the Lint job, beside check-style.sh) and locally:
#    make check-tool-pins   (or: bash scripts/check-tool-pins.sh)
#  Exit 0 = clean, 1 = a restated pin was found, 2 = the guard itself errored.
# =============================================================================
set -uo pipefail
cd "$(dirname "$0")/.." || exit 2

# Fail CLOSED. A guard that cannot find its inputs must not report clean: that is
# the failure this file was written against (backend#1729).
[[ -f Makefile ]] || { echo "check-tool-pins: no Makefile — refusing to report clean" >&2; exit 2; }
[[ -d .github/workflows ]] || { echo "check-tool-pins: no .github/workflows — refusing to report clean" >&2; exit 2; }

# Tools whose version the Makefile owns, as  <make var>:<module path fragment>.
# Add a row when a tool moves to a `make` target that CI calls.
TOOLS=(
  "GOVULNCHECK_VERSION:golang.org/x/vuln/cmd/govulncheck"
)

fail=0
checked=0

for row in "${TOOLS[@]}"; do
  var="${row%%:*}"
  module="${row#*:}"

  # Parse the REAL declaration. `?=` or `=`, any surrounding spaces.
  version="$(sed -nE "s/^[[:space:]]*${var}[[:space:]]*\\??=[[:space:]]*([^[:space:]#]+).*/\\1/p" Makefile | head -1)"
  if [[ -z "$version" ]]; then
    echo "check-tool-pins: ${var} is not declared in the Makefile, so this guard cannot" >&2
    echo "  verify anything about ${module}. Either restore the declaration or drop the" >&2
    echo "  row from TOOLS — an unparseable input is a finding, not a pass." >&2
    exit 2
  fi

  # Any workflow naming the module with an @version is holding its own copy.
  # `make <target>` references carry no version and are therefore invisible here,
  # which is the whole point.
  offenders="$(grep -rn -- "${module}@" .github/workflows/ 2>/dev/null || true)"
  if [[ -n "$offenders" ]]; then
    echo "A workflow pins ${module} directly:" >&2
    printf '%s\n' "$offenders" >&2
    echo >&2
    echo "  ${var} in the Makefile already declares this (${version}), and CI runs it" >&2
    echo "  via a make target. A second copy here is what backend#1972 removed: three" >&2
    echo "  copies agreeing today, drifting on the next bump, with nothing to notice." >&2
    echo "  Call the make target instead." >&2
    fail=1
  fi
  checked=$((checked + 1))
done

if (( fail )); then
  exit 1
fi
echo "check-tool-pins: ${checked} tool pin(s) declared once, in the Makefile"
