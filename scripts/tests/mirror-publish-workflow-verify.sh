#!/usr/bin/env bash
# =============================================================================
#  mirror-publish-workflow-verify.sh — pin the decisions
#  .github/workflows/mirror-publish.yml takes ITSELF, in step bodies no script
#  owns: what to publish (plan), that the release tag is fetched as data and
#  only at the expected commit (src), that a prerelease keeps the mirror's
#  default branch (keep), and that a publisher refusal reaches the step log
#  (target).
#
#  THE CODE UNDER TEST IS THE WORKFLOW. Each step's `run:` body is read out of
#  the YAML and executed under bash with the step's env set and `gh` shimmed —
#  the same text Actions runs, not a copy of it. The gh shim answers
#  `release view` and `api` from env; the tag fetch runs against a real bare
#  repository over file://.
#
#  Pinned, and the review finding each answers:
#    * a prerelease sets publish_tree=false and says why; a stable release sets
#      it true — and every step that pushes a tree is gated on that output, the
#      release step is not ("prerelease overwrites public default branch")
#    * no actions/checkout step takes a `ref:` — the tooling runs from this
#      workflow's own commit; the release tag is fetched into a detached
#      worktree and refused unless it resolves to the commit the plan step
#      expects ("checkout of untrusted code in a privileged context")
#    * a refusal from publish-mirror.sh is a `::error::` line in the step's
#      stdout, so no step captures the publisher through `$(...)` ("captured
#      output hides publish refusals")
#
#  FAILS CLOSED: an unreadable workflow, a missing step id, or PyYAML absent is
#  a named refusal (exit 2), never "nothing to check". The shape check is one
#  function run over the real workflow AND over mutated copies, each mutation
#  asserted to have changed the document before it is judged.
# =============================================================================
set -uo pipefail

SELF_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SELF_DIR/../.." && pwd)"
WF="$REPO_ROOT/.github/workflows/mirror-publish.yml"
[ -f "$WF" ] || { printf 'mirror-publish-workflow-verify: %s missing — refusing to report clean\n' "$WF" >&2; exit 2; }
command -v python3 >/dev/null 2>&1 || { echo 'mirror-publish-workflow-verify: python3 missing — refusing to report clean' >&2; exit 2; }
python3 -c 'import yaml' 2>/dev/null || { echo '[ERROR] PyYAML required (pip install pyyaml) — refusing to report clean' >&2; exit 2; }

PASS=0
FAIL=0
ok()  { printf '  ok   %s\n' "$1"; PASS=$((PASS+1)); }
bad() { printf '  FAIL %s\n' "$1"; FAIL=$((FAIL+1)); }

ROOT="$(mktemp -d "${TMPDIR:-/tmp}/mirror-publish-workflow-verify.XXXXXX")"
trap 'rm -rf "$ROOT"' EXIT
SHIM="$ROOT/shim"; WORK="$ROOT/work"; mkdir -p "$SHIM" "$WORK" "$ROOT/runner-temp"
# gh shim: `release view` prints GH_RELEASE_JSON (or fails with GH_RELEASE_RC);
# `api ... --jq .sha` prints GH_API_SHA (or fails with GH_API_RC). Every call is
# logged so a case can assert WHICH question the step asked.
cat >"$SHIM/gh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"${GH_LOG:?}"
case "${1:-} ${2:-}" in
  "release view")
    [ "${GH_RELEASE_RC:-0}" -eq 0 ] || { echo "release not found" >&2; exit "$GH_RELEASE_RC"; }
    printf '%s\n' "${GH_RELEASE_JSON:?}" ;;
  "api "*)
    [ "${GH_API_RC:-0}" -eq 0 ] || { echo "HTTP 409: Git Repository is empty" >&2; exit "$GH_API_RC"; }
    printf '%s\n' "${GH_API_SHA:?}" ;;
