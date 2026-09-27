package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
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
		key, err := (EmbeddingConfig{APIKeyEnv: name}).ResolveAPIKey()
		require.NoError(t, err)
		assert.Empty(t, key)
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

func TestEmbeddingAPIKeyFile(t *testing.T) {
	for _, tc := range []struct {
		name, contents string
		mode           os.FileMode
		want           string
	}{
		{"private", "file-secret\n", 0o600, "file-secret"},
		{"CRLF", "file-secret\r\n", 0o600, "file-secret"},
		{"empty", "\n", 0o600, ""},
		{"readable", "file-secret", 0o644, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "embedding.key")
			require.NoError(t, os.WriteFile(path, []byte(tc.contents), tc.mode))
			cfgPath := filepath.Join(t.TempDir(), "config.toml")
			require.NoError(t, os.WriteFile(cfgPath, []byte(fmt.Sprintf(`[search.embeddings]
base_url = "https://api.example.test/v1"
model = "test"
dims = 2
api_key_file = %q
`, filepath.ToSlash(path))), 0o600))
			cfg, err := LoadGlobalFrom(cfgPath)
			require.NoError(t, err)
			key, err := cfg.Search.Embeddings.ResolveAPIKey()
			require.NoError(t, err)
			want := tc.want
			if runtime.GOOS == "windows" && tc.name == "readable" {
				want = "file-secret"
			}
			assert.Equal(t, want, key)
		})
	}
}

func FuzzEmbeddingKeySources(f *testing.F) {
	for _, flags := range []uint8{0, 1, 2, 3, 4, 5, 6, 7} {
		f.Add(flags)
	}
	f.Fuzz(func(t *testing.T, flags uint8) {
		sources := ""
		count := 0
		for i, line := range []string{`api_key = "secret"`, `api_key_env = "EMBEDDING_KEY"`, `api_key_file = "missing.key"`} {
			if flags&(1<<i) != 0 {
				sources += line + "\n"
				count++
			}
		}
		path := filepath.Join(t.TempDir(), "config.toml")
		require.NoError(t, os.WriteFile(path, []byte("[search.embeddings]\nbase_url = \"https://api.example.test/v1\"\nmodel = \"test\"\ndims = 2\n"+sources), 0o600))
		cfg, err := LoadGlobalFrom(path)
		if count > 1 {
			require.ErrorContains(t, err, "mutually exclusive")
			return
		}
		require.NoError(t, err)
		_, err = cfg.Search.Embeddings.ResolveAPIKey()
		require.NoError(t, err)
	})
}

func TestEmbeddingCredentialFileHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	require.NoError(t, os.WriteFile(filepath.Join(home, "embedding.key"), []byte("file-secret\n"), 0o600))
	credential, err := (EmbeddingConfig{APIKeyFile: "~/embedding.key"}).ResolveCredential()
	require.NoError(t, err)
	assert.Equal(t, "file-secret", credential.Key)
	assert.Equal(t, "file:~/embedding.key", credential.Source)
	assert.Empty(t, credential.Reason)
}

func TestEmbeddingCredentialUnavailableSources(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		cfg                    EmbeddingConfig
		wantSource, wantReason string
	}{
		{"missing file", EmbeddingConfig{APIKeyFile: filepath.Join(t.TempDir(), "missing.key")}, "", "missing or unreadable"},
		{"whitespace inline", EmbeddingConfig{APIKey: " "}, "inline", "inline key is empty"},
		{"missing env", EmbeddingConfig{APIKeyEnv: "ROBOREV_TEST_NO_KEY"}, "env:ROBOREV_TEST_NO_KEY", "env ROBOREV_TEST_NO_KEY is unset"},
		{"no source", EmbeddingConfig{}, "", "no key source"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ROBOREV_TEST_NO_KEY", "")
			credential, err := tc.cfg.ResolveCredential()
			require.NoError(t, err)
			assert.Empty(t, credential.Key)
			if tc.wantSource != "" {
				assert.Equal(t, tc.wantSource, credential.Source)
			}
			assert.Contains(t, credential.Reason, tc.wantReason)
		})
	}
}

func TestEmbeddingKeyFileConflicts(t *testing.T) {
	for _, cfg := range []EmbeddingConfig{
		{APIKey: "secret", APIKeyFile: "missing.key"},
		{APIKeyEnv: "EMBEDDING_KEY", APIKeyFile: "missing.key"},
	} {
		_, err := cfg.ResolveAPIKey()
		require.ErrorContains(t, err, "mutually exclusive")
	}
}
