package daemon

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testenv"
	"go.kenn.io/roborev/internal/testutil"
	"go.kenn.io/roborev/internal/tokens"
	"go.kenn.io/roborev/pkg/client"
	"go.kenn.io/roborev/pkg/client/generated"
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
	originalCodex, err := agent.Get("codex")
	require.NoError(t, err)
	t.Cleanup(func() { agent.Register(originalCodex) })
	agent.Register(&agent.FakeAgent{NameStr: "codex", ReviewFn: func(_ context.Context, _, _, _ string, out io.Writer) (string, error) {
		calls++
		fmt.Fprintf(out, "{\"type\":\"thread.started\",\"thread_id\":\"attempt-%d\"}\n{\"type\":\"turn.completed\",\"usage\":{\"input_tokens\":%d,\"output_tokens\":3}}\n", calls, calls*10)
		if calls == 1 {
			return "partial response\xff", errors.New("connection reset by peer: diagnostic \xfe")
		}
		return "context exhausted", errors.New("context window exceeded")
	}})
	agent.RegisterForTest(t, &agent.FakeAgent{NameStr: "invocation-backup", ReviewFn: func(context.Context, string, string, string, io.Writer) (string, error) {
		return string(testutil.ReviewFixtureJSON("No issues found.")), nil
	}})
	job := enqueueViaHTTP(t, server, EnqueueRequest{RepoPath: repo.Path(), GitRef: head, Agent: "codex", Model: "primary-model", ReviewPrompt: "Review the change.", ReviewSettings: &SuppliedReviewSettings{BackupAgent: "invocation-backup", BackupModel: "backup-model"}})
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
		assert.Equal("partial response\ufffd", records[0].RawOutput)
		assert.Equal("connection reset by peer: diagnostic \ufffd", records[0].Error)
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
	httpServer := httptest.NewServer(server.httpServer.Handler)
	defer httpServer.Close()
	api, err := client.NewWithHTTPClient(httpServer.URL, httpServer.Client())
	require.NoError(t, err)
	decoded, err := api.GetJobInvocations(t.Context(), &generated.GetJobInvocationsRequestOptions{
		Query: &generated.GetJobInvocationsQuery{JobID: job.ID},
	})
	require.NoError(t, err)
	require.Len(t, decoded.Invocations, 3)
	structured, err := json.Marshal(decoded.Invocations[2].StructuredOutput)
	require.NoError(t, err)
	assert.JSONEq(string(testutil.ReviewFixtureJSON("No issues found.")), string(structured))
	assert.Nil(decoded.Invocations[2].Usage, "the generated client must preserve unavailable usage")
	require.NotNil(t, decoded.Invocations[0].Usage.InputTokens)
	assert.Equal(int64(10), *decoded.Invocations[0].Usage.InputTokens)
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

func TestSuppliedReviewInvocationCapturesNativeAdapterFailures(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture uses a POSIX executable")
	}
	for _, tc := range []struct {
		name, command, stream, exit, want, session string
	}{
		{"codex", "codex", `{"type":"item.completed","item":{"id":"item_1","type":"agent_message","text":"incomplete review response"}}`, "0", "incomplete review response", ""},
		{"claude-code", "claude", `{"type":"result","is_error":true,"result":"provider unavailable, partial review"}`, "1", "provider unavailable, partial review", ""},
		{"copilot", "copilot", `{"type":"assistant.message","data":{"messageId":"msg-1","content":"Partial review before failure","toolRequests":[]}}` + "\n" +
			`{"type":"result","exitCode":1,"sessionId":"failed-session"}`, "1", "Partial review before failure", "failed-session"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testenv.SetDataDir(t)
			script := "#!/bin/sh\nfor arg in \"$@\"; do if [ \"$arg\" = --help ]; then echo '--sandbox --effort --output-format'; exit 0; fi; done\ncat >/dev/null\ncat <<'OUTPUT'\n" + tc.stream + "\nOUTPUT\nexit " + tc.exit + "\n"
			t.Cleanup(testutil.MockBinaryInPath(t, tc.command, script))
			server, db, _ := newTestServer(t)
			repo := testutil.NewGitRepo(t)
			head := repo.CommitFile("file.txt", "target\n", "target")
			job := enqueueViaHTTP(t, server, EnqueueRequest{RepoPath: repo.Path(), GitRef: head, Agent: tc.name, ReviewPrompt: "Review this.", ReviewSettings: &SuppliedReviewSettings{}})
			claimed, err := db.ClaimJob(testWorkerID)
			require.NoError(t, err)
			require.NotNil(t, claimed)
			server.workerPool.processJob(testWorkerID, claimed)
			reports, err := db.ListReviewInvocations(t.Context(), job.ID)
			require.NoError(t, err)
			require.Len(t, reports, 1)
			assert.Equal(t, "failed", reports[0].Status)
			assert.Contains(t, reports[0].RawOutput, tc.want)
			assert.Equal(t, tc.session, reports[0].SessionID)
			assert.NotEmpty(t, reports[0].Error)
		})
	}
}

