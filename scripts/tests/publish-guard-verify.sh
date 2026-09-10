#!/usr/bin/env bash
# =============================================================================
#  publish-guard-verify.sh — pin the properties of scripts/publish-guard.sh,
#  the staging guard in front of the public mirror.
#
#  Driven the same way install-verify.sh drives install.sh: the REAL script is
#  executed against a fixture git repository this harness builds, with gitleaks
#  replaced by a PATH shim so the verdict plumbing is exercised hermetically.
#  The allowlist and forbidden list the fixtures use are written HERE — never
#  read from the repo's own .publish-include / .publish-forbidden — so the guard
#  is not tested against its own copy of the rule. The repo's real lists get
#  their own cases at the end, fed inputs this file writes.
#
#  Three verdicts, and every case names the one it expects: 0 clean, 1 refused
#  (the `[guard]` and the offending path or needle are asserted, not just the
#  exit code), 2 could not tell.
# =============================================================================
# pipefail so a failing producer is not masked. Deliberately NO -e: this harness
# counts its own pass/fail and must survive a failed assertion.
set -uo pipefail

SELF_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO="$(cd "$SELF_DIR/../.." && pwd)"
GUARD="$REPO/scripts/publish-guard.sh"
[ -f "$GUARD" ] || { printf 'publish-guard-verify: %s missing — refusing to report clean\n' "$GUARD" >&2; exit 2; }

PASS=0
FAIL=0
ok()  { printf '  ok   %s\n' "$1"; PASS=$((PASS+1)); }
bad() { printf '  FAIL %s\n' "$1"; FAIL=$((FAIL+1)); }

ROOT="$(mktemp -d "${TMPDIR:-/tmp}/publish-guard-verify.XXXXXX")"
trap 'rm -rf "$ROOT"' EXIT
SHIM="$ROOT/shim"; mkdir -p "$SHIM"
cat >"$SHIM/gitleaks" <<'EOF'
#!/usr/bin/env bash
case "${1:-}" in version) echo "shim-9.9.9"; exit 0 ;; esac
echo "shim gitleaks ran: $*"
case "${GL_MODE:-clean}" in
  clean) exit 0 ;;
  leak)  echo "Finding: REDACTED"; exit 9 ;;
  crash) echo "panic: shim crash"; exit 1 ;;
esac
EOF
chmod +x "$SHIM/gitleaks"
export PUBLISH_GUARD_GITLEAKS="$SHIM/gitleaks"

# ---- fixture ---------------------------------------------------------------------
SRC=""; OUT=""
add_file() { mkdir -p "$SRC/$(dirname "$1")"; printf '%s\n' "$2" >"$SRC/$1"; git -C "$SRC" add -f "$1"; }
commit()   { git -C "$SRC" commit -q -m fixture --allow-empty; }
write_include()   { printf '# fixture allowlist\n' >"$SRC/.publish-include"; printf '%s\n' "$@" >>"$SRC/.publish-include"; }
write_forbidden() {
  {
    printf '[paths]\n'; printf '%s\n' 'tests/' 'scripts/tests/' 'Makefile' 'CLAUDE.md' '.github/' '*.go' 'go.mod' 'kubeconfig*'
    printf '\n[strings-refuse]\n'; printf '%s\n' '[A-Za-z0-9._%+-]+@tracebloc\.io' 'arn:aws:'
    printf '\n[strings-report]\n'; printf '%s\n' 'backend#' 'RFC-0' 'dev-api\.tracebloc\.io'
    printf '\n[allow]\n'; printf '%s\n' 'support@tracebloc\.io'
  } >"$SRC/.publish-forbidden"
}
# fresh NAME — a new fixture repo + empty out dir under $ROOT/NAME.
fresh() {
  SRC="$ROOT/$1/src"; OUT="$ROOT/$1/out"
  mkdir -p "$SRC"
  git -C "$SRC" init -q
  git -C "$SRC" config user.email t@example.invalid
  git -C "$SRC" config user.name t
  add_file README.md 'Fixture CLI. Help: support@tracebloc.io'
  add_file LICENSE 'Apache-2.0'
  add_file docs/usage.md 'usage'
  add_file docs/rfcs/0001.md 'rfc'
  add_file scripts/install.sh '#!/bin/sh'
  add_file scripts/tests/x-verify.sh 'x'
  add_file Makefile 'all:'
  add_file CLAUDE.md 'guidance'
  add_file go.mod 'module x'
  add_file internal/cli/main.go 'package cli'
  add_file .github/workflows/ci.yml 'on: push'
  commit
  write_include 'README.md' 'LICENSE' 'docs/*.md'
  write_forbidden
}
# plant PATH LINE — append, commit, and PROVE the mutation landed.
plant() {
  printf '%s\n' "$2" >>"$SRC/$1"; git -C "$SRC" add "$1"; commit
  [ "$(grep -cF -- "$2" "$SRC/$1")" -eq 1 ] || { bad "mutation did not land in $1"; return 1; }
}
guard() { OUTPUT="$(bash "$GUARD" --source "$SRC" --out "$OUT" "$@" 2>&1)"; RC=$?; }
has()   { [[ "$OUTPUT" == *"$1"* ]]; }
staged(){ ( cd "$OUT/tree" && find . -type f | sed 's|^\./||' | sort | paste -sd' ' - ); }

