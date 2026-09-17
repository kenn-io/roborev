package searchindex

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode"

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
	partial            *partialEmbedding
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

	started := r.config.Now()
	fillCtx, cancel := context.WithTimeout(ctx, r.config.MaxFillTime)
	defer cancel()
	providerCalls := 0
	for providerCalls < r.config.MaxFillBatches && r.config.Now().Sub(started) < r.config.MaxFillTime {
		batchSize := max(r.embedder.BatchSize(), 1)
		pending, err := r.index.PendingGeneration(ctx, key, batchSize)
		if err != nil {
			return false, err
		}
		if len(pending) == 0 {
			break
		}
		used, stale, err := r.fillPendingBatch(fillCtx, key, pending, r.config.MaxFillBatches-providerCalls)
		providerCalls += used
		if err != nil {
			return false, err
		}
		if stale || used == 0 {
			break
		}
	}

	counts, err = r.index.GenerationCounts(ctx, key)
	if err != nil {
		return false, err
	}
	if counts.Backlog == 0 {
		if err := r.index.ActivateGeneration(ctx, key); err != nil {
			return false, err
		}
		activeGeneration = key
	}
	r.updateGenerationHealth(key, counts, activeGeneration)
	return counts.Backlog > 0, nil
}

type pendingDocument struct {
	pending vector.Pending[string]
	chunks  []vector.Chunk
}

