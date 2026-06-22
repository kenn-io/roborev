package storage

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hasReviewJobsColumn reports whether review_jobs has the named column.
func hasReviewJobsColumn(t *testing.T, db *DB, name string) bool {
	t.Helper()
	var count int
	err := db.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('review_jobs') WHERE name = ?`, name,
	).Scan(&count)
	require.NoError(t, err)
	return count > 0
}

func TestPanelColumnsMigration(t *testing.T) {
	assert := assert.New(t)
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })

	for _, col := range []string{
		"panel_run_uuid", "panel_role", "panel_name",
		"panel_member_name", "panel_member_index",
		"panel_member_config_json", "claim_blocked",
	} {
		assert.True(hasReviewJobsColumn(t, db, col), "missing column %q", col)
	}

	// claim_blocked defaults to 0 and is NOT NULL.
	var dflt string
	var notNull int
	err := db.QueryRow(`
		SELECT COALESCE("dflt_value", ''), "notnull"
		FROM pragma_table_info('review_jobs') WHERE name = 'claim_blocked'
	`).Scan(&dflt, &notNull)
	require.NoError(t, err)
	assert.Equal("0", dflt)
	assert.Equal(1, notNull)

	// The composite index exists.
	var idxCount int
	err = db.QueryRow(`
		SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'index' AND name = 'idx_review_jobs_panel'
	`).Scan(&idxCount)
	require.NoError(t, err)
	assert.Equal(1, idxCount)
}

func TestPanelColumnsMigrationIdempotent(t *testing.T) {
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })

	// Running migrate() again must not error (ALTER guarded by pragma probe,
	// CREATE INDEX IF NOT EXISTS).
	require.NoError(t, db.migrate())
	require.NoError(t, db.migrate())
}

func TestPanelColumnsRoundTrip(t *testing.T) {
	assert := assert.New(t)
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })

	repo := createRepo(t, db, "/tmp/panel-rt")
	commit := createCommit(t, db, repo.ID, "abc123")

	member, err := db.EnqueueJob(EnqueueOpts{
		RepoID:                repo.ID,
		CommitID:              commit.ID,
		GitRef:                "abc123",
		Agent:                 "test",
		JobType:               JobTypeReview,
		ReviewType:            "security",
		PanelRunUUID:          "run-1",
		PanelRole:             "member",
		PanelName:             "branch_final",
		PanelMemberName:       "security",
		PanelMemberIndex:      1,
		PanelMemberConfigJSON: `{"agent":"test","review_type":"security"}`,
	})
	require.NoError(t, err)
	assert.Equal("run-1", member.PanelRunUUID)
	assert.Equal("member", member.PanelRole)
	assert.Equal("branch_final", member.PanelName)
	assert.Equal("security", member.PanelMemberName)
	assert.Equal(1, member.PanelMemberIndex)
	assert.JSONEq(`{"agent":"test","review_type":"security"}`, member.PanelMemberConfigJSON)
	assert.False(member.ClaimBlocked, "members are never claim_blocked")

	// GetJobByID round-trips.
	got, err := db.GetJobByID(member.ID)
	require.NoError(t, err)
	assert.Equal("run-1", got.PanelRunUUID)
	assert.Equal("member", got.PanelRole)
	assert.Equal(1, got.PanelMemberIndex)
	assert.JSONEq(`{"agent":"test","review_type":"security"}`, got.PanelMemberConfigJSON)

	// ListJobs round-trips the role (used to exclude members later).
	jobs, err := db.ListJobs("", "", 0, 0)
	require.NoError(t, err)
	require.Len(t, jobs, 1)
	assert.Equal("member", jobs[0].PanelRole)
	assert.Equal("run-1", jobs[0].PanelRunUUID)
}

func TestJobTypeHelpersForSynthesis(t *testing.T) {
	assert := assert.New(t)

	synth := ReviewJob{JobType: JobTypeSynthesis}
	assert.True(synth.IsSynthesisJob(), "synthesis IsSynthesisJob")
	assert.False(synth.IsReviewJob(), "synthesis must not be a review job")
	assert.False(synth.IsTaskJob(), "synthesis must not be a task job")
	assert.False(synth.UsesStoredPrompt(), "synthesis must not be a stored-prompt job")
	assert.False(synth.IsFixJob(), "synthesis must not be a fix job")

	// A done synthesis job has viewable output (status-keyed, type-agnostic).
	synth.Status = JobStatusDone
	assert.True(synth.HasViewableOutput(), "done synthesis HasViewableOutput")

	// A member keeps its natural review type and stays a review job.
	member := ReviewJob{JobType: JobTypeRange, PanelRole: "member"}
	assert.True(member.IsReviewJob(), "member is still a review job")
	assert.False(member.IsSynthesisJob(), "member is not synthesis")
}

func TestClaimJobSkipsClaimBlocked(t *testing.T) {
	assert := assert.New(t)
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })

	repo := createRepo(t, db, "/tmp/panel-gate")
	commit := createCommit(t, db, repo.ID, "abc123")

	// A gated synthesis job (claim_blocked=1) plus a claimable member.
	_, err := db.EnqueueJob(EnqueueOpts{
		RepoID: repo.ID, GitRef: "abc123..abc123", Agent: "test",
		JobType: JobTypeSynthesis, PanelRunUUID: "run-1", PanelRole: "synthesis",
		ClaimBlocked: true,
	})
	require.NoError(t, err)
	member, err := db.EnqueueJob(EnqueueOpts{
		RepoID: repo.ID, CommitID: commit.ID, GitRef: "abc123", Agent: "test",
		JobType: JobTypeReview, PanelRunUUID: "run-1", PanelRole: "member", PanelMemberIndex: 0,
	})
	require.NoError(t, err)

	// First claim must return the member, never the gated synthesis job.
	claimed, err := db.ClaimJob("worker-1")
	require.NoError(t, err)
	require.NotNil(t, claimed)
	assert.Equal(member.ID, claimed.ID)

	// No further claimable jobs (synthesis is still gated).
	none, err := db.ClaimJob("worker-2")
	require.NoError(t, err)
	assert.Nil(none, "gated synthesis job must not be claimed")
}

// enqueuePanelRun seeds a synthesis job (claim_blocked=1) plus n members
// for a run, returning the synthesis job and the member jobs.
func enqueuePanelRun(t *testing.T, db *DB, repoID int64, runUUID string, n int) (*ReviewJob, []*ReviewJob) {
	t.Helper()
	synth, err := db.EnqueueJob(EnqueueOpts{
		RepoID: repoID, GitRef: "base..head", Agent: "test",
		JobType: JobTypeSynthesis, PanelRunUUID: runUUID, PanelRole: "synthesis",
		PanelName: "branch_final", ClaimBlocked: true,
	})
	require.NoError(t, err)
	members := make([]*ReviewJob, n)
	for i := range n {
		m, err := db.EnqueueJob(EnqueueOpts{
			RepoID: repoID, GitRef: "base..head", Agent: "test",
			JobType: JobTypeRange, PanelRunUUID: runUUID, PanelRole: "member",
			PanelMemberName: "m", PanelMemberIndex: i,
		})
		require.NoError(t, err)
		members[i] = m
	}
	return synth, members
}

func setStatus(t *testing.T, db *DB, jobID int64, status JobStatus) {
	t.Helper()
	_, err := db.Exec(`UPDATE review_jobs SET status = ? WHERE id = ?`, string(status), jobID)
	require.NoError(t, err)
}

func claimBlockedOf(t *testing.T, db *DB, jobID int64) bool {
	t.Helper()
	var cb int
	require.NoError(t, db.QueryRow(`SELECT claim_blocked FROM review_jobs WHERE id = ?`, jobID).Scan(&cb))
	return cb != 0
}

func TestMaybeReleasePanelSynthesis(t *testing.T) {
	tests := []struct {
		name      string
		statuses  []JobStatus
		wantClear bool
	}{
		{"all done releases", []JobStatus{JobStatusDone, JobStatusDone}, true},
		{"all failed releases", []JobStatus{JobStatusFailed, JobStatusFailed}, true},
		{"all canceled releases", []JobStatus{JobStatusCanceled, JobStatusCanceled}, true},
		{"mixed terminal releases", []JobStatus{JobStatusDone, JobStatusFailed, JobStatusSkipped}, true},
		{"applied and rebased releases", []JobStatus{JobStatusApplied, JobStatusRebased}, true},
		{"one running blocks", []JobStatus{JobStatusDone, JobStatusRunning}, false},
		{"one queued blocks", []JobStatus{JobStatusDone, JobStatusQueued}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := openTestDB(t)
			t.Cleanup(func() { db.Close() })
			repo := createRepo(t, db, "/tmp/release-"+tt.name)
			synth, members := enqueuePanelRun(t, db, repo.ID, "run-"+tt.name, len(tt.statuses))
			for i, st := range tt.statuses {
				setStatus(t, db, members[i].ID, st)
			}

			require.NoError(t, db.MaybeReleasePanelSynthesis("run-"+tt.name))

			assert.Equal(t, !tt.wantClear, claimBlockedOf(t, db, synth.ID),
				"claim_blocked after release")
		})
	}
}

func TestMaybeReleasePanelSynthesisIdempotent(t *testing.T) {
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })
	repo := createRepo(t, db, "/tmp/release-idem")
	synth, members := enqueuePanelRun(t, db, repo.ID, "run-idem", 2)
	setStatus(t, db, members[0].ID, JobStatusDone)
	setStatus(t, db, members[1].ID, JobStatusDone)

	// Calling repeatedly is a no-op after the first release.
	require.NoError(t, db.MaybeReleasePanelSynthesis("run-idem"))
	require.NoError(t, db.MaybeReleasePanelSynthesis("run-idem"))
	require.NoError(t, db.MaybeReleasePanelSynthesis("run-idem"))
	assert.False(t, claimBlockedOf(t, db, synth.ID))
}

func TestMaybeReleasePanelSynthesisUnknownRun(t *testing.T) {
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })
	// No rows for this run — must be a clean no-op.
	require.NoError(t, db.MaybeReleasePanelSynthesis("does-not-exist"))
}

func TestApplyJobVerdictForSynthesis(t *testing.T) {
	assert := assert.New(t)
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })

	repo := createRepo(t, db, "/tmp/panel-verdict")
	synth, err := db.EnqueueJob(EnqueueOpts{
		RepoID: repo.ID, GitRef: "abc123..abc123", Agent: "test",
		JobType: JobTypeSynthesis, PanelRunUUID: "run-1", PanelRole: "synthesis",
	})
	require.NoError(t, err)

	// Move to running so CompleteJob persists the review + verdict.
	_, err = db.Exec(`UPDATE review_jobs SET status='running', worker_id='w1' WHERE id=?`, synth.ID)
	require.NoError(t, err)
	require.NoError(t, db.CompleteJob(synth.ID, "test", "prompt", "Critical — boom"))

	got, err := db.GetJobByID(synth.ID)
	require.NoError(t, err)
	jobs, err := db.ListJobs("", "", 0, 0)
	require.NoError(t, err)
	require.Len(t, jobs, 1)
	// ListJobs applies the verdict; synthesis must be verdict-bearing.
	require.NotNil(t, jobs[0].Verdict, "synthesis verdict must be set")
	assert.Equal("F", *jobs[0].Verdict)
	assert.Equal(JobTypeSynthesis, got.JobType)
}

func TestWithPanelRunAndExcludeMembers(t *testing.T) {
	assert := assert.New(t)
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })
	repo := createRepo(t, db, "/tmp/panel-list")
	synth, members := enqueuePanelRun(t, db, repo.ID, "run-1", 2)
	// A plain non-panel review in the same repo.
	commit := createCommit(t, db, repo.ID, "plain1")
	plain, err := db.EnqueueJob(EnqueueOpts{RepoID: repo.ID, CommitID: commit.ID, GitRef: "plain1", Agent: "test", JobType: JobTypeReview})
	require.NoError(t, err)

	// Default-style listing excludes members -> synthesis + plain only.
	parentsOnly, err := db.ListJobs("", "", 0, 0, WithExcludePanelRole("member"))
	require.NoError(t, err)
	ids := map[int64]bool{}
	for _, j := range parentsOnly {
		ids[j.ID] = true
	}
	assert.True(ids[synth.ID], "synthesis present")
	assert.True(ids[plain.ID], "plain review present")
	assert.False(ids[members[0].ID], "member 0 excluded")
	assert.False(ids[members[1].ID], "member 1 excluded")

	// WithPanelRun returns exactly the run's members + synthesis.
	runJobs, err := db.ListJobs("", "", 0, 0, WithPanelRun("run-1"))
	require.NoError(t, err)
	assert.Len(runJobs, 3) // 2 members + synthesis
	for _, j := range runJobs {
		assert.Equal("run-1", j.PanelRunUUID)
	}
}

func TestGetPanelMembersOrdered(t *testing.T) {
	assert := assert.New(t)
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })
	repo := createRepo(t, db, "/tmp/panel-members")
	enqueuePanelRun(t, db, repo.ID, "run-1", 3)

	got, err := db.GetPanelMembers("run-1")
	require.NoError(t, err)
	require.Len(t, got, 3)
	assert.Equal(0, got[0].PanelMemberIndex)
	assert.Equal(1, got[1].PanelMemberIndex)
	assert.Equal(2, got[2].PanelMemberIndex)
	for _, m := range got {
		assert.Equal("member", m.PanelRole)
	}

	// Unknown run returns empty (synthesis row is not a member).
	none, err := db.GetPanelMembers("no-such-run")
	require.NoError(t, err)
	assert.Empty(none)
}

func TestPanelIndexSurvivesLegacyRebuild(t *testing.T) {
	// A legacy DB (status CHECK without 'skipped', no skip_reason) triggers
	// migrateReviewJobsConstraintsForAutoDesign's table rebuild (DROP+RENAME).
	// The panel index must be created AFTER that rebuild, or it is dropped
	// until the next daemon restart. The panel columns must also survive.
	db := prepareMigratedDB(t, "panel_legacy.db", legacyReviewJobSchema, legacyReviewJobSeed)

	var idxCount int
	require.NoError(t, db.QueryRow(`
		SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'index' AND name = 'idx_review_jobs_panel'
	`).Scan(&idxCount))
	assert.Equal(t, 1, idxCount, "idx_review_jobs_panel must survive the legacy table rebuild")

	for _, col := range []string{
		"panel_run_uuid", "panel_role", "panel_name",
		"panel_member_name", "panel_member_index",
		"panel_member_config_json", "claim_blocked",
	} {
		assert.True(t, hasReviewJobsColumn(t, db, col), "panel column %q lost in rebuild", col)
	}
}

func TestReviewHydrationIncludesPanelFields(t *testing.T) {
	assert := assert.New(t)
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })
	repo := createRepo(t, db, "/tmp/panel-review-hydrate")
	commit := createCommit(t, db, repo.ID, "abc123")

	member, err := db.EnqueueJob(EnqueueOpts{
		RepoID: repo.ID, CommitID: commit.ID, GitRef: "abc123", Agent: "test",
		JobType: JobTypeReview, PanelRunUUID: "run-1", PanelRole: "member",
		PanelName: "branch_final", PanelMemberName: "security", PanelMemberIndex: 3,
		PanelMemberConfigJSON: `{"agent":"test"}`,
	})
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE review_jobs SET status='running', worker_id='w1' WHERE id=?`, member.ID)
	require.NoError(t, err)
	require.NoError(t, db.CompleteJob(member.ID, "test", "prompt", "No issues found."))

	// The by-id hydration paths reach a member directly and must round-trip its
	// panel fields.
	assertMember := func(name string, job *ReviewJob) {
		require.NotNil(t, job, name)
		assert.Equal("run-1", job.PanelRunUUID, name)
		assert.Equal("member", job.PanelRole, name)
		assert.Equal("branch_final", job.PanelName, name)
		assert.Equal("security", job.PanelMemberName, name)
		assert.Equal(3, job.PanelMemberIndex, name)
		assert.JSONEq(`{"agent":"test"}`, job.PanelMemberConfigJSON, name)
	}

	byJob, err := db.GetReviewByJobID(member.ID)
	require.NoError(t, err)
	assertMember("GetReviewByJobID", byJob.Job)

	batch, err := db.GetJobsWithReviewsByIDs([]int64{member.ID})
	require.NoError(t, err)
	jw, ok := batch[member.ID]
	require.True(t, ok, "job missing from batch result")
	assertMember("GetJobsWithReviewsByIDs", &jw.Job)

	// SHA resolution targets the synthesis (canonical) review, not a member,
	// and must hydrate the synthesis panel fields.
	synth, err := db.EnqueueJob(EnqueueOpts{
		RepoID: repo.ID, CommitID: commit.ID, GitRef: "abc123", Agent: "test",
		JobType: JobTypeSynthesis, PanelRunUUID: "run-1", PanelRole: "synthesis",
		PanelName: "branch_final",
	})
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE review_jobs SET status='running', worker_id='w1' WHERE id=?`, synth.ID)
	require.NoError(t, err)
	require.NoError(t, db.CompleteJob(synth.ID, "test", "prompt", "Synthesized."))

	bySHA, err := db.GetReviewByCommitSHA("abc123")
	require.NoError(t, err)
	require.NotNil(t, bySHA.Job)
	assert.Equal(synth.ID, bySHA.Job.ID, "SHA resolves to the synthesis, not a member")
	assert.Equal("run-1", bySHA.Job.PanelRunUUID)
	assert.Equal("synthesis", bySHA.Job.PanelRole)
	assert.Equal("branch_final", bySHA.Job.PanelName)
}

func TestGetPanelSummaries(t *testing.T) {
	assert := assert.New(t)
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })
	repo := createRepo(t, db, "/tmp/panel-summary")

	// Run A: 3 members — done, failed, skipped (all terminal; 1 succeeded).
	_, a := enqueuePanelRun(t, db, repo.ID, "run-A", 3)
	setStatus(t, db, a[0].ID, JobStatusDone)
	setStatus(t, db, a[1].ID, JobStatusFailed)
	setStatus(t, db, a[2].ID, JobStatusSkipped)
	seedCost(t, db, a[0].ID, `{"cost_usd":0.10,"has_cost":true}`)
	seedCost(t, db, a[1].ID, `{"cost_usd":0.25,"has_cost":true}`)
	seedCost(t, db, a[2].ID, `{"cost_usd":0.05,"has_cost":true}`)

	// Run B: 2 members — done + canceled (all terminal; 1 succeeded).
	_, b := enqueuePanelRun(t, db, repo.ID, "run-B", 2)
	setStatus(t, db, b[0].ID, JobStatusDone)
	setStatus(t, db, b[1].ID, JobStatusCanceled)
	seedCost(t, db, b[0].ID, `{"cost_usd":0.20,"has_cost":true}`)

	// Run C: 2 members — done + running (1 terminal of 2).
	_, c := enqueuePanelRun(t, db, repo.ID, "run-C", 2)
	setStatus(t, db, c[0].ID, JobStatusDone)
	setStatus(t, db, c[1].ID, JobStatusRunning)

	// run-D has no members enqueued, so it must be absent from the result.
	got, err := db.GetPanelSummaries([]string{"run-A", "run-B", "run-C", "run-D"})
	require.NoError(t, err)

	sumA := got["run-A"]
	assert.Equal(3, sumA.MembersTotal)
	assert.Equal(3, sumA.MembersTerminal)
	assert.Equal(1, sumA.MembersSucceeded)
	assert.Equal(1, sumA.MembersFailed)
	assert.Equal(0, sumA.MembersCanceled)
	assert.Equal(1, sumA.MembersSkipped)
	assert.Equal(3, sumA.MembersWithCost)
	assert.True(sumA.MembersCostComplete)
	assert.InDelta(0.40, sumA.MembersCostUSD, 0.000001)

	sumB := got["run-B"]
	assert.Equal(2, sumB.MembersTotal)
	assert.Equal(2, sumB.MembersTerminal)
	assert.Equal(1, sumB.MembersSucceeded)
	assert.Equal(1, sumB.MembersCanceled)
	assert.Equal(0, sumB.MembersFailed)
	assert.Equal(0, sumB.MembersSkipped)
	assert.Equal(1, sumB.MembersWithCost)
	assert.False(sumB.MembersCostComplete, "partial member cost is not complete")
	assert.InDelta(0.20, sumB.MembersCostUSD, 0.000001)

	sumC := got["run-C"]
	assert.Equal(2, sumC.MembersTotal)
	assert.Equal(1, sumC.MembersTerminal) // running is not terminal
	assert.Equal(1, sumC.MembersSucceeded)
	assert.Equal(0, sumC.MembersFailed)
	assert.Equal(0, sumC.MembersCanceled)
	assert.Equal(0, sumC.MembersSkipped)
	assert.Equal(0, sumC.MembersWithCost)
	assert.False(sumC.MembersCostComplete)

	// A run with no member rows is absent from the map (not a zero-value entry).
	_, hasD := got["run-D"]
	assert.False(hasD, "run-D has no members and must be absent")

	// Empty input is a clean no-op.
	none, err := db.GetPanelSummaries(nil)
	require.NoError(t, err)
	assert.Empty(none)
}

func TestGetJobsToSyncIncludesPanelColumns(t *testing.T) {
	assert := assert.New(t)
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })
	machineID, err := db.GetMachineID()
	require.NoError(t, err)

	repo := createRepo(t, db, "/tmp/panel-sync")
	commit := createCommit(t, db, repo.ID, "abc123")
	member, err := db.EnqueueJob(EnqueueOpts{
		RepoID: repo.ID, CommitID: commit.ID, GitRef: "abc123", Agent: "test",
		JobType: JobTypeReview, PanelRunUUID: "run-1", PanelRole: "member",
		PanelName: "branch_final", PanelMemberName: "security", PanelMemberIndex: 2,
		PanelMemberConfigJSON: `{"agent":"test"}`,
	})
	require.NoError(t, err)
	// Only terminal jobs sync.
	setStatus(t, db, member.ID, JobStatusDone)

	jobs, err := db.GetJobsToSync(machineID, 100)
	require.NoError(t, err)
	require.Len(t, jobs, 1)
	s := jobs[0]
	assert.Equal("run-1", s.PanelRunUUID)
	assert.Equal("member", s.PanelRole)
	assert.Equal("branch_final", s.PanelName)
	assert.Equal("security", s.PanelMemberName)
	assert.Equal(2, s.PanelMemberIndex)
	assert.JSONEq(`{"agent":"test"}`, s.PanelMemberConfigJSON)
}

func TestUpsertPulledJobRoundTripsPanelColumns(t *testing.T) {
	assert := assert.New(t)
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })
	repo := createRepo(t, db, "/tmp/panel-pull")

	pulled := PulledJob{
		UUID:                  "uuid-1",
		GitRef:                "abc123",
		Agent:                 "test",
		Reasoning:             "thorough",
		JobType:               JobTypeReview,
		Status:                "done",
		EnqueuedAt:            time.Now(),
		UpdatedAt:             time.Now(),
		SourceMachineID:       "remote-machine",
		PanelRunUUID:          "run-9",
		PanelRole:             "member",
		PanelName:             "branch_final",
		PanelMemberName:       "design",
		PanelMemberIndex:      4,
		PanelMemberConfigJSON: `{"agent":"test","review_type":"design"}`,
	}
	require.NoError(t, db.UpsertPulledJob(pulled, repo.ID, nil))

	var got ReviewJob
	var cb int
	row := db.QueryRow(`
		SELECT COALESCE(panel_run_uuid,''), COALESCE(panel_role,''), COALESCE(panel_name,''),
		       COALESCE(panel_member_name,''), panel_member_index, COALESCE(panel_member_config_json,''),
		       COALESCE(claim_blocked,0)
		FROM review_jobs WHERE uuid = 'uuid-1'
	`)
	require.NoError(t, row.Scan(&got.PanelRunUUID, &got.PanelRole, &got.PanelName,
		&got.PanelMemberName, &got.PanelMemberIndex, &got.PanelMemberConfigJSON, &cb))
	assert.Equal("run-9", got.PanelRunUUID)
	assert.Equal("member", got.PanelRole)
	assert.Equal("design", got.PanelMemberName)
	assert.Equal(4, got.PanelMemberIndex)
	assert.JSONEq(`{"agent":"test","review_type":"design"}`, got.PanelMemberConfigJSON)
	// claim_blocked is local-only and never set by sync ingest.
	assert.Equal(0, cb)
}

func TestGetSynthesisJob(t *testing.T) {
	assert := assert.New(t)
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })
	repo := createRepo(t, db, "/tmp/panel-get-synth")
	synth, _ := enqueuePanelRun(t, db, repo.ID, "run-1", 2)

	got, err := db.GetSynthesisJob("run-1")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(synth.ID, got.ID)
	assert.Equal(PanelRoleSynthesis, got.PanelRole)
	assert.Equal("run-1", got.PanelRunUUID)

	// Empty uuid is a clean (nil, nil).
	none, err := db.GetSynthesisJob("")
	require.NoError(t, err)
	assert.Nil(none)

	// Unknown uuid is a clean (nil, nil).
	none, err = db.GetSynthesisJob("no-such-run")
	require.NoError(t, err)
	assert.Nil(none)
}

func TestGetPanelMemberReviews(t *testing.T) {
	assert := assert.New(t)
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })
	repo := createRepo(t, db, "/tmp/panel-member-reviews")

	synth, err := db.EnqueueJob(EnqueueOpts{
		RepoID: repo.ID, GitRef: "base..head", Agent: "test",
		JobType: JobTypeSynthesis, PanelRunUUID: "run-1", PanelRole: PanelRoleSynthesis,
		ClaimBlocked: true,
	})
	require.NoError(t, err)

	// Three members inserted out of index order (2, 0, 1).
	indices := []int{2, 0, 1}
	byIndex := make(map[int]*ReviewJob, len(indices))
	for _, idx := range indices {
		m, err := db.EnqueueJob(EnqueueOpts{
			RepoID: repo.ID, GitRef: "base..head", Agent: "test",
			JobType: JobTypeReview, PanelRunUUID: "run-1", PanelRole: PanelRoleMember,
			PanelMemberName: "m", PanelMemberIndex: idx,
		})
		require.NoError(t, err)
		byIndex[idx] = m
	}

	// Member 0 has a completed review so its output/status surface.
	m0 := byIndex[0]
	_, err = db.Exec(`UPDATE review_jobs SET status='running', worker_id='w1' WHERE id=?`, m0.ID)
	require.NoError(t, err)
	require.NoError(t, db.CompleteJob(m0.ID, "test", "prompt", "No issues found."))

	got, err := db.GetPanelMemberReviews("run-1")
	require.NoError(t, err)
	require.Len(t, got, 3)

	// Ordered by panel_member_index; the synthesis row is excluded.
	assert.Equal(byIndex[0].ID, got[0].JobID)
	assert.Equal(byIndex[1].ID, got[1].JobID)
	assert.Equal(byIndex[2].ID, got[2].JobID)
	for _, r := range got {
		assert.NotEqual(synth.ID, r.JobID, "synthesis row must be excluded")
	}

	// The completed member surfaces its review output and done status.
	assert.Equal("done", got[0].Status)
	assert.Equal("No issues found.", got[0].Output)
	// A member without a review has an empty output but its own status.
	assert.Empty(got[1].Output)

	// Empty uuid is a clean (nil, nil).
	none, err := db.GetPanelMemberReviews("")
	require.NoError(t, err)
	assert.Nil(none)
}

func TestListStuckPanelRuns(t *testing.T) {
	assert := assert.New(t)
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })
	repo := createRepo(t, db, "/tmp/panel-stuck")

	// (a) all members terminal + synthesis still claim_blocked=1 -> stuck.
	stuckSynth, stuckMembers := enqueuePanelRun(t, db, repo.ID, "run-stuck", 2)
	setStatus(t, db, stuckMembers[0].ID, JobStatusDone)
	setStatus(t, db, stuckMembers[1].ID, JobStatusFailed)
	require.True(t, claimBlockedOf(t, db, stuckSynth.ID))

	// (b) a still-running member -> not stuck (synthesis legitimately blocked).
	_, runningMembers := enqueuePanelRun(t, db, repo.ID, "run-running", 2)
	setStatus(t, db, runningMembers[0].ID, JobStatusDone)
	setStatus(t, db, runningMembers[1].ID, JobStatusRunning)

	// (c) synthesis already released (claim_blocked=0) -> not stuck.
	relSynth, relMembers := enqueuePanelRun(t, db, repo.ID, "run-released", 2)
	setStatus(t, db, relMembers[0].ID, JobStatusDone)
	setStatus(t, db, relMembers[1].ID, JobStatusDone)
	require.NoError(t, db.MaybeReleasePanelSynthesis("run-released"))
	require.False(t, claimBlockedOf(t, db, relSynth.ID))

	got, err := db.ListStuckPanelRuns()
	require.NoError(t, err)
	assert.Equal([]string{"run-stuck"}, got)
}

func TestEnqueuePanelRunAtomic(t *testing.T) {
	assert := assert.New(t)
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })
	repo := createRepo(t, db, "/tmp/panel-enqueue-atomic")

	const n = 3
	members := make([]EnqueueOpts, n)
	for i := range n {
		members[i] = EnqueueOpts{
			RepoID: repo.ID, GitRef: "base..head", Agent: "test",
			JobType: JobTypeRange, PanelRunUUID: "run-1", PanelRole: PanelRoleMember,
			PanelMemberName: "m", PanelMemberIndex: i,
		}
	}
	synthesis := EnqueueOpts{
		RepoID: repo.ID, GitRef: "base..head", Agent: "test",
		JobType: JobTypeSynthesis, PanelRunUUID: "run-1", PanelRole: PanelRoleSynthesis,
		PanelName: "branch_final", ClaimBlocked: true,
	}

	gotMembers, gotSynth, err := db.EnqueuePanelRun(members, synthesis)
	require.NoError(t, err)
	require.Len(t, gotMembers, n)
	require.NotNil(t, gotSynth)

	// All rows share the panel_run_uuid.
	for i, m := range gotMembers {
		assert.Equal("run-1", m.PanelRunUUID)
		assert.Equal(PanelRoleMember, m.PanelRole)
		assert.Equal(i, m.PanelMemberIndex, "member order preserved")
	}
	assert.Equal("run-1", gotSynth.PanelRunUUID)
	assert.Equal(PanelRoleSynthesis, gotSynth.PanelRole)

	// The synthesis row is gated in the DB.
	assert.True(claimBlockedOf(t, db, gotSynth.ID), "synthesis must be claim_blocked")

	// GetPanelMembers returns exactly n.
	persisted, err := db.GetPanelMembers("run-1")
	require.NoError(t, err)
	assert.Len(persisted, n)

	// Total rows for the run is n+1.
	var count int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM review_jobs WHERE panel_run_uuid = ?`, "run-1").Scan(&count))
	assert.Equal(n+1, count)
}

