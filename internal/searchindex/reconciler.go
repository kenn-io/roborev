package searchindex

import (
	"context"
	"errors"
	"math"
	"net/http"
	"sync"
	"time"

	"go.kenn.io/kit/vector"

	"go.kenn.io/roborev/internal/embedding"
	"go.kenn.io/roborev/internal/searchdoc"
	"go.kenn.io/roborev/internal/storage"
)

const (
	defaultMirrorPageSize = 500
	defaultMaxFillBatches = 4
	defaultMaxFillTime    = 30 * time.Second
	defaultSweepInterval  = 5 * time.Minute
	defaultMinBackoff     = time.Second
	defaultMaxBackoff     = 5 * time.Minute
	searchChunkRunes      = 2000
	searchChunkOverlap    = 200
)

type searchDocumentStore interface {
	ListSearchDocuments(context.Context, int64, int) ([]storage.SearchReviewSource, error)
}

// Embedder is the storage-free document embedding contract.
type Embedder interface {
	Embed(context.Context, embedding.InputKind, []string) ([][]float32, error)
	Generation() vector.Generation
	BatchSize() int
}

// ReconcilerConfig bounds each background reconciliation turn.
type ReconcilerConfig struct {
	MirrorPageSize int
	// MaxFillBatches is the maximum number of pending documents kit Fill may
	// start in one turn. Kit completes every started document, including
	// those that span multiple provider calls.
	MaxFillBatches int
	MaxFillTime    time.Duration
	SweepInterval  time.Duration
	MinBackoff     time.Duration
	MaxBackoff     time.Duration
	Now            func() time.Time
}

// Reconciler keeps the disposable search sidecar current with canonical data.
type Reconciler struct {
	store    searchDocumentStore
	index    *Index
	embedder Embedder
	config   ReconcilerConfig
	wake     chan struct{}

	mu                 sync.Mutex
	health             HealthSnapshot
	mirrorCursor       int64
	mirrorSeen         map[string]struct{}
	generationStarted  time.Time
	generationBaseline int64
	failures           int
}

// NewReconciler constructs a bounded, wake-coalescing reconciler.
func NewReconciler(store searchDocumentStore, index *Index, embedder Embedder, config ReconcilerConfig) *Reconciler {
	config = normalizeReconcilerConfig(config)
	state := HealthSnapshot{
		EmbeddingsConfigured: embedder != nil,
		VectorState:          "unconfigured",
	}
	if embedder != nil {
		state.VectorState = "building"
		state.Generation = embedder.Generation().Fingerprint()
	}
	return &Reconciler{
		store: store, index: index, embedder: embedder, config: config,
		wake: make(chan struct{}, 1), health: state,
	}
}

func normalizeReconcilerConfig(config ReconcilerConfig) ReconcilerConfig {
	if config.MirrorPageSize <= 0 || config.MirrorPageSize > defaultMirrorPageSize {
		config.MirrorPageSize = defaultMirrorPageSize
	}
	if config.MaxFillBatches <= 0 || config.MaxFillBatches > defaultMaxFillBatches {
		config.MaxFillBatches = defaultMaxFillBatches
	}
	if config.MaxFillTime <= 0 || config.MaxFillTime > defaultMaxFillTime {
		config.MaxFillTime = defaultMaxFillTime
	}
	if config.SweepInterval <= 0 {
		config.SweepInterval = defaultSweepInterval
	}
	if config.MinBackoff <= 0 {
		config.MinBackoff = defaultMinBackoff
	}
	if config.MaxBackoff <= 0 {
		config.MaxBackoff = defaultMaxBackoff
	}
	if config.MaxBackoff < config.MinBackoff {
		config.MaxBackoff = config.MinBackoff
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return config
}

// Wake requests reconciliation without blocking the caller. Concurrent wakes coalesce.
func (r *Reconciler) Wake() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// Health returns an immutable sanitized snapshot.
func (r *Reconciler) Health() HealthSnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	return cloneHealth(r.health)
}

// Run reconciles immediately on startup, then on wakes and safety sweeps.
func (r *Reconciler) Run(ctx context.Context) error {
	for {
		more, err := r.reconcileTurn(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			r.recordError(err)
			delay := r.backoffFor(err)
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				if !timer.Stop() {
					<-timer.C
				}
				return ctx.Err()
			case <-timer.C:
			}
			continue
		}
		r.mu.Lock()
		r.failures = 0
		r.mu.Unlock()
		if more {
			continue
		}
		timer := time.NewTimer(r.config.SweepInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-r.wake:
			if !timer.Stop() {
				<-timer.C
			}
		case <-timer.C:
		}
	}
}

