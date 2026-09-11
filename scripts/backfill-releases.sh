#!/usr/bin/env bash
# =============================================================================
#  backfill-releases.sh — one-shot backfill of this repository's HISTORICAL
#  releases onto the public deliverable mirror.
#
#  .github/workflows/mirror-publish.yml publishes each NEW release to the
#  mirror as it is cut. Releases that existed before the mirror did are carried
#  over once, by this script, run by a human. Idempotent: a re-run over an
#  already-backfilled mirror reads everything and writes nothing.
#
#  THE DECISION (taken once, stated here so nobody re-derives it):
#    * every published release gets its tag and its GitHub release on the
#      mirror, with its TEXT assets — install.sh, install.ps1, SHA256SUMS and
#      any other asset SHA256SUMS does not list;
#    * BINARIES (the files SHA256SUMS lists) and their cosign companions
#      (<binary>.sig, <binary>.cert) are carried only for the newest
#      BINARY_KEEP releases (default 10). Older pinned binary URLs 404 on the
#      mirror; the documented answer is "re-run the installer";
#    * prereleases and drafts are skipped unless --include-prerelease.
#
#  HOW MIRROR TAGS ARE ANCHORED: the mirror holds a README, not the source, so
#  no tag can point at the commit a release was built from. Each tag is created
#  as an ANNOTATED tag on the mirror's default-branch head. The annotation
#  carries the ORIGINAL date (the source tag's tagger date when the source tag
#  is annotated, the release's created_at otherwise) and the original message,
#  and says in plain words that the tag is a RELEASE MARKER on the mirror, not
#  a source snapshot. A tag already on the mirror is accepted only if it points
#  at a commit the mirror has; a dangling one is refused, never repointed.
#
#  RELEASE NOTES: --notes fixed (DEFAULT) writes the same fixed text the
#  workflow writes for new releases. Historical source bodies are GitHub's
#  generated ones — merged pull requests by title — and nearly every one
#  carries strings the guard's report tier counts, which the public mirror
#  should not repeat; so the source body is an explicit opt-in: --notes source
#  carries it, and it then goes through the guard's forbidden-string scan like
#  any text asset (a hit refuses the release and names the tier). Either way a
#  footer names the original publish date (GitHub does not let a created
#  release carry a past date, so the footer and the tag annotation are where
#  the date survives).
#
#  WHAT IS REUSED: scripts/publish-mirror.sh `target` decides the mirror name
#  (unset, malformed, or equal to the source is refused there — one rule, one
#  place); scripts/publish-guard.sh scans every text asset and the notes with
#  the repo's own .publish-forbidden (and the private needles from
#  BACKFILL_EXTRA_FORBIDDEN, when given) plus gitleaks, before anything is
#  uploaded. Binaries are opaque to a string scan by design; each one is
#  verified against the SOURCE release's SHA256SUMS before upload and refused
#  on a mismatch, naming the asset. Companions (.sig/.cert) are downloaded
#  with their binary under --apply and scanned like text assets then.
#
#  Usage:
#    MIRROR_REPO=NAME scripts/backfill-releases.sh [--dry-run | --apply]
#        [--from-tag TAG | --only-tag TAG] [--include-prerelease]
#        [--notes fixed|source] [--strict]
#
#  Environment:
#    SOURCE_REPO     OWNER/REPO to read releases from (default: the repository
#                    `gh repo view` reports for the current checkout)
#    MIRROR_REPO     bare repository name in the source's organisation; REQUIRED
#    BINARY_KEEP     how many of the newest releases carry binaries (default 10)
#    BACKFILL_EXTRA_FORBIDDEN
#                    file of extra refuse-tier needles for the guard (the
#                    private list the workflow gets from a secret); optional
#    PUBLISH_MIRROR_GIT_NAME / PUBLISH_MIRROR_GIT_EMAIL
#                    tagger identity on the mirror tags (default github-actions[bot])
#    GH_TOKEN        gh's; must be able to write the mirror under --apply
#
#  Modes:
#    --dry-run       DEFAULT. Every read runs, the text assets and notes of
#                    each release that would change are downloaded and put
#                    through the guard, the plan is printed, nothing is written.
#    --apply         performs the writes: tag, release, uploads.
#    --from-tag TAG  resume: process TAG and every release newer than it
#                    (releases are processed oldest to newest).
#    --only-tag TAG  process TAG alone.
#                    Both keep the BINARY_KEEP decision of the FULL list, so a
#                    partial run carries the same binaries a full run would.
#
#  Exit 0 done (every release created or already present); 1 at least one
#  release was REFUSED (the table says which and why; the rest went ahead);
#  2 COULD NOT TELL — a read that did not complete, a tool missing, an input
#  malformed. "Cannot tell" stops the run at once and never writes.
# =============================================================================
set -euo pipefail

SCRIPTS_DIR="${BACKFILL_SCRIPTS_DIR:-$(cd "$(dirname "$0")" && pwd)}"
REPO_ROOT="$(cd "$SCRIPTS_DIR/.." && pwd)"
PUBLISH_MIRROR="$SCRIPTS_DIR/publish-mirror.sh"
PUBLISH_GUARD="$SCRIPTS_DIR/publish-guard.sh"
FORBIDDEN_LIST="$REPO_ROOT/.publish-forbidden"

