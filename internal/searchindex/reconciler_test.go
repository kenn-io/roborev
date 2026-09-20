package searchindex

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/vector"

	"go.kenn.io/roborev/internal/embedding"
	"go.kenn.io/roborev/internal/searchdoc"
	"go.kenn.io/roborev/internal/storage"
)

type reconcilerStore struct {
	mu      sync.Mutex
	sources []storage.SearchReviewSource
	calls   []feedCall
	err     error
}

type feedCall struct {
	after int64
	limit int
}

func (s *reconcilerStore) ListSearchDocuments(_ context.Context, after int64, limit int) ([]storage.SearchReviewSource, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, feedCall{after: after, limit: limit})
	if s.err != nil {
		return nil, s.err
	}
	result := make([]storage.SearchReviewSource, 0, limit)
	for _, source := range s.sources {
		if source.ReviewID > after {
			result = append(result, source)
			if len(result) == limit {
				break
			}
		}
	}
	return result, nil
}

func (s *reconcilerStore) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

type reconcilerEmbedder struct {
	model     vector.Generation
	batchSize int
	mu        sync.Mutex
	calls     [][]string
	embed     func(context.Context, []string) ([][]float32, error)
}

var _ Embedder = (*embedding.Client)(nil)

func TestEmbeddingClientSatisfiesReconcilerBatchContract(t *testing.T) {
	client, err := embedding.New(embedding.Config{
		BaseURL: "http://127.0.0.1:9", Model: "model", Dims: 2, BatchSize: 7,
	})
	require.NoError(t, err)
	assert.Equal(t, 7, client.BatchSize())
}

func (e *reconcilerEmbedder) Embed(ctx context.Context, kind embedding.InputKind, texts []string) ([][]float32, error) {
	if kind != embedding.InputDocument {
		return nil, fmt.Errorf("unexpected input kind %q", kind)
	}
	e.mu.Lock()
	e.calls = append(e.calls, append([]string(nil), texts...))
	e.mu.Unlock()
	if e.embed != nil {
		return e.embed(ctx, texts)
	}
	result := make([][]float32, len(texts))
	for i := range result {
		result[i] = make([]float32, e.model.Dimensions)
		result[i][0] = 1
	}
	return result, nil
}

func (e *reconcilerEmbedder) Generation() vector.Generation { return e.model }
func (e *reconcilerEmbedder) BatchSize() int                { return e.batchSize }
func (e *reconcilerEmbedder) callCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.calls)
}

func TestReconcilerWakeCoalescesAndRunScansOnStartup(t *testing.T) {
	index := openGenerationTestIndex(t)
	store := &reconcilerStore{}
	r := NewReconciler(store, index, nil, ReconcilerConfig{SweepInterval: time.Hour})

	for range 100 {
		r.Wake()
	}
	assert.Len(t, r.wake, 1)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	require.Eventually(t, func() bool { return store.callCount() > 0 }, time.Second, time.Millisecond)
	cancel()
	assert.ErrorIs(t, <-done, context.Canceled)
}

func TestReconcilerMirrorPagesAreCappedAt500(t *testing.T) {
	index := openGenerationTestIndex(t)
	store := &reconcilerStore{sources: makeSearchSources(700)}
	r := NewReconciler(store, index, nil, ReconcilerConfig{MirrorPageSize: 900})

	more, err := r.reconcileTurn(context.Background())
	require.NoError(t, err)
	assert.True(t, more)
	require.NotEmpty(t, store.calls)
	assert.Equal(t, 500, store.calls[0].limit)
	assert.Equal(t, int64(500), r.Health().Indexed)
	assert.False(t, r.Health().MirrorComplete)
	assert.Nil(t, r.Health().MirrorBacklog,
		"the remaining canonical row count is unknown during a bounded scan")

	_, err = r.reconcileTurn(context.Background())
	require.NoError(t, err)
	assert.Equal(t, int64(700), r.Health().Indexed)
	assert.True(t, r.Health().MirrorComplete)
	require.NotNil(t, r.Health().MirrorBacklog)
	assert.Zero(t, *r.Health().MirrorBacklog)
}

