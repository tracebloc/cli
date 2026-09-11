# Release checklist

The release workflow is fully automated — pushing a `v*.*.*` tag
triggers it. This document covers the per-release manual steps that
either ARE or AREN'T automated, so the on-call engineer doesn't
have to reverse-engineer the surface area on release day.

## Release policy

- **Trigger:** cut a release when a customer-visible feature merges
  to `develop`, or weekly if anything customer-visible is sitting
  unreleased — whichever comes first. Don't let `develop` drift
  releases behind (the v0.8 gap reached 46 unreleased commits).
- **Owner:** the DevEx squad (role, not a person) cuts the tag and
  walks this checklist.

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

7. `.github/workflows/mirror-publish.yml` fires when the Release
   workflow completes. It stages the public deliverable (README,
   LICENSE, `docs/*.md` per `.publish-include`; the release assets)
   through `scripts/publish-guard.sh` — allowlist, forbidden paths,
   forbidden strings, gitleaks, all fail-closed — and pushes it, plus
   a copy of the release, to the public mirror named by the
   `MIRROR_REPO` variable. Until that variable is set the job refuses
   to publish; `Actions → Mirror publish → Run workflow` with
   `dry-run: true` shows what would ship. The string scan has two
   tiers: `[strings-refuse]` hits refuse; `[strings-report]` hits
   (internal ticket references, non-production hostnames) are counted
   and printed with the most-hit files, and refuse only under the
   `strict` input or the `PUBLISH_STRICT=true` repository variable.
   The guard and publisher run from the workflow's own commit; the
   release tag is fetched separately as data and refused unless it
   resolves to the commit the Release run ran on. A prerelease
   (`-rc.N`) mirrors only its GitHub release, marked prerelease and
   pinned to the mirror's current default-branch head — the mirror's
   default branch keeps the last stable release.

8. Releases that predate the mirror are carried over ONCE, by hand, with
   `scripts/backfill-releases.sh` (the workflow only publishes releases
   cut after it exists). The decision it implements: every published
   release gets its tag, its GitHub release and its text assets
   (`install.sh`, `install.ps1`, `SHA256SUMS`, anything else SHA256SUMS
   does not list); the binaries and their `.sig`/`.cert` only for the
   newest `BINARY_KEEP` releases (default 10) — older pinned binary
   URLs 404 on the mirror, and the answer is "re-run the installer".
   Prereleases are skipped unless `--include-prerelease`. Mirror tags
   are annotated RELEASE MARKERS on the mirror's default-branch head,
   carrying the original date and message — the mirror has no source
   commit to point at, and the annotation says so. Release notes are
   the same fixed text the workflow writes (`--notes fixed`, the
   default) plus a footer naming the original publish date — the
   historical bodies are GitHub's generated pull-request lists, and
   nearly every one carries strings the guard's report tier counts,
   which the mirror should not repeat. `--notes source` carries the
   source body instead, as an explicit opt-in. Every text asset and
   every release body goes through `publish-guard.sh` first; every
   binary is checked against the source's `SHA256SUMS`; anything
   already on the mirror with the same SHA256 is skipped, so a re-run
   writes nothing. Dry-run is the default:

   ```bash
   MIRROR_REPO=<mirror name> scripts/backfill-releases.sh            # plan
   MIRROR_REPO=<mirror name> scripts/backfill-releases.sh --apply    # write
   # resume after a failure, or redo one release:
   MIRROR_REPO=<mirror name> scripts/backfill-releases.sh --apply --from-tag vX.Y.Z
   MIRROR_REPO=<mirror name> scripts/backfill-releases.sh --apply --only-tag vX.Y.Z
   ```

   Needs `gh` (token with write on the mirror), `jq`, `gitleaks`; set
   `BACKFILL_EXTRA_FORBIDDEN` to a file with the private needle list
   the workflow gets from its secret, or the string scan runs without
   them. Exit 1 means at least one release was refused (the table says
   which and why); exit 2 means a read did not complete and nothing was
   written. The script's header carries the full contract.

GitHub Releases plus the cosign-verified `install.sh` are the
install path — a Homebrew tap and the `install.tracebloc.io`
vanity URL were considered and dropped
([#299](https://github.com/tracebloc/cli/issues/299); the job and
formula template are recoverable from git history if ever needed).

## Per-release manual steps

The release-cutter runs these on tag day.

### 1. Pre-flight

- [ ] No release-blocking tickets open on the milestone
- [ ] `develop` is green on CI + Bugbot
- [ ] Local smoke: `go test -race ./...` passes
- [ ] Real EKS smoke: `tracebloc data ingest ./cats-dogs ...`
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

**If there is no GitHub Release at all** — the tag exists and nothing is
attached — one build leg failed, and `publish` is gated on all eight, by
design (a release missing `darwin/arm64` is worse than no release). That is
the backend#2379 shape: on 2026-08-23 a sigstore TUF CDN reset failed the
darwin/arm64 leg and `v0.10.10-rc.2` sat as a tag with zero assets for
6h04m. Recover with:

```bash
gh run rerun <run-id> --repo tracebloc/cli --failed
```

Re-run the failed jobs, or dispatch the workflow **at the tag ref**. Never
dispatch from a branch: cosign embeds the run's ref in the keyless identity
and the installers trust only `@refs/tags/v.*`, so a branch-dispatched
"rebuild of a tag" publishes signatures every customer install rejects
(release.yml's own comment; Bugbot on promotion #428).

Transient sigstore failures are now retried in-place by
`scripts/cosign-retry.sh` (bounded, transient-only), so this recovery should
be rare. If a leg still fails, read its log before re-running: the wrapper
does **not** retry a genuine signing refusal, and re-running one of those
just fails again.

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
