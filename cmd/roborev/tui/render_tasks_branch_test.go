package tui

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

// TestTaskCellsBranchColumn covers the Tasks view "Branch" column (#499):
// a job enqueued on a detached HEAD should show a "(detached @ <sha>)"
// placeholder instead of a blank cell, while jobs that have (or should
// have) no branch concept stay blank.
func TestTaskCellsBranchColumn(t *testing.T) {
	repo := testutil.InitTestRepo(t)
	firstSHA := repo.CommitFile("a.txt", "one", "first")
	repo.CheckoutDetached()
	detachedSHA := repo.CommitFile("a.txt", "two", "second, on detached HEAD")
	// Branch from the first commit so detachedSHA stays unreachable.
	repo.CheckoutNewBranch("feature-x", firstSHA)
	reachableSHA := repo.CommitFile("b.txt", "three", "third, on a branch")

	commitID := int64(7)
	tests := []struct {
		name       string
		job        storage.ReviewJob
		wantBranch string
	}{
		{
			name:       "stored branch is used as-is",
			job:        storage.ReviewJob{ID: 1, JobType: storage.JobTypeReview, Branch: "main", GitRef: detachedSHA, CommitID: &commitID},
			wantBranch: "main",
		},
		{
			name:       "detached commit review shows placeholder",
			job:        storage.ReviewJob{ID: 2, JobType: storage.JobTypeReview, GitRef: detachedSHA, RepoPath: repo.Path(), CommitID: &commitID},
			wantBranch: "(detached @ " + detachedSHA[:7] + ")",
		},
		{
			name:       "task job stays blank",
			job:        storage.ReviewJob{ID: 3, JobType: storage.JobTypeTask, GitRef: "analyze"},
			wantBranch: "",
		},
		{
			name:       "fix job on a detached commit shows placeholder (Tasks view data path)",
			job:        storage.ReviewJob{ID: 4, JobType: storage.JobTypeFix, GitRef: detachedSHA, RepoPath: repo.Path()},
			wantBranch: "(detached @ " + detachedSHA[:7] + ")",
		},
		{
			name:       "branchless fix job on a reachable commit shows the real branch",
			job:        storage.ReviewJob{ID: 5, JobType: storage.JobTypeFix, GitRef: reachableSHA, RepoPath: repo.Path()},
			wantBranch: "feature-x",
		},
		{
			name:       "fix job from a task parent stays blank",
			job:        storage.ReviewJob{ID: 6, JobType: storage.JobTypeFix, GitRef: "analyze", RepoPath: repo.Path()},
			wantBranch: "",
		},
		{
			name:       "unverifiable branchNone keeps the sentinel",
			job:        storage.ReviewJob{ID: 7, JobType: storage.JobTypeReview, Branch: branchNone, GitRef: detachedSHA, CommitID: &commitID},
			wantBranch: branchNone,
		},
		{
			name:       "branchNone on task job keeps the sentinel",
			job:        storage.ReviewJob{ID: 8, JobType: storage.JobTypeTask, Branch: branchNone, GitRef: "analyze"},
			wantBranch: branchNone,
		},
	}
	m := newModel(localhostEndpoint, withExternalIODisabled())
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cells := m.taskCells(tt.job)
			// taskCells omits the always-visible tcolSel selection column, so
			// every other column constant is offset by one in the slice.
			assert.Equal(t, tt.wantBranch, cells[tcolBranch-1])
		})
	}
}
