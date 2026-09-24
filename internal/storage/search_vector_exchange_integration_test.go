//go:build postgres

package storage_test

import (
	"context"
	"encoding/json"
	"fmt"
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

	"go.kenn.io/roborev/internal/embedding"
	"go.kenn.io/roborev/internal/searchindex"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

// sharedEmbeddingServer is one provider endpoint for every daemon in a test,
// because the endpoint URL is part of the generation fingerprint. Daemons are
// told apart by their bearer credential, which is not.
type sharedEmbeddingServer struct {
	*httptest.Server
	mu        sync.Mutex
	documents map[string][]string
	failOn    map[string]bool
}

func newSharedEmbeddingServer(t *testing.T) *sharedEmbeddingServer {
	t.Helper()
	server := &sharedEmbeddingServer{documents: map[string][]string{}, failOn: map[string]bool{}}
	server.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		var body struct {
			Input     []string `json:"input"`
			InputType string   `json:"input_type"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		caller := strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")
		if body.InputType == "document" {
			server.mu.Lock()
			server.documents[caller] = append(server.documents[caller], body.Input...)
			strict := server.failOn[caller]
			server.mu.Unlock()
			if strict {
				http.Error(w, "document input forbidden", http.StatusBadRequest)
				return
			}
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

func (s *sharedEmbeddingServer) failOnDocuments(caller string) {
	s.mu.Lock()
	s.failOn[caller] = true
	s.mu.Unlock()
}

func (s *sharedEmbeddingServer) documentCount(caller string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.documents[caller])
}

type sharedSearchDaemon struct {
	db         *storage.DB
	worker     *storage.SyncWorker
	client     *embedding.Client
	reconciler *searchindex.Reconciler
	service    *searchindex.Service
}

func startSharedSearchDaemon(
	t *testing.T, db *storage.DB, dbPath string, worker *storage.SyncWorker,
	server *sharedEmbeddingServer, apiKey string, config searchindex.ReconcilerConfig,
) *sharedSearchDaemon {
	t.Helper()
	client, err := embedding.New(embedding.Config{
		BaseURL: server.URL, Model: "shared-search-model", APIKey: apiKey,
		Dims: 3, BatchSize: 8, InputTypeMode: "retrieval",
		ChunkMaxRunes: searchindex.ChunkMaxRunes, ChunkOverlapRunes: searchindex.ChunkOverlapRunes,
	})
	require.NoError(t, err)
	index, err := searchindex.Open(t.Context(), searchindex.PathFor(dbPath))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, index.Close()) })
	config.MaxFillTime = time.Second
	config.SweepInterval = time.Hour
	reconciler := searchindex.NewReconciler(db, index, client, config)
	exchange, err := worker.VectorExchange()
	require.NoError(t, err)
	reconciler.SetVectorExchange(exchange)
	worker.SetAfterPullWrite(reconciler.Wake)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- reconciler.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		require.ErrorIs(t, <-done, context.Canceled)
	})
	return &sharedSearchDaemon{
		db: db, worker: worker, client: client, reconciler: reconciler,
		service: searchindex.NewService(db, index, client, reconciler),
	}
}

func resetSharedSearchPostgres(t *testing.T) string {
	t.Helper()
	postgresURL := os.Getenv("TEST_POSTGRES_URL")
	if postgresURL == "" {
		postgresURL = "postgres://roborev_test:roborev_test_password@localhost:5433/roborev_test" // betterleaks:allow (public fixture from docker-compose.test.yml)
	}
	pool, err := storage.NewPgPool(t.Context(), postgresURL, storage.DefaultPgPoolConfig())
	require.NoError(t, err)
	defer pool.Close()
	_, err = pool.Pool().Exec(t.Context(), "DROP SCHEMA IF EXISTS roborev CASCADE")
	require.NoError(t, err)
	require.NoError(t, pool.EnsureSchema(t.Context()))
	return postgresURL
}

func openSharedSearchDB(t *testing.T, name string) (*storage.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), name+".db")
	db, err := storage.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	return db, path
}

func semanticTopHit(t *testing.T, daemon *sharedSearchDaemon, query string) string {
	t.Helper()
	result, err := daemon.service.Search(t.Context(), searchindex.SearchParams{
		Query: query, Mode: searchindex.ModeSemantic, Limit: 5,
	})
	require.NoError(t, err)
	require.NotEmpty(t, result.Hits)
	return result.Hits[0].ReviewUUID
}

func TestIntegration_SharedSearchVectorsPeerImportsWithoutProviderCalls(t *testing.T) {
	postgresURL := resetSharedSearchPostgres(t)
	server := newSharedEmbeddingServer(t)
	ownerDB, ownerPath := openSharedSearchDB(t, "owner")
	peerDB, peerPath := openSharedSearchDB(t, "peer")
	ownerWorker := startSearchSyncWorker(t, ownerDB, postgresURL, "shared-owner")
	peerWorker := startSearchSyncWorker(t, peerDB, postgresURL, "shared-peer")

	job, reviewUUID := completeSearchReview(t, ownerDB)
	_, err := ownerWorker.SyncNow()
	require.NoError(t, err)
	_, err = peerWorker.SyncNow()
	require.NoError(t, err)

	server.failOnDocuments("peer-key")
	fast := searchindex.ReconcilerConfig{LookupMinBackoff: 10 * time.Millisecond}
	owner := startSharedSearchDaemon(t, ownerDB, ownerPath, ownerWorker, server, "owner-key", fast)
	peer := startSharedSearchDaemon(t, peerDB, peerPath, peerWorker, server, "peer-key", fast)
	fingerprint := owner.client.Generation().Fingerprint()
	require.Equal(t, fingerprint, peer.client.Generation().Fingerprint())

	waitForSearchCondition(t, "peer imports the owner's vectors", func() bool {
		health := peer.reconciler.Health()
		return health.Imported == 1 && health.ActiveGeneration == fingerprint && health.EmbeddingBacklog == 0
	})
	assert.Equal(t, 1, server.documentCount("owner-key"))
	assert.Zero(t, server.documentCount("peer-key"))
	assert.Equal(t, searchindex.SourceOK, peer.reconciler.Health().SourceStatus)
	assert.Equal(t, int64(1), owner.reconciler.Health().Published)
	assert.Equal(t, reviewUUID, semanticTopHit(t, owner, "original concept"))
	assert.Equal(t, reviewUUID, semanticTopHit(t, peer, "original concept"),
		"the peer answers a paraphrase with the same top hit as the owner")

	// A local edit on the peer changes its text: the document goes pending and
	// the peer still makes no document call while the owner is responsible.
	var peerJobID int64
	require.NoError(t, peerDB.QueryRow(`SELECT id FROM review_jobs WHERE uuid = ?`, job.UUID).Scan(&peerJobID))
	_, err = peerDB.AddCommentToJob(peerJobID, "reviewer", "peer side note")
	require.NoError(t, err)
	peer.reconciler.Wake()
	waitForSearchCondition(t, "peer sees its edit as awaiting a peer", func() bool {
		return peer.reconciler.Health().AwaitingPeer == 1
	})
	pending, err := peer.service.Search(t.Context(), searchindex.SearchParams{
		Query: "original concept", Mode: searchindex.ModeSemantic, Limit: 5,
	})
	require.NoError(t, err)
	assert.True(t, pending.Partial, "results say coverage is partial while a peer vector is awaited")
	_, err = peerWorker.SyncNow()
	require.NoError(t, err)
	_, err = ownerWorker.SyncNow()
	require.NoError(t, err)
	waitForSearchCondition(t, "peer imports the re-embedded edit", func() bool {
		health := peer.reconciler.Health()
		return health.AwaitingPeer == 0 && health.EmbeddingBacklog == 0 && health.Imported == 1
	})
	assert.Equal(t, 2, server.documentCount("owner-key"), "the owner re-embeds the edited text once")
	assert.Zero(t, server.documentCount("peer-key"))
}

func TestIntegration_SharedSearchVectorsClaimRaceEmbedsEachReviewOnce(t *testing.T) {
	postgresURL := resetSharedSearchPostgres(t)
	server := newSharedEmbeddingServer(t)
	writerDB, _ := openSharedSearchDB(t, "writer")
	firstDB, firstPath := openSharedSearchDB(t, "first")
	secondDB, secondPath := openSharedSearchDB(t, "second")
	writer := startSearchSyncWorker(t, writerDB, postgresURL, "shared-writer")
	firstWorker := startSearchSyncWorker(t, firstDB, postgresURL, "shared-first")
	secondWorker := startSearchSyncWorker(t, secondDB, postgresURL, "shared-second")

	const reviews = 4
	repo, err := writerDB.GetOrCreateRepo(t.TempDir(), "github.com/example/shared-race")
	require.NoError(t, err)
	for i := range reviews {
		sha := fmt.Sprintf("%040d", i+1)
		commit, err := writerDB.GetOrCreateCommit(repo.ID, sha, "Test", fmt.Sprintf("race %d", i), time.Now())
		require.NoError(t, err)
		job, err := writerDB.EnqueueJob(storage.EnqueueOpts{
			RepoID: repo.ID, CommitID: commit.ID, GitRef: sha, Branch: "main", Agent: "test",
		})
		require.NoError(t, err)
		claimed, err := writerDB.ClaimJob("shared-race")
		require.NoError(t, err)
		require.Equal(t, job.ID, claimed.ID)
		require.NoError(t, testutil.CompleteReviewFixture(
			writerDB, job.ID, "test", "review prompt", fmt.Sprintf("race review %d body", i)))
	}
	_, err = writer.SyncNow()
	require.NoError(t, err)
	_, err = firstWorker.SyncNow()
	require.NoError(t, err)
	_, err = secondWorker.SyncNow()
	require.NoError(t, err)

	eager := searchindex.ReconcilerConfig{
		PeerClaimDelay: time.Millisecond, LookupMinBackoff: 10 * time.Millisecond,
	}
	first := startSharedSearchDaemon(t, firstDB, firstPath, firstWorker, server, "first-key", eager)
	second := startSharedSearchDaemon(t, secondDB, secondPath, secondWorker, server, "second-key", eager)

	waitForSearchCondition(t, "both daemons cover every review", func() bool {
		for _, daemon := range []*sharedSearchDaemon{first, second} {
			health := daemon.reconciler.Health()
			if health.EmbeddingBacklog != 0 || health.ActiveGeneration == "" || health.Indexed != reviews {
				return false
			}
		}
		return true
	})
	assert.Equal(t, reviews, server.documentCount("first-key")+server.documentCount("second-key"),
		"claims make exactly one daemon embed each review")
	assert.Equal(t, int64(reviews),
		first.reconciler.Health().Imported+first.reconciler.Health().Published)
	assert.Equal(t, int64(reviews),
		second.reconciler.Health().Imported+second.reconciler.Health().Published)
}
