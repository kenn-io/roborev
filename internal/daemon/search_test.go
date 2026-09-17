package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/searchindex"
	"go.kenn.io/roborev/internal/testutil"
)

type recordingReviewSearcher struct {
	mu     sync.Mutex
	params []searchindex.SearchParams
	result searchindex.SearchResult
	err    error
}

func (s *recordingReviewSearcher) Search(
	_ context.Context, params searchindex.SearchParams,
) (searchindex.SearchResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.params = append(s.params, params)
	return s.result, s.err
}

func (s *recordingReviewSearcher) lastParams(t *testing.T) searchindex.SearchParams {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	require.NotEmpty(t, s.params)
	return s.params[len(s.params)-1]
}

type recordingSearchReconciler struct {
	started chan struct{}
	stopped chan struct{}
	wakes   chan struct{}
	health  searchindex.HealthSnapshot
}

type countingSearchReconciler struct {
	mu      sync.Mutex
	started int
	stopped int
	starts  chan struct{}
}

func newCountingSearchReconciler() *countingSearchReconciler {
	return &countingSearchReconciler{
		starts: make(chan struct{}, 4),
	}
}

func (r *countingSearchReconciler) Run(ctx context.Context) error {
	r.mu.Lock()
	r.started++
	r.mu.Unlock()
	r.starts <- struct{}{}
	<-ctx.Done()
	r.mu.Lock()
	r.stopped++
	r.mu.Unlock()
	return ctx.Err()
}

func (r *countingSearchReconciler) Wake() {}

func (r *countingSearchReconciler) Health() searchindex.HealthSnapshot {
	return searchindex.HealthSnapshot{}
}

func (r *countingSearchReconciler) counts() (int, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.started, r.stopped
}

type blockingSearchBroadcaster struct {
	Broadcaster
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockingSearchBroadcaster) Subscribe(repoPath string) (int, <-chan Event) {
	b.once.Do(func() { close(b.entered) })
	<-b.release
	return b.Broadcaster.Subscribe(repoPath)
}

func newRecordingSearchReconciler() *recordingSearchReconciler {
	return &recordingSearchReconciler{
		started: make(chan struct{}),
		stopped: make(chan struct{}),
		wakes:   make(chan struct{}, 8),
	}
}

func (r *recordingSearchReconciler) Run(ctx context.Context) error {
	close(r.started)
	<-ctx.Done()
	close(r.stopped)
	return ctx.Err()
}

func (r *recordingSearchReconciler) Wake() {
	r.wakes <- struct{}{}
}

func (r *recordingSearchReconciler) Health() searchindex.HealthSnapshot {
	return r.health
}

func newSearchHTTPServer(t *testing.T, search reviewSearcher) *Server {
	t.Helper()
	db := testutil.OpenTestDB(t)
	server := newServerWithLogs(
		db, config.DefaultConfig(), "", newTestErrorLog(), newTestActivityLog(),
	)
	server.search = search
	t.Cleanup(func() { _ = server.Close() })
	return server
}

func executeSearch(server *Server, values url.Values) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/api/search?"+values.Encode(), nil)
	recorder := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(recorder, req)
	return recorder
}

func TestSearchOpenAPIParameterContract(t *testing.T) {
	api := (&Server{}).registerHumaAPI(http.NewServeMux())
	operation := api.OpenAPI().Paths["/api/search"].Get
	require.NotNil(t, operation)
	assert.Equal(t, "searchReviews", operation.OperationID)
	assert.Equal(t, []string{"Reviews"}, operation.Tags)
	require.Len(t, operation.Parameters, 8)

	parameters := make(map[string]*huma.Param, len(operation.Parameters))
	for _, parameter := range operation.Parameters {
		parameters[parameter.Name] = parameter
	}

	query := parameters["q"]
	assert.True(t, query.Required)
	assert.Equal(t, huma.TypeString, query.Schema.Type)
	require.NotNil(t, query.Schema.MinLength)
	assert.Equal(t, 1, *query.Schema.MinLength)
	require.NotNil(t, query.Schema.MaxLength)
	assert.Equal(t, 2000, *query.Schema.MaxLength)

	assert.Equal(t, []any{"auto", "lexical", "hybrid", "semantic"}, parameters["mode"].Schema.Enum)
	assert.Equal(t, "auto", parameters["mode"].Schema.Default)
	assert.Equal(t, []any{"pass", "fail"}, parameters["verdict"].Schema.Enum)
	assert.Equal(t, []any{"all", "open", "closed"}, parameters["state"].Schema.Enum)
	assert.Equal(t, "all", parameters["state"].Schema.Default)

	limit := parameters["limit"]
	assert.Equal(t, huma.TypeInteger, limit.Schema.Type)
	assert.Equal(t, 20, limit.Schema.Default)
	require.NotNil(t, limit.Schema.Minimum)
	assert.InDelta(t, 1, *limit.Schema.Minimum, 0)
	require.NotNil(t, limit.Schema.Maximum)
	assert.InDelta(t, 100, *limit.Schema.Maximum, 0)

	for _, name := range []string{"repo", "branch", "since"} {
		assert.Equal(t, huma.TypeString, parameters[name].Schema.Type)
		assert.False(t, parameters[name].Required)
	}
}

