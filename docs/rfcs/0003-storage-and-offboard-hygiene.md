# RFC 0003 — Dataset storage & offboard hygiene: where data lives, and what "delete" means

> **Status: DRAFT — for discussion.** Owner: @LukasWodka. Last updated: 2026-07-21.
>
> This RFC examines two user-visible problems that turn out to share one
> root cause:
>
> 1. **`tracebloc delete` (offboard) does not reliably leave a clean slate.**
>    Leftover data from earlier installs/versions can be silently re-adopted
>    by the next install.
> 2. **Ingested datasets are stored as world-readable files on the *host*
>    filesystem**, bind-mounted into the cluster — which sits awkwardly
>    against the promise that a user's data stays "within the secure
>    environment."
>
> Both stem from one design choice: **datasets live as `chmod 777` files
> under `~/.tracebloc`, bind-mounted into the local cluster, and engineered
> to survive cluster deletion.**
>
> Grounded in a code-level read of the shipped installer (`client`
> `scripts/lib/{common,cluster}.sh`), the offboard command (CLI
> `internal/cli/delete.go`), and a controlled reproduction on a local k3d
> install (2026-07-21). **Concept only — no code proposed to merge yet.**
>
> Related: RFC-0001 (auth & client provisioning), RFC-0002 (data ingest flow
> & terminology), and the terminology source-of-truth (v2 `TERMINOLOGY.md`,
> docs `main`). Spans two repos: `cli` (offboard) and `client` (installer +
> chart).

## 1. Summary

Two problems, one root.

- **Problem A — "delete" doesn't reliably mean gone (§3).** For the default
  data directory, offboard *does* wipe correctly. But data written by an
  *earlier* install — a different on-host layout, an older version, or a
  custom directory — can be silently re-adopted by the next install, so a
  "fresh" reinstall isn't fresh. The failure isn't in the wipe; it's that
  **the installer adopts whatever it finds with no guard.**

- **Problem B — data lives "outside" the secure environment (§4).**
  Datasets are plain files in `~/.tracebloc` on the host (mode `777`),
  bind-mounted into the cluster. That is on the user's *machine* (so the
  real privacy promise holds) but is **not** sealed inside the cluster's
  isolation boundary, and it is readable by any local process.

- **Root cause.** The two are the same choice seen from different angles.
  Storing datasets as host files that *outlive the cluster* is what makes
  offboard fragile (A) **and** what puts the data outside the environment
  boundary (B). Fix the storage model and both improve together.

The guiding principle this RFC lands on: **separate restart-persistence
from delete-persistence** (§4.4). Users reboot their laptops and must keep
their data; users who offboard expect their data destroyed. Today's design
conflates the two.

## 2. How it works today (verified)

### 2.1 Where datasets physically live
- MySQL runs **in-cluster**; ingested datasets are tables in the
  `training_test_datasets` database.
- The MySQL PersistentVolume uses a **hostPath** under `/tracebloc/<release>/mysql`.
- The local k3d cluster **bind-mounts `~/.tracebloc → /tracebloc`**
  (`HOST_DATA_DIR`, default `$HOME/.tracebloc` — `common.sh:329`;
  mapping documented at `cluster.sh:40-41`).
- Net effect: dataset tables physically live at
  `~/.tracebloc/<release>/mysql` on the host disk. *(Confirmed: `.ibd`
  files present there before the repro.)*
