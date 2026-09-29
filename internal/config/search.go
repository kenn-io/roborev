package config

import (
	"fmt"

	"go.kenn.io/kit/embedconfig"
)

// SearchConfig controls semantic review-history search.
type SearchConfig struct {
	// Embeddings uses kit's standard embedder keys, shared by every Kenn
	// application that reads embedding settings.
	Embeddings *embedconfig.Embedder `toml:"embeddings"`
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
