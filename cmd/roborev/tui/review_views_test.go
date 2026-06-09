package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
)

func TestTUIEscapeFromReviewTriggersRefreshWithHideClosed(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.currentView = viewReview
	m.hideClosed = true
	m.loadingJobs = false

	m.jobs = []storage.ReviewJob{
		makeJob(1, withClosed(new(false))),
	}
	m.currentReview = makeReview(1, &storage.ReviewJob{ID: 1})

	// Press escape to return to queue view
	m2, cmd := pressSpecial(m, tea.KeyEscape)

	if m2.currentView != viewQueue {
		assert.Condition(t, func() bool {
			return false
		}, "Expected to return to queue view")
	}
	if !m2.loadingJobs {
		assert.Condition(t, func() bool {
			return false
		}, "Expected loadingJobs to be true when escaping with hideClosed active")
	}
	if cmd == nil {
		assert.Condition(t, func() bool {
			return false
		}, "Expected a command to be returned for refresh")
	}
}

func TestTUIEscapeFromReviewNoRefreshWithoutHideClosed(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.currentView = viewReview
	m.hideClosed = false
	m.loadingJobs = false

	m.jobs = []storage.ReviewJob{
		makeJob(1, withClosed(new(false))),
	}
	m.currentReview = makeReview(1, &storage.ReviewJob{ID: 1})

	// Press escape to return to queue view
	m2, cmd := pressSpecial(m, tea.KeyEscape)

	if m2.currentView != viewQueue {
		assert.Condition(t, func() bool {
			return false
		}, "Expected to return to queue view")
	}
	if m2.loadingJobs {
		assert.Condition(t, func() bool {
			return false
		}, "Should not trigger refresh when hideClosed is not active")
	}
	// cmd may be non-nil (mouse re-enable on view transition) but
	// the important assertion is that no job refresh was triggered
	// (loadingJobs stays false above).
	_ = cmd
}

func TestTUICommitMsgViewNavigationFromQueue(t *testing.T) {
	// Test that pressing escape in commit message view returns to the originating view (queue)
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.jobs = []storage.ReviewJob{makeJob(1, withRef("abc123"))}
	m.selectedIdx = 0
	m.selectedJobID = 1
	m.currentView = viewQueue
	m.commitMsgJobID = 1            // Set to match incoming message (normally set by 'm' key handler)
	m.commitMsgFromView = viewQueue // Track where we came from

	// Simulate receiving commit message content (sets view to CommitMsg)
	m2, _ := updateModel(t, m, commitMsgMsg{jobID: 1, content: "test message"})

	if m2.currentView != viewCommitMsg {
		assert.Condition(t, func() bool {
			return false
		}, "Expected viewCommitMsg, got %d", m2.currentView)
	}

	// Press escape to go back
	m3, _ := pressSpecial(m2, tea.KeyEscape)

	if m3.currentView != viewQueue {
		assert.Condition(t, func() bool {
			return false
		}, "Expected to return to viewQueue, got %d", m3.currentView)
	}
	if m3.commitMsgContent != "" {
		assert.Condition(t, func() bool {
			return false
		}, "Expected commitMsgContent to be cleared")
	}
}

func TestTUICommitMsgViewNavigationFromReview(t *testing.T) {
	// Test that pressing escape in commit message view returns to the originating view (review)
	m := newModel(localhostEndpoint, withExternalIODisabled())
	j := makeJob(1, withRef("abc123"))
	m.jobs = []storage.ReviewJob{j}
	m.currentReview = makeReview(1, &j)
	m.currentView = viewReview
	m.commitMsgFromView = viewReview
	m.commitMsgContent = "test message"
	m.currentView = viewCommitMsg

	// Press escape to go back
	m2, _ := pressSpecial(m, tea.KeyEscape)

	if m2.currentView != viewReview {
		assert.Condition(t, func() bool {
			return false
		}, "Expected to return to viewReview, got %d", m2.currentView)
	}
}

