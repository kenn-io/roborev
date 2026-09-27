package config

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
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
	APIKeyFile          string `toml:"api_key_file"`
	APIKeyEnv           string `toml:"api_key_env"`
	InputTypeMode       string `toml:"input_type_mode"`
	FingerprintSalt     string `toml:"fingerprint_salt"`
	BatchSize           int    `toml:"batch_size"`
	TimeoutSeconds      int    `toml:"timeout_seconds"`
	TrustPrivateNetwork bool   `toml:"trust_private_network"`
}

// EmbeddingCredential describes a startup credential without exposing its value
// through health. Key is only passed to the provider client.
type EmbeddingCredential struct {
	Key    string
	Source string
	Reason string
}

// ResolveAPIKey resolves credentials at daemon startup. An unavailable source
// returns an empty key; only conflicting configuration returns an error.
func (c EmbeddingConfig) ResolveAPIKey() (string, error) {
	credential, err := c.ResolveCredential()
	return credential.Key, err
}

// ResolveCredential separates normal credential unavailability from invalid
// configuration. Inspection of config never invokes this method.
func (c EmbeddingConfig) ResolveCredential() (EmbeddingCredential, error) {
	if err := c.validateKeySources(); err != nil {
		return EmbeddingCredential{}, err
	}
	missing := func(source, detail string) EmbeddingCredential {
		reason := "no embedding API key"
		if detail != "" {
			reason += " (" + detail + ")"
		}
		return EmbeddingCredential{Source: source, Reason: reason}
	}
	if c.APIKey != "" {
		if strings.TrimSpace(c.APIKey) == "" {
			return missing("inline", "inline key is empty"), nil
		}
		return EmbeddingCredential{Key: c.APIKey, Source: "inline"}, nil
	}
	if raw := strings.TrimSpace(c.APIKeyFile); raw != "" {
		source := "file:" + raw
		path := raw
		if raw == "~" || strings.HasPrefix(raw, "~/") {
			home, err := os.UserHomeDir()
			if err != nil {
				return missing(source, "key file home directory is unavailable"), nil
			}
			path = home
			if raw != "~" {
				path = filepath.Join(home, strings.TrimPrefix(raw, "~/"))
			}
		}
		info, err := os.Stat(path)
		if err != nil {
			return missing(source, "key file is missing or unreadable"), nil
		}
		if !info.Mode().IsRegular() {
			return missing(source, "key file is not a readable regular file"), nil
		}
		file, err := os.Open(path)
		if err != nil {
			return missing(source, "key file is missing or unreadable"), nil
		}
		defer file.Close()
		info, err = file.Stat()
		if err != nil || !info.Mode().IsRegular() {
			return missing(source, "key file is not a readable regular file"), nil
		}
		if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
			return missing(source, "key file permissions must restrict access to its owner (0600)"), nil
		}
		data, err := io.ReadAll(file)
		if err != nil {
			return missing(source, "key file is unreadable"), nil
		}
		key := strings.TrimRight(string(data), "\r\n")
		if strings.TrimSpace(key) == "" {
			return missing(source, "key file is empty"), nil
		}
		return EmbeddingCredential{Key: key, Source: source}, nil
	}
	if name := strings.TrimSpace(c.APIKeyEnv); name != "" {
		value := os.Getenv(name)
		if strings.TrimSpace(value) == "" {
			return missing("env:"+name, "env "+name+" is unset or empty"), nil
		}
		return EmbeddingCredential{Key: value, Source: "env:" + name}, nil
	}
	return missing("", "no key source is configured"), nil
}

func (c EmbeddingConfig) validateKeySources() error {
	sources := make([]string, 0, 3)
	if c.APIKey != "" {
		sources = append(sources, "api_key")
	}
	if strings.TrimSpace(c.APIKeyFile) != "" {
		sources = append(sources, "api_key_file")
	}
	if strings.TrimSpace(c.APIKeyEnv) != "" {
		sources = append(sources, "api_key_env")
	}
	if len(sources) > 1 {
		return fmt.Errorf("search.embeddings %s are mutually exclusive", strings.Join(sources, " and "))
	}
	return nil
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

	if err := embeddings.validateKeySources(); err != nil {
		return err
	}

	baseSet := strings.TrimSpace(embeddings.BaseURL) != ""
	modelSet := strings.TrimSpace(embeddings.Model) != ""
	dimsSet := embeddings.Dims > 0
	anySet := baseSet || modelSet || embeddings.Dims != 0 || embeddings.APIKey != "" ||
		strings.TrimSpace(embeddings.APIKeyEnv) != "" || strings.TrimSpace(embeddings.APIKeyFile) != "" || embeddings.InputTypeMode != "" ||
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