func (r *Reconciler) reconcileTurn(ctx context.Context) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	mirrorMore, err := r.refreshMirror(ctx)
	if err != nil {
		return false, err
	}
	if mirrorMore || r.embedder == nil {
		return mirrorMore, nil
	}
	return r.fillGeneration(ctx)
}

func (r *Reconciler) refreshMirror(ctx context.Context) (bool, error) {
	r.mu.Lock()
	if r.mirrorSeen == nil {
		r.mirrorSeen = make(map[string]struct{})
	}
	cursor := r.mirrorCursor
	seen := r.mirrorSeen
	r.mu.Unlock()

	sources, err := r.store.ListSearchDocuments(ctx, cursor, r.config.MirrorPageSize)
	if err != nil {
		return false, err
	}
	docs := make([]searchdoc.Document, len(sources))
	for i, source := range sources {
		docs[i] = searchdoc.Render(source)
	}
	if _, err := r.index.RefreshMirrorPage(ctx, docs, seen); err != nil {
		return false, err
	}
	if len(sources) > 0 {
		cursor = sources[len(sources)-1].ReviewID
	}
	complete := len(sources) < r.config.MirrorPageSize
	if complete {
		if _, err := r.index.DeleteMissing(ctx, seen); err != nil {
			return false, err
		}
	}
	indexed, err := r.index.mirrorCount(ctx)
	if err != nil {
		return false, err
	}
	now := r.config.Now()
	r.mu.Lock()
	r.health.Indexed = indexed
	r.health.MirrorComplete = complete
	if complete {
		r.health.MirrorBacklog = new(int64)
		r.mirrorCursor = 0
		r.mirrorSeen = nil
	} else {
		r.health.MirrorBacklog = nil
		r.mirrorCursor = cursor
	}
	r.health.LastSuccessAt = new(now)
	r.health.LastError = ""
	r.health.LastErrorStatus = 0
	r.mu.Unlock()
	return !complete, nil
}

func (r *Reconciler) fillGeneration(ctx context.Context) (bool, error) {
	model := r.embedder.Generation()
	key, err := r.index.EnsureGeneration(ctx, model)
	if err != nil {
		return false, err
	}
	counts, err := r.index.GenerationCounts(ctx, key)
	if err != nil {
		return false, err
	}
	activeInfo, hasActive, err := r.index.ActiveGeneration(ctx)
	if err != nil {
		return false, err
	}
	activeGeneration := ""
	if hasActive {
		activeGeneration = activeInfo.Fingerprint
	}
	r.beginGeneration(key, counts, activeGeneration)
	if counts.Backlog == 0 {
		if err := r.index.ActivateGeneration(ctx, key); err != nil {
			return false, err
		}
		r.updateGenerationHealth(key, counts, key)
		return false, nil
	}

	fillCtx, cancel := context.WithTimeout(ctx, r.config.MaxFillTime)
	defer cancel()
	_, err = r.index.Fill(
		fillCtx,
		&turnLimitedStore{Store: r.index.vectors, remaining: r.config.MaxFillBatches},
		key,
		encodeDocuments(r.embedder),
		max(r.embedder.BatchSize(), 1),
		nil,
	)
	countsAfter, countErr := r.index.GenerationCounts(ctx, key)
	if countErr == nil {
		counts = countsAfter
		if counts.Backlog == 0 && err == nil {
			if activateErr := r.index.ActivateGeneration(ctx, key); activateErr != nil {
				return false, activateErr
			}
			activeGeneration = key
		}
		r.updateGenerationHealth(key, counts, activeGeneration)
	}
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return counts.Backlog > 0, err
		}
		return counts.Backlog > 0, providerFailure(err)
	}
	return counts.Backlog > 0, nil
}

type turnLimitedStore struct {
	vector.Store[string, string]
	remaining int
}

func (s *turnLimitedStore) PendingForGeneration(ctx context.Context, gen string, limit int) ([]vector.Pending[string], error) {
	if s.remaining <= 0 {
		return nil, nil
	}
	if limit > s.remaining {
		limit = s.remaining
	}
	pending, err := s.Store.PendingForGeneration(ctx, gen, limit)
	if err != nil {
		return nil, err
	}
	if len(pending) > s.remaining {
		pending = pending[:s.remaining]
	}
	s.remaining -= len(pending)
	return pending, nil
}

type embeddingProviderError struct{ cause error }

func (e *embeddingProviderError) Error() string { return "embedding provider request failed" }
func (e *embeddingProviderError) Unwrap() error { return e.cause }

