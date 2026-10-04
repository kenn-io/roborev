package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveIsolateReviews(t *testing.T) {
	for _, tc := range []struct {
		name   string
		global *Config
		repo   string
		want   bool
	}{
		{name: "nil defaults"},
		{name: "defaults", global: DefaultConfig()},
		{name: "global true", global: &Config{IsolateReviews: true}, want: true},
		{name: "repo true", repo: "isolate_reviews = true", want: true},
		{name: "repo false wins", global: &Config{IsolateReviews: true}, repo: "isolate_reviews = false"},
		{name: "unrelated config inherits", global: &Config{IsolateReviews: true}, repo: "review_agent = 'test'", want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.repo != "" {
				require.NoError(t, os.WriteFile(filepath.Join(dir, ".roborev.toml"), []byte(tc.repo), 0o600))
			}
			got := ResolveIsolateReviews(dir, tc.global)
			assert.Equal(t, tc.want, got)
		})
	}
}
