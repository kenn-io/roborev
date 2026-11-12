package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testenv"
	"go.kenn.io/roborev/internal/testutil"
)

// readySuppliedSynthesis runs a member through the API and worker, then returns
// the claimed synthesis job. Tests can fault only synthesis persistence.
func readySuppliedSynthesis(t *testing.T, server *Server, synthesisAgent, instructions, memberOutput string) *storage.ReviewJob {
	t.Helper()
	repo := testutil.NewGitRepo(t)
	head := repo.CommitFile("file.txt", "target", "target")
	agent.RegisterForTest(t, &agent.FakeAgent{NameStr: "history-member", ReviewFn: func(context.Context, string, string, string, io.Writer) (string, error) {
		return memberOutput, nil
	}})
	w := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(w, testutil.MakeJSONRequest(t, http.MethodPost, "/api/panel/enqueue", map[string]any{
		"repo_path": repo.Path(), "git_ref": head,
		"members":   []any{map[string]any{"name": "reviewer", "agent": "history-member", "review_prompt": "Review.", "review_settings": map[string]any{}}},
		"synthesis": map[string]any{"agent": synthesisAgent, "instructions": instructions},
	}))
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	member, err := server.db.ClaimJob(testWorkerID)
	require.NoError(t, err)
	require.NotNil(t, member)
	server.workerPool.processJob(testWorkerID, member)
	job, err := server.db.ClaimJob(testWorkerID)
	require.NoError(t, err)
	require.NotNil(t, job)
	require.True(t, job.IsSynthesisJob())
	return job
}

func TestSuppliedSynthesisHistoryPreservesRetryOutput(t *testing.T) {
	testenv.SetDataDir(t)
	assert := assert.New(t)
	server, db, _ := newTestServer(t)
	server.workerPool.retryBackoff = 0
	calls := 0
	agent.RegisterForTest(t, &agent.FakeAgent{NameStr: "history-retry", ReviewFn: func(_ context.Context, _, _, _ string, out io.Writer) (string, error) {
		calls++
		if calls == 1 {
			// An adapter may stream evidence but return no final response.
			fmt.Fprintln(out, "partial response")
			return "", errors.New("connection reset by peer")
		}
		return "invalid synthesis document", nil
	}})
	job := readySuppliedSynthesis(t, server, "history-retry", "Verify.", string(testutil.ReviewFixtureJSON("## Findings\n- [P1] Missing validation")))
	for attempt := range 2 {
		server.workerPool.processJob(testWorkerID, job)
		if attempt == 0 {
			var err error
			job, err = db.ClaimJob(testWorkerID)
			require.NoError(t, err)
			require.NotNil(t, job)
		}
	}
	reports, err := db.ListReviewInvocations(t.Context(), job.ID)
	require.NoError(t, err)
	require.Len(t, reports, 2)
	assert.Equal("failed", reports[0].Status)
	assert.Equal("partial response\n", reports[0].RawOutput)
	assert.Equal("connection reset by peer", reports[0].Error)
	assert.Equal("failed", reports[1].Status)
	assert.Equal("invalid synthesis document", reports[1].RawOutput)
	assert.NotEmpty(reports[1].Error)
	assert.Empty(reports[1].StructuredOutput)
	assert.Nil(reports[1].Usage)
	assert.Equal(1, reports[1].RetryCount)
	assert.Equal(2, calls)
}

