# RFC 0003 — The secure environment: dataset storage, offboard hygiene & the boundary

> **Status: DRAFT v2 — decisions recorded 2026-07-22.** Owner: @LukasWodka.
> Co-design & validation: @saadqbal.
>
> **v1 → v2.** v1 (2026-07-21) framed two problems — offboard hygiene and
> dataset storage — and left §7 as a decision menu. v2 records the decisions
> taken (with prototype validation on a real install, client#368), folds in
> the review feedback, and widens the lens to what those decisions were
> always serving: a precise, honest definition of the **secure environment**
> — its boundary (§1), threat model (§2), model-IP stance (§6), in-cluster
> walls (§7), and per-substrate conformance (§8).
>
> Section map from v1: §2→§3, §3→§4, §4→§5, §5→§9, §6→§11, §7→§10
> (now a decision log), §8→§12, §9→§13.
>
> Related: RFC-0001 (auth & client provisioning), RFC-0002 (data ingest flow
> & terminology), terminology source-of-truth (v2 `TERMINOLOGY.md`, docs
> `main`). Design ticket: backend#1151. Storage prototype: client#368
> (flag-gated, draft). Offboard honesty fix: cli#389. Spans `cli`, `client`,
> `client-runtime`, `backend`.

## 1. The secure environment — definition

A **secure environment** is a sealed enclave on infrastructure the customer
controls — a laptop (k3d), a cloud cluster (EKS/AKS/OpenShift), or bare
metal; the promise is the same in every form. It has **exactly three
ingress channels, one egress channel, and nothing sideways**:

| Direction | Channel | Carries | Transport |
|---|---|---|---|
| In | **Data ingester** | raw datasets, from sources the customer configures | ingest jobs (local files / network share) |
| In | **tracebloc control plane** | models, weights, experiment instructions | backend API + Service Bus (TLS) |
| In | **tracebloc software** | container images, chart, CLI | registries — all images digest-pinned |
| Out | **tracebloc control plane** | trained weights, metrics, status/logs | Service Bus + allowlisted FQDNs (TLS) |

Everything else is sealed: the environment does not reach into host
directories (§5), nothing on the host casually reads the environment's data
(§5), and workloads inside cannot reach arbitrary networks (§8). The
environment only dials out — no inbound ports are required.

**Wording discipline.** Externally we say: *defined, auditable ingress and
egress — and raw data is never an egress channel.* We do **not** say
"air-gapped": the environment *requires* outbound connectivity (it
authenticates to the backend to obtain its Service Bus credentials; no
backend egress ⇒ experiments sit Pending), so a literal air-gap claim fails
the first serious security review. "Almost air-gapped" is internal
shorthand only.

## 2. Threat model — protect what, from whom

The environment protects **two assets for two parties**: the data owner's
datasets, and the model provider's model IP. Adversary by adversary:

| Adversary | Datasets protected by | Model IP protected by | Status |
|---|---|---|---|
| External network attacker | TLS everywhere; outbound-only; NetworkPolicy | same | enforced |
| Other services/users on the host | node-local storage — no host files, no 777, no bind-mount (§5) | same | decided (D1/D2) |
| Vendor training code in-cluster | egress lockdown (§8.1) + dataset-scoped mounts + scoped DB grants (§7) | n/a — it *is* the model | built-inert / decided |
| tracebloc itself | raw data never leaves; only weights + defined metrics egress | n/a | by design |
| Cloud provider (cloud form) | customer-run infra; encrypted PVs; TEE later | TEE later (§6.5) | partial |
| **Environment owner (root)** | out of scope — the data is theirs | hygiene + watermark/audit now (§6.3–6.4); TEE phase 2 (§6.5) | decided (D7/D8) |

**Honest ceilings — never claim past these:**

1. **The machine owner has root, Docker, and the disk.** On classical
   hardware, nothing that executes there can be made absolutely
   inaccessible to them. For datasets that is fine (the data is theirs);
   for model IP, see §6.
2. **The sanctioned exit carries data-derived information.** Weights and
   metrics are functions of the training data — that is the product. The
   defensible claim is "**raw data never leaves**", not "nothing derived
   from your data leaves". Residual leakage (memorization, membership
   inference) is a known property of federated learning; differential
   privacy / secure aggregation are future options, out of scope here.
3. **Tampering can be made detectable, not impossible** (owner-root
   again). Dataset integrity & score reproducibility is deliberately a
   separate RFC (§11).

## 3. How it works today (verified 2026-07-21; evidence refreshed 2026-07-22)

