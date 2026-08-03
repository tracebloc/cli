#!/usr/bin/env bash
# =============================================================================
#  install-verify.sh — assert the MANDATORY cosign verification in install.sh
#  (RFC-0001 R8, backend#889).
#
#  The property under test: the CLI installer must NOT silently skip signature
#  verification when cosign is absent (the previous behavior, and the default on
#  a fresh box). It must bootstrap a verifier or FAIL CLOSED — never degrade to
#  the same-channel SHA256 alone.
#
#  install.sh is a POSIX `curl | sh` entrypoint, so we drive it as a subprocess
#  with curl + cosign + the sha tools replaced by PATH shims and a fake release
#  served from a temp dir. No network, no real download. This harness is bash
#  (for arrays/locals); the script under test stays POSIX sh.
# =============================================================================
# pipefail so a failing pipeline producer (sha256sum | awk in _sha) can't be
# masked by its last stage exiting 0. Deliberately NO -e: this harness counts
# pass/fail itself and must keep running after a failed assertion.
set -uo pipefail

SELF_DIR="$(cd "$(dirname "$0")" && pwd)"
INSTALLER="$SELF_DIR/../install.sh"

PASS=0
FAIL=0
ok()   { printf '  ok   %s\n' "$1"; PASS=$((PASS+1)); }
bad()  { printf '  FAIL %s\n' "$1"; FAIL=$((FAIL+1)); }

# real sha helper for building the fake SHA256SUMS
_sha() { if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | awk '{print $1}'; else shasum -a 256 "$1" | awk '{print $1}'; fi; }

# Build a sandbox release + mock bin. Args: COSIGN_PRESENT(yes/no)
make_sandbox() {
  SBX="$(mktemp -d)"
  BIN="$SBX/bin"; REL="$SBX/release"; DEST="$SBX/dest"; HOMEDIR="$SBX/home"
  # Every run gets its OWN $HOME. The installer persists a PATH line to the
  # shell rc, so without this the harness appends to the *developer's* real
  # ~/.bash_profile — once per successful case, with a fresh mktemp --prefix
  # each time. That is how the v0.10.1 validation box ended up with ten
  # tracebloc PATH blocks, all naming temp dirs that no longer existed (#433).
  mkdir -p "$BIN" "$REL" "$DEST" "$HOMEDIR"

  # The "binary" and its SHA256SUMS, named exactly as resolve_tag/detect_* expect.
  os="$(uname -s | tr '[:upper:]' '[:lower:]')"; [ "$os" = darwin ] || os=linux
  case "$(uname -m)" in x86_64|amd64) arch=amd64 ;; arm64|aarch64) arch=arm64 ;; *) arch=amd64 ;; esac
  TAG="v9.9.9"
  BF="tracebloc-${TAG}-${os}-${arch}"
  printf 'fake-binary\n' > "$REL/$BF"
  printf '%s  %s\n' "$(_sha "$REL/$BF")" "$BF" > "$REL/SHA256SUMS"
  printf 'SIG\n'  > "$REL/$BF.sig"
  printf 'CERT\n' > "$REL/$BF.cert"

  # Mock curl: map release-asset URLs to the served files; everything else 404s
  # (so a cosign-bootstrap download fails → exercises the fail-closed path).
  cat > "$BIN/curl" <<EOF
#!/usr/bin/env bash
url=""; out=""
while [ \$# -gt 0 ]; do case "\$1" in -o) out="\$2"; shift 2;; -fsSI) shift;; -*) shift;; *) url="\$1"; shift;; esac; done
rel="$REL"
case "\$url" in
  *"/releases/latest") printf 'location: https://github.com/tracebloc/cli/releases/tag/$TAG\r\n';;
  *"/releases/download/$TAG/"*) f="\${url##*/}"; [ -f "\$rel/\$f" ] || { echo "404 \$url" >&2; exit 22; }; if [ -n "\$out" ]; then cp "\$rel/\$f" "\$out"; else cat "\$rel/\$f"; fi;;
  *) echo "unmapped \$url" >&2; exit 22;;
