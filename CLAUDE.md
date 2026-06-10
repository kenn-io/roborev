# CLAUDE.md

## Project Overview

roborev is an automatic code review daemon for git commits. It runs locally, triggered by post-commit hooks, and uses AI agents (Codex, Claude Code, Gemini, Copilot, etc.) to review commits in parallel. It also supports background fix jobs, CI integration via GitHub PRs, and PostgreSQL sync for multi-machine setups.

## General Workflow

When a task involves multiple steps (e.g., implement + commit + PR), complete ALL steps in sequence without stopping. If creating a branch, committing, and opening a PR, finish the entire chain.

### Handling review findings

When the user pastes review findings directly in the conversation (verdicts, severities, file paths, suggested fixes), treat them as direct instructions and fix the code normally. Do NOT invoke the `/roborev-fix` skill — that skill is only for discovering and fetching open reviews from the daemon, not for processing findings already present in the prompt.

## Go Development

After making any Go code changes, always run `go fmt ./...` and `go vet ./...` before committing. Stage ALL resulting changes, including formatting-only files.

## Git Workflow

Always commit after completing each piece of work — do not wait to be asked. When committing changes, always stage ALL modified files (including formatting, generated files, and ancillary changes). Run `git diff` and `git status` before committing to ensure nothing is left unstaged.

## Architecture

```
CLI (roborev) → HTTP API → Daemon (roborev daemon run) → Worker Pool → Agents
                              ↓                              ↓
                          SQLite DB ←──sync──→ PostgreSQL (optional)
                              ↓
                       CI Poller → GitHub PRs
```

- **Daemon**: HTTP server on port 7373 (auto-finds available port if busy)
- **Workers**: Pool of 4 (configurable) parallel review workers
- **Storage**: SQLite at `~/.roborev/reviews.db` with WAL mode
- **Config**: Global at `~/.roborev/config.toml`, per-repo at `.roborev.toml`
- **Data dir**: Set `ROBOREV_DATA_DIR` env var to override `~/.roborev`
- **Color mode**: `ROBOREV_COLOR_MODE=auto|dark|light|none` controls TUI color theme; `NO_COLOR=1` strips all colors
- **Runtime info**: Daemon writes PID/addr/version to `~/.roborev/runtime/daemon.<pid>.json` (kit runtime store); pre-v0.57 daemons wrote `~/.roborev/daemon.json`, still read for upgrade-time stop/cleanup

## Package Map

| Package | Purpose |
|---------|---------|
| `cmd/roborev/` | CLI entry point, 25+ Cobra subcommands |
| `cmd/roborev/tui/` | Bubbletea terminal UI (queue/review/task views) |
| `internal/agent/` | Agent interface, registry, 11 implementations |
| `internal/config/` | Config structs, loading, 20+ Resolve* functions |
| `internal/daemon/` | HTTP server, worker pool, CI poller, hooks, SSE |
| `internal/storage/` | SQLite + PostgreSQL, schema migrations, job CRUD |
| `internal/prompt/` | Review prompt construction, system prompts |
| `internal/git/` | Git operations (diff, commit info, branches) |
| `internal/worktree/` | Isolated git worktrees for fix jobs |
| `internal/review/` | Synthesis, batch processing, verdict parsing |
| `internal/github/` | GitHub REST API wrappers |
| `internal/githook/` | Git hook installation/management |
| `internal/ghaction/` | GitHub Actions integration |
| `internal/kata/` | Kata task-ledger client (CLI shell-out), ref parsing, context resolution |
| `internal/skills/` | Agent skill discovery and management |
| `internal/streamfmt/` | Streaming output formatting |
| `internal/testutil/` | Test helpers (TestRepo, HTTP fixtures) |
| `internal/testenv/` | Test environment setup |
| `internal/update/` | Self-update mechanics |
| `internal/version/` | Version string (injected at build time) |

## Key Files

