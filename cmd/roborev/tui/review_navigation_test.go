package tui

import (
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"

	"go.kenn.io/roborev/internal/storage"
)

func TestTUIReviewNavigation(t *testing.T) {
	tests := []struct {
		name                 string
		initialJobs          []storage.ReviewJob
		initialIdx           int
		initialID            int64
		initialScroll        int
		key                  any
		wantIdx              int
		wantJobID            int64
		wantScroll           int
		wantFlash            string
		wantCmd              bool // Expect a command (fetch)
		checkFailedJobInline bool
	}{
		{
			name: "J Next skips queued/running",
			initialJobs: []storage.ReviewJob{
				makeJob(1),
				makeJob(2, withStatus(storage.JobStatusQueued)),
				makeJob(3, withStatus(storage.JobStatusRunning)),
				makeJob(4, withStatus(storage.JobStatusFailed)),
				makeJob(5),
			},
			initialIdx:    0,
			initialID:     1,
			initialScroll: 5,
			key:           'j',
			wantIdx:       3,
			wantJobID:     4,
			wantScroll:    0,
			wantCmd:       false, // Failed job = inline, no fetch
		},
		{
			name: "K Prev skips queued/running",
			initialJobs: []storage.ReviewJob{
				makeJob(1),
				makeJob(2, withStatus(storage.JobStatusQueued)),
				makeJob(3, withStatus(storage.JobStatusRunning)),
				makeJob(4, withStatus(storage.JobStatusFailed)),
				makeJob(5),
			},
			initialIdx:    4,
			initialID:     5,
			initialScroll: 10,
			key:           'k',
			wantIdx:       3,
			wantJobID:     4,
			wantScroll:    0,
			wantCmd:       false,
		},
		{
			name: "Left Arrow acts like J (Prev)",
			initialJobs: []storage.ReviewJob{
				makeJob(1),
				makeJob(2),
				makeJob(3),
			},
			initialIdx: 1,
			initialID:  2,
			key:        tea.KeyLeft,
			wantIdx:    2,
			wantJobID:  3,
			wantCmd:    true,
		},
		{
			name: "Right Arrow acts like K (Next)",
			initialJobs: []storage.ReviewJob{
				makeJob(1),
				makeJob(2),
				makeJob(3),
			},
			initialIdx: 1,
			initialID:  2,
			key:        tea.KeyRight,
			wantIdx:    0,
			wantJobID:  1,
			wantCmd:    true,
		},
		{
			name: "Boundary Start (K/Right)",
			initialJobs: []storage.ReviewJob{
				makeJob(1),
				makeJob(2, withStatus(storage.JobStatusQueued)),
				makeJob(3),
			},
			initialIdx: 0,
			initialID:  1,
			key:        'k',
			wantIdx:    0,
			wantJobID:  1,
			wantFlash:  "No newer review",
		},
		{
			name: "Boundary End (J/Left)",
			initialJobs: []storage.ReviewJob{
				makeJob(1),
				makeJob(2, withStatus(storage.JobStatusQueued)),
				makeJob(3),
			},
			initialIdx: 2,
			initialID:  3,
			key:        'j',
			wantIdx:    2,
			wantJobID:  3,
			wantFlash:  "No older review",
		},
		{
			name: "Navigate to Failed Job Inline",
			initialJobs: []storage.ReviewJob{
				makeJob(1),
				makeJob(2, withStatus(storage.JobStatusFailed), withAgent("codex"), withError("something went wrong")),
			},
			initialIdx:           0,
			initialID:            1,
			key:                  'j',
			wantIdx:              1,
			wantJobID:            2,
			wantCmd:              false,
			checkFailedJobInline: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newModel(localhostEndpoint, withExternalIODisabled())
			m.jobs = tt.initialJobs
			m.selectedIdx = tt.initialIdx
			m.selectedJobID = tt.initialID
			m.currentView = viewReview
			// Setup current review to match initial selection
			m.currentReview = makeReview(10, &tt.initialJobs[tt.initialIdx])
			m.reviewScroll = tt.initialScroll

			var m2 model
			var cmd tea.Cmd

			k, ok := tt.key.(rune)
			assert.True(t, ok)
			m2, cmd = pressSpecial(m, k)

			assertSelection(t, m2, tt.wantIdx, tt.wantJobID)

			assert.False(t, tt.wantScroll != -1 && m2.reviewScroll != tt.wantScroll)

			if tt.wantFlash != "" {
				assert.Equal(t, tt.wantFlash, m2.flashMessage)
				assert.Equal(t, viewReview, m2.flashView)
				assert.False(t, m2.flashExpiresAt.IsZero())
				assert.True(t, m2.flashExpiresAt.After(time.Now()))
			}

			assert.Equal(t, tt.wantCmd, cmd != nil, "command presence mismatch")

			// Specific check for failed job inline content
			if tt.checkFailedJobInline {
				assert.NotNil(t, m2.currentReview)
				assert.Contains(t, m2.currentReview.Output, "something went wrong")
				assert.Equal(t, "codex", m2.currentReview.Agent)
			}
		})
	}
}

