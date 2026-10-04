package daemon

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
	"go.kenn.io/roborev/internal/tokens"
	roborevclient "go.kenn.io/roborev/pkg/client"
	"go.kenn.io/roborev/pkg/client/generated"
)

func TestBackfillTokensUsesCodexJobLogWhenAgentsviewMissing(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("ROBOREV_DATA_DIR", dataDir)
	t.Setenv("PATH", t.TempDir())

	server, db, _ := newTestServer(t)

	repo, err := db.GetOrCreateRepo(filepath.Join(t.TempDir(), "repo"))
	require.NoError(t, err)
	commit, err := db.GetOrCreateCommit(repo.ID, "abc123", "Author", "Subject", time.Now())
	require.NoError(t, err)
	job, err := db.EnqueueJob(storage.EnqueueOpts{
		RepoID:    repo.ID,
		CommitID:  commit.ID,
		GitRef:    "abc123",
		Agent:     "codex",
		SessionID: "thread-123",
	})
	require.NoError(t, err)
	claimed, err := db.ClaimJob("worker-1")
	require.NoError(t, err)
	require.NotNil(t, claimed)
	require.Equal(t, job.ID, claimed.ID)
	require.NoError(t, testutil.CompleteReviewFixture(db, job.ID, "codex", "prompt", "No issues found."))

	logPath := JobLogPath(job.ID)
	require.NoError(t, os.MkdirAll(filepath.Dir(logPath), 0o700))
	require.NoError(t, os.WriteFile(logPath, []byte(
		`{"type":"thread.started","thread_id":"thread-123"}`+"\n"+
			`{"type":"turn.completed","usage":{"input_tokens":79150,`+
			`"cached_input_tokens":2560,"output_tokens":3389}}`+"\n",
	), 0o600))

	// Filesystem write timestamps can lag the nanosecond-precision job start.
	// Give this current-attempt fixture an explicit, whole-second timestamp.
	require.NotNil(t, claimed.StartedAt)
	logTime := claimed.StartedAt.Add(time.Second).Truncate(time.Second)
	require.NoError(t, os.Chtimes(logPath, logTime, logTime))

	dryResponse := maintenanceRequest(t, server, "/api/maintenance/tokens/backfill", map[string]any{"dry_run": true})
	require.Equal(t, http.StatusOK, dryResponse.Code, dryResponse.Body.String())
	var report ScanTokenUsageReport
	require.NoError(t, json.Unmarshal(dryResponse.Body.Bytes(), &report))
	assert.Equal(t, 1, report.Updated)
	require.Len(t, report.Jobs, 1)
	untouched, err := db.GetJobByID(job.ID)
	require.NoError(t, err)
	assert.Empty(t, untouched.TokenUsage)

	response := maintenanceRequest(t, server, "/api/maintenance/tokens/backfill", map[string]any{"dry_run": false})
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())

	updated, err := db.GetJobByID(job.ID)
	require.NoError(t, err)
	usage := tokens.ParseJSON(updated.TokenUsage)
	require.NotNil(t, usage)
	assert.Equal(t, int64(79150), usage.InputTokens)
	assert.Equal(t, int64(2560), usage.CachedInputTokens)
	assert.Equal(t, int64(3389), usage.OutputTokens)
	assert.Equal(t, "job_log_turn_completed", usage.UsageSource)
	assert.Equal(t, "thread-123", usage.ThreadID)
}

