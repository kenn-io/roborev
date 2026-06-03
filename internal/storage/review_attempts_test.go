package storage

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func openReviewAttemptsTestDB(t *testing.T) *DB {
	t.Helper()
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })
	return db
}

func TestReviewAttemptsTableExists(t *testing.T) {
	db := openReviewAttemptsTestDB(t)
	insert := `INSERT INTO ci_pr_review_attempts
		(github_repo, pr_number, head_sha, attempt, first_attempt_at, next_attempt_at,
		 last_error_class, consecutive_genuine_attempts, last_error_excerpt,
		 last_panel_run_uuid, state, updated_at)
		VALUES ('o/r', 1, 'sha', 1, datetime('now'), NULL, '', 0, '', '', 'pending', datetime('now'))`
	_, err := db.Exec(insert)
	require.NoError(t, err)

	// UNIQUE(github_repo, pr_number, head_sha) must reject a duplicate HEAD.
	// T7's compare-and-swap logic relies on this constraint.
	_, err = db.Exec(insert)
	require.Error(t, err)
}

func TestReviewAttemptLifecycle(t *testing.T) {
	assert := assert.New(t)
	db := openReviewAttemptsTestDB(t)
	now := time.Now()

	created, err := db.ReserveReviewAttempt("o/r", 7, "sha1", now)
	require.NoError(t, err)
	assert.True(created)
	// Second reserve for same key is a no-op (dedup).
	created2, err := db.ReserveReviewAttempt("o/r", 7, "sha1", now)
	require.NoError(t, err)
	assert.False(created2)

	// Defer (transient) resets genuine streak.
	require.NoError(t, db.DeferReviewAttempt("o/r", 7, "sha1", "transient", "429", "uuid1",
		now.Add(-time.Minute), false))
	a, err := db.GetReviewAttempt("o/r", 7, "sha1")
	require.NoError(t, err)
	assert.Equal("deferred", a.State)
	assert.Equal(0, a.ConsecutiveGenuineAttempts)

	// Claim the due row (CAS) — exactly one claim succeeds.
	claimed, attempt, _, err := db.ClaimDueReviewAttempt("o/r", 7, "sha1", now)
	require.NoError(t, err)
	assert.True(claimed)
	assert.Equal(2, attempt)
	claimedAgain, _, _, err := db.ClaimDueReviewAttempt("o/r", 7, "sha1", now)
	require.NoError(t, err)
	assert.False(claimedAgain) // now 'pending', not 'deferred'

	// Genuine defer bumps the streak.
	require.NoError(t, db.DeferReviewAttempt("o/r", 7, "sha1", "genuine", "bad model", "uuid2",
		now.Add(-time.Minute), true))
	a, _ = db.GetReviewAttempt("o/r", 7, "sha1")
	assert.Equal(1, a.ConsecutiveGenuineAttempts)

	// Closed-PR cleanup deletes it.
	n, err := db.DeleteReviewAttemptsForPR("o/r", 7)
	require.NoError(t, err)
	assert.Equal(int64(1), n)
	a, err = db.GetReviewAttempt("o/r", 7, "sha1")
	require.NoError(t, err)
	assert.Nil(a)
}

