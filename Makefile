# Top-level Makefile for tracebloc/cli.
#
# Purpose: keep the local feedback loop the same shape as the CI
# loop. Anything that fails in `make ci` would have failed on a PR,
# and vice versa. Don't add targets here that aren't also enforced
# by .github/workflows/build.yml — divergence between local and CI
# is the bug this file exists to prevent.

# ---- uniform entry points (backend#1606) -------------------------
#
# Every active tracebloc repo exposes the SAME three targets, so
# "run your tests before you push" stops being a rule you can only
# obey with per-repo tribal knowledge:
#
#   make check      lint + fast tests.   Budget: under 60 s.
#   make check-all  everything CI runs (bar the CI-only heavy suites).
#   make setup      install what those targets need, and a git pre-push hook
#                   that runs `make check` (skip once with --no-verify).
#
# These are thin aliases over the targets that already existed here —
# they add no new tool and no new configuration.

.DEFAULT_GOAL := help

.PHONY: help
help:
	@echo "tracebloc/cli — make targets"
	@echo
	@echo "  check       lint + fast tests (under 60 s) — run this before every push"
	@echo "  check-all   everything CI runs; alias for 'ci'"
	@echo "  setup       download Go module dependencies; installs the pre-push hook"
	@echo "  install-hooks  (re)install the git pre-push hook that runs 'make check'"
	@echo "  build       build ./tracebloc"
	@echo "  install     go install ./cmd/tracebloc"
	@echo
	@echo "  individual: vet test lint lint-full fmt fmt-check fmt-selftest"
	@echo "              schema-check"
	@echo "              vulncheck deadcode file-budget check-style clean"
	@echo "              cover cover-integration cover-merge test-integration"

# check: the pre-push tier. Measured 18 s on a warm module cache
# (macOS, 14 cores) versus 56 s for the same set with -race.
#
# What is deliberately NOT here, and why:
#   * -race          — 3x the wall clock for the same assertions; `ci`
#                      still runs it, so the race signal is not lost.
#   * lint/lint-full — `go run tool@version` builds the tool from
#                      source on a cold cache (golangci-lint alone is
#                      1-2 min) and needs the network.
#   * vulncheck      — needs https://vuln.go.dev.
#   * schema-check   — fetches data-ingestors at the pinned ref.
#   * deadcode       — another `go run tool@version` fetch.
.PHONY: check
check: vet test-fast fmt-check fmt-selftest file-budget check-style check-tool-pins
	@echo "==> check: green (run 'make check-all' for the full CI set)"

# check-all: the full PR gate. `ci` is the original name and stays —
# too much muscle memory and too many docs point at it.
.PHONY: check-all
check-all: ci

# setup: warm the module cache so the first `make check` in a fresh
# clone isn't dominated by downloads, then install the pre-push hook
# (backend#1606).
.PHONY: setup
setup:
	$(GO) mod download
	@echo "==> setup: module cache warm; run 'make check'"
	@$(MAKE) --no-print-directory install-hooks

# install-hooks: put a pre-push hook in place that runs `make check`, so the
# canon's "run the tests before you push" is carried by the tooling rather than
# by memory. Factored out of `setup` so it is independently runnable and
# testable, and so a contributor who only wants the hook need not rerun the
# full `make setup`.
#
# Honest by design: the hook catches FORGETTING, not defiance — `git push
# --no-verify` skips it and always will. And it refuses to clobber a pre-push
# hook that is already there and not ours (e.g. one the pre-commit framework
# manages), rather than silently stomping a contributor's setup.
#
# `git rev-parse --git-path hooks` (not a hard-coded `.git/hooks`) so it lands
# in the right place inside a linked worktree or a submodule, where the git dir
# is not `.git`.
.PHONY: install-hooks
install-hooks:
	@if ! git rev-parse --git-dir >/dev/null 2>&1; then \
	  echo "note: not a git checkout — skipping pre-push hook install"; \
	else \
	  hook="$$(git rev-parse --git-path hooks)/pre-push"; \
	  if [ -e "$$hook" ] && ! grep -q 'tracebloc pre-push hook' "$$hook" 2>/dev/null; then \
	    echo "note: $$hook already exists and is not ours — leaving it untouched."; \
	    echo "      add 'make check' to it, or remove it and re-run 'make install-hooks'."; \
	  else \
	    mkdir -p "$$(dirname "$$hook")" && \
	    printf '%s\n' \
	      '#!/bin/sh' \
	      '# tracebloc pre-push hook installed by make setup (backend#1606).' \
	      '# Runs make check so a push that would be red in CI is caught locally first.' \
	      '# It catches forgetting, not defiance: git push --no-verify skips it.' \
	      '#' \
	      '# Git exports GIT_DIR/GIT_WORK_TREE/etc into hook processes; a nested git' \
	      '# (e.g. Go buildvcs under go test) then fails in a linked worktree with' \
	      '# exit status 128. Clear them so make check runs as if from the shell.' \
	      'unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE GIT_PREFIX GIT_COMMON_DIR GIT_OBJECT_DIRECTORY' \
	      'exec make check' > "$$hook" && \
	    chmod +x "$$hook" && \
	    echo "==> pre-push hook installed at $$hook" && \
	    echo "    'make check' now runs before each push (skip once with: git push --no-verify)"; \
	  fi; \
	fi