func TestBackfillTokensUsesCodexJobLogsWithoutAgentsviewEligibleSession(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("ROBOREV_DATA_DIR", dataDir)
	t.Setenv("PATH", t.TempDir())

	server, db, _ := newTestServer(t)

	repo, err := db.GetOrCreateRepo(filepath.Join(t.TempDir(), "repo"))
	require.NoError(t, err)
	commit, err := db.GetOrCreateCommit(repo.ID, "abc123", "Author", "Subject", time.Now())
	require.NoError(t, err)

	missingSession := enqueueCompleteJob(t, db, repo.ID, commit.ID, "")
	sharedSessionA := enqueueCompleteJob(t, db, repo.ID, commit.ID, "shared-session")
	sharedSessionB := enqueueCompleteJob(t, db, repo.ID, commit.ID, "shared-session")

	writeCodexUsageLog(t, missingSession, "missing-session-thread", 1000, 100, 200)
	writeCodexUsageLog(t, sharedSessionA, "shared-session", 2000, 200, 300)
	writeCodexUsageLog(t, sharedSessionB, "shared-session", 3000, 300, 400)

	response := maintenanceRequest(t, server, "/api/maintenance/tokens/backfill", map[string]any{"dry_run": false})
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())

	assertJobUsage := func(jobID int64, threadID string, input, cached, output int64) {
		updated, err := db.GetJobByID(jobID)
		require.NoError(t, err)
		usage := tokens.ParseJSON(updated.TokenUsage)
		require.NotNil(t, usage)
		assert.Equal(t, input, usage.InputTokens)
		assert.Equal(t, cached, usage.CachedInputTokens)
		assert.Equal(t, output, usage.OutputTokens)
		assert.Equal(t, threadID, usage.ThreadID)
		assert.Equal(t, "job_log_turn_completed", usage.UsageSource)
	}
	assertJobUsage(missingSession.ID, "missing-session-thread", 1000, 100, 200)
	assertJobUsage(sharedSessionA.ID, "shared-session", 2000, 200, 300)
	assertJobUsage(sharedSessionB.ID, "shared-session", 3000, 300, 400)

	updatedMissingSession, err := db.GetJobByID(missingSession.ID)
	require.NoError(t, err)
	assert.Equal(t, "missing-session-thread", updatedMissingSession.SessionID)
}

func TestBackfillTokensRejectsLogFromPriorCanceledAttempt(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("ROBOREV_DATA_DIR", dataDir)
	t.Setenv("PATH", t.TempDir())

	server, db, _ := newTestServer(t)

	repo, err := db.GetOrCreateRepo(filepath.Join(t.TempDir(), "repo"))
	require.NoError(t, err)
	commit, err := db.GetOrCreateCommit(
		repo.ID, "abc123", "Author", "Subject", time.Now(),
	)
	require.NoError(t, err)
	job := enqueueCompleteJob(t, db, repo.ID, commit.ID, "prior-session")
	writeCodexUsageLog(t, job, "prior-session", 1000, 100, 200)

	require.NoError(t, db.ReenqueueJob(job.ID, storage.ReenqueueOpts{}))
	claimed, err := db.ClaimJob("worker-2")
	require.NoError(t, err)
	require.NotNil(t, claimed)
	require.Equal(t, job.ID, claimed.ID)
	require.NotNil(t, claimed.StartedAt)
	require.NoError(t, db.CancelJob(job.ID))

	logPath := JobLogPath(job.ID)
	staleTime := claimed.StartedAt.Add(-time.Minute)
	require.NoError(t, os.Chtimes(logPath, staleTime, staleTime))

	response := maintenanceRequest(t, server, "/api/maintenance/tokens/backfill", map[string]any{"dry_run": false})
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())

	updated, err := db.GetJobByID(job.ID)
	require.NoError(t, err)
	assert.Empty(t, updated.TokenUsage)
	assert.Empty(t, updated.SessionID)
}

func TestBackfillTokensAggregatesPlanPhaseSessions(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	server, db, dir := newTestServer(t)
	repo, err := db.GetOrCreateRepo(filepath.Join(dir, "repo"))
	require.NoError(t, err)
	commit, err := db.GetOrCreateCommit(repo.ID, "plan-usage", "test", "plan usage", time.Now())
	require.NoError(t, err)
	job := enqueuePlanUsageCandidate(t, db, repo.ID, commit.ID)

	requested, usageServer := configurePlanUsageEndpoint(t, server, map[string]string{
		"planner-session": `{"session_id":"planner-session","agent":"codex",` +
			`"has_token_data":true,"input_tokens":100,"total_output_tokens":15,` +
			`"has_cost":true,"cost_usd":0.13}`,
		"implementation-session": `{"session_id":"implementation-session","agent":"codex",` +
			`"has_token_data":true,"input_tokens":200,"total_output_tokens":25,` +
			`"has_cost":true,"cost_usd":0.29}`,
	})
	t.Cleanup(usageServer.Close)

	response := maintenanceRequest(t, server, "/api/maintenance/tokens/backfill", map[string]any{"dry_run": false})
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var report ScanTokenUsageReport
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &report))
	assert.Equal(t, 1, report.Updated)
	assert.ElementsMatch(t, []string{"planner-session", "implementation-session"}, *requested)

	updated, err := db.GetJobByID(job.ID)
	require.NoError(t, err)
	usage := tokens.ParseJSON(updated.TokenUsage)
	require.NotNil(t, usage)
	assert.Equal(t, int64(300), usage.InputTokens)
	assert.Equal(t, int64(40), usage.OutputTokens)
	assert.Equal(t, []string{"planner-session", "implementation-session"}, usage.ProviderSessionIDs)
	assert.Equal(t, 2, usage.ExpectedProviderSessions)
	assert.True(t, usage.HasCost)
	assert.InDelta(t, 0.42, usage.CostUSD, 1e-9)
}

