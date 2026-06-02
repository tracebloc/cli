# CLI integration tests

Build-tagged (`//go:build integration`) tests that run against a **real
Kubernetes cluster** — covering the real-I/O seams the unit suite mocks
out (and where every live bug this project hit actually lived).

## What they cover
- `cluster.Load` + `cluster.NewClientset` — kubeconfig → live API server.
- `push.SPDYExecutor.Exec` + `push.StreamLayout` — the **tar-over-exec
  stream** against a live Pod + PVC (the highest-risk, 0%-unit-covered
  path), verified by exec-ing back into the pod to confirm the bytes
  landed.

## Running
```sh
make test-integration            # uses your current kubeconfig context
# or:
go test -tags integration -count=1 -v ./test/integration/...
```
Requires a reachable cluster (kind, k3d, or any dev cluster) with a
default StorageClass and the ability to pull the digest-pinned alpine
stage-pod image. Each test creates a throwaway namespace and cleans up
after itself.

In CI these run via `.github/workflows/e2e.yml` (kind), gated to
nightly / manual dispatch / `e2e`-labeled PRs (kind boot is too heavy
for every PR).

## Follow-ups (not yet covered)
- `PortForwardJobsManager` against a live Service (needs a small HTTP
  pod + Service fixture).
- A full `dataset push` through a real jobs-manager + ingestor (needs
  the `tracebloc/client` chart installed in-cluster) — the
  highest-fidelity test; gated on a reproducible in-CI chart install.
  This would catch cross-component issues like the jobs-manager↔ingestor
  schema skew end-to-end.
