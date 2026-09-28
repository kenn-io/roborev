package daemon

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/embedconfig"
	"go.kenn.io/kit/embedmodel"
	"go.kenn.io/kit/vector"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/searchdoc"
	"go.kenn.io/roborev/internal/searchindex"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

type replacementHealthStore struct {
	sources []storage.SearchReviewSource
}

func (s *replacementHealthStore) ListSearchDocuments(
	_ context.Context, after int64, limit int,
) ([]storage.SearchReviewSource, error) {
	result := make([]storage.SearchReviewSource, 0, limit)
	for _, source := range s.sources {
		if source.ReviewID > after {
			result = append(result, source)
		}
		if len(result) == limit {
			break
		}
	}
	return result, nil
}

type replacementHealthEmbedder struct {
	space   embedmodel.Descriptor
	calls   int
	blocked chan struct{}
}

func (e *replacementHealthEmbedder) EncodeFunc(embedconfig.Role) vector.EncodeFunc {
	return e.embed
}

func (e *replacementHealthEmbedder) embed(ctx context.Context, texts []string) ([][]float32, error) {
	e.calls++
	if e.calls == 2 {
		close(e.blocked)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	result := make([][]float32, len(texts))
	for i := range result {
		result[i] = []float32{1, 0}
	}
	return result, nil
}

func (e *replacementHealthEmbedder) Space() embedmodel.Descriptor { return e.space }
func (e *replacementHealthEmbedder) BatchSize() int               { return 1 }

func cosineSpace(model string, dims int) embedmodel.Descriptor {
	return embedmodel.Descriptor{Model: embedconfig.Model{
		Name: model, Dimensions: dims,
		Metric: embedconfig.MetricCosine, Normalization: embedconfig.NormalizationL2,
	}}
}

// setupTestServer creates a temporary DB and Server, handling cleanup automatically.
func setupTestServer(t *testing.T) *Server {
	t.Helper()
	db := testutil.OpenTestDB(t)

	cfg := config.DefaultConfig()
	server := newServerWithLogs(db, cfg, "", newTestErrorLog(), newTestActivityLog())
	t.Cleanup(func() { server.Close() })
	return server
}

// executeHealthCheck sends a request to the health endpoint and returns the recorder.
func executeHealthCheck(server *Server, method string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "/api/health", nil)
	w := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(w, req)
	return w
}

// decodeHealthStatus parses a HealthStatus from the response body.
func decodeHealthStatus(t *testing.T, w *httptest.ResponseRecorder) storage.HealthStatus {
	t.Helper()
	var health storage.HealthStatus
	err := json.NewDecoder(w.Result().Body).Decode(&health)
	require.NoError(t, err, "Failed to parse health response")
	return health
}

func TestHealth(t *testing.T) {
	t.Run("Happy Path", func(t *testing.T) {
		server := setupTestServer(t)
		w := executeHealthCheck(server, http.MethodGet)
		assert.Equal(t, http.StatusOK, w.Code)

		health := decodeHealthStatus(t, w)
		assert.NotEmpty(t, health.Uptime, "Uptime")
		assert.NotEmpty(t, health.Version, "Version")
		assert.True(t, hasComponent(health.Components, "database"), "Expected component 'database' in health check")
		assert.True(t, hasComponent(health.Components, "workers"), "Expected component 'workers' in health check")
		assert.True(t, health.Healthy, "Expected health to be OK")
	})

	t.Run("With Errors", func(t *testing.T) {
		server := setupTestServer(t)
		// Log specific errors
		if server.errorLog != nil {
			server.errorLog.LogError("worker", "test error 1", 123)
			server.errorLog.LogError("worker", "test error 2", 456)
		}

		w := executeHealthCheck(server, http.MethodGet)
		health := decodeHealthStatus(t, w)

		assert.Positive(t, health.ErrorCount, "Expected error count > 0")
		assert.True(t, hasError(health.RecentErrors, "worker", 456), "Expected error for component 'worker' with JobID 456")
	})

	t.Run("Method Not Allowed", func(t *testing.T) {
		server := setupTestServer(t)
		w := executeHealthCheck(server, http.MethodPost)
		assert.Equal(t, http.StatusMethodNotAllowed, w.Code)
	})
}

