package daemon

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

func TestReviewExecutionSnapshot(t *testing.T) {
	for _, raw := range []string{"", `{}`, `{"name":"legacy"}`} {
		snapshot, err := decodeReviewExecutionSnapshot(raw)
		require.NoError(t, err)
		assert.Nil(t, snapshot.Tools)
	}
	raw := `{"tools":{"mode":"read-only","mcp":{}},"binary_evidence":true,"evidence_manifest":"evidence.toml","instructions":"Check sources","timeout":"90s","acp":{"reader":{"Command":"reader-cli","Args":["acp"]}}}`
	snapshot, err := decodeReviewExecutionSnapshot(raw)
	require.NoError(t, err)
	assert.Equal(t, &config.ReviewToolPolicy{Mode: "read-only", MCP: map[string][]string{}}, snapshot.Tools)
	assert.Equal(t, 90*time.Second, snapshot.timeout)
	assert.Equal(t, "reader-cli", snapshot.ACP["reader"].Command)
	assert.Equal(t, "Check sources", snapshot.Instructions)
	assert.True(t, snapshot.BinaryEvidence)
	assert.Equal(t, "evidence.toml", snapshot.EvidenceManifest)
	assert.Equal(t, []string{"acp"}, snapshot.ACP["reader"].Args)
}

func TestReviewExecutionRejectsMalformed(t *testing.T) {
	for _, raw := range []string{
		`null`, `[]`, `{`, `{"tools":null}`, `{"tools":{}}`,
		`{"tools":{"mode":"ordinary"}}`, `{"tools":{"mode":"read-only","mcp":null}}`,
		`{"tools":{"mode":"read-only","mcp":{"source":["*"]}}}`,
		`{"instructions":null}`, `{"timeout":null}`, `{"timeout":"-1s"}`, `{"timeout":"0s"}`,
		`{"binary_evidence":"false"}`, `{"binary_evidence":null}`, `{"evidence_manifest":false}`,
		`{"acp":{"reader":{"args":["acp"]}}}`,
	} {
		t.Run(raw, func(t *testing.T) {
			_, err := decodeReviewExecutionSnapshot(raw)
			require.Error(t, err)
		})
	}
}

func TestReviewExecutionWorkerRejectsBeforeInvocation(t *testing.T) {
	for _, raw := range []string{`{"tools":null}`, `{"timeout":"later"}`, `{"binary_evidence":"false"}`, `{"tools":{"mode":"read-only","mcp":{}}}`} {
		t.Run(raw, func(t *testing.T) {
			tc := newWorkerTestContext(t, 1)
			var captured string
			registerCapturingAgent(t, "policy-unsupported", &captured)
			job, err := tc.DB.EnqueueJob(storage.EnqueueOpts{
				RepoID: tc.Repo.ID, GitRef: testutil.GetHeadSHA(t, tc.TmpDir), Agent: "policy-unsupported", PanelMemberConfigJSON: raw,
			})
			require.NoError(t, err)
			claimed, err := tc.DB.ClaimJob(testWorkerID)
			require.NoError(t, err)
			require.NotNil(t, claimed)
			tc.Pool.processJob(testWorkerID, claimed)
			stored, err := tc.DB.GetJobByID(job.ID)
			require.NoError(t, err)
			assert.NotEqual(t, storage.JobStatusDone, stored.Status)
			assert.Empty(t, captured)
			_, err = tc.DB.GetReviewByJobID(job.ID)
			require.Error(t, err)
		})
	}
}

func TestReviewExecutionTrustedCIMissingDefaultRef(t *testing.T) {
	_, err := loadCIRepoConfig(t.TempDir())
	require.Error(t, err)
}

func TestReviewExecutionEnqueueRejectsUnsupported(t *testing.T) {
	server, db, _ := newTestServer(t)
	server.configWatcher.Config().Review = config.ReviewConfig{
		Subagents: map[string]config.SubagentSpec{"reader": {Agent: "test", ReviewType: "default", Tools: &config.ReviewToolPolicy{Mode: "read-only"}}},
		Panels:    map[string]config.PanelSpec{"panel": {Members: []string{"reader"}}},
	}
	repo := testutil.NewGitRepo(t)
	repo.CommitFile("draft.md", "Draft", "add draft")
	request := testutil.MakeJSONRequest(t, http.MethodPost, "/api/enqueue", EnqueueRequest{RepoPath: repo.Path(), GitRef: "HEAD", Panel: "panel", Agent: "test"})
	response := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(response, request)
	require.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
	var rejection ErrorResponse
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &rejection))
	assert.Equal(t, `panel member "reader": invalid review tool policy: agent "test" does not support strict review tools`, rejection.Error)
	var count int
	require.NoError(t, db.QueryRow("SELECT COUNT(*) FROM review_jobs").Scan(&count))
	assert.Zero(t, count)
}

