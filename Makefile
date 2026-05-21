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

.PHONY: lint
lint:
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
