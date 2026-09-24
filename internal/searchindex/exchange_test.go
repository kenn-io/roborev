package searchindex

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/vector"

	"go.kenn.io/roborev/internal/embedding"
	"go.kenn.io/roborev/internal/searchdoc"
	"go.kenn.io/roborev/internal/storage"
)

var _ VectorExchange = (*storage.VectorExchange)(nil)

type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func newTestClock() *testClock { return &testClock{now: time.Unix(1_800_000_000, 0)} }

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

type fakeRecordKey struct {
	fingerprint string
	storage.VectorKey
}

type fakeClaim struct {
	machine string
	expires time.Time
}

// fakeExchangeDB is the shared state several fake daemons talk to.
type fakeExchangeDB struct {
	mu      sync.Mutex
	now     func() time.Time
	records map[fakeRecordKey]storage.VectorRecord
	claims  map[fakeRecordKey]fakeClaim
	touched map[string]int
	gcCalls int
}

func newFakeExchangeDB(now func() time.Time) *fakeExchangeDB {
	return &fakeExchangeDB{
		now: now, records: map[fakeRecordKey]storage.VectorRecord{},
		claims: map[fakeRecordKey]fakeClaim{}, touched: map[string]int{},
	}
}

type fakeExchange struct {
	db        *fakeExchangeDB
	machineID string

	mu        sync.Mutex
	targetErr error
	lookupErr error
	lookups   int
	publishes int
	discards  int
}

func newFakeExchange(db *fakeExchangeDB, machineID string) *fakeExchange {
	return &fakeExchange{db: db, machineID: machineID}
}

func (e *fakeExchange) setTargetErr(err error) {
	e.mu.Lock()
	e.targetErr = err
	e.mu.Unlock()
}

func (e *fakeExchange) Target(context.Context) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.targetErr != nil {
		return "", e.targetErr
	}
	return "fake-target", nil
}

func (e *fakeExchange) TouchGeneration(_ context.Context, gen storage.VectorGeneration) error {
	e.db.mu.Lock()
	defer e.db.mu.Unlock()
	e.db.touched[gen.Fingerprint]++
	return nil
}

func (e *fakeExchange) Lookup(_ context.Context, fingerprint string, keys []storage.VectorKey) ([]storage.VectorRecord, error) {
	e.mu.Lock()
	e.lookups++
	lookupErr := e.lookupErr
	e.mu.Unlock()
	if lookupErr != nil {
		return nil, lookupErr
	}
	e.db.mu.Lock()
	defer e.db.mu.Unlock()
	var out []storage.VectorRecord
	for _, key := range keys {
		if record, ok := e.db.records[fakeRecordKey{fingerprint, key}]; ok {
			out = append(out, record)
		}
	}
	return out, nil
}

func (e *fakeExchange) Claim(_ context.Context, fingerprint string, key storage.VectorKey, ttl time.Duration) (bool, error) {
	e.db.mu.Lock()
	defer e.db.mu.Unlock()
	recordKey := fakeRecordKey{fingerprint, key}
	if _, ok := e.db.records[recordKey]; ok {
		return false, nil
	}
	now := e.db.now()
	if claim, ok := e.db.claims[recordKey]; ok && claim.machine != e.machineID && claim.expires.After(now) {
		return false, nil
	}
	e.db.claims[recordKey] = fakeClaim{machine: e.machineID, expires: now.Add(ttl)}
	return true, nil
}

func (e *fakeExchange) Publish(_ context.Context, fingerprint string, records []storage.VectorRecord) error {
	e.mu.Lock()
	e.publishes += len(records)
	e.mu.Unlock()
	e.db.mu.Lock()
	defer e.db.mu.Unlock()
	for _, record := range records {
		e.db.records[fakeRecordKey{fingerprint, record.Key}] = record
	}
	return nil
}