func TestSearchEndpointValidatesParameters(t *testing.T) {
	tests := []struct {
		name   string
		values url.Values
		detail string
	}{
		{name: "missing query", values: url.Values{}, detail: "q is required"},
		{name: "blank query", values: url.Values{"q": {" \t"}}, detail: "q is required"},
		{name: "query rune limit", values: url.Values{"q": {strings.Repeat("界", 2001)}}, detail: "q must be at most 2000 runes"},
		{name: "mode", values: url.Values{"q": {"needle"}, "mode": {"vector"}}, detail: "invalid mode"},
		{name: "state", values: url.Values{"q": {"needle"}, "state": {"active"}}, detail: "invalid state"},
		{name: "verdict", values: url.Values{"q": {"needle"}, "verdict": {"maybe"}}, detail: "invalid verdict"},
		{name: "zero limit", values: url.Values{"q": {"needle"}, "limit": {"0"}}, detail: "limit must be between 1 and 100"},
		{name: "large limit", values: url.Values{"q": {"needle"}, "limit": {"101"}}, detail: "limit must be between 1 and 100"},
		{name: "since", values: url.Values{"q": {"needle"}, "since": {"yesterday"}}, detail: "since must be a Go duration or RFC3339 timestamp"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := newSearchHTTPServer(t, &recordingReviewSearcher{})
			response := executeSearch(server, tc.values)
			assert.Equal(t, http.StatusBadRequest, response.Code)
			assert.Contains(t, response.Body.String(), tc.detail)
		})
	}
}

func TestSearchEndpointParsesDefaultsAndSinceForms(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	searcher := &recordingReviewSearcher{result: searchindex.SearchResult{Hits: []searchindex.SearchHit{}}}
	server := newSearchHTTPServer(t, searcher)
	server.searchNow = func() time.Time { return now }

	response := executeSearch(server, url.Values{"q": {"  needle  "}, "since": {"24h"}})
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	params := searcher.lastParams(t)
	assert.Equal(t, "needle", params.Query)
	assert.Equal(t, searchindex.ModeAuto, params.Mode)
	assert.Equal(t, searchindex.StateAll, params.State)
	assert.Equal(t, 20, params.Limit)
	require.NotNil(t, params.Since)
	assert.Equal(t, now.Add(-24*time.Hour), *params.Since)

	timestamp := "2026-09-01T03:04:05Z"
	response = executeSearch(server, url.Values{
		"q": {"needle"}, "since": {timestamp}, "mode": {"lexical"},
		"state": {"open"}, "verdict": {"fail"}, "branch": {"refs/heads/Main"},
		"limit": {"7"},
	})
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	params = searcher.lastParams(t)
	expectedSince, err := time.Parse(time.RFC3339, timestamp)
	require.NoError(t, err)
	assert.Equal(t, expectedSince, *params.Since)
	assert.Equal(t, "refs/heads/Main", params.Branch)
	assert.Equal(t, "fail", params.Verdict)
	assert.Equal(t, searchindex.StateOpen, params.State)
	assert.Equal(t, 7, params.Limit)
}

func TestSearchEndpointResolvesRepositoryAndKeepsGlobalScope(t *testing.T) {
	db := testutil.OpenTestDB(t)
	repo, err := db.GetOrCreateRepo("/src/example", "github.com/example/repo")
	require.NoError(t, err)
	searcher := &recordingReviewSearcher{result: searchindex.SearchResult{Hits: []searchindex.SearchHit{}}}
	server := newServerWithLogs(
		db, config.DefaultConfig(), "", newTestErrorLog(), newTestActivityLog(),
	)
	server.search = searcher
	t.Cleanup(func() { _ = server.Close() })

	for _, identifier := range []string{repo.RootPath, repo.Name, repo.Identity} {
		response := executeSearch(server, url.Values{"q": {"needle"}, "repo": {identifier}})
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		assert.Equal(t, repo.ID, searcher.lastParams(t).RepoID)
	}

	response := executeSearch(server, url.Values{"q": {"needle"}})
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.Zero(t, searcher.lastParams(t).RepoID)

	response = executeSearch(server, url.Values{"q": {"needle"}, "repo": {"missing"}})
	assert.Equal(t, http.StatusBadRequest, response.Code)
	assert.Contains(t, response.Body.String(), "repository not found")
}

