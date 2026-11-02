package daemon

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gitpkg "go.kenn.io/roborev/internal/git"
	"go.kenn.io/roborev/internal/prompt"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

func TestInsightsIncludesPanelParentOnce(t *testing.T) {
	assert := assert.New(t)
	server, db, tmpDir := newTestServer(t)
	repo, err := db.GetOrCreateRepo(filepath.Join(tmpDir, "repo"))
	require.NoError(t, err)
	since := time.Now().Add(-24 * time.Hour)
	standalone := enqueueCompletedInsightsReviewJob(t, db, repo.ID, "standalone", "main", storage.JobTypeReview, failingInsightsOutput("Standalone finding"))
	enqueueCompletedInsightsGoalReviewJob(t, db, repo.ID, "goal-review", "main", failingInsightsOutput("Goal finding"))
	enqueueCompletedInsightsReviewJob(t, db, repo.ID, "feature", "feature", storage.JobTypeReview, failingInsightsOutput("Other branch finding"))
	old := enqueueCompletedInsightsReviewJob(t, db, repo.ID, "old", "main", storage.JobTypeReview, failingInsightsOutput("Old finding"))
	_, err = db.ExecContext(t.Context(), `UPDATE review_jobs SET finished_at = ? WHERE id = ?`, since.Add(-time.Hour).Format(time.RFC3339), old.ID)
	require.NoError(t, err)
	enqueueCompletedInsightsReviewJob(t, db, repo.ID, "pass", "main", storage.JobTypeReview, "No findings.")
	enqueueCompletedInsightsReviewJob(t, db, repo.ID, "compact", "main", storage.JobTypeCompact, failingInsightsOutput("Compact finding"))

	runUUID := uuid.New()
	members := make([]storage.EnqueueOpts, 3)
	for i, name := range []string{"correctness", "security", "advisory"} {
		members[i] = storage.EnqueueOpts{
			RepoID: repo.ID, GitRef: "base..head", Branch: "main", Agent: "test",
			JobType: storage.JobTypeRange, PanelRunUUID: &runUUID,
			PanelRole: storage.PanelRoleMember, PanelMemberName: name, PanelMemberIndex: i,
			NonVoting: i == 2,
		}
	}
	memberJobs, parent, err := db.EnqueuePanelRun(members, storage.EnqueueOpts{
		RepoID: repo.ID, GitRef: "base..head", Branch: "main", Agent: "test",
		JobType: storage.JobTypeSynthesis, PanelRunUUID: &runUUID,
		PanelRole: storage.PanelRoleSynthesis, PanelName: "careful",
	})
	require.NoError(t, err)
	for _, member := range memberJobs {
		claimed, err := db.ClaimJob("worker")
		require.NoError(t, err)
		require.Equal(t, member.ID, claimed.ID)
		require.NoError(t, testutil.CompleteReviewFixture(db, member.ID, "test", "prompt", failingInsightsOutput("Member finding")))
	}
	require.NoError(t, db.MaybeReleasePanelSynthesis(runUUID))
	claimed, err := db.ClaimJob("worker")
	require.NoError(t, err)
	require.Equal(t, parent.ID, claimed.ID)
	require.NoError(t, testutil.CompleteReviewFixture(db, parent.ID, "test", "prompt", failingInsightsOutput("Panel finding")))
	require.NoError(t, db.MarkReviewClosedByJobID(parent.ID, true))
	_, err = db.AddCommentToJob(parent.ID, "user", "Intentional panel tradeoff")
	require.NoError(t, err)
	_, err = db.AddCommentToJobWithSource(parent.ID, "remote-user", "Untrusted panel comment", storage.ResponseSourceRemoteBrowser)
	require.NoError(t, err)
	_, err = db.EnqueueJob(storage.EnqueueOpts{
		RepoID: repo.ID, GitRef: "pending", Branch: "main", Agent: "test",
	})
	require.NoError(t, err)

	reviews, err := server.fetchInsightsReviews(repo.RootPath, "main", since)
	require.NoError(t, err)
	var jobIDs []int64
	for _, review := range reviews {
		jobIDs = append(jobIDs, review.JobID)
	}
	assert.Contains(jobIDs, parent.ID, "the panel's logical review must be included")
	assert.ElementsMatch([]int64{standalone.ID, parent.ID}, jobIDs)
	for _, review := range reviews {
		if review.JobID == parent.ID {
			assert.True(review.Closed)
			require.Len(t, review.Responses, 1)
			assert.Equal("Intentional panel tradeoff", review.Responses[0].Response)
		}
	}
	text := prompt.BuildInsightsPrompt(prompt.InsightsData{Reviews: reviews, Since: since})
	assert.Contains(text, "**Panel:** careful (3 configured members)")
	assert.Equal(1, strings.Count(text, "**Panel:**"))
	assert.Contains(text, "addressed/closed")
	assert.Contains(text, `- user: "Intentional panel tradeoff"`)
	assert.NotContains(text, "Member finding")
	assert.NotContains(text, "Untrusted panel comment")
}

