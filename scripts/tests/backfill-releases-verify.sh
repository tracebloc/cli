#!/usr/bin/env bash
# =============================================================================
#  backfill-releases-verify.sh — pin the properties of
#  scripts/backfill-releases.sh, the one-shot historical backfill of releases
#  to the public mirror.
#
#  Offline. `gh` is a recording FAKE on PATH that serves a source repository's
#  releases, tags and assets from a fixture directory and keeps the mirror's
#  state (tags, releases, uploaded assets with their digests) in JSON files it
#  mutates on every write, so a second run sees what the first one created.
#  Every call is logged; a WRITE is any `api -X POST` or `release upload` line.
#  gitleaks is a stub (the guard needs one on PATH; the real scanner is the
#  workflow's business).
#
#  Pinned: dry-run writes nothing; --apply makes exactly the expected writes,
#  oldest release first, newest stable release last and marked latest; a second
#  --apply writes nothing; the BINARY_KEEP boundary (the 10th newest carries
#  binaries, the 11th does not — and the count is over the FILTERED list, so
#  --include-prerelease shifts it); a binary whose SHA256 disagrees with the
#  source's SHA256SUMS is refused by name and nothing of that release is
#  written; an unset mirror and a mirror equal to the source are refused by
#  publish-mirror's rule; prereleases are excluded by default; a read that
#  fails is could-not-tell (exit 2) with the failing call named, never an empty
#  list; the DEFAULT notes are the workflow's fixed text (the source body is
#  never written or scanned unless --notes source asks for it); under --notes
#  source a refuse-tier needle in a release body refuses that release naming
#  the tier; a mirror asset already present with a different digest is refused,
#  never replaced; a mirror tag that dangles is refused, never repointed;
#  --only-tag / --from-tag narrow the run without changing the binary decision;
#  the mirror tag carries the original date and, for an annotated source tag,
#  the original message.
#
#  --mutations: copies the script, breaks ONE rule per copy at its
#  `# mutation-anchor: NAME` line, proves the mutation landed (the anchor was
#  found exactly once and the copy differs), runs THIS suite against the copy
#  and demands red. A mutation the suite survives is a vacuous test, reported
#  as a failure of this harness.
#
#  Environment (mutation runs only): BACKFILL_UNDER_TEST names the script copy
#  to test instead of ../backfill-releases.sh.
# =============================================================================
set -uo pipefail

SELF="$(cd "$(dirname "$0")" && pwd)/$(basename "$0")"
SELF_DIR="$(cd "$(dirname "$0")" && pwd)"
SCRIPTS_DIR="$(cd "$SELF_DIR/.." && pwd)"
REAL="$SCRIPTS_DIR/backfill-releases.sh"
BACKFILL="${BACKFILL_UNDER_TEST:-$REAL}"
[ -f "$REAL" ] || { printf 'backfill-releases-verify: %s missing — refusing to report clean\n' "$REAL" >&2; exit 2; }
[ -f "$BACKFILL" ] || { printf 'backfill-releases-verify: %s missing — refusing to report clean\n' "$BACKFILL" >&2; exit 2; }
for t in jq git awk; do command -v "$t" >/dev/null 2>&1 || { printf 'backfill-releases-verify: %s is not on PATH — refusing to report clean\n' "$t" >&2; exit 2; }; done

PASS=0
FAIL=0
ok()  { printf '  ok   %s\n' "$1"; PASS=$((PASS+1)); }
bad() { printf '  FAIL %s\n' "$1"; FAIL=$((FAIL+1)); }

ROOT="$(mktemp -d "${TMPDIR:-/tmp}/backfill-releases-verify.XXXXXX")"
trap 'rm -rf "$ROOT"' EXIT
if command -v sha256sum >/dev/null 2>&1; then sha256_of() { sha256sum "$1" | cut -d' ' -f1; }; else sha256_of() { shasum -a 256 "$1" | cut -d' ' -f1; }; fi

# =============================================================================
#  --mutations mode: break one rule per copy, demand red.
# =============================================================================
if [ "${1:-}" = "--mutations" ]; then
  echo "== backfill-releases.sh mutations =="
  # NAME|REPLACEMENT — the line carrying `# mutation-anchor: NAME` becomes REPLACEMENT.
  # Each replacement is a valid program that drops exactly the rule the anchor names.
  MUTATIONS=(
    'releases-read-fail-closed|gh api --paginate "repos/$SRC/releases" >"$TMP/src-pages.json" 2>/dev/null || printf "[]" >"$TMP/src-pages.json"'
    'prerelease-filter|FILTER='"'"'[ .[] | select(.draft == false) ] | sort_by(.created_at) | reverse'"'"
    'binary-keep|cp "$TMP/tags-newest-first.txt" "$TMP/tags-with-binaries.txt"'
    'idempotent-skip|:'
    'sha-check|:'
    'guard-refusal|      1) ;;'
    'notes-default-fixed|APPLY=0; FROM_TAG=""; ONLY_TAG=""; INCLUDE_PRE=0; NOTES_MODE=source; STRICT=0'
  )
  MUT_PASS=0; MUT_FAIL=0
  # Every mutant is prepared and PROVEN to have landed first; then the suite
  # runs against all of them in parallel (each run is ~a minute of forks).
  NAMES=()
  for entry in "${MUTATIONS[@]}"; do
    name="${entry%%|*}"; repl="${entry#*|}"
    copy="$ROOT/mutant-$name.sh"
    n="$(grep -c -- "# mutation-anchor: $name\$" "$REAL")"
    if [ "$n" -ne 1 ]; then bad "mutation $name: anchor found $n time(s) in $REAL, need exactly 1"; MUT_FAIL=$((MUT_FAIL+1)); continue; fi
    awk -v a="# mutation-anchor: $name" -v r="$repl" 'index($0, a) && substr($0, length($0) - length(a) + 1) == a { print r; next } { print }' "$REAL" >"$copy"
    if cmp -s "$REAL" "$copy"; then bad "mutation $name: the copy is identical to the original — the mutation did not apply"; MUT_FAIL=$((MUT_FAIL+1)); continue; fi
    if ! bash -n "$copy" 2>"$ROOT/mutant.err"; then bad "mutation $name: the mutant does not parse: $(cat "$ROOT/mutant.err")"; MUT_FAIL=$((MUT_FAIL+1)); continue; fi
    NAMES+=("$name")
  done
  for name in "${NAMES[@]+"${NAMES[@]}"}"; do
    ( if BACKFILL_UNDER_TEST="$ROOT/mutant-$name.sh" bash "$SELF" >"$ROOT/mutant-$name.log" 2>&1; then echo green; else echo red; fi >"$ROOT/mutant-$name.verdict" ) &
  done
  wait
  for name in "${NAMES[@]+"${NAMES[@]}"}"; do
    out="$ROOT/mutant-$name.log"
    if [ "$(cat "$ROOT/mutant-$name.verdict" 2>/dev/null)" = red ]; then
      ok "mutation $name: caught — $(grep -c '^  FAIL' "$out") test(s) reddened: $(grep '^  FAIL' "$out" | head -2 | sed -E 's/^  FAIL ([^(:]*).*/\1/' | paste -sd'|' -)"; MUT_PASS=$((MUT_PASS+1))
    else
      bad "mutation $name: the suite stayed GREEN against the mutant — a test is vacuous"; MUT_FAIL=$((MUT_FAIL+1))
      sed 's/^/      /' "$out" | tail -5
    fi
  done
  echo
  printf 'backfill-releases-verify --mutations: %d caught, %d survived\n' "$MUT_PASS" "$MUT_FAIL"
  [ "$MUT_FAIL" -eq 0 ] && [ "$MUT_PASS" -ge 4 ]
  exit $?
