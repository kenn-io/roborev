# Zero-Output Comment Gate Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Prevent daemon-free CI from publishing comments when every reviewer produced zero output, keep daemon operational notices free of raw errors, and route deterministic Codex startup failures through backup failover without changing existing quota, session, or transient behavior.

**Architecture:** Put the publication invariant in `internal/review` and call it at both forge-posting boundaries. Represent pre-protocol agent failures with an in-memory typed wrapper, classify existing provider/session signals before unavailable failures, and persist only stable category prefixes. Keep daemon panel outcomes and status transitions unchanged.

**Tech Stack:** Go, Cobra command handlers, SQLite-backed daemon workers, `testify`, GitHub/GitLab HTTP stubs.

---

### Task 1: Share the substantive-output invariant and gate daemon-free posting

**Files:**
- Modify: `internal/review/result.go`
- Modify: `internal/review/result_test.go`
- Modify: `internal/daemon/panel_outcome.go`
- Modify: `internal/daemon/panel_outcome_test.go`
- Modify: `cmd/roborev/ci.go`
- Modify: `cmd/roborev/ci_test.go`

**Step 1: Write failing behavior tests**

Add table tests for `review.HasSubstantiveOutput` covering done/nonempty, done/whitespace, failed/nonempty, skipped/nonempty, and empty batches. Add a GitLab-stub test around a small `postCIReviewComment` helper that proves an all-failed batch makes zero note requests while a partial-success batch makes one.

**Step 2: Run the focused tests and confirm the red state**

Run: `go test ./internal/review ./internal/daemon ./cmd/roborev -run 'TestHasSubstantiveOutput|TestClassifyPanelOutcome|TestPostCIReviewComment'`

Expected: compile failures because the shared helper and gated posting helper do not exist.

**Step 3: Implement the minimum gate**

Add:

```go
func HasSubstantiveOutput(results []ReviewResult) bool {
	for _, result := range results {
		if result.Status == ResultDone && strings.TrimSpace(result.Output) != "" {
			return true
		}
	}
	return false
}
```

Replace the daemon-local `hasReviewOutput` with the shared helper. Route the daemon-free forge call through a helper that returns without calling `postForgeComment` when `HasSubstantiveOutput(results)` is false. Keep stdout output and `ErrAllFailed` unchanged.

**Step 4: Run the focused tests and commit**

Run: `go test ./internal/review ./internal/daemon ./cmd/roborev -run 'TestHasSubstantiveOutput|TestClassifyPanelOutcome|TestPostCIReviewComment'`

Commit with a message that explains why zero-output batches cannot reach a forge API.

### Task 2: Remove raw errors from fixed daemon notices

**Files:**
- Modify: `internal/review/synthesis.go`
- Modify: `internal/review/synthesis_test.go`
- Modify: `internal/daemon/ci_poller.go`
- Modify: `internal/daemon/panel_posting_test.go`

**Step 1: Tighten the existing tests**

Change formatter tests to pass only `headSHA` and assert that synthetic raw stderr is absent. Extend genuine and transient panel give-up tests to seed distinctive multiline errors, then assert the fixed notice, commit status, and `giveup_posted` outcome remain unchanged while the raw text is absent.

**Step 2: Run the tests and confirm they fail to compile**

Run: `go test ./internal/review ./internal/daemon -run 'TestGiveUpAndSoftNoteComments|TestPostPanelRunGenuineGiveUp|TestPostPanelRunTransientGiveUp'`

Expected: formatter signature mismatches until production removes the excerpt parameters.

**Step 3: Remove excerpt rendering**

Change both formatters to accept only `headSHA`, delete the now-unused `oneLineExcerpt`, and update the two daemon callers. Do not change status values, descriptions, retry limits, or terminal outcomes.

**Step 4: Run the focused tests and commit**

Run: `go test ./internal/review ./internal/daemon -run 'TestGiveUpAndSoftNoteComments|TestPostPanelRunGenuineGiveUp|TestPostPanelRunTransientGiveUp'`

Commit with a message that explains why operational notices use fixed language.

### Task 3: Type Codex failures that occur before valid protocol output

**Files:**
- Create: `internal/agent/unavailable.go`
- Create: `internal/agent/unavailable_test.go`
- Modify: `internal/agent/codex.go`
- Modify: `internal/agent/codex_test.go`

**Step 1: Write typed-error and Codex regression tests**

Test that `agent.MarkUnavailable` preserves `errors.Is`, avoids double wrapping, and is detected by `agent.IsUnavailable`. Update Codex tests so propagated dangerous/noninteractive capability-probe errors, command startup errors, and no-valid-JSON exits are unavailable. Keep the existing `codexSupportsIgnoreUserConfig` rejection test and assert it still returns `(false, nil)`.

**Step 2: Run the focused tests and confirm the red state**

Run: `go test ./internal/agent -run 'TestUnavailable|TestCodex.*(Probe|Command|JSON|IgnoreUserConfig)'`

Expected: compile failures because the typed wrapper API does not exist, followed by assertion failures until Codex marks the specified paths.