# die2 REASON — could-not-tell: the reason, then exit 2. The reason goes to
# STDERR on purpose: most callers (jq_of above all) sit inside "$(...)", where
# stdout is the variable being assigned — a stdout reason would be captured
# into it and never seen, leaving a bare exit 2. On stderr it reaches the
# operator either way, and the substitution's status 2 aborts the assignment
# under `set -e`. That abort is the ONLY thing ending the parent, so never
# put a die2-capable "$(...)" inside an && / || list or a `[ ]` test, where
# `set -e` is suspended — hoist it into its own assignment first (the
# per-release block does).
die2() { echo "::error::backfill-releases: COULD NOT TELL — $1 (nothing more is written)" >&2; exit 2; }   # mutation-anchor: die2-stderr
note() { echo "backfill-releases: $1"; }

# ---- arguments -----------------------------------------------------------------
APPLY=0; FROM_TAG=""; ONLY_TAG=""; INCLUDE_PRE=0; NOTES_MODE=fixed; STRICT=0   # mutation-anchor: notes-default-fixed
while [ "$#" -gt 0 ]; do
  case "$1" in
    --dry-run)            APPLY=0; shift ;;
    --apply)              APPLY=1; shift ;;
    --from-tag)           FROM_TAG="${2:-}"; shift 2 ;;
    --only-tag)           ONLY_TAG="${2:-}"; shift 2 ;;
    --include-prerelease) INCLUDE_PRE=1; shift ;;
    --notes)              NOTES_MODE="${2:-}"; shift 2 ;;
    --strict)             STRICT=1; shift ;;
    -h|--help)            sed -n '2,/^# ====/p' "$0" | sed 's/^# \{0,2\}//'; exit 0 ;;
    *) die2 "unknown argument '$1' (see --help)" ;;
  esac
done
[ -z "$FROM_TAG" ] || [ -z "$ONLY_TAG" ] || die2 "--from-tag and --only-tag exclude each other"
case "$NOTES_MODE" in fixed|source) ;; *) die2 "--notes must be 'fixed' or 'source', not '$NOTES_MODE'" ;; esac
BINARY_KEEP="${BINARY_KEEP:-10}"
[[ "$BINARY_KEEP" =~ ^[0-9]+$ ]] || die2 "BINARY_KEEP '$BINARY_KEEP' is not a non-negative integer"
TAG_RE='^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$'
COMPANION_RE='\.(sig|cert)$'   # <binary>.sig / <binary>.cert travel with their binary
[ -z "$FROM_TAG" ] || [[ "$FROM_TAG" =~ $TAG_RE ]] || die2 "--from-tag '$FROM_TAG' is not a release tag"
[ -z "$ONLY_TAG" ] || [[ "$ONLY_TAG" =~ $TAG_RE ]] || die2 "--only-tag '$ONLY_TAG' is not a release tag"

# ---- tools -----------------------------------------------------------------------
for t in gh jq git awk; do command -v "$t" >/dev/null 2>&1 || die2 "'$t' is not on PATH"; done
command -v "${PUBLISH_GUARD_GITLEAKS:-gitleaks}" >/dev/null 2>&1 || die2 "'${PUBLISH_GUARD_GITLEAKS:-gitleaks}' is not on PATH — the guard treats a missing scanner as could-not-tell, so nothing could be uploaded"
[ -f "$PUBLISH_MIRROR" ] || die2 "$PUBLISH_MIRROR is missing"
[ -f "$PUBLISH_GUARD" ]  || die2 "$PUBLISH_GUARD is missing"
[ -r "$FORBIDDEN_LIST" ] || die2 "$FORBIDDEN_LIST is missing or unreadable — the scan has no rules"
if [ -n "${BACKFILL_EXTRA_FORBIDDEN:-}" ]; then
  [ -s "$BACKFILL_EXTRA_FORBIDDEN" ] || die2 "BACKFILL_EXTRA_FORBIDDEN '$BACKFILL_EXTRA_FORBIDDEN' is missing or empty"
fi
if command -v sha256sum >/dev/null 2>&1; then
  sha256_of() { sha256sum "$1" | cut -d' ' -f1; }
elif command -v shasum >/dev/null 2>&1; then
  sha256_of() { shasum -a 256 "$1" | cut -d' ' -f1; }
else
  die2 "neither sha256sum nor shasum is on PATH"
fi

TMP="$(mktemp -d "${TMPDIR:-/tmp}/backfill-releases.XXXXXX")" && [ -d "$TMP" ] || die2 "could not create a scratch directory"
trap 'rm -rf "$TMP"' EXIT

# ---- gh wrappers -----------------------------------------------------------------
# gh_read OUTFILE ARGS... — a READ that must complete. Any failure is
# could-not-tell: an unreadable list is never an empty list.
gh_read() {
  local out="$1"; shift
  if ! gh "$@" >"$out" 2>"$TMP/gh.err"; then
    die2 "gh $* failed: $(tr '\n' ' ' <"$TMP/gh.err")"
  fi
}
# gh_read_maybe OUTFILE ARGS... — a READ where "not there" is an answer.
# Returns 0 on success, 1 on a clear HTTP 404 or the HTTP 409 GitHub gives for
# a commit read on an EMPTY repository; anything else is could-not-tell.
gh_read_maybe() {
  local out="$1"; shift
  if gh "$@" >"$out" 2>"$TMP/gh.err"; then return 0; fi
  grep -qE 'HTTP 404|HTTP 409' "$TMP/gh.err" && return 1
  die2 "gh $* failed: $(tr '\n' ' ' <"$TMP/gh.err")"
}
# gh_write OUTFILE ARGS... — a WRITE (apply only). A failed write is fatal:
# the mirror may now be half-changed and the human decides, with the table.
gh_write() {
  local out="$1"; shift
  [ "$APPLY" -eq 1 ] || die2 "internal: gh_write reached in dry-run (gh $*)"
  if ! gh "$@" >"$out" 2>"$TMP/gh.err"; then
    die2 "gh $* failed: $(tr '\n' ' ' <"$TMP/gh.err") — re-run to resume; completed steps are skipped"
  fi
}
# jq_of FILE FILTER — jq over a file that MUST parse; a malformed answer is
# could-not-tell, not an empty one.
jq_of() { jq -r "$2" "$1" 2>"$TMP/jq.err" || die2 "could not parse $1 with '$2': $(tr '\n' ' ' <"$TMP/jq.err")"; }