# ---- toggles -----------------------------------------------------

GO            ?= go
PKGS          := ./...

# Pinned lint/analysis tool versions (reproducibility — no more @latest drift).
# Keep these in lockstep with .github/workflows/build.yml — and
# GOLANGCI_LINT_VERSION with .github/workflows/golangci.yml. Bump deliberately.
GOLANGCI_LINT_VERSION ?= v2.12.2
ERRCHECK_VERSION    ?= v1.20.0
INEFFASSIGN_VERSION ?= v0.2.0
MISSPELL_VERSION    ?= v0.3.4
DEADCODE_VERSION    ?= v0.48.0
GOVULNCHECK_VERSION ?= v1.1.4
STATICCHECK_VERSION ?= 2025.1.1
GOIMPORTS_VERSION   ?= v0.48.0

# ---- top-level targets -------------------------------------------

# ci mirrors the PR gates exactly — including golangci-lint (lint-full),
# which fails on findings since #430. A green `make ci` must imply a green
# PR; lint-full's own guard tells you how to install the tool if missing.
.PHONY: ci
ci: vet test lint lint-full fmt-check fmt-selftest schema-check vulncheck file-budget deadcode check-style check-tool-pins
	@echo "==> ci: all green"

.PHONY: build
build:
	$(GO) build -o tracebloc ./cmd/tracebloc

.PHONY: install
install:
	$(GO) install ./cmd/tracebloc

# ---- individual targets (also runnable in isolation) -------------

.PHONY: vet
vet:
	$(GO) vet $(PKGS)

.PHONY: test
test:
	$(GO) test -race -cover $(PKGS)

# test-fast: the same assertions without the race detector — 18 s vs
# 56 s, which is the difference between `make check` fitting the
# under-a-minute budget and not. `test` (with -race) still runs in
# `ci` / `check-all`, so nothing is dropped from the PR gate.
.PHONY: test-fast
test-fast:
	$(GO) test $(PKGS)

# Integration tests (build-tagged `integration`) run against a REAL
# cluster reachable via the ambient kubeconfig — kind in CI, or your
# own dev cluster locally. They cover the real-I/O seams the unit
# suite mocks (clientset connectivity, the SPDYExecutor tar-over-exec
# stream against a live Pod + PVC). -count=1 disables caching since
# these touch live cluster state. See .github/workflows/e2e.yml.
.PHONY: test-integration
test-integration:
	$(GO) test -tags integration -count=1 -timeout 10m -v ./test/integration/...

# ---- merged coverage (unit + integration) ------------------------
# The real-cluster I/O seams — cluster.NewClientset, push.SPDYExecutor.Exec,
# submit.PortForwardJobsManager, cluster.DiscoverInClusterClient — are 0% in the
# unit suite BY DESIGN; the integration suite (kind, e2e.yml) is what exercises
# them. These targets merge BOTH into one honest per-package picture using the
# built-in `go tool covdata` (no external merger needed). `cover` runs anywhere;
# `cover-integration` needs a reachable cluster; `cover-merge` combines whichever
# data dirs exist and prints per-package + total. Used by e2e.yml so submit /
# cluster reflect the coverage the e2e suite already provides.
COVERDIR ?= .coverdata

# cover: honest own-coverage per package (NO -coverpkg, so a package instruments
# only itself — no transitive cross-package inflation). The integration run keeps
# -coverpkg so it can credit the internal packages its tests exercise; the merge
# is the union of the two.
.PHONY: cover
cover:
	@mkdir -p $(COVERDIR)/unit
	$(GO) test -count=1 -cover $(PKGS) -args -test.gocoverdir="$(CURDIR)/$(COVERDIR)/unit"

.PHONY: cover-integration
cover-integration:
	@mkdir -p $(COVERDIR)/int
	$(GO) test -tags integration -count=1 -timeout 10m -cover -coverpkg=$(PKGS) ./test/integration/... -args -test.gocoverdir="$(CURDIR)/$(COVERDIR)/int"

.PHONY: cover-merge
cover-merge:
	@dirs="$$(ls -d $(COVERDIR)/unit $(COVERDIR)/int 2>/dev/null | paste -sd, -)"; \
	if [ -z "$$dirs" ]; then echo "no coverage data — run \`make cover\` and/or \`make cover-integration\` first"; exit 1; fi; \
	echo "==> merged coverage from: $$dirs"; \
	$(GO) tool covdata percent -i="$$dirs"; \
	$(GO) tool covdata textfmt -i="$$dirs" -o=$(COVERDIR)/merged.txt; \
	echo "==> overall (unit union integration):"; \
	$(GO) tool cover -func=$(COVERDIR)/merged.txt | tail -1