func (e *fakeExchange) Release(_ context.Context, fingerprint string, keys []storage.VectorKey) error {
	e.db.mu.Lock()
	defer e.db.mu.Unlock()
	for _, key := range keys {
		recordKey := fakeRecordKey{fingerprint, key}
		if e.db.claims[recordKey].machine == e.machineID {
			delete(e.db.claims, recordKey)
		}
	}
	return nil
}

func (e *fakeExchange) Discard(_ context.Context, fingerprint string, key storage.VectorKey) error {
	e.mu.Lock()
	e.discards++
	e.mu.Unlock()
	e.db.mu.Lock()
	defer e.db.mu.Unlock()
	delete(e.db.records, fakeRecordKey{fingerprint, key})
	return nil
}

func (e *fakeExchange) CollectGarbage(context.Context, time.Duration) (int64, error) {
	e.db.mu.Lock()
	defer e.db.mu.Unlock()
	e.db.gcCalls++
	return 0, nil
}

func (db *fakeExchangeDB) put(fingerprint string, record storage.VectorRecord) {
	db.mu.Lock()
	defer db.mu.Unlock()
	db.records[fakeRecordKey{fingerprint, record.Key}] = record
}

func (db *fakeExchangeDB) record(fingerprint string, key storage.VectorKey) (storage.VectorRecord, bool) {
	db.mu.Lock()
	defer db.mu.Unlock()
	record, ok := db.records[fakeRecordKey{fingerprint, key}]
	return record, ok
}

func (db *fakeExchangeDB) claimCount() int {
	db.mu.Lock()
	defer db.mu.Unlock()
	return len(db.claims)
}

func (db *fakeExchangeDB) setClaim(fingerprint string, key storage.VectorKey, machine string, expires time.Time) {
	db.mu.Lock()
	defer db.mu.Unlock()
	db.claims[fakeRecordKey{fingerprint, key}] = fakeClaim{machine: machine, expires: expires}
}

func sharedSources(state storage.SearchShareState, ids ...int64) []storage.SearchReviewSource {
	sources := make([]storage.SearchReviewSource, len(ids))
	for i, id := range ids {
		sources[i] = storage.SearchReviewSource{
			ReviewID: id, JobID: 1000 + id, RepoID: 7,
			ReviewUUID: fmt.Sprintf("00000000-0000-4000-8000-%012d", id),
			JobUUID:    fmt.Sprintf("10000000-0000-4000-8000-%012d", id),
			RepoName:   "example/repo", GitRef: "HEAD", ReviewType: "review",
			Output:     fmt.Sprintf("shared review output %d", id),
			ShareState: state,
		}
	}
	return sources
}

func vectorKeyFor(source storage.SearchReviewSource) storage.VectorKey {
	doc := searchdoc.Render(source)
	return storage.VectorKey{ReviewUUID: doc.DocKey, ContentSHA256: doc.ContentHash}
}

func okRecord(source storage.SearchReviewSource, values ...float32) storage.VectorRecord {
	return storage.VectorRecord{
		Key: vectorKeyFor(source), Status: storage.VectorStatusOK, Dims: len(values),
		Chunks: []storage.VectorChunk{{Index: 0, Vector: values}},
	}
}

func noDocumentProvider(_ *testing.T, model vector.Generation) *reconcilerEmbedder {
	return &reconcilerEmbedder{model: model, batchSize: 8, embed: func(_ context.Context, texts []string) ([][]float32, error) {
		return nil, errors.New("provider must not be called")
	}}
}

func runUntilIdle(t *testing.T, r *Reconciler) {
	t.Helper()
	var more bool
	for range 50 {
		var err error
		more, err = r.reconcileTurn(context.Background())
		require.NoError(t, err)
		if !more {
			return
		}
	}
	require.False(t, more, "reconciler did not become idle")
}

func documentTexts(e *reconcilerEmbedder) []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var texts []string
	for _, call := range e.calls {
		texts = append(texts, call...)
	}
	return texts
}

