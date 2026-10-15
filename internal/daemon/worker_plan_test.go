package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/prompt"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
	"go.kenn.io/roborev/internal/tokens"
)

func TestHandleFixJobPlanEnvelope(t *testing.T) {
	fixture := setupFixJobFixture(t, "planning-repo", "test-revision")
	req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/job/fix", FixJobRequest{ParentJobID: fixture.reviewJob.ID, PlanFirst: true, Prompt: "Preserve cancellation behavior"})
	w := httptest.NewRecorder()
	fixture.server.httpServer.Handler.ServeHTTP(w, req)
	assertHandlerStatus(t, w, http.StatusCreated)
	var job storage.ReviewJob
	testutil.DecodeJSON(t, w, &job)
	stored, err := fixture.db.GetJobByID(job.ID)
	require.NoError(t, err)
	planning, implementation, planned, err := prompt.DecodeFixPlan(stored.Prompt)
	require.NoError(t, err)
	assert.True(t, planned)
	assert.Contains(t, planning, "Fix Plan Request")
	assert.Contains(t, planning, fixture.reviewJob.GitRef)
	assert.Contains(t, planning, "Preserve cancellation behavior")
	assert.Contains(t, implementation, "FAIL: issues found")
}

func enqueuePlanTestJob(t *testing.T, tc *workerTestContext, name, stored string) *storage.ReviewJob {
	t.Helper()
	parent := tc.createAndClaimJob(t, testutil.GetHeadSHA(t, tc.TmpDir), "parent-worker")
	require.NoError(t, testutil.CompleteReviewFixture(tc.DB, parent.ID, "test", "review prompt", "High: missing cancellation check"))
	job, err := tc.DB.EnqueueJob(storage.EnqueueOpts{RepoID: tc.Repo.ID, GitRef: parent.GitRef, Agent: name, Prompt: stored, Agentic: true, JobType: storage.JobTypeFix, ParentJobID: parent.ID})
	require.NoError(t, err)
	claimed, err := tc.DB.ClaimJob(testWorkerID)
	require.NoError(t, err)
	require.Equal(t, job.ID, claimed.ID)
	return claimed
}

func TestWorkerPlanPhases(t *testing.T) {
	for _, mode := range []string{"success", "error", "empty", "mutation", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			tc := newWorkerTestContext(t, 1)
			calls := 0
			var jobID int64
			paths := []string{}
			refs := []string{}
			name := "worker-plan-" + mode
			agent.Register(&agent.FakeAgent{NameStr: name, ReviewFn: func(ctx context.Context, path, ref, p string, out io.Writer) (string, error) {
				calls++
				paths = append(paths, path)
				refs = append(refs, ref)
				if calls == 1 {
					assert.Contains(t, p, "Analyze cancellation")
					fmt.Fprintln(out, `{"type":"session_meta","payload":{"id":"planner-session"}}`)
					switch mode {
					case "error":
						return "", errors.New("provider temporarily unavailable")
					case "empty":
						return " ", nil
					case "mutation":
						require.NoError(t, os.WriteFile(filepath.Join(path, "mutation.txt"), []byte("bad"), 0o600))
					case "cancel":
						require.NoError(t, tc.DB.CancelJob(jobID))
						require.True(t, tc.Pool.CancelJob(jobID))
						return "", context.Canceled
					}
					return "Check cancellation before claiming work", nil
				}
				assert.Contains(t, p, "## Plan\n\nCheck cancellation before claiming work")
				require.NoError(t, os.WriteFile(filepath.Join(path, "fixed.txt"), []byte("fixed"), 0o600))
				return "Added cancellation check", nil
			}})
			t.Cleanup(func() { agent.Unregister(name) })
			stored := prompt.EncodeFixPlan("Analyze cancellation", "## Review Findings\n\nHigh: missing cancellation")
			job := enqueuePlanTestJob(t, tc, name, stored)
			jobID = job.ID
			beforeStatus := tc.GitRepo.Run("status", "--porcelain")
			headBefore := testutil.GetHeadSHA(t, tc.TmpDir)
			tc.Pool.processJob(testWorkerID, job)
			updated, err := tc.DB.GetJobByID(job.ID)
			require.NoError(t, err)
			assert.Equal(t, stored, updated.Prompt)
			expected := 1
			if mode == "success" {
				expected = 2
			}
			assert.Equal(t, expected, calls)
			assert.Equal(t, beforeStatus, tc.GitRepo.Run("status", "--porcelain"))
			assert.Equal(t, headBefore, testutil.GetHeadSHA(t, tc.TmpDir))
			assert.NotEqual(t, "planner-session", updated.SessionID)
			if mode == "success" {
				assert.Equal(t, storage.JobStatusDone, updated.Status)
				require.NotNil(t, updated.Patch)
				assert.Contains(t, *updated.Patch, "fixed.txt")
				review, err := tc.DB.GetReviewByJobID(job.ID)
				require.NoError(t, err)
				assert.Contains(t, review.Output, "## Plan")
				assert.Contains(t, review.Output, "## Implementation")
				assert.Equal(t, stored, review.Prompt)
				comments, err := tc.DB.GetCommentsForJob(*job.ParentJobID)
				require.NoError(t, err)
				require.Len(t, comments, 1)
				assert.Equal(t, "roborev-plan", comments[0].Responder)
				assert.NotEqual(t, paths[0], paths[1])
				assert.Equal(t, refs[0], refs[1])
			} else {
				assert.NotEqual(t, storage.JobStatusDone, updated.Status)
				assert.Nil(t, updated.Patch)
			}
			for _, path := range paths {
				_, err := os.Stat(path)
				require.ErrorIs(t, err, os.ErrNotExist)
			}
		})
	}
}

