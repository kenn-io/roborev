package tui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/google/go-cmp/cmp"
	"github.com/mattn/go-runewidth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
)

func TestTUICloseReviewSuccess(t *testing.T) {
	_, m := mockServerModel(t, expectJSONPost(t, "", closeRequest{JobID: 100, Closed: true}, map[string]bool{"success": true}))
	cmd := m.closeReview(42, 100, true, false, 1)
	msg := cmd()

	result := assertMsgType[closedResultMsg](t, msg)
	require.NoError(t, result.err, "Expected no error, got %v", result.err)
	require.True(t, result.reviewView, "Expected reviewView to be true")
	require.EqualValues(t, 100, result.jobID, "Expected jobID=100, got %d", result.jobID)
}

func TestTUICloseReviewNotFound(t *testing.T) {
	_, m := mockServerModel(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	cmd := m.closeReview(999, 100, true, false, 1)
	msg := cmd()

	result := assertMsgType[closedResultMsg](t, msg)
	if result.err == nil || result.err.Error() != "review not found" {
		assert.True(t, result.err != nil && result.err.Error() == "review not found", "Expected 'review not found' error, got: %v", result.err)
	}
}

func TestTUIToggleClosedForJobSuccess(t *testing.T) {
	_, m := mockServerModel(t, expectJSONPost(t, "/api/review/close", closeRequest{JobID: 1, Closed: true}, map[string]bool{"success": true}))
	currentState := false
	cmd := m.toggleClosedForJob(1, &currentState)
	msg := cmd()

	closed := assertMsgType[closedMsg](t, msg)
	require.True(t, bool(closed), "Expected closed to be true")
}

func TestTUIToggleClosedNoReview(t *testing.T) {
	_, m := mockServerModel(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	cmd := m.toggleClosedForJob(999, nil)
	msg := cmd()

	errMsg := assertMsgType[errMsg](t, msg)
	assert.Equal(t, "no review for this job", errMsg.Error(), "Expected 'no review for this job', got: %v", errMsg)
}

func TestTUICloseFromReviewView_Navigation(t *testing.T) {
	cases := []struct {
		name         string
		initialIdx   int
		initialJobID int64
		actions      func(model) model
		expectedIdx  int
		expectedJob  int64
		expectedView viewKind
	}{
		{
			name:         "NextVisible",
			initialIdx:   1,
			initialJobID: 2,
			actions: func(m model) model {
				m2, _ := pressKey(m, 'a')

				assertSelection(t, m2, 1, 2)
				assertView(t, m2, viewReview)

				m3, _ := pressSpecial(m2, tea.KeyEscape)
				return m3
			},
			expectedIdx:  2,
			expectedJob:  3,
			expectedView: viewQueue,
		},
		{
			name:         "FallbackPrev",
			initialIdx:   2,
			initialJobID: 3,
			actions: func(m model) model {
				m2, _ := pressKey(m, 'a')
				m3, _ := pressSpecial(m2, tea.KeyEscape)
				return m3
			},
			expectedIdx:  1,
			expectedJob:  2,
			expectedView: viewQueue,
		},
		{
			name:         "ExitWithQ",
			initialIdx:   1,
			initialJobID: 2,
			actions: func(m model) model {
				m2, _ := pressKey(m, 'a')
				m3, _ := pressKey(m2, 'q')
				return m3
			},
			expectedIdx:  2,
			expectedJob:  3,
			expectedView: viewQueue,
		},
		{
			name:         "ExitWithCtrlC",
			initialIdx:   1,
			initialJobID: 2,
			actions: func(m model) model {
				m2, _ := pressKey(m, 'a')
				m3, _ := pressCtrl(m2, 'c')
				return m3
			},
			expectedIdx:  2,
			expectedJob:  3,
			expectedView: viewQueue,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			jobs := []storage.ReviewJob{
				makeJob(1, withClosed(new(false))),
				makeJob(2, withClosed(new(false))),
				makeJob(3, withClosed(new(false))),
			}

			m := setupTestModel(jobs, func(m *model) {
				m.currentView = viewReview
				m.hideClosed = true
				m.selectedIdx = tc.initialIdx
				m.selectedJobID = tc.initialJobID
				m.currentReview = makeReview(10, &m.jobs[tc.initialIdx])
			})

			m2 := tc.actions(m)

			assertSelection(t, m2, tc.expectedIdx, tc.expectedJob)
			assertView(t, m2, tc.expectedView)
		})
	}
}

type closeRequest struct {
	JobID  int64 `json:"job_id"`
	Closed bool  `json:"closed"`
}

func TestTUICloseReviewInBackgroundSuccess(t *testing.T) {
	_, m := mockServerModel(t, expectJSONPost(t, "/api/review/close", closeRequest{JobID: 42, Closed: true}, map[string]bool{"success": true}))
	cmd := m.closeReviewInBackground(42, true, false, 1, false)
	msg := cmd()

	result := assertMsgType[closedResultMsg](t, msg)
	require.NoError(t, result.err, "Expected no error, got %v", result.err)
	require.EqualValues(t, 42, result.jobID, "Expected jobID=42, got %d", result.jobID)
	require.False(t, result.oldState, "Expected oldState=false")
	require.False(t, result.reviewView, "Expected reviewView to be false")
}

func TestTUICloseReviewInBackgroundNotFound(t *testing.T) {
	_, m := mockServerModel(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/review/close" || r.Method != http.MethodPost {
			assert.Equal(t, "/api/review/close", r.URL.Path, "Unexpected request path, got: %s", r.URL.Path)
			assert.Equal(t, http.MethodPost, r.Method, "Unexpected request method: %s", r.Method)
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	cmd := m.closeReviewInBackground(42, true, false, 1, false)
	msg := cmd()

	result := assertMsgType[closedResultMsg](t, msg)
	if result.err == nil || !strings.Contains(result.err.Error(), "no review") {
		assert.False(t, result.err == nil || !strings.Contains(result.err.Error(), "no review"), "Expected error containing 'no review', got: %v", result.err)
	}
	assert.EqualValues(t, 42, result.jobID, "Expected jobID=42 for rollback, got %d", result.jobID)

	if result.oldState != false {
		assert.False(t, result.oldState, "Expected oldState=false for rollback, got %v", result.oldState)
	}
}

func TestTUICloseReviewInBackgroundServerError(t *testing.T) {
	_, m := mockServerModel(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/review/close" || r.Method != http.MethodPost {
			assert.Equal(t, "/api/review/close", r.URL.Path, "Unexpected request: %s %s", r.Method, r.URL.Path)
			assert.Equal(t, http.MethodPost, r.Method, "Unexpected request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	})
	cmd := m.closeReviewInBackground(42, true, false, 1, false)
	msg := cmd()

	result := assertMsgType[closedResultMsg](t, msg)
	require.Error(t, result.err,
		"expected error from server failure")
	assert.EqualValues(t, 42, result.jobID, "Expected jobID=42 for rollback, got %d", result.jobID)

	if result.oldState != false {
		assert.False(t, result.oldState, "Expected oldState=false for rollback, got %v", result.oldState)
	}
}

func TestTUIClosedRollbackOnError(t *testing.T) {
	m := setupTestModel([]storage.ReviewJob{
		makeJob(42, withStatus(storage.JobStatusDone), withClosed(new(false))),
	}, func(m *model) {
		m.selectedIdx = 0
		m.selectedJobID = 42
		m.jobStats = storage.JobStats{Done: 1, Closed: 1, Open: 0}
	})

	*m.jobs[0].Closed = true
	m.pendingClosed[42] = pendingState{newState: true, seq: 1}

	errMsg := closedResultMsg{
		jobID:            42,
		restoreSelection: false,
		oldState:         false,
		newState:         true,
		seq:              1,
		err:              fmt.Errorf("server error"),
	}

	m, _ = updateModel(t, m, errMsg)

	if m.jobs[0].Closed == nil || *m.jobs[0].Closed != false {
		if m.jobs[0].Closed == nil {
			assert.NotNil(t, m.jobs[0].Closed, "Expected closed=false after rollback, got nil")
		} else {
			assert.False(t, *m.jobs[0].Closed, "Expected closed=false after rollback, got %v", m.jobs[0].Closed)
		}
	}
	require.Error(t, m.err,
		"expected server error for rollback")

	assertJobStats(t, m, 0, 1)
}

func TestTUIClosedRollbackAfterPollRefresh(t *testing.T) {
	m := setupTestModel([]storage.ReviewJob{
		makeJob(42, withStatus(storage.JobStatusDone), withClosed(new(false))),
	}, func(m *model) {
		m.currentView = viewQueue
		m.selectedIdx = 0
		m.selectedJobID = 42
		m.jobStats = storage.JobStats{Done: 1, Closed: 0, Open: 1}
		m.pendingClosed = make(map[int64]pendingState)
	})

	result, _ := m.handleCloseKey()
	m = result.(model)
	assertJobStats(t, m, 1, 0)

	pollMsg := jobsMsg{
		jobs: []storage.ReviewJob{
			makeJob(42, withStatus(storage.JobStatusDone),
				withClosed(new(false))),
		},
		stats: storage.JobStats{Done: 1, Closed: 0, Open: 1},
	}
	m, _ = updateModel(t, m, pollMsg)

	assertJobStats(t, m, 1, 0)

	errMsg := closedResultMsg{
		jobID:            42,
		restoreSelection: false,
		oldState:         false,
		newState:         true,
		seq:              1,
		err:              fmt.Errorf("server error"),
	}
	m, _ = updateModel(t, m, errMsg)
	assertJobStats(t, m, 0, 1)
}

func TestTUIClosedPollConfirmsNoDoubleCount(t *testing.T) {
	m := setupTestModel([]storage.ReviewJob{
		makeJob(42, withStatus(storage.JobStatusDone), withClosed(new(false))),
	}, func(m *model) {
		m.currentView = viewQueue
		m.selectedIdx = 0
		m.selectedJobID = 42
		m.jobStats = storage.JobStats{Done: 1, Closed: 0, Open: 1}
		m.pendingClosed = make(map[int64]pendingState)
	})

	result, _ := m.handleCloseKey()
	m = result.(model)
	assertJobStats(t, m, 1, 0)

	pollMsg := jobsMsg{
		jobs: []storage.ReviewJob{
			makeJob(42, withStatus(storage.JobStatusDone),
				withClosed(new(true))),
		},
		stats: storage.JobStats{Done: 1, Closed: 1, Open: 0},
	}
	m, _ = updateModel(t, m, pollMsg)

	assertJobStats(t, m, 1, 0)
}

func TestTUIClosedSuccessNoRollback(t *testing.T) {
	m := setupTestModel([]storage.ReviewJob{
		makeJob(42, withStatus(storage.JobStatusDone), withClosed(new(false))),
	})

	*m.jobs[0].Closed = true

	successMsg := closedResultMsg{
		jobID:            42,
		restoreSelection: false,
		oldState:         false,
		seq:              1,
		err:              nil,
	}

	m, _ = updateModel(t, m, successMsg)

	if m.jobs[0].Closed == nil || *m.jobs[0].Closed != true {
		if m.jobs[0].Closed == nil {
			assert.NotNil(t, m.jobs[0].Closed, "Expected closed=true after success, got nil")
		} else {
			assert.True(t, *m.jobs[0].Closed, "Expected closed=true after success, got %v", m.jobs[0].Closed)
		}
	}
	require.NoError(t, m.err, "Expected no error, got %v", m.err)
}

func TestTUIClosedToggleMovesSelectionWithHideActive(t *testing.T) {
	m := setupTestModel([]storage.ReviewJob{
		makeJob(1, withClosed(new(false))),
		makeJob(2, withClosed(new(false))),
		makeJob(3, withClosed(new(false))),
	}, func(m *model) {
		m.currentView = viewQueue
		m.hideClosed = true
		m.selectedIdx = 1
		m.selectedJobID = 2
	})

	m.jobs[1].Closed = new(true)
	assert.
		False(t, m.isJobVisible(m.jobs[1]),
			"closed job should be hidden")

	prevIdx := m.findPrevVisibleJob(m.selectedIdx)
	assert.Equal(t, 2, prevIdx, "Expected prev visible job at index 2, got %d", prevIdx)
}

func TestTUIClosedRollbackRestoresSelectionAfterHideClosedMove(t *testing.T) {
	m := setupTestModel([]storage.ReviewJob{
		makeJob(1, withStatus(storage.JobStatusDone), withClosed(new(false))),
		makeJob(2, withStatus(storage.JobStatusDone), withClosed(new(false))),
		makeJob(3, withStatus(storage.JobStatusDone), withClosed(new(false))),
	}, func(m *model) {
		m.currentView = viewQueue
		m.hideClosed = true
		m.selectedIdx = 1
		m.selectedJobID = 2
		m.jobStats = storage.JobStats{Done: 3, Closed: 0, Open: 3}
		m.pendingClosed = make(map[int64]pendingState)
	})

	result, _ := m.handleCloseKey()
	m = result.(model)
	assertSelection(t, m, 2, 3)

	m, _ = updateModel(t, m, closedResultMsg{
		jobID:            2,
		restoreSelection: true,
		oldState:         false,
		newState:         true,
		seq:              1,
		err:              fmt.Errorf("server error"),
	})

	assertSelection(t, m, 1, 2)
	if m.jobs[1].Closed == nil || *m.jobs[1].Closed {
		require.False(t, m.jobs[1].Closed == nil || *m.jobs[1].Closed, "Expected job 2 to be rolled back to open, got %v", m.jobs[1].Closed)
	}
	require.Equal(t, "server error", m.flashMessage, "Expected warning flash for rollback, got %q", m.flashMessage)
	require.Equal(t, viewQueue, m.flashView, "Expected queue flash view, got %v", m.flashView)

	assert.True(t, m.flashWarning, "Expected rollback flash to use warning styling")
}

func TestTUIClosedRollbackAfterPollRefreshRestoresSelection(t *testing.T) {
	m := setupTestModel([]storage.ReviewJob{
		makeJob(1, withStatus(storage.JobStatusDone), withClosed(new(false))),
		makeJob(2, withStatus(storage.JobStatusDone), withClosed(new(false))),
		makeJob(3, withStatus(storage.JobStatusDone), withClosed(new(false))),
	}, func(m *model) {
		m.currentView = viewQueue
		m.hideClosed = true
		m.selectedIdx = 1
		m.selectedJobID = 2
		m.jobStats = storage.JobStats{Done: 3, Closed: 0, Open: 3}
		m.pendingClosed = make(map[int64]pendingState)
	})

	result, _ := m.handleCloseKey()
	m = result.(model)
	assertSelection(t, m, 2, 3)

	m, _ = updateModel(t, m, jobsMsg{
		jobs: []storage.ReviewJob{
			makeJob(1, withStatus(storage.JobStatusDone), withClosed(new(false))),
			makeJob(2, withStatus(storage.JobStatusDone), withClosed(new(false))),
			makeJob(3, withStatus(storage.JobStatusDone), withClosed(new(false))),
		},
		stats: storage.JobStats{Done: 3, Closed: 0, Open: 3},
	})
	assertSelection(t, m, 2, 3)

	m, _ = updateModel(t, m, closedResultMsg{
		jobID:            2,
		restoreSelection: true,
		oldState:         false,
		newState:         true,
		seq:              1,
		err:              fmt.Errorf("server error"),
	})

	assertSelection(t, m, 1, 2)
}

func TestTUIClosedRollbackRestoresSelectionAfterLeavingQueue(t *testing.T) {
	m := setupTestModel([]storage.ReviewJob{
		makeJob(1, withStatus(storage.JobStatusDone), withClosed(new(false))),
		makeJob(2, withStatus(storage.JobStatusDone), withClosed(new(false))),
		makeJob(3, withStatus(storage.JobStatusDone), withClosed(new(false))),
	}, func(m *model) {
		m.currentView = viewQueue
		m.hideClosed = true
		m.selectedIdx = 1
		m.selectedJobID = 2
		m.jobStats = storage.JobStats{Done: 3, Closed: 0, Open: 3}
		m.pendingClosed = make(map[int64]pendingState)
	})

	result, _ := m.handleCloseKey()
	m = result.(model)
	assertSelection(t, m, 2, 3)

	m.currentView = viewReview

	m, _ = updateModel(t, m, closedResultMsg{
		jobID:            2,
		restoreSelection: true,
		oldState:         false,
		newState:         true,
		seq:              1,
		err:              fmt.Errorf("server error"),
	})

	require.Equal(t, viewReview, m.currentView, "Expected to remain in review view, got %v", m.currentView)
	assertSelection(t, m, 1, 2)
}

// Regression: closing the last open job from review view, then
// receiving an SSE-triggered refresh that excludes the closed job,
// should preserve the anchor so ←/→ navigation and escape-to-queue
// both land near the bottom — not at the top.
func TestCloseFromReviewViewRefreshPreservesAnchor(t *testing.T) {
	assert := assert.New(t)

	// 5 open jobs; user is on the last one (index 4, ID 1).
	m := setupTestModel([]storage.ReviewJob{
		makeJob(5, withStatus(storage.JobStatusDone), withClosed(new(false))),
		makeJob(4, withStatus(storage.JobStatusDone), withClosed(new(false))),
		makeJob(3, withStatus(storage.JobStatusDone), withClosed(new(false))),
		makeJob(2, withStatus(storage.JobStatusDone), withClosed(new(false))),
		makeJob(1, withStatus(storage.JobStatusDone), withClosed(new(false))),
	}, func(m *model) {
		m.currentView = viewReview
		m.hideClosed = true
		m.selectedIdx = 4
		m.selectedJobID = 1
		m.currentReview = &storage.Review{
			ID:     10,
			Closed: false,
			Job:    &storage.ReviewJob{ID: 1, Status: storage.JobStatusDone},
		}
		m.jobStats = storage.JobStats{Done: 5, Closed: 0, Open: 5}
		m.pendingClosed = make(map[int64]pendingState)
		m.pendingReviewClosed = make(map[int64]pendingState)
	})

	// Close the review from review view (press 'a').
	result, _ := m.handleCloseKey()
	m = result.(model)

	// Simulate SSE refresh: server returns only open jobs (job 1 filtered
	// out server-side because closed=false was sent).
	m, _ = updateModel(t, m, jobsMsg{
		jobs: []storage.ReviewJob{
			makeJob(5, withStatus(storage.JobStatusDone), withClosed(new(false))),
			makeJob(4, withStatus(storage.JobStatusDone), withClosed(new(false))),
			makeJob(3, withStatus(storage.JobStatusDone), withClosed(new(false))),
			makeJob(2, withStatus(storage.JobStatusDone), withClosed(new(false))),
		},
		stats: storage.JobStats{Done: 5, Closed: 1, Open: 4},
	})

	// Anchor should be preserved (out of bounds) so navigation stays
	// relative to the closed review's original position.
	assert.Equal(4, m.selectedIdx,
		"selectedIdx should be unchanged in review view")
	assert.Equal(int64(1), m.selectedJobID,
		"selectedJobID should be unchanged in review view")

	// → (next newer) should navigate to the immediate adjacent review
	// (Job 2 at index 3), not skip it.
	nextIdx := m.stepVisibleJobIndex(-1, eligibleReviewRow)
	assert.Equal(3, nextIdx,
		"next viewable should be Job 2 (adjacent to closed Job 1)")
	assert.Equal(int64(2), m.jobs[nextIdx].ID)

	// Escape to queue: normalizeSelectionIfHidden should clamp to the
	// nearest visible job near the bottom.
	m.currentView = viewQueue
	m.normalizeSelectionIfHidden()
	assert.Equal(3, m.selectedIdx,
		"escape to queue should land near bottom, not top")
	assert.Equal(int64(2), m.selectedJobID)
}

// Regression: when a job is removed from the middle of the list while
// in a review-anchored view, selectedIdx stays in bounds but points to
// a different job. normalizeSelectionIfHidden must resync selectedJobID.
func TestNormalizeResyncsStaleJobID(t *testing.T) {
	assert := assert.New(t)

	m := setupTestModel([]storage.ReviewJob{
		makeJob(5, withStatus(storage.JobStatusDone), withClosed(new(false))),
		makeJob(4, withStatus(storage.JobStatusDone), withClosed(new(false))),
		makeJob(3, withStatus(storage.JobStatusDone), withClosed(new(false))),
		makeJob(2, withStatus(storage.JobStatusDone), withClosed(new(false))),
		makeJob(1, withStatus(storage.JobStatusDone), withClosed(new(false))),
	}, func(m *model) {
		m.currentView = viewReview
		m.hideClosed = true
		m.selectedIdx = 2
		m.selectedJobID = 3
		m.currentReview = &storage.Review{
			ID:  10,
			Job: &storage.ReviewJob{ID: 3, Status: storage.JobStatusDone},
		}
		m.pendingClosed = make(map[int64]pendingState)
		m.pendingReviewClosed = make(map[int64]pendingState)
	})

	// Refresh removes Job 3 from the middle.
	m, _ = updateModel(t, m, jobsMsg{
		jobs: []storage.ReviewJob{
			makeJob(5, withStatus(storage.JobStatusDone), withClosed(new(false))),
			makeJob(4, withStatus(storage.JobStatusDone), withClosed(new(false))),
			makeJob(2, withStatus(storage.JobStatusDone), withClosed(new(false))),
			makeJob(1, withStatus(storage.JobStatusDone), withClosed(new(false))),
		},
	})

	// Anchor preserved in review view (selectedIdx=2, selectedJobID=3).
	assert.Equal(2, m.selectedIdx)
	assert.Equal(int64(3), m.selectedJobID)

	// Escape to queue: selectedIdx=2 is in bounds but points to Job 2.
	// normalizeSelectionIfHidden must resync selectedJobID.
	m.currentView = viewQueue
	m.normalizeSelectionIfHidden()
	assert.Equal(2, m.selectedIdx)
	assert.Equal(int64(2), m.selectedJobID,
		"selectedJobID should be resynced to match jobs[selectedIdx]")
}

// Regression: prompt view opened from review should preserve the anchor
// so esc back to review keeps ←/→ navigation correct.
func TestPromptFromReviewRefreshPreservesAnchor(t *testing.T) {
	assert := assert.New(t)

	m := setupTestModel([]storage.ReviewJob{
		makeJob(3, withStatus(storage.JobStatusDone), withClosed(new(false))),
		makeJob(2, withStatus(storage.JobStatusDone), withClosed(new(false))),
		makeJob(1, withStatus(storage.JobStatusDone), withClosed(new(false))),
	}, func(m *model) {
		m.currentView = viewKindPrompt
		m.promptFromQueue = false // opened from review
		m.hideClosed = true
		m.selectedIdx = 2
		m.selectedJobID = 1
		m.currentReview = &storage.Review{
			ID:  10,
			Job: &storage.ReviewJob{ID: 1, Status: storage.JobStatusDone},
		}
	})

	// Refresh removes Job 1.
	m, _ = updateModel(t, m, jobsMsg{
		jobs: []storage.ReviewJob{
			makeJob(3, withStatus(storage.JobStatusDone), withClosed(new(false))),
			makeJob(2, withStatus(storage.JobStatusDone), withClosed(new(false))),
		},
	})

	// Anchor should be preserved (review-rooted prompt).
	assert.Equal(2, m.selectedIdx,
		"selectedIdx should be unchanged in prompt-from-review")
	assert.Equal(int64(1), m.selectedJobID,
		"selectedJobID should be unchanged in prompt-from-review")

	// → from this position should find the adjacent review (Job 2).
	nextIdx := m.stepVisibleJobIndex(-1, eligibleReviewRow)
	assert.Equal(1, nextIdx,
		"next viewable should be Job 2 (adjacent to removed Job 1)")
}

// Regression: log view opened from a review-rooted context (e.g.,
// review → prompt → log) should preserve the anchor. logReviewAnchored
// is set by openLogView based on the calling view's isReviewAnchored().
func TestLogFromReviewRefreshPreservesAnchor(t *testing.T) {
	assert := assert.New(t)

	m := setupTestModel([]storage.ReviewJob{
		makeJob(3, withStatus(storage.JobStatusDone), withClosed(new(false))),
		makeJob(2, withStatus(storage.JobStatusDone), withClosed(new(false))),
		makeJob(1, withStatus(storage.JobStatusDone), withClosed(new(false))),
	}, func(m *model) {
		m.currentView = viewLog
		m.logFromView = viewQueue // real value from review→prompt→log
		m.logReviewAnchored = true
		m.hideClosed = true
		m.selectedIdx = 2
		m.selectedJobID = 1
	})

	// Refresh removes Job 1.
	m, _ = updateModel(t, m, jobsMsg{
		jobs: []storage.ReviewJob{
			makeJob(3, withStatus(storage.JobStatusDone), withClosed(new(false))),
			makeJob(2, withStatus(storage.JobStatusDone), withClosed(new(false))),
		},
	})

	assert.Equal(2, m.selectedIdx,
		"selectedIdx should be unchanged in log-from-review")
	assert.Equal(int64(1), m.selectedJobID,
		"selectedJobID should be unchanged in log-from-review")
}

// Regression: empty-list refresh in a review-anchored view should
// preserve selectedJobID so esc back to review doesn't lose context.
func TestReviewAnchoredEmptyRefreshPreservesJobID(t *testing.T) {
	assert := assert.New(t)

	m := setupTestModel([]storage.ReviewJob{
		makeJob(1, withStatus(storage.JobStatusDone), withClosed(new(false))),
	}, func(m *model) {
		m.currentView = viewKindPrompt
		m.promptFromQueue = false
		m.hideClosed = true
		m.selectedIdx = 0
		m.selectedJobID = 1
		m.currentReview = &storage.Review{
			ID:  10,
			Job: &storage.ReviewJob{ID: 1, Status: storage.JobStatusDone},
		}
	})

	// Refresh returns empty list.
	m, _ = updateModel(t, m, jobsMsg{jobs: []storage.ReviewJob{}})

	assert.Equal(-1, m.selectedIdx)
	assert.Equal(int64(1), m.selectedJobID,
		"selectedJobID should be preserved in review-anchored prompt view")
}

// Regression: review-anchored log view without currentReview should
// still preserve selectedJobID on empty-list refresh.
func TestLogReviewAnchoredEmptyRefreshPreservesJobID(t *testing.T) {
	assert := assert.New(t)

	m := setupTestModel([]storage.ReviewJob{
		makeJob(1, withStatus(storage.JobStatusDone), withClosed(new(false))),
	}, func(m *model) {
		m.currentView = viewLog
		m.logReviewAnchored = true
		m.hideClosed = true
		m.selectedIdx = 0
		m.selectedJobID = 1
		// No currentReview — log view doesn't require it.
	})

	m, _ = updateModel(t, m, jobsMsg{jobs: []storage.ReviewJob{}})

	assert.Equal(-1, m.selectedIdx)
	assert.Equal(int64(1), m.selectedJobID,
		"selectedJobID should be preserved in review-anchored log view")
}

// Regression: prompt view opened from queue should normalize selection
// when a refresh removes the viewed job, so esc/q doesn't leave stale
// selectedIdx.
func TestPromptFromQueueRefreshNormalizesSelection(t *testing.T) {
	assert := assert.New(t)

	m := setupTestModel([]storage.ReviewJob{
		makeJob(3, withStatus(storage.JobStatusDone), withClosed(new(false))),
		makeJob(2, withStatus(storage.JobStatusDone), withClosed(new(false))),
		makeJob(1, withStatus(storage.JobStatusDone), withClosed(new(false))),
	}, func(m *model) {
		m.currentView = viewKindPrompt
		m.promptFromQueue = true
		m.hideClosed = true
		m.selectedIdx = 2
		m.selectedJobID = 1
	})

	// Refresh removes Job 1 (server-side filter).
	m, _ = updateModel(t, m, jobsMsg{
		jobs: []storage.ReviewJob{
			makeJob(3, withStatus(storage.JobStatusDone), withClosed(new(false))),
			makeJob(2, withStatus(storage.JobStatusDone), withClosed(new(false))),
		},
	})

	// Unlike viewReview, prompt view should clamp selection.
	assert.True(m.selectedIdx >= 0 && m.selectedIdx < len(m.jobs),
		"selectedIdx should be in bounds after refresh in prompt view")
}

// Regression: log view should normalize selection when a refresh removes
// the viewed job, so esc/q doesn't leave stale selectedIdx.
func TestLogViewRefreshNormalizesSelection(t *testing.T) {
	assert := assert.New(t)

	m := setupTestModel([]storage.ReviewJob{
		makeJob(3, withStatus(storage.JobStatusDone), withClosed(new(false))),
		makeJob(2, withStatus(storage.JobStatusDone), withClosed(new(false))),
		makeJob(1, withStatus(storage.JobStatusDone), withClosed(new(false))),
	}, func(m *model) {
		m.currentView = viewLog
		m.logFromView = viewQueue
		m.hideClosed = true
		m.selectedIdx = 2
		m.selectedJobID = 1
	})

	// Refresh removes Job 1.
	m, _ = updateModel(t, m, jobsMsg{
		jobs: []storage.ReviewJob{
			makeJob(3, withStatus(storage.JobStatusDone), withClosed(new(false))),
			makeJob(2, withStatus(storage.JobStatusDone), withClosed(new(false))),
		},
	})

	assert.True(m.selectedIdx >= 0 && m.selectedIdx < len(m.jobs),
		"selectedIdx should be in bounds after refresh in log view")
}

func TestTUISetJobClosedHelper(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())

	m.jobs = []storage.ReviewJob{
		makeJob(100),
	}

	m.setJobClosed(100, true)

	assert.NotNil(t, m.jobs[0].Closed, "Expected Closed to be allocated")
	if *m.jobs[0].Closed != true {
		assert.True(t, *m.jobs[0].Closed, "Expected Closed=true, got %v", *m.jobs[0].Closed)
	}

	m.setJobClosed(100, false)
	if *m.jobs[0].Closed != false {
		assert.False(t, *m.jobs[0].Closed, "Expected Closed=false, got %v", *m.jobs[0].Closed)
	}

	m.setJobClosed(999, true)
	if *m.jobs[0].Closed != false {
		assert.False(t, *m.jobs[0].Closed, "Non-existent job should not affect existing job")
	}
}

func TestTUICancelJobSuccess(t *testing.T) {
	type cancelRequest struct {
		JobID int64 `json:"job_id"`
	}
	_, m := mockServerModel(t, expectJSONPost(t, "/api/job/cancel", cancelRequest{JobID: 42}, map[string]any{"success": true}))
	oldFinishedAt := time.Now().Add(-1 * time.Hour)
	cmd := m.cancelJob(42, storage.JobStatusRunning, &oldFinishedAt, false)
	msg := cmd()

	result := assertMsgType[cancelResultMsg](t, msg)
	require.NoError(t, result.err, "Expected no error, got %v", result.err)
	assert.EqualValues(t, 42, result.jobID, "Expected jobID=42, got %d", result.jobID)
	assert.Equal(t, storage.JobStatusRunning, result.oldState, "Expected oldState=running, got %s", result.oldState)

	if result.oldFinishedAt == nil || !result.oldFinishedAt.Equal(oldFinishedAt) {
		assert.True(t, result.oldFinishedAt != nil && result.oldFinishedAt.Equal(oldFinishedAt), "Expected oldFinishedAt to be preserved")
	}
}

func TestTUICancelJobNotFound(t *testing.T) {
	_, m := mockServerModel(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": "not found"})
	})
	cmd := m.cancelJob(99, storage.JobStatusQueued, nil, false)
	msg := cmd()

	result := assertMsgType[cancelResultMsg](t, msg)
	require.Error(t, result.err,
		"expected not-found response to include error")
	assert.Equal(t, storage.JobStatusQueued, result.oldState, "Expected oldState=queued for rollback, got %s", result.oldState)
	assert.Nil(t, result.oldFinishedAt, "Expected oldFinishedAt=nil for queued job, got %v", result.oldFinishedAt)
}

func TestTUICancelRollbackOnError(t *testing.T) {
	startTime := time.Now().Add(-5 * time.Minute)
	m := setupTestModel([]storage.ReviewJob{
		makeJob(42, withStatus(storage.JobStatusRunning), withStartedAt(startTime), withFinishedAt(nil)),
	}, func(m *model) {
		m.selectedIdx = 0
		m.selectedJobID = 42
	})

	now := time.Now()
	m.jobs[0].Status = storage.JobStatusCanceled
	m.jobs[0].FinishedAt = &now

	errResult := cancelResultMsg{
		jobID:         42,
		oldState:      storage.JobStatusRunning,
		oldFinishedAt: nil,
		err:           fmt.Errorf("server error"),
	}

	m2, _ := updateModel(t, m, errResult)
	assert.Equal(t, storage.JobStatusRunning, m2.jobs[0].Status, "Expected status to rollback to 'running', got '%s'", m2.jobs[0].Status)
	assert.Nil(t, m2.jobs[0].FinishedAt, "Expected FinishedAt to rollback to nil, got %v", m2.jobs[0].FinishedAt)

	require.Error(t, m2.err,
		"expected cancel error on rollback")
}

func TestTUICancelRollbackWithNonNilFinishedAt(t *testing.T) {
	startTime := time.Now().Add(-5 * time.Minute)
	originalFinished := time.Now().Add(-2 * time.Minute)
	m := setupTestModel([]storage.ReviewJob{
		makeJob(42, withStatus(storage.JobStatusQueued), withStartedAt(startTime), withFinishedAt(&originalFinished)),
	}, func(m *model) {
		m.selectedIdx = 0
		m.selectedJobID = 42
	})

	now := time.Now()
	m.jobs[0].Status = storage.JobStatusCanceled
	m.jobs[0].FinishedAt = &now

	errResult := cancelResultMsg{
		jobID:         42,
		oldState:      storage.JobStatusQueued,
		oldFinishedAt: &originalFinished,
		err:           fmt.Errorf("server error"),
	}

	m2, _ := updateModel(t, m, errResult)
	assert.Equal(t, storage.JobStatusQueued, m2.jobs[0].Status, "Expected status to rollback to 'queued', got '%s'", m2.jobs[0].Status)

	assert.NotNil(t, m2.jobs[0].FinishedAt,
		"expected finishedAt to be restored")
	require.Error(t, m2.err,
		"expected rollback error on save failure")
}

func TestTUICancelOptimisticUpdate(t *testing.T) {
	startTime := time.Now().Add(-5 * time.Minute)
	m := setupTestModel([]storage.ReviewJob{
		makeJob(42, withStatus(storage.JobStatusRunning), withStartedAt(startTime), withFinishedAt(nil)),
	}, func(m *model) {
		m.selectedIdx = 0
		m.selectedJobID = 42
		m.currentView = viewQueue
	})

	m2, cmd := pressKey(m, 'x')

	assert.Equal(t, storage.JobStatusCanceled, m2.jobs[0].Status, "Expected status 'canceled', got '%s'", m2.jobs[0].Status)

	assert.NotNil(t, m2.jobs[0].FinishedAt, "expected optimistic cancel to set finishedAt")
	assert.NotNil(t, cmd, "expected cancel command to be returned")
}

func TestTUICancelOnlyRunningOrQueued(t *testing.T) {
	testCases := []storage.JobStatus{
		storage.JobStatusDone,
		storage.JobStatusFailed,
		storage.JobStatusCanceled,
	}

	for _, status := range testCases {
		t.Run(string(status), func(t *testing.T) {
			finishedAt := time.Now().Add(-1 * time.Hour)
			m := setupTestModel([]storage.ReviewJob{
				makeJob(1, withStatus(status), withFinishedAt(&finishedAt)),
			}, func(m *model) {
				m.selectedIdx = 0
				m.currentView = viewQueue
			})

			m2, cmd := pressKey(m, 'x')
			assert.Equal(t, status, m2.jobs[0].Status, "Expected status to remain '%s', got '%s'", status, m2.jobs[0].Status)

			if m2.jobs[0].FinishedAt == nil || !m2.jobs[0].FinishedAt.Equal(finishedAt) {
				assert.True(t, m2.jobs[0].FinishedAt != nil && m2.jobs[0].FinishedAt.Equal(finishedAt), "Expected FinishedAt to remain unchanged")
			}
			assert.Nil(t, cmd, "Expected no command for non-cancellable job, got %v", cmd)
		})
	}
}

func TestTUIRespondTextPreservation(t *testing.T) {
	m := setupTestModel([]storage.ReviewJob{
		makeJob(1, withRef("abc1234")),
		makeJob(2, withRef("def5678")),
	}, func(m *model) {
		m.selectedIdx = 0
		m.selectedJobID = 1
		m.width = 80
		m.height = 24
	})

	m, _ = pressKey(m, 'c')

	require.Equal(t, viewKindComment, m.currentView, "Expected viewKindComment, got %v", m.currentView)
	require.Equal(t, int64(1), m.commentJobID, "Expected commentJobID=1, got %d", m.commentJobID)

	m.commentText = "My draft response"

	m.currentView = m.commentFromView
	errMsg := commentResultMsg{jobID: 1, err: fmt.Errorf("network error")}
	m, _ = updateModel(t, m, errMsg)
	assert.Equal(t, "My draft response", m.commentText, "Expected text preserved after error, got %q", m.commentText)
	assert.Equal(t, int64(1), m.commentJobID, "Expected commentJobID preserved after error, got %d", m.commentJobID)

	m.currentView = viewQueue
	m.selectedIdx = 0
	m, _ = pressKey(m, 'c')
	assert.Equal(t, "My draft response", m.commentText, "Expected text preserved on retry for same job, got %q", m.commentText)

	m.currentView = viewQueue
	m.selectedIdx = 1
	m.selectedJobID = 2
	m, _ = pressKey(m, 'c')

	if m.commentText != "" {
		assert.Empty(t, m.commentText, "Expected text cleared for different job, got %q", m.commentText)
	}
	assert.Equal(t, int64(2), m.commentJobID, "Expected commentJobID=2, got %d", m.commentJobID)
}

func TestTUIRespondSuccessClearsOnlyMatchingJob(t *testing.T) {
	m := setupTestModel([]storage.ReviewJob{
		makeJob(1, withRef("abc1234")),
		makeJob(2, withRef("def5678")),
	}, func(m *model) {
		m.commentJobID = 2
		m.commentText = "New draft for job 2"
	})

	successMsg := commentResultMsg{jobID: 1, err: nil}
	m, _ = updateModel(t, m, successMsg)
	assert.Equal(t, "New draft for job 2", m.commentText, "Expected draft preserved for different job, got %q", m.commentText)
	assert.Equal(t, int64(2), m.commentJobID, "Expected commentJobID=2 preserved, got %d", m.commentJobID)

	successMsg = commentResultMsg{jobID: 2, err: nil}
	m, _ = updateModel(t, m, successMsg)

	if m.commentText != "" {
		assert.Empty(t, m.commentText, "Expected text cleared for matching job, got %q", m.commentText)
	}
	assert.Equal(t, int64(0), m.commentJobID, "Expected commentJobID=0 after success, got %d", m.commentJobID)
}

func TestTUIRespondBackspaceMultiByte(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.currentView = viewKindComment
	m.commentJobID = 1

	m, _ = pressKeys(m, []rune("Hello 世界"))
	assert.Equal(t, "Hello 世界", m.commentText, "Expected commentText='Hello 世界', got %q", m.commentText)

	m, _ = pressSpecial(m, tea.KeyBackspace)
	assert.Equal(t, "Hello 世", m.commentText, "Expected commentText='Hello 世' after backspace, got %q", m.commentText)

	m, _ = pressSpecial(m, tea.KeyBackspace)
	assert.Equal(t, "Hello ", m.commentText, "Expected commentText='Hello ' after second backspace, got %q", m.commentText)
}

func isValidUTF8(s string) bool {
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			return false
		}
		i += size
	}
	return true
}

