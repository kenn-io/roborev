# Controlled Update Restarts Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `roborev update` choose a running-review policy before installation, safely drain or interrupt work under a renewable daemon lease, verify the replacement daemon version, and present compact phase-oriented output.

**Architecture:** Keep the persisted `shutdown_draining` key as the single atomic claim gate, while a new in-memory daemon coordinator distinguishes update-lease ownership from normal shutdown ownership. The worker pool owns update-specific cancellation and attempt-scoped requeueing; the CLI owns policy selection, lease renewal, installation, shutdown handoff, readiness/version verification, and user-facing phases. No schema, updater-side operation file, data-directory lock, or new dependency is introduced.

**Tech Stack:** Go, Cobra, Huma/OpenAPI, SQLite, `testify`, `httptest`, existing `go.kenn.io/kit/selfupdate` installer.

## Global Constraints

- A daemon update lease lasts exactly 60 seconds and is renewed every 20 seconds.
- Interrupt unwind is bounded to 30 seconds before installation is refused.
- `--running` accepts only `wait`, `interrupt`, or `abort`; `--yes` without an explicit policy uses `wait`.
- `--no-restart` bypasses daemon preparation and preserves binary-only installation behavior.
- `--force` continues to mean only “replace a development build with the latest official release.”
- Enqueues remain accepted during an update drain, while `ClaimJob` remains blocked by the existing persisted `shutdown_draining` key.
- Update-interrupted attempts preserve `retry_count`, discard partial output on the fresh attempt, and emit no cancel/terminal event, panel release, hook, cooldown, or failover side effect.
- User cancellation wins the database race and is never changed back to queued.
- Success requires a responsive replacement daemon whose version matches after removing at most one leading `v` from each version.
- Use the existing coupled, checksum-verifying `update.PerformUpdate`; do not split or duplicate its download/install security path.
- Do not add a schema migration, updater-side operation record, data-directory lock, or third-party dependency.
- Keep all daemon tests and command tests isolated from the live data directory and installed binary.

---

## File Map

- `internal/storage/jobs.go`: attempt-scoped update-interruption requeue plus running-job ID/count queries.
- `internal/storage/db_job_test.go`: storage transition, ownership race, and field-reset coverage.
- `internal/storage/models.go`: additive update-drain fields in daemon status JSON.
- `internal/daemon/worker.go`: target registration, late-registration cancellation, centralized interruption guard, and normal review/fix paths.
- `internal/daemon/worker_classify.go`: route classifier failures through the centralized guard.
- `internal/daemon/synthesis_worker.go`: route synthesis failures and cancellation through the centralized guard.
- `internal/daemon/worker_update_interrupt_test.go`: focused cancellation/requeue and side-effect tests.
- `internal/daemon/update_drain.go`: in-memory single-lease coordinator and prepare/renew/release handlers.
- `internal/daemon/update_drain_test.go`: lease, expiry, ownership handoff, enqueue, and cutover-race tests.
- `internal/daemon/server.go`: initialize/close the coordinator, expose its status, and hand gate ownership to normal shutdown.
- `internal/daemon/routes.go`: register the three update-drain endpoints.
- `internal/daemon/types.go`: typed request and response models for Huma/OpenAPI.
- `internal/daemon/shutdown_test.go`: prove release cannot reopen a normal shutdown drain.
- `pkg/client/openapi.yaml` and `pkg/client/generated/*`: regenerated additive public API models/client methods.
- `cmd/roborev/update_daemon.go`: updater-side daemon protocol, lease heartbeat, unwind polling, legacy fallback, shutdown handoff, and version check.
- `cmd/roborev/update_daemon_test.go`: updater protocol, compatibility, cancellation, and version tests.
- `cmd/roborev/update.go`: command flags, confirmation/policy selection, phase output, and orchestration.
- `cmd/roborev/main_test.go`: update command parsing, prompt, output, and ordering coverage near existing update tests.
- `internal/update/update.go` and `internal/update/update_test.go`: expose the existing secure installer through a caller-supplied reporter so the CLI can own phase formatting without duplicating download logic.
- `cmd/roborev/status.go` and `cmd/roborev/status_test.go`: human-readable active/recovering drain state.
- `cmd/roborev/daemon_integration_test.go`: scratch-data-dir replacement-daemon integration coverage.
- `docs/commands.md`: user-facing prompt, flag, defaults, drain, enqueue, and interruption behavior.

---

### Task 1: Add the attempt-scoped storage contract

**Files:**

- Modify: `internal/storage/jobs.go:570-640,1166-1225`
- Modify: `internal/storage/models.go:332-360`
- Test: `internal/storage/db_job_test.go:128-170,1498-1540`

**Interfaces:**

- Consumes: existing `(*storage.DB).ClaimJob(workerID string) (*ReviewJob, error)` and the `review_jobs` attempt fields reset by `RetryJob`/`ResetStaleJobs`.
- Produces: `(*storage.DB).RequeueUpdateInterruptedJob(jobID int64, workerID string) (bool, error)`, `(*storage.DB).ListRunningJobIDs() ([]int64, error)`, `(*storage.DB).CountRunningJobsByID(jobIDs []int64) (int, error)`, and additive `DaemonStatus` fields.

- [ ] **Step 1: Write failing storage tests for the successful transition and user-cancel race**

Add tests that claim with `worker-A`, populate all attempt fields, requeue, and prove the same row is queued with `retry_count` unchanged. Then cancel a second claimed row before requeueing and prove the requeue reports `false` and leaves it canceled.

```go
func TestRequeueUpdateInterruptedJobResetsAttemptWithoutRetry(t *testing.T) {
	env := setupJobEnv(t, "/tmp/update-requeue", "abc123")
	claimed, err := env.db.ClaimJob("worker-A")
	require.NoError(t, err)
	require.Equal(t, env.job.ID, claimed.ID)
	require.NoError(t, env.db.MarkJobAgentInvoked(env.job.ID, "worker-A", "test-agent run"))
	require.NoError(t, env.db.SaveJobSessionID(env.job.ID, "worker-A", "session-1"))
	require.NoError(t, env.db.SaveJobTokenUsage(
		env.job.ID, "session-1", `{"cost_usd":1.25,"has_cost":true}`))

	requeued, err := env.db.RequeueUpdateInterruptedJob(env.job.ID, "worker-A")
	require.NoError(t, err)
	assert.True(t, requeued)

	got, err := env.db.GetJobByID(env.job.ID)
	require.NoError(t, err)
	assert.Equal(t, JobStatusQueued, got.Status)
	assert.Equal(t, 0, got.RetryCount)
	assert.Empty(t, got.WorkerID)
	assert.Nil(t, got.StartedAt)
	assert.Empty(t, got.SessionID)
	assert.Empty(t, got.TokenUsage)
	assert.Empty(t, got.CommandLine)
	assert.False(t, getJobAgentInvoked(t, env.db, env.job.ID))
}

func TestRequeueUpdateInterruptedJobUserCancelWins(t *testing.T) {
	env := setupJobEnv(t, "/tmp/update-cancel-race", "def456")
	_, err := env.db.ClaimJob("worker-A")
	require.NoError(t, err)
	require.NoError(t, env.db.CancelJob(env.job.ID))

	requeued, err := env.db.RequeueUpdateInterruptedJob(env.job.ID, "worker-A")
	require.NoError(t, err)
	assert.False(t, requeued)
	got, err := env.db.GetJobByID(env.job.ID)
	require.NoError(t, err)
	assert.Equal(t, JobStatusCanceled, got.Status)
}
```

- [ ] **Step 2: Run the focused tests and verify they fail**

Run: `go test ./internal/storage -run 'TestRequeueUpdateInterruptedJob' -count=1`

Expected: FAIL because `RequeueUpdateInterruptedJob` does not exist.

- [ ] **Step 3: Implement the guarded requeue and running-job queries**

Use the same reset list as `ResetStaleJobs`, but do not increment `retry_count`. The ownership predicates must remain in the SQL statement.