| Path | Purpose |
|------|---------|
| `cmd/roborev/main.go` | CLI entry point, all Cobra commands |
| `internal/daemon/server.go` | HTTP API routes and handlers (~2000 lines) |
| `internal/daemon/worker.go` | Worker pool, job processing, retry/failover |
| `internal/daemon/ci_poller.go` | GitHub PR polling, synthesis, comment posting |
| `internal/storage/db.go` | SQLite schema definition, 18 migrations |
| `internal/storage/jobs.go` | Job CRUD, state transitions, verdict parsing |
| `internal/storage/models.go` | Core types: ReviewJob, Repo, Commit, Review |
| `internal/storage/postgres.go` | PostgreSQL schema (v1-v6) and operations |
| `internal/storage/sync.go` | Sync state management (cursors, machine IDs) |
| `internal/agent/agent.go` | Agent interface, registry, alias resolution |
| `internal/config/config.go` | Config/RepoConfig structs, Resolve* functions |
| `internal/prompt/prompt.go` | Prompt builder (single, range, dirty) |
| `internal/worktree/worktree.go` | Worktree create/patch-capture/apply |
| `internal/review/synthesis.go` | Multi-agent review synthesis for CI |
| `internal/kata/client.go` | Kata CLI client (Binding, List, Show, Create) |
| `internal/kata/context.go` | Resolve kata context for prompts (off/current/open) |

## Agent System

### Interface (`internal/agent/agent.go`)

```go
type Agent interface {
    Name() string
    Review(ctx context.Context, repoPath, commitSHA, prompt string, output io.Writer) (string, error)
    WithReasoning(level ReasoningLevel) Agent
    WithAgentic(agentic bool) Agent
    WithModel(model string) Agent
    CommandLine() string
}
```

### Registered agents

codex, claude-code, gemini, copilot, opencode, cursor, kiro, kilo, droid, pi, test

### Aliases

- `"claude"` → `"claude-code"`
- `"agent"` → `"cursor"`

### Availability

Agents are discovered via PATH lookup (`CommandAgent.CommandName()`). The `test` agent is always available. `GetAvailable(preferred)` walks a fallback cascade: codex → claude-code → gemini → copilot → opencode → cursor → kiro → kilo → droid → pi.

### Reasoning levels

`ReasoningFast` ("fast"/"low"), `ReasoningStandard` ("standard"/"medium"), `ReasoningThorough` ("thorough"/"high")

### Adding a new agent

1. Create `internal/agent/newagent.go`
2. Implement the `Agent` interface
3. Call `Register()` in `init()`

## Database Schema

### Tables

**repos**: id, root_path (UNIQUE), name, created_at

**commits**: id, repo_id → repos, sha (UNIQUE per repo), author, subject, timestamp

**review_jobs** (48 columns): Core job state including agent, model, reasoning, status, timestamps, worker tracking, retry_count, prompt, diff_content, output_prefix, job_type, review_type, parent_job_id, patch, and sync fields (uuid, source_machine_id, updated_at, synced_at).

**reviews**: id, job_id → review_jobs (UNIQUE), agent, prompt, output, closed (bool), verdict_bool, sync fields

**responses**: id, job_id (or commit_id for legacy), responder, response, sync fields

**ci_pr_batches**: Tracks CI review batches per PR (total/completed/failed/synthesized jobs)

**ci_pr_batch_jobs**: Links batches to individual review jobs

### Job states

`queued` → `running` → `done` | `failed` | `canceled` | `applied` | `rebased`

### Job types

`review` (single commit), `range` (commit range), `dirty` (uncommitted), `task` (custom prompt), `compact` (consolidated multi-agent), `fix` (background worktree fix)

### Review types

Empty/`"review"` (standard), `"security"`, `"design"` — changes the system prompt

### Migrations

18 incremental migrations in `db.go` handle column additions, CHECK constraint updates, and table rebuilds. Each migration runs idempotically.

## Config System

### Hierarchy (highest to lowest precedence)

1. CLI flags (`--agent`, `--model`, etc.)
2. Per-repo `.roborev.toml` (RepoConfig)
3. Global `~/.roborev/config.toml` (Config)
4. Hardcoded defaults

### Key Resolve functions (`internal/config/config.go`)

| Function | Purpose |
|----------|---------|
| `ResolveAgent()` | CLI → repo → global → default |
| `ResolveModel()` | Same chain for model selection |
| `ResolveAgentForWorkflow(cli, repoPath, globalCfg, workflow, level)` | Workflow + reasoning-level routing |
| `ResolveModelForWorkflow(cli, repoPath, globalCfg, workflow, level)` | Same for models |
| `ResolveBackupAgentForWorkflow(repoPath, globalCfg, workflow)` | Failover agent: repo workflow → repo generic → global workflow → global default |
| `ResolveBackupModelForWorkflow(repoPath, globalCfg, workflow)` | Same chain for backup models |
| `ResolveJobTimeout()` | Per-repo or global (default 30 min) |
| `ResolveMaxPromptSize()` | Prompt truncation threshold |
| `ResolveReviewReasoning()` / `RefineReasoning()` / `FixReasoning()` | Default reasoning level per workflow |
| `ResolveKataContext(repoPath, globalCfg)` | Kata prompt-context mode/max_chars; repo overrides global field-by-field (repo `max_chars = 0` inherits global; final `<= 0` → 50000 default) |