# ---- source and mirror ---------------------------------------------------------------
SRC="${SOURCE_REPO:-}"
if [ -z "$SRC" ]; then
  gh_read "$TMP/self.json" repo view --json nameWithOwner
  SRC="$(jq_of "$TMP/self.json" '.nameWithOwner')"
fi
[[ "$SRC" =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]] || die2 "source repository '$SRC' is not OWNER/REPO"

# The mirror name is decided by publish-mirror.sh `target`, so an unset or
# malformed name and a mirror equal to the source are refused by the same rule
# the workflow applies. Run directly, output captured to a file: its
# ::error:: line is the reason, and its exit status is ours.
rc=0
bash "$PUBLISH_MIRROR" target --mirror "${MIRROR_REPO:-}" --source-repo "$SRC" >"$TMP/target.out" 2>&1 || rc=$?
if [ "$rc" -ne 0 ]; then cat "$TMP/target.out"; echo "::error::backfill-releases: REFUSED — the mirror target was refused above"; exit "$rc"; fi
MIRROR="$(tail -1 "$TMP/target.out")"

gh_read "$TMP/mirror.json" api "repos/$MIRROR"
MIRROR_FULL="$(jq_of "$TMP/mirror.json" '.full_name')"
MIRROR_BRANCH="$(jq_of "$TMP/mirror.json" '.default_branch')"
if [ "$(printf '%s' "$MIRROR_FULL" | tr '[:upper:]' '[:lower:]')" = "$(printf '%s' "$SRC" | tr '[:upper:]' '[:lower:]')" ]; then
  echo "::error::backfill-releases: REFUSED — mirror '$MIRROR_FULL' resolves to the source repository"; exit 1
fi
[ -n "$MIRROR_BRANCH" ] && [ "$MIRROR_BRANCH" != null ] || die2 "mirror '$MIRROR' reports no default branch"
# Every mirror tag is anchored here. An empty mirror has no head: that is a
# refusal with instructions, not a guess.
if ! gh_read_maybe "$TMP/head.json" api "repos/$MIRROR/commits/$MIRROR_BRANCH"; then
  echo "::error::backfill-releases: REFUSED — mirror '$MIRROR' has no commit on '$MIRROR_BRANCH' to anchor tags to; publish the README first"; exit 1
fi
MIRROR_HEAD="$(jq_of "$TMP/head.json" '.sha')"
[[ "$MIRROR_HEAD" =~ ^[0-9a-f]{40}$ ]] || die2 "mirror head '$MIRROR_HEAD' is not a commit sha"

# ---- the release list, derived from the API -----------------------------------------
# --paginate concatenates one JSON array per page; `jq -s add` joins them.
gh_read "$TMP/src-pages.json" api --paginate "repos/$SRC/releases"   # mutation-anchor: releases-read-fail-closed
jq -s 'add // []' "$TMP/src-pages.json" >"$TMP/src-releases.json" 2>"$TMP/jq.err" || die2 "release list of '$SRC' did not parse: $(tr '\n' ' ' <"$TMP/jq.err")"
# Newest first by created_at; drafts never; prereleases only when asked.
FILTER='[ .[] | select(.draft == false) | select($pre == 1 or .prerelease == false) ] | sort_by(.created_at) | reverse'   # mutation-anchor: prerelease-filter
jq --argjson pre "$INCLUDE_PRE" "$FILTER" "$TMP/src-releases.json" >"$TMP/releases.json" 2>"$TMP/jq.err" || die2 "could not filter the release list: $(tr '\n' ' ' <"$TMP/jq.err")"
N_ALL="$(jq_of "$TMP/releases.json" 'length')"
[ "$N_ALL" -gt 0 ] || die2 "'$SRC' has no published release matching the filter — nothing to backfill is not a clean run"
jq -r '.[].tag_name' "$TMP/releases.json" >"$TMP/tags-newest-first.txt"
while IFS= read -r t; do [[ "$t" =~ $TAG_RE ]] || die2 "release tag '$t' on '$SRC' is not a release tag"; done <"$TMP/tags-newest-first.txt"
# The newest BINARY_KEEP of the FULL filtered list carry binaries — decided
# before --from-tag/--only-tag narrow the run, so a partial run agrees with a
# full one.
head -n "$BINARY_KEEP" "$TMP/tags-newest-first.txt" >"$TMP/tags-with-binaries.txt"   # mutation-anchor: binary-keep
NEWEST_STABLE="$(jq_of "$TMP/releases.json" '[ .[] | select(.prerelease == false) ] | .[0].tag_name // ""')"
carries_binaries() { grep -qxF -- "$1" "$TMP/tags-with-binaries.txt"; }

