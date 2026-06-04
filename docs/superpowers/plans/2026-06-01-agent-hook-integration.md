# Agent Hook Integration Implementation Plan

> **For agentic workers:** REQUIRED: Use superpowers:subagent-driven-development (if subagents available) or superpowers:executing-plans to implement this plan. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship `roborev agent-hook` as an optional Codex/Claude harness integration with a separate local hook-state daemon.

**Architecture:** Move the current command-local prototype into `internal/agenthook`, keep `cmd/roborev` as thin Cobra glue, and preserve process isolation from the main roborev daemon. The local agent-hook daemon owns session counters only; the main roborev daemon remains the source of review/job truth.

**Tech Stack:** Go stdlib, Cobra, existing `internal/config`, `internal/daemon`, `internal/storage`, targeted process-launch env filtering, and roborev's existing `go.kenn.io/kit` dependency for daemon lifecycle and git subprocess helpers.

---

## Current Branch Baseline

Commit `92125941` already contains a working command-local prototype under `cmd/roborev/agent_hook_*.go`. Treat Tasks 1, 2, 4, and 5 as behavior-preserving refactors of working code, not greenfield implementation. Keep tests green during each move; reserve red/green TDD for genuinely new behavior such as native TOML config, latency budgets, runtime identity coverage, and parity gaps.

`roborev-hook` was a spike with no production users. Do not preserve its JSON config file or `ROBOREV_HOOK_*` environment names for compatibility.

## File Map

- Create: `internal/procutil/env.go` for shared process-launch helpers currently in `cmd/roborev/env.go`.
- Modify: `cmd/roborev/env.go` to delegate to `internal/procutil`.
- Create: `internal/agenthook/types.go` for payloads, responses, state structs, option structs.
- Create: `internal/agenthook/config.go` for defaults and option resolution.
- Create: `internal/agenthook/install.go` for Codex/Claude config generation, install, and dump.
- Create: `internal/agenthook/client.go` for local daemon discovery/start and hook posting.
- Create: `internal/agenthook/daemon.go` for local HTTP routes.
- Create: `internal/agenthook/state.go` for session counter and trigger behavior.
- Create: `internal/agenthook/git.go` for git signal helpers.
- Create: `internal/agenthook/roborev.go` for main roborev daemon queries.
- Create: `internal/agenthook/*_test.go` for package-level behavior tests.
- Modify: `cmd/roborev/agent_hook_cmd.go` to call `internal/agenthook`.
- Modify: `cmd/roborev/main.go` to keep command registration.
- Modify: `internal/config/config.go` to add `[agent_hook]` config.
- Modify: `cmd/roborev/agent_hook_test.go` to assert command-level behavior against the internal package.
- Modify: `README.md` for user docs.

## Task 1: Move Core Types And Output Mapping

**Files:**
- Create: `internal/agenthook/types.go`
- Create: `internal/agenthook/output.go`
- Create: `internal/agenthook/output_test.go`
- Modify: `cmd/roborev/agent_hook_cmd.go`

- [ ] **Step 1: Move existing output tests to the internal package**

Move/adapt the already-passing assertions from `cmd/roborev/agent_hook_test.go` into `internal/agenthook/output_test.go`:

```go
func TestBuildOutputBlocksStop(t *testing.T) {
	got := BuildOutput(Input{HookEventName: "Stop"}, Response{
		Triggered: true,
		Reason:    "Invoke $roborev-fix.",
	})
	require.Equal(t, "block", got["decision"])
	require.Equal(t, "Invoke $roborev-fix.", got["reason"])
}

func TestBuildOutputAddsPostToolUseContext(t *testing.T) {
	got := BuildOutput(Input{HookEventName: "PostToolUse"}, Response{
		Triggered: true,
		Reason:    "Invoke $roborev-fix.",
	})
	require.NotContains(t, got, "decision")
	specific, ok := got["hookSpecificOutput"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "PostToolUse", specific["hookEventName"])
	require.Equal(t, "Invoke $roborev-fix.", specific["additionalContext"])
}
```

Also add one test per supported harness that names the expected output contract. If Codex and Claude share the same JSON shape, make that explicit in the test names rather than assuming it from install config.

- [ ] **Step 2: Run baseline tests before moving implementation**

Run:

```bash
go test ./cmd/roborev -run 'TestBuildAgentHookOutput|TestRunAgentHookFailsOpenWhenDaemonUnavailable' -count=1
```

Expected: PASS.

- [ ] **Step 3: Move types and output builder**

Move hook payload/response/state option types from `cmd/roborev/agent_hook_types.go` into `internal/agenthook/types.go`. Move output mapping into `internal/agenthook/output.go`.