- **Dataset *files*** (as opposed to the MySQL tables) can go to a separate
  location via `HOST_DATASET_DIR` — a network mount by design (backend#743)
  — bind-mounted to a **different** path, `/tracebloc-data` (`cluster.sh:312`).

### 2.2 Permissions
- The installer runs `chmod -R 777` on the mysql + logs directories
  (`cluster.sh:31` flat layout, `cluster.sh:50` per-release layout). The
  data is **world-readable/writable** to any local user or process.

### 2.3 Install-time behavior — the actual bug
- The installer does `mkdir -p "$HOST_DATA_DIR/…/{logs,mysql}"`
  (`cluster.sh:30`, `:49`). There is **no wipe and no "existing data
  detected" guard** — if data is already present, MySQL silently adopts it.
- Two layouts exist in the same script: a **flat** layout
  (`$HOST_DATA_DIR/{logs,mysql}`, `:30-31`) and a **per-release** layout
  (`$HOST_DATA_DIR/<release>/{logs,mysql}`, `:49-50`). A machine that has
  seen both (e.g. across a version bump) accumulates both — this is exactly
  the transition artifact behind Problem A.

### 2.4 Offboard behavior (`tracebloc delete`)
- Teardown order (`internal/cli/delete.go`): revoke credential → clear the
  active-client pointer → Helm uninstall → k3d cluster delete → prune images
  → `removeHostDataDir()` → remove self.
- `removeHostDataDir()` = `os.RemoveAll(config.Dir())` =
  `os.RemoveAll(~/.tracebloc)`.
- Flags: `--yes` (skip name confirmation), `--keep-data` (keep
  `~/.tracebloc`), `--force` (skip the online guard).
- **`HOST_DATASET_DIR` is never touched** — by design it's a network mount,
  but that means dataset *files* there survive offboard silently.
- Prints `✔ Removed local tracebloc data and config` — but the removal is
  **not verified** before the message prints.

### 2.5 What the 2026-07-21 reproduction actually showed
- An earlier hypothesis — "delete isn't deleting data" — was **wrong**.
  `tracebloc delete --force --yes` fully wiped `~/.tracebloc`: data, install
  marker, all 4 `.ibd` files, **0 survivors**.
- The earlier "data survived" observation was a **version/multi-install
  transition artifact**: mixed flat + per-release MySQL layouts across a
  0.9.2 → 0.9.3 bump, plus two CLI binaries installed
  (`~/.local/bin/tracebloc`, removed by delete, and `/usr/local/bin/tracebloc`,
  which survived).
- Conclusion: for the *default* dir, delete already clean-slates. The real
  gaps are (a) **alternate/legacy locations** delete doesn't know about
  (custom `HOST_DATA_DIR` from a prior install; `HOST_DATASET_DIR`), and (b)
  **the installer silently re-adopting** whatever it finds (§2.3).

## 3. Problem A — offboard should leave a clean slate

### 3.1 What we want
"Delete" should mean **the environment's data is gone** — current version
*and* any leftover from earlier versions/installs — so a reinstall starts
empty. This is both a UX expectation ("I deleted it") and a privacy
expectation (offboard = data destroyed).

### 3.2 The one real objection: scope
"Wipe *all* tracebloc data on the machine" is dangerous, because:

- A machine can host **more than one environment**. *(Confirmed: a second
  k3d cluster, `tb-copyreview`, exists on this Mac right now.)* A blind
  machine-wide wipe would destroy the other environment.
- `HOST_DATASET_DIR` may be a **shared network mount** used by other tools.
  Recursively deleting it is not ours to do by default.

So clean-slate must be **scoped to the environment being offboarded**, never
a machine-wide `rm -rf`.

### 3.3 Proposal
1. **Verified, scoped wipe.** Delete removes everything belonging to the
   offboarded environment — its `~/.tracebloc/<env>` across *both* layouts
   (flat + per-release), its `HOST_DATASET_DIR` data for that env, and any
   legacy remnants for *that* env — and **verifies the paths are gone before
   printing success** (don't claim `✔ Removed` on an unverified `RemoveAll`).
2. **Installer leftover-guard.** If `HOST_DATA_DIR/…/mysql` is non-empty at
   install time, the installer **stops and makes the user choose**
   (reuse / wipe / pick a new dir) instead of silently adopting it. *This is
   the guard that actually prevents the bug that was hit.*
3. **Keep `--keep-data`** as the explicit opt-out of the wipe.
4. **Optional `--purge-all`**, clearly labeled, for the rare "remove every
   tracebloc trace on this machine" case — **never** the default, and it must
   enumerate what it will destroy (including other environments) before
   proceeding.

## 4. Problem B — data lives "outside" the secure environment

### 4.1 The tension
We tell users their data stays "within the secure environment." Physically,
datasets are **plain files in `~/.tracebloc` on the host** (mode `777`),
bind-mounted into the cluster. That is not "inside the cluster's isolation
boundary."

### 4.2 What's true vs. what's imprecise
- **True (and the promise that matters):** the data never leaves the user's
  own machine / infrastructure. Nothing is uploaded anywhere. The
  federated-learning trust model holds completely.
- **Imprecise:** "within the secure environment," read as "sealed inside the
  cluster," is not accurate. The data is host files, readable by any local
  process — not sandboxed, not encrypted at rest.

### 4.3 Why it's on the host at all
Local Kubernetes (k3d) has no durable storage except host-backed volumes.
tracebloc bind-mounts to `~/.tracebloc` specifically so data survives
**cluster deletion + recreation**. Note the subtlety: a **restart**
(Docker off/on, `k3d cluster start`) already preserves the node container and
its storage — the bind-mount is only needed to survive a **delete + recreate**,
which is exactly the persistence questioned in §3. **So the storage choice
directly causes Problem A.**

### 4.4 The key principle
**Separate restart-persistence from delete-persistence.**

- Restart-persistence (must keep): laptop reboots, Docker restarts, cluster
  stop/start. The k3d node container already survives these.
- Delete-persistence (must not keep): offboard, `cluster delete`. Data
  surviving these is the bug, not the feature.

Today's bind-mount conflates them by making data survive *everything*.

### 4.5 Storage options (trade-offs)

| Option | Restart-persists | Delete wipes it | Host exposure | Notes |
|---|---|---|---|---|
| **A. Status quo** — bind-mount `~/.tracebloc`, mode `777` | ✅ | ⚠️ only if delete chases every path | ❌ world-readable files in `$HOME` | Survives delete → causes Problem A |
| **B. Docker managed named volume** (env-scoped) | ✅ | ✅ if the volume is removed on offboard | ✅ under Docker's dir, not `$HOME`, not `777` | Less exposed; lifecycle can bind to the env |
| **C. Node-local storage** (local-path provisioner, inside the k3d node) | ✅ (node container persists across stop/start) | ✅ (dies with `cluster delete`) | ✅ not visible as host files | Clean-slate by construction; loses "survive delete + recreate" |
| **D. B or C + drop `777` + encrypt at rest** | ✅ | ✅ | ✅✅ | Makes "within your secure environment" literally truer |

For **real clusters** (EKS/AKS/OpenShift/bare-metal), dataset storage already
lands on proper PVs on the customer's own infrastructure — that is fine and
stays as-is. This RFC's storage question is about the **local** install.

### 4.6 Security posture
- **Drop the `chmod 777`** regardless of which storage option — customer data
  should not be world-readable/writable. (Low-risk, high-value; can ship
  independently of the bigger decision.)
- **Consider encryption at rest**, so "secure environment" means more than
  "a directory on your box."

## 5. Messaging reconciliation
Whatever we build, the copy must match reality. Two directions:

- **Tighten the wording** to the always-defensible claim — "your data never
  leaves your infrastructure" — rather than implying cluster-sealed
  isolation; **or**
- **Change the storage** (Option C/D) so "within the secure environment"
  becomes literally true, and keep the stronger wording.

Best outcome: do the storage work *and* keep the strong claim honest. Align
final wording with the terminology source-of-truth ("secure environment" is
the decided term).

## 6. Non-goals
- Not changing how *remote* (cloud) clusters store data — PVs there are fine.
- Not breaking restart-persistence.
- Not (yet) selecting a specific volume implementation — §4.5 is the menu
  §7 decides from.

## 7. Decisions to make (Lukas + Asad)
- **[decision 7.1]** Offboard clean-slate: adopt the scoped, verified wipe
  (incl. `HOST_DATASET_DIR`) **plus** the installer leftover-guard? (§3.3)
- **[decision 7.2]** Local storage target: **A** (status quo), **B** (managed
  volume), **C** (node-local), or **D** (B/C + perms + encryption)? (§4.5)
- **[decision 7.3]** Drop `chmod 777` now, independent of the larger storage
  decision? (§4.6 — low-risk, high-value)
- **[decision 7.4]** Messaging: tighten copy, change storage, or both? (§5)
- **[decision 7.5]** Migration for existing installs if we move the store (§8).

## 8. Migration / rollout
If we move the store (Option B/C), existing installs have data in
`~/.tracebloc`. Options: a one-time migration (copy into the new store on
upgrade) or an accepted "re-ingest after upgrade" for local dev. Real
clusters are unaffected. Sequence this behind the installer's version
detection, and guard it so we **never silently strand data**.

## 9. Appendix — evidence (verified 2026-07-21)
- `client/scripts/lib/common.sh:329` — `HOST_DATA_DIR="${HOST_DATA_DIR:-$HOME/.tracebloc}"`
- `client/scripts/lib/common.sh:335` — `HOST_DATASET_DIR="${HOST_DATASET_DIR:-}"`
- `client/scripts/lib/cluster.sh:30`, `:49` — `mkdir -p …/{logs,mysql}` (no existing-data guard)
- `client/scripts/lib/cluster.sh:31`, `:50` — `chmod -R 777 …/logs …/mysql`
- `client/scripts/lib/cluster.sh:40-41` — PVs `/tracebloc/<release>/{data,logs,mysql}` ↔ `$HOST_DATA_DIR/<release>/…` via the k3d `-v` mount
- `client/scripts/lib/cluster.sh:312` — dataset bind mount `-v "${HOST_DATASET_DIR}:/tracebloc-data@all"`
- `cli/internal/cli/delete.go` — `removeHostDataDir()` = `os.RemoveAll(config.Dir())`; flags `--yes` / `--keep-data` / `--force`; `HOST_DATASET_DIR` untouched; success message unverified
- Repro: `tracebloc delete --force --yes` → `~/.tracebloc` gone, 0 `.ibd` survivors; earlier "survival" = version/multi-install transition artifact