# Processing order: oldest to newest, so the mirror's release order reads like
# the source's and the newest stable release is created last.
sed -n '1!G;h;$p' "$TMP/tags-newest-first.txt" >"$TMP/tags-ordered.txt"
if [ -n "$ONLY_TAG" ]; then
  grep -qxF -- "$ONLY_TAG" "$TMP/tags-ordered.txt" || die2 "--only-tag '$ONLY_TAG' is not a release of '$SRC' matching the filter"
  printf '%s\n' "$ONLY_TAG" >"$TMP/tags-run.txt"
elif [ -n "$FROM_TAG" ]; then
  grep -qxF -- "$FROM_TAG" "$TMP/tags-ordered.txt" || die2 "--from-tag '$FROM_TAG' is not a release of '$SRC' matching the filter"
  awk -v t="$FROM_TAG" 'f || $0 == t { f = 1; print }' "$TMP/tags-ordered.txt" >"$TMP/tags-run.txt"
else
  cp "$TMP/tags-ordered.txt" "$TMP/tags-run.txt"
fi
N_RUN="$(grep -c . "$TMP/tags-run.txt" || true)"

# ---- what the mirror already has ----------------------------------------------------
gh_read "$TMP/mirror-pages.json" api --paginate "repos/$MIRROR/releases"
jq -s 'add // []' "$TMP/mirror-pages.json" >"$TMP/mirror-releases.json" 2>"$TMP/jq.err" || die2 "release list of '$MIRROR' did not parse"
gh_read "$TMP/mirror-tag-pages.json" api --paginate "repos/$MIRROR/git/matching-refs/tags/"
jq -s 'add // []' "$TMP/mirror-tag-pages.json" >"$TMP/mirror-tags.json" 2>"$TMP/jq.err" || die2 "tag list of '$MIRROR' did not parse"

MODE=dry-run; [ "$APPLY" -eq 1 ] && MODE=apply
note "source $SRC → mirror $MIRROR ($MIRROR_BRANCH @ ${MIRROR_HEAD:0:12}); $N_ALL release(s) match the filter, $N_RUN in this run; binaries for the newest $BINARY_KEEP; notes=$NOTES_MODE; mode=$MODE"
[ "$STRICT" -eq 0 ] || note "--strict: the guard's [strings-report] tier refuses"

# ---- the guard's scratch source tree -------------------------------------------------
# publish-guard.sh stages a tree from a git checkout by design. The backfill has
# no tree to publish, so it hands the guard a one-file scratch checkout and puts
# what matters — the text assets and the notes — in --assets, where guards
# 2–4 (forbidden paths, forbidden strings, gitleaks) read them.
SCRATCH="$TMP/scratch-src"; mkdir -p "$SCRATCH"
git -C "$SCRATCH" init -q || die2 "could not init the guard's scratch checkout"
printf 'backfill scratch tree\n' >"$SCRATCH/README.md"
# This runs on a human's machine: a global commit.gpgsign=true would try to
# sign the scratch commit as backfill@localhost, fail, and end the run before
# a single release is planned. The scratch commit is never published — unsigned.
git -C "$SCRATCH" add README.md && git -C "$SCRATCH" -c user.name=backfill -c user.email=backfill@localhost -c commit.gpgsign=false commit -q -m scratch || die2 "could not commit the guard's scratch checkout"   # mutation-anchor: scratch-commit-unsigned
printf 'README.md\n' >"$TMP/include.txt"

# run_guard ASSETS_DIR OUT_DIR — the guard over ASSETS_DIR. Returns the guard's
# exit status (0 clean, 1 refused, 2 could not tell); its output is in OUT_DIR.log.
run_guard() {
  local assets="$1" out="$2" rc=0
  local -a args=(--source "$SCRATCH" --include "$TMP/include.txt" --forbidden "$FORBIDDEN_LIST" --out "$out" --assets "$assets")
  [ -z "${BACKFILL_EXTRA_FORBIDDEN:-}" ] || args+=(--extra-forbidden "$BACKFILL_EXTRA_FORBIDDEN")
  [ "$STRICT" -eq 0 ] || args+=(--strict)
  bash "$PUBLISH_GUARD" "${args[@]}" >"$out.log" 2>&1 || rc=$?
  return "$rc"
}