func TestGetDueReviewAttempts(t *testing.T) {
	assert := assert.New(t)
	db := openReviewAttemptsTestDB(t)
	now := time.Now()

	// Row A: deferred in the past -> due.
	created, err := db.ReserveReviewAttempt("o/r", 1, "a", now)
	require.NoError(t, err)
	require.True(t, created)
	require.NoError(t, db.DeferReviewAttempt("o/r", 1, "a", "transient", "e", "u",
		now.Add(-time.Minute), false))

	// Row B: deferred in the future -> not yet due (guards the <= comparison).
	created, err = db.ReserveReviewAttempt("o/r", 2, "b", now)
	require.NoError(t, err)
	require.True(t, created)
	require.NoError(t, db.DeferReviewAttempt("o/r", 2, "b", "transient", "e", "u",
		now.Add(time.Hour), false))

	// Row C: pending only, next_attempt_at NULL -> excluded (guards state and
	// the NULL-guard; a fresh row must never read as due).
	created, err = db.ReserveReviewAttempt("o/r", 3, "c", now)
	require.NoError(t, err)
	require.True(t, created)

	// Row D: done -> terminal, excluded (also exercises MarkReviewAttemptDone).
	created, err = db.ReserveReviewAttempt("o/r", 4, "d", now)
	require.NoError(t, err)
	require.True(t, created)
	require.NoError(t, db.MarkReviewAttemptDone("o/r", 4, "d"))

	// Cross-repo due row -> must be excluded by repo scoping.
	created, err = db.ReserveReviewAttempt("o/r2", 1, "a", now)
	require.NoError(t, err)
	require.True(t, created)
	require.NoError(t, db.DeferReviewAttempt("o/r2", 1, "a", "transient", "e", "u",
		now.Add(-time.Minute), false))

	due, err := db.GetDueReviewAttempts("o/r", now)
	require.NoError(t, err)
	assert.Len(due, 1)
	assert.Equal("a", due[0].HeadSHA)
	assert.Equal("deferred", due[0].State)
}

func TestGetNonTerminalAttemptPRs(t *testing.T) {
	assert := assert.New(t)
	db := openReviewAttemptsTestDB(t)
	now := time.Now()

	// PR 1 has two non-terminal HEADs (pending + deferred) that must collapse
	// to a single ref (asserts DISTINCT).
	created, err := db.ReserveReviewAttempt("o/r", 1, "a", now)
	require.NoError(t, err)
	require.True(t, created)
	created, err = db.ReserveReviewAttempt("o/r", 1, "b", now)
	require.NoError(t, err)
	require.True(t, created)
	require.NoError(t, db.DeferReviewAttempt("o/r", 1, "b", "transient", "e", "u",
		now.Add(-time.Minute), false))

	// PR 2 is done -> terminal, excluded.
	created, err = db.ReserveReviewAttempt("o/r", 2, "c", now)
	require.NoError(t, err)
	require.True(t, created)
	require.NoError(t, db.MarkReviewAttemptDone("o/r", 2, "c"))

	// PR 3 is deferred -> included.
	created, err = db.ReserveReviewAttempt("o/r", 3, "d", now)
	require.NoError(t, err)
	require.True(t, created)
	require.NoError(t, db.DeferReviewAttempt("o/r", 3, "d", "transient", "e", "u",
		now.Add(-time.Minute), false))

	refs, err := db.GetNonTerminalAttemptPRs("o/r")
	require.NoError(t, err)
	assert.ElementsMatch([]PanelPRRef{
		{GithubRepo: "o/r", PRNumber: 1},
		{GithubRepo: "o/r", PRNumber: 3},
	}, refs)
}

func TestGetPendingReviewAttempts(t *testing.T) {
	assert := assert.New(t)
	db := openReviewAttemptsTestDB(t)
	now := time.Now()

	// PR 1: pending (fresh reserve) -> included.
	created, err := db.ReserveReviewAttempt("o/r", 1, "a", now)
	require.NoError(t, err)
	require.True(t, created)

	// PR 2: deferred -> excluded (only pending rows are reconcile candidates).
	created, err = db.ReserveReviewAttempt("o/r", 2, "b", now)
	require.NoError(t, err)
	require.True(t, created)
	require.NoError(t, db.DeferReviewAttempt("o/r", 2, "b", "transient", "e", "u",
		now.Add(-time.Minute), false))

	// PR 3: done -> excluded (terminal).
	created, err = db.ReserveReviewAttempt("o/r", 3, "c", now)
	require.NoError(t, err)
	require.True(t, created)
	require.NoError(t, db.MarkReviewAttemptDone("o/r", 3, "c"))

	// Cross-repo pending row -> excluded by repo scoping.
	created, err = db.ReserveReviewAttempt("o/r2", 9, "z", now)
	require.NoError(t, err)
	require.True(t, created)

	pending, err := db.GetPendingReviewAttempts("o/r")
	require.NoError(t, err)
	assert.Len(pending, 1)
	assert.Equal("a", pending[0].HeadSHA)
	assert.Equal("pending", pending[0].State)
	assert.Equal(1, pending[0].PRNumber)
}

