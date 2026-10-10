package storage

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFailedReviewAttemptStatesDuringDelivery(t *testing.T) {
	t.Parallel()
	db := openReviewAttemptsTestDB(t)
	for pr := 1; pr <= 8; pr++ {
		head := fmt.Sprintf("head-%d", pr)
		seedPanelRow(t, db, "acme/api", pr, head)
		_, err := db.ReserveReviewAttempt("acme/api", pr, head, time.Now())
		require.NoError(t, err)
	}
	_, err := db.Exec(`UPDATE ci_pr_review_attempts SET last_error_class = 'transient'`)
	require.NoError(t, err)

	// Delivery changes the attempt and panel atomically. Every observation must
	// see either pending work or no unresolved failure, never a mixture of both.
	stop := make(chan struct{})
	writerErrors := make(chan error, 1)
	go func() {
		writerErrors <- toggleReviewDelivery(db, stop)
	}()
	defer func() { close(stop); require.NoError(t, <-writerErrors) }()
	for range 100 {
		states, err := db.GetFailedReviewAttemptStates([]string{"acme/api"})
		require.NoError(t, err)
		for _, state := range states {
			require.NotNil(t, state.Panel)
			assert.Nil(t, state.Panel.PostedAt, "an unresolved attempt and delivered panel cannot belong to the same snapshot")
		}
	}
}

func toggleReviewDelivery(db *DB, stop <-chan struct{}) error {
	for {
		select {
		case <-stop:
			return nil
		default:
		}
		for _, delivered := range []bool{true, false} {
			tx, err := db.Begin()
			if err != nil {
				return err
			}
			state, posted := "pending", any(nil)
			if delivered {
				state, posted = "done", time.Now().Format(time.RFC3339)
			}
			_, err = tx.Exec(`UPDATE ci_pr_review_attempts SET state = ?`, state)
			if err == nil {
				_, err = tx.Exec(`UPDATE ci_pr_panels SET posted_at = ?, outcome = ?`, posted, PanelOutcomeReviewPosted)
			}
			if err != nil {
				_ = tx.Rollback()
				return err
			}
			if err := tx.Commit(); err != nil {
				return err
			}
		}
	}
}