func TestWorkerPlanUsageIncludesBothSessions(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	tc := newWorkerTestContext(t, 1)
	name := "worker-plan-usage-accounting"
	calls := 0
	var jobID int64
	var fetchedSessions []string
	agent.Register(&agent.FakeAgent{NameStr: name, ReviewFn: func(_ context.Context, path, _, _ string, out io.Writer) (string, error) {
		calls++
		if calls == 1 {
			_, _ = fmt.Fprintln(out, `{"type":"thread.started","thread_id":"planner-session"}`)
			_, _ = fmt.Fprintln(out, `{"type":"turn.completed","usage":{"input_tokens":100,"cached_input_tokens":10,"cache_write_input_tokens":4,"output_tokens":15}}`)
			return "Check cancellation", nil
		}
		_, _ = fmt.Fprintln(out, `{"type":"thread.started","thread_id":"implementation-session"}`)
		_, _ = fmt.Fprintln(out, `{"type":"turn.completed","usage":{"input_tokens":200,"cached_input_tokens":20,"cache_write_input_tokens":8,"output_tokens":25}}`)
		require.NoError(t, os.WriteFile(filepath.Join(path, "fixed.txt"), []byte("fixed"), 0o600))
		return "Fixed cancellation", nil
	}})
	t.Cleanup(func() { agent.Unregister(name) })
	tc.Pool.tokenUsageFetcher = func(_ context.Context, sessionID string) (*tokens.Usage, error) {
		current, err := tc.DB.GetJobByID(jobID)
		require.NoError(t, err)
		queuedUsage := tokens.ParseJSON(current.TokenUsage)
		require.NotNil(t, queuedUsage)
		assert.Equal(t, []string{"planner-session", "implementation-session"}, queuedUsage.ProviderSessionIDs)
		assert.Equal(t, 2, queuedUsage.ExpectedProviderSessions)
		assert.False(t, queuedUsage.HasCost)
		fetchedSessions = append(fetchedSessions, sessionID)
		costs := map[string]float64{
			"planner-session":        0.13,
			"implementation-session": 0.29,
		}
		return &tokens.Usage{CostUSD: costs[sessionID], HasCost: true}, nil
	}
	job := enqueuePlanTestJob(t, tc, name, prompt.EncodeFixPlan("Analyze cancellation", "Fix cancellation"))
	jobID = job.ID

	tc.Pool.processJob(testWorkerID, job)

	updated := tc.assertJobStatus(t, job.ID, storage.JobStatusDone)
	usage := tokens.ParseJSON(updated.TokenUsage)
	require.NotNil(t, usage)
	assert.Equal(t, "implementation-session", updated.SessionID)
	assert.ElementsMatch(t, []string{"planner-session", "implementation-session"}, fetchedSessions)
	assert.Equal(t, int64(300), usage.InputTokens)
	assert.Equal(t, int64(30), usage.CachedInputTokens)
	assert.Equal(t, int64(12), usage.CacheCreationTokens)
	assert.Equal(t, int64(40), usage.OutputTokens)
	assert.True(t, usage.HasCost)
	assert.InDelta(t, 0.42, usage.CostUSD, 1e-9)
	assert.Equal(t, 2, usage.ExpectedProviderSessions)
}

