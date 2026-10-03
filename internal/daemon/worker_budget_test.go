package daemon

import (
	"context"
	"errors"
	"io"
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
	reviewpkg "go.kenn.io/roborev/internal/review"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
	"go.kenn.io/roborev/internal/tokens"
)

type budgetRecordingAgent struct {
	name, model, prompt string
	calls               int
	err                 error
}

func (a *budgetRecordingAgent) Name() string                                   { return a.name }
func (a *budgetRecordingAgent) WithReasoning(agent.ReasoningLevel) agent.Agent { return a }
func (a *budgetRecordingAgent) WithAgentic(bool) agent.Agent                   { return a }
func (a *budgetRecordingAgent) WithModel(model string) agent.Agent             { a.model = model; return a }
func (a *budgetRecordingAgent) CommandLine() string                            { return a.name }
func (a *budgetRecordingAgent) Review(_ context.Context, _, _, prompt string, _ io.Writer) (string, error) {
	a.calls++
	a.prompt = prompt
	if a.err != nil {
		return "", a.err
	}
	return `{"schema_version":2,"summary":"No issues found.","verdict":"pass","findings":[]}`, nil
}

func configureUnpricedBudgetRouting(t *testing.T, tc *workerTestContext) (*config.Config, time.Time) {
	t.Helper()
	for _, name := range []string{"codex", "gemini"} {
		original, err := agent.Get(name)
		require.NoError(t, err)
		agent.Register(&budgetRecordingAgent{name: name})
		t.Cleanup(func() { agent.Register(original) })
	}
	cfg := config.DefaultConfig()
	cfg.Budget = config.BudgetConfig{
		Enabled: true, DailyLimitCents: 100, ReserveFloorCents: 100,
		AgentCosts: config.BudgetAgentCosts{"codex": 15, "gemini": 5},
	}
	tc.reconfigurePool(cfg)
	now := time.Now().UTC()
	tc.Pool.budgetRouter.now = func() time.Time { return now }
	selected, err := tc.Pool.budgetRouter.ResolveAgent("codex", nil, cfg, config.ReviewTypeDefault)
	require.NoError(t, err)
	assert.Equal(t, "gemini", selected.Name())
	return cfg, now
}

