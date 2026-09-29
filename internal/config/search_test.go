package config

import (
	"os"
	"path/filepath"
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
