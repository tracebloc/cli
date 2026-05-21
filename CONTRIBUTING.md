# Contributing

## Local development

```bash
go build -o tracebloc ./cmd/tracebloc
./tracebloc version

go test ./...
golangci-lint run    # https://golangci-lint.run/usage/install/
```

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
