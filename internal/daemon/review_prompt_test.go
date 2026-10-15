package daemon

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/prompt"
	"go.kenn.io/roborev/internal/storage"
)

const displayedFixPlanPrompt = "## Planning Prompt\n\nplanning instructions\n\n" +
	"## Implementation Prompt\n\nimplementation instructions"

// completedPlanFixJob stores a finished plan-first fix job, whose stored
// prompt is the fix plan envelope.
func completedPlanFixJob(t *testing.T, db *storage.DB, repoPath string) int64 {
	t.Helper()
	repo, err := db.GetOrCreateRepo(repoPath)
	require.NoError(t, err)
	job, err := db.EnqueueJob(storage.EnqueueOpts{
		RepoID: repo.ID, GitRef: "a..b", Agent: "test", JobType: storage.JobTypeFix,
		Prompt: prompt.EncodeFixPlan("planning instructions", "implementation instructions"),
	})
	require.NoError(t, err)
	_, err = db.ClaimJob("worker")
	require.NoError(t, err)
	require.NoError(t, db.CompleteFixJob(job.ID, "test", "Fixed it", "patch"))
	return job.ID
}

func TestBatchJobsDisplaysFixPlanPrompt(t *testing.T) {
	server, db, tmpDir := newTestServer(t)
	jobID := completedPlanFixJob(t, db, tmpDir)

	out, err := server.humaBatchJobs(t.Context(), &BatchJobsInput{Body: BatchJobsRequest{JobIDs: []int64{jobID}}})
	require.NoError(t, err)
	review := out.Body.Results[jobID].Review
	require.NotNil(t, review)
	assert.Equal(t, displayedFixPlanPrompt, review.Prompt)
}

func TestGetReviewDisplaysFixPlanPrompt(t *testing.T) {
	server, db, tmpDir := newTestServer(t)
	jobID := completedPlanFixJob(t, db, tmpDir)

	out, err := server.humaGetReview(t.Context(), &GetReviewInput{JobID: jobID})
	require.NoError(t, err)
	assert.Equal(t, displayedFixPlanPrompt, out.Body.Prompt)
}
