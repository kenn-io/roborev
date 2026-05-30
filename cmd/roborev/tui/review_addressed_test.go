package tui

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
)

func TestTUIReviewViewClosedRollbackOnError(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())

	// Initial state with review view showing an open review
	m.currentView = viewReview
	m.currentReview = makeReview(42, &storage.ReviewJob{ID: 100})

	// Simulate optimistic update (what happens when 'a' is pressed in review view)
	m.currentReview.Closed = true
	m.pendingClosed[100] = pendingState{newState: true, seq: 1} // Track pending state

	// Error result from server (reviewID must match currentReview.ID for rollback)
	errMsg := closedResultMsg{
		reviewID:   42,  // Must match currentReview.ID
		jobID:      100, // Must match for isCurrentRequest check
		reviewView: true,
		oldState:   false, // Was false before optimistic update
		newState:   true,  // The requested state (matches pendingClosed)
		seq:        1,     // Must match pending seq to be treated as current
		err:        fmt.Errorf("server error"),
	}

	m, _ = updateModel(t, m, errMsg)

	// Should have rolled back to false
	assert.False(t, m.currentReview.Closed)
	require.Error(t, m.err)
}

func TestTUIReviewViewClosedSuccessNoRollback(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())

	// Initial state with review view
	m.currentView = viewReview
	m.currentReview = makeReview(42, &storage.ReviewJob{})

	// Simulate optimistic update
	m.currentReview.Closed = true

	// Success result (err is nil)
	successMsg := closedResultMsg{
		reviewView: true,
		oldState:   false,
		seq:        1, // Not strictly needed for success but included for consistency
		err:        nil,
	}

	m, _ = updateModel(t, m, successMsg)

	// Should stay true (no rollback on success)
	assert.True(t, m.currentReview.Closed)
	require.NoError(t, m.err)
}

func TestTUIReviewViewNavigateAwayBeforeError(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())

	// Setup: jobs in queue with closed=false
	addrA := false
	addrB := false
	m.jobs = []storage.ReviewJob{
		{ID: 100, Status: storage.JobStatusDone, Closed: &addrA}, // Job for review A
		{ID: 200, Status: storage.JobStatusDone, Closed: &addrB}, // Job for review B
	}

	// User views review A, toggles closed (optimistic update)
	m.currentView = viewReview
	m.currentReview = makeReview(42, &storage.ReviewJob{ID: 100})
	m.currentReview.Closed = true                               // Optimistic update to review
	*m.jobs[0].Closed = true                                    // Optimistic update to job in queue
	m.pendingClosed[100] = pendingState{newState: true, seq: 1} // Track pending state for job A

	// User navigates to review B before error response arrives
	m.currentReview = makeReview(99, &storage.ReviewJob{ID: 200})

	// Error arrives for review A's toggle
	errMsg := closedResultMsg{
		reviewID:   42,  // Review A
		jobID:      100, // Job A
		reviewView: true,
		oldState:   false,
		newState:   true, // The requested state (matches pendingClosed)
		seq:        1,    // Must match pending seq to be treated as current
		err:        fmt.Errorf("server error"),
	}

	m, _ = updateModel(t, m, errMsg)

	// Review B should be unchanged (still false)
	assert.False(t, m.currentReview.Closed)

	// Job A in queue should be rolled back to false
	assert.False(t, *m.jobs[0].Closed)

	// Job B in queue should be unchanged
	assert.False(t, *m.jobs[1].Closed)
}

func TestTUIReviewViewToggleSyncsQueueJob(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())

	// Setup: job in queue with closed=false
	addr := false
	m.jobs = []storage.ReviewJob{
		{ID: 100, Status: storage.JobStatusDone, Closed: &addr},
	}

	// User views review for job 100 and presses 'a'
	m.currentView = viewReview
	m.currentReview = makeReview(42, &storage.ReviewJob{ID: 100})

	// Simulate the optimistic update that happens when 'a' is pressed
	oldState := m.currentReview.Closed
	newState := !oldState
	m.currentReview.Closed = newState
	m.setJobClosed(100, newState)

	// Both should be updated
	assert.True(t, m.currentReview.Closed)
	assert.True(t, *m.jobs[0].Closed)
}

