package daemon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

func TestEnqueueSingleReviewPersistsExperiment(t *testing.T) {
	server, db, _ := newTestServer(t)
	enabled := true
	ratio := 1.0
	server.configWatcher.Config().Experiments = map[string]config.ExperimentDefinition{
		"session-v1": {
			Enabled: &enabled, Ratio: &ratio,
			Workflows: []config.ExperimentWorkflow{config.ExperimentWorkflowReview},
			Config: map[string]any{
				"reuse_review_session": true,
				"review_min_severity":  "high",
			},
		},
	}

	repo := testutil.NewGitRepo(t)
	repo.CommitFile("review.go", "package review\n", "add review")
	req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/enqueue", EnqueueRequest{
		RepoPath: repo.Path(),
		GitRef:   "HEAD",
		Branch:   "feature/experiment",
		Agent:    "test",
	})
	recorder := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(recorder, req)
	require.Equal(t, http.StatusCreated, recorder.Code, recorder.Body.String())

	var job storage.ReviewJob
	testutil.DecodeJSON(t, recorder, &job)
	assert.NotEmpty(t, job.BranchSubjectHash)
	assert.Equal(t, "high", job.MinSeverity)
	require.Len(t, job.Experiments, 1)
	assert.Equal(t, "session-v1", job.Experiments[0].ID)
	assert.Equal(t, "experiment", job.Experiments[0].Arm)

	stored, err := db.GetJobByID(job.ID)
	require.NoError(t, err)
	assert.Equal(t, job.BranchSubjectHash, stored.BranchSubjectHash)
	assert.Equal(t, job.Experiments, stored.Experiments)

	repo.CommitFile("review.go", "package review\n\nfunc changed() {}\n", "change review")
	req = testutil.MakeJSONRequest(t, http.MethodPost, "/api/enqueue", EnqueueRequest{
		RepoPath:    repo.Path(),
		GitRef:      "HEAD",
		Branch:      "feature/experiment",
		Agent:       "test",
		MinSeverity: "low",
	})
	recorder = httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(recorder, req)
	require.Equal(t, http.StatusCreated, recorder.Code, recorder.Body.String())
	testutil.DecodeJSON(t, recorder, &job)
	assert.Equal(t, "low", job.MinSeverity)
}

