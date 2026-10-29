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
	_, err = db.Exec(`UPDATE review_jobs SET finished_at = ? WHERE id = ?`, since.Add(-time.Hour).Format(time.RFC3339), old.ID)
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
	_, err = db.Exec(
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

// A wrong completion cutoff/population would mix the measured and excerpt windows.
func TestBuildInsightsPromptMeasuredWindow(t *testing.T) {
	server, db, tmpDir := newTestServer(t)
	repo, err := db.GetOrCreateRepo(filepath.Join(tmpDir, "repo"))
	require.NoError(t, err)
	since := time.Now().Add(-24 * time.Hour)
	included := enqueueCompletedInsightsReviewJob(t, db, repo.ID, "included", "main", storage.JobTypeReview, failingInsightsOutput("Current finding"))
	enqueueCompletedInsightsReviewJob(t, db, repo.ID, "passing", "main", storage.JobTypeReview, "No findings.")
	future := enqueueCompletedInsightsReviewJob(t, db, repo.ID, "future", "main", storage.JobTypeReview, failingInsightsOutput("FUTURE FINDING"))
	_, err = db.Exec(`UPDATE review_jobs SET finished_at=? WHERE id=?`, time.Now().Add(time.Hour).Format(time.RFC3339), future.ID)
	require.NoError(t, err)
	require.NoError(t, db.MarkReviewClosedByJobID(included.ID, true))
	_, err = db.AddCommentToJob(included.ID, "user", "This is intentional")
	require.NoError(t, err)
	text, count, err := server.buildInsightsPrompt(t.Context(), repo.RootPath, "main", since)
	require.NoError(t, err)
	assert := assert.New(t)
	assert.Equal(1, count)
	assert.Contains(text, `"logical_reviews": 2`)
	assert.Contains(text, `"labeled": 0`)
	assert.Contains(text, `"closed_reviews": 1`)
	assert.NotContains(text, "FUTURE FINDING")
}

func TestPriorInsightsCompletionAndScope(t *testing.T) {
	server, db, tmpDir := newTestServer(t)
	repo, err := db.GetOrCreateRepo(filepath.Join(tmpDir, "repo"))
	require.NoError(t, err)
	asOf := time.Now()
	makePrior := func(branch, suggestion string, completed time.Time) *storage.ReviewJob {
		t.Helper()
		job, err := db.EnqueueJob(storage.EnqueueOpts{RepoID: repo.ID, GitRef: "insights", Branch: branch, Agent: "test", JobType: storage.JobTypeInsights, Prompt: "old prompt"})
		require.NoError(t, err)
		claimed, err := db.ClaimJob("worker")
		require.NoError(t, err)
		require.Equal(t, job.ID, claimed.ID)
		require.NoError(t, testutil.CompleteReviewFixture(db, job.ID, "test", "old prompt", "## 5. Suggested Guideline Additions\n\n"+suggestion))
		_, err = db.Exec(`UPDATE review_jobs SET finished_at=? WHERE id=?`, completed.Format(time.RFC3339Nano), job.ID)
		require.NoError(t, err)
		return job
	}
	latest := makePrior("", "ALL BRANCHES", asOf.Add(-time.Hour))
	_, err = db.Exec(`UPDATE review_jobs SET uuid=? WHERE id=?`, "00000000-0000-4000-8000-000000000002", latest.ID)
	require.NoError(t, err)
	tie := makePrior("", "LOWER STABLE IDENTITY", asOf.Add(-time.Hour))
	_, err = db.Exec(`UPDATE review_jobs SET uuid=? WHERE id=?`, "00000000-0000-4000-8000-000000000001", tie.ID)
	require.NoError(t, err)
	makePrior("", "OLDER COMPLETION", asOf.Add(-2*time.Hour))
	makePrior("main", "EXACT MAIN", asOf.Add(-time.Minute))
	makePrior("", "FUTURE", asOf.Add(time.Hour))
	makePrior("", "EXACT CUTOFF", asOf)
	prior, err := server.fetchPriorInsights(repo.RootPath, "", asOf)
	require.NoError(t, err)
	require.NotNil(t, prior)
	assert.Equal(t, latest.ID, prior.JobID)
	assert.Equal(t, "ALL BRANCHES", prior.Suggestions)
	assert.Nil(t, prior.Baseline)
	mainPrior, err := server.fetchPriorInsights(repo.RootPath, "main", asOf)
	require.NoError(t, err)
	require.NotNil(t, mainPrior)
	assert.Equal(t, "EXACT MAIN", mainPrior.Suggestions)

	// Baselines are recovered only from complete, scope-matching stored context.
	baseline := &storage.FindingStats{Since: asOf.Add(-48 * time.Hour), AsOf: asOf.Add(-24 * time.Hour), RepoPath: repo.RootPath, Branch: "main", TimeBasis: "finished_at", LogicalReviews: 7}
	baselinePrompt := prompt.BuildInsightsPrompt(prompt.InsightsData{Measured: baseline})
	require.NoError(t, db.SaveJobPrompt(mainPrior.JobID, baselinePrompt))
	withBaseline, err := server.fetchPriorInsights(repo.RootPath, "main", asOf)
	require.NoError(t, err)
	require.NotNil(t, withBaseline.Baseline)
	assert.Equal(t, 7, withBaseline.Baseline.LogicalReviews)
	baseline.Branch = "other"
	require.NoError(t, db.SaveJobPrompt(mainPrior.JobID, prompt.BuildInsightsPrompt(prompt.InsightsData{Measured: baseline})))
	wrongScope, err := server.fetchPriorInsights(repo.RootPath, "main", asOf)
	require.NoError(t, err)
	assert.Nil(t, wrongScope.Baseline)

	empty := makePrior("main", "", asOf.Add(-30*time.Second))
	missing, err := server.fetchPriorInsights(repo.RootPath, "main", asOf)
	require.NoError(t, err)
	assert.Equal(t, empty.ID, missing.JobID)
	assert.Empty(t, missing.Suggestions, "do not fall back to an older recoverable section")
}

func TestInsightsReviewSnapshotRejectsRerun(t *testing.T) {
	asOf := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	since := asOf.Add(-24 * time.Hour)
	finished := asOf.Add(-time.Hour)
	verdict := "F"
	version := uuid.MustParse("00000000-0000-4000-8000-000000000010")
	review := &storage.Review{JobID: 1, UUID: &version, Output: "Current finding", Job: &storage.ReviewJob{ID: 1, JobType: storage.JobTypeReview, Status: storage.JobStatusDone, FinishedAt: &finished, Verdict: &verdict}}
	entry, ok := currentInsightsReview(review, since, asOf)
	require.True(t, ok)
	assert.Equal(t, version.String(), entry.ReviewUUID)
	future := asOf.Add(time.Hour)
	review.Job.FinishedAt = &future
	_, ok = currentInsightsReview(review, since, asOf)
	assert.False(t, ok, "joined completion must be checked after loading review text")
	review.Job.FinishedAt = &finished
	review.Job.Status = storage.JobStatusRunning
	_, ok = currentInsightsReview(review, since, asOf)
	assert.False(t, ok)
	review.Job.Status = storage.JobStatusDone
	verdict = "P"
	_, ok = currentInsightsReview(review, since, asOf)
	assert.False(t, ok)
}

func TestHandleEnqueueInsightsPriorWithQuotedNoopWithoutCurrentFailures(t *testing.T) {
	server, db, tmpDir := newTestServer(t)
	repoDir := filepath.Join(tmpDir, "repo")
	testutil.InitTestGitRepo(t, repoDir)
	root, err := gitpkg.GetMainRepoRoot(repoDir)
	require.NoError(t, err)
	repo, err := db.GetOrCreateRepo(root)
	require.NoError(t, err)
	prior, err := db.EnqueueJob(storage.EnqueueOpts{RepoID: repo.ID, GitRef: "insights", Branch: "main", Agent: "test", JobType: storage.JobTypeInsights, Prompt: "old prompt"})
	require.NoError(t, err)
	claimed, err := db.ClaimJob("worker")
	require.NoError(t, err)
	require.Equal(t, prior.ID, claimed.ID)
	require.NoError(t, testutil.CompleteReviewFixture(db, prior.ID, "test", "old prompt", "## 5. Suggested Guideline Additions\n\nCheck returned errors.\n\nSupported by Jobs #101, #102 and #103. Unlike the earlier \"No guideline change recommended\" result, these are three distinct examples."))
	_, err = db.Exec(`UPDATE review_jobs SET finished_at=? WHERE id=?`, time.Now().Add(-2*time.Hour).Format(time.RFC3339Nano), prior.ID)
	require.NoError(t, err)
	w := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(w, testutil.MakeJSONRequest(t, http.MethodPost, "/api/enqueue", EnqueueRequest{
		RepoPath: repoDir, GitRef: "insights", Branch: "main", Agent: "test", JobType: storage.JobTypeInsights, Since: time.Now().Add(-time.Hour).Format(time.RFC3339),
	}))
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	var job storage.ReviewJob
	testutil.DecodeJSON(t, w, &job)
	stored, err := db.GetJobByID(job.ID)
	require.NoError(t, err)
	assert := assert.New(t)
	assert.Contains(stored.Prompt, "Fetched failing reviews: 0; included excerpts: 0")
	assert.Contains(stored.Prompt, `"logical_reviews": 0`)
	assert.Contains(stored.Prompt, "Check returned errors.")
	assert.Contains(stored.Prompt, `Unlike the earlier "No guideline change recommended" result`)
	assert.Contains(stored.Prompt, "insufficient comparable evidence")
}

func TestBuildInsightsPromptSkipsPriorNoopWithoutFailures(t *testing.T) {
	server, db, tmpDir := newTestServer(t)
	repo, err := db.GetOrCreateRepo(filepath.Join(tmpDir, "repo"))
	require.NoError(t, err)
	job, err := db.EnqueueJob(storage.EnqueueOpts{RepoID: repo.ID, GitRef: "insights", Agent: "test", JobType: storage.JobTypeInsights, Prompt: "old prompt"})
	require.NoError(t, err)
	claimed, err := db.ClaimJob("worker")
	require.NoError(t, err)
	require.Equal(t, job.ID, claimed.ID)
	require.NoError(t, testutil.CompleteReviewFixture(db, job.ID, "test", "old prompt", "## 5. Suggested Guideline Additions\n\nNo guideline change recommended: too few examples."))
	text, count, err := server.buildInsightsPrompt(t.Context(), repo.RootPath, "", time.Now().Add(-time.Hour))
	require.NoError(t, err)
	assert.Empty(t, text)
	assert.Zero(t, count)
}
