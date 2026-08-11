# Drain Reviews Before Daemon Restart Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan directly in the current agent, task-by-task. Never use subagent-driven development. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make every daemon restart pause new job claims and wait indefinitely
for running jobs before stopping the daemon.

**Approved spec/design:**
`docs/superpowers/specs/2026-08-11-drain-reviews-before-daemon-restart-design.md`

**Architecture:** Add a CLI lifecycle coordinator that uses the existing status
and queue-pause HTTP APIs to drain work before invoking a restart-specific
replacement callback. Keep process replacement mechanics separate, and route
manual, automatic compatibility, and post-update restarts through the shared
coordinator.

**Tech Stack:** Go, Cobra, `net/http`, `context`, SQLite-backed daemon queue,
and `testify`.

## Global Constraints

- Restart waits have no overall timeout.
- Once draining begins, queued jobs must not transition to running.
- Unknown running state while a daemon is alive must never permit shutdown or
  force-kill.
- Waiting messages go to stderr and are never gated by verbose mode.
- Preserve the queue's pre-restart pause state.
- Do not change explicit `roborev daemon stop` behavior.
- Do not add a daemon API, storage migration, or compatibility shim.
- Never use `--no-verify` for commits.

## File Structure

- Create `cmd/roborev/restart_drain.go` for status/pause HTTP operations and
  the shared drain coordinator.
- Create `cmd/roborev/restart_drain_test.go` for coordinator behavior.
- Modify `cmd/roborev/pause.go` and `pause_test.go` to share the low-level queue
  request without changing pause-command output.
- Modify `cmd/roborev/daemon_lifecycle.go`, `daemon_cmd.go`, and their tests to
  separate safe restart policy from local process replacement.
- Modify `cmd/roborev/update.go` and `main_test.go` to drain before updater PID
  handoff and replacement.
- Modify `cmd/roborev/daemon_integration_test.go` for a controlled blocking-job
  drain test.
- Modify `docs/commands.md` and `docs/changelog.md` for user-facing behavior.

---

### Task 1: Shared restart drain coordinator

**Files:**

- Create: `cmd/roborev/restart_drain.go`
- Create: `cmd/roborev/restart_drain_test.go`
- Modify: `cmd/roborev/pause.go:38-81`
- Modify: `cmd/roborev/pause_test.go`

**Interfaces:**

- Produces: `type restartDrainTarget struct { Endpoint daemon.DaemonEndpoint; PID int }`.
- Produces: `func drainAndRestart(ctx context.Context, target restartDrainTarget, replace func() error) error`.
- Produces: `func requestQueuePaused(ctx context.Context, ep daemon.DaemonEndpoint, paused bool) (bool, error)`.
- Produces test seams `restartDrainPollInterval`, `restartDrainProcessExists`,
  `restartDrainStatus`, `restartDrainSetPaused`, `restartDrainCurrentEndpoint`,
  and `restartDrainStderr`.
- Consumes: `storage.DaemonStatus` and the existing `/api/status`,
  `/api/queue/pause`, and `/api/queue/unpause` contracts.

- [ ] **Step 1: Write failing coordinator tests**

Use call recording and controlled status sequences:

```go
func TestDrainAndRestartWaitsForRunningJobs(t *testing.T) {
    calls := []string{}
    statuses := []storage.DaemonStatus{
        {RunningJobs: 2, QueuePaused: false},
        {RunningJobs: 1, QueuePaused: true},
        {RunningJobs: 0, QueuePaused: true},
    }
    restartDrainStatus = func(context.Context, daemon.DaemonEndpoint) (storage.DaemonStatus, error) {
        status := statuses[0]
        statuses = statuses[1:]
        calls = append(calls, fmt.Sprintf("status:%d", status.RunningJobs))
        return status, nil
    }
    restartDrainSetPaused = func(_ context.Context, _ daemon.DaemonEndpoint, paused bool) (bool, error) {
        calls = append(calls, fmt.Sprintf("pause:%t", paused))
        return paused, nil
    }

    err := drainAndRestart(t.Context(), restartDrainTarget{}, func() error {
        calls = append(calls, "replace")
        return nil
    })

    require.NoError(t, err)
    assert.Equal(t, []string{
        "status:2", "pause:true", "status:1", "status:0", "replace", "pause:false",
    }, calls)
}
```

Add direct tests for initially paused queues, indefinite positive counts,
temporary status errors while the daemon is alive, independently dead daemons,
context cancellation, replacement errors, restoration errors, and progress
messages without per-poll spam.

- [ ] **Step 2: Run focused tests and verify RED**

```bash
go test ./cmd/roborev -run 'TestDrainAndRestart|TestRequestQueuePaused' -count=1
```

Expected: build failure because the coordinator and seams do not exist.

- [ ] **Step 3: Extract the context-aware queue helper**

