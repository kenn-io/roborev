# Makefile for roborev development builds

VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS := -X go.kenn.io/roborev/internal/version.Version=$(VERSION)
ACP_TEST_COMMAND ?= $(abspath scripts/acp-agent)
ACP_TEST_ADAPTER ?= codex
ACP_TEST_ARGS ?=
ACP_TEST_DISABLE_MODE ?=
ACP_TEST_MODE ?=
ACP_TEST_MODEL ?=

# Pinned golangci-lint version. Single source of truth: CI reads it via
# `make print-golangci-lint-version` (see .github/workflows/ci.yml), and
# `make lint`/`make lint-ci` refuse to run unless the local binary matches.
# A mismatched version can silently apply different formatting/fixes.
GOLANGCI_LINT_VERSION := 2.12.2

# Keep the golangci-lint cache per-checkout. The default user-global cache
# stores raw linter findings keyed by package content with absolute file
# paths from whichever checkout computed them. Post-processors (nolint,
# generated-file exclusion) re-read files at those paths on every run; if
# another worktree with identical content populated the cache and was later
# deleted, those reads fail and suppressed findings leak through as errors
# (golangci-lint #3502). A per-checkout cache dies with its checkout.
export GOLANGCI_LINT_CACHE := $(CURDIR)/.golangci-lint-cache

.PHONY: build install clean test test-git-isolation test-integration test-acp-integration test-acp-integration-codex test-acp-integration-claude test-acp-integration-gemini test-postgres test-all postgres-up postgres-down test-postgres-ci api-generate lint lint-ci check-golangci-lint print-golangci-lint-version check-renovate-config install-hooks

build:
	@mkdir -p bin
	go build -ldflags="$(LDFLAGS)" -o bin/roborev ./cmd/roborev

install:
	@# Install to ~/.local/bin for development (creates directory if needed)
	@if [ -z "$(HOME)" ]; then echo "error: HOME is not set" >&2; exit 1; fi
	@mkdir -p "$(HOME)/.local/bin"
	go build -ldflags="$(LDFLAGS)" -o "$(HOME)/.local/bin/roborev" ./cmd/roborev
	@echo "Installed to ~/.local/bin/roborev"

clean:
	rm -rf bin/

# Regenerate the checked-in OpenAPI document and public Go client.
api-generate:
	go generate ./pkg/client/generated

# Unit tests only (excludes integration and postgres tests)
test:
	go test ./...

test-git-isolation:
	go test -run '^TestGitUsingTestPackagesUseIsolatedTestMain$$' .

# Unit + slow integration tests (no postgres required)
test-integration:
	go test -tags=integration ./...

# ACP adapter integration smoke test (opt-in external dependency)
# Usage:
#   make test-acp-integration
#   make test-acp-integration ACP_TEST_ADAPTER=claude
#   make test-acp-integration ACP_TEST_COMMAND=codex-acp
#   make test-acp-integration ACP_TEST_ARGS="--provider codex"
#   make test-acp-integration ACP_TEST_DISABLE_MODE=1
#   make test-acp-integration ACP_TEST_MODE=plan ACP_TEST_MODEL=gpt-5
test-acp-integration:
	ROBOREV_RUN_ACP_INTEGRATION=1 \
	ROBOREV_ACP_ADAPTER="$(ACP_TEST_ADAPTER)" \
	ROBOREV_ACP_TEST_COMMAND="$(ACP_TEST_COMMAND)" \
	ROBOREV_ACP_TEST_ARGS="$(ACP_TEST_ARGS)" \
	ROBOREV_ACP_TEST_DISABLE_MODE="$(ACP_TEST_DISABLE_MODE)" \
	ROBOREV_ACP_TEST_MODE="$(ACP_TEST_MODE)" \
	ROBOREV_ACP_TEST_MODEL="$(ACP_TEST_MODEL)" \
		go test -tags="integration acp" ./internal/agent -run TestACPReviewViaExternalAdapter -count=1 -v

test-acp-integration-codex:
	@if [ "$(ACP_TEST_COMMAND)" = "$(abspath scripts/acp-agent)" ] && \
		[ -z "$$ROBOREV_ACP_ADAPTER_COMMAND" ] && \
		! command -v codex-acp >/dev/null 2>&1; then \
		echo "error: codex-acp was not found on PATH."; \
		echo ""; \
		echo "Install it with:"; \
		echo "  npm install -g @zed-industries/codex-acp"; \
		echo ""; \
		echo "Or override the wrapper command:"; \
		echo "  make test-acp-integration-codex ACP_TEST_COMMAND=/path/to/your/acp-wrapper"; \
		echo "  export ROBOREV_ACP_ADAPTER_COMMAND=/path/to/your/acp-wrapper"; \
		exit 127; \
	fi
	@$(MAKE) test-acp-integration ACP_TEST_ADAPTER=codex ACP_TEST_DISABLE_MODE=1