esac
EOF
  chmod +x "$BIN/curl"

  # coreutils the installer needs, so PATH=$BIN alone works (host cosign hidden).
  for t in bash sh env mkdir mktemp cp cat awk grep sed head tr uname chmod mv rm sleep printf install dirname basename sha256sum shasum ln readlink; do
    p="$(command -v "$t" 2>/dev/null)" && ln -sf "$p" "$BIN/$t"
  done

  if [ "$1" = "yes" ]; then
    # cosign present and PASSES
    cat > "$BIN/cosign" <<'EOF'
#!/usr/bin/env bash
exit "${COSIGN_RESULT:-0}"
EOF
    chmod +x "$BIN/cosign"
  fi
}

drop_sandbox() { rm -rf "$SBX"; }

# Run installer with PATH=$BIN only (host cosign can't shadow), into $DEST, with
# $HOME pointed at the sandbox so the rc write can never touch real dotfiles.
# FAKE_SHELL selects which rc the installer targets; /bin/sh → $HOME/.profile,
# which is the same answer on Linux and macOS (bash differs between them), so the
# assertions below don't have to branch per platform.
run_installer() {
  PATH="$BIN" HOME="$HOMEDIR" SHELL="${FAKE_SHELL:-/bin/sh}" \
    "$BIN/bash" "$INSTALLER" --prefix "$DEST" "$@" >"$SBX/out" 2>&1
  echo $?
}

# Same, but installs into an explicit prefix (and optionally with that prefix
# pre-seeded onto PATH) so the profile-write behaviour can be exercised across
# several installs that share one $HOME.
run_installer_at() {
  local prefix="$1" extra_path="${2:-}"
  PATH="$BIN${extra_path:+:$extra_path}" HOME="$HOMEDIR" SHELL="${FAKE_SHELL:-/bin/sh}" \
    "$BIN/bash" "$INSTALLER" --prefix "$prefix" >"$SBX/out" 2>&1
  echo $?
}

# How many PATH blocks the installer owns in the sandbox rc. Prefix match, not a
# whole-line one: the marker we write now carries a " (prefix: <dir>)" suffix,
# while blocks from older installers are the bare comment.
tb_blocks() { grep -c '^# Added by the tracebloc CLI installer' "$1" 2>/dev/null || true; }

# ── 1. cosign present + valid signature → installs ──────────────────────────
make_sandbox yes
rc="$(COSIGN_RESULT=0 run_installer)"
if [ "$rc" = 0 ] && grep -q "cosign signature valid" "$SBX/out" && [ -x "$DEST/tracebloc" ]; then
  ok "cosign present + valid sig installs"
else
  bad "cosign present + valid sig installs (rc=$rc)"; sed 's/^/      /' "$SBX/out"
fi
# the tb short alias rides along on the happy path (best-effort, but with ln
# available — as here — it must exist and point at the binary)
if [ "$(readlink "$DEST/tb" 2>/dev/null)" = "$DEST/tracebloc" ]; then
  ok "tb short alias created next to the binary"
else
  bad "tb short alias missing or wrong target ($(readlink "$DEST/tb" 2>/dev/null || echo none))"
fi
drop_sandbox

# ── 2. cosign present + FAILED signature → aborts ───────────────────────────
make_sandbox yes
rc="$(COSIGN_RESULT=1 run_installer)"
if [ "$rc" != 0 ] && grep -q "signature verification FAILED" "$SBX/out" && [ ! -e "$DEST/tracebloc" ]; then
  ok "cosign present + bad sig fails closed"
else
  bad "cosign present + bad sig fails closed (rc=$rc)"; sed 's/^/      /' "$SBX/out"
fi
drop_sandbox

# ── 3. cosign ABSENT, can't bootstrap → FAILS CLOSED (the core regression) ──
make_sandbox no
rc="$(run_installer)"
if [ "$rc" != 0 ] && grep -q "cosign is required" "$SBX/out" && [ ! -e "$DEST/tracebloc" ]; then
  ok "cosign absent fails closed (no silent skip)"
else
  bad "cosign absent fails closed (rc=$rc)"; sed 's/^/      /' "$SBX/out"
fi
# The exact old-behavior string must NEVER appear.
if grep -q "signature skipped" "$SBX/out"; then bad "found the old 'signature skipped' path"; else ok "no 'signature skipped' degrade path"; fi
drop_sandbox

# ── 4. cosign absent + explicit opt-in → installs with a loud warning ───────
make_sandbox no
rc="$(TRACEBLOC_ALLOW_UNVERIFIED=1 run_installer)"
if [ "$rc" = 0 ] && grep -qi "signature NOT verified" "$SBX/out" && [ -x "$DEST/tracebloc" ]; then
  ok "opt-in degrades with a warning"