```go
func (db *DB) RequeueUpdateInterruptedJob(
	jobID int64, workerID string,
) (bool, error) {
	result, err := db.Exec(`
		UPDATE review_jobs
		SET status = 'queued',
		    worker_id = NULL,
		    started_at = NULL,
		    session_id = NULL,
		    token_usage = NULL,
		    command_line = NULL,
		    agent_invoked = 0,
		    synced_at = NULL
		WHERE id = ? AND status = 'running' AND worker_id = ?
	`, jobID, workerID)
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	return rows > 0, err
}

func (db *DB) ListRunningJobIDs() ([]int64, error) {
	rows, err := db.Query(`SELECT id FROM review_jobs WHERE status = 'running' ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (db *DB) CountRunningJobsByID(jobIDs []int64) (int, error) {
	count := 0
	for _, id := range jobIDs {
		job, err := db.GetJobByID(id)
		if err != nil {
			return 0, err
		}
		if job.Status == JobStatusRunning {
			count++
		}
	}
	return count, nil
}
```

Add the status fields without `omitempty` on the boolean, while keeping empty policy/expiry absent:

```go
UpdateDraining       bool   `json:"update_draining"`
UpdateDrainPolicy    string `json:"update_drain_policy,omitempty"`
UpdateDrainExpiresAt string `json:"update_drain_expires_at,omitempty"`
```

- [ ] **Step 4: Add ownership and query tests**

Add a wrong-worker assertion and prove the running-ID helpers count only targeted rows still in `running` state.

```go
requeued, err := db.RequeueUpdateInterruptedJob(job.ID, "worker-B")
require.NoError(t, err)
assert.False(t, requeued)

ids, err := db.ListRunningJobIDs()
require.NoError(t, err)
assert.Contains(t, ids, job.ID)
count, err := db.CountRunningJobsByID(ids)
require.NoError(t, err)
assert.Equal(t, 1, count)
```

- [ ] **Step 5: Run storage tests**

Run: `go test ./internal/storage -run 'Test(RequeueUpdateInterruptedJob|RunningJobIDs)' -count=1`

Expected: PASS.

- [ ] **Step 6: Commit the storage contract**

```bash
git add internal/storage/jobs.go internal/storage/models.go internal/storage/db_job_test.go
git commit -m "Add update interruption storage transitions"
```

---

### Task 2: Centralize worker update interruption

**Files:**

- Modify: `internal/daemon/worker.go:35-120,193-330,580-980,1100-1255,1622-1675`
- Modify: `internal/daemon/worker_classify.go:86-205,259-305`
- Modify: `internal/daemon/synthesis_worker.go:22-125,240-340`
- Create: `internal/daemon/worker_update_interrupt_test.go`

**Interfaces:**

- Consumes: `DB.RequeueUpdateInterruptedJob`, `DB.CountRunningJobsByID`, and current running-job cancellation registration.
- Produces: `WorkerPool.InterruptJobsForUpdate(jobIDs []int64)`, `WorkerPool.ClearUpdateInterrupt(jobIDs []int64)`, the internal guard `handleUpdateInterruption(ctx context.Context, workerID string, job *storage.ReviewJob) bool`, and context-aware `failOrRetry`, `failOrRetryAgent`, `failCooldownOrFailover`, `failoverOrFailNonRetryableAgent`, and `failoverOrFailWithPrefix` helpers.

- [ ] **Step 1: Write failing tests for registered and late-registered workers**

Create a blocking test agent and assert that both an already registered job and a job paused immediately before registration receive context cancellation.

```go
func waitForStoredJobStatus(
	t *testing.T, db *storage.DB, jobID int64, status storage.JobStatus,
) {
	t.Helper()
	require.Eventually(t, func() bool {
		job, err := db.GetJobByID(jobID)
		return err == nil && job.Status == status
	}, time.Second, 10*time.Millisecond)
}

func TestInterruptJobsForUpdateCancelsLateRegistration(t *testing.T) {
	tc := newWorkerTestContext(t, 1)
	late := tc.createJob(t, "late")
	lateAtRegistration := make(chan struct{})
	releaseRegistration := make(chan struct{})
	tc.Pool.testHookBeforeRunningRegistration = func(jobID int64) {
		if jobID != late.ID { return }
		close(lateAtRegistration)
		<-releaseRegistration
	}
	tc.Pool.Start()
	<-lateAtRegistration

	tc.Pool.InterruptJobsForUpdate([]int64{late.ID})
	close(releaseRegistration)

	require.Eventually(t, func() bool {
		job, err := tc.DB.GetJobByID(late.ID)
		return err == nil && job.Status == storage.JobStatusQueued
	}, time.Second, 10*time.Millisecond)
}
```

Add `testHookBeforeRunningRegistration func(jobID int64)` beside the existing
worker test hooks and call it after `ClaimJob` returns but immediately before
`registerRunningJob`. Add a separate registered-worker test using a
`agent.TestAgent{Delay: time.Second}`: wait until `runningJobs[job.ID]` exists
under `runningJobsMu`, call `InterruptJobsForUpdate`, and assert the same queued
state. Reset the hook with `t.Cleanup` to avoid a cross-test leak.

- [ ] **Step 2: Run the focused test and verify it fails**

Run: `go test ./internal/daemon -run TestInterruptJobsForUpdateCancelsLateRegistration -count=1`

Expected: FAIL because `InterruptJobsForUpdate` is undefined.

- [ ] **Step 3: Add the target registry and centralized guard**

Protect update targets with `runningJobsMu`, check them during registration, and call cancel functions only after unlocking.

```go
type WorkerPool struct {
	// existing fields...
	updateInterrupted map[int64]struct{} // protected by runningJobsMu
}