**Step 3: Implement the typed wrapper and mark only propagated failures**

Add an unexported error type with `Error` and `Unwrap`, plus documented `MarkUnavailable(error) error` and `IsUnavailable(error) bool` functions. In `CodexAgent.Review`, wrap propagated dangerous/noninteractive probe errors and `runStreamingCLI` startup errors. Wrap wait/parse errors only when `errNoCodexJSON` proves that no valid protocol event was parsed. Preserve the `codexSupportsIgnoreUserConfig` compatibility fallback exactly.

**Step 4: Run the focused tests and commit**

Run: `go test ./internal/agent -run 'TestUnavailable|TestCodex.*(Probe|Command|JSON|IgnoreUserConfig)'`

Commit with a message that explains why callers need a stable pre-protocol category.

### Task 4: Preserve classification precedence and stable batch prefixes

**Files:**
- Modify: `internal/review/result.go`
- Modify: `internal/review/result_test.go`
- Modify: `internal/review/failclass_test.go`
- Modify: `internal/review/batch.go`
- Modify: `internal/review/batch_test.go`

**Step 1: Write failing classification tests**

Add `UnavailableErrorPrefix` behavior tests and a table for the daemon-free batch error formatter:

- typed unknown startup failure becomes `unavailable:`;
- typed `503 Service Unavailable` remains `outage:`;
- typed quota exhaustion remains `quota:`;
- typed session-limit text remains `outage:`;
- an ordinary unknown failure remains unprefixed.

Assert a persisted `unavailable:` result remains genuine.

**Step 2: Run the focused tests and confirm the red state**

Run: `go test ./internal/review -run 'TestUnavailable|TestFormatBatchAgentError|TestIsGenuineFailure'`

Expected: compile failures because the prefix and formatter do not exist.

**Step 3: Implement prefixing with existing classifiers first**

Add an idempotent `UnavailableError` formatter beside `OutageError`. In `runSingle`, pass agent review errors through a helper that calls `agent.ClassifyLimit` on the complete rendered error first. Map quota to `quota:`, session/transient to `outage:`, and only map `LimitKindNone` plus `agent.IsUnavailable(err)` to `unavailable:`.

**Step 4: Run the focused tests and commit**

Run: `go test ./internal/review -run 'TestUnavailable|TestFormatBatchAgentError|TestIsGenuineFailure'`

Commit with a message that explains why established provider signals take precedence.

### Task 5: Route unavailable daemon jobs through immediate backup failover

**Files:**
- Modify: `internal/daemon/worker.go`
- Modify: `internal/daemon/worker_test.go`

**Step 1: Write worker transition tests**

Exercise the typed agent-error entry point with claimed jobs and assert:

- an unknown unavailable error with no backup fails immediately, keeps retry count at zero, and stores `unavailable:` plus the complete diagnostic;
- a configured distinct non-cooling backup requeues immediately on that backup;
- a cooling backup is not selected;
- typed `503`, quota, and session errors retain their existing retry/cooldown/prefix behavior instead of entering the unavailable path.

**Step 2: Run the focused tests and confirm the red state**

Run: `go test ./internal/daemon -run 'TestUnavailableAgentError|TestUnavailableAgentErrorPreservesLimitClassification'`

Expected: compile failures because the typed worker entry point does not exist.

**Step 3: Add the typed worker entry point**

Keep existing string-based helpers for current callers and tests. Add a helper used by `processJob` that renders the full error, calls `wp.classify` first, and sends only `LimitKindNone` plus `agent.IsUnavailable(err)` to `failoverOrFailNonRetryableAgentContext` with `review.UnavailableError`. Send every other case through the existing `failOrRetryAgentContext` path.

**Step 4: Run the focused tests and commit**

Run: `go test ./internal/daemon -run 'TestUnavailableAgentError|TestUnavailableAgentErrorPreservesLimitClassification'`

Commit with a message that explains why deterministic startup failures skip same-agent retries.

### Task 6: Document and verify the completed behavior

**Files:**
- Modify: `docs/commands.md`
- Modify: `docs/integrations/github.md`

**Step 1: Update user-facing behavior**

Document that `ci review --comment` returns nonzero but makes no forge request when no agent produced substantive output. Document that daemon give-up notices retain fixed category language while detailed errors remain local.

**Step 2: Run focused and full verification**

Run:

```sh
go test ./internal/agent ./internal/review ./internal/daemon ./cmd/roborev
go test ./...
golangci-lint run
```

Run repository hooks on the intended staged files, review `git diff --check`, and inspect every commit and final diff against the design. Do not run or install the branch binary.

**Step 3: Commit documentation and verification adjustments**

Commit any documentation or verification-driven fixes with a message focused on the operator-visible contract.

**Step 4: Prepare the branch for review**

Remove the temporary `docs/superpowers/specs` and `docs/superpowers/plans` artifacts as required by repository policy, commit that cleanup, scrub all outgoing public surfaces for private data, and push the feature branch. Do not merge.
