# AGENTS.md

## Purpose

This repo hosts roborev, a local daemon + CLI for AI-assisted code review. Use this guide to
navigate the codebase quickly, understand subsystem boundaries, and avoid missing adjacent
changes when working in a growing Go project.

## Start Here

- Product behavior and user-facing workflows: `README.md`
- CLI entry point and command registration: `cmd/roborev/main.go`
- Command discovery: `rg '^func .*Cmd\(' cmd/roborev`
- Daemon HTTP API and route wiring: `internal/daemon/server.go`
- Worker pool and job execution: `internal/daemon/worker.go`
- SQLite schema and migrations: `internal/storage/db.go`
- Config loading and resolution: `internal/config/config.go`
- Prompt construction: `internal/prompt/prompt.go`
- TUI entry point: `cmd/roborev/tui/tui.go`

## Architecture At A Glance

```text
CLI (roborev) -> HTTP API -> Daemon -> Worker Pool -> Agent adapters
                      |            |
                      |            -> Hooks / Activity log / CI poller
                      |
                      -> SQLite DB <-> Sync worker <-> PostgreSQL (optional)
```

- The daemon is the long-lived control plane. Many CLI commands are thin HTTP clients.
- Background daemon work must not edit tracked source files in the user's checked-out working tree or apply agent changes there. Repo metadata is different: `roborev init` may update the usually tracked `.gitignore` so the configured `snapshot_dir` (default `.roborev/`) is ignored, and daemon review work may create disposable ignored snapshot artifacts there so sandboxed agents can read oversized diffs. Runtime snapshot creation may also add a local `.git/info/exclude` fallback when an existing checkout is missing the ignore rule.
- Foreground agentic flows such as `roborev fix` and `roborev refine` may modify code.
- Isolated background fix work uses temporary git worktrees and stores patches in the DB.

## Package Map

| Path | Purpose | Start with |
|------|---------|------------|
| `cmd/roborev/` | Cobra CLI commands and daemon-facing client logic | `main.go`, `review.go`, `fix.go`, `refine.go`, `analyze.go`, `daemon_cmd.go` |
| `cmd/roborev/tui/` | Bubble Tea terminal UI | `tui.go`, `api.go`, `fetch.go`, `handlers.go`, `render_*.go` |
| `internal/daemon/` | HTTP server, handlers, worker pool, SSE, hooks, runtime, CI poller | `server.go`, `worker.go`, `client.go`, `runtime.go`, `ci_poller.go` |
| `internal/storage/` | SQLite models/queries/migrations, sync logic, PostgreSQL mirror | `db.go`, `models.go`, `jobs.go`, `reviews.go`, `sync.go`, `postgres.go` |
| `internal/agent/` | Agent interface, registry, command-backed implementations, ACP support | `agent.go`, `codex.go`, `claude.go`, `acp.go`, `test_agent.go` |
| `internal/config/` | Global config, repo config, validation, key metadata, resolve helpers | `config.go`, `keyval.go` |
| `internal/prompt/` | Review prompt builder and template loading | `prompt.go`, `templates.go`, `prompt/analyze/` |
| `internal/review/` | Daemon-free batch review, synthesis, comment sizing/formatting | `batch.go`, `synthesis.go`, `result.go` |
| `internal/git/` | Shared git helpers for refs, diffs, branch logic, repo discovery | `git.go` |
| `internal/worktree/` | Temporary worktree creation, patch capture/apply/check | `worktree.go` |
| `internal/skills/` | Embedded Codex/Claude skill files and installer logic | `skills.go`, `internal/skills/claude/`, `internal/skills/codex/` |
| `internal/streamfmt/` | Formatting streamed agent output for CLI and TUI | `streamfmt.go`, `render.go` |
| `internal/githook/` | Hook install/upgrade logic | `githook.go` |
| `internal/github/` | GitHub REST helpers used by CI/comment flows | `comment.go` |
| `internal/ghaction/` | GitHub Actions integration | `ghaction.go` |
| `internal/update/` | Self-update logic and release fetches | `update.go` |
| `internal/testenv/` | Test environment setup helpers | `testenv.go` |
| `internal/testutil/` | Temp git repos and shared test helpers | `git.go`, `testutil.go` |
| `docs/plans/` | Design notes for larger features | open the matching date/topic file |
| `skills/` | Human-readable skill docs shipped to agents | `README.md` and `roborev-*.md` |