# ---- per-release helpers -------------------------------------------------------------------
# Decision files: one line per asset, `name<TAB>action<TAB>sha`, action one of
# upload | skip | compare | refuse. `compare` means the mirror has the asset and
# the source's digest is unknown, so the download is hashed before deciding.
count_action() { awk -F'\t' -v a="$2" '$2 == a { n++ } END { print n + 0 }' "$1"; }
in_sums() { awk -F'\t' -v n="$1" '$2 == n { f = 1 } END { exit !f }' "$R/sums.txt"; }
sum_of() { awk -F'\t' -v n="$1" '$2 == n { print $1; exit }' "$R/sums.txt"; }
mirror_digest() { # NAME → sha256 hex; "" when absent; "?" when present without a digest
  [ "$MREL_PRESENT" -eq 1 ] || return 0
  awk -F'\t' -v n="$1" '$1 == n { print ($2 == "" ? "?" : $2); exit }' "$R/mirror-assets.tsv"
}
# decide LIST NAME EXPECTED_SHA — EXPECTED_SHA may be "" (unknown).
decide() {
  local list="$1" name="$2" expected="$3" have
  have="$(mirror_digest "$name")"
  if [ "$have" = "?" ]; then die2 "$TAG: mirror asset '$name' has no digest — cannot tell whether it matches"; fi
  if [ -z "$have" ]; then printf '%s\tupload\t%s\n' "$name" "$expected" >>"$list"; return 0; fi
  if [ -z "$expected" ]; then printf '%s\tcompare\t%s\n' "$name" "$have" >>"$list"; return 0; fi
  if [ "$have" = "$expected" ]; then printf '%s\tskip\t%s\n' "$name" "$expected" >>"$list"; return 0; fi   # mutation-anchor: idempotent-skip
  [ -n "$REFUSAL" ] || REFUSAL="asset '$name' is on the mirror with SHA256 $have but the source release says $expected — a published asset is never replaced"
  printf '%s\trefuse\t%s\n' "$name" "$expected" >>"$list"
}
# resolve_compares LIST DIR — hash each `compare` download; equal → skip,
# different → REFUSAL (a published asset is never replaced).
resolve_compares() {
  local list="$1" dir="$2" aname action have got
  while IFS=$'\t' read -r aname action have; do
    [ "$action" = compare ] || continue
    [ -f "$dir/$aname" ] || die2 "$TAG: '$aname' did not download from '$SRC'"
    got="$(sha256_of "$dir/$aname")"
    if [ "$got" = "$have" ]; then
      awk -F'\t' -v OFS='\t' -v n="$aname" '$1 == n && $2 == "compare" { $2 = "skip" } { print }' "$list" >"$list.new" && mv "$list.new" "$list"
    else
      [ -n "$REFUSAL" ] || REFUSAL="asset '$aname' is on the mirror with SHA256 $have but the source's is $got — a published asset is never replaced"
    fi
  done <"$list"
}
# download_listed LIST DIR — `gh release download` of every upload/compare
# entry in LIST into DIR (one call; a pattern that matches nothing is an error
# gh reports, which is could-not-tell here).
download_listed() {
  local list="$1" dir="$2" f
  local -a pats=()
  while IFS= read -r f; do pats+=(--pattern "$f"); done < <(awk -F'\t' '$2 == "upload" || $2 == "compare" { print $1 }' "$list")
  [ "${#pats[@]}" -gt 0 ] || return 0
  gh_read "$dir.log" release download "$TAG" --repo "$SRC" --dir "$dir" "${pats[@]}"
}
cols() { # → TEXT_COL / BIN_COL from the decision files
  local tu ts bu bs cu cs
  tu="$(count_action "$R/upload-text.txt" upload)"; ts="$(count_action "$R/upload-text.txt" skip)"
  TEXT_COL="$tu up/$ts skip"
  BIN_COL="-"
  [ "$WANT_BIN" -eq 1 ] || return 0
  bu="$(count_action "$R/upload-bin.txt" upload)"; bs="$(count_action "$R/upload-bin.txt" skip)"
  cu="$(count_action "$R/upload-companion.txt" upload)"; cs="$(count_action "$R/upload-companion.txt" skip)"
  BIN_COL="$bu up/$bs skip (+$cu/$cs sig+cert)"
}

