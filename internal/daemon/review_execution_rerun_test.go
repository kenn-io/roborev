package daemon

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

func TestReviewExecutionExperimentRerunKeepsFrozenACP(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(strconv.FormatBool(changed), func(t *testing.T) {
			server, db, _ := newTestServer(t)
			command, err := os.Executable()
			require.NoError(t, err)
			enabled, ratio := true, 1.0
			cfg := server.configWatcher.Config()
			cfg.Experiments = map[string]config.ExperimentDefinition{"frozen-acp": {
				Enabled: &enabled, Ratio: &ratio,
				Workflows: []config.ExperimentWorkflow{config.ExperimentWorkflowReview},
				Config:    map[string]any{"review_reasoning": "high"},
			}}
			repo := testutil.NewGitRepo(t)
			repo.CommitFile("draft.md", "Draft", "add draft")
			require.NoError(t, os.WriteFile(filepath.Join(repo.Path(), ".roborev.toml"),
				[]byte("[acp.reader]\ncommand = "+strconv.Quote(command)+"\nmodel = 'frozen-model'\nargs = ['frozen-argument']\n"), 0o600))
			job := enqueueViaHTTP(t, server, EnqueueRequest{RepoPath: repo.Path(), GitRef: "HEAD", Agent: "acp.reader", Panel: config.PanelNone})
			frozen, err := db.GetJobByID(job.ID)
			require.NoError(t, err)
			require.Len(t, frozen.Experiments, 1)
			claimed, err := db.ClaimJob("frozen-acp-worker")
			require.NoError(t, err)
			require.NotNil(t, claimed)
			require.Equal(t, job.ID, claimed.ID)
			require.NoError(t, testutil.CompleteReviewFixture(db, job.ID, job.Agent, "prompt", "No issues found."))
			live := ""
			if changed {
				live = "[acp.reader]\ncommand = " + strconv.Quote(filepath.Join(t.TempDir(), "unavailable-reader")) + "\nmodel = 'changed-model'\n"
			}
			require.NoError(t, os.WriteFile(filepath.Join(repo.Path(), ".roborev.toml"), []byte(live), 0o600))
			request := testutil.MakeJSONRequest(t, http.MethodPost, "/api/job/rerun", RerunJobRequest{JobID: job.ID})
			response := httptest.NewRecorder()
			server.httpServer.Handler.ServeHTTP(response, request)
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			rerun, err := db.GetJobByID(job.ID)
			require.NoError(t, err)
			assert := assert.New(t)
			assert.Equal(storage.JobStatusQueued, rerun.Status)
			assert.Equal(frozen.Agent, rerun.Agent)
			assert.Equal(frozen.Model, rerun.Model)
			assert.Equal(frozen.PanelMemberConfigJSON, rerun.PanelMemberConfigJSON)
			assert.Equal(frozen.Experiments, rerun.Experiments)
			selected, err := resolveReviewJobAgent(rerun, cfg)
			require.NoError(t, err)
			acp, ok := selected.(*agent.ACPAgent)
			require.True(t, ok)
			assert.Equal(command, acp.CommandName())
		})
	}
}

func TestReviewExecutionLegacyExperimentRerunUsesRepoACP(t *testing.T) {
	server, db, _ := newTestServer(t)
	cfg := server.configWatcher.Config()
	require.NotContains(t, cfg.ACP, "reader")
	command, err := os.Executable()
	require.NoError(t, err)
	repo := testutil.NewGitRepo(t)
	sha := repo.CommitFile("draft.md", "Draft", "add draft")
	repo.WriteFile(".roborev.toml", "[acp.reader]\ncommand = "+strconv.Quote(command)+"\nmodel = 'repository-model'\n")
	registered, err := db.GetOrCreateRepo(repo.Path())
	require.NoError(t, err)
	opts := storage.EnqueueOpts{
		RepoID: registered.ID, GitRef: sha, Agent: "acp.reader",
		Model: "legacy-model", Reasoning: "high", ReviewType: "default",
		JobType: storage.JobTypeReview,
	}
	// Before execution snapshots were added, standalone experiment plans
	// fingerprinted this same plan with empty member JSON.
	plan := experimentPlanForJob(opts)
	require.Empty(t, plan.PanelMemberConfigJSON)
	planJSON, planHash, err := config.EncodeExperimentConfig(plan)
	require.NoError(t, err)
	definitionJSON, definitionHash, err := config.EncodeExperimentConfig(map[string]any{
		"ratio": 1, "workflows": []string{"review"},
		"config": map[string]any{"review_reasoning": "high"},
	})
	require.NoError(t, err)
	opts.Experiment = &storage.ExperimentAssignmentInput{
		ExperimentID: "legacy-repo-acp", Arm: "experiment", SubjectHash: "legacy-subject",
		DefinitionJSON: definitionJSON, DefinitionHash: definitionHash,
		EffectiveConfigJSON: planJSON, EffectiveConfigHash: planHash,
	}
	job, err := db.EnqueueJob(opts)
	require.NoError(t, err)
	require.Len(t, job.Experiments, 1)
	require.Equal(t, planHash, job.Experiments[0].EffectiveConfigHash)
	claimed, err := db.ClaimJob("legacy-acp-worker")
	require.NoError(t, err)
	require.NotNil(t, claimed)
	require.Equal(t, job.ID, claimed.ID)
	require.NoError(t, testutil.CompleteReviewFixture(db, job.ID, job.Agent, "prompt", "No issues found."))
	terminal, err := db.GetJobByID(job.ID)
	require.NoError(t, err)
	require.Equal(t, storage.JobStatusDone, terminal.Status)

	request := testutil.MakeJSONRequest(t, http.MethodPost, "/api/job/rerun", RerunJobRequest{JobID: job.ID})
	response := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(response, request)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	rerun, err := db.GetJobByID(job.ID)
	require.NoError(t, err)
	assert := assert.New(t)
	assert.Equal(storage.JobStatusQueued, rerun.Status)
	assert.Equal(job.Agent, rerun.Agent)
	assert.Equal(job.Model, rerun.Model)
	assert.Empty(rerun.PanelMemberConfigJSON)
	assert.Equal(job.Experiments, rerun.Experiments)
	selected, err := resolveReviewJobAgent(rerun, cfg)
	require.NoError(t, err)
	acp, ok := selected.(*agent.ACPAgent)
	require.True(t, ok)
	assert.Equal(command, acp.CommandName())
}
