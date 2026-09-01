# Agent Hook fix-session deduplication implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use
> superpowers:subagent-driven-development (recommended) or
> superpowers:executing-plans to implement this plan task-by-task. Steps use
> checkbox (`- [ ]`) syntax for tracking.

**Goal:** Allow only one Agent Hook session to receive a fix instruction for a
repository lineage until that session completes or its fixed 12-hour ownership
period expires.

**Architecture:** Store the current or most recently completed fix session in
the existing Agent Hook JSON snapshot, keyed by repository lineage. Reminder
delivery grants ownership under the state-store mutex, the owner releases it
through a completion endpoint, and later owner Stop events block with the exact
completion command.

**Tech Stack:** Go 1.27, standard `uuid` package, Cobra, Huma, JSON snapshot
persistence, Testify.

**Spec:**
`docs/superpowers/specs/2026-08-31-agent-hook-fix-session-deduplication-design.md`

## Global constraints

- Coordinate only Agent Hook-triggered work. Direct `roborev fix` and direct
  `roborev-fix` behavior must not change.
- Use the existing repository lineage key and state-store mutex.
- Ownership lasts exactly 12 hours from grant. Hook traffic never renews it.
- Expose a fix-session ID only after its snapshot replacement succeeds.
- Cursor must not acquire ownership because current Cursor hooks cannot receive
  Stop or PostToolUse control output.
- Deferred Hermes reminders acquire ownership only at Stop delivery.
- Use Testify and preserve isolated git-test setup.
- Add no database migration, configurable lease, pending claim, or compatibility
  shim.

---

### Task 1: Persisted fix-session state machine

**Files:**

- Create: `internal/agenthook/fix_session.go`
- Create: `internal/agenthook/fix_session_test.go`
- Modify: `internal/agenthook/types.go`
- Modify: `internal/agenthook/state.go:56-188`

**Interfaces:**

- Produces: `FixSession`, `FixSessionLifetime`, `ErrFixSessionNotFound`,
  `(*StateStore).FixSessions()`, `(*StateStore).CompleteFixSession(uuid.UUID)`,
  `tryGrantFixSession`, and `saveSessionAndFixSessionsLocked`.
- Consumes: `hookScope`, `Request`, `Snapshot`, and `saveLocked`.

- [ ] **Step 1: Write failing state-machine tests**

Create fixed-clock tests with UUID literals. The central grant test starts as:

```go
func TestTryGrantFixSessionBlocksActiveOwner(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	lineage := "/repo\x00main"
	fixSessions := map[string]FixSession{}

	first, granted := tryGrantFixSession(
		fixSessions,
		Request{Agent: "claude", Event: Input{SessionID: "session-a"}},
		hookScope{WorktreeRoot: "/worktree", Branch: "main"},
		lineage, now,
		uuid.MustParse("00000000-0000-4000-8000-000000000001"),
	)
	require.True(t, granted)
	assert.Equal(t, now.Add(12*time.Hour), first.ExpiresAt)

	_, granted = tryGrantFixSession(
		fixSessions,
		Request{Agent: "claude", Event: Input{SessionID: "session-b"}},
		hookScope{WorktreeRoot: "/worktree", Branch: "main"},
		lineage, now.Add(time.Hour), uuid.New(),
	)
	assert.False(t, granted)
}
```

Add cases for same-owner regrant rejection, exact expiry replacement,
non-renewal, completion, repeat completion, stale ID rejection, snapshot reload,
deep-copy status, session reset, reset-all, and save rollback.

- [ ] **Step 2: Run the tests and confirm missing symbols fail**

```bash
go test ./internal/agenthook -run 'Test(TryGrantFixSession|CompleteFixSession|FixSessions|LoadState.*Fix|StateStoreReset.*Fix)' -count=1
```

Expected: compile failure because the fix-session types do not exist.

- [ ] **Step 3: Add repository-owned UUID contracts and state**

