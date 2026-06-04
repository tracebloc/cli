# Release checklist

The release workflow is fully automated — pushing a `v*.*.*` tag
triggers it. This document covers the per-release manual steps that
either ARE or AREN'T automated, so the on-call engineer doesn't
have to reverse-engineer the surface area on release day.

## What runs automatically on `git push origin v0.1.0`

1. `.github/workflows/release.yml` fires.
2. Matrix build of 8 platform binaries (linux/{amd64,arm64,386,arm},
   darwin/{amd64,arm64}, windows/{amd64,arm64}).
3. Each binary is signed with cosign keyless OIDC; the per-binary
   `.cert` + `.sig` files are produced.
4. SHA256SUMS file is aggregated across the matrix.
5. `install.sh` + `install.ps1` from `scripts/` are staged into
   the release.
6. A GitHub Release is created with the tag, auto-generated notes,
   and all artifacts attached. `prerelease=true` if the tag
   contains a `-` (e.g. `v0.1.0-rc1`).
7. (If the `bump-homebrew-tap` job is enabled — see below)
   the Homebrew tap formula in `tracebloc/homebrew-tap` is
   updated with the new version + per-platform SHAs.

## What needs manual setup ONCE (not per-release)

These steps land outside the `tracebloc/cli` repo and are NOT
on the per-release checklist; they're foundational.

### Homebrew tap repo

1. Create `tracebloc/homebrew-tap` (must use the `homebrew-`
   prefix for `brew tap tracebloc/tap` to resolve).
2. Add an initial `Formula/tracebloc.rb` (the release workflow
   will overwrite this on the first tag — any valid-Ruby
   placeholder is fine).
3. Mint a fine-grained PAT with `Contents: read+write` on the
   tap repo. Save as `HOMEBREW_TAP_TOKEN` repo secret on
   `tracebloc/cli`.
4. Flip `bump-homebrew-tap`'s `if: false` to `if: true` in
   `release.yml`.

### Vanity install URL (deferred)

`install.tracebloc.io` is the eventual customer-facing URL but
isn't on the v0.1 critical path — the GitHub Release raw URL
(`https://github.com/tracebloc/cli/releases/latest/download/install.sh`)
serves v0.1 customers. Migrating to the vanity URL is a v0.2
follow-up (DNS + CNAME or Cloudflare worker).

## Per-release manual steps

The release-cutter runs these on tag day.

### 1. Pre-flight

- [ ] All v0.1 phase tickets closed (`#147-#153`)
- [ ] `develop` is green on CI + Bugbot
- [ ] Local smoke: `go test -race ./...` passes
- [ ] Real EKS smoke: `tracebloc dataset push ./cats-dogs ...`
      end-to-end reports the expected row count

### 2. Tag + push

```bash
# From develop, with a clean working tree
git tag -a v0.1.0 -m "tracebloc CLI v0.1.0"
git push origin v0.1.0
```

The workflow takes 5-10 minutes. Monitor at
`https://github.com/tracebloc/cli/actions`.

### 3. Verify the release

- [ ] All 8 binaries attached to the GitHub Release
- [ ] SHA256SUMS file present
- [ ] install.sh + install.ps1 present
- [ ] Each binary has a `.cert` + `.sig` pair
- [ ] (If tap is set up) the `tracebloc/homebrew-tap` repo has
      a new commit bumping `Formula/tracebloc.rb`

### 4. Sanity-test each install path on a clean host

```bash
# Linux:
curl -fsSL https://github.com/tracebloc/cli/releases/latest/download/install.sh | sh
tracebloc version

# macOS:
curl -fsSL https://github.com/tracebloc/cli/releases/latest/download/install.sh | sh
# OR (if tap is set up):
brew install tracebloc/tap/tracebloc
tracebloc version

# Windows (PowerShell):
irm https://github.com/tracebloc/cli/releases/latest/download/install.ps1 | iex
tracebloc version
```

### 5. Announce

- [ ] Bump `images.ingestor.digest` in `tracebloc/client` if a new
      ingestor release is coupled to this CLI release
- [ ] Update `README.md` install instructions to reference the new
      version (or leave at `latest` — preferred)
- [ ] Post in the team channel + customer Slack