func TestWorkerPlanPhaseSessionsArePersistedBeforeTerminalReconciliation(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	worker := newWorkerTestContext(t, 1)
	name := "worker-plan-reconcile-race"
	calls := 0
	var jobID int64
	var fetchedSessions []string
	plannerFetches := 0
	worker.Pool.tokenUsageFetcher = func(_ context.Context, sessionID string) (*tokens.Usage, error) {
		fetchedSessions = append(fetchedSessions, sessionID)
		if sessionID == "planner-session" {
			plannerFetches++
			return nil, errors.New("planner pricing is delayed")
		}
		return &tokens.Usage{CostUSD: 0.29, HasCost: true}, nil
	}
	agent.Register(&agent.FakeAgent{NameStr: name, ReviewFn: func(_ context.Context, _ string, _, _ string, out io.Writer) (string, error) {
		calls++
		if calls == 1 {
			_, _ = fmt.Fprintln(out, `{"type":"system","subtype":"init","session_id":"planner-session"}`)
			return "Check cancellation", nil
		}
		_, _ = fmt.Fprintln(out, `{"type":"system","subtype":"init","session_id":"implementation-session"}`)
		current, err := worker.DB.GetJobByID(jobID)
		require.NoError(t, err)
		metadata := tokens.ParseJSON(current.TokenUsage)
		require.NotNil(t, metadata)
		assert.ElementsMatch(t, []string{"planner-session", "implementation-session"}, metadata.ProviderSessionIDs)
		assert.Equal(t, 2, metadata.ExpectedProviderSessions)

		// Complete while the implementation call is still returning to exercise
		// the reconciler window that precedes deferred terminal capture.
		require.NoError(t, testutil.CompleteReviewFixture(
			worker.DB, jobID, "test", "prompt", "No review findings.",
		))
		candidate, err := worker.DB.GetTokenCostCandidate(jobID)
		require.NoError(t, err)
		require.NotNil(t, candidate)
		resolved, reconcileErr := worker.Pool.reconcileTokenCostCandidate(t.Context(), *candidate)
		require.Error(t, reconcileErr)
		assert.False(t, resolved)
		return "", agent.MarkUnavailable(errors.New("implementation unavailable"))
	}})
	t.Cleanup(func() { agent.Unregister(name) })
	job := enqueuePlanTestJob(t, worker, name, prompt.EncodeFixPlan("Analyze cancellation", "Fix cancellation"))
	jobID = job.ID

	worker.Pool.processJob(testWorkerID, job)

	updated, err := worker.DB.GetJobByID(job.ID)
	require.NoError(t, err)
	usage := tokens.ParseJSON(updated.TokenUsage)
	require.NotNil(t, usage)
	assert.Equal(t, []string{"planner-session", "implementation-session"}, usage.ProviderSessionIDs)
	assert.Equal(t, 2, usage.ExpectedProviderSessions)
	assert.False(t, usage.HasCost)
	assert.Equal(t, 2, plannerFetches)
	assert.Equal(t, []string{"planner-session", "planner-session", "implementation-session"}, fetchedSessions)
}