func TestReviewExecutionFrozenSettings(t *testing.T) {
	for _, tools := range []*config.ReviewToolPolicy{nil, {Mode: "read-only", MCP: map[string][]string{}}} {
		for _, enabled := range []bool{false, true} {
			original := config.ReviewSettings{Tools: tools, BinaryEvidence: enabled}
			data, err := json.Marshal(reviewExecutionSnapshot{ReviewSettings: original})
			require.NoError(t, err)
			job := &storage.ReviewJob{JobType: storage.JobTypeReview, ReviewType: "quotes", PanelMemberConfigJSON: string(data)}
			snapshot, err := decodeReviewExecutionSnapshot(job.PanelMemberConfigJSON)
			require.NoError(t, err)
			live := config.DefaultConfig()
			live.Review.Types = map[string]config.ReviewTypeSpec{"quotes": {Template: "changed.md", BinaryEvidence: !enabled, Tools: &config.ReviewToolPolicy{Mode: "read-only", MCP: map[string][]string{"source": {"read"}}}}}
			settings, err := resolveReviewJobSettings(job, snapshot, t.TempDir(), live)
			require.NoError(t, err)
			assert.Equal(t, original, settings)
		}
	}
}

// Only package-local structured fakes promise strict capability. Production
// adapters stay unsupported until the strict Claude implementation lands.
func (a *structuredWorkerTestAgent) WithStrictReview(config.ReviewToolPolicy, string) agent.Agent {
	return a
}

func TestReviewExecutionSynthesisRetainsFailedStrictVoter(t *testing.T) {
	for _, raw := range []string{`{"tools":{"mode":"read-only","mcp":{}}}`, `{"tools":null}`} {
		t.Run(raw, func(t *testing.T) {
			tc := newWorkerTestContext(t, 1)
			var called string
			const name = "synthesis-policy-unsupported"
			registerCapturingAgent(t, name, &called)
			run, members, _ := enqueuePanelRun(t, tc, "policy", []memberSpec{{name: "strict", agent: name}, {name: "first", agent: name}, {name: "second", agent: name}})
			_, err := tc.DB.Exec("UPDATE review_jobs SET panel_member_config_json = ? WHERE id = ?", raw, members[0].ID)
			require.NoError(t, err)
			failMember(t, tc, members[0].ID)
			completeMember(t, tc, members[1].ID, name, "- High: Check a.go:1")
			completeMember(t, tc, members[2].ID, name, "- High: Check b.go:1")
			setSynthesisAgent(t, tc, run, name)
			synth := releaseAndClaimSynthesis(t, tc, run)
			tc.Pool.processSynthesisJob(context.Background(), testWorkerID, synth)
			assert.Empty(t, called)
			stored, err := tc.DB.GetJobByID(synth.ID)
			require.NoError(t, err)
			assert.NotEqual(t, storage.JobStatusDone, stored.Status)
		})
	}
}

func TestReviewExecutionRerunRejectsPolicy(t *testing.T) {
	tc := newWorkerTestContext(t, 1)
	for _, raw := range []string{`{"tools":null}`, `{"tools":{"mode":"read-only","mcp":{}}}`} {
		_, err := resolveRerunOpts(&storage.ReviewJob{RepoPath: tc.TmpDir, Agent: "test", JobType: storage.JobTypeReview, PanelMemberConfigJSON: raw}, config.DefaultConfig(), nil, "")
		require.Error(t, err)
	}
}