fi

# =============================================================================
#  The fake gh and the gitleaks stub
# =============================================================================
SHIM="$ROOT/shim"; mkdir -p "$SHIM"
cat >"$SHIM/gitleaks" <<'EOF'
#!/usr/bin/env bash
[ "${1:-}" = version ] && { echo "gitleaks-stub"; exit 0; }
exit 0
EOF
cat >"$SHIM/gh" <<'EOF'
#!/usr/bin/env bash
# Recording fake gh. FIX: fixtures (read-only). STATE: the mirror, mutated by writes.
set -uo pipefail
FIX="${FAKE_GH_FIX:?}"; STATE="${FAKE_GH_STATE:?}"
printf '%s\n' "$*" >>"${GH_LOG:?}"
if [ -n "${FAKE_GH_FAIL_RE:-}" ] && [[ "$*" =~ $FAKE_GH_FAIL_RE ]]; then echo "gh: Internal Server Error (HTTP 500)" >&2; exit 1; fi
SRC="$(cat "$FIX/src-repo")"; MIRROR="$(cat "$FIX/mirror-repo")"; HEAD_SHA="$(cat "$FIX/mirror-head")"
if command -v sha256sum >/dev/null 2>&1; then sha() { sha256sum "$1" | cut -d' ' -f1; }; else sha() { shasum -a 256 "$1" | cut -d' ' -f1; }; fi
fake_sha() { printf '%s' "$1" | { command -v sha256sum >/dev/null 2>&1 && sha256sum || shasum -a 256; } | cut -c1-40; }   # a 40-hex git object id
notfound() { echo "gh: Not Found (HTTP 404)" >&2; exit 1; }
fieldval() { # KEY from -f/-F pairs; @file is read
  local k="$1" f v
  for f in "${FIELDS[@]+"${FIELDS[@]}"}"; do
    case "$f" in "$k="*) v="${f#*=}"; case "$v" in @*) cat "${v#@}" ;; *) printf '%s' "$v" ;; esac; return 0 ;; esac
  done
  return 1
}
cmd="${1:-}"; shift || true
case "$cmd" in
  repo)
    printf '{"nameWithOwner":"%s"}\n' "$SRC" ;;
  api)
    METHOD=GET; PATHP=""; FIELDS=()
    while [ "$#" -gt 0 ]; do
      case "$1" in
        -X) METHOD="$2"; shift 2 ;;
        --paginate) shift ;;
        -f|-F) FIELDS+=("$2"); shift 2 ;;
        *) PATHP="$1"; shift ;;
      esac
    done
    case "$METHOD $PATHP" in
      "GET repos/$SRC/releases")
        # Two "pages": the fixture is split so --paginate's concatenated-arrays shape is exercised.
        jq -c '.[0:7]' "$FIX/src-releases.json"; jq -c '.[7:]' "$FIX/src-releases.json" ;;
      "GET repos/$SRC/git/ref/tags/"*)
        t="${PATHP##*/}"; jq -e --arg r "refs/tags/$t" '.[] | select(.ref == $r)' "$FIX/src-tags.json" >/dev/null || notfound
        jq --arg r "refs/tags/$t" '.[] | select(.ref == $r)' "$FIX/src-tags.json" ;;
      "GET repos/$SRC/git/tags/"*)
        s="${PATHP##*/}"; jq -e --arg s "$s" '.[] | select(.sha == $s)' "$FIX/src-tagobjs.json" >/dev/null || notfound
        jq --arg s "$s" '.[] | select(.sha == $s)' "$FIX/src-tagobjs.json" ;;
      "GET repos/$MIRROR")
        printf '{"full_name":"%s","default_branch":"main","visibility":"public"}\n' "$MIRROR" ;;
      "GET repos/$MIRROR/commits/main")
        [ -z "${FAKE_GH_EMPTY_MIRROR:-}" ] || { echo "gh: Git Repository is empty. (HTTP 409)" >&2; exit 1; }
        printf '{"sha":"%s"}\n' "$HEAD_SHA" ;;
      "GET repos/$MIRROR/releases")
        cat "$STATE/mirror-releases.json" ;;
      "GET repos/$MIRROR/git/matching-refs/tags/")
        cat "$STATE/mirror-tags.json" ;;
      "GET repos/$MIRROR/git/commits/"*)
        s="${PATHP##*/}"; [ "$s" = "$HEAD_SHA" ] || notfound; printf '{"sha":"%s"}\n' "$s" ;;
      "GET repos/$MIRROR/git/tags/"*)
        s="${PATHP##*/}"; jq -e --arg s "$s" '.[] | select(.sha == $s)' "$STATE/mirror-tagobjs.json" >/dev/null || notfound
        jq --arg s "$s" '.[] | select(.sha == $s)' "$STATE/mirror-tagobjs.json" ;;
      "POST repos/$MIRROR/git/tags")
        tag="$(fieldval tag)"; msg="$(fieldval message)"; obj="$(fieldval object)"; date="$(fieldval 'tagger[date]')"
        s="$(fake_sha "tagobj:$tag")"
        jq --arg s "$s" --arg tag "$tag" --arg msg "$msg" --arg obj "$obj" --arg date "$date" \
          '. + [{sha: $s, tag: $tag, message: $msg, tagger: {date: $date}, object: {sha: $obj, type: "commit"}}]' "$STATE/mirror-tagobjs.json" >"$STATE/t.json" && mv "$STATE/t.json" "$STATE/mirror-tagobjs.json"
        printf '{"sha":"%s"}\n' "$s" ;;
      "POST repos/$MIRROR/git/refs")
        ref="$(fieldval ref)"; s="$(fieldval sha)"
        jq -e --arg r "$ref" '.[] | select(.ref == $r)' "$STATE/mirror-tags.json" >/dev/null && { echo "gh: Reference already exists (HTTP 422)" >&2; exit 1; }
        jq --arg r "$ref" --arg s "$s" '. + [{ref: $r, object: {sha: $s, type: "tag"}}]' "$STATE/mirror-tags.json" >"$STATE/t.json" && mv "$STATE/t.json" "$STATE/mirror-tags.json"
        printf '{"ref":"%s"}\n' "$ref" ;;
      "POST repos/$MIRROR/releases")
        tag="$(fieldval tag_name)"; name="$(fieldval name)"; body="$(fieldval body)"; pre="$(fieldval prerelease)"; latest="$(fieldval make_latest)"
        jq -e --arg r "refs/tags/$tag" '.[] | select(.ref == $r)' "$STATE/mirror-tags.json" >/dev/null || { echo "gh: fake: release for '$tag' before its tag (HTTP 422)" >&2; exit 1; }
        jq --arg tag "$tag" --arg name "$name" --arg body "$body" --arg pre "$pre" --arg latest "$latest" \
          '. + [{id: (length + 1), tag_name: $tag, name: $name, body: $body, prerelease: ($pre == "true"), make_latest: $latest, draft: false, assets: []}]' "$STATE/mirror-releases.json" >"$STATE/t.json" && mv "$STATE/t.json" "$STATE/mirror-releases.json"
        printf '{"id":1,"tag_name":"%s"}\n' "$tag" ;;
      *) echo "gh: fake: unhandled $METHOD $PATHP" >&2; exit 1 ;;
    esac ;;
  release)
    sub="${1:-}"; shift || true
    case "$sub" in
      download)
        tag="$1"; shift; repo=""; dir=""; pats=()
        while [ "$#" -gt 0 ]; do case "$1" in --repo) repo="$2"; shift 2 ;; --dir) dir="$2"; shift 2 ;; --pattern) pats+=("$2"); shift 2 ;; *) echo "gh: fake: unknown download arg $1" >&2; exit 1 ;; esac; done
        [ "$repo" = "$SRC" ] || { echo "gh: fake: download from '$repo' is not the source" >&2; exit 1; }
        mkdir -p "$dir"
        for p in "${pats[@]}"; do
          [ -f "$FIX/assets/$tag/$p" ] || { echo "gh: no assets match the file pattern ($p)" >&2; exit 1; }
          [ ! -e "$dir/$p" ] || { echo "gh: fake: $dir/$p already exists (gh refuses without --clobber)" >&2; exit 1; }
          cp "$FIX/assets/$tag/$p" "$dir/$p"
        done ;;
      upload)
        tag="$1"; shift; repo=""; files=()
        while [ "$#" -gt 0 ]; do case "$1" in --repo) repo="$2"; shift 2 ;; *) files+=("$1"); shift ;; esac; done
        [ "$repo" = "$MIRROR" ] || { echo "gh: fake: upload to '$repo' is not the mirror" >&2; exit 1; }
        jq -e --arg t "$tag" '.[] | select(.tag_name == $t)' "$STATE/mirror-releases.json" >/dev/null || { echo "gh: release not found" >&2; exit 1; }
        for f in "${files[@]}"; do
          [ -f "$f" ] || { echo "gh: fake: $f is not a file" >&2; exit 1; }
          n="$(basename "$f")"; d="$(sha "$f")"
          jq -e --arg t "$tag" --arg n "$n" '.[] | select(.tag_name == $t) | .assets[] | select(.name == $n)' "$STATE/mirror-releases.json" >/dev/null && { echo "gh: fake: asset '$n' already on '$tag' (HTTP 422)" >&2; exit 1; }
          jq --arg t "$tag" --arg n "$n" --arg d "sha256:$d" 'map(if .tag_name == $t then .assets += [{name: $n, digest: $d}] else . end)' "$STATE/mirror-releases.json" >"$STATE/t.json" && mv "$STATE/t.json" "$STATE/mirror-releases.json"
        done ;;
      *) echo "gh: fake: unhandled release $sub" >&2; exit 1 ;;
    esac ;;
  *) echo "gh: fake: unhandled command $cmd" >&2; exit 1 ;;
