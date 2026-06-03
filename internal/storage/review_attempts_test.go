package storage

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReviewAttemptsTableExists(t *testing.T) {
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })
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
	db := openTestDB(t)
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

func TestClaimDueReviewAttemptIsExclusive(t *testing.T) {
	db := openTestDB(t)
	now := time.Now()
	// reserve + defer the row so it is due:
	created, err := db.ReserveReviewAttempt("o/r", 9, "s", now)
	require.NoError(t, err)
	require.True(t, created)
	require.NoError(t, db.DeferReviewAttempt("o/r", 9, "s", "transient", "x", "u",
		now.Add(-time.Minute), false))
	var wins int32
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if ok, _, _, _ := db.ClaimDueReviewAttempt("o/r", 9, "s", now); ok {
				atomic.AddInt32(&wins, 1)
			}
		})
	}
	wg.Wait()
	assert.Equal(t, int32(1), atomic.LoadInt32(&wins))
}