func TestReconcilerRefreshesMirrorBeforeEachFourBatchFillTurn(t *testing.T) {
	index := openGenerationTestIndex(t)
	store := &reconcilerStore{sources: makeSearchSources(8)}
	embedder := &reconcilerEmbedder{
		model: vector.Generation{Model: "model", Dimensions: 2}, batchSize: 1,
	}
	embedder.embed = func(_ context.Context, texts []string) ([][]float32, error) {
		assert.GreaterOrEqual(t, store.callCount(), 1,
			"mirror work must run before the bounded vector turn")
		return [][]float32{{1, 0}}, nil
	}
	r := NewReconciler(store, index, embedder, ReconcilerConfig{MaxFillBatches: 9, MaxFillTime: time.Hour})

	more, err := r.reconcileTurn(context.Background())
	require.NoError(t, err)
	assert.True(t, more)
	assert.Equal(t, 4, embedder.callCount(), "a turn must yield after four provider calls")
	assert.Equal(t, int64(4), r.Health().Embedded)
	assert.Equal(t, int64(4), r.Health().EmbeddingBacklog)
	assert.Empty(t, r.Health().ActiveGeneration)

	_, err = r.reconcileTurn(context.Background())
	require.NoError(t, err)
	assert.GreaterOrEqual(t, store.callCount(), 2,
		"the next bounded vector turn must refresh mirror work again")
	assert.Equal(t, 8, embedder.callCount())
	assert.Equal(t, embedder.model.Fingerprint(), r.Health().ActiveGeneration)
	assert.Equal(t, int64(0), r.Health().EmbeddingBacklog)
}

func TestReconcilerKeepsMatchingActiveGenerationAvailableDuringIncrementalFill(t *testing.T) {
	ctx := context.Background()
	index := openGenerationTestIndex(t)
	store := &reconcilerStore{sources: makeSearchSources(1)}
	embedder := &reconcilerEmbedder{
		model: vector.Generation{Model: "model", Dimensions: 2}, batchSize: 1,
	}
	r := NewReconciler(store, index, embedder, ReconcilerConfig{})

	more, err := r.reconcileTurn(ctx)
	require.NoError(t, err)
	assert.False(t, more)
	key := embedder.model.Fingerprint()
	assert.Equal(t, key, r.Health().ActiveGeneration)

	store.mu.Lock()
	store.sources = makeSearchSources(7)
	store.mu.Unlock()
	more, err = r.reconcileTurn(ctx)
	require.NoError(t, err)
	assert.True(t, more)
	health := r.Health()
	assert.Equal(t, "ready", health.VectorState)
	assert.Equal(t, key, health.ActiveGeneration)
	assert.Equal(t, int64(5), health.Embedded)
	assert.Equal(t, int64(2), health.EmbeddingBacklog)
	available, err := index.GenerationAvailable(ctx, key)
	require.NoError(t, err)
	assert.True(t, available)

	embedder.model = vector.Generation{Model: "replacement", Dimensions: 2}
	replacement := embedder.model.Fingerprint()
	more, err = r.reconcileTurn(ctx)
	require.NoError(t, err)
	assert.True(t, more)
	health = r.Health()
	assert.Equal(t, "building", health.VectorState)
	assert.Equal(t, key, health.ActiveGeneration)
	assert.Equal(t, int64(3), health.EmbeddingBacklog)
	available, err = index.GenerationAvailable(ctx, replacement)
	require.NoError(t, err)
	assert.False(t, available)
}