func TestWorkerPlanUsageIsCapturedAfterTerminalImplementationExit(t *testing.T) {
	for _, tcCase := range []struct {
		name        string
		wantStatus  storage.JobStatus
		cancelJob   bool
		wantFailure bool
	}{
		{name: "failure", wantStatus: storage.JobStatusFailed, wantFailure: true},
		{name: "cancellation", wantStatus: storage.JobStatusCanceled, cancelJob: true},
	} {
		t.Run(tcCase.name, func(t *testing.T) {
			t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
			worker := newWorkerTestContext(t, 1)
			name := "worker-plan-terminal-usage-" + tcCase.name
			calls := 0
			var jobID int64
			var fetchedSessions []string
			agent.Register(&agent.FakeAgent{NameStr: name, ReviewFn: func(_ context.Context, _ string, _, _ string, out io.Writer) (string, error) {
				calls++
				if calls == 1 {
					_, _ = fmt.Fprintln(out, `{"type":"thread.started","thread_id":"planner-session"}`)
					_, _ = fmt.Fprintln(out, `{"type":"turn.completed","usage":{"input_tokens":100,"output_tokens":15}}`)
					return "Check cancellation", nil
				}
				_, _ = fmt.Fprintln(out, `{"type":"thread.started","thread_id":"implementation-session"}`)
				_, _ = fmt.Fprintln(out, `{"type":"turn.completed","usage":{"input_tokens":200,"output_tokens":25}}`)
				if tcCase.cancelJob {
					require.NoError(t, worker.DB.CancelJob(jobID))
					require.True(t, worker.Pool.CancelJob(jobID))
					return "", context.Canceled
				}
				return "", agent.MarkUnavailable(errors.New("implementation unavailable"))
			}})
			t.Cleanup(func() { agent.Unregister(name) })
			worker.Pool.tokenUsageFetcher = func(_ context.Context, sessionID string) (*tokens.Usage, error) {
				current, err := worker.DB.GetJobByID(jobID)
				require.NoError(t, err)
				usage := tokens.ParseJSON(current.TokenUsage)
				require.NotNil(t, usage)
				assert.Equal(t, []string{"planner-session", "implementation-session"}, usage.ProviderSessionIDs)
				assert.Equal(t, 2, usage.ExpectedProviderSessions)
				assert.Equal(t, tcCase.wantStatus, current.Status)
				fetchedSessions = append(fetchedSessions, sessionID)
				costs := map[string]float64{
					"planner-session":        0.13,
					"implementation-session": 0.29,
				}
				return &tokens.Usage{CostUSD: costs[sessionID], HasCost: true}, nil
			}
			job := enqueuePlanTestJob(t, worker, name, prompt.EncodeFixPlan("Analyze cancellation", "Fix cancellation"))
			jobID = job.ID

			worker.Pool.processJob(testWorkerID, job)

			updated := worker.assertJobStatus(t, job.ID, tcCase.wantStatus)
			usage := tokens.ParseJSON(updated.TokenUsage)
			require.NotNil(t, usage)
			assert.Equal(t, "implementation-session", updated.SessionID)
			assert.ElementsMatch(t, []string{"planner-session", "implementation-session"}, fetchedSessions)
			assert.Equal(t, int64(300), usage.InputTokens)
			assert.Equal(t, int64(40), usage.OutputTokens)
			assert.True(t, usage.HasCost)
			assert.InDelta(t, 0.42, usage.CostUSD, 1e-9)
			assert.Equal(t, 2, calls)
			assert.Equal(t, tcCase.wantFailure, updated.Status == storage.JobStatusFailed)
		})
	}
}

func TestWorkerPlanUsageIsCapturedForTerminalPlanningFailure(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	worker := newWorkerTestContext(t, 1)
	name := "worker-plan-planning-failure-usage"
	fetches := 0
	worker.Pool.tokenUsageFetcher = func(_ context.Context, sessionID string) (*tokens.Usage, error) {
		assert.Equal(t, "planner-session", sessionID)
		fetches++
		return &tokens.Usage{InputTokens: 100, OutputTokens: 15, CostUSD: 0.13, HasCost: true}, nil
	}
	agent.Register(&agent.FakeAgent{NameStr: name, ReviewFn: func(_ context.Context, _ string, _, _ string, out io.Writer) (string, error) {
		_, _ = fmt.Fprintln(out, `{"type":"system","subtype":"init","session_id":"planner-session"}`)
		return "", agent.MarkUnavailable(errors.New("planning unavailable"))
	}})
	t.Cleanup(func() { agent.Unregister(name) })
	job := enqueuePlanTestJob(t, worker, name, prompt.EncodeFixPlan("Analyze cancellation", "Fix cancellation"))

	worker.Pool.processJob(testWorkerID, job)

	updated := worker.assertJobStatus(t, job.ID, storage.JobStatusFailed)
	usage := tokens.ParseJSON(updated.TokenUsage)
	require.NotNil(t, usage)
	assert.Equal(t, "planner-session", updated.SessionID)
	assert.Equal(t, []string{"planner-session"}, usage.ProviderSessionIDs)
	assert.Equal(t, 1, usage.ExpectedProviderSessions)
	assert.Equal(t, int64(100), usage.InputTokens)
	assert.True(t, usage.HasCost)
	assert.InDelta(t, 0.13, usage.CostUSD, 1e-9)
	assert.Equal(t, 1, fetches)
}