func TestSharedReconcilerImportsPeerVectorsWithoutProviderCalls(t *testing.T) {
	clock := newTestClock()
	model := vector.Generation{Model: "model", Dimensions: 2}
	store := &reconcilerStore{sources: sharedSources(storage.SearchSharePeer, 1, 2, 3)}
	db := newFakeExchangeDB(clock.Now)
	for _, source := range store.sources {
		db.put(model.Fingerprint(), okRecord(source, 0.6, 0.8))
	}
	exchange := newFakeExchange(db, "machine-b")
	embedder := noDocumentProvider(t, model)
	r := NewReconciler(store, openGenerationTestIndex(t), embedder, ReconcilerConfig{Now: clock.Now})
	r.SetVectorExchange(exchange)

	runUntilIdle(t, r)

	health := r.Health()
	assert.Equal(t, SourceOK, health.SourceStatus)
	assert.Equal(t, int64(3), health.Imported)
	assert.Zero(t, health.EmbeddingBacklog)
	assert.Equal(t, model.Fingerprint(), health.ActiveGeneration)
	assert.Zero(t, embedder.callCount())
	assert.Zero(t, exchange.publishes, "imported vectors are not published again")
	assert.Equal(t, 1, db.touched[model.Fingerprint()])
	assert.Equal(t, 1, db.gcCalls)
	chunks, err := r.index.readChunkVectors(context.Background(), model.Fingerprint(), vectorKeyFor(store.sources[0]).ReviewUUID)
	require.NoError(t, err)
	assert.Equal(t, []storage.VectorChunk{{Index: 0, Vector: []float32{0.6, 0.8}}}, chunks)
}

func TestSharedReconcilerClaimsOwnDocumentsAndPublishes(t *testing.T) {
	clock := newTestClock()
	model := vector.Generation{Model: "model", Dimensions: 2}
	store := &reconcilerStore{sources: sharedSources(storage.SearchShareOwn, 1, 2)}
	db := newFakeExchangeDB(clock.Now)
	exchange := newFakeExchange(db, "machine-a")
	embedder := &reconcilerEmbedder{model: model, batchSize: 8}
	r := NewReconciler(store, openGenerationTestIndex(t), embedder, ReconcilerConfig{Now: clock.Now})
	r.SetVectorExchange(exchange)

	runUntilIdle(t, r)

	assert.Len(t, documentTexts(embedder), 2)
	for _, source := range store.sources {
		record, ok := db.record(model.Fingerprint(), vectorKeyFor(source))
		require.True(t, ok)
		assert.Equal(t, storage.VectorStatusOK, record.Status)
		assert.Equal(t, []storage.VectorChunk{{Index: 0, Vector: []float32{1, 0}}}, record.Chunks)
	}
	assert.Zero(t, db.claimCount(), "finished claims are released")
	health := r.Health()
	assert.Equal(t, int64(2), health.Published)
	assert.Zero(t, health.ClaimsHeld)
	assert.Equal(t, model.Fingerprint(), health.ActiveGeneration)
}

func TestSharedReconcilerWaitsBeforeClaimingPeerDocumentsAndActivatesPartially(t *testing.T) {
	clock := newTestClock()
	model := vector.Generation{Model: "model", Dimensions: 2}
	sources := append(sharedSources(storage.SearchSharePeer, 1), sharedSources(storage.SearchShareLocal, 2)...)
	store := &reconcilerStore{sources: sources}
	db := newFakeExchangeDB(clock.Now)
	embedder := &reconcilerEmbedder{model: model, batchSize: 8}
	r := NewReconciler(store, openGenerationTestIndex(t), embedder, ReconcilerConfig{Now: clock.Now})
	r.SetVectorExchange(newFakeExchange(db, "machine-b"))

	runUntilIdle(t, r)

	localDoc := searchdoc.Render(sources[1])
	assert.Equal(t, []string{localDoc.Content}, documentTexts(embedder), "only the local document is embedded")
	health := r.Health()
	assert.Equal(t, int64(1), health.AwaitingPeer)
	assert.Equal(t, int64(1), health.EmbeddingBacklog)
	assert.Equal(t, model.Fingerprint(), health.ActiveGeneration,
		"a sharing daemon activates once every shared document had one lookup")
	assert.LessOrEqual(t, r.idleDelay(), 30*time.Second, "the next lookup is scheduled before the sweep")
	_, published := db.record(model.Fingerprint(), vectorKeyFor(sources[1]))
	assert.False(t, published, "local-only documents are never published")

	clock.Advance(defaultPeerClaimDelay)
	runUntilIdle(t, r)

	assert.Len(t, documentTexts(embedder), 2, "the peer document is claimed after the delay")
	_, published = db.record(model.Fingerprint(), vectorKeyFor(sources[0]))
	assert.True(t, published)
	assert.Zero(t, r.Health().AwaitingPeer)
}