esac
EOF
chmod +x "$SHIM/gh" "$SHIM/gitleaks"

# =============================================================================
#  Fixtures: 12 stable releases v0.1.0..v0.1.11 (created a day apart) and one
#  newer prerelease v0.1.12-rc.1. Each carries install.sh, install.ps1,
#  SHA256SUMS, two "binaries" and their .sig/.cert. v0.1.3's source tag is
#  annotated. The fixture list is deliberately NOT in date order.
# =============================================================================
STABLE=(v0.1.0 v0.1.1 v0.1.2 v0.1.3 v0.1.4 v0.1.5 v0.1.6 v0.1.7 v0.1.8 v0.1.9 v0.1.10 v0.1.11)
PRE=v0.1.12-rc.1
ALL=("${STABLE[@]}" "$PRE")
HEAD_SHA=1111111111111111111111111111111111111111
ANNOT_TAG=v0.1.3; ANNOT_DATE=2025-12-31T10:00:00Z; ANNOT_MSG="tracebloc CLI v0.1.3 original annotation"

# build_fixtures DIR [CORRUPT_SUMS_TAG] [BAD_BODY_TAG]
build_fixtures() {
  local fix="$1" corrupt="${2:-}" badbody="${3:-}" i tag pre body created f
  mkdir -p "$fix/assets"
  printf 'acme/src' >"$fix/src-repo"; printf 'acme/mirror' >"$fix/mirror-repo"; printf '%s' "$HEAD_SHA" >"$fix/mirror-head"
  : >"$fix/releases.ndjson"; : >"$fix/tags.ndjson"; printf '[]' >"$fix/src-tagobjs.json"
  i=0
  for tag in "${ALL[@]}"; do
    i=$((i + 1)); mkdir -p "$fix/assets/$tag"
    printf '#!/bin/sh\necho install %s\n' "$tag" >"$fix/assets/$tag/install.sh"
    printf 'Write-Host install %s\n' "$tag" >"$fix/assets/$tag/install.ps1"
    for f in "tracebloc-$tag-linux-amd64" "tracebloc-$tag-darwin-arm64"; do
      printf 'binary %s\n' "$f" >"$fix/assets/$tag/$f"
      printf 'MEUCIQ%s\n' "$f" >"$fix/assets/$tag/$f.sig"
      printf -- '-----BEGIN CERTIFICATE-----\n%s\n-----END CERTIFICATE-----\n' "$f" >"$fix/assets/$tag/$f.cert"
    done
    ( cd "$fix/assets/$tag" && for f in tracebloc-*; do case "$f" in *.sig|*.cert) ;; *) printf '%s  %s\n' "$(sha256_of "$f")" "$f" ;; esac; done ) >"$fix/assets/$tag/SHA256SUMS"
    if [ "$tag" = "$corrupt" ]; then
      awk -v n="tracebloc-$tag-darwin-arm64" '$2 == n { $1 = "0000000000000000000000000000000000000000000000000000000000000000" } { print $1 "  " $2 }' "$fix/assets/$tag/SHA256SUMS" >"$fix/s" && mv "$fix/s" "$fix/assets/$tag/SHA256SUMS"
    fi
    pre=false; [ "$tag" = "$PRE" ] && pre=true
    created="$(printf '2026-01-%02dT12:00:00Z' "$i")"
    body="## What's Changed\n* fix: something in $tag by @dev in https://github.com/acme/src/pull/$i"
    [ "$tag" != "$badbody" ] || body="$body\n* ops: moved to role arn:aws:iam::000000000000:role/planted"
    ( cd "$fix/assets/$tag" && for f in *; do printf '%s\t%s\t%s\n' "$f" "sha256:$(sha256_of "$f")" "$(wc -c <"$f" | tr -d ' ')"; done ) \
      | jq -R -s -c --arg tag "$tag" --arg pre "$pre" --arg created "$created" --arg body "$(printf "$body")" '
          split("\n") | map(select(length > 0) | split("\t") | {name: .[0], digest: .[1], size: (.[2]|tonumber)}) as $assets
          | {tag_name: $tag, name: $tag, body: $body, draft: false, prerelease: ($pre == "true"), created_at: $created, published_at: $created, target_commitish: "develop", assets: $assets}' >>"$fix/releases.ndjson"
    if [ "$tag" = "$ANNOT_TAG" ]; then
      jq -n -c --arg t "$tag" '{ref: ("refs/tags/" + $t), object: {sha: "3333333333333333333333333333333333333333", type: "tag"}}' >>"$fix/tags.ndjson"
      jq -n --arg m "$ANNOT_MSG" --arg d "$ANNOT_DATE" '[{sha: "3333333333333333333333333333333333333333", message: $m, tagger: {name: "dev", date: $d}, object: {sha: "cccccccccccccccccccccccccccccccccccccccc", type: "commit"}}]' >"$fix/src-tagobjs.json"
    else
      jq -n -c --arg t "$tag" --argjson i "$i" '{ref: ("refs/tags/" + $t), object: {sha: ("c" * 39 + ($i|tostring)|.[0:40]), type: "commit"}}' >>"$fix/tags.ndjson"
    fi
  done
  # Shuffle the release order (newest in the middle) so sorting is the script's, not the fixture's.
  jq -s '[.[12], .[3], .[0], .[11], .[7], .[1], .[10], .[2], .[9], .[4], .[8], .[5], .[6]]' "$fix/releases.ndjson" >"$fix/src-releases.json"
  jq -s '.' "$fix/tags.ndjson" >"$fix/src-tags.json"
}
fresh_state() { # DIR — an empty mirror state
  mkdir -p "$1"; printf '[]' >"$1/mirror-releases.json"; printf '[]' >"$1/mirror-tags.json"; printf '[]' >"$1/mirror-tagobjs.json"
}

