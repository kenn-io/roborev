package storage

import (
	"context"
	"database/sql"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// boolCount returns how many elements of s equal v. Used by the race test to
// assert that exactly one concurrent CreateCIPanelRun caller wins.
func boolCount(s []bool, v bool) int {
	n := 0
	for _, b := range s {
		if b == v {
			n++
		}
	}
	return n
}

// countRepoJobs returns the number of review_jobs rows for a repo.
func countRepoJobs(t *testing.T, db *DB, repoID int64) int {
	t.Helper()
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM review_jobs WHERE repo_id = ?`, repoID).Scan(&n)
	require.NoError(t, err, "count review_jobs")
	return n
}

// countCIPanels returns the number of ci_pr_panels rows for a (repo, pr).
func countCIPanels(t *testing.T, db *DB, githubRepo string, prNumber int) int {
	t.Helper()
	var n int
	err := db.QueryRow(
		`SELECT COUNT(*) FROM ci_pr_panels WHERE github_repo = ? AND pr_number = ?`,
		githubRepo, prNumber).Scan(&n)
	require.NoError(t, err, "count ci_pr_panels")
	return n
}

// insertTestCIPanel inserts a ci_pr_panels row via raw SQL and returns its id.
// Production creation is owned by CreateCIPanelRun (task A3); this helper only
// seeds rows so the read queries can be exercised in isolation.
func insertTestCIPanel(t *testing.T, db *DB, githubRepo string, prNumber int, headSHA, panelRunUUID string, synthesisJobID int64) int64 {
	t.Helper()
	res, err := db.Exec(
		`INSERT INTO ci_pr_panels (github_repo, pr_number, head_sha, panel_run_uuid, synthesis_job_id) VALUES (?,?,?,?,?)`,
		githubRepo, prNumber, headSHA, panelRunUUID, synthesisJobID)
	require.NoError(t, err, "insert ci_pr_panels row")
	id, err := res.LastInsertId()
	require.NoError(t, err, "get inserted ci_pr_panels id")
	return id
}

// seedPanelRow inserts a bare ci_pr_panels row (no jobs needed for posting-claim
// tests) and returns its id.
func seedPanelRow(t *testing.T, db *DB, githubRepo string, pr int, headSHA string) int64 {
	t.Helper()
	res, err := db.Exec(`INSERT INTO ci_pr_panels (github_repo, pr_number, head_sha, panel_run_uuid, created_at)
		VALUES (?, ?, ?, ?, datetime('now'))`, githubRepo, pr, headSHA, "run-"+headSHA)
	require.NoError(t, err)
	id, err := res.LastInsertId()
	require.NoError(t, err)
	return id
}

func TestClaimPanelForPosting(t *testing.T) {
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })
	id := seedPanelRow(t, db, "o/r", 7, "h")
	staleWindow := 5 * time.Minute

	got1, err := db.ClaimPanelForPosting(id, staleWindow)
	require.NoError(t, err)
	assert.True(t, got1, "first claim wins")
	got2, err := db.ClaimPanelForPosting(id, staleWindow)
	require.NoError(t, err)
	assert.False(t, got2, "fresh existing claim is not reclaimable")

	require.NoError(t, db.ReleasePanelPostClaim(id))
	got3, err := db.ClaimPanelForPosting(id, staleWindow)
	require.NoError(t, err)
	assert.True(t, got3, "re-claimable after release")

	require.NoError(t, db.MarkPanelPosted(id))
	got4, err := db.ClaimPanelForPosting(id, staleWindow)
	require.NoError(t, err)
	assert.False(t, got4, "posted row never claims again")
}

// TestClaimPanelForPostingStaleReclaim proves the stale-window reclaim works,
// which directly guards timestamp-format correctness: backdating the claim to be
// older than staleWindow makes it reclaimable, while a fresh claim does not.
func TestClaimPanelForPostingStaleReclaim(t *testing.T) {
	assert := assert.New(t)
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })
	id := seedPanelRow(t, db, "o/r", 8, "stale")
	staleWindow := 5 * time.Minute

	got1, err := db.ClaimPanelForPosting(id, staleWindow)
	require.NoError(t, err)
	assert.True(got1, "first claim wins")

	// A fresh claim (no backdating) is not reclaimable.
	gotFresh, err := db.ClaimPanelForPosting(id, staleWindow)
	require.NoError(t, err)
	assert.False(gotFresh, "fresh claim is not reclaimable")

	// Force the claim to be older than the stale window: a crashed poster's
	// lease becomes reclaimable.
	_, err = db.Exec("UPDATE ci_pr_panels SET posting_claimed_at = datetime('now','-10 minutes') WHERE id = ?", id)
	require.NoError(t, err)
	gotStale, err := db.ClaimPanelForPosting(id, staleWindow)
	require.NoError(t, err)
	assert.True(gotStale, "stale lease is reclaimed")
}

// TestClaimPanelForPostingRace covers F3: N concurrent posters for one panel row
// produce exactly one winner, guaranteeing a single PR comment per run.
func TestClaimPanelForPostingRace(t *testing.T) {
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })
	id := seedPanelRow(t, db, "o/r", 9, "race")

	const n = 8
	results := make([]bool, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // release all goroutines together to maximize contention
			results[i], errs[i] = db.ClaimPanelForPosting(id, 5*time.Minute)
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		require.NoError(t, err, "goroutine %d", i)
	}
	assert.Equal(t, 1, boolCount(results, true), "exactly one poster wins")
}

// TestGetPendingPanelPRsAndDelete covers F13: GetPendingPanelPRs returns only
// the distinct (github_repo, pr_number) pairs with un-posted panel runs for the
// queried repo, and DeleteCIPanel removes a single mapping row. The query is
// DISTINCT with no ORDER BY, so results are compared order-independently.
func TestGetPendingPanelPRsAndDelete(t *testing.T) {
	assert := assert.New(t)
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })

	// PR 1 posted (excluded), PR 2 + PR 3 pending (included). A row under a
	// different repo proves the github_repo filter excludes it.
	postedID := seedPanelRow(t, db, "o/r", 1, "sha1")
	require.NoError(t, db.MarkPanelPosted(postedID))
	pendingID2 := seedPanelRow(t, db, "o/r", 2, "sha2")
	seedPanelRow(t, db, "o/r", 3, "sha3")
	seedPanelRow(t, db, "x/y", 9, "sha9")

	refs, err := db.GetPendingPanelPRs("o/r")
	require.NoError(t, err)

	got := make([]int, 0, len(refs))
	for _, r := range refs {
		assert.Equal("o/r", r.GithubRepo, "only the queried repo is returned")
		got = append(got, r.PRNumber)
	}
	sort.Ints(got)
	assert.Equal([]int{2, 3}, got, "only pending PRs for o/r")

	// Deleting a pending row removes its PR from the pending set.
	require.NoError(t, db.DeleteCIPanel(pendingID2))
	refs, err = db.GetPendingPanelPRs("o/r")
	require.NoError(t, err)
	got = got[:0]
	for _, r := range refs {
		got = append(got, r.PRNumber)
	}
	assert.NotContains(got, 2, "deleted PR no longer pending")
	assert.Equal([]int{3}, got, "only PR 3 remains pending")
}

// TestGetActivePanelsForPR covers supersede + closed-PR cleanup support: the
// query returns only the un-posted (posted_at IS NULL) rows for the given
// (github_repo, pr_number), excluding posted rows and rows for other PRs/repos.
func TestGetActivePanelsForPR(t *testing.T) {
	assert := assert.New(t)
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })

	// Two rows for PR 7: one posted (excluded), one pending (included). Rows for
	// a different PR and a different repo prove the filters.
	postedID := seedPanelRow(t, db, "o/r", 7, "posted")
	require.NoError(t, db.MarkPanelPosted(postedID))
	seedPanelRow(t, db, "o/r", 7, "pending")
	seedPanelRow(t, db, "o/r", 8, "other-pr")
	seedPanelRow(t, db, "x/y", 7, "other-repo")

	rows, err := db.GetActivePanelsForPR("o/r", 7)
	require.NoError(t, err)
	require.Len(t, rows, 1, "only the pending row for o/r#7")
	assert.Equal("pending", rows[0].HeadSHA)
	assert.Equal("run-pending", rows[0].PanelRunUUID)
}

// TestGetTimedOutPanels covers the timeout sweep selection: only un-posted runs
// with a running member older than maxAge are returned. Panel created_at is not
// the timeout clock because CI throttling uses it as the immutable review-attempt
// time. A recent running member, an old posted run, and an old queued member are
// all excluded.
func TestGetTimedOutPanels(t *testing.T) {
	assert := assert.New(t)
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })

	repo := createRepo(t, db, "/tmp/ci-panel-timeouts")
	seedRun := func(pr int, head string, startedAt string) (*CIPanel, *ReviewJob) {
		t.Helper()
		members := []EnqueueOpts{{
			RepoID: repo.ID, GitRef: "base..head", Agent: "test",
			PanelMemberIndex: 0,
		}}
		synthesis := EnqueueOpts{RepoID: repo.ID, GitRef: "base..head", Agent: "test"}
		created, memberJobs, _, err := db.CreateCIPanelRun("o/r", pr, head, members, synthesis)
		require.NoError(t, err)
		require.True(t, created)
		require.Len(t, memberJobs, 1)
		_, err = db.Exec(`UPDATE review_jobs SET status = 'running', started_at = ? WHERE id = ?`,
			startedAt, memberJobs[0].ID)
		require.NoError(t, err)
		panel, err := db.GetCIPanelByPRSHA("o/r", pr, head)
		require.NoError(t, err)
		return panel, memberJobs[0]
	}

	oldUnposted, _ := seedRun(1, "old-unposted", time.Now().Add(-1*time.Hour).Format(time.RFC3339))
	recentUnposted, _ := seedRun(2, "recent-unposted", time.Now().Format(time.RFC3339))
	oldPosted, _ := seedRun(3, "old-posted", time.Now().Add(-1*time.Hour).Format(time.RFC3339))
	queuedOld, queuedMember := seedRun(4, "queued-old", time.Now().Add(-1*time.Hour).Format(time.RFC3339))
	_, err := db.Exec(`UPDATE review_jobs SET status = 'queued', started_at = NULL WHERE id = ?`, queuedMember.ID)
	require.NoError(t, err)
	require.NoError(t, db.MarkPanelPosted(oldPosted.ID))
	_ = recentUnposted
	_ = queuedOld

	rows, err := db.GetTimedOutPanels("o/r", 5*time.Minute)
	require.NoError(t, err)
	require.Len(t, rows, 1, "only the old un-posted run times out")
	assert.Equal(oldUnposted.PanelRunUUID, rows[0].PanelRunUUID,
		"the recent, posted, and queued runs are excluded")
}

func TestResetStaleJobsPreservesCIPanelCreatedAtAndClearsTimeoutRuntime(t *testing.T) {
	assert := assert.New(t)
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })

	repo := createRepo(t, db, "/tmp/ci-panel-restart")
	members := []EnqueueOpts{{
		RepoID: repo.ID, GitRef: "base..head", Agent: "test",
		PanelMemberIndex: 0,
	}}
	synthesis := EnqueueOpts{RepoID: repo.ID, GitRef: "base..head", Agent: "test"}
	created, memberJobs, _, err := db.CreateCIPanelRun("o/r", 12, "head", members, synthesis)
	require.NoError(t, err)
	require.True(t, created)
	require.Len(t, memberJobs, 1)

	_, err = db.Exec("UPDATE ci_pr_panels SET created_at = datetime('now','-1 hour') WHERE github_repo = 'o/r' AND pr_number = 12")
	require.NoError(t, err)
	createdBefore, err := db.LatestPanelTimeForPR("o/r", 12)
	require.NoError(t, err)

	claimed, err := db.ClaimJob("worker-before-restart")
	require.NoError(t, err)
	require.NotNil(t, claimed)
	assert.Equal(memberJobs[0].ID, claimed.ID)
	assert.Equal(JobStatusRunning, claimed.Status)
	_, err = db.Exec("UPDATE review_jobs SET started_at = datetime('now','-1 hour') WHERE id = ?", memberJobs[0].ID)
	require.NoError(t, err)

	rows, err := db.GetTimedOutPanels("o/r", 5*time.Minute)
	require.NoError(t, err)
	require.Len(t, rows, 1, "old active panel is timed out before restart recovery")

	require.NoError(t, db.ResetStaleJobs())

	recovered, err := db.GetJobByID(memberJobs[0].ID)
	require.NoError(t, err)
	assert.Equal(JobStatusQueued, recovered.Status)
	assert.Nil(recovered.StartedAt)
	createdAfter, err := db.LatestPanelTimeForPR("o/r", 12)
	require.NoError(t, err)
	assert.Equal(createdBefore, createdAfter, "restart recovery must not extend CI throttle time")

	rows, err = db.GetTimedOutPanels("o/r", 5*time.Minute)
	require.NoError(t, err)
	assert.Empty(rows, "restart recovery clears running-member timeout clock")
}

// TestDeleteCIPanelByRun covers F13: deleting by panel_run_uuid removes the
// mapping row. seedPanelRow sets panel_run_uuid to "run-"+headSHA.
func TestDeleteCIPanelByRun(t *testing.T) {
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })

	seedPanelRow(t, db, "o/r", 4, "runsha")

	require.NoError(t, db.DeleteCIPanelByRun("run-runsha"))

	_, err := db.GetCIPanelByPRSHA("o/r", 4, "runsha")
	require.ErrorIs(t, err, sql.ErrNoRows, "row gone after delete by run uuid")
}

func TestGetCIPanelByPRSHAAndSynthesisJobID(t *testing.T) {
	assert := assert.New(t)
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })

	repo := createRepo(t, db, "/tmp/ci-panels-lookup")

	// Seed a real panel run so synthesis_job_id references a real job.
	members := []EnqueueOpts{{
		RepoID: repo.ID, GitRef: "b..h", Agent: "test",
		PanelRunUUID: "run-1", PanelMemberIndex: 0,
	}}
	synthesis := EnqueueOpts{
		RepoID: repo.ID, GitRef: "b..h", Agent: "test", PanelRunUUID: "run-1",
	}
	_, synthJob, err := db.EnqueuePanelRun(members, synthesis)
	require.NoError(t, err)
	require.NotNil(t, synthJob)

	insertTestCIPanel(t, db, "o/r", 7, "headsha", "run-1", synthJob.ID)

	// Lookup by (github_repo, pr_number, head_sha).
	byPR, err := db.GetCIPanelByPRSHA("o/r", 7, "headsha")
	require.NoError(t, err)
	require.NotNil(t, byPR)
	assert.Equal("run-1", byPR.PanelRunUUID)
	assert.Equal("o/r", byPR.GithubRepo)
	assert.Equal(7, byPR.PRNumber)
	assert.Equal("headsha", byPR.HeadSHA)
	require.NotNil(t, byPR.SynthesisJobID)
	assert.Equal(synthJob.ID, *byPR.SynthesisJobID)

	// Lookup by synthesis_job_id.
	byJob, err := db.GetCIPanelBySynthesisJobID(synthJob.ID)
	require.NoError(t, err)
	require.NotNil(t, byJob)
	assert.Equal("headsha", byJob.HeadSHA)
	assert.Equal("run-1", byJob.PanelRunUUID)
}

func TestGetCIPanelByPRSHANotFound(t *testing.T) {
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })

	got, err := db.GetCIPanelByPRSHA("o/r", 999, "missing")
	require.ErrorIs(t, err, sql.ErrNoRows)
	assert.Nil(t, got)
}

func TestGetCIPanelBySynthesisJobIDNotFound(t *testing.T) {
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })

	got, err := db.GetCIPanelBySynthesisJobID(123456)
	require.ErrorIs(t, err, sql.ErrNoRows)
	assert.Nil(t, got)
}

// TestLatestPanelTimeForPR covers the throttle helper: it returns the most
// recent run's created_at across SHAs, the zero time when no run exists, and
// honors the github_repo + pr_number filter.
func TestLatestPanelTimeForPR(t *testing.T) {
	assert := assert.New(t)
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })

	// No runs yet: zero time, no error.
	got, err := db.LatestPanelTimeForPR("o/r", 7)
	require.NoError(t, err)
	assert.True(got.IsZero(), "no panel run yields zero time")

	// Seed two runs for PR 7 at distinct created_at, plus an unrelated PR/repo.
	older := seedPanelRow(t, db, "o/r", 7, "old")
	newer := seedPanelRow(t, db, "o/r", 7, "new")
	seedPanelRow(t, db, "o/r", 8, "other-pr")
	seedPanelRow(t, db, "x/y", 7, "other-repo")
	_, err = db.Exec("UPDATE ci_pr_panels SET created_at = datetime('now','-10 minutes') WHERE id = ?", older)
	require.NoError(t, err)
	_, err = db.Exec("UPDATE ci_pr_panels SET created_at = datetime('now','-1 minutes') WHERE id = ?", newer)
	require.NoError(t, err)

	got, err = db.LatestPanelTimeForPR("o/r", 7)
	require.NoError(t, err)
	require.False(t, got.IsZero(), "expected a non-zero time")
	// The returned time must be the newer run's (~1 min ago), not the older.
	assert.WithinDuration(time.Now().Add(-1*time.Minute), got, 30*time.Second,
		"returns the most recent run's created_at")
}

func TestLatestPanelTimeForPRHandlesMixedTimestampFormats(t *testing.T) {
	assert := assert.New(t)
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })

	older := seedPanelRow(t, db, "o/r", 7, "old-rfc3339")
	newer := seedPanelRow(t, db, "o/r", 7, "new-sqlite")
	_, err := db.Exec("UPDATE ci_pr_panels SET created_at = ? WHERE id = ?",
		"2026-06-01T18:16:42Z", older)
	require.NoError(t, err)
	_, err = db.Exec("UPDATE ci_pr_panels SET created_at = ? WHERE id = ?",
		"2026-06-01 18:51:02", newer)
	require.NoError(t, err)

	got, err := db.LatestPanelTimeForPR("o/r", 7)
	require.NoError(t, err)
	assert.Equal(time.Date(2026, 6, 1, 18, 51, 2, 0, time.UTC), got)
}

// seedPanelRunForRepo creates a real panel run (one member + synthesis) for
// (githubRepo, pr, headSHA) via CreateCIPanelRun and returns the panel row plus
// its synthesis job, so recovery-set tests can drive the synthesis status and
// posted_at independently.
func seedPanelRunForRepo(t *testing.T, db *DB, repoID int64, githubRepo string, pr int, headSHA string) (*CIPanel, *ReviewJob) {
	t.Helper()
	members := []EnqueueOpts{{
		RepoID: repoID, GitRef: "b..h", Agent: "test", PanelMemberIndex: 0,
	}}
	synthesis := EnqueueOpts{RepoID: repoID, GitRef: "b..h", Agent: "test"}
	created, _, synth, err := db.CreateCIPanelRun(githubRepo, pr, headSHA, members, synthesis)
	require.NoError(t, err)
	require.True(t, created, "panel run should be created")
	panel, err := db.GetCIPanelByPRSHA(githubRepo, pr, headSHA)
	require.NoError(t, err)
	return panel, synth
}

// TestGetUnpostedTerminalPanels covers the spec §10 dropped-event recovery set:
// a panel whose synthesis is terminal (done OR failed) but posted_at is NULL is
// returned, while a non-terminal synthesis or an already-posted row is excluded,
// and the github_repo filter scopes the result. The failed-unposted case proves
// raw-fallback runs (synthesis crashed, no review) still get a recovery pass.
func TestGetUnpostedTerminalPanels(t *testing.T) {
	assert := assert.New(t)
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })
	repo := createRepo(t, db, "/tmp/ci-panels-recover")

	// (a) synthesis done, posted_at NULL -> INCLUDED.
	aPanel, aSynth := seedPanelRunForRepo(t, db, repo.ID, "o/r", 1, "done-unposted")
	setStatus(t, db, aSynth.ID, JobStatusDone)
	// (b) synthesis running, posted_at NULL -> EXCLUDED (non-terminal).
	_, bSynth := seedPanelRunForRepo(t, db, repo.ID, "o/r", 2, "running-unposted")
	setStatus(t, db, bSynth.ID, JobStatusRunning)
	// (c) synthesis failed, posted_at SET -> EXCLUDED (already posted).
	cPanel, cSynth := seedPanelRunForRepo(t, db, repo.ID, "o/r", 3, "failed-posted")
	setStatus(t, db, cSynth.ID, JobStatusFailed)
	require.NoError(t, db.MarkPanelPosted(cPanel.ID))
	// (d) synthesis failed, posted_at NULL -> INCLUDED (raw-fallback must post).
	dPanel, dSynth := seedPanelRunForRepo(t, db, repo.ID, "o/r", 4, "failed-unposted")
	setStatus(t, db, dSynth.ID, JobStatusFailed)
	// Different repo, terminal + unposted -> EXCLUDED by github_repo scoping.
	_, otherSynth := seedPanelRunForRepo(t, db, repo.ID, "x/y", 5, "other-repo")
	setStatus(t, db, otherSynth.ID, JobStatusDone)

	rows, err := db.GetUnpostedTerminalPanels("o/r")
	require.NoError(t, err)

	got := make(map[string]bool, len(rows))
	for _, r := range rows {
		got[r.PanelRunUUID] = true
	}
	assert.Len(rows, 2, "exactly the done-unposted and failed-unposted runs")
	assert.True(got[aPanel.PanelRunUUID], "done-unposted included")
	assert.True(got[dPanel.PanelRunUUID], "failed-unposted included (raw fallback)")
	assert.False(got[cPanel.PanelRunUUID], "already-posted excluded")
}

// TestCreateCIPanelRunHappyPath covers F9: the generated run uuid is stamped on
// every member and the synthesis job, the mapping records that uuid, and
// synthesis_job_id is backfilled to the synthesis job's id.
func TestCreateCIPanelRunHappyPath(t *testing.T) {
	assert := assert.New(t)
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })

	repo := createRepo(t, db, "/tmp/ci-panels-create")
	members := []EnqueueOpts{
		{RepoID: repo.ID, GitRef: "b..h", Agent: "test", PanelMemberIndex: 0},
		{RepoID: repo.ID, GitRef: "b..h", Agent: "test", PanelMemberIndex: 1},
	}
	synthesis := EnqueueOpts{RepoID: repo.ID, GitRef: "b..h", Agent: "test"}

	created, mems, syn, err := db.CreateCIPanelRun("o/r", 5, "headsha", members, synthesis)
	require.NoError(t, err)
	assert.True(created, "first creator should win")
	require.Len(t, mems, 2)
	require.NotNil(t, syn)

	panel, err := db.GetCIPanelByPRSHA("o/r", 5, "headsha")
	require.NoError(t, err)
	require.NotNil(t, panel)
	assert.NotEmpty(panel.PanelRunUUID, "mapping must record the run uuid")
	require.NotNil(t, panel.SynthesisJobID)
	assert.Equal(syn.ID, *panel.SynthesisJobID, "synthesis_job_id backfilled")

	// F9: every job shares the mapping's run uuid.
	for i, m := range mems {
		assert.Equal(panel.PanelRunUUID, m.PanelRunUUID, "member %d run uuid", i)
		assert.Equal(PanelRoleMember, m.PanelRole, "member %d role", i)
	}
	assert.Equal(panel.PanelRunUUID, syn.PanelRunUUID, "synthesis run uuid")
	assert.Equal(PanelRoleSynthesis, syn.PanelRole, "synthesis role")
	assert.True(syn.ClaimBlocked, "synthesis gated until members finish")
}

func TestCreateCIPanelRunReclaimsRetiredSameHead(t *testing.T) {
	assert := assert.New(t)
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })

	repo := createRepo(t, db, "/tmp/ci-panels-retired-reclaim")
	members := []EnqueueOpts{{RepoID: repo.ID, GitRef: "b..h", Agent: "test", PanelMemberIndex: 0}}
	synthesis := EnqueueOpts{RepoID: repo.ID, GitRef: "b..h", Agent: "test"}

	created, _, _, err := db.CreateCIPanelRun("o/r", 6, "headsha", members, synthesis)
	require.NoError(t, err)
	require.True(t, created, "first create")
	firstPanel, err := db.GetCIPanelByPRSHA("o/r", 6, "headsha")
	require.NoError(t, err)
	require.NoError(t, db.MarkPanelRetired(firstPanel.ID))

	created, mems, syn, err := db.CreateCIPanelRun("o/r", 6, "headsha", members, synthesis)
	require.NoError(t, err)
	assert.True(created, "retired same-head row must not block a new panel")
	require.Len(t, mems, 1)
	require.NotNil(t, syn)
	secondPanel, err := db.GetActiveCIPanelByPRSHA("o/r", 6, "headsha")
	require.NoError(t, err)
	assert.NotEqual(firstPanel.PanelRunUUID, secondPanel.PanelRunUUID)
	assert.Equal(1, countCIPanels(t, db, "o/r", 6), "retired mapping is reclaimed")
}

func TestMarkPanelRetiredDoesNotRetirePostedPanel(t *testing.T) {
	assert := assert.New(t)
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })

	id := seedPanelRow(t, db, "o/r", 6, "posted-head")
	require.NoError(t, db.MarkPanelPosted(id))
	require.NoError(t, db.MarkPanelRetired(id))

	panel, err := db.GetActiveCIPanelByPRSHA("o/r", 6, "posted-head")
	require.NoError(t, err)
	require.NotNil(t, panel.PostedAt)
	assert.Nil(panel.RetiredAt, "posted mapping must keep counting as reviewed")
}

// TestCreateCIPanelRunRace covers F2: two concurrent creators for the same
// (repo, pr, sha) produce exactly one winner, and the loser creates no jobs.
func TestCreateCIPanelRunRace(t *testing.T) {
	assert := assert.New(t)
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })

	repo := createRepo(t, db, "/tmp/ci-panels-race")
	makeRun := func() (bool, error) {
		members := []EnqueueOpts{
			{RepoID: repo.ID, GitRef: "b..h", Agent: "test", PanelMemberIndex: 0},
			{RepoID: repo.ID, GitRef: "b..h", Agent: "test", PanelMemberIndex: 1},
		}
		synthesis := EnqueueOpts{RepoID: repo.ID, GitRef: "b..h", Agent: "test"}
		created, _, _, err := db.CreateCIPanelRun("o/r", 9, "racesha", members, synthesis)
		return created, err
	}

	const n = 2
	results := make([]bool, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // release both goroutines together to maximize contention
			results[i], errs[i] = makeRun()
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		require.NoError(t, err, "goroutine %d", i)
	}
	assert.Equal(1, boolCount(results, true), "exactly one creator wins")
	assert.Equal(1, boolCount(results, false), "exactly one creator loses")
	// Loser created no jobs: only the winner's 2 members + 1 synthesis persist.
	assert.Equal(3, countRepoJobs(t, db, repo.ID), "only winner's jobs persist")
	assert.Equal(1, countCIPanels(t, db, "o/r", 9), "single mapping row")
}

// TestCreateCIPanelRunAtomicity covers F2: a failure inside createCIPanelRunTx
// (after the mapping INSERT) rolls back fully — no orphan mapping, no orphan
// jobs. The failure is injected with failingExecer rather than a foreign-key
// violation: modernc.org/sqlite enforces foreign_keys per connection and the
// pool's later connections have it OFF, so an FK-based trigger is a false pass.
// failingExecer is pragma-independent — it forces the synthesis insert to fail.
func TestCreateCIPanelRunAtomicity(t *testing.T) {
	assert := assert.New(t)
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })

	repo := createRepo(t, db, "/tmp/ci-panels-atomic")
	members := []EnqueueOpts{
		{RepoID: repo.ID, GitRef: "b..h", Agent: "test", PanelMemberIndex: 0},
		{RepoID: repo.ID, GitRef: "b..h", Agent: "test", PanelMemberIndex: 1},
	}
	synthesis := EnqueueOpts{RepoID: repo.ID, GitRef: "b..h", Agent: "test"}

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

	// Call sequence: call 1 = retired-row cleanup, call 2 = INSERT OR IGNORE
	// mapping, call 3 = reserve attempt row, calls 4..len+3 = member inserts,
	// call len+4 = synthesis insert. Failing on the synthesis insert proves the
	// mapping row, the reserved attempt row, AND every member job row roll back
	// together.
	failing := &failingExecer{inner: conn, failAt: len(members) + 4}
	_, _, _, err = db.createCIPanelRunTx(ctx, failing, "o/r", 11, "atomicsha", members, synthesis, machineID, time.Now())
	require.Error(t, err)
	require.ErrorContains(t, err, "insert panel synthesis")

	_, rbErr := conn.ExecContext(ctx, "ROLLBACK")
	require.NoError(t, rbErr)

	// Full rollback: no mapping row, no attempt row, and no job rows for the repo.
	assert.Equal(0, countCIPanels(t, db, "o/r", 11), "no orphan mapping")
	assert.Equal(0, countRepoJobs(t, db, repo.ID), "no orphan jobs")
	_, err = db.GetCIPanelByPRSHA("o/r", 11, "atomicsha")
	require.ErrorIs(t, err, sql.ErrNoRows)
	attempt, err := db.GetReviewAttempt("o/r", 11, "atomicsha")
	require.NoError(t, err)
	assert.Nil(attempt, "reserved attempt row rolls back with the failed run")
}