// TestEnqueuePanelRunEnforcesSynthesisGate verifies EnqueuePanelRun enforces the
// panel-run invariants even when the caller omits them: the synthesis row is
// stored gated (claim_blocked=1, job_type=synthesis, panel_role=synthesis) and
// members are stored with panel_role=member. Without enforcement a forgotten
// gate would let the synthesis be claimed and run before its members exist.
func TestEnqueuePanelRunEnforcesSynthesisGate(t *testing.T) {
	assert := assert.New(t)
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })
	repo := createRepo(t, db, "/tmp/panel-enqueue-gate")

	// Caller "forgets" the gate: synthesis omits ClaimBlocked/JobType/PanelRole,
	// member omits PanelRole.
	members := []EnqueueOpts{{
		RepoID: repo.ID, GitRef: "base..head", Agent: "test",
		JobType: JobTypeRange, PanelRunUUID: "run-gate", PanelMemberIndex: 0,
	}}
	synthesis := EnqueueOpts{
		RepoID: repo.ID, GitRef: "base..head", Agent: "test",
		PanelRunUUID: "run-gate", PanelName: "p",
	}

	gotMembers, gotSynth, err := db.EnqueuePanelRun(members, synthesis)
	require.NoError(t, err)
	require.Len(t, gotMembers, 1)
	require.NotNil(t, gotSynth)

	// Synthesis gate enforced in the persisted row.
	assert.True(claimBlockedOf(t, db, gotSynth.ID), "synthesis must be gated even if caller omits ClaimBlocked")
	var jobType, role string
	require.NoError(t, db.QueryRow(
		`SELECT job_type, COALESCE(panel_role, '') FROM review_jobs WHERE id = ?`, gotSynth.ID).
		Scan(&jobType, &role))
	assert.Equal(JobTypeSynthesis, jobType)
	assert.Equal(PanelRoleSynthesis, role)

	// Member stored with panel_role=member even though the caller omitted it.
	var memberRole string
	require.NoError(t, db.QueryRow(
		`SELECT COALESCE(panel_role, '') FROM review_jobs WHERE id = ?`, gotMembers[0].ID).
		Scan(&memberRole))
	assert.Equal(PanelRoleMember, memberRole)

	// The returned structs reflect the enforced values too.
	assert.Equal(PanelRoleSynthesis, gotSynth.PanelRole)
	assert.True(gotSynth.ClaimBlocked)
	assert.Equal(PanelRoleMember, gotMembers[0].PanelRole)
}

