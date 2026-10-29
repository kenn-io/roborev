package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCICommentPolicy(t *testing.T) {
	for _, tc := range []struct {
		name, global, repo string
		cap                int
		clean              bool
	}{
		{"defaults", "", "", 0, true},
		{"inherit", "comment_max_findings = 2\ncomment_when_clean = false", "", 2, false},
		{"reset", "comment_max_findings = 2\ncomment_when_clean = false", "comment_max_findings = 0\ncomment_when_clean = true", 0, true},
		{"override", "comment_max_findings = 4", "comment_max_findings = 1\ncomment_when_clean = false", 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var global Config
			var repo RepoConfig
			_, err := toml.Decode("[ci]\n"+tc.global, &global)
			require.NoError(t, err)
			_, err = toml.Decode("[ci]\n"+tc.repo, &repo)
			require.NoError(t, err)
			assert.Equal(t, tc.cap, ResolveCICommentMaxFindings(&repo, &global))
			assert.Equal(t, tc.clean, ResolveCICommentWhenClean(&repo, &global))
			require.NoError(t, global.Validate())
			require.NoError(t, repo.Validate())
		})
	}
	assert.Equal(t, 0, ResolveCICommentMaxFindings(nil, nil))
	assert.True(t, ResolveCICommentWhenClean(nil, nil))
	var global Config
	var repo RepoConfig
	require.NoError(t, SetConfigValue(&global, "ci.comment_max_findings", "-1"))
	require.ErrorContains(t, global.Validate(), "ci.comment_max_findings")
	require.NoError(t, SetConfigValue(&repo, "ci.comment_max_findings", "-1"))
	require.ErrorContains(t, repo.Validate(), "ci.comment_max_findings")
}

// Invalid daemon publication policy must be rejected at the actual global I/O
// boundaries, while workflow-only clients retain the existing warning behavior.
func TestCICommentPolicyGlobalBoundaries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(path, []byte("[ci]\ncomment_max_findings = -1\n"), 0o600))
	_, err := LoadGlobalFrom(path)
	require.ErrorContains(t, err, "ci.comment_max_findings")
	client, err := LoadGlobalClientFrom(path)
	require.NoError(t, err)
	require.ErrorContains(t, errors.Join(client.DaemonConfigWarnings()...), "ci.comment_max_findings")

	cfg := DefaultConfig()
	cfg.CI.CommentMaxFindings = 2
	require.NoError(t, SaveGlobalTo(path, cfg))
	original, err := os.ReadFile(path)
	require.NoError(t, err)
	cfg.CI.CommentMaxFindings = -1
	require.ErrorContains(t, SaveGlobalTo(path, cfg), "ci.comment_max_findings")
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, string(original), string(after), "invalid save must not replace the active positive cap")
	for _, cap := range []int{0, 2} {
		cfg.CI.CommentMaxFindings = cap
		require.NoError(t, SaveGlobalTo(path, cfg))
		loaded, err := LoadGlobalFrom(path)
		require.NoError(t, err)
		assert.Equal(t, cap, loaded.CI.CommentMaxFindings)
	}
}