func TestTUIReviewViewErrorWithoutJobID(t *testing.T) {
	// Test that review-view errors without jobID are still handled if
	// pendingReviewClosed matches
	m := newModel(localhostEndpoint, withExternalIODisabled())

	// Review without an associated job (Job is nil)
	m.currentView = viewReview
	m.currentReview = &storage.Review{ID: 42}

	// Simulate optimistic update (what happens when 'a' is pressed)
	m.currentReview.Closed = true
	m.pendingReviewClosed[42] = pendingState{newState: true, seq: 1} // Track pending state by review ID

	// Error arrives for this toggle (no jobID since Job was nil)
	errMsg := closedResultMsg{
		reviewID:   42,
		jobID:      0, // No job
		reviewView: true,
		oldState:   false,
		newState:   true, // Matches pendingReviewClosed
		seq:        1,    // Matches pending seq
		err:        fmt.Errorf("server error"),
	}

	m2, _ := updateModel(t, m, errMsg)

	// Should have rolled back to false
	assert.False(t, m2.currentReview.Closed)

	// Error should be set
	require.Error(t, m2.err)

	// pendingReviewClosed should be cleared
	_, ok := m2.pendingReviewClosed[42]
	assert.False(t, ok)
}

func TestTUIReviewViewStaleErrorWithoutJobID(t *testing.T) {
	// Test that stale review-view errors without jobID are ignored
	m := newModel(localhostEndpoint, withExternalIODisabled())

	// Review without an associated job
	m.currentView = viewReview
	m.currentReview = &storage.Review{ID: 42}

	// User toggled to true, then back to false
	// pendingReviewClosed is now false (from the second toggle)
	m.currentReview.Closed = false
	m.pendingReviewClosed[42] = pendingState{newState: false, seq: 1}

	// A stale error arrives from the earlier toggle to true
	staleErrorMsg := closedResultMsg{
		reviewID:   42,
		jobID:      0, // No job
		reviewView: true,
		oldState:   false, // What it was before the stale toggle
		newState:   true,  // Stale: pendingReviewClosed is false, not true
		seq:        0,     // Stale: doesn't match pending seq (1)
		err:        fmt.Errorf("network error"),
	}

	m2, _ := updateModel(t, m, staleErrorMsg)

	// State should NOT be rolled back (stale error)
	assert.False(t, m2.currentReview.Closed)

	// Error should NOT be set (stale error)
	require.NoError(t, m2.err)

	// pendingReviewClosed should still be set (not cleared by stale response)
	_, ok := m2.pendingReviewClosed[42]
	assert.True(t, ok)
}

func TestTUIReviewViewSameStateLateError(t *testing.T) {
	// Test: true (seq 1) -> false (seq 2) -> true (seq 3), with late error from first true
	// The late error has newState=true which matches current pending newState,
	// but sequence numbers now distinguish same-state toggles.
	m := newModel(localhostEndpoint, withExternalIODisabled())

	// Review without an associated job
	m.currentView = viewReview
	m.currentReview = &storage.Review{ID: 42}

	// Sequence: toggle true (seq 1) -> toggle false (seq 2) -> toggle true (seq 3)
	// After third toggle, state is true and pendingReviewClosed has seq 3
	m.currentReview.Closed = true
	m.pendingReviewClosed[42] = pendingState{newState: true, seq: 3} // Third toggle

	// A late error arrives from the FIRST toggle (seq 1)
	// This error has newState=true which matches current pending newState,
	// but seq doesn't match, so it should be treated as stale and ignored.
	lateErrorMsg := closedResultMsg{
		reviewID:   42,
		jobID:      0,
		reviewView: true,
		oldState:   false, // First toggle was from false to true
		newState:   true,  // Same newState as current pending...
		seq:        1,     // ...but different seq, so this is stale
		err:        fmt.Errorf("network error from first toggle"),
	}

	m2, _ := updateModel(t, m, lateErrorMsg)

	// With sequence numbers, the late error should be IGNORED (not rolled back)
	// because seq: 1 != pending seq: 3
	assert.True(t, m2.currentReview.Closed)

	// Error should NOT be set (stale error)
	require.NoError(t, m2.err)

	// pendingReviewClosed should still be set (not cleared by stale response)
	_, ok := m2.pendingReviewClosed[42]
	assert.True(t, ok)
}
