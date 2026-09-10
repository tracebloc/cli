#!/usr/bin/env bash
# =============================================================================
#  publish-mirror-verify.sh — pin the properties of scripts/publish-mirror.sh,
#  the publish half of the mirror pipeline.
#
#  `tree` is driven against REAL bare repositories over file:// (the clone /
#  replace / commit / plain-push path is the production one); `release` against
#  a recording `gh` shim, since a real release needs GitHub. `target` is pure.
#
#  Pinned: the refusals that keep a publish from landing in the wrong place (no
#  mirror named, the mirror IS the source, an unreachable remote), that the
#  mirror branch ends up holding EXACTLY the stage (removed files vanish,
#  history is appended, never rewritten), and that a mirrored release is never
#  overwritten.
# =============================================================================
set -uo pipefail

SELF_DIR="$(cd "$(dirname "$0")" && pwd)"
PUB="$SELF_DIR/../publish-mirror.sh"
[ -f "$PUB" ] || { printf 'publish-mirror-verify: %s missing — refusing to report clean\n' "$PUB" >&2; exit 2; }

PASS=0
FAIL=0
ok()  { printf '  ok   %s\n' "$1"; PASS=$((PASS+1)); }
bad() { printf '  FAIL %s\n' "$1"; FAIL=$((FAIL+1)); }

ROOT="$(mktemp -d "${TMPDIR:-/tmp}/publish-mirror-verify.XXXXXX")"
trap 'rm -rf "$ROOT"' EXIT
SHIM="$ROOT/shim"; mkdir -p "$SHIM"
cat >"$SHIM/gh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"${GH_LOG:?}"
if [ "${1:-}" = release ] && [ "${2:-}" = view ]; then
  printf '%s\n' "${GH_VIEW_ERR:-release not found}" >&2
  exit "${GH_VIEW_RC:-1}"
fi
exit 0
EOF
chmod +x "$SHIM/gh"
export GH_LOG="$ROOT/gh.log"

pub() { OUTPUT="$(bash "$PUB" "$@" 2>&1)"; RC=$?; }
has() { [[ "$OUTPUT" == *"$1"* ]]; }

echo "== publish-mirror.sh harness =="

# ---- target ------------------------------------------------------------------------
pub target --mirror '' --source-repo tracebloc/cli
if [ "$RC" -eq 1 ] && has "REFUSED — no mirror repository is configured (MIRROR_REPO is unset)"; then ok "target: no mirror configured is refused — there is no default"; else bad "target unset (rc=$RC): $OUTPUT"; fi

pub target --mirror cli --source-repo tracebloc/cli; a="$RC"; pub target --mirror CLI --source-repo tracebloc/cli
if [ "$a" -eq 1 ] && [ "$RC" -eq 1 ] && has "REFUSED — mirror 'tracebloc/CLI' is this repository"; then ok "target: the source repository itself is refused, case-insensitively"; else bad "target self (a=$a rc=$RC): $OUTPUT"; fi

pub target --mirror 'cli mirror' --source-repo tracebloc/cli; a="$RC"; o1="$OUTPUT"; pub target --mirror 'other/cli' --source-repo tracebloc/cli
if [ "$a" -eq 1 ] && [[ "$o1" == *"contains characters a repository name cannot"* ]] && [ "$RC" -eq 1 ] && has "must be a bare repository name"; then ok "target: bad characters and OWNER/NAME are refused"; else bad "target shape (a=$a rc=$RC): $o1 / $OUTPUT"; fi

pub target --mirror cli-public --source-repo tracebloc/cli
if [ "$RC" -eq 0 ] && [ "$OUTPUT" = "tracebloc/cli-public" ]; then ok "target: a valid mirror prints OWNER/NAME in the source's organisation"; else bad "target ok (rc=$RC): $OUTPUT"; fi

pub target --mirror cli-public
if [ "$RC" -eq 2 ] && has "COULD NOT TELL — target: --source-repo is required"; then ok "target: a missing --source-repo is could-not-tell"; else bad "target no source (rc=$RC): $OUTPUT"; fi

# --output: the workflow runs the publisher DIRECTLY and reads results from a
# file, so a refusal's ::error:: line is on stdout where Actions annotates it —
# captured through $(...) it would be swallowed by set -e (Bugbot on the PR).
OUTF="$ROOT/out"
pub target --mirror cli-public --source-repo tracebloc/cli --output "$OUTF"
if [ "$RC" -eq 0 ] && [ "$OUTPUT" = "tracebloc/cli-public" ] && [ "$(cat "$OUTF")" = $'repo=tracebloc/cli-public\nname=cli-public' ]; then ok "target: --output writes repo= and name=; stdout still names the mirror"; else bad "target output (rc=$RC): $OUTPUT / $(cat "$OUTF" 2>&1)"; fi
rm -f "$OUTF"
pub target --mirror '' --source-repo tracebloc/cli --output "$OUTF"; a="$RC"; o1="$OUTPUT"
pub target --mirror cli --source-repo tracebloc/cli --output "$OUTF"
if [ "$a" -eq 1 ] && [[ "$o1" == "::error::publish-mirror: REFUSED — no mirror repository is configured"* ]] && [ "$RC" -eq 1 ] && [ ! -e "$OUTF" ]; then ok "target: a refusal puts the ::error:: line on stdout and writes nothing to --output"; else bad "target refusal output (a=$a rc=$RC, out exists=$([ -e "$OUTF" ] && echo yes || echo no)): $o1"; fi