func TestResolveSearchRepoSentinelsPreserveLookupCause(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	db := testutil.OpenTestDB(t)
	server := newServerWithLogs(
		db, config.DefaultConfig(), "", newTestErrorLog(), newTestActivityLog(),
	)
	t.Cleanup(func() { _ = server.Close() })

	repoID, err := server.resolveSearchRepo("missing")
	assert.Zero(repoID)
	require.Error(err)
	require.ErrorIs(err, errSearchRepoNotFound)

	repoID, err = server.resolveSearchRepo("")
	assert.Zero(repoID)
	assert.NoError(err)

	// Storage failures wrap the lookup sentinel and keep the cause so
	// server-side logs can record what actually failed.
	require.NoError(db.Close())
	repoID, err = server.resolveSearchRepo("/src/example")
	assert.Zero(repoID)
	require.Error(err)
	require.ErrorIs(err, errSearchRepoLookup)
	require.NotErrorIs(err, errSearchRepoNotFound)
	assert.ErrorContains(err, "sql: database is closed")
}

func TestSearchEndpointMapsRepoLookupFailureToFixed500(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	db := testutil.OpenTestDB(t)
	server := newServerWithLogs(
		db, config.DefaultConfig(), "", newTestErrorLog(), newTestActivityLog(),
	)
	server.search = &recordingReviewSearcher{}
	t.Cleanup(func() { _ = server.Close() })

	// An unknown repository keeps the fixed 400 response.
	response := executeSearch(server, url.Values{"q": {"needle"}, "repo": {"missing"}})
	require.Equal(http.StatusBadRequest, response.Code)
	assert.Contains(response.Body.String(), "repository not found")

	// Storage failures behind the repository lookup are logged server-side
	// and reported as a fixed 500 without leaking database details.
	require.NoError(db.Close())
	response = executeSearch(server, url.Values{"q": {"needle"}, "repo": {"/src/example"}})
	require.Equal(http.StatusInternalServerError, response.Code)
	assert.Contains(response.Body.String(), "repository lookup failed")
	assert.NotContains(response.Body.String(), "sql: database is closed")
	assert.NotContains(response.Body.String(), "resolve repository")

	entries := server.errorLog.Recent()
	require.NotEmpty(entries, "lookup failure is logged server-side")
	assert.Contains(entries[0].Message, "sql: database is closed")
}

func TestSearchEndpointPreservesResponseAndEmptyHits(t *testing.T) {
	backlog := int64(3)
	finished := time.Date(2026, 9, 14, 10, 30, 0, 0, time.UTC)
	want := searchindex.SearchResult{
		Query: "needle", Mode: searchindex.ModeHybrid,
		Degraded: true, DegradedReason: searchindex.ReasonSemanticCeiling,
		Bounded: true, BoundedReason: searchindex.ReasonSemanticCeiling, Partial: true,
		Coverage: searchindex.SearchCoverage{
			MirrorComplete: true, MirrorBacklog: &backlog, EmbeddingsConfigured: true,
			VectorState: searchindex.VectorReplacing, EmbeddingBacklog: 9, Skipped: 2,
		},
		Hits: []searchindex.SearchHit{{
			JobID: 11, JobUUID: "job-uuid", ReviewID: 12, ReviewUUID: "review-uuid",
			RepoName: "repo", RepoPath: "/src/repo", GitRef: "abc123", CommitSHA: "abc123",
			CommitSubject: "fix search", Branch: "main", ReviewType: "security",
			PanelRole: "member", Agent: "test", Verdict: "fail", Closed: true,
			FinishedAt: finished, Score: 0.75,
			MatchedIn: []string{searchindex.MatchLexical, searchindex.MatchSemantic},
			Excerpt:   "matching excerpt",
		}},
	}
	searcher := &recordingReviewSearcher{result: want}
	server := newSearchHTTPServer(t, searcher)

	response := executeSearch(server, url.Values{"q": {"needle"}})
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var got SearchResponse
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &got))
	assert.Equal(t, searchResponseFromResult(want), got)

	searcher.result = searchindex.SearchResult{Query: "absent", Mode: searchindex.ModeLexical}
	response = executeSearch(server, url.Values{"q": {"absent"}})
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var raw map[string]any
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &raw))
	hits, ok := raw["hits"].([]any)
	require.True(t, ok)
	assert.Empty(t, hits)
}

func TestSearchEndpointMapsSanitizedModeErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		reason string
	}{
		{name: "unconfigured", status: http.StatusBadRequest, reason: searchindex.ReasonEmbeddingsUnconfigured},
		{name: "unavailable", status: http.StatusServiceUnavailable, reason: searchindex.ReasonSemanticUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			searcher := &recordingReviewSearcher{err: &searchindex.ModeError{
				Status: tc.status, Reason: tc.reason, Err: errors.New("provider body secret-value"),
			}}
			server := newSearchHTTPServer(t, searcher)
			response := executeSearch(server, url.Values{"q": {"needle"}, "mode": {"semantic"}})
			assert.Equal(t, tc.status, response.Code)
			assert.Contains(t, response.Body.String(), tc.reason)
			assert.NotContains(t, response.Body.String(), "secret-value")
		})
	}
}

func TestSearchReconcilerLifecycleAndEventWakes(t *testing.T) {
	server := setupTestServer(t)
	reconciler := newRecordingSearchReconciler()
	server.searchReconciler = reconciler
	initialSubscribers := server.broadcaster.SubscriberCount()

	server.startSearch(t.Context())
	require.Eventually(t, func() bool {
		select {
		case <-reconciler.started:
			return true
		default:
			return false
		}
	}, time.Second, time.Millisecond)
	assert.Equal(t, initialSubscribers+1, server.broadcaster.SubscriberCount())

	for _, eventType := range []string{
		"review.completed", "review.closed", "review.reopened", "review.remapped", "review.commented",
	} {
		server.broadcaster.Broadcast(Event{Type: eventType})
	}
	server.broadcaster.Broadcast(Event{Type: "review.started"})

	for range 5 {
		select {
		case <-reconciler.wakes:
		case <-time.After(time.Second):
			require.FailNow(t, "expected search reconciler wake")
		}
	}
	select {
	case <-reconciler.wakes:
		require.FailNow(t, "unexpected wake for unrelated event")
	case <-time.After(10 * time.Millisecond):
	}

	server.stopSearch()
	select {
	case <-reconciler.stopped:
	case <-time.After(time.Second):
		require.FailNow(t, "search reconciler did not stop")
	}
	assert.Equal(t, initialSubscribers, server.broadcaster.SubscriberCount())
}

func TestSearchLifecycleIgnoresRepeatedStartAndJoinsRepeatedStop(t *testing.T) {
	server := setupTestServer(t)
	reconciler := newCountingSearchReconciler()
	server.searchReconciler = reconciler
	initialSubscribers := server.broadcaster.SubscriberCount()

	server.startSearch(t.Context())
	select {
	case <-reconciler.starts:
	case <-time.After(time.Second):
		require.FailNow(t, "search reconciler did not start")
	}
	server.searchMu.Lock()
	firstCancel := server.searchCancel
	server.searchMu.Unlock()

	server.startSearch(t.Context())
	assert.Equal(t, initialSubscribers+1, server.broadcaster.SubscriberCount())
	// Also releases the overwritten first run in the broken implementation so
	// the regression reports the duplicate start instead of hanging.
	firstCancel()

	stopped := make(chan struct{})
	go func() {
		server.stopSearch()
		server.stopSearch()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		require.FailNow(t, "repeated search stop hung")
	}

	started, stopCount := reconciler.counts()
	assert.Equal(t, 1, started)
	assert.Equal(t, 1, stopCount)
}

func TestSearchLifecycleSerializesOverlappingStartAndStop(t *testing.T) {
	server := setupTestServer(t)
	reconciler := newCountingSearchReconciler()
	server.searchReconciler = reconciler
	initialSubscribers := server.broadcaster.SubscriberCount()
	broadcaster := &blockingSearchBroadcaster{
		Broadcaster: server.broadcaster,
		entered:     make(chan struct{}),
		release:     make(chan struct{}),
	}
	server.broadcaster = broadcaster

	started := make(chan struct{})
	go func() {
		server.startSearch(t.Context())
		close(started)
	}()
	select {
	case <-broadcaster.entered:
	case <-time.After(time.Second):
		require.FailNow(t, "search start did not enter subscription")
	}

	stopped := make(chan struct{})
	go func() {
		server.stopSearch()
		close(stopped)
	}()

	close(broadcaster.release)
	select {
	case <-started:
	case <-time.After(time.Second):
		require.FailNow(t, "search start did not finish")
	}
	select {
	case <-stopped:
	case <-time.After(time.Second):
		require.FailNow(t, "search stop did not join the reconciler")
	}

	startedCount, stoppedCount := reconciler.counts()
	assert.Equal(t, 1, startedCount)
	assert.Equal(t, 1, stoppedCount)
	assert.Equal(t, initialSubscribers, server.broadcaster.SubscriberCount())
	// Cleans the active run in the broken implementation and is a repeated
	// no-op after the fixed stop has joined it.
	server.stopSearch()

	server.startSearch(t.Context())
	assert.Equal(t, initialSubscribers, server.broadcaster.SubscriberCount(),
		"search must not subscribe after shutdown")
	startedCount, _ = reconciler.counts()
	assert.Equal(t, 1, startedCount, "search must not start after shutdown")
}