// failingExecer wraps a real execer and returns an error on the failAt-th
// (1-based) ExecContext call, so a panel-run transaction can be forced to fail
// mid-flight against a real SQLite transaction.
type failingExecer struct {
	inner  execer
	calls  int
	failAt int
}

func (f *failingExecer) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	f.calls++
	if f.calls == f.failAt {
		return nil, errors.New("injected insert failure")
	}
	return f.inner.ExecContext(ctx, query, args...)
}

func TestEnqueuePanelRunRollback(t *testing.T) {
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })
	repo := createRepo(t, db, "/tmp/panel-enqueue-rollback")

	const n = 2
	members := make([]EnqueueOpts, n)
	for i := range n {
		members[i] = EnqueueOpts{
			RepoID: repo.ID, GitRef: "base..head", Agent: "test",
			JobType: JobTypeRange, PanelRunUUID: "run-roll", PanelRole: PanelRoleMember,
			PanelMemberName: "m", PanelMemberIndex: i,
		}
	}
	synthesis := EnqueueOpts{
		RepoID: repo.ID, GitRef: "base..head", Agent: "test",
		JobType: JobTypeSynthesis, PanelRunUUID: "run-roll", PanelRole: PanelRoleSynthesis,
		ClaimBlocked: true,
	}

	ctx := context.Background()
	// Resolve the machine id before opening the write transaction: GetMachineID
	// writes on a pooled connection, which would deadlock against the dedicated
	// BEGIN IMMEDIATE lock until the busy timeout fires.
	machineID, _ := db.GetMachineID()

	conn, err := db.Conn(ctx)
	require.NoError(t, err)
	defer conn.Close()

	_, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE")
	require.NoError(t, err)

	// Fail on the synthesis insert (call n+1), after the members inserted.
	failing := &failingExecer{inner: conn, failAt: n + 1}
	_, _, err = db.enqueuePanelRunTx(ctx, failing, members, synthesis, machineID, time.Now())
	require.Error(t, err, "forced failure must surface")

	_, rbErr := conn.ExecContext(ctx, "ROLLBACK")
	require.NoError(t, rbErr)

	// The members inserted in calls 1..n are rolled back with the synthesis.
	var count int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM review_jobs WHERE panel_run_uuid = ?`, "run-roll").Scan(&count))
	assert.Equal(t, 0, count, "rolled-back run must leave zero rows")
}

func TestSynthBlockedIndexExists(t *testing.T) {
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })

	// Running migrate() again must be a clean no-op (CREATE INDEX IF NOT EXISTS).
	require.NoError(t, db.migrate())

	var idxCount int
	require.NoError(t, db.QueryRow(`
		SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'index' AND name = 'idx_review_jobs_synth_blocked'
	`).Scan(&idxCount))
	assert.Equal(t, 1, idxCount, "idx_review_jobs_synth_blocked must exist exactly once")
}

// TestGetReviewByCommitSHAExcludesPanelMembers verifies SHA resolution lands on
// the synthesis (canonical) review and never an individual member, even when
// only members have completed (synthesis pending or failed) — members and
// synthesis share the frozen git_ref.
func TestGetReviewByCommitSHAExcludesPanelMembers(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	repo := createRepo(t, db, "/tmp/sha-excl-members")

	synth, members := enqueuePanelRun(t, db, repo.ID, "run-sha", 2)

	complete := func(jobID int64, output string) {
		setStatus(t, db, jobID, JobStatusRunning)
		require.NoError(t, db.CompleteJob(jobID, "test", "p", output))
	}

	// Only members have reviews yet (synthesis still pending or failed). The
	// SHA-resolution path must not surface a member as the review for the SHA.
	complete(members[0].ID, "member 0 output")
	complete(members[1].ID, "member 1 output")

	_, err := db.GetReviewByCommitSHA("base..head")
	require.ErrorIs(t, err, sql.ErrNoRows,
		"a member review must not resolve as the canonical review for a SHA")

	// Once synthesis completes, it is the canonical review for the SHA.
	complete(synth.ID, "synthesis output")
	rev, err := db.GetReviewByCommitSHA("base..head")
	require.NoError(t, err)
	assert.Equal(t, synth.ID, rev.JobID, "SHA resolves to the synthesis job")
	assert.Equal(t, "synthesis output", rev.Output)
}

// TestGetReviewByCommitSHAPendingSynthesisHidesStaleReview verifies that when a
// newer panel synthesis for a ref has no review row yet (queued/failed), SHA
// resolution returns sql.ErrNoRows ("synthesis pending") rather than a stale
// older standalone review for the same ref.
func TestGetReviewByCommitSHAPendingSynthesisHidesStaleReview(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	repo := createRepo(t, db, "/tmp/sha-pending-synth")
	commit := createCommit(t, db, repo.ID, "abc123")

	// An OLDER completed standalone review for the ref.
	older := createCompletedJobWithOptions(t, db, EnqueueOpts{
		RepoID:   repo.ID,
		CommitID: commit.ID,
		GitRef:   "abc123",
		Agent:    "test",
	}, "older standalone output")

	// Sanity: with only the older review present, it resolves.
	rev, err := db.GetReviewByCommitSHA("abc123")
	require.NoError(t, err)
	assert.Equal(t, older.ID, rev.JobID)

	// A NEWER panel run for the same frozen ref. The synthesis has no review
	// (left queued); members complete but must not surface for the SHA.
	synth, members := enqueuePanelRun(t, db, repo.ID, "run-pending", 2)
	// Pin the panel rows to the same git_ref as the standalone review.
	_, err = db.Exec(`UPDATE review_jobs SET git_ref = 'abc123' WHERE panel_run_uuid = 'run-pending'`)
	require.NoError(t, err)
	complete := func(jobID int64, output string) {
		setStatus(t, db, jobID, JobStatusRunning)
		require.NoError(t, db.CompleteJob(jobID, "test", "p", output))
	}
	complete(members[0].ID, "member 0 output")
	complete(members[1].ID, "member 1 output")
	// Synthesis is the newest non-member job but has no review yet (failed).
	setStatus(t, db, synth.ID, JobStatusFailed)

	_, err = db.GetReviewByCommitSHA("abc123")
	require.ErrorIs(t, err, sql.ErrNoRows,
		"a pending synthesis must hide the stale older standalone review")
}