```go
func requestQueuePaused(
    ctx context.Context, ep daemon.DaemonEndpoint, paused bool,
) (bool, error) {
    path := "/api/queue/unpause"
    if paused {
        path = "/api/queue/pause"
    }
    req, err := http.NewRequestWithContext(ctx, http.MethodPost, ep.BaseURL()+path, nil)
    if err != nil {
        return false, fmt.Errorf("build queue pause request: %w", err)
    }
    resp, err := ep.HTTPClient(2 * time.Second).Do(req)
    if err != nil {
        return false, fmt.Errorf("update queue pause state: %w", err)
    }
    defer resp.Body.Close()
    if resp.StatusCode != http.StatusOK {
        return false, fmt.Errorf("update queue pause state: daemon returned %s", resp.Status)
    }
    var result queuePauseResponse
    if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
        return false, fmt.Errorf("parse queue pause response: %w", err)
    }
    return result.QueuePaused, nil
}
```

Have `setQueuePaused` call this operation and use the returned state for its
existing exact human output, including malformed-response errors.

- [ ] **Step 4: Implement the drain state machine**

```go
var (
    restartDrainPollInterval = 200 * time.Millisecond
    restartDrainProcessExists = daemon.ProcessExists
    restartDrainStatus = fetchRestartDrainStatus
    restartDrainSetPaused = requestQueuePaused
    restartDrainCurrentEndpoint = getDaemonEndpoint
    restartDrainStderr io.Writer = os.Stderr
)

func drainAndRestart(
    ctx context.Context,
    target restartDrainTarget,
    replace func() error,
) (err error) {
    // Observe original pause state, pause, and poll until RunningJobs == 0.
    // Retry bounded status calls indefinitely unless a known PID has exited.
    // Restore only a pause introduced by this invocation.
    // Never call replace after context cancellation or unknown live state.
    return nil
}
```

Use a context-aware timer instead of `time.Sleep`. HTTP unresponsiveness is not
proof of exit. Proceed after lost status only when `target.PID > 0` and
`restartDrainProcessExists(target.PID)` is false; when no PID is known, keep
waiting. Emit progress only when the running count or availability state
changes. Join restoration failures to the primary cancellation or replacement
error with `errors.Join`. Resolve the current endpoint again after replacement
before restoring a temporary pause, because a process-manager handoff may
publish a different endpoint.

- [ ] **Step 5: Run focused tests and pause tests**

```bash
go test ./cmd/roborev -run 'TestDrainAndRestart|TestRequestQueuePaused|TestWriteLocalQueuePaused|TestSetQueuePaused' -count=1
```

Expected: PASS.

- [ ] **Step 6: Commit the coordinator**

```bash
git add cmd/roborev/restart_drain.go cmd/roborev/restart_drain_test.go cmd/roborev/pause.go cmd/roborev/pause_test.go
git commit -m "Drain running jobs before daemon replacement"
```

### Task 2: Manual and automatic restart wiring

**Files:**

- Modify: `cmd/roborev/daemon_lifecycle.go:48-58,226-285,438-470`
- Modify: `cmd/roborev/daemon_lifecycle_test.go:79-150`
- Modify: `cmd/roborev/daemon_cmd.go:55-75`
- Modify: `cmd/roborev/daemon_cmd_test.go`

**Interfaces:**

- Consumes: `drainAndRestart` from Task 1.
- Produces: `func restartDaemonContext(ctx context.Context, target restartDrainTarget) error`.
- Produces: `func discoverRestartTarget(ctx context.Context) (restartDrainTarget, bool, error)`, where the boolean reports confirmed daemon presence.
- Preserves: `func restartDaemon() error` for direct callers that must discover
  an endpoint.
- Produces: `func replaceDaemon() error` containing the existing stop, orphan
  cleanup, WAL checkpoint, and start mechanics.

- [ ] **Step 1: Write failing lifecycle wiring tests**

```go
func TestDaemonRestartUsesCommandContext(t *testing.T) {
    ctx, cancel := context.WithCancel(t.Context())
    cancel()
    cmd := daemonCmd()
    cmd.SetContext(ctx)
    cmd.SetArgs([]string{"restart"})

    err := cmd.Execute()

    require.ErrorIs(t, err, context.Canceled)
    assert.Zero(t, replaceCalls)
}
```

Retain the existing version-mismatch tests and explicit “started (was not
running)” versus “restarted” output cases. Assert that the running manual case
calls the safe wrapper rather than `stopDaemon` directly.

- [ ] **Step 2: Run lifecycle tests and verify RED**

```bash
go test ./cmd/roborev -run 'TestEnsureDaemon.*Restart|TestDaemonRestart' -count=1
```

Expected: failure because manual restart bypasses draining and
`restartDaemonContext` does not exist.

- [ ] **Step 3: Split safe policy from replacement mechanics**

