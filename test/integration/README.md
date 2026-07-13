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
- **`tracebloc delete` offboard teardown** (cli#140, `delete_e2e_test.go`):
  - `TestE2E_RevokeUsesPostNotDelete` — a real HTTP round-trip against a
    recording stub proving the credential is REVOKED via
    `POST /edge-device/<id>/revoke/` and **never** a hard `DELETE` of the
    row (the row is kept as history per RFC-0001 §7.10).
  - `TestE2E_DeleteTeardown` — builds the real binary and runs
    `tracebloc delete --yes --force` black-box against a throwaway **k3d**
    cluster with a real Helm release, asserting the release is uninstalled,
    the cluster is deleted, `~/.tracebloc` is wiped, and a foreign `tb`
    alias is left in place (#171 guard). **Opt-in** via `TB_E2E_K3D=1` (CI
    sets it) so it never clobbers a dev machine's `tracebloc` k3d cluster.
    The black-box run is kept fully offline (egress through a dead proxy),
    so the revoke exercises its best-effort transport-failure path; the
    POST-not-DELETE contract a live backend enforces is covered by the stub
    test above. **Not covered in CI:** the revoke against a live backend
    (the actual server-side row-preserving revoke) — see the pre-prod FR.

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