func TestProcessJobBudgetSelection(t *testing.T) {
	for _, tc := range []struct {
		name      string
		disabled  bool
		setup     func(*config.Config) string
		wantModel string
		mutate    func(*storage.ReviewJob)
		want      string
	}{
		{name: "substitutes agent and clears foreign model", want: "gemini"},
		{name: "native backup ignores foreign global model", want: "gemini", setup: func(c *config.Config) string {
			c.DefaultBackupAgent = "codex"
			c.DefaultBackupModel = "foreign-model"
			return "review_backup_agent = \"gemini\"\n"
		}},
		{name: "native backup uses paired repo model", want: "gemini", wantModel: "paired-model", setup: func(c *config.Config) string {
			c.DefaultBackupAgent = "codex"
			c.DefaultBackupModel = "foreign-model"
			return "review_backup_agent = \"gemini\"\nreview_backup_model = \"paired-model\"\n"
		}},
		{name: "native backup uses paired global model", want: "gemini", wantModel: "paired-model", setup: func(c *config.Config) string {
			c.DefaultBackupAgent = "gemini"
			c.DefaultBackupModel = "paired-model"
			return ""
		}},
		{name: "explicit agent uses backup model when defaults match", want: "gemini", wantModel: "paired-model", setup: func(c *config.Config) string {
			c.DefaultAgent = "gemini"
			c.DefaultBackupAgent = "gemini"
			c.DefaultBackupModel = "paired-model"
			return ""
		}},
		{name: "disabled", disabled: true, want: "codex"},
		{name: "explicit model", mutate: func(j *storage.ReviewJob) { j.RequestedModel = "pinned" }, want: "codex"},
		{name: "explicit provider", mutate: func(j *storage.ReviewJob) { j.RequestedProvider = "pinned" }, want: "codex"},
		{name: "resumed session", mutate: func(j *storage.ReviewJob) { j.SessionID = "session-existing" }, want: "codex"},
		{name: "retry", mutate: func(j *storage.ReviewJob) { j.RetryCount = 1 }, want: "codex"},
		{name: "failover lock", mutate: func(j *storage.ReviewJob) { j.BudgetRoutingLocked = true }, want: "codex"},
		{name: "frozen experiment", mutate: func(j *storage.ReviewJob) { j.FrozenExperimentPlan = &storage.ExperimentAssignmentInput{} }, want: "codex"},
		{name: "panel member", mutate: func(j *storage.ReviewJob) { j.PanelRole = storage.PanelRoleMember }, want: "codex"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := newWorkerTestContext(t, 1)
			primary := &budgetRecordingAgent{name: "codex"}
			cheap := &budgetRecordingAgent{name: "gemini"}
			for _, a := range []*budgetRecordingAgent{primary, cheap} {
				original, err := agent.Get(a.name)
				require.NoError(t, err)
				agent.Register(a)
				t.Cleanup(func() { agent.Register(original) })
			}
			cfg := config.DefaultConfig()
			cfg.Budget = config.BudgetConfig{Enabled: !tc.disabled, DailyLimitCents: 500, ReserveFloorCents: 100, AgentCosts: config.BudgetAgentCosts{"codex": 15, "gemini": 5}}
			if tc.setup != nil {
				repoConfig := tc.setup(cfg)
				if repoConfig != "" {
					require.NoError(t, os.WriteFile(filepath.Join(ctx.TmpDir, ".roborev.toml"), []byte(repoConfig), 0o600))
				}
			}
			ctx.reconfigurePool(cfg)
			prior, err := ctx.DB.EnqueueJob(storage.EnqueueOpts{RepoID: ctx.Repo.ID, GitRef: "HEAD", Agent: "test"})
			require.NoError(t, err)
			_, err = ctx.DB.Exec(`UPDATE review_jobs SET status='done',started_at=?,finished_at=?,agent_invoked=1,token_usage='{"has_cost":true,"cost_usd":5}' WHERE id=?`, time.Now().UTC().Format(time.RFC3339), time.Now().UTC().Format(time.RFC3339), prior.ID)
			require.NoError(t, err)
			sha := testutil.GetHeadSHA(t, ctx.TmpDir)
			job := ctx.createAndClaimJobWithAgent(t, sha, testWorkerID, "codex")
			job.Model = "primary-model"
			job.Provider = "primary-provider"
			_, err = ctx.DB.Exec(`UPDATE review_jobs SET model=?,provider=? WHERE id=?`, job.Model, job.Provider, job.ID)
			require.NoError(t, err)
			if tc.mutate != nil {
				tc.mutate(job)
			}
			ctx.Pool.processJob(testWorkerID, job)
			got := ctx.assertJobStatus(t, job.ID, storage.JobStatusDone)
			assert.Equal(t, tc.want, got.Agent)
			if tc.want == "gemini" {
				assert.Equal(t, tc.wantModel, got.Model)
				assert.Empty(t, got.Provider)
				assert.True(t, got.BudgetRoutingLocked)
				assert.Equal(t, 1, cheap.calls)
				assert.Zero(t, primary.calls)
				assert.Equal(t, tc.wantModel, cheap.model)
				assert.Equal(t, got.Prompt, cheap.prompt)
			} else {
				assert.Equal(t, "primary-model", got.Model)
				assert.Equal(t, 1, primary.calls)
				assert.Zero(t, cheap.calls)
			}
		})
	}
}

func TestBudgetRoutingPreservesTestAgent(t *testing.T) {
	tc := newWorkerTestContext(t, 1)
	cfg, _ := configureUnpricedBudgetRouting(t, tc)
	job := tc.createAndClaimJobWithAgent(t, testutil.GetHeadSHA(t, tc.TmpDir), testWorkerID, "test")

	selected, proceed := tc.Pool.selectBudgetJobAgent(context.Background(), testWorkerID, job, cfg)

	require.True(t, proceed)
	assert.Nil(t, selected)
	assert.Equal(t, "test", job.Agent)
	assert.False(t, job.BudgetRoutingLocked)
}

