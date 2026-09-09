#!/usr/bin/env bash
# =============================================================================
#  tool-pins-verify.sh — the properties scripts/check-tool-pins.sh must not lose
#
#  The guard exists to turn "keep these versions in lockstep" comments into a
#  check. A guard that stops reddening is indistinguishable from one that has
#  nothing to find, so this harness asserts, against the REAL script (copied
#  verbatim into a fixture tree — never a re-implementation of its rules):
#
#    1. clean tree                                -> exit 0
#    2. `<module>@<version>` restated in a workflow -> exit 1, names the file
#    3. literal `version:` on the tool's action    -> exit 1, names the file
#    4. literal `version:` on an UNRELATED action  -> exit 0 (no false positive)
#    5. Makefile missing a declared tool           -> exit 2 (fail closed)
#    6. no workflow files at all                   -> exit 2 (fail closed)
#
#  The tool rows are DERIVED from the guard's own TOOLS array, so this file holds
#  no module path or make variable of its own to drift from it.
#  Hermetic: no Go, no network, temp dir only.
#
#  Runs in CI (the Installer job, beside the other *-verify.sh harnesses) and
#  locally via `make tool-pins-selftest`. Exit 0 = all properties hold.
# =============================================================================
set -uo pipefail

REPO="$(cd "$(dirname "$0")/../.." && pwd)"
GUARD="$REPO/scripts/check-tool-pins.sh"
[[ -f "$GUARD" ]] || { echo "tool-pins-verify: $GUARD not found" >&2; exit 2; }

# Derive the rows from the guard: every quoted "<VAR>:<module>[:<action>]" inside
# the TOOLS=( ... ) block. Zero rows means the parse broke — fail, don't pass.
rows=()
while IFS= read -r r; do rows+=("$r"); done < <(
  sed -n '/^TOOLS=(/,/^)/p' "$GUARD" | sed -nE 's/^[[:space:]]*"([A-Z_]+:[^"]+)".*/\1/p'
)
(( ${#rows[@]} > 0 )) || { echo "tool-pins-verify: parsed 0 TOOLS rows from the guard — refusing to report clean" >&2; exit 2; }

module_row=""   # first row with no action field  (shape 1)
action_row=""   # first row with an action field   (shape 2)
for r in "${rows[@]}"; do
  rest="${r#*:}"
  if [[ "$rest" == *:* ]]; then
    [[ -z "$action_row" ]] && action_row="$r"
  else
    [[ -z "$module_row" ]] && module_row="$r"
  fi
done
[[ -n "$module_row" && -n "$action_row" ]] || { echo "tool-pins-verify: need one module-only row and one action row in TOOLS to exercise both shapes" >&2; exit 2; }

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
fails=0

# fixture: a fresh tree with the real guard at scripts/, a Makefile declaring
# every row, and an empty workflows dir. Callers append workflow files.
fixture() {
  rm -rf "$tmp/repo"
  mkdir -p "$tmp/repo/scripts" "$tmp/repo/.github/workflows"
  cp "$GUARD" "$tmp/repo/scripts/check-tool-pins.sh"
  : > "$tmp/repo/Makefile"
  local r
  for r in "${rows[@]}"; do
    printf '%s ?= v9.9.9-fixture\n' "${r%%:*}" >> "$tmp/repo/Makefile"
  done
}

# run <expected-rc> <label> [<expected stderr substring>]
run() {
  local want="$1" label="$2" needle="${3:-}" out rc
  out="$(bash "$tmp/repo/scripts/check-tool-pins.sh" 2>&1)"
  rc=$?
  if (( rc != want )); then
    echo "FAIL  $label: exit $rc, wanted $want" >&2
    printf '%s\n' "$out" | sed 's/^/      /' >&2
    fails=$((fails + 1))
    return
  fi
  if [[ -n "$needle" ]] && ! grep -qF -- "$needle" <<<"$out"; then
    echo "FAIL  $label: exit $rc as wanted, but output does not name '$needle'" >&2
    printf '%s\n' "$out" | sed 's/^/      /' >&2
    fails=$((fails + 1))
    return
  fi
  echo "ok    $label (exit $rc)"
}

mod_var="${module_row%%:*}"
mod_path="${module_row#*:}"
act_var="${action_row%%:*}"
act_rest="${action_row#*:}"
act_action="${act_rest#*:}"

clean_workflow() {
  cat > "$tmp/repo/.github/workflows/build.yml" <<YML
name: fixture
jobs:
  lint:
    steps:
      - name: lint
        run: make lint
      - name: read pin
        id: pin
        run: echo "version=\$(make -s print-${act_var})" >> "\$GITHUB_OUTPUT"
      - name: tool via action
        uses: ${act_action}@0000000000000000000000000000000000000000 # v1.0.0
        with:
          version: \${{ steps.pin.outputs.version }}
YML
}

# 1. clean
fixture; clean_workflow
run 0 "clean tree passes"

# 2. shape 1: module@version restated
fixture; clean_workflow
cat >> "$tmp/repo/.github/workflows/build.yml" <<YML
      - name: restated
        run: go install ${mod_path}@v1.2.3
YML
run 1 "restated ${mod_path}@version reddens" "build.yml"

# 3. shape 2: literal version: on the tool's action
fixture; clean_workflow
cat > "$tmp/repo/.github/workflows/extra.yml" <<YML
name: fixture-2
jobs:
  x:
    steps:
      - uses: ${act_action}@0000000000000000000000000000000000000000 # v1.0.0
        with:
          version: v1.2.3
YML
run 1 "literal version: on ${act_action} reddens" "extra.yml"

# 4. precision: literal version: on an unrelated action is not an offender
fixture; clean_workflow
cat > "$tmp/repo/.github/workflows/extra.yml" <<YML
name: fixture-3
jobs:
  x:
    steps:
      - uses: someone-else/some-setup-action@0000000000000000000000000000000000000000 # v1.0.0
        with:
          version: 1.2.3
YML
run 0 "literal version: on an unrelated action passes"

# 5. fail closed: a declared tool missing from the Makefile
fixture; clean_workflow
sed -i.bak "/^${mod_var} /d" "$tmp/repo/Makefile" && rm -f "$tmp/repo/Makefile.bak"
run 2 "missing ${mod_var} in the Makefile fails closed" "${mod_var}"

# 6. fail closed: no workflow files
fixture
run 2 "empty .github/workflows fails closed" "refusing to report clean"

if (( fails )); then
  echo "tool-pins-verify: ${fails} propert(y/ies) lost" >&2
  exit 1
fi
echo "tool-pins-verify: all 6 properties hold"