FIX="$ROOT/fix"; build_fixtures "$FIX"
FIX_CORRUPT="$ROOT/fix-corrupt"; build_fixtures "$FIX_CORRUPT" v0.1.11
FIX_BADBODY="$ROOT/fix-badbody"; build_fixtures "$FIX_BADBODY" "" v0.1.5

# run STATE_DIR FIX_DIR ARGS... — the script with the fake gh; OUTPUT, RC, GH_LOG set.
run() {
  local state="$1" fix="$2"; shift 2
  GH_LOG="$ROOT/gh-$RANDOM$RANDOM.log"; : >"$GH_LOG"
  OUTPUT="$(PATH="$SHIM:$PATH" GH_LOG="$GH_LOG" FAKE_GH_FIX="$fix" FAKE_GH_STATE="$state" PUBLISH_GUARD_GITLEAKS="$SHIM/gitleaks" \
    BACKFILL_SCRIPTS_DIR="$SCRIPTS_DIR" SOURCE_REPO="${SOURCE_REPO-acme/src}" MIRROR_REPO="${MIRROR_REPO-mirror}" BINARY_KEEP="${BINARY_KEEP-10}" \
    bash "$BACKFILL" "$@" 2>&1)"; RC=$?
}
has()    { [[ "$OUTPUT" == *"$1"* ]]; }
hasg()   { [[ "$OUTPUT" == *$1* ]]; }   # $1 is a glob: `a*b`; escape [ ] as \[ \]
writes() { grep -cE '^(api -X POST|release upload) ' "$GH_LOG" || true; }
posts()  { grep -c "^api -X POST repos/acme/mirror/$1 " "$GH_LOG" || true; }
verdicts() { printf '%s\n' "$OUTPUT" | awk -v v="$1" '$NF == v && $1 ~ /^v[0-9]/ { n++ } END { print n + 0 }'; }

echo "== backfill-releases.sh harness =="

# ---- 1. dry-run (the default) writes nothing ---------------------------------------------
S1="$ROOT/s1"; fresh_state "$S1"
run "$S1" "$FIX"
if [ "$RC" -eq 0 ] && [ "$(writes)" -eq 0 ] && [ "$(verdicts planned)" -eq 12 ] && has "12 release(s) match the filter, 12 in this run; binaries for the newest 10" && has "mode=dry-run" \
   && has "v0.1.0 [stable] would: tag: create | release: create | text: 3 up/0 skip | binaries: none (older than the newest 10)" \
   && has "v0.1.11 [stable] would: tag: create | release: create | text: 3 up/0 skip | binaries: 2 up/0 skip (+4/0 sig+cert)" \
   && grep -q '^release download v0.1.11 --repo acme/src --dir .*/text --pattern' "$GH_LOG" && ! grep -q -- '--pattern tracebloc-' "$GH_LOG"; then
  ok "dry-run (default): every read runs, text assets are fetched for the guard, no binary is fetched, zero writes, 12 rows planned"
else bad "dry-run (rc=$RC writes=$(writes) planned=$(verdicts planned)): $OUTPUT"; fi
if [ "$(jq length "$S1/mirror-releases.json")" -eq 0 ] && [ "$(jq length "$S1/mirror-tags.json")" -eq 0 ]; then ok "dry-run: the mirror state is untouched"; else bad "dry-run touched the mirror state"; fi