func TestTUICommitMsgViewNavigationWithQ(t *testing.T) {
	// Test that pressing 'q' in commit message view also returns to originating view
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.currentView = viewCommitMsg
	m.commitMsgFromView = viewReview
	m.commitMsgContent = "test message"

	// Press 'q' to go back
	m2, _ := pressKey(m, 'q')

	if m2.currentView != viewReview {
		assert.Condition(t, func() bool {
			return false
		}, "Expected to return to viewReview after 'q', got %d", m2.currentView)
	}
}

func TestTUICtrlDQuitsFromQueueView(t *testing.T) {
	m := initTestModel(withCurrentView(viewQueue))

	_, cmd := pressCtrl(m, 'd')

	if cmd == nil {
		require.Condition(t, func() bool {
			return false
		}, "Expected quit command")
	}
	assertMsgType[tea.QuitMsg](t, cmd())
}

func TestTUICtrlDNavigatesBackFromReviewView(t *testing.T) {
	job := makeJob(1)
	m := initTestModel(
		withCurrentView(viewReview),
		withReview(&storage.Review{JobID: 1, Job: &job}),
		withReviewFromView(viewQueue),
	)

	got, _ := pressCtrl(m, 'd')

	assertView(t, got, viewQueue)
}

func TestTUICtrlDNoOpInCommentModal(t *testing.T) {
	m := initTestModel(withCurrentView(viewKindComment))
	m.commentFromView = viewQueue
	m.commentText = "draft comment"
	m.commentJobID = 42

	got, cmd := pressCtrl(m, 'd')

	assertView(t, got, viewKindComment)
	if got.commentText != "draft comment" {
		assert.Condition(t, func() bool {
			return false
		}, "Expected comment text preserved, got %q", got.commentText)
	}
	if got.commentJobID != 42 {
		assert.Condition(t, func() bool {
			return false
		}, "Expected comment job ID preserved, got %d", got.commentJobID)
	}
	if cmd != nil {
		require.Condition(t, func() bool {
			return false
		}, "Expected no command from Ctrl-D in comment modal")
	}
}

func TestFetchCommitMsgJobTypeDetection(t *testing.T) {
	// Test that fetchCommitMsg correctly identifies job types and returns appropriate errors
	// This is critical: Prompt field is populated for ALL jobs (stores review prompt),
	// so we must use IsTaskJob() to identify task jobs, not Prompt != ""

	m := newModel(localhostEndpoint, withExternalIODisabled())

	tests := []struct {
		name        string
		job         storage.ReviewJob
		expectError string // empty means no early error (will try git lookup)
	}{
		{
			name: "regular commit with Prompt populated should not error early",
			job: storage.ReviewJob{
				ID:       1,
				JobType:  storage.JobTypeReview,
				GitRef:   "abc123def456",               // valid commit SHA
				Prompt:   "You are a code reviewer...", // review prompt is stored for all jobs
				CommitID: func() *int64 { id := int64(123); return &id }(),
			},
			expectError: "", // should attempt git lookup, not return "task jobs" error
		},
		{
			name: "run task (GitRef=prompt) should error",
			job: storage.ReviewJob{
				ID:      2,
				JobType: storage.JobTypeTask,
				GitRef:  "prompt",
				Prompt:  "Explain this codebase",
			},
			expectError: "no commit message for task jobs",
		},
		{
			name: "run task (GitRef=run) should error",
			job: storage.ReviewJob{
				ID:      8,
				JobType: storage.JobTypeTask,
				GitRef:  "run",
				Prompt:  "Do something",
			},
			expectError: "no commit message for task jobs",
		},
		{
			name: "analyze task should error",
			job: storage.ReviewJob{
				ID:      9,
				JobType: storage.JobTypeTask,
				GitRef:  "analyze",
				Prompt:  "Analyze these files",
			},
			expectError: "no commit message for task jobs",
		},
		{
			name: "custom label task should error",
			job: storage.ReviewJob{
				ID:      10,
				JobType: storage.JobTypeTask,
				GitRef:  "my-custom-task",
				Prompt:  "Do my custom task",
			},
			expectError: "no commit message for task jobs",
		},
		{
			name: "dirty job (JobType=dirty) should error",
			job: storage.ReviewJob{
				ID:      3,
				JobType: storage.JobTypeDirty,
				GitRef:  "dirty",
			},
			expectError: "no commit message for uncommitted changes",
		},
		{
			name: "dirty job with DiffContent should error",
			job: storage.ReviewJob{
				ID:          4,
				JobType:     storage.JobTypeDirty,
				GitRef:      "some-ref",
				DiffContent: func() *string { s := "diff content"; return &s }(),
			},
			expectError: "no commit message for uncommitted changes",
		},
		{
			name: "empty GitRef should error with missing ref message",
			job: storage.ReviewJob{
				ID:     5,
				GitRef: "",
			},
			expectError: "no git reference available for this job",
		},
		{
			name: "empty GitRef with Prompt (backward compat run job) should error with missing ref",
			job: storage.ReviewJob{
				ID:     6,
				GitRef: "",
				Prompt: "Explain this codebase", // older run job without GitRef=prompt
			},
			expectError: "no git reference available for this job",
		},
		{
			name: "dirty job with nil DiffContent but JobType=dirty should error",
			job: storage.ReviewJob{
				ID:          7,
				JobType:     storage.JobTypeDirty,
				GitRef:      "dirty",
				DiffContent: nil,
			},
			expectError: "no commit message for uncommitted changes",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := m.fetchCommitMsg(&tt.job)
			msg := cmd()

			result, ok := msg.(commitMsgMsg)
			if !ok {
				require.Condition(t, func() bool {
					return false
				}, "Expected commitMsgMsg, got %T", msg)
			}

			if tt.expectError != "" {
				if result.err == nil {
					assert.Condition(t, func() bool {
						return false
					}, "Expected error %q, got nil", tt.expectError)
				} else if result.err.Error() != tt.expectError {
					assert.Condition(t, func() bool {
						return false
					}, "Expected error %q, got %q", tt.expectError, result.err.Error())
				}
			} else {
				// For valid commits, we expect a git error (repo doesn't exist in test)
				// but NOT the "task jobs" or "uncommitted changes" error
				if result.err != nil {
					errMsg := result.err.Error()
					if errMsg == "no commit message for task jobs" {
						assert.Condition(t, func() bool {
							return false
						}, "Regular commit with Prompt should not be detected as task job")
					}
					if errMsg == "no commit message for uncommitted changes" {
						assert.Condition(t, func() bool {
							return false
						}, "Regular commit should not be detected as uncommitted changes")
					}
					// Other errors (like git errors) are expected in test environment
				}
			}
		})
	}
}

