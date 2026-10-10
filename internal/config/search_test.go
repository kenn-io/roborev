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
	assert := assert.New(t)
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
	assert.Equal(embedconfig.Embedder{
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
	}, cfg.Search.Embeddings.Embedder)
	assert.Equal("title: none | text: ", cfg.Search.Embeddings.DocumentPrefix)
	assert.Equal("task: search result | query: ", cfg.Search.Embeddings.QueryPrefix)
	// Load uses BurntSushi TOML, while save uses pelletier TOML. Both codecs
	// must keep the promoted standard keys in the original flat table.
	require.NoError(t, SaveGlobalTo(path, cfg))
	loaded, err := LoadGlobalFrom(path)
	require.NoError(t, err)
	assert.Equal(cfg.Search.Embeddings, loaded.Search.Embeddings)
	raw, err := LoadRawTOML(path)
	require.NoError(t, err)
	assert.True(IsKeyInTOMLFile(raw, "search.embeddings.base_url"))
	assert.False(IsKeyInTOMLFile(raw, "search.embeddings.Embedder"))
}

func TestSearchConfigRejectsAnInvalidEmbedderAtLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(path, []byte("[search.embeddings]\nbase_url = \"https://api.example.test/v1\"\n"), 0o600))

	_, err := LoadGlobalFrom(path)
	require.ErrorContains(t, err, "search.embeddings: ")
}

func TestSearchConfigRejectsPrefixesWithoutCoreSettings(t *testing.T) {
	for _, key := range []string{"document_prefix", "query_prefix"} {
		for _, value := range []string{"task: search result | query: ", " "} {
			path := filepath.Join(t.TempDir(), "config.toml")
			require.NoError(t, os.WriteFile(path, []byte("[search.embeddings]\n"+key+" = \""+value+"\"\n"), 0o600))
			_, err := LoadGlobalFrom(path)
			require.ErrorContains(t, err, "search.embeddings:")
		}
	}
}

func TestSearchEmbeddingAPIKeyIsSensitive(t *testing.T) {
	assert.True(t, IsSensitiveKey("search.embeddings.api_key"))
	assert.False(t, IsSensitiveKey("search.embeddings.model"))
}

func TestSearchConfigEmptyEmbeddingSettingsStayDisabled(t *testing.T) {
	for _, contents := range []string{"", "[search.embeddings]\n", "[search.embeddings]\ndocument_prefix = \"\"\nquery_prefix = \"\"\n"} {
		path := filepath.Join(t.TempDir(), "config.toml")
		require.NoError(t, os.WriteFile(path, []byte(contents), 0o600))
		cfg, err := LoadGlobalFrom(path)
		require.NoError(t, err)
		if contents == "" {
			assert.Nil(t, cfg.Search.Embeddings)
		} else {
			require.NotNil(t, cfg.Search.Embeddings)
			assert.False(t, cfg.Search.Embeddings.Enabled())
		}
	}
}

func TestSearchPrefixesAreLiteralGlobalConfigKeys(t *testing.T) {
	assert := assert.New(t)
	cfg := DefaultConfig()
	cfg.Search.Embeddings = &SearchEmbeddingsConfig{
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
	require.NoError(t, cfg.Search.Embeddings.Validate())
	parts, err := cfg.Search.Embeddings.Parts()
	require.NoError(t, err)
	assert.Equal(" ", parts.Roles.DocumentPrefix)
	assert.Equal(" ", parts.Roles.QueryPrefix)
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
		cfg.Search.Embeddings = &SearchEmbeddingsConfig{
			BaseURL: "https://api.example.test/v1", Model: "embed-large", Dims: 8, APIKey: key,
		}
		require.NoError(t, SaveGlobalTo(path, cfg))

		loaded, err := LoadGlobalFrom(path)
		require.NoError(t, err)
		require.NotNil(t, loaded.Search.Embeddings)
		assert.Equal(t, key, loaded.Search.Embeddings.APIKey)
	}
}