func containsRune(s string, r rune) bool {
	for _, c := range s {
		if c == r {
			return true
		}
	}
	return false
}

func TestTUIRespondViewTruncationMultiByte(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.currentView = viewKindComment
	m.commentJobID = 1
	m.width = 30
	m.height = 20

	m.commentText = "あいうえおかきくけこさしすせそ"

	output := m.renderRespondView()
	assert.
		True(t, isValidUTF8(output),
			"rendered output should remain valid UTF-8")
	assert.
		True(t, containsRune(output, 'あ'),
			"rendered output should include original response text")

	lines := strings.Split(stripANSI(output), "\n")
	var expectedWidth int
	for _, line := range lines {
		if strings.HasPrefix(line, "│") && strings.HasSuffix(line, "│") {

			width := runewidth.StringWidth(line)
			if expectedWidth == 0 {
				expectedWidth = width
			}
			assert.Equal(t, expectedWidth, width, "Line visual width %d != expected %d: %q", width, expectedWidth, line)
		}
	}
	assert.NotEqual(t, expectedWidth, 0,
		"expected content area to produce visual width")
}

func TestTUIRespondViewTabExpansion(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.currentView = viewKindComment
	m.commentJobID = 1
	m.width = 40
	m.height = 20

	m.commentText = "a\tb\tc"

	output := m.renderRespondView()
	plainOutput := stripANSI(output)
	assert.
		NotContains(t, plainOutput, "\t",
			"tabs should be expanded to spaces in rendered output")

	assert.Contains(t, plainOutput, "a    b    c", "Expected tabs expanded to 4 spaces, got: %q", plainOutput)
}