func TestTUIHelpViewToggleFromQueue(t *testing.T) {
	// Test that '?' opens help from queue and pressing '?' again returns to queue
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.currentView = viewQueue

	// Press '?' to open help
	m2, _ := pressKey(m, '?')

	if m2.currentView != viewHelp {
		assert.Condition(t, func() bool {
			return false
		}, "Expected viewHelp, got %d", m2.currentView)
	}
	if m2.helpFromView != viewQueue {
		assert.Condition(t, func() bool {
			return false
		}, "Expected helpFromView to be viewQueue, got %d", m2.helpFromView)
	}

	// Press '?' again to close help
	m3, _ := pressKey(m2, '?')

	if m3.currentView != viewQueue {
		assert.Condition(t, func() bool {
			return false
		}, "Expected to return to viewQueue, got %d", m3.currentView)
	}
}

func TestTUIHelpViewToggleFromReview(t *testing.T) {
	// Test that '?' opens help from review and escape returns to review
	m := newModel(localhostEndpoint, withExternalIODisabled())
	j := makeJob(1, withRef("abc123"))
	m.currentReview = makeReview(1, &j)
	m.currentView = viewReview

	// Press '?' to open help
	m2, _ := pressKey(m, '?')

	if m2.currentView != viewHelp {
		assert.Condition(t, func() bool {
			return false
		}, "Expected viewHelp, got %d", m2.currentView)
	}
	if m2.helpFromView != viewReview {
		assert.Condition(t, func() bool {
			return false
		}, "Expected helpFromView to be viewReview, got %d", m2.helpFromView)
	}

	// Press escape to close help
	m3, _ := pressSpecial(m2, tea.KeyEscape)

	if m3.currentView != viewReview {
		assert.Condition(t, func() bool {
			return false
		}, "Expected to return to viewReview, got %d", m3.currentView)
	}
}
