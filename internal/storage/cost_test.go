package storage

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetCostAggregate(t *testing.T) {
	assert := assert.New(t)
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })

	repo := createRepo(t, db, "/tmp/cost-repo")

	mkJob := func(sha, branch string, status JobStatus, costJSON string) *ReviewJob {
		commit := createCommit(t, db, repo.ID, sha)
		job := enqueueJob(t, db, repo.ID, commit.ID, sha)
		setJobStatus(t, db, job.ID, status)
		// Done/failed/canceled rows represent an agent that actually ran, so
		// they carry the agent_invoked marker and count toward cost eligibility.
		// Priced rows also get it via seedCost below; this covers the unpriced
		// ones.
		switch status {
		case JobStatusDone, JobStatusFailed, JobStatusCanceled:
			setJobAgentInvoked(t, db, job.ID)
		}
		if branch != "" {
			setJobBranch(t, db, job.ID, branch)
		}
		if costJSON != "" {
			seedCost(t, db, job.ID, costJSON)
		}
		return job
	}

	// Eligible jobs on branch "feat":
	mkJob("sha-done", "feat", JobStatusDone, `{"cost_usd":1.00,"has_cost":true}`)           // priced
	mkJob("sha-failed", "feat", JobStatusFailed, "")                                        // eligible, unpriced
	mkJob("sha-cancel-run", "feat", JobStatusCanceled, `{"cost_usd":0.50,"has_cost":true}`) // priced
	// Eligible + priced on empty branch:
	mkJob("sha-empty", "", JobStatusDone, `{"cost_usd":0.25,"has_cost":true}`)

	// Ineligible jobs:
	mkJob("sha-running", "feat", JobStatusRunning, "") // no finished_at
	mkJob("sha-queued", "feat", JobStatusQueued, "")   // never ran

	skipped := mkJob("sha-skip", "feat", JobStatusDone, `{"cost_usd":9.99,"has_cost":true}`)
	_, err := db.Exec(`UPDATE review_jobs SET status='skipped' WHERE id=?`, skipped.ID)
	require.NoError(t, err)

	cq := mkJob("sha-cancel-queue", "feat", JobStatusQueued, "")
	_, err = db.Exec(`UPDATE review_jobs SET status='canceled', started_at=NULL, finished_at=datetime('now') WHERE id=?`, cq.ID)
	require.NoError(t, err)

	// Whole repo: 4 eligible (done, failed, cancel-run, empty), 3 priced, $1.75.
	all, err := db.GetCostAggregate(CostOptions{RepoPaths: []string{repo.RootPath}})
	require.NoError(t, err)
	assert.Equal(4, all.JobsTotal)
	assert.Equal(3, all.JobsWithCost)
	assert.InDelta(1.75, all.TotalUSD, 0.0001)
	assert.False(all.Complete)

	// Branch "feat": 3 eligible, 2 priced, $1.50.
	feat, err := db.GetCostAggregate(CostOptions{RepoPaths: []string{repo.RootPath}, Branch: "feat"})
	require.NoError(t, err)
	assert.Equal(3, feat.JobsTotal)
	assert.Equal(2, feat.JobsWithCost)
	assert.InDelta(1.50, feat.TotalUSD, 0.0001)

	// Empty branch only: 1 eligible, 1 priced, $0.25, complete.
	empty, err := db.GetCostAggregate(CostOptions{RepoPaths: []string{repo.RootPath}, BranchEmpty: true})
	require.NoError(t, err)
	assert.Equal(1, empty.JobsTotal)
	assert.Equal(1, empty.JobsWithCost)
	assert.InDelta(0.25, empty.TotalUSD, 0.0001)
	assert.True(empty.Complete)

	// Empty scope: no eligible jobs.
	none, err := db.GetCostAggregate(CostOptions{RepoPaths: []string{"/tmp/does-not-exist"}})
	require.NoError(t, err)
	assert.Equal(0, none.JobsTotal)
	assert.False(none.Complete)
}