# ---- 2. --apply makes exactly the expected writes -----------------------------------------
S2="$ROOT/s2"; fresh_state "$S2"
run "$S2" "$FIX" --apply
first_rel="$(grep '^api -X POST repos/acme/mirror/releases ' "$GH_LOG" | head -1)"
last_rel="$(grep '^api -X POST repos/acme/mirror/releases ' "$GH_LOG" | tail -1)"
if [ "$RC" -eq 0 ] && [ "$(writes)" -eq 48 ] && [ "$(posts git/tags)" -eq 12 ] && [ "$(posts git/refs)" -eq 12 ] && [ "$(posts releases)" -eq 12 ] \
   && [ "$(grep -c '^release upload ' "$GH_LOG")" -eq 12 ] && [ "$(verdicts "done")" -eq 12 ] && has "12 release(s) in this run — 12 written, 0 already complete, 0 refused"; then
  ok "apply: 12 releases → exactly 48 writes (tag object, ref, release, one upload call each), all 12 done"
else bad "apply (rc=$RC writes=$(writes) tags=$(posts git/tags) refs=$(posts git/refs) rel=$(posts releases)): $OUTPUT"; fi
if [[ "$first_rel" == *"-f tag_name=v0.1.0 "*"-f make_latest=false"* ]] && [[ "$last_rel" == *"-f tag_name=v0.1.11 "*"-f make_latest=true"* ]] \
   && [ "$(grep -c -- '-f make_latest=true' "$GH_LOG")" -eq 1 ] && ! grep -q 'v0.1.12-rc.1' "$GH_LOG"; then
  ok "apply: oldest first, newest stable last and the only make_latest=true; the prerelease is never touched"
else bad "apply order/latest: first='$first_rel' last='$last_rel'"; fi
if [ "$(jq -r '.[] | .tag_name' "$S2/mirror-releases.json" | paste -sd' ' -)" = "${STABLE[*]}" ] && [ "$(jq -r '[.[] | select(.prerelease)] | length' "$S2/mirror-releases.json")" -eq 0 ] \
   && [ "$(jq -r '.[] | select(.tag_name == "v0.1.11") | .assets | length' "$S2/mirror-releases.json")" -eq 9 ] && [ "$(jq -r '.[] | select(.tag_name == "v0.1.0") | .assets | length' "$S2/mirror-releases.json")" -eq 3 ]; then
  ok "apply: the mirror holds the 12 stable releases; the newest carries 9 assets (3 text + 2 binaries + 4 sig/cert), the oldest 3"
else bad "apply mirror state: $(jq -c '[.[] | {tag_name, n: (.assets|length)}]' "$S2/mirror-releases.json")"; fi
tag0="$(jq -r '.[] | select(.tag == "v0.1.0")' "$S2/mirror-tagobjs.json")"; tag3="$(jq -r '.[] | select(.tag == "v0.1.3")' "$S2/mirror-tagobjs.json")"
if [ "$(printf '%s' "$tag0" | jq -r .object.sha)" = "$HEAD_SHA" ] && [ "$(printf '%s' "$tag0" | jq -r .tagger.date)" = "2026-01-01T12:00:00Z" ] \
   && [[ "$(printf '%s' "$tag0" | jq -r .message)" == *"Mirror release marker for v0.1.0"*"not at the sources"* ]] \
   && [ "$(printf '%s' "$tag3" | jq -r .tagger.date)" = "$ANNOT_DATE" ] && [[ "$(printf '%s' "$tag3" | jq -r .message)" == *"--- original tag message ---"*"$ANNOT_MSG"* ]] \
   && [ "$(jq -r '[.[] | .object.sha] | unique | length' "$S2/mirror-tagobjs.json")" -eq 1 ]; then
  ok "tags: every mirror tag is an annotated marker on the mirror head; the date is the release's, or the source tag's own when annotated, with its message carried"
else bad "tags: v0.1.0=$(printf '%s' "$tag0" | jq -c .) v0.1.3=$(printf '%s' "$tag3" | jq -c .)"; fi
body0="$(jq -r '.[] | select(.tag_name == "v0.1.0") | .body' "$S2/mirror-releases.json")"; body11="$(jq -r '.[] | select(.tag_name == "v0.1.11") | .body' "$S2/mirror-releases.json")"
if [[ "$body0" == "tracebloc CLI v0.1.0."*"verify it against SHA256SUMS"*"originally published 2026-01-01T12:00:00Z"*"re-run the installer"* ]] && [[ "$body0" != *"What's Changed"* ]] && [[ "$body0" != *"acme/src/pull/"* ]] \
   && [[ "$body11" == "tracebloc CLI v0.1.11."*"originally published 2026-01-12T12:00:00Z"* ]] && [[ "$body11" != *"acme/src/pull/"* ]] && [[ "$body11" != *"re-run the installer"* ]] && has "notes=fixed"; then
  ok "notes (default): the workflow's fixed text plus an original-date footer, no trace of the source body; the installer hint appears only where binaries are not carried"
else bad "notes default: body0='$body0' body11='$body11'"; fi
# The tag is created before its release (the fake refuses the other order) and the release before its uploads.
if [ "$(grep -nE '^(api -X POST repos/acme/mirror/(git/tags|git/refs|releases)|release upload) ' "$GH_LOG" | head -4 | sed -E 's/^[0-9]+://; s/ .*//' | paste -sd' ' -)" = "api api api release" ]; then
  ok "apply: per release the order is tag object, ref, release, upload"
else bad "apply order: $(head -8 "$GH_LOG")"; fi
# --notes source is the explicit opt-in that carries the source body.
S2B="$ROOT/s2b"; fresh_state "$S2B"
run "$S2B" "$FIX" --apply --notes source
body0="$(jq -r '.[] | select(.tag_name == "v0.1.0") | .body' "$S2B/mirror-releases.json")"; body11="$(jq -r '.[] | select(.tag_name == "v0.1.11") | .body' "$S2B/mirror-releases.json")"
if [ "$RC" -eq 0 ] && has "notes=source" && [[ "$body0" == "## What's Changed"*"acme/src/pull/1"*"originally published 2026-01-01T12:00:00Z"*"re-run the installer"* ]] && [[ "$body0" != *"tracebloc CLI v0.1.0."* ]] \
   && [[ "$body11" == "## What's Changed"*"acme/src/pull/12"*"originally published 2026-01-12T12:00:00Z"* ]] && [[ "$body11" != *"re-run the installer"* ]]; then
  ok "--notes source: the source body is carried with the same original-date footer, and only when asked for"
else bad "notes source (rc=$RC): body0='$body0' body11='$body11'"; fi
run "$S2B" "$FIX" --notes generated
if [ "$RC" -eq 2 ] && has "--notes must be 'fixed' or 'source', not 'generated'" && [ "$(wc -l <"$GH_LOG" | tr -d ' ')" -eq 0 ]; then
  ok "--notes with anything else is could-not-tell before any gh call"
else bad "notes bogus (rc=$RC): $OUTPUT"; fi

