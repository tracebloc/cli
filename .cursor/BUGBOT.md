# Bugbot guide — tracebloc/cli

## Context

Public Go CLI (Apache-2.0), shipped as **signed 8-platform releases** (cosign keyless,
verified by `scripts/install.sh`). Customers run it on their own machines against their own
Kubernetes to operate a self-hosted secure environment. It talks to a public HTTPS backend
(`internal/api`) and to an in-cluster jobs-manager (`internal/submit`), and shells out to
`kubectl`/`helm`/`docker`.

Two things make this repo unusual and should shape every finding:

1. **Its exit codes are a scripting contract** — customers branch on them
   (`internal/cli/exitcodes.go`: "the numeric values are FROZEN").
2. **`make ci` mirrors CI exactly.** The `Makefile` header states it outright: "divergence
   between local and CI is the bug this file exists to prevent." Tool versions are pinned in
   lockstep with `.github/workflows/build.yml`.

## Always flag

- **A command that reports success it hasn't earned.** Exiting 0 is not the same as
  succeeding. The reference pattern is `classifyPushOutcome`
  (`internal/cli/data_ingest_output.go:25`): a Job that exits 0 but whose summary reports row
  failures returns `"completed_with_failures"` + `exitIngestFailed`, *not* `"succeeded"` — its
  comment cites this class explicitly. `internal/doctor` carries the same idea in
  `StatusUnknown`: a check that ran but cannot back a green prints a neutral line rather than a
  false ✔. Flag any new multi-step command where a partial failure collapses into success, and
  any path where the `--output-json` status and the process exit code can disagree.

- **An exit code that isn't a named constant from `internal/cli/exitcodes.go`**, a repurposed
  numeric value, or a new failure path returning generic `1` when a specific code already
  exists. Every non-test `&exitError{}` names its code.

- **A prompt whose cancellation produces no visible feedback.** `errInteractiveCancelled`
  (`internal/cli/interactive.go:26`) must print a `Cancelled — …` line via the Printer and then
  return cleanly — see `resources_set.go:219`, `data_delete.go:233`,
  `data_ingest_local.go:103`, `data_ingest_cluster.go:339`. Watch for
  **`mapClientErr` (`internal/cli/client.go:858`), which maps it straight to `nil` with no
  output at all** — a Ctrl-C routed through it exits 0 in total silence, right next to a
  declined-answer branch that *does* print (`client.go:334`, `delete.go:196`). Check this at
  every new or changed prompt site. Signals are wired centrally via `signal.NotifyContext`
  (`cmd/tracebloc/main.go:58`) so deferred cleanup runs — a bare handler skips it and breaks
  `push.Stage`'s cleanup contract. Interrupted-but-clean paths exit 130.

- **HTTP 426 treated as anything other than a hard stop.** It is detected centrally
  (`internal/api/client.go`, `parseUpgradeRequired` → `*UpgradeRequiredError`) so every caller
  degrades to the same actionable "run `tracebloc upgrade`" message — see `auth.go:336`,
  `doctor.go:119`, `client_status.go:129`, `delete.go:151,218`, `client.go:253`. Flag a new API
  consumer that retries through it, frames it as a transient outage, or folds it into a generic
  error. A too-old CLI never recovers by waiting, so `--wait` loops must fail fast on it.

- **Verification that degrades to a warning.** In `scripts/install.sh` the SHA256 compare
  aborts when no hashing tool is present, and `verify_cosign_signature()` bootstraps a pinned,
  checksum-verified cosign (`COSIGN_VERSION=v2.4.1`) rather than skipping; the only bypass is an
  explicit `TRACEBLOC_ALLOW_UNVERIFIED=1` with a loud warning. A previous "warn + continue +
  still print ✓ matches" branch was caught as *both* a security regression and a dishonest log.
  Also flag any `--version` / `RELEASE_VERSION` use that skips `validate_version_tag` before URL
  interpolation. `tracebloc upgrade` and host prep must keep delegating to this verified script
  instead of reimplementing verification in Go.

- **An external call with no ceiling.** Backend HTTP: `defaultTimeout = 30 * time.Second`
  (`internal/api/client.go:31`). In-cluster submit: `SubmitTimeout`
  (`internal/submit/client.go:21`). Doctor probes: `httpProbeTimeout = 8s`. Every shell-out uses
  `exec.CommandContext`. Flag a bare `exec.Command` in non-test code, an `http.Client{}` with no
  `Timeout`, or a watch/poll loop with no deadline.

- **A missing empty / nil / zero guard on anything crossing a boundary** (user input, API
  response, cluster state). There is no shared validator — the convention is a colocated
  `validate*` func: `internal/push/spec.go:100` (`ValidateTableName`),
  `internal/cli/interactive.go:537-568`. Two specifics: a bare Enter yields `""` and must not be
  treated as a real path (`validateDatasetPath` documents exactly this); and pagination must
  fail loudly on an unparseable `next` link rather than silently truncating the list
  (`internal/api/client.go`, `nextPath`). Where "empty" and "unknown" are different answers,
  prefer a three-valued return (`internal/cluster/discover.go:302`).