```go
func restartDaemon() error {
    target, running, err := discoverRestartTarget(context.Background())
    if err != nil {
        return err
    }
    if !running {
        return startDaemon()
    }
    return restartDaemonContext(context.Background(), target)
}

func restartDaemonContext(
    ctx context.Context, target restartDrainTarget,
) error {
    return drainAndRestart(ctx, target, replaceDaemon)
}

func replaceDaemon() error {
    _ = stopDaemon()
    killAllDaemons()
    // Keep the existing WAL checkpoint loop here unchanged.
    return startDaemon()
}
```

Propagate access-denied or indeterminate discovery instead of replacing a
possibly live daemon.

- [ ] **Step 4: Route the explicit command through the safe wrapper**

```go
target, running, err := discoverRestartTarget(cmd.Context())
if err != nil {
    return err
}
if !running {
    if err := ensureDaemon(); err != nil {
        return err
    }
    fmt.Println("Daemon started (was not running)")
    return nil
}
if err := restartDaemonContext(cmd.Context(), target); err != nil {
    return err
}
fmt.Println("Daemon restarted")
return nil
```

`discoverRestartTarget` must fall back to the configured/default endpoint and
call the status API before declaring the daemon absent, so a manually launched
daemon without a runtime file is still restarted. Change
`restartDaemonForEnsure` to accept the already discovered `restartDrainTarget`
and pass the known runtime endpoint and PID at every automatic restart call.
This is required for probe-failure restarts: rediscovery may fail while the
original runtime target is still the only safe place to observe and drain
running work.

- [ ] **Step 5: Run focused lifecycle tests**

```bash
go test ./cmd/roborev -run 'TestEnsureDaemon|TestDaemonRestart|TestRestartDaemon' -count=1
```

Expected: PASS.

- [ ] **Step 6: Commit lifecycle wiring**

```bash
git add cmd/roborev/daemon_lifecycle.go cmd/roborev/daemon_lifecycle_test.go cmd/roborev/daemon_cmd.go cmd/roborev/daemon_cmd_test.go
git commit -m "Route daemon restarts through safe draining"
```

### Task 3: Post-update restart wiring

**Files:**

- Modify: `cmd/roborev/update.go:197-345,400-520`
- Modify: `cmd/roborev/main_test.go:886-1622`

**Interfaces:**

- Consumes: `drainAndRestart` from Task 1.
- Produces: `func restartDaemonAfterUpdate(ctx context.Context, binDir string, noRestart bool) error`.
- Produces: `func replaceDaemonAfterUpdate(binDir string) error` containing the
  existing PID handoff and updated-binary start flow.
- Produces: `func targetFromRunningInfoOrRuntime(info *daemon.RuntimeInfo) (restartDrainTarget, bool, error)` for the temporarily unresponsive runtime and confirmed-absence cases.

- [ ] **Step 1: Write the failing updater drain-order test**

```go
func TestRestartDaemonAfterUpdateDrainsBeforeStop(t *testing.T) {
    ctx, cancel := context.WithCancel(t.Context())
    restartDrainStatus = func(context.Context, daemon.DaemonEndpoint) (storage.DaemonStatus, error) {
        cancel()
        return storage.DaemonStatus{RunningJobs: 1}, nil
    }
    stopDaemonForUpdate = func() error {
        require.Fail(t, "stop must not run while a review is active")
        return nil
    }

    err := restartDaemonAfterUpdate(ctx, "/tmp/bin", false)

    require.ErrorIs(t, err, context.Canceled)
}
```

Update existing updater test calls to pass `t.Context()` and assert returned
errors.

- [ ] **Step 2: Run updater tests and verify RED**

```bash
go test ./cmd/roborev -run 'TestRestartDaemonAfterUpdate' -count=1
```

Expected: build failure because the function has no context or error result.

- [ ] **Step 3: Separate updater replacement from drain policy**

```go
func restartDaemonAfterUpdate(
    ctx context.Context, binDir string, noRestart bool,
) error {
    runningInfo, discoveryErr := getAnyRunningDaemon()
    target, running, err := targetFromRunningInfoOrRuntime(runningInfo)
    if err != nil {
        return err
    }
    if discoveryErr != nil && !running {
        return nil
    }
    if noRestart {
        fmt.Println("Skipping daemon restart (--no-restart)")
        return nil
    }
    return drainAndRestart(ctx, target, func() error {
        return replaceDaemonAfterUpdate(binDir)
    })
}
```

Move PID snapshots, stop, manager handoff, cleanup gates, updated-binary start,
readiness polling, and output into `replaceDaemonAfterUpdate`. Return errors for
failed replacement outcomes so the coordinator can restore pause state. The
runtime fallback must distinguish a confirmed empty runtime list from a
permission or I/O error, return a concrete endpoint and PID when discovery is
temporarily unresponsive, and never dereference a nil `runningInfo` merely
because a runtime PID exists.

