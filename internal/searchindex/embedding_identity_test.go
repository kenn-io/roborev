package searchindex

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/embedconfig"
	"go.kenn.io/kit/embedmodel"
	"go.kenn.io/kit/secretref"
	"go.kenn.io/kit/vector"
	"go.kenn.io/kit/vector/sqlitevec"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/searchdoc"
)

func identityFixture() config.SearchEmbeddingsConfig {
	return config.SearchEmbeddingsConfig{
		BaseURL: "https://example.test/v1", Model: "embeddinggemma-2-text-r1", Dims: 768,
	}
}

type embeddingIdentity struct{ vector, input, generation, shared string }

func identities(t *testing.T, settings config.SearchEmbeddingsConfig, recipe int) (embedmodel.Descriptor, embeddingIdentity) {
	t.Helper()
	client, err := NewEmbeddings(settings, "", recipe)
	require.NoError(t, err)
	space := client.Space()
	v, err := space.VectorIdentity()
	require.NoError(t, err)
	i, err := space.InputIdentity()
	require.NoError(t, err)
	g, err := space.Generation()
	require.NoError(t, err)
	s, err := sharedSpace(space)
	require.NoError(t, err)
	return space, embeddingIdentity{v, i, g.Fingerprint(), s}
}

func TestEmptyPrefixesPreserveOriginalIdentityGoldens(t *testing.T) {
	// Recorded from the untouched pre-prefix adapter with recipe 2, before
	// implementation. These pins cover current Kit and shared-vector identity.
	for _, tc := range []struct {
		settings embedconfig.Embedder
		want     embeddingIdentity
		legacy   string
	}{
		{
			embedconfig.Embedder{BaseURL: "https://api.voyageai.com/v1", Model: "voyage-4-large", Dims: 1024, InputTypeMode: "retrieval"},
			embeddingIdentity{"a64ee4ec7eb52b78ab77e5219ae1839fe8afc3fdf3c7c445500e614ac703cbce", "a64ee4ec7eb52b78ab77e5219ae1839fe8afc3fdf3c7c445500e614ac703cbce", "534c5b3284d50bb0", "df340d3b83c20297"},
			"28b082fb2ca22d4d",
		},
		{
			embedconfig.Embedder{BaseURL: "http://127.0.0.1:11434/v1/", Model: "nomic", Dims: 768, FingerprintSalt: "s1"},
			embeddingIdentity{"d90a2839a7fd6b67c5654f496b20d085d0931e936c2d65f87530a7a3ea2127bc", "d90a2839a7fd6b67c5654f496b20d085d0931e936c2d65f87530a7a3ea2127bc", "54d60b696107314f", "1fbba4a8e110a7e6"},
			"ad50719e7306a26f",
		},
	} {
		space, got := identities(t, config.SearchEmbeddingsConfig{Embedder: tc.settings}, 2)
		assert.Equal(t, tc.want, got)
		assert.Equal(t, []string{tc.legacy}, space.Legacy)
	}
}

func TestEmbeddingIdentitySeparatesEncodingControls(t *testing.T) {
	base := identityFixture()
	old, original := identities(t, base, 2)
	for _, tc := range []struct {
		name   string
		change func(*config.SearchEmbeddingsConfig, *int)
	}{
		{"model", func(c *config.SearchEmbeddingsConfig, _ *int) { c.Model = "embeddinggemma-2-text-r2" }},
		{"width", func(c *config.SearchEmbeddingsConfig, _ *int) { c.Dims = 512 }},
		{"endpoint", func(c *config.SearchEmbeddingsConfig, _ *int) { c.BaseURL = "https://other.example.test/v1" }},
		{"input type", func(c *config.SearchEmbeddingsConfig, _ *int) { c.InputTypeMode = "retrieval" }},
		{"salt", func(c *config.SearchEmbeddingsConfig, _ *int) { c.FingerprintSalt = "deployment-2" }},
		{"recipe", func(_ *config.SearchEmbeddingsConfig, recipe *int) { *recipe = 3 }},
		{"document prefix", func(c *config.SearchEmbeddingsConfig, _ *int) { c.DocumentPrefix = "title: none | text: " }},
		{"query prefix", func(c *config.SearchEmbeddingsConfig, _ *int) { c.QueryPrefix = "task: search result | query: " }},
		{"whitespace prefix", func(c *config.SearchEmbeddingsConfig, _ *int) { c.QueryPrefix = " " }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			settings, recipe := base, 2
			tc.change(&settings, &recipe)
			space, changed := identities(t, settings, recipe)
			assert := assert.New(t)
			assert.NotEqual(original.vector, changed.vector)
			assert.NotEqual(original.input, changed.input)
			assert.NotEqual(original.generation, changed.generation)
			assert.NotEqual(original.shared, changed.shared)
			for _, previous := range []string{original.generation, old.Legacy[0]} {
				matches, err := space.Matches(previous)
				require.NoError(t, err)
				assert.False(matches)
			}
			matches, err := space.Matches(changed.generation)
			require.NoError(t, err)
			assert.True(matches)
		})
	}
}