echo "== publish-guard.sh harness =="

# ---- clean case ---------------------------------------------------------------------
fresh clean; guard
if [ "$RC" -eq 0 ] && has "[allowlist] staged 3 of 11 tracked file(s)" && has "[forbidden-paths] clean (8 pattern(s) against 3 staged path(s))" \
   && has "[forbidden-strings] clean (2 refuse + 3 report needle(s), 1 allow token(s); 3 text file(s) scanned, 0 binary" && has "[gitleaks] clean" \
   && has "publish-guard: OK — all 4 guards ran and passed" && [ "$(staged)" = "LICENSE README.md docs/usage.md" ]; then
  ok "clean fixture: exactly the allowlisted files are staged, all four guards report, exit 0"
else bad "clean fixture (rc=$RC, staged='$(staged)'): $OUTPUT"; fi

fresh untracked; printf 'x\n' >"$SRC/docs/scratch.md"; guard
if [ "$RC" -eq 0 ] && [ ! -e "$OUT/tree/docs/scratch.md" ]; then ok "an untracked file matching the allowlist is not staged"; else bad "untracked file (rc=$RC): $OUTPUT"; fi

# docs/*.md is one level: docs/rfcs/0001.md is not staged by the glob at all.
fresh onelevel; guard
if [ "$RC" -eq 0 ] && [ ! -e "$OUT/tree/docs/rfcs" ]; then ok "docs/*.md does not cross into docs/rfcs/ (glob * does not cross /)"; else bad "one-level glob (rc=$RC)"; fi

# ---- guard 2: forbidden paths -----------------------------------------------------------
fresh gofile; write_include 'README.md' 'internal/**'; guard
if [ "$RC" -eq 1 ] && has "[forbidden-paths] REFUSED — forbidden path pattern '*.go' matched:" && has "tree:internal/cli/main.go" \
   && has "[forbidden-strings] clean" && has "[gitleaks] clean"; then
  ok "mutation: allowlisting Go source is refused by *.go, and the later guards still run"
else bad "go source (rc=$RC): $OUTPUT"; fi

fresh gomod; write_include 'README.md' 'go.mod' 'Makefile' 'CLAUDE.md'; guard
if [ "$RC" -eq 1 ] && has "pattern 'go.mod' matched:" && has "tree:go.mod" && has "pattern 'Makefile' matched:" && has "pattern 'CLAUDE.md' matched:"; then
  ok "mutation: go.mod, Makefile and CLAUDE.md are each refused by name"
else bad "go.mod/Makefile/CLAUDE.md (rc=$RC): $OUTPUT"; fi

fresh workflows; write_include 'README.md' '.github/**'; guard
if [ "$RC" -eq 1 ] && has "pattern '.github/' matched:" && has "tree:.github/workflows/ci.yml"; then ok "mutation: a workflow directory is refused by .github/"; else bad ".github (rc=$RC): $OUTPUT"; fi