esac
exit 0
EOF
chmod +x "$SHIM/gh"
export GH_LOG="$ROOT/gh.log"
export GITHUB_OUTPUT="$ROOT/github-output"
export RUNNER_TEMP="$ROOT/runner-temp"
export GITHUB_REPOSITORY="example/source"
export GITHUB_WORKSPACE="$REPO_ROOT"
SHA_A=1111111111111111111111111111111111111111
SHA_B=2222222222222222222222222222222222222222

# reset_env — the plan step's job-level env, every field set (the body runs
# under set -u); a fresh GITHUB_OUTPUT and gh log per case.
reset_env() {
  export EVENT_NAME=workflow_run INPUT_TAG="" INPUT_DRY_RUN="" INPUT_MIRROR="" INPUT_STRICT=""
  export RUN_HEAD_BRANCH="" RUN_HEAD_SHA="" VAR_MIRROR="" VAR_STRICT=""
  export TAG="" EXPECT_SHA="" BRANCH="" REPO=""
  unset GH_RELEASE_JSON GH_RELEASE_RC GH_API_SHA GH_API_RC
  : >"$GITHUB_OUTPUT"; : >"$GH_LOG"
  rm -rf "$RUNNER_TEMP"; mkdir -p "$RUNNER_TEMP"
}

# step_run <workflow> <step id> — print that step's `run:` body; refuse when absent.
step_run() {
  python3 - "$1" "$2" <<'PY'
import sys
try:
    import yaml
except ImportError:
    sys.exit("[ERROR] PyYAML required (pip install pyyaml)")
path, want = sys.argv[1], sys.argv[2]
try:
    with open(path) as fh:
        doc = yaml.safe_load(fh)
except (OSError, yaml.YAMLError) as e:
    sys.exit("FAIL: cannot read or parse workflow %s: %s" % (path, e))
steps = ((doc.get("jobs") or {}).get("publish") or {}).get("steps") or []
for s in steps:
    if isinstance(s, dict) and s.get("id") == want:
        if "run" not in s:
            sys.exit("FAIL: step %r has no run: body" % want)
        sys.stdout.write(s["run"])
        sys.exit(0)
sys.exit("FAIL: no step with id %r in %s" % (want, path))
PY
}

# run_step <step id> [cwd] — execute the step body as Actions would: its own
# bash, the exported env, the gh shim first on PATH. Sets OUTPUT and RC.
run_step() {
  local body="$ROOT/step-$1.sh"
  if ! step_run "$WF" "$1" >"$body"; then OUTPUT="$(cat "$body")"; RC=2; return; fi
  local dir="${2:-$WORK}"
  OUTPUT="$(PATH="$SHIM:$PATH" bash -c "cd '$dir' && bash '$body'" 2>&1)"; RC=$?
}
out() { grep -E "^$1=" "$GITHUB_OUTPUT" | tail -1 | cut -d= -f2-; }
has() { [[ "$OUTPUT" == *"$1"* ]]; }
release_json() { printf '{"tagName":"%s","isDraft":false,"isPrerelease":%s}' "$1" "$2"; }

echo "== mirror-publish.yml step bodies =="

# ---- plan ---------------------------------------------------------------------------
reset_env; export RUN_HEAD_BRANCH=v1.2.3 RUN_HEAD_SHA="$SHA_A"; GH_RELEASE_JSON="$(release_json v1.2.3 false)"; export GH_RELEASE_JSON
run_step plan
if [ "$RC" -eq 0 ] && [ "$(out tag)" = v1.2.3 ] && [ "$(out dry_run)" = false ] && [ "$(out prerelease)" = false ] && [ "$(out publish_tree)" = true ] && [ "$(out expect_sha)" = "$SHA_A" ] \
   && grep -q '^release view v1.2.3 --repo example/source --json tagName,isDraft,isPrerelease$' "$GH_LOG" && ! has "::notice::"; then
  ok "plan: a stable release from workflow_run publishes tree and release, pinned to head_sha"
else bad "plan stable (rc=$RC): $OUTPUT / $(cat "$GITHUB_OUTPUT")"; fi

