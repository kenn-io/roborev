package config

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGoalReviewConfigPersistWatch(t *testing.T) {
	for _, tc := range []struct {
		name  string
		watch []string
		want  []string
	}{
		{"default", nil, []string{"goal", "kata_graph"}},
		{"explicit empty", []string{}, []string{}},
		{"explicit graph", []string{"kata_graph"}, []string{"kata_graph"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, ".roborev.toml")
			cfg := &RepoConfig{GoalReview: GoalReviewConfig{Enabled: true, Watch: tc.watch}}
			require.NoError(t, SaveRepoConfigTo(path, cfg))
			loaded, err := LoadRepoConfig(root)
			require.NoError(t, err)
			assert.Equal(t, tc.want, ResolveGoalReview(loaded).Watch)
		})
	}
}