func TestTUIReviewStaleResponseIgnored(t *testing.T) {
	// Test that stale review responses are ignored (race condition fix)
	m := newModel(localhostEndpoint, withExternalIODisabled())

	m.jobs = []storage.ReviewJob{
		makeJob(1),
		makeJob(2),
	}
	m.selectedIdx = 1
	m.selectedJobID = 2 // Currently viewing job 2
	m.currentView = viewReview
	m.currentReview = makeReview(20, &storage.ReviewJob{ID: 2}, withReviewOutput("Review for job 2"))

	// Simulate a stale response arriving for job 1 (user navigated away)
	staleMsg := reviewMsg{
		review: makeReview(10, &storage.ReviewJob{ID: 1}, withReviewOutput("Stale review for job 1")),
		jobID:  1, // This doesn't match selectedJobID (2)
	}

	m2, _ := updateModel(t, m, staleMsg)

	// Should ignore the stale response
	assert.Equal(t, "Review for job 2", m2.currentReview.Output)
	assert.EqualValues(t, 20, m2.currentReview.ID)
}

func TestTUIReviewMsgWithMatchingJobID(t *testing.T) {
	// Test that review responses with matching job ID are accepted
	m := newModel(localhostEndpoint, withExternalIODisabled())

	m.jobs = []storage.ReviewJob{
		makeJob(1),
	}
	m.selectedIdx = 0
	m.selectedJobID = 1
	m.currentView = viewQueue // Still in queue view, waiting for fetch

	validMsg := reviewMsg{
		review: makeReview(10, &storage.ReviewJob{ID: 1}, withReviewOutput("New review")),
		jobID:  1,
	}

	m2, _ := updateModel(t, m, validMsg)

	// Should accept the response and switch to review view
	assert.Equal(t, viewReview, m2.currentView)
	assert.False(t, m2.currentReview == nil || m2.currentReview.Output != "New review")
	assert.Equal(t, 0, m2.reviewScroll)
}

func TestTUISelectionSyncInReviewView(t *testing.T) {
	// Test that selectedIdx syncs with currentReview.Job.ID when jobs refresh
	m := newModel(localhostEndpoint, withExternalIODisabled())

	// Initial state: viewing review for job 2
	m.jobs = []storage.ReviewJob{
		makeJob(3),
		makeJob(2),
		makeJob(1),
	}
	m.selectedIdx = 1
	m.selectedJobID = 2
	m.currentView = viewReview
	m.currentReview = makeReview(20, &storage.ReviewJob{ID: 2})

	// New job arrives at the top, shifting indices
	newJobs := jobsMsg{jobs: []storage.ReviewJob{
		makeJob(4), // New job at top
		makeJob(3),
		makeJob(2), // Now at index 2
		makeJob(1),
	}}

	m2, _ := updateModel(t, m, newJobs)

	// selectedIdx should sync with currentReview.Job.ID (2), now at index 2
	assert.Equal(t, 2, m2.selectedIdx)
	assert.EqualValues(t, 2, m2.selectedJobID)
}