## Command Map

- Review queueing and results: `review.go`, `wait.go`, `status.go`, `list.go`, `show.go`, `comment.go`, `compact.go`
- Agentic fix flows: `fix.go`, `refine.go`, `run.go`, `analyze.go`
- Daemon lifecycle and transport: `daemon_cmd.go`, `daemon_client.go`, `stream.go`, `log_cmd.go`
- Repo/bootstrap/hooks: `init_cmd.go`, `hook.go`, `repo.go`, `remap.go`
- Configuration and maintenance: `config_cmd.go`, `skills.go`, `update.go`, `version.go`, `check_agents.go`
- Automation/integration: `ci.go`, `ghaction.go`, `sync.go`
- TUI entry command: `tui_cmd.go`

## Follow The Flow

### Review path

`roborev review` typically flows through:

1. CLI validation and repo/ref resolution in `cmd/roborev/review.go`
2. Daemon startup/runtime lookup in `cmd/roborev/daemon_cmd.go` and `cmd/roborev/daemon_client.go`
3. `POST /api/enqueue` handled by `internal/daemon/server.go`
4. Job persistence in `internal/storage/`
5. Prompt building in `internal/prompt/prompt.go`
6. Agent execution from `internal/agent/`
7. Review/result persistence plus broadcasts/hooks in `internal/daemon/`

### Fix path

- `roborev fix` runs agents synchronously in the foreground from `cmd/roborev/fix.go`.
- It often discovers candidate jobs through daemon APIs, but the actual code modification happens locally.
- When behavior changes, inspect both the CLI-side flow and the job state transitions in storage.

### Refine path

- `roborev refine` is the automated review-fix-review loop in `cmd/roborev/refine.go`.
- It uses `internal/worktree/` to keep agent work isolated before applying changes back.
- If you touch refine behavior, also inspect branch validation, worktree patching, and re-review polling.

### Analyze path

- `roborev analyze` uses prompt templates from `internal/prompt/analyze/`.
- It can enqueue analysis jobs, wait on them, and optionally pass results into a fix flow.
- Changes here usually need command tests plus prompt/template coverage.

### TUI path

- TUI state/model setup: `cmd/roborev/tui/tui.go`
- HTTP requests: `cmd/roborev/tui/api.go`
- Background fetch/update logic: `cmd/roborev/tui/fetch.go`
- Input handling: `cmd/roborev/tui/handlers*.go`
- Rendering: `cmd/roborev/tui/render_*.go`
- Filtering/search/tree state: `cmd/roborev/tui/filter*.go`

## Change Impact Guide

- CLI flag or output changes usually require updates in the command file, its tests, and sometimes `README.md`.
- Daemon API changes usually require updates in `internal/daemon/server.go`, `internal/daemon/client.go`, CLI callers, TUI callers, and API/integration tests.
- Worker/job lifecycle changes usually affect `internal/daemon/worker.go`, `internal/storage/jobs.go`, status/event broadcasting, cancellation, and rerun behavior.
- Storage schema changes must stay minimal and need both SQLite and PostgreSQL consideration.
- SQLite changes live in `internal/storage/db.go`; PostgreSQL schema/versioning lives in `internal/storage/postgres.go` plus `internal/storage/schemas/postgres_v*.sql`.
- Config key changes usually touch `internal/config/config.go`, `internal/config/keyval.go`, `cmd/roborev/config_cmd.go`, and related tests.
- Agent changes usually touch the adapter file, `internal/agent/agent.go` registry/fallback logic, and tests that use `test_agent.go`.
- TUI changes rarely live in one file; expect to touch fetch, handlers, render, and tests together.
- Hook behavior spans `cmd/roborev/init_cmd.go`, `internal/githook/`, and daemon hook execution in `internal/daemon/hooks.go`.
- CI/sync work often crosses daemon, storage, and config packages. Do not patch only one side.

## Runtime And Config Notes

