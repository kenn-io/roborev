package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSearchConfigLoadsCompleteEmbeddingBlock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(path, []byte(`[search.embeddings]
base_url = "https://api.example.test/v1"
model = "embed-large"
dims = 1024
api_key_env = "EMBEDDING_API_KEY"
input_type_mode = "retrieval"
fingerprint_salt = "deployment-v2"
batch_size = 12
timeout_seconds = 9
trust_private_network = true
`), 0o600))

	cfg, err := LoadGlobalFrom(path)
	require.NoError(t, err)
	require.NotNil(t, cfg.Search.Embeddings)
	assert.Equal(t, EmbeddingConfig{
		BaseURL:             "https://api.example.test/v1",
		Model:               "embed-large",
		Dims:                1024,
		APIKeyEnv:           "EMBEDDING_API_KEY",
		InputTypeMode:       "retrieval",
		FingerprintSalt:     "deployment-v2",
		BatchSize:           12,
		TimeoutSeconds:      9,
		TrustPrivateNetwork: true,
	}, *cfg.Search.Embeddings)
}

func TestSearchConfigAppliesEmbeddingDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(path, []byte(`[search.embeddings]
base_url = "https://api.example.test/v1"
model = "embed-large"
dims = 1024
`), 0o600))

	cfg, err := LoadGlobalFrom(path)
	require.NoError(t, err)
	require.NotNil(t, cfg.Search.Embeddings)
	assert.Equal(t, "none", cfg.Search.Embeddings.InputTypeMode)
	assert.Equal(t, 64, cfg.Search.Embeddings.BatchSize)
	assert.Equal(t, 30, cfg.Search.Embeddings.TimeoutSeconds)
}

func TestSearchConfigRejectsPartialEmbeddingBlocks(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "base URL only", body: `base_url = "https://api.example.test/v1"`},
		{name: "model only", body: `model = "embed-large"`},
		{name: "dimensions only", body: `dims = 1024`},
		{name: "missing dimensions", body: "base_url = \"https://api.example.test/v1\"\nmodel = \"embed-large\""},
		{name: "settings without endpoint", body: `api_key_env = "EMBEDDING_API_KEY"`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			body := "[search.embeddings]\n" + tt.body + "\n"
			require.NoError(t, os.WriteFile(path, []byte(body), 0o600))

			_, err := LoadGlobalFrom(path)
			require.EqualError(t, err,
				"config: search.embeddings base_url, model, and dims must be configured together")
		})
	}
}

func TestSearchConfigRejectsMutuallyExclusiveEmbeddingKeySources(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(path, []byte(`[search.embeddings]
base_url = "https://api.example.test/v1"
model = "embed-large"
dims = 1024
api_key = "inline-secret"
api_key_env = "EMBEDDING_API_KEY"
`), 0o600))

	_, err := LoadGlobalFrom(path)
	require.EqualError(t, err,
		"config: search.embeddings api_key and api_key_env are mutually exclusive")
}

func TestSearchConfigRejectsInvalidEmbeddingInputTypeMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(path, []byte(`[search.embeddings]
base_url = "https://api.example.test/v1"
model = "embed-large"
dims = 1024
input_type_mode = "classification"
`), 0o600))

	_, err := LoadGlobalFrom(path)
	require.EqualError(t, err,
		`config: search.embeddings input_type_mode must be "none" or "retrieval"`)
}

func TestEmbeddingConfigResolveAPIKey(t *testing.T) {
	t.Run("inline", func(t *testing.T) {
		got, err := (EmbeddingConfig{APIKey: "inline-secret"}).ResolveAPIKey()
		require.NoError(t, err)
		assert.Equal(t, "inline-secret", got)
	})

	t.Run("environment", func(t *testing.T) {
		t.Setenv("ROBOREV_TEST_EMBEDDING_KEY", "environment-secret")
		got, err := (EmbeddingConfig{APIKeyEnv: "ROBOREV_TEST_EMBEDDING_KEY"}).ResolveAPIKey()
		require.NoError(t, err)
		assert.Equal(t, "environment-secret", got)
	})

	t.Run("missing environment variable", func(t *testing.T) {
		const name = "ROBOREV_TEST_MISSING_EMBEDDING_KEY"
		require.NoError(t, os.Unsetenv(name))
		_, err := (EmbeddingConfig{APIKeyEnv: name}).ResolveAPIKey()
		require.EqualError(t, err,
			`embedding API key environment variable "ROBOREV_TEST_MISSING_EMBEDDING_KEY" is missing or empty`)
	})
}

func TestSearchConfigIsGlobalOnly(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".roborev.toml"), []byte(`[search.embeddings]
base_url = "https://api.example.test/v1"
model = "embed-large"
dims = 1024
`), 0o600))

	_, err := LoadRepoConfig(dir)
	require.EqualError(t, err,
		`repository config key "search" is global-only; move it to ~/.roborev/config.toml`)
}