func TestBudgetRoutingRerunRestoresAgent(t *testing.T) {
	for _, selected := range []string{"", "gemini"} {
		t.Run("selected="+selected, func(t *testing.T) {
			assert := assert.New(t)
			tc := newWorkerTestContext(t, 1)
			cfg, now := configureUnpricedBudgetRouting(t, tc)
			cfg.DefaultAgent = "codex"
			cfg.DefaultModel = "primary-model"
			cfg.DefaultBackupAgent = "gemini"
			cfg.DefaultBackupModel = "backup-model"
			activity, err := NewActivityLog(filepath.Join(t.TempDir(), "activity.log"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, activity.Close()) })
			tc.Pool.activityLog = activity
			job := tc.createAndClaimJobWithAgent(t, testutil.GetHeadSHA(t, tc.TmpDir), testWorkerID, "codex")
			job.Model = "primary-model"
			job.BackupAgent = "gemini"
			job.BackupModel = "backup-model"
			_, err = tc.DB.Exec(`UPDATE review_jobs SET model=?,backup_agent=?,backup_model=? WHERE id=?`,
				job.Model, job.BackupAgent, job.BackupModel, job.ID)
			require.NoError(t, err)
			tc.Pool.processJob(testWorkerID, job)
			done := tc.assertJobStatus(t, job.ID, storage.JobStatusDone)
			require.Equal(t, "gemini", done.Agent)
			var startedAgent string
			for _, entry := range activity.Recent() {
				if entry.Event == "job.started" {
					startedAgent = entry.Details["agent"]
				}
			}
			assert.Equal("gemini", startedAgent)

			// A new day is below the reserve threshold. Exercise the rerun API
			// so model resolution and persisted agent restoration agree.
			cfg.Budget.ReserveFloorCents = 20
			tc.Pool.budgetRouter.now = func() time.Time { return now.AddDate(0, 0, 1) }
			server := NewServer(tc.DB, cfg, "")
			t.Cleanup(func() { require.NoError(t, server.Close()) })
			subscriberID, events := server.broadcaster.Subscribe("")
			defer server.broadcaster.Unsubscribe(subscriberID)
			req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/job/rerun", RerunJobRequest{
				JobID: job.ID, Agent: selected,
			})
			w := httptest.NewRecorder()
			server.httpServer.Handler.ServeHTTP(w, req)
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			rerun, err := tc.DB.ClaimJob(testWorkerID)
			require.NoError(t, err)
			require.NotNil(t, rerun)
			wantAgent, wantModel := "codex", "primary-model"
			if selected != "" {
				wantAgent, wantModel = "gemini", "backup-model"
			}
			assert.Equal(wantAgent, rerun.Agent)
			require.Len(t, events, 1)
			assert.Equal(wantAgent, (<-events).Agent)
			assert.Equal(wantModel, rerun.Model)
			assert.Equal("gemini", rerun.BackupAgent)
			assert.Equal("backup-model", rerun.BackupModel)
			tc.Pool.processJob(testWorkerID, rerun)
			done = tc.assertJobStatus(t, rerun.ID, storage.JobStatusDone)
			assert.Equal(wantAgent, done.Agent)
			assert.Equal(wantModel, done.Model)
		})
	}
}