func TestSharedReconcilerLosingClaimWaitsThenImportsWinner(t *testing.T) {
	clock := newTestClock()
	model := vector.Generation{Model: "model", Dimensions: 2}
	store := &reconcilerStore{sources: sharedSources(storage.SearchShareOwn, 1)}
	db := newFakeExchangeDB(clock.Now)
	key := vectorKeyFor(store.sources[0])
	db.setClaim(model.Fingerprint(), key, "machine-winner", clock.Now().Add(time.Minute))
	embedder := noDocumentProvider(t, model)
	r := NewReconciler(store, openGenerationTestIndex(t), embedder, ReconcilerConfig{Now: clock.Now})
	r.SetVectorExchange(newFakeExchange(db, "machine-loser"))

	runUntilIdle(t, r)
	assert.Equal(t, int64(1), r.Health().AwaitingPeer)

	db.put(model.Fingerprint(), okRecord(store.sources[0], 0, 1))
	clock.Advance(defaultLookupMinBackoff)
	runUntilIdle(t, r)

	assert.Equal(t, int64(1), r.Health().Imported)
	assert.Zero(t, embedder.callCount())
}

func TestSharedReconcilerUnreachableHoldsSharedDocumentsUntilFallback(t *testing.T) {
	clock := newTestClock()
	model := vector.Generation{Model: "model", Dimensions: 2}
	sources := append(sharedSources(storage.SearchSharePeer, 1), sharedSources(storage.SearchShareLocal, 2)...)
	sources = append(sources, sharedSources(storage.SearchShareOwn, 3)...)
	store := &reconcilerStore{sources: sources}
	db := newFakeExchangeDB(clock.Now)
	exchange := newFakeExchange(db, "machine-a")
	exchange.setTargetErr(fmt.Errorf("%w: connection refused", storage.ErrVectorExchangeUnreachable))
	embedder := &reconcilerEmbedder{model: model, batchSize: 8}
	r := NewReconciler(store, openGenerationTestIndex(t), embedder, ReconcilerConfig{Now: clock.Now})
	r.SetVectorExchange(exchange)

	runUntilIdle(t, r)

	assert.Equal(t, []string{searchdoc.Render(sources[1]).Content}, documentTexts(embedder))
	health := r.Health()
	assert.Equal(t, SourceUnreachable, health.SourceStatus)
	assert.Empty(t, health.ActiveGeneration, "shared documents never had a lookup")
	assert.Equal(t, int64(2), health.AwaitingPeer)
	assert.Equal(t, defaultLookupMinBackoff, r.idleDelay(),
		"an unreachable exchange is polled at the lookup backoff, not in a tight loop")

	clock.Advance(defaultLocalFallbackAfter)
	runUntilIdle(t, r)
	assert.Len(t, documentTexts(embedder), 3, "the 24h fallback embeds held documents locally")
	assert.Equal(t, model.Fingerprint(), r.Health().ActiveGeneration)

	exchange.setTargetErr(nil)
	runUntilIdle(t, r)
	for _, source := range []storage.SearchReviewSource{sources[0], sources[2]} {
		_, ok := db.record(model.Fingerprint(), vectorKeyFor(source))
		assert.True(t, ok, "fallback vectors are published once reachable")
	}
	_, ok := db.record(model.Fingerprint(), vectorKeyFor(sources[1]))
	assert.False(t, ok)
	assert.Equal(t, SourceOK, r.Health().SourceStatus)
}

