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
#  Its own properties (reddens on each restatement shape, fails closed on missing
#  inputs) are pinned by scripts/tests/tool-pins-verify.sh (make tool-pins-selftest).
# =============================================================================
set -uo pipefail
cd "$(dirname "$0")/.." || exit 2

# Fail CLOSED. A guard that cannot find its inputs must not report clean: that is
# the failure this file was written against (backend#1729).
[[ -f Makefile ]] || { echo "check-tool-pins: no Makefile — refusing to report clean" >&2; exit 2; }
[[ -d .github/workflows ]] || { echo "check-tool-pins: no .github/workflows — refusing to report clean" >&2; exit 2; }

# Tools whose version the Makefile owns, as
#
#   <make var>:<module path fragment>[:<GitHub action>]
#
# Add a row when a tool moves to a `make` target that CI calls. Two restatement
# shapes are caught:
#
#   1. `<module>@<version>` anywhere in a workflow — the `go install` / `go run`
#      form. Every row is checked for this.
#   2. A literal `version:` input under a `uses: <action>@...` step — the form a
#      tool takes when a workflow runs it through its GitHub action instead of
#      `go run`. Only rows with the optional third field are checked for this;
#      a `version: ${{ steps.<id>.outputs.<x> }}` that reads the Makefile
#      (`make print-<VAR>`) is not a restatement and passes.
TOOLS=(
  "GOVULNCHECK_VERSION:golang.org/x/vuln/cmd/govulncheck"
  # cli#549: the Lint job's two inline formatter steps became `make fmt-check`,
  # so build.yml no longer holds its own goimports version. This row is what
  # keeps that true on the next bump.
  "GOIMPORTS_VERSION:golang.org/x/tools/cmd/goimports"
  # The Lint job's four inline `go install tool@version` steps became one
  # `make lint`. These rows are what keep the Makefile the only declaration.
  "ERRCHECK_VERSION:github.com/kisielk/errcheck"
  "INEFFASSIGN_VERSION:github.com/gordonklaus/ineffassign"
  "MISSPELL_VERSION:github.com/client9/misspell/cmd/misspell"
  "STATICCHECK_VERSION:honnef.co/go/tools/cmd/staticcheck"
  # golangci.yml runs golangci-lint through its action, so the restatement to
  # catch is the action's `version:` input (shape 2), not `module@version`.
  "GOLANGCI_LINT_VERSION:github.com/golangci/golangci-lint/v2/cmd/golangci-lint:golangci/golangci-lint-action"
)

# The workflow files, listed once. An empty list is a malfunction, not a pass:
# with nothing to scan, "no offender found" is the unearned green.
workflows=()
while IFS= read -r -d '' f; do workflows+=("$f"); done < <(
  find .github/workflows -maxdepth 1 -type f \( -name '*.yml' -o -name '*.yaml' \) -print0 | sort -z
)
if (( ${#workflows[@]} == 0 )); then
  echo "check-tool-pins: no workflow files under .github/workflows — refusing to report clean" >&2
  exit 2
fi

fail=0
checked=0

for row in "${TOOLS[@]}"; do
  var="${row%%:*}"
  rest="${row#*:}"
  module="${rest%%:*}"
  action=""
  [[ "$rest" == *:* ]] && action="${rest#*:}"

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
  #
  # No 2>/dev/null and no `|| true`: grep rc 1 is "no offender" and fine, but rc>=2
  # is a real error (unreadable tree, bad invocation) and must fail CLOSED. Laundering
  # it into an empty hit list is the unearned exit 0 this guard was written against
  # (backend#1729); scan() in check-style.sh handles the same grep class this way.
  offenders="$(grep -rn -- "${module}@" .github/workflows/)"
  rc=$?
  if (( rc >= 2 )); then
    echo "check-tool-pins: grep errored (rc=${rc}) scanning .github/workflows for '${module}@' — refusing to report clean" >&2
    exit 2
  fi
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

  # Shape 2: a literal `version:` input on the tool's GitHub action. The state
  # machine is one step wide and indentation-aware:
  #
  #   - a `uses:` of the action ARMS it and records the column of the `uses:`
  #     key (the same column for `- uses:` and for `uses:` under `- name:`,
  #     which is where the step's sibling keys `with:`/`id:`/`name:` sit);
  #   - any later non-blank, non-comment line SHALLOWER than that column is a
  #     sibling step (`- name:` / `- uses:`) or a parent key and DISARMS it.
  #     YAML list items inside the step's own `with:` block (`args:` items) are
  #     deeper and do not — disarming on any `- ` line let a literal `version:`
  #     after such a list pass clean;
  #   - state resets at every file boundary (FNR == 1), so a file that ends on
  #     the action's `uses:` line cannot arm the next file in sort order;
  #   - while armed, a `version:` whose value is not a `${{ ... }}` expression
  #     is a copy — any literal, not just a digit-leading one. `latest` is both
  #     an un-pinning and a restatement the action honors.
  if [[ -n "$action" ]]; then
    action_offenders="$(awk -v action="$action" '
      FNR == 1 { armed = 0 }
      /^[[:space:]]*#/ || /^[[:space:]]*$/ { next }
      { match($0, /^[[:space:]]*/); ind = RLENGTH }
      armed && ind < key { armed = 0 }
      index($0, "uses:") && index($0, action "@") { armed = 1; key = index($0, "uses:") - 1; next }
      armed && /^[[:space:]]*version:/ {
        v = $0
        sub(/^[[:space:]]*version:[[:space:]]*["]?/, "", v)
        if (index(v, "${{") != 1) printf "%s:%d:%s\n", FILENAME, FNR, $0
      }
    ' "${workflows[@]}")"
    rc=$?
    if (( rc != 0 )); then
      echo "check-tool-pins: awk errored (rc=${rc}) scanning for a literal version: on ${action} — refusing to report clean" >&2
      exit 2
    fi
    if [[ -n "$action_offenders" ]]; then
      echo "A workflow gives ${action} a literal version:" >&2
      printf '%s\n' "$action_offenders" >&2
      echo >&2
      echo "  ${var} in the Makefile already declares this (${version}). Read it in a step" >&2
      echo "  (make print-${var}) and pass \${{ steps.<id>.outputs.version }} instead, so the" >&2
      echo "  action and make lint-full can never run different versions." >&2
      fail=1
    fi
  fi
  checked=$((checked + 1))
done

if (( fail )); then
  exit 1
fi
echo "check-tool-pins: ${checked} tool pin(s) declared once, in the Makefile"