# ---- tree ---------------------------------------------------------------------------
STAGE="$ROOT/stage"; mkdir -p "$STAGE/docs"
printf 'readme\n' >"$STAGE/README.md"; printf 'license\n' >"$STAGE/LICENSE"; printf 'doc\n' >"$STAGE/docs/a.md"
BARE="$ROOT/mirror.git"; git init -q --bare "$BARE"
tree() { pub tree --stage "$STAGE" --repo tracebloc/mirror --branch main --message "Publish v1.0.0" --remote "file://$BARE" "$@"; }
mirror_files() { git -C "$BARE" ls-tree -r --name-only main | sort | paste -sd' ' -; }

tree
if [ "$RC" -eq 0 ] && [[ "$OUTPUT" == pushed\ [0-9a-f]* ]] && [ "$(mirror_files)" = "LICENSE README.md docs/a.md" ] && [ "$(git -C "$BARE" rev-list --count main)" -eq 1 ]; then
  ok "tree: the first publish starts the branch; the mirror holds exactly the stage"
else bad "tree first (rc=$RC, files='$(mirror_files)'): $OUTPUT"; fi
first="$(git -C "$BARE" rev-parse main)"

tree
if [ "$RC" -eq 0 ] && [[ "$OUTPUT" == unchanged\ [0-9a-f]* ]] && [ "$(git -C "$BARE" rev-list --count main)" -eq 1 ]; then ok "tree: an identical stage is a no-op, reported as unchanged"; else bad "tree unchanged (rc=$RC): $OUTPUT"; fi

rm "$STAGE/docs/a.md"; printf 'new\n' >"$STAGE/CHANGES.md"; tree
if [ "$RC" -eq 0 ] && [ "$(mirror_files)" = "CHANGES.md LICENSE README.md" ] && [ "$(git -C "$BARE" rev-list --count main)" -eq 2 ] && [ "$(git -C "$BARE" rev-parse main^)" = "$first" ]; then
  ok "tree: a later publish replaces the content — removed files vanish, history is appended"
else bad "tree replace (rc=$RC, files='$(mirror_files)'): $OUTPUT"; fi

pub tree --stage "$STAGE" --repo tracebloc/mirror --branch main --message m --remote "file://$ROOT/no-such.git"
if [ "$RC" -eq 2 ] && has "COULD NOT TELL — tree: the mirror remote did not answer"; then ok "tree: an unreachable remote is could-not-tell, not a fresh start"; else bad "tree unreachable (rc=$RC): $OUTPUT"; fi

EMPTY="$ROOT/empty"; mkdir -p "$EMPTY"
pub tree --stage "$EMPTY" --repo tracebloc/mirror --branch main --message m --remote "file://$BARE"; a="$RC"; o1="$OUTPUT"
mkdir -p "$STAGE/.git"; tree; rm -r "$STAGE/.git"
if [ "$a" -eq 2 ] && [[ "$o1" == *"holds no files"* ]] && [ "$RC" -eq 2 ] && has "contains a .git entry"; then ok "tree: an empty stage, or one that is a checkout, is could-not-tell"; else bad "tree stage shape (a=$a rc=$RC): $o1 / $OUTPUT"; fi

if ! grep -qE -- '--force|\+refs/|-f[[:space:]]' "$PUB"; then ok "tree: the script never forces a push"; else bad "a force-push spelling is present in $PUB"; fi

BARE2="$ROOT/mirror2.git"; git init -q --bare "$BARE2"
tree2() { pub tree --stage "$STAGE" --repo tracebloc/mirror --branch main --message "Publish v1.0.0" --remote "file://$BARE2" "$@"; }
rm -f "$OUTF"; tree2 --output "$OUTF"; a="$RC"; l1="$(sed -n 1p "$OUTF" 2>/dev/null)"; l2="$(sed -n 2p "$OUTF" 2>/dev/null)"
rm -f "$OUTF"; tree2 --output "$OUTF"; b="$RC"; m1="$(sed -n 1p "$OUTF" 2>/dev/null)"; m2="$(sed -n 2p "$OUTF" 2>/dev/null)"
head2="$(git -C "$BARE2" rev-parse main)"
if [ "$a" -eq 0 ] && [ "$l1" = "result=pushed" ] && [ "$l2" = "sha=$head2" ] && [ "$b" -eq 0 ] && [ "$m1" = "result=unchanged" ] && [ "$m2" = "sha=$head2" ]; then
  ok "tree: --output writes result= and sha= (pushed, then unchanged)"