- [ ] **Step 4: Update Cobra glue**

Change `cmd/roborev/agent_hook_cmd.go` to use `agenthook.Input`, `agenthook.Request`, `agenthook.Response`, and `agenthook.BuildOutput`.

- [ ] **Step 5: Verify**

Run:

```bash
go test ./internal/agenthook -run TestBuildOutput -count=1
go test ./cmd/roborev -run 'TestBuildAgentHookOutput|TestRunAgentHookFailsOpenWhenDaemonUnavailable' -count=1
```

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/agenthook cmd/roborev/agent_hook_*.go
git commit -m "refactor: move agent hook output mapping internal"
```

## Task 2: Move Installer And Dump Logic

**Files:**
- Create: `internal/agenthook/install.go`
- Create: `internal/agenthook/install_test.go`
- Modify: `cmd/roborev/agent_hook_cmd.go`

- [ ] **Step 1: Keep command glue coverage and expand installer tests**

Keep a command-level Codex dump test in `cmd/roborev/agent_hook_test.go` so Cobra glue is exercised, then add package-level installer parity tests in `internal/agenthook/install_test.go`:

- `TestDumpCodexCreatesHookConfig`
- `TestDumpClaudePreservesExistingSettings`
- `TestInstallCodexIdempotent`
- `TestInstallPreservesSymlinkedConfig`
- `TestDumpUpdatesExistingTimeout`
- `TestDumpAddsHookUnderTargetMatcherEvenIfPresentElsewhere`

- [ ] **Step 2: Run baseline and parity tests**

Run:

```bash
go test ./cmd/roborev -run TestAgentHookDumpCodexCreatesHookConfig -count=1
go test ./internal/agenthook -run 'TestDump|TestInstall' -count=1
```

Expected: command-level glue test passes before moving code. New package-level parity tests may fail until the installer implementation is moved and exposed.

- [ ] **Step 3: Move install implementation**

Move config merge and write-through logic from `cmd/roborev/agent_hook_install.go` to `internal/agenthook/install.go`.

Expose:

```go
func Install(opts InstallOptions, stdout io.Writer) error
func Dump(opts DumpOptions, stdout io.Writer) error
func DefaultCodexHooksPath() string
func DefaultClaudeSettingsPath() string
func DefaultInstallCommand(exe string) string
```

- [ ] **Step 4: Keep Cobra thin**

`cmd/roborev/agent_hook_cmd.go` should bind flags and call `agenthook.Install` or `agenthook.Dump`.

- [ ] **Step 5: Verify**

Run:

```bash
go test ./internal/agenthook -run 'TestDump|TestInstall' -count=1
go test ./cmd/roborev -run TestAgentHookDumpCodexCreatesHookConfig -count=1
```

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/agenthook cmd/roborev/agent_hook_*.go
git commit -m "feat: add agent hook installer internals"
```

## Task 3: Add Native Roborev Config

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`
- Create: `internal/agenthook/config.go`
- Create: `internal/agenthook/config_test.go`

- [ ] **Step 1: Write failing config tests**

Add tests for:

- defaults when no config is present
- global `[agent_hook]` values
- env overriding global config
- flags overriding env
- negative thresholds rejected
- adjacent JSON config is ignored/removed
- `ROBOREV_HOOK_*` aliases are not recognized

- [ ] **Step 2: Run red tests**

Run: `go test ./internal/config ./internal/agenthook -run 'AgentHook|ResolveOptions' -count=1`

Expected: FAIL because config fields/resolution do not exist.

- [ ] **Step 3: Add config struct**

Add to `internal/config.Config`:

```go
AgentHook AgentHookConfig `toml:"agent_hook"`
```

Add:

```go
type AgentHookConfig struct {
	TurnThreshold         int    `toml:"turn_threshold"`
	CommitThreshold       int    `toml:"commit_threshold"`
	FailedReviewThreshold int    `toml:"failed_review_threshold"`
	Instruction           string `toml:"instruction"`
}
```

Do not add persistent TOML fields for `roborev_server_addr` or the agent-hook daemon address. Keep those as flags/env-only operational overrides.

- [ ] **Step 4: Implement option resolution**

In `internal/agenthook/config.go`, resolve:

```text
flags > env > config.AgentHook > defaults
```

Delete the adjacent JSON config loader from the prototype. Do not read it as a primary or fallback mechanism.

- [ ] **Step 5: Verify**

Run:

```bash
go test ./internal/config ./internal/agenthook -run 'AgentHook|ResolveOptions' -count=1
go test ./cmd/roborev -run TestAgentHook -count=1
```

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/config internal/agenthook cmd/roborev
git commit -m "feat: configure agent hook from roborev config"
```