else
  bad "opt-in degrades with a warning (rc=$rc)"; sed 's/^/      /' "$SBX/out"
fi
drop_sandbox

# ── 5. SHA256 mismatch → aborts before cosign even matters ──────────────────
make_sandbox yes
# Overwrite the served binary AFTER SHA256SUMS was computed, so its digest no
# longer matches → the SHA256 gate must abort even with a passing cosign.
bf="$(ls "$REL"/tracebloc-v9.9.9-* | grep -vE '\.(sig|cert)$' | grep -v 'SHA256SUMS' | head -1)"
printf 'TAMPERED-CONTENT\n' > "$bf"
rc="$(COSIGN_RESULT=0 run_installer)"
if [ "$rc" != 0 ] && grep -q "SHA256 mismatch" "$SBX/out" && [ ! -e "$DEST/tracebloc" ]; then
  ok "sha256 mismatch fails closed"
else
  bad "sha256 mismatch fails closed (rc=$rc)"; sed 's/^/      /' "$SBX/out"
fi
drop_sandbox

# ── 6. path-traversal --version → rejected before any download (RFC-0001 R8) ─
# 'v1.2.3-../../heads/main' once flowed verbatim into BASE_URL=.../download/$TAG;
# curl would collapse the '..' and fetch from a path other than the release. The
# new validate_tag must reject it: non-zero, nothing installed, no binary fetch.
make_sandbox yes
rc="$(COSIGN_RESULT=0 run_installer --version 'v1.2.3-../../heads/main')"
if [ "$rc" != 0 ] \
   && grep -Eq "path separator or '\.\.'|not a valid release tag" "$SBX/out" \
   && [ ! -e "$DEST/tracebloc" ] \
   && ! grep -q "Downloading binary" "$SBX/out"; then
  ok "traversal --version rejected before download"
else
  bad "traversal --version rejected before download (rc=$rc)"; sed 's/^/      /' "$SBX/out"
fi
drop_sandbox

# ── 7. bare path separator in --version → rejected ──────────────────────────
make_sandbox yes
rc="$(COSIGN_RESULT=0 run_installer --version 'v1.2.3/heads/main')"
if [ "$rc" != 0 ] \
   && grep -Eq "path separator or '\.\.'|not a valid release tag" "$SBX/out" \
   && [ ! -e "$DEST/tracebloc" ]; then
  ok "slash in --version rejected"
else
  bad "slash in --version rejected (rc=$rc)"; sed 's/^/      /' "$SBX/out"
fi
drop_sandbox

# ── 8. malformed (non-vX.Y.Z) --version → rejected ──────────────────────────
make_sandbox yes
rc="$(COSIGN_RESULT=0 run_installer --version 'not-a-tag')"
if [ "$rc" != 0 ] && grep -q "not a valid release tag" "$SBX/out" && [ ! -e "$DEST/tracebloc" ]; then
  ok "malformed --version rejected"
else
  bad "malformed --version rejected (rc=$rc)"; sed 's/^/      /' "$SBX/out"
fi
drop_sandbox

# ── 9. a well-formed --version still installs (validator isn't over-tight) ──
# Guards against a regression where validate_tag rejects legitimate tags. The
# sandbox serves v9.9.9, so request exactly that explicitly via --version.
make_sandbox yes
rc="$(COSIGN_RESULT=0 run_installer --version 'v9.9.9')"
if [ "$rc" = 0 ] && grep -q "cosign signature valid" "$SBX/out" && [ -x "$DEST/tracebloc" ]; then
  ok "well-formed --version passes validation and installs"
else
  bad "well-formed --version passes validation and installs (rc=$rc)"; sed 's/^/      /' "$SBX/out"
fi
drop_sandbox

# ── 10. malformed COSIGN_VERSION → rejected before the cosign bootstrap fetch ─
# COSIGN_VERSION is env-overridable and is interpolated into the Sigstore release
# download URL, so it gets the same semver + path-traversal gate as the release
# tag. cosign is ABSENT here so ensure_cosign reaches the bootstrap path that
# validates it; a crafted value must abort, not redirect which release we fetch.
make_sandbox no
rc="$(COSIGN_VERSION='v2.4.1/../../heads/main' run_installer)"
if [ "$rc" != 0 ] \
   && grep -Eq "cosign version .*contains a path separator or '\.\.'|not a valid cosign version" "$SBX/out" \
   && [ ! -e "$DEST/tracebloc" ]; then
  ok "malformed COSIGN_VERSION rejected"
