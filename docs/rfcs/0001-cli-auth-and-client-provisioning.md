# RFC 0001 — Browser-based auth & one-command client provisioning

> **Status: ACCEPTED — implemented.** The design in this RFC shipped in
> **CLI v0.4.0** ([cli#107](https://github.com/tracebloc/cli/pull/107)); the
> tracking epic ([cli#54](https://github.com/tracebloc/cli/issues/54)) is closed.
> This document is retained as the design-of-record. The Phase 2 items in §11
> (server-side token revoke [backend#887], short-lived auto-refreshing tokens,
> `client rotate`, atomic fleet enrollment) remain tracked but out of the v0.4.0
> scope. Owner: @saadqbal. Last updated: 2026-07-03.
>
> The revision history below is kept as the record of how the design converged;
> Rev 6–7 are accuracy fixes reconciling the doc with what actually shipped.
>
> **Rev 2 (2026-06-23)** folds in the code-grounded review on the tracking epic
> ([backend#830](https://github.com/tracebloc/backend/issues/830)) and a
> user-perspective teardown of the end-to-end CLI flow. Net change: the
> auth handshake turned out to be the *easy* half; the design now leads with the
> **client lifecycle on a machine** (§7), which is where the real bugs hide. Three
> product decisions are settled (§0).
>
> **Rev 3 (2026-06-23)** makes the **cluster the idempotency anchor** (1:1
> client↔cluster, keyed on the `kube-system` UID) and closes the review's blocking
> gaps: cross-account adoption (§6.3/§7.2/R6), the existing-fleet `cluster_id`
> backfill via the heartbeat (§4.5/§10/R7), server-side `logout` revoke (§7.5/§9),
> and the orphan password-reset gate (§7.9). cluster-id + Q1 + Q5 are confirmed (§12).
>
> **Rev 4 (2026-06-23)** adds a hardening pass from an independent security + product
> review: bootstrap supply-chain (R8), audit trail (R9), multi-env config clobber
> (R10, a confirmed bug), version negotiation (R11), clean uninstall (R12), plus
> machine-credential revoke + authenticated `cluster_id` (§9), the silent-failure
> story (§8.5), adopt-keeps-namespace (§7.2), and delete-mid-experiment (§7.4).
>
> **Rev 5 (2026-06-23)** adds **Appendix C — the implementation-grade API & data
> contracts** (grounded in the shipped CLI client) to pin before parallel work, so
> the doc can be built from directly without a separate SDD.
>
> **Rev 6 (2026-06-23)** closes the two anchor residuals flagged on #96: `cluster_id`
> is set/backfilled by the **CLI/installer** (the kubeconfig-holder that can read the
> `kube-system` UID) — **not** the heartbeat, whose sender can't (§4.5/§10/C.4/R7);
> and §7.2's account-scoped check now gates **adoption itself**, so the live-release
> path can't bypass the cross-account `409` (R6).
>
> **Rev 7 (2026-06-25)** FR correction (connect-flow FR on dev, #877): `logout`
> server-side revoke reframed as **pending** — today logout is **local-only**; the
> server-side revoke needs the `POST /auth/revoke` endpoint (backend#887, not built)
> + a CLI `logout`→call (backend#845 shipped only the `revoke()` primitive) —
> §6.3/§7.5/§9/§13/C.6. The earlier "revokes server-side via #845" claim overstated it.
>
> **Rev 8 (2026-07-06) — PROPOSED amendment, not yet accepted ([cli#136], draft).**
> Simplifies the client command surface to a strict **one machine = one client =
> one cluster** model and reaffirms the installer as the create front door. See
> **§15** for the full amendment. Net: `client use` is **removed** and the D3
> arrow-key picker is dropped (nothing to select); `client list` is **hidden**
> (kept only for the installer's client#303 pre-flight); `client create` stays
> **provision-only** (the one-line installer bootstraps the cluster, then calls it —
> the CLI does **not** reimplement k3s/k3d/GPU/proxy); `client delete` gains
> ownership of the inverse **teardown** (deprovision + Helm uninstall + local
> cluster delete), folding in R12. This narrows §6.2/§7.1/§7.3 and supersedes the
> §7.3 "selected-vs-connected" picker work (the closed [cli#133]).

## 0. Decisions settled in this revision

These were open forks between the first draft, the in-flight implementation, and
the review. They are now decided; the rest of the doc assumes them.

| # | Decision | Choice |
|---|---|---|
| D1 | **Setup is silent / auto, not interactive.** | Common path asks **zero questions**: name = sanitized hostname, location = auto-detect, both *surfaced* in progress and correctable with flags — never prompted. (§6.7, §7.7, §8) |
| D2 | **The machine credential is never shown.** | `client create` prints only name + status. The credential is written straight into the cluster secret (mode `0600`) + stored hashed in the backend, and never touches stdout, scrollback, the clipboard, or `~/.tracebloc`. Rotation = delete + recreate. (§7.1, §7.8, §9) |
| D3 | **Clients are referred to by a human handle, never a secret or backend id.** | The handle is the per-account-unique namespace **slug** (e.g. `munich-hospital-radiology`); bare `use` / `delete` open an arrow-key picker. The UUID / username / password are never displayed. (§7.1) |

The cluster identifier (`kube-system` UID) and the two epic scope calls — **air-gap
out of scope** (Q1) and **one client per cluster / re-parenting deferred** (Q5) —
are now confirmed (§12).

## 1. Summary

Replace the current "go to the web UI, hand-create a client, copy a Client ID +
password, paste them into the installer" onboarding with a single flow:

```
sign in (browser) → (this machine names + locates itself automatically) → done
```

The human authenticates once in a browser (works even on a headless box over
SSH), the CLI provisions the client automatically and silently, the machine
credential is written straight into the cluster, and the installer proceeds. No
copied secrets, no separate visit to `/clients`, and — in the common case — no
prompts at all.

## 2. Motivation

Today (`client/scripts/install.sh`) the operator must:

1. Sign up at `https://ai.tracebloc.io`.
2. Navigate to `https://ai.tracebloc.io/clients` and hand-create a client.
3. Copy a **Client ID + password** out of the web UI.
4. Paste both into the installer prompt — often over SSH, into a `curl | bash`
   process.

Problems:

- **Context-switch + copy-paste of two long secrets**, frequently over SSH —
  error-prone and intimidating for a junior operator.
- **The installer process sees the user's password.** For a piped-from-the-internet
  installer this is a trust and phishing concern, and it blocks SSO/MFA/SAML —
  table stakes for selling an on-prem, private-data product into regulated orgs.
- **The credential is a long-lived static secret** that doubles as both the
  human's proof and the machine's permanent credential (see §3.1).

Goal: make first-time setup *stupid simple* for a beginner, while staying secure
and working on the headless/remote/proxied boxes where tracebloc actually runs.

## 3. The core reframe — two identities, two contexts

### 3.1 Two identities, not one

There are **two** things being authenticated, with opposite lifetimes:

| | **Human** (account) | **Client** (machine/daemon) |
|---|---|---|
| Who | A person | An EC2 box / on-prem server |
| Lifetime | A login session | Runs 24/7 for months |
| Auth | Browser SSO/MFA | A long-lived machine credential |
| Created at | `ai.tracebloc.io` | Provisioned by backend |

**A client is attached 1:1 to a cluster** — one cluster holds exactly one client,
and a client connected to a live cluster is *in use*. Client identity is therefore
*per-cluster*: that is the anchor the whole lifecycle keys on (§7.2).

Today both collapse into one **Client ID + password**. The fix is **not** "browser
auth instead of credentials" — it's: *authenticate the human in the browser, and
let that authorization mint the machine credential automatically.* (This is the
`tailscale up` / `cloudflared tunnel login` / `aws sso login` model: a browser
authorizes a long-running daemon, the control plane issues the node its own key.)

### 3.2 Two operational contexts (this is where the work actually is)

Authenticating is the easy half. The review made clear that the hard half is the
**lifecycle of a client on a machine** — and the CLI lives in two contexts, with
a hand-off between them:

- **Account context** — *"you are a signed-in user."* You hold a user token and
  manage the *clients* (machines) in your account: create, list, select, delete.
- **Client context** — *"a client is active **and connected**."* Commands now act
  on the active client's **cluster** (on-prem data never leaves it): ingest a
  dataset, list datasets, delete a dataset.

The bridge between them — *create or select ⇒ a client is active on this machine*
— is the single most bug-prone seam in the product, because "active" is a local
pointer while the data commands need a *reachable cluster*. §7 is devoted to it.

**Command map (grounded to the repo, 2026-06-23):**

| Context | Command | Status | This RFC |
|---|---|---|---|
| (signed out) | any client/data command | — | must refuse with *"run `tracebloc login`"* (§7.6) |
| login | `tracebloc login` | ✅ merged (cli#83) | keep |
| account | `client create` | ⚠️ in flight (cli#84 / PR #92) | **revise** → silent + idempotent + auto name/location, never print the credential |
| account | `client use` / select | ⚠️ in flight | **revise** → by slug / arrow-key picker, not numeric id |
| account | `client list` | ⚠️ in flight | **revise** → show *selected* vs *connected* |
| account | `client delete` | 🆕 | **new** — destructive guards (§7.4) |
| account | `logout` · `auth status` | ✅ merged (cli#83) | revise → scope active client to account; show token expiry |
| client | `data ingest` | ✅ built (was `dataset push`) | bind target to the active client's cluster (§7.3) |
| client | `data list` | ✅ built (was `dataset list`) | same |
| client | `data delete` | ✅ built (was `dataset rm`) | same |

## 4. What already exists (grounded findings, refreshed 2026-06-23)

A survey of `backend`, `client-runtime`, and `cli` shows **most of the data model
and much of the CLI already exist** — the net-new surface is the browser handshake
plus the lifecycle wiring in §7.

### 4.1 Client = `EdgeDevice` (a Django `User` subclass)

`backend/metaApi/models/User.py:314`. A client *is* an account (`type=EDGE`) with
`username` (UUID) + hashed `password` — i.e. the "Client ID + password".

| Need | Already exists | Field |
|---|---|---|
| Human-readable display name | ✅ | `first_name` |
| Stable machine ID | ✅ | `username` (UUID) |
| DNS-safe slug | ✅ (separate) | `namespace` |
| Physical location (structured) | ✅ | `location` → `ZONE_CHOICES` (350+ Electricity Maps grid zones) |
| Carbon intensity | ✅ | `carbon_intensity` (gCO₂/kWh) |
| Account → many clients | ✅ | `account` FK |

The two requirements we set out to add — **a human-readable name** and **physical
location for gCO₂** — are *already* `first_name` and `location`. `location` is a
controlled vocabulary (`backend/metaApi/models/zone_choices.py`) wired to a real
carbon pipeline (daily Electricity Maps fetch → `CarbonIntensity` model →
`update_estimated_gco2()` → `ExperimentSustainabilityMatrix`). **No new model
fields required.**

### 4.2 Provisioning API already exists

`POST /edge-device/` (`EdgeDeviceViewSet`, permission `CanManageClient`). Writable
fields are exactly `('first_name', 'account', 'location', 'password')`
(`edge_device_serializer.py:94`). `username`/email are auto-generated server-side
(`create()`); `namespace` is **not** set here today — the client heartbeat
(`EdgeDeviceHeartbeatView`) stores whatever the client reports, **verbatim: no slug
derivation, no format validation, no uniqueness** (`common/utils/edge_device_utils.py`
keeps `client_info.namespace` as-is). A CLI holding a user token can call this
endpoint to auto-provision a client. (Namespace sequencing is the catch — see §6.6;
the slug rule + uniqueness are net-new — R4.)

### 4.3 Auth — the device grant is half-built

- DRF Token auth: `POST /api-token-auth/` (login → token), `POST /register/`
  (signup → token). **Google + GitHub OAuth already wired for web** — these mint
  the *same* token, so the activation page needs **no new IdP wiring** (Q6).
- **CLI side is shipped:** `tracebloc login` / `logout` / `auth status` +
  `internal/api` (env→base-URL, proxy/CA-aware HTTP) + `internal/config`
  (`~/.tracebloc/config.json`, `0600`) landed in **cli#83** (commit `e322613`).
  `login` already implements the RFC 8628 poll loop (`authorization_pending` /
  `slow_down` / `expired_token`).
- **Backend side is the gap:** the `/device/*` endpoints
  ([backend#835](https://github.com/tracebloc/backend/issues/835)) and the
  provisioning hardening ([backend#836](https://github.com/tracebloc/backend/issues/836))
  don't exist yet — `login` returns a clear *"this backend doesn't support browser
  login yet"* until they land. ← *This is the only net-new backend surface.*

### 4.4 There is already a Go CLI, and it's the right home

`tracebloc/cli` — Go + Cobra, v0.2.0+, cosign-signed, multi-arch, actively
maintained, and **already installed by the client bash installer**
(`client/scripts/lib/install-cli.sh`). It does dataset/ingest work and now carries
the auth scaffold (§4.3). This is the correct home for the lifecycle commands.

### 4.5 Energy/carbon telemetry

`client-runtime/Node-deploy/resource_monitor.py` sends CPU/GPU TDP + utilization
to `/edge-device-heartbeat/`. Carbon is computed backend-side from
`EdgeDevice.carbon_intensity` (location-driven). The heartbeat does **not**
auto-detect or report location — confirming location must be captured at
provisioning time, which is exactly what this RFC does (silently — §6.7). The
heartbeat *does* re-report `namespace` on every ping, which constrains §6.6. (It
does **not** carry `cluster_id`: the heartbeat sender can't read the `kube-system`
UID — the CLI/installer sets `cluster_id` instead — §7.2, R7.)

### 4.6 How data commands target a cluster today (sets up §7.3)

`data ingest` / `data delete` (today `dataset push` / `dataset rm`) resolve their cluster from
`--kubeconfig` / `--context` / `-n <namespace>` flags (default `$KUBECONFIG` →
`~/.kube/config`, current-context), then discover the parent release + shared PVC
by reading the chart's Deployment labels (`cluster.DiscoverParentRelease`,
`cluster.DiscoverSharedPVC`). **They do not read `config.ActiveClientID` at all.**
So today "the active client" and "the cluster the data commands act on" are two
unrelated mechanisms. Closing that gap is loophole §7.3.

## 5. Goals / Non-goals

**Goals**
- One-command setup for a beginner; **zero prompts in the common case** (D1).
- Browser-based human auth that works on headless/SSH/proxied boxes.
- Auto-provision the client (no manual `/clients` visit, no copied secrets, no
  printed secrets — D2).
- Capture a human-readable name + structured location at provisioning — silently.
- Keep a non-interactive path for automation (`--token`, env vars).
- Don't break existing Client ID + password installs (dual-mode).
- **Make the whole client lifecycle safe and idempotent** — re-runs, deletes,
  account switches, and interrupted installs all behave (§7).

**Non-goals (this RFC)**
- Building account signup *in the terminal* (browser activation page handles
  login **and** signup — keep ToS/GDPR/MFA/CAPTCHA where they already live).
- Short-lived/rotating client credentials + revocation (desirable; deferred to a
  later phase — see §11). Rotation in phase 1 = delete + recreate.
- Fleet/enrollment-key management UI (phase 2).
- **True air-gapped (no-egress) installs** — out of scope per Q1; see §6.5.

## 6. Proposed design

### 6.1 Auth mechanism — OAuth 2.0 Device Authorization Grant (RFC 8628)

Chosen because installs are **headless** (remote servers over SSH). The
browser-and-CLI need not share a machine, network, or continent. The CLI half is
already built (§4.3); the backend endpoints are §6.3.

Rejected alternatives:
- **Type credentials into the installer** (today): blocks SSO/MFA; exposes
  password to the script; phishing-prone.
- **Localhost-callback / PKCE** (`gcloud`, `vercel`): requires a browser on the
  *same* machine — breaks over SSH.
- **Paste a token**: kept as the *fallback* (§6.5), not the default.

### 6.2 CLI commands (in `tracebloc/cli`)

```
tracebloc login                 # device flow → store user token (~/.tracebloc, 0600)   [✅]
tracebloc logout                # clear token AND the active-client pointer (§7.5)        [revise]
tracebloc auth status           # account + env + token expiry + active/connected client  [revise]

tracebloc client create         # silent, idempotent provision for THIS machine (§7.2)    [revise]
tracebloc client list           # show each client's slug + selected/connected state      [revise]
tracebloc client use [<slug>]   # select by slug; bare → arrow-key picker (§7.1, §7.3)     [revise]
tracebloc client delete [<slug>]# guarded teardown; bare → picker (§7.4)                   [new]

tracebloc data ingest|list|delete  # act on the ACTIVE client's cluster (§7.3)             [revise]
```

**Data verbs — `ingest`, never `push` (locked, 2026-06-23).** The data commands are
`data ingest / list / delete`. *ingest*, not *push*: the data is loaded **into** the
client's own on-prem cluster and never leaves it — *push* wrongly implies sending it to
a remote (git/cloud) and undermines the product's core trust message. *delete*, not *rm*:
spelled-out and consistent with `client delete`, for beginner clarity. `dataset` +
`push`/`rm` stay as hidden aliases for one deprecation cycle. (Convention applies to all
copy, not just the command name.)

`login` stores a short-lived **user** token. `client create` mints the **machine**
credential and routes it straight into the cluster (never to stdout — D2/§9).
`client create` **operates against an already-reachable cluster** — it reads that
cluster's identity as the anchor (§7.2), so the k8s API must be up first (the
installer bootstraps the base cluster before calling `create`; a clear error if
none is reachable). `create` never *creates* a cluster.

Global: `--verbose` / `TRACEBLOC_LOG_LEVEL` for diagnosability (§8.5), and a
`--uninstall` teardown on `client delete` / the installer for clean offboarding
(R12). `tracebloc cluster doctor` extends to also check auth/config/token state, so
it can diagnose a failed *provision*, not just cluster health.

### 6.3 Backend additions (in `tracebloc/backend`)

- `POST /device/code` → `{ device_code, user_code, verification_uri,
  verification_uri_complete, expires_in, interval }`. ([backend#835])
- `POST /device/token` → polled by the CLI; returns a user token once approved
  (`authorization_pending` / `slow_down` / `expired_token` per RFC 8628). ([backend#835])
- A web **activation page** `https://ai.tracebloc.io/activate` — a *token-authed*
  endpoint that reuses the existing web login/signup (incl. Google/GitHub) and
  binds the approval to `request.user`. No new IdP wiring (Q6). It shows **what is
  being authorized** ("Connect machine *X* to account *Y*?") as the phishing
  mitigation.
- **Split the client permission read/write** (Q4): listing clients must not
  require `CanManageClient`. Today one permission gates both, so a user who may
  *select* an existing client but not *create* one gets a bare `403`. Split into a
  read scope (list/use) and a write scope (create/delete); a write `403` routes to
  "ask an admin" (§7.4), already stubbed as `askAnAdmin` in PR #92. ([backend#836])
- **Enforce `namespace` uniqueness** per-account at the DB layer
  ([backend#863]) — the CLI's collision suffix is advisory + racy; only a
  `UniqueConstraint(account, namespace)` actually guarantees it (§6.6).
- **Record the attached cluster + enforce one client per cluster** — add a
  `cluster_id` to `EdgeDevice` (no such field today — only `namespace`) carrying
  the target cluster's stable fingerprint (the `kube-system` namespace UID) with
  `unique=True`. This is the durable attachment record that makes `client create`
  idempotent across CLI-config loss and the pre-install orphan window (§7.2), and a
  stronger backstop than the namespace constraint: `create` becomes get-or-create
  keyed on `cluster_id`. Two musts, because the `kube-system` UID is **not a
  secret** (anyone with cluster access reads it):
  - **Account-scoped get-or-create.** Return the existing client **only if it's in
    the requester's account**; a `cluster_id` already bound to a *different* account
    is an explicit **409 conflict**, never a silent adoption — otherwise re-pointing
    a cluster from another account would hijack the first account's client (R6).
  - **Backfill existing clients.** `cluster_id` is net-new, so every current client
    has it null. The **CLI/installer** PATCHes it on the next run — it holds the
    operator kubeconfig and can read the `kube-system` UID, which the heartbeat
    sender can't (§10, R7). Until then, the §7.2 step-2a live-release guard stops a
    re-mint. (The backend accepts a CLI-supplied `cluster_id`; see C.3.)
  - (Anchor field + get-or-create + 409 + adopt-backfill: [backend#883],
    split out of #836 — which ships namespace validation + RBAC only (see PR
    [backend#862]). Server-side token revoke for `logout` lands as the `POST /auth/revoke`
    endpoint ([backend#887] + a CLI call); backend#845 shipped only the `revoke()`
    primitive, so logout is local-only until #887.)

### 6.4 Installer reorder (in `tracebloc/client`)

Move CLI install + `tracebloc login` + `tracebloc client create` to run **before**
the Helm install, because the minted credential feeds the chart. (Today the CLI
installs *after* the cluster.) The credential is written to the chart's
values/secret (mode `0600`) **before** `helm install` runs, so an interrupted
install can be resumed without re-minting (§7.9). Keep CLI-install failure
non-fatal only for the *data* convenience path, not for the auth path. The
installer's existing one-per-cluster guard and the CLI's idempotent
`create` must key on the **same** cluster identity (§7.2).

### 6.5 Fallbacks — automation (air-gap is out of scope)

- `TRACEBLOC_ENROLL_TOKEN` / `--token`: a pre-issued credential for
  Ansible/Terraform/CI/golden-images and for **egress-restricted-but-online**
  on-prem boxes (the #172 corporate-proxy segment). The device-flow HTTP client
  honors `HTTPS_PROXY`/`NO_PROXY` + custom CAs (reuse the #172 hardening).
- **True air-gap (no egress at all) is out of scope (Q1).** Preflight hard-fails
  on no egress; #172 is *corporate-proxy* support (TLS-inspecting proxy), not
  air-gap. If a real no-egress segment appears later, the enrollment-key fallback
  becomes first-class — but we will not design for a customer we don't have.
- Existing **Client ID + password** path stays working (dual-mode) for one full
  deprecation cycle.

### 6.6 Name → namespace: derive once, set both, then freeze

Today there are effectively two names: `first_name` (display) and `namespace`
(k8s). Asking for both is redundant; in the silent flow we ask for **neither**
(§6.7) — we derive both from the hostname.

> **All of this is net-new.** Today the backend does *no* namespace processing — it
> stores the client-reported `namespace` verbatim (§4.2), with no slug derivation,
> no format validation, and no uniqueness. The slug rule below, setting `namespace`
> at create, and the §6.3 constraint are all new work; the slug rule currently lives
> only in the CLI (`cli/internal/slug`) + Appendix A, not in the backend.

**Proposal: derive the `namespace` slug from the name once, at creation, set it on
*both* `EdgeDevice.namespace` and the install-time `TB_NAMESPACE`, and freeze it.**

Why derive-and-freeze, and why set *both*:

- **Kubernetes namespaces are immutable.** You cannot rename one, and the name is
  baked into resource names (`<ns>-jobs-manager`, `<ns>-requests-proxy`), DNS, and
  PVCs. Deriving once and freezing keeps `first_name` (mutable display) decoupled
  from `namespace` (frozen), matching the model.
- **The heartbeat re-reports `namespace` on every ping** (§4.5). So if the
  provisioned slug and the install-time `TB_NAMESPACE` disagree, the heartbeat
  will overwrite the backend's namespace and the two **drift**. Resolution (Q2):
  `name → slug → set EdgeDevice.namespace at create AND pass the same slug as
  TB_NAMESPACE to the chart`. They are equal by construction, so the heartbeat is
  a no-op re-report.

Derivation rules (reference algorithm + validation in Appendix A):

- **Slugify:** lowercase, transliterate unicode, spaces/punctuation → `-`, collapse
  repeats, strip to DNS-1123 (`[a-z0-9-]`, ≤63 chars, no leading/trailing `-`).
- **Collision-suffix:** append `-2`, `-3`, … against the account's existing
  namespaces. This is the friendly UX layer; the **DB constraint** (backend#863)
  is what actually guarantees uniqueness against races / direct API calls. It
  fires only for *different* clusters that derive the same base name — never for
  the same cluster re-running, which the cluster anchor catches first (§7.2).
- **Empty-slug guard:** a name that slugifies to empty (e.g. all-CJK) falls back to
  `client-<short-id>`.
- **Surface, don't ask:** show the derived slug in progress
  (`slug: munich-hospital-radiology`); `--namespace` overrides for multi-client
  hosts / naming conventions.
- **Backfill:** existing clients keep their current `namespace`; only set
  `first_name` as a display backfill — never re-derive an existing slug.

### 6.7 Location: silent auto-detect, never block

`location` is **optional at the model layer** (`CharField(..., blank=True)`), so a
client can be created with no location and `carbon_intensity` defaults to `0` —
i.e. it silently reads as "carbon-free", quietly corrupting the exact metric
tracebloc sells.

**Proposal: auto-detect the zone and use it silently; never prompt, never block,
never fake a zero.**

- **Detection (`internal/geo`, cli#93):** cloud instance metadata first (AWS
  IMDSv2/v1, GCP, Azure — probed concurrently under one short deadline, first to
  answer wins), GeoIP fallback (Cloudflare `cdn-cgi/trace`, flagged low
  confidence). Output is always an ISO alpha-2 country code, always a valid
  top-level `ZONE_CHOICES` value.
- **Silent (D1):** the detected zone is used directly and *surfaced* in progress
  (`Setting up gpu-box-01 in 🇩🇪 DE`), not prompted. `--location DE` overrides
  and skips detection entirely.
- **Never block (the §7.7 fallback):** bare-metal / offline / egress-restricted →
  no detection. Rather than prompt (D1) or fake a zero, fall back to an
  **account-default zone** if one is set, else mark the client **location-unset**
  (an explicit, visible state — *not* `0`) and nudge in the dashboard. Setup still
  completes.
- **Keep the DB `blank=True`** for backward compatibility; enforce "must be an
  explicit value or an explicit unset" at the provisioning layer, not with a DB
  constraint.
- **Mutable post-install** (`--location` / dashboard). Changing it affects
  **future** readings only — gCO₂ is a frozen per-experiment snapshot and is never
  re-derived from current location (Q3, confirmed in the carbon pipeline).

## 7. Client lifecycle — the loopholes and how the design closes them

This is the heart of rev 2. Each item is a user-perspective failure mode from the
review, with the resolution and its status. Ordered by how much rides on it;
**[decision]** items shape the command surface and were settled in §0.

### 7.1 Picking a client without ever showing an id or secret — **[D3]**

**Risk.** "No secret in the terminal" (D2) collides with `use` / `delete` / `list`,
which all need to *name* a client. Hide everything and the user can't pick one.

**Resolution.** The user-facing handle is the **namespace slug** (unique per
account, DNS-safe, human-meaningful, e.g. `gpu-box-01`) plus status — never the
backend UUID / username / password. `use <slug>` / `delete <slug>` take the slug;
run bare, they drop into an arrow-key **picker** over the account's clients. Only
the *credential* is truly hidden; the *name* is the interface.

**Rename is cosmetic.** The slug is frozen at creation (it *is* the k8s namespace —
§6.6); renaming a client changes only `first_name` (the display name), **not** the
handle — the slug you type in `use`/`delete` stays the original. The picker is right
for a handful of clients; an account with dozens needs filter/search (phase 2,
alongside the enrollment-key path).

### 7.2 Re-running setup must not mint a duplicate client — **[decision]**

**Invariant.** A client is attached to exactly one cluster, and a cluster holds
exactly one client (1:1, §3.1). Identity is *per-cluster*, so "does this host
already have a client?" is really *"is this cluster already attached to a
client?"* — and a cluster can always answer that.

**Risk.** Run the installer (or `client create`) twice against one cluster → two
backend clients, doubled "capacity", a confusing dashboard. Today `create` always
mints — and worse, the collision suffix (§6.6) turns the re-run into a *new* slug
(`gpu-server` → `gpu-server-2`), actively manufacturing the duplicate, because the
slug is derived against the account's namespaces *including this cluster's own*.

**Resolution.** `create` is **get-or-create keyed on the cluster**, not a mint.
The anchor is the **cluster identity** (proposed: the `kube-system` namespace UID —
the conventional stable fingerprint), readable *before* anything is installed, so
it exists from t=0 — which the CLI config, a mere cache, cannot provide once it is
lost:

1. Read the target cluster's identity (the `kube-system` UID) — and, if a tracebloc
   release already sits in the namespace, the in-cluster `TB_CLIENT_ID`.
2. **Ask the backend, account-scoped: is this cluster already mine?** This runs
   *before* any adoption, so the live-release path can't bypass it (R6):
   - **Owned by this account** — matched by `cluster_id`, or by the live
     `TB_CLIENT_ID` (whose null `cluster_id` the CLI backfills now) → adopt,
     reconcile/upgrade. **No mint.** (normal re-run / orphan resume, §7.9)
   - **Bound to a *different* account** → `409`, refuse — **never silent-adopt**,
     even when a live release occupies the namespace.
3. **Not attached anywhere** → mint, stamp `cluster_id`, write the credential
   (`0600`) before Helm, install.

One-client-per-cluster is enforced server-side by the `unique` `cluster_id` (§6.3):
a second attach returns the existing client, never a duplicate, even under a race.
Because step 2's account-scoped check gates **adoption itself** (not just the mint),
reading a live `TB_CLIENT_ID` off the cluster can't sidestep the cross-account
`409`. The installer's one-per-cluster guard and the CLI agree because they key on
the **same** cluster identity. The `-2`/`-3` suffix only disambiguates cosmetic name
clashes *across different clusters* — it can't produce a same-cluster duplicate,
because the cluster-id is checked first.

**Existing fleet — `cluster_id` is null until backfilled (R7).** Current clients
predate the anchor, so a bare `cluster_id` lookup can't match them yet. The guard:
the installer **never mints when a live tracebloc release already occupies the
target namespace** — it reads the in-cluster `TB_CLIENT_ID`, confirms (account-scoped,
step 2) that the client is this account's, **PATCHes the freshly-read `cluster_id`
onto it** (the CLI/installer holds the kubeconfig and can read the `kube-system` UID;
the heartbeat sender can't), and adopts. After that the cluster_id lookup takes over.
Without this guard, the first re-run on every *existing* box would mint a duplicate
and orphan the live client.

**Adopt keeps the existing namespace.** On the adopt branches (2a/2b), identity and
`namespace` come from the existing client/cluster — the silent flow's
hostname-derived slug and collision suffix apply **only** on the mint branch (step
3). A re-run from a different jump host (or after a hostname change) must never
re-derive a slug for an adopted client: that would drift the backend or mismatch the
live, immutable `TB_NAMESPACE`.

### 7.3 "Selected" is not "connected" — **[decision]**

**Risk.** `client use` sets a *local pointer*. But `data ingest` talks to the
client's **cluster** (§4.6). If the active client lives on another machine, ingest
can't reach it from here — and today the data commands don't even consult the
pointer, so they'd silently act on whatever `~/.kube/config` points at.

**Resolution.** Bind the active client to a **reachable cluster context**:

- The active client carries its `namespace`; data commands default `-n` to it and
  resolve a kube-context that hosts `<ns>-jobs-manager` (reuse
  `DiscoverParentRelease`). `--context` / `-n` still override.
- If no reachable context hosts the active client (it runs elsewhere), **fail
  clearly**: *"client `X` runs on another machine — run `data` commands there, or
  `tracebloc client use` a local one."* No silent wrong-target.
- `client list` distinguishes **selected** (the local pointer) from **connected**
  (cluster reachable + recent heartbeat = 🟢).

### 7.4 Delete is silently destructive — **[decision]**

**Risk.** Deleting a client orphans its cluster install + on-prem data; if it's
bound to a remote running machine, that machine's pods crash-loop (their backend
identity vanished).

**Resolution.** `client delete`:

- **Confirms** (the *one* justified interactive prompt — destructive; D1's "no
  prompts" is about *setup*, not destruction).
- **Refuses/warns** if the client is online (recent heartbeat), holds datasets, or
  has a **running experiment / training job** — the heartbeat guard is advisory, so
  gate on live job state too; a delete (or a `cluster_id`-changing rebuild) mid-run
  silently loses work. `--force` to override.
- **Offers to tear down** the local Helm release for the active client.
- **Checks RBAC** (write `403` → "ask an admin", §6.3).
- **Clears the stale active pointer** afterward (§7.5).

### 7.5 Stale active client — cross-account *and* cross-env (confirmed bug today)

**Risk.** The active client is cached locally. **`logout` today clears only the
token + email and leaves `ActiveClientID` set** ([auth.go](internal/cli/auth.go));
log into a *different* account and the cached pointer references a client you no
longer own → data commands hit something foreign (`401`/`403`, or worse, a
wrong-but-valid target).

**Resolution.** Scope the active client to the account:

- `logout` clears the active-client pointer (and `login` to a different account
  drops it if the client isn't in the new account).
- **`logout` must also revoke the token server-side**, not just locally — a DRF
  token is static, so a copied/leaked token survives a local-only clear for its full
  life (R2). **Not wired yet (FR-confirmed 2026-06-25):** today `logout` clears only
  the local token; server-side revoke needs the `POST /auth/revoke` endpoint
  ([backend#887], not built) plus a CLI `logout`→revoke call. backend#845 shipped
  only the underlying `revoke()` primitive.
- **Scope the active client to the *environment* too, not just the account (R10).**
  `~/.tracebloc` holds one `Env`+`Token`+`ActiveClientID`; `login --env` overwrites
  env+token but today leaves the *old* env's `ActiveClientID` stranded → prod
  commands silently hit a dev client. Either key config by env (a profile map) or
  clear / reselect the active client whenever `login` changes `Env`.
- Cheap re-validation on each client/data command: if the active client isn't in
  the signed-in account, drop it → *"your selected client is gone — pick one."*

### 7.6 Auth lifecycle & expiry

**Risk.** The user token expires mid-use → raw `401`s. Wrong env (dev vs prod). A
headless box with no browser.

**Resolution.** Map `401` → *"session expired — run `tracebloc login`."* `login`
sets the env; `auth status` shows account + env + **token expiry** (today it shows
account + env but not expiry — add it). Signed-out client/data commands refuse with
*"run `tracebloc login`."* Headless already works via the device flow (URL + code
to open on another device) — keep that copy clear.

### 7.7 Auto name & location with no prompt — **[D1]**

**Risk.** Silent setup means name = hostname and location = auto-detect — but two
machines can share a hostname, and a bare-metal host may have no detectable
location, and we've committed to *no prompts*.

**Resolution.** Name = sanitized hostname; hostname collisions get the `-2`
namespace suffix (§6.6) so the *slug* stays unique even when display names match.
Location = the cli#93 auto-detect; if undetectable, fall back to the account
default / `unset` rather than block (§6.7). Surface the chosen name + zone in
friendly progress (*"Setting up gpu-box-01 in DE"*) — visible but not
interactive; correct later with `--name` / `--location` or in the dashboard.

### 7.8 If nothing is shown, how does the user manage it later? — **[D2]**

**Risk.** "Never show the credential" (D2) is right — but the user still needs to
find, re-point, or rotate a client later.

**Resolution.** Management is always **by slug**, via the CLI (`list` / `use` /
`delete`) and the web app — never by handling secrets. The credential lives only
in the cluster secret (mode `0600`) + the backend (hashed); the CLI never persists
or prints it. **Rotation = delete + recreate** in phase 1 (a dedicated `rotate`
verb is a phase-2 nicety). Confirm the credential is genuinely
non-user-retrievable end to end.

### 7.9 Interrupted setup leaves an orphan

**Risk.** `login` ✓ → `create` ✓ → Helm **fails**. A client now exists in the
backend but no cluster runs it. A naive re-run mints a *second* orphan. Worse, if
the CLI minted the password and didn't route it anywhere before Helm failed, the
plaintext is lost (backend stores only the hash) and the orphan is unusable.

**Resolution.** The orphan is found by its `cluster_id` (§7.2 step 2b): the re-run
reads the same cluster identity, the backend returns the recorded client, and the
CLI **resumes into it** instead of minting a second.

- The CLI writes the machine credential into the chart's values/secret (`0600`)
  **before** invoking Helm. An interrupted install leaves that file in place, so
  the resume reuses the credential and just re-runs Helm.
- If the credential was lost (no values file — e.g. a fresh box), resume **resets**
  the orphan's password (a `PATCH` on the existing `EdgeDevice`) and rewrites it —
  but **only after confirming the client is offline** (no recent heartbeat). A
  client that connected once and *then* lost its local values file still has a
  running pod using the old credential; a blind reset would break it. Gate the reset
  on heartbeat recency, not just "no values file."
- Concurrent attaches to one cluster are serialized by the `unique` `cluster_id`
  constraint (§6.3); the loser fetches and adopts the winner.

## 8. UX — drafted flows

> **Tracked as four acceptance families** on the epic (#830) — **#877** connect/install ·
> **#878** manage clients · **#879** data · **#880** uninstall — that FR runs end-to-end,
> while the build work stays in the per-component tickets (§13). All four follow one
> **2-phase shape**: a single human gate (browser sign-in), then unattended, idempotent
> convergence. The design is graded against seven principles — front-loaded human gate ·
> idempotency-as-backbone · never-silent / never-lie · quiet-by-default · failure-UX
> first-class · secure-by-invisibility · installer = thin CLI orchestrator. The
> connect/install flow's full ordered step-spec lives with #877.

### 8.1 First-time, headless box (zero prompts)

```
$ bash <(curl -fsSL https://tracebloc.io/i.sh)
✔ Checking this machine… ready (8 CPU · 30 GiB RAM · 46 GiB free · network OK)

  To connect this machine, sign in to tracebloc:
     →  https://ai.tracebloc.io/activate
        code:  WDJB-MJHT
  Waiting for you to finish in your browser…  (Ctrl-C to cancel)

# (user opens URL on laptop → logs in / signs up → approves "WDJB-MJHT")

✔ Signed in as asad@acme.com
  Setting up this machine as “gpu-box-01” in 🇩🇪 DE   (rename later — display only; handle stays gpu-box-01)
✔ Provisioned — credential written to the cluster (not shown; managed by name)
→ Installing… (streaming rollout)  jobs-manager ✔  requests-proxy ✔  resource-monitor …  ~3 min
✔ Connected — this machine is 🟢 Online   https://ai.tracebloc.io/clients
```

No name prompt, no location prompt, **no credential on screen**. The two values
are *surfaced* and correctable, not asked (D1, D2).

### 8.2 Returning / re-run (already enrolled)

Read the target cluster's identity → it's already attached to a client (§7.2) →
**skip auth and setup entirely** → resume into that client and reconcile / upgrade.
Idempotent re-runs are non-negotiable; a re-run after a failed install resumes the
same client rather than minting a second (§7.9).

### 8.3 Automation (non-interactive, online)

```
TRACEBLOC_ENROLL_TOKEN=… TRACEBLOC_CLIENT_NAME="Lab A" TRACEBLOC_LOCATION=DE \
  bash <(curl -fsSL https://tracebloc.io/i.sh)   # zero prompts, zero browser
```

(True air-gap is out of scope — §6.5. This path still needs egress to the backend.)

### 8.4 Managing clients later (by name, never by secret)

```
$ tracebloc client list
  SLUG                          STATE                 LOCATION
  gpu-box-01  (active)          🟢 connected           DE
  munich-radiology              ⚪ selected-elsewhere   DE
  lab-a                         ⚫ offline              FR

$ tracebloc client use            # bare → arrow-key picker
$ tracebloc client delete lab-a   # confirms; refuses if online/holds data; offers teardown
```

### 8.5 When setup fails (headless, no prompts)

The flow is zero-prompt on a box with no human at the console, so the *failure*
path has to stand on its own — otherwise "easy when it works" becomes "stuck when
it doesn't":

- Always write a persistent `~/.tracebloc/install-<ts>.log`; add `--verbose` /
  `TRACEBLOC_LOG_LEVEL` that streams the device-flow → provision → Helm steps (today
  the CLI has no verbosity flag, only `--plain`).
- On any failure, print **the exact resume command** (re-running is idempotent —
  §7.2/§8.2) and point at `tracebloc cluster doctor`, **extended to also check
  auth/config/token state** (today it's cluster-health only, so it can't diagnose a
  failed *provision*).
- Stream per-Deployment rollout progress with an overall timeout and a "still
  pulling images" heartbeat, so a slow first run is distinguishable from a wedge —
  and *"🟢 Connected" must reflect a real readiness probe, not a sleep* (the core
  Deployments need readiness/liveness probes for that to be honest).

## 9. Security considerations

- **The credential never enters the terminal, scrollback, clipboard, shell history,
  or `~/.tracebloc`** (D2). The CLI generates it, `POST`s it (backend stores the
  hash), and writes the plaintext only into the cluster secret / Helm values
  (mode `0600`), which is also its sole durable home alongside the backend hash.
- Password leaves the installer process space entirely — the CLI only ever holds a
  device code, then a scoped user token, then routes the per-client credential to
  the cluster.
- Device-code phishing (RFC 8628): bind `user_code` → account, short TTL (the impl
  uses 10 min), **rate-limit `/device/code` + `/device/token`** (they are
  unauthenticated public surface — a DoS / guessing target), and an approval page
  that names the device and warns *"only approve if you started this on that box."*
- Secret-at-rest: write cluster credentials `0600`. (Observed `drwxrwxrwx` data
  dirs and a world-ish `values.yaml` on an existing box — tighten when we start
  auto-writing credentials.) Note the credential also lands **base64 in the k8s /
  Helm release Secret (etcd)** — acceptable for single-tenant on-prem, but state it
  as a conscious call (encrypt etcd at rest where the customer requires it).
- Tokens: store the user token `0600` in `~/.tracebloc`; `logout` clears it **and**
  the active-client pointer, and **must revoke it server-side** — but today it is
  **local-only** (FR-confirmed): server-side revoke is pending the `POST /auth/revoke`
  endpoint ([backend#887] + a CLI call; #845 is only the primitive). A local clear
  alone leaves a static DRF token valid for its full life (R2).
- Least privilege: list/use need only the read scope; create/delete need the write
  scope (§6.3, Q4).
- **Bootstrap supply-chain (R8) — the dominant gap for a regulated buyer.** "The CLI
  binary is cosign-signed" covers only the leaf. The `curl|bash` entry pulls ~14
  sub-scripts from a *mutable branch ref* with no checksum/signature, and the CLI
  installer's cosign check is **skipped when cosign is absent** (the default),
  degrading to a SHA256 fetched over the *same channel* as the binary. The most
  privileged code (writes the credential, runs Helm) is the least verified. *Fix:*
  pin to an immutable release tag, verify each sub-script against a signed manifest,
  and make signature verification mandatory on the default path.
- **Audit trail (R9) — table-stakes for SOC 2 / HIPAA.** Emit an append-only event
  (actor, account, `cluster_id`, time, source IP) for every device-flow approval,
  client mint/adopt/delete, credential issue/reset (§7.9), cross-account `409` (R6 —
  an *attempted* tenant crossing), and logout/revoke. None exists today.
- **Revoke a *compromised machine credential* in phase 1**, not just delete+recreate:
  a stolen edge-node key needs a first-class backend "revoke this client now"
  (invalidate the hash, force re-enroll) independent of cluster teardown — and don't
  gate the §7.9 reset on heartbeat recency alone (the heartbeat isn't authenticated).
- **`cluster_id` is set by the authenticated CLI, never self-reported.** The
  CLI/installer reads the real `kube-system` UID with the operator's kubeconfig and
  sets `cluster_id` on a Bearer-authed call (create / adopt-backfill — §7.2), so
  there's no unauthenticated client self-report to spoof and the heartbeat does
  **not** carry it. Backend: reject a `cluster_id` change on an already-set client
  except via an authorized re-enroll.
- **Tenancy boundary (state it).** The 1:1 client↔cluster rule + `cluster_id`
  uniqueness implies **one account per cluster** — make that the explicit isolation
  invariant; if multi-namespace-per-cluster is ever allowed, document the namespace
  trust boundary (it gates `DiscoverSharedPVC`, §7.3 targeting, and the shared etcd).

## 10. Backwards compatibility & migration

- Dual-mode: Client ID + password and `--token` paths keep working for one
  deprecation cycle.
- Backfill `first_name` for existing clients; do not touch `namespace`.
- **`cluster_id` backfill (blocking — R7).** The anchor is net-new, so every
  existing client has `cluster_id=null` and a `cluster_id` lookup can't match them.
  The **CLI/installer** backfills it on the next run: it reads the `kube-system` UID
  (operator kubeconfig) and PATCHes it onto the adopted client (§7.2, C.3). The
  heartbeat sender can't read that UID, so it is **not** the carrier. Until a box is
  re-run, the §7.2 step-2a live-release guard stops a re-mint — so **that guard must
  ship with or before the idempotent installer**, or existing customers
  double-provision on their next upgrade.
- The DB namespace uniqueness constraint (backend#863) must ship with a migration
  that resolves any *existing* collisions first — run the Appendix A check (R4).
- Deprecate the manual `/clients` "create" path only after device flow is GA;
  keep `/clients` as **manage/revoke**.

## 11. Phased rollout

**Critical path (Phase 1)** — the CLI is mostly done or quick; it is *gated on*
backend + frontend work, not the reverse (R1): device endpoints (backend#835)
**and** the frontend `/activate` page unblock `login`; the RBAC read/write split
(backend#836) unblocks `list` + the picker (D3); namespace uniqueness (backend#863)
unblocks safe idempotent `create` — *after* the R4 collision check.

- **Phase 0** (no backend work, partly shipped): stop sending users to "create a
  client first"; add `--token` / `TRACEBLOC_ENROLL_TOKEN` so the secret isn't typed
  inline. The CLI auth scaffold (cli#83) is already merged.
- **Phase 1 — backend + frontend first:** device-flow endpoints (backend#835); the
  `/activate` page (frontend — **assign an owner, R1**); RBAC read/write split + a
  `unique` `cluster_id` field (backend#836); namespace uniqueness *after the R4
  check* (backend#863).
- **Phase 1 — CLI, once the above land:** revise `client create` to silent +
  idempotent (cluster anchor) + never-show + auto name/location; add `client delete`
  (+ `--uninstall`, R12); slug + picker for `use`/`delete`; selected-vs-connected in
  `list`; wire the active client → cluster context (§7.3); env-scope the active
  pointer (R10); send a CLI version header (R11); `--verbose` + failure flow (§8.5);
  installer reorder. Dual-mode.
- **Phase 1 — security must-haves (regulated buyer):** bootstrap supply-chain
  hardening (R8); the provisioning/auth **audit trail** (R9); a machine-credential
  **revoke**; and **authenticated `cluster_id`** claims (§9). Not deferrable for the
  on-prem/regulated sell.
- **Phase 2** (hardening): short-lived auto-refreshing tokens + **server-side token
  revocation (R2)**; a `client rotate` verb; atomic fleet enrollment (R5); picker
  filter/search at fleet scale.

## 12. Open questions — resolved on backend#830 (owner to confirm the two product calls)

The original §11 questions were worked to resolution with a code-grounded sweep on
the tracking epic ([backend#830]). Most are dictated by the code:

| Q | Topic | Resolution |
|---|---|---|
| Q1 | Air-gap segment | **Out of scope** (confirmed). Egress-restricted-but-online (TLS-inspecting proxy, #172) is in; true no-egress is not. |
| Q2 | Namespace derivation | `name → slug → set both EdgeDevice.namespace + TB_NAMESPACE` (heartbeat re-reports namespace, so they must be equal — §6.6). |
| Q3 | Location-change semantics | **Future-only**, already clean — gCO₂ is a frozen per-experiment snapshot, never re-derived. |
| Q4 | RBAC | **Split read from write**; write `403` → "pick existing / ask an admin" (§6.3, §7.4). |
| Q5 | Multi-client / re-parenting | **One client per cluster** (confirmed; 1:1, enforced by `unique` `cluster_id` — §6.3 / §7.2); "multi-client per host" means multi-*cluster*, one client each. **Re-parenting deferred** (the viewset force-stamps `account`). |
| Q6 | Device-flow IdP | **Reuse the existing web login as-is**; `/activate` is a token-authed endpoint binding to `request.user` — no new IdP wiring. |

**Cluster identifier — confirmed:** the `kube-system` namespace UID (`EdgeDevice`
gains a `cluster_id` for it — §6.3). The §7.3 "connected" check falls out of it:
cluster-id match + heartbeat recency. With `cluster_id` as the authoritative
one-per-cluster guard, the per-account namespace `UniqueConstraint` (backend#863) is
demoted to **cosmetic cross-cluster dedup**, not an idempotency guarantee.

## 13. Work breakdown (cross-repo; tracked on backend#830)

- **`backend`**: `/device/code` + `/device/token` + activation page (#835);
  provisioning hardening + RBAC read/write split + a `cluster_id` field
  (`unique=True`) with an **account-scoped** get-or-create that gates **adoption**
  (cross-account = `409`, even on the live-release path — R6/§7.2) and accepts a
  CLI-supplied `cluster_id` (set at create, PATCHed on adopt-backfill — R7) (#836);
  server-side token revoke for `logout` (the `POST /auth/revoke` endpoint #887; #845
  shipped only the `revoke()` primitive); `namespace`
  `UniqueConstraint(account, namespace)` + collision migration after the R4 check
  (#863). Plus an append-only **audit trail** (R9), a **machine-credential revoke**
  endpoint, and a **min-supported CLI version** advertised for skew handling (R11).
- **`cli`**: revise `client create` → silent + idempotent (get-or-create keyed on
  the cluster identity — read the target cluster's `kube-system` UID, confirm
  account-scoped ownership, then adopt + backfill `cluster_id` onto a live
  in-namespace release or an orphaned client before minting) + never-show + auto
  name/location (cli#84/#92); location auto-detect (cli#93); `client delete`; slug +
  picker for `use`/`delete`; selected-vs-connected `list`; bind active client →
  cluster context for the `data` commands; scope the active pointer to the account,
  clear **and server-side revoke** on logout (CLI half of [backend#887]; #845 is only
  the primitive); `auth status` token expiry.
  Also: a `User-Agent: tracebloc-cli/<ver>` version header (R11); env-scoped config /
  profiles (R10); `--verbose` + `~/.tracebloc/install-*.log` and `cluster doctor`
  auth/config checks (§8.5); `client delete --uninstall` (R12).
- **`client` (installer)**: reorder CLI install + auth before Helm; write the
  credential to values/secret (`0600`) before Helm; share the one-per-cluster
  cluster-id anchor with the CLI; dual-mode env fallbacks; idempotent re-run /
  orphan resume. Plus **supply-chain hardening** (immutable release tag, sub-scripts
  verified against a signed manifest, mandatory signature check — R8); a first-class
  **uninstall/offboard** path (tear down the release, de-register the `EdgeDevice`,
  clear local state — R12); and streamed per-Deployment rollout progress (§8.5).

## 14. Risks & dependencies

The §7 loopholes are bugs *inside* the flow. These are the risks *around* it —
delivery sequencing, security blast radius, and migration. **R1–R12 are the ones to
act on** (R6–R9 are the blocking finds; R8–R12 came from the latest hardening pass);
the rest are watch-items.

### R1 — Critical path crosses three repos, and one piece is unowned

`login` is merged but inert until the device endpoints (backend#835) ship; the
slug + picker (D3) and "ask an admin" need the RBAC read/write split (backend#836)
— today one permission gates both *list* and *create*, so a normal user can't even
list to pick; and the **`/activate` page is frontend work** (the Next.js app), not
covered by the backend / cli / client breakdown (§13). *Mitigation:* make the
dependency chain explicit (§11) and **assign the activation page before Phase 1** —
it is the single point of failure for the whole device flow.

### R2 — The blast radius is the user token, not the machine credential

The device-issued user token is account-scoped (create/delete *any* client),
long-lived, stored `0600` on every edge box, and `logout` only clears it locally —
DRF tokens are static, so a leaked token stays valid for its full life regardless
of logout. One compromised box = fleet-wide client control until expiry. D2 hid the
*small* secret and left the *big* one on disk. *Mitigation (§9):* scope the device
token to provisioning, short TTL + refresh, **discard it after install on
unattended boxes** (unneeded once the machine credential is in the cluster), and
**revoke server-side on logout** via the `POST /auth/revoke` endpoint ([backend#887];
#845 shipped only the `revoke()` primitive) — a Phase-1 must, **not wired yet**:
logout is local-only today (FR-confirmed 2026-06-25).

### R3 — The cluster anchor has a precondition

Keying idempotency on the cluster identity (§7.2) assumes the cluster's k8s API is
already up when `create` runs — true for managed/EKS, but the bare-metal "bootstrap
k3s *then* install tracebloc" path now has a hard order: **k8s up → read cluster-id
→ create → helm install** (§6.4 must mean the *tracebloc* chart, not the base
cluster). And the `kube-system` UID changes on a cluster rebuild → a rebuilt
cluster reads as new and mints a new client, orphaning the old. *Mitigation:* state
the ordering in §6.4 + the installer; treat "rebuilt cluster = new client" as
intended, and add a **reaper / teardown hook** (or a dashboard sweep) so orphaned
`EdgeDevice`s from rebuilds don't accumulate — their `cluster_id` never returns.

### R4 — The namespace-uniqueness migration can hit an immutability wall

**Nothing prevents namespace collisions today** — the backend stores the
client-reported `namespace` verbatim (§4.2): no validation, no dedup, no uniqueness.
The only guard is the CLI's advisory `-2/-3` suffix, which a race, an older client,
or a direct API call all bypass. So backend#863's `UniqueConstraint(account,
namespace)` must assume existing duplicates and **resolve them in the migration
first** — and because `namespace` *is* the live k8s namespace (**immutable**), a real
collision can't be renamed; resolving it is destroy + rebuild of a running client.
*Mitigation:* the **code** already tells us collisions are unprevented; only the
**data** tells us whether any *exist* (paper cut vs wall), so the read-only collision
check (Appendix A) is the implementer's pre-migration step against staging/prod — not
an RFC blocker.

### R5 — Fleet provisioning is a thundering herd on that constraint

Automation across N identically-named boxes (`TRACEBLOC_ENROLL_TOKEN`,
Ansible/Terraform) all derive the same base slug, TOCTOU-collide, and retry against
the unique constraint at once. The cluster-id dedup covers same-box re-runs, not
N-different-boxes-same-name. *Mitigation:* atomic server-side suffix allocation, or
a per-box name convention in the automation contract (name + location are already
per-box via env, §8.3).

### R6 — Cross-account cluster adoption (blocking)

The `kube-system` UID is not a secret, so a *global* `cluster_id` + blind
get-or-create would let account B — pointed at account A's cluster — silently adopt
A's client. *Mitigation:* account-scoped get-or-create; a cross-account `cluster_id`
is a `409` conflict (§6.3 / §7.2).

### R7 — Existing fleet has no `cluster_id` → re-run double-provisions (blocking)

`cluster_id` is net-new, so every current client is null and a `cluster_id` lookup
won't match it — a naive re-run on a live box would mint a duplicate and orphan the
running client. *Mitigation:* the **CLI/installer** backfills `cluster_id` (it reads
the `kube-system` UID with the operator kubeconfig and PATCHes it on adopt — the
heartbeat sender can't read that UID), **and** the §7.2 step-2a "never mint over a
live in-namespace release" guard covers the window until a box is re-run. **Ship the
guard with/before the idempotent installer.**

### R8 — Bootstrap supply-chain is unverified (blocking, security)

`curl|bash` pulls ~14 sub-scripts from a mutable branch ref (no checksum/signature),
and the CLI installer's cosign check is skipped when cosign is absent (the default),
falling back to a same-channel SHA256. The most privileged code is the least
verified — upstream of every §9 mitigation. *Mitigation:* immutable release tag +
signed sub-script manifest + mandatory signature verification (§9). *(installer / cli
repos; named here.)*

### R9 — No audit trail (blocking, compliance)

No who-did-what-when record of provisioning / login / delete / credential-reset /
cross-account `409`. SOC 2 / HIPAA require it; the §2 regulated-org thesis depends on
it. *Mitigation:* append-only audit events from the backend, exportable (§9).
*(backend.)*

### R10 — Multi-env config clobber (confirmed bug today)

`~/.tracebloc` holds one `Env`+`Token`+`ActiveClientID`; `login --env` overwrites
env+token but strands the previous env's `ActiveClientID`, so a dev/stg/prod user
silently targets the wrong client. §7.5 handled only cross-*account*. *Mitigation:*
env-scoped config (profiles) or clear/reselect the active client on `Env` change
(§7.5). *(cli.)*

### R11 — No CLI↔backend↔chart version negotiation (future-proof)

The api client sends no `User-Agent`/version header, there's no min-version
handshake, and the chart `appVersion` isn't reported — three repos on different
cadences + a new contract = undefined failures on skew. *Mitigation:* CLI sends a
version header; backend advertises a min-supported version with a clear "upgrade
required" message; `chart_version` on the heartbeat. *(cli + backend + chart.)*

### R12 — No clean uninstall / offboarding (ease-of-use, future-proof)

There's no inverse of install: nothing stops the heartbeat, de-registers the box, or
clears local state, so every retired/rebuilt box becomes an R3 orphan. *Mitigation:*
`client delete --uninstall` / installer `--uninstall` that tears down the release,
de-registers the `EdgeDevice`, and clears `~/.tracebloc` — the intended cure for R3
rebuild-orphans. *(cli + installer.)*

### Watch-items (not blockers)

- **`client list` "connected" is two data sources** (§12): local kube-context
  reachability ("connected *here*") vs the backend heartbeat ("online *somewhere*")
  — different columns, and per-row cluster probing is slow.
- **Transient credential file:** the values/secret written pre-Helm (§7.9) is a
  third home for the secret — specify its cleanup post-install.
- **Heartbeat staleness** makes the delete "is it online?" guard advisory; the
  teardown step is the real safety (§7.4).
- **`/device/code` is unauthenticated public surface** — rate-limit + sufficient
  `user_code` entropy, or it's a DoS / guessing target (§8).
- **Compromised-client (FL) threat model** — name what a valid machine credential
  can/can't do to *other* tenants' training (poisoned gradients, dataset-size lies,
  model exfiltration), and link the FL-runtime doc that owns Byzantine defenses.
- **Data-residency attestation** — the auto-detected `location` (§6.7) is
  residency-grade but used only for carbon; record its provenance and expose a
  per-client residency attestation (a GDPR signal) separate from the mutable zone.
- **Emoji / flag glyphs** mojibake in CI logs and Windows consoles — gate *glyphs*
  (not just color) on a TTY, ASCII fallbacks (`[ok]`/`[x]`), and prefer the plain
  zone code (`DE`) over a flag emoji.
- **`--token` re-apply** should run the same cluster-id get-or-create (a Terraform
  re-apply of the *same* box is then a no-op) and fail clearly if the k8s API isn't
  reachable yet (R5 covers *different* boxes, not same-box re-apply).

## Appendix A — name→slug reference rule & validation

Reference algorithm (CLI ports to Go; Python shown for prototyping):

```python
import re, unicodedata

def slugify_dns1123(name: str) -> str:
    s = unicodedata.normalize("NFKD", name).encode("ascii", "ignore").decode()
    s = re.sub(r"[^a-z0-9]+", "-", s.lower())   # non-alnum -> hyphen
    s = re.sub(r"-+", "-", s).strip("-")         # collapse repeats, trim
    return s[:63].rstrip("-")                     # DNS-1123 label ≤63

def derive(name, existing: set) -> str:
    base = slugify_dns1123(name) or f"client-{short_id()}"  # empty-slug guard
    slug, n = base, 2
    while slug in existing:                                  # collision suffix
        suf = f"-{n}"; slug = base[:63-len(suf)].rstrip("-") + suf; n += 1
    return slug
```

Prototype run (2026-06-05) — every output is DNS-1123-valid (`[a-z0-9]([a-z0-9-]*[a-z0-9])?`, ≤63):

| Input | Slug | Note |
|---|---|---|
| `divya` | `divya` | existing ns — backfill-safe, unchanged |
| `Munich Hospital — Radiology` | `munich-hospital-radiology` | em-dash + spaces |
| `GPU Server` ×3 | `gpu-server`, `gpu-server-2`, `gpu-server-3` | collision suffix |
| `  Acme   Research  Lab #1  ` | `acme-research-lab-1` | trim + collapse |
| `Klinikum München Röntgen` | `klinikum-munchen-rontgen` | transliteration |
| `São Paulo Edge` | `sao-paulo-edge` | transliteration |
| `北京医院` | `client-<id>` | all-CJK → empty-slug guard |
| `---` | `client-<id>` | punctuation-only → guard |
| `a`×80 / very long name | (truncated to 63) | length cap |

**Known sharp edge:** a mixed name like `東京-Lab` slugifies to just `lab` (only the
ASCII survives transliteration) — semantically lossy. Acceptable for a derived slug
(display name is preserved), and the slug is surfaced in progress for the rare
hand-run that wants to override.

**On "authoritative":** with cluster-id confirmed as the one-per-cluster guard
(§7.2), **`cluster_id` is authoritative for idempotency**; the per-account namespace
`UniqueConstraint` (backend#863) is **cosmetic cross-cluster dedup** — it keeps two
*different* clusters' slugs distinct, but is no longer what prevents a same-cluster
duplicate. The CLI `-2/-3` suffix remains best-effort UX (TOCTOU). Run this
**read-only** check (R4) against staging/prod
*before* adding the constraint — it reports the `(account, namespace)` collisions
that would block it, plus slug drift:

```python
# READ-ONLY.  python manage.py shell < check_namespace_collisions.py
from collections import Counter
from metaApi.models import EdgeDevice
try:
    from common.utils.slug import slugify_dns1123          # the authoritative rule
except Exception:                                           # fallback = RFC Appendix A rule
    import re, unicodedata
    def slugify_dns1123(name):
        s = unicodedata.normalize("NFKD", name or "").encode("ascii", "ignore").decode()
        s = re.sub(r"[^a-z0-9]+", "-", s.lower())
        return re.sub(r"-+", "-", s).strip("-")[:63].rstrip("-")

rows = list(EdgeDevice.objects.values("id", "account_id", "first_name", "namespace"))
print(f"EdgeDevices: {len(rows)}")

# (1) THE BLOCKER for UniqueConstraint(account, namespace): existing duplicates.
acct_ns = Counter((r["account_id"], r["namespace"]) for r in rows if r["namespace"])
blockers = {k: c for k, c in acct_ns.items() if c > 1}
print(f"(1) (account, namespace) collisions [BLOCK the per-account constraint]: {len(blockers)}")
for (acct, ns), c in sorted(blockers.items(), key=lambda x: -x[1]):
    ids = [r["id"] for r in rows if r["account_id"] == acct and r["namespace"] == ns]
    print(f"    account={acct} namespace={ns!r} x{c}  ids={ids}")

# (2) global namespace collisions [only matter if you pick a global constraint].
g = Counter(r["namespace"] for r in rows if r["namespace"])
print(f"(2) global namespace collisions: {sum(1 for v in g.values() if v > 1)}")

# (3) blank namespace (never heartbeated) + (4) name->slug drift (informational).
print(f"(3) blank namespace: {sum(1 for r in rows if not r['namespace'])}")
drift = [(r["id"], r["first_name"], slugify_dns1123(r["first_name"]), r["namespace"])
         for r in rows if r["namespace"] and slugify_dns1123(r["first_name"]) != r["namespace"]]
empty = [r["id"] for r in rows if not slugify_dns1123(r["first_name"])]
print(f"(4) name->slug != stored namespace: {len(drift)};  slugify-to-empty: {len(empty)}")
for rid, nm, d, ns in drift[:30]:
    print(f"    id={rid} name={nm!r} derived={d!r} stored={ns!r}")

print("\nVERDICT: (1) must be 0 before adding UniqueConstraint(account, namespace).")
print("If > 0, those clients share a namespace — and k8s namespaces are immutable,")
print("so resolving a collision is destroy + rebuild, not a rename (RFC R4).")
```

## Appendix B — closest prior art

Tailscale (daemon enrollment via browser → node key — nearly our exact shape),
GitHub CLI (device-flow ergonomics), AWS SSO (headless device flow), cloudflared
(browser-authorized long-running tunnel).

## Appendix C — API & data contracts (pin before parallel work)

Everything above is design altitude; this is the wire contract the repos must agree
on. Shapes are **grounded in the CLI client already shipped/in-flight**
(`internal/api/client.go`) — the backend must match these or reconcile deliberately.
`[NEW]` marks net-new backend work; everything else the CLI already encodes.

### C.1 Conventions

- **Base URLs:** dev `https://dev-api.tracebloc.io` · stg `https://stg-api.tracebloc.io`
  · prod `https://api.tracebloc.io` (CLI `ResolveEnv`).
- **Auth:** `Authorization: Bearer <token>` (ClientAccessToken — keyword **`Bearer`**,
  *not* DRF's `Token`).
- **Versioning `[NEW]` (R11):** every CLI request sends
  `User-Agent: tracebloc-cli/<ver> (<os>/<arch>)`; backend MAY reply `426 Upgrade
  Required` + `{ error: "upgrade_required", min_version }` below the floor.

### C.2 Device Authorization Grant (RFC 8628) — backend#835 `[NEW]`

```http
POST /device/code        # unauthenticated; rate-limit (§9). CLI sends no body.
 200 { device_code, user_code, verification_uri, verification_uri_complete,
       expires_in, interval }

POST /device/token       # unauthenticated; rate-limit. Polled every `interval`s.
 req  { device_code }
 200  { token }                            # approved
 400  { error: "authorization_pending" }   # keep polling
 400  { error: "slow_down" }               # keep polling, interval++
 400  { error: "expired_token" }           # terminal
 400  { error: "access_denied" }           # terminal (denied in browser)
        # CLI keys off these EXACT error strings; pending/slow_down are non-terminal.

GET  /userinfo/          # Bearer → { email, type, account }  (CLI confirms token post-login)
GET  /activate           # frontend, token-authed (R1): "connect machine X to account Y",
                         #   binds approval to request.user (RFC §6.3)
```

### C.3 Provisioning — `/edge-device/` (Bearer) — backend#836 (namespace + RBAC); the `[NEW]` `cluster_id` items: backend#883

```http
POST /edge-device/       # get-or-create, account-scoped on cluster_id
 req  { first_name, namespace, location, password, cluster_id[NEW] }
        # account stamped from the token (Q5); password CLI-generated, write-only
 201  <ProvisionedClient>          # minted a new client
 200  <ProvisionedClient>  [NEW]   # adopted the existing client for this cluster_id
                                   #   (same account) — the idempotent re-run (§7.2)
 409  { error:"cluster_conflict", cluster_id }  [NEW]
                                   # cluster_id bound to ANOTHER account — never
                                   #   silent-adopt (R6)
 403  → ask-an-admin               # no CLIENT_WRITE (§7.4 / Q4)

ProvisionedClient = { id, first_name, username, namespace, location, status,
                      cluster_id[NEW] }
        # status = the online/offline code `client list` renders; cluster_id NEW

GET    /edge-device/            → { next, results: [ProvisionedClient] }  # account's clients (paginated)
GET    /edge-device/admins/     → [ { name, email } ]                      # Q4 ask-an-admin
DELETE /edge-device/<id>/  [NEW] → 204                                     # `client delete` (§7.4);
                                   #   guards are client-side + server RBAC
PATCH  /edge-device/<id>/  [NEW] { cluster_id } → 200    # adopt-backfill (R7): set cluster_id on
                                   #   an existing (null) client the CLI found live in-cluster;
                                   #   account-scoped; 409 if already set to another cluster
```

### C.4 Heartbeat — unchanged for `cluster_id` (R7)

The heartbeat sender (jobs-manager) has no `namespaces` RBAC and **cannot read the
`kube-system` UID**, so it does **not** carry `cluster_id`. The CLI/installer sets
and backfills `cluster_id` instead (C.3 create + adopt-`PATCH`; the kubeconfig-holder
reads the UID and the call is Bearer-authed — no self-reported claim to spoof, §9).
The heartbeat keeps reporting `namespace`/`status`; "connected" (§7.3) = recent
heartbeat + the CLI-set `cluster_id`.

### C.5 Audit events — backend `[NEW]` (R9), append-only + exportable

```
event = { id, ts, actor{ email, account }, action, cluster_id?, client_id?, source_ip, meta }
action ∈ { device.approve, client.create, client.adopt, client.delete,
           credential.issue, credential.reset, client.conflict_409,
           auth.login, auth.logout, token.revoke }
```

### C.6 Revoke — endpoint [backend#887] `[NOT YET BUILT]` (primitive: backend#845)

```http
POST /auth/revoke        # Bearer → 204   # `logout` MUST call this (not built — #887); invalidates the token
                         #   server-side (a local clear leaves it valid — R2)
```

Machine-credential revoke ("kill a compromised node *now*", §9) is a **separate**
EdgeDevice action, distinct from `DELETE`: invalidate the password hash + force
re-enroll, without tearing down the cluster install.

### C.7 Data model — backend `[NEW]`

```python
# EdgeDevice gains one field (the kube-system namespace UID — a UUID string):
cluster_id = models.CharField(max_length=64, null=True, blank=True, db_index=True)

class Meta:
    constraints = [
        # a physical cluster maps to exactly ONE client — GLOBAL unique:
        models.UniqueConstraint(fields=["cluster_id"],
            condition=Q(cluster_id__isnull=False), name="uniq_edgedevice_cluster"),
        # backend#863, per-account namespace (cosmetic cross-cluster dedup):
        models.UniqueConstraint(fields=["account","namespace"],
            name="uniq_account_namespace"),
    ]
# Account-scoping for adoption lives in the POST logic (C.3), not the constraint:
# global-unique cluster_id + a 409 when the requester's account differs (R6).
```

**Migration order (R4 / R7) — do not reorder:**
1. Add `cluster_id` (nullable, indexed) — no constraint yet.
2. Ship the §7.2 step-2a live-release guard + the CLI/installer adopt-backfill (the
   CLI PATCHes `cluster_id` on the next run — C.3) so existing boxes populate it and
   never double-mint in the meantime.
3. Add the **namespace** `UniqueConstraint` *only after* the Appendix A collision
   check is clean (R4).
4. Add the **cluster_id** `UniqueConstraint` once backfill coverage is acceptable.

### C.8 CLI config — `~/.tracebloc/config.json` `[NEW]` (R10)

```jsonc
// v2 — env-scoped (migrate v1 by wrapping it under profiles[env]); mode 0600
{ "version": 2, "current_env": "prod",
  "profiles": {
    "dev":  { "email", "token", "expires_at", "active_client_id" },
    "stg":  { … }, "prod": { … } } }
// One active_client_id PER env → fixes the cross-env clobber (R10).
// `login --env X` switches current_env without clearing the other profiles.
```

## 15. Amendment (Rev 8) — CLI client-surface simplification

> **Status: PROPOSED, not yet accepted** — prototyped on [cli#136] (draft).
> This section narrows the command surface in §6.2 / §7.1 / §7.3 and the D3
> decision. Where it conflicts with the body above, this amendment wins **once
> accepted**; until then the body is the design of record.

### 15.1 Motivation

Live testing surfaced that the account-context, multi-client surface (`list`,
`use`, the D3 picker, selected-vs-connected) is confusing on the box that *is*
a cluster: an operator saw ~18 unrelated clients, a stale "active" pointer to a
client running on another machine, and `cluster info` targeting the wrong
namespace. The product reality is stricter than the RFC assumed: **one machine
runs one cluster, which holds one client** (the §3.1 / Q5 invariant). There is
nothing to *select*, so the selection surface is cost without benefit.

### 15.2 Decisions

| # | Was (body) | Amended |
|---|---|---|
| A1 | `client use [<slug>]` selects among clients; D3 arrow-key picker (§6.2/§7.1). | **Removed.** One client per machine — nothing to select. Drop `use` and the picker. |
| A2 | `client list` is the account-wide fleet view (§8.4). | **Hidden** (not user-facing). Kept callable only for the installer's one-client-per-machine pre-flight (client#303, `provision.sh:_account_owns_namespace`). |
| A3 | `create` "operates against an already-reachable cluster… never creates a cluster" (§6.2); installer bootstraps first (§6.4). | **Reaffirmed, explicitly.** The one-line installer is the create front door: it bootstraps the cluster (k3s/k3d + GPU + proxy — its existing `scripts/lib/*`) and then calls `client create` to provision. The Go CLI does **not** reimplement cluster bootstrap (a `create → installer` call would recurse; reimplementing the installer in Go is a large, duplicative risk). |
| A4 | Clean uninstall deferred to a `--uninstall` flag (R12, §6.2). | **`client delete` owns the inverse teardown**: deprovision the backend client (`DELETE /edge-device/<id>/`, C.3), `helm uninstall`, delete the local cluster, clear the local pointer. Confirms; `--yes` skips; `403 → ask-an-admin`. This is R12, realized as `delete`. |

### 15.3 What stays

- **Auth** (§6.1/§6.3), **provisioning + cluster_id anchor** (§6.6/§7.2/§7.9),
  **the installer/CLI credential contract** (§6.4) — all unchanged. `create`
  is still get-or-create keyed on the cluster.
- **§7.3 binding for the *data* + `cluster info` commands** — they still default
  their namespace to the **active client** (now set only by `create`, since
  `use` is gone) and give the "runs on another machine" guidance when the
  reachable cluster doesn't host it.
- The **1:1 client↔cluster invariant** and **re-parenting deferred** (Q5) are
  unchanged; a rebuilt cluster still mints a new client (R3).

### 15.4 Consequences / open items

- The §7.3 "selected-vs-connected" picker + connection-state-on-`use` work is
  **superseded** (the closed [cli#133]); only its `cluster info` binding + the
  `explain`-on-`ErrNoParentRelease` cleanup were folded into [cli#136].
- `client delete`'s teardown is **k3d/Linux + shell-out** in the prototype, and
  is unit-tested only — it needs a real run against a throwaway cluster before
  release.
- **Tickets to open on acceptance:** (a) the CLI surface change (this amendment)
  under the epic; (b) e2e-verify `client delete` teardown; (c) confirm the
  installer's client#303 pre-flight still works against a hidden `list`.

[cli#133]: https://github.com/tracebloc/cli/pull/133
[cli#136]: https://github.com/tracebloc/cli/pull/136

[backend#830]: https://github.com/tracebloc/backend/issues/830
[backend#835]: https://github.com/tracebloc/backend/issues/835
[backend#836]: https://github.com/tracebloc/backend/issues/836
[backend#862]: https://github.com/tracebloc/backend/pull/862
[backend#863]: https://github.com/tracebloc/backend/issues/863
[backend#883]: https://github.com/tracebloc/backend/issues/883