# ---- 3. a second --apply writes nothing ---------------------------------------------------
before="$(cat "$S2/mirror-releases.json" "$S2/mirror-tags.json" | sha256_of /dev/stdin)"
run "$S2" "$FIX" --apply
after="$(cat "$S2/mirror-releases.json" "$S2/mirror-tags.json" | sha256_of /dev/stdin)"
if [ "$RC" -eq 0 ] && [ "$(writes)" -eq 0 ] && [ "$(verdicts skipped)" -eq 12 ] && has "12 release(s) in this run — 0 written, 12 already complete, 0 refused" && [ "$before" = "$after" ] \
   && ! grep -q '^release download .* --pattern install' "$GH_LOG"; then
  ok "idempotent: a second --apply over the same mirror makes zero writes, downloads no asset it can compare by digest, and reports 12 already complete"
else bad "idempotent (rc=$RC writes=$(writes) skipped=$(verdicts skipped)): $OUTPUT"; fi

# ---- 4. BINARY_KEEP boundary ---------------------------------------------------------------
# Newest 10 of the 12 stable: v0.1.2 (10th) carries binaries, v0.1.1 (11th) does not.
if [ "$(jq -r '.[] | select(.tag_name == "v0.1.2") | .assets | length' "$S2/mirror-releases.json")" -eq 9 ] && [ "$(jq -r '.[] | select(.tag_name == "v0.1.1") | .assets | length' "$S2/mirror-releases.json")" -eq 3 ] \
   && [ "$(jq -r '.[] | select(.tag_name == "v0.1.1") | [.assets[].name] | sort | join(" ")' "$S2/mirror-releases.json")" = "SHA256SUMS install.ps1 install.sh" ]; then
  ok "BINARY_KEEP=10: the 10th newest (v0.1.2) carries binaries + sig/cert, the 11th (v0.1.1) carries exactly the three text assets"
else bad "binary-keep boundary: v0.1.2=$(jq -c '.[] | select(.tag_name == "v0.1.2") | [.assets[].name]' "$S2/mirror-releases.json") v0.1.1=$(jq -c '.[] | select(.tag_name == "v0.1.1") | [.assets[].name]' "$S2/mirror-releases.json")"; fi
S4="$ROOT/s4"; fresh_state "$S4"
BINARY_KEEP=1 run "$S4" "$FIX"
if [ "$RC" -eq 0 ] && hasg "v0.1.11 \[stable\] would: *binaries: 2 up/0 skip" && hasg "v0.1.10 \[stable\] would: *binaries: none (older than the newest 1)"; then
  ok "BINARY_KEEP is read: with 1, only the newest release carries binaries"
else bad "BINARY_KEEP=1 (rc=$RC): $OUTPUT"; fi
BINARY_KEEP=ten run "$S4" "$FIX"
if [ "$RC" -eq 2 ] && has "BINARY_KEEP 'ten' is not a non-negative integer"; then ok "BINARY_KEEP that is not a number is could-not-tell"; else bad "BINARY_KEEP=ten (rc=$RC): $OUTPUT"; fi

# ---- 5. a binary that disagrees with SHA256SUMS is refused by name ---------------------------
S5="$ROOT/s5"; fresh_state "$S5"
run "$S5" "$FIX_CORRUPT" --apply
if [ "$RC" -eq 1 ] && has "REFUSED v0.1.11 — binary 'tracebloc-v0.1.11-darwin-arm64' hashes to " && has "but the source release's SHA256SUMS says 0000000000000000000000000000000000000000000000000000000000000000 — not uploaded" \
   && [ "$(verdicts refused)" -eq 1 ] && [ "$(verdicts "done")" -eq 11 ] && ! grep -q 'v0.1.11' <(grep -E '^(api -X POST|release upload) ' "$GH_LOG") && has "1 release(s) were refused" \
   && [ "$(jq -r '[.[] | select(.tag_name == "v0.1.11")] | length' "$S5/mirror-releases.json")" -eq 0 ] && [ "$(jq length "$S5/mirror-releases.json")" -eq 11 ]; then
  ok "sha mismatch: the binary is named, nothing of that release is written (no tag, no release, no upload), the other 11 go ahead, exit 1"
else bad "sha mismatch (rc=$RC refused=$(verdicts refused) done=$(verdicts "done")): $OUTPUT"; fi
run "$S5" "$FIX_CORRUPT"
if [ "$RC" -eq 0 ] && [ "$(verdicts planned)" -eq 1 ] && [ "$(verdicts skipped)" -eq 11 ] && [ "$(writes)" -eq 0 ]; then
  ok "sha mismatch: a dry-run afterwards plans only the refused release (binaries are checked at apply, not fetched for a plan)"
else bad "sha mismatch dry-run after (rc=$RC planned=$(verdicts planned) skipped=$(verdicts skipped)): $OUTPUT"; fi

# ---- 6. mirror unset / equal to the source: publish-mirror's rule ----------------------------
S6="$ROOT/s6"; fresh_state "$S6"
MIRROR_REPO='' run "$S6" "$FIX" --apply; a="$RC"; o1="$OUTPUT"; l1="$(wc -l <"$GH_LOG" | tr -d ' ')"
MIRROR_REPO=src run "$S6" "$FIX" --apply; b="$RC"; o2="$OUTPUT"; l2="$(wc -l <"$GH_LOG" | tr -d ' ')"
if [ "$a" -eq 1 ] && [[ "$o1" == *"publish-mirror: REFUSED — no mirror repository is configured (MIRROR_REPO is unset)"* ]] && [[ "$o1" == *"backfill-releases: REFUSED — the mirror target was refused above"* ]] \
   && [ "$b" -eq 1 ] && [[ "$o2" == *"publish-mirror: REFUSED — mirror 'acme/src' is this repository"* ]] && [ "$l1" -eq 0 ] && [ "$l2" -eq 0 ]; then
  ok "mirror unset, or equal to the source, is refused by publish-mirror's own rule before any gh call"
else bad "mirror target (a=$a b=$b calls=$l1/$l2): $o1 / $o2"; fi
MIRROR_REPO=SRC run "$S6" "$FIX"
if [ "$RC" -eq 1 ] && has "is this repository"; then ok "mirror equal to the source is refused case-insensitively"; else bad "mirror case (rc=$RC): $OUTPUT"; fi

# ---- 7. source derived from gh repo view when SOURCE_REPO is unset ----------------------------
S7="$ROOT/s7"; fresh_state "$S7"
SOURCE_REPO='' run "$S7" "$FIX"
if [ "$RC" -eq 0 ] && has "source acme/src → mirror acme/mirror" && [ "$(head -1 "$GH_LOG")" = "repo view --json nameWithOwner" ]; then
  ok "SOURCE_REPO unset: the source is what gh repo view reports, never a hardcoded name"
