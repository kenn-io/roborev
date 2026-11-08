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
	"go.kenn.io/roborev/internal/tokens"
)

func TestSuppliedReviewInvocationHistorySurvivesRetryAndRerun(t *testing.T) {
	testenv.SetDataDir(t)
	assert := assert.New(t)
	server, db, _ := newTestServer(t)
	server.workerPool.retryBackoff = 0
	server.workerPool.tokenUsageFetcher = func(context.Context, string) (*tokens.Usage, error) { return nil, tokens.ErrUsageProviderUnavailable }
	repo := testutil.NewGitRepo(t)
	head := repo.CommitFile("file.txt", "target\n", "target")
	calls := 0
	agent.RegisterForTest(t, &agent.FakeAgent{NameStr: "invocation-primary", ReviewFn: func(_ context.Context, _, _, _ string, out io.Writer) (string, error) {
		calls++
		fmt.Fprintf(out, "{\"type\":\"thread.started\",\"thread_id\":\"attempt-%d\"}\n{\"type\":\"turn.completed\",\"usage\":{\"input_tokens\":%d,\"output_tokens\":3}}\n", calls, calls*10)
		if calls == 1 {
			return "partial response", errors.New("connection reset by peer")
		}
		return "context exhausted", errors.New("context window exceeded")
	}})
	agent.RegisterForTest(t, &agent.FakeAgent{NameStr: "invocation-backup", ReviewFn: func(context.Context, string, string, string, io.Writer) (string, error) {
		return string(testutil.ReviewFixtureJSON("No issues found.")), nil
	}})
	job := enqueueViaHTTP(t, server, EnqueueRequest{RepoPath: repo.Path(), GitRef: head, Agent: "invocation-primary", Model: "primary-model", ReviewPrompt: "Review the change.", ReviewSettings: &SuppliedReviewSettings{BackupAgent: "invocation-backup", BackupModel: "backup-model"}})
	for range 3 {
		claimed, err := db.ClaimJob(testWorkerID)
		require.NoError(t, err)
		require.NotNil(t, claimed)
		server.workerPool.processJob(testWorkerID, claimed)
	}
	stored, err := db.GetJobByID(job.ID)
	require.NoError(t, err)
	require.Equal(t, storage.JobStatusDone, stored.Status, stored.Error)
	// Fetch through HTTP twice; reading the report neither changes it nor runs agents.
	type invocation struct {
		ID               int64          `json:"id"`
		Status           string         `json:"status"`
		Agent            string         `json:"agent"`
		Model            string         `json:"model"`
		SessionID        string         `json:"session_id"`
		RawOutput        string         `json:"raw_output"`
		Error            string         `json:"error"`
		StructuredOutput map[string]any `json:"structured_output"`
		Usage            *tokens.Usage  `json:"usage"`
	}
	var first []invocation
	for range 2 {
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, fmt.Sprintf("/api/job/invocations?job_id=%d", job.ID), nil))
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		var response struct {
			Invocations []invocation `json:"invocations"`
		}
		testutil.DecodeJSON(t, w, &response)
		require.Len(t, response.Invocations, 3)
		records := response.Invocations
		assert.Equal("failed", records[0].Status)
		assert.Equal("partial response", records[0].RawOutput)
		assert.Contains(records[0].Error, "connection reset")
		assert.Equal("attempt-1", records[0].SessionID)
		assert.Equal("attempt-2", records[1].SessionID)
		require.NotNil(t, records[0].Usage)
		assert.Equal(int64(10), records[0].Usage.InputTokens)
		require.NotNil(t, records[1].Usage)
		assert.Equal(int64(20), records[1].Usage.InputTokens)
		assert.Equal("succeeded", records[2].Status)
		assert.Equal("invocation-backup", records[2].Agent)
		assert.Equal("backup-model", records[2].Model)
		assert.Equal("pass", records[2].StructuredOutput["verdict"])
		assert.Nil(records[2].Usage, "missing usage is not zero")
		if first == nil {
			first = records
		} else {
			assert.Equal(first, records)
		}
	}
	assert.Equal(2, calls)
	_, err = server.humaRerunJob(t.Context(), &RerunJobInput{Body: RerunJobRequest{JobID: job.ID}})
	require.NoError(t, err)
	claimed, err := db.ClaimJob(testWorkerID)
	require.NoError(t, err)
	require.NotNil(t, claimed)
	server.workerPool.processJob(testWorkerID, claimed)
	w := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, fmt.Sprintf("/api/job/invocations?job_id=%d", job.ID), nil))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var response struct {
		Invocations []invocation `json:"invocations"`
	}
	testutil.DecodeJSON(t, w, &response)
	require.Len(t, response.Invocations, 4)
	assert.Equal(first, response.Invocations[:3])
	assert.Greater(response.Invocations[3].ID, first[2].ID)
}