func TestBackfillTokensPreservesStoredCountsWhenPlanPhaseHasNoCounts(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	server, db, dir := newTestServer(t)
	repo, err := db.GetOrCreateRepo(filepath.Join(dir, "repo"))
	require.NoError(t, err)
	commit, err := db.GetOrCreateCommit(repo.ID, "plan-usage", "test", "plan usage", time.Now())
	require.NoError(t, err)
	job := enqueuePlanUsageCandidate(t, db, repo.ID, commit.ID)
	require.NoError(t, db.SaveJobTokenUsage(job.ID, "implementation-session",
		`{"input_tokens":300,"total_output_tokens":40,`+
			`"provider_session_ids":["planner-session","implementation-session"],`+
			`"expected_provider_sessions":2}`))

	requested, usageServer := configurePlanUsageEndpoint(t, server, map[string]string{
		"planner-session": `{"session_id":"planner-session","agent":"codex",` +
			`"has_token_data":false,"has_cost":true,"cost_usd":0.13}`,
		"implementation-session": `{"session_id":"implementation-session","agent":"codex",` +
			`"has_token_data":true,"input_tokens":200,"total_output_tokens":25,` +
			`"has_cost":true,"cost_usd":0.29}`,
	})
	t.Cleanup(usageServer.Close)

	response := maintenanceRequest(t, server, "/api/maintenance/tokens/backfill", map[string]any{"dry_run": false})
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var report ScanTokenUsageReport
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &report))
	assert.Equal(t, 1, report.Updated)
	assert.ElementsMatch(t, []string{"planner-session", "implementation-session"}, *requested)

	updated, err := db.GetJobByID(job.ID)
	require.NoError(t, err)
	usage := tokens.ParseJSON(updated.TokenUsage)
	require.NotNil(t, usage)
	assert.Equal(t, int64(300), usage.InputTokens)
	assert.Equal(t, int64(40), usage.OutputTokens)
	assert.True(t, usage.HasCost)
	assert.InDelta(t, 0.42, usage.CostUSD, 1e-9)
}

func TestBackfillTokensIgnoresPartialPlanLogCountsWhenFetchingCost(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	server, db, dir := newTestServer(t)
	repo, err := db.GetOrCreateRepo(filepath.Join(dir, "repo"))
	require.NoError(t, err)
	commit, err := db.GetOrCreateCommit(repo.ID, "plan-log-usage", "test", "plan log usage", time.Now())
	require.NoError(t, err)
	job := enqueuePlanUsageCandidate(t, db, repo.ID, commit.ID)
	writeCodexUsageLog(t, job, "implementation-session", 200, 0, 25)

	requested, usageServer := configurePlanUsageEndpoint(t, server, map[string]string{
		"planner-session": `{"session_id":"planner-session","agent":"codex",` +
			`"has_token_data":false,"has_cost":true,"cost_usd":0.13}`,
		"implementation-session": `{"session_id":"implementation-session","agent":"codex",` +
			`"has_token_data":true,"input_tokens":200,"total_output_tokens":25,` +
			`"has_cost":true,"cost_usd":0.29}`,
	})
	t.Cleanup(usageServer.Close)

	response := maintenanceRequest(t, server, "/api/maintenance/tokens/backfill", map[string]any{"dry_run": false})
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var report ScanTokenUsageReport
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &report))
	assert.Equal(t, 1, report.Updated)
	assert.ElementsMatch(t, []string{"planner-session", "implementation-session"}, *requested)

	updated, err := db.GetJobByID(job.ID)
	require.NoError(t, err)
	usage := tokens.ParseJSON(updated.TokenUsage)
	require.NotNil(t, usage)
	assert.Equal(t, int64(300), usage.InputTokens)
	assert.Equal(t, int64(40), usage.OutputTokens)
	assert.Equal(t, 2, usage.ExpectedProviderSessions)
	assert.True(t, usage.HasCost)
	assert.InDelta(t, 0.42, usage.CostUSD, 1e-9)
}