reset_env; export RUN_HEAD_BRANCH=v1.2.3-rc.1 RUN_HEAD_SHA="$SHA_A"; GH_RELEASE_JSON="$(release_json v1.2.3-rc.1 true)"; export GH_RELEASE_JSON
run_step plan
if [ "$RC" -eq 0 ] && [ "$(out prerelease)" = true ] && [ "$(out publish_tree)" = false ] && [ "$(out expect_sha)" = "$SHA_A" ] \
   && has "::notice::'v1.2.3-rc.1' is a prerelease: only its GitHub release is mirrored (marked prerelease). The mirror's default branch is not pushed"; then
  ok "plan: a prerelease mirrors only its release — publish_tree=false, and the log says why"
else bad "plan prerelease (rc=$RC): $OUTPUT / $(cat "$GITHUB_OUTPUT")"; fi

reset_env; export RUN_HEAD_BRANCH=develop RUN_HEAD_SHA="$SHA_A"
run_step plan
if [ "$RC" -eq 1 ] && has "::error::'develop' is not a release tag" && [ ! -s "$GITHUB_OUTPUT" ] && [ ! -s "$GH_LOG" ]; then ok "plan: a workflow_run whose head is a branch is refused before anything is read"; else bad "plan branch head (rc=$RC): $OUTPUT"; fi

reset_env; export RUN_HEAD_BRANCH=v1.2.3 RUN_HEAD_SHA="$SHA_A"; GH_RELEASE_JSON="$(release_json v9.9.9 false)"; export GH_RELEASE_JSON
run_step plan
if [ "$RC" -eq 1 ] && has "::error::release 'v1.2.3' reports tag_name 'v9.9.9' — the tag and the release disagree, refusing." && [ ! -s "$GITHUB_OUTPUT" ]; then ok "plan: a release whose tag_name is not the run's tag is refused"; else bad "plan tag mismatch (rc=$RC): $OUTPUT"; fi

reset_env; export RUN_HEAD_BRANCH=v1.2.3 RUN_HEAD_SHA=abc123; GH_RELEASE_JSON="$(release_json v1.2.3 false)"; export GH_RELEASE_JSON
run_step plan
if [ "$RC" -eq 1 ] && has "::error::cannot determine the commit release 'v1.2.3' was cut from (got 'abc123')" && [ ! -s "$GITHUB_OUTPUT" ]; then ok "plan: a workflow_run without a full head_sha to pin the tag to is refused"; else bad "plan bad head_sha (rc=$RC): $OUTPUT"; fi

reset_env; export EVENT_NAME=workflow_dispatch INPUT_TAG=v1.2.3 INPUT_DRY_RUN=true GH_API_SHA="$SHA_B"; GH_RELEASE_JSON="$(release_json v1.2.3 false)"; export GH_RELEASE_JSON
run_step plan; a="$RC"; dry="$(out dry_run)"; exp="$(out expect_sha)"; asked=0; grep -q '^api repos/example/source/commits/v1.2.3 --jq .sha$' "$GH_LOG" && asked=1
: >"$GITHUB_OUTPUT"; export GH_API_RC=1; run_step plan
if [ "$a" -eq 0 ] && [ "$dry" = true ] && [ "$exp" = "$SHA_B" ] && [ "$asked" -eq 1 ] && [ "$RC" -eq 1 ] && has "::error::cannot determine the commit release 'v1.2.3' was cut from (got '<empty>')"; then
  ok "plan: a dispatch takes the expected commit from the API, stays a dry run unless told 'false', and refuses when the API does not answer"
else bad "plan dispatch (a=$a dry=$dry exp=$exp asked=$asked rc=$RC): $OUTPUT"; fi