func TestSharedReconcilerUnsupportedExchangeEmbedsLocallyLikeToday(t *testing.T) {
	clock := newTestClock()
	model := vector.Generation{Model: "model", Dimensions: 2}
	store := &reconcilerStore{sources: sharedSources(storage.SearchSharePeer, 1, 2)}
	db := newFakeExchangeDB(clock.Now)
	exchange := newFakeExchange(db, "machine-a")
	exchange.setTargetErr(storage.ErrVectorExchangeUnsupported)
	embedder := &reconcilerEmbedder{model: model, batchSize: 1}
	r := NewReconciler(store, openGenerationTestIndex(t), embedder, ReconcilerConfig{Now: clock.Now})
	r.SetVectorExchange(exchange)

	runUntilIdle(t, r)

	assert.Len(t, documentTexts(embedder), 2)
	assert.Equal(t, SourceUnsupported, r.Health().SourceStatus)
	assert.Zero(t, exchange.lookups)
	assert.Zero(t, exchange.publishes)
	assert.Equal(t, model.Fingerprint(), r.Health().ActiveGeneration)
}

func TestSharedReconcilerRejectsMalformedRecordsAndReembeds(t *testing.T) {
	clock := newTestClock()
	model := vector.Generation{Model: "model", Dimensions: 2}
	store := &reconcilerStore{sources: sharedSources(storage.SearchShareOwn, 1, 2, 3, 4, 5, 6)}
	db := newFakeExchangeDB(clock.Now)
	fingerprint := model.Fingerprint()
	tooMany := make([]storage.VectorChunk, 65)
	for i := range tooMany {
		tooMany[i] = storage.VectorChunk{Index: i, Vector: []float32{1, 0}}
	}
	bad := []storage.VectorRecord{
		{Status: storage.VectorStatusOK, Dims: 3, Chunks: []storage.VectorChunk{{Index: 0, Vector: []float32{1, 0, 0}}}},
		{Status: storage.VectorStatusOK, Dims: 2, Chunks: []storage.VectorChunk{{Index: 0, Vector: []float32{float32(math.NaN()), 0}}}},
		{Status: storage.VectorStatusOK, Dims: 2, Chunks: []storage.VectorChunk{{Index: 0, Vector: []float32{2, 0}}}},
		{Status: storage.VectorStatusOK, Dims: 2, Chunks: tooMany},
		{Status: storage.VectorStatusOK, Dims: 2, Malformed: true},
		{Status: storage.VectorStatusOK, Dims: 2},
	}
	for i, source := range store.sources {
		record := bad[i]
		record.Key = vectorKeyFor(source)
		db.put(fingerprint, record)
	}
	exchange := newFakeExchange(db, "machine-a")
	embedder := &reconcilerEmbedder{model: model, batchSize: 8}
	r := NewReconciler(store, openGenerationTestIndex(t), embedder, ReconcilerConfig{Now: clock.Now})
	r.SetVectorExchange(exchange)

	runUntilIdle(t, r)

	assert.Equal(t, int64(len(bad)), r.Health().Rejected)
	assert.Equal(t, len(bad), exchange.discards)
	assert.Len(t, documentTexts(embedder), len(bad))
	for _, source := range store.sources {
		chunks, err := r.index.readChunkVectors(context.Background(), fingerprint, vectorKeyFor(source).ReviewUUID)
		require.NoError(t, err)
		assert.Equal(t, []storage.VectorChunk{{Index: 0, Vector: []float32{1, 0}}}, chunks,
			"only provider vectors reach the index")
		record, ok := db.record(fingerprint, vectorKeyFor(source))
		require.True(t, ok)
		assert.Equal(t, chunks, record.Chunks, "the re-embedded record replaces the rejected one")
	}
}

