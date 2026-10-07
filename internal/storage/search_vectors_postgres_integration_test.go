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
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/searchdoc"
	"go.kenn.io/roborev/internal/searchindex"
	"go.kenn.io/roborev/internal/storage"
)

func TestIntegration_SyncingDaemonsShareSearchVectors(t *testing.T) { //nolint:paralleltest // shares the roborev schema in the PostgreSQL database at TEST_POSTGRES_URL
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

	provider := newCountingEmbeddingServer(t)
	source := startSharingDaemon(t, postgresURL, "vectors-source", provider.URL)
	target := startSharingDaemon(t, postgresURL, "vectors-target", provider.URL)
	// Keep the source vectors local until their review has already synced.
	source.reconciler.ShareVectors(nil)

	_, reviewUUID := completeSearchReview(t, source.db)
	waitForSearchCondition(t, "source embeds the review", func() bool {
		return source.reconciler.Health().Embedded == 1
	})
	assert.Equal(t, 0, sharedVectorRows(t, pool), "a review that sync has not pushed is not shared")

	_, err = source.worker.SyncNow()
	require.NoError(t, err)
	source.reconciler.ShareVectors(source.worker)
	waitForSearchCondition(t, "reconciliation publishes the existing source vectors", func() bool {
		return sharedVectorRows(t, pool) == 1
	})

	stats, err := target.worker.SyncNow()
	require.NoError(t, err)
	assert.Equal(t, 1, stats.PulledReviews)
	waitForSearchCondition(t, "target semantic search finds the pulled review", func() bool {
		result, searchErr := target.service.Search(ctx, searchindex.SearchParams{
			Query: "original concept", Mode: searchindex.ModeSemantic, Branch: "main", Limit: 10,
		})
		return searchErr == nil && len(result.Hits) == 1 && result.Hits[0].ReviewUUID == reviewUUID
	})
	assert.Equal(t, 1, provider.documents("vectors-source"))
	assert.Zero(t, provider.documents("vectors-target"), "the target imports instead of embedding")
}

type sharingTestDaemon struct {
	db         *storage.DB
	worker     *storage.SyncWorker
	reconciler *searchindex.Reconciler
	service    *searchindex.Service
}

func startSharingDaemon(t *testing.T, postgresURL, name, providerURL string) *sharingTestDaemon {
	t.Helper()
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), name+".db")
	db, err := storage.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	worker := startSearchSyncWorker(t, db, postgresURL, name)

	// The API key tells the daemons apart at the provider; it is not part of
	// the vector space, so both daemons share one.
	client, err := searchindex.NewEmbeddings(config.SearchEmbeddingsConfig{
		BaseURL: providerURL, Model: "shared-vectors-model",
		Dims: 3, BatchSize: 8, InputTypeMode: "retrieval",
	}, name, searchdoc.RecipeVersion)
	require.NoError(t, err)
	index, err := searchindex.Open(ctx, searchindex.PathFor(path))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, index.Close()) })
	reconciler := searchindex.NewReconciler(db, index, client, searchindex.ReconcilerConfig{
		MaxFillTime: time.Second, SweepInterval: 50 * time.Millisecond,
	})
	reconciler.ShareVectors(worker)
	worker.SetAfterReviewPush(reconciler.ReviewsPushed)
	worker.SetAfterPullWrite(reconciler.Wake)

	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- reconciler.Run(runCtx) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return &sharingTestDaemon{
		db: db, worker: worker, reconciler: reconciler,
		service: searchindex.NewService(db, index, client, reconciler),
	}
}

func sharedVectorRows(t *testing.T, pool *storage.PgPool) int {
	t.Helper()
	var count int
	require.NoError(t, pool.Pool().QueryRow(t.Context(),
		`SELECT count(*) FROM review_search_vectors`).Scan(&count))
	return count
}

type countingEmbeddingServer struct {
	*httptest.Server
	mu     sync.Mutex
	inputs map[string]int
}

// newCountingEmbeddingServer counts document inputs per API key.
func newCountingEmbeddingServer(t *testing.T) *countingEmbeddingServer {
	t.Helper()
	server := &countingEmbeddingServer{inputs: make(map[string]int)}
	server.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		var body struct {
			Input     []string `json:"input"`
			InputType string   `json:"input_type"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		if body.InputType == "document" {
			caller := strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")
			server.mu.Lock()
			server.inputs[caller] += len(body.Input)
			server.mu.Unlock()
		}
		data := make([]map[string]any, len(body.Input))
		for index := range body.Input {
			data[index] = map[string]any{"index": index, "embedding": []float32{1, 0, 0}}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	t.Cleanup(server.Close)
	return server
}

func (s *countingEmbeddingServer) documents(caller string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inputs[caller]
}
