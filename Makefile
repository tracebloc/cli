# Top-level Makefile for tracebloc/cli.
#
# Purpose: keep the local feedback loop the same shape as the CI
# loop. Anything that fails in `make ci` would have failed on a PR,
# and vice versa. Don't add targets here that aren't also enforced
# by .github/workflows/build.yml — divergence between local and CI
# is the bug this file exists to prevent.

# ---- toggles -----------------------------------------------------

GO            ?= go
GOLANGCI_LINT ?= golangci-lint
PKGS          := ./...

# Pinned lint/analysis tool versions (reproducibility — no more @latest drift).
# Keep these in lockstep with .github/workflows/build.yml. Bump deliberately.
ERRCHECK_VERSION    ?= v1.20.0
INEFFASSIGN_VERSION ?= v0.2.0
MISSPELL_VERSION    ?= v0.3.4
DEADCODE_VERSION    ?= v0.48.0
GOVULNCHECK_VERSION ?= v1.1.4
STATICCHECK_VERSION ?= 2025.1.1
GOIMPORTS_VERSION   ?= v0.48.0

# ---- top-level targets -------------------------------------------

.PHONY: ci
ci: vet test lint fmt-check schema-check vulncheck deadcode
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

# deadcode: reachability scan from the CLI entrypoint (~5s). ADVISORY for now
# (non-blocking) — it prints unreachable funcs but never fails the build. The
# module still carries pre-existing dead-ish funcs that are unsafe to delete
# blindly: Stringer methods (Status.String, JobOutcome.String) reached only via
# fmt reflection that static analysis can't see, plus test-only parity harnesses
# (ReadLabelValues, inferColumnType — di#349). Flip to blocking once that
# backlog is cleared. Tracked in tracebloc/cli#6 / #127.
.PHONY: deadcode
deadcode:
	@echo "==> deadcode (advisory): unreachable funcs from ./cmd/tracebloc"
	@$(GO) run golang.org/x/tools/cmd/deadcode@$(DEADCODE_VERSION) ./cmd/tracebloc || true

# vulncheck: govulncheck reachability scan for known CVEs (stdlib + deps).
# BLOCKING — this is a customer-installed binary; v0.8.0 shipped with 6
# reachable vulns before this gate existed (#276). Mirrors the govulncheck
# job in build.yml (PR gate) and vulncheck.yml (weekly cron on develop).
# Needs network for the vuln DB (https://vuln.go.dev), like schema-check.
.PHONY: vulncheck
vulncheck:
	$(GO) run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...

.PHONY: lint-full
lint-full:
	@command -v $(GOLANGCI_LINT) >/dev/null 2>&1 || { \
	  echo "==> $(GOLANGCI_LINT) not on PATH"; \
	  echo "    install via:  brew install golangci-lint"; \
	  echo "    or see:       https://golangci-lint.run/usage/install/"; \
	  exit 1; \
	}
	$(GOLANGCI_LINT) run

.PHONY: fmt
fmt:
	gofmt -s -w .
	$(GO) run golang.org/x/tools/cmd/goimports@$(GOIMPORTS_VERSION) -local github.com/tracebloc/cli -w .

# fmt-check: gofmt -s (simplification) + goimports -local (import grouping:
# stdlib / third-party / our own — matches .golangci.yml's local-prefixes).
.PHONY: fmt-check
fmt-check:
	@diff="$$(gofmt -s -l . 2>/dev/null)"; \
	if [ -n "$$diff" ]; then \
	  echo "==> gofmt -s needed on:"; \
	  echo "$$diff" | sed 's/^/    /'; \
	  echo "==> run \`make fmt\` to fix"; \
	  exit 1; \
	fi
	@drift="$$($(GO) run golang.org/x/tools/cmd/goimports@$(GOIMPORTS_VERSION) -local github.com/tracebloc/cli -l .)"; \
	if [ -n "$$drift" ]; then \
	  echo "==> goimports (import grouping) needed on:"; \
	  echo "$$drift" | sed 's/^/    /'; \
	  echo "==> run \`make fmt\` to fix"; \
	  exit 1; \
	fi

.PHONY: schema-check
schema-check:
	./scripts/sync-schema.sh --check

.PHONY: schema-sync
schema-sync:
	./scripts/sync-schema.sh

# ---- cleanup -----------------------------------------------------

.PHONY: clean
clean:
	rm -rf tracebloc dist/ coverage.out coverage.html $(COVERDIR)