func TestTUIJobsRefreshDuringReviewNavigation(t *testing.T) {
	// Test that jobs refresh during review navigation doesn't reset selection
	// This tests the race condition fix: user navigates to job 3, but jobs refresh
	// arrives before the review loads. Selection should stay on job 3, not revert
	// to the currently displayed review's job (job 2).
	m := newModel(localhostEndpoint, withExternalIODisabled())

	m.jobs = []storage.ReviewJob{
		makeJob(1),
		makeJob(2),
		makeJob(3),
	}
	m.selectedIdx = 1
	m.selectedJobID = 2
	m.currentView = viewReview
	m.currentReview = makeReview(20, &storage.ReviewJob{ID: 2}, withReviewOutput("Review for job 2"))

	// Simulate user navigating to next review (job 3)
	// This updates selectedIdx and selectedJobID but doesn't update currentReview yet
	m.selectedIdx = 2
	m.selectedJobID = 3

	// Before the review for job 3 arrives, a jobs refresh comes in
	refreshedJobs := jobsMsg{jobs: []storage.ReviewJob{
		makeJob(1),
		makeJob(2),
		makeJob(3),
	}}

	m2, _ := updateModel(t, m, refreshedJobs)

	// Selection should stay on job 3 (user's navigation intent), not revert to job 2
	assert.EqualValues(t, 3, m2.selectedJobID)
	assert.Equal(t, 2, m2.selectedIdx)

	// currentReview should still be the old one (review for job 3 hasn't loaded)
	assert.EqualValues(t, 2, m2.currentReview.Job.ID)

	// Now when the review for job 3 arrives, it should be accepted
	newReviewMsg := reviewMsg{
		review: makeReview(30, &storage.ReviewJob{ID: 3}, withReviewOutput("Review for job 3")),
		jobID:  3,
	}

	m3, _ := updateModel(t, m2, newReviewMsg)

	assert.EqualValues(t, 30, m3.currentReview.ID)
	assert.Equal(t, "Review for job 3", m3.currentReview.Output)
}

func TestTUIEmptyRefreshWhileViewingReview(t *testing.T) {
	// Test that transient empty jobs refresh doesn't break selection
	// when viewing a review. Selection should restore to displayed review
	// when jobs repopulate.
	m := newModel(localhostEndpoint, withExternalIODisabled())

	m.jobs = []storage.ReviewJob{
		makeJob(1),
		makeJob(2),
		makeJob(3),
	}
	m.selectedIdx = 1
	m.selectedJobID = 2
	m.currentView = viewReview
	m.currentReview = makeReview(20, &storage.ReviewJob{ID: 2}, withReviewOutput("Review for job 2"))

	// Transient empty refresh arrives
	emptyJobs := jobsMsg{jobs: []storage.ReviewJob{}}

	m2, _ := updateModel(t, m, emptyJobs)

	// selectedJobID should be preserved (not cleared) while viewing a review
	assert.EqualValues(t, 2, m2.selectedJobID)

	// Jobs repopulate
	repopulatedJobs := jobsMsg{jobs: []storage.ReviewJob{
		makeJob(1),
		makeJob(2),
		makeJob(3),
	}}

	m3, _ := updateModel(t, m2, repopulatedJobs)

	// Selection should restore to job 2 (the displayed review)
	assert.EqualValues(t, 2, m3.selectedJobID)
	assert.Equal(t, 1, m3.selectedIdx)
}

func TestTUIEmptyRefreshSeedsFromCurrentReview(t *testing.T) {
	// Test that if selectedJobID somehow becomes 0 while viewing a review,
	// it gets seeded from the current review when jobs repopulate
	m := newModel(localhostEndpoint, withExternalIODisabled())

	m.jobs = []storage.ReviewJob{}
	m.selectedIdx = 0
	m.selectedJobID = 0 // Somehow cleared
	m.currentView = viewReview
	m.currentReview = makeReview(20, &storage.ReviewJob{ID: 2}, withReviewOutput("Review for job 2"))

	// Jobs repopulate
	repopulatedJobs := jobsMsg{jobs: []storage.ReviewJob{
		makeJob(1),
		makeJob(2),
		makeJob(3),
	}}

	m2, _ := updateModel(t, m, repopulatedJobs)

	// Selection should be seeded from currentReview.Job.ID
	assert.EqualValues(t, 2, m2.selectedJobID)
	assert.Equal(t, 1, m2.selectedIdx)
}
