package storage_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"go.kenn.io/roborev/internal/storage"
)

func TestIsTaskJob(t *testing.T) {
	assert := assert.New(t)

	tests := []struct {
		name string
		job  storage.ReviewJob
		want bool
	}{
		// Explicit JobType
		{
			name: "explicit: single commit review by job_type",
			job:  storage.ReviewJob{JobType: storage.JobTypeReview, CommitID: new(int64(1)), GitRef: "abc123"},
			want: false,
		},
		{
			name: "explicit: dirty review by job_type",
			job:  storage.ReviewJob{JobType: storage.JobTypeDirty, GitRef: "dirty"},
			want: false,
		},
		{
			name: "explicit: dirty review by job_type with diff content",
			job:  storage.ReviewJob{JobType: storage.JobTypeDirty, GitRef: "dirty", DiffContent: new("diff")},
			want: false,
		},
		{
			name: "explicit: range review by job_type",
			job:  storage.ReviewJob{JobType: storage.JobTypeRange, GitRef: "abc123..def456"},
			want: false,
		},
		{
			name: "explicit: task job by job_type",
			job:  storage.ReviewJob{JobType: storage.JobTypeTask, GitRef: "run:lint"},
			want: true,
		},
		{
			name: "explicit: task job analyze by job_type",
			job:  storage.ReviewJob{JobType: storage.JobTypeTask, GitRef: "analyze"},
			want: true,
		},
		{
			name: "explicit: insights job by job_type",
			job:  storage.ReviewJob{JobType: storage.JobTypeInsights, GitRef: "insights"},
			want: true,
		},
		// Inferred JobType
		{
			name: "inferred: single commit review",
			job:  storage.ReviewJob{CommitID: new(int64(1)), GitRef: "abc123"},
			want: false,
		},
		{
			name: "inferred: dirty review",
			job:  storage.ReviewJob{GitRef: "dirty"},
			want: false,
		},
		{
			name: "inferred: dirty review with diff content",
			job:  storage.ReviewJob{GitRef: "dirty", DiffContent: new("diff")},
			want: false,
		},
		{
			name: "inferred: branch range review",
			job:  storage.ReviewJob{GitRef: "abc123..def456"},
			want: false,
		},
		{
			name: "inferred: triple-dot range review",
			job:  storage.ReviewJob{GitRef: "main...feature"},
			want: false,
		},
		{
			name: "inferred: task job with label",
			job:  storage.ReviewJob{GitRef: "run:lint"},
			want: true,
		},
		{
			name: "inferred: task job analyze",
			job:  storage.ReviewJob{GitRef: "analyze"},
			want: true,
		},
		{
			name: "inferred: task job run",
			job:  storage.ReviewJob{GitRef: "run"},
			want: true,
		},
		{
			name: "inferred: empty git ref",
			job:  storage.ReviewJob{GitRef: ""},
			want: false,
		},
	}

	for _, tt := range tests {
		assert.Equal(tt.want, tt.job.IsTaskJob(), "%q: IsTaskJob() = %v, want %v", tt.name, tt.job.IsTaskJob(), tt.want)
	}
}

func TestIsDirtyJob(t *testing.T) {
	assert := assert.New(t)

	tests := []struct {
		name string
		job  storage.ReviewJob
		want bool
	}{
		// Explicit JobType
		{
			name: "explicit: dirty by job_type",
			job:  storage.ReviewJob{JobType: storage.JobTypeDirty, GitRef: "dirty"},
			want: true,
		},
		{
			name: "explicit: review by job_type",
			job:  storage.ReviewJob{JobType: storage.JobTypeReview, GitRef: "abc123"},
			want: false,
		},
		{
			name: "explicit: task by job_type",
			job:  storage.ReviewJob{JobType: storage.JobTypeTask, GitRef: "run"},
			want: false,
		},
		// Inferred JobType
		{
			name: "inferred: git_ref dirty",
			job:  storage.ReviewJob{GitRef: "dirty"},
			want: true,
		},
		{
			name: "inferred: diff content set",
			job:  storage.ReviewJob{GitRef: "some-ref", DiffContent: new("diff")},
			want: true,
		},
		{
			name: "inferred: normal commit",
			job:  storage.ReviewJob{GitRef: "abc123", CommitID: new(int64(1))},
			want: false,
		},
		{
			name: "inferred: range",
			job:  storage.ReviewJob{GitRef: "abc..def"},
			want: false,
		},
	}

	for _, tt := range tests {
		assert.Equal(tt.want, tt.job.IsDirtyJob(), "%q: IsDirtyJob() = %v, want %v", tt.name, tt.job.IsDirtyJob(), tt.want)
	}
}

