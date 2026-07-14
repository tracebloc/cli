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

GitHub Releases plus the cosign-verified `install.sh` are the
install path — a Homebrew tap and the `install.tracebloc.io`
vanity URL were considered and dropped
([#299](https://github.com/tracebloc/cli/issues/299); the job and
formula template are recoverable from git history if ever needed).

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

### 4. Sanity-test each install path on a clean host

```bash
# Linux:
curl -fsSL https://github.com/tracebloc/cli/releases/latest/download/install.sh | sh
tracebloc version

# macOS:
curl -fsSL https://github.com/tracebloc/cli/releases/latest/download/install.sh | sh
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