Implement:

```go
const FixSessionLifetime = 12 * time.Hour

var ErrFixSessionNotFound = errors.New("agent hook fix session not found")

type FixSession struct {
	ID           uuid.UUID `json:"id"`
	Agent        string    `json:"agent"`
	SessionID    string    `json:"session_id"`
	WorktreeRoot string    `json:"worktree_root"`
	Branch       string    `json:"branch,omitempty"`
	StartedAt    time.Time `json:"started_at"`
	ExpiresAt    time.Time `json:"expires_at"`
	CompletedAt  time.Time `json:"completed_at,omitzero"`
}

func (f FixSession) Active(now time.Time) bool {
	return f.CompletedAt.IsZero() && now.Before(f.ExpiresAt)
}
```

Add `Agent string` to `Request`, `*uuid.UUID FixSessionID` to `Response`,
`FixSessions map[string]FixSession` to `Snapshot`, and `fixSessions` plus a test
clock to `StateStore`. Initialize absent maps in `LoadState`.

- [ ] **Step 4: Implement atomic persistence and completion**

Add:

```go
func (s *StateStore) saveSessionAndFixSessionsLocked(
	sessionID string,
	state SessionState,
	fixSessions map[string]FixSession,
) error
```

Replace both maps, call `saveLocked`, and restore both previous maps on error.
Make `saveSessionLocked` delegate to it. Implement `tryGrantFixSession` against a
cloned map. Implement `FixSessions`, `CompleteFixSession`, and reset behavior
under `s.mu`. Completion matches the exact UUID, records `CompletedAt`, and
returns an existing completed entry for a repeat call.

- [ ] **Step 5: Run state-machine tests**

```bash
go test ./internal/agenthook -run 'Test(TryGrantFixSession|CompleteFixSession|FixSessions|LoadState.*Fix|StateStoreReset.*Fix)' -count=1
```

Expected: PASS.

- [ ] **Step 6: Commit the state machine**

Stage only Task 1 files. Commit with a body explaining why reminder ownership
must persist in the same snapshot as session counters.

### Task 2: Atomic delivery grants and owner Stop closeout

**Files:**

- Modify: `internal/agenthook/state.go:207-327`
- Modify: `internal/agenthook/state.go:389-606`
- Modify: `internal/agenthook/state.go:738-890`
- Modify: `internal/agenthook/state_test.go`

**Interfaces:**

- Consumes: Task 1 grant, active-owner, and atomic-save functions.
- Produces: one owner across all delivery paths and
  `TriggeredBy == "fix_session"` owner Stop responses.

- [ ] **Step 1: Write failing concurrent delivery tests**

Synchronize two session requests in the fake review source. Cover Stop,
failed-review, and commit triggers. Each case asserts:

```go
assert.Equal(t, 1, triggeredCount)
assert.Len(t, store.FixSessions(), 1)
assert.NotNil(t, triggeredResponse.FixSessionID)
```

Add cases for different lineages, blocked-session retry eligibility, Cursor
without ownership, deferred Hermes ownership only at Stop, and snapshot failure
without a grant.

- [ ] **Step 2: Write failing owner Stop tests**

After a grant, send Stop from the same agent and session. Assert the response
uses `TriggeredBy: "fix_session"`, the same UUID, and the exact `fix-done`
command. Send `StopHookActive: true` and assert it remains skipped.

- [ ] **Step 3: Run tests and observe duplicate delivery**

```bash
go test ./internal/agenthook -run 'TestRecord.*FixSession|TestDeferred.*FixSession|TestOwnerStop.*FixSession' -count=1
```

Expected: FAIL because delivery paths do not grant ownership.

- [ ] **Step 4: Integrate grants at each delivery boundary**

For direct Stop and non-deferred PostToolUse, clone fix-session state and grant
only after finding a trigger candidate. For a blocked candidate, do not
acknowledge review IDs or reset Stop/commit progress. Remove its lineage from
`FailedReviewTriggeredCounts` so a later event can retry. Persist ordinary
session progress without emitting an instruction.