func TestBackfillTokensDoesNotPriceIncompletePlanPhaseSessions(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	server, db, dir := newTestServer(t)
	repo, err := db.GetOrCreateRepo(filepath.Join(dir, "repo"))
	require.NoError(t, err)
	commit, err := db.GetOrCreateCommit(repo.ID, "plan-usage", "test", "plan usage", time.Now())
	require.NoError(t, err)
	job := enqueuePlanUsageCandidate(t, db, repo.ID, commit.ID)
	require.NoError(t, db.SaveJobTokenUsage(job.ID, "implementation-session",
		`{"input_tokens":300,"total_output_tokens":40,`+
			`"provider_session_ids":["implementation-session","planner-session"],`+
			`"expected_provider_sessions":2}`))

	requested, usageServer := configurePlanUsageEndpoint(t, server, map[string]string{
		"implementation-session": `{"session_id":"implementation-session","agent":"codex",` +
			`"has_token_data":true,"input_tokens":200,"total_output_tokens":25,` +
			`"has_cost":true,"cost_usd":0.29}`,
	})
	t.Cleanup(usageServer.Close)

	response := maintenanceRequest(t, server, "/api/maintenance/tokens/backfill", map[string]any{"dry_run": false})
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var report ScanTokenUsageReport
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &report))
	assert.Equal(t, 0, report.Updated)
	assert.Equal(t, 1, report.Skipped)
	assert.ElementsMatch(t, []string{"planner-session", "implementation-session"}, *requested)

	unchanged, err := db.GetJobByID(job.ID)
	require.NoError(t, err)
	usage := tokens.ParseJSON(unchanged.TokenUsage)
	require.NotNil(t, usage)
	assert.Equal(t, int64(300), usage.InputTokens)
	assert.Equal(t, int64(40), usage.OutputTokens)
	assert.False(t, usage.HasCost)
	assert.Zero(t, usage.CostUSD)
}

func enqueuePlanUsageCandidate(t *testing.T, db *storage.DB, repoID, commitID int64) *storage.ReviewJob {
	t.Helper()
	job, err := db.EnqueueJob(storage.EnqueueOpts{
		RepoID: repoID, CommitID: commitID, GitRef: "plan-usage",
		Agent: "codex",
	})
	require.NoError(t, err)
	claimed, err := db.ClaimJob("plan-usage-worker")
	require.NoError(t, err)
	require.Equal(t, job.ID, claimed.ID)
	require.NoError(t, db.MarkJobAgentInvoked(job.ID, "plan-usage-worker", "codex review"))
	require.NoError(t, db.SaveJobSessionID(job.ID, "plan-usage-worker", "implementation-session"))
	require.NoError(t, testutil.CompleteReviewFixture(
		db, job.ID, "codex", "prompt", "No issues found.",
	))
	require.NoError(t, db.SaveJobTokenUsage(job.ID, "implementation-session",
		`{"input_tokens":300,"total_output_tokens":40,`+
			`"provider_session_ids":["planner-session","implementation-session"],`+
			`"expected_provider_sessions":2}`))
	updated, err := db.GetJobByID(job.ID)
	require.NoError(t, err)
	return updated
}