else
  bad "malformed COSIGN_VERSION rejected (rc=$rc)"; sed 's/^/      /' "$SBX/out"
fi
drop_sandbox

# ── 11. the rc PATH block is idempotent and never accumulates (#433) ─────────
# Pre-#433 the append was guarded only by "does the rc mention $PREFIX", so an
# install with a DIFFERENT prefix appended another block every time and the
# profile grew without bound. Assert exactly ONE block whatever the history:
# same prefix repeated, then a run of distinct prefixes.
make_sandbox yes
RC="$HOMEDIR/.profile"
COSIGN_RESULT=0 run_installer >/dev/null
COSIGN_RESULT=0 run_installer >/dev/null
COSIGN_RESULT=0 run_installer >/dev/null
if [ "$(tb_blocks "$RC")" = 1 ]; then
  ok "same prefix installed 3x leaves one PATH block"
else
  bad "same prefix installed 3x leaves one PATH block (got $(tb_blocks "$RC"))"; sed 's/^/      /' "$RC"
fi

# …and the repeat runs must not even rewrite the file. `tracebloc upgrade`
# re-execs this installer, so the same prefix comes back on every upgrade; the rc
# is the user's file and an install that changes nothing must touch nothing.
cp "$RC" "$SBX/rc.before"
COSIGN_RESULT=0 run_installer >/dev/null
if diff -q "$RC" "$SBX/rc.before" >/dev/null && grep -q 'already in your PATH config' "$SBX/out"; then
  ok "re-install with the same prefix leaves the rc byte-identical"
else
  bad "re-install with the same prefix leaves the rc byte-identical"; diff "$SBX/rc.before" "$RC" | sed 's/^/      /'
fi

# Distinct prefixes — the exact shape that produced ten blocks on the v0.10.1 box.
p1="$SBX/p1"; p2="$SBX/p2"; p3="$SBX/p3"
for p in "$p1" "$p2" "$p3"; do COSIGN_RESULT=0 run_installer_at "$p" >/dev/null; done
if [ "$(tb_blocks "$RC")" = 1 ] && grep -qF "$p3" "$RC" && ! grep -qF "$p1" "$RC"; then
  ok "three different prefixes leave one block, naming the newest"
else
  bad "three different prefixes leave one block, naming the newest (got $(tb_blocks "$RC"))"; sed 's/^/      /' "$RC"
fi
drop_sandbox

# ── 12. a prefix already on PATH is not persisted at all ────────────────────
# Nothing to fix, so the rc must not be created or touched — the common
# /usr/local/bin case used to get a line that changed nothing (#433).
make_sandbox yes
COSIGN_RESULT=0 run_installer_at "$DEST" "$DEST" >/dev/null
if [ ! -e "$HOMEDIR/.profile" ]; then
  ok "prefix already on PATH writes no rc at all"
else
  bad "prefix already on PATH writes no rc at all"; sed 's/^/      /' "$HOMEDIR/.profile"
fi
drop_sandbox

# ── 13. the user's own rc content survives byte-for-byte ────────────────────
# We are editing a file we don't own: replacing our block must not reorder,
# rewrite or drop a single unrelated line.
make_sandbox yes
RC="$HOMEDIR/.profile"
cat > "$RC" <<'PROF'
# my profile
export EDITOR=vim
export PATH="$HOME/mytools:$PATH"

alias ll='ls -la'
PROF
cp "$RC" "$SBX/rc.orig"
COSIGN_RESULT=0 run_installer_at "$SBX/q1" >/dev/null
COSIGN_RESULT=0 run_installer_at "$SBX/q2" >/dev/null
# Strip our block back out; what remains must equal the original exactly.
grep -vF '# Added by the tracebloc CLI installer' "$RC" | grep -vF "$SBX/q2" | sed '${/^$/d;}' > "$SBX/rc.stripped"
if [ "$(tb_blocks "$RC")" = 1 ] && diff -q "$SBX/rc.stripped" "$SBX/rc.orig" >/dev/null; then
  ok "unrelated rc lines preserved across a block replacement"