Keep deferred PostToolUse as queue-only. Grant inside the existing final locked
section of `deliverPendingReminder` before deleting or acknowledging the queued
candidate. Skip grants for `Request.Agent == "cursor"`.

- [ ] **Step 5: Add owner Stop closeout**

After the recursive Stop guard and scope resolution, use the resolved lineage to
return this shape for the active matching `{agent, session_id}` owner without
changing expiry. This keeps one harness session's owners distinct when it works
in multiple repositories:

```go
Response{
	SessionID: req.Event.SessionID,
	Triggered: true,
	TriggeredBy: "fix_session",
	FixSessionID: new(fixSession.ID),
	Reason: ownerStopReason(fixSession),
}
```

- [ ] **Step 6: Run focused and race tests**

```bash
go test ./internal/agenthook -run 'TestRecord.*FixSession|TestDeferred.*FixSession|TestOwnerStop.*FixSession' -count=1
go test -race ./internal/agenthook -run 'TestRecord.*FixSession' -count=1
```

Expected: PASS with exactly one same-lineage grant.

- [ ] **Step 7: Commit delivery coordination**

Stage Task 2 files. Commit with a body naming the overlapping Agent Hook edit
problem and the atomic delivery boundary.

### Task 3: Completion API, CLI, and native output

**Files:**

- Modify: `internal/daemon/types.go:407-436`
- Modify: `internal/daemon/routes.go:447-460`
- Modify: `internal/daemon/agent_hook.go:90-150`
- Modify: `internal/daemon/server_agent_hook_test.go`
- Modify: `cmd/roborev/agent_hook_cmd.go`
- Modify: `cmd/roborev/agent_hook_client.go`
- Modify: `cmd/roborev/agent_hook_handler.go`
- Modify: `cmd/roborev/agent_hook_test.go`
- Modify: `internal/agenthook/output.go`
- Modify: `internal/agenthook/output_test.go`

**Interfaces:**

- Consumes: `CompleteFixSession`, `FixSessions`, and response fix-session IDs.
- Produces: `POST /api/agent-hook/fix-done`, the matching Cobra command,
  expanded status JSON, profile propagation, and completion text.

- [ ] **Step 1: Write failing daemon tests**

Test status with `fix_sessions`, successful completion, repeated completion,
unknown UUID as 404, and zero UUID as 400. Use:

```go
type AgentHookFixDoneRequest struct {
	FixSessionID uuid.UUID `json:"fix_session_id"`
}

type AgentHookFixDoneOutput struct {
	Body struct {
		FixSession agenthook.FixSession `json:"fix_session"`
	}
}
```

- [ ] **Step 2: Write failing CLI and output tests**

Assert `runAgentHookFixDone` posts the typed UUID and prints one completion
line. Reject malformed UUIDs before HTTP. Assert normal typed output, Grok,
legacy, and deferred Hermes output contain exactly the matching command. Assert
owner closeout contains it once without guidelines or skill warnings. Cover a
hook configured with an explicit daemon address and verify both the emitted
command and completion request preserve that address.

- [ ] **Step 3: Run tests and confirm missing routes fail**

```bash
go test ./internal/daemon ./cmd/roborev ./internal/agenthook -run 'TestAgentHook.*(FixDone|FixSession|Completion|Output)' -count=1
```

Expected: compile or route failures.

- [ ] **Step 4: Implement the daemon API**

Add fix sessions to status. Register `/api/agent-hook/fix-done` on the private
Agent Hook route set. Reject `uuid.Nil()`, return 404 for
`ErrFixSessionNotFound`, and return 500 for persistence errors.

- [ ] **Step 5: Propagate profile and append lifecycle text**