fresh anchored; write_include 'README.md' 'scripts/tests/**'; printf '[paths]\nscripts/tests/\n[strings-refuse]\narn:aws:\n' >"$SRC/.publish-forbidden"; guard
if [ "$RC" -eq 1 ] && has "pattern 'scripts/tests/' matched:" && has "tree:scripts/tests/x-verify.sh"; then ok "an anchored directory pattern refuses the root-level directory"; else bad "anchored (rc=$RC): $OUTPUT"; fi

fresh asset-kube; mkdir -p "$ROOT/asset-kube/assets"; printf 'k\n' >"$ROOT/asset-kube/assets/kubeconfig"; guard --assets "$ROOT/asset-kube/assets"
if [ "$RC" -eq 1 ] && has "pattern 'kubeconfig*' matched:" && has "assets:kubeconfig"; then ok "a release asset named like a credential file is refused"; else bad "asset kubeconfig (rc=$RC): $OUTPUT"; fi

fresh nopaths; printf '[strings-refuse]\narn:aws:\n' >"$SRC/.publish-forbidden"; guard
if [ "$RC" -eq 2 ] && has "[forbidden-paths] COULD NOT TELL" && has "has no [paths] entries"; then ok "no [paths] entries is could-not-tell"; else bad "no paths (rc=$RC): $OUTPUT"; fi

# ---- guard 3: forbidden strings — the refuse tier ----------------------------------------
fresh ref; plant README.md 'role arn:aws:iam::000000000000:role/planted' && guard
if [ "$RC" -eq 1 ] && has "[forbidden-strings] REFUSED — [strings-refuse] needle 'arn:aws:' found in 1 staged line(s):" && has "    tree/README.md:2" && ! has "role/planted" \
   && has "[forbidden-strings] 1 refuse-tier hit(s), 0 report-tier hit(s) counted"; then
  ok "mutation: a refuse-tier needle is refused; tier and file:line named, text not echoed"
else bad "refuse tier (rc=$RC): $OUTPUT"; fi

fresh case; plant README.md 'ARN:AWS:s3:::planted' && guard
if [ "$RC" -eq 1 ] && has "[strings-refuse] needle 'arn:aws:' found in 1 staged line(s)"; then ok "needles match case-insensitively"; else bad "case (rc=$RC): $OUTPUT"; fi

fresh allow; guard; a="$RC"; rm -rf "$OUT"; plant README.md 'or someone@tracebloc.io / support@tracebloc.io' && guard
if [ "$a" -eq 0 ] && [ "$RC" -eq 1 ] && has "[strings-refuse] needle '[A-Za-z0-9._%+-]+@tracebloc\.io' found in 1 staged line(s):" && has "tree/README.md:2"; then
  ok "[allow] spares the support mailbox alone, not a personal mailbox beside it"
else bad "allow (first rc=$a, second rc=$RC): $OUTPUT"; fi

# The unanchored strip this replaces left `dev` behind and the mailbox rule no
# longer matched, so an internal address ending in the public one shipped.
fresh allow-tail; plant README.md 'escalate to devsupport@tracebloc.io' && guard
if [ "$RC" -eq 1 ] && has "[strings-refuse] needle '[A-Za-z0-9._%+-]+@tracebloc\.io' found in 1 staged line(s):" && has "tree/README.md:2"; then
  ok "mutation: an [allow] token is stripped as a whole word only — a mailbox that merely ends in it is refused"
else bad "allow tail (rc=$RC): $OUTPUT"; fi

fresh allow-case; plant README.md 'Questions? Write to Support@Tracebloc.io.' && guard
if [ "$RC" -eq 0 ] && has "[forbidden-strings] clean ("; then ok "an [allow] token matches case-insensitively, like the scan, and a sentence-ending dot is still a boundary"; else bad "allow case (rc=$RC): $OUTPUT"; fi