func configurePlanUsageEndpoint(
	t *testing.T, server *Server, responses map[string]string,
) (*[]string, *httptest.Server) {
	t.Helper()
	var requested []string
	usageServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sessionID := strings.TrimPrefix(r.URL.Path, "/usage/")
		requested = append(requested, sessionID)
		body, ok := responses[sessionID]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, body)
	}))
	cfg := config.DefaultConfig()
	cfg.Cost.Endpoint = usageServer.URL + "/usage/{session_id}"
	server.configWatcher.cfgMu.Lock()
	server.configWatcher.cfg = cfg
	server.configWatcher.cfgMu.Unlock()
	return &requested, usageServer
}

func enqueueCompleteJob(
	t *testing.T, db *storage.DB, repoID, commitID int64, sessionID string,
) *storage.ReviewJob {
	t.Helper()
	job, err := db.EnqueueJob(storage.EnqueueOpts{
		RepoID:    repoID,
		CommitID:  commitID,
		GitRef:    "abc123",
		Agent:     "codex",
		SessionID: sessionID,
	})
	require.NoError(t, err)
	claimed, err := db.ClaimJob("worker-1")
	require.NoError(t, err)
	require.NotNil(t, claimed)
	require.Equal(t, job.ID, claimed.ID)
	require.NoError(t, testutil.CompleteReviewFixture(db, job.ID, "codex", "prompt", "No issues found."))
	return claimed
}

func writeCodexUsageLog(
	t *testing.T, job *storage.ReviewJob, threadID string, input, cached, output int64,
) {
	t.Helper()
	logPath := JobLogPath(job.ID)
	require.NoError(t, os.MkdirAll(filepath.Dir(logPath), 0o700))
	require.NoError(t, os.WriteFile(logPath, []byte(
		`{"type":"thread.started","thread_id":"`+threadID+`"}`+"\n"+
			`{"type":"turn.completed","usage":{"input_tokens":`+
			fmt.Sprintf("%d", input)+`,"cached_input_tokens":`+
			fmt.Sprintf("%d", cached)+`,"output_tokens":`+
			fmt.Sprintf("%d", output)+`}}`+"\n",
	), 0o600))
	require.NotNil(t, job.StartedAt)
	logTime := job.StartedAt.Add(time.Second).Truncate(time.Second)
	require.NoError(t, os.Chtimes(logPath, logTime, logTime))
}

func maintenanceRequest(t *testing.T, server *Server, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(response, req)
	return response
}