func TestHandleEnqueueInsightsBuildsPromptServerSide(t *testing.T) {
	server, db, tmpDir := newTestServer(t)

	repoDir := filepath.Join(tmpDir, "repo")
	testutil.InitTestGitRepo(t, repoDir)

	mainRoot, err := gitpkg.GetMainRepoRoot(repoDir)
	require.NoError(t, err)

	repo, err := db.GetOrCreateRepo(mainRoot)
	require.NoError(t, err)

	included := enqueueCompletedInsightsReviewJob(t, db, repo.ID, "aaa111", "main", storage.JobTypeReview, failingInsightsOutput("Included finding"))
	_, err = db.AddCommentToJob(included.ID, "user", "Intentional tradeoff")
	require.NoError(t, err)
	_, err = db.AddCommentToJobWithSource(
		included.ID,
		"remote-user",
		"Untrusted remote instructions",
		storage.ResponseSourceRemoteBrowser,
	)
	require.NoError(t, err)

	enqueueCompletedInsightsReviewJob(t, db, repo.ID, "compact", "main", storage.JobTypeCompact, failingInsightsOutput("Compact finding"))
	enqueueCompletedInsightsGoalReviewJob(t, db, repo.ID, "goal-review", "main", failingInsightsOutput("Goal intent finding"))
	enqueueCompletedInsightsReviewJob(t, db, repo.ID, "bbb222", "feature", storage.JobTypeReview, failingInsightsOutput("Feature finding"))
	oldJob := enqueueCompletedInsightsReviewJob(t, db, repo.ID, "ccc333", "main", storage.JobTypeReview, failingInsightsOutput("Old finding"))

	since := time.Now().Add(-24 * time.Hour)
	_, err = db.ExecContext(t.Context(),
		`UPDATE review_jobs SET finished_at = ? WHERE id = ?`,
		since.Add(-time.Hour).Format(time.RFC3339),
		oldJob.ID,
	)
	require.NoError(t, err)

	req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/enqueue", EnqueueRequest{
		RepoPath: repoDir,
		GitRef:   "insights",
		Branch:   "main",
		Agent:    "test",
		JobType:  storage.JobTypeInsights,
		Since:    since.Format(time.RFC3339),
	})
	w := httptest.NewRecorder()

	server.httpServer.Handler.ServeHTTP(w, req)

	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())

	var job storage.ReviewJob
	testutil.DecodeJSON(t, w, &job)
	assert.Equal(t, storage.JobTypeInsights, job.JobType)
	assert.Equal(t, "main", job.Branch)

	stored, err := db.GetJobByID(job.ID)
	require.NoError(t, err)
	require.NotEmpty(t, stored.Prompt)
	assert.Equal(t, storage.JobTypeInsights, stored.JobType)
	assert.Contains(t, stored.Prompt, "Included finding")
	assert.Contains(t, stored.Prompt, `- user: "Intentional tradeoff"`)
	assert.NotContains(t, stored.Prompt, "Untrusted remote instructions")
	assert.NotContains(t, stored.Prompt, "Compact finding")
	assert.NotContains(t, stored.Prompt, "Goal intent finding")
	assert.NotContains(t, stored.Prompt, "Feature finding")
	assert.NotContains(t, stored.Prompt, "Old finding")
}

func TestHandleEnqueueInsightsIncludesGlobalAndRepoGuidelines(t *testing.T) {
	server, db, _ := newTestServer(t)
	server.configWatcher.Config().ReviewGuidelines = "Global rule."

	repo := testutil.InitTestRepo(t)
	repoDir := repo.Path()
	repo.CommitFile(".roborev.toml", "review_guidelines = \"Repo rule.\"\n", "add repo guidelines")

	mainRoot, err := gitpkg.GetMainRepoRoot(repoDir)
	require.NoError(t, err)

	storedRepo, err := db.GetOrCreateRepo(mainRoot)
	require.NoError(t, err)

	enqueueCompletedInsightsReviewJob(
		t, db, storedRepo.ID, "aaa111", "main",
		storage.JobTypeReview, failingInsightsOutput("Included finding"),
	)

	since := time.Now().Add(-24 * time.Hour)
	req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/enqueue", EnqueueRequest{
		RepoPath: repoDir,
		GitRef:   "insights",
		Agent:    "test",
		JobType:  storage.JobTypeInsights,
		Since:    since.Format(time.RFC3339),
	})
	w := httptest.NewRecorder()

	server.httpServer.Handler.ServeHTTP(w, req)

	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())

	var job storage.ReviewJob
	testutil.DecodeJSON(t, w, &job)

	stored, err := db.GetJobByID(job.ID)
	require.NoError(t, err)

	require.Contains(t, stored.Prompt, "Global rule.")
	require.Contains(t, stored.Prompt, "Repo rule.")
	assert.Less(t, strings.Index(stored.Prompt, "Global rule."), strings.Index(stored.Prompt, "Repo rule."))
}