func TestSuppliedSynthesisHistoryWriteFailureStopsExecution(t *testing.T) {
	for _, stage := range []string{"start", "completion"} {
		t.Run(stage, func(t *testing.T) {
			testenv.SetDataDir(t)
			assert := assert.New(t)
			server, db, _ := newTestServer(t)
			calls := 0
			agent.RegisterForTest(t, &agent.FakeAgent{NameStr: "history-storage", ReviewFn: func(context.Context, string, string, string, io.Writer) (string, error) {
				calls++
				return "partial", errors.New("connection reset by peer")
			}})
			job := readySuppliedSynthesis(t, server, "history-storage", "Verify.", string(testutil.ReviewFixtureJSON("## Findings\n- [P1] Missing validation")))
			statement := `CREATE TRIGGER fail_invocation_write BEFORE INSERT ON review_invocations BEGIN SELECT RAISE(ABORT, 'storage unavailable'); END`
			if stage == "completion" {
				statement = `CREATE TRIGGER fail_invocation_write BEFORE UPDATE ON review_invocations BEGIN SELECT RAISE(ABORT, 'storage unavailable'); END`
			}
			_, err := db.ExecContext(t.Context(), statement)
			require.NoError(t, err)
			server.workerPool.processJob(testWorkerID, job)
			stored, err := db.GetJobByID(job.ID)
			require.NoError(t, err)
			assert.Equal(storage.JobStatusFailed, stored.Status)
			assert.Contains(stored.Error, "storage unavailable")
			assert.Zero(stored.RetryCount)
			reports, err := db.ListReviewInvocations(t.Context(), job.ID)
			require.NoError(t, err)
			if stage == "start" {
				assert.Zero(calls)
				assert.Empty(reports)
			} else {
				assert.Equal(1, calls)
				require.Len(t, reports, 1)
				assert.Equal("unfinished", reports[0].Status)
				assert.Nil(reports[0].FinishedAt)
			}
		})
	}
}

func TestSuppliedSynthesisHistoryKeepsCanceledOutput(t *testing.T) {
	testenv.SetDataDir(t)
	assert := assert.New(t)
	server, db, _ := newTestServer(t)
	entered := make(chan struct{})
	agent.RegisterForTest(t, &agent.FakeAgent{NameStr: "history-cancel", ReviewFn: func(ctx context.Context, _, _, _ string, _ io.Writer) (string, error) {
		close(entered)
		<-ctx.Done()
		return "partial canceled synthesis", ctx.Err()
	}})
	job := readySuppliedSynthesis(t, server, "history-cancel", "Verify.", string(testutil.ReviewFixtureJSON("## Findings\n- [P1] Missing validation")))
	done := make(chan struct{})
	go func() { defer close(done); server.workerPool.processJob(testWorkerID, job) }()
	t.Cleanup(func() { server.workerPool.CancelJob(job.ID); <-done })
	<-entered
	running, err := db.ListReviewInvocations(t.Context(), job.ID)
	require.NoError(t, err)
	require.Len(t, running, 1)
	assert.Equal("unfinished", running[0].Status)
	_, err = server.humaCancelJob(t.Context(), &CancelJobInput{Body: CancelJobRequest{JobID: job.ID}})
	require.NoError(t, err)
	<-done
	reports, err := db.ListReviewInvocations(t.Context(), job.ID)
	require.NoError(t, err)
	require.Len(t, reports, 1)
	assert.Equal("canceled", reports[0].Status)
	assert.Equal("partial canceled synthesis", reports[0].RawOutput)
	assert.NotNil(reports[0].FinishedAt)
}

func TestSuppliedSynthesisWithoutAgentHasNoInvocation(t *testing.T) {
	for _, tc := range []struct{ name, member, instructions string }{
		{"passthrough", "## Findings\n- [P1] Missing validation", ""},
		{"clean", "No issues found.", "Verify."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testenv.SetDataDir(t)
			assert := assert.New(t)
			server, db, _ := newTestServer(t)
			calls := 0
			agent.RegisterForTest(t, &agent.FakeAgent{NameStr: "history-no-call", ReviewFn: func(context.Context, string, string, string, io.Writer) (string, error) {
				calls++
				return string(testutil.ReviewFixtureJSON("No issues found.")), nil
			}})
			job := readySuppliedSynthesis(t, server, "history-no-call", tc.instructions, string(testutil.ReviewFixtureJSON(tc.member)))
			server.workerPool.processJob(testWorkerID, job)
			stored, err := db.GetJobByID(job.ID)
			require.NoError(t, err)
			assert.Equal(storage.JobStatusDone, stored.Status)
			reports, err := db.ListReviewInvocations(t.Context(), job.ID)
			require.NoError(t, err)
			assert.Empty(reports)
			assert.Zero(calls)
		})
	}
}