else bad "source derivation (rc=$RC): $(head -2 "$GH_LOG") / $OUTPUT"; fi
if ! grep -qE 'tracebloc/cli|"tracebloc"' "$REAL"; then ok "the script hardcodes no repository name"; else bad "a repository name is hardcoded in $REAL"; fi

# ---- 8. prereleases: excluded by default, included on request, and they move the boundary -----
S8="$ROOT/s8"; fresh_state "$S8"
run "$S8" "$FIX" --include-prerelease
if [ "$RC" -eq 0 ] && [ "$(verdicts planned)" -eq 13 ] && has "13 release(s) match the filter" && has "v0.1.12-rc.1 [prerelease] would: tag: create | release: create | text: 3 up/0 skip | binaries: 2 up/0 skip" \
   && hasg "v0.1.2 \[stable\] would: *binaries: none (older than the newest 10)" && hasg "v0.1.3 \[stable\] would: *binaries: 2 up/0 skip"; then
  ok "--include-prerelease: the rc is planned, and being the newest it takes a binary slot — v0.1.2 drops out of the newest 10"
else bad "include-prerelease (rc=$RC planned=$(verdicts planned)): $OUTPUT"; fi
run "$S8" "$FIX" --include-prerelease --apply
if [ "$RC" -eq 0 ] && [ "$(grep -c -- '-F prerelease=true' "$GH_LOG")" -eq 1 ] && [ "$(grep -c -- '-f tag_name=v0.1.12-rc.1 ' "$GH_LOG")" -eq 1 ] \
   && [[ "$(grep -- '-f tag_name=v0.1.12-rc.1 ' "$GH_LOG")" == *"-f make_latest=false"* ]] && [[ "$(grep -- '-f tag_name=v0.1.11 ' "$GH_LOG")" == *"-f make_latest=true"* ]]; then
  ok "--include-prerelease --apply: the rc is created as a prerelease and is never make_latest; the newest STABLE is"
else bad "include-prerelease apply (rc=$RC): $(grep -- 'tag_name=v0.1.1' "$GH_LOG")"; fi

# ---- 9. a read that fails is could-not-tell, naming the call ----------------------------------
S9="$ROOT/s9"; fresh_state "$S9"
FAKE_GH_FAIL_RE='^api --paginate repos/acme/src/releases$' run "$S9" "$FIX" --apply; a="$RC"; o1="$OUTPUT"; w1="$(writes)"
FAKE_GH_FAIL_RE='^api --paginate repos/acme/mirror/releases$' run "$S9" "$FIX" --apply; b="$RC"; o2="$OUTPUT"; w2="$(writes)"
FAKE_GH_FAIL_RE='^release download v0.1.4 ' run "$S9" "$FIX" --apply; c="$RC"; o3="$OUTPUT"
if [ "$a" -eq 2 ] && [[ "$o1" == *"COULD NOT TELL — gh api --paginate repos/acme/src/releases failed: gh: Internal Server Error (HTTP 500)"* ]] && [ "$w1" -eq 0 ] \
   && [ "$b" -eq 2 ] && [[ "$o2" == *"COULD NOT TELL — gh api --paginate repos/acme/mirror/releases failed"* ]] && [ "$w2" -eq 0 ] \
   && [ "$c" -eq 2 ] && [[ "$o3" == *"COULD NOT TELL — gh release download v0.1.4 --repo acme/src"*"failed"* ]]; then
  ok "a failing read (source list, mirror list, an asset download) is exit 2 naming the call — never 'no releases', never a write"
else bad "api failure (a=$a b=$b c=$c w=$w1/$w2): $o1 / $o2 / $o3"; fi
if [ "$(jq -r '[.[] | select(.tag_name | test("^v0.1.[0-3]$"))] | length' "$S9/mirror-releases.json")" -eq 4 ]; then
  S9B="$ROOT/s9b"; fresh_state "$S9B"
  FAKE_GH_FAIL_RE='^release download v0.1.4 ' run "$S9B" "$FIX" --apply
  run "$S9B" "$FIX" --apply
  if [ "$RC" -eq 0 ] && [ "$(verdicts skipped)" -eq 4 ] && [ "$(verdicts "done")" -eq 8 ] && [ "$(jq length "$S9B/mirror-releases.json")" -eq 12 ]; then
    ok "resume after a mid-run failure: the completed releases are skipped, the rest are written, the mirror ends complete"
  else bad "resume (rc=$RC skipped=$(verdicts skipped) done=$(verdicts "done")): $OUTPUT"; fi
else bad "mid-run failure did not stop at v0.1.4: $(jq -c '[.[].tag_name]' "$S9/mirror-releases.json")"; fi
FAKE_GH_EMPTY_MIRROR=1 run "$S9" "$FIX" --apply
if [ "$RC" -eq 1 ] && has "REFUSED — mirror 'acme/mirror' has no commit on 'main' to anchor tags to; publish the README first" && [ "$(writes)" -eq 0 ]; then
  ok "an empty mirror is refused with instructions — tags are never anchored to an invented commit"
else bad "empty mirror (rc=$RC): $OUTPUT"; fi

# ---- 10. under --notes source a refuse-tier needle in a body refuses that release, naming the tier;
#          the default never reads the body into the notes, so the same release goes through -------
S10="$ROOT/s10"; fresh_state "$S10"
run "$S10" "$FIX_BADBODY" --apply --notes source
if [ "$RC" -eq 1 ] && has "REFUSED v0.1.5 — the guard refused the notes or a text asset: [forbidden-strings] REFUSED — [strings-refuse] needle 'arn:aws:' found in 1 staged line(s)" \
   && has "assets/RELEASE_NOTES.md:" && ! has "role/planted" && [ "$(verdicts refused)" -eq 1 ] && [ "$(verdicts "done")" -eq 11 ] \
   && ! grep -q 'v0.1.5' <(grep -E '^(api -X POST|release upload) ' "$GH_LOG") && [ "$(jq -r '[.[] | select(.tag_name == "v0.1.5")] | length' "$S10/mirror-releases.json")" -eq 0 ]; then
  ok "--notes source, forbidden string in a body: the release is refused naming the tier and the notes file, the text is not echoed, nothing of it is written, exit 1"
else bad "bad body under --notes source (rc=$RC refused=$(verdicts refused)): $OUTPUT"; fi
run "$S10" "$FIX_BADBODY" --apply
if [ "$RC" -eq 0 ] && [ "$(verdicts "done")" -eq 1 ] && [[ "$(jq -r '.[] | select(.tag_name == "v0.1.5") | .body' "$S10/mirror-releases.json")" == "tracebloc CLI v0.1.5."*"originally published 2026-01-06T12:00:00Z"* ]] \
   && ! grep -q 'role/planted' "$S10/mirror-releases.json"; then
  ok "default notes: the same release goes through with the workflow's fixed text plus the date footer; the planted body never reaches the mirror"