fresh extra; printf 'planted-tenant\n' >"$ROOT/extra/tenants.txt"; plant docs/usage.md 'for Planted-Tenant' && guard --extra-forbidden "$ROOT/extra/tenants.txt"
# The private pattern is the identifier kept out of the public list; it must not
# surface in the log (teed into the public run summary) or in the report.
if [ "$RC" -eq 1 ] && has "[strings-refuse] private needle #1 found in 1 staged line(s):" && has "tree/docs/usage.md:2" && has "(3 refuse + 3 report needle(s)" \
   && ! grep -qi 'planted-tenant' <<<"$OUTPUT" && ! grep -qi 'planted-tenant' "$OUT/publish-guard-report.txt"; then
  ok "mutation: a private needle from --extra-forbidden joins the refuse tier, named by number only"
else bad "extra needle (rc=$RC): $OUTPUT"; fi

fresh extra-empty; printf '# none\n\n' >"$ROOT/extra-empty/tenants.txt"; guard --extra-forbidden "$ROOT/extra-empty/tenants.txt"
if [ "$RC" -eq 2 ] && has "[forbidden-strings] COULD NOT TELL — extra forbidden list '" && has "' is empty"; then ok "an empty --extra-forbidden list is could-not-tell"; else bad "extra empty (rc=$RC): $OUTPUT"; fi

fresh extra-missing; guard --extra-forbidden "$ROOT/extra-missing/absent.txt"
if [ "$RC" -eq 2 ] && has "extra forbidden list '" && has "absent.txt' is missing or unreadable"; then ok "a missing --extra-forbidden list is could-not-tell"; else bad "extra missing (rc=$RC): $OUTPUT"; fi

fresh asset-ref; mkdir -p "$ROOT/asset-ref/assets"; printf '#!/bin/sh\n# arn:aws:s3:::planted\n' >"$ROOT/asset-ref/assets/install.sh"; guard --assets "$ROOT/asset-ref/assets"
if [ "$RC" -eq 1 ] && has "[assets] staged 1 release asset(s):" && has "[strings-refuse] needle 'arn:aws:' found in 1 staged line(s):" && has "assets/install.sh:2"; then
  ok "a refuse-tier needle inside a release asset is refused with the asset named"
else bad "asset ref (rc=$RC): $OUTPUT"; fi

fresh binary; mkdir -p "$ROOT/binary/assets"; printf 'ELF\000arn:aws:x\000' >"$ROOT/binary/assets/tracebloc-linux-amd64"; guard --assets "$ROOT/binary/assets"
if [ "$RC" -eq 0 ] && has "3 text file(s) scanned, 1 binary file(s) opaque to this scan"; then ok "a binary asset is opaque to the string scan and counted as such"; else bad "binary (rc=$RC): $OUTPUT"; fi

# ---- guard 3: forbidden strings — the report tier and --strict -----------------------------
fresh report; plant README.md 'see backend#1234 for the rationale' && guard
if [ "$RC" -eq 0 ] && has "[forbidden-strings] [strings-report] needle 'backend#' found in 1 staged line(s) — counted, not refused (--strict refuses)" \
   && has "[forbidden-strings] [strings-report] 1 hit(s) in 1 file(s); most-hit files:" && has "         1  tree/README.md" \
   && has "[forbidden-strings] 0 refuse-tier hit(s), 1 report-tier hit(s) counted" && ! has "REFUSED" && has "publish-guard: OK — all 4 guards ran and passed" \
   && ! has "for the rationale" && grep -qF "[strings-report] needle 'backend#':" "$OUT/publish-guard-report.txt" && grep -qF "tree/README.md:2" "$OUT/publish-guard-report.txt"; then
  ok "a report-tier hit alone is counted, not refused: exit 0, per-needle total, most-hit files, report written"
else bad "report tier (rc=$RC): $OUTPUT"; fi

fresh strict; plant README.md 'see backend#1234 for the rationale' && guard --strict
if [ "$RC" -eq 1 ] && has "[forbidden-strings] REFUSED — [strings-report (strict)] needle 'backend#' found in 1 staged line(s):" && has "    tree/README.md:2" \
   && has "[forbidden-strings] 0 refuse-tier hit(s), 1 report-tier hit(s) refused under --strict" && has "publish-guard: REFUSED — do not publish"; then
  ok "mutation: the same report-tier hit under --strict is refused, tier named"
else bad "strict (rc=$RC): $OUTPUT"; fi

