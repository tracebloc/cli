#!/usr/bin/env bash
# =============================================================================
#  publish-guard.sh — stage the public deliverable of this repo and refuse
#  anything else.
#
#  The public mirror of this repo carries a DELIVERABLE, not the source tree.
#  This script builds that deliverable in a clean directory from an explicit
#  allowlist, then runs four guards over what it staged. Nothing outside the
#  allowlist can be staged (exclusion by construction), and four independent
#  scans stand between the staged tree and the push:
#
#    1. [allowlist]          .publish-include names what MAY ship. Tracked files
#                            only (`git ls-files`), matched by glob; a `!glob`
#                            line takes files back out again.
#    2. [forbidden-paths]    .publish-forbidden `[paths]`: names that must never
#                            be in the staged tree even if allowlisted by
#                            mistake (gitignore-style matching).
#    3. [forbidden-strings]  .publish-forbidden needles (extended regex,
#                            case-insensitive) scanned over every staged text
#                            file, in two tiers:
#                              [strings-refuse]  a hit refuses the publish
#                                                (mailboxes, cloud account
#                                                identifiers; the private
#                                                needles from --extra-forbidden
#                                                join this tier and are named
#                                                `private needle #N` in every
#                                                line this script prints or
#                                                writes — the pattern itself
#                                                never reaches a log).
#                              [strings-report]  hits are COUNTED and printed —
#                                                per-needle totals and the ten
#                                                most-hit files — but refuse
#                                                only under --strict. Internal
#                                                ticket references and
#                                                non-production hostnames live
#                                                here until the decision to
#                                                strip them is taken; --strict
#                                                arms that decision.
#                            `[allow]` entries are exact tokens spared before a
#                            needle is re-tested (a public support mailbox
#                            beside a rule that bans every other mailbox): a
#                            token is stripped only as a whole word, case-
#                            insensitively like the scan — `devsupport@…` is
#                            not spared by `support@…`.
#                            A needle may sit in one tier only, [strings-refuse]
#                            may not be empty, and a section header the guard
#                            does not know is refused: each of those is a list
#                            the guard cannot vouch for (exit 2).
#    4. [gitleaks]           gitleaks detect --no-git --redact over everything
#                            staged, default rules.
#
#  FAIL CLOSED. Exit 0 only when every guard RAN and every guard PASSED.
#    exit 1  a guard REFUSED — the message names the guard and the rule.
#    exit 2  COULD NOT TELL — unreadable or empty allowlist / forbidden list,
#            a malformed forbidden list (unknown section, a needle in both
#            tiers, an empty refuse tier), zero tracked files, an allowlist
#            that matched nothing, a symlink in the allowlisted set, a missing
#            or erroring scanner, a guard that did not run, a non-empty --out.
#            "Cannot tell" is never clean.
#  Every guard runs even after an earlier one has refused, so one run reports
#  everything; the exit status is the worst verdict seen.
#
#  Usage:
#    publish-guard.sh --source DIR --out DIR
#                     [--include FILE]           default DIR/.publish-include
#                     [--forbidden FILE]         default DIR/.publish-forbidden
#                     [--extra-forbidden FILE]   more refuse-tier needles (repeat
#                                                as needed); must be readable
#                                                and non-empty
#                     [--assets DIR]             release assets to publish next
#                                                to the tree; guards 2–4 scan
#                                                them too
#                     [--strict]                 a [strings-report] hit refuses
#                                                instead of being counted
#
#  Output: one line per guard, the staged file list, a final verdict.
#  OUT/tree holds the staged tree, OUT/assets the assets; OUT must not exist or
#  must be empty (a stale staging directory could carry a file no guard read).
#  A full findings report is written to OUT/publish-guard-report.txt.
#
#  Environment (tests only): PUBLISH_GUARD_GITLEAKS names the gitleaks binary.
# =============================================================================
set -uo pipefail

SOURCE=""; OUT=""; INCLUDE=""; FORBIDDEN=""; ASSETS=""; STRICT=0
EXTRA_FORBIDDEN=()
while [ "$#" -gt 0 ]; do
  case "$1" in
    --source)          SOURCE="${2:-}"; shift 2 ;;
    --out)             OUT="${2:-}"; shift 2 ;;
    --include)         INCLUDE="${2:-}"; shift 2 ;;
    --forbidden)       FORBIDDEN="${2:-}"; shift 2 ;;
    --extra-forbidden) EXTRA_FORBIDDEN+=("${2:-}"); shift 2 ;;
    --assets)          ASSETS="${2:-}"; shift 2 ;;
    --strict)          STRICT=1; shift ;;
    -h|--help)         sed -n '2,/^# ====/p' "$0" | sed 's/^# \{0,2\}//'; exit 0 ;;
    *) echo "publish-guard: unknown argument '$1'" >&2; exit 2 ;;
  esac