else
  bad "unrelated rc lines preserved across a block replacement"; diff "$SBX/rc.orig" "$SBX/rc.stripped" | sed 's/^/      /'
fi
drop_sandbox

# ── 14. a marker left dangling above unrelated content can't eat it ─────────
# Removal keys off our marker, so it only takes the next line when that line is
# shaped like a PATH op we wrote — never arbitrary user content.
make_sandbox yes
RC="$HOMEDIR/.profile"
printf '# Added by the tracebloc CLI installer\nalias precious="keep me"\n' > "$RC"
COSIGN_RESULT=0 run_installer_at "$SBX/r1" >/dev/null
if grep -q 'precious' "$RC"; then
  ok "dangling marker doesn't consume the line below it"
else
  bad "dangling marker doesn't consume the line below it"; sed 's/^/      /' "$RC"
fi
drop_sandbox

# The nastier variant: a dangling marker directly above the user's OWN PATH
# export. The marker is not proof of ownership, so the line only goes if we can
# vouch for the directory it names — otherwise we'd silently delete a PATH entry
# we never added (Bugbot on #434).
make_sandbox yes
RC="$HOMEDIR/.profile"
mkdir -p "$SBX/mytools"
printf '# Added by the tracebloc CLI installer\nexport PATH="%s:$PATH"\n' "$SBX/mytools" > "$RC"
COSIGN_RESULT=0 run_installer_at "$SBX/t1" >/dev/null
if grep -qF "$SBX/mytools" "$RC"; then
  ok "dangling marker above the user's own PATH export keeps that export"
else
  bad "dangling marker above the user's own PATH export keeps that export"; sed 's/^/      /' "$RC"
fi
# Same, with an unexpanded $HOME — a '$' can never appear in a path we wrote.
printf '# Added by the tracebloc CLI installer\nexport PATH="$HOME/mytools:$PATH"\n' > "$RC"
COSIGN_RESULT=0 run_installer_at "$SBX/t2" >/dev/null
if grep -qF '$HOME/mytools' "$RC"; then
  ok "dangling marker above an unexpanded \$HOME PATH line keeps that line"
else
  bad "dangling marker above an unexpanded \$HOME PATH line keeps that line"; sed 's/^/      /' "$RC"
fi
drop_sandbox

# The nastiest variant of all, and why the marker now records its prefix: a
# dangling marker above the user's own literal-path line for a directory they have
# NOT created yet. "The directory vanished" used to be taken as proof the block was
# ours, which deleted that line (Bugbot on #434). A legacy marker — one that
# recorded no prefix — can no longer claim a missing directory.
make_sandbox yes
RC="$HOMEDIR/.profile"
printf '# Added by the tracebloc CLI installer\nexport PATH="%s/not-created-yet:$PATH"\n' "$SBX" > "$RC"
COSIGN_RESULT=0 run_installer_at "$SBX/t3" >/dev/null
if grep -qF 'not-created-yet' "$RC"; then
  ok "legacy marker can't claim a missing dir — user's line survives"
else
  bad "legacy marker can't claim a missing dir — user's line survives"; sed 's/^/      /' "$RC"
fi
# Nor an existing directory that holds no tracebloc binary.
printf '# Added by the tracebloc CLI installer\nexport PATH="%s/mytools:$PATH"\n' "$SBX" > "$RC"
COSIGN_RESULT=0 run_installer_at "$SBX/t4" >/dev/null
if grep -qF '/mytools:' "$RC"; then
  ok "legacy marker can't claim a dir without our binary"
else
  bad "legacy marker can't claim a dir without our binary"; sed 's/^/      /' "$RC"
fi
drop_sandbox

# …but a block whose marker RECORDS the prefix is provably ours, so a vanished
# directory is still reclaimed — that is the #433 cruft, and cleaning it is the
# whole point. This is what a post-fix installer writes.
make_sandbox yes
RC="$HOMEDIR/.profile"
printf '\n# Added by the tracebloc CLI installer (prefix: %s/gone)\nexport PATH="%s/gone:$PATH"\n' "$SBX" "$SBX" > "$RC"
COSIGN_RESULT=0 run_installer_at "$SBX/t5" >/dev/null
if [ "$(tb_blocks "$RC")" = 1 ] && ! grep -qF '/gone:' "$RC"; then
  ok "a recorded-prefix block naming a vanished dir is cleaned up"