- [ ] **Step 4: Pass the Cobra context from `updateCmd`**

```go
if err := restartDaemonAfterUpdate(cmd.Context(), binDir, noRestart); err != nil {
    return fmt.Errorf("restart daemon after update: %w", err)
}
repairHooksAfterUpdate(binDir, noRestart, nil)
```

Do not continue hook repair or updated-binary skill commands after a canceled
or failed restart.

- [ ] **Step 5: Run updater lifecycle tests**

```bash
go test ./cmd/roborev -run 'TestRestartDaemonAfterUpdate|TestUpdateCmd|TestRepairHooksAfterUpdate' -count=1
```

Expected: PASS, including existing manager-restart and lingering-runtime cases.

- [ ] **Step 6: Commit updater wiring**

```bash
git add cmd/roborev/update.go cmd/roborev/main_test.go
git commit -m "Drain reviews before post-update restart"
```

### Task 4: Integration coverage and documentation

**Files:**

- Modify: `cmd/roborev/daemon_integration_test.go`
- Modify: `docs/commands.md:808-845`
- Modify: `docs/changelog.md:8-11`

**Interfaces:**

- Consumes the production coordinator and worker queue-pause behavior.
- Produces no new production interface.

- [ ] **Step 1: Add a controlled drain integration test**

```go
func TestDaemonRestartDrainFinishesRunningJobBeforeQueuedJobStarts(t *testing.T) {
    started := make(chan int64, 2)
    releaseFirst := make(chan struct{})
    // Start the existing isolated in-process daemon with one worker and a test
    // agent that blocks job 1 on releaseFirst and records every invocation.
    // Enqueue job 1, begin drainAndRestart, then enqueue job 2 after pause is
    // observable. Verify job 2 has not started. Release job 1 and verify the
    // replacement callback runs only after job 1 is done.
}
```

Use bounded test-only `require.Eventually` checks so a broken test cannot hang.
Assert job states and callback ordering, not implementation text.

- [ ] **Step 2: Run the integration test**

```bash
go test -tags=integration ./cmd/roborev -run TestDaemonRestartDrainFinishesRunningJobBeforeQueuedJobStarts -count=1
```

Expected after connecting the harness to the production coordinator: PASS.

- [ ] **Step 3: Document the behavior**

Add near the daemon command table:

```markdown
All daemon restart paths pause queue processing before shutdown. If reviews are
running, roborev reports that it is waiting and lets them finish without a
timeout; queued reviews do not start during the drain. The replacement daemon
restores the queue's previous paused or unpaused state.
```

Add an Unreleased bug-fix bullet:

```markdown
- Daemon restarts now pause new work and wait for running reviews to finish,
  preventing manual, automatic version, and post-update restarts from
  interrupting active agents.
```

- [ ] **Step 4: Format and run focused verification**

```bash
make markdown
go test ./cmd/roborev -count=1
go test -tags=integration ./cmd/roborev -run TestDaemonRestartDrainFinishesRunningJobBeforeQueuedJobStarts -count=1
go test ./internal/daemon ./internal/storage -count=1
```

Expected: PASS.

- [ ] **Step 5: Commit integration coverage and docs**

```bash
git add cmd/roborev/daemon_integration_test.go docs/commands.md docs/changelog.md
git commit -m "Document non-interrupting daemon restarts"
```

### Task 5: Final verification and handoff

**Files:**

- Review all files changed by Tasks 1-4.

**Interfaces:**

- Consumes the complete implementation.
- Produces a verified and pushed branch.

- [ ] **Step 1: Run repository quality gates**

```bash
go test ./...
go test -tags=integration ./cmd/roborev -run TestDaemonRestartDrainFinishesRunningJobBeforeQueuedJobStarts -count=1
go build ./...
make lint-ci
make markdown-ci
```

Expected: all commands PASS.

- [ ] **Step 2: Review the final diff and restart call graph**

```bash
git status --short
git diff origin/main...HEAD
rg -n 'restartDaemon|replaceDaemon|stopDaemonForUpdate|killAllDaemonsForUpdate' cmd/roborev
```

Verify every manual, automatic compatibility, and updater restart reaches
`drainAndRestart` before stop or force-kill; explicit stop remains unchanged;
no unrelated files or private runtime data are present.

- [ ] **Step 3: Commit verification-driven corrections if needed**

Use the mandatory commit skill, stage only the correction files, and create a
new commit. Never amend. Do not create an empty commit when no correction is
needed.

- [ ] **Step 4: Rebase and push without changing branches**

```bash
git pull --rebase
git push
git status --short --branch
```

Expected: the current branch is up to date with its upstream and the working
tree is clean. Do not switch, create, or merge branches.
