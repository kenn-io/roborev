package tui

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
)

func TestGoalReviewCommitMessage(t *testing.T) {
	t.Parallel()
	m := newModel(localhostEndpoint, withExternalIODisabled())
	job := storage.ReviewJob{
		ID: 1, JobType: storage.JobTypeGoalReview,
		GitRef: strings.Repeat("a", 64), RepoPath: t.TempDir(),
	}
	msg := m.fetchCommitMsg(&job)()
	result, ok := msg.(commitMsgMsg)
	require.True(t, ok)
	assert.Equal(t, job.ID, result.jobID)
	require.EqualError(t, result.err, "no commit message for goal reviews")
	assert.Empty(t, result.content)
}

func TestGoalReviewBranchDisplay(t *testing.T) {
	t.Parallel()
	m := newModel(localhostEndpoint, withExternalIODisabled())
	job := storage.ReviewJob{
		ID: 1, JobType: storage.JobTypeGoalReview,
		GitRef: strings.Repeat("a", 64), RepoPath: t.TempDir(),
	}
	assert := assert.New(t)
	assert.Empty(m.getBranchForJob(job))
	assert.Empty(reviewBranchName(&job))
	assert.Empty(detachedBranchLabel(job))
	branch, persist := backfillBranchValue(job, nil)
	assert.Equal(branchNone, branch)
	assert.True(persist)

	job.Branch = "feature"
	assert.Equal("feature", m.getBranchForJob(job))
	assert.Equal("feature", reviewBranchName(&job))
}