else
  bad "a recorded-prefix block naming a vanished dir is cleaned up"; sed 's/^/      /' "$RC"
fi
# Ten of them — the shape the v0.10.1 box was in — collapse to one.
: > "$RC"
i=1
while [ "$i" -le 10 ]; do
  printf '\n# Added by the tracebloc CLI installer (prefix: /tmp/dead-%s)\nexport PATH="/tmp/dead-%s:$PATH"\n' "$i" "$i" >> "$RC"
  i=$((i+1))
done
COSIGN_RESULT=0 run_installer_at "$SBX/t6" >/dev/null
if [ "$(tb_blocks "$RC")" = 1 ] && ! grep -qF '/tmp/dead-' "$RC"; then
  ok "ten recorded-prefix blocks collapse to one"
else
  bad "ten recorded-prefix blocks collapse to one (got $(tb_blocks "$RC"))"; sed 's/^/      /' "$RC"
fi
drop_sandbox

# An unclaimable legacy block must not cause write CHURN either: we leave it
# alone, add ours once, and every later run with the same prefix is a no-op.
make_sandbox yes
RC="$HOMEDIR/.profile"
mkdir -p "$SBX/legacy"
printf '# Added by the tracebloc CLI installer\nexport PATH="%s/legacy:$PATH"\n' "$SBX" > "$RC"
COSIGN_RESULT=0 run_installer_at "$SBX/t7" >/dev/null
cp "$RC" "$SBX/rc.after1"
COSIGN_RESULT=0 run_installer_at "$SBX/t7" >/dev/null
COSIGN_RESULT=0 run_installer_at "$SBX/t7" >/dev/null
if diff -q "$RC" "$SBX/rc.after1" >/dev/null && grep -qF '/legacy:' "$RC"; then
  ok "an unclaimable legacy block is kept without churning the rc"
else
  bad "an unclaimable legacy block is kept without churning the rc"; diff "$SBX/rc.after1" "$RC" | sed 's/^/      /'
fi
drop_sandbox

# Finding 2: when the user's OWN line already persists the prefix and all we fail
# to do is remove a redundant block of ours, the rc is already correct — so the
# installer must NOT tell them to add a PATH line by hand.
make_sandbox yes
RC="$HOMEDIR/.profile"
printf 'export PATH="%s:$PATH"\n\n# Added by the tracebloc CLI installer (prefix: %s)\nexport PATH="%s:$PATH"\n' \
  "$DEST" "$DEST" "$DEST" > "$RC"
chmod 444 "$RC"
COSIGN_RESULT=0 run_installer >/dev/null
chmod 644 "$RC"
if ! grep -q 'Add this line to it' "$SBX/out" \
   && ! grep -q "couldn't update your shell config" "$SBX/out" \
   && grep -q 'already in your PATH config' "$SBX/out" \
   && grep -q "couldn't remove a leftover tracebloc PATH line" "$SBX/out"; then
  ok "failed tidy-up of a redundant block never asks for a manual PATH line"
else
  bad "failed tidy-up of a redundant block never asks for a manual PATH line"; sed 's/^/      /' "$SBX/out"
fi
drop_sandbox

# …while a genuine failure — prefix NOT persisted anywhere, rc unwritable — must
# still give the manual instruction, because the user's PATH really is wrong.
make_sandbox yes
RC="$HOMEDIR/.profile"
printf '# nothing of ours here\n' > "$RC"
chmod 444 "$RC"
COSIGN_RESULT=0 run_installer >/dev/null
chmod 644 "$RC"
if grep -q "couldn't update your shell config" "$SBX/out" && grep -q 'Add this line to it' "$SBX/out"; then
  ok "a real failure to persist still prints the manual instruction"
else
  bad "a real failure to persist still prints the manual instruction"; sed 's/^/      /' "$SBX/out"
fi
drop_sandbox