# Table rows: tag | kind | tag-action | release-action | text | binaries | verdict
: >"$TMP/table.txt"
N_REFUSED=0; N_CREATED=0; N_SKIPPED=0
# For the `latest` fallback after the loop: was the newest stable release
# refused before its own POST (the one carrying make_latest=true), and which
# stable release did this run create last (= newest, the run is oldest-first).
NEWEST_STABLE_REFUSED=0; LAST_STABLE_CREATED=""; LAST_STABLE_CREATED_ID=""
row() { printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\n' "$@" >>"$TMP/table.txt"; }
refuse() { # TAG REASON — the release is refused, the run goes on
  echo "::error::backfill-releases: REFUSED $1 — $2"
  # Refusing a release the mirror already has leaves `latest` where it is;
  # refusing the newest stable BEFORE it is created leaves nothing marked.
  if [ "$1" = "$NEWEST_STABLE" ] && [ "$REL_ACTION" = create ]; then NEWEST_STABLE_REFUSED=1; fi
  N_REFUSED=$((N_REFUSED + 1)); cols; row "$1" "$KIND" "$TAG_ACTION" "$REL_ACTION" "$TEXT_COL" "$BIN_COL" refused
}

# ---- per-release work -------------------------------------------------------------------
while IFS= read -r TAG; do
  R="$TMP/r-$TAG"; mkdir -p "$R/text" "$R/bin" "$R/sums" "$R/guard-assets"
  jq --arg t "$TAG" '.[] | select(.tag_name == $t)' "$TMP/releases.json" >"$R/release.json"
  # Own assignment, not `[ "$(jq_of …)" = true ]`: inside a test the
  # substitution's exit 2 is swallowed and a malformed release.json would
  # silently read as "stable".
  PRERELEASE="$(jq_of "$R/release.json" '.prerelease')"
  KIND=stable; [ "$PRERELEASE" = true ] && KIND=prerelease
  NAME="$(jq_of "$R/release.json" '.name // .tag_name')"
  CREATED="$(jq_of "$R/release.json" '.created_at')"
  PUBLISHED="$(jq_of "$R/release.json" '.published_at // .created_at')"
  jq -r '.body // ""' "$R/release.json" >"$R/body.md"
  WANT_BIN=0; carries_binaries "$TAG" && WANT_BIN=1
  TAG_ACTION=create; REL_ACTION=create; REFUSAL=""
  : >"$R/upload-text.txt"; : >"$R/upload-bin.txt"; : >"$R/upload-companion.txt"

  # Assets, classified. SHA256SUMS is the authority on what a binary is: the
  # names it lists are binaries, <name>.sig / <name>.cert their companions,
  # everything else a text asset. A release without SHA256SUMS has no binaries
  # this tool can vouch for, so every asset is text and none is a binary.
  jq -r '.assets[] | [.name, (.digest // "")] | @tsv' "$R/release.json" >"$R/assets.tsv"
  : >"$R/sums.txt"
  if awk -F'\t' '$1 == "SHA256SUMS" { f = 1 } END { exit !f }' "$R/assets.tsv"; then
    gh_read "$R/sums.log" release download "$TAG" --repo "$SRC" --dir "$R/sums" --pattern SHA256SUMS
    [ -s "$R/sums/SHA256SUMS" ] || die2 "$TAG: SHA256SUMS did not download from '$SRC'"
    awk 'NF >= 2 { print $1 "\t" $NF }' "$R/sums/SHA256SUMS" >"$R/sums.txt"
  fi
  : >"$R/text.txt"; : >"$R/bin.txt"; : >"$R/companion.txt"
  while IFS=$'\t' read -r aname adigest; do
    if in_sums "$aname"; then printf '%s\t%s\n' "$aname" "$adigest" >>"$R/bin.txt"
    elif [[ "$aname" =~ $COMPANION_RE ]] && in_sums "${aname%.*}"; then printf '%s\t%s\n' "$aname" "$adigest" >>"$R/companion.txt"
    else printf '%s\t%s\n' "$aname" "$adigest" >>"$R/text.txt"; fi
  done <"$R/assets.tsv"

  # -- mirror state for this tag ----------------------------------------------------------
  jq --arg t "$TAG" '[ .[] | select(.tag_name == $t) ] | .[0] // empty' "$TMP/mirror-releases.json" >"$R/mirror-release.json"
  MREL_PRESENT=0; [ -s "$R/mirror-release.json" ] && MREL_PRESENT=1
  : >"$R/mirror-assets.tsv"
  [ "$MREL_PRESENT" -eq 0 ] || jq -r '.assets[] | [.name, ((.digest // "") | ltrimstr("sha256:"))] | @tsv' "$R/mirror-release.json" >"$R/mirror-assets.tsv"
  jq --arg r "refs/tags/$TAG" '[ .[] | select(.ref == $r) ] | .[0] // empty' "$TMP/mirror-tags.json" >"$R/mirror-tag.json"
  MTAG_PRESENT=0; [ -s "$R/mirror-tag.json" ] && MTAG_PRESENT=1
  if [ "$MTAG_PRESENT" -eq 1 ]; then
    # The tag exists: it must point at a commit the mirror has. Dereference an
    # annotated tag first; a dangling tag is refused, never repointed.
    OBJ_SHA="$(jq_of "$R/mirror-tag.json" '.object.sha')"; OBJ_TYPE="$(jq_of "$R/mirror-tag.json" '.object.type')"
    if [ "$OBJ_TYPE" = tag ]; then
      gh_read "$R/mirror-tagobj.json" api "repos/$MIRROR/git/tags/$OBJ_SHA"
      OBJ_SHA="$(jq_of "$R/mirror-tagobj.json" '.object.sha')"; OBJ_TYPE="$(jq_of "$R/mirror-tagobj.json" '.object.type')"
    fi
    if [ "$OBJ_TYPE" != commit ] || ! gh_read_maybe "$R/mirror-tagcommit.json" api "repos/$MIRROR/git/commits/$OBJ_SHA"; then
      REFUSAL="tag '$TAG' exists on the mirror but points at $OBJ_TYPE $OBJ_SHA, which the mirror does not have — a dangling tag is not repointed"
    fi
    TAG_ACTION=present
  fi
  [ "$MREL_PRESENT" -eq 0 ] || REL_ACTION=present

  # -- assets: skip / upload / compare / refuse, per asset --------------------------------------
  # An asset already on the mirror is skipped when its SHA256 equals the
  # source's, refused when it differs (a published asset is never replaced),
  # uploaded when absent. The mirror's digest comes from the API; a mirror
  # asset without one cannot be compared, and "cannot compare" is not "equal".
  while IFS=$'\t' read -r aname adigest; do decide "$R/upload-text.txt" "$aname" "${adigest#sha256:}"; done <"$R/text.txt"
  if [ "$WANT_BIN" -eq 1 ]; then
    while IFS=$'\t' read -r aname _; do decide "$R/upload-bin.txt" "$aname" "$(sum_of "$aname")"; done <"$R/bin.txt"
    while IFS=$'\t' read -r aname adigest; do decide "$R/upload-companion.txt" "$aname" "${adigest#sha256:}"; done <"$R/companion.txt"
  fi
  if [ -n "$REFUSAL" ]; then refuse "$TAG" "$REFUSAL"; continue; fi
  N_TODO="$(cat "$R"/upload-*.txt | awk -F'\t' '$2 == "upload" || $2 == "compare" { n++ } END { print n + 0 }')"
  if [ "$TAG_ACTION" = present ] && [ "$REL_ACTION" = present ] && [ "$N_TODO" -eq 0 ]; then
    note "$TAG: already on the mirror, every asset matches — nothing to do"
    N_SKIPPED=$((N_SKIPPED + 1)); cols; row "$TAG" "$KIND" present present "$TEXT_COL" "$BIN_COL" skipped; continue
  fi

  # -- downloads ------------------------------------------------------------------------------
  # Text assets to upload or compare come down in both modes (the guard reads
  # them). Binaries and companions come down under --apply only: they are large
  # and opaque to the scan; each binary is checked against SHA256SUMS at once.
  download_listed "$R/upload-text.txt" "$R/text"
  resolve_compares "$R/upload-text.txt" "$R/text"
  if [ -n "$REFUSAL" ]; then refuse "$TAG" "$REFUSAL"; continue; fi
  if [ "$APPLY" -eq 1 ] && [ "$WANT_BIN" -eq 1 ]; then
    cat "$R/upload-bin.txt" "$R/upload-companion.txt" >"$R/upload-binlike.txt"
    download_listed "$R/upload-binlike.txt" "$R/bin"
    while IFS=$'\t' read -r aname action expected; do
      [ "$action" = upload ] || continue
      [ -f "$R/bin/$aname" ] || die2 "$TAG: binary '$aname' did not download from '$SRC'"
      got="$(sha256_of "$R/bin/$aname")"
      [ "$got" = "$expected" ] || { REFUSAL="binary '$aname' hashes to $got but the source release's SHA256SUMS says $expected — not uploaded"; break; }   # mutation-anchor: sha-check
    done <"$R/upload-bin.txt"
    if [ -n "$REFUSAL" ]; then refuse "$TAG" "$REFUSAL"; continue; fi
    resolve_compares "$R/upload-companion.txt" "$R/bin"
    if [ -n "$REFUSAL" ]; then refuse "$TAG" "$REFUSAL"; continue; fi
  fi

  # -- notes --------------------------------------------------------------------------------
  if [ "$REL_ACTION" = create ]; then
    if [ "$NOTES_MODE" = fixed ]; then
      {
        echo "tracebloc CLI $TAG."
        echo
        echo "Install with the one-liner in the README, or download a binary below and"
        echo "verify it against SHA256SUMS and its cosign .sig/.cert (recipe in the README)."
      } >"$R/notes.md"
    else
      cp "$R/body.md" "$R/notes.md"
    fi
    {
      echo
      echo "---"
      echo "Backfilled release marker: originally published $PUBLISHED. The tag \`$TAG\` on this repository points at the default branch, not at the sources this release was built from."
      if [ "$WANT_BIN" -eq 0 ]; then
        echo "Binaries are carried only for the newest $BINARY_KEEP releases; re-run the installer to get a current build."
      fi
    } >>"$R/notes.md"
  fi

  # -- the guard: text assets, companions and notes, before any write --------------------------
  while IFS= read -r f; do cp "$R/text/$f" "$R/guard-assets/$f"; done < <(awk -F'\t' '$2 == "upload" { print $1 }' "$R/upload-text.txt")
  if [ "$APPLY" -eq 1 ] && [ "$WANT_BIN" -eq 1 ]; then
    while IFS= read -r f; do cp "$R/bin/$f" "$R/guard-assets/$f"; done < <(awk -F'\t' '$2 == "upload" { print $1 }' "$R/upload-companion.txt")
  fi
  [ "$REL_ACTION" != create ] || cp "$R/notes.md" "$R/guard-assets/RELEASE_NOTES.md"
  if [ -n "$(ls -A "$R/guard-assets")" ]; then
    rc=0; run_guard "$R/guard-assets" "$R/guard-out" || rc=$?
    case "$rc" in
      0) ;;
      1) REFUSAL="the guard refused the notes or a text asset: $(grep -E 'REFUSED' "$R/guard-out.log" | sed 's/^::error::publish-guard: //' | paste -sd';' -)" ;;   # mutation-anchor: guard-refusal
      *) cat "$R/guard-out.log"; die2 "$TAG: the guard could not tell (exit $rc)" ;;
    esac
    if [ -n "$REFUSAL" ]; then grep -E 'REFUSED|^    ' "$R/guard-out.log" | sed 's/^/    /'; refuse "$TAG" "$REFUSAL"; continue; fi
  fi

  cols
  if [ "$WANT_BIN" -eq 1 ]; then BIN_PLAN="$BIN_COL"; else BIN_PLAN="none (older than the newest $BINARY_KEEP)"; fi
  PLAN="tag: $TAG_ACTION | release: $REL_ACTION | text: $TEXT_COL | binaries: $BIN_PLAN"
  if [ "$APPLY" -eq 0 ]; then
    note "$TAG [$KIND] would: $PLAN"
    N_CREATED=$((N_CREATED + 1)); row "$TAG" "$KIND" "$TAG_ACTION" "$REL_ACTION" "$TEXT_COL" "$BIN_COL" planned; continue
  fi

  # -- writes -----------------------------------------------------------------------------------
  note "$TAG [$KIND]: $PLAN"
  if [ "$TAG_ACTION" = create ]; then
    # The original tag's date and message, when it is annotated; the release's
    # created_at otherwise. Read from the source, never invented.
    gh_read "$R/src-ref.json" api "repos/$SRC/git/ref/tags/$TAG"
    SRC_OBJ_TYPE="$(jq_of "$R/src-ref.json" '.object.type')"; SRC_OBJ_SHA="$(jq_of "$R/src-ref.json" '.object.sha')"
    TAG_DATE="$CREATED"; ORIG_MSG=""
    if [ "$SRC_OBJ_TYPE" = tag ]; then
      gh_read "$R/src-tagobj.json" api "repos/$SRC/git/tags/$SRC_OBJ_SHA"
      TAG_DATE="$(jq_of "$R/src-tagobj.json" '.tagger.date // empty')"; [ -n "$TAG_DATE" ] || TAG_DATE="$CREATED"
      ORIG_MSG="$(jq_of "$R/src-tagobj.json" '.message // ""')"
    fi
    {
      echo "Release $TAG"
      echo
      echo "Mirror release marker for $TAG: this tag points at the mirror's default-branch head, not at the sources the release was built from. Original tag date: $TAG_DATE."
      if [ -n "$ORIG_MSG" ]; then echo; echo "--- original tag message ---"; printf '%s\n' "$ORIG_MSG"; fi
    } >"$R/tag-message.txt"
    gh_write "$R/tagobj.json" api -X POST "repos/$MIRROR/git/tags" \
      -f "tag=$TAG" -F "message=@$R/tag-message.txt" -f "object=$MIRROR_HEAD" -f type=commit \
      -f "tagger[name]=${PUBLISH_MIRROR_GIT_NAME:-github-actions[bot]}" \
      -f "tagger[email]=${PUBLISH_MIRROR_GIT_EMAIL:-github-actions[bot]@users.noreply.github.com}" \
      -f "tagger[date]=$TAG_DATE"
    TAGOBJ_SHA="$(jq_of "$R/tagobj.json" '.sha')"
    [[ "$TAGOBJ_SHA" =~ ^[0-9a-f]{40}$ ]] || die2 "$TAG: the created tag object has no sha"
    gh_write "$R/ref.json" api -X POST "repos/$MIRROR/git/refs" -f "ref=refs/tags/$TAG" -f "sha=$TAGOBJ_SHA"
  fi
  if [ "$REL_ACTION" = create ]; then
    LATEST=false; [ "$TAG" = "$NEWEST_STABLE" ] && LATEST=true
    PRE=false; [ "$KIND" = prerelease ] && PRE=true
    gh_write "$R/created.json" api -X POST "repos/$MIRROR/releases" \
      -f "tag_name=$TAG" -f "name=$NAME" -F "body=@$R/notes.md" -F "prerelease=$PRE" -F draft=false -f "make_latest=$LATEST"
  fi
  UPLOADS=()
  while IFS= read -r f; do UPLOADS+=("$f"); done < <(
    awk -F'\t' -v d="$R/text" '$2 == "upload" { print d "/" $1 }' "$R/upload-text.txt"
    awk -F'\t' -v d="$R/bin"  '$2 == "upload" { print d "/" $1 }' "$R/upload-bin.txt" "$R/upload-companion.txt"
  )
  if [ "${#UPLOADS[@]}" -gt 0 ]; then
    gh_write "$R/upload.log" release upload "$TAG" "${UPLOADS[@]}" --repo "$MIRROR"
  fi
  if [ "$KIND" = stable ] && [ "$REL_ACTION" = create ]; then
    LAST_STABLE_CREATED="$TAG"; LAST_STABLE_CREATED_ID="$(jq_of "$R/created.json" '.id')"
    [[ "$LAST_STABLE_CREATED_ID" =~ ^[0-9]+$ ]] || die2 "$TAG: the created release has no numeric id"
  fi
  N_CREATED=$((N_CREATED + 1))
  row "$TAG" "$KIND" "$TAG_ACTION" "$REL_ACTION" "$TEXT_COL" "$BIN_COL" "done"
