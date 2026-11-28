package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/embedconfig"
	"go.kenn.io/kit/secretref"
)

func TestSearchConfigLoadsKitEmbedderKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(path, []byte(`[search.embeddings]
base_url = "https://api.example.test/v1"
model = "embed-large"
dims = 1024
api_key = { env = "EMBEDDING_API_KEY" }
input_type_mode = "retrieval"
fingerprint_salt = "deployment-v2"
batch_size = 12
model_context_tokens = 512
max_batch_tokens = 8192
timeout_seconds = 9
trust_private_network = true
document_prefix = "title: none | text: "
query_prefix = "task: search result | query: "
`), 0o600))

	cfg, err := LoadGlobalFrom(path)
	require.NoError(t, err)
	require.NotNil(t, cfg.Search.Embeddings)
	assert.Equal(t, embedconfig.Embedder{
		BaseURL:             "https://api.example.test/v1",
		Model:               "embed-large",
		Dims:                1024,
		APIKey:              secretref.Ref{Env: "EMBEDDING_API_KEY"},
		InputTypeMode:       "retrieval",
		FingerprintSalt:     "deployment-v2",
		BatchSize:           12,
		ModelContextTokens:  512,
		MaxBatchTokens:      8192,
		TimeoutSeconds:      9,
		TrustPrivateNetwork: true,
		DocumentPrefix:      "title: none | text: ",
		QueryPrefix:         "task: search result | query: ",
	}, *cfg.Search.Embeddings)
}

func TestSearchConfigRejectsAnInvalidEmbedderAtLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(path, []byte("[search.embeddings]\nbase_url = \"https://api.example.test/v1\"\n"), 0o600))

	_, err := LoadGlobalFrom(path)
	require.ErrorContains(t, err, "search.embeddings: ")
}

func TestSearchEmbeddingAPIKeyIsSensitive(t *testing.T) {
	assert.True(t, IsSensitiveKey("search.embeddings.api_key"))
	assert.False(t, IsSensitiveKey("search.embeddings.model"))
}

func TestSearchPrefixesAreLiteralGlobalConfigKeys(t *testing.T) {
	assert := assert.New(t)
	cfg := DefaultConfig()
	cfg.Search.Embeddings = &embedconfig.Embedder{
		BaseURL: "https://example.test/v1", Model: "test", Dims: 768,
	}
	for _, key := range []string{"search.embeddings.document_prefix", "search.embeddings.query_prefix"} {
		assert.True(IsGlobalKey(key))
		assert.False(hasLeafConfigKey(reflect.ValueOf(RepoConfig{}), key))
		require.NoError(t, SetConfigValue(cfg, key, " "))
		got, err := GetConfigValue(cfg, key)
		require.NoError(t, err)
		assert.Equal(" ", got)
		assert.Contains(ListConfigKeys(cfg), KeyValue{Key: key, Value: " "})
	}
}

func TestSearchEmbeddingAPIKeyIsOneConfigValue(t *testing.T) {
	assert.True(t, IsValidKey("search.embeddings.api_key"))
	assert.False(t, IsValidKey("search.embeddings.api_key.env"))

	cfg := DefaultConfig()
	for input, want := range map[string]secretref.Ref{
		`{ env = "EMBEDDING_API_KEY" }`:    {Env: "EMBEDDING_API_KEY"},
		`{ file = "~/.config/embed.key" }`: {File: "~/.config/embed.key"},
		`"sk-quoted"`:                      secretref.Literal("sk-quoted"),
		`sk-bare`:                          secretref.Literal("sk-bare"),
	} {
		require.NoError(t, SetConfigValue(cfg, "search.embeddings.api_key", input), input)
		require.NotNil(t, cfg.Search.Embeddings)
		assert.Equal(t, want, cfg.Search.Embeddings.APIKey, input)
	}
	require.NoError(t, SetConfigValue(cfg, "search.embeddings.api_key", `{ env = "EMBEDDING_API_KEY" }`))
	got, err := GetConfigValue(cfg, "search.embeddings.api_key")
	require.NoError(t, err)
	assert.Equal(t, `{ env = "EMBEDDING_API_KEY" }`, got)
	require.Error(t, SetConfigValue(cfg, "search.embeddings.api_key", `{ vault = "embed" }`))
}

func TestSearchEmbeddingAPIKeySurvivesSaveAndLoad(t *testing.T) {
	for _, key := range []secretref.Ref{
		{Env: "EMBEDDING_API_KEY"},
		{File: "~/.config/embed.key"},
		secretref.Literal("sk-inline"),
	} {
		path := filepath.Join(t.TempDir(), "config.toml")
		cfg := DefaultConfig()
		cfg.Search.Embeddings = &embedconfig.Embedder{
			BaseURL: "https://api.example.test/v1", Model: "embed-large", Dims: 8, APIKey: key,
		}
		require.NoError(t, SaveGlobalTo(path, cfg))

		loaded, err := LoadGlobalFrom(path)
		require.NoError(t, err)
		require.NotNil(t, loaded.Search.Embeddings)
		assert.Equal(t, key, loaded.Search.Embeddings.APIKey)
	}
}