# ---- src: the release tag is data, fetched only at the expected commit -----------------
make_origin() { # a bare origin with one commit tagged v1.2.3 (annotated); WORK becomes its clone; prints the commit
  local seed="$ROOT/seed" bare="$ROOT/origin.git"
  rm -rf "$seed" "$bare" "$WORK"
  git init -q --bare "$bare"
  # The bare HEAD is pinned to `main` explicitly: with init.defaultBranch unset
  # (a fresh runner) it would point at a `master` that never receives a push,
  # the clone would have an unborn HEAD, and `rev-parse HEAD` would print the
  # literal word HEAD as the expected sha (measured on the first CI run).
  git -C "$bare" symbolic-ref HEAD refs/heads/main
  git init -q "$seed"
  printf 'readme\n' >"$seed/README.md"
  git -C "$seed" -c user.name=t -c user.email=t@example.invalid add README.md
  git -C "$seed" -c user.name=t -c user.email=t@example.invalid commit -q -m one
  git -C "$seed" -c user.name=t -c user.email=t@example.invalid tag -a v1.2.3 -m v1.2.3
  git -C "$seed" push -q "file://$bare" HEAD:refs/heads/main refs/tags/v1.2.3
  git clone -q "file://$bare" "$WORK" 2>/dev/null
  git -C "$seed" rev-parse --verify HEAD
}

reset_env; sha="$(make_origin)"; export TAG=v1.2.3 EXPECT_SHA="$sha"
run_step src
if [ "$RC" -eq 0 ] && [ "$(out dir)" = "$RUNNER_TEMP/release-src" ] && [ "$(git -C "$RUNNER_TEMP/release-src" rev-parse HEAD 2>/dev/null)" = "$sha" ] && [ -f "$RUNNER_TEMP/release-src/README.md" ] && has "release source: v1.2.3 at $sha (data only)"; then
  ok "src: the tag is fetched into a detached worktree outside the checkout, only at the expected commit"
else bad "src fetch (rc=$RC): $OUTPUT"; fi

reset_env; make_origin >/dev/null; export TAG=v1.2.3 EXPECT_SHA="$SHA_B"
run_step src
if [ "$RC" -eq 1 ] && has "::error::tag 'v1.2.3' resolves to " && has " but the release was cut at $SHA_B — the tag has moved or the run is not this release's; refusing." && [ ! -e "$RUNNER_TEMP/release-src" ] && [ ! -s "$GITHUB_OUTPUT" ]; then
  ok "src: a tag that does not resolve to the expected commit is refused and nothing is checked out"
else bad "src sha mismatch (rc=$RC): $OUTPUT"; fi

reset_env; make_origin >/dev/null; export TAG=v9.9.9 EXPECT_SHA="$SHA_A"
run_step src
if [ "$RC" -eq 1 ] && has "::error::could not fetch tag 'v9.9.9' from origin" && [ ! -e "$RUNNER_TEMP/release-src" ]; then ok "src: a tag origin does not have is refused"; else bad "src missing tag (rc=$RC): $OUTPUT"; fi

# ---- target / keep: refusals annotate, results go to GITHUB_OUTPUT -------------------
reset_env; export VAR_MIRROR="" INPUT_MIRROR=""
run_step target "$REPO_ROOT"
if [ "$RC" -eq 1 ] && has "::error::publish-mirror: REFUSED — no mirror repository is configured (MIRROR_REPO is unset)" && [ ! -s "$GITHUB_OUTPUT" ]; then ok "target: an unset MIRROR_REPO is refused with the ::error:: line IN THE STEP LOG, nothing captured"; else bad "target unset (rc=$RC): $OUTPUT"; fi

reset_env; export VAR_MIRROR=source-public INPUT_MIRROR=""
run_step target "$REPO_ROOT"
if [ "$RC" -eq 0 ] && [ "$(out repo)" = example/source-public ] && [ "$(out name)" = source-public ]; then ok "target: a configured mirror lands in GITHUB_OUTPUT as repo= and name="; else bad "target set (rc=$RC): $OUTPUT / $(cat "$GITHUB_OUTPUT")"; fi

