package searchindex

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/vector"
	"go.kenn.io/kit/vector/sqlitevec"

	"go.kenn.io/roborev/internal/searchdoc"
)

func TestReopenPreservesExistingGenerationAndVectors(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "reviews.search.db")
	index, err := Open(ctx, path)
	require.NoError(t, err)

	doc := testDocument(1, "durable semantic content")
	_, err = index.RefreshMirrorPage(ctx, []searchdoc.Document{doc}, nil)
	require.NoError(t, err)
	model := vector.Generation{Model: "durability-test", Dimensions: 2}
	key, err := index.EnsureGeneration(ctx, model)
	require.NoError(t, err)
	pending, err := index.PendingGeneration(ctx, key, 1)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	require.NoError(t, index.SaveGenerationVectors(ctx, key, pending[0],
		[]vector.ChunkVector{{ChunkIndex: 0, Vector: vector.Vector{1, 0}}}))
	require.NoError(t, index.ActivateGeneration(ctx, key))
	require.NoError(t, index.Close())

	index, err = Open(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, index.Close()) })

	active, ok, err := index.ActiveGeneration(ctx)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, GenerationInfo{
		Key:         key,
		Fingerprint: model.Fingerprint(),
		Dimensions:  model.Dimensions,
		State:       sqlitevec.StateActive,
	}, active)
	hits, err := index.QueryGeneration(ctx, key, vector.Vector{1, 0}, 10)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	assert.Equal(t, doc.DocKey, hits[0].Doc)
}