func TestWorkerPlanUsageIsCapturedForTerminalEmptyPlan(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	worker := newWorkerTestContext(t, 1)
	name := "worker-plan-empty-plan-usage"
	calls := 0
	var fetchedSessions []string
	worker.Pool.tokenUsageFetcher = func(_ context.Context, sessionID string) (*tokens.Usage, error) {
		fetchedSessions = append(fetchedSessions, sessionID)
		return &tokens.Usage{CostUSD: 0.13, HasCost: true}, nil
	}
	agent.Register(&agent.FakeAgent{NameStr: name, ReviewFn: func(_ context.Context, _ string, _, _ string, out io.Writer) (string, error) {
		calls++
		_, _ = fmt.Fprintf(out, `{"type":"system","subtype":"init","session_id":"planner-session-%d"}`+"\n", calls)
		return "", nil
	}})
	t.Cleanup(func() { agent.Unregister(name) })
	job := enqueuePlanTestJob(t, worker, name, prompt.EncodeFixPlan("Analyze cancellation", "Fix cancellation"))

	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			claimed, err := worker.DB.ClaimJob(testWorkerID)
			require.NoError(t, err)
			require.NotNil(t, claimed)
			job = claimed
		}
		worker.Pool.processJob(testWorkerID, job)
	}

	updated := worker.assertJobStatus(t, job.ID, storage.JobStatusFailed)
	expectedSession := fmt.Sprintf("planner-session-%d", maxRetries+1)
	usage := tokens.ParseJSON(updated.TokenUsage)
	require.NotNil(t, usage)
	assert.Equal(t, expectedSession, updated.SessionID)
	assert.Equal(t, []string{expectedSession}, usage.ProviderSessionIDs)
	assert.Equal(t, 1, usage.ExpectedProviderSessions)
	assert.True(t, usage.HasCost)
	assert.Equal(t, []string{expectedSession}, fetchedSessions)
	assert.Equal(t, maxRetries+1, calls)
}

func TestWorkerPlanUsageUsesPlannerAnchorWhenImplementationSessionIsMissing(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	worker := newWorkerTestContext(t, 1)
	name := "worker-plan-missing-implementation-session"
	calls := 0
	var fetchedSessions []string
	worker.Pool.tokenUsageFetcher = func(_ context.Context, sessionID string) (*tokens.Usage, error) {
		fetchedSessions = append(fetchedSessions, sessionID)
		return &tokens.Usage{InputTokens: 100, OutputTokens: 15, CostUSD: 0.13, HasCost: true}, nil
	}
	agent.Register(&agent.FakeAgent{NameStr: name, ReviewFn: func(_ context.Context, _ string, _, _ string, out io.Writer) (string, error) {
		calls++
		if calls == 1 {
			_, _ = fmt.Fprintln(out, `{"type":"system","subtype":"init","session_id":"planner-session"}`)
			return "Check cancellation", nil
		}
		return "", agent.MarkUnavailable(errors.New("implementation unavailable"))
	}})
	t.Cleanup(func() { agent.Unregister(name) })
	job := enqueuePlanTestJob(t, worker, name, prompt.EncodeFixPlan("Analyze cancellation", "Fix cancellation"))

	worker.Pool.processJob(testWorkerID, job)

	updated := worker.assertJobStatus(t, job.ID, storage.JobStatusFailed)
	usage := tokens.ParseJSON(updated.TokenUsage)
	require.NotNil(t, usage)
	assert.Equal(t, "planner-session", updated.SessionID)
	assert.Equal(t, []string{"planner-session"}, usage.ProviderSessionIDs)
	assert.Equal(t, 2, usage.ExpectedProviderSessions)
	assert.False(t, usage.HasCost)
	assert.Equal(t, []string{"planner-session"}, fetchedSessions)
}

