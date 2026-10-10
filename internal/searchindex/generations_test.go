package searchindex

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/embedconfig"
	"go.kenn.io/kit/embedmodel"
	"go.kenn.io/kit/vector"
	"go.kenn.io/kit/vector/sqlitevec"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/searchdoc"
)

func TestGenerationIsUnavailableUntilCompleteAndReplacementRequiresMatchingFingerprint(t *testing.T) {
	ctx := context.Background()
	index := openGenerationTestIndex(t)
	first := testSpace("first", 2)
	second := testSpace("second", 3)

	firstKey, err := index.ResolveGeneration(ctx, first)
	require.NoError(t, err)
	_, ok, err := index.ActiveGeneration(ctx)
	require.NoError(t, err)
	assert.False(t, ok)
	_, serving, err := index.ServingGeneration(ctx, first)
	require.NoError(t, err)
	assert.False(t, serving, "a building generation never serves")

	require.NoError(t, index.ActivateGeneration(ctx, firstKey))
	active, ok, err := index.ActiveGeneration(ctx)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, firstKey, active.Key)
	served, serving, err := index.ServingGeneration(ctx, first)
	require.NoError(t, err)
	assert.True(t, serving)
	assert.Equal(t, firstKey, served.Key)

	secondKey, err := index.ResolveGeneration(ctx, second)
	require.NoError(t, err)
	_, serving, err = index.ServingGeneration(ctx, second)
	require.NoError(t, err)
	assert.False(t, serving, "old vectors must not be queried with a new client")
	active, ok, err = index.ActiveGeneration(ctx)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, firstKey, active.Key, "old generation stays stored while replacement builds")
	assert.NotEqual(t, firstKey, secondKey)

	again, err := index.ResolveGeneration(ctx, second)
	require.NoError(t, err)
	assert.Equal(t, secondKey, again, "the building generation is reused, not recreated")
}

func TestGenerationStoredByEarlierReleaseKeepsServingWithoutReembedding(t *testing.T) {
	ctx := context.Background()
	index := openGenerationTestIndex(t)
	doc := testDocument(1, "legacy generation")
	_, err := index.RefreshMirrorPage(ctx, []searchdoc.Document{doc}, nil)
	require.NoError(t, err)

	settings := config.SearchEmbeddingsConfig{
		BaseURL: "https://api.voyageai.com/v1", Model: "voyage-4-large", Dims: 2,
		InputTypeMode: "retrieval",
	}
	// This is exactly how the pre-kit client keyed and fingerprinted the
	// generation: the key and the stored fingerprint are both its legacy form.
	legacy := vector.Generation{Model: settings.Model, Dimensions: settings.Dims, Params: map[string]string{
		"endpoint": "https://api.voyageai.com/v1", "input_type_mode": "retrieval", "recipe": "2",
	}}
	key := legacy.Fingerprint()
	require.NoError(t, index.vectors.EnsureGeneration(ctx, key, legacy, sqlitevec.StateBuilding))
	pending, err := index.PendingGeneration(ctx, key, 1)
	require.NoError(t, err)
	require.NoError(t, index.SaveGenerationVectors(ctx, key, pending[0],
		[]vector.ChunkVector{{ChunkIndex: 0, Vector: vector.Vector{1, 0}}}))
	require.NoError(t, index.ActivateGeneration(ctx, key))

	embeddings, err := NewEmbeddings(settings, "", searchdoc.RecipeVersion)
	require.NoError(t, err)
	resolved, err := index.ResolveGeneration(ctx, embeddings.Space())
	require.NoError(t, err)
	assert.Equal(t, key, resolved)
	served, serving, err := index.ServingGeneration(ctx, embeddings.Space())
	require.NoError(t, err)
	require.True(t, serving)
	assert.Equal(t, key, served.Key)
	counts, err := index.GenerationCounts(ctx, resolved)
	require.NoError(t, err)
	assert.Equal(t, GenerationCounts{Embedded: 1}, counts)
	generations, err := index.vectors.Generations(ctx)
	require.NoError(t, err)
	assert.Len(t, generations, 1, "no replacement generation is created")

	candidates, err := flatten(index.SearchSemantic(ctx, served.Key, vector.Vector{1, 0}, 5, SearchFilters{}))
	require.NoError(t, err)
	require.Len(t, candidates, 1)
	assert.Equal(t, doc.DocKey, candidates[0].DocKey)
}

func TestLegacyFingerprintMatchesEarlierReleases(t *testing.T) {
	// Fingerprints computed by the pre-kit embedding client for these settings.
	tests := []struct {
		settings config.SearchEmbeddingsConfig
		want     string
	}{
		{
			settings: config.SearchEmbeddingsConfig{
				BaseURL: "https://api.voyageai.com/v1", Model: "voyage-4-large", Dims: 1024,
				InputTypeMode: "retrieval",
			},
			want: "28b082fb2ca22d4d",
		},
		{
			settings: config.SearchEmbeddingsConfig{
				BaseURL: "http://127.0.0.1:11434/v1/", Model: "nomic", Dims: 768,
				FingerprintSalt: "s1",
			},
			want: "ad50719e7306a26f",
		},
	}
	for _, tt := range tests {
		embeddings, err := NewEmbeddings(tt.settings, "", 2)
		require.NoError(t, err)
		assert.Equal(t, []string{tt.want}, embeddings.Space().Legacy)
		matches, err := embeddings.Space().Matches(tt.want)
		require.NoError(t, err)
		assert.True(t, matches)
	}
}

