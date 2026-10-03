package config

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveReviewInWorktree(t *testing.T) {
	for _, tc := range []struct {
		name   string
		global *Config
		repo   string
		want   bool
	}{
		{name: "nil defaults"},
		{name: "defaults", global: DefaultConfig()},
		{name: "global true", global: &Config{ReviewInWorktree: new(true)}, want: true},
		{name: "repo true", repo: "review_in_worktree = true", want: true},
		{name: "repo false wins", global: &Config{ReviewInWorktree: new(true)}, repo: "review_in_worktree = false"},
		{name: "unrelated config inherits", global: &Config{ReviewInWorktree: new(true)}, repo: "review_agent = 'test'", want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.repo != "" {
				require.NoError(t, os.WriteFile(filepath.Join(dir, ".roborev.toml"), []byte(tc.repo), 0o600))
			}
			got, err := ResolveReviewInWorktree(dir, tc.global)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
	t.Run("invalid config", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, ".roborev.toml"), []byte("review_in_worktree = ["), 0o600))
		_, err := ResolveReviewInWorktree(dir, &Config{ReviewInWorktree: new(true)})
		require.Error(t, err)
	})
}

func TestConfigReviewInWorktreeRoundTrip(t *testing.T) {
	assert := assert.New(t)
	assert.True(IsGlobalKey("review_in_worktree"))
	assert.True(IsValidKey("review_in_worktree"))
	for _, value := range []bool{true, false} {
		var buf bytes.Buffer
		require.NoError(t, toml.NewEncoder(&buf).Encode(&Config{ReviewInWorktree: &value}))
		var global Config
		_, err := toml.Decode(buf.String(), &global)
		require.NoError(t, err)
		require.NotNil(t, global.ReviewInWorktree)
		assert.Equal(value, *global.ReviewInWorktree)
		buf.Reset()
		require.NoError(t, toml.NewEncoder(&buf).Encode(&RepoConfig{ReviewInWorktree: &value}))
		var repo RepoConfig
		_, err = toml.Decode(buf.String(), &repo)
		require.NoError(t, err)
		require.NotNil(t, repo.ReviewInWorktree)
		assert.Equal(value, *repo.ReviewInWorktree)
	}
}
