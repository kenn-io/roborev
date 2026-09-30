package searchindex

import (
	"context"
	"errors"
	"math"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
)

// memorySharedVectors stands in for the sync database: it only accepts
// vectors for reviews that sync has already pushed.
type memorySharedVectors struct {
	mu        sync.Mutex
	synced    map[string]bool
	rows      map[string]map[storage.SearchVectorKey][]storage.SearchVectorChunk
	lookupErr error
}

func newMemorySharedVectors() *memorySharedVectors {
	return &memorySharedVectors{
		synced: make(map[string]bool),
		rows:   make(map[string]map[storage.SearchVectorKey][]storage.SearchVectorChunk),
	}
}

func (m *memorySharedVectors) LookupSearchVectors(
	_ context.Context, space string, keys []storage.SearchVectorKey,
) (map[storage.SearchVectorKey][]storage.SearchVectorChunk, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.lookupErr != nil {
		return nil, m.lookupErr
	}
	found := make(map[storage.SearchVectorKey][]storage.SearchVectorChunk)
	for _, key := range keys {
		if chunks, ok := m.rows[space][key]; ok {
			found[key] = chunks
		}
	}
	return found, nil
}

func (m *memorySharedVectors) PublishSearchVectors(
	_ context.Context, space string, key storage.SearchVectorKey, chunks []storage.SearchVectorChunk,
) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.synced[key.ReviewUUID] {
		return false, nil
	}
	if m.rows[space] == nil {
		m.rows[space] = make(map[storage.SearchVectorKey][]storage.SearchVectorChunk)
	}
	if _, ok := m.rows[space][key]; ok {
		return false, nil
	}
	m.rows[space][key] = chunks
	return true, nil
}

func (m *memorySharedVectors) push(reviewUUIDs ...string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, reviewUUID := range reviewUUIDs {
		m.synced[reviewUUID] = true
	}
}

func (m *memorySharedVectors) rowCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	count := 0
	for _, rows := range m.rows {
		count += len(rows)
	}
	return count
}

func (m *memorySharedVectors) replaceAll(chunks []storage.SearchVectorChunk) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, rows := range m.rows {
		for key := range rows {
			rows[key] = chunks
		}
	}
}

type sharingDaemon struct {
	reconciler *Reconciler
	embedder   *reconcilerEmbedder
	index      *Index
}

func newSharingDaemon(t *testing.T, sources []storage.SearchReviewSource, shared SharedVectors) *sharingDaemon {
	t.Helper()
	index := openGenerationTestIndex(t)
	embedder := &reconcilerEmbedder{space: testSpace("model", 2), batchSize: 8}
	r := NewReconciler(&reconcilerStore{sources: sources}, index, embedder, ReconcilerConfig{})
	r.ShareVectors(shared)
	return &sharingDaemon{reconciler: r, embedder: embedder, index: index}
}

// embeddedTexts counts document chunks sent to the provider.
func (d *sharingDaemon) embeddedTexts() int {
	d.embedder.mu.Lock()
	defer d.embedder.mu.Unlock()
	count := 0
	for _, call := range d.embedder.calls {
		count += len(call)
	}
	return count
}

func (d *sharingDaemon) reconcile(t *testing.T) {
	t.Helper()
	for {
		more, err := d.reconciler.reconcileTurn(t.Context())
		require.NoError(t, err)
		if !more {
			return
		}
	}
}

func TestSharedVectorsLetPeersSkipTheProvider(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	sources := makeSearchSources(3)
	shared := newMemorySharedVectors()

	origin := newSharingDaemon(t, sources, shared)
	origin.reconcile(t)
	assert.Equal(3, origin.embeddedTexts())
	assert.Zero(shared.rowCount(), "reviews that sync has not pushed are not published")

	uuids := []string{sources[0].ReviewUUID, sources[1].ReviewUUID, sources[2].ReviewUUID}
	shared.push(uuids...)
	origin.reconciler.ReviewsPushed(uuids)
	origin.reconcile(t)
	assert.Equal(3, shared.rowCount(), "pushed reviews publish their existing vectors")
	assert.Equal(3, origin.embeddedTexts(), "publishing does not re-embed")

	peer := newSharingDaemon(t, sources, shared)
	peer.reconcile(t)
	assert.Zero(peer.embeddedTexts(), "the peer imports every review")
	health := peer.reconciler.Health()
	assert.Equal(int64(3), health.Embedded)
	assert.Equal("ready", health.VectorState)
}

func TestSharedVectorsPublishWhenTheReviewIsAlreadySynced(t *testing.T) {
	t.Parallel()
	sources := makeSearchSources(2)
	shared := newMemorySharedVectors()
	shared.push(sources[0].ReviewUUID, sources[1].ReviewUUID)

	first := newSharingDaemon(t, sources, shared)
	first.reconcile(t)
	assert.Equal(t, 2, shared.rowCount())

	second := newSharingDaemon(t, sources, shared)
	second.reconcile(t)
	assert.Zero(t, second.embeddedTexts())
}

func TestSharedVectorsFallBackToTheProvider(t *testing.T) {
	t.Parallel()
	sources := makeSearchSources(1)
	for name, setup := range map[string]func(*memorySharedVectors){
		"unreachable": func(m *memorySharedVectors) { m.lookupErr = errors.New("connection refused") },
		"non-finite": func(m *memorySharedVectors) {
			m.replaceAll([]storage.SearchVectorChunk{{Index: 0, Vector: []float32{float32(math.NaN()), 1}}})
		},
		"zero norm": func(m *memorySharedVectors) {
			m.replaceAll([]storage.SearchVectorChunk{{Index: 0, Vector: []float32{0, 0}}})
		},
		"wrong dimensions": func(m *memorySharedVectors) {
			m.replaceAll([]storage.SearchVectorChunk{{Index: 0, Vector: []float32{1, 0, 0}}})
		},
		"wrong chunks": func(m *memorySharedVectors) {
			m.replaceAll([]storage.SearchVectorChunk{{Index: 0, Vector: []float32{1, 0}}, {Index: 1, Vector: []float32{1, 0}}})
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			shared := newMemorySharedVectors()
			shared.push(sources[0].ReviewUUID)
			newSharingDaemon(t, sources, shared).reconcile(t)
			require.Equal(t, 1, shared.rowCount())
			setup(shared)

			peer := newSharingDaemon(t, sources, shared)
			peer.reconcile(t)
			assert.Equal(t, 1, peer.embeddedTexts(), "the peer embeds the review itself")
			assert.Equal(t, int64(1), peer.reconciler.Health().Embedded)
		})
	}
}

func TestSharedVectorsMissForDifferentText(t *testing.T) {
	t.Parallel()
	sources := makeSearchSources(1)
	shared := newMemorySharedVectors()
	shared.push(sources[0].ReviewUUID)
	newSharingDaemon(t, sources, shared).reconcile(t)

	edited := makeSearchSources(1)
	edited[0].Output = "edited review output"
	peer := newSharingDaemon(t, edited, shared)
	peer.reconcile(t)
	assert.Equal(t, 1, peer.embeddedTexts())
	assert.Equal(t, 2, shared.rowCount(), "each exact text is its own row")
}