func TestBudgetRoutingStoredPromptIgnoresMalformedRepoConfig(t *testing.T) {
	for _, price := range []string{"priced", "unpriced"} {
		t.Run(price, func(t *testing.T) {
			setupTestEnv(t)
			tc := newWorkerTestContext(t, 1)
			cfg, _ := configureUnpricedBudgetRouting(t, tc)
			if price == "unpriced" {
				delete(cfg.Budget.AgentCosts, "codex")
			}
			job, err := tc.DB.EnqueueJob(storage.EnqueueOpts{
				RepoID: tc.Repo.ID, GitRef: "run:task", Agent: "codex",
				Prompt: "Do the task", JobType: storage.JobTypeTask,
			})
			require.NoError(t, err)
			claimed, err := tc.DB.ClaimJob(testWorkerID)
			require.NoError(t, err)
			require.NotNil(t, claimed)
			require.Equal(t, job.ID, claimed.ID)
			require.NoError(t, os.WriteFile(filepath.Join(tc.TmpDir, ".roborev.toml"), []byte("invalid = ["), 0o600))

			tc.Pool.processJob(testWorkerID, claimed)
			got := tc.assertJobStatus(t, job.ID, storage.JobStatusDone)
			assert.Equal(t, "codex", got.Agent)
			assert.False(t, got.BudgetRoutingLocked)
			assert.Zero(t, got.RetryCount)
		})
	}
}

func TestBudgetRoutingErrorsPreserveAgentAndIsolateLog(t *testing.T) {
	for _, failure := range []string{"repo config", "persist selection"} {
		t.Run(failure, func(t *testing.T) {
			setupTestEnv(t)
			assert := assert.New(t)
			tc := newWorkerTestContext(t, 1)
			configureUnpricedBudgetRouting(t, tc)
			job := tc.createAndClaimJobWithAgent(t, testutil.GetHeadSHA(t, tc.TmpDir), testWorkerID, "codex")
			require.NoError(t, os.MkdirAll(filepath.Dir(JobLogPath(job.ID)), 0o700))
			require.NoError(t, os.WriteFile(JobLogPath(job.ID), []byte("previous attempt output\n"), 0o600))
			if failure == "repo config" {
				require.NoError(t, os.WriteFile(filepath.Join(tc.TmpDir, ".roborev.toml"), []byte("invalid = ["), 0o600))
			} else {
				_, err := tc.DB.Exec(`CREATE TRIGGER reject_budget_selection BEFORE UPDATE OF budget_routing_locked ON review_jobs
					WHEN NEW.budget_routing_locked = 1 BEGIN SELECT RAISE(FAIL, 'budget selection unavailable'); END`)
				require.NoError(t, err)
			}
			tc.Pool.processJob(testWorkerID, job)
			got := tc.assertJobStatus(t, job.ID, storage.JobStatusQueued)
			assert.Equal("codex", got.Agent)
			assert.False(got.BudgetRoutingLocked)
			data, err := os.ReadFile(JobLogPath(job.ID))
			require.NoError(t, err)
			assert.NotContains(string(data), "previous attempt output")
		})
	}
}

func TestBudgetReconciliationInvalidatesCachedSpend(t *testing.T) {
	tc := newWorkerTestContext(t, 1)
	cfg := config.DefaultConfig()
	cfg.CodexCmd = "go"
	cfg.GeminiCmd = "go"
	cfg.Budget = config.BudgetConfig{Enabled: true, DailyLimitCents: 500, AgentCosts: config.BudgetAgentCosts{"codex": 15, "gemini": 5}}
	tc.reconfigurePool(cfg)
	job := seedTokenCostCandidate(t, tc, "budget-session", `{"total_output_tokens":1}`)
	now := time.Now()
	tc.Pool.budgetRouter.now = func() time.Time { return now }
	before, err := tc.Pool.budgetRouter.ResolveAgent("codex", nil, cfg, config.ReviewTypeDefault)
	require.NoError(t, err)
	assert.Equal(t, "codex", before.Name())
	tc.Pool.tokenUsageFetcher = func(context.Context, string) (*tokens.Usage, error) {
		return &tokens.Usage{HasCost: true, CostUSD: 5}, nil
	}
	updated, err := tc.Pool.reconcileTokenCostJob(context.Background(), job.ID)
	require.NoError(t, err)
	require.True(t, updated)
	after, err := tc.Pool.budgetRouter.ResolveAgent("codex", nil, cfg, config.ReviewTypeDefault)
	require.NoError(t, err)
	assert.Equal(t, "gemini", after.Name())
}