done

# ---- verdict bookkeeping -----------------------------------------------------
# WORST is the exit status: 0 clean, 1 refused, 2 could not tell. RAN counts the
# guards that reached a verdict; the final check refuses to report green unless
# all four did — a refactor that drops a stage must not look like a clean run.
WORST=0
RAN=0
GUARDS_EXPECTED=4
worsen() { [ "$1" -gt "$WORST" ] && WORST="$1"; return 0; }
# The workflow-command prefix goes to STDOUT: Actions reads ::error:: from
# stdout only. Plain lines are the guard's narration.
refuse()   { echo "::error::publish-guard: [$1] REFUSED — $2"; worsen 1; }
cant_tell(){ echo "::error::publish-guard: [$1] COULD NOT TELL — $2 (never reported as clean)"; worsen 2; }
note()     { echo "publish-guard: [$1] $2"; }
# A guard error before any guard can run: nothing to stage, nothing to report.
die2() { echo "::error::publish-guard: COULD NOT TELL — $1 (never reported as clean)"; exit 2; }

[ -n "$SOURCE" ] || die2 "--source is required"
[ -n "$OUT" ]    || die2 "--out is required"
[ -d "$SOURCE" ] || die2 "--source '$SOURCE' is not a directory"
SOURCE="$(cd "$SOURCE" && pwd)"
[ -n "$INCLUDE" ]   || INCLUDE="$SOURCE/.publish-include"
[ -n "$FORBIDDEN" ] || FORBIDDEN="$SOURCE/.publish-forbidden"
if [ -e "$OUT" ]; then
  [ -d "$OUT" ] || die2 "--out '$OUT' exists and is not a directory"
  [ -z "$(ls -A "$OUT")" ] || die2 "--out '$OUT' is not empty; a stale staging directory could carry a file no guard read"
fi
mkdir -p "$OUT/tree" || die2 "cannot create '$OUT/tree'"
OUT="$(cd "$OUT" && pwd)"
TREE="$OUT/tree"

# Scratch, armed only once it exists (a failed mktemp must not make the trap
# expand to `rm -rf /*`).
TMP="$(mktemp -d "${TMPDIR:-/tmp}/publish-guard.XXXXXX")" && [ -d "$TMP" ] || die2 "could not create a scratch directory"
trap 'rm -rf "$TMP"' EXIT
REPORT="$TMP/report.txt"
: >"$REPORT"

# ---- list files: strip comments and blanks, keep order ------------------------
# A section header is a line that is nothing but one bracketed token. The match
# is deliberately loose (`[strings refuse]`, `[Strings-Refuse]` are headers too)
# so a misspelt header is refused by name below instead of being read as a
# needle of the section before it.
SECTION_RE='^[[][^]]*[]][[:space:]]*$'   # bracket expressions, so no awk escape processing applies
# read_list FILE SECTION — print the entries of SECTION ([paths] /
# [strings-refuse] / [strings-report] / [allow]) from a sectioned list file;
# SECTION "" prints every entry of a file that has no section headers (the
# allowlist, an --extra-forbidden list).
read_list() {
  awk -v want="$2" -v hdr="$SECTION_RE" '
    /^[[:space:]]*(#|$)/ { next }
    $0 ~ hdr { sec = $0; sub(/^\[/, "", sec); sub(/\].*$/, "", sec); next }
    { line = $0; sub(/[[:space:]]+$/, "", line)
      if (want == "" || sec == want) print line }
  ' "$1"
}

# The sections the forbidden list may declare. Both guards that read the list
# check every header against this set: a header the guard does not read would
# silently orphan the rules under it.
FORBIDDEN_SECTIONS="paths strings-refuse strings-report allow"
# forbidden_sections_ok GUARD — could-not-tell (and return 1) on the first
# header of $FORBIDDEN that is not one of FORBIDDEN_SECTIONS.
forbidden_sections_ok() {
  local sec
  while IFS= read -r sec; do
    case " $FORBIDDEN_SECTIONS " in
      *" $sec "*) ;;
      *) cant_tell "$1" "'$FORBIDDEN' has an unknown section [$sec] — the guard reads only [${FORBIDDEN_SECTIONS// /] [}]"; return 1 ;;
    esac
  done < <(awk -v hdr="$SECTION_RE" '$0 ~ hdr { sec = $0; sub(/^\[/, "", sec); sub(/\].*$/, "", sec); print sec }' "$FORBIDDEN")
  return 0
}