### Workflow-specific config pattern

Fields follow the naming `{Workflow}{Setting}{Level}`:
- Workflows: Review, Refine, Fix, Security, Design
- Settings: Agent, Model, BackupAgent, BackupModel
- Levels (agent/model only): Fast, Standard, Thorough

TOML tags: `review_agent_fast`, `fix_model_thorough`, `security_backup_agent`, etc.

The internal `lookupFieldByTag()` helper resolves these via reflection on the struct's TOML tags.

### RepoConfig notable fields

`agent`, `model`, `backup_agent`, `backup_model`, `review_context_count`, `review_guidelines`, `job_timeout_minutes`, `max_prompt_size`, `allow_unsafe_agents`, per-workflow agent/model/backup overrides, hooks config, `kata_context` (mode/max_chars)

The built-in `[[hooks]] type = "kata"` files review findings as kata issues (hook fields: `project`, `labels`, `priority`); idempotency keys prefer the job UUID. All hooks also accept an optional `branches` glob allowlist (`path.Match`; empty = all branches), gated centrally in `matchBranch`. The matched `Event.Branch` is the commit's branch for local reviews and the PR base (target) branch for CI reviews — never the fork-controlled head ref. CI jobs store the base branch in the dedicated `ci_base_branch` column and leave `branch` empty so branch-scoped local flows (fix/refine discovery, fix-ref selection, session reuse) never treat a CI review as local work on the base branch; event construction merges the two via `ReviewJob.HookBranch()`. See `internal/daemon/hooks.go`.

## Worker Pool (`internal/daemon/worker.go`)

### Job processing lifecycle

1. Worker goroutine calls `db.ClaimJob(workerID)` — atomically sets status=running
2. Registers job for cancellation tracking (`registerRunningJob`)
3. Checks agent cooldown (quota exhaustion)
4. Builds prompt via `prompt.Builder` (or uses stored prompt for task/compact/fix jobs)
5. Gets agent via `agent.GetAvailableWithConfig()`, applies reasoning/model/agentic settings
6. For fix jobs: creates isolated worktree via `worktree.Create()`
7. Invokes `Agent.Review()` with streaming output capture
8. For fix jobs: captures patch via `worktree.CapturePatch()`
9. Stores result via `db.CompleteJob()` or `db.CompleteFixJob()`
10. Broadcasts `review.completed` event, fires hooks

### Retry and failover

- **Retries**: Up to 3 retries for transient failures (`db.RetryJob`). Resets status to queued.
- **Failover**: After retries exhausted (or on quota errors), switches to backup agent via `db.FailoverJob`. Resets retry_count, sets backup agent/model.
- **Cooldown**: Quota exhaustion errors (classified by `agent.ClassifyLimit` as `LimitKindQuota`) trigger per-agent cooldown (default 30 min, parsed from the error message via `agent.ParseResetDuration`/`ParseResetTime`). Cooldowns are tracked in-memory with RWMutex. `LimitKindSession` follows the same cooldown path, but no production rule emits it yet — a Claude session-cap rule is pending a captured error message.

### Workflow derivation for failover

`failoverWorkflow(job)` maps job kind to config workflow key:
- Fix jobs (`job.IsFixJob()`) → `"fix"`
- Non-default ReviewType (security, design) → that review type
- Everything else → `"review"`

Both `resolveBackupAgent` and `resolveBackupModel` use this shared helper.

### Cancellation

Race-safe 3-phase check: running map → DB lookup → pending cancels set. Test hooks (`testHookAfterSecondCheck`, `testHookCooldownLockUpgrade`) allow deterministic synchronization in tests.

## HTTP API (`internal/daemon/server.go`)

All endpoints prefixed with `/api/`:

