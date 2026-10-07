package config

import (
	"fmt"

	"go.kenn.io/kit/embedconfig"
)

// SearchConfig controls semantic review-history search.
type SearchConfig struct {
	Embeddings *SearchEmbeddingsConfig `toml:"embeddings"`
}

// SearchEmbeddingsConfig extends kit's standard keys with literal search role
// prefixes. Empty prefixes retain the existing input and vector identities.
type SearchEmbeddingsConfig struct {
	embedconfig.Embedder
	DocumentPrefix string `toml:"document_prefix"`
	QueryPrefix    string `toml:"query_prefix"`
}

// Validate checks the standard keys and requires an enabled endpoint for prefixes.
func (e SearchEmbeddingsConfig) Validate() error {
	if err := e.Embedder.Validate(); err != nil {
		return err
	}
	if !e.Enabled() && (e.DocumentPrefix != "" || e.QueryPrefix != "") {
		return fmt.Errorf("embed base_url, model, and dims are required with role prefixes")
	}
	return nil
}

// Parts preserves kit's model and operational settings and adds literal prefixes.
func (e SearchEmbeddingsConfig) Parts() (embedconfig.Parts, error) {
	parts, err := e.Embedder.Parts()
	if err != nil {
		return parts, err
	}
	parts.Roles.DocumentPrefix = e.DocumentPrefix
	parts.Roles.QueryPrefix = e.QueryPrefix
	return parts, nil
}

func validateSearchConfig(search SearchConfig) error {
	if search.Embeddings == nil {
		return nil
	}
	if err := search.Embeddings.Validate(); err != nil {
		return fmt.Errorf("search.embeddings: %w", err)
	}
	return nil
}