# ── 15. each shell's rc is the one a fresh interactive shell reads ──────────
# zsh → ~/.zshrc, fish → ~/.config/fish/config.fish (and fish gets
# fish_add_path, not a bash `export`), anything else → ~/.profile. Dedupe has to
# hold on whichever file we picked, so re-run and re-assert per shell.
for spec in "zsh:.zshrc:export PATH=" "fish:.config/fish/config.fish:fish_add_path"; do
  sh_name="${spec%%:*}"; rest="${spec#*:}"; rc_rel="${rest%%:*}"; want_op="${rest#*:}"
  make_sandbox yes
  RC="$HOMEDIR/$rc_rel"
  FAKE_SHELL="/bin/$sh_name" COSIGN_RESULT=0 run_installer_at "$SBX/s1" >/dev/null
  FAKE_SHELL="/bin/$sh_name" COSIGN_RESULT=0 run_installer_at "$SBX/s2" >/dev/null
  if [ "$(tb_blocks "$RC")" = 1 ] && grep -qF "$want_op" "$RC" && grep -qF "$SBX/s2" "$RC"; then
    ok "$sh_name: one block in $rc_rel using $want_op"
  else
    bad "$sh_name: one block in $rc_rel using $want_op (got $(tb_blocks "$RC"))"; sed 's/^/      /' "$RC" 2>/dev/null
  fi
  drop_sandbox
done

# -- 16. a fish prefix containing spaces is recognised as already listed ------
# We WRITE `fish_add_path "$PREFIX"`, so rc_lists_dir must parse the quotes
# rather than field-split: it used to see two whitespace fields, match neither,
# append a second block, and report adding an entry that was already present.
for quote_style in double single; do
  make_sandbox yes
  RC="$HOMEDIR/.config/fish/config.fish"
  mkdir -p "$(dirname "$RC")"
  SPACED="$SBX/my tools/bin"
  mkdir -p "$SPACED"
  if [ "$quote_style" = double ]; then
    printf 'fish_add_path "%s"\n' "$SPACED" > "$RC"
  else
    printf "fish_add_path '%s'\n" "$SPACED" > "$RC"
  fi
  before=$(cat "$RC")
  FAKE_SHELL=/bin/fish COSIGN_RESULT=0 run_installer_at "$SPACED" >/dev/null 2>&1
  if [ "$(cat "$RC")" = "$before" ]; then
    ok "fish: $quote_style-quoted prefix with spaces seen as already listed"
  else
    bad "fish: $quote_style-quoted prefix with spaces not matched (rc rewritten)"; sed 's/^/      /' "$RC"
  fi
  drop_sandbox
done

# -- 17. an inline comment that mentions fish_add_path must not defeat the match
# A greedy strip through the LAST `fish_add_path` on the line would leave the
# comment text as the "path" and miss the real argument (Bugbot, cli#439).
make_sandbox yes
RC="$HOMEDIR/.config/fish/config.fish"
mkdir -p "$(dirname "$RC")"
printf 'fish_add_path "%s"  # set by fish_add_path\n' "$SBX/s1" > "$RC"
mkdir -p "$SBX/s1"
before=$(cat "$RC")
FAKE_SHELL=/bin/fish COSIGN_RESULT=0 run_installer_at "$SBX/s1" >/dev/null 2>&1
if [ "$(cat "$RC")" = "$before" ]; then
  ok "fish: inline comment naming fish_add_path does not break the match"
else
  bad "fish: inline comment naming fish_add_path broke the match (rc rewritten)"; sed 's/^/      /' "$RC"
fi
drop_sandbox

# -- 18. the prefix is the SECOND quoted arg on a multi-path fish line --------
# fish_add_path accepts several directories. Parsing that stopped at the first
# closing quote missed a later one, appended a redundant block, and reported a
# PATH add that was already there (Bugbot, cli#439).
make_sandbox yes
RC="$HOMEDIR/.config/fish/config.fish"
mkdir -p "$(dirname "$RC")"
mkdir -p "$SBX/other" "$SBX/s1"
printf 'fish_add_path "%s" "%s"\n' "$SBX/other" "$SBX/s1" > "$RC"
before=$(cat "$RC")
FAKE_SHELL=/bin/fish COSIGN_RESULT=0 run_installer_at "$SBX/s1" >/dev/null 2>&1
if [ "$(cat "$RC")" = "$before" ]; then
  ok "fish: prefix as the second quoted arg is seen as already listed"
else
  bad "fish: second quoted arg was not matched (rc rewritten)"; sed 's/^/      /' "$RC"
fi
drop_sandbox

echo
echo "install-verify: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