reset_env; export REPO=example/source-public BRANCH=main TAG=v1.2.3-rc.1 GH_API_SHA="$SHA_B"
run_step keep; a="$RC"; got="$(out sha)"; asked=0; grep -q '^api repos/example/source-public/commits/main --jq .sha$' "$GH_LOG" && asked=1; o1="$OUTPUT"
: >"$GITHUB_OUTPUT"; export GH_API_RC=1; run_step keep
if [ "$a" -eq 0 ] && [ "$got" = "$SHA_B" ] && [ "$asked" -eq 1 ] && [[ "$o1" == *"prerelease v1.2.3-rc.1: default branch 'main' left untouched"* ]] \
   && [ "$RC" -eq 1 ] && has "::error::'v1.2.3-rc.1' is a prerelease and the mirror has no commit on 'main' to pin it to — a prerelease cannot be the first publish to an empty mirror" && [ ! -s "$GITHUB_OUTPUT" ]; then
  ok "keep: a prerelease is pinned to the mirror's default-branch head; an empty mirror is refused"
else bad "keep (a=$a got=$got asked=$asked rc=$RC): $o1 / $OUTPUT"; fi

# ---- shape: derived from the workflow, one implementation for real and mutated ---------
# shape <workflow.yml> — OK lines / one FAIL line. Every rule is derived from the
# steps themselves (which steps check out, which invoke the publisher), never
# from a list of step names held here.
shape() {
  OUTPUT="$(python3 - "$1" <<'PY' 2>&1
import re, sys
try:
    import yaml
except ImportError:
    sys.exit("[ERROR] PyYAML required (pip install pyyaml)")

path = sys.argv[1]


def fail(msg):
    print("FAIL: " + msg)
    sys.exit(1)


try:
    with open(path) as fh:
        doc = yaml.safe_load(fh)
except (OSError, yaml.YAMLError) as e:
    fail("cannot read or parse workflow %s: %s" % (path, e))
publish = ((doc or {}).get("jobs") or {}).get("publish")
if not isinstance(publish, dict):
    fail("no `publish` job in %s" % path)
steps = [s for s in (publish.get("steps") or []) if isinstance(s, dict)]
if not steps:
    fail("`publish` has no steps")

GATE = "steps.plan.outputs.publish_tree == 'true'"

checkouts = [s for s in steps if str(s.get("uses", "")).startswith("actions/checkout")]
if not checkouts:
    fail("no actions/checkout step — the tooling has to come from somewhere")
for s in checkouts:
    with_ = s.get("with") or {}
    if "ref" in with_:
        fail("checkout step %r takes a ref (%r): the tooling must come from this workflow's own commit, the release tag is data" % (s.get("name"), with_["ref"]))
print("OK: %d checkout step(s), none with a ref" % len(checkouts))

tree_pushes = [s for s in steps if re.search(r"publish-mirror\.sh\s+tree\b", str(s.get("run", "")))]
if not tree_pushes:
    fail("no step invokes `publish-mirror.sh tree` — nothing to gate")
for s in tree_pushes:
    if GATE not in str(s.get("if", "")):
        fail("step %r pushes a tree without `if: ... %s` — a prerelease would replace the mirror's branch" % (s.get("name"), GATE))
print("OK: %d tree push step(s), each gated on publish_tree" % len(tree_pushes))

releases = [s for s in steps if re.search(r"publish-mirror\.sh\s+\"?\$\{?args|publish-mirror\.sh\s+release\b", str(s.get("run", "")))]
if len(releases) != 1:
    fail("expected exactly one release step, found %d" % len(releases))
if "publish_tree" in str(releases[0].get("if", "")):
    fail("the release step is gated on publish_tree — a prerelease must still get its release")
print("OK: the release step is not gated on publish_tree")

captured = [s for s in steps if re.search(r"\$\(\s*bash\s+scripts/publish-mirror\.sh", str(s.get("run", "")))]
if captured:
    fail("step %r captures publish-mirror.sh through $(...) — a refusal's ::error:: line would never reach the log" % captured[0].get("name"))
print("OK: no step captures the publisher's output")

fetches = [s for s in steps if re.search(r"git fetch[^\n]*refs/tags/", str(s.get("run", "")))]
if len(fetches) != 1:
    fail("expected exactly one step fetching a tag, found %d" % len(fetches))
if "EXPECT_SHA" not in str(fetches[0].get("run", "")):
    fail("the tag fetch step does not compare against EXPECT_SHA")
print("OK: the one tag fetch compares against the expected commit")
PY
)"; RC=$?
}