func TestHealthAllowsRunningJobsWithinTheirTimeout(t *testing.T) {
	for _, tc := range []struct {
		name          string
		globalMinutes int
		repoConfig    string
		panelConfig   string
	}{
		{name: "global", globalMinutes: 75},
		{name: "repository", repoConfig: "job_timeout_minutes = 75\n"},
		{name: "panel", panelConfig: `{"timeout":"75m"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := setupTestServer(t)
			server.configWatcher.cfg.JobTimeoutMinutes = tc.globalMinutes
			repoPath := t.TempDir()
			testutil.InitTestGitRepo(t, repoPath)
			if tc.repoConfig != "" {
				require.NoError(t, os.WriteFile(filepath.Join(repoPath, ".roborev.toml"), []byte(tc.repoConfig), 0o600))
			}
			repo, err := server.db.GetOrCreateRepo(repoPath)
			require.NoError(t, err)
			const agentName = "health-long-review"
			started := make(chan time.Time, 1)
			release := make(chan struct{})
			t.Cleanup(func() { close(release) })
			agent.RegisterForTest(t, &agent.FakeAgent{
				NameStr: agentName,
				ReviewFn: func(ctx context.Context, _, _, _ string, _ io.Writer) (string, error) {
					deadline, _ := ctx.Deadline()
					started <- deadline
					select {
					case <-release:
						return string(testutil.ReviewFixtureJSON("No issues found.")), nil
					case <-ctx.Done():
						return "", ctx.Err()
					}
				},
			})
			opts := storage.EnqueueOpts{
				RepoID: repo.ID, GitRef: testutil.GetHeadSHA(t, repoPath), Agent: agentName,
			}
			if tc.panelConfig != "" {
				runID := uuid.New()
				opts.PanelRunUUID = &runID
				opts.PanelRole = storage.PanelRoleMember
				opts.PanelMemberConfigJSON = tc.panelConfig
			}
			job, err := server.db.EnqueueJob(opts)
			require.NoError(t, err)
			server.workerPool.Start()
			deadline := testutil.ReceiveWithTimeout(t, started, 10*time.Second)
			require.Greater(t, time.Until(deadline), time.Hour)

			// Simulate an old running attempt without waiting for the wall clock.
			_, err = server.db.Exec("UPDATE review_jobs SET started_at = ? WHERE id = ?",
				time.Now().Add(-45*time.Minute).UTC().Format(time.RFC3339), job.ID)
			require.NoError(t, err)
			// Reloading config must not shorten the running attempt's deadline.
			server.configWatcher.cfgMu.Lock()
			server.configWatcher.cfg = config.DefaultConfig()
			server.configWatcher.cfgMu.Unlock()

			response := executeHealthCheck(server, http.MethodGet)
			require.Equal(t, http.StatusOK, response.Code)
			health := decodeHealthStatus(t, response)
			assert.True(t, health.Healthy)
			assert.Contains(t, health.Components, storage.ComponentHealth{Name: "workers", Healthy: true})
		})
	}
}

func TestHealthReportsAbandonedAndOverdueJobs(t *testing.T) {
	for _, tracked := range []bool{false, true} {
		name := "abandoned"
		if tracked {
			name = "overdue"
		}
		t.Run(name, func(t *testing.T) {
			server := setupTestServer(t)
			repo, err := server.db.GetOrCreateRepo(t.TempDir())
			require.NoError(t, err)
			_, err = server.db.EnqueueJob(storage.EnqueueOpts{RepoID: repo.ID, GitRef: "HEAD", Agent: "test"})
			require.NoError(t, err)
			job, err := server.db.ClaimJob("worker-test")
			require.NoError(t, err)
			require.NotNil(t, job)
			_, err = server.db.Exec("UPDATE review_jobs SET started_at = ? WHERE id = ?",
				time.Now().Add(-45*time.Minute).UTC().Format(time.RFC3339), job.ID)
			require.NoError(t, err)
			if tracked {
				server.workerPool.registerRunningJob(job.ID, func() {}, time.Now().Add(-time.Minute))
			}

			response := executeHealthCheck(server, http.MethodGet)
			require.Equal(t, http.StatusOK, response.Code)
			health := decodeHealthStatus(t, response)
			assert.False(t, health.Healthy)
			assert.Contains(t, health.Components, storage.ComponentHealth{
				Name: "workers", Healthy: false,
				Message: "1 stalled job(s) running beyond their allowed time",
			})
		})
	}
}

func TestHealthIncludesOptionalSearchWithoutDegradingDaemon(t *testing.T) {
	server := setupTestServer(t)
	backlog := int64(4)
	rate := 1.5
	eta := int64(8)
	lastSuccess := time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC)
	reconciler := newRecordingSearchReconciler()
	reconciler.health = searchindex.HealthSnapshot{
		Indexed: 20, MirrorComplete: true, MirrorBacklog: &backlog,
		EmbeddingsConfigured: true, VectorState: "error", Embedded: 12,
		Skipped: 2, EmbeddingBacklog: 6, ActiveGeneration: "generation-a",
		LastSuccessAt: &lastSuccess, RatePerSecond: &rate, ETASeconds: &eta,
		LastError: "embedding authentication rejected", LastErrorStatus: http.StatusUnauthorized,
	}
	server.searchReconciler = reconciler

	response := executeHealthCheck(server, http.MethodGet)
	require.Equal(t, http.StatusOK, response.Code)
	health := decodeHealthStatus(t, response)

	assert.True(t, health.Healthy)
	require.NotNil(t, health.Search)
	assert.Equal(t, int64(20), health.Search.Indexed)
	assert.Equal(t, &backlog, health.Search.MirrorBacklog)
	assert.Equal(t, searchindex.VectorUnavailable, health.Search.VectorState)
	assert.Equal(t, "embedding authentication rejected", health.Search.LastError)
	assert.Equal(t, http.StatusUnauthorized, health.Search.LastErrorStatus)

	server.searchReconciler = nil
	response = executeHealthCheck(server, http.MethodGet)
	health = decodeHealthStatus(t, response)
	assert.Nil(t, health.Search)
}

func TestSearchHealthMapsReconcilerStateToPublicVectorState(t *testing.T) {
	for _, tc := range []struct {
		name     string
		snapshot searchindex.HealthSnapshot
		want     string
	}{
		{
			name: "unconfigured is disabled",
			snapshot: searchindex.HealthSnapshot{
				VectorState: "unconfigured",
			},
			want: searchindex.VectorDisabled,
		},
		{
			name: "initial generation is building",
			snapshot: searchindex.HealthSnapshot{
				EmbeddingsConfigured: true, VectorState: "building", Generation: "next",
			},
			want: searchindex.VectorBuilding,
		},
		{
			name: "matching active generation is active",
			snapshot: searchindex.HealthSnapshot{
				EmbeddingsConfigured: true, VectorState: "ready",
				Generation: "current", ActiveGeneration: "current",
			},
			want: searchindex.VectorActive,
		},
		{
			name: "different active generation is replacing",
			snapshot: searchindex.HealthSnapshot{
				EmbeddingsConfigured: true, VectorState: "building",
				Generation: "next", ActiveGeneration: "current",
			},
			want: searchindex.VectorReplacing,
		},
		{
			name: "replacement error is unavailable despite old active generation",
			snapshot: searchindex.HealthSnapshot{
				EmbeddingsConfigured: true, VectorState: "error",
				Generation: "next", ActiveGeneration: "current",
			},
			want: searchindex.VectorUnavailable,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			health := searchHealthFromSnapshot(tc.snapshot)
			assert.Equal(t, tc.want, health.VectorState)
		})
	}
}

func TestHealthReportsConcreteReconcilerReplacement(t *testing.T) {
	ctx := context.Background()
	sources := []storage.SearchReviewSource{
		{
			ReviewID: 1, JobID: 101, RepoID: 7,
			ReviewUUID: "00000000-0000-4000-8000-000000000001",
			JobUUID:    "10000000-0000-4000-8000-000000000001",
			RepoName:   "example/repo", Output: "first review",
		},
		{
			ReviewID: 2, JobID: 102, RepoID: 7,
			ReviewUUID: "00000000-0000-4000-8000-000000000002",
			JobUUID:    "10000000-0000-4000-8000-000000000002",
			RepoName:   "example/repo", Output: "second review",
		},
	}
	index, err := searchindex.Open(ctx, searchindex.PathFor(t.TempDir()+"/reviews.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, index.Close()) })
	docs := make([]searchdoc.Document, len(sources))
	for i := range sources {
		docs[i] = searchdoc.Render(sources[i])
	}
	_, err = index.RefreshMirrorPage(ctx, docs, nil)
	require.NoError(t, err)
	activeKey, err := index.ResolveGeneration(ctx, cosineSpace("active", 2))
	require.NoError(t, err)
	pending, err := index.PendingGeneration(ctx, activeKey, len(docs))
	require.NoError(t, err)
	require.Len(t, pending, len(docs))
	for _, document := range pending {
		require.NoError(t, index.SaveGenerationVectors(ctx, activeKey, document,
			[]vector.ChunkVector{{ChunkIndex: 0, Vector: vector.Vector{1, 0}}}))
	}
	require.NoError(t, index.ActivateGeneration(ctx, activeKey))

	embedder := &replacementHealthEmbedder{
		space:   cosineSpace("replacement", 2),
		blocked: make(chan struct{}),
	}
	reconciler := searchindex.NewReconciler(
		&replacementHealthStore{sources: sources}, index, embedder,
		searchindex.ReconcilerConfig{MaxFillBatches: 1, SweepInterval: time.Hour},
	)
	runCtx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- reconciler.Run(runCtx) }()
	select {
	case <-embedder.blocked:
	case <-time.After(time.Second):
		require.FailNow(t, "replacement reconciliation did not reach second batch")
	}

	server := setupTestServer(t)
	server.searchReconciler = reconciler
	response := executeHealthCheck(server, http.MethodGet)
	require.Equal(t, http.StatusOK, response.Code)
	health := decodeHealthStatus(t, response)
	require.NotNil(t, health.Search)
	assert.Equal(t, searchindex.VectorReplacing, health.Search.VectorState)
	assert.Equal(t, activeKey, health.Search.ActiveGeneration)

	cancel()
	assert.ErrorIs(t, <-done, context.Canceled)
}

// Helpers

func hasComponent(components []storage.ComponentHealth, name string) bool {
	for _, c := range components {
		if c.Name == name {
			return true
		}
	}
	return false
}

func hasError(errors []storage.ErrorEntry, component string, jobID int64) bool {
	for _, e := range errors {
		if e.Component == component && e.JobID == jobID {
			return true
		}
	}
	return false
}

func TestHealthCredentialAvailability(t *testing.T) {
	for _, tc := range []struct {
		credential, source, reason string
		status                     int
	}{
		{"missing", "env:EMBEDDING_KEY", "no embedding API key (env EMBEDDING_KEY is unset)", 0},
		{"rejected", "file:~/.config/roborev/embedding.key", "embedding authentication rejected (401)", 401},
		{"ok", "inline", "", 0},
	} {
		t.Run(tc.credential, func(t *testing.T) {
			server := setupTestServer(t)
			r := newRecordingSearchReconciler()
			r.health = searchindex.HealthSnapshot{
				EmbeddingsConfigured: true, VectorState: "error",
				Credential: tc.credential, CredentialSource: tc.source, CredentialReason: tc.reason, LastErrorStatus: tc.status,
			}
			server.searchReconciler = r
			response := executeHealthCheck(server, http.MethodGet)
			require.Equal(t, 200, response.Code)
			var body map[string]any
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
			search, ok := body["search"].(map[string]any)
			require.True(t, ok)
			assert.Equal(t, tc.credential, search["credential"])
			assert.Equal(t, tc.source, search["credential_source"])
			if tc.reason != "" {
				assert.Equal(t, tc.reason, search["credential_reason"])
			}
			assert.True(t, decodeHealthStatus(t, response).Healthy)
		})
	}
}
