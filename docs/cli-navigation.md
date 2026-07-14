# tracebloc CLI — navigation map

**The single source of truth for how a user moves through the CLI.** Every command, decision point, and place a user can end up. Diagrams are [Mermaid](https://mermaid.js.org) — they render natively on GitHub and in the docs, and this file is version-controlled, so **edit this file + open a PR to change the map** (that PR is where we discuss flow changes).

The map tracks `develop`. Everything drawn solid is shipped there — including `resources show` (#237), `resources set` (#241), and the status-aware home screen (#244).

**How to read it:** `{diamond}` = decision · `[box]` = step · `([rounded])` = where you end up · green = exit 0 · red = non-zero exit · grey dashed = hidden (installer/back-compat).

---

## 1. Top-level map — what's reachable from `tracebloc` / `tb`

```mermaid
flowchart TD
  H0["tracebloc / tb — home screen"]

  H0 --> ACCT["Account (needs sign-in)"]
  H0 --> ENVC["Your secure environment &amp; data (needs a reachable client)"]
  H0 --> OFF(["delete — offboard this machine"])

  ACCT --> login["login"]
  ACCT --> logout["logout"]
  ACCT --> authst["auth status"]
  ACCT --> clis["client status"]
  ACCT -.-> clcreate["client create"]:::hidden
  ACCT -.-> cllist["client list"]:::hidden

  ENVC --> di["data ingest"]
  ENVC --> dl["data list"]
  ENVC --> dd["data delete"]
  ENVC --> dv["data validate (local only, no cluster)"]
  ENVC --> ci["cluster info"]
  ENVC --> doc["doctor (cluster doctor = hidden alias)"]
  ENVC --> res["resources (bare = show)"]
  ENVC --> resset["resources set"]

  classDef hidden fill:#eee,stroke:#999,stroke-dasharray:3 3,color:#666;
```

`tb` is a convenience alias for `tracebloc` (installer-placed symlink; identical behavior). Aliases kept one deprecation cycle: `data`↔`dataset`, `data ingest`↔`push`, `data delete`↔`rm`. Hidden nodes are fully functional but off the everyday surface (installer / back-compat).

---

## 2. The two gate chains — the key structural fact

Commands split into **two independent families** that authenticate differently. Account commands need you signed in (a user token). Data / environment commands are **auth-free w.r.t. your login** — they reach the cluster via kubeconfig RBAC and mint an in-cluster token. Only `client create` and `delete` cross both.

```mermaid
flowchart TD
  subgraph A["Chain A — Account (your user token)"]
    a0["login · logout · auth status · client * · delete"] --> a1{"signed in?"}
    a1 -->|no| aL(["exit 1 → run: login"]):::fail
    a1 -->|yes| a2{"token valid? (WhoAmI)"}
    a2 -->|"401 / 403"| aL
    a2 -->|"426"| aU(["exit 1 → upgrade the CLI"]):::fail
    a2 -->|ok| aOK(["proceed"]):::ok
  end

  subgraph B["Chain B — Data &amp; environment (kubeconfig + in-cluster token; NO sign-in)"]
    b0["data ingest / list / delete · cluster info · resources"] --> b1{"kubeconfig reachable?"}
    b1 -->|no| bK(["exit 3 → fix kubeconfig · doctor"]):::fail
    b1 -->|yes| b2{"client / release found?"}
    b2 -->|no| bR(["exit 4 → run the installer, or --namespace"]):::fail
    b2 -->|yes| b3{"shared storage bound? (ingest / delete)"}
    b3 -->|no| bR
    b3 -->|yes| b4{"in-cluster token mintable?"}
    b4 -->|no| bT(["exit 5 → grant RBAC"]):::fail
    b4 -->|yes| bOK(["proceed"]):::ok
  end

  classDef fail fill:#fdecec,stroke:#d9534f;
  classDef ok fill:#eafaea,stroke:#5cb85c;
```

---

## 3. `data ingest` — the core flow

The wizard fills only what flags left empty; off a TTY (or `--no-input` / `--output-json`) every gap becomes a hard error instead of a prompt.

```mermaid
flowchart TD
  di["data ingest &lt;path&gt;"] --> wiz{"interactive TTY<br/>and not --no-input?"}
  wiz -->|yes| W["guided wizard — fills only what's missing:<br/>train/test → name → path → task → label → extras → review → confirm"]
  wiz -->|no| FLAGS["flag-only path"]
  W -->|"cancel / Ctrl-C"| cwiz(["exit 0 — nothing ingested"]):::ok
  W --> LOCAL
  FLAGS --> LOCAL

  LOCAL{"local checks — path exists · name valid · task supported ·<br/>flags relevant · layout walk · schema · content preflight"}
  LOCAL -->|"bad input"| e2(["exit 2"]):::fail
  LOCAL -->|"unreadable / parse"| e3(["exit 3"]):::fail
  LOCAL -->|ok| CONN["Connecting to your workspace… (Chain B: kubeconfig→3, no client/storage→4)"]

  CONN --> DRY{"--dry-run?"}
  DRY -->|yes| e0dry(["exit 0 — checks out, nothing created"]):::ok
  DRY -->|no| DEST{"destination already exists?"}
  DEST -->|"new"| STAGE
  DEST -->|"exists · interactive"| REPL{"replace it?"}
  REPL -->|no| e0a(["exit 0 — left as-is"]):::ok
  REPL -->|yes| TD
  DEST -->|"exists · non-interactive · no --overwrite"| e6(["exit 6 — already exists"]):::fail
  DEST -->|"--overwrite"| TD["teardown existing (fail → exit 7)"]
  TD --> STAGE

  STAGE["Step 2/3 — copy into your workspace (stage pod; fail → exit 7)"] --> SUB["Step 3/3 — submit → watch → summary"]
  SUB --> OUT{"outcome"}
  OUT -->|"token 401/403"| e5(["exit 5"]):::fail
  OUT -->|"submit rejected 4xx/5xx"| e8(["exit 8"]):::fail
  OUT -->|"Failed / Unknown / watch lost / completed-with-failures"| e9(["exit 9"]):::fail
  OUT -->|"--detach / Ctrl-C mid-watch"| e0d(["exit 0 — detached (kubectl logs -f)"]):::ok
  OUT -->|"Succeeded, clean"| e0s(["exit 0 — done"]):::ok

  classDef fail fill:#fdecec,stroke:#d9534f;
  classDef ok fill:#eafaea,stroke:#5cb85c;
```

> Note: exit codes are **not** monotonic in execution order — staging (exit 7) runs *before* the token mint (exit 5). The diagram shows the true order.

---

## 4. `resources` — show & set  *(both shipped on `develop`: `show` #237, `set` #241)*

```mermaid
flowchart TD
  R0["resources (bare = show)"] --> Rg{"Chain B gates<br/>kubeconfig→3 · no client→4"}
  Rg -->|ok| RS(["show machine capacity + per-run ceiling — exit 0"]):::ok

  RSET["resources set [max]"] --> RVsh{"request valid?<br/>(not max+flags; not empty)"}
  RVsh -->|no| re2a(["exit 2"]):::fail
  RVsh -->|ok| Rmode{"max · flags · or wizard?"}
  Rmode -->|"wizard (TTY)"| RW["current vs machine →<br/>'Use as much as possible' (default) / choose / leave"]
  Rmode -->|flags| RF["override only the passed dimensions"]
  Rmode -->|max| RM["whole machine − overhead"]
  RW --> FIT
  RF --> FIT
  RM --> FIT

  FIT{"fits the machine? (+ floors)"}
  FIT -->|no| re2b(["exit 2 — too big / too small<br/>(macOS: raise Docker Desktop)"]):::fail
  FIT -->|"no change"| re0n(["exit 0 — nothing to change"]):::ok
  FIT -->|ok| CONF{"confirm? (--yes skips)"}
  CONF -->|"declined / non-TTY, no --yes"| re0c(["exit 0 declined / exit 1 non-TTY"]):::fail
  CONF -->|yes| PIN{"chart version pinned?"}
  PIN -->|no| re1(["exit 1 — refuse unpinned upgrade"]):::fail
  PIN -->|yes| APPLY(["helm upgrade — applies to your next run — exit 0"]):::ok

  classDef fail fill:#fdecec,stroke:#d9534f;
  classDef ok fill:#eafaea,stroke:#5cb85c;
```

---

## Exit codes

| code | meaning |
|---|---|
| 0 | success (incl. dry-run, detached, "nothing to change", declined-safely) |
| 1 | generic / account-auth (not signed in, token rejected, upgrade required) |
| 2 | bad input / schema violation / doesn't fit |
| 3 | kubeconfig unreachable, or a local file/parse error |
| 4 | cluster reached but no tracebloc client / storage |
| 5 | in-cluster token could not be minted (RBAC) |
| 6 | destination dataset already exists |
| 7 | staging / teardown failed |
| 8 | jobs-manager rejected the submit |
| 9 | ingestion Job failed / partial-failure / watch error |
| 130 | Ctrl-C |

## Cross-links — where a dead-end points

- **not signed in / token 401·403** → `login`
- **426 upgrade-required** → upgrade the CLI
- **kubeconfig (exit 3)** → fix `--kubeconfig`/`--context`, then `doctor`
- **no client / environment (exit 4)** → run the installer (or `--namespace`); triage with `doctor`
- **no token (exit 5)** → grant RBAC; diagnose with `cluster info` / `doctor`
- **destination exists (exit 6)** → `--overwrite`, a different `--name`, or `data delete` first
- **staging partial (exit 7)** → `data delete` then re-ingest
- **ingest failed (exit 9)** → the panel / `kubectl get job` / `kubectl logs -f`
- **no active client** (client status / delete) → `client create` / re-run installer

## Known gaps / decisions (raise in review)

1. **`delete` (offboard) exits 0 even on a *partial/degraded* teardown** — it warns but never returns non-zero, so a script can't detect an incomplete offboard. A dedicated non-zero "partial offboard" code would close this.
2. **`cluster info`'s home is open** — the `doctor` promotion shipped (top-level `doctor`, with `cluster doctor` kept as a hidden alias); whether `cluster info` stays under `cluster` or is also promoted is undecided.
3. Terminology in the live copy (client / cluster / `<table>`) is pre-cleanup; the map uses the agreed target words (secure environment, etc.). The rename wave aligns the code later.

Resolved since the first cut of this map: the status-aware home screen shipped ([#244](https://github.com/tracebloc/cli/pull/244) — greeting + sign-in + environment state on bare `tracebloc`/`tb`), and `resources set` shipped ([#241](https://github.com/tracebloc/cli/pull/241)).