func TestCancelKeyMovesSelectionWithHideClosed(t *testing.T) {
	m := setupTestModel([]storage.ReviewJob{
		makeJob(1, withStatus(storage.JobStatusDone), withClosed(new(false))),
		makeJob(2, withStatus(storage.JobStatusRunning)),
		makeJob(3, withStatus(storage.JobStatusDone), withClosed(new(false))),
	}, func(m *model) {
		m.currentView = viewQueue
		m.hideClosed = true
		m.selectedIdx = 1
		m.selectedJobID = 2
	})

	result, _ := m.handleCancelKey()
	m2 := result.(model)

	require.Equal(t, storage.JobStatusCanceled, m2.jobs[1].Status, "expected canceled, got %s", m2.jobs[1].Status)
	assert.
		NotEqual(t, 1, m2.selectedIdx,
			"selected index should move away from hidden canceled job")
	assert.True(t, m2.isJobVisible(m2.jobs[m2.selectedIdx]),
		"selected job should remain visible after selection move")
}

func TestClosedKeyUpdatesStatsOptimistically(t *testing.T) {
	m := setupTestModel([]storage.ReviewJob{
		makeJob(1, withStatus(storage.JobStatusDone),
			withClosed(new(false))),
		makeJob(2, withStatus(storage.JobStatusDone),
			withClosed(new(false))),
	}, func(m *model) {
		m.currentView = viewQueue
		m.selectedIdx = 0
		m.selectedJobID = 1
		m.jobStats = storage.JobStats{
			Done: 2, Closed: 0, Open: 2,
		}
		m.pendingClosed = make(map[int64]pendingState)
	})

	result, _ := m.handleCloseKey()
	m2 := result.(model)

	assertJobStats(t, m2, 1, 1)
}