func TestMaintenanceLegacyReviewLifecycle(t *testing.T) {
	server, db, dir := newTestServer(t)
	repo, err := db.GetOrCreateRepo(filepath.Join(dir, "repo"))
	require.NoError(t, err)
	convertible := testutil.CreateCompletedReview(t, db, repo.ID, "convertible-head", "test", "No issues found.")
	prose := testutil.CreateCompletedReview(t, db, repo.ID, "prose-head", "test", "No issues found.")
	for jobID, markdown := range map[int64]string{
		convertible.ID: "## Review Findings\n\n- **Severity**: Medium\n- **Location**: store/save.go:88\n" +
			"- **Problem**: The write is not atomic.\n- **Fix**: Rename a temporary file.\n\n## Summary\n\nThe change adds a save routine.",
		prose.ID: "The save routine looks risky. Consider a rename.",
	} {
		_, err := db.Exec(`INSERT INTO legacy_reviews (id, job_id, agent, prompt, output, created_at, closed, verdict_bool, uuid, updated_at, migration_error)
 SELECT id, job_id, agent, prompt, ?, created_at, closed, 0, uuid, updated_at, 'No valid review JSON document; AI conversion required'
 FROM reviews WHERE job_id = ?`, markdown, jobID)
		require.NoError(t, err)
		_, err = db.Exec(`DELETE FROM reviews WHERE job_id = ?`, jobID)
		require.NoError(t, err)
	}

	// Exporting a synthesis must retain the completed member's document.
	source := testutil.CreateCompletedReview(t, db, repo.ID, "source-head", "test", "Source assessment.")
	runID := uuid.New()
	_, err = db.Exec("UPDATE review_jobs SET job_type = 'synthesis', panel_role = 'synthesis', panel_run_uuid = ? WHERE id = ?", runID, prose.ID)
	require.NoError(t, err)
	_, err = db.Exec("UPDATE review_jobs SET panel_role = 'member', panel_run_uuid = ?, panel_member_index = 0 WHERE id = ?", runID, source.ID)
	require.NoError(t, err)
	sourceReview, err := db.GetReviewByJobID(source.ID)
	require.NoError(t, err)

	response := maintenanceRequest(t, server, "/api/maintenance/legacy/convert", map[string]any{"dry_run": true})
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var report storage.LegacyConversionReport
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &report))
	assert.Equal(t, 2, report.Unresolved)
	assert.Equal(t, 1, report.Converted)
	assert.Equal(t, map[string]int{"unrecognized_format": 1}, report.Refused)
	_, err = db.GetReviewByJobID(convertible.ID)
	require.ErrorIs(t, err, storage.ErrLegacyReviewMigration)

	response = maintenanceRequest(t, server, "/api/maintenance/legacy/export", map[string]any{"db": filepath.Join(dir, "test.db")})
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var exported ExportLegacyReviewsOutput
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &exported.Body))
	require.Len(t, exported.Body.SQLiteRecords, 2)

	response = maintenanceRequest(t, server, "/api/maintenance/legacy/convert", map[string]any{"dry_run": false})
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &report))
	assert.Equal(t, 1, report.Converted)
	review, err := db.GetReviewByJobID(convertible.ID)
	require.NoError(t, err)
	assert.Equal(t, "The change adds a save routine.", review.StructuredOutput["summary"])
	assert.Equal(t, storage.VerdictFail, review.Verdict())

	response = maintenanceRequest(t, server, "/api/maintenance/legacy/export", map[string]any{})
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &exported.Body))
	require.Len(t, exported.Body.SQLiteRecords, 1)
	record := exported.Body.SQLiteRecords[0]
	assert.Equal(t, prose.ID, record.JobID)
	assert.Equal(t, "The save routine looks risky. Consider a rename.", record.Output)

	httpServer := httptest.NewServer(server.httpServer.Handler)
	t.Cleanup(httpServer.Close)
	api, err := roborevclient.NewWithHTTPClient(httpServer.URL, httpServer.Client())
	require.NoError(t, err)
	typedExport, err := api.ExportLegacyReviews(t.Context(), &generated.ExportLegacyReviewsRequestOptions{Body: &generated.LegacyMaintenanceTarget{}})
	require.NoError(t, err)
	require.Len(t, typedExport.SqliteRecords, 1)
	require.Len(t, typedExport.SqliteRecords[0].Sources, 1)
	exportedSource, err := json.Marshal(typedExport.SqliteRecords[0].Sources[0].Document)
	require.NoError(t, err)
	storedSource, err := json.Marshal(sourceReview.StructuredOutput)
	require.NoError(t, err)
	assert.JSONEq(t, string(storedSource), string(exportedSource))

	_, err = api.ImportLegacyReview(t.Context(), &generated.ImportLegacyReviewRequestOptions{
		Body: &generated.ImportLegacyReviewInputBody{
			ID: new(record.ID), Document: jsontext.Value(`{"schema_version":2,"summary":"Converted review.","verdict":"fail","findings":[{"severity":"medium","problem":"The save routine loses data.","fix":"Use an atomic rename.","location":null,"sources":[1]}]}`),
		},
	})
	require.NoError(t, err)
	restored, err := db.GetReviewByJobID(prose.ID)
	require.NoError(t, err)
	assert.Equal(t, "Converted review.", restored.StructuredOutput["summary"])
	var original string
	require.NoError(t, db.QueryRow("SELECT output FROM legacy_reviews WHERE archive_id = ?", record.ID).Scan(&original))
	assert.Equal(t, record.Output, original)

	response = maintenanceRequest(t, server, "/api/maintenance/legacy/convert", map[string]any{"dry_run": false})
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &report))
	assert.Equal(t, 0, report.Unresolved)
	assert.Equal(t, 0, report.Converted)
}

func TestMaintenanceRejectsOtherDatabase(t *testing.T) {
	server, _, _ := newTestServer(t)
	response := maintenanceRequest(t, server, "/api/maintenance/legacy/export", map[string]any{"db": filepath.Join(t.TempDir(), "other.db")})
	assert.Equal(t, http.StatusBadRequest, response.Code)
	assert.Contains(t, response.Body.String(), "use --server")
}

