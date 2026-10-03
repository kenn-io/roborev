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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/prompt"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
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
				assert.Contains(t, review.Prompt, "Check cancellation")
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
	return fmt.Sprintf("phase-command --agentic=%t", a.agentic)
}

func TestWorkerPlanCommandLineTracksImplementation(t *testing.T) {
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