fresh strict-clean; guard --strict
if [ "$RC" -eq 0 ] && has "[forbidden-strings] clean (2 refuse + 3 report needle(s)"; then ok "--strict with no report-tier hit is still clean"; else bad "strict clean (rc=$RC): $OUTPUT"; fi

fresh host; plant docs/usage.md 'API dev-api.tracebloc.io' && guard; a="$RC"; b="$OUTPUT"; rm -rf "$OUT"; guard --strict
if [ "$a" -eq 0 ] && [[ "$b" == *"[strings-report] needle 'dev-api\.tracebloc\.io' found in 1 staged line(s) — counted, not refused"* ]] \
   && [ "$RC" -eq 1 ] && has "REFUSED — [strings-report (strict)] needle 'dev-api\.tracebloc\.io' found in 1 staged line(s):" && has "tree/docs/usage.md:2"; then
  ok "a non-production hostname is report-tier: counted, and refused under --strict"
else bad "hostname (a=$a rc=$RC): $b // $OUTPUT"; fi

fresh table; plant README.md 'backend#1 and RFC-0001 on one line' && plant README.md 'backend#2 on another' && plant docs/usage.md '# backend#3' && guard
if [ "$RC" -eq 0 ] && has "[strings-report] needle 'backend#' found in 3 staged line(s)" && has "[strings-report] needle 'RFC-0' found in 1 staged line(s)" \
   && [[ "$OUTPUT" == *"[strings-report] 4 hit(s) in 2 file(s); most-hit files:"*"         3  tree/README.md"*"         1  tree/docs/usage.md"* ]]; then
  ok "the most-hit table sums every report-tier needle per file, largest first"
else bad "table (rc=$RC): $OUTPUT"; fi

fresh table-cap; for i in 01 02 03 04 05 06 07 08 09 10 11; do add_file "docs/n$i.md" "ref backend#$i"; done; commit; guard
if [ "$RC" -eq 0 ] && has "[strings-report] 11 hit(s) in 11 file(s); most-hit files:" && [ "$(printf '%s\n' "$OUTPUT" | grep -cE '^ +[0-9]+  (tree|assets)/')" -eq 10 ]; then
  ok "the most-hit table stops at ten rows"
else bad "table cap (rc=$RC): $OUTPUT"; fi

fresh both; plant README.md 'arn:aws:iam::000000000000:root — see backend#9' && guard
if [ "$RC" -eq 1 ] && has "REFUSED — [strings-refuse] needle 'arn:aws:' found in 1 staged line(s):" && has "[strings-report] needle 'backend#' found in 1 staged line(s) — counted, not refused" \
   && has "[forbidden-strings] 1 refuse-tier hit(s), 1 report-tier hit(s) counted"; then
  ok "a refuse-tier and a report-tier hit in one run: refused, and the report tier still counted"
else bad "both tiers (rc=$RC): $OUTPUT"; fi

# ---- guard 3: the forbidden list itself ----------------------------------------------------
fresh norefuse; printf '[paths]\ntests/\n[strings-report]\nbackend#\n' >"$SRC/.publish-forbidden"; guard; a="$RC"; b="$OUTPUT"
rm -rf "$OUT"; printf '[paths]\ntests/\n[strings-refuse]\n# none yet\n[strings-report]\nbackend#\n' >"$SRC/.publish-forbidden"; guard
if [ "$a" -eq 2 ] && [[ "$b" == *"[forbidden-strings] COULD NOT TELL — '"*"' has no [strings-refuse] entries — a guard with nothing to refuse is misconfigured"* ]] \
   && [ "$RC" -eq 2 ] && has "has no [strings-refuse] entries"; then
  ok "no [strings-refuse] entries (absent or empty section) is could-not-tell"
else bad "no refuse tier (a=$a rc=$RC): $b // $OUTPUT"; fi

fresh norefuse-extra; printf '[paths]\ntests/\n[strings-report]\nbackend#\n' >"$SRC/.publish-forbidden"; printf 'planted-tenant\n' >"$ROOT/norefuse-extra/tenants.txt"; guard --extra-forbidden "$ROOT/norefuse-extra/tenants.txt"
if [ "$RC" -eq 2 ] && has "has no [strings-refuse] entries"; then ok "an empty [strings-refuse] is judged before the private needles join it"; else bad "no refuse tier + extra (rc=$RC): $OUTPUT"; fi