func TestSearchEmbeddingsDeprecatedAPIKeyEnvMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(path, []byte(`[search.embeddings]
base_url = "https://api.voyageai.com/v1"
model = "voyage-3-lite"
dims = 512
api_key_env = "VOYAGE_API_KEY"
`), 0o600))

	cfg, err := LoadGlobalFrom(path)
	require.NoError(t, err)
	require.NotNil(t, cfg.Search.Embeddings)
	assert.Equal(t, secretref.Ref{Env: "VOYAGE_API_KEY"}, cfg.Search.Embeddings.APIKey)
	legacyEnv, legacyNote := cfg.SearchEmbeddingsLegacyAPIKeyEnv()
	assert.Equal(t, "VOYAGE_API_KEY", legacyEnv)
	assert.Empty(t, legacyNote)
	require.Len(t, cfg.DeprecationWarnings(), 1)
	assert.Contains(t, cfg.DeprecationWarnings()[0], `api_key = { env = "VOYAGE_API_KEY" }`)
}

func TestSearchEmbeddingsExplicitAPIKeyWinsOverDeprecatedKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(path, []byte(`[search.embeddings]
base_url = "https://api.voyageai.com/v1"
model = "voyage-3-lite"
dims = 512
api_key_env = "OLD_KEY"
api_key = { env = "NEW_KEY" }
`), 0o600))

	cfg, err := LoadGlobalFrom(path)
	require.NoError(t, err)
	require.NotNil(t, cfg.Search.Embeddings)
	assert.Equal(t, secretref.Ref{Env: "NEW_KEY"}, cfg.Search.Embeddings.APIKey)
	legacyEnv, legacyNote := cfg.SearchEmbeddingsLegacyAPIKeyEnv()
	assert.Empty(t, legacyEnv)
	assert.Empty(t, legacyNote)
	assert.NotEmpty(t, cfg.DeprecationWarnings())
}

func TestSearchEmbeddingsEmptyDeprecatedAPIKeyEnvSetsNote(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(path, []byte(`[search.embeddings]
base_url = "https://api.voyageai.com/v1"
model = "voyage-3-lite"
dims = 512
api_key_env = ""
`), 0o600))

	cfg, err := LoadGlobalFrom(path)
	require.NoError(t, err)
	legacyEnv, legacyNote := cfg.SearchEmbeddingsLegacyAPIKeyEnv()
	assert.Empty(t, legacyEnv)
	assert.Contains(t, legacyNote, `api_key = { env = "NAME" }`)
}

func TestSearchEmbeddingsInvalidLegacyAPIKeyEnvSurvivesSaveAndReload(t *testing.T) {
	for _, tc := range []struct {
		name      string
		rawLine   string
		wantError bool
	}{
		{name: "empty string", rawLine: `api_key_env = ""`},
		{name: "non-string", rawLine: `api_key_env = 123`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			require.NoError(t, os.WriteFile(path, []byte(`[search.embeddings]
base_url = "https://api.voyageai.com/v1"
model = "voyage-3-lite"
dims = 512
`+tc.rawLine+"\n"), 0o600))

			cfg, err := LoadGlobalFrom(path)
			require.NoError(t, err)
			cfg.DefaultAgent = "claude"
			require.NoError(t, SaveGlobalTo(path, cfg))

			reloaded, err := LoadGlobalFrom(path)
			require.NoError(t, err)
			assert.Equal(t, "claude", reloaded.DefaultAgent)
			require.NotNil(t, reloaded.Search.Embeddings)
			assert.True(t, reloaded.Search.Embeddings.APIKey.IsZero())
			_, legacyNote := reloaded.SearchEmbeddingsLegacyAPIKeyEnv()
			assert.Contains(t, legacyNote, `api_key = { env = "NAME" }`)
			assert.NotEmpty(t, reloaded.DeprecationWarnings())

			// Once explicitly corrected with api_key, the legacy key is not re-emitted.
			reloaded.Search.Embeddings.APIKey = secretref.Ref{Env: "VOYAGE_API_KEY"}
			require.NoError(t, SaveGlobalTo(path, reloaded))
			fixed, err := LoadGlobalFrom(path)
			require.NoError(t, err)
			assert.Equal(t, secretref.Ref{Env: "VOYAGE_API_KEY"}, fixed.Search.Embeddings.APIKey)
			_, fixedNote := fixed.SearchEmbeddingsLegacyAPIKeyEnv()
			assert.Empty(t, fixedNote)
		})
	}
}