Set `Request.Agent` in typed, Grok, and legacy paths. Append the completion
paragraph outside custom/default instructions, before guidelines and skill
warnings. Skip the append for owner closeout because its reason already names
the command. Preserve empty Cursor output and deferred Hermes delivery.

- [ ] **Step 6: Implement the Cobra command**

Add:

```go
func agentHookFixDoneCmd() *cobra.Command
func runAgentHookFixDone(ctx context.Context, rawID, serverAddr string, stdout io.Writer) error
```

Parse with `uuid.Parse`, ensure the runtime-discovered daemon when no address is
specified, post the typed request, and print the completed UUID. Accept
`--roborev-server` so an emitted completion command can target the same daemon
as the triggering hook. Take exactly one argument and do not inspect the current
repository.

- [ ] **Step 7: Run focused tests**

```bash
go test ./internal/daemon ./cmd/roborev ./internal/agenthook -run 'TestAgentHook.*(FixDone|FixSession|Completion|Output)' -count=1
```

Expected: PASS.

- [ ] **Step 8: Commit the completion protocol**

Stage Task 3 files. Commit with a body explaining immediate release and the
separate owner Stop closeout response.

### Task 4: Hook skill closeout and user documentation

**Files:**

- Modify: `internal/skills/claude/roborev-fix/SKILL.md`
- Modify: `internal/skills/codex/roborev-fix/SKILL.md`
- Modify: `internal/skills/droid/roborev-fix/SKILL.md`
- Modify: `internal/skills/grok/roborev-fix/SKILL.md`
- Modify: `docs/agent-hook.md`

**Interfaces:**

- Consumes: the exact completion command emitted by Task 3.
- Produces: hook-only skill completion and public lifecycle documentation.

- [ ] **Step 1: Add the final hook-only skill step**

Add this section to all four skill variants after the review audit:

```markdown
### Complete an Agent Hook fix session

When the current invocation came directly from Agent Hook and its instruction
contains an exact `roborev agent-hook fix-done <fix-session-id>` command, run
that exact command after the original review audit. Run it even when no code
changed or a valid out-of-scope finding remains open. Do not invent or discover
a fix-session ID. Skip this step for direct user invocations.
```

- [ ] **Step 2: Document the lifecycle**

Update `docs/agent-hook.md` with single-lineage ownership, silent duplicate
suppression, immediate completion, owner Stop closeout, fixed non-renewing
12-hour expiry, status/reset behavior, and the direct-fix exclusion.

- [ ] **Step 3: Format and test skills and docs**

```bash
make markdown
make markdown-ci
go test ./internal/skills ./internal/agenthook ./cmd/roborev -count=1
```

Expected: PASS with no unrelated Markdown changes.

- [ ] **Step 4: Commit skills and docs**

Stage the four skills and `docs/agent-hook.md`. Commit with a body explaining
how hook-invoked agents close persisted ownership.

### Task 5: Full verification and scope review

**Files:**

- Review: every file changed since `origin/main`

**Interfaces:**

- Consumes: Tasks 1 through 4.
- Produces: fresh behavioral, race, build, lint, formatting, and scope evidence.

- [ ] **Step 1: Run focused packages**

```bash
go test ./internal/agenthook ./internal/daemon ./cmd/roborev ./internal/skills -count=1
```

- [ ] **Step 2: Run race tests**

```bash
go test -race ./internal/agenthook ./internal/daemon ./cmd/roborev -count=1
```

- [ ] **Step 3: Run repository checks**

```bash
go test ./... -count=1
go build ./...
make markdown-ci
make lint-ci
```

- [ ] **Step 4: Review scope and public data**

Inspect `git diff origin/main...HEAD`, `git status --short`, and the configured
private-data denylist against new tests, docs, skills, and commit messages.
Confirm the branch changes only hook-triggered coordination.

- [ ] **Step 5: Commit corrections only when needed**

If verification requires an in-scope correction, commit it separately after
rerunning the focused check. Otherwise create no empty commit.