func TestWorkerPlanUsageCaptureSkipsRequeuedAttempt(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	worker := newWorkerTestContext(t, 1)
	job := enqueuePlanTestJob(t, worker, "worker-plan-requeued-usage", prompt.EncodeFixPlan("Analyze", "Implement"))
	retried, err := worker.DB.RetryJob(job.ID, testWorkerID, maxRetries, 0)
	require.NoError(t, err)
	assert.True(t, retried)

	fetches := 0
	worker.Pool.tokenUsageFetcher = func(context.Context, string) (*tokens.Usage, error) {
		fetches++
		return &tokens.Usage{HasCost: true, CostUSD: 0.42}, nil
	}
	worker.Pool.capturePlanTokenUsageForTerminalAttempt(
		context.Background(), testWorkerID, job, []planTokenPhase{
			{invoked: true, sessionID: "planner-session", logUsage: &tokens.Usage{InputTokens: 100}},
			{invoked: true, implementation: true, sessionID: "implementation-session", logUsage: &tokens.Usage{InputTokens: 200}},
		},
	)

	updated, err := worker.DB.GetJobByID(job.ID)
	require.NoError(t, err)
	assert.Equal(t, storage.JobStatusQueued, updated.Status)
	assert.Empty(t, updated.TokenUsage)
	assert.Zero(t, fetches)
}

func TestWorkerPlanPhasesHaveDistinctDeadlines(t *testing.T) {
	tc := newWorkerTestContext(t, 1)
	name := "worker-plan-phase-deadlines"
	calls := 0
	var planningDeadline, implementationDeadline time.Time
	assertions := assert.New(t)
	agent.Register(&agent.FakeAgent{NameStr: name, ReviewFn: func(ctx context.Context, path, _, _ string, _ io.Writer) (string, error) {
		deadline, ok := ctx.Deadline()
		require.True(t, ok)
		calls++
		if calls == 1 {
			planningDeadline = deadline
			return "Check cancellation", nil
		}
		implementationDeadline = deadline
		require.NoError(t, os.WriteFile(filepath.Join(path, "fixed.txt"), []byte("fixed"), 0o600))
		return "Fixed cancellation", nil
	}})
	t.Cleanup(func() { agent.Unregister(name) })
	job := enqueuePlanTestJob(t, tc, name, prompt.EncodeFixPlan("Analyze cancellation", "Fix cancellation"))

	tc.Pool.processJob(testWorkerID, job)

	updated, err := tc.DB.GetJobByID(job.ID)
	require.NoError(t, err)
	assertions.Equal(storage.JobStatusDone, updated.Status)
	require.Equal(t, 2, calls)
	assertions.True(implementationDeadline.After(planningDeadline))
}

func TestWorkerPlanTimeoutMessageUsesPerJobTimeout(t *testing.T) {
	tc := newWorkerTestContext(t, 1)
	cfg := tc.Pool.cfgGetter.Config()
	cfg.JobTimeoutMinutes = 1
	tc.reconfigurePool(cfg)

	name := "worker-plan-timeout-message"
	calls := 0
	assertions := assert.New(t)
	agent.Register(&agent.FakeAgent{NameStr: name, ReviewFn: func(context.Context, string, string, string, io.Writer) (string, error) {
		calls++
		return "", context.DeadlineExceeded
	}})
	t.Cleanup(func() { agent.Unregister(name) })
	job := enqueuePlanTestJob(t, tc, name, prompt.EncodeFixPlan("Analyze cancellation", "Fix cancellation"))

	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			claimed, err := tc.DB.ClaimJob(testWorkerID)
			require.NoError(t, err)
			require.NotNil(t, claimed)
			job = claimed
		}
		tc.Pool.processJob(testWorkerID, job)
	}

	updated, err := tc.DB.GetJobByID(job.ID)
	require.NoError(t, err)
	assertions.Equal(storage.JobStatusFailed, updated.Status)
	assertions.Equal("agent timeout after 1m0s", updated.Error)
	assertions.Equal(maxRetries+1, calls)
}