func TestReviewExecutionWorkerRejectsUnsupportedBackup(t *testing.T) {
	tc := newWorkerTestContext(t, 1)
	capable := &structuredWorkerTestAgent{name: "policy-primary"}
	agent.RegisterForTest(t, capable)
	job, err := tc.DB.EnqueueJob(storage.EnqueueOpts{RepoID: tc.Repo.ID, GitRef: testutil.GetHeadSHA(t, tc.TmpDir), Agent: capable.Name(), BackupAgent: "test", PanelMemberConfigJSON: `{"tools":{"mode":"read-only","mcp":{}}}`})
	require.NoError(t, err)
	claimed, err := tc.DB.ClaimJob(testWorkerID)
	require.NoError(t, err)
	tc.Pool.processJob(testWorkerID, claimed)
	assert.Empty(t, capable.prompt)
	assert.Zero(t, jobAgentInvoked(t, tc, job.ID))
}

func TestReviewExecutionSynthesisSnapshotIgnoresObservers(t *testing.T) {
	strict := &config.ReviewToolPolicy{Mode: "read-only", MCP: map[string][]string{"source": {"read"}}}
	members := []config.ResolvedMember{{Tools: strict, NonVoting: true}, {Name: "voter"}}
	snapshot := synthesisExecutionSnapshot(members, nil)
	assert.Nil(t, snapshot.Tools)
	members[0].NonVoting = false
	snapshot = synthesisExecutionSnapshot(members, nil)
	assert.Equal(t, &config.ReviewToolPolicy{Mode: "read-only", MCP: map[string][]string{}}, snapshot.Tools)
	assert.Equal(t, []string{"read"}, strict.MCP["source"])
}