func TestClosedKeyUpdatesStatsFromReviewView(t *testing.T) {
	m := setupTestModel([]storage.ReviewJob{
		makeJob(1, withStatus(storage.JobStatusDone),
			withClosed(new(false))),
	}, func(m *model) {
		m.currentView = viewReview
		m.currentReview = &storage.Review{
			ID:     42,
			Closed: false,
			Job: &storage.ReviewJob{
				ID:     1,
				Status: storage.JobStatusDone,
			},
		}
		m.jobStats = storage.JobStats{
			Done: 1, Closed: 0, Open: 1,
		}
		m.pendingClosed = make(map[int64]pendingState)
		m.pendingReviewClosed = make(map[int64]pendingState)
	})

	result, _ := m.handleCloseKey()
	m2 := result.(model)

	assertJobStats(t, m2, 1, 0)
}

func setupTestModel(jobs []storage.ReviewJob, opts ...func(*model)) model {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.jobs = jobs
	for _, opt := range opts {
		opt(&m)
	}
	return m
}

func assertSelection(t *testing.T, m model, idx int, jobID int64) {
	t.Helper()
	assert.Equal(t, idx, m.selectedIdx, "Expected selectedIdx=%d, got %d", idx, m.selectedIdx)
	assert.Equal(t, jobID, m.selectedJobID, "Expected selectedJobID=%d, got %d", jobID, m.selectedJobID)
}