// TestGetCostAggregateExcludesNoAgentRows verifies a terminal row that never
// invoked an agent (a panel synthesis passthrough, an all-failed/all-passed
// synthesis, or a job that failed before any agent was available) is left out
// of the denominator, so coverage is not dragged below 100% by a row that can
// never report cost.
func TestGetCostAggregateExcludesNoAgentRows(t *testing.T) {
	assert := assert.New(t)
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })

	repo := createRepo(t, db, "/tmp/cost-no-agent")

	// An agent ran and reported cost (seedCost sets the agent_invoked marker).
	agentJob := enqueueJob(t, db, repo.ID,
		createCommit(t, db, repo.ID, "agent-sha").ID, "agent-sha")
	setJobStatus(t, db, agentJob.ID, JobStatusDone)
	seedCost(t, db, agentJob.ID, `{"cost_usd":0.50,"has_cost":true}`)

	// A no-agent synthesis row: terminal and finished, but no agent_invoked
	// marker and no token usage. It can never report cost, so it must not be
	// counted.
	noAgent := enqueueJob(t, db, repo.ID,
		createCommit(t, db, repo.ID, "passthrough-sha").ID, "passthrough-sha")
	setJobStatus(t, db, noAgent.ID, JobStatusDone)

	got, err := db.GetCostAggregate(CostOptions{RepoPaths: []string{repo.RootPath}})
	require.NoError(t, err)
	assert.Equal(1, got.JobsTotal, "no-agent row excluded from the denominator")
	assert.Equal(1, got.JobsWithCost)
	assert.InDelta(0.50, got.TotalUSD, 0.0001)
	assert.True(got.Complete, "coverage is complete despite the no-agent row")
}

func TestGetCostAggregateMultiRepo(t *testing.T) {
	assert := assert.New(t)
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })

	mkJob := func(repoID int64, sha, costJSON string) {
		commit := createCommit(t, db, repoID, sha)
		job := enqueueJob(t, db, repoID, commit.ID, sha)
		setJobStatus(t, db, job.ID, JobStatusDone)
		setJobBranch(t, db, job.ID, "feat")
		seedCost(t, db, job.ID, costJSON)
	}

	repoA := createRepo(t, db, "/tmp/cost-multi-a")
	repoB := createRepo(t, db, "/tmp/cost-multi-b")
	mkJob(repoA.ID, "multi-a-sha", `{"cost_usd":1.50,"has_cost":true}`)
	mkJob(repoB.ID, "multi-b-sha", `{"cost_usd":2.00,"has_cost":true}`)

	// Both repos on branch "feat": 2 eligible, 2 priced, $3.50, complete.
	both, err := db.GetCostAggregate(CostOptions{
		RepoPaths: []string{repoA.RootPath, repoB.RootPath},
		Branch:    "feat",
	})
	require.NoError(t, err)
	assert.Equal(2, both.JobsTotal)
	assert.Equal(2, both.JobsWithCost)
	assert.InDelta(3.50, both.TotalUSD, 0.0001)
	assert.True(both.Complete)

	// Scoping to one repo proves the IN clause filters.
	onlyA, err := db.GetCostAggregate(CostOptions{
		RepoPaths: []string{repoA.RootPath},
		Branch:    "feat",
	})
	require.NoError(t, err)
	assert.InDelta(1.50, onlyA.TotalUSD, 0.0001)
}

