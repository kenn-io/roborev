package tui

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"go.kenn.io/roborev/internal/storage"
)

// TestTaskCellsBranchColumn covers the Tasks view "Branch" column (#499):
// a job enqueued on a detached HEAD should show a "(detached @ <sha>)"
// placeholder instead of a blank cell, while jobs that have (or should
// have) no branch concept stay blank.
func TestTaskCellsBranchColumn(t *testing.T) {
	commitID := int64(7)
	tests := []struct {
		name       string
		job        storage.ReviewJob
		wantBranch string
	}{
		{
			name:       "stored branch is used as-is",
			job:        storage.ReviewJob{JobType: storage.JobTypeReview, Branch: "main", GitRef: "abc1234567", CommitID: &commitID},
			wantBranch: "main",
		},
		{
			name:       "detached commit review shows placeholder",
			job:        storage.ReviewJob{JobType: storage.JobTypeReview, GitRef: "abc1234567", CommitID: &commitID},
			wantBranch: "(detached @ abc1234)",
		},
		{
			name:       "task job stays blank",
			job:        storage.ReviewJob{JobType: storage.JobTypeTask, GitRef: "abc1234567"},
			wantBranch: "",
		},
		{
			name:       "backfilled branchNone renders detached placeholder",
			job:        storage.ReviewJob{JobType: storage.JobTypeReview, Branch: branchNone, GitRef: "abc1234567", CommitID: &commitID},
			wantBranch: "(detached @ abc1234)",
		},
		{
			name:       "branchNone on task job keeps the sentinel",
			job:        storage.ReviewJob{JobType: storage.JobTypeTask, Branch: branchNone, GitRef: "abc1234567"},
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
