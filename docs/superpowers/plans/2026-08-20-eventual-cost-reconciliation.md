# Eventual Cost Reconciliation Implementation Plan

> **For Codex:** Implement each task test-first and commit each completed piece.

**Goal:** Make missing job prices recover automatically after delayed usage
indexing, repair all eligible terminal statuses during manual backfill, and
document the eventual behavior.

**Architecture:** SQLite remains the durable source of retry work. A minimal,
paged storage query finds unique-session terminal jobs without recorded prices.
One worker-pool goroutine schedules short retries for new misses and walks the
persistent candidates at a bounded rate, merging late prices through the
existing session-scoped backfill write.

**Tech Stack:** Go, SQLite, Cobra, testify.

---

## Task 1: Select durable reconciliation candidates

**Files:**

- Create: `internal/storage/token_cost.go`
- Create: `internal/storage/token_cost_test.go`
- Modify: `internal/backfill/tokens.go`
- Modify: `internal/backfill/tokens_test.go`
- Modify: `cmd/roborev/backfill_tokens.go`
- Modify: `cmd/roborev/backfill_tokens_test.go`

1. Add failing storage tests that seed terminal jobs across all statuses and
   assert that the paged query returns only agent-run, unique-session jobs that
   lack a recorded dollar amount. Cover exclusive cursor ordering and page
   limits.
2. Run `go test ./internal/storage -run TokenCostCandidate -count=1` and confirm
   the new tests fail for the missing API.
3. Add `TokenCostCandidate`, `ListTokenCostCandidates`, and
   `GetTokenCostCandidate`. Reuse the existing cost eligibility and recorded
   cost SQL predicates.
4. Run the focused storage tests and confirm they pass.
5. Add failing backfill tests showing failed, canceled, and skipped terminal
   jobs with recoverable log/session data are candidates while queued and
   running jobs are not.
6. Replace the viewable-output check with a terminal-backfill eligibility
   helper. Change the command's provider lookup set to the storage query so its
   agent-run and uniqueness rules match the daemon.
7. Run `go test ./internal/backfill ./cmd/roborev -run Backfill -count=1` and
   confirm it passes.
8. Review the diff and commit this task.

## Task 2: Reconcile delayed prices in the daemon

**Files:**

- Create: `internal/daemon/token_cost_reconciler.go`
- Create: `internal/daemon/token_cost_reconciler_test.go`
- Modify: `internal/daemon/worker.go`

1. Add failing behavioral tests for:
   - a newly completed job whose price appears after immediate capture;
   - a persisted missing-price job discovered without a completion signal;
   - forward progress when an earlier candidate remains unavailable; and
   - stopping the worker pool while reconciliation is active.
2. Run
   `go test ./internal/daemon -run 'TokenCostReconcil|DelayedTokenCost' -count=1`
   and confirm the new tests fail.
3. Add one buffered completion channel, bounded scheduling state, configurable
   test intervals, and one reconciler goroutine to `WorkerPool` lifecycle.
4. Implement short retry scheduling plus the paged persistent scan. Provider
   calls use the existing cost configuration and are canceled by pool shutdown.
5. Merge only provider responses carrying a recorded price and save them with
   `BackfillJobTokenUsage`.
6. Signal the reconciler when immediate fresh-session capture ends without a
   recorded price.
7. Run the focused daemon tests and existing token-capture tests.
8. Review the diff and commit this task.

## Task 3: Document eventual pricing behavior

**Files:**

- Modify: `docs/configuration.md`
- Modify: `docs/commands.md`

1. Explain that the daemon retries delayed cost lookups in the background and
   that missing/deleted or reused sessions can remain unresolved.
2. Document that `backfill-tokens` scans all eligible terminal jobs, including
   failed and canceled jobs where an agent ran.
3. Run `make markdown-ci`.
4. Review the diff and commit this task.

## Task 4: Verify the roborev branch

1. Run focused package tests:
   `go test ./internal/storage ./internal/backfill ./internal/daemon ./cmd/roborev`.
2. Run `go test ./...` with a timeout of at least ten minutes.
3. Run `go build ./...`.
4. Run `make lint-ci` and `make markdown-ci`.
5. Inspect `git diff origin/main...HEAD`, `git status`, and the commit list for
   unintended or private content.

## Task 5: Update the operations dashboard

**Files in the operations repository:**

- Modify: `deploy/grafana/provisioning/dashboards/json/roborev-ci-metrics.json`

1. In an approved isolated operations worktree based on its main branch, change
   the dashboard timezone from browser-local time to UTC.
2. Run the existing dashboard JSON validation and the documented Grafana checks.
3. Review and commit the operations change without including private incident
   details.

## Task 6: Deploy, backfill, and verify

1. Push reviewed commits required by the deployment workflows.
2. Create a production database backup through the administrative interface and
   record the recovery point privately.
3. Deploy the exact verified roborev commit through the operations workflow.
4. Run `backfill-tokens` against the production data directory.
5. Replay cost metrics for the full affected reporting window.
6. Deploy the UTC dashboard change.
7. Verify daemon health, price coverage, UTC dashboard configuration, and
   downstream metrics. Compare eligible and priced counts before and after.
8. Remove the temporary superpowers design/plan documents before any pull
   request, run final checks, commit the cleanup, and push all final commits.
