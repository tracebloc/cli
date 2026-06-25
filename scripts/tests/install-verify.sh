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
set -u

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
  BIN="$SBX/bin"; REL="$SBX/release"; DEST="$SBX/dest"
  mkdir -p "$BIN" "$REL" "$DEST"

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
  for t in bash sh env mkdir mktemp cp cat awk grep sed head tr uname chmod mv rm sleep printf install dirname basename sha256sum shasum; do
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

# Run installer with PATH=$BIN only (host cosign can't shadow), into $DEST.
run_installer() { PATH="$BIN" "$BIN/bash" "$INSTALLER" --prefix "$DEST" "$@" >"$SBX/out" 2>&1; echo $?; }

# ── 1. cosign present + valid signature → installs ──────────────────────────
make_sandbox yes
rc="$(COSIGN_RESULT=0 run_installer)"
if [ "$rc" = 0 ] && grep -q "cosign signature valid" "$SBX/out" && [ -x "$DEST/tracebloc" ]; then
  ok "cosign present + valid sig installs"
else
  bad "cosign present + valid sig installs (rc=$rc)"; sed 's/^/      /' "$SBX/out"
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

echo
echo "install-verify: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
