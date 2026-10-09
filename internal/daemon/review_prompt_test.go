package daemon

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/prompt"
	"go.kenn.io/roborev/internal/storage"
)

func TestGetReviewDisplaysFixPlanPrompt(t *testing.T) {
	server, db, tmpDir := newTestServer(t)
	repo, err := db.GetOrCreateRepo(tmpDir)
	require.NoError(t, err)
	job, err := db.EnqueueJob(storage.EnqueueOpts{
		RepoID: repo.ID, GitRef: "a..b", Agent: "test", JobType: storage.JobTypeFix,
		Prompt: prompt.EncodeFixPlan("planning instructions", "implementation instructions"),
	})
	require.NoError(t, err)
	_, err = db.ClaimJob("worker")
	require.NoError(t, err)
	require.NoError(t, db.CompleteFixJob(job.ID, "test", "Fixed it", "patch"))

	out, err := server.humaGetReview(t.Context(), &GetReviewInput{JobID: job.ID})
	require.NoError(t, err)
	assert.Equal(t,
		"## Planning Prompt\n\nplanning instructions\n\n## Implementation Prompt\n\nimplementation instructions",
		out.Body.Prompt,
	)
}
