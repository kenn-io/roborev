package searchindex

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/vector"

	"go.kenn.io/roborev/internal/searchdoc"
)

func TestGenerationIsUnavailableUntilCompleteAndReplacementRequiresMatchingFingerprint(t *testing.T) {
	ctx := context.Background()
	index := openGenerationTestIndex(t)
	first := vector.Generation{Model: "first", Dimensions: 2}
	second := vector.Generation{Model: "second", Dimensions: 3}

	firstKey, err := index.EnsureGeneration(ctx, first)
	require.NoError(t, err)
	_, ok, err := index.ActiveGeneration(ctx)
	require.NoError(t, err)
	assert.False(t, ok)
	available, err := index.GenerationAvailable(ctx, first.Fingerprint())
	require.NoError(t, err)
	assert.False(t, available)

	require.NoError(t, index.ActivateGeneration(ctx, firstKey))
	active, ok, err := index.ActiveGeneration(ctx)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, firstKey, active.Key)
	available, err = index.GenerationAvailable(ctx, first.Fingerprint())
	require.NoError(t, err)
	assert.True(t, available)

	secondKey, err := index.EnsureGeneration(ctx, second)
	require.NoError(t, err)
	available, err = index.GenerationAvailable(ctx, second.Fingerprint())
	require.NoError(t, err)
	assert.False(t, available,
		"old vectors must not be queried with a new client")
	active, ok, err = index.ActiveGeneration(ctx)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, firstKey, active.Key, "old generation stays stored while replacement builds")
	assert.NotEqual(t, firstKey, secondKey)
}

func TestGenerationCutoverReclaimsRetiredVectorTables(t *testing.T) {
	ctx := context.Background()
	index := openGenerationTestIndex(t)
	doc := testDocument(1, "generation lifecycle")
	_, err := index.RefreshMirrorPage(ctx, []searchdoc.Document{doc}, nil)
	require.NoError(t, err)

	first := vector.Generation{Model: "first", Dimensions: 2}
	firstKey, err := index.EnsureGeneration(ctx, first)
	require.NoError(t, err)
	pending, err := index.PendingGeneration(ctx, firstKey, 1)
	require.NoError(t, err)
	require.NoError(t, index.SaveGenerationVectors(ctx, firstKey, pending[0],
		[]vector.ChunkVector{{ChunkIndex: 0, Vector: vector.Vector{1, 0}}}))
	require.NoError(t, index.ActivateGeneration(ctx, firstKey))
	hits, err := index.QueryGeneration(ctx, firstKey, vector.Vector{1, 0}, 5)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	assert.Equal(t, doc.DocKey, hits[0].Doc)

	var oldTable string
	err = index.db.QueryRowContext(ctx, `
		SELECT 'review_vectors_v' || ordinal
		  FROM review_vectors_generations WHERE gen_key = ?`, firstKey).Scan(&oldTable)
	require.NoError(t, err)

	second := vector.Generation{Model: "second", Dimensions: 3}
	secondKey, err := index.EnsureGeneration(ctx, second)
	require.NoError(t, err)
	pending, err = index.PendingGeneration(ctx, secondKey, 1)
	require.NoError(t, err)
	require.NoError(t, index.SaveGenerationVectors(ctx, secondKey, pending[0],
		[]vector.ChunkVector{{ChunkIndex: 0, Vector: vector.Vector{0, 1, 0}}}))
	require.NoError(t, index.ActivateGeneration(ctx, secondKey))

	active, ok, err := index.ActiveGeneration(ctx)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, secondKey, active.Key)
	var oldTables int
	err = index.db.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, oldTable).Scan(&oldTables)
	require.NoError(t, err)
	assert.Zero(t, oldTables)
	var oldRows int
	err = index.db.QueryRowContext(ctx,
		`SELECT count(*) FROM review_vectors_generations WHERE gen_key = ?`, firstKey).Scan(&oldRows)
	require.NoError(t, err)
	assert.Zero(t, oldRows)
}

func TestGenerationSaveRejectsStaleMirrorRevision(t *testing.T) {
	ctx := context.Background()
	index := openGenerationTestIndex(t)
	doc := testDocument(1, "before")
	_, err := index.RefreshMirrorPage(ctx, []searchdoc.Document{doc}, nil)
	require.NoError(t, err)

	model := vector.Generation{Model: "model", Dimensions: 2}
	key, err := index.EnsureGeneration(ctx, model)
	require.NoError(t, err)
	pending, err := index.PendingGeneration(ctx, key, 1)
	require.NoError(t, err)
	require.Len(t, pending, 1)

	updated := testDocument(1, "after")
	_, err = index.RefreshMirrorPage(ctx, []searchdoc.Document{updated}, nil)
	require.NoError(t, err)
	err = index.SaveGenerationVectors(ctx, key, pending[0],
		[]vector.ChunkVector{{ChunkIndex: 0, Vector: vector.Vector{1, 0}}})
	require.ErrorIs(t, err, vector.ErrStale)

	pending, err = index.PendingGeneration(ctx, key, 1)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, updated.ContentHash, pending[0].Revision)
}

func TestGenerationCountsEmbeddedSkippedAndBacklog(t *testing.T) {
	ctx := context.Background()
	index := openGenerationTestIndex(t)
	docs := []searchdoc.Document{
		testDocument(1, "embedded"),
		testDocument(2, "skipped"),
		testDocument(3, "pending"),
	}
	_, err := index.RefreshMirrorPage(ctx, docs, nil)
	require.NoError(t, err)
	key, err := index.EnsureGeneration(ctx, vector.Generation{Model: "model", Dimensions: 2})
	require.NoError(t, err)
	pending, err := index.PendingGeneration(ctx, key, 3)
	require.NoError(t, err)
	require.NoError(t, index.SaveGenerationVectors(ctx, key, pending[0],
		[]vector.ChunkVector{{ChunkIndex: 0, Vector: vector.Vector{1, 0}}}))
	require.NoError(t, index.SaveGenerationVectors(ctx, key, pending[1], nil))

	counts, err := index.GenerationCounts(ctx, key)
	require.NoError(t, err)
	assert.Equal(t, int64(1), counts.Embedded)
	assert.Equal(t, int64(1), counts.Skipped)
	assert.Equal(t, int64(1), counts.Backlog)
}

func TestGenerationCutoverChecksCompletenessInsideTransaction(t *testing.T) {
	ctx := context.Background()
	index := openGenerationTestIndex(t)
	key, err := index.EnsureGeneration(ctx, vector.Generation{Model: "model", Dimensions: 2})
	require.NoError(t, err)

	tx, err := index.db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	require.NoError(t, upsertMirrorRow(ctx, tx, rowForDocument(testDocument(1, "arrived during cutover"))))

	err = index.activateGenerationTx(ctx, tx, key)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "1 documents remain")
}

func openGenerationTestIndex(t *testing.T) *Index {
	t.Helper()
	index, err := Open(context.Background(), filepath.Join(t.TempDir(), "reviews.search.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, index.Close()) })
	return index
}

func generationTableOrdinal(t *testing.T, db *sql.DB, key string) int64 {
	t.Helper()
	var ordinal int64
	require.NoError(t, db.QueryRow(`SELECT ordinal FROM review_vectors_generations WHERE gen_key = ?`, key).Scan(&ordinal))
	return ordinal
}

func generationVectorCount(t *testing.T, index *Index, key string) int {
	t.Helper()
	var count int
	table := fmt.Sprintf("review_vectors_v%d", generationTableOrdinal(t, index.db, key))
	require.NoError(t, index.db.QueryRow(`SELECT count(*) FROM `+table).Scan(&count))
	return count
}
