# RFC 0001 — Browser-based auth & one-command client provisioning

> **Status: DRAFT** — circulated for discussion; not yet approved. Everything here
> is open to change. Owner: @saadqbal. Last updated: 2026-06-05.

## 1. Summary

Replace the current "go to the web UI, hand-create a client, copy a Client ID +
password, paste them into the installer" onboarding with a single flow:

```
sign in (browser) → name this machine + confirm its location → done
```

The human authenticates once in a browser (works even on a headless box over
SSH), the CLI provisions the client automatically, and the installer proceeds.
No copied secrets, no separate visit to `/clients`.

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
  human's proof and the machine's permanent credential (see §4).

Goal: make first-time setup *stupid simple* for a beginner, while staying secure
and working on the headless/remote/proxied boxes where tracebloc actually runs.

## 3. The core reframe — two identities, not one

There are **two** things being authenticated, with opposite lifetimes:

| | **Human** (account) | **Client** (machine/daemon) |
|---|---|---|
| Who | A person | An EC2 box / on-prem server |
| Lifetime | A login session | Runs 24/7 for months |
| Auth | Browser SSO/MFA | A long-lived machine credential |
| Created at | `ai.tracebloc.io` | Provisioned by backend |

Today both collapse into one **Client ID + password**. The fix is **not** "browser
auth instead of credentials" — it's: *authenticate the human in the browser, and
let that authorization mint the machine credential automatically.* (This is the
`tailscale up` / `cloudflared tunnel login` / `aws sso login` model: a browser
authorizes a long-running daemon, the control plane issues the node its own key.)

## 4. What already exists (grounded findings, 2026-06-05)

A survey of `backend`, `client-runtime`, and `cli` shows **most of the data model
is already there** — the only real gap is the browser handshake.

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
(`create()`); `namespace` is **not** set here — it's reported later by the client
heartbeat (`EdgeDeviceHeartbeatView`), so it's chosen at install time. A CLI
holding a user token can call this endpoint to auto-provision a client.

### 4.3 Auth today

DRF Token auth: `POST /api-token-auth/` (login → token), `POST /register/`
(signup → token). **Google + GitHub OAuth already wired for web.**
**No device-authorization grant (RFC 8628). No personal-access-token concept.**
← *This is the only net-new backend surface.*

### 4.4 There is already a Go CLI

`tracebloc/cli` — Go + Cobra, v0.2.0, cosign-signed, multi-arch, actively
maintained, and **already installed by the client bash installer**
(`client/scripts/lib/install-cli.sh`, currently Step 5 / post-cluster). It does
dataset/ingest work via kube ServiceAccount tokens; it has **no login or
provisioning** today. This is the correct home for the new flow.

### 4.5 Energy/carbon telemetry

`client-runtime/Node-deploy/resource_monitor.py` sends CPU/GPU TDP + utilization
to `/edge-device-heartbeat/`. Carbon is computed backend-side from
`EdgeDevice.carbon_intensity` (location-driven). The heartbeat does **not**
auto-detect or report location — confirming location must be captured at
provisioning time, which is exactly what this RFC does.

## 5. Goals / Non-goals

**Goals**
- One-command setup for a beginner; ≤2 prompts in the common case.
- Browser-based human auth that works on headless/SSH/proxied boxes.
- Auto-provision the client (no manual `/clients` visit, no copied secrets).
- Capture a human-readable name + structured location at provisioning.
- Keep a non-interactive path for automation and air-gapped installs.
- Don't break existing Client ID + password installs (dual-mode).

**Non-goals (this RFC)**
- Building account signup *in the terminal* (browser activation page handles
  login **and** signup — keep ToS/GDPR/MFA/CAPTCHA where they already live).
- Short-lived/rotating client credentials + revocation (desirable; deferred to a
  later phase — see §10).
- Fleet/enrollment-key management UI (phase 2).

## 6. Proposed design

### 6.1 Auth mechanism — OAuth 2.0 Device Authorization Grant (RFC 8628)

Chosen because installs are **headless** (remote servers over SSH). The
browser-and-CLI need not share a machine, network, or continent.

Rejected alternatives:
- **Type credentials into the installer** (today): blocks SSO/MFA; exposes
  password to the script; phishing-prone.
- **Localhost-callback / PKCE** (`gcloud`, `vercel`): requires a browser on the
  *same* machine — breaks over SSH.
- **Paste a token**: kept as the *fallback* (§6.5), not the default.

### 6.2 New CLI commands (in `tracebloc/cli`)

```
tracebloc login                 # device flow → store user token in ~/.tracebloc
tracebloc logout
tracebloc auth status
tracebloc client create         # POST /edge-device/  (--name, --location)
tracebloc client list
tracebloc client use <id>       # select an existing client for this machine
```

`login` stores a short-lived **user** token (config `~/.tracebloc/`, `0600`).
`client create` mints the **machine** credential and hands it to the installer.

### 6.3 Backend additions (in `tracebloc/backend`)

- `POST /device/code` → `{ device_code, user_code, verification_uri,
  verification_uri_complete, expires_in, interval }`.
