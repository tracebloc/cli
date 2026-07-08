[![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE) [![Platform](https://img.shields.io/badge/platform-tracebloc-00C9A7.svg)](https://ai.tracebloc.io)

# tracebloc CLI

The customer-facing CLI for the tracebloc declarative ingestion path. Wraps the same `POST /internal/submit-ingestion-run` protocol the [`tracebloc/ingestor`](https://github.com/tracebloc/client/tree/main/ingestor) Helm chart uses, so any cluster running the parent [`tracebloc/client`](https://github.com/tracebloc/client) chart can be targeted directly from a developer's workstation.

## Status

**v0.3.0 is released** — the latest stable [release](https://github.com/tracebloc/cli/releases/latest), cut from `develop`. It builds on v0.2.0's guided `data ingest` and `dataset rm` with a new `dataset list` command plus home-screen / output polish (clearer copy, guided-first framing). The binary implements `version`, `completion`, `data validate`, `cluster info`, and the full `data ingest` / `dataset list` / `dataset rm` flow — local schema validation, cluster discovery, data staging, submission, and Job watching, end to end.

`data ingest` covers **9 of 10 task categories**: `image_classification`, `object_detection`, `keypoint_detection`, `text_classification`, `masked_language_modeling`, `tabular_classification`, `tabular_regression`, `time_series_forecasting`, and `time_to_event_prediction`. `semantic_segmentation` is pending mask-sidecar support upstream ([data-ingestors#136](https://github.com/tracebloc/data-ingestors/issues/136)).

The release pipeline ships [`v0.3.0`](https://github.com/tracebloc/cli/releases/latest) as **cosign-signed, multi-arch binaries** — Linux (`amd64`, `arm64`, `386`, `arm`), macOS (`amd64`, `arm64`), and Windows (`amd64`, `arm64`) — each with `SHA256SUMS` and the install scripts. Install via [Customer experience](#customer-experience) or [build from source](#building-from-source). (A Homebrew tap and the `install.tracebloc.io` vanity URL are later follow-ups; the GitHub release URL serves installs today.)

The Helm chart remains a sibling interface for the Kubernetes-native workflow: `helm install tracebloc/ingestor --set-file ingestConfig=./ingest.yaml` (see the chart's [README](https://github.com/tracebloc/client/blob/develop/ingestor/README.md)).

## Why a CLI in addition to the chart?

The chart works well for one persona — a Kubernetes-fluent ML engineer at a customer who already has data staged on a shared cluster filesystem. For everyone else (data scientists with local data, ML engineers with cloud sources, platform engineers running GitOps, repeat-ingest users), the chart hands them at least three foreign mental models in a trench coat.

The CLI is a sibling interface to the chart, not a replacement. Both translate to the same protocol; the customer picks the one that matches their workflow.

```
Customer interfaces (pick one or many):
┌─────────────────────────────────────────────────┐
│  Web UI       Studio for clicking through       │  ← future
│  CLI          `tracebloc data ingest ./data`   │  ← this repo
│  Python SDK   `IngestConfig(...).submit()`      │  ← future
│  K8s CRD      `kubectl apply` Ingestion CR      │  ← future
│  Helm chart   tracebloc/ingestor                │  ← today
└─────────────────────────────────────────────────┘
                       │
                       │  All translate to the same protocol —
                       │  no special-casing in jobs-manager
                       ▼
┌─────────────────────────────────────────────────┐
│  POST /internal/submit-ingestion-run            │
│  Protocol: ingest.v1.json schema                │
└─────────────────────────────────────────────────┘
```

The protocol — the v1 schema + the POST endpoint — is the stable point. Everything above is interchangeable.

## Customer experience

> Installs the latest stable release. Pin a specific version with `--version vX.Y.Z` (`sh`) or `$env:RELEASE_VERSION` (PowerShell), or [build from source](#building-from-source).

```bash
# Install — Linux / macOS
curl -fsSL https://github.com/tracebloc/cli/releases/latest/download/install.sh | sh
# Install — Windows (PowerShell)
irm https://github.com/tracebloc/cli/releases/latest/download/install.ps1 | iex
# The installer also creates a short alias: tb works everywhere tracebloc does.

# Per dataset
tracebloc data ingest ./my-data \
  --table cats_dogs_train \
  --category image_classification \
  --intent train \
  --label-column label
```

> **`tracebloc: command not found` after installing?** The binary installs to `~/.local/bin` when `/usr/local/bin` isn't writable, and an already-running shell won't see the new PATH entry until you open a new terminal (or `. ~/.bashrc`). See **[Troubleshooting installation](docs/troubleshooting.md)**.

> **Signature verification is mandatory.** The installer verifies the binary's
> SHA256 **and** its cosign signature before installing. If `cosign` isn't on
> PATH it bootstraps a pinned, checksum-verified copy; if it can't, the install
> **fails closed** rather than trusting the same-channel checksum alone (it no
> longer silently skips the signature). The one escape, for a genuinely
> constrained environment, is to re-run with `TRACEBLOC_ALLOW_UNVERIFIED=1` —
> which prints a loud warning. For the highest trust, pre-install `cosign`
> (`brew install cosign`, your package manager, or the [released binary](https://github.com/sigstore/cosign/releases)) before running the installer. (RFC-0001 R8.)

What that runs under the curtain:

1. Reads kubeconfig, discovers the parent `tracebloc/client` release in the cluster
2. Validates the implied `ingest.yaml` against the v1 schema locally (instant feedback)
3. Mints an `ingestor` ServiceAccount token via TokenRequest
4. Stages the local files into the cluster's shared PVC via an ephemeral Pod
5. POSTs the request to jobs-manager
6. Watches the ingestor Job; streams logs; prints the summary

The customer never touches Helm, never edits YAML, never runs `kubectl cp`.

## Building from source

```bash
git clone https://github.com/tracebloc/cli.git
cd cli
go build -o tracebloc ./cmd/tracebloc
./tracebloc version
```

Requires Go 1.26 or newer (the `k8s.io/*` dependencies set the floor; see `go.mod`). The binary self-reports its build metadata; a `go build` without `-ldflags` reports `dev / unknown / unknown` for version/git-sha/build-date so support can tell a local hack apart from a release build.

```bash
# Release-style build with version metadata
go build -ldflags "\
  -X main.version=$(git describe --tags --always) \
  -X main.gitSHA=$(git rev-parse --short HEAD) \
  -X main.buildDate=$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  -o tracebloc ./cmd/tracebloc
```

## Roadmap

All v0.1 phases are merged:

| Phase | Ticket | What | Status |
|---|---|---|---|
| 0 | [#148](https://github.com/tracebloc/client/issues/148) | Repo bootstrap + Go module + CI + `tracebloc version` | ✅ |
| 1 | [#149](https://github.com/tracebloc/client/issues/149) | Embed `ingest.v1.json` + `tracebloc data validate <path>` (local-only) | ✅ |
| 2 | [#150](https://github.com/tracebloc/client/issues/150) | Cluster discovery + ingestor SA token via TokenRequest | ✅ |
| 3 | [#151](https://github.com/tracebloc/client/issues/151) | Stage data into the shared PVC via ephemeral Pod | ✅ |
| 4 | [#152](https://github.com/tracebloc/client/issues/152) | Submit to jobs-manager + watch ingestor Job + summary | ✅ |
| 5 | [#153](https://github.com/tracebloc/client/issues/153) | GitHub Releases + install.sh distribution (Homebrew tap deferred) | ✅ — [`v0.1.0`](https://github.com/tracebloc/cli/releases/tag/v0.1.0) released (stable, 8-platform) |

Beyond the original phases, `data ingest` was widened from image-classification-only to 9 of 10 modalities, and the test suite gained unit-coverage wins plus a kind-based integration harness for the real-I/O seams.

**v0.2.0** added a friendlier guided `data ingest` and `dataset rm` on the home screen (#44, #47). **v0.3.0** added the `dataset list` command (#53) plus home-screen / output-spacing polish and feedback-copy refinements (#52, #56). **Next:** cloud-source ingestion (S3/GCS/HTTPS) for datasets above the 1 GiB local cap; `semantic_segmentation` ([data-ingestors#136](https://github.com/tracebloc/data-ingestors/issues/136)). Smaller follow-ups are tracked as [open issues](https://github.com/tracebloc/cli/issues).

Epic: [tracebloc/client#147](https://github.com/tracebloc/client/issues/147).

## Related

- [tracebloc/client](https://github.com/tracebloc/client) — the parent Helm chart this CLI submits to, and where the [`tracebloc/ingestor`](https://github.com/tracebloc/client/tree/main/ingestor) subchart lives.
- [tracebloc/data-ingestors](https://github.com/tracebloc/data-ingestors) — the ingestor image + JSON schema this CLI validates against.

## License

Apache 2.0 — see [LICENSE](LICENSE).
