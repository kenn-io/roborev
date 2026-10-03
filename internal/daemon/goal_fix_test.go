package daemon

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/goalreview"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

func TestGoalReviewCannotParentCodeFix(t *testing.T) {
	server, db, _ := newTestServer(t)
	repo := testutil.NewGitRepo(t)
	repo.RunGit("commit", "--allow-empty", "-m", "Initial test commit")
	record, err := db.GetOrCreateRepo(repo.Path())
	require.NoError(t, err)
	job, err := db.EnqueueJob(storage.EnqueueOpts{RepoID: record.ID, Agent: "test", GitRef: "snapshot-digest", JobType: storage.JobTypeGoalReview, ReviewType: "goal"})
	require.NoError(t, err)
	_, err = db.ClaimJob(testWorkerID)
	require.NoError(t, err)
	require.NoError(t, db.CompleteJob(job.ID, "test", "frozen prompt", "- medium: spec.md:1: Clarify the requirement"))
	w := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(w, testutil.MakeJSONRequest(t, http.MethodPost, "/api/job/fix", FixJobRequest{ParentJobID: job.ID}))
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "goal reviews")
	jobs, err := db.ListJobs("", "", 0, 0)
	require.NoError(t, err)
	assert.Len(t, jobs, 1)
}

func TestGoalWorkerHonorsQuotaCooldown(t *testing.T) {
	c := newWorkerTestContext(t, 1)
	c.Pool.cfgGetter.Config().DefaultBackupAgent = ""
	snapshot := goalreview.Snapshot{Source: "superpowers", Stage: "spec", Artifacts: []goalreview.Artifact{{Kind: "spec", Path: "spec.md", Content: "# Feature\n"}}}
	job, err := c.DB.EnqueueJob(storage.EnqueueOpts{RepoID: c.Repo.ID, Agent: "test", GitRef: snapshot.ID(), JobType: storage.JobTypeGoalReview, ReviewType: "goal", Prompt: goalreview.BuildPrompt(snapshot), PromptPrebuilt: true})
	require.NoError(t, err)
	c.Pool.cooldownAgent("test", time.Now().Add(time.Hour))
	claimed, err := c.DB.ClaimJob(testWorkerID)
	require.NoError(t, err)
	require.NotNil(t, claimed)
	c.Pool.processJob(testWorkerID, claimed)
	completed, err := c.DB.GetJobByID(job.ID)
	require.NoError(t, err)
	assert.Equal(t, storage.JobStatusFailed, completed.Status)
	assert.Contains(t, completed.Error, "quota cooldown")
}
