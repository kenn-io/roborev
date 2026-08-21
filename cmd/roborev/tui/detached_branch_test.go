package tui

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"go.kenn.io/roborev/internal/storage"
)

// TestDetachedBranchLabel exercises the placeholder used when a review job's
// branch is empty because the commit was made on top of a detached HEAD
// (e.g. mid git-bisect), per issue #499. A blank branch cell reads as a bug,
// so the queue/tasks views and review screen should show something like
// "(detached @ <sha>)" instead - but only for jobs where a branch is
// meaningful in the first place.
func TestDetachedBranchLabel(t *testing.T) {
	commitID := int64(42)
	tests := []struct {
		name string
		job  storage.ReviewJob
		want string
	}{
		{
			name: "stored branch wins, no placeholder needed",
			job:  storage.ReviewJob{Branch: "main", GitRef: "abc1234", CommitID: &commitID},
			want: "",
		},
		{
			name: "detached commit review with no stored branch",
			job:  storage.ReviewJob{JobType: storage.JobTypeReview, GitRef: "abc1234567", CommitID: &commitID},
			want: "(detached @ abc1234)",
		},
		{
			name: "detached commit review, short sha left as-is",
			job:  storage.ReviewJob{JobType: storage.JobTypeReview, GitRef: "abc12", CommitID: &commitID},
			want: "(detached @ abc12)",
		},
		{
			name: "backfilled branchNone sentinel counts as stored, no placeholder",
			job:  storage.ReviewJob{JobType: storage.JobTypeReview, Branch: branchNone, GitRef: "abc1234567", CommitID: &commitID},
			want: "",
		},
		{
			name: "task job has no branch concept",
			job:  storage.ReviewJob{JobType: storage.JobTypeTask, GitRef: "abc1234567"},
			want: "",
		},
		{
			name: "dirty job has no branch concept",
			job:  storage.ReviewJob{JobType: storage.JobTypeDirty, GitRef: "dirty"},
			want: "",
		},
		{
			name: "fix job from a task parent carries a non-SHA ref, no placeholder",
			job:  storage.ReviewJob{JobType: storage.JobTypeFix, GitRef: "analyze"},
			want: "",
		},
		{
			name: "fix job on a detached commit shows placeholder",
			job:  storage.ReviewJob{JobType: storage.JobTypeFix, GitRef: "abc1234def"},
			want: "(detached @ abc1234)",
		},
		{
			name: "panel synthesis row on a detached commit shows placeholder",
			job:  storage.ReviewJob{JobType: storage.JobTypeSynthesis, GitRef: "abc1234def", CommitID: &commitID},
			want: "(detached @ abc1234)",
		},
		{
			name: "synthesis of a dirty panel stays blank",
			job:  storage.ReviewJob{JobType: storage.JobTypeSynthesis, GitRef: "dirty"},
			want: "",
		},
		{
			name: "synthesis of a range panel stays blank",
			job:  storage.ReviewJob{JobType: storage.JobTypeSynthesis, GitRef: "abc1234..def5678"},
			want: "",
		},
		{
			name: "synthesis of a prompt panel carries a non-SHA ref, stays blank",
			job:  storage.ReviewJob{JobType: storage.JobTypeSynthesis, GitRef: "analyze"},
			want: "",
		},
		{
			name: "CI review uses its stored branch",
			job: storage.ReviewJob{
				JobType:      storage.JobTypeReview,
				GitRef:       "abc1234567",
				CommitID:     &commitID,
				Branch:       "feature/contributor",
				Source:       storage.JobSourceCI,
				CIBaseBranch: "main",
			},
			want: "",
		},
		{
			name: "range refs are left unchanged",
			job:  storage.ReviewJob{JobType: storage.JobTypeRange, GitRef: "abc1234..def5678", CommitID: &commitID},
			want: "",
		},
		{
			name: "empty ref",
			job:  storage.ReviewJob{JobType: storage.JobTypeReview},
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := detachedBranchLabel(tt.job)
			assert.Equal(t, tt.want, got)
		})
	}
}
