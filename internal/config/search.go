package config

import (
	"fmt"
	"os"
	"strings"
)

const (
	defaultEmbeddingBatchSize      = 64
	defaultEmbeddingTimeoutSeconds = 30
)

// SearchConfig controls semantic review-history search.
type SearchConfig struct {
	Embeddings *EmbeddingConfig `toml:"embeddings"`
}

// EmbeddingConfig configures the provider-neutral embeddings endpoint.
type EmbeddingConfig struct {
	BaseURL             string `toml:"base_url"`
	Model               string `toml:"model"`
	Dims                int    `toml:"dims"`
	APIKey              string `toml:"api_key" sensitive:"true"`
	APIKeyEnv           string `toml:"api_key_env"`
	InputTypeMode       string `toml:"input_type_mode"`
	FingerprintSalt     string `toml:"fingerprint_salt"`
	BatchSize           int    `toml:"batch_size"`
	TimeoutSeconds      int    `toml:"timeout_seconds"`
	TrustPrivateNetwork bool   `toml:"trust_private_network"`
}

// ResolveAPIKey returns the inline key or resolves the configured environment
// variable. Callers should invoke it only while starting the daemon so config
// loading and inspection never depend on process-local credentials.
func (c EmbeddingConfig) ResolveAPIKey() (string, error) {
	if c.APIKey != "" && strings.TrimSpace(c.APIKeyEnv) != "" {
		return "", fmt.Errorf("search.embeddings api_key and api_key_env are mutually exclusive")
	}
	if c.APIKey != "" {
		return c.APIKey, nil
	}
	name := strings.TrimSpace(c.APIKeyEnv)
	if name == "" {
		return "", nil
	}
	value, ok := os.LookupEnv(name)
	if !ok || strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("embedding API key environment variable %q is missing or empty", name)
	}
	return value, nil
}

func normalizeSearchConfig(search *SearchConfig) error {
	if search == nil || search.Embeddings == nil {
		return nil
	}
	embeddings := search.Embeddings
	if err := validateEmbeddingConfig(embeddings); err != nil {
		return err
	}
	if !embeddingConfigEnabled(embeddings) {
		return nil
	}
	if embeddings.InputTypeMode == "" {
		embeddings.InputTypeMode = "none"
	}
	if embeddings.BatchSize == 0 {
		embeddings.BatchSize = defaultEmbeddingBatchSize
	}
	if embeddings.TimeoutSeconds == 0 {
		embeddings.TimeoutSeconds = defaultEmbeddingTimeoutSeconds
	}
	return nil
}

func validateEmbeddingConfig(embeddings *EmbeddingConfig) error {
	if embeddings == nil {
		return nil
	}

	if embeddings.APIKey != "" && strings.TrimSpace(embeddings.APIKeyEnv) != "" {
		return fmt.Errorf("search.embeddings api_key and api_key_env are mutually exclusive")
	}

	baseSet := strings.TrimSpace(embeddings.BaseURL) != ""
	modelSet := strings.TrimSpace(embeddings.Model) != ""
	dimsSet := embeddings.Dims > 0
	anySet := baseSet || modelSet || embeddings.Dims != 0 || embeddings.APIKey != "" ||
		strings.TrimSpace(embeddings.APIKeyEnv) != "" || embeddings.InputTypeMode != "" ||
		embeddings.FingerprintSalt != "" || embeddings.BatchSize != 0 ||
		embeddings.TimeoutSeconds != 0 || embeddings.TrustPrivateNetwork
	if anySet && (!baseSet || !modelSet || !dimsSet) {
		return fmt.Errorf("search.embeddings base_url, model, and dims must be configured together")
	}

	mode := embeddings.InputTypeMode
	if mode != "" && mode != "none" && mode != "retrieval" {
		return fmt.Errorf("search.embeddings input_type_mode must be %q or %q", "none", "retrieval")
	}
	if embeddings.BatchSize < 0 {
		return fmt.Errorf("search.embeddings batch_size must be positive")
	}
	if embeddings.TimeoutSeconds < 0 {
		return fmt.Errorf("search.embeddings timeout_seconds must be positive")
	}
	return nil
}

func embeddingConfigEnabled(embeddings *EmbeddingConfig) bool {
	return embeddings != nil && strings.TrimSpace(embeddings.BaseURL) != "" &&
		strings.TrimSpace(embeddings.Model) != "" && embeddings.Dims > 0
}