fresh dup; printf '[paths]\ntests/\n[strings-refuse]\narn:aws:\nbackend#\n[strings-report]\nbackend#\nRFC-0\n' >"$SRC/.publish-forbidden"; guard
if [ "$RC" -eq 2 ] && has "[forbidden-strings] COULD NOT TELL — '" && has "' lists needle 'backend#' in both [strings-refuse] and [strings-report] — a needle has one tier"; then
  ok "a needle listed in both string tiers is could-not-tell, the duplicate named"
else bad "duplicate needle (rc=$RC): $OUTPUT"; fi

fresh unknown; printf '[paths]\ntests/\n[strings]\narn:aws:\n' >"$SRC/.publish-forbidden"; guard; a="$RC"; b="$OUTPUT"
rm -rf "$OUT"; printf '[paths]\ntests/\n[strings-refuse]\narn:aws:\n[strings report]\nbackend#\n' >"$SRC/.publish-forbidden"; guard
if [ "$a" -eq 2 ] && [[ "$b" == *"[forbidden-paths] COULD NOT TELL — '"*"' has an unknown section [strings] — the guard reads only [paths] [strings-refuse] [strings-report] [allow]"* ]] \
   && [[ "$b" == *"[forbidden-strings] COULD NOT TELL — '"*"' has an unknown section [strings]"* ]] \
   && [ "$RC" -eq 2 ] && has "has an unknown section [strings report]" && ! has "needle 'backend#'"; then
  ok "an unknown section header (the retired [strings], a header with a space) is could-not-tell for both scans"
else bad "unknown section (a=$a rc=$RC): $b // $OUTPUT"; fi

fresh noforbidden; rm "$SRC/.publish-forbidden"; guard
if [ "$RC" -eq 2 ] && has "[forbidden-paths] COULD NOT TELL — forbidden list '" && has "[forbidden-strings] COULD NOT TELL — forbidden list '"; then ok "a missing forbidden list is could-not-tell for both scans"; else bad "no forbidden (rc=$RC): $OUTPUT"; fi

# ---- guard 1: allowlist fail-closed -------------------------------------------------------
fresh emptyinc; printf '# comments only\n' >"$SRC/.publish-include"; guard
if [ "$RC" -eq 2 ] && has "[allowlist] COULD NOT TELL" && has "lists no include entries"; then ok "an allowlist with no include entries is could-not-tell"; else bad "empty include (rc=$RC): $OUTPUT"; fi

fresh noinc; rm "$SRC/.publish-include"; guard
if [ "$RC" -eq 2 ] && has "[allowlist] COULD NOT TELL — allowlist '" && has "is missing or unreadable"; then ok "a missing allowlist is could-not-tell"; else bad "missing include (rc=$RC): $OUTPUT"; fi

fresh nomatch; write_include 'nothing/**'; guard
if [ "$RC" -eq 2 ] && has "the allowlist matched none of the 11 tracked files"; then ok "an allowlist matching nothing is could-not-tell"; else bad "no match (rc=$RC): $OUTPUT"; fi

fresh symlink; ln -s ../Makefile "$SRC/docs/link.md"; git -C "$SRC" add docs/link.md; commit; guard
if [ "$RC" -eq 2 ] && has "'docs/link.md' is a symlink"; then ok "a symlink in the allowlisted set is could-not-tell"; else bad "symlink (rc=$RC): $OUTPUT"; fi

fresh dirty-out; mkdir -p "$OUT"; printf 's\n' >"$OUT/stale"; guard
if [ "$RC" -eq 2 ] && has "is not empty"; then ok "a non-empty --out is could-not-tell"; else bad "dirty out (rc=$RC): $OUTPUT"; fi

fresh noassets; mkdir -p "$ROOT/noassets/assets"; guard --assets "$ROOT/noassets/assets"
if [ "$RC" -eq 2 ] && has "holds no files"; then ok "an --assets directory with no files is could-not-tell"; else bad "no assets (rc=$RC): $OUTPUT"; fi