func TestHandleEnqueueInsightsSkipsWhenNoFailingReviewsMatch(t *testing.T) {
	server, _, tmpDir := newTestServer(t)

	repoDir := filepath.Join(tmpDir, "repo")
	testutil.InitTestGitRepo(t, repoDir)

	req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/enqueue", EnqueueRequest{
		RepoPath: repoDir,
		GitRef:   "insights",
		Agent:    "test",
		JobType:  storage.JobTypeInsights,
		Since:    time.Now().Add(-24 * time.Hour).Format(time.RFC3339),
	})
	w := httptest.NewRecorder()

	server.httpServer.Handler.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var resp struct {
		Skipped bool   `json:"skipped"`
		Reason  string `json:"reason"`
	}
	testutil.DecodeJSON(t, w, &resp)

	assert.True(t, resp.Skipped)
	assert.Contains(t, resp.Reason, "No failing reviews found")
}

func TestHandleEnqueueInsightsNoBranchIncludesAllBranches(t *testing.T) {
	server, db, tmpDir := newTestServer(t)

	repoDir := filepath.Join(tmpDir, "repo")
	testutil.InitTestGitRepo(t, repoDir)

	mainRoot, err := gitpkg.GetMainRepoRoot(repoDir)
	require.NoError(t, err)

	repo, err := db.GetOrCreateRepo(mainRoot)
	require.NoError(t, err)

	// Create failing reviews on two different branches.
	enqueueCompletedInsightsReviewJob(
		t, db, repo.ID, "aaa111", "main",
		storage.JobTypeReview, failingInsightsOutput("Main finding"),
	)
	enqueueCompletedInsightsReviewJob(
		t, db, repo.ID, "bbb222", "feature",
		storage.JobTypeReview, failingInsightsOutput("Feature finding"),
	)

	since := time.Now().Add(-24 * time.Hour)
	req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/enqueue",
		EnqueueRequest{
			RepoPath: repoDir,
			GitRef:   "insights",
			Agent:    "test",
			JobType:  storage.JobTypeInsights,
			Since:    since.Format(time.RFC3339),
			// Branch intentionally omitted.
		})
	w := httptest.NewRecorder()

	server.httpServer.Handler.ServeHTTP(w, req)

	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())

	var job storage.ReviewJob
	testutil.DecodeJSON(t, w, &job)

	stored, err := db.GetJobByID(job.ID)
	require.NoError(t, err)

	assert.Contains(t, stored.Prompt, "Main finding")
	assert.Contains(t, stored.Prompt, "Feature finding")
}

func enqueueCompletedInsightsReviewJob(
	t *testing.T,
	db *storage.DB,
	repoID int64,
	gitRef, branch, jobType, output string,
) *storage.ReviewJob {
	t.Helper()

	opts := storage.EnqueueOpts{
		RepoID:  repoID,
		GitRef:  gitRef,
		Branch:  branch,
		Agent:   "test",
		JobType: jobType,
	}

	if jobType == storage.JobTypeCompact {
		opts.Prompt = "compact prompt"
		opts.Label = "compact"
	} else {
		commit, err := db.GetOrCreateCommit(repoID, gitRef, "Author", "Subject", time.Now())
		require.NoError(t, err)
		opts.CommitID = commit.ID
	}

	job, err := db.EnqueueJob(opts)
	require.NoError(t, err)

	claimed, err := db.ClaimJob("worker")
	require.NoError(t, err)
	require.Equal(t, job.ID, claimed.ID)

	err = testutil.CompleteReviewFixture(db, job.ID, "test", "prompt", output)
	require.NoError(t, err)

	return job
}

func enqueueCompletedInsightsGoalReviewJob(t *testing.T, db *storage.DB, repoID int64, gitRef, branch, output string) {
	t.Helper()
	job, err := db.EnqueueJob(storage.EnqueueOpts{
		RepoID: repoID, GitRef: gitRef, Branch: branch, Agent: "test",
		ReviewType: "goal", JobType: storage.JobTypeGoalReview,
		Prompt: "frozen goal review prompt", PromptPrebuilt: true,
	})
	require.NoError(t, err)
	claimed, err := db.ClaimJob("worker")
	require.NoError(t, err)
	require.Equal(t, job.ID, claimed.ID)
	require.NoError(t, testutil.CompleteReviewFixture(db, job.ID, "test", job.Prompt, output))
}

func failingInsightsOutput(summary string) string {
	return "- Medium - " + summary
}