else bad "tree output (a=$a b=$b): '$l1' '$l2' / '$m1' '$m2' head=$head2"; fi

rm -f "$OUTF"
pub tree --stage "$STAGE" --repo tracebloc/mirror --branch main --message m --remote "file://$ROOT/no-such.git" --output "$OUTF"
if [ "$RC" -eq 2 ] && [[ "$OUTPUT" == "::error::publish-mirror: COULD NOT TELL — tree: the mirror remote did not answer"* ]] && [ ! -e "$OUTF" ]; then ok "tree: a refusal annotates stdout and writes nothing to --output"; else bad "tree refusal output (rc=$RC, out exists=$([ -e "$OUTF" ] && echo yes || echo no)): $OUTPUT"; fi

tree2 --output "$ROOT/no-such-dir/out"
if [ "$RC" -eq 2 ] && has "COULD NOT TELL — could not write results to"; then ok "tree: an unwritable --output is could-not-tell — a result the caller never receives is not a publish"; else bad "tree unwritable output (rc=$RC): $OUTPUT"; fi

# ---- release ------------------------------------------------------------------------
NOTES="$ROOT/notes.md"; printf 'Release notes\n' >"$NOTES"
SHA=0123456789abcdef0123456789abcdef01234567
release() { : >"$GH_LOG"; PATH="$SHIM:$PATH" pub release --tag v1.0.0 --repo tracebloc/mirror --target "$SHA" --assets "$STAGE" --notes "$NOTES" "$@"; }

release
create="$(grep '^release create' "$GH_LOG")"
if [ "$RC" -eq 0 ] && [ "$OUTPUT" = "released v1.0.0 on tracebloc/mirror at $SHA with 3 asset(s)" ] && grep -q '^release view v1.0.0 --repo tracebloc/mirror$' "$GH_LOG" \
   && [ "$create" = "release create v1.0.0 --repo tracebloc/mirror --target $SHA --title v1.0.0 --notes-file $NOTES $STAGE/CHANGES.md $STAGE/LICENSE $STAGE/README.md" ]; then
  ok "release: creates the tag at the target with every asset, fixed notes, no --prerelease"
else bad "release create (rc=$RC): $OUTPUT / $create"; fi

release --prerelease
if [ "$RC" -eq 0 ] && grep -q '^release create .* --prerelease ' "$GH_LOG"; then ok "release: --prerelease is passed through"; else bad "release prerelease (rc=$RC): $OUTPUT"; fi

GH_VIEW_RC=0 release
if [ "$RC" -eq 1 ] && has "REFUSED — release: 'v1.0.0' already exists on 'tracebloc/mirror'" && ! grep -q '^release create' "$GH_LOG"; then ok "release: an existing tag on the mirror is refused, never overwritten"; else bad "release exists (rc=$RC): $OUTPUT"; fi

GH_VIEW_RC=1 GH_VIEW_ERR='HTTP 401: Bad credentials' release
if [ "$RC" -eq 2 ] && has "COULD NOT TELL — release: could not read releases of 'tracebloc/mirror'" && ! grep -q '^release create' "$GH_LOG"; then ok "release: a view failure that is not 'not found' is could-not-tell"; else bad "release view error (rc=$RC): $OUTPUT"; fi

: >"$GH_LOG"
PATH="$SHIM:$PATH" pub release --tag main --repo tracebloc/mirror --target "$SHA" --assets "$STAGE" --notes "$NOTES"; a="$RC"; o1="$OUTPUT"
PATH="$SHIM:$PATH" pub release --tag v1.0.0 --repo tracebloc/mirror --target abc123 --assets "$STAGE" --notes "$NOTES"; b="$RC"; o2="$OUTPUT"
: >"$ROOT/empty.md"
PATH="$SHIM:$PATH" pub release --tag v1.0.0 --repo tracebloc/mirror --target "$SHA" --assets "$STAGE" --notes "$ROOT/empty.md"; c="$RC"; o3="$OUTPUT"
PATH="$SHIM:$PATH" pub release --tag v1.0.0 --repo tracebloc/mirror --target "$SHA" --assets "$EMPTY" --notes "$NOTES"; d="$RC"; o4="$OUTPUT"
if [ "$a" -eq 1 ] && [[ "$o1" == *"'main' is not a release tag"* ]] && [ "$b" -eq 2 ] && [[ "$o2" == *"is not a full commit sha"* ]] \
   && [ "$c" -eq 2 ] && [[ "$o3" == *"is missing or empty"* ]] && [ "$d" -eq 2 ] && [[ "$o4" == *"holds no files"* ]] && ! grep -q '^release create' "$GH_LOG"; then
  ok "release: a malformed tag is refused; a short sha, empty notes or no assets are could-not-tell; nothing was created"
else bad "release inputs (a=$a b=$b c=$c d=$d): $o1 / $o2 / $o3 / $o4"; fi

echo
printf 'publish-mirror-verify: %d passed, %d failed\n' "$PASS" "$FAIL"
[ "$FAIL" -eq 0 ] && [ "$PASS" -ge 19 ]