## Task 4: Move Daemon, Client, And Runtime Logic

**Files:**
- Create: `internal/procutil/env.go`
- Create: `internal/procutil/env_test.go`
- Create: `internal/agenthook/client.go`
- Create: `internal/agenthook/daemon.go`
- Create: `internal/agenthook/daemon_test.go`
- Modify: `cmd/roborev/env.go`
- Modify: `cmd/roborev/agent_hook_cmd.go`

- [ ] **Step 1: Extract process launch helpers**

Move these helpers from `cmd/roborev/env.go` into `internal/procutil`:

- git repo-context env filtering
- `IsGoTestBinaryPath`
- `IsGoBuildCacheBinary`
- `ShouldRefuseAutoStartDaemon`

Keep wrapper functions in `cmd/roborev/env.go` only if needed to minimize unrelated churn in daemon lifecycle code.

- [ ] **Step 2: Verify process helper tests**

Run:

```bash
go test ./internal/procutil ./cmd/roborev -run 'Test.*GitEnv|Test.*GoBuild|Test.*AutoStart' -count=1
```

Expected: PASS. Existing daemon lifecycle tests should still compile.

- [ ] **Step 3: Add runtime isolation and latency tests**

Add tests that verify:

- runtime service is `roborev-agent-hook`
- runtime files live under `${ROBOREV_DATA_DIR}/agent-hook/runtime`
- probe rejects a `roborev` main daemon ping as the wrong service
- `RunHook` fails open when `PostHook` returns an error
- malformed hook JSON and missing `session_id` return hard CLI errors
- local daemon startup/probe/query use an explicit bounded timeout
- `PostToolUse` does not pay first-start daemon latency if the chosen policy is Stop-only auto-start

- [ ] **Step 4: Run new behavior tests**

Run: `go test ./internal/agenthook -run 'Runtime|Daemon|FailOpen' -count=1`

Expected: new tests for timeout/malformed payload may fail until the implementation is adjusted.

- [ ] **Step 5: Move daemon and client implementation**

Move local daemon HTTP routes and local client discovery/start into `internal/agenthook`.

Keep these rules:

- no main daemon runtime writes
- use roborev's existing kit helpers where they replace spike-local daemon or
  git code without regressing main-daemon launch behavior
- no production paths in tests
- no auto-start from Go test/go run cache binaries unless a test explicitly opts in
- no unbounded waits in the hook hot path

- [ ] **Step 6: Update Cobra commands**

`agent-hook run`, `agent-hook daemon`, `agent-hook status`, and `agent-hook reset` should call exported `internal/agenthook` functions.

- [ ] **Step 7: Verify**

Run:

```bash
go test ./internal/procutil -count=1
go test ./internal/agenthook -run 'Runtime|Daemon|FailOpen' -count=1
go test ./cmd/roborev -run TestRunAgentHookFailsOpenWhenDaemonUnavailable -count=1
```

Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/procutil internal/agenthook cmd/roborev
git commit -m "feat: isolate agent hook daemon internals"
```

## Task 5: Port Trigger Behavior

**Files:**
- Create: `internal/agenthook/state.go`
- Create: `internal/agenthook/git.go`
- Create: `internal/agenthook/roborev.go`
- Create: `internal/agenthook/state_test.go`
- Create: `internal/agenthook/git_test.go`
- Create: `internal/agenthook/roborev_test.go`

- [ ] **Step 1: Move and expand state tests**

Move existing trigger-adjacent command tests into `internal/agenthook`, then port/adapt additional tests for:

- unsupported events skip
- Stop increments counters
- `stop_hook_active` skips
- turn threshold does not trigger without failed reviews
- turn threshold triggers with failed reviews
- commit-producing Bash command increments commits
- non-Bash PostToolUse skips
- failed-review threshold triggers only on threshold-size count increases
- prompt counters reset after trigger
- reset/status persistence
- conservative `Verdict == "F"` counting under-counts jobs with empty/missing verdict

- [ ] **Step 2: Write failing git tests**

Add tests for `git commit`, `git cherry-pick`, `git revert`, `git -C`, and non-commit commands.

- [ ] **Step 3: Write failing roborev query tests**

Use `httptest.Server` and explicit endpoint config. Verify query params:

```text
repo
branch
branch_include_empty=true
status=done
closed=false
limit=10000
```

Add a separate test documenting the deliberate divergence from `roborev fix`: the hook counts only API jobs with `Verdict == "F"`, while `fix` may act on additional non-passing jobs after fetching full review content. This is acceptable for the hook because it is a conservative prompt trigger.

- [ ] **Step 4: Run red tests**

Run: `go test ./internal/agenthook -run 'State|Commit|FailedReview|Roborev' -count=1`

Expected: FAIL until behavior is moved/implemented.

- [ ] **Step 5: Move and adapt implementation**

Move state, git, and roborev query logic from `cmd/roborev/agent_hook_state.go` into focused internal files.

- [ ] **Step 6: Verify**

Run:

```bash
go test ./internal/agenthook -run 'State|Commit|FailedReview|Roborev' -count=1
go test ./cmd/roborev -run TestAgentHook -count=1
```

Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/agenthook cmd/roborev
git commit -m "feat: port agent hook trigger behavior"
```