# glob_to_ere GLOB — an anchored extended regex for a path glob: `*` and `?` do
# not cross `/`, `**` does (`**/` also matches zero directories). Every other
# regex metacharacter in the glob is escaped, so a `.` in `*.go` is a dot.
glob_to_ere() {
  local g="$1" out="" i c n
  n=${#g}
  for ((i = 0; i < n; i++)); do
    c="${g:i:1}"
    case "$c" in
      '*')
        if [ "${g:i+1:1}" = '*' ]; then
          if [ "${g:i+2:1}" = '/' ]; then out+='(.*/)?'; i=$((i + 2)); else out+='.*'; i=$((i + 1)); fi
        else
          out+='[^/]*'
        fi ;;
      '?') out+='[^/]' ;;
      '['|']'|'.'|'^'|'$'|'+'|'('|')'|'{'|'}'|'|'|'\') out+="\\$c" ;;
      *) out+="$c" ;;
    esac
  done
  printf '^%s$' "$out"
}

# ---- guard 1: allowlist ---------------------------------------------------------
guard_allowlist() {
  local g="allowlist" n_inc=0 n_exc=0 line re
  local -a inc_re=() exc_re=()
  if [ ! -r "$INCLUDE" ]; then cant_tell "$g" "allowlist '$INCLUDE' is missing or unreadable"; RAN=$((RAN + 1)); return; fi
  while IFS= read -r line; do
    case "$line" in
      '!'*) exc_re+=("$(glob_to_ere "${line#!}")"); n_exc=$((n_exc + 1)) ;;
      *)    inc_re+=("$(glob_to_ere "$line")"); n_inc=$((n_inc + 1)) ;;
    esac
  done < <(read_list "$INCLUDE" "")
  if [ "$n_inc" -eq 0 ]; then cant_tell "$g" "allowlist '$INCLUDE' lists no include entries — nothing may ship, so nothing can be vouched for"; RAN=$((RAN + 1)); return; fi

  # Tracked files only: an untracked file in the checkout is never a deliverable.
  # A path containing a newline is unrepresentable in the line-oriented list
  # below, so the NUL-separated count must equal the line count.
  local listed nul_count line_count
  listed="$TMP/tracked.txt"
  if ! git -C "$SOURCE" -c core.quotePath=false ls-files >"$listed" 2>"$TMP/git.err"; then
    cant_tell "$g" "git ls-files failed in '$SOURCE': $(tr '\n' ' ' <"$TMP/git.err")"; RAN=$((RAN + 1)); return
  fi
  nul_count="$(git -C "$SOURCE" ls-files -z | tr -cd '\0' | wc -c | tr -d ' ')"
  line_count="$(wc -l <"$listed" | tr -d ' ')"
  if [ "$line_count" -eq 0 ]; then cant_tell "$g" "'$SOURCE' has zero tracked files"; RAN=$((RAN + 1)); return; fi
  if [ "$nul_count" != "$line_count" ]; then cant_tell "$g" "a tracked path contains a newline ($nul_count entries, $line_count lines) — cannot match it safely"; RAN=$((RAN + 1)); return; fi

  local staged=0 f matched
  : >"$TMP/staged.txt"
  while IFS= read -r f; do
    matched=0
    for re in "${inc_re[@]}"; do [[ "$f" =~ $re ]] && { matched=1; break; }; done
    [ "$matched" -eq 1 ] || continue
    for re in "${exc_re[@]+"${exc_re[@]}"}"; do [[ "$f" =~ $re ]] && { matched=0; break; }; done
    [ "$matched" -eq 1 ] || continue
    if [ -L "$SOURCE/$f" ]; then cant_tell "$g" "'$f' is a symlink — a link can point outside the tree, so it is not staged"; RAN=$((RAN + 1)); return; fi
    [ -f "$SOURCE/$f" ] || { cant_tell "$g" "tracked file '$f' is missing from the checkout"; RAN=$((RAN + 1)); return; }
    mkdir -p "$TREE/$(dirname "$f")" || { cant_tell "$g" "cannot create '$TREE/$(dirname "$f")'"; RAN=$((RAN + 1)); return; }
    cp -p "$SOURCE/$f" "$TREE/$f" || { cant_tell "$g" "cannot copy '$f'"; RAN=$((RAN + 1)); return; }
    printf '%s\n' "$f" >>"$TMP/staged.txt"
    staged=$((staged + 1))
  done <"$listed"
  if [ "$staged" -eq 0 ]; then cant_tell "$g" "the allowlist matched none of the $line_count tracked files — a mirror with nothing in it is not a deliverable"; RAN=$((RAN + 1)); return; fi
  note "$g" "staged $staged of $line_count tracked file(s) ($n_inc include, $n_exc exclude pattern(s)):"
  sort "$TMP/staged.txt" | sed 's/^/    /'
  RAN=$((RAN + 1))
}