func TestNewGenerationsUseKitIdentity(t *testing.T) {
	ctx := context.Background()
	index := openGenerationTestIndex(t)
	embeddings, err := NewEmbeddings(config.SearchEmbeddingsConfig{
		BaseURL: "https://example.test/v1", Model: "model", Dims: 2,
	}, "", 2)
	require.NoError(t, err)
	key, err := index.ResolveGeneration(ctx, embeddings.Space())
	require.NoError(t, err)
	kit, err := embeddings.Space().Generation()
	require.NoError(t, err)
	assert.Equal(t, kit.Fingerprint(), key)
	assert.NotEqual(t, embeddings.Space().Legacy[0], key)
}

func TestGenerationIdentitySeparatesVectorSpaceInputs(t *testing.T) {
	type input struct {
		config config.SearchEmbeddingsConfig
		recipe int
	}
	base := input{
		config: config.SearchEmbeddingsConfig{
			BaseURL: "http://127.0.0.1:9/v1", Model: "embed-large", Dims: 1024, InputTypeMode: "none",
		},
		recipe: 3,
	}
	variants := []func(*input){
		func(*input) {},
		func(in *input) { in.config.Model = "embed-other" },
		func(in *input) { in.config.Dims = 768 },
		func(in *input) { in.recipe = 4 },
		func(in *input) { in.config.InputTypeMode = "retrieval" },
		func(in *input) { in.config.FingerprintSalt = "deployment-v2" },
		func(in *input) { in.config.BaseURL = "http://127.0.0.1:10/v1" },
		func(in *input) { in.config.BaseURL = "http://127.0.0.1:9/v2" },
	}
	legacy := map[string]struct{}{}
	kit := map[string]struct{}{}
	for _, variant := range variants {
		in := base
		variant(&in)
		embeddings, err := NewEmbeddings(in.config, "", in.recipe)
		require.NoError(t, err)
		legacy[embeddings.Space().Legacy[0]] = struct{}{}
		generation, err := embeddings.Space().Generation()
		require.NoError(t, err)
		kit[generation.Fingerprint()] = struct{}{}
	}
	assert.Len(t, legacy, len(variants))
	assert.Len(t, kit, len(variants))

	secret, err := NewEmbeddings(base.config, "secret", base.recipe)
	require.NoError(t, err)
	plain, err := NewEmbeddings(base.config, "", base.recipe)
	require.NoError(t, err)
	assert.Equal(t, plain.Space().Legacy, secret.Space().Legacy)
}

func TestGenerationCutoverReclaimsRetiredVectorTables(t *testing.T) {
	ctx := context.Background()
	index := openGenerationTestIndex(t)
	doc := testDocument(1, "generation lifecycle")
	_, err := index.RefreshMirrorPage(ctx, []searchdoc.Document{doc}, nil)
	require.NoError(t, err)

	first := testSpace("first", 2)
	firstKey, err := index.ResolveGeneration(ctx, first)
	require.NoError(t, err)
	pending, err := index.PendingGeneration(ctx, firstKey, 1)
	require.NoError(t, err)
	require.NoError(t, index.SaveGenerationVectors(ctx, firstKey, pending[0],
		[]vector.ChunkVector{{ChunkIndex: 0, Vector: vector.Vector{1, 0}}}))
	require.NoError(t, index.ActivateGeneration(ctx, firstKey))
	candidates, err := flatten(index.SearchSemantic(ctx, firstKey, vector.Vector{1, 0}, 5, SearchFilters{}))
	require.NoError(t, err)
	require.Len(t, candidates, 1)
	assert.Equal(t, doc.DocKey, candidates[0].DocKey)

	oldTable := fmt.Sprintf("review_vectors_v%d", generationTableOrdinal(t, index.db, firstKey))

	second := testSpace("second", 3)
	secondKey, err := index.ResolveGeneration(ctx, second)
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
	var oldChunks int
	err = index.db.QueryRowContext(ctx,
		`SELECT count(*) FROM review_vectors_chunks c JOIN review_vectors_generations g USING (ordinal)
		  WHERE g.gen_key = ?`, firstKey).Scan(&oldChunks)
	require.NoError(t, err)
	assert.Zero(t, oldChunks)

	returned, err := index.ResolveGeneration(ctx, first)
	require.NoError(t, err)
	assert.NotEqual(t, firstKey, returned, "a retired generation is never refilled")
	_, serving, err := index.ServingGeneration(ctx, first)
	require.NoError(t, err)
	assert.False(t, serving)
}

func TestGenerationSaveRejectsStaleMirrorRevision(t *testing.T) {
	ctx := context.Background()
	index := openGenerationTestIndex(t)
	doc := testDocument(1, "before")
	_, err := index.RefreshMirrorPage(ctx, []searchdoc.Document{doc}, nil)
	require.NoError(t, err)

	key, err := index.ResolveGeneration(ctx, testSpace("model", 2))
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
	key, err := index.ResolveGeneration(ctx, testSpace("model", 2))
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

func TestGenerationCutoverRefusesUncoveredDocuments(t *testing.T) {
	ctx := context.Background()
	index := openGenerationTestIndex(t)
	key, err := index.ResolveGeneration(ctx, testSpace("model", 2))
	require.NoError(t, err)
	_, err = index.RefreshMirrorPage(ctx, []searchdoc.Document{testDocument(1, "arrived before cutover")}, nil)
	require.NoError(t, err)

	err = index.ActivateGeneration(ctx, key)
	require.ErrorIs(t, err, sqlitevec.ErrUncovered)
	_, ok, err := index.ActiveGeneration(ctx)
	require.NoError(t, err)
	assert.False(t, ok)
}

func testSpace(model string, dims int) embedmodel.Descriptor {
	return embedmodel.Descriptor{Model: embedconfig.Model{
		Name: model, Dimensions: dims,
		Metric: embedconfig.MetricCosine, Normalization: embedconfig.NormalizationL2,
	}}
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