- Daemon default address: `127.0.0.1:7373` and auto-increments if busy.
- Runtime info: `~/.roborev/daemon.json`
- SQLite DB: `~/.roborev/reviews.db` using WAL mode
- Data dir override: `ROBOREV_DATA_DIR`
- Color mode: `ROBOREV_COLOR_MODE` env var (`auto`, `dark`, `light`, `none`); `NO_COLOR=1` also supported
- Global config: `~/.roborev/config.toml`
- Repo config: `.roborev.toml` at repo root
- Config precedence is generally: CLI flags -> repo config -> global config -> defaults
- Reasoning defaults: review = `thorough`, fix = `standard`, refine = `standard`
- Repo config can include `review_guidelines`; prompt building pulls these into reviews
- `roborev init` installs or upgrades git hooks; daemon startup warns about stale hooks
- `roborev refine` is agentic and may run with unsafe capabilities depending on flags/config; use only on trusted code

## Testing

Use `testify` (`github.com/stretchr/testify`) for all test assertions. Use `require.*` for fatal preconditions and `assert.*` for non-fatal checks. Do not use raw `if`/`t.Errorf`/`t.Fatalf` patterns.

- After any Go code changes, run `go fmt ./...` and `go vet ./...` before committing.
- Fast test pass: `go test ./...`
- Integration tests: `go test -tags=integration ./...`
- PostgreSQL tests: `go test -tags=postgres -v ./internal/storage/... -run Integration`
- ACP smoke tests: use the `make test-acp-integration*` targets in `Makefile`
- Pre-commit hooks in this repo are managed with `prek`; run `prek install` after cloning or `make install-hooks` as a wrapper.
- The local pre-commit hook is a `prek` system hook that runs `make lint` with `always_run`, so it executes on every commit and may auto-fix files before the commit succeeds.
- If the hook rewrites files, re-stage them and rerun the commit. Use `prek run --all-files` to execute the hook manually and `make lint-ci` for a non-mutating lint check.
- Useful build/lint checks: `go build ./...`, `make lint`, `make lint-ci`, `prek run --all-files`

Test conventions:

- Prefer fast, isolated tests that use `t.TempDir()`.
- Use the `test` agent path to avoid calling real AI agents.
- Integration tests use `//go:build integration`.
- PostgreSQL-only tests use `//go:build postgres`.
- ACP adapter integration tests use `//go:build integration && acp`.
- Shared helpers live in `internal/testenv/`, `internal/testutil/`, and package-local `*_test_helpers*.go`.
- In tests with more than three assertions, prefer `assert := assert.New(t)` to make grouped assertions cleaner.
- Test packages that run git or use git repo helpers from `internal/testutil`
  must define `TestMain` and call `testenv.RunIsolatedMain`; the
  `test-git-isolation` pre-commit hook enforces this.
- `assert.True*` / `require.True*` / `assert.False*` / `require.False*` should only be used for actual boolean values (fields, return values); never use them as comparisons (e.g., `assert.True(t, x == y)` should be `assert.Equal(t, y, x)`).
- Do not use `assert.Fail`/`require.Fail` in tests.
- Prefer `assert.Equal` for explicit expectations.
- Convert redundant `if` wrappers around assertions into direct assertions (for example, `if err != nil { require.NoError(t, err) }` should become `require.NoError(t, err)`).
- Do not replace assertions with manual control-flow (`if`, `t.Fatal*`, `t.Error*`) when a direct testify check covers the same condition.
- Enforce a no-redundant-guards policy for assertions:
  - replace `if err != nil { require.NoError(t, err, ...) }` and `if err == nil { ... } else { ... }` with direct `require.Error`/`require.NoError`/`require.NotNil` statements.
  - avoid manual `if` prechecks such as `if got != want` or `if cfg != nil`; convert to direct `assert.Equal`/`assert.NotNil` assertions.
  - remove `assert`/`require` fail helpers and `t.Fatal`/`t.Fatalf`/`t.Error` usage when a direct assertion provides the same check.

## Search Shortcuts