### 3.1 Where data and weights physically live
- MySQL runs **in-cluster**; ingested datasets are tables in the
  `training_test_datasets` database. Weight files at rest also live inside
  the environment (DB/data volume): an environment runs **tens of
  concurrent trainings**, so weights cannot simply be held in memory.
- The MySQL PersistentVolume uses a **hostPath** under
  `/tracebloc/<release>/mysql`; the local k3d cluster **bind-mounts
  `~/.tracebloc → /tracebloc`** (`HOST_DATA_DIR`, default
  `$HOME/.tracebloc`). Net effect: dataset tables are host files.
- Dataset *files* can additionally come from `HOST_DATASET_DIR` (a network
  mount by design, backend#743), bind-mounted **cluster-wide** (`@all`) at
  `/tracebloc-data` — a wider window than a single ingest needs (§7).

### 3.2 Permissions (corrected from v1)
- `_ensure_tracebloc_dirs` chmods **the `logs`/`mysql`/`data` subdirs** to
  `777` — not all of `~/.tracebloc`, and `values.yaml` is spared. v1
  overstated the blast radius.
- On the hostPath model that 777 is **load-bearing** (the host user writes
  into the shared dirs, and kubelet does not apply fsGroup to hostPath
  volumes) — so v1 §4.6's "can ship independently" was wrong. The 777
  disappears **with Option C**, not before it (D2).

### 3.3 Install & upgrade behavior
- Install does `mkdir -p` with **no existing-data guard** — leftover data
  is silently adopted. Flat and per-release layouts coexist across
  versions; a machine that has seen both accumulates both (Problem A).
- On re-run, the installer **reuses** an existing cluster ("already exists"
  → use it; `cluster start`) — it does not recreate. In-place upgrades
  therefore keep data; only an explicit `cluster delete` destroys it. This
  fact is load-bearing for D1 and D4.

### 3.4 Offboard (`tracebloc delete`)
- Teardown order: revoke credential → clear active-client pointer → Helm
  uninstall → k3d cluster delete → prune images → remove host data dir →
  remove self. Flags: `--yes`, `--keep-data`, `--force`.
- The host-data wipe is now **verified before printing ✔** (cli#389) —
  a nil `RemoveAll` is no longer treated as proof.
- `HOST_DATASET_DIR` is never touched (by design — shared network mount).

### 3.5 Egress today (added in v2)
- The training NetworkPolicy ships **enabled**, allowing DNS, in-cluster
  MySQL, the requests-proxy, and the egress gateway — **plus a
  `0.0.0.0/0:443` rule** (`networkPolicy.training.allowExternalHttps`
  defaults to `true`). The squid egress gateway (FQDN allowlist: backend +
  App Insights) ships **inert** (`egressProxy.routeWorkloads: false`).
- Translation: the lockdown is **built but not flipped** — today a training
  pod can still reach any external host on :443 (§8.1).
- Enforcement tooling exists: a `helm test` probe verifies the CNI actually
  blocks egress, and a reachability check verifies required backend egress
  works.

### 3.6 Spawned-job hardening today
- New-architecture images run with `readOnlyRootFilesystem`, write weights
  and scratch to a pod-scoped `emptyDir` (`EXPERIMENT_SCRATCH_PATH` — dies
  with the pod), and get read-only shared mounts. **Legacy images are
  carved out** of parts of this (they write inside the image filesystem).

### 3.7 The 2026-07-21 reproduction (unchanged from v1)
- "Delete isn't deleting" was **wrong**: `tracebloc delete --force --yes`
  fully wiped `~/.tracebloc` (0 survivors). The earlier "survival" was a
  flat/per-release **transition artifact** across a version bump, plus a
  second CLI binary in `/usr/local/bin`. The real gaps: alternate/legacy
  locations, and the installer silently re-adopting whatever it finds.

## 4. Problem A — offboard leaves a clean slate → DECIDED

**What we want:** "delete" means the environment's data is gone — current
version and leftovers — so a reinstall starts empty. UX expectation and
privacy expectation at once.

**Scope constraint (unchanged):** never a machine-wide wipe. A machine can
host multiple environments, and `HOST_DATASET_DIR` may be a shared network
mount other tools use. Clean-slate is scoped to the environment being
offboarded.

**Decision (D3):**
1. **Installer leftover-guard** — if data is present at install time, stop
   and make the user choose (reuse / wipe / new dir) instead of silently
   adopting. This is the guard that prevents the bug that was actually hit,
   and it doubles as the migration prompt (D4).
2. **Drop the heavy multi-path wipe** proposed in v1 — under node-local
   storage (§5) there are no host data paths left to chase. `delete` keeps
   config/token cleanup and **verifies the wipe before printing ✔**
   (shipped as cli#389).
3. `--keep-data` stays as the explicit opt-out. A `--purge-all` (enumerate,
   confirm, never default) is deferred until someone actually needs it.

## 5. Problem B — where datasets live → DECIDED: Option C (node-local)

**The principle (unchanged from v1): separate restart-persistence from
delete-persistence.** Laptops reboot and data must survive; offboard/delete
means destroyed. The hostPath bind-mount conflated the two by making data
survive everything — which is exactly what made offboard fragile (Problem
A) *and* put data outside the environment boundary (Problem B).

The v1 menu, with the decision marked:

| Option | Restart-persists | Delete wipes it | Host exposure |
|---|---|---|---|
| A. Status quo — bind-mount `~/.tracebloc`, 777 subdirs | ✅ | ⚠️ only if delete chases every path | ❌ world-readable files in `$HOME` |
| B. Docker managed named volume (env-scoped) | ✅ | ✅ if removed on offboard | ✅ |
| **C. Node-local storage (k3s `local-path`, inside the k3d node) — CHOSEN (D1)** | ✅ (node container persists across stop/start) | ✅ (dies with `cluster delete`) | ✅ not visible as host files |
| D. C + encryption at rest | ✅ | ✅ | ✅✅ — **phase 2** (D5) |

**Why C:** datasets and MySQL move onto k3s's built-in `local-path`
provisioner *inside the k3d node*. Delete destroys the data by
construction; there is no browsable host folder; the 777 goes away by
construction (D2); restarts keep working (the node container and its
Docker volume survive stop/start); and upgrades keep data because the
installer **reuses** the cluster (§3.3). The v1 objection to C —
"loses survive-delete+recreate" — dissolves: delete+recreate is precisely
the case where data *should* die.

**Validated on a real credentialed dev install (client#368, 2026-07-22):**
single-node cluster, all PVCs Bound on `local-path`, zero hostPath PVs;
real jobs-manager-spawned ingest jobs mounted the shared PVC and registered
datasets against the backend; stop/start preserved data; `cluster delete`
destroyed the node, its Docker volume, and all data; nothing under
`~/.tracebloc`. The prototype run also caught and fixed a real leak (a
second `_ensure_tracebloc_dirs` call site still creating empty 777 dirs).

**Constraint C1 — single-node.** `local-path` is RWO/WaitForFirstConsumer,
and spawned Jobs must land on the node that holds the volume, so node-local
forces `AGENTS=0`. Since `AGENTS` defaults to `1` today, **flipping the
default also changes the default local topology from two nodes to one** —
call this out in release notes. (k3s agents on one Docker host provide no
real isolation, so nothing of value is lost.)

**Out of scope for now:** `HOST_DATASET_DIR` (network-mount ingest sources)
stays on the hostpath path; combining it with node-local is a follow-up.

**Migration (D4): no data-copier.** `--reuse-values` upgrades keep existing
installs on their current `~/.tracebloc`; they move to C on a clean
delete + reinstall — consistent with "delete means gone". The leftover-guard
(D3) ensures data is never silently stranded or adopted.

**Encryption at rest (D5): phase 2.** For local installs, recommend host
full-disk encryption (FileVault/LUKS); in cloud, encrypted PVs. App-level
crypto for the volume is not warranted for a local dev tool unless the
threat model changes — and the real answer to "protected from a local
admin" is §6.5, not filesystem crypto.

**Open (O1):** flip node-local from flag-gated (`TB_STORAGE_MODE`) to
**default** for local installs — the step that actually delivers this RFC
to users. Recommendation: flip after one green end-to-end **training run**
on node-local (the only client#368-checklist item not yet exercised; it was
blocked by an orthogonal dev auth issue, not by storage).

## 6. Model IP protection — two-sided trust (new in v2)

### 6.1 Goal
The environment owner must not be able to access the vendor models that
run inside their environment. This completes the marketplace trust story in
both directions — the data owner's data is protected from the vendor,
**and** the vendor's model is protected from the data owner. Almost nobody
in the FL space offers the second half; it is worth building toward
deliberately.

### 6.2 The ceiling — state it before designing around it
To execute, weights must exist in plaintext in RAM/VRAM, and any decryption
key must be present in the environment. Root can dump process memory, GPU
memory, or the disk at any moment *during* execution — so at-rest
encryption alone, obfuscation, and delete-after-run all raise the bar
without changing the outcome. **On classical hardware, prevention against
owner-root is impossible; only trusted execution environments (§6.5)
change that.** Everything in 6.3–6.4 is deterrence, detection, and
minimization — valuable, and never to be sold as prevention.

### 6.3 Now — watermarking + audit (D7)
Fingerprint delivered weights **per environment** (traitor tracing): a
leaked model is attributable to the environment it leaked from, which makes
the contractual protection enforceable. Log model-delivery events. This is
detection and legal recourse, not prevention — and it is what makes 6.4
credible commercially.

### 6.4 Now — runtime hygiene, maximum practical (D8)
1. **Weights enter at runtime over TLS, never baked into images** (already
   true — images stay generic per task type; `docker save` yields no IP).
2. **The active working copy** lives in pod-scoped scratch
   (`EXPERIMENT_SCRATCH_PATH`, an `emptyDir`) and dies with the pod —
   already true on new-architecture images; close the legacy-image
   carve-out (D11).
3. **At rest: envelope encryption + crypto-shredding.** An environment runs
   tens of concurrent trainings, so weight files cannot live in RAM — they
   rest in the environment's DB / data volume. Therefore: store weight
   blobs as **ciphertext**; the per-experiment data-encryption key
   (~32 bytes) is issued by the backend at cycle start and held **only in
   memory**, never persisted inside the environment. N concurrent
   experiments cost N small keys in RAM — not N weight files. On experiment
   completion (or offboard, or revocation) the key is discarded and the
   backend refuses re-issue: every at-rest copy — including disk snapshots
   and backups — becomes unrecoverable. **Crypto-shredding is deletion that
   is instant, size-independent, and verifiable**, and it upgrades what
   "delete" means for weights on top of §4/§5.
4. **Lifecycle:** TTL finished Jobs (`ttlSecondsAfterFinished`); offboard
   already prunes images (§3.4).

Stated ceiling: root can still capture plaintext *during* execution — 6.4
stops casual and after-the-fact access, not a determined owner. That gap
belongs to 6.5.

### 6.5 Phase 2 — confidential computing (TEE) (D14)
AMD SEV-SNP / Intel TDX confidential VMs plus NVIDIA H100/H200
confidential-computing mode encrypt memory with keys held in silicon; the
host, hypervisor, and root **cannot** read enclave memory. Remote
attestation lets tracebloc act as key broker: weights decrypt **only**
inside a measured, attested enclave. This is the one mechanism that
actually delivers "the owner cannot access the model" — and it also
strengthens the *data* story in cloud deployments (data protected from the
cloud provider). Azure sells confidential GPU VMs today, so a pilot on
cloud-form environments is the natural first step **when build capacity
allows — explicitly deferred for now** (decided 2026-07-22). On-prem
follows where customer hardware supports it; local k3d never gets it.
Bridge until then: **sensitivity tiering** — vendors can flag crown-jewel
models to run only on TEE-attested environments; commodity/open-backbone
models run anywhere.

### 6.6 Rejected
Homomorphic encryption / MPC for training: orders of magnitude too slow for
deep learning at this scale. Named here so it doesn't resurface.

## 7. In-cluster walls — dataset & experiment scoping (new in v2, DECIDED)

Being inside the box must not mean seeing everything in the box. The
precise promise is "no service **outside the sanctioned training flow**
touches the data" — and the sanctioned flow is *per experiment*. A training
job must reach exactly its own dataset and its own artifacts, nothing else.

1. **Dataset-scoped mounts (D9).** Jobs-manager currently mounts the whole
   shared data PVC into spawned jobs. Near term: mount only the job's
   dataset directory (`subPath`), read-only for training. End state under
   Option C: **one PVC per dataset** — `local-path` dynamic provisioning
   makes per-dataset PVCs cheap — so a job physically cannot see other
   datasets. This also narrows the cluster-wide `HOST_DATASET_DIR` window
   where that path is used.
2. **The database layer (D10).** Tabular and time-series datasets are
   MySQL tables, and the training NetworkPolicy allows training→MySQL —
   PVC scoping alone does not cover them. Adopt **per-experiment DB
   credentials whose grants cover only that experiment's dataset tables**
   (and its own weight rows, which composes with §6.4): short-lived,
   injected at job start, revoked after.
3. **Finish spawned-pod hardening (D11).** Close the legacy-image
   read-only carve-out; set `automountServiceAccountToken: false` on
   spawned jobs; keep `readOnlyRootFilesystem` + read-only shared mounts as
   the floor.

## 8. Boundary enforcement & conformance (new in v2)

### 8.1 Egress lockdown — flip it (D6, separate ticket)
The outbound wall exists (training NetworkPolicy, squid FQDN-allowlist
gateway, enforcement probe, backend-reachability check) but **ships
permissive**: `allowExternalHttps: true` keeps a `0.0.0.0/0:443` rule and
`routeWorkloads: false` leaves the gateway inert. Until the per-fleet flip
(verify gateway → route workloads → drop the 443 rule), "nothing gets out"
is not an enforced property for training pods on :443. Tracked as its own
ticket; this RFC records the dependency: **the golden-box claim is gated on
that flip** more than on anything else in this document.

### 8.2 The seal check (D12)
Productize the existing probes into **one conformance suite** — the
egress-enforcement probe, the required-backend-reachability check, and
storage checks (post-C: no hostPath PVs, PVCs on the expected class,
nothing under `~/.tracebloc`) — runnable at install, at upgrade, and on
demand, surfaced pass/fail in the CLI. Design stance the chart already
takes: **silent non-protection is worse than explicit disabling.** An
environment that cannot enforce a guarantee is explicitly marked *unsealed*
— never silently claimed sealed.

### 8.3 The guarantee matrix (D12)
Enforcement differs per substrate; the matrix is the honest artifact a
customer security review can quote. To be filled precisely as part of D12
(cells: enforced / conditional / recommended / not available):

| Guarantee | k3d local | EKS | AKS | OpenShift | bare metal |
|---|---|---|---|---|---|
| Storage inside cluster boundary (§5) | decided (C) | native PV | native PV | native PV | native PV |
| NetworkPolicy egress enforcement | verify (k3s embedded) | conditional (CNI mode) | conditional (CNI) | native (OVN) | conditional (CNI) |
| Encryption at rest | host FDE (recommended) | encrypted EBS | encrypted disks | platform | site policy |
| Confidential compute (§6.5) | not available | phase 2 | phase 2 (pilot) | phase 2 | hardware-dependent |

### 8.4 Verify local enforcement (D12)
Do not assume k3d enforces NetworkPolicy — verify the k3s-embedded
controller blocks egress on a local install and fold that probe into the
seal check.

## 9. Messaging reconciliation

- The **channel list in §1 is the quotable definition** of the secure
  environment — align docs, website, and sales material with it and with
  the terminology source-of-truth.
- Keep the strong "within the secure environment" wording — Option C makes
  it literally true for local storage. Internal precision: node-local means
  *not-host-visible + dies-with-cluster*, not *cryptographically secure*.
- Never "air-gapped" in external copy (§1).
- Model-IP claims: "technically protected" only where TEE-attested (§6.5,
  phase 2). Until then the accurate sentence is: *runtime-hygienic,
  watermarked, and contractually protected.*

## 10. Decision log (2026-07-22, Lukas + Asad)

| # | Decision (v1 ref) | Outcome |
|---|---|---|
| D1 | Local storage target (§7.2) | **Option C — node-local**, validated in client#368 |
| D2 | Drop `chmod 777` (§7.3) | **Yes — by construction with C** (load-bearing on hostpath, cannot ship separately) |
| D3 | Offboard clean-slate (§7.1) | **Leftover-guard yes; heavy multi-path wipe no; verify-before-✔** (cli#389) |
| D4 | Migration (§7.5) | **No data-copier**; guard prevents stranding; installs move to C on delete+reinstall |
| D5 | At-rest encryption (§7.4) | **Phase 2**; recommend host FDE; keep wording honest |
| D6 | Egress lockdown flip | **Separate ticket**; golden-box claim gated on it (§8.1) |
| D7 | Model IP — now | **Watermarking + audit** (§6.3) |
| D8 | Weights at rest | **Envelope encryption + crypto-shredding — keys in memory, not weight files** (§6.4) |
| D9 | Dataset scoping | **Scoped mounts → per-dataset PVCs under C** (§7.1) |
| D10 | DB scoping | **Per-experiment DB credentials, table-scoped grants** (§7.2) |
| D11 | Pod hardening | **Close legacy carve-out; no SA token; read-only floor** (§7.3) |
| D12 | Conformance | **Guarantee matrix + seal check + verify k3d enforcement** (§8.2–8.4) |
| D13 | Score/tamper integrity | **Separate RFC** — versioning+integrity root cause, not storage (§11) |
| D14 | Confidential compute | **Phase 2 — deferred for capacity**; Azure confidential-GPU pilot first; tiering as bridge (§6.5) |

Open items:

| # | Question | Recommendation |
|---|---|---|
| O1 | Flip node-local to **default** for local installs (also flips default local topology to single-node) | Yes — after one green training run on node-local |
| O2 | Weight retention: exactly when does crypto-shred fire (experiment completion? grace window? resume/audit needs?) | Shred at completion + configurable grace window |
| O3 | DEK custody: backend-issued per cycle vs in-environment key service | Backend-issued — revocation/offboard instant; the environment already requires backend egress |
| O4 | Watermarking mechanics & owner (backend work) | Scope inside the D7 ticket |

## 11. Non-goals

- Remote/cloud PV storage model — unchanged (already on proper PVs on
  customer infrastructure).
- Restart-persistence — unchanged (laptops reboot; data survives).
- **Dataset integrity & score reproducibility** — fingerprint at ingest,
  verify at scoring, bind every score to the dataset fingerprint. Genuinely
  important for the benchmark product, but its root cause is verifiable
  *versioning*, not storage location; it gets its **own RFC** (D13) rather
  than blurring this one.
- Cryptographic training (HE/MPC) — rejected (§6.6). Differential privacy /
  secure aggregation — future, separate.

## 12. Rollout sequence

1. Land this RFC with the §10 decision log.
2. cli#389 (verify-before-✔) and client#368 (flag-gated C) merge on their
   own review tracks.
3. Build the **installer leftover-guard** (D3).
4. **Egress-lockdown flip** (D6) proceeds on its own ticket, per fleet.
5. One green **training run on node-local** → flip the default (O1), with
   the single-node topology change in release notes.
6. **Scoping & hygiene epic** (D8–D11) as backend#1151 children:
   envelope-encrypted weights + crypto-shred; scoped mounts; per-experiment
   DB grants; pod-hardening completion; watermarking (D7).
7. **Seal check + guarantee matrix** (D12).
8. Phase 2 when capacity allows: TEE pilot on Azure confidential GPU (D14);
   at-rest encryption revisit (D5).

## 13. Appendix — evidence (refreshed 2026-07-22; line numbers as of that date)

- `client/scripts/lib/common.sh:386` — `HOST_DATA_DIR="${HOST_DATA_DIR:-$HOME/.tracebloc}"` (v1 cited :329 — drifted)
- `client/scripts/lib/common.sh:392` — `HOST_DATASET_DIR="${HOST_DATASET_DIR:-}"` (v1 cited :335)
- `client/scripts/lib/common.sh:383` — `AGENTS="${AGENTS:-1}"` (default two-node local topology; §5 C1)
- `client/scripts/lib/cluster.sh:28-56` — `_ensure_tracebloc_dirs`: `mkdir -p` with no existing-data guard; `chmod -R 777` scoped to `logs`/`mysql`/`data` subdirs only, `values.yaml` spared
- `client/scripts/lib/cluster.sh:197`, `:355-356` — cluster **reuse** on re-run (`cluster start`; "already exists → Using existing cluster")
- `client/scripts/lib/cluster.sh:312` — `-v "${HOST_DATASET_DIR}:/tracebloc-data@all"` (cluster-wide dataset-source mount)
- `client/client/values.yaml` — `networkPolicy.training.{enabled,allowExternalHttps,enforcementProbeHost,clusterCidrs}`, `egressProxy.{enabled,routeWorkloads}`, `egressReachabilityCheck` (lockdown built, ships permissive; §3.5/§8.1)
- `client-runtime/jobs_manager.py` (~:825-885) — `EXPERIMENT_SCRATCH_PATH` emptyDir scratch, `readOnlyRootFilesystem`, read-only shared mounts; legacy-image carve-out (~:77, :829)
- `cli/internal/cli/delete.go` — teardown order; wipe verified before ✔ as of cli#389
- Validation evidence — client#368 comments (2026-07-22): single-node node-local install on dev; PVCs on `local-path`; real ingest-jobs sharing the data PVC; stop/start survives; delete destroys volume + data; prototype fix `5e4ea45` (second `_ensure_tracebloc_dirs` call site)
- Repro (2026-07-21): `tracebloc delete --force --yes` → `~/.tracebloc` gone, 0 `.ibd` survivors; earlier "survival" = flat/per-release transition artifact + second CLI binary