func TestReconcilerBoundsActualProviderCallsForOversizedDocument(t *testing.T) {
	var providerCalls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		providerCalls.Add(1)
		_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[1,0]}]}`))
	}))
	defer server.Close()
	client, err := embedding.New(embedding.Config{
		BaseURL: server.URL, Model: "model", Dims: 2, BatchSize: 1,
	})
	require.NoError(t, err)
	index := openGenerationTestIndex(t)
	sources := makeSearchSources(1)
	sources[0].Output = strings.Repeat("x", searchChunkRunes+6*(searchChunkRunes-searchChunkOverlap))
	wantProviderCalls := len(vector.Split(searchdoc.Render(sources[0]).Content,
		vector.SplitOptions{MaxRunes: searchChunkRunes, Overlap: searchChunkOverlap}))
	r := NewReconciler(&reconcilerStore{sources: sources}, index, client, ReconcilerConfig{})

	more, err := r.reconcileTurn(context.Background())
	require.NoError(t, err)
	assert.False(t, more)
	assert.Equal(t, int64(wantProviderCalls), providerCalls.Load())
	assert.Equal(t, int64(0), r.Health().EmbeddingBacklog)
	assert.Equal(t, client.Generation().Fingerprint(), r.Health().ActiveGeneration)
}

func TestReconcilerAttachesFillDeadlineToProviderContext(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		index := openGenerationTestIndex(t)
		store := &reconcilerStore{sources: makeSearchSources(1)}
		embedder := &reconcilerEmbedder{model: vector.Generation{Model: "model", Dimensions: 2}, batchSize: 1}
		embedder.embed = func(ctx context.Context, _ []string) ([][]float32, error) {
			_, ok := ctx.Deadline()
			require.True(t, ok)
			<-ctx.Done()
			return nil, ctx.Err()
		}
		r := NewReconciler(store, index, embedder, ReconcilerConfig{MaxFillTime: 20 * time.Millisecond})

		started := time.Now()
		_, err := r.reconcileTurn(context.Background())
		require.ErrorIs(t, err, context.DeadlineExceeded)
		assert.Equal(t, 20*time.Millisecond, time.Since(started))
	})
}

func TestReconcilerHonorsRetryAfterAndUsesFixedDefinitiveErrors(t *testing.T) {
	r := NewReconciler(&reconcilerStore{}, nil, nil, ReconcilerConfig{
		MinBackoff: time.Second, MaxBackoff: 5 * time.Minute,
	})

	transient := &embedding.APIError{StatusCode: http.StatusTooManyRequests, RetryAfter: 17 * time.Second}
	assert.Equal(t, 17*time.Second, r.backoffFor(transient))
	definitive := &embedding.APIError{StatusCode: http.StatusUnauthorized}
	assert.Equal(t, 5*time.Minute, r.backoffFor(definitive))

	r.recordError(definitive)
	health := r.Health()
	assert.Equal(t, "authentication", health.LastError)
	assert.Equal(t, http.StatusUnauthorized, health.LastErrorStatus)
	assert.NotContains(t, fmt.Sprintf("%+v", health), "endpoint")
}

func TestReconcilerWakeDoesNotBypassProviderBackoff(t *testing.T) {
	tests := []struct {
		name   string
		first  error
		config ReconcilerConfig
	}{
		{
			name:   "retry after",
			first:  &embedding.APIError{StatusCode: http.StatusTooManyRequests, RetryAfter: 80 * time.Millisecond},
			config: ReconcilerConfig{MinBackoff: time.Millisecond, MaxBackoff: 100 * time.Millisecond},
		},
		{
			name:   "definitive",
			first:  &embedding.APIError{StatusCode: http.StatusUnauthorized},
			config: ReconcilerConfig{MinBackoff: time.Millisecond, MaxBackoff: 80 * time.Millisecond},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				index := openGenerationTestIndex(t)
				store := &reconcilerStore{sources: makeSearchSources(1)}
				firstCall := make(chan struct{})
				var count atomic.Int64
				embedder := &reconcilerEmbedder{model: vector.Generation{Model: "model", Dimensions: 2}, batchSize: 1}
				embedder.embed = func(_ context.Context, _ []string) ([][]float32, error) {
					if count.Add(1) == 1 {
						close(firstCall)
						return nil, tt.first
					}
					return [][]float32{{1, 0}}, nil
				}
				tt.config.SweepInterval = time.Hour
				r := NewReconciler(store, index, embedder, tt.config)
				ctx, cancel := context.WithCancel(context.Background())
				done := make(chan error, 1)
				go func() { done <- r.Run(ctx) }()
				<-firstCall
				for range 10 {
					r.Wake()
				}
				time.Sleep(40 * time.Millisecond)
				synctest.Wait()
				assert.Equal(t, int64(1), count.Load())
				time.Sleep(40 * time.Millisecond)
				synctest.Wait()
				assert.Equal(t, int64(2), count.Load())
				cancel()
				assert.ErrorIs(t, <-done, context.Canceled)
			})
		})
	}
}

func TestReconcilerRequestDeadlineRetriesUntilParentCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		index := openGenerationTestIndex(t)
		store := &reconcilerStore{sources: makeSearchSources(1)}
		second := make(chan struct{})
		var calls atomic.Int64
		embedder := &reconcilerEmbedder{model: vector.Generation{Model: "model", Dimensions: 2}, batchSize: 1}
		embedder.embed = func(_ context.Context, _ []string) ([][]float32, error) {
			if calls.Add(1) == 1 {
				return nil, fmt.Errorf("embedding request failed: %w", context.DeadlineExceeded)
			}
			close(second)
			return [][]float32{{1, 0}}, nil
		}
		r := NewReconciler(store, index, embedder, ReconcilerConfig{
			MinBackoff: time.Millisecond, MaxBackoff: 2 * time.Millisecond, SweepInterval: time.Hour,
		})
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- r.Run(ctx) }()
		<-second
		assert.Equal(t, int64(2), calls.Load())
		cancel()
		assert.ErrorIs(t, <-done, context.Canceled)
	})
}

func TestReconcilerRejectsEmbedderCardinalityMismatchWithoutPanic(t *testing.T) {
	for _, tt := range []struct {
		name    string
		vectors [][]float32
	}{
		{name: "short", vectors: nil},
		{name: "extra", vectors: [][]float32{{1, 0}, {0, 1}}},
		{name: "wrong dimensions", vectors: [][]float32{{1}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			index := openGenerationTestIndex(t)
			embedder := &reconcilerEmbedder{model: vector.Generation{Model: "model", Dimensions: 2}, batchSize: 1}
			embedder.embed = func(_ context.Context, _ []string) ([][]float32, error) {
				return tt.vectors, nil
			}
			r := NewReconciler(&reconcilerStore{sources: makeSearchSources(1)}, index, embedder, ReconcilerConfig{})
			var reconcileErr error
			assert.NotPanics(t, func() {
				_, reconcileErr = r.reconcileTurn(context.Background())
			})
			require.Error(t, reconcileErr)
			r.recordError(reconcileErr)
			assert.Equal(t, "provider", r.Health().LastError)
		})
	}
}

func TestReconcilerSkipsOnlyProvenContentSpecific400(t *testing.T) {
	index := openGenerationTestIndex(t)
	store := &reconcilerStore{sources: makeSearchSources(1)}
	embedder := &reconcilerEmbedder{model: vector.Generation{Model: "model", Dimensions: 2}, batchSize: 1}
	embedder.embed = func(_ context.Context, texts []string) ([][]float32, error) {
		if texts[0] == benignChunkText(texts[0]) {
			return [][]float32{{1, 0}}, nil
		}
		return nil, &embedding.APIError{StatusCode: http.StatusBadRequest}
	}
	r := NewReconciler(store, index, embedder, ReconcilerConfig{})

	_, err := r.reconcileTurn(context.Background())
	require.NoError(t, err)
	health := r.Health()
	assert.Equal(t, int64(1), health.Skipped)
	assert.Equal(t, int64(0), health.EmbeddingBacklog)
	assert.Equal(t, embedder.model.Fingerprint(), health.ActiveGeneration)
	assert.Equal(t, 0, generationVectorCount(t, index, embedder.model.Fingerprint()))
}

func TestReconcilerDoesNotSkipAuthenticationOrUnproven400(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			index := openGenerationTestIndex(t)
			store := &reconcilerStore{sources: makeSearchSources(1)}
			embedder := &reconcilerEmbedder{model: vector.Generation{Model: "model", Dimensions: 2}, batchSize: 1}
			embedder.embed = func(_ context.Context, _ []string) ([][]float32, error) {
				return nil, &embedding.APIError{StatusCode: status}
			}
			r := NewReconciler(store, index, embedder, ReconcilerConfig{})

			_, err := r.reconcileTurn(context.Background())
			require.Error(t, err)
			assert.Equal(t, int64(0), r.Health().Skipped)
			assert.Equal(t, int64(1), r.Health().EmbeddingBacklog)
		})
	}
}

func TestReconcilerHealthIsImmutableAndReportsProgressRateETA(t *testing.T) {
	index := openGenerationTestIndex(t)
	store := &reconcilerStore{sources: makeSearchSources(2)}
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	embedder := &reconcilerEmbedder{model: vector.Generation{Model: "model", Dimensions: 2}, batchSize: 1}
	r := NewReconciler(store, index, embedder, ReconcilerConfig{Now: func() time.Time { return now }, MaxFillBatches: 1})

	_, err := r.reconcileTurn(context.Background())
	require.NoError(t, err)
	first := r.Health()
	require.NotNil(t, first.MirrorBacklog)
	require.NotNil(t, first.LastSuccessAt)
	require.NotNil(t, first.LastProgressAt)
	require.NotNil(t, first.RatePerSecond)
	require.NotNil(t, first.ETASeconds)
	assert.Equal(t, int64(1), first.Embedded)
	assert.Equal(t, int64(1), first.EmbeddingBacklog)

	*first.LastSuccessAt = time.Time{}
	*first.RatePerSecond = 999
	*first.MirrorBacklog = 999
	second := r.Health()
	assert.Equal(t, now, *second.LastSuccessAt)
	assert.NotEqual(t, float64(999), *second.RatePerSecond)
	assert.Zero(t, *second.MirrorBacklog)
}

func TestReconcilerCancellationInterruptsProviderCall(t *testing.T) {
	index := openGenerationTestIndex(t)
	store := &reconcilerStore{sources: makeSearchSources(1)}
	started := make(chan struct{})
	embedder := &reconcilerEmbedder{model: vector.Generation{Model: "model", Dimensions: 2}, batchSize: 1}
	embedder.embed = func(ctx context.Context, _ []string) ([][]float32, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	r := NewReconciler(store, index, embedder, ReconcilerConfig{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	<-started
	cancel()
	assert.ErrorIs(t, <-done, context.Canceled)
}

func TestReconcilerMirrorFailureLeavesFixedHealthCategory(t *testing.T) {
	index := openGenerationTestIndex(t)
	store := &reconcilerStore{err: errors.New("canonical output should not be retained")}
	r := NewReconciler(store, index, nil, ReconcilerConfig{})
	_, err := r.reconcileTurn(context.Background())
	require.Error(t, err)
	r.recordError(err)
	assert.Equal(t, "mirror", r.Health().LastError)
}

func TestReconcilerErrorsPreserveMatchingActiveGeneration(t *testing.T) {
	ctx := context.Background()
	index := openGenerationTestIndex(t)
	sources := makeSearchSources(1)
	doc := searchdoc.Render(sources[0])
	_, err := index.RefreshMirrorPage(ctx, []searchdoc.Document{doc}, nil)
	require.NoError(t, err)
	embedder := &reconcilerEmbedder{model: vector.Generation{Model: "model", Dimensions: 2}, batchSize: 1}
	key, err := index.EnsureGeneration(ctx, embedder.model)
	require.NoError(t, err)
	pending, err := index.PendingGeneration(ctx, key, 1)
	require.NoError(t, err)
	require.NoError(t, index.SaveGenerationVectors(ctx, key, pending[0],
		[]vector.ChunkVector{{ChunkIndex: 0, Vector: vector.Vector{1, 0}}}))
	require.NoError(t, index.ActivateGeneration(ctx, key))

	r := NewReconciler(&reconcilerStore{err: errors.New("canonical read unavailable")}, index, embedder, ReconcilerConfig{})
	_, err = r.reconcileTurn(ctx)
	require.Error(t, err)
	r.recordError(err)
	health := r.Health()
	assert.Equal(t, "mirror", health.LastError)
	assert.Equal(t, key, health.ActiveGeneration)
	assert.Equal(t, "ready", health.VectorState)
}

func TestReconcilerErrorsPreserveDifferentActiveGenerationIdentity(t *testing.T) {
	ctx := context.Background()
	index := openGenerationTestIndex(t)
	sources := makeSearchSources(1)
	doc := searchdoc.Render(sources[0])
	_, err := index.RefreshMirrorPage(ctx, []searchdoc.Document{doc}, nil)
	require.NoError(t, err)
	activeModel := vector.Generation{Model: "active", Dimensions: 2}
	seedActiveGeneration(t, index, activeModel, map[string][]vector.ChunkVector{
		doc.DocKey: {{ChunkIndex: 0, Vector: vector.Vector{1, 0}}},
	})
	replacement := &reconcilerEmbedder{
		model: vector.Generation{Model: "replacement", Dimensions: 2}, batchSize: 1,
	}
	r := NewReconciler(
		&reconcilerStore{err: errors.New("canonical read unavailable")},
		index,
		replacement,
		ReconcilerConfig{},
	)

	_, err = r.reconcileTurn(ctx)
	require.Error(t, err)
	r.recordError(err)
	health := r.Health()
	assert.Equal(t, "error", health.VectorState)
	assert.Equal(t, replacement.model.Fingerprint(), health.Generation)
	assert.Equal(t, activeModel.Fingerprint(), health.ActiveGeneration)
}

func makeSearchSources(count int) []storage.SearchReviewSource {
	result := make([]storage.SearchReviewSource, count)
	for i := range result {
		id := int64(i + 1)
		result[i] = storage.SearchReviewSource{
			ReviewID: id, JobID: 1000 + id, RepoID: 7,
			ReviewUUID: fmt.Sprintf("00000000-0000-4000-8000-%012d", id),
			JobUUID:    fmt.Sprintf("10000000-0000-4000-8000-%012d", id),
			RepoName:   "example/repo", GitRef: "HEAD", ReviewType: "review",
			Output: fmt.Sprintf("review output %d", id),
		}
	}
	return result
}