- **"Couldn't confirm" used as "confirmed" — in a BRANCH, not just a return type.** The rule
  above is about the value a function hands back; this is about the `if` that consumes it, and
  it is the defect this repo produces most often. cli#515 shipped it three times in one PR,
  each in a different file, each after the previous one was fixed: a failed cluster scan
  reported as "no client is running here"; an empty `cluster_id` on a legacy client read as
  "runs elsewhere" (`ProvisionedClient.ClusterID` is documented empty on not-yet-backfilled
  records); and `reachStateOf(x) != ReachNoEnv` used to mean "an environment is here", when
  `ReachState` also has `ReachUnreachable` and `ReachError`. Two concrete shapes to flag:
  - **A negated comparison against ONE member of a multi-valued enum.** `!= ReachNoEnv`,
    `!= StatusFail` and friends silently include every member added later. Compare against the
    member you actually require (`== ReachOK`), and derive the test's input domain from the
    enum's declared surface — mutation coverage cannot see a vocabulary gap.
  - **A lenient "not found" default reused where the question is "may I believe this?"**
    `reachStateOf` returns `ReachOK` for an ABSENT check, which is right for a verdict roll-up
    and wrong for authorising a claim — hence the separate `reachConfirmedOK`
    (`internal/cli/doctor.go`). The same default is rarely correct for both.

  The customer-visible cost is never a wrong log line: on #515 each instance ended in advice to
  run `client create` on a cluster nothing was confirmed on, where it MINTS rather than adopts —
  i.e. the guidance manufactured the orphaned phantom of `backend#970`.

- **A cross-repo contract change that only lands on one side.** `scripts/.data-ingestors-ref`,
  `scripts/.client-ref` and `scripts/.backend-ref` pin upstream refs deliberately so an
  unrelated upstream commit can't red every open PR. Flag a hand-edit to a generated artifact
  (`internal/schema/*.json`, `internal/api/testdata/*.json`,
  `internal/push/testdata/parity/goldens.json`, `internal/cli/testdata/golden/*.golden`) that
  doesn't also bump and re-sync its pin, and any change to a chart assumption (discovery labels,
  jobs-manager port, PVC mount path) that doesn't update `scripts/chart-invariants` — a chart
  rename otherwise ships green in both repos and breaks discovery in the field.

- **Output that breaks the style contract** (`STYLE.md`): all colour goes through
  `internal/ui`'s Printer — never inline an escape or brand hex outside `internal/ui`
  (`scripts/check-style.sh` greps for it). Colour is never load-bearing: headings carry bold,
  alerts carry a glyph, so the output still reads under `NO_COLOR`, in a pipe, and for a
  colour-blind reader. User-facing copy follows the terminology table ("secure environment",
  "ingest", "delete", "Online/Offline", "collaborators", "task"); only the workspace → secure
  environment swap is grep-enforced, the rest is review judgement. A new user-facing string
  almost always needs its golden regenerated:
  `TB_UPDATE_GOLDEN=1 go test ./internal/cli/ -run TestCopyCatalog`.

- **Errors that lose their type.** `%w` wrapping is the house convention (~325 sites), with
  typed errors for the cases callers branch on: `APIError`, `UpgradeRequiredError`,
  `SubmitError`, `WatchError`, `exitError`, `noParentReleaseError`. Flag string-matching on an
  error message where `errors.Is`/`errors.As` applies.

## Known non-issues — do not flag

- **`.golangci.yml` does not gate CI.** `golangci-lint` is never invoked in a workflow (its
  `staticcheck`/`unused` are disabled there for runner OOM reasons); the blocking Lint job runs
  pinned standalone binaries — `errcheck`, `gofmt -s`, `goimports`, `ineffassign`, `misspell`,
  `staticcheck`, plus `deadcode-check.sh`, `file-budget.sh`, `check-style.sh`. Don't infer
  coverage from that file.
- **`staticcheck` runs `-checks all,-ST1005` deliberately** — do not flag error-string
  capitalisation or punctuation. It is a tracked, intentional exclusion (cli#279).
- `internal/submit/client.go:78` — `InsecureSkipVerify` is intentional for cluster-internal
  traffic with no recognisable CA, documented in place and marked `//nolint:gosec`. It is the
  only `nolint` in the repo.
- `scripts/deadcode-allowlist.txt` entries are verified false positives (Stringers reached only
  through `fmt` reflection; test-only parity harnesses that must live in production source).
- `test/integration/*` uses 30s–5min timeouts because it drives a real cluster — not the
  production timeout convention.
- `mutation.yml` and `head-drift-canary.yml` are advisory and never gate a merge.
- No `vendor/` directory — the module cache is used on purpose.
- `// style-guard: allow` is a defined escape hatch but is currently used nowhere; if one
  appears, it is a novel exception worth scrutiny rather than an accepted pattern.
- A `code-quality-caller.yml` that passes **no `secrets:` line** is correct, not an omission.
  The shared `code-quality.yml` reusable references no secrets by contract
  (RFC-BACKEND-1405 Q5, backend#1526): secretless callees get no secrets line, and if the
  reusable ever gains one, callers switch to explicit per-secret passing — never
  `secrets: inherit`. Flag the *addition* of `secrets: inherit` on this caller instead.

## Tone

Direct. Name the file and line. Give a concrete fix, not "consider". State the customer-visible
consequence — what they see, and which exit code they get — not just the code smell.

This repo is **public**: never put a customer name, internal hostname, or internal-only ticket
detail in a finding. A bare `tracebloc/backend#NNNN` reference is fine.

## Working with Bugbot findings (team norm)

Every Bugbot review thread gets a reply, then gets resolved:
- **Fixed**: say what changed and in which commit.
- **False positive**: say why, with evidence (file/line, measured behavior).
Unresolved cursor threads HOLD release-train promotions (soft gate) — an
unaddressed finding blocks the fleet, not just this PR.
