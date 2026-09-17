package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/embedding"
	"go.kenn.io/roborev/internal/searchdoc"
	"go.kenn.io/roborev/internal/searchindex"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

type deterministicEmbeddingServer struct {
	server       *httptest.Server
	blockModel   string
	blocked      chan struct{}
	release      chan struct{}
	edited       chan struct{}
	blockOnce    sync.Once
	editedOnce   sync.Once
	rejectedBody string
}

func newDeterministicEmbeddingServer(t *testing.T) *deterministicEmbeddingServer {
	t.Helper()
	fake := &deterministicEmbeddingServer{
		blocked: make(chan struct{}), release: make(chan struct{}), edited: make(chan struct{}),
	}
	fake.server = httptest.NewServer(http.HandlerFunc(fake.serveHTTP))
	t.Cleanup(fake.server.Close)
	t.Cleanup(func() {
		select {
		case <-fake.release:
		default:
			close(fake.release)
		}
	})
	return fake
}

func (fake *deterministicEmbeddingServer) serveHTTP(w http.ResponseWriter, request *http.Request) {
	if fake.rejectedBody != "" {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(fake.rejectedBody))
		return
	}
	var body struct {
		Model     string   `json:"model"`
		Input     []string `json:"input"`
		InputType string   `json:"input_type"`
	}
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if body.Model == fake.blockModel && body.InputType == "document" {
		fake.blockOnce.Do(func() { close(fake.blocked) })
		select {
		case <-fake.release:
		case <-request.Context().Done():
			return
		}
	}

	data := make([]map[string]any, len(body.Input))
	for index, text := range body.Input {
		if body.InputType == "document" && strings.Contains(text, "edited response axis") {
			fake.editedOnce.Do(func() { close(fake.edited) })
		}
		data[index] = map[string]any{"index": index, "embedding": deterministicAxis(text)}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
}

func TestDeterministicEmbeddingServerStopsBlockedRequestOnCancellation(t *testing.T) {
	fake := newDeterministicEmbeddingServer(t)
	fake.blockModel = "cancel-model"
	body := strings.NewReader(`{"model":"cancel-model","input":["document"],"input_type":"document"}`)
	ctx, cancel := context.WithCancel(t.Context())
	request := httptest.NewRequest(http.MethodPost, "/embeddings", body).WithContext(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		fake.serveHTTP(httptest.NewRecorder(), request)
	}()

	blocked := false
	select {
	case <-fake.blocked:
		blocked = true
	case <-time.After(time.Second):
	}
	require.True(t, blocked, "fake provider did not block")
	cancel()
	finished := false
	select {
	case <-done:
		finished = true
	case <-time.After(time.Second):
	}
	require.True(t, finished, "fake provider ignored request cancellation")
}

func deterministicAxis(text string) []float32 {
	switch {
	case strings.Contains(text, "edited response axis"), strings.Contains(text, "edited concept"):
		return []float32{0, 1, 0}
	case strings.Contains(text, "original response axis"), strings.Contains(text, "original concept"):
		return []float32{1, 0, 0}
	default:
		return []float32{0, 0, 1}
	}
}

func newIntegrationEmbeddingClient(t *testing.T, endpoint, model, apiKey string) *embedding.Client {
	t.Helper()
	client, err := embedding.New(embedding.Config{
		BaseURL: endpoint, Model: model, APIKey: apiKey, Dims: 3,
		BatchSize: 8, InputTypeMode: "retrieval",
	})
	require.NoError(t, err)
	return client
}

func runSearchReconciler(t *testing.T, reconciler *searchindex.Reconciler) (context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- reconciler.Run(ctx) }()
	return cancel, done
}

