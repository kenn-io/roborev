# Makefile for roborev development builds

VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS := -X go.kenn.io/roborev/internal/version.Version=$(VERSION)
ACP_TEST_COMMAND ?= $(abspath scripts/acp-agent)
ACP_TEST_ADAPTER ?= codex
ACP_TEST_ARGS ?=
ACP_TEST_DISABLE_MODE ?=
ACP_TEST_MODE ?=
ACP_TEST_MODEL ?=

.PHONY: build install clean test test-git-isolation test-integration test-acp-integration test-acp-integration-codex test-acp-integration-claude test-acp-integration-gemini test-postgres test-all postgres-up postgres-down test-postgres-ci lint lint-ci install-hooks

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
lint:
	@if ! command -v golangci-lint >/dev/null 2>&1; then \
		echo "golangci-lint not found. Install with: go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.10.1" >&2; \
		exit 1; \
	fi
	golangci-lint run --fix ./...

# Lint Go code without fixing (for CI)
lint-ci:
	@if ! command -v golangci-lint >/dev/null 2>&1; then \
		echo "golangci-lint not found. Install with: go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.10.1" >&2; \
		exit 1; \
	fi
	golangci-lint run ./...

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