func TestDeleteReviewAttemptScopesToOneRow(t *testing.T) {
	db := openReviewAttemptsTestDB(t)
	now := time.Now()

	created, err := db.ReserveReviewAttempt("o/r", 5, "x", now)
	require.NoError(t, err)
	require.True(t, created)
	created, err = db.ReserveReviewAttempt("o/r", 5, "y", now)
	require.NoError(t, err)
	require.True(t, created)

	require.NoError(t, db.DeleteReviewAttempt("o/r", 5, "x"))

	deleted, err := db.GetReviewAttempt("o/r", 5, "x")
	require.NoError(t, err)
	assert.Nil(t, deleted)

	survivor, err := db.GetReviewAttempt("o/r", 5, "y")
	require.NoError(t, err)
	assert.NotNil(t, survivor)
}

func TestRearmStuckReviewAttempt(t *testing.T) {
	assert := assert.New(t)
	db := openReviewAttemptsTestDB(t)
	now := time.Now()

	// A HEAD that accumulated a genuine-failure streak, then got claimed by the
	// retry sweep (-> 'pending') and stranded when the re-enqueue failed.
	created, err := db.ReserveReviewAttempt("o/r", 1, "a", now)
	require.NoError(t, err)
	require.True(t, created)
	require.NoError(t, db.DeferReviewAttempt("o/r", 1, "a", "genuine", "bad model", "uuid1",
		now.Add(-time.Minute), true))
	require.NoError(t, db.DeferReviewAttempt("o/r", 1, "a", "genuine", "bad model", "uuid1",
		now.Add(-time.Minute), true))
	claimed, _, _, err := db.ClaimDueReviewAttempt("o/r", 1, "a", now)
	require.NoError(t, err)
	require.True(t, claimed)
	before, err := db.GetReviewAttempt("o/r", 1, "a")
	require.NoError(t, err)
	require.Equal(t, "pending", before.State)
	require.Equal(t, 2, before.ConsecutiveGenuineAttempts)

	// Re-arm re-defers the row but PRESERVES the genuine streak and error fields:
	// a failed enqueue must not reset progress toward genuine give-up.
	next := now.Add(time.Hour)
	rearmed, err := db.RearmStuckReviewAttempt("o/r", 1, "a", next)
	require.NoError(t, err)
	assert.True(rearmed)
	after, err := db.GetReviewAttempt("o/r", 1, "a")
	require.NoError(t, err)
	assert.Equal("deferred", after.State)
	assert.Equal(2, after.ConsecutiveGenuineAttempts) // not reset to 0
	assert.Equal("genuine", after.LastErrorClass)
	assert.Equal("bad model", after.LastErrorExcerpt)
	assert.NotNil(after.NextAttemptAt)

	// CAS: the row is no longer 'pending', so a second re-arm is a no-op.
	rearmedAgain, err := db.RearmStuckReviewAttempt("o/r", 1, "a", next)
	require.NoError(t, err)
	assert.False(rearmedAgain)
}

func TestClaimDueReviewAttemptIsExclusive(t *testing.T) {
	db := openReviewAttemptsTestDB(t)
	now := time.Now()
	// reserve + defer the row so it is due:
	created, err := db.ReserveReviewAttempt("o/r", 9, "s", now)
	require.NoError(t, err)
	require.True(t, created)
	require.NoError(t, db.DeferReviewAttempt("o/r", 9, "s", "transient", "x", "u",
		now.Add(-time.Minute), false))
	var wins atomic.Int32
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if ok, _, _, _ := db.ClaimDueReviewAttempt("o/r", 9, "s", now); ok {
				wins.Add(1)
			}
		})
	}
	wg.Wait()
	assert.Equal(t, int32(1), wins.Load())
}