| Endpoint | Method | Purpose |
|----------|--------|---------|
| `/enqueue` | POST | Queue review job |
| `/jobs` | GET | List/filter jobs (supports repo_id, status, branch, job_type filters) |
| `/job/cancel` | POST | Cancel running job |
| `/job/rerun` | POST | Re-enqueue done/failed job |
| `/job/output` | GET | Accumulated output lines |
| `/job/log` | GET | SSE stream of job output |
| `/job/fix` | POST | Enqueue fix job (parent_job_id, agentic) |
| `/job/patch` | POST | Get patch for completed fix job |
| `/job/applied` | POST | Mark fix patch as applied |
| `/job/rebased` | POST | Mark fix patch as rebased |
| `/repos` | GET | List registered repos |
| `/repos/register` | POST | Register repo by path |
| `/branches` | GET | List branches |
| `/review` | GET | Get review by job_id |
| `/review/close` | POST | Mark review closed |
| `/comment` | POST | Add comment to review |
| `/comments` | GET | List comments |
| `/jobs/batch` | POST | Enqueue batch (for CI) |
| `/status` | GET | Daemon status (version, queue stats, workers) |
| `/health` | GET | Health check (uptime, components, errors) |
| `/stream/events` | GET | SSE for real-time events |
| `/sync/now` | POST | Trigger immediate Postgres sync |
| `/sync/status` | GET | Sync status |
| `/activity` | GET | Activity log |
| `/remap` | POST | Remap job UUIDs after recovery |
| `/ping` | GET | Liveness identity (ok, service, version, pid) |
| `/shutdown` | POST | Graceful daemon shutdown (only graceful stop path on Windows) |

## CI Poller (`internal/daemon/ci_poller.go`)

Polls GitHub PRs at a configurable interval. For each open PR:
1. Finds registered local repo matching `owner/repo`
2. Fetches PR head ref, computes merge base
3. Creates CIPRBatch, enqueues jobs for configured agents x review types
4. Listens for `review.completed` events
5. Synthesizes results across agents (via `review.BuildSynthesisPrompt`)
6. Posts PR comment, sets commit status (pending/success/failure)

Configured via `[ci]` section: `enabled`, `github_repo`, `poll_interval`, `agents`, `review_types`, `min_severity`, etc.

## Prompt System (`internal/prompt/`)

**Builder methods**: `Build()` (single/range), `BuildDirty()` (uncommitted changes)

**System prompts**: Vary by review type (standard, security, design) and agent. Include bug/security/testing/regression/quality criteria.

**Context**: Includes recent reviews in repo, project guidelines from `.roborev.toml`, previous review attempts for same commit, developer responses.

**Max prompt size**: 250KB (configurable). Falls back to file listing if diff exceeds limit.

## Worktree System (`internal/worktree/`)

Used for fix jobs to run agents in isolated environments:
- `Create(repoPath, ref)` — Creates detached HEAD worktree, inits submodules
- `CapturePatch()` — `git add -A` + `git diff --cached`, returns patch string
- `ApplyPatch(repoPath, patch)` — `git apply --binary` from stdin
- `CheckPatch(repoPath, patch)` — Dry-run check, returns `PatchConflictError` on conflicts
- Suppresses hooks via `core.hooksPath=/dev/null`

## PostgreSQL Sync (`internal/storage/sync.go`, `syncworker.go`)

Cursor-based bidirectional sync:
- Jobs synced by ID cursor
- Reviews synced by (updated_at, id) compound cursor
- Responses synced by ID cursor
- Machine ID differentiates local vs remote jobs
- `ResolveRepoIdentity()` maps repos across machines (git remote URL or `.roborev-id` file)

## Commands

```bash
go build ./...                       # Build
go test ./...                        # Test (unit tests only)
go test -tags integration ./...      # Test (unit + integration)
go test -tags postgres ./...         # Test (requires TEST_POSTGRES_URL)
make install                         # Install to ~/.local/bin
make lint                            # golangci-lint with --fix
roborev init                         # Initialize in a repo
roborev status                       # Check daemon/queue
roborev daemon run                   # Start daemon in foreground
roborev tui                          # Terminal UI
```

## Testing

### Test tags

- No tag: unit tests only (default `go test ./...`)
- `//go:build integration`: Slow integration tests
- `//go:build postgres`: Requires `TEST_POSTGRES_URL`
- `//go:build acp`: ACP adapter tests (requires `ROBOREV_RUN_ACP_INTEGRATION=1`)

### Key test helpers