- Commands: `rg '^func .*Cmd\(' cmd/roborev`
- Daemon handlers: `rg 'handle[A-Z]' internal/daemon`
- Job state usage: `rg 'JobStatus|job_type|review_type' internal cmd/roborev`
- Build tags: `rg '^//go:build' .`
- Config resolution: `rg 'Resolve[A-Z]|LoadRepoConfig|LoadGlobal' internal/config`
- Agent capability methods: `rg 'WithReasoning|WithAgentic|WithModel' internal/agent`
- TUI view logic: `rg 'view[A-Z]|currentView|handle.*Key' cmd/roborev/tui`

## Development Preferences

- Keep changes simple; avoid over-engineering.
- Prefer Go stdlib over new dependencies.
- No emojis in code or output (commit messages are fine).
- Never amend commits; fixes should be new commits.
- Never push/pull unless explicitly asked.
- NEVER merge pull requests.
- NEVER change git branches without explicit user confirmation. Always ask before switching, creating, or checking out branches.
- NEVER run `make install`, `go install ./cmd/roborev`, or any build/install command that writes a `roborev` binary into a user PATH without explicit user permission. Installing a development build can replace the production local daemon binary, trigger schema migrations against `~/.roborev/reviews.db`, and damage the user's live review environment.
- Release builds use `CGO_ENABLED=0`.
- Release workflow testing must not publish anything. Do not run any
  command, workflow mode, or release tool path that creates public
  releases, uploads release assets, publishes package repositories,
  updates taps, signs release artifacts for distribution, pushes tags,
  or writes to any external release channel. Use local, non-publishing
  validation that proves artifact names, checksums, and package contents.
  If a release tool cannot validate without publishing, stop and ask the
  user instead of approximating with a live publish.

## Workflow + Commits

- For multi-step tasks (for example: implement + commit + PR), complete the full requested sequence without stopping partway.
- Commit after completing each piece of work; do not wait to be asked.
- When committing, stage ALL modified files related to the work (including formatting-only and ancillary updates).
- Before committing, run `git diff` and `git status` to verify nothing is unintentionally left unstaged.
- When creating PRs, write a clean GitHub-facing summary with relevant context and links.
- PR descriptions should not include standalone "Verification" or
  "Test Plan" sections unless the user explicitly asks for them. Keep PR
  bodies focused on summary, context, and behavior changes; rely on CI/status
  checks for routine validation evidence.

## Review / Refine Guidance

When reviewing or fixing issues:

- Focus on correctness, concurrency safety, and error handling in daemon/worker code.
- For storage changes, keep migrations minimal and validate schema and queries.
- For API changes, preserve HTTP/JSON conventions.
- For daemon changes, preserve the rule that background jobs must not edit tracked source files in the checked-out working tree or apply agent changes there. The repo metadata exception is snapshot ignore setup: `.gitignore` may be updated by `roborev init`, and daemon snapshot writes may add a local `.git/info/exclude` fallback. Ignored repo-local snapshot artifacts are allowed only for oversized diff handoff and must stay under the configured `snapshot_dir`, remain gitignored, and be cleaned up best-effort.
- When addressing review feedback, update tests if behavior changes.
- If the user pastes review findings or review text directly into the prompt, treat that as direct fix input and work from the pasted content.
- Do not invoke review-fetching skills for pasted review text unless the user explicitly asks for that skill or provides only a review/job ID that must be fetched first.
- If diffs are large or truncated, inspect with `git show <sha>`.
## Landing the Plane (Session Completion)

**When ending a work session**, you MUST complete ALL steps below. Work is NOT complete until `git push` succeeds.

**MANDATORY WORKFLOW:**

1. **File issues for remaining work** - Create issues for anything that needs follow-up
2. **Run quality gates** (if code changed) - Tests, linters, builds
3. **Update issue status** - Close finished work, update in-progress items
4. **PUSH TO REMOTE** - This is MANDATORY:
   ```bash
   git pull --rebase
   git push
   git status  # MUST show "up to date with origin"
   ```
5. **Clean up** - Clear stashes, prune remote branches
6. **Verify** - All changes committed AND pushed
7. **Hand off** - Provide context for next session

**CRITICAL RULES:**
- Work is NOT complete until `git push` succeeds
- NEVER stop before pushing - that leaves work stranded locally
- NEVER say "ready to push when you are" - YOU must push
- If push fails, resolve and retry until it succeeds