# Lint set matched to .github/workflows/build.yml's lint job: errcheck +
# ineffassign + misspell + staticcheck (gofmt -s is `fmt-check`, go vet
# is `vet`). CI runs the SAME pinned standalone tools, keeping the
# "make ci green => CI green" invariant this Makefile exists to protect.
# `make lint-full` keeps golangci-lint available for a richer local pass.
#
# staticcheck runs `-checks all,-ST1005`: ST1005 (error-string style) is
# excluded pending a deliberate review of the ~58 customer-visible error
# strings it flags — follow-up to tracebloc/cli#279.
.PHONY: lint
lint:
	$(GO) run github.com/kisielk/errcheck@$(ERRCHECK_VERSION) ./...
	$(GO) run github.com/gordonklaus/ineffassign@$(INEFFASSIGN_VERSION) ./...
	$(GO) run github.com/client9/misspell/cmd/misspell@$(MISSPELL_VERSION) -error .
	$(GO) run honnef.co/go/tools/cmd/staticcheck@$(STATICCHECK_VERSION) -checks all,-ST1005 ./...

# deadcode: BLOCKING reachability scan from the CLI entrypoint (~5s). The four
# legit unreachables — Stringer methods (Status.String, JobOutcome.String)
# reached only via fmt reflection that static analysis can't see, plus the
# di#349 test-only parity harnesses (ReadLabelValues, inferColumnType) — are
# declared in scripts/deadcode-allowlist.txt with reasons. Anything else
# unreachable fails the build (#281 flipped this from advisory).
.PHONY: deadcode
deadcode:
	@DEADCODE_VERSION=$(DEADCODE_VERSION) ./scripts/deadcode-check.sh

# vulncheck: govulncheck reachability scan for known CVEs (stdlib + deps).
# BLOCKING — this is a customer-installed binary; v0.8.0 shipped with 6
# reachable vulns before this gate existed (#276). Mirrors the govulncheck
# job in build.yml (PR gate) and vulncheck.yml (weekly cron on develop).
# Needs network for the vuln DB (https://vuln.go.dev), like schema-check.
# backend#1972: asserts no workflow restates a version this Makefile declares.
# Runs in the Lint job beside check-style.sh.
.PHONY: check-tool-pins
check-tool-pins:
	bash scripts/check-tool-pins.sh

.PHONY: vulncheck
vulncheck:
	$(GO) run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...

# Pinned to the exact version the golangci CI job runs (see
# .github/workflows/golangci.yml), via the same `go run tool@version`
# pattern as the tools above — no PATH dependency, so a green
# `make ci` and the PR gate can never disagree on golangci version.
# First run builds from source (~1-2 min); cached afterwards.
.PHONY: lint-full
lint-full:
	$(GO) run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run

# fmt / fmt-check: gofmt -s (simplification) + goimports -local (import
# grouping: stdlib / third-party / our own — matches .golangci.yml's
# local-prefixes).
#
# Both scope to `git ls-files '*.go'` rather than `.` (cli#549). `.` is the whole
# working TREE, so an untracked scratch directory holding Go files — a nested git
# worktree, a vendored copy, a build sandbox — failed `make check` while every
# tracked file was clean, and `make fmt` then rewrote content the repo does not
# track. build.yml's Lint job calls these same targets, so the file set has one
# definition; see scripts/format.sh for the fail-closed cases.
.PHONY: fmt
fmt:
	@GO="$(GO)" GOIMPORTS_VERSION=$(GOIMPORTS_VERSION) ./scripts/format.sh --write

.PHONY: fmt-check
fmt-check:
	@GO="$(GO)" GOIMPORTS_VERSION=$(GOIMPORTS_VERSION) ./scripts/format.sh --check

# fmt-selftest: the properties scripts/format.sh must not lose — the formatters
# are stubbed, so it is hermetic and ~6 s. It exists because the FIRST cut of
# format.sh shipped a false green: run_formatter `exit`ed from inside a command
# substitution, which ends only the subshell, so check mode read an empty capture
# and printed "clean" on a formatter that never ran (caught in review on #550).
# A comment cannot hold that shut; this can.
.PHONY: fmt-selftest
fmt-selftest:
	@bash scripts/tests/format-verify.sh

.PHONY: schema-check
schema-check:
	./scripts/sync-schema.sh --check

.PHONY: schema-sync
schema-sync:
	./scripts/sync-schema.sh

# file-budget: per-file line ceilings (ratchet down only — raising one is a
# reviewed edit to scripts/file-budget.sh). Keeps the next 1500-line data.go
# from growing quietly (backend#1106 WS-B). Also enforced by build.yml's
# lint job, keeping the "make ci green => CI green" invariant.
.PHONY: file-budget
file-budget:
	./scripts/file-budget.sh

# check-style: enforce the terminal style system + terminology (STYLE.md) —
# no hardcoded brand colour outside internal/ui, no status emoji, "secure
# environment" not "workspace". Mirrors the Lint job's guard so `make ci`
# matches CI. Mechanical only; role/wording judgement stays with review.
.PHONY: check-style
check-style:
	./scripts/check-style.sh

# ---- cleanup -----------------------------------------------------

.PHONY: clean
clean:
	rm -rf tracebloc dist/ coverage.out coverage.html $(COVERDIR)