func (wp *WorkerPool) InterruptJobsForUpdate(jobIDs []int64) {
	wp.runningJobsMu.Lock()
	var cancels []context.CancelFunc
	for _, id := range jobIDs {
		wp.updateInterrupted[id] = struct{}{}
		if running, ok := wp.runningJobs[id]; ok {
			cancels = append(cancels, running.cancel)
		}
	}
	wp.runningJobsMu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

func (wp *WorkerPool) handleUpdateInterruption(
	ctx context.Context, workerID string, job *storage.ReviewJob,
) bool {
	if !errors.Is(ctx.Err(), context.Canceled) || !wp.isUpdateInterrupted(job.ID) {
		return false
	}
	requeued, err := wp.db.RequeueUpdateInterruptedJob(job.ID, workerID)
	if err != nil {
		log.Printf("[%s] requeue update-interrupted job %d: %v", workerID, job.ID, err)
		return true // leave the row running for ResetStaleJobs; suppress side effects
	}
	if requeued {
		log.Printf("[%s] Job %d interrupted for update and requeued", workerID, job.ID)
		return true
	}
	current, err := wp.db.GetJobByID(job.ID)
	if err != nil {
		log.Printf("[%s] inspect update-interrupted job %d: %v", workerID, job.ID, err)
		return true
	}
	return current.Status != storage.JobStatusCanceled
}
```

In `registerRunningJob`, calculate `updateCanceled := wp.isUpdateInterruptedLocked(jobID)` alongside the existing pending-cancel check, unlock, then cancel. Initialize and clear the map explicitly:

```go
updateInterrupted: make(map[int64]struct{}),

func (wp *WorkerPool) ClearUpdateInterrupt(jobIDs []int64) {
	wp.runningJobsMu.Lock()
	defer wp.runningJobsMu.Unlock()
	for _, id := range jobIDs {
		delete(wp.updateInterrupted, id)
	}
}
```

- [ ] **Step 4: Thread `context.Context` through every fail/retry/failover helper**

Change the helper signatures and put the guard before classification, cooldown, retry, failover, failure broadcasts, or panel release:

```go
func (wp *WorkerPool) failOrRetry(
	ctx context.Context, workerID string, job *storage.ReviewJob,
	agentName, errorMsg string,
) {
	wp.failOrRetryInner(ctx, workerID, job, agentName, errorMsg, false)
}

func (wp *WorkerPool) failOrRetryInner(
	ctx context.Context, workerID string, job *storage.ReviewJob,
	agentName, errorMsg string, agentError bool,
) {
	if wp.handleUpdateInterruption(ctx, workerID, job) {
		return
	}
	if agentError {
		wp.handleAgentFailure(ctx, workerID, job, agentName, errorMsg)
		return
	}
	wp.retryOrFail(ctx, workerID, job, agentName, errorMsg, false)
}

func (wp *WorkerPool) failoverOrFailNonRetryableAgent(
	ctx context.Context, workerID string, job *storage.ReviewJob,
	agentName, errorMsg string,
) {
	if wp.handleUpdateInterruption(ctx, workerID, job) {
		return
	}
	wp.failoverOrFailNonRetryableAgentAfterGuard(workerID, job, agentName, errorMsg)
}
```

Extract the current bodies into `handleAgentFailure`, `retryOrFail`, and
`failoverOrFailNonRetryableAgentAfterGuard` without changing their ordering or
side effects. Change `failCooldownOrFailover` and
`failoverOrFailWithPrefix` to accept `ctx` and run the same guard before agent
cooldown or failover. Update every call in `worker.go`,
`worker_classify.go`, and `synthesis_worker.go` to pass the attempt context. At
the agent cancellation branches, call the guard before the normal
`review.canceled`/panel-release branch:

```go
if err != nil {
	if wp.handleUpdateInterruption(ctx, workerID, job) {
		return
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		// existing normal user-cancel event and panel behavior
	}
}
```

- [ ] **Step 5: Add table-driven guard coverage for every exit family**

Add a package-local `updateInterruptPathResult` and
`runUpdateInterruptedPath(t, path)` test harness in the new test file. Reuse
`newWorkerTestContext`, `test` agents, existing worker test hooks, and injected
function variables; the harness subscribes to broadcaster events, reads the
row, records panel synthesis state, and snapshots cooldown/failover state. Use
it to trigger checkout, prebuilt prompt, min-severity, prompt building, agent
lookup, prompt-size, fix-worktree, agent execution, patch capture, classifier,
and synthesis failures after cancellation:

```go
type updateInterruptPathResult struct {
	Job *storage.ReviewJob
	InitialRetryCount int
	EventTypes []string
	PanelReleased bool
	HookRan bool
	AgentCooledDown bool
	FailedOver bool
}

func countEventTypes(events []Event, eventType string, jobID int64) int {
	count := 0
	for _, event := range events {
		if event.Type == eventType && event.JobID == jobID { count++ }
	}
	return count
}

for _, path := range []string{
	"checkout", "prebuilt-prompt", "min-severity", "prompt", "agent-lookup",
	"prompt-size", "fix-worktree", "agent", "patch-capture", "classifier", "synthesis",
} {
	t.Run(path, func(t *testing.T) {
		result := runUpdateInterruptedPath(t, path)
		assert.Equal(t, storage.JobStatusQueued, result.Job.Status)
		assert.Equal(t, result.InitialRetryCount, result.Job.RetryCount)
		assert.NotContains(t, result.EventTypes, "review.canceled")
		assert.NotContains(t, result.EventTypes, "review.failed")
		assert.False(t, result.PanelReleased)
		assert.False(t, result.HookRan)
		assert.False(t, result.AgentCooledDown)
		assert.False(t, result.FailedOver)
	})
}
```

- [ ] **Step 6: Add a user-cancel-wins worker test**

Race `DB.CancelJob` ahead of the guard and assert the existing user cancellation event and panel release still occur exactly once while the row remains canceled.

```go
require.NoError(t, tc.DB.CancelJob(job.ID))
require.True(t, tc.Pool.CancelJob(job.ID))
require.Eventually(t, func() bool {
	return countEventTypes(events, "review.canceled", job.ID) == 1
}, time.Second, 10*time.Millisecond)
got, err := tc.DB.GetJobByID(job.ID)
require.NoError(t, err)
assert.Equal(t, storage.JobStatusCanceled, got.Status)
jobs, err := tc.DB.ListJobs("", "", 0, 0, storage.WithPanelRun(job.PanelRunUUID))
require.NoError(t, err)
var synthesis storage.ReviewJob
for _, candidate := range jobs {
	if candidate.PanelRole == storage.PanelRoleSynthesis { synthesis = candidate }
}
assert.False(t, synthesis.ClaimBlocked)
```

- [ ] **Step 7: Run worker tests**

Run: `go test ./internal/daemon -run 'Test(UpdateInterrupt|InterruptJobsForUpdate)' -count=1`

Expected: PASS, with no real agent process started.

- [ ] **Step 8: Commit worker interruption behavior**

```bash
git add internal/daemon/worker.go internal/daemon/worker_classify.go internal/daemon/synthesis_worker.go internal/daemon/worker_update_interrupt_test.go
git commit -m "Requeue reviews interrupted for updates"
```

---

### Task 3: Add the daemon-owned update lease and API

**Files:**

- Create: `internal/daemon/update_drain.go`
- Create: `internal/daemon/update_drain_test.go`
- Modify: `internal/daemon/server.go:40-80,570-715,1888-1935,3332-3370`
- Modify: `internal/daemon/routes.go:120-190`
- Modify: `internal/daemon/types.go:425-450,588-610`
- Modify: `internal/daemon/shutdown_test.go:1-135`
- Regenerate: `pkg/client/openapi.yaml`
- Regenerate: `pkg/client/generated/*`

**Interfaces:**

- Consumes: `DB.SetShutdownDraining`, `DB.ListRunningJobIDs`, `DB.CountRunningJobsByID`, `WorkerPool.InterruptJobsForUpdate`, `WorkerPool.ClearUpdateInterrupt`, and `WorkerPool.ActiveWorkers`.
- Produces: `POST /api/update/prepare`, `POST /api/update/renew`, `POST /api/update/release`, `updateDrainCoordinator`, `updateDrainCoordinator.releaseExpiredLocked(*updateDrainLease) error`, `Server.updateDrainStatus() (bool, string, time.Time)`, and generated client methods `PrepareUpdateWithResponse`, `RenewUpdateWithResponse`, and `ReleaseUpdateWithResponse`.

- [ ] **Step 1: Define typed API models and failing route tests**

Add exact request/response types:

```go
type UpdateDrainRequestBody struct {
	OwnerID string `json:"owner_id" minLength:"1"`
	Policy  string `json:"policy" enum:"wait,interrupt,abort"`
}

type PrepareUpdateInput struct { Body UpdateDrainRequestBody }

type UpdateLeaseRequestBody struct {
	LeaseToken string `json:"lease_token" minLength:"1"`
}

type RenewUpdateInput struct { Body UpdateLeaseRequestBody }
type ReleaseUpdateInput struct { Body UpdateLeaseRequestBody }

type UpdateDrainStatus struct {
	LeaseToken           string    `json:"lease_token,omitempty"`
	Policy               string    `json:"policy"`
	ExpiresAt            time.Time `json:"expires_at"`
	RunningJobs          int       `json:"running_jobs"`
	TargetedRunningJobs  int       `json:"targeted_running_jobs"`
	ActiveWorkers        int       `json:"active_workers"`
	Recovering           bool      `json:"recovering"`
}

type PrepareUpdateOutput struct { Body UpdateDrainStatus }
type RenewUpdateOutput struct { Body UpdateDrainStatus }
type ReleaseUpdateOutput struct {
	Body struct { Released bool `json:"released"` }
}
```

Register operation IDs `prepare-update`, `renew-update`, and `release-update`, then add route tests expecting success schemas and 409 conflict responses.

- [ ] **Step 2: Run the route tests and verify they fail**

Run: `go test ./internal/daemon -run 'Test(UpdateDrainRoutes|PrepareUpdate)' -count=1`

Expected: FAIL because the handlers and coordinator do not exist.

- [ ] **Step 3: Implement the in-memory lease coordinator**

Use one lifecycle mutex for update and shutdown ownership. The lease record is purely in memory and same-owner idempotence requires the same policy.

```go
const (
	updateLeaseDuration = 60 * time.Second
	updatePolicyWait = "wait"
	updatePolicyInterrupt = "interrupt"
	updatePolicyAbort = "abort"
)

type updateDrainLease struct {
	ownerID    string
	token      string
	policy     string
	expiresAt  time.Time
	targeted   []int64
	recovering bool
	timer      *time.Timer
}

type updateDrainCoordinator struct {
	server *Server
	now    func() time.Time
}

func (c *updateDrainCoordinator) prepare(
	ownerID, policy string,
) (UpdateDrainStatus, error) {
	s := c.server
	s.shutdownDrainMu.Lock()
	defer s.shutdownDrainMu.Unlock()
	if s.shutdownDraining {
		return UpdateDrainStatus{}, errShutdownInProgress
	}
	if lease := s.updateDrain; lease != nil {
		if !lease.expiresAt.After(c.now()) {
			if err := c.releaseExpiredLocked(lease); err != nil || s.updateDrain != nil {
				return UpdateDrainStatus{}, errUpdateRecoveryInProgress
			}
		} else {
			if lease.ownerID == ownerID && lease.policy == policy {
				return c.snapshotLocked(lease)
			}
			if lease.ownerID == ownerID {
				return UpdateDrainStatus{}, errUpdatePolicyConflict
			}
			return UpdateDrainStatus{}, &updateLeaseConflict{Remaining: lease.expiresAt.Sub(c.now())}
		}
	}
	if err := s.db.SetShutdownDraining(true); err != nil {
		return UpdateDrainStatus{}, fmt.Errorf("block job claims for update: %w", err)
	}
	ids, err := s.db.ListRunningJobIDs()
	if err != nil {
		return UpdateDrainStatus{}, c.rollbackPreparationLocked(ownerID, policy, err)
	}
	if policy == updatePolicyAbort && len(ids) != 0 {
		return UpdateDrainStatus{}, c.rollbackPreparationLocked(ownerID, policy, errReviewsRunning)
	}
	lease := &updateDrainLease{
		ownerID: ownerID, token: storage.GenerateUUID(), policy: policy,
		expiresAt: c.now().Add(updateLeaseDuration), targeted: ids,
	}
	s.updateDrain = lease
	if policy == updatePolicyInterrupt {
		s.workerPool.InterruptJobsForUpdate(ids)
	}
	c.armExpiryLocked(lease)
	return c.snapshotLocked(lease)
}
```

`renew` validates the token and requires an unexpired, non-recovering lease,
extends expiry by 60 seconds, rearms the timer, and returns current counts.
`release` validates the token and shutdown ownership. A wait lease clears the
DB gate immediately; an interrupt lease clears only when `ActiveWorkers()==0`
and `CountRunningJobsByID(targeted)==0`. Otherwise mark it recovering and let
the expiry recovery callback poll until both conditions hold.

Implement `releaseExpiredLocked` as the one clearing path used by timer expiry,
explicit release, and prepare-time stale-lease cleanup:

```go
func (c *updateDrainCoordinator) releaseExpiredLocked(lease *updateDrainLease) error {
	if c.server.updateDrain != lease { return nil }
	remaining, err := c.server.db.CountRunningJobsByID(lease.targeted)
	if err != nil { lease.recovering = true; return err }
	if lease.policy == updatePolicyInterrupt &&
		(remaining != 0 || c.server.workerPool.ActiveWorkers() != 0) {
		lease.recovering = true
		c.armRecoveryRetryLocked(lease)
		return nil
	}
	if err := c.server.db.SetShutdownDraining(false); err != nil {
		lease.recovering = true
		c.armRecoveryRetryLocked(lease)
		return err
	}
	c.server.workerPool.ClearUpdateInterrupt(lease.targeted)
	c.server.updateDrain = nil
	return nil
}
```

`rollbackPreparationLocked` first tries `SetShutdownDraining(false)`. If that
clear fails, retain an in-memory recovery record with an already-expired lease,
arm the same retry callback, and return an error joining the original failure
with the clear failure. This ensures `/api/status` exposes the blocked gate
instead of leaving a silent drain after an abort or running-ID query failure:

```go
func (c *updateDrainCoordinator) rollbackPreparationLocked(
	ownerID, policy string, cause error,
) error {
	if err := c.server.db.SetShutdownDraining(false); err == nil {
		return cause
	} else {
		lease := &updateDrainLease{
			ownerID: ownerID, token: storage.GenerateUUID(), policy: policy,
			expiresAt: c.now(), recovering: true,
		}
		c.server.updateDrain = lease
		c.armExpiryLocked(lease)
		return errors.Join(cause, fmt.Errorf("release update claim gate: %w", err))
	}
}
```

- [ ] **Step 4: Wire handlers and routes with stable conflict responses**

Initialize `s.updateCoordinator = &updateDrainCoordinator{server: s, now:
time.Now}` in `NewServer`. Add handlers that distinguish review-busy,
same-owner-policy, other-owner, recovery, and shutdown conflicts from internal
errors. The other-owner message must include a nonnegative rounded remaining
duration.

```go
func (s *Server) humaPrepareUpdate(
	ctx context.Context, input *PrepareUpdateInput,
) (*PrepareUpdateOutput, error) {
	status, err := s.updateCoordinator.prepare(input.Body.OwnerID, input.Body.Policy)
	if err != nil {
		var leaseConflict *updateLeaseConflict
		switch {
		case errors.Is(err, errReviewsRunning):
			return nil, huma.Error409Conflict("reviews are running")
		case errors.Is(err, errUpdatePolicyConflict):
			return nil, huma.Error409Conflict("update owner already holds a lease with a different policy")
		case errors.Is(err, errUpdateRecoveryInProgress):
			return nil, huma.Error409Conflict("previous update interruption is still recovering")
		case errors.Is(err, errShutdownInProgress):
			return nil, huma.Error409Conflict("daemon shutdown in progress")
		case errors.As(err, &leaseConflict):
			remaining := max(time.Duration(0), leaseConflict.Remaining).Round(time.Second)
			return nil, huma.Error409Conflict(
				fmt.Sprintf("another update is in progress; lease expires in %s", remaining))
		default:
			return nil, huma.Error500InternalServerError(fmt.Sprintf("prepare update: %v", err))
		}
	}
	return &PrepareUpdateOutput{Body: status}, nil
}

func (s *Server) humaRenewUpdate(
	ctx context.Context, input *RenewUpdateInput,
) (*RenewUpdateOutput, error) {
	status, err := s.updateCoordinator.renew(input.Body.LeaseToken)
	if err != nil { return nil, updateLeaseHTTPError("renew", err) }
	return &RenewUpdateOutput{Body: status}, nil
}

func (s *Server) humaReleaseUpdate(
	ctx context.Context, input *ReleaseUpdateInput,
) (*ReleaseUpdateOutput, error) {
	released, err := s.updateCoordinator.release(input.Body.LeaseToken)
	if err != nil { return nil, updateLeaseHTTPError("release", err) }
	resp := &ReleaseUpdateOutput{}
	resp.Body.Released = released
	return resp, nil
}

func updateLeaseHTTPError(op string, err error) error {
	switch {
	case errors.Is(err, errLeaseTokenMismatch):
		return huma.Error409Conflict("update lease is not owned by this updater")
	case errors.Is(err, errUpdateRecoveryInProgress):
		return huma.Error409Conflict("update interruption is still recovering")
	case errors.Is(err, errShutdownInProgress):
		return huma.Error409Conflict("daemon shutdown owns the claim drain")
	default:
		return huma.Error500InternalServerError(fmt.Sprintf("%s update lease: %v", op, err))
	}
}
```

Register the routes next to `/api/shutdown`:

```go
huma.Post(api, "/api/update/prepare", s.humaPrepareUpdate, func(o *huma.Operation) {
	o.OperationID = "prepare-update"
	o.Summary = "Prepare a leased update drain"
	o.Tags = []string{"daemon"}
})
huma.Post(api, "/api/update/renew", s.humaRenewUpdate, func(o *huma.Operation) {
	o.OperationID = "renew-update"
	o.Summary = "Renew an update drain lease"
	o.Tags = []string{"daemon"}
})
huma.Post(api, "/api/update/release", s.humaReleaseUpdate, func(o *huma.Operation) {
	o.OperationID = "release-update"
	o.Summary = "Release an update drain lease"
	o.Tags = []string{"daemon"}
})
```

- [ ] **Step 5: Hand ownership to normal shutdown without opening the gate**

Change `beginShutdownDrain` under the same mutex. If an update already owns the persisted gate, do not clear or rewrite it; mark shutdown ownership first, invalidate the lease, then stop claims in memory.

```go
func (s *Server) beginShutdownDrain() error {
	s.shutdownDrainMu.Lock()
	defer s.shutdownDrainMu.Unlock()
	if s.shutdownDraining {
		return nil
	}
	if s.updateDrain == nil {
		if err := s.db.SetShutdownDraining(true); err != nil {
			return fmt.Errorf("block job claims for shutdown: %w", err)
		}
	}
	s.shutdownDraining = true
	if s.updateDrain != nil {
		s.updateDrain.timer.Stop()
		s.updateDrain = nil
	}
	s.workerPool.BeginStop()
	return nil
}
```

Keep `clearShutdownDrain` exclusively for shutdown teardown. Update release must return 409 after `shutdownDraining` becomes true and must never call `SetShutdownDraining(false)` in that state.

- [ ] **Step 6: Expose active and recovering state through `/api/status`**

Read a coordinator snapshot under `shutdownDrainMu` and populate the additive fields:

```go
func (s *Server) updateDrainStatus() (bool, string, time.Time) {
	s.shutdownDrainMu.Lock()
	defer s.shutdownDrainMu.Unlock()
	if s.updateDrain == nil { return false, "", time.Time{} }
	return true, s.updateDrain.policy, s.updateDrain.expiresAt
}

active, policy, expiresAt := s.updateDrainStatus()
resp.Body.UpdateDraining = active
resp.Body.UpdateDrainPolicy = policy
if !expiresAt.IsZero() {
	resp.Body.UpdateDrainExpiresAt = expiresAt.Format(time.RFC3339)
}
```

An interrupt lease whose expiry is in the past stays `update_draining=true`; the past expiry plus `interrupt` policy is the stable recovery signal until unwind completes.

- [ ] **Step 7: Add lease and ownership tests**

Cover:

```go
func TestPrepareUpdateSameOwnerRequiresSamePolicy(t *testing.T) {
	s := newTestServer(t)
	first := prepareUpdate(t, s, "owner-1", "wait")
	second := prepareUpdate(t, s, "owner-1", "wait")
	assert.Equal(t, first.LeaseToken, second.LeaseToken)

	rr := prepareUpdateResponse(t, s, "owner-1", "interrupt")
	assert.Equal(t, http.StatusConflict, rr.Code)
}

func TestReleaseUpdateCannotClearShutdownOwnership(t *testing.T) {
	s := newTestServer(t)
	lease := prepareUpdate(t, s, "owner-1", "wait")
	require.NoError(t, s.beginShutdownDrain())

	rr := releaseUpdateResponse(t, s, lease.LeaseToken)
	assert.Equal(t, http.StatusConflict, rr.Code)
	draining, err := s.db.IsShutdownDraining()
	require.NoError(t, err)
	assert.True(t, draining)
}
```

Also cover different-owner 409 with remaining duration, renewal, expiry
release, interrupt recovery, failed rollback remaining visible, abort
atomicity, prepare-after-shutdown refusal, claim-at-boundary inclusion, and
enqueue-success/claim-blocked behavior. Add a table that changes each targeted
row from `running` to `done`, `canceled`, `failed`, and ordinary-retry `queued`;
each state must make `TargetedRunningJobs` zero and allow unwind once
`ActiveWorkers` reaches zero.

- [ ] **Step 8: Run daemon lease tests**

Run: `go test ./internal/daemon -run 'Test(UpdateDrain|PrepareUpdate|ReleaseUpdate|ShutdownDrain)' -count=1`

Expected: PASS.

- [ ] **Step 9: Regenerate and verify the public OpenAPI client**

Run: `make api-generate`

Expected: `pkg/client/openapi.yaml` and split files under `pkg/client/generated/` contain the three new endpoints and additive status fields.

Run: `make api-check`

Expected: PASS with no generated diff after the check.

- [ ] **Step 10: Commit the daemon protocol**

```bash
git add internal/daemon/update_drain.go internal/daemon/update_drain_test.go internal/daemon/server.go internal/daemon/routes.go internal/daemon/types.go internal/daemon/shutdown_test.go pkg/client/openapi.yaml pkg/client/generated
git commit -m "Add leased daemon update drains"
```

---

### Task 4: Build the updater-side daemon handoff

**Files:**

- Create: `cmd/roborev/update_daemon.go`
- Create: `cmd/roborev/update_daemon_test.go`
- Modify: `cmd/roborev/update.go:1-325`
- Modify: `internal/update/update.go:60-75,304-335`
- Modify: `internal/update/update_test.go:360-460`

**Interfaces:**

- Consumes: generated update API methods, current runtime discovery/start/stop helpers, `waitForDaemonExit`, and `/api/ping` version data.
- Produces: `runningReviewPolicy`, `updateDaemonSession`, `prepareUpdateDaemon`, `waitForPreparedDrain`, `restartAndVerifyUpdatedDaemon`, `normalizeUpdateVersion`, and context-aware `update.PerformUpdateWithReporter(ctx context.Context, info *UpdateInfo, reporter Reporter) error`.

- [ ] **Step 1: Write failing policy and protocol tests**

Use `httptest.Server` to cover prepare success, 409, heartbeat renewal, release, 404 compatibility by policy, and version normalization.

```go
func newUpdateTestEndpoint(t *testing.T, prepareStatus int) daemon.DaemonEndpoint {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/status" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"running_jobs":0}`)
			return
		}
		w.WriteHeader(prepareStatus)
	}))
	t.Cleanup(server.Close)
	u, err := url.Parse(server.URL)
	require.NoError(t, err)
	return daemon.DaemonEndpoint{Network: "tcp", Address: u.Host}
}

func TestNormalizeUpdateVersion(t *testing.T) {
	for _, tc := range []struct{ got, want string }{
		{"v0.65.0", "0.65.0"},
		{"0.65.0", "0.65.0"},
		{"vv0.65.0", "v0.65.0"},
	} {
		assert.Equal(t, tc.want, normalizeUpdateVersion(tc.got))
	}
}

func TestPrepareUpdateDaemonLegacyPolicies(t *testing.T) {
	for _, tc := range []struct {
		policy runningReviewPolicy
		wantFallback bool
		wantErr bool
	}{
		{policyWait, true, false},
		{policyInterrupt, false, true},
		{policyAbort, true, false},
	} {
		t.Run(string(tc.policy), func(t *testing.T) {
			endpoint := newUpdateTestEndpoint(t, http.StatusNotFound)
			session, err := prepareUpdateDaemon(
				t.Context(), endpoint, "owner", tc.policy, io.Discard)
			if tc.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.NotNil(t, session)
			assert.Equal(t, tc.wantFallback, session.Legacy)
		})
	}
}
```

- [ ] **Step 2: Run focused tests and verify they fail**

Run: `go test ./cmd/roborev -run 'Test(NormalizeUpdateVersion|PrepareUpdateDaemonLegacyPolicies)' -count=1`

Expected: FAIL because the updater protocol types/functions do not exist.

- [ ] **Step 3: Implement the daemon session and lease heartbeat**

Keep the owner ID and token in memory only. The heartbeat must outlive wait and continue through download/install until shutdown takes ownership.

```go
type runningReviewPolicy string

const (
	policyWait runningReviewPolicy = "wait"
	policyInterrupt runningReviewPolicy = "interrupt"
	policyAbort runningReviewPolicy = "abort"
	updateLeaseRenewInterval = 20 * time.Second
	updateDrainPollInterval = 250 * time.Millisecond
	updateInterruptUnwindTimeout = 30 * time.Second
)

type updateDaemonSession struct {
	Endpoint daemon.DaemonEndpoint
	OwnerID string
	Token string
	Policy runningReviewPolicy
	Legacy bool
	Prepared bool
	Installed bool
	ShutdownOwned bool
	cancelHeartbeat context.CancelFunc
}

func prepareUpdateDaemon(
	ctx context.Context, endpoint daemon.DaemonEndpoint,
	ownerID string, policy runningReviewPolicy, out io.Writer,
) (*updateDaemonSession, error) {
	api, err := roborevclient.NewWithHTTPClient(
		endpoint.BaseURL(), endpoint.HTTPClient(5*time.Second))
	if err != nil { return nil, err }
	resp, callErr := api.PrepareUpdateWithResponse(ctx,
		&generated.PrepareUpdateRequestOptions{Body: &generated.UpdateDrainRequestBody{
			OwnerID: ownerID, Policy: generated.UpdateDrainRequestBodyPolicy(policy),
		}})
	if resp != nil && resp.StatusCode == http.StatusNotFound {
		return prepareLegacyUpdateDaemon(ctx, endpoint, ownerID, policy, out)
	}
	if callErr != nil { return nil, fmt.Errorf("prepare daemon update: %w", callErr) }
	if resp.JSON200 == nil { return nil, fmt.Errorf("prepare daemon update returned %d", resp.StatusCode) }
	return &updateDaemonSession{
		Endpoint: endpoint, OwnerID: ownerID, Token: resp.JSON200.LeaseToken,
		Policy: policy, Prepared: true,
	}, nil
}

func (s *updateDaemonSession) startHeartbeat(ctx context.Context) <-chan error {
	errCh := make(chan error, 1)
	heartbeatCtx, cancel := context.WithCancel(ctx)
	s.cancelHeartbeat = cancel
	go func() {
		defer close(errCh)
		ticker := time.NewTicker(updateLeaseRenewInterval)
		defer ticker.Stop()
		for {
			select {
			case <-heartbeatCtx.Done():
				return
			case <-ticker.C:
				if _, err := s.renew(heartbeatCtx); err != nil {
					errCh <- err
					return
				}
			}
		}
	}()
	return errCh
}
```

Make the installer accept the orchestration context while preserving its
existing API for other callers:

```go
func PerformUpdateWithReporter(
	ctx context.Context, info *UpdateInfo, reporter Reporter,
) error {
	return defaultUpdater().PerformUpdateContext(ctx, info, reporter)
}

func (u *Updater) PerformUpdate(info *UpdateInfo, reporter Reporter) error {
	return u.PerformUpdateContext(context.Background(), info, reporter)
}

func (u *Updater) PerformUpdateContext(
	ctx context.Context, info *UpdateInfo, reporter Reporter,
) error {
	reporter = normalizeReporter(reporter)
	if info == nil { return fmt.Errorf("update info is nil") }
	if info.Checksum == "" {
		return fmt.Errorf("no checksum available for %s - refusing to install unverified binary", info.AssetName)
	}
	installDir, err := u.installDir()
	if err != nil { return err }
	targetBinary := executableName(u.deps.GOOS)
	dstPath := filepath.Join(installDir, targetBinary)
	reporter.Stepf("Downloading %s...\n", info.AssetName)
	ctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()
	if err := u.client().Install(ctx, info, selfupdate.InstallOptions{
		DestinationPath: dstPath,
		ArchiveBinaryName: targetBinary,
		Progress: reporter.Progress,
	}); err != nil { return err }
	reporter.Stepf("Installing %s... OK\n", targetBinary)
	return nil
}
```

Add a test that cancels the supplied context during a blocking download and
asserts no destination binary is installed.

During the wait phase, call `renew` every 250 ms and render its counts. Wait mode completes when `RunningJobs==0`; interrupt completes only when `TargetedRunningJobs==0 && ActiveWorkers==0`. Return a timeout error after 30 seconds only for interrupt.

- [ ] **Step 4: Implement legacy behavior without weakening verification**

Handle only HTTP 404 as missing prepare support:

```go
func prepareLegacyUpdateDaemon(
	ctx context.Context, endpoint daemon.DaemonEndpoint,
	ownerID string, policy runningReviewPolicy, out io.Writer,
) (*updateDaemonSession, error) {
	session := &updateDaemonSession{
		Endpoint: endpoint, OwnerID: ownerID, Policy: policy, Legacy: true,
	}
	switch policy {
	case policyWait:
		fmt.Fprintln(out, "Daemon       compatibility mode; waiting for graceful shutdown")
	case policyInterrupt:
		return nil, errors.New("running daemon does not support safe review interruption; use --running=wait or restart it first")
	case policyAbort:
		status, err := fetchDaemonStatus(ctx, endpoint)
		if err != nil { return nil, err }
		if status.RunningJobs != 0 {
			return nil, errReviewsRunning
		}
		fmt.Fprintln(out, "Daemon       compatibility mode; a racing review will be preserved by graceful shutdown")
	}
	return session, nil
}
```

For legacy wait, retain today’s synchronous graceful shutdown. For legacy abort, repeat `/api/status` immediately before installation and then use graceful shutdown so a racing claim is preserved. In both cases continue through replacement readiness and version verification.

- [ ] **Step 5: Implement the failure-safe orchestration state machine**

After successful preparation, defer a 2-second best-effort release until
`session.ShutdownOwned` is true. Create an operation context and cancel it when
the heartbeat reports an error, so lease loss during download cancels the
installer before atomic replacement. Set `Installed` only after the installer
returns nil; set `ShutdownOwned` only after `/api/shutdown` accepts ownership.

```go
operationCtx, cancelOperation := context.WithCancel(ctx)
defer cancelOperation()
heartbeatErr := session.startHeartbeat(operationCtx)
defer func() {
	if session.Prepared && !session.ShutdownOwned {
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		_ = session.release(releaseCtx)
	}
}()
go func() {
	if err := <-heartbeatErr; err != nil { cancelOperation() }
}()

if err := waitForPreparedDrain(operationCtx, session); err != nil { return err }
if err := install(operationCtx); err != nil { return err }
session.Installed = true
if err := session.shutdown(operationCtx); err != nil { return err }
session.ShutdownOwned = true
```

For prepare, renew, wait timeout, download/checksum, install, and shutdown
errors, return without starting a second daemon. The deferred release leaves
the old daemon running unless shutdown already owns the gate. An interrupt
unwind timeout leaves the coordinator’s visible recovery state intact rather
than opening the gate.

- [ ] **Step 6: Make restart return an error and verify exact version**

Refactor `restartDaemonAfterUpdate` into an error-returning helper. Accept manager handoff only after the old PID is gone, the replacement is responsive, and `/api/ping` matches the target.

```go
func normalizeUpdateVersion(v string) string { return strings.TrimPrefix(v, "v") }

func requireUpdatedDaemonVersion(observed, expected string) error {
	if normalizeUpdateVersion(observed) != normalizeUpdateVersion(expected) {
		return fmt.Errorf("daemon version mismatch: expected %s, observed %s", expected, observed)
	}
	return nil
}

func restartAndVerifyUpdatedDaemon(
	ctx context.Context, binDir, expectedVersion string, previous *daemon.RuntimeInfo,
) error {
	exited, replacementPID := waitForDaemonExit(previous.PID, updateRestartWaitTimeout)
	if !exited { return fmt.Errorf("daemon pid %d is still running", previous.PID) }
	if replacementPID == 0 {
		if err := startUpdatedDaemon(binDir); err != nil { return err }
	}
	ping, err := waitForResponsiveDaemon(ctx, updateRestartWaitTimeout)
	if err != nil { return err }
	return requireUpdatedDaemonVersion(ping.Version, expectedVersion)
}
```

- [ ] **Step 7: Add cancellation tests before and after installation**

In `updateCmd.RunE`, derive the orchestration context from
`signal.NotifyContext(cmd.Context(), os.Interrupt)` and defer its stop
function. Track `session.Installed` immediately after `PerformUpdate` returns
nil. On context cancellation, call `session.release` unless shutdown already
owns the gate, then select the message from those two booleans:

```go
ctx, stopSignals := signal.NotifyContext(cmd.Context(), os.Interrupt)
defer stopSignals()

if err := ctx.Err(); err != nil {
	releaseCtx, cancelRelease := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancelRelease()
	_ = session.release(releaseCtx)
	if session.Installed && session.ShutdownOwned {
		return fmt.Errorf("binary installed; daemon is finishing shutdown — run roborev daemon restart")
	}
	if session.Installed {
		return fmt.Errorf("binary installed; daemon still running old version — run roborev daemon restart")
	}
	return err
}
```

Cancel the command context while waiting and assert release occurs. Cancel
after the injected installer reports success and assert the command returns
nonzero with exactly the recovery text and does not print the final `Updated`
line.

```go
assert.Contains(t, output,
	"binary installed; daemon still running old version — run roborev daemon restart")
assert.Error(t, err)
assert.Equal(t, 1, server.ReleaseCalls())
assert.NotContains(t, output, "Updated roborev to")
```

Add the shutdown-owned variant and assert the message says the daemon is finishing shutdown before the same recovery command.

- [ ] **Step 8: Run updater handoff tests**

Run: `go test ./internal/update ./cmd/roborev -run 'Test(UpdateContext|UpdateDaemon|PrepareUpdateDaemon|RestartAndVerify|NormalizeUpdateVersion)' -count=1`

Expected: PASS.

- [ ] **Step 9: Commit updater handoff behavior**

```bash
git add cmd/roborev/update_daemon.go cmd/roborev/update_daemon_test.go cmd/roborev/update.go internal/update/update.go internal/update/update_test.go
git commit -m "Verify daemon handoff during updates"
```

---

### Task 5: Wire policy selection and compact phase output

**Files:**

- Modify: `cmd/roborev/update.go:326-515`
- Modify: `cmd/roborev/main_test.go:780-1120`

**Interfaces:**

- Consumes: Task 4’s `runningReviewPolicy`, daemon-session orchestration, and context-aware `update.PerformUpdateWithReporter`.
- Produces: Cobra `--running` flag, `chooseRunningReviewPolicy`, stable phase output, and final success gating.

- [ ] **Step 1: Write failing command tests for parsing, defaults, and prompts**

Cover all three valid policies, invalid values before daemon mutation,
`--check` returning before daemon discovery, `--yes` defaulting to wait,
interactive no-running-review confirmation selecting wait for a cutover race,
`--no-restart` skipping status/prepare, interactive `a` returning nil, and
explicit abort-busy returning a sentinel nonzero error.

```go
func TestUpdateRunningFlag(t *testing.T) {
	cmd := updateCmd()
	flag := cmd.Flags().Lookup("running")
	require.NotNil(t, flag)
	assert.Equal(t, "", flag.DefValue)
	assert.Contains(t, flag.Usage, "wait, interrupt, or abort")
}

func TestChooseRunningReviewPolicyAbortIsSuccessfulCancellation(t *testing.T) {
	policy, confirmed, err := chooseRunningReviewPolicy(strings.NewReader("a\n"), io.Discard, 3)
	require.NoError(t, err)
	assert.Empty(t, policy)
	assert.False(t, confirmed)
}
```

- [ ] **Step 2: Run command tests and verify they fail**

Run: `go test ./cmd/roborev -run 'Test(UpdateRunningFlag|ChooseRunningReviewPolicy|UpdatePolicy)' -count=1`

Expected: FAIL because the flag and prompt helper do not exist.

- [ ] **Step 3: Add the policy flag and single-prompt confirmation flow**

Validate the raw flag before status, preparation, download, or install. Return
from `--check` before daemon discovery. When initial status has running reviews
and no explicit policy, use the combined prompt; otherwise retain `Proceed
with update? [y/N]`. After that normal confirmation, select `wait` so a review
that starts between status and preparation is preserved without another
prompt.

```go
func parseRunningReviewPolicy(raw string) (runningReviewPolicy, error) {
	switch runningReviewPolicy(raw) {
	case policyWait, policyInterrupt, policyAbort:
		return runningReviewPolicy(raw), nil
	case "":
		return "", nil
	default:
		return "", fmt.Errorf("invalid --running value %q: use wait, interrupt, or abort", raw)
	}
}

func chooseRunningReviewPolicy(in io.Reader, out io.Writer, running int) (
	runningReviewPolicy, bool, error,
) {
	fmt.Fprintf(out, "%d reviews are currently running.\n\n", running)
	fmt.Fprintln(out, "  [w] Wait for them to finish, then update")
	fmt.Fprintln(out, "  [u] Update now; interrupt and restart them automatically")
	fmt.Fprintln(out, "  [a] Abort")
	fmt.Fprint(out, "\nChoice [a]: ")
	var answer string
	_, err := fmt.Fscanln(in, &answer)
	if err != nil && !errors.Is(err, io.EOF) { return "", false, err }
	switch strings.ToLower(answer) {
	case "w", "wait": return policyWait, true, nil
	case "u", "update", "interrupt": return policyInterrupt, true, nil
	default: return "", false, nil
	}
}
```

Use a dedicated sentinel/typed error for `--running=abort` conflicts so Cobra exits nonzero, while interactive `[a]` prints `Update cancelled` and returns nil.

- [ ] **Step 4: Replace verbose sections with stable summary and phase rendering**

Add small CLI output helpers and a reporter whose `Stepf` intentionally emits
nothing because the command owns phase labels. Its `Progress` renders one
carriage-returned `Downloading` line, and `Finish` always emits a terminating
newline before `Installing`:

```go
type commandUpdateReporter struct {
	out io.Writer
	wroteProgress bool
}

func (r *commandUpdateReporter) Stepf(string, ...any) {}

func (r *commandUpdateReporter) Progress(downloaded, total int64) {
	if total <= 0 { return }
	r.wroteProgress = true
	fmt.Fprintf(r.out, "\r%-13s%d%% (%s)", "Downloading",
		downloaded*100/total, update.FormatSize(total))
}

func (r *commandUpdateReporter) Finish(total int64, success bool) {
	if r.wroteProgress {
		fmt.Fprintln(r.out)
		return
	}
	if success {
		printUpdatePhase(r.out, "Downloading", fmt.Sprintf("100%% (%s)", update.FormatSize(total)))
	}
}

func printUpdateSummary(out io.Writer, info *update.UpdateInfo, installPath string) {
	fmt.Fprintln(out, "Update available")
	fmt.Fprintf(out, "  Version  %s -> %s\n", info.CurrentVersion, info.LatestVersion)
	fmt.Fprintf(out, "  Package  %s (%s)\n", info.AssetName, update.FormatSize(info.Size))
	fmt.Fprintf(out, "  Install  %s\n", installPath)
	if verbose {
		fmt.Fprintf(out, "  URL      %s\n", info.DownloadURL)
		if info.Checksum != "" { fmt.Fprintf(out, "  SHA256   %s\n", info.Checksum) }
	}
}

func printUpdatePhase(out io.Writer, phase, result string) {
	fmt.Fprintf(out, "%-13s%s\n", phase, result)
}
```

The success path must print phases in this completion order:

```go
reporter := &commandUpdateReporter{out: out}
err := update.PerformUpdateWithReporter(operationCtx, info, reporter)
reporter.Finish(info.Size, err == nil)
if err != nil { return fmt.Errorf("update failed: %w", err) }
printUpdatePhase(out, "Installing", "done")
printUpdatePhase(out, "Daemon", "restarted ("+info.LatestVersion+")")
printUpdatePhase(out, "Git hooks", hookResult)
printUpdatePhase(out, "Skills", skillsResult)
fmt.Fprintf(out, "\nUpdated roborev to %s\n", info.LatestVersion)
```

If no daemon is running initially, rediscover it immediately before and after
installation. Prepare and drain a daemon found before installation; prepare,
drain, and restart one found afterward. Print `Daemon       not running` only
when both checks remain empty. Do not print the final success line if
restart/readiness/version verification fails. Keep hook and skill failures as
phase warnings after daemon verification.

- [ ] **Step 5: Add exact-output and ordering tests**

Assert the compact default block, verbose URL/checksum, progress newline, no-daemon phase, and ordering with string indexes.

```go
assert.Contains(t, output, "Downloading  100%")
assert.Contains(t, output, "Installing   done")
assert.Less(t, strings.Index(output, "Daemon       restarted"), strings.Index(output, "Git hooks"))
assert.Less(t, strings.Index(output, "Git hooks"), strings.Index(output, "Skills"))
assert.NotContains(t, normalOutput, "SHA256")
assert.Contains(t, verboseOutput, "SHA256")
```

- [ ] **Step 6: Run command tests**

Run: `go test ./cmd/roborev -run 'Test(Update|RestartDaemonAfterUpdate|RepairHooksAfterUpdate)' -count=1`

Expected: PASS.

- [ ] **Step 7: Commit the command UX**

```bash
git add cmd/roborev/update.go cmd/roborev/main_test.go
git commit -m "Add controlled update restart policies"
```

---

### Task 6: Surface drain recovery and document the behavior

**Files:**

- Modify: `cmd/roborev/status.go:120-175`
- Modify: `cmd/roborev/status_test.go`
- Modify: `docs/commands.md`

**Interfaces:**

- Consumes: `DaemonStatus.UpdateDraining`, `UpdateDrainPolicy`, and `UpdateDrainExpiresAt`.
- Produces: `formatUpdateDrainStatus(status storage.DaemonStatus, now time.Time) string` and published update-command documentation.

- [ ] **Step 1: Write failing status rendering tests**

Cover active wait, active interrupt, and expired interrupt recovery:

```go
func TestFormatUpdateDrainStatus(t *testing.T) {
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	assert.Equal(t, "update waiting (lease 40s)", formatUpdateDrainStatus(storage.DaemonStatus{
		UpdateDraining: true, UpdateDrainPolicy: "wait",
		UpdateDrainExpiresAt: now.Add(40 * time.Second).Format(time.RFC3339),
	}, now))
	assert.Equal(t, "update recovery (interrupt)", formatUpdateDrainStatus(storage.DaemonStatus{
		UpdateDraining: true, UpdateDrainPolicy: "interrupt",
		UpdateDrainExpiresAt: now.Add(-time.Second).Format(time.RFC3339),
	}, now))
}
```

- [ ] **Step 2: Run the status test and verify it fails**

Run: `go test ./cmd/roborev -run TestFormatUpdateDrainStatus -count=1`

Expected: FAIL because `formatUpdateDrainStatus` does not exist.

- [ ] **Step 3: Render the drain beside worker/queue state**

```go
func formatUpdateDrainStatus(status storage.DaemonStatus, now time.Time) string {
	if !status.UpdateDraining { return "" }
	expires, err := time.Parse(time.RFC3339, status.UpdateDrainExpiresAt)
	if err == nil && !expires.After(now) {
		return fmt.Sprintf("update recovery (%s)", status.UpdateDrainPolicy)
	}
	if err == nil {
		return fmt.Sprintf("update %s (lease %s)", status.UpdateDrainPolicy,
			expires.Sub(now).Round(time.Second))
	}
	return "update " + status.UpdateDrainPolicy
}
```

Use the injected/current `now` consistently rather than `time.Until` inside the helper, and append the returned text to the `Workers:` line after the existing `(paused)` marker.

- [ ] **Step 4: Update command documentation**

Add these user-facing facts to the `roborev update` section in `docs/commands.md`:

```markdown
- `--running=wait` prevents new reviews from starting and waits for active reviews.
- `--running=interrupt` cleanly stops active attempts and requeues them without consuming a retry.
- `--running=abort` updates only when the daemon atomically confirms no reviews are running; a busy result exits nonzero.
- Without `--running`, interactive updates prompt when reviews are active; `--yes` defaults to `wait`.
- `--no-restart` skips daemon preparation and restart.

The daemon continues accepting enqueues while an update drain is active, but
does not claim them until the replacement daemon is ready. User cancellation is
terminal; update interruption starts a fresh attempt and discards the partial
attempt log. A non-interactive wait has no updater-specific deadline and is
bounded by job timeouts (30 minutes by default).
```

Document the three-line policy prompt and the compact phase output verbatim from the design spec.

- [ ] **Step 5: Format docs and run focused tests**

Run: `make markdown`

Expected: `docs/commands.md` is wrapped consistently.

Run: `go test ./cmd/roborev -run 'Test(Status|FormatUpdateDrainStatus)' -count=1`

Expected: PASS.

- [ ] **Step 6: Commit status and documentation**

```bash
git add cmd/roborev/status.go cmd/roborev/status_test.go docs/commands.md
git commit -m "Document update drain status and policies"
```

---

### Task 7: Prove the full cutover in an isolated integration test

**Files:**

- Modify: `cmd/roborev/daemon_integration_test.go`

**Interfaces:**

- Consumes: the real daemon endpoints, worker pool, scratch runtime/data directory, and updater handoff helpers from Tasks 2–5.
- Produces: an integration test proving active and newly enqueued work cannot overlap the replacement boundary.

- [ ] **Step 1: Add the isolated two-job integration test**

Use `t.TempDir()` and `ROBOREV_DATA_DIR`, a synthetic blocking agent, and a test-built daemon binary outside the user PATH. Run wait and interrupt subtests. For each policy:

```go
func TestUpdateDrainCutoverIntegration(t *testing.T) {
	for _, policy := range []runningReviewPolicy{policyWait, policyInterrupt} {
		t.Run(string(policy), func(t *testing.T) {
			dataDir := setupIsolatedDataDir(t)
			oldBin := buildVersionedTestDaemon(t, dataDir, "roborev-old", "v-old")
			newBin := buildVersionedTestDaemon(t, dataDir, "roborev-new", "v-new")
			old := startIsolatedDaemon(t, oldBin, dataDir)
			active := enqueueSyntheticJob(t, old)
			waitForJobStatus(t, old, active.ID, storage.JobStatusRunning)

			lease := prepareIsolatedUpdate(t, old, policy)
			queued := enqueueSyntheticJob(t, old)
			assertJobRemainsQueued(t, old, queued.ID, 250*time.Millisecond)

			waitForPreparedDrain(t, lease)
			replacement := restartIsolatedDaemon(t, old, newBin, dataDir)

			assertDaemonVersion(t, replacement, "v-new")
			waitForJobStatus(t, replacement, active.ID, storage.JobStatusDone)
			waitForJobStatus(t, replacement, queued.ID, storage.JobStatusDone)
			assert.Equal(t, 0, jobRetryCount(t, replacement, active.ID))
		})
	}
}
```

Add these package-local helpers in the same integration test file, following
the subprocess/runtime pattern already used by `TestDaemonLifecycleEndToEnd`:

```go
type isolatedDaemon struct {
	Cmd *exec.Cmd
	Endpoint daemon.DaemonEndpoint
	Done <-chan error
}

func buildVersionedTestDaemon(t *testing.T, dir, name, version string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if runtime.GOOS == "windows" { path += ".exe" }
	cmd := exec.Command("go", "build", "-tags", "kit_posthog_disabled",
		"-ldflags", "-X go.kenn.io/roborev/internal/version.Version="+version,
		"-o", path, ".")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "build test daemon: %s", out)
	return path
}
```

Define `startIsolatedDaemon(t, binary, dataDir) isolatedDaemon` to run
`daemon run --db <dataDir>/reviews.db --config <dataDir>/config.toml --addr
127.0.0.1:0`, wait for its runtime record, and register process cleanup.
Define `enqueueSyntheticJob`, `waitForJobStatus`, `assertJobRemainsQueued`,
`prepareIsolatedUpdate`, `restartIsolatedDaemon`, `assertDaemonVersion`, and
`jobRetryCount` as HTTP helpers against `isolatedDaemon.Endpoint`; each request
must decode the real endpoint response and use `require`/`assert` on observable
status. Build separate `v-old` and `v-new` binaries so version verification is
real. Configure the built-in `test` agent with a 2-second job delay in the
isolated config; this creates a deterministic active interval without invoking
an external agent. The test must observe behavior through HTTP/DB state, not by
searching source text. Keep both binaries and all runtime state under the
temporary directory.

- [ ] **Step 2: Run the integration test**

Run: `go test -tags=integration ./cmd/roborev -run TestUpdateDrainCutoverIntegration -count=1 -v`

Expected: PASS for both `wait` and `interrupt`; the second job stays queued until replacement readiness, and the interrupted job finishes with unchanged retry count.

- [ ] **Step 3: Commit the integration coverage**

```bash
git add cmd/roborev/daemon_integration_test.go
git commit -m "Test update drain cutover end to end"
```

---

### Task 8: Run repository quality gates and review the final diff

**Files:**

- Inspect: all files changed in Tasks 1–7

**Interfaces:**

- Consumes: all prior task deliverables.
- Produces: verified, formatted, generated-code-clean implementation ready for review.

- [ ] **Step 1: Run formatting and generated-code checks**

Run: `gofmt -w internal/storage/jobs.go internal/storage/models.go internal/storage/db_job_test.go internal/daemon/worker.go internal/daemon/worker_classify.go internal/daemon/synthesis_worker.go internal/daemon/worker_update_interrupt_test.go internal/daemon/update_drain.go internal/daemon/update_drain_test.go internal/daemon/server.go internal/daemon/routes.go internal/daemon/types.go internal/daemon/shutdown_test.go internal/update/update.go internal/update/update_test.go cmd/roborev/update_daemon.go cmd/roborev/update_daemon_test.go cmd/roborev/update.go cmd/roborev/main_test.go cmd/roborev/status.go cmd/roborev/status_test.go cmd/roborev/daemon_integration_test.go`

Expected: command exits zero.

Run: `make api-check && make markdown-ci`

Expected: both checks PASS without modifying files.

- [ ] **Step 2: Run focused package tests**

Run: `go test ./internal/storage ./internal/daemon ./cmd/roborev -count=1`

Expected: PASS.

- [ ] **Step 3: Run repository-wide tests, build, and non-mutating hooks**

Run: `go test ./... -count=1`

Expected: PASS.

Run: `CGO_ENABLED=0 go build ./...`

Expected: PASS without installing a binary.

Run: `make lint-ci && prek run --all-files`

Expected: PASS.

- [ ] **Step 4: Inspect scope and privacy**

Run: `git status --short && git diff --stat origin/main...HEAD && git diff --check && git diff origin/main...HEAD -- docs/commands.md cmd/roborev/update.go internal/daemon/update_drain.go internal/daemon/worker.go internal/storage/jobs.go`

Expected: only update-restart files are changed, no whitespace errors, no updater-side state file/lock/schema/dependency change, and no private identifiers or machine paths appear.

- [ ] **Step 5: Run the explicit design-contract checklist**

Confirm from tests and diff:

```text
[ ] same owner + same policy is idempotent
[ ] same owner + different policy returns 409
[ ] different unexpired owner returns 409 with remaining duration
[ ] no targeted job still running is the interrupt completion condition
[ ] requeue WHERE includes status='running' AND worker_id=?
[ ] normal shutdown ownership prevents update release from opening the gate
[ ] old-daemon fallback still verifies replacement readiness and version
[ ] Daemon phase precedes Git hooks and Skills
[ ] interactive abort exits zero; explicit busy abort exits nonzero
[ ] no updater-side operation record or data-directory lock exists
```

Expected: every item is backed by a named automated test.

- [ ] **Step 6: Commit any formatting-only corrections from the gates**

If the gates changed tracked files, commit only those related corrections:

```bash
git add internal/storage internal/daemon cmd/roborev pkg/client docs/commands.md
git commit -m "Polish controlled update restart implementation"
```

If `git status --short` is empty, do not create an empty commit.
