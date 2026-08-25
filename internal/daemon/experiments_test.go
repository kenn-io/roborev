package daemon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
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
	assert.Equal(t, "feature/experiment", job.Branch)
	assert.Equal(t, "high", job.MinSeverity)
	require.Len(t, job.Experiments, 1)
	assert.Equal(t, "session-v1", job.Experiments[0].ID)
	assert.Equal(t, "experiment", job.Experiments[0].Arm)

	stored, err := db.GetJobByID(job.ID)
	require.NoError(t, err)
	assert.Equal(t, job.Branch, stored.Branch)
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
		assert.Equal(t, synthesis.Branch, member.Branch)
		assert.Equal(t, synthesis.Experiments, member.Experiments)
	}

	var assignmentCount int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM experiment_assignments WHERE review_unit_uuid = ?`,
		response.PanelRunUUID,
	).Scan(&assignmentCount))
	assert.Equal(t, 1, assignmentCount)
}

func TestFrozenExperimentPlanSelectsPanelMember(t *testing.T) {
	members := []storage.EnqueueOpts{
		{
			Agent: "first", JobType: storage.JobTypeReview,
			PanelName: "review", PanelMemberName: "first", PanelMemberIndex: 0,
			MinSeverity: "high", BackupAgent: "test",
		},
		{
			Agent: "second", JobType: storage.JobTypeReview,
			PanelName: "review", PanelMemberName: "second", PanelMemberIndex: 1,
			MinSeverity: "", BackupAgent: "", BackupModel: "",
		},
	}
	synthesis := storage.EnqueueOpts{
		Agent: "test", JobType: storage.JobTypeSynthesis,
		PanelName: "review", PanelRole: storage.PanelRoleSynthesis,
	}
	assignment, err := storageAssignmentForExperiment(&config.ExperimentAssignment{
		ID: "panel-clear-v1", DefinitionHash: "definition-hash",
		DefinitionJSON: `{"ratio":1}`, Arm: config.ExperimentArmExperimental,
		SubjectHash: "subject-hash",
	}, experimentPlanForPanel(members, synthesis))
	require.NoError(t, err)

	job := &storage.ReviewJob{
		Agent:        "current-attempt-agent",
		PanelRunUUID: "run", PanelRole: storage.PanelRoleMember,
		PanelName: "review", PanelMemberName: "second", PanelMemberIndex: 1,
		MinSeverity: "critical", BackupAgent: "claude-code", BackupModel: "stale",
	}
	require.NoError(t, applyFrozenExperimentSettings(job, assignment))
	assert.Equal(t, "current-attempt-agent", job.Agent)
	assert.Empty(t, job.MinSeverity)
	assert.Empty(t, job.BackupAgent)
	assert.Empty(t, job.BackupModel)
}

func TestExperimentPanelRerunPreservesNormalTargetType(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(*testutil.TestRepo) string
		want    string
	}{
		{
			name: "single commit",
			prepare: func(repo *testutil.TestRepo) string {
				repo.CommitFile("review.go", "package review\n", "add review")
				return "HEAD"
			},
			want: storage.JobTypeReview,
		},
		{
			name: "range",
			prepare: func(repo *testutil.TestRepo) string {
				base := repo.CommitFile("review.go", "package review\n", "add review")
				head := repo.CommitFile(
					"review.go", "package review\n\nfunc changed() {}\n", "change review",
				)
				return base + ".." + head
			},
			want: storage.JobTypeRange,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server, db, _ := newTestServer(t)
			enabled := true
			ratio := 1.0
			server.configWatcher.Config().Experiments = map[string]config.ExperimentDefinition{
				"panel-rerun-v1": {
					Enabled: &enabled, Ratio: &ratio,
					Workflows: []config.ExperimentWorkflow{config.ExperimentWorkflowReview},
					Config:    map[string]any{"reuse_review_session": true},
				},
			}

			repo := testutil.NewGitRepo(t)
			repo.WriteFile(".roborev.toml", panelTOML)
			gitRef := tt.prepare(repo)
			response := enqueuePanelViaHTTP(t, server, EnqueueRequest{
				RepoPath: repo.Path(), GitRef: gitRef,
				Branch: "feature/panel-rerun", Agent: "test",
			})

			_, members := rerunAndLoadNewRun(
				t, server, db, response.PanelRunUUID, response.ID,
			)
			require.NotEmpty(t, members)
			for _, member := range members {
				assert.Equal(t, tt.want, member.JobType)
			}
		})
	}
}

func TestExperimentPersistsLocalReviewBackupPlans(t *testing.T) {
	const backupName = "claude-code"

	server, db, _ := newTestServer(t)
	enabled := true
	ratio := 1.0
	server.configWatcher.Config().Experiments = map[string]config.ExperimentDefinition{
		"backup-v1": {
			Enabled: &enabled, Ratio: &ratio,
			Workflows: []config.ExperimentWorkflow{config.ExperimentWorkflowReview},
			Config: map[string]any{
				"backup_agent": backupName,
				"backup_model": "backup-model",
			},
		},
	}

	standaloneRepo := testutil.NewGitRepo(t)
	standaloneRepo.CommitFile("review.go", "package review\n", "add review")
	job := enqueueViaHTTP(t, server, EnqueueRequest{
		RepoPath: standaloneRepo.Path(), GitRef: "HEAD",
		Branch: "feature/standalone-backup", Agent: "test", Panel: config.PanelNone,
	})
	assert.Equal(t, backupName, job.BackupAgent)
	assert.Equal(t, "backup-model", job.BackupModel)

	panelRepo := testutil.NewGitRepo(t)
	panelRepo.WriteFile(".roborev.toml", panelTOML)
	panelRepo.CommitFile("review.go", "package review\n", "add panel review")
	panel := enqueuePanelViaHTTP(t, server, EnqueueRequest{
		RepoPath: panelRepo.Path(), GitRef: "HEAD",
		Branch: "feature/panel-backup", Agent: "test",
	})
	members, err := db.GetPanelMembers(panel.PanelRunUUID)
	require.NoError(t, err)
	require.NotEmpty(t, members)
	for _, member := range members {
		assert.Equal(t, backupName, member.BackupAgent)
		assert.Equal(t, "backup-model", member.BackupModel)
	}
}

func TestNonExperimentLocalReviewsKeepRuntimeBackupResolution(t *testing.T) {
	server, db, _ := newTestServer(t)
	server.configWatcher.Config().ReviewBackupAgent = "claude-code"
	server.configWatcher.Config().ReviewBackupModel = "runtime-backup-model"

	standaloneRepo := testutil.NewGitRepo(t)
	standaloneRepo.CommitFile("review.go", "package review\n", "add review")
	job := enqueueViaHTTP(t, server, EnqueueRequest{
		RepoPath: standaloneRepo.Path(), GitRef: "HEAD",
		Branch: "feature/standalone-backup", Agent: "test", Panel: config.PanelNone,
	})
	assert.Empty(t, job.BackupAgent)
	assert.Empty(t, job.BackupModel)

	panelRepo := testutil.NewGitRepo(t)
	panelRepo.WriteFile(".roborev.toml", panelTOML)
	panelRepo.CommitFile("review.go", "package review\n", "add panel review")
	panel := enqueuePanelViaHTTP(t, server, EnqueueRequest{
		RepoPath: panelRepo.Path(), GitRef: "HEAD",
		Branch: "feature/panel-backup", Agent: "test",
	})
	members, err := db.GetPanelMembers(panel.PanelRunUUID)
	require.NoError(t, err)
	require.NotEmpty(t, members)
	for _, member := range members {
		assert.Empty(t, member.BackupAgent)
		assert.Empty(t, member.BackupModel)
	}
}

func TestExperimentStandaloneRerunPreservesFrozenPlan(t *testing.T) {
	server, db, _ := newTestServer(t)
	enabled := true
	ratio := 1.0
	cfg := server.configWatcher.Config()
	cfg.Experiments = map[string]config.ExperimentDefinition{
		"frozen-plan-v1": {
			Enabled: &enabled, Ratio: &ratio,
			Workflows: []config.ExperimentWorkflow{config.ExperimentWorkflowReview},
			Config: map[string]any{
				"review_model":        "frozen-model",
				"review_reasoning":    "high",
				"review_min_severity": "high",
				"backup_agent":        "claude-code",
				"backup_model":        "frozen-backup-model",
			},
		},
	}

	repo := testutil.NewGitRepo(t)
	repo.CommitFile("review.go", "package review\n", "add review")
	job := enqueueViaHTTP(t, server, EnqueueRequest{
		RepoPath: repo.Path(), GitRef: "HEAD", Branch: "feature/frozen-rerun",
		Agent: "test", Provider: "openai", Panel: config.PanelNone,
	})
	require.Len(t, job.Experiments, 1)
	frozenExperiments := job.Experiments

	claimed, err := db.ClaimJob("experiment-rerun-worker")
	require.NoError(t, err)
	require.NotNil(t, claimed)
	require.Equal(t, job.ID, claimed.ID)
	failedOver, err := db.FailoverJob(
		job.ID, "experiment-rerun-worker", job.BackupAgent, job.BackupModel,
	)
	require.NoError(t, err)
	assert.True(t, failedOver)
	claimed, err = db.ClaimJob("experiment-backup-worker")
	require.NoError(t, err)
	require.NotNil(t, claimed)
	require.Equal(t, job.ID, claimed.ID)
	assert.Equal(t, "claude-code", claimed.Agent)
	require.NoError(t, db.CompleteJob(job.ID, claimed.Agent, "prompt", "No issues found."))

	cfg.ReviewModel = "changed-model"
	cfg.ReviewReasoning = "low"
	cfg.ReviewMinSeverity = "critical"
	cfg.ReviewBackupAgent = ""
	cfg.ReviewBackupModel = ""

	req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/job/rerun", RerunJobRequest{JobID: job.ID})
	recorder := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(recorder, req)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())

	rerun, err := db.GetJobByID(job.ID)
	require.NoError(t, err)
	assert.Equal(t, storage.JobStatusQueued, rerun.Status)
	assert.Equal(t, "test", rerun.Agent)
	assert.Equal(t, "frozen-model", rerun.Model)
	assert.Equal(t, "openai", rerun.Provider)
	assert.Equal(t, "high", rerun.Reasoning)
	assert.Equal(t, "high", rerun.MinSeverity)
	assert.Equal(t, "claude-code", rerun.BackupAgent)
	assert.Equal(t, "frozen-backup-model", rerun.BackupModel)
	assert.Equal(t, frozenExperiments, rerun.Experiments)
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
	require.Equal(t, "bug", claimed.PanelMemberName)
	require.NoError(t, db.CompleteJob(
		claimed.ID, "test", "prompt", "No issues found.",
	))
	const sessionID = "session-panel-1"
	_, err = db.Exec(`UPDATE review_jobs SET session_id = ? WHERE id = ?`, sessionID, claimed.ID)
	require.NoError(t, err)

	repo.WriteFile(".roborev.toml", strings.Replace(
		panelTOML,
		"[review.subagents.design]\nagent = \"test\"\nreview_type = \"design\"",
		"[review.subagents.design]\nagent = \"test\"\nmodel = \"design-v2\"\nreview_type = \"design\"",
		1,
	))
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

func TestCIPollerUsesStoredRepoAndSourceBranchExperimentIdentity(t *testing.T) {
	poller, db, storedRepo, repo, cfg := newCIPanelGitHarness(t)
	enabled := true
	ratio := 1.0
	cfg.Experiments = map[string]config.ExperimentDefinition{
		"ci-session-v1": {
			Enabled: &enabled, Ratio: &ratio,
			Workflows: []config.ExperimentWorkflow{
				config.ExperimentWorkflowReview,
				config.ExperimentWorkflowCI,
			},
			Config: map[string]any{"reuse_review_session": true},
		},
	}

	base := repo.HeadSHA()
	head := repo.CommitFile("review.go", "package review\n", "add CI review")
	poller.mergeBaseFn = func(_, _, _ string) (string, error) { return base, nil }
	err := poller.processPR(context.Background(), "acme/api", ghPR{
		Number:      41,
		HeadRefOid:  head,
		HeadRefName: "feature/ci-experiment",
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
	assert.Equal(t, "feature/ci-experiment", synthesis.Branch)
	expected, err := config.SelectReviewExperiment(config.ExperimentSelectionInput{
		Workflow: config.ExperimentWorkflowReview,
		Subject: config.ExperimentSubject{
			Repository: storedRepo.Identity,
			Branch:     "feature/ci-experiment",
		},
		Global: cfg,
	})
	require.NoError(t, err)
	require.NotNil(t, expected.Assignment)
	assert.Equal(t, expected.Assignment.SubjectHash, synthesis.Experiments[0].SubjectHash)
	for _, member := range members {
		assert.Equal(t, synthesis.Experiments, member.Experiments)
		assert.Equal(t, "feature/ci-experiment", member.Branch)
	}
}

func TestCIPollerExperimentFreezesSynthesisSeverity(t *testing.T) {
	poller, db, _, repo, cfg := newCIPanelGitHarness(t)
	enabled := true
	ratio := 1.0
	cfg.CI.MinSeverity = "high"
	cfg.Experiments = map[string]config.ExperimentDefinition{
		"ci-severity-v1": {
			Enabled: &enabled, Ratio: &ratio,
			Workflows: []config.ExperimentWorkflow{config.ExperimentWorkflowCI},
			Config: map[string]any{"ci": map[string]any{
				"min_severity": "critical",
			}},
		},
	}

	base := repo.HeadSHA()
	head := repo.CommitFile("review.go", "package review\n", "add CI review")
	poller.mergeBaseFn = func(_, _, _ string) (string, error) { return base, nil }
	err := poller.processPR(context.Background(), "acme/api", ghPR{
		Number:      42,
		HeadRefOid:  head,
		HeadRefName: "feature/ci-severity",
		BaseRefName: "main",
	}, cfg)
	require.NoError(t, err)

	panel, err := db.GetCIPanelByPRSHA("acme/api", 42, head)
	require.NoError(t, err)
	require.NotNil(t, panel)
	synthesis, err := db.GetJobByID(*panel.SynthesisJobID)
	require.NoError(t, err)
	assert.Equal(t, "critical", synthesis.MinSeverity)
}

func TestExperimentSessionReuseStaysOnSourceMachine(t *testing.T) {
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
	repo.CommitFile("review.go", "package review\n", "first review")
	first := enqueuePanelViaHTTP(t, server, EnqueueRequest{
		RepoPath: repo.Path(), GitRef: "HEAD",
		Branch: "feature/machine-session", Agent: "test",
	})
	claimed, err := db.ClaimJob("experiment-worker")
	require.NoError(t, err)
	require.Equal(t, first.PanelRunUUID, claimed.PanelRunUUID)
	require.NoError(t, db.CompleteJob(claimed.ID, "test", "prompt", "No issues found."))
	_, err = db.Exec(`UPDATE review_jobs SET session_id = ?, source_machine_id = ? WHERE id = ?`,
		"foreign-session", "foreign-machine", claimed.ID)
	require.NoError(t, err)

	repo.CommitFile("review.go", "package review\n\nfunc changed() {}\n", "second review")
	second := enqueuePanelViaHTTP(t, server, EnqueueRequest{
		RepoPath: repo.Path(), GitRef: "HEAD",
		Branch: "feature/machine-session", Agent: "test",
	})
	members, err := db.GetPanelMembers(second.PanelRunUUID)
	require.NoError(t, err)
	for _, member := range members {
		assert.Empty(t, member.SessionID)
		assert.Empty(t, member.ResumeSourceJobUUID)
	}
}