func TestSuppliedReviewInvocationUsageDoesNotDependOnJobLog(t *testing.T) {
	if runtime.GOOS == "windows" || os.Getuid() == 0 {
		t.Skip("fixture needs Unix file permissions enforced")
	}
	for _, recoverLog := range []bool{false, true} {
		t.Run(fmt.Sprintf("recover=%t", recoverLog), func(t *testing.T) {
			testenv.SetDataDir(t)
			previousInterval := jobLogOpenRetryInterval
			jobLogOpenRetryInterval = time.Hour
			t.Cleanup(func() { jobLogOpenRetryInterval = previousInterval })
			server, db, _ := newTestServer(t)
			server.workerPool.retryBackoff = 0
			server.workerPool.tokenUsageFetcher = func(context.Context, string) (*tokens.Usage, error) { return nil, tokens.ErrUsageProviderUnavailable }
			repo := testutil.NewGitRepo(t)
			head := repo.CommitFile("file.txt", "target\n", "target")
			calls := 0
			var logPath string
			originalCodex, err := agent.Get("codex")
			require.NoError(t, err)
			t.Cleanup(func() { agent.Register(originalCodex) })
			agent.Register(&agent.FakeAgent{NameStr: "codex", ReviewFn: func(_ context.Context, _, _, _ string, out io.Writer) (string, error) {
				calls++
				fmt.Fprintf(out, "{\"type\":\"turn.completed\",\"usage\":{\"input_tokens\":%d,\"output_tokens\":3}}\n", calls*10)
				if calls == 1 {
					return "partial response", errors.New("connection reset by peer")
				}
				if recoverLog {
					require.NoError(t, os.Chmod(logPath, 0o600))
				}
				return string(testutil.ReviewFixtureJSON("No issues found.")), nil
			}})
			job := enqueueViaHTTP(t, server, EnqueueRequest{RepoPath: repo.Path(), GitRef: head, Agent: "codex", ReviewPrompt: "Review this.", ReviewSettings: &SuppliedReviewSettings{}})
			logPath = JobLogPath(job.ID)
			for attempt := range 2 {
				claimed, err := db.ClaimJob(testWorkerID)
				require.NoError(t, err)
				require.NotNil(t, claimed)
				server.workerPool.processJob(testWorkerID, claimed)
				if attempt == 0 {
					require.NoError(t, os.Chmod(logPath, 0o400))
				}
			}
			reports, err := db.ListReviewInvocations(t.Context(), job.ID)
			require.NoError(t, err)
			require.Len(t, reports, 2)
			require.NotNil(t, reports[0].Usage)
			require.NotNil(t, reports[1].Usage)
			assert.Equal(t, int64(10), reports[0].Usage.InputTokens)
			assert.Equal(t, int64(20), reports[1].Usage.InputTokens)
			assert.Empty(t, reports[1].UsageError)
			data, err := os.ReadFile(logPath)
			require.NoError(t, err)
			assert.Equal(t, recoverLog, strings.Contains(string(data), `"input_tokens":20`), "only the recovered log receives the second invocation's usage")
		})
	}
}