func providerFailure(err error) error {
	if _, ok := errors.AsType[*embeddingProviderError](err); ok {
		return err
	}
	return &embeddingProviderError{cause: err}
}

func (r *Reconciler) beginGeneration(key string, counts GenerationCounts, activeGeneration string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.health.Generation != key || r.generationStarted.IsZero() {
		r.health.Generation = key
		r.generationStarted = r.config.Now()
		r.generationBaseline = counts.Embedded + counts.Skipped
	}
	setGenerationAvailability(&r.health, key, activeGeneration)
	r.health.Embedded = counts.Embedded
	r.health.Skipped = counts.Skipped
	r.health.EmbeddingBacklog = counts.Backlog
}

func (r *Reconciler) updateGenerationHealth(
	key string, counts GenerationCounts, activeGeneration string,
) {
	now := r.config.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	previousHandled := r.health.Embedded + r.health.Skipped
	handled := counts.Embedded + counts.Skipped
	r.health.Embedded = counts.Embedded
	r.health.Skipped = counts.Skipped
	r.health.EmbeddingBacklog = counts.Backlog
	r.health.Generation = key
	if handled > previousHandled {
		r.health.LastProgressAt = new(now)
	}
	r.health.LastSuccessAt = new(now)
	r.health.LastError = ""
	r.health.LastErrorStatus = 0
	elapsed := now.Sub(r.generationStarted).Seconds()
	if elapsed < 1 {
		elapsed = 1
	}
	rate := float64(max(handled-r.generationBaseline, 0)) / elapsed
	r.health.RatePerSecond = &rate
	if rate > 0 && counts.Backlog > 0 {
		eta := int64(math.Ceil(float64(counts.Backlog) / rate))
		r.health.ETASeconds = &eta
	} else {
		r.health.ETASeconds = nil
	}
	setGenerationAvailability(&r.health, key, activeGeneration)
}

func setGenerationAvailability(health *HealthSnapshot, key, activeGeneration string) {
	if activeGeneration == key {
		health.VectorState = "ready"
		health.ActiveGeneration = key
		return
	}
	health.VectorState = "building"
	health.ActiveGeneration = activeGeneration
}

func (r *Reconciler) recordError(err error) {
	category, status := errorCategory(err)
	matchingActive := false
	activeGeneration := ""
	if r.embedder != nil && r.index != nil {
		active, hasActive, activeErr := r.index.ActiveGeneration(context.Background())
		if activeErr == nil && hasActive {
			activeGeneration = active.Fingerprint
			matchingActive = activeGeneration == r.embedder.Generation().Fingerprint()
		}
	}
	r.mu.Lock()
	r.failures++
	r.health.LastError = category
	r.health.LastErrorStatus = status
	if r.embedder != nil {
		if matchingActive {
			r.health.VectorState = "ready"
			r.health.ActiveGeneration = activeGeneration
		} else {
			r.health.VectorState = "error"
			r.health.ActiveGeneration = activeGeneration
		}
	}
	r.mu.Unlock()
}

func errorCategory(err error) (string, int) {
	if apiErr, ok := errors.AsType[*embedding.APIError](err); ok {
		switch apiErr.StatusCode {
		case http.StatusBadRequest:
			return "request", apiErr.StatusCode
		case http.StatusUnauthorized, http.StatusForbidden:
			return "authentication", apiErr.StatusCode
		case http.StatusNotFound:
			return "configuration", apiErr.StatusCode
		case http.StatusTooManyRequests:
			return "rate_limit", apiErr.StatusCode
		default:
			return "provider", apiErr.StatusCode
		}
	}
	if _, ok := errors.AsType[*CapabilityError](err); ok {
		return "capability", 0
	}
	if _, ok := errors.AsType[*embeddingProviderError](err); ok {
		return "provider", 0
	}
	return "mirror", 0
}

func (r *Reconciler) backoffFor(err error) time.Duration {
	if apiErr, ok := errors.AsType[*embedding.APIError](err); ok {
		if apiErr.RetryAfter > 0 {
			return apiErr.RetryAfter
		}
		if apiErr.Definitive() {
			return r.config.MaxBackoff
		}
	}
	r.mu.Lock()
	failures := max(r.failures, 1)
	r.mu.Unlock()
	delay := r.config.MinBackoff
	for i := 1; i < failures && delay < r.config.MaxBackoff; i++ {
		if delay > r.config.MaxBackoff/2 {
			return r.config.MaxBackoff
		}
		delay *= 2
	}
	return min(delay, r.config.MaxBackoff)
}
