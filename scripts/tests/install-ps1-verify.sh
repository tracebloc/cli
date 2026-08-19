#!/usr/bin/env bash
# =============================================================================
#  install-ps1-verify.sh — assert the MANDATORY cosign verification in
#  install.ps1 (RFC-0001 R8, backend#2078).
#
#  install-verify.sh's sibling. Same property, other platform: the Windows
#  installer must NOT install on the same-channel SHA256 alone when cosign is
#  absent. Until backend#2078 it did exactly that, printing
#  "(cosign not installed; SHA256 verified, signature skipped)" — so Windows
#  was the one platform where the README's "verification is mandatory … fails
#  closed" was false.
#
#  Two tiers:
#    1. string-level, here — the old degrade path is gone and stays gone.
#    2. behavioural, in install-ps1-functions.tests.ps1 — the helpers are
#       extracted from install.ps1 by AST and actually executed.
#
#  Why not drive install.ps1 end-to-end the way install-verify.sh drives
#  install.sh: it finishes with Windows-registry PATH writes that throw on a
#  Linux runner, so a full run would fail for reasons unrelated to what is
#  under test. Tier 2 runs the parts that hold the security decisions.
# =============================================================================
# Deliberately NO -e: this harness counts pass/fail itself and must keep going
# after a failed assertion (same shape as install-verify.sh).
set -uo pipefail

SELF_DIR="$(cd "$(dirname "$0")" && pwd)"
INSTALLER="$SELF_DIR/../install.ps1"

PASS=0
FAIL=0
ok()  { printf '  ok   %s\n' "$1"; PASS=$((PASS+1)); }
bad() { printf '  FAIL %s\n' "$1"; FAIL=$((FAIL+1)); }

if [ ! -f "$INSTALLER" ]; then
  # Fail closed: an installer we cannot read is not an installer that verifies.
  printf '  FAIL install.ps1 not found at %s\n' "$INSTALLER"
  exit 1
fi

# ── 1. the exact old-behaviour string must never come back ──────────────────
# Named in the ticket's acceptance criteria. install-verify.sh asserts this
# against install.sh; this is the Windows equivalent.
if grep -q 'signature skipped' "$INSTALLER"; then
  bad "found the old 'signature skipped' degrade path"
else
  ok "no 'signature skipped' degrade path"
fi

# ── 2. verification is not gated on cosign happening to be installed ────────
# The old shape was `if (Get-Command cosign …) { verify } else { skip }`, which
# makes the default — a fresh Windows box, where cosign is never present —
# the unverified one.
if grep -Eq 'if *\( *Get-Command +cosign' "$INSTALLER"; then
  bad 'verification is still gated on cosign being pre-installed'
else
  ok 'verification is not gated on cosign being pre-installed'
fi

# ── 3. the header no longer advertises verification as optional ─────────────
# A stale header is how the next reader concludes the skip is intended.
if grep -Eq '^# *[0-9]+\. *\(Optional\).*cosign' "$INSTALLER"; then
  bad 'the header still describes cosign verification as (Optional)'
else
  ok 'the header describes verification as mandatory'
fi

# ── 4. the bootstrap fetches the only Windows asset sigstore publishes ──────
# There has never been a cosign-windows-arm64.exe. Asking for one 404s and
# blocks Windows-on-ARM permanently — the bug this repo's sibling hit in
# tracebloc/client#734. The amd64 build under emulation is correct: cosign
# verifies a signature over bytes.
if grep -q 'cosign-windows-amd64.exe' "$INSTALLER" \
   && ! grep -q 'cosign-windows-arm64' "$INSTALLER"; then
  ok 'the cosign bootstrap asks for the amd64 asset on both architectures'
else
  bad 'the cosign bootstrap asks for an asset sigstore does not publish'
fi

# ── 5. the TLS floor is ASSIGNED, not OR-ed onto the default ────────────────
# `-bor Tls12` onto [Net.ServicePointManager]::SecurityProtocol leaves SSL3/
# TLS1.0/1.1 advertised, so a fetch can still negotiate down — the exact thing
# the floor exists to remove (cli#528 Bugbot). The floor must ASSIGN the value.
# Collapse newlines first: the old form spanned two lines (`= \n <default> -bor`).
_ps1_flat=$(tr '\n' ' ' < "$INSTALLER")
if printf '%s' "$_ps1_flat" | grep -Eq 'SecurityProtocol[[:space:]]*=[[:space:]]*\[Net\.ServicePointManager\]::SecurityProtocol[[:space:]]*-bor'; then
  bad 'the TLS floor OR-s onto the default SecurityProtocol (SSL3/TLS1.0/1.1 stay advertised)'
elif grep -q 'SecurityProtocolType\]::Tls12' "$INSTALLER" \
     && printf '%s' "$_ps1_flat" | grep -Eq 'ServicePointManager\]::SecurityProtocol[[:space:]]*=[[:space:]]*\$'; then
  ok 'the TLS floor assigns Tls12 (weak protocols dropped)'
else
  bad 'no assigned TLS 1.2 floor found in the installer'
fi

# ── 6. behavioural tier ─────────────────────────────────────────────────────
# pwsh is preinstalled on GitHub-hosted ubuntu runners. If it is missing we
# cannot tell whether the helpers behave, and "cannot tell" is a finding, not a
# pass (CLAUDE.md rule 3). Set ALLOW_NO_PWSH=1 to downgrade it on a dev box
# that genuinely has no pwsh — CI never sets it.
if command -v pwsh >/dev/null 2>&1; then
  echo
  if pwsh -NoProfile -File "$SELF_DIR/install-ps1-functions.tests.ps1"; then
    ok 'behavioural tier (install-ps1-functions.tests.ps1)'
  else
    bad 'behavioural tier (install-ps1-functions.tests.ps1)'
  fi
  echo
elif [ "${ALLOW_NO_PWSH:-0}" = "1" ]; then
  printf '  SKIP behavioural tier — no pwsh, ALLOW_NO_PWSH=1\n'
else
  bad 'pwsh not found, so the behavioural tier could not run (ALLOW_NO_PWSH=1 to allow)'
fi

echo "install-ps1-verify: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
