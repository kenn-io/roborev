package searchindex

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/vector"

	"go.kenn.io/roborev/internal/searchdoc"
	"go.kenn.io/roborev/internal/storage"
)

func sharedTestDocument(id int64, output string, state storage.SearchShareState) searchdoc.Document {
	source := testDocument(id, output).Source
	source.ShareState = state
	return searchdoc.Render(source)
}

func exchangeRow(t *testing.T, index *Index, doc string) (hash string, firstPending int64, attempts int) {
	t.Helper()
	require.NoError(t, index.db.QueryRowContext(context.Background(),
		`SELECT content_hash, first_pending_at, attempts FROM review_exchange WHERE doc_key = ?`, doc).
		Scan(&hash, &firstPending, &attempts))
	return hash, firstPending, attempts
}

func TestValidateExchangeRecord(t *testing.T) {
	unit := []float32{0.6, 0.8}
	chunks := func(vectors ...[]float32) []storage.VectorChunk {
		out := make([]storage.VectorChunk, len(vectors))
		for i, values := range vectors {
			out[i] = storage.VectorChunk{Index: i, Vector: values}
		}
		return out
	}
	tooMany := make([][]float32, 65)
	for i := range tooMany {
		tooMany[i] = unit
	}
	for _, tc := range []struct {
		name    string
		content string
		record  storage.VectorRecord
		want    []vector.ChunkVector
		err     string
	}{
		{
			name: "ok", record: storage.VectorRecord{Status: "ok", Dims: 2, Chunks: chunks(unit)},
			want: []vector.ChunkVector{{ChunkIndex: 0, Vector: unit}},
		},
		{name: "skipped", record: storage.VectorRecord{Status: "skipped", Dims: 2}},
		{
			name: "skipped with vectors", record: storage.VectorRecord{Status: "skipped", Dims: 2, Chunks: chunks(unit)},
			err: "skipped record carries vectors",
		},
		{
			name: "record dims", record: storage.VectorRecord{Status: "ok", Dims: 3, Chunks: chunks(unit)},
			err: "record has 3 dimensions, generation expects 2",
		},
		{
			name: "vector length", record: storage.VectorRecord{Status: "ok", Dims: 2, Chunks: chunks([]float32{1, 0, 0})},
			err: "chunk 0 has 3 dimensions, expected 2",
		},
		{
			name: "nan", record: storage.VectorRecord{Status: "ok", Dims: 2, Chunks: chunks([]float32{float32(math.NaN()), 1})},
			err: "chunk 0 is not finite",
		},
		{
			name: "inf", record: storage.VectorRecord{Status: "ok", Dims: 2, Chunks: chunks([]float32{float32(math.Inf(1)), 0})},
			err: "chunk 0 is not finite",
		},
		{
			name: "short norm", record: storage.VectorRecord{Status: "ok", Dims: 2, Chunks: chunks([]float32{0.5, 0})},
			err: "chunk 0 has norm 0.5000 outside [0.99, 1.01]",
		},
		{
			name: "long norm", record: storage.VectorRecord{Status: "ok", Dims: 2, Chunks: chunks([]float32{1.1, 0})},
			err: "chunk 0 has norm 1.1000 outside [0.99, 1.01]",
		},
		{
			name: "no chunks", record: storage.VectorRecord{Status: "ok", Dims: 2},
			err: "record has 0 chunks, local text splits into 1",
		},
		{
			name: "wrong chunk count", record: storage.VectorRecord{Status: "ok", Dims: 2, Chunks: chunks(tooMany...)},
			err: "record has 65 chunks, local text splits into 1",
		},
		{name: "duplicate index", content: strings.Repeat("x", 2001), record: storage.VectorRecord{Status: "ok", Dims: 2, Chunks: []storage.VectorChunk{
			{Index: 0, Vector: unit}, {Index: 0, Vector: unit},
		}}, err: "chunk index 0 repeats"},
		{name: "wrong index", content: strings.Repeat("x", 2001), record: storage.VectorRecord{Status: "ok", Dims: 2, Chunks: []storage.VectorChunk{
			{Index: 0, Vector: unit}, {Index: 2, Vector: unit},
		}}, err: "chunk 1 has index 2, want 1"},
		{name: "negative index", record: storage.VectorRecord{Status: "ok", Dims: 2, Chunks: []storage.VectorChunk{
			{Index: -1, Vector: unit},
		}}, err: "chunk index -1 is negative"},
		{
			name: "malformed", record: storage.VectorRecord{Status: "ok", Dims: 2, Malformed: true},
			err: "record arrays are malformed",
		},
		{
			name: "unknown status", record: storage.VectorRecord{Status: "pending", Dims: 2},
			err: `record status "pending" is unknown`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			content := tc.content
			if content == "" {
				content = "shared text"
			}
			got, err := validateExchangeRecord(tc.record, content, 2)
			if tc.err != "" {
				require.EqualError(t, err, tc.err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestValidateExchangeRecordAcceptsMoreThan64LocalChunks(t *testing.T) {
	content := strings.Repeat("x", 117200) // 65 chunks at 2000 runes with 200 overlap.
	chunks := make([]storage.VectorChunk, 65)
	for i := range chunks {
		chunks[i] = storage.VectorChunk{Index: i, Vector: []float32{1, 0}}
	}
	vectors, err := validateExchangeRecord(storage.VectorRecord{
		Status: storage.VectorStatusOK, Dims: 2, Chunks: chunks,
	}, content, 2)
	require.NoError(t, err)
	require.Len(t, vectors, 65)
	assert.Equal(t, 64, vectors[64].ChunkIndex)
}

func TestObserveSharedPendingTracksGenerationAndText(t *testing.T) {
	ctx := context.Background()
	index := openGenerationTestIndex(t)
	shared := sharedTestDocument(1, "shared text", storage.SearchSharePeer)
	stamped := sharedTestDocument(2, "already embedded", storage.SearchShareOwn)
	local := sharedTestDocument(3, "local text", storage.SearchShareLocal)
	_, err := index.RefreshMirrorPage(ctx, []searchdoc.Document{shared, stamped, local}, nil)
	require.NoError(t, err)
	key, err := index.EnsureGeneration(ctx, vector.Generation{Model: "model", Dimensions: 2})
	require.NoError(t, err)
	require.NoError(t, index.SaveGenerationVectors(ctx, key,
		vector.Pending[string]{Doc: stamped.DocKey, Revision: stamped.ContentHash},
		[]vector.ChunkVector{{ChunkIndex: 0, Vector: vector.Vector{1, 0}}}))

	t0 := time.Unix(1_800_000_000, 0)
	require.NoError(t, index.observeSharedPending(ctx, key, t0))
	hash, first, attempts := exchangeRow(t, index, shared.DocKey)
	assert.Equal(t, shared.ContentHash, hash)
	assert.Equal(t, t0.Unix(), first)
	assert.Zero(t, attempts)
	assert.Zero(t, countRowsForDoc(t, index.db, "review_exchange", stamped.DocKey), "covered documents are not pending")
	assert.Zero(t, countRowsForDoc(t, index.db, "review_exchange", local.DocKey), "local documents never use the exchange")

	candidate := exchangeCandidate{DocKey: shared.DocKey, ContentHash: shared.ContentHash}
	require.NoError(t, index.recordLookup(ctx, key, candidate, t0.Add(time.Minute)))
	require.NoError(t, index.observeSharedPending(ctx, key, t0.Add(time.Hour)))
	_, first, attempts = exchangeRow(t, index, shared.DocKey)
	assert.Equal(t, t0.Unix(), first, "the same text keeps its first-pending time")
	assert.Equal(t, 1, attempts)

	edited := sharedTestDocument(1, "shared text edited", storage.SearchSharePeer)
	_, err = index.RefreshMirrorPage(ctx, []searchdoc.Document{edited}, nil)
	require.NoError(t, err)
	t2 := t0.Add(2 * time.Hour)
	require.NoError(t, index.observeSharedPending(ctx, key, t2))
	hash, first, attempts = exchangeRow(t, index, shared.DocKey)
	assert.Equal(t, edited.ContentHash, hash)
	assert.Equal(t, t2.Unix(), first, "new text restarts the fallback clock")
	assert.Zero(t, attempts)
}

func TestForgetPublishedTargetMakesCoveredDocumentsPublishable(t *testing.T) {
	ctx := context.Background()
	index := openGenerationTestIndex(t)
	doc := sharedTestDocument(1, "shared text", storage.SearchShareOwn)
	_, err := index.RefreshMirrorPage(ctx, []searchdoc.Document{doc}, nil)
	require.NoError(t, err)
	key, err := index.EnsureGeneration(ctx, vector.Generation{Model: "model", Dimensions: 2})
	require.NoError(t, err)
	require.NoError(t, index.SaveGenerationVectors(ctx, key,
		vector.Pending[string]{Doc: doc.DocKey, Revision: doc.ContentHash},
		[]vector.ChunkVector{{ChunkIndex: 0, Vector: vector.Vector{1, 0}}}))
	now := time.Unix(1_800_000_000, 0)
	require.NoError(t, index.recordExchanged(ctx, key, doc.DocKey, doc.ContentHash,
		exchangeOriginLocal, "target-1", now))
	candidates, err := index.publishCandidates(ctx, key, "target-1", 10)
	require.NoError(t, err)
	assert.Empty(t, candidates)

	require.NoError(t, index.forgetPublishedTarget(ctx, key, "target-1"))
	candidates, err = index.publishCandidates(ctx, key, "target-1", 10)
	require.NoError(t, err)
	assert.Equal(t, []publishCandidate{{DocKey: doc.DocKey, ContentHash: doc.ContentHash}}, candidates)
}

func TestPublishCandidatesReadChunkVectorsAndRecordExchanged(t *testing.T) {
	ctx := context.Background()
	index := openGenerationTestIndex(t)
	withVectors := sharedTestDocument(1, "two chunks", storage.SearchShareOwn)
	skipped := sharedTestDocument(2, "provider rejected", storage.SearchSharePeer)
	local := sharedTestDocument(3, "local only", storage.SearchShareLocal)
	_, err := index.RefreshMirrorPage(ctx, []searchdoc.Document{withVectors, skipped, local}, nil)
	require.NoError(t, err)
	key, err := index.EnsureGeneration(ctx, vector.Generation{Model: "model", Dimensions: 2})
	require.NoError(t, err)
	stored := []vector.ChunkVector{
		{ChunkIndex: 0, Vector: vector.Vector{0.6, 0.8}},
		{ChunkIndex: 2, Vector: vector.Vector{0, 1}},
	}
	for _, save := range []struct {
		doc     searchdoc.Document
		vectors []vector.ChunkVector
	}{{withVectors, stored}, {skipped, nil}, {local, stored[:1]}} {
		require.NoError(t, index.SaveGenerationVectors(ctx, key,
			vector.Pending[string]{Doc: save.doc.DocKey, Revision: save.doc.ContentHash}, save.vectors))
	}

	candidates, err := index.publishCandidates(ctx, key, "target-1", 10)
	require.NoError(t, err)
	assert.Equal(t, []publishCandidate{
		{DocKey: withVectors.DocKey, ContentHash: withVectors.ContentHash},
		{DocKey: skipped.DocKey, ContentHash: skipped.ContentHash},
	}, candidates, "local-only documents are never published")

	chunks, err := index.readChunkVectors(ctx, key, withVectors.DocKey)
	require.NoError(t, err)
	assert.Equal(t, []storage.VectorChunk{
		{Index: 0, Vector: []float32{0.6, 0.8}}, {Index: 2, Vector: []float32{0, 1}},
	}, chunks)
	chunks, err = index.readChunkVectors(ctx, key, skipped.DocKey)
	require.NoError(t, err)
	assert.Empty(t, chunks)

	now := time.Unix(1_800_000_000, 0)
	require.NoError(t, index.recordExchanged(ctx, key, withVectors.DocKey, withVectors.ContentHash,
		exchangeOriginLocal, "target-1", now))
	candidates, err = index.publishCandidates(ctx, key, "target-1", 10)
	require.NoError(t, err)
	assert.Equal(t, []publishCandidate{{DocKey: skipped.DocKey, ContentHash: skipped.ContentHash}}, candidates)
	candidates, err = index.publishCandidates(ctx, key, "target-2", 10)
	require.NoError(t, err)
	assert.Len(t, candidates, 2, "a different sync database needs every record again")
}

func TestPublishCandidatesIncludesMoreThan64Chunks(t *testing.T) {
	ctx := context.Background()
	index := openGenerationTestIndex(t)
	doc := sharedTestDocument(1, strings.Repeat("x", 117200), storage.SearchShareOwn)
	_, err := index.RefreshMirrorPage(ctx, []searchdoc.Document{doc}, nil)
	require.NoError(t, err)
	key, err := index.EnsureGeneration(ctx, vector.Generation{Model: "model", Dimensions: 2})
	require.NoError(t, err)
	vectors := make([]vector.ChunkVector, 65)
	for i := range vectors {
		vectors[i] = vector.ChunkVector{ChunkIndex: i, Vector: vector.Vector{1, 0}}
	}
	require.NoError(t, index.SaveGenerationVectors(ctx, key,
		vector.Pending[string]{Doc: doc.DocKey, Revision: doc.ContentHash}, vectors))

	candidates, err := index.publishCandidates(ctx, key, "target", 10)
	require.NoError(t, err)
	assert.Equal(t, []publishCandidate{{DocKey: doc.DocKey, ContentHash: doc.ContentHash}}, candidates)
	chunks, err := index.readChunkVectors(ctx, key, doc.DocKey)
	require.NoError(t, err)
	assert.Len(t, chunks, 65)
}

func TestSharedActivationWaitsForLocalDocumentsAndFirstAttempts(t *testing.T) {
	ctx := context.Background()
	index := openGenerationTestIndex(t)
	local := sharedTestDocument(1, "local", storage.SearchShareLocal)
	shared := sharedTestDocument(2, "shared", storage.SearchSharePeer)
	_, err := index.RefreshMirrorPage(ctx, []searchdoc.Document{local, shared}, nil)
	require.NoError(t, err)
	key, err := index.EnsureGeneration(ctx, vector.Generation{Model: "model", Dimensions: 2})
	require.NoError(t, err)
	now := time.Unix(1_800_000_000, 0)
	cutoff := now.Add(-defaultLocalFallbackAfter)
	require.NoError(t, index.observeSharedPending(ctx, key, now))

	require.ErrorContains(t, index.ActivateSharedGeneration(ctx, key, now, cutoff), "2 documents remain")
	require.NoError(t, index.SaveGenerationVectors(ctx, key,
		vector.Pending[string]{Doc: local.DocKey, Revision: local.ContentHash},
		[]vector.ChunkVector{{ChunkIndex: 0, Vector: vector.Vector{1, 0}}}))
	require.ErrorContains(t, index.ActivateSharedGeneration(ctx, key, now, cutoff), "1 documents remain",
		"a shared document must have one lookup first")

	require.NoError(t, index.recordLookup(ctx, key,
		exchangeCandidate{DocKey: shared.DocKey, ContentHash: shared.ContentHash}, now.Add(time.Minute)))
	require.NoError(t, index.ActivateSharedGeneration(ctx, key, now, cutoff))
	active, ok, err := index.ActiveGeneration(ctx)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, key, active.Key)
	counts, err := index.GenerationCounts(ctx, key)
	require.NoError(t, err)
	assert.Equal(t, int64(1), counts.Backlog, "the shared document is still pending: search reports partial")
}

func TestSharedClaimLifecycleBookkeeping(t *testing.T) {
	ctx := context.Background()
	index := openGenerationTestIndex(t)
	shared := sharedTestDocument(1, "claimed text", storage.SearchShareOwn)
	local := sharedTestDocument(2, "local text", storage.SearchShareLocal)
	_, err := index.RefreshMirrorPage(ctx, []searchdoc.Document{shared, local}, nil)
	require.NoError(t, err)
	key, err := index.EnsureGeneration(ctx, vector.Generation{Model: "model", Dimensions: 2})
	require.NoError(t, err)
	now := time.Unix(1_800_000_000, 0)
	cutoff := now.Add(-defaultLocalFallbackAfter)
	require.NoError(t, index.observeSharedPending(ctx, key, now))

	due, err := index.dueSharedCandidates(ctx, key, now, cutoff, 10)
	require.NoError(t, err)
	require.Len(t, due, 1)
	assert.Equal(t, exchangeCandidate{
		DocKey: shared.DocKey, ContentHash: shared.ContentHash,
		ShareState: storage.SearchShareOwn, FirstPendingAt: now,
	}, due[0])
	backlog, err := index.localFillBacklog(ctx, key, now, cutoff, true)
	require.NoError(t, err)
	assert.Equal(t, int64(1), backlog, "only the local document may be embedded before a claim")

	expires := now.Add(defaultClaimTTL)
	require.NoError(t, index.recordClaim(ctx, key, due[0], expires))
	due, err = index.dueSharedCandidates(ctx, key, now, cutoff, 10)
	require.NoError(t, err)
	assert.Empty(t, due, "a held claim is not looked up again")
	backlog, err = index.localFillBacklog(ctx, key, now, cutoff, true)
	require.NoError(t, err)
	assert.Equal(t, int64(2), backlog, "the claimed document joins the local fill")
	next, err := index.nextExchangeDue(ctx, key, defaultLocalFallbackAfter)
	require.NoError(t, err)
	assert.Equal(t, expires, next)

	require.NoError(t, index.SaveGenerationVectors(ctx, key,
		vector.Pending[string]{Doc: shared.DocKey, Revision: shared.ContentHash},
		[]vector.ChunkVector{{ChunkIndex: 0, Vector: vector.Vector{1, 0}}}))
	finished, err := index.finishedClaims(ctx, key)
	require.NoError(t, err)
	assert.Equal(t, []publishCandidate{{DocKey: shared.DocKey, ContentHash: shared.ContentHash}}, finished)
	require.NoError(t, index.clearClaim(ctx, key, shared.DocKey, shared.ContentHash))
	finished, err = index.finishedClaims(ctx, key)
	require.NoError(t, err)
	assert.Empty(t, finished)

	later := now.Add(defaultLocalFallbackAfter)
	backlog, err = index.localFillBacklog(ctx, key, later, later.Add(-defaultLocalFallbackAfter), true)
	require.NoError(t, err)
	assert.Equal(t, int64(1), backlog, "at the fallback deadline only the uncovered local document remains")
}