func TestGetCostAggregateIncludesPanelMembers(t *testing.T) {
	assert := assert.New(t)
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })

	repo := createRepo(t, db, "/tmp/cost-panel")
	commit := createCommit(t, db, repo.ID, "panel-sha")

	mkPanelJob := func(role string, costJSON string) *ReviewJob {
		job, err := db.EnqueueJob(EnqueueOpts{
			RepoID: repo.ID, CommitID: commit.ID, GitRef: "panel-sha", Agent: "test",
			PanelRunUUID: "run-1", PanelRole: role,
		})
		require.NoError(t, err)
		setJobStatus(t, db, job.ID, JobStatusDone)
		seedCost(t, db, job.ID, costJSON)
		return job
	}

	mkPanelJob("member", `{"cost_usd":0.30,"has_cost":true}`)
	mkPanelJob("member", `{"cost_usd":0.40,"has_cost":true}`)
	mkPanelJob("synthesis", `{"cost_usd":0.10,"has_cost":true}`)

	got, err := db.GetCostAggregate(CostOptions{RepoPaths: []string{repo.RootPath}})
	require.NoError(t, err)
	assert.Equal(3, got.JobsTotal, "members + synthesis all counted")
	assert.Equal(3, got.JobsWithCost)
	assert.InDelta(0.80, got.TotalUSD, 0.0001)
	assert.True(got.Complete)
}

// TestGetCostAggregateIncludesPricedRowsWithoutMarker guards the agentRanByUsage
// fallback: a priced row carrying neither the agent_invoked marker nor a command
// line — historical, backfilled, or synced from a machine that wrote it before
// the marker existed — still proves an agent ran via its cost JSON, so it must
// count toward coverage.
func TestGetCostAggregateIncludesPricedRowsWithoutMarker(t *testing.T) {
	assert := assert.New(t)
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })

	repo := createRepo(t, db, "/tmp/cost-no-marker")
	commit := createCommit(t, db, repo.ID, "no-marker-sha")
	job := enqueueJob(t, db, repo.ID, commit.ID, "no-marker-sha")
	setJobStatus(t, db, job.ID, JobStatusDone)

	// Priced row, but the agent_invoked marker was never set. seedCost would
	// set it, so write the usage directly against the session.
	sessionID := "sess-no-marker"
	setJobSession(t, db, job.ID, sessionID)
	require.NoError(t, db.SaveJobTokenUsage(
		job.ID, sessionID, `{"cost_usd":0.75,"has_cost":true}`))

	c, err := db.GetCostAggregate(CostOptions{RepoPaths: []string{repo.RootPath}})
	require.NoError(t, err)
	assert.Equal(1, c.JobsTotal, "priced row counts even without the agent_invoked marker")
	assert.Equal(1, c.JobsWithCost)
	assert.InDelta(0.75, c.TotalUSD, 0.0001)
	assert.True(c.Complete, "coverage is complete when the only row is priced")
}

// TestGetCostAggregateRerunClearsStaleCost guards against attributing a prior
// run's spend to a re-run job. Re-enqueuing must clear token_usage and the
// agent_invoked marker, so a second run that reports no cost is counted as
// eligible-but-unpriced, not as priced.
func TestGetCostAggregateRerunClearsStaleCost(t *testing.T) {
	assert := assert.New(t)
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })

	repo := createRepo(t, db, "/tmp/cost-rerun")
	commit := createCommit(t, db, repo.ID, "rerun-sha")
	job := enqueueJob(t, db, repo.ID, commit.ID, "rerun-sha")

	// First run completes with reported cost.
	setJobStatus(t, db, job.ID, JobStatusDone)
	seedCost(t, db, job.ID, `{"cost_usd":5.00,"has_cost":true}`)

	opts := CostOptions{RepoPaths: []string{repo.RootPath}}
	before, err := db.GetCostAggregate(opts)
	require.NoError(t, err)
	assert.Equal(1, before.JobsWithCost, "first run is priced")
	assert.InDelta(5.00, before.TotalUSD, 0.0001)

	// Re-enqueue: this clears the prior attempt's cost and agent-ran marker.
	require.NoError(t, db.ReenqueueJob(job.ID, ReenqueueOpts{}))
	assert.False(getJobAgentInvoked(t, db, job.ID),
		"rerun clears the prior attempt's agent-ran marker")

	// The second run's agent runs (agent_invoked set) but reports no cost.
	setJobStatus(t, db, job.ID, JobStatusDone)
	setJobAgentInvoked(t, db, job.ID)

	after, err := db.GetCostAggregate(opts)
	require.NoError(t, err)
	assert.Equal(1, after.JobsTotal, "still eligible after rerun")
	assert.Equal(0, after.JobsWithCost, "stale cost cleared on rerun")
	assert.InDelta(0.0, after.TotalUSD, 0.0001)
	assert.False(after.Complete)
}