- `POST /device/token` → polled by the CLI; returns a user token once approved
  (`authorization_pending` / `slow_down` / `expired_token` per RFC 8628).
- A web **activation page** `https://ai.tracebloc.io/activate` that reuses
  existing web login/signup (incl. Google/GitHub) and shows **what is being
  authorized** ("Connect machine *X* to account *Y*?") as the phishing mitigation.

### 6.4 Installer reorder (in `tracebloc/client`)

Move CLI install + `tracebloc login` + `tracebloc client create` to run **before**
the Helm install, because the minted credential feeds the chart. (Today the CLI
installs *after* the cluster.) Keep CLI-install failure non-fatal only for the
*dataset* convenience path, not for the auth path.

### 6.5 Fallbacks — automation & air-gap (must ship together with the above)

- `TRACEBLOC_ENROLL_TOKEN` / `--token`: a pre-issued credential for
  Ansible/Terraform/CI/golden-images and for egress-restricted on-prem boxes that
  can't reach the device endpoints.
- Existing **Client ID + password** path stays working (dual-mode) for one full
  deprecation cycle.
- The device-flow HTTP client must honor `HTTPS_PROXY`/`NO_PROXY` + custom CAs —
  reuse the corporate-proxy hardening already shipped in the installer (#172).

### 6.6 Name → namespace: derive once, then freeze

Today there are effectively two names: `first_name` (display) and `namespace`
(k8s, chosen separately at install). Asking for both is redundant and confusing.

**Proposal: ask for one human-readable name, store it as `first_name`, and *derive*
the `namespace` slug from it — once, at creation. After that the two are
decoupled: the display name stays mutable; the namespace is frozen forever.**

Why decouple rather than keep them coupled:

- **Kubernetes namespaces are immutable.** You cannot rename one, and the name is
  baked into resource names (`<ns>-jobs-manager`, `<ns>-requests-proxy`), DNS, and
  PVCs. If name and slug stayed coupled, the first display-name rename would force
  either a stale/disagreeing slug or a destroy-and-rebuild of the running client.
  Deriving once and freezing avoids this — and matches the model, which already
  separates `first_name` (mutable) from `namespace`.

Derivation rules:

- **Slugify:** lowercase, transliterate unicode, spaces/punctuation → `-`, collapse
  repeats, strip to DNS-1123 (`[a-z0-9-]`, ≤63 chars, no leading/trailing `-`).
- **Collision-suffix:** append `-2`, `-3`, … when the slug already exists (two
  clients may share a display name; namespaces must be unique).
- **Empty-slug guard:** a name that slugifies to empty (e.g. all-CJK) falls back to
  `client-<short-id>`.
- **Hide from the junior, expose to the power user:** show the derived slug as a
  confirmation line (`slug: munich-hospital-radiology ✔`); offer `--namespace` to
  override for multi-client hosts / naming conventions.
- **Backfill:** existing clients keep their current `namespace`; only set
  `first_name` as the display backfill — never re-derive an existing slug.

> **Sequencing caveat (open question §11.2):** `namespace` is currently reported by
> the client *heartbeat*, not set at `POST /edge-device/`. The installer must use
> the CLI-derived slug as `TB_NAMESPACE` so the provisioned slug and the
> install-time namespace can't disagree. A reference slug implementation + a run
> against existing production namespaces (collision/empty-slug check) accompanies
> this RFC.

### 6.7 Location: soft-required (required, but pre-filled)

`location` is **optional at the model layer today** (`CharField(..., blank=True)`,
serializer doesn't force it), so a client can be created with no location and
`carbon_intensity` defaults to `0` — i.e. it silently reads as "carbon-free". That
quietly corrupts the exact metric tracebloc sells.

**Proposal: treat location as *soft-required* in the new flow — the user must make
an explicit choice, but it's pre-filled so it costs nothing in the common case.**

- Prompt: *"Where does this machine physically run? (used to calculate carbon
  footprint)"*.
- **Auto-detect a default**, then **require confirmation** (never assume silently):
  - **Cloud instance metadata first** (AWS/GCP/Azure region → zone; e.g. EC2
    `eu-central-1` → `DE`). High confidence → usually one keystroke (Enter).
  - **GeoIP fallback** — flagged *low confidence*, because on-prem boxes egress
    through corporate proxies often in another country (the #172 segment).
- Input is a pick from `ZONE_CHOICES` (structured), not free text.
- **Never accept a silent empty.** The only skip is an explicit, labeled
  *"Set later — carbon reporting unavailable until you do"* choice that visibly
  marks the client location-unset (no fake zero) and nudges in the dashboard.
- **Keep the DB `blank=True`** for backward compatibility (existing location-less
  clients keep working); enforce "soft-required" at the provisioning UX layer, not
  with a DB constraint.
- Mutable post-install (`--location` / dashboard); changing it affects **future**
  readings only (historical gCO₂ not re-based) — TBD, see §11.

## 7. UX — drafted flows

### 7.1 First-time, headless box

```
$ bash <(curl -fsSL https://tracebloc.io/i.sh)
✔ Checking this machine… ready (8 CPU · 30 GiB RAM · 46 GiB free · network OK)

  To connect this machine, sign in to tracebloc:
     →  https://ai.tracebloc.io/activate
        code:  WDJB-MJHT
  Waiting for you to finish in your browser…  (Ctrl-C to cancel)

# (user opens URL on laptop → logs in / signs up → approves "WDJB-MJHT")

✔ Signed in as asad@acme.com
  Name this client (shown on your dashboard & carbon reports):
     →  Munich Hospital — Radiology         slug: munich-hospital-radiology ✔
  Where does it physically run? (for carbon footprint)
     detected 🇩🇪 Germany — eu-central-1 (Frankfurt)   →  [Enter to accept]

✔ Provisioning client “Munich Hospital — Radiology” (DE)…
✔ Installing (first run pulls images — a few minutes)……
✔ Connected — this machine is 🟢 Online   https://ai.tracebloc.io/clients
```

### 7.2 Returning / re-run (already enrolled)

Detect a valid client credential on the box → **skip auth and prompts entirely** →
reconcile / upgrade. Idempotent re-runs are non-negotiable.

### 7.3 Automation / air-gap

```
TRACEBLOC_ENROLL_TOKEN=… TRACEBLOC_CLIENT_NAME="Lab A" TRACEBLOC_LOCATION=DE \
  bash <(curl -fsSL https://tracebloc.io/i.sh)   # zero prompts
```

## 8. Security considerations

- Password leaves the installer process space entirely — the CLI only ever holds
  a device code, then a scoped user token, then a per-client machine credential.
- Device-code phishing: short `user_code` TTL, bind the code to the account, and
  show *what is being authorized* on the approval page.
- Secret-at-rest: write client credentials `0600`. (Observed `drwxrwxrwx` data
  dirs and a world-ish `values.yaml` on an existing box — tighten when we start
  auto-writing credentials.)
- Tokens: store user token `0600` in `~/.tracebloc`; `logout` revokes/clears.

## 9. Backwards compatibility & migration

- Dual-mode: Client ID + password and `--token` paths keep working.
- Backfill `first_name` for existing clients; do not touch `namespace`.
- Deprecate the manual `/clients` "create" path only after device flow is GA;
  keep `/clients` as **manage/revoke**.

## 10. Phased rollout

- **Phase 0** (no backend work): stop sending users to "create a client first";
  add `--token` / `TRACEBLOC_ENROLL_TOKEN` so the secret isn't typed inline.
- **Phase 1** (the unlock): device-flow endpoints + activation page; `tracebloc
  login` + `client create`; installer reorder; location auto-detect. Dual-mode.
- **Phase 2** (hardening): short-lived auto-refreshing client tokens, revocation,
  enrollment keys for fleets, `auth login/logout/status` polish.

## 11. Open questions

1. **Air-gapped / no-egress on-prem** — real segment? If yes, the
   token/enrollment-key fallback is first-class, not optional. *(Blocking for §6.5
   priority.)*
2. **Namespace derivation** — confirmed today it's reported via heartbeat, not set
   at `/edge-device/`. If we derive slug from name, how do we reconcile with the
   installer-chosen `TB_NAMESPACE`? (Lean: name → slug → `TB_NAMESPACE`.)
3. **Location change semantics** — future-only vs re-baseline historical gCO₂?
4. **RBAC** — a user without `CanManageClient`: flow must offer "pick existing /
   ask an admin" instead of failing.
5. **Multi-client per host** and **re-parenting** to another account — support or
   explicitly block in phase 1?
6. **Where the device-flow identity providers live** — reuse Google/GitHub OAuth
   on the activation page (preferred) vs. password-only.

## 12. Work breakdown (for tickets, once this firms up)

- `backend`: device-code + device-token endpoints; activation page; (later)
  client token issuance/refresh/revoke.
- `cli`: `login`/`logout`/`auth status`; `client create/list/use`; location
  auto-detect (cloud metadata + GeoIP); config store (`~/.tracebloc`, `0600`);
  proxy/CA-aware HTTP client.
- `client` (installer): reorder CLI install + auth before Helm; dual-mode env
  fallbacks; name/location prompts; idempotent re-run detection.

## Appendix B — name→slug reference rule & validation

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
ASCII survives transliteration) — semantically lossy. Acceptable for a hidden slug
(display name is preserved), but worth surfacing the derived slug for confirmation.

**Validate against full production data before locking the rule** — run in the
backend and check for collisions or empty-slug fallbacks against real names:

```python
# manage.py shell
from metaApi.models import EdgeDevice
rows = EdgeDevice.objects.values_list("first_name", "namespace")
# Re-derive slug from first_name, compare to stored namespace; report:
#  - names whose derived slug != current namespace (migration mismatch)
#  - derived-slug collisions within an account
#  - names that hit the empty-slug guard
```

## Appendix — closest prior art

Tailscale (daemon enrollment via browser → node key — nearly our exact shape),
GitHub CLI (device-flow ergonomics), AWS SSO (headless device flow), cloudflared
(browser-authorized long-running tunnel).