func TestReviewExecutionCIRejectsStrictBeforeInvocation(t *testing.T) {
	for _, prebuilt := range []bool{false, true} {
		t.Run(fmt.Sprint(prebuilt), func(t *testing.T) {
			tc := newWorkerTestContext(t, 1)
			var captured string
			const name = "ci-policy-unsupported"
			registerCapturingAgent(t, name, &captured)
			sha := testutil.GetHeadSHA(t, tc.TmpDir)
			tc.GitRepo.Run("update-ref", "refs/remotes/origin/main", sha)
			tc.GitRepo.Run("symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
			prompt := ""
			if prebuilt {
				prompt = "Prebuilt trusted review."
			}
			job, err := tc.DB.EnqueueJob(storage.EnqueueOpts{RepoID: tc.Repo.ID, GitRef: sha, Agent: name, JobType: storage.JobTypeReview, Source: storage.JobSourceCI, Prompt: prompt, PromptPrebuilt: prebuilt, PanelMemberConfigJSON: `{"tools":{"mode":"read-only","mcp":{}}}`})
			require.NoError(t, err)
			tc.Pool.processJob(testWorkerID, claimNext(t, tc))
			stored, err := tc.DB.GetJobByID(job.ID)
			require.NoError(t, err)
			assert.NotEqual(t, storage.JobStatusDone, stored.Status)
			assert.Empty(t, captured)
			assert.Zero(t, jobAgentInvoked(t, tc, job.ID))
		})
	}
}

func TestReviewExecutionExperimentSnapshotRerun(t *testing.T) {
	for _, strict := range []bool{false, true} {
		t.Run(fmt.Sprint(strict), func(t *testing.T) {
			server, db, _ := newTestServer(t)
			const name = "experiment-policy-agent"
			agent.RegisterForTest(t, &structuredWorkerTestAgent{name: name})
			cfg := server.configWatcher.Config()
			cfg.Review.Types = map[string]config.ReviewTypeSpec{"quotes": {Template: "review.md"}}
			settings := map[string]any{"binary_evidence": strict, "evidence_manifest": "evidence.toml"}
			if strict {
				settings["tools"] = map[string]any{"mode": "read-only", "mcp": map[string]any{}}
			}
			enabled, ratio := true, 1.0
			cfg.Experiments = map[string]config.ExperimentDefinition{"frozen-settings": {Enabled: &enabled, Ratio: &ratio, Workflows: []config.ExperimentWorkflow{config.ExperimentWorkflowReview}, Config: map[string]any{"review": map[string]any{"types": map[string]any{"quotes": settings}}}}}
			repo := testutil.NewGitRepo(t)
			repo.CommitFile("review.md", "Review the material.", "add rubric")
			job := enqueueViaHTTP(t, server, EnqueueRequest{RepoPath: repo.Path(), GitRef: "HEAD", Agent: name, ReviewType: "quotes", Panel: config.PanelNone})
			stored, err := db.GetJobByID(job.ID)
			require.NoError(t, err)
			snapshot, err := decodeReviewExecutionSnapshot(stored.PanelMemberConfigJSON)
			require.NoError(t, err)
			assert.Equal(t, strict, snapshot.BinaryEvidence)
			assert.Equal(t, "evidence.toml", snapshot.EvidenceManifest)
			if strict {
				require.Equal(t, &config.ReviewToolPolicy{Mode: "read-only", MCP: map[string][]string{}}, snapshot.Tools)
			} else {
				require.Nil(t, snapshot.Tools)
			}
			claimed, err := db.ClaimJob("frozen-settings-worker")
			require.NoError(t, err)
			require.NotNil(t, claimed)
			require.Equal(t, job.ID, claimed.ID)
			require.NoError(t, testutil.CompleteReviewFixture(db, job.ID, name, "prompt", "No issues found."))
			cfg.Review.Types["quotes"] = config.ReviewTypeSpec{Template: "changed.md", BinaryEvidence: !strict, Tools: &config.ReviewToolPolicy{Mode: "read-only", MCP: map[string][]string{"changed": {"read"}}}}
			request := testutil.MakeJSONRequest(t, http.MethodPost, "/api/job/rerun", RerunJobRequest{JobID: job.ID})
			response := httptest.NewRecorder()
			server.httpServer.Handler.ServeHTTP(response, request)
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			rerun, err := db.GetJobByID(job.ID)
			require.NoError(t, err)
			assert.Equal(t, stored.PanelMemberConfigJSON, rerun.PanelMemberConfigJSON)
			assert.Equal(t, stored.Experiments, rerun.Experiments)
		})
	}
}

func TestReviewExecutionRerunPreservesProjectModel(t *testing.T) {
	for _, ci := range []bool{false, true} {
		for _, selected := range []string{"", "test"} {
			t.Run(fmt.Sprintf("ci=%v/selected=%s", ci, selected), func(t *testing.T) {
				repo := testutil.NewTestRepoWithCommit(t)
				repo.AddRemote("origin", "https://example.com/team/review-fixture.git")
				if ci {
					repo.Run("update-ref", "refs/remotes/origin/main", repo.HeadSHA())
					repo.Run("symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
					// Trusted absence must remain authoritative over malformed checkout data.
					require.NoError(t, os.WriteFile(filepath.Join(repo.Path(), ".roborev.toml"), []byte("invalid = [\n"), 0o600))
				}
				cfg := config.DefaultConfig()
				cfg.DefaultAgent = "test"
				cfg.ReviewModel = "global-review-model"
				cfg.Projects = map[string]config.ProjectConfig{"example.com/team/review-fixture": {ReviewModel: "project-review-model"}}
				job := &storage.ReviewJob{RepoPath: repo.Path(), JobType: storage.JobTypeReview, ReviewType: "default", Reasoning: "thorough", Agent: "test"}
				if ci {
					job.Source = storage.JobSourceCI
				}
				opts, err := resolveRerunOpts(job, cfg, nil, selected)
				require.NoError(t, err)
				assert.Equal(t, "project-review-model", opts.Model)
				assert.Equal(t, selected, opts.Agent)
			})
		}
	}
}

func TestReviewExecutionGrantListShape(t *testing.T) {
	for _, grants := range []string{`null`, `"read"`, `{}`, `true`, `[null]`, `[1]`} {
		t.Run(grants, func(t *testing.T) {
			_, err := decodeReviewExecutionSnapshot(`{"tools":{"mode":"read-only","mcp":{"source":` + grants + `}}}`)
			require.Error(t, err)
		})
	}
	for _, raw := range []string{`{}`, `{"tools":{"mode":"read-only"}}`, `{"tools":{"mode":"read-only","mcp":{}}}`, `{"tools":{"mode":"read-only","mcp":{"source":[]}}}`, `{"tools":{"mode":"read-only","mcp":{"source":["read"]}}}`} {
		_, err := decodeReviewExecutionSnapshot(raw)
		require.NoError(t, err, raw)
	}
}

func TestReviewExecutionRerunUsesLinkedWorktreePolicy(t *testing.T) {
	tc := newWorkerTestContext(t, 1)
	const capable = "linked-policy-reviewer"
	const unsupported = "linked-policy-unsupported"
	agent.RegisterForTest(t, &structuredWorkerTestAgent{name: capable})
	agent.RegisterForTest(t, &budgetStructuredReviewAgent{&budgetRecordingAgent{name: unsupported}})
	linked := filepath.Join(t.TempDir(), "linked")
	tc.GitRepo.RunGit("worktree", "add", "--detach", linked)
	for _, strict := range []bool{false, true} {
		t.Run(fmt.Sprintf("strict=%v", strict), func(t *testing.T) {
			text := "[review.types.quotes]\ntemplate = 'rubric.md'\nmodel = 'linked-model'\n"
			if strict {
				text += "[review.types.quotes.tools]\nmode = 'read-only'\n"
			}
			require.NoError(t, os.WriteFile(filepath.Join(linked, ".roborev.toml"), []byte(text), 0o600))
			for _, name := range []string{capable, unsupported} {
				job := &storage.ReviewJob{RepoPath: tc.TmpDir, WorktreePath: linked, Agent: name, JobType: storage.JobTypeReview, ReviewType: "quotes"}
				for _, selected := range []string{"", name} {
					t.Run(fmt.Sprintf("agent=%s/selected=%s", name, selected), func(t *testing.T) {
						opts, err := resolveRerunOpts(job, config.DefaultConfig(), nil, selected)
						if strict && name == unsupported {
							require.ErrorIs(t, err, agent.ErrReviewToolPolicy)
							require.ErrorContains(t, err, "does not support strict review tools")
							return
						}
						require.NoError(t, err)
						assert.Equal(t, "linked-model", opts.Model)
					})
				}
			}
		})
	}
}

func TestReviewExecutionBudgetUsesLinkedWorktreePolicy(t *testing.T) {
	for _, kind := range []string{"strict", "ordinary", "removed", "unrelated"} {
		t.Run(kind, func(t *testing.T) {
			tc := newWorkerTestContext(t, 1)
			for _, candidate := range []agent.Agent{&structuredWorkerTestAgent{name: "codex"}, &budgetStructuredReviewAgent{&budgetRecordingAgent{name: "gemini"}}} {
				original, err := agent.Get(candidate.Name())
				require.NoError(t, err)
				agent.Register(candidate)
				t.Cleanup(func() { agent.Register(original) })
			}
			linked := filepath.Join(t.TempDir(), "linked")
			tc.GitRepo.RunGit("worktree", "add", "--detach", linked)
			text := "[review.types.quotes]\ntemplate = 'rubric.md'\n"
			if kind == "strict" {
				text += "[review.types.quotes.tools]\nmode = 'read-only'\n"
			}
			require.NoError(t, os.WriteFile(filepath.Join(linked, ".roborev.toml"), []byte(text), 0o600))
			// A stale path must fail even if the main checkout has a valid type.
			if kind == "removed" || kind == "unrelated" {
				require.NoError(t, os.WriteFile(filepath.Join(tc.TmpDir, ".roborev.toml"), []byte(text), 0o600))
				if kind == "removed" {
					tc.GitRepo.RunGit("worktree", "remove", "--force", linked)
				} else {
					linked = testutil.NewGitRepo(t).Path()
				}
			}
			cfg := config.DefaultConfig()
			cfg.Budget = config.BudgetConfig{Enabled: true, DailyLimitCents: 100, ReserveFloorCents: 100, AgentCosts: config.BudgetAgentCosts{"codex": 15, "gemini": 5}}
			tc.reconfigurePool(cfg)
			job := tc.createAndClaimJobWithAgent(t, testutil.GetHeadSHA(t, tc.TmpDir), testWorkerID, "codex")
			job.WorktreePath, job.ReviewType, job.JobType = linked, "quotes", storage.JobTypeReview
			selected, proceed := tc.Pool.selectBudgetJobAgent(t.Context(), testWorkerID, job, cfg)
			if kind == "removed" || kind == "unrelated" {
				require.False(t, proceed)
				require.Nil(t, selected)
				return
			}
			require.True(t, proceed)
			require.NotNil(t, selected)
			want := "gemini"
			if kind == "strict" {
				want = "codex"
			}
			assert.Equal(t, want, selected.Name())
		})
	}
}