func TestBudgetRoutingSkipsUnsupportedCustomReviewCandidates(t *testing.T) {
	original, err := agent.Get("gemini")
	require.NoError(t, err)
	agent.Register(&budgetRecordingAgent{name: "gemini"})
	t.Cleanup(func() { agent.Register(original) })

	for _, tc := range []struct {
		name  string
		costs config.BudgetAgentCosts
		want  string
	}{
		{name: "keep capable configured agent", costs: config.BudgetAgentCosts{"codex": 15, "gemini": 5}, want: "codex"},
		{name: "leave unpriced agent to ordinary resolution", costs: config.BudgetAgentCosts{"gemini": 5}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := newWorkerTestContext(t, 1)
			cfg := config.DefaultConfig()
			cfg.CodexCmd = "go"
			cfg.Budget = config.BudgetConfig{
				Enabled: true, DailyLimitCents: 500, ReserveFloorCents: 100, AgentCosts: tc.costs,
			}
			ctx.reconfigurePool(cfg)
			prior, err := ctx.DB.EnqueueJob(storage.EnqueueOpts{RepoID: ctx.Repo.ID, GitRef: "HEAD", Agent: "test"})
			require.NoError(t, err)
			now := time.Now().UTC().Format(time.RFC3339)
			_, err = ctx.DB.Exec(`UPDATE review_jobs SET status='done',started_at=?,finished_at=?,agent_invoked=1,token_usage='{"has_cost":true,"cost_usd":5}' WHERE id=?`, now, now, prior.ID)
			require.NoError(t, err)

			job := ctx.createAndClaimJobWithAgent(t, testutil.GetHeadSHA(t, ctx.TmpDir), testWorkerID, "codex")
			job.ReviewType = "custom"
			selected, proceed := ctx.Pool.selectBudgetJobAgent(context.Background(), testWorkerID, job, cfg)
			require.True(t, proceed)
			if tc.want == "" {
				assert.Nil(t, selected)
			} else {
				require.NotNil(t, selected)
				assert.Equal(t, tc.want, selected.Name())
			}
			assert.Equal(t, "codex", job.Agent)
			assert.False(t, job.BudgetRoutingLocked)
		})
	}
}

func TestBudgetRoutingPreservesConfiguredAgentAsFailover(t *testing.T) {
	tc := newWorkerTestContext(t, 1)
	primary := &budgetRecordingAgent{name: "codex"}
	backup := &budgetRecordingAgent{name: "gemini", err: errors.New("quota exceeded")}
	for _, a := range []*budgetRecordingAgent{primary, backup} {
		original, err := agent.Get(a.name)
		require.NoError(t, err)
		agent.Register(a)
		t.Cleanup(func() { agent.Register(original) })
	}

	cfg := config.DefaultConfig()
	cfg.DefaultBackupAgent = "gemini"
	cfg.Budget = config.BudgetConfig{
		Enabled: true, DailyLimitCents: 500, ReserveFloorCents: 100,
		AgentCosts: config.BudgetAgentCosts{"codex": 15, "gemini": 5},
	}
	tc.reconfigurePool(cfg)

	prior, err := tc.DB.EnqueueJob(storage.EnqueueOpts{RepoID: tc.Repo.ID, GitRef: "HEAD", Agent: "test"})
	require.NoError(t, err)
	now := time.Now().UTC().Format(time.RFC3339)
	_, err = tc.DB.Exec(`UPDATE review_jobs SET status='done',started_at=?,finished_at=?,agent_invoked=1,token_usage='{"has_cost":true,"cost_usd":5}' WHERE id=?`, now, now, prior.ID)
	require.NoError(t, err)

	job := tc.createAndClaimJobWithAgent(t, testutil.GetHeadSHA(t, tc.TmpDir), testWorkerID, "codex")
	tc.Pool.processJob(testWorkerID, job)

	requeued := tc.assertJobStatus(t, job.ID, storage.JobStatusQueued)
	assert.Equal(t, "codex", requeued.Agent)
	assert.Equal(t, "codex", requeued.BackupAgent)
	assert.Empty(t, requeued.BackupModel)
	job, err = tc.DB.ClaimJob(testWorkerID)
	require.NoError(t, err)
	require.NotNil(t, job)
	tc.Pool.processJob(testWorkerID, job)

	done := tc.assertJobStatus(t, requeued.ID, storage.JobStatusDone)
	assert.Equal(t, "codex", done.Agent)
	assert.Equal(t, "codex", done.BackupAgent)
	assert.Equal(t, 1, backup.calls)
	assert.Equal(t, 1, primary.calls)
}