done <"$TMP/tags-run.txt"

# ---- latest, when the newest stable release was refused -----------------------------
# make_latest=true travels on the newest stable release's own POST; every older
# release is created with make_latest=false. Refused before that POST, the newest
# stable leaves the mirror's releases/latest answering 404 until a human re-runs
# --only-tag for it. Until then the newest stable release this run DID write is
# marked latest — that re-run's POST moves `latest` forward again.
if [ "$APPLY" -eq 1 ] && [ "$NEWEST_STABLE_REFUSED" -eq 1 ]; then   # mutation-anchor: latest-fallback
  if [ -n "$LAST_STABLE_CREATED" ]; then
    note "latest: $NEWEST_STABLE was refused — marking $LAST_STABLE_CREATED, the newest stable release written in this run, as latest until $NEWEST_STABLE is re-run"
    gh_write "$TMP/latest.json" api -X PATCH "repos/$MIRROR/releases/$LAST_STABLE_CREATED_ID" -F make_latest=true
  else
    echo "::warning::backfill-releases: $NEWEST_STABLE was refused and this run wrote no stable release — nothing is newly marked latest; re-run --only-tag $NEWEST_STABLE once the refusal is fixed"
  fi
fi

# ---- report -------------------------------------------------------------------------
echo
echo "backfill-releases: $MODE report — $SRC → $MIRROR"
{
  printf 'TAG\tKIND\tTAG-ON-MIRROR\tRELEASE\tTEXT ASSETS\tBINARIES\tVERDICT\n'
  cat "$TMP/table.txt"
} | column -t -s "$(printf '\t')" 2>/dev/null || cat "$TMP/table.txt"
echo
VERB=written; [ "$APPLY" -eq 1 ] || VERB=planned
echo "backfill-releases: $N_RUN release(s) in this run — $N_CREATED $VERB, $N_SKIPPED already complete, $N_REFUSED refused"
if [ "$N_REFUSED" -gt 0 ]; then
  echo "::error::backfill-releases: REFUSED — $N_REFUSED release(s) were refused (see the table); the rest went ahead"
  exit 1
fi
exit 0