// TestSaveJobTokenUsageIgnoresStaleSession guards the late-write race: a
// delayed token-usage fetch from a completed attempt must not stamp its cost
// onto the row after a rerun cleared it and a new attempt took over under a
// different session.
func TestSaveJobTokenUsageIgnoresStaleSession(t *testing.T) {
	assert := assert.New(t)
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })

	repo := createRepo(t, db, "/tmp/cost-stale-session")
	commit := createCommit(t, db, repo.ID, "stale-sha")
	job := enqueueJob(t, db, repo.ID, commit.ID, "stale-sha")

	// Attempt A runs under "sess-A" and completes; its usage write lands.
	setJobStatus(t, db, job.ID, JobStatusDone)
	setJobSession(t, db, job.ID, "sess-A")
	require.NoError(t, db.SaveJobTokenUsage(
		job.ID, "sess-A", `{"cost_usd":5.00,"has_cost":true}`))

	// The job is re-enqueued (clearing session_id + token_usage) and a new
	// attempt claims it under "sess-B".
	require.NoError(t, db.ReenqueueJob(job.ID, ReenqueueOpts{}))
	setJobSession(t, db, job.ID, "sess-B")

	// Attempt A's delayed usage fetch finally writes back, keyed by the old
	// session. It must not land.
	require.NoError(t, db.SaveJobTokenUsage(
		job.ID, "sess-A", `{"cost_usd":5.00,"has_cost":true}`))
	reloaded, err := db.GetJobByID(job.ID)
	require.NoError(t, err)
	assert.Empty(reloaded.TokenUsage, "stale write from the prior session is ignored")

	// A write under the current session still updates.
	require.NoError(t, db.SaveJobTokenUsage(
		job.ID, "sess-B", `{"cost_usd":1.00,"has_cost":true}`))
	reloaded, err = db.GetJobByID(job.ID)
	require.NoError(t, err)
	assert.Contains(reloaded.TokenUsage, "1.00", "current-session write lands")
}

// TestResetStaleJobsClearsCostMetadata verifies restart recovery does not
// carry a prior attempt's session id or cost into the requeued run.
func TestResetStaleJobsClearsCostMetadata(t *testing.T) {
	assert := assert.New(t)
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })

	repo := createRepo(t, db, "/tmp/reset-stale")
	commit := createCommit(t, db, repo.ID, "reset-sha")
	job := enqueueJob(t, db, repo.ID, commit.ID, "reset-sha")

	// A running job that already carries a session id, the agent_invoked marker,
	// and a cost (e.g. from a late write that landed on the running re-attempt).
	setJobStatus(t, db, job.ID, JobStatusRunning)
	setJobSession(t, db, job.ID, "sess-stale")
	setJobAgentInvoked(t, db, job.ID)
	require.NoError(t, db.SaveJobTokenUsage(
		job.ID, "sess-stale", `{"cost_usd":3.00,"has_cost":true}`))

	require.NoError(t, db.ResetStaleJobs())

	reloaded, err := db.GetJobByID(job.ID)
	require.NoError(t, err)
	assert.Equal(JobStatusQueued, reloaded.Status, "running job requeued")
	assert.Empty(reloaded.SessionID, "session id cleared on restart recovery")
	assert.Empty(reloaded.TokenUsage, "stale cost cleared on restart recovery")
	assert.Empty(reloaded.CommandLine, "stale command line cleared on restart recovery")
	assert.False(getJobAgentInvoked(t, db, job.ID),
		"stale agent-ran marker cleared on restart recovery")
}