else bad "bad body under the default notes (rc=$RC): $OUTPUT"; fi
S10B="$ROOT/s10b"; fresh_state "$S10B"
run "$S10B" "$FIX_BADBODY" --apply
if [ "$RC" -eq 0 ] && [ "$(verdicts "done")" -eq 12 ] && [ "$(jq length "$S10B/mirror-releases.json")" -eq 12 ] && ! grep -q 'role/planted' "$S10B/mirror-releases.json"; then
  ok "default notes on a fresh mirror: all 12 written, none refused — a bad source body is not a reason to hold up a release the mirror never quotes"
else bad "fresh mirror, default notes (rc=$RC done=$(verdicts "done")): $OUTPUT"; fi

# ---- 11. present-but-different is refused, never replaced; a dangling mirror tag is refused ------
S11="$ROOT/s11"; fresh_state "$S11"
jq -n '[{id: 1, tag_name: "v0.1.9", name: "v0.1.9", body: "x", prerelease: false, draft: false, assets: [{name: "install.sh", digest: "sha256:deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"}]}]' >"$S11/mirror-releases.json"
jq -n --arg h "$HEAD_SHA" '[{ref: "refs/tags/v0.1.9", object: {sha: $h, type: "commit"}}, {ref: "refs/tags/v0.1.7", object: {sha: "9999999999999999999999999999999999999999", type: "commit"}}]' >"$S11/mirror-tags.json"
run "$S11" "$FIX" --apply
if [ "$RC" -eq 1 ] && has "REFUSED v0.1.9 — asset 'install.sh' is on the mirror with SHA256 deadbeef" && has "a published asset is never replaced" \
   && has "REFUSED v0.1.7 — tag 'v0.1.7' exists on the mirror but points at commit 9999999999999999999999999999999999999999, which the mirror does not have — a dangling tag is not repointed" \
   && [ "$(verdicts refused)" -eq 2 ] && [ "$(verdicts "done")" -eq 10 ] && ! grep -qE 'v0.1.(7|9)' <(grep -E '^(api -X POST|release upload) ' "$GH_LOG"); then
  ok "present-with-a-different-digest and a dangling mirror tag are each refused by name, nothing of those releases is written, the other 10 go ahead"
else bad "present/dangling (rc=$RC refused=$(verdicts refused) done=$(verdicts "done")): $OUTPUT"; fi

# ---- 12. --only-tag / --from-tag narrow the run, not the binary decision --------------------------
S12="$ROOT/s12"; fresh_state "$S12"
run "$S12" "$FIX" --apply --only-tag v0.1.1
if [ "$RC" -eq 0 ] && [ "$(writes)" -eq 4 ] && has "12 release(s) match the filter, 1 in this run" && [ "$(jq -r '.[0].assets | length' "$S12/mirror-releases.json")" -eq 3 ]; then
  ok "--only-tag: one release, 4 writes; v0.1.1 stays outside the newest 10 even when it is the only release in the run"
else bad "only-tag (rc=$RC writes=$(writes)): $OUTPUT"; fi
run "$S12" "$FIX" --apply --from-tag v0.1.10
if [ "$RC" -eq 0 ] && [ "$(writes)" -eq 8 ] && has "2 in this run" && [ "$(jq -r '[.[].tag_name] | join(" ")' "$S12/mirror-releases.json")" = "v0.1.1 v0.1.10 v0.1.11" ]; then
  ok "--from-tag: that release and every newer one, oldest first"
else bad "from-tag (rc=$RC writes=$(writes)): $OUTPUT / $(jq -c '[.[].tag_name]' "$S12/mirror-releases.json")"; fi
run "$S12" "$FIX" --only-tag v9.9.9; a="$RC"; o1="$OUTPUT"
run "$S12" "$FIX" --only-tag v0.1.12-rc.1; b="$RC"; o2="$OUTPUT"
run "$S12" "$FIX" --from-tag v0.1.1 --only-tag v0.1.2; c="$RC"; o3="$OUTPUT"
if [ "$a" -eq 2 ] && [[ "$o1" == *"--only-tag 'v9.9.9' is not a release of 'acme/src' matching the filter"* ]] && [ "$b" -eq 2 ] && [[ "$o2" == *"is not a release of 'acme/src' matching the filter"* ]] \
   && [ "$c" -eq 2 ] && [[ "$o3" == *"--from-tag and --only-tag exclude each other"* ]]; then
  ok "an unknown tag, a filtered-out prerelease, or both flags at once are could-not-tell"
else bad "tag flags (a=$a b=$b c=$c): $o1 / $o2 / $o3"; fi

# ---- 13. the guard is really consulted: a strict run refuses the report tier ----------------------
S13="$ROOT/s13"; fresh_state "$S13"
FIX_REPORT="$ROOT/fix-report"; build_fixtures "$FIX_REPORT"
jq '(.[] | select(.tag_name == "v0.1.6") | .body) |= . + "\n* tested against https://dev-api.tracebloc.io"' "$FIX_REPORT/src-releases.json" >"$FIX_REPORT/t.json" && mv "$FIX_REPORT/t.json" "$FIX_REPORT/src-releases.json"
run "$S13" "$FIX_REPORT" --notes source; a="$RC"; o1="$OUTPUT"
run "$S13" "$FIX_REPORT" --notes source --strict; b="$RC"; o2="$OUTPUT"
run "$S13" "$FIX_REPORT" --strict; c="$RC"; o3="$OUTPUT"
if [ "$a" -eq 0 ] && [ "$b" -eq 1 ] && [[ "$o2" == *"REFUSED v0.1.6 — the guard refused"*"[strings-report (strict)] needle 'dev-api\.tracebloc\.io' found in 1 staged line(s)"* ]]; then
  ok "--strict is passed to the guard: under --notes source a report-tier needle in a body is counted, and refuses under --strict, tier named"
else bad "strict (a=$a b=$b): $o1 / $o2"; fi
if [ "$c" -eq 0 ] && [ "$(printf '%s\n' "$o3" | awk '$NF == "planned" && $1 ~ /^v[0-9]/ { n++ } END { print n + 0 }')" -eq 12 ] && [[ "$o3" != *"dev-api"* ]]; then
  ok "--strict with the default notes: the report-tier body is never staged, so all 12 are planned — the reason fixed notes are the default"
else bad "strict default notes (c=$c): $o3"; fi

echo
printf 'backfill-releases-verify: %d passed, %d failed\n' "$PASS" "$FAIL"
[ "$FAIL" -eq 0 ] && [ "$PASS" -ge 30 ]