test-acp-integration-claude:
	@if [ "$(ACP_TEST_COMMAND)" = "$(abspath scripts/acp-agent)" ] && \
		[ -z "$$ROBOREV_ACP_ADAPTER_COMMAND" ] && \
		! command -v claude-agent-acp >/dev/null 2>&1; then \
		echo "error: claude-agent-acp was not found on PATH."; \
		echo ""; \
		echo "Install it with:"; \
		echo "  npm install -g @zed-industries/claude-agent-acp"; \
		echo ""; \
		echo "Then rerun this target."; \
		echo ""; \
		echo "Or override the wrapper command:"; \
		echo "  make test-acp-integration-claude ACP_TEST_COMMAND=/path/to/your/acp-wrapper"; \
		echo "  export ROBOREV_ACP_ADAPTER_COMMAND=/path/to/your/acp-wrapper"; \
		exit 127; \
	fi
	@$(MAKE) test-acp-integration ACP_TEST_ADAPTER=claude ACP_TEST_DISABLE_MODE=1

test-acp-integration-gemini:
	@if [ "$(ACP_TEST_COMMAND)" = "$(abspath scripts/acp-agent)" ] && \
		[ -z "$$ROBOREV_ACP_ADAPTER_COMMAND" ] && \
		! command -v gemini >/dev/null 2>&1; then \
		echo "error: gemini was not found on PATH."; \
		echo ""; \
		echo "Install it with:"; \
		echo "  npm install -g @google/gemini-cli"; \
		echo ""; \
		echo "Then rerun this target."; \
		echo ""; \
		echo "Or override the wrapper command:"; \
		echo "  make test-acp-integration-gemini ACP_TEST_COMMAND=/path/to/your/acp-wrapper"; \
		echo "  export ROBOREV_ACP_ADAPTER_COMMAND=/path/to/your/acp-wrapper"; \
		exit 127; \
	fi
	@$(MAKE) test-acp-integration ACP_TEST_ADAPTER=gemini ACP_TEST_DISABLE_MODE=1

# Start postgres for postgres tests
postgres-up:
	docker compose -f docker-compose.test.yml up -d --wait

# Stop postgres
postgres-down:
	docker compose -f docker-compose.test.yml down

# Postgres tests (requires postgres running)
test-postgres: postgres-up
	@echo "Waiting for postgres to be ready..."
	@sleep 2
	TEST_POSTGRES_URL="postgres://roborev_test:roborev_test_password@localhost:5433/roborev_test" \
		go test -tags=postgres -v ./internal/storage/... -run Integration

# Run all tests (unit + integration + postgres)
test-all: test-integration test-postgres

# Lint Go code and auto-fix where possible (local development)
# Verify golangci-lint is installed and exactly matches the pinned version.
# Fails loudly on mismatch rather than letting a different version silently
# reformat files or report different findings than CI.
check-golangci-lint:
	@if ! command -v golangci-lint >/dev/null 2>&1; then \
		echo "golangci-lint not found. Install the pinned version:" >&2; \
		echo "  go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v$(GOLANGCI_LINT_VERSION)" >&2; \
		exit 1; \
	fi; \
	have="$$(golangci-lint version --short 2>/dev/null)"; \
	if [ "$$have" != "$(GOLANGCI_LINT_VERSION)" ]; then \
		echo "Error: golangci-lint version mismatch (must match CI)." >&2; \
		echo "  found:    $${have:-unknown}" >&2; \
		echo "  required: $(GOLANGCI_LINT_VERSION)" >&2; \
		echo "Install the pinned version:" >&2; \
		echo "  go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v$(GOLANGCI_LINT_VERSION)" >&2; \
		exit 1; \
	fi

# Print the pinned golangci-lint version (consumed by CI to stay in lockstep).
print-golangci-lint-version:
	@echo "$(GOLANGCI_LINT_VERSION)"

lint: check-golangci-lint
	golangci-lint run --fix ./...

# Lint Go code without fixing (for CI)
lint-ci: check-golangci-lint
	golangci-lint run ./...

# Validate Renovate config.
check-renovate-config:
	@if ! command -v renovate-config-validator >/dev/null 2>&1; then \
		echo "renovate-config-validator not found. Install with: mise use --global npm:renovate@latest" >&2; \
		exit 1; \
	fi
	renovate-config-validator renovate.json

# Install pre-commit hooks via prek.
install-hooks:
	@if ! command -v prek >/dev/null 2>&1; then \
		echo "prek not found. Install with: brew install prek" >&2; \
		exit 1; \
	fi
	prek install

# CI target: run postgres tests without managing docker (assumes postgres is running)
test-postgres-ci:
	go test -tags=postgres -v ./internal/storage/... -run Integration
