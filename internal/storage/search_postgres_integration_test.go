//go:build postgres

package storage_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/embedding"
	"go.kenn.io/roborev/internal/searchindex"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

func TestIntegration_SearchPullWakeReconcilesAllModes(t *testing.T) {
	ctx := t.Context()
	postgresURL := os.Getenv("TEST_POSTGRES_URL")
	if postgresURL == "" {
		postgresURL = "postgres://roborev_test:roborev_test_password@localhost:5433/roborev_test"
	}
	pool, err := storage.NewPgPool(ctx, postgresURL, storage.DefaultPgPoolConfig())
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	_, err = pool.Pool().Exec(ctx, "DROP SCHEMA IF EXISTS roborev CASCADE")
	require.NoError(t, err)
	require.NoError(t, pool.EnsureSchema(ctx))

	sourcePath := filepath.Join(t.TempDir(), "source.db")
	sourceDB, err := storage.Open(sourcePath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, sourceDB.Close()) })
	targetPath := filepath.Join(t.TempDir(), "target.db")
	targetDB, err := storage.Open(targetPath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, targetDB.Close()) })

	sourceWorker := startSearchSyncWorker(t, sourceDB, postgresURL, "postgres-search-source")
	targetWorker := startSearchSyncWorker(t, targetDB, postgresURL, "postgres-search-target")
	job, reviewUUID := completeSearchReview(t, sourceDB)
	_, err = sourceDB.AddCommentToJob(job.ID, "human", "synced response")
	require.NoError(t, err)
	_, err = sourceWorker.SyncNow()
	require.NoError(t, err)

	embeddingServer := newSearchEmbeddingServer(t)
	client, err := embedding.New(embedding.Config{
		BaseURL: embeddingServer.URL, Model: "postgres-search-model", APIKey: "api-key-secret",
		Dims: 3, BatchSize: 8, InputTypeMode: "retrieval",
	})
	require.NoError(t, err)
	index, err := searchindex.Open(ctx, searchindex.PathFor(targetPath))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, index.Close()) })
	reconciler := searchindex.NewReconciler(targetDB, index, client, searchindex.ReconcilerConfig{
		MaxFillTime: time.Second, SweepInterval: time.Hour,
	})
	service := searchindex.NewService(targetDB, index, client, reconciler)
	reconcilerCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- reconciler.Run(reconcilerCtx) }()
	defer func() {
		cancel()
		require.ErrorIs(t, <-done, context.Canceled)
	}()
	waitForSearchCondition(t, "empty target reconciler startup", func() bool {
		return reconciler.Health().ActiveGeneration == client.Generation().Fingerprint()
	})
	targetWorker.SetAfterPullWrite(reconciler.Wake)

	stats, err := targetWorker.SyncNow()
	require.NoError(t, err)
	assert.Equal(t, 1, stats.PulledJobs)
	assert.Equal(t, 1, stats.PulledReviews)
	assert.Equal(t, 1, stats.PulledResponses)

	for _, testCase := range []struct {
		mode  searchindex.SearchMode
		query string
	}{
		{mode: searchindex.ModeLexical, query: "racewindow"},
		{mode: searchindex.ModeSemantic, query: "original concept"},
		{mode: searchindex.ModeHybrid, query: "original concept"},
	} {
		waitForSearchCondition(t, string(testCase.mode)+" PostgreSQL pull result", func() bool {
			result, searchErr := service.Search(ctx, searchindex.SearchParams{
				Query: testCase.query, Mode: testCase.mode, Branch: "main", Limit: 10,
			})
			return searchErr == nil && len(result.Hits) == 1 && result.Hits[0].ReviewUUID == reviewUUID
		})
	}
}

func startSearchSyncWorker(
	t *testing.T, db *storage.DB, postgresURL, machineName string,
) *storage.SyncWorker {
	t.Helper()
	worker := storage.NewSyncWorker(db, config.SyncConfig{
		Enabled: true, PostgresURL: postgresURL, Interval: "1h",
		MachineName: machineName, ConnectTimeout: "5s",
	})
	require.NoError(t, worker.SetSkipInitialSync(true))
	require.NoError(t, worker.Start())
	t.Cleanup(worker.Stop)
	waitForSearchCondition(t, machineName+" sync connection", func() bool {
		healthy, _ := worker.HealthCheck()
		return healthy
	})
	return worker
}

func completeSearchReview(t *testing.T, db *storage.DB) (*storage.ReviewJob, string) {
	t.Helper()
	repo, err := db.GetOrCreateRepo(t.TempDir(), "github.com/example/search-integration")
	require.NoError(t, err)
	commit, err := db.GetOrCreateCommit(repo.ID, "abcdef1234567890", "Test", "search integration", time.Now())
	require.NoError(t, err)
	job, err := db.EnqueueJob(storage.EnqueueOpts{
		RepoID: repo.ID, CommitID: commit.ID, GitRef: commit.SHA, Branch: "main", Agent: "test",
	})
	require.NoError(t, err)
	claimed, err := db.ClaimJob("search-integration")
	require.NoError(t, err)
	require.Equal(t, job.ID, claimed.ID)
	require.NoError(t, testutil.CompleteReviewFixture(
		db, job.ID, "test", "review prompt", "racewindow original response axis",
	))
	review, err := db.GetReviewByJobID(job.ID)
	require.NoError(t, err)
	require.NotNil(t, review.UUID)
	return job, review.UUID.String()
}

func newSearchEmbeddingServer(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		var body struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		data := make([]map[string]any, len(body.Input))
		for index, text := range body.Input {
			axis := []float32{0, 0, 1}
			if strings.Contains(text, "original response axis") || strings.Contains(text, "original concept") {
				axis = []float32{1, 0, 0}
			}
			data[index] = map[string]any{"index": index, "embedding": axis}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	t.Cleanup(server.Close)
	return server
}

func waitForSearchCondition(t *testing.T, description string, predicate func() bool) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		if predicate() {
			return
		}
		select {
		case <-deadline.C:
			require.True(t, predicate(), "timed out waiting for "+description)
			return
		case <-ticker.C:
		}
	}
}