func TestSuppliedReviewInvocationPersistenceFailureDoesNotRetryAgent(t *testing.T) {
	for _, stage := range []string{"start", "completion"} {
		t.Run(stage, func(t *testing.T) {
			testenv.SetDataDir(t)
			server, db, _ := newTestServer(t)
			repo := testutil.NewGitRepo(t)
			head := repo.CommitFile("file.txt", "target\n", "target")
			calls := 0
			name := "invocation-storage-" + stage
			agent.RegisterForTest(t, &agent.FakeAgent{NameStr: name, ReviewFn: func(context.Context, string, string, string, io.Writer) (string, error) {
				calls++
				return string(testutil.ReviewFixtureJSON("No issues found.")), nil
			}})
			job := enqueueViaHTTP(t, server, EnqueueRequest{RepoPath: repo.Path(), GitRef: head, Agent: name, ReviewPrompt: "Review this.", ReviewSettings: &SuppliedReviewSettings{}})
			statement := `CREATE TRIGGER fail_invocation_write BEFORE INSERT ON review_invocations BEGIN SELECT RAISE(ABORT, 'storage unavailable'); END`
			if stage == "completion" {
				statement = `CREATE TRIGGER fail_invocation_write BEFORE UPDATE ON review_invocations BEGIN SELECT RAISE(ABORT, 'storage unavailable'); END`
			}
			_, err := db.ExecContext(t.Context(), statement)
			require.NoError(t, err)
			claimed, err := db.ClaimJob(testWorkerID)
			require.NoError(t, err)
			require.NotNil(t, claimed)
			server.workerPool.processJob(testWorkerID, claimed)
			stored, err := db.GetJobByID(job.ID)
			require.NoError(t, err)
			assert.Equal(t, storage.JobStatusFailed, stored.Status)
			assert.Contains(t, stored.Error, "storage unavailable")
			assert.Zero(t, stored.RetryCount)
			reports, err := db.ListReviewInvocations(t.Context(), job.ID)
			require.NoError(t, err)
			if stage == "start" {
				assert.Zero(t, calls)
				assert.Empty(t, reports)
			} else {
				assert.Equal(t, 1, calls)
				require.Len(t, reports, 1)
				assert.Equal(t, "unfinished", reports[0].Status)
				assert.Nil(t, reports[0].FinishedAt)
			}
		})
	}
}

func TestSuppliedReviewInvocationCancellationKeepsPartialOutput(t *testing.T) {
	testenv.SetDataDir(t)
	server, db, _ := newTestServer(t)
	repo := testutil.NewGitRepo(t)
	head := repo.CommitFile("file.txt", "target\n", "target")
	entered := make(chan struct{})
	agent.RegisterForTest(t, &agent.FakeAgent{NameStr: "invocation-cancel", ReviewFn: func(ctx context.Context, _, _, _ string, _ io.Writer) (string, error) {
		close(entered)
		<-ctx.Done()
		return "partial canceled response", ctx.Err()
	}})
	job := enqueueViaHTTP(t, server, EnqueueRequest{RepoPath: repo.Path(), GitRef: head, Agent: "invocation-cancel", ReviewPrompt: "Review this.", ReviewSettings: &SuppliedReviewSettings{}})
	claimed, err := db.ClaimJob(testWorkerID)
	require.NoError(t, err)
	require.NotNil(t, claimed)
	done := make(chan struct{})
	go func() { defer close(done); server.workerPool.processJob(testWorkerID, claimed) }()
	t.Cleanup(func() { server.workerPool.CancelJob(job.ID); <-done })
	<-entered
	running, err := db.ListReviewInvocations(t.Context(), job.ID)
	require.NoError(t, err)
	require.Len(t, running, 1)
	assert.Equal(t, "unfinished", running[0].Status)
	assert.Nil(t, running[0].FinishedAt)
	_, err = server.humaCancelJob(t.Context(), &CancelJobInput{Body: CancelJobRequest{JobID: job.ID}})
	require.NoError(t, err)
	<-done
	reports, err := db.ListReviewInvocations(t.Context(), job.ID)
	require.NoError(t, err)
	require.Len(t, reports, 1)
	assert.Equal(t, "canceled", reports[0].Status)
	assert.Equal(t, "partial canceled response", reports[0].RawOutput)
	assert.NotNil(t, reports[0].FinishedAt)
	assert.Nil(t, reports[0].Usage)
}