# ---- assets ----------------------------------------------------------------------
stage_assets() {
  [ -n "$ASSETS" ] || return 0
  [ -d "$ASSETS" ] || die2 "--assets '$ASSETS' is not a directory"
  local n
  n="$(find "$ASSETS" -mindepth 1 -maxdepth 1 -type f | wc -l | tr -d ' ')"
  [ "$n" -gt 0 ] || die2 "--assets '$ASSETS' holds no files — a release with no assets is not what a customer downloads"
  [ "$(find "$ASSETS" -mindepth 1 -maxdepth 1 ! -type f | wc -l | tr -d ' ')" -eq 0 ] || die2 "--assets '$ASSETS' holds something other than plain files (a directory or a symlink)"
  mkdir -p "$OUT/assets" && cp -p "$ASSETS"/* "$OUT/assets"/ || die2 "cannot copy assets from '$ASSETS'"
  note "assets" "staged $n release asset(s):"
  find "$OUT/assets" -mindepth 1 -maxdepth 1 -type f | sed "s|^$OUT/assets/||" | sort | sed 's/^/    /'
}

# staged_paths — every staged path as `<area>:<relative path>` (area = tree or
# assets), one per line.
staged_paths() {
  ( cd "$OUT" && find tree assets -type f 2>/dev/null ) | sed -E 's#^(tree|assets)/#\1:#' | sort
}

# ---- guard 2: forbidden paths ----------------------------------------------------
# gitignore-style: a pattern with a `/` inside it is anchored to the staged root
# (`scripts/tests/` matches only that directory); one without matches ANY path
# component (`tests/` matches `client/tests/x`, `*.go` matches `a/b/c.go`); a
# trailing `/` means "as a directory" (`tests/` does not match a file named
# tests). The staged area prefix (tree/, assets/) is not part of the path.
path_pattern_hits() { # $1 = pattern, reads staged paths on stdin, prints hits
  local pat="$1" dir_only=0 anchored=0 re
  case "$pat" in */) dir_only=1; pat="${pat%/}" ;; esac
  pat="${pat#/}"
  case "$pat" in */*) anchored=1 ;; esac
  re="$(glob_to_ere "$pat")"
  local entry p comp
  local -a comps
  while IFS= read -r entry; do
    p="${entry#*:}"
    if [ "$anchored" -eq 1 ]; then
      if [ "$dir_only" -eq 0 ] && [[ "$p" =~ $re ]]; then printf '%s\n' "$entry"; continue; fi
      [[ "$p/" == "${pat}/"* ]] && printf '%s\n' "$entry"
      continue
    fi
    IFS='/' read -r -a comps <<<"$p"
    local i last=$(( ${#comps[@]} - 1 ))
    for i in "${!comps[@]}"; do
      comp="${comps[$i]}"
      [ "$dir_only" -eq 1 ] && [ "$i" -eq "$last" ] && continue
      if [[ "$comp" =~ $re ]]; then printf '%s\n' "$entry"; break; fi
    done
  done
}

guard_forbidden_paths() {
  local g="forbidden-paths" n=0 pat hits total=0
  if [ ! -r "$FORBIDDEN" ]; then cant_tell "$g" "forbidden list '$FORBIDDEN' is missing or unreadable"; RAN=$((RAN + 1)); return; fi
  forbidden_sections_ok "$g" || { RAN=$((RAN + 1)); return; }
  read_list "$FORBIDDEN" paths >"$TMP/paths.txt"
  n="$(grep -c . "$TMP/paths.txt" || true)"
  if [ "$n" -eq 0 ]; then cant_tell "$g" "'$FORBIDDEN' has no [paths] entries — a scan with no rules proves nothing"; RAN=$((RAN + 1)); return; fi
  staged_paths >"$TMP/all.txt"
  while IFS= read -r pat; do
    hits="$(path_pattern_hits "$pat" <"$TMP/all.txt")"
    [ -n "$hits" ] || continue
    total=$((total + $(printf '%s\n' "$hits" | grep -c .)))
    refuse "$g" "forbidden path pattern '$pat' matched:"
    printf '%s\n' "$hits" | sed 's/^/    /' | tee -a "$REPORT"
  done <"$TMP/paths.txt"
  [ "$total" -gt 0 ] || note "$g" "clean ($n pattern(s) against $(grep -c . "$TMP/all.txt") staged path(s))"
  RAN=$((RAN + 1))
}

# ---- guard 3: forbidden strings --------------------------------------------------
# Two tiers over the same scan. A [strings-refuse] needle (or any needle from
# --extra-forbidden) refuses on a hit. A [strings-report] needle is counted and
# printed — per-needle totals and the ten most-hit files — and refuses only
# under --strict: the tier can be measured on the real deliverable before the
# decision to strip it is taken, and one flag arms that decision.
# Text files only (`grep -I`): a binary asset is opaque to a string scan; its
# integrity is the release's own SHA256SUMS + signature. The count of binaries
# skipped is printed so "scanned everything" and "skipped half" read differently.
guard_forbidden_strings() {
  local g="forbidden-strings" needle rc hits n_refuse n_report n_allow=0 extra dup
  if [ ! -r "$FORBIDDEN" ]; then cant_tell "$g" "forbidden list '$FORBIDDEN' is missing or unreadable"; RAN=$((RAN + 1)); return; fi
  forbidden_sections_ok "$g" || { RAN=$((RAN + 1)); return; }
  read_list "$FORBIDDEN" strings-refuse >"$TMP/needles-refuse.txt"
  read_list "$FORBIDDEN" strings-report >"$TMP/needles-report.txt"
  read_list "$FORBIDDEN" allow          >"$TMP/allow.txt"
  # One tier per needle: the same text in both would be refused by one loop and
  # counted by the other, and whichever the reader saw first would be the rule.
  dup="$(comm -12 <(sort -u "$TMP/needles-refuse.txt") <(sort -u "$TMP/needles-report.txt") | grep . | head -1)"
  if [ -n "$dup" ]; then cant_tell "$g" "'$FORBIDDEN' lists needle '$dup' in both [strings-refuse] and [strings-report] — a needle has one tier"; RAN=$((RAN + 1)); return; fi
  # The committed refuse tier is judged BEFORE the private needles join it: a
  # list whose only hard rules arrive from a secret is misconfigured.
  n_refuse="$(grep -c . "$TMP/needles-refuse.txt" || true)"
  if [ "$n_refuse" -eq 0 ]; then cant_tell "$g" "'$FORBIDDEN' has no [strings-refuse] entries — a guard with nothing to refuse is misconfigured"; RAN=$((RAN + 1)); return; fi
  for extra in "${EXTRA_FORBIDDEN[@]+"${EXTRA_FORBIDDEN[@]}"}"; do
    if [ ! -r "$extra" ]; then cant_tell "$g" "extra forbidden list '$extra' is missing or unreadable"; RAN=$((RAN + 1)); return; fi
    if [ "$(read_list "$extra" "" | grep -c .)" -eq 0 ]; then cant_tell "$g" "extra forbidden list '$extra' is empty — the private needles were not supplied, so this scan cannot vouch for them"; RAN=$((RAN + 1)); return; fi
    read_list "$extra" "" >>"$TMP/needles-private.txt"
  done
  : >>"$TMP/needles-private.txt"
  n_refuse="$(( $(grep -c . "$TMP/needles-refuse.txt" || true) + $(grep -c . "$TMP/needles-private.txt" || true) ))"
  n_report="$(grep -c . "$TMP/needles-report.txt" || true)"
  n_allow="$(grep -c . "$TMP/allow.txt" || true)"

  # Census of what the scan can and cannot see.
  local n_text=0 n_bin=0 f
  while IFS= read -r f; do
    if [ "$(tr -d -c '\000' <"$f" | wc -c | tr -d ' ')" -gt 0 ]; then n_bin=$((n_bin + 1)); else n_text=$((n_text + 1)); fi
  done < <(find "$OUT/tree" "$OUT/assets" -type f 2>/dev/null)
  if [ "$n_text" -eq 0 ]; then cant_tell "$g" "no text file staged — nothing this scan can read"; RAN=$((RAN + 1)); return; fi

  local -a scan_dirs=("$OUT/tree")
  [ -d "$OUT/assets" ] && scan_dirs+=("$OUT/assets")
  local allow_expr
  allow_expr="$(paste -sd'|' "$TMP/allow.txt")"
  # needle_hits NEEDLE SHOWN — write the `area/file:line` locations NEEDLE
  # matches, after [allow] stripping, to $TMP/hits.txt. Returns 2 when grep
  # itself failed, with the reason in $GREP_ERR; the caller reports
  # could-not-tell. SHOWN is how the needle is named in any message: the
  # pattern for a committed needle, `private needle #N` for one that came from
  # --extra-forbidden — those are the identifiers kept out of the public list,
  # and this log is public too.
  needle_hits() {
    local needle="$1" shown="$2" rc
    # Hits go through a FILE, never `producer | grep -q`: a closed pipe would
    # turn a real finding into "clean" via SIGPIPE.
    grep -rIinE -e "$needle" "${scan_dirs[@]}" >"$TMP/hits.txt" 2>"$TMP/grep.err"; rc=$?
    if [ "$rc" -ge 2 ]; then GREP_ERR="grep exited $rc on $shown: $(tr '\n' ' ' <"$TMP/grep.err")"; return 2; fi
    if [ "$rc" -ne 0 ]; then : >"$TMP/hits.txt"; return 0; fi
    # [allow] tokens are removed from each hit line and the needle re-tested, so
    # a line is spared only when the allowed token was the whole reason it hit.
    # A token is removed only as a WHOLE word — not when it is the tail of a
    # longer mailbox (`devsupport@…`) or the head of a longer domain — and
    # case-insensitively, as the scan itself matches. A sentence-ending `.`
    # after the token is still a boundary.
    # Split each hit into its location and its text; only the TEXT is re-tested,
    # so the `file:line` prefix can never be what matches.
    awk -F: '{ print $1 ":" $2 }' "$TMP/hits.txt" >"$TMP/locs.txt"
    sed -E 's/^[^:]*:[^:]*://' "$TMP/hits.txt" >"$TMP/texts.txt"
    if [ "$n_allow" -gt 0 ]; then
      sed -E "s#(^|[^[:alnum:]._%+-])($allow_expr)($|[^[:alnum:]._%+-]|\.([^[:alnum:]]|$))#\1 \3#gI" "$TMP/texts.txt" >"$TMP/texts2.txt" && mv "$TMP/texts2.txt" "$TMP/texts.txt"
    fi
    grep -inE -e "$needle" "$TMP/texts.txt" | cut -d: -f1 >"$TMP/kept.txt"; rc=${PIPESTATUS[0]}
    if [ "$rc" -ge 2 ]; then GREP_ERR="re-test after [allow] stripping exited $rc on $shown"; return 2; fi
    awk 'NR == FNR { keep[$1] = 1; next } (FNR in keep)' "$TMP/kept.txt" "$TMP/locs.txt" | sed "s|^$OUT/||" >"$TMP/hits.txt"
    return 0
  }

  # Three passes: the committed refuse tier, the private needles (refuse tier,
  # named by number only), the report tier.
  local tier label shown k n_refused=0 n_reported=0
  : >"$TMP/report-locs.txt"
  for tier in refuse private report; do
    k=0
    while IFS= read -r needle; do
      k=$((k + 1))
      if [ "$tier" = private ]; then shown="private needle #$k"; else shown="needle '$needle'"; fi
      needle_hits "$needle" "$shown" || { cant_tell "$g" "$GREP_ERR"; RAN=$((RAN + 1)); return; }
      hits="$(grep -c . "$TMP/hits.txt" || true)"
      [ "$hits" -gt 0 ] || continue
      if [ "$tier" != report ]; then
        { echo "[strings-refuse] $shown:"; cat "$TMP/hits.txt"; } >>"$REPORT"
        n_refused=$((n_refused + hits)); label="strings-refuse"
      else
        { echo "[strings-report] $shown:"; cat "$TMP/hits.txt"; } >>"$REPORT"
        n_reported=$((n_reported + hits)); cat "$TMP/hits.txt" >>"$TMP/report-locs.txt"
        if [ "$STRICT" -eq 1 ]; then
          label="strings-report (strict)"
        else
          note "$g" "[strings-report] $shown found in $hits staged line(s) — counted, not refused (--strict refuses)"
          continue
        fi
      fi
      refuse "$g" "[$label] $shown found in $hits staged line(s):"
      head -20 "$TMP/hits.txt" | sed 's/^/    /'
      [ "$hits" -le 20 ] || echo "    … and $((hits - 20)) more (full list in publish-guard-report.txt)"
    done <"$TMP/needles-$tier.txt"
  done
  if [ "$n_reported" -gt 0 ]; then
    # Where the report tier lands, so the clean-up (or the decision not to) has
    # a map: count per file, ten most-hit first.
    sed 's/:[0-9]*$//' "$TMP/report-locs.txt" | sort | uniq -c | sort -rn >"$TMP/report-files.txt"
    note "$g" "[strings-report] $n_reported hit(s) in $(grep -c . "$TMP/report-files.txt") file(s); most-hit files:"
    head -10 "$TMP/report-files.txt" | awk '{ n = $1; sub(/^ *[0-9]+ /, ""); printf "    %6d  %s\n", n, $0 }'
  fi
  local tally="$n_refuse refuse + $n_report report needle(s), $n_allow allow token(s); $n_text text file(s) scanned, $n_bin binary file(s) opaque to this scan"
  if [ "$n_refused" -eq 0 ] && [ "$n_reported" -eq 0 ]; then
    note "$g" "clean ($tally)"
  elif [ "$STRICT" -eq 1 ]; then
    note "$g" "$n_refused refuse-tier hit(s), $n_reported report-tier hit(s) refused under --strict ($tally)"
  else
    note "$g" "$n_refused refuse-tier hit(s), $n_reported report-tier hit(s) counted ($tally)"
  fi
  RAN=$((RAN + 1))
}

# ---- guard 4: gitleaks -----------------------------------------------------------
guard_gitleaks() {
  local g="gitleaks" bin="${PUBLISH_GUARD_GITLEAKS:-gitleaks}" rc
  if ! command -v "$bin" >/dev/null 2>&1; then cant_tell "$g" "scanner '$bin' is not on PATH — a scan that did not run is not a clean scan"; RAN=$((RAN + 1)); return; fi
  # Leaks exit with a code no crash uses (default 1 is also "something broke"),
  # so a scanner failure cannot be misread as either verdict.
  "$bin" detect --no-git --redact --no-banner --exit-code 9 --source "$OUT" >"$TMP/gitleaks.out" 2>&1; rc=$?
  case "$rc" in
    0) note "$g" "clean ($("$bin" version 2>/dev/null | head -1 || echo 'version unknown'), default rules, $(staged_paths | grep -c .) staged file(s))" ;;
    9) refuse "$g" "secrets detected in the staged tree:"; grep -vE '^[0-9]+:[0-9]+[AP]M' "$TMP/gitleaks.out" | sed 's/^/    /' | tee -a "$REPORT" ;;
    *) cant_tell "$g" "scanner exited $rc: $(tail -3 "$TMP/gitleaks.out" | tr '\n' ' ')" ;;
  esac
  RAN=$((RAN + 1))
}

# ---- run everything, then judge ---------------------------------------------------
guard_allowlist
stage_assets
guard_forbidden_paths
guard_forbidden_strings
guard_gitleaks

cp "$REPORT" "$OUT/publish-guard-report.txt" 2>/dev/null || true

if [ "$RAN" -ne "$GUARDS_EXPECTED" ]; then
  cant_tell "self-check" "$RAN of $GUARDS_EXPECTED guards reached a verdict"
fi
case "$WORST" in
  0) echo "publish-guard: OK — all $GUARDS_EXPECTED guards ran and passed; $OUT/tree is the deliverable." ;;
  1) echo "::error::publish-guard: REFUSED — do not publish $OUT (see the [guard] lines above)." ;;
  *) echo "::error::publish-guard: COULD NOT TELL — do not publish $OUT (see the [guard] lines above)." ;;
esac
exit "$WORST"