## Task 6: Delete Command-Local Prototype Logic

**Files:**
- Modify/delete: `cmd/roborev/agent_hook_client.go`
- Modify/delete: `cmd/roborev/agent_hook_config.go`
- Modify/delete: `cmd/roborev/agent_hook_daemon.go`
- Modify/delete: `cmd/roborev/agent_hook_install.go`
- Modify/delete: `cmd/roborev/agent_hook_state.go`
- Modify/delete: `cmd/roborev/agent_hook_types.go`
- Keep: `cmd/roborev/agent_hook_cmd.go`
- Keep/adapt: `cmd/roborev/agent_hook_test.go`

- [ ] **Step 1: Verify command package has only Cobra glue**

Inspect: `ls cmd/roborev/agent_hook_*.go`

Expected after cleanup: only command/test glue remains in `cmd/roborev`.

- [ ] **Step 2: Run command tests**

Run: `go test ./cmd/roborev -run TestAgentHook -count=1`

Expected: PASS.

- [ ] **Step 3: Run internal package tests**

Run: `go test ./internal/agenthook -count=1`

Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add cmd/roborev internal/agenthook
git commit -m "refactor: keep agent hook command glue thin"
```

## Task 7: Add Documentation

**Files:**
- Modify: `README.md`
- Optionally create: `docs/agent-hook.md` if docs directory is acceptable for product docs.

- [ ] **Step 1: Add user docs**

Document:

- what `agent-hook` does
- how it differs from git post-commit hooks
- `install`, `dump`, `status`, `reset`
- Codex/Claude support
- fail-open behavior
- separate local hook daemon

- [ ] **Step 2: Verify docs mention optional setup**

Search:

```bash
rg -n "agent-hook" README.md docs/agent-hook.md
rg -n "Codex|Claude" README.md docs/agent-hook.md
rg -n "fail open|fails open" README.md docs/agent-hook.md
rg -n "agent-hook daemon|roborev-agent-hook" README.md docs/agent-hook.md
```

Expected: each required topic appears in user-facing docs, not only in the plan/spec files.

- [ ] **Step 3: Commit**

```bash
git add README.md docs
git commit -m "docs: document agent hook integration"
```

## Task 8: Final Verification

**Files:**
- All touched files.

- [ ] **Step 1: Format**

Run: `go fmt ./...`

Expected: no errors.

- [ ] **Step 2: Run full tests**

Run: `go test ./...`

Expected: PASS.

- [ ] **Step 3: Run vet**

Run: `go vet ./...`

Expected: PASS.

- [ ] **Step 4: Build**

Run: `go build ./...`

Expected: PASS.

- [ ] **Step 5: Manual CLI smoke**

Run:

```bash
tmp=$(mktemp -d)
ROBOREV_DATA_DIR="$tmp/roborev" CODEX_HOME="$tmp/codex" \
  go run ./cmd/roborev agent-hook dump --agent codex --config "$tmp/hooks.json"
```

Expected: printed JSON contains `PostToolUse`, `Stop`, and `agent-hook run`.

- [ ] **Step 6: Check dependency boundary**

Run:

```bash
go list -m all | rg 'go.kenn.io/kit'
```

Expected: existing `go.kenn.io/kit` module is present; no spike-only modules or
standalone `roborev-hook` compatibility dependencies are introduced.

- [ ] **Step 7: Commit final cleanup**

```bash
git status --short
git add .
git commit -m "test: verify agent hook integration"
```

Only commit if there are final cleanup changes.

## Notes For Implementers

- Do not run commands that target real `~/.roborev` during tests. Set `ROBOREV_DATA_DIR`, `HOME`, `USERPROFILE`, and `XDG_CONFIG_HOME` to temp dirs in tests and manual smoke checks.
- Do not use production daemon runtime files in tests. Use explicit `httptest` endpoints.
- Preserve fail-open behavior for `agent-hook run`.
- Preserve soft-fail behavior for main roborev daemon lookup.
- Keep the main roborev daemon process independent from the agent-hook daemon.
