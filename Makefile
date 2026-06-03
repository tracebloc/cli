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

# ---- top-level targets -------------------------------------------

.PHONY: ci
ci: vet test lint fmt-check schema-check
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

# Lint set matched to .github/workflows/build.yml's lint job: errcheck +
# ineffassign + misspell (gofmt -s is `fmt-check`, go vet is `vet`).
# golangci-lint-action is disabled in CI pending tracebloc/cli#6 — so
# until it's re-enabled there, `make ci` runs the SAME standalone tools
# the CI lint job runs, keeping the "make ci green => CI green" invariant
# this Makefile exists to protect. `make lint-full` keeps golangci-lint
# available for a richer local pass.
.PHONY: lint
lint:
	$(GO) run github.com/kisielk/errcheck@latest ./...
	$(GO) run github.com/gordonklaus/ineffassign@latest ./...
	$(GO) run github.com/client9/misspell/cmd/misspell@latest -error .

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

.PHONY: fmt-check
fmt-check:
	@diff="$$(gofmt -s -l . 2>/dev/null)"; \
	if [ -n "$$diff" ]; then \
	  echo "==> gofmt -s needed on:"; \
	  echo "$$diff" | sed 's/^/    /'; \
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
	rm -rf tracebloc dist/ coverage.out coverage.html