func TestWorkerPlanRetryPreservesEnvelope(t *testing.T) {
	tc := newWorkerTestContext(t, 1)
	calls := 0
	name := "worker-plan-retry"
	agent.Register(&agent.FakeAgent{NameStr: name, ReviewFn: func(_ context.Context, path, ref, p string, _ io.Writer) (string, error) {
		calls++
		if strings.Contains(p, "Analyze cancellation") {
			return "Check cancellation", nil
		}
		if calls == 2 {
			return "", errors.New("provider temporarily unavailable")
		}
		require.NoError(t, os.WriteFile(filepath.Join(path, "fixed.txt"), []byte("fixed"), 0o600))
		return "Fixed cancellation", nil
	}})
	t.Cleanup(func() { agent.Unregister(name) })
	stored := prompt.EncodeFixPlan("Analyze cancellation", "## Review Findings\nHigh: cancellation")
	job := enqueuePlanTestJob(t, tc, name, stored)
	tc.Pool.processJob(testWorkerID, job)
	reloaded, err := tc.DB.GetJobByID(job.ID)
	require.NoError(t, err)
	assert.Equal(t, storage.JobStatusQueued, reloaded.Status)
	assert.Equal(t, stored, reloaded.Prompt)
	// A fresh pool consumes the persisted request just as daemon recovery does.
	cfg := tc.Pool.cfgGetter.Config()
	tc.reconfigurePool(cfg)
	retried, err := tc.DB.ClaimJob(testWorkerID)
	require.NoError(t, err)
	require.NotNil(t, retried)
	require.Equal(t, job.ID, retried.ID)
	tc.Pool.processJob(testWorkerID, retried)
	updated, err := tc.DB.GetJobByID(job.ID)
	require.NoError(t, err)
	assert.Equal(t, storage.JobStatusDone, updated.Status)
	assert.Equal(t, stored, updated.Prompt)
	assert.Equal(t, 4, calls)
}

func TestWorkerPlanRejectsMalformedEnvelope(t *testing.T) {
	tc := newWorkerTestContext(t, 1)
	calls := 0
	name := "worker-plan-malformed"
	agent.Register(&agent.FakeAgent{NameStr: name, ReviewFn: func(context.Context, string, string, string, io.Writer) (string, error) { calls++; return "", nil }})
	t.Cleanup(func() { agent.Unregister(name) })
	job := enqueuePlanTestJob(t, tc, name, "roborev-fix-plan-v1\ninvalid")
	tc.Pool.processJob(testWorkerID, job)
	updated, err := tc.DB.GetJobByID(job.ID)
	require.NoError(t, err)
	assert.Equal(t, storage.JobStatusFailed, updated.Status)
	assert.Zero(t, calls)
}

type phaseCommandAgent struct {
	agent.FakeAgent
	agentic bool
}

func (a *phaseCommandAgent) WithAgentic(enabled bool) agent.Agent {
	clone := *a
	clone.agentic = enabled
	return &clone
}

func (a *phaseCommandAgent) WithReasoning(agent.ReasoningLevel) agent.Agent {
	clone := *a
	return &clone
}

func (a *phaseCommandAgent) WithModel(string) agent.Agent {
	clone := *a
	return &clone
}

func (a *phaseCommandAgent) CommandLine() string {
	return fmt.Sprintf("phase-command --agentic=%t", a.agentic || agent.AllowUnsafeAgents())
}

func (a *phaseCommandAgent) PlanningCommandLine() string {
	return "phase-command --agentic=false"
}

func TestWorkerPlanCommandLineTracksImplementation(t *testing.T) {
	originalUnsafe := agent.AllowUnsafeAgents()
	agent.SetAllowUnsafeAgents(true)
	t.Cleanup(func() { agent.SetAllowUnsafeAgents(originalUnsafe) })
	tc := newWorkerTestContext(t, 1)
	calls := 0
	var jobID int64
	name := "worker-plan-command"
	a := &phaseCommandAgent{NameStr: name, ReviewFn: func(_ context.Context, path, _, _ string, _ io.Writer) (string, error) {
		calls++
		job, err := tc.DB.GetJobByID(jobID)
		require.NoError(t, err)
		if calls == 1 {
			assert.Equal(t, "phase-command --agentic=false", job.CommandLine)
			return "Add cancellation check", nil
		}
		assert.Equal(t, "phase-command --agentic=true", job.CommandLine)
		require.NoError(t, os.WriteFile(filepath.Join(path, "fixed.txt"), []byte("fixed"), 0o600))
		return "Added cancellation check", nil
	}}
	agent.RegisterForTest(t, a)
	job := enqueuePlanTestJob(t, tc, name, prompt.EncodeFixPlan("Analyze cancellation", "Fix cancellation"))
	jobID = job.ID
	tc.Pool.processJob(testWorkerID, job)
	updated, err := tc.DB.GetJobByID(job.ID)
	require.NoError(t, err)
	assert.Equal(t, 2, calls)
	assert.Equal(t, storage.JobStatusDone, updated.Status)
	assert.Equal(t, "phase-command --agentic=true", updated.CommandLine)
}