func TestEmbeddingOperationalSettingsPreserveIdentities(t *testing.T) {
	assert := assert.New(t)
	for _, prefixed := range []bool{false, true} {
		base := identityFixture()
		if prefixed {
			base.DocumentPrefix, base.QueryPrefix = "title: none | text: ", "task: search result | query: "
		}
		old, original := identities(t, base, 2)
		for _, change := range []func(*config.SearchEmbeddingsConfig){
			func(c *config.SearchEmbeddingsConfig) { c.BatchSize = 4 },
			func(c *config.SearchEmbeddingsConfig) { c.TimeoutSeconds = 120 },
			func(c *config.SearchEmbeddingsConfig) { c.ModelContextTokens, c.MaxBatchTokens = 8192, 32768 },
			func(c *config.SearchEmbeddingsConfig) { c.TrustPrivateNetwork = true },
			func(c *config.SearchEmbeddingsConfig) { c.APIKey = secretref.Literal("synthetic-key") },
		} {
			changed := base
			change(&changed)
			require.NoError(t, changed.Validate())
			space, got := identities(t, changed, 2)
			assert.Equal(original, got)
			assert.Equal(old.Legacy, space.Legacy)
		}
	}
}

func TestChangedPrefixesCannotServePersistedGenerations(t *testing.T) {
	for _, format := range []string{"legacy", "kit"} {
		for _, role := range []string{"document", "query"} {
			t.Run(format+"/"+role, func(t *testing.T) {
				assert := assert.New(t)
				var requests atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					requests.Add(1)
					w.WriteHeader(http.StatusInternalServerError)
				}))
				defer server.Close()
				settings := identityFixture()
				settings.BaseURL = server.URL + "/v1"
				settings.TrustPrivateNetwork = true
				// Prepare historical rows independently of the wrapper's prefixes.
				parts, err := settings.Embedder.Parts()
				require.NoError(t, err)
				parts.Deployment.PinEndpoint = true
				parts.Roles.DocumentFormatter = "roborev.searchdoc/v2"
				old := embedmodel.Descriptor{Model: parts.Model, Roles: parts.Roles, Deployment: parts.Deployment}
				generation, err := old.Generation()
				require.NoError(t, err)
				if format == "legacy" {
					generation = vector.Generation{Model: settings.Model, Dimensions: settings.Dims, Params: map[string]string{
						"endpoint": server.URL + "/v1", "input_type_mode": "none", "recipe": "2",
					}}
				}
				key := generation.Fingerprint()
				index := openGenerationTestIndex(t)
				doc := queryTestDocument(1, "panel", "synthetic needle", queryDocOptions{})
				_, err = index.RefreshMirrorPage(t.Context(), []searchdoc.Document{doc}, nil)
				require.NoError(t, err)
				require.NoError(t, index.vectors.EnsureGeneration(t.Context(), key, generation, sqlitevec.StateBuilding))
				pending, err := index.PendingGeneration(t.Context(), key, 1)
				require.NoError(t, err)
				require.Len(t, pending, 1)
				require.NoError(t, index.SaveGenerationVectors(t.Context(), key, pending[0], []vector.ChunkVector{{ChunkIndex: 0, Vector: unitFixture(768)}}))
				require.NoError(t, index.ActivateGeneration(t.Context(), key))
				control, err := NewEmbeddings(settings, "", 2)
				require.NoError(t, err)
				reused, err := index.ResolveGeneration(t.Context(), control.Space())
				require.NoError(t, err)
				assert.Equal(key, reused)
				served, ok, err := index.ServingGeneration(t.Context(), control.Space())
				require.NoError(t, err)
				require.True(t, ok)
				assert.Equal(key, served.Key)
				if role == "document" {
					settings.DocumentPrefix = "title: none | text: "
				} else {
					settings.QueryPrefix = "task: search result | query: "
				}
				changed, err := NewEmbeddings(settings, "", 2)
				require.NoError(t, err)
				assert.Empty(changed.Space().Legacy)
				replacement, err := index.ResolveGeneration(t.Context(), changed.Space())
				require.NoError(t, err)
				assert.NotEqual(key, replacement)
				again, err := index.ResolveGeneration(t.Context(), changed.Space())
				require.NoError(t, err)
				assert.Equal(replacement, again)
				_, ok, err = index.ServingGeneration(t.Context(), changed.Space())
				require.NoError(t, err)
				assert.False(ok)
				generations, err := index.vectors.Generations(t.Context())
				require.NoError(t, err)
				require.Len(t, generations, 2)
				states := make(map[string]sqlitevec.State, len(generations))
				for _, g := range generations {
					states[g.Key] = g.State
				}
				assert.Equal(sqlitevec.StateBuilding, states[replacement])
				counts, err := index.GenerationCounts(t.Context(), key)
				require.NoError(t, err)
				assert.Equal(int64(1), counts.Embedded, "old vectors remain stored")
				service := NewService(newServiceStore(doc), index, changed, activeServiceRuntime(changed.Space()))
				auto, err := service.Search(t.Context(), SearchParams{Query: "needle", Mode: ModeAuto})
				require.NoError(t, err)
				assert.Equal(ModeLexical, auto.Mode)
				assert.True(auto.Degraded)
				require.Len(t, auto.Hits, 1)
				assert.Equal(doc.Source.JobID, auto.Hits[0].JobID)
				for _, mode := range []SearchMode{ModeSemantic, ModeHybrid} {
					_, err := service.Search(t.Context(), SearchParams{Query: "needle", Mode: mode})
					var unavailable *ModeError
					require.ErrorAs(t, err, &unavailable)
					assert.Equal(http.StatusServiceUnavailable, unavailable.Status)
				}
				assert.Zero(requests.Load(), "stable resolveMode incompatibility blocks query dispatch")
			})
		}
	}
}