# ---- guard 4: gitleaks plumbing -----------------------------------------------------------
fresh gl-missing; PUBLISH_GUARD_GITLEAKS="$ROOT/no-such-gitleaks" guard
if [ "$RC" -eq 2 ] && has "[gitleaks] COULD NOT TELL — scanner '" && has "is not on PATH" && has "publish-guard: COULD NOT TELL — do not publish"; then ok "a missing scanner is could-not-tell, never clean"; else bad "gl missing (rc=$RC): $OUTPUT"; fi

fresh gl-leak; GL_MODE=leak guard
if [ "$RC" -eq 1 ] && has "[gitleaks] REFUSED — secrets detected in the staged tree:" && has "Finding: REDACTED" && has "shim gitleaks ran: detect --no-git --redact --no-banner --exit-code 9 --source "; then
  ok "a scanner finding refuses; the scanner ran with --no-git --redact over the staged tree"
else bad "gl leak (rc=$RC): $OUTPUT"; fi

fresh gl-crash; GL_MODE=crash guard
if [ "$RC" -eq 2 ] && has "[gitleaks] COULD NOT TELL — scanner exited 1"; then ok "a scanner crash is could-not-tell"; else bad "gl crash (rc=$RC): $OUTPUT"; fi

if command -v gitleaks >/dev/null 2>&1; then
  fresh gl-real
  key="AKIA$(LC_ALL=C tr -dc 'A-Z2-7' </dev/urandom | head -c 16)"   # built at run time: no key-shaped literal in this file
  plant docs/usage.md "aws_key: $key" && PUBLISH_GUARD_GITLEAKS= guard
  if [ "$RC" -eq 1 ] && has "[gitleaks] REFUSED — secrets detected" && ! has "$key"; then ok "real gitleaks: a planted access-key-shaped string is refused and redacted"; else bad "gl real (rc=$RC): $OUTPUT"; fi
else
  printf '  skip real gitleaks (not on PATH; the publish workflow installs the pinned binary)\n'
fi

# ---- the repo's OWN lists, fed inputs written here -----------------------------------------
# Inputs written independently of .publish-forbidden; the summary line's needle
# counts are asserted so a needle added to either tier without an input here
# reddens this harness.
fresh real-refuse; cp "$REPO/.publish-forbidden" "$SRC/.publish-forbidden"; guard; a="$RC"; b="$OUTPUT"; rm -rf "$OUT"
plant README.md 'ask someone@tracebloc.io' && plant docs/usage.md '# role arn:aws:iam::000000000000:role/x' && plant LICENSE 'IMG=000000000000.dkr.ecr.eu-central-1.amazonaws.com/x' && guard
missing=""; for needle in '[A-Za-z0-9._%+-]+@tracebloc\.io' 'arn:aws:' '[0-9]{12}\.dkr\.ecr\.'; do has "REFUSED — [strings-refuse] needle '$needle' found in 1 staged line(s):" || missing="$missing $needle"; done
if [ "$a" -eq 0 ] && [[ "$b" == *"[forbidden-strings] clean (3 refuse + 10 report needle(s), 1 allow token(s)"* ]] && [ "$RC" -eq 1 ] && [ -z "$missing" ] && has "[forbidden-strings] 3 refuse-tier hit(s), 0 report-tier hit(s) counted"; then
  ok "the committed .publish-forbidden spares support@ and refuses every refuse-tier needle by name"
else bad "real refuse tier (a=$a rc=$RC, not refused:$missing): $b // $OUTPUT"; fi

fresh real-report; cp "$REPO/.publish-forbidden" "$SRC/.publish-forbidden"
plant README.md 'see backend#1 and rfcs#2 and RFC-0003 and RFC-BACKEND-0004' && plant README.md 'see e2e-test-agent#5 and tracebloc/backend' \
  && plant docs/usage.md 'A=https://dev-api.tracebloc.io/ B=https://stg-api.tracebloc.io/' && plant docs/usage.md 'C=https://dev.tracebloc.io/ D=https://stg.tracebloc.io/' && guard