func TestSharedReconcilerIgnoresRecordsOfAnotherGeneration(t *testing.T) {
	clock := newTestClock()
	local := vector.Generation{Model: "model", Dimensions: 2, Params: map[string]string{"endpoint": "https://a.example"}}
	other := vector.Generation{Model: "model", Dimensions: 2, Params: map[string]string{"endpoint": "https://b.example"}}
	store := &reconcilerStore{sources: sharedSources(storage.SearchShareOwn, 1)}
	db := newFakeExchangeDB(clock.Now)
	db.put(other.Fingerprint(), okRecord(store.sources[0], 0, 1))
	embedder := &reconcilerEmbedder{model: local, batchSize: 8}
	r := NewReconciler(store, openGenerationTestIndex(t), embedder, ReconcilerConfig{Now: clock.Now})
	r.SetVectorExchange(newFakeExchange(db, "machine-a"))

	runUntilIdle(t, r)

	assert.Len(t, documentTexts(embedder), 1, "a record from another vector space is never imported")
	assert.Zero(t, r.Health().Imported)
	_, ok := db.record(local.Fingerprint(), vectorKeyFor(store.sources[0]))
	assert.True(t, ok, "the daemon publishes under its own fingerprint")
}

func TestSharedReconcilersEmbedEachSharedDocumentOnce(t *testing.T) {
	clock := newTestClock()
	model := vector.Generation{Model: "model", Dimensions: 2}
	ids := []int64{1, 2, 3, 4, 5, 6, 7, 8}
	db := newFakeExchangeDB(clock.Now)
	var reconcilers []*Reconciler
	var embedders []*reconcilerEmbedder
	for _, machine := range []string{"machine-a", "machine-b"} {
		store := &reconcilerStore{sources: sharedSources(storage.SearchSharePeer, ids...)}
		embedder := &reconcilerEmbedder{model: model, batchSize: 1}
		r := NewReconciler(store, openGenerationTestIndex(t), embedder, ReconcilerConfig{
			Now: clock.Now, PeerClaimDelay: time.Nanosecond, LookupMinBackoff: time.Nanosecond,
		})
		r.SetVectorExchange(newFakeExchange(db, machine))
		reconcilers = append(reconcilers, r)
		embedders = append(embedders, embedder)
	}
	for _, r := range reconcilers {
		_, err := r.reconcileTurn(context.Background())
		require.NoError(t, err)
	}
	assert.Zero(t, len(documentTexts(embedders[0]))+len(documentTexts(embedders[1])),
		"nobody claims a peer document before the claim delay")
	clock.Advance(time.Second)

	var wg sync.WaitGroup
	errs := make(chan error, len(reconcilers))
	for _, r := range reconcilers {
		wg.Go(func() {
			for range 200 {
				if _, err := r.reconcileTurn(context.Background()); err != nil {
					errs <- err
					return
				}
				if r.Health().EmbeddingBacklog == 0 && r.Health().ActiveGeneration != "" {
					return
				}
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}

	total := len(documentTexts(embedders[0])) + len(documentTexts(embedders[1]))
	assert.Equal(t, len(ids), total, "claims allow exactly one provider embed per shared document")
	for _, r := range reconcilers {
		assert.Zero(t, r.Health().EmbeddingBacklog)
	}
}

func TestReconcilerWithoutExchangeReportsDisabledSource(t *testing.T) {
	embedder := &reconcilerEmbedder{model: vector.Generation{Model: "model", Dimensions: 2}, batchSize: 1}
	r := NewReconciler(&reconcilerStore{}, openGenerationTestIndex(t), embedder, ReconcilerConfig{})
	assert.Equal(t, SourceDisabled, r.Health().SourceStatus)
}

func TestSharedReconcilerExchangeFailureMidTurnHoldsSharedDocuments(t *testing.T) {
	clock := newTestClock()
	model := vector.Generation{Model: "model", Dimensions: 2}
	sources := append(sharedSources(storage.SearchShareOwn, 1), sharedSources(storage.SearchShareLocal, 2)...)
	store := &reconcilerStore{sources: sources}
	exchange := newFakeExchange(newFakeExchangeDB(clock.Now), "machine-a")
	exchange.lookupErr = fmt.Errorf("%w: connection reset", storage.ErrVectorExchangeUnreachable)
	embedder := &reconcilerEmbedder{model: model, batchSize: 8}
	r := NewReconciler(store, openGenerationTestIndex(t), embedder, ReconcilerConfig{Now: clock.Now})
	r.SetVectorExchange(exchange)

	runUntilIdle(t, r)

	assert.Equal(t, []string{searchdoc.Render(sources[1]).Content}, documentTexts(embedder),
		"a failed lookup never turns into a provider call for a shared document")
	assert.Equal(t, SourceUnreachable, r.Health().SourceStatus)
	assert.Empty(t, r.Health().LastError, "exchange trouble is not a reconciler error")
}

func TestSharedReconcilerProviderFailureKeepsClaimUntilExpiry(t *testing.T) {
	clock := newTestClock()
	model := vector.Generation{Model: "model", Dimensions: 2}
	store := &reconcilerStore{sources: sharedSources(storage.SearchShareOwn, 1)}
	db := newFakeExchangeDB(clock.Now)
	failing := true
	embedder := &reconcilerEmbedder{model: model, batchSize: 8}
	embedder.embed = func(_ context.Context, texts []string) ([][]float32, error) {
		if failing {
			return nil, &embedding.APIError{StatusCode: 503}
		}
		out := make([][]float32, len(texts))
		for i := range out {
			out[i] = []float32{1, 0}
		}
		return out, nil
	}
	r := NewReconciler(store, openGenerationTestIndex(t), embedder, ReconcilerConfig{Now: clock.Now})
	r.SetVectorExchange(newFakeExchange(db, "machine-a"))

	_, err := r.reconcileTurn(context.Background())
	require.Error(t, err)
	assert.Equal(t, 1, db.claimCount(), "the claim stays in PostgreSQL until it expires")
	assert.Equal(t, int64(1), r.Health().ClaimsHeld)

	failing = false
	clock.Advance(defaultClaimTTL)
	runUntilIdle(t, r)
	_, ok := db.record(model.Fingerprint(), vectorKeyFor(store.sources[0]))
	assert.True(t, ok, "after the claim expires the daemon claims again, embeds and publishes")
	assert.Zero(t, db.claimCount())
}

func TestSharedReconcilerPublishesAndImportsLongDocuments(t *testing.T) {
	clock := newTestClock()
	model := vector.Generation{Model: "model", Dimensions: 2}
	sources := sharedSources(storage.SearchShareOwn, 1)
	sources[0].Output = strings.Repeat("long review text ", 7000)
	store := &reconcilerStore{sources: sources}
	db := newFakeExchangeDB(clock.Now)
	exchange := newFakeExchange(db, "machine-a")
	embedder := &reconcilerEmbedder{model: model, batchSize: 128}
	r := NewReconciler(store, openGenerationTestIndex(t), embedder, ReconcilerConfig{Now: clock.Now})
	r.SetVectorExchange(exchange)

	runUntilIdle(t, r)

	assert.Greater(t, len(documentTexts(embedder)), 64, "the document is embedded locally")
	assert.Equal(t, 1, exchange.publishes)
	assert.Zero(t, db.claimCount(), "the claim is released so a peer can embed its own copy")
	assert.Zero(t, r.Health().EmbeddingBacklog)

	peerEmbedder := noDocumentProvider(t, model)
	peer := NewReconciler(store, openGenerationTestIndex(t), peerEmbedder, ReconcilerConfig{Now: clock.Now})
	peer.SetVectorExchange(newFakeExchange(db, "machine-b"))
	runUntilIdle(t, peer)
	assert.Zero(t, peerEmbedder.callCount())
	assert.Equal(t, int64(1), peer.Health().Imported)
	assert.Zero(t, peer.Health().EmbeddingBacklog)
}