func FuzzEmbeddingPrefixIdentity(f *testing.F) {
	for _, pair := range [][2]string{{"", " "}, {" ", ""}, {"one", "two"}, {"", "title: none | text: "}, {"", "task: search result | query: "}, {"λ", "猫"}, {"a\nb", "a\\nb"}, {"x=y", "x|y"}} {
		for _, query := range []bool{false, true} {
			f.Add(pair[0], pair[1], query)
		}
	}
	f.Fuzz(func(t *testing.T, before, after string, query bool) {
		// Bound materialized test fixtures only; production prefixes have no cap.
		bounded := func(s string) string {
			out := make([]rune, 0, 64)
			for _, r := range s {
				out = append(out, r)
				if len(out) == 64 {
					break
				}
			}
			return string(out)
		}
		before, after = bounded(before), bounded(after)
		if before == after {
			t.Skip("unchanged after materialization")
		}
		require.True(t, utf8.ValidString(before))
		require.True(t, utf8.ValidString(after))
		a, b := identityFixture(), identityFixture()
		if query {
			a.QueryPrefix, b.QueryPrefix = before, after
		} else {
			a.DocumentPrefix, b.DocumentPrefix = before, after
		}
		previous, oldID := identities(t, a, 2)
		changed, newID := identities(t, b, 2)
		assert := assert.New(t)
		assert.NotEqual(oldID.vector, newID.vector)
		assert.NotEqual(oldID.input, newID.input)
		assert.NotEqual(oldID.generation, newID.generation)
		assert.NotEqual(oldID.shared, newID.shared)
		for _, oldKey := range append([]string{oldID.generation}, previous.Legacy...) {
			matches, err := changed.Matches(oldKey)
			require.NoError(t, err)
			assert.False(matches)
		}
		if after != "" {
			assert.Empty(changed.Legacy)
		}
		own, err := changed.Matches(newID.generation)
		require.NoError(t, err)
		assert.True(own)
	})
}