func TestSuppliedReviewInvocationKeepsAdapterEvidence(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture uses a POSIX executable")
	}
	// A review can legitimately discuss provider event formats. Its rendered
	// examples must not replace the invocation's measured usage.
	quoted := "No issues found. Example events:\n```json\n" +
		`{"type":"turn.completed","usage":{"input_tokens":999,"output_tokens":999}}` + "\n" +
		`{"type":"result","usage":{"input_tokens":999,"output_tokens":999}}` + "\n```"
	document := string(testutil.ReviewFixtureJSON(quoted))
	encoded, err := json.Marshal(document)
	require.NoError(t, err)
	narration := quoted + "\n" + document
	encodedNarration, err := json.Marshal(narration)
	require.NoError(t, err)
	for _, tc := range []struct {
		name, command, stream string
		streamUsage           bool
	}{
		{"codex", "codex", `{"type":"thread.started","thread_id":"invocation-session"}` + "\n" +
			`{"type":"item.completed","item":{"id":"item_1","type":"agent_message","text":` + string(encoded) + `}}` + "\n" +
			`{"type":"turn.completed","usage":{"input_tokens":10,"output_tokens":3}}`, true},
		{"claude-code", "claude", `{"type":"result","session_id":"invocation-session","result":` + string(encoded) + `,"usage":{"input_tokens":10,"output_tokens":3}}`, true},
		{"copilot", "copilot", `{"type":"assistant.message","data":{"messageId":"msg-1","content":` + string(encodedNarration) + `,"toolRequests":[]}}` + "\n" +
			`{"type":"result","exitCode":0,"sessionId":"invocation-session"}`, false},
		{"muse", "muse", narration, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testenv.SetDataDir(t)
			script := "#!/bin/sh\nfor arg in \"$@\"; do if [ \"$arg\" = --help ]; then echo '--sandbox --effort --allow-all-tools --stream --output-format --disable-builtin-mcps'; exit 0; fi; done\ncat >/dev/null\ncat <<'OUTPUT'\n" + tc.stream + "\nOUTPUT\n"
			t.Cleanup(testutil.MockBinaryInPath(t, tc.command, script))
			server, db, _ := newTestServer(t)
			var usageSessions []string
			server.workerPool.tokenUsageFetcher = func(_ context.Context, sessionID string) (*tokens.Usage, error) {
				usageSessions = append(usageSessions, sessionID)
				return &tokens.Usage{InputTokens: 10, OutputTokens: 3}, nil
			}
			repo := testutil.NewGitRepo(t)
			head := repo.CommitFile("file.txt", "target\n", "target")
			job := enqueueViaHTTP(t, server, EnqueueRequest{RepoPath: repo.Path(), GitRef: head, Agent: tc.name, ReviewPrompt: "Review this.", ReviewSettings: &SuppliedReviewSettings{}})
			claimed, err := db.ClaimJob(testWorkerID)
			require.NoError(t, err)
			require.NotNil(t, claimed)
			server.workerPool.processJob(testWorkerID, claimed)
			stored, err := db.GetJobByID(job.ID)
			require.NoError(t, err)
			require.Equal(t, storage.JobStatusDone, stored.Status, stored.Error)
			if tc.name != "muse" {
				assert.Equal(t, "invocation-session", stored.SessionID)
				assert.Contains(t, usageSessions, "invocation-session")
			}
			reports, err := db.ListReviewInvocations(t.Context(), job.ID)
			require.NoError(t, err)
			require.Len(t, reports, 1)
			assert.Equal(t, stored.SessionID, reports[0].SessionID)
			assert.Contains(t, reports[0].Output, quoted)
			if tc.streamUsage {
				require.NotNil(t, reports[0].Usage)
				assert.Equal(t, int64(10), reports[0].Usage.InputTokens)
				assert.Equal(t, int64(3), reports[0].Usage.OutputTokens)
			} else {
				assert.Nil(t, reports[0].Usage, "session lookups and quoted examples are not invocation stream usage")
			}
			log, err := os.ReadFile(JobLogPath(job.ID))
			require.NoError(t, err)
			assert.Contains(t, string(log), quoted, "the rendered review remains available in the job log")
		})
	}
}