func TestIsReviewJob(t *testing.T) {
	assert := assert.New(t)

	tests := []struct {
		name string
		job  storage.ReviewJob
		want bool
	}{
		// Explicit JobType
		{name: "explicit: review", job: storage.ReviewJob{JobType: storage.JobTypeReview, GitRef: "abc123"}, want: true},
		{name: "explicit: range", job: storage.ReviewJob{JobType: storage.JobTypeRange, GitRef: "a..b"}, want: true},
		{name: "explicit: dirty", job: storage.ReviewJob{JobType: storage.JobTypeDirty, GitRef: "dirty"}, want: true},
		{name: "explicit: task", job: storage.ReviewJob{JobType: storage.JobTypeTask, GitRef: "run"}, want: false},
		{name: "explicit: insights", job: storage.ReviewJob{JobType: storage.JobTypeInsights, GitRef: "insights"}, want: false},
		{name: "explicit: compact", job: storage.ReviewJob{JobType: storage.JobTypeCompact, GitRef: "compact"}, want: false},
		{name: "explicit: fix", job: storage.ReviewJob{JobType: storage.JobTypeFix, GitRef: "abc123"}, want: false},
		// Inferred (empty job_type from old data)
		{name: "inferred: commit review", job: storage.ReviewJob{CommitID: new(int64(1)), GitRef: "abc123"}, want: true},
		{name: "inferred: dirty", job: storage.ReviewJob{GitRef: "dirty"}, want: true},
		{name: "inferred: range", job: storage.ReviewJob{GitRef: "abc..def"}, want: true},
		{name: "inferred: task label", job: storage.ReviewJob{GitRef: "run:lint"}, want: false},
		{name: "inferred: empty ref", job: storage.ReviewJob{GitRef: ""}, want: false},
	}

	for _, tt := range tests {
		assert.Equal(tt.want, tt.job.IsReviewJob(), "%q: IsReviewJob() = %v, want %v", tt.name, tt.job.IsReviewJob(), tt.want)
	}
}

func TestJobStatusSkipped(t *testing.T) {
	assert.Equal(t, storage.JobStatusSkipped, storage.JobStatus("skipped"))
}

func TestJobTypeClassify(t *testing.T) {
	assert.Equal(t, storage.JobTypeClassify, "classify")
}

func TestReviewJobHasSkipReason(t *testing.T) {
	j := storage.ReviewJob{}
	j.SkipReason = "trivial diff"
	assert.Equal(t, "trivial diff", j.SkipReason)
}

func TestUsesStoredPrompt(t *testing.T) {
	assert := assert.New(t)

	tests := []struct {
		name string
		job  storage.ReviewJob
		want bool
	}{
		{name: "task", job: storage.ReviewJob{JobType: storage.JobTypeTask}, want: true},
		{name: "insights", job: storage.ReviewJob{JobType: storage.JobTypeInsights}, want: true},
		{name: "compact", job: storage.ReviewJob{JobType: storage.JobTypeCompact}, want: true},
		{name: "review", job: storage.ReviewJob{JobType: storage.JobTypeReview}, want: false},
		{name: "range", job: storage.ReviewJob{JobType: storage.JobTypeRange}, want: false},
		{name: "dirty", job: storage.ReviewJob{JobType: storage.JobTypeDirty}, want: false},
		{name: "empty", job: storage.ReviewJob{JobType: ""}, want: false},
		{name: "security", job: storage.ReviewJob{JobType: "security"}, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(tt.want, tt.job.UsesStoredPrompt())
		})
	}
}