func TestMaintenanceBackfillVerdicts(t *testing.T) {
	server, db, dir := newTestServer(t)
	repo, err := db.GetOrCreateRepo(filepath.Join(dir, "repo"))
	require.NoError(t, err)
	job := testutil.CreateCompletedReview(t, db, repo.ID, "test-head", "test", "No issues found.")
	_, err = db.Exec("UPDATE reviews SET verdict_bool = NULL WHERE job_id = ?", job.ID)
	require.NoError(t, err)
	response := maintenanceRequest(t, server, "/api/maintenance/verdicts/backfill", nil)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var result BackfillVerdictsOutput
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &result.Body))
	assert.Equal(t, 1, result.Body.Count)
	var verdict bool
	require.NoError(t, db.QueryRow("SELECT verdict_bool FROM reviews WHERE job_id = ?", job.ID).Scan(&verdict))
	assert.True(t, verdict)
}

func TestMaintenanceCleanJobLogs(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	server, _, _ := newTestServer(t)
	logPath := JobLogPath(7)
	require.NoError(t, os.MkdirAll(filepath.Dir(logPath), 0o700))
	require.NoError(t, os.WriteFile(logPath, []byte("old log"), 0o600))
	oldTime := time.Now().Add(-8 * 24 * time.Hour)
	require.NoError(t, os.Chtimes(logPath, oldTime, oldTime))
	response := maintenanceRequest(t, server, "/api/logs/clean", map[string]any{"days": 7})
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var result CleanJobLogsOutput
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &result.Body))
	assert.Equal(t, 1, result.Body.Removed)
	_, err := os.Stat(logPath)
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestSyncStatusReportsDaemonPendingWork(t *testing.T) {
	server, db, dir := newTestServer(t)
	cfg := config.DefaultConfig()
	cfg.Sync.Enabled = true
	cfg.Sync.Interval = "7m"
	cfg.Sync.MachineName = "test-machine"
	server.configWatcher.cfgMu.Lock()
	server.configWatcher.cfg = cfg
	server.configWatcher.cfgMu.Unlock()
	server.syncWorker = storage.NewSyncWorker(db, cfg.Sync)

	repo, err := db.GetOrCreateRepo(filepath.Join(dir, "repo"))
	require.NoError(t, err)
	testutil.CreateCompletedReview(t, db, repo.ID, "pending-job-head", "test", "No issues found.")
	synced := testutil.CreateCompletedReview(t, db, repo.ID, "synced-job-head", "test", "No issues found.")
	_, err = db.Exec("UPDATE review_jobs SET synced_at = updated_at WHERE id = ?", synced.ID)
	require.NoError(t, err)
	_, err = db.AddCommentToJob(synced.ID, "tester", "First pending comment")
	require.NoError(t, err)
	_, err = db.AddCommentToJob(synced.ID, "tester", "Second pending comment")
	require.NoError(t, err)
	machineID, err := db.GetMachineID()
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "/api/sync/status", nil)
	response := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(response, req)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var result SyncStatusOutput
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &result.Body))
	assert := assert.New(t)
	assert.True(result.Body.Enabled)
	assert.False(result.Body.Connected)
	assert.Equal("not running", result.Body.Message)
	assert.Equal("7m", result.Body.Interval)
	assert.Equal("test-machine", result.Body.MachineName)
	require.NotNil(t, result.Body.MachineID)
	assert.Equal(machineID, *result.Body.MachineID)
	assert.Equal(SyncPendingCounts{Jobs: 1, Reviews: 1, Comments: 2}, result.Body.PendingPush)
	assert.Equal(1000, result.Body.PendingLimit)
	assert.False(result.Body.PendingIncomplete)
	assert.Equal([]string{"sync.enabled is true but sync.postgres_url is not set"}, result.Body.Warnings)
}

func TestMaintenanceAcceptsDatabaseAlias(t *testing.T) {
	server, _, dir := newTestServer(t)
	alias := filepath.Join(t.TempDir(), "alias.db")
	if err := os.Symlink(filepath.Join(dir, "test.db"), alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	response := maintenanceRequest(t, server, "/api/maintenance/legacy/export", map[string]any{"db": alias})
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
}
