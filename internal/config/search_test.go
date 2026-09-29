package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/embedconfig"
)

func TestSearchConfigLoadsKitEmbedderKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(path, []byte(`[search.embeddings]
base_url = "https://api.example.test/v1"
model = "embed-large"
dims = 1024
api_key = "env:EMBEDDING_API_KEY"
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
		APIKey:              "env:EMBEDDING_API_KEY",
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