func TestEnqueuePanelPersistsOneExperimentForWholeRun(t *testing.T) {
	server, db, _ := newTestServer(t)
	enabled := true
	ratio := 1.0
	server.configWatcher.Config().Experiments = map[string]config.ExperimentDefinition{
		"panel-v1": {
			Enabled: &enabled, Ratio: &ratio,
			Workflows: []config.ExperimentWorkflow{config.ExperimentWorkflowReview},
			Config:    map[string]any{"reuse_review_session": true},
		},
	}

	repo := testutil.NewGitRepo(t)
	repo.WriteFile(".roborev.toml", panelTOML)
	repo.CommitFile("review.go", "package review\n", "add panel review")
	response := enqueuePanelViaHTTP(t, server, EnqueueRequest{
		RepoPath: repo.Path(),
		GitRef:   "HEAD",
		Branch:   "feature/panel-experiment",
		Agent:    "test",
	})

	members, err := db.GetPanelMembers(response.PanelRunUUID)
	require.NoError(t, err)
	require.NotEmpty(t, members)
	synthesis, err := db.GetJobByID(response.ID)
	require.NoError(t, err)
	require.Len(t, synthesis.Experiments, 1)
	for _, member := range members {
		assert.Equal(t, synthesis.BranchSubjectHash, member.BranchSubjectHash)
		assert.Equal(t, synthesis.Experiments, member.Experiments)
	}

	var assignmentCount int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM experiment_assignments WHERE review_unit_uuid = ?`,
		response.PanelRunUUID,
	).Scan(&assignmentCount))
	assert.Equal(t, 1, assignmentCount)
}

func TestPanelExperimentResumesCompatibleMemberSession(t *testing.T) {
	server, db, _ := newTestServer(t)
	enabled := true
	ratio := 1.0
	server.configWatcher.Config().Experiments = map[string]config.ExperimentDefinition{
		"session-v1": {
			Enabled: &enabled, Ratio: &ratio,
			Workflows: []config.ExperimentWorkflow{config.ExperimentWorkflowReview},
			Config:    map[string]any{"reuse_review_session": true},
		},
	}

	repo := testutil.NewGitRepo(t)
	repo.WriteFile(".roborev.toml", panelTOML)
	firstSHA := repo.CommitFile("review.go", "package review\n", "first review")
	first := enqueuePanelViaHTTP(t, server, EnqueueRequest{
		RepoPath: repo.Path(), GitRef: "HEAD",
		Branch: "feature/session-experiment", Agent: "test",
	})
	claimed, err := db.ClaimJob("experiment-worker")
	require.NoError(t, err)
	require.Equal(t, first.PanelRunUUID, claimed.PanelRunUUID)
	require.NoError(t, db.CompleteJob(
		claimed.ID, "test", "prompt", "No issues found.",
	))
	const sessionID = "session-panel-1"
	_, err = db.Exec(`UPDATE review_jobs SET session_id = ? WHERE id = ?`, sessionID, claimed.ID)
	require.NoError(t, err)

	secondSHA := repo.CommitFile("review.go", "package review\n\nfunc changed() {}\n", "second review")
	require.NotEqual(t, firstSHA, secondSHA)
	second := enqueuePanelViaHTTP(t, server, EnqueueRequest{
		RepoPath: repo.Path(), GitRef: "HEAD",
		Branch: "feature/session-experiment", Agent: "test",
	})
	members, err := db.GetPanelMembers(second.PanelRunUUID)
	require.NoError(t, err)
	var resumed *storage.ReviewJob
	for i := range members {
		if members[i].PanelMemberName == claimed.PanelMemberName {
			resumed = &members[i]
			break
		}
	}
	require.NotNil(t, resumed)
	assert.Equal(t, sessionID, resumed.SessionID)
	assert.Equal(t, claimed.UUID, resumed.ResumeSourceJobUUID)

	synthesis, err := db.GetJobByID(second.ID)
	require.NoError(t, err)
	assert.Empty(t, synthesis.SessionID)
	assert.Empty(t, synthesis.ResumeSourceJobUUID)
}

func TestCIPollerUsesSourceBranchExperimentIdentity(t *testing.T) {
	poller, db, _, repo, cfg := newCIPanelGitHarness(t)
	enabled := true
	ratio := 1.0
	cfg.Experiments = map[string]config.ExperimentDefinition{
		"ci-session-v1": {
			Enabled: &enabled, Ratio: &ratio,
			Workflows: []config.ExperimentWorkflow{config.ExperimentWorkflowCI},
			Config:    map[string]any{"reuse_review_session": true},
		},
	}

	base := repo.HeadSHA()
	head := repo.CommitFile("review.go", "package review\n", "add CI review")
	poller.mergeBaseFn = func(_, _, _ string) (string, error) { return base, nil }
	err := poller.processPR(context.Background(), "acme/api", ghPR{
		Number:      41,
		HeadRefOid:  head,
		HeadRefName: "feature/ci-experiment",
		HeadRepo:    "contributor/api",
		BaseRefName: "main",
	}, cfg)
	require.NoError(t, err)

	panel, err := db.GetCIPanelByPRSHA("acme/api", 41, head)
	require.NoError(t, err)
	members, err := db.GetPanelMembers(panel.PanelRunUUID)
	require.NoError(t, err)
	require.NotEmpty(t, members)
	synthesis, err := db.GetJobByID(*panel.SynthesisJobID)
	require.NoError(t, err)
	require.Len(t, synthesis.Experiments, 1)
	assert.Equal(t, "ci-session-v1", synthesis.Experiments[0].ID)
	assert.NotEmpty(t, synthesis.BranchSubjectHash)
	assert.Empty(t, synthesis.Branch)
	for _, member := range members {
		assert.Equal(t, synthesis.BranchSubjectHash, member.BranchSubjectHash)
		assert.Equal(t, synthesis.Experiments, member.Experiments)
		assert.Empty(t, member.Branch)
	}
}
