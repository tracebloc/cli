# Contributing

## Local development

The `Makefile` mirrors the CI pipeline — `make ci` runs the exact same checks that PR #N's GitHub Actions run. If `make ci` passes locally, CI will too (modulo non-deterministic flakes). **Run `make ci` before pushing.** Skipping it has cost us at least one PR's worth of fix-up commits per bug class so far.

```bash
make ci          # vet + test + lint + fmt-check + schema-check  (run this before push)
make build       # produces ./tracebloc
make fmt         # fixes gofmt -s drift in place
make schema-sync # pulls latest ingest.v1.json from data-ingestors master
```

Individual targets are also runnable in isolation — `make test`, `make lint`, etc. See the `Makefile` for the full list.

Requires [`golangci-lint`](https://golangci-lint.run/usage/install/) (install via `brew install golangci-lint` or your platform's equivalent).

Cobra autocomplete for `bash` / `zsh` / `fish` / `powershell` is available via the `completion` subcommand. Useful while developing too:

```bash
source <(./tracebloc completion bash)
```

## Repo layout

```
cli/
├── cmd/
│   └── tracebloc/         # thin main() — owns -ldflags-injected version
│       └── main.go
├── internal/
│   └── cli/               # cobra command tree, testable in isolation
│       ├── root.go
│       ├── version.go
│       ├── *_test.go
│       └── …              # one file per subcommand as we add them
├── .github/
│   └── workflows/         # CI for build/test/lint + release
├── go.mod
├── .golangci.yml
├── LICENSE
└── README.md
```

The split between `cmd/tracebloc/main.go` and `internal/cli/` is deliberate: anything that's testable lives in the package, so tests can drive commands without going through `os.Args` / `os.Exit`. The thin `main()` owns process entry + build-metadata injection.

## Pull request conventions

This repo follows the same conventions as the rest of the tracebloc org:

- **Commit messages** use [conventional-commits](https://www.conventionalcommits.org/) prefixes with a `(#N)` scope referencing the issue/ticket number:

  ```
  feat(#149): embed ingest.v1.json schema and add validate command
  fix(#150): handle empty kubeconfig gracefully
  ```

- **PR body** should include a `Closes #N` line (on its own line) for any ticket the PR fully resolves. GitHub auto-closes the issue on merge. The `feat(#N):` convention in the title is for kanban tracking; `Closes #N` in the body is what triggers auto-close.

- **One PR per ticket** when practical. Roll-up sync PRs (`Sync develop → main for vX.Y.Z release`) are an exception.

## Branch flow

- `main` reflects what's currently released.
- `develop` is the active integration branch.
- Feature branches off `develop`. Squash-merge to `develop` is the norm. Sync `develop → main` via a merge commit (preserves the full history of every commit).

## Issue tracking

All work goes through the [tracebloc engineering kanban](https://github.com/orgs/tracebloc/projects/2/views/1). New tickets get added automatically via the `add-to-kanban.yml` workflow.

## Tests

Every new subcommand needs at least:

- A happy-path test in `internal/cli/<cmd>_test.go`
- A test for the `--help` and any flag-validation behavior
- Snapshot-style tests for output formats where the format is part of the contract (e.g. `--output-json`)

See `internal/cli/version_test.go` for the pattern.

### Test seams

Packages stub their I/O boundaries through package-level function variables (`watchJobFn`, `newAPIClient`, `helm.Runner`, …). To swap one in a test, use the shared helper instead of a hand-rolled save/stub/restore block:

```go
import "github.com/tracebloc/cli/internal/testutil"

testutil.SwapSeam(t, &watchJobFn, func(...) (*WatchResult, error) {
    return wr, nil
})
```

`SwapSeam` sets the stub and restores the original via `t.Cleanup` (LIFO, so nested swaps unwind correctly). It's generic — non-function seams like timeouts work too. `internal/testutil` is test-support only: production code must never import it.

## Mutation testing

Coverage says a line *ran*; mutation testing says a test would *fail* if the line's logic flipped. We use [gremlins](https://github.com/go-gremlins/gremlins) as an **advisory, on-demand** check — it never gates a merge.

**The ritual:**

1. Trigger the [`Mutation (gremlins)` workflow](../../actions/workflows/mutation.yml) via *Run workflow*, giving it **one package per run** (e.g. `internal/push`). Whole-module runs take hours and produce an untriageable wall of survivors. Locally: `go install github.com/go-gremlins/gremlins/cmd/gremlins@v0.5.0 && gremlins unleash ./internal/push --timeout-coefficient 3`.
2. Read the run's summary: every `LIVED` mutant is a logic flip no test catches. (`TIMED OUT` usually means the timeout coefficient is too tight — re-run with a higher `timeout_coefficient` input before treating it as signal.)
3. Triage each survivor. Not every survivor matters: mutants in log strings, cosmetic branches, or defensive checks that are structurally unreachable are noise — note and skip them. Survivors in validation boundaries, error mapping, or anything a customer's data flows through are real gaps.
4. File one issue per real gap, titled `test(<pkg>): pin <behavior> (mutation survivor)`, quoting the gremlins line (mutant type + file:line) and what behavior the missing test must pin. That issue then flows through the kanban like any other test ticket — #262, #263, #264 are the pattern.

Survivors are *findings to triage*, not build failures — the workflow stays green even when mutants live, on purpose.
