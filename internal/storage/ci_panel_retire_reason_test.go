package storage

import (
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func panelRetireReason(t *testing.T, db *DB, id int64) sql.NullString {
	t.Helper()
	var reason sql.NullString
	require.NoError(t, db.QueryRow(`SELECT retire_reason FROM ci_pr_panels WHERE id = ?`, id).Scan(&reason))
	return reason
}

func TestPanelRetirementPreservesFirstReason(t *testing.T) {
	t.Parallel()
	db := openReviewAttemptsTestDB(t)
	cases := []struct {
		reason string
		want   string
	}{
		{PanelRetireReasonClosedPR, "closed_pr"},
		{PanelRetireReasonSuperseded, "superseded"},
		{PanelRetireReasonStaleHead, "stale_head"},
		{PanelRetireReasonDeferredRetry, "deferred_retry"},
		{PanelRetireReasonRepoMismatch, "repo_mismatch"},
		{PanelRetireReasonCanceled, "canceled"},
	}
	for i, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			assert := assert.New(t)
			id := seedPanelRow(t, db, "acme/api", i+1, tc.want)
			require.NoError(t, db.MarkPanelRetired(id, tc.reason))
			var firstRetiredAt string
			require.NoError(t, db.QueryRow(`SELECT retired_at FROM ci_pr_panels WHERE id = ?`, id).Scan(&firstRetiredAt))
			assert.Equal(sql.NullString{String: tc.want, Valid: true}, panelRetireReason(t, db, id))
			for _, later := range cases {
				require.NoError(t, db.MarkPanelRetired(id, later.reason))
				require.NoError(t, db.RetirePanelAndDeleteAttempt(id, later.reason))
				retired, err := db.MarkPanelRetiredIfStalePostDisallowed(id, later.reason)
				require.NoError(t, err)
				assert.False(retired)
				assert.Equal(sql.NullString{String: tc.want, Valid: true}, panelRetireReason(t, db, id))
			}
			var retiredAt string
			require.NoError(t, db.QueryRow(`SELECT retired_at FROM ci_pr_panels WHERE id = ?`, id).Scan(&retiredAt))
			assert.Equal(firstRetiredAt, retiredAt)
		})
	}
}

func TestPanelRetirementDeletesAttemptAfterStaleRetirement(t *testing.T) {
	t.Parallel()
	db := openReviewAttemptsTestDB(t)
	id := seedPanelRow(t, db, "acme/api", 7, "head")
	_, err := db.ReserveReviewAttempt("acme/api", 7, "head", time.Now())
	require.NoError(t, err)
	retired, err := db.MarkPanelRetiredIfStalePostDisallowed(id, PanelRetireReasonStaleHead)
	require.NoError(t, err)
	require.True(t, retired)
	require.NoError(t, db.RetirePanelAndDeleteAttempt(id, PanelRetireReasonCanceled))
	attempt, err := db.GetReviewAttempt("acme/api", 7, "head")
	require.NoError(t, err)
	assert.Nil(t, attempt, "cleanup deletes an attempt even after the stale-head CAS retired the row")
	assert.Equal(t, sql.NullString{String: "stale_head", Valid: true}, panelRetireReason(t, db, id))
}

func TestPanelRetirementPreservesUnknownHistoryAndPostedRows(t *testing.T) {
	t.Parallel()
	db := openReviewAttemptsTestDB(t)
	unknown := seedPanelRow(t, db, "acme/api", 7, "unknown")
	_, err := db.Exec(`UPDATE ci_pr_panels SET retired_at = datetime('now') WHERE id = ?`, unknown)
	require.NoError(t, err)
	posted := seedPanelRow(t, db, "acme/api", 7, "posted")
	require.NoError(t, db.MarkPanelPosted(posted, PanelOutcomeAbandoned, "", ""))
	for _, id := range []int64{unknown, posted} {
		require.NoError(t, db.MarkPanelRetired(id, PanelRetireReasonClosedPR))
		require.NoError(t, db.RetirePanelAndDeleteAttempt(id, PanelRetireReasonCanceled))
		retired, err := db.MarkPanelRetiredIfStalePostDisallowed(id, PanelRetireReasonStaleHead)
		require.NoError(t, err)
		assert.False(t, retired)
		assert.False(t, panelRetireReason(t, db, id).Valid)
	}
	// Removing finalized failure history is ownership cleanup, not discarded work.
	_, err = db.DeleteReviewAttemptsForPR("acme/api", 7)
	require.NoError(t, err)
	assert.False(t, panelRetireReason(t, db, posted).Valid)
	row, err := db.GetCIPanelByPRSHA("acme/api", 7, "posted")
	require.NoError(t, err)
	assert.NotNil(t, row.RetiredAt)
	assert.NotNil(t, row.PostedAt)
}