func assertView(t *testing.T, m model, view viewKind) {
	t.Helper()
	assert.Equal(t, view, m.currentView, "Expected view=%d, got %d", view, m.currentView)
}

func withStartedAt(t time.Time) func(*storage.ReviewJob) {
	return func(j *storage.ReviewJob) { j.StartedAt = &t }
}

func withFinishedAt(t *time.Time) func(*storage.ReviewJob) {
	return func(j *storage.ReviewJob) { j.FinishedAt = t }
}

func expectJSONPost[Req any, Res any](t *testing.T, path string, expected Req, response Res) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method, "Expected POST, got %s", r.Method)

		if path != "" && r.URL.Path != path {
			assert.Equal(t, path, r.URL.Path, "Expected path %s, got %s", path, r.URL.Path)
		}

		var req Req
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			assert.Error(t, err, "Failed to decode request body: %v", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if diff := cmp.Diff(expected, req); diff != "" {
			assert.Empty(t, diff, "Request payload mismatch (-want +got):\n%s", diff)
			http.Error(w, "payload mismatch", http.StatusBadRequest)
			return
		}
		json.NewEncoder(w).Encode(response)
	}
}

func assertMsgType[T any](t *testing.T, msg tea.Msg) T {
	t.Helper()
	result, ok := msg.(T)
	if !ok {
		require.False(t, ok, "Expected %T, got %T: %v", new(T), msg, msg)
	}
	return result
}

func assertJobStats(t *testing.T, m model, closed, open int) {
	t.Helper()
	require.Equal(t, m.jobStats.Closed, closed, "expected Closed=%d, got %d", closed, m.jobStats.Closed)
	require.Equal(t, m.jobStats.Open, open, "expected Open=%d, got %d", open, m.jobStats.Open)
}
