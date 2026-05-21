[![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE) [![Platform](https://img.shields.io/badge/platform-tracebloc-00C9A7.svg)](https://ai.tracebloc.io)

# tracebloc CLI

The customer-facing CLI for the tracebloc declarative ingestion path. Wraps the same `POST /internal/submit-ingestion-run` protocol the [`tracebloc/ingestor`](https://github.com/tracebloc/client/tree/main/ingestor) Helm chart uses, so any cluster running the parent [`tracebloc/client`](https://github.com/tracebloc/client) chart can be targeted directly from a developer's workstation.

## Status

🚧 **Pre-alpha** — Phase 0 of the [v0.1 roadmap](https://github.com/tracebloc/client/issues/147). Today this binary implements only `version` and `completion`. Subsequent phases (#149–#153) land schema validation, cluster discovery, data staging, submission, and distribution.

For the production ingestion flow today, use the Helm chart: `helm install tracebloc/ingestor --set-file ingestConfig=./ingest.yaml`. See the chart's [README](https://github.com/tracebloc/client/blob/develop/ingestor/README.md) for the full customer journey including the data-staging recipe.

## Why a CLI in addition to the chart?

The chart works well for one persona — a Kubernetes-fluent ML engineer at a customer who already has data staged on a shared cluster filesystem. For everyone else (data scientists with local data, ML engineers with cloud sources, platform engineers running GitOps, repeat-ingest users), the chart hands them at least three foreign mental models in a trench coat.

The CLI is a sibling interface to the chart, not a replacement. Both translate to the same protocol; the customer picks the one that matches their workflow.

```
Customer interfaces (pick one or many):
┌─────────────────────────────────────────────────┐
│  Web UI       Studio for clicking through       │  ← future
│  CLI          `tracebloc dataset push ./data`   │  ← this repo
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

## v0.1 design (target customer experience, once Phase 5 ships)

```bash
# Install once
brew install tracebloc/tap/tracebloc        # macOS
curl -fsSL https://install.tracebloc.io | sh # Linux/macOS
irm https://install.tracebloc.io/install.ps1 | iex  # Windows

# Per dataset
tracebloc dataset push ./my-data \
  --table cats_dogs_train \
  --category image_classification \
  --intent train \
  --label-column label
```

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

Requires Go 1.22 or newer. The binary self-reports its build metadata; a `go build` without `-ldflags` reports `dev / unknown / unknown` for version/git-sha/build-date so support can tell a local hack apart from a release build.

```bash
# Release-style build with version metadata
go build -ldflags "\
  -X main.version=$(git describe --tags --always) \
  -X main.gitSHA=$(git rev-parse --short HEAD) \
  -X main.buildDate=$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  -o tracebloc ./cmd/tracebloc
```

## Roadmap

| Phase | Ticket | What |
|---|---|---|
| **0 (this PR)** | [#148](https://github.com/tracebloc/client/issues/148) | Repo bootstrap + Go module + CI + `tracebloc version` |
| 1 | [#149](https://github.com/tracebloc/client/issues/149) | Embed `ingest.v1.json` + `tracebloc ingest validate <path>` (local-only) |
| 2 | [#150](https://github.com/tracebloc/client/issues/150) | Cluster discovery + ingestor SA token via TokenRequest |
| 3 | [#151](https://github.com/tracebloc/client/issues/151) | Stage data into the shared PVC via ephemeral Pod |
| 4 | [#152](https://github.com/tracebloc/client/issues/152) | Submit to jobs-manager + watch ingestor Job + summary |
| 5 | [#153](https://github.com/tracebloc/client/issues/153) | GitHub Releases, Homebrew tap, install.sh distribution |

Epic: [tracebloc/client#147](https://github.com/tracebloc/client/issues/147).

## Related

- [tracebloc/client](https://github.com/tracebloc/client) — the parent Helm chart this CLI submits to, and where the [`tracebloc/ingestor`](https://github.com/tracebloc/client/tree/main/ingestor) subchart lives.
- [tracebloc/data-ingestors](https://github.com/tracebloc/data-ingestors) — the ingestor image + JSON schema this CLI validates against.

## License

Apache 2.0 — see [LICENSE](LICENSE).