a="$RC"; b="$OUTPUT"; missing=""
for needle in 'backend#' 'rfcs#' 'RFC-0' 'RFC-BACKEND' 'e2e-test-agent#' 'tracebloc/backend' 'dev-api\.tracebloc\.io' 'stg-api\.tracebloc\.io' 'dev\.tracebloc\.io' 'stg\.tracebloc\.io'; do
  has "[strings-report] needle '$needle' found in 1 staged line(s) — counted, not refused" || missing="$missing $needle"
done
rm -rf "$OUT"; guard --strict
if [ "$a" -eq 0 ] && [ -z "$missing" ] && [[ "$b" == *"[strings-report] 10 hit(s) in 2 file(s); most-hit files:"* ]] && [[ "$b" == *"0 refuse-tier hit(s), 10 report-tier hit(s) counted (3 refuse + 10 report needle(s)"* ]] \
   && [ "$RC" -eq 1 ] && has "REFUSED — [strings-report (strict)] needle 'backend#' found in 1 staged line(s):" && has "tree/README.md:2" \
   && has "REFUSED — [strings-report (strict)] needle 'stg\.tracebloc\.io' found in 1 staged line(s):" && has "tree/docs/usage.md:3"; then
  ok "the committed .publish-forbidden counts every report-tier needle by name, and --strict refuses them"
else bad "real report tier (a=$a rc=$RC, not counted:$missing): $b // $OUTPUT"; fi

fresh real-paths; cp "$REPO/.publish-forbidden" "$SRC/.publish-forbidden"
add_file go.sum 'h1:'; add_file STYLE.md 's'; add_file .cursor/BUGBOT.md 'b'; add_file secret.pem 'p'; add_file .env.local 'e'; add_file docs/migration-tools/t.sh 't'; commit
write_include 'README.md' 'go.mod' 'go.sum' 'STYLE.md' '.cursor/**' 'secret.pem' '.env.local' 'docs/**' 'internal/**' 'Makefile' '.github/**' 'scripts/tests/**'
guard; missing=""
for pat in 'go.mod' 'go.sum' 'STYLE.md' '.cursor/' '*.pem' '.env*' 'docs/rfcs/' 'docs/migration-tools/' '*.go' 'Makefile' '.github/' 'scripts/tests/'; do
  has "forbidden path pattern '$pat' matched:" || missing="$missing $pat"
done
if [ "$RC" -eq 1 ] && [ -z "$missing" ]; then ok "the committed .publish-forbidden refuses every forbidden path class by name"; else bad "real forbidden paths (rc=$RC, not refused:$missing)"; fi

# The real repo through its real allowlist: README, LICENSE and docs/*.md, no
# source, no build files, no workflows. Asserted CLEAN: the refuse tier must
# hold on the real deliverable; the report tier (the known backlog of internal
# references in README and the installers) is counted, not refused, until
# --strict is the policy. A refuse-tier needle landing in a deliverable file
# reddens this harness — which is the point.
OUTPUT="$(bash "$GUARD" --source "$REPO" --out "$ROOT/real/out" 2>&1)"; RC=$?
missing=""; for f in README.md LICENSE docs/troubleshooting.md; do [ -f "$ROOT/real/out/tree/$f" ] || missing="$missing $f"; done
present=""; for f in go.mod go.sum Makefile CLAUDE.md STYLE.md cmd internal .github .cursor scripts docs/rfcs; do [ ! -e "$ROOT/real/out/tree/$f" ] || present="$present $f"; done
if [ "$RC" -eq 0 ] && has "[forbidden-paths] clean" && { has "[forbidden-strings] 0 refuse-tier hit(s), " || has "[forbidden-strings] clean ("; } && [ -z "$missing" ] && [ -z "$present" ]; then
  ok "the committed .publish-include stages README/LICENSE/docs of the real repo and no source; the refuse tier holds"
else bad "real allowlist (rc=$RC, missing:$missing, staged-but-forbidden:$present): $OUTPUT"; fi

echo
printf 'publish-guard-verify: %d passed, %d failed\n' "$PASS" "$FAIL"
[ "$FAIL" -eq 0 ] && [ "$PASS" -ge 42 ]