func TestBudgetRoutingInvalidatesSpendAfterUnpricedAgentFailure(t *testing.T) {
	tc := newWorkerTestContext(t, 1)
	primary := &budgetRecordingAgent{name: "codex"}
	cheap := &budgetRecordingAgent{name: "gemini", err: errors.New("quota exceeded")}
	for _, a := range []*budgetRecordingAgent{primary, cheap} {
		original, err := agent.Get(a.name)
		require.NoError(t, err)
		agent.Register(a)
		t.Cleanup(func() { agent.Register(original) })
	}

	cfg := config.DefaultConfig()
	cfg.Budget = config.BudgetConfig{
		Enabled: true, DailyLimitCents: 100, ReserveFloorCents: 100,
		AgentCosts: config.BudgetAgentCosts{"codex": 15, "gemini": 5},
	}
	tc.reconfigurePool(cfg)
	now := time.Now().UTC()
	tc.Pool.budgetRouter.now = func() time.Time { return now }

	job := tc.createAndClaimJobWithAgent(t, testutil.GetHeadSHA(t, tc.TmpDir), testWorkerID, "codex")
	tc.Pool.processJob(testWorkerID, job)

	failed := tc.assertJobStatus(t, job.ID, storage.JobStatusFailed)
	assert.Equal(t, "gemini", failed.Agent)
	assert.Equal(t, 1, cheap.calls)

	spend, err := tc.DB.GetBudgetSpend(now)
	require.NoError(t, err)
	assert.Equal(t, 1, spend.JobsTotal)
	assert.Zero(t, spend.JobsWithCost)

	// Let the candidate back into selection so this assertion isolates spend
	// cache freshness from the quota cooldown applied by the failed attempt.
	tc.Pool.cooldownAgent("gemini", time.Now().Add(-time.Second))
	selected, err := tc.Pool.budgetRouter.ResolveAgent("codex", nil, cfg, config.ReviewTypeDefault)
	require.NoError(t, err)
	assert.Equal(t, "codex", selected.Name())
}

func TestBudgetRoutingInvalidatesSpendAfterUnpricedAgentCancellation(t *testing.T) {
	tc := newWorkerTestContext(t, 1)
	for _, name := range []string{"codex", "gemini"} {
		original, err := agent.Get(name)
		require.NoError(t, err)
		agent.Register(&budgetRecordingAgent{name: name})
		t.Cleanup(func() { agent.Register(original) })
	}
	cfg := config.DefaultConfig()
	cfg.CodexCmd = "go"
	cfg.GeminiCmd = "go"
	cfg.Budget = config.BudgetConfig{
		Enabled: true, DailyLimitCents: 100, ReserveFloorCents: 100,
		AgentCosts: config.BudgetAgentCosts{"codex": 15, "gemini": 5},
	}
	tc.reconfigurePool(cfg)
	now := time.Now().UTC()
	tc.Pool.budgetRouter.now = func() time.Time { return now }

	job := tc.createAndClaimJobWithAgent(t, testutil.GetHeadSHA(t, tc.TmpDir), testWorkerID, "codex")
	require.NoError(t, tc.DB.MarkJobAgentInvoked(job.ID, testWorkerID, "budget-test-agent"))
	before, err := tc.Pool.budgetRouter.ResolveAgent("codex", nil, cfg, config.ReviewTypeDefault)
	require.NoError(t, err)
	assert.Equal(t, "gemini", before.Name())

	require.NoError(t, tc.DB.CancelJob(job.ID))
	require.True(t, tc.Pool.CancelJob(job.ID))
	tc.Pool.finishRunningJob(testWorkerID, job.ID)

	spend, err := tc.DB.GetBudgetSpend(now)
	require.NoError(t, err)
	assert.Equal(t, 1, spend.JobsTotal)
	assert.Zero(t, spend.JobsWithCost)
	selected, err := tc.Pool.budgetRouter.ResolveAgent("codex", nil, cfg, config.ReviewTypeDefault)
	require.NoError(t, err)
	assert.Equal(t, "codex", selected.Name())
}