func waitForSearch(t *testing.T, description string, predicate func() bool) {
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

func completeSearchIntegrationReview(t *testing.T, db *storage.DB) (*storage.ReviewJob, string) {
	t.Helper()
	repo, err := db.GetOrCreateRepo(t.TempDir(), "github.com/example/search-integration")
	require.NoError(t, err)
	return completeSearchIntegrationReviewForRepo(t, db, repo.ID, "abcdef1234567890")
}

func completeSearchIntegrationReviewForRepo(
	t *testing.T, db *storage.DB, repoID int64, commitSHA string,
) (*storage.ReviewJob, string) {
	t.Helper()
	commit, err := db.GetOrCreateCommit(repoID, commitSHA, "Test", "search integration", time.Now())
	require.NoError(t, err)
	job, err := db.EnqueueJob(storage.EnqueueOpts{
		RepoID: repoID, CommitID: commit.ID, GitRef: commit.SHA, Branch: "main", Agent: "test",
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

func TestSearchIntegrationLocalReviewResponseRevisionAndModelCutover(t *testing.T) {
	ctx := t.Context()
	db := testutil.OpenTestDB(t)
	job, reviewUUID := completeSearchIntegrationReview(t, db)
	response, err := db.AddCommentToJob(job.ID, "human", "original response axis")
	require.NoError(t, err)

	fake := newDeterministicEmbeddingServer(t)
	clientA := newIntegrationEmbeddingClient(t, fake.server.URL, "model-a", "api-key-secret")
	index, err := searchindex.Open(ctx, searchindex.PathFor(t.TempDir()+"/reviews.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, index.Close()) })
	reconcilerA := searchindex.NewReconciler(db, index, clientA, searchindex.ReconcilerConfig{
		MaxFillTime: time.Second, SweepInterval: time.Hour,
	})
	serviceA := searchindex.NewService(db, index, clientA, reconcilerA)
	cancelA, doneA := runSearchReconciler(t, reconcilerA)
	waitForSearch(t, "initial vector generation", func() bool {
		return reconcilerA.Health().ActiveGeneration == clientA.Generation().Fingerprint()
	})

	server := newServerWithLogs(db, config.DefaultConfig(), "", newTestErrorLog(), newTestActivityLog())
	server.search = serviceA
	t.Cleanup(func() { _ = server.Close() })
	for _, testCase := range []struct {
		mode  string
		query string
	}{
		{mode: "lexical", query: "racewindow"},
		{mode: "semantic", query: "original concept"},
		{mode: "hybrid", query: "racewindow"},
	} {
		result := executeSearch(server, url.Values{"q": {testCase.query}, "mode": {testCase.mode}})
		require.Equal(t, http.StatusOK, result.Code, result.Body.String())
		var decoded SearchResponse
		require.NoError(t, json.Unmarshal(result.Body.Bytes(), &decoded))
		require.NotEmpty(t, decoded.Hits)
		assert.Equal(t, reviewUUID, decoded.Hits[0].ReviewUUID)
	}

	before, err := db.GetSearchDocument(ctx, reviewUUID)
	require.NoError(t, err)
	require.NotNil(t, before)
	beforeHash := searchdoc.Render(*before).ContentHash
	fake.blockModel = "model-a"
	_, err = db.Exec(`UPDATE responses SET response = ? WHERE id = ?`, "edited response axis", response.ID)
	require.NoError(t, err)
	reconcilerA.Wake()
	replacementStarted := false
	select {
	case <-fake.blocked:
		replacementStarted = true
	case <-time.After(5 * time.Second):
	}
	require.True(t, replacementStarted, "response replacement embedding did not begin")
	oldResult, err := serviceA.Search(ctx, searchindex.SearchParams{
		Query: "original concept", Mode: searchindex.ModeSemantic, Limit: 10,
	})
	require.NoError(t, err)
	assert.Empty(t, oldResult.Hits, "stale response vector must be excluded while replacement is pending")
	close(fake.release)
	editedEmbedded := false
	select {
	case <-fake.edited:
		editedEmbedded = true
	case <-time.After(5 * time.Second):
	}
	require.True(t, editedEmbedded, "edited response was not embedded")
	waitForSearch(t, "edited response vector", func() bool {
		result, searchErr := serviceA.Search(ctx, searchindex.SearchParams{
			Query: "edited concept", Mode: searchindex.ModeSemantic, Limit: 10,
		})
		return searchErr == nil && len(result.Hits) == 1
	})
	after, err := db.GetSearchDocument(ctx, reviewUUID)
	require.NoError(t, err)
	require.NotNil(t, after)
	assert.NotEqual(t, beforeHash, searchdoc.Render(*after).ContentHash)
	oldResult, err = serviceA.Search(ctx, searchindex.SearchParams{
		Query: "original concept", Mode: searchindex.ModeSemantic, Limit: 10,
	})
	require.NoError(t, err)
	assert.Empty(t, oldResult.Hits, "stale response vector must not remain searchable")

	secondJob, secondReviewUUID := completeSearchIntegrationReviewForRepo(t, db, job.RepoID, job.GitRef)
	_, err = db.AddCommentToJob(secondJob.ID, "human", "original response axis")
	require.NoError(t, err)
	reconcilerA.Wake()
	waitForSearch(t, "second review vector", func() bool {
		result, searchErr := serviceA.Search(ctx, searchindex.SearchParams{
			Query: "original concept", Mode: searchindex.ModeSemantic, Limit: 10,
		})
		return searchErr == nil && len(result.Hits) == 1 && result.Hits[0].ReviewUUID == secondReviewUUID
	})
	firstBeforeRemap, err := db.GetSearchDocument(ctx, reviewUUID)
	require.NoError(t, err)
	secondBeforeRemap, err := db.GetSearchDocument(ctx, secondReviewUUID)
	require.NoError(t, err)
	require.NotNil(t, firstBeforeRemap)
	require.NotNil(t, secondBeforeRemap)
	firstRemapHash := searchdoc.Render(*firstBeforeRemap).ContentHash
	secondRemapHash := searchdoc.Render(*secondBeforeRemap).ContentHash
	const remappedSHA = "fedcba9876543210"
	remapped, err := db.RemapJob(
		job.RepoID, job.GitRef, remappedSHA, "", "Test", "remapped search integration", time.Now(),
	)
	require.NoError(t, err)
	require.Equal(t, 2, remapped)
	reconcilerA.Wake()
	waitForSearch(t, "both remapped review vectors", func() bool {
		edited, editedErr := serviceA.Search(ctx, searchindex.SearchParams{
			Query: "edited concept", Mode: searchindex.ModeSemantic, Limit: 10,
		})
		original, originalErr := serviceA.Search(ctx, searchindex.SearchParams{
			Query: "original concept", Mode: searchindex.ModeSemantic, Limit: 10,
		})
		return editedErr == nil && originalErr == nil && len(edited.Hits) == 1 && len(original.Hits) == 1 &&
			edited.Hits[0].CommitSHA == remappedSHA && original.Hits[0].CommitSHA == remappedSHA
	})
	firstAfterRemap, err := db.GetSearchDocument(ctx, reviewUUID)
	require.NoError(t, err)
	secondAfterRemap, err := db.GetSearchDocument(ctx, secondReviewUUID)
	require.NoError(t, err)
	require.NotNil(t, firstAfterRemap)
	require.NotNil(t, secondAfterRemap)
	assert.NotEqual(t, firstRemapHash, searchdoc.Render(*firstAfterRemap).ContentHash)
	assert.NotEqual(t, secondRemapHash, searchdoc.Render(*secondAfterRemap).ContentHash)

	cancelA()
	require.ErrorIs(t, <-doneA, context.Canceled)

	fakeB := newDeterministicEmbeddingServer(t)
	fakeB.blockModel = "model-b"
	clientB := newIntegrationEmbeddingClient(t, fakeB.server.URL, "model-b", "api-key-secret")
	reconcilerB := searchindex.NewReconciler(db, index, clientB, searchindex.ReconcilerConfig{
		MaxFillTime: time.Second, SweepInterval: time.Hour,
	})
	serviceB := searchindex.NewService(db, index, clientB, reconcilerB)
	cancelB, doneB := runSearchReconciler(t, reconcilerB)
	defer func() {
		cancelB()
		require.ErrorIs(t, <-doneB, context.Canceled)
	}()
	replacementStarted = false
	select {
	case <-fakeB.blocked:
		replacementStarted = true
	case <-time.After(5 * time.Second):
	}
	require.True(t, replacementStarted, "replacement generation did not begin")
	available, err := index.GenerationAvailable(ctx, clientA.Generation().Fingerprint())
	require.NoError(t, err)
	assert.True(t, available, "old active generation stays available during replacement")
	auto, err := serviceB.Search(ctx, searchindex.SearchParams{
		Query: "racewindow", Mode: searchindex.ModeAuto, Limit: 10,
	})
	require.NoError(t, err)
	assert.Equal(t, searchindex.ModeLexical, auto.Mode)
	assert.True(t, auto.Degraded)
	close(fakeB.release)
	waitForSearch(t, "replacement generation cutover", func() bool {
		return reconcilerB.Health().ActiveGeneration == clientB.Generation().Fingerprint()
	})
	replaced, err := serviceB.Search(ctx, searchindex.SearchParams{
		Query: "edited concept", Mode: searchindex.ModeSemantic, Limit: 10,
	})
	require.NoError(t, err)
	require.Len(t, replaced.Hits, 1)
	assert.Equal(t, reviewUUID, replaced.Hits[0].ReviewUUID)
}

func TestSearchIntegrationProviderRejectionIsSanitized(t *testing.T) {
	db := testutil.OpenTestDB(t)
	_, _ = completeSearchIntegrationReview(t, db)
	fake := newDeterministicEmbeddingServer(t)
	fake.rejectedBody = `{"error":"provider-body-secret vector=[0.120987654321,0.340987654321]"}`
	const apiKey = "credential-secret"
	client := newIntegrationEmbeddingClient(t, fake.server.URL+"/endpoint-secret", "reject-model", apiKey)
	index, err := searchindex.Open(t.Context(), searchindex.PathFor(t.TempDir()+"/reviews.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, index.Close()) })
	reconciler := searchindex.NewReconciler(db, index, client, searchindex.ReconcilerConfig{
		MinBackoff: time.Hour, MaxBackoff: time.Hour, SweepInterval: time.Hour,
	})
	service := searchindex.NewService(db, index, client, reconciler)
	cancel, done := runSearchReconciler(t, reconciler)
	defer func() {
		cancel()
		require.ErrorIs(t, <-done, context.Canceled)
	}()
	waitForSearch(t, "sanitized provider rejection", func() bool {
		return reconciler.Health().LastError == "authentication"
	})

	server := newServerWithLogs(db, config.DefaultConfig(), "", newTestErrorLog(), newTestActivityLog())
	server.search = service
	t.Cleanup(func() { _ = server.Close() })
	response := executeSearch(server, url.Values{"q": {"query-secret"}, "mode": {"semantic"}})
	require.Equal(t, http.StatusServiceUnavailable, response.Code)
	health, err := json.Marshal(reconciler.Health())
	require.NoError(t, err)
	errorEntries, err := json.Marshal(server.errorLog.Recent())
	require.NoError(t, err)
	activityEntries, err := json.Marshal(server.activityLog.Recent())
	require.NoError(t, err)
	combined := response.Body.String() + string(health) + string(errorEntries) + string(activityEntries)
	for _, secret := range []string{
		apiKey, "provider-body-secret", "endpoint-secret", "query-secret", "0.120987654321", "0.340987654321",
	} {
		leaked := strings.Contains(combined, secret)
		assert.False(t, leaked, "sensitive value leaked")
	}
	assert.Contains(t, combined, "authentication")
}