| Helper | Package | Purpose |
|--------|---------|---------|
| `testutil.NewTestRepo()` | testutil | Temp git repo with git init |
| `testutil.NewTestRepoWithCommit()` | testutil | Repo with initial commit |
| `testutil.InitTestRepo(t)` | testutil | Standard setup with base.txt |
| `testutil.GetHeadSHA(t, dir)` | testutil | Get HEAD SHA of a test repo |
| `openTestDB(t)` | storage | In-memory SQLite for tests |
| `createJobChain(t, db, path, sha)` | storage | Creates repo + commit + job |
| `createRepo(t, db, path)` | storage | Creates repo only |
| `createCommit(t, db, repoID, sha)` | storage | Creates commit only |
| `claimJob(t, db, workerID)` | storage | Claims next queued job |
| `newWorkerTestContext(t, n)` | daemon | Full worker pool test setup |
| `workerTestContext.createAndClaimJob()` | daemon | Enqueue + claim in one call |
| `workerTestContext.createAndClaimJobWithAgent()` | daemon | Same with custom agent |
| `workerTestContext.exhaustRetries()` | daemon | Exhaust retry count for failover tests |
| `NewStaticConfig(cfg)` | daemon | ConfigGetter that returns fixed config |

### Test conventions

- All tests use `t.TempDir()` for isolation
- `test` agent is always available (no PATH lookup)
- Worker pool test hooks enable deterministic synchronization
- Table-driven tests are preferred
- Use `testify` (`assert`/`require`) for all test assertions -- do not use raw `if`/`t.Errorf`/`t.Fatalf` patterns
- `require.*` for fatal preconditions (test cannot continue if this fails), `assert.*` for non-fatal checks
- In tests with more than three assertions, prefer `assert := assert.New(t)` shorthand

## Conventions

- **HTTP over gRPC**: Simple HTTP/JSON for the daemon API
- **No CGO in releases**: Build with `CGO_ENABLED=0` for static binaries (except sqlite which needs CGO locally)
- **Test agent**: Use `agent = "test"` for testing without calling real AI
- **Isolated tests**: All tests use `t.TempDir()` for temp directories

## Terminal Safety

Sanitize untrusted strings before TUI display: `stripControlChars()` for table cells, `sanitizeForDisplay()` for multi-line content, `sanitizeEscapes()` for streaming lines.

## Design Constraints

- **Daemon tasks must not edit tracked source or apply code changes in the user's git working tree.** Repo metadata is different: `roborev init` may update the usually tracked `.gitignore` so the configured `snapshot_dir` (default `.roborev/`) is ignored, and daemon review work may create disposable ignored snapshot artifacts there when oversized diffs must be handed to sandboxed agents. Runtime snapshot creation may also add a local `.git/info/exclude` fallback when an existing checkout is missing the ignore rule. Background jobs (reviews, CI polling, synthesis) may read source files and write results to the database. CLI commands like `roborev fix` run synchronously in the foreground and may modify files. Background `fix` jobs run agents in isolated git worktrees (via `internal/worktree`) and store resulting patches in the database; patches are only applied to the working tree when the user explicitly confirms in the TUI.

## Agent Skills

Skills in `internal/skills/` provide agent-specific instructions (Claude Code and Codex variants) for common workflows. Several skills mirror CLI commands (`refine`, `fix`, `review`).

**Keeping skills in sync with the CLI:** When modifying a skill that has a CLI counterpart (e.g., `roborev-refine` ↔ `cmd/roborev/refine.go`), check that flags, default values, and loop semantics still match. Key sync points:
- Default `--max-iterations` must match the CLI constant
- Review/fix/commit/re-review loop order must match
- CLI commands referenced in skills (`roborev wait`, `roborev review --branch --wait`, etc.) must use valid flags and syntax

When modifying a CLI command that has a skill counterpart, update the corresponding skills in both `internal/skills/claude/` and `internal/skills/codex/`.

## Pull Requests

When creating PRs, do NOT include a "Test plan" section. The PR body should contain only a Summary section with bullet points describing what changed.

## Style Preferences

- Keep it simple, no over-engineering
- Prefer stdlib over external dependencies
- Tests should be fast and isolated
- No emojis in code or output (except commit messages)
- Never amend commits; always create new commits for fixes
- Never push or pull unless explicitly asked by the user
- **NEVER merge pull requests.** Do not run `gh pr merge` or any equivalent. Only the user merges PRs. This is non-negotiable.
- **NEVER change git branches without explicit user confirmation**. Always ask before switching, creating, or checking out branches. This is non-negotiable.