# mutate <python statement over `steps`> — write a mutated copy of the real
# workflow and print its path. Applied to the PARSED document and asserted to
# have changed it, so an inert edit cannot pass as coverage.
mutate() {
  local out="$ROOT/mutated-$RANDOM.yml"
  python3 - "$WF" "$out" "$1" <<'PY' || return 1
import copy, sys
try:
    import yaml
except ImportError:
    sys.exit("[ERROR] PyYAML required (pip install pyyaml)")
src, dst, expr = sys.argv[1], sys.argv[2], sys.argv[3]
with open(src) as fh:
    doc = yaml.safe_load(fh)
before = copy.deepcopy(doc)
steps = doc["jobs"]["publish"]["steps"]
exec(expr, {"doc": doc, "steps": steps})
if doc == before:
    sys.exit("mutation did not change the document: " + expr)
with open(dst, "w") as fh:
    yaml.safe_dump(doc, fh, sort_keys=False)
print(dst)
PY
}

shape "$WF"
if [ "$RC" -eq 0 ] && has "OK: 1 checkout step(s), none with a ref" && has "OK: 1 tree push step(s), each gated on publish_tree" && has "OK: the release step is not gated on publish_tree" \
   && has "OK: no step captures the publisher's output" && has "OK: the one tag fetch compares against the expected commit"; then
  ok "shape: no checkout ref, tree push gated, release ungated, nothing captured, one pinned tag fetch"
else bad "shape real (rc=$RC): $OUTPUT"; fi

if m="$(mutate "[s for s in steps if str(s.get('uses','')).startswith('actions/checkout')][0]['with'] = {'ref': '\${{ steps.plan.outputs.tag }}'}")"; then
  shape "$m"
  if [ "$RC" -eq 1 ] && has "FAIL: checkout step " && has "takes a ref"; then ok "shape mutation: a checkout that takes a ref reddens"; else bad "shape mutation checkout ref (rc=$RC): $OUTPUT"; fi
else bad "shape mutation checkout ref: mutation did not apply: $m"; fi

if m="$(mutate "s = [s for s in steps if s.get('id') == 'push'][0]; s['if'] = \"steps.plan.outputs.dry_run != 'true'\"")"; then
  shape "$m"
  if [ "$RC" -eq 1 ] && has "FAIL: step " && has "pushes a tree without"; then ok "shape mutation: a tree push without the publish_tree gate reddens"; else bad "shape mutation ungated push (rc=$RC): $OUTPUT"; fi
else bad "shape mutation ungated push: mutation did not apply: $m"; fi

if m="$(mutate "s = [s for s in steps if s.get('id') == 'target'][0]; s['run'] = 'REPO=\"\$(bash scripts/publish-mirror.sh target --mirror x --source-repo a/b)\"\n'")"; then
  shape "$m"
  if [ "$RC" -eq 1 ] && has "FAIL: step " && has "captures publish-mirror.sh through"; then ok "shape mutation: capturing the publisher through \$(...) reddens"; else bad "shape mutation capture (rc=$RC): $OUTPUT"; fi
else bad "shape mutation capture: mutation did not apply: $m"; fi

if m="$(mutate "s = [s for s in steps if s.get('id') == 'src'][0]; s['run'] = s['run'].replace('EXPECT_SHA', 'IGNORED')")"; then
  shape "$m"
  if [ "$RC" -eq 1 ] && has "FAIL: the tag fetch step does not compare against EXPECT_SHA"; then ok "shape mutation: a tag fetch that skips the EXPECT_SHA comparison reddens"; else bad "shape mutation unpinned fetch (rc=$RC): $OUTPUT"; fi
else bad "shape mutation unpinned fetch: mutation did not apply: $m"; fi

echo
printf 'mirror-publish-workflow-verify: %d passed, %d failed\n' "$PASS" "$FAIL"
[ "$FAIL" -eq 0 ] && [ "$PASS" -ge 17 ]