func TestBudgetRoutingInvalidatesSpendAfterClassifierSkip(t *testing.T) {
	for _, tc := range []struct {
		name       string
		transition func(*workerTestContext, *storage.ReviewJob)
	}{
		{
			name: "clean verdict",
			transition: func(tc *workerTestContext, job *storage.ReviewJob) {
				tc.Pool.applyClassifyVerdict(testWorkerID, job, false, "no design review needed")
			},
		},
		{
			name: "classifier failure",
			transition: func(tc *workerTestContext, job *storage.ReviewJob) {
				tc.Pool.completeClassifyAsSkip(testWorkerID, job, "classifier failed", "provider error")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := newWorkerTestContext(t, 1)
			cfg, now := configureUnpricedBudgetRouting(t, ctx)
			job := ctx.createAndClaimClassifyJob(t, "budget-classifier", "classifier", "+change\n")
			require.NoError(t, ctx.DB.MarkJobAgentInvoked(job.ID, testWorkerID, "budget classifier"))

			tc.transition(ctx, job)

			ctx.assertJobStatus(t, job.ID, storage.JobStatusSkipped)
			spend, err := ctx.DB.GetBudgetSpend(now)
			require.NoError(t, err)
			assert.Equal(t, 1, spend.JobsTotal)
			assert.Zero(t, spend.JobsWithCost)
			selected, err := ctx.Pool.budgetRouter.ResolveAgent("codex", nil, cfg, config.ReviewTypeDefault)
			require.NoError(t, err)
			assert.Equal(t, "codex", selected.Name())
		})
	}
}

func TestBudgetRoutingInvalidatesSpendAfterSynthesisCompletionWithoutUsage(t *testing.T) {
	tc := newWorkerTestContext(t, 1)
	cfg, now := configureUnpricedBudgetRouting(t, tc)

	queued, err := tc.DB.EnqueueJob(storage.EnqueueOpts{
		RepoID: tc.Repo.ID, GitRef: "budget-synthesis", Agent: "codex",
		JobType: storage.JobTypeSynthesis,
	})
	require.NoError(t, err)
	job, err := tc.DB.ClaimJob(testWorkerID)
	require.NoError(t, err)
	require.NotNil(t, job)
	assert.Equal(t, queued.ID, job.ID)
	require.NoError(t, tc.DB.MarkJobAgentInvoked(job.ID, testWorkerID, "budget synthesis"))

	tc.Pool.completeSynthesisContext(testWorkerID, job, synthesisResult{
		review: reviewpkg.ReviewResult{
			Agent: "codex", Verdict: storage.VerdictPass,
			StructuredOutput: testutil.ReviewFixtureJSON("No issues found."),
		},
	})

	tc.assertJobStatus(t, job.ID, storage.JobStatusDone)
	spend, err := tc.DB.GetBudgetSpend(now)
	require.NoError(t, err)
	assert.Equal(t, 1, spend.JobsTotal)
	assert.Zero(t, spend.JobsWithCost)
	selected, err := tc.Pool.budgetRouter.ResolveAgent("codex", nil, cfg, config.ReviewTypeDefault)
	require.NoError(t, err)
	assert.Equal(t, "codex", selected.Name())
}
