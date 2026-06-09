package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"

	"go.kenn.io/roborev/internal/storage"
)

func TestTUIReviewMsgSetsBranchName(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.jobs = []storage.ReviewJob{
		makeJob(1),
	}
	m.selectedIdx = 0
	m.selectedJobID = 1
	m.currentView = viewQueue

	// Receive review message with branch name
	msg := reviewMsg{
		review:     makeReview(10, &storage.ReviewJob{ID: 1}, withReviewOutput("Review text")),
		jobID:      1,
		branchName: "main",
	}

	m2, _ := updateModel(t, m, msg)

	assert.Equal(t, "main", m2.currentBranch)
}

func TestReviewBranchName(t *testing.T) {
	tests := []struct {
		name string
		job  *storage.ReviewJob
		want string
	}{
		{
			name: "nil job",
			job:  nil,
			want: "",
		},
		{
			name: "stored branch preferred over git lookup",
			job:  &storage.ReviewJob{Branch: "main", GitRef: "abc123", RepoPath: "/tmp/repo"},
			want: "main",
		},
		{
			name: "stored branch for range review",
			job:  &storage.ReviewJob{Branch: "main", GitRef: "abc123..def456"},
			want: "main",
		},
		{
			name: "branchNone sentinel treated as empty",
			job:  &storage.ReviewJob{Branch: "(none)", GitRef: "abc123"},
			want: "",
		},
		{
			name: "branchNone with repo path skips git lookup",
			job:  &storage.ReviewJob{Branch: "(none)", GitRef: "abc123", RepoPath: "/tmp/repo"},
			want: "",
		},
		{
			name: "no stored branch and range skips git lookup",
			job:  &storage.ReviewJob{GitRef: "abc123..def456", RepoPath: "/tmp/repo"},
			want: "",
		},
		{
			name: "no stored branch and no repo path",
			job:  &storage.ReviewJob{GitRef: "abc123"},
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := reviewBranchName(tt.job)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestTUIReviewMsgEmptyBranchForRange(t *testing.T) {
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.jobs = []storage.ReviewJob{
		makeJob(1, withRef("abc123..def456")),
	}
	m.selectedIdx = 0
	m.selectedJobID = 1
	m.currentView = viewQueue

	// Receive review message with empty branch (range commits don't have branches)
	msg := reviewMsg{
		review:     makeReview(10, &storage.ReviewJob{ID: 1, GitRef: "abc123..def456"}, withReviewOutput("Review text")),
		jobID:      1,
		branchName: "", // Empty for ranges
	}

	m2, _ := updateModel(t, m, msg)

	assert.Empty(t, m2.currentBranch)
}

func TestTUIBranchClearedOnFailedJobNavigation(t *testing.T) {
	// Test that navigating from a successful review with branch to a failed job clears the branch
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.width = 100
	m.height = 30
	m.currentView = viewReview
	m.currentBranch = "main" // Cached from previous review
	m.selectedIdx = 0
	m.selectedJobID = 1

	// Set up jobs: current is done (idx 0), next is failed (idx 1)
	m.jobs = []storage.ReviewJob{
		makeJob(1, withRef("abc123")),
		makeJob(2, withStatus(storage.JobStatusFailed), withRef("def456"), withError("some error")),
	}
	m.currentReview = makeReview(10, &m.jobs[0], withReviewOutput("Good review"))

	// Navigate down to failed job (j or down key in review view)
	m2, _ := pressKey(m, 'j')

	// Branch should be cleared
	assert.Empty(t, m2.currentBranch)

	// Should still be in review view showing the failed job
	assert.Equal(t, viewReview, m2.currentView)
	assert.False(t, m2.currentReview == nil || !strings.Contains(m2.currentReview.Output, "Job failed"))
}

func TestTUIBranchClearedOnFailedJobEnter(t *testing.T) {
	// Test that pressing Enter on a failed job clears the branch
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.width = 100
	m.height = 30
	m.currentView = viewQueue
	m.currentBranch = "feature/old" // Stale from previous review
	m.selectedIdx = 0
	m.selectedJobID = 1

	m.jobs = []storage.ReviewJob{
		makeJob(1, withStatus(storage.JobStatusFailed), withRef("abc123"), withError("build failed")),
	}

	// Press Enter to view the failed job
	m2, _ := pressSpecial(m, tea.KeyEnter)

	// Branch should be cleared
	assert.Empty(t, m2.currentBranch)

	// Should show review view with error
	assert.Equal(t, viewReview, m2.currentView)
}

func TestTUIRenderQueueViewBranchFilterOnlyNoPanic(t *testing.T) {
	// Test that renderQueueView doesn't panic when branch filter is active
	// but repo filter is empty (regression test for index out of range)
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.width = 100
	m.height = 30
	m.currentView = viewQueue
	m.activeBranchFilter = "feature"
	m.activeRepoFilter = nil // Empty repo filter
	m.filterStack = []string{"branch"}
	m.jobs = []storage.ReviewJob{
		makeJob(1, withBranch("feature"), withRepoName("test")),
	}

	// This should not panic
	output := m.View().Content

	// Should show branch filter indicator
	assert.Contains(t, output, "[b: feature]")
	// Should NOT show repo filter indicator (since no repo filter)
	assert.NotContains(t, output, "[f:")
}