type partialEmbedding struct {
	generation string
	revision   string
	pending    vector.Pending[string]
	chunks     []vector.Chunk
	vectors    []vector.ChunkVector
	next       int
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

func (r *Reconciler) fillPendingBatch(
	ctx context.Context, key string, pending []vector.Pending[string], callBudget int,
) (calls int, stale bool, err error) {
	if callBudget <= 0 {
		return 0, false, nil
	}
	batchSize := max(r.embedder.BatchSize(), 1)
	if r.partial != nil {
		currentRevision, found, err := r.index.mirrorRevision(ctx, r.partial.pending.Doc)
		if err != nil {
			return 0, false, err
		}
		if r.partial.generation != key || !found || currentRevision != r.partial.revision {
			r.partial = nil
		} else {
			return r.fillPartial(ctx, key, batchSize, callBudget)
		}
	}
	documents := make([]pendingDocument, 0, len(pending))
	texts := make([]string, 0, batchSize)
	for _, item := range pending {
		chunks := vector.Split(item.Content, vector.SplitOptions{MaxRunes: searchChunkRunes, Overlap: searchChunkOverlap})
		if len(chunks) == 0 {
			if err := r.index.SaveGenerationVectors(ctx, key, item, nil); err != nil {
				if errors.Is(err, vector.ErrStale) {
					return calls, true, nil
				}
				return calls, false, err
			}
			continue
		}
		if len(chunks) > batchSize {
			r.partial = &partialEmbedding{
				generation: key,
				revision:   revisionText(item.Revision),
				pending:    item,
				chunks:     chunks,
				vectors:    make([]vector.ChunkVector, 0, len(chunks)),
			}
			return r.fillPartial(ctx, key, batchSize, callBudget)
		}
		if len(texts) > 0 && len(texts)+len(chunks) > batchSize {
			break
		}
		documents = append(documents, pendingDocument{pending: item, chunks: chunks})
		for _, chunk := range chunks {
			texts = append(texts, chunk.Text)
		}
		if len(texts) >= batchSize {
			break
		}
	}
	if len(texts) == 0 {
		return calls, false, nil
	}

	vectors, err := r.embedder.Embed(ctx, embedding.InputDocument, texts)
	calls++
	if err != nil {
		var apiErr *embedding.APIError
		if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusBadRequest {
			return calls, false, providerFailure(err)
		}
		return r.isolateBadRequest(ctx, key, documents, callBudget-calls, calls)
	}
	if err := validateEmbeddingResponse(vectors, len(texts), r.embedder.Generation().Dimensions); err != nil {
		return calls, false, err
	}
	stale, err = r.saveEmbeddedDocuments(ctx, key, documents, vectors)
	return calls, stale, err
}

func (r *Reconciler) fillPartial(
	ctx context.Context, key string, batchSize, callBudget int,
) (calls int, stale bool, err error) {
	partial := r.partial
	end := min(partial.next+batchSize, len(partial.chunks))
	chunks := partial.chunks[partial.next:end]
	texts := chunkTexts(chunks)
	encoded, err := r.embedder.Embed(ctx, embedding.InputDocument, texts)
	calls++
	if err != nil {
		var apiErr *embedding.APIError
		if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusBadRequest {
			return calls, false, providerFailure(err)
		}
		if calls >= callBudget {
			return calls, false, providerFailure(err)
		}
		benign := make([]string, len(texts))
		for i, text := range texts {
			benign[i] = benignReplay(text)
		}
		encoded, err = r.embedder.Embed(ctx, embedding.InputDocument, benign)
		calls++
		if err != nil {
			return calls, false, providerFailure(err)
		}
		if err := validateEmbeddingResponse(encoded, len(benign), r.embedder.Generation().Dimensions); err != nil {
			return calls, false, err
		}
		if err := r.index.SaveGenerationVectors(ctx, key, partial.pending, nil); err != nil {
			r.partial = nil
			if errors.Is(err, vector.ErrStale) {
				return calls, true, nil
			}
			return calls, false, err
		}
		r.partial = nil
		return calls, false, nil
	}
	if err := validateEmbeddingResponse(encoded, len(texts), r.embedder.Generation().Dimensions); err != nil {
		return calls, false, err
	}
	for i, chunk := range chunks {
		partial.vectors = append(partial.vectors, vector.ChunkVector{
			ChunkIndex: chunk.Index,
			Vector:     vector.Vector(encoded[i]),
		})
	}
	partial.next = end
	if partial.next < len(partial.chunks) {
		return calls, false, nil
	}
	r.partial = nil
	if err := r.index.SaveGenerationVectors(ctx, key, partial.pending, partial.vectors); err != nil {
		if errors.Is(err, vector.ErrStale) {
			return calls, true, nil
		}
		return calls, false, err
	}
	return calls, false, nil
}

func (r *Reconciler) isolateBadRequest(
	ctx context.Context, key string, documents []pendingDocument, budget, calls int,
) (int, bool, error) {
	for _, document := range documents {
		texts := chunkTexts(document.chunks)
		if len(documents) > 1 {
			if budget == 0 {
				return calls, false, nil
			}
			vectors, err := r.embedder.Embed(ctx, embedding.InputDocument, texts)
			calls++
			budget--
			if err == nil {
				if err := validateEmbeddingResponse(vectors, len(texts), r.embedder.Generation().Dimensions); err != nil {
					return calls, false, err
				}
				stale, saveErr := r.saveEmbeddedDocuments(ctx, key, []pendingDocument{document}, vectors)
				if saveErr != nil {
					return calls, false, saveErr
				}
				if stale {
					return calls, true, nil
				}
				continue
			}
			var apiErr *embedding.APIError
			if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusBadRequest {
				return calls, false, providerFailure(err)
			}
		}
		if budget == 0 {
			return calls, false, &embedding.APIError{StatusCode: http.StatusBadRequest}
		}
		benign := make([]string, len(texts))
		for i, text := range texts {
			benign[i] = benignReplay(text)
		}
		encoded, err := r.embedder.Embed(ctx, embedding.InputDocument, benign)
		calls++
		budget--
		if err != nil {
			return calls, false, providerFailure(err)
		}
		if err := validateEmbeddingResponse(encoded, len(benign), r.embedder.Generation().Dimensions); err != nil {
			return calls, false, err
		}
		if err := r.index.SaveGenerationVectors(ctx, key, document.pending, nil); err != nil {
			if errors.Is(err, vector.ErrStale) {
				return calls, true, nil
			}
			return calls, false, err
		}
	}
	return calls, false, nil
}

func (r *Reconciler) saveEmbeddedDocuments(
	ctx context.Context, key string, documents []pendingDocument, encoded [][]float32,
) (bool, error) {
	wanted := 0
	for _, document := range documents {
		wanted += len(document.chunks)
	}
	if err := validateEmbeddingResponse(encoded, wanted, r.embedder.Generation().Dimensions); err != nil {
		return false, err
	}
	offset := 0
	for _, document := range documents {
		vectors := make([]vector.ChunkVector, len(document.chunks))
		for i, chunk := range document.chunks {
			vectors[i] = vector.ChunkVector{ChunkIndex: chunk.Index, Vector: vector.Vector(encoded[offset+i])}
		}
		offset += len(document.chunks)
		if err := r.index.SaveGenerationVectors(ctx, key, document.pending, vectors); err != nil {
			if errors.Is(err, vector.ErrStale) {
				return true, nil
			}
			return false, err
		}
	}
	return false, nil
}

func validateEmbeddingResponse(encoded [][]float32, wanted, dimensions int) error {
	if len(encoded) != wanted {
		return providerFailure(errors.New("embedding result count mismatch"))
	}
	for _, item := range encoded {
		if len(item) != dimensions {
			return providerFailure(errors.New("embedding result dimension mismatch"))
		}
	}
	return nil
}

func revisionText(revision any) string {
	switch value := revision.(type) {
	case string:
		return value
	case []byte:
		return string(value)
	default:
		return fmt.Sprint(value)
	}
}

func chunkTexts(chunks []vector.Chunk) []string {
	texts := make([]string, len(chunks))
	for i, chunk := range chunks {
		texts[i] = chunk.Text
	}
	return texts
}

func benignReplay(content string) string {
	return strings.Map(func(value rune) rune {
		if unicode.IsSpace(value) {
			return value
		}
		return 'x'
	}, content)
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