// TestGetCostAggregateExcludesPreAgentFailure verifies a job that reached a
// terminal failed state before any agent ran — a pre-agent gate failure such as
// an oversized prompt or a worktree-creation error — is left out of the
// denominator. The worker never reached MarkJobAgentInvoked, so the marker is
// unset and no usage was captured; the row can never report cost and must not
// drag coverage below 100%.
func TestGetCostAggregateExcludesPreAgentFailure(t *testing.T) {
	assert := assert.New(t)
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })

	repo := createRepo(t, db, "/tmp/cost-pre-agent-fail")

	// A priced row that did run an agent (seedCost sets the marker).
	ran := enqueueJob(t, db, repo.ID,
		createCommit(t, db, repo.ID, "ran-sha").ID, "ran-sha")
	setJobStatus(t, db, ran.ID, JobStatusDone)
	seedCost(t, db, ran.ID, `{"cost_usd":0.40,"has_cost":true}`)

	// A job that failed before an agent ran: terminal and finished, but with no
	// agent_invoked marker and no token usage.
	failed := enqueueJob(t, db, repo.ID,
		createCommit(t, db, repo.ID, "gate-fail-sha").ID, "gate-fail-sha")
	setJobStatus(t, db, failed.ID, JobStatusFailed)

	got, err := db.GetCostAggregate(CostOptions{RepoPaths: []string{repo.RootPath}})
	require.NoError(t, err)
	assert.Equal(1, got.JobsTotal, "pre-agent failure excluded from the denominator")
	assert.Equal(1, got.JobsWithCost)
	assert.InDelta(0.40, got.TotalUSD, 0.0001)
	assert.True(got.Complete, "coverage complete despite the pre-agent failure")
}

// TestGetCostAggregateCountsPulledUnpricedJob verifies the agent_invoked marker
// survives a pull from another machine and that a pulled terminal row which ran
// an agent but reported no cost still counts toward the denominator. command_line
// is not synced, so without the synced marker such a row (no usage locally) would
// be invisible to cost coverage. A pulled row that never ran an agent stays out.
func TestGetCostAggregateCountsPulledUnpricedJob(t *testing.T) {
	assert := assert.New(t)
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })

	repo, err := db.GetOrCreateRepo("/test/repo-pulled-unpriced")
	require.NoError(t, err)

	ran := time.Now().UTC()

	// A terminal row pulled from another machine that ran an agent but reported
	// no cost. UpsertPulledJob writes the synced agent_invoked marker.
	invoked := PulledJob{
		UUID:            "pulled-invoked-uuid",
		RepoIdentity:    "/test/repo-pulled-unpriced",
		GitRef:          "HEAD",
		Agent:           "codex",
		Status:          string(JobStatusDone),
		AgentInvoked:    true,
		StartedAt:       &ran,
		FinishedAt:      &ran,
		SourceMachineID: "machine-a",
		EnqueuedAt:      ran,
		UpdatedAt:       ran,
	}
	require.NoError(t, db.UpsertPulledJob(invoked, repo.ID, nil))

	// A terminal row that never ran an agent (no marker, no usage) stays out of
	// the denominator even though it synced from the same machine.
	noAgent := invoked
	noAgent.UUID = "pulled-no-agent-uuid"
	noAgent.AgentInvoked = false
	require.NoError(t, db.UpsertPulledJob(noAgent, repo.ID, nil))

	got, err := db.GetCostAggregate(CostOptions{RepoPaths: []string{repo.RootPath}})
	require.NoError(t, err)
	assert.Equal(1, got.JobsTotal, "pulled agent-invoked row counts; no-agent row excluded")
	assert.Equal(0, got.JobsWithCost, "row ran an agent but reported no cost")
	assert.InDelta(0.0, got.TotalUSD, 0.0001)
	assert.False(got.Complete, "coverage is incomplete with an unpriced eligible row")
}
