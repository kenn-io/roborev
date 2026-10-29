package config

import (
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCICommentMinConfidence(t *testing.T) {
	value := func(v float64) *float64 { return &v }
	for _, tc := range []struct {
		name         string
		global, repo *float64
		want         *float64
	}{
		{"default", nil, nil, nil}, {"global", value(0.7), nil, value(0.7)},
		{"repo", value(0.7), value(0.9), value(0.9)}, {"repo zero reset", value(0.7), value(0), value(0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.CI.CommentMinConfidence = tc.global
			repo := &RepoConfig{CI: RepoCIConfig{CommentMinConfidence: tc.repo}}
			got, err := ResolveCICommentMinConfidence(repo, cfg)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
	got, err := ResolveCICommentMinConfidence(nil, nil)
	require.NoError(t, err)
	assert.Nil(t, got)
	for _, v := range []float64{-0.1, 1.1, math.NaN(), math.Inf(1), math.Inf(-1)} {
		t.Run("invalid", func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.CI.CommentMinConfidence = &v
			repo := &RepoConfig{CI: RepoCIConfig{CommentMinConfidence: &v}}
			_, err := ResolveCICommentMinConfidence(nil, cfg)
			require.ErrorContains(t, err, "ci.comment_min_confidence")
			_, err = ResolveCICommentMinConfidence(repo, nil)
			require.ErrorContains(t, err, "ci.comment_min_confidence")
			require.ErrorContains(t, cfg.Validate(), "ci.comment_min_confidence")
			require.ErrorContains(t, repo.Validate(), "ci.comment_min_confidence")
		})
	}
	for _, text := range []string{"comment_min_confidence = 0", "comment_min_confidence = 0.7"} {
		var cfg Config
		var repo RepoConfig
		_, err := toml.Decode("[ci]\n"+text, &cfg)
		require.NoError(t, err)
		_, err = toml.Decode("[ci]\n"+text, &repo)
		require.NoError(t, err)
		require.NotNil(t, cfg.CI.CommentMinConfidence)
		assert.Equal(t, cfg.CI.CommentMinConfidence, repo.CI.CommentMinConfidence)
	}
}

func TestCICommentMinConfidenceKeyValue(t *testing.T) {
	for _, cfg := range []any{&Config{}, &RepoConfig{}} {
		require.NoError(t, SetConfigValue(cfg, "ci.comment_min_confidence", "0.7"))
		got, err := GetConfigValue(cfg, "ci.comment_min_confidence")
		require.NoError(t, err)
		assert.Equal(t, "0.7", got)
		require.NoError(t, SetConfigValue(cfg, "ci.comment_min_confidence", "0"))
		got, err = GetConfigValue(cfg, "ci.comment_min_confidence")
		require.NoError(t, err)
		assert.Equal(t, "0", got)
		require.Error(t, SetConfigValue(cfg, "ci.comment_min_confidence", "not-a-number"))
	}
	assert.True(t, IsGlobalKey("ci.comment_min_confidence"))
	assert.True(t, IsValidKey("ci.comment_min_confidence"))
}

func TestCICommentMinConfidenceLoadSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	for _, text := range []string{"-0.1", "1.1", "nan", "+inf", "-inf"} {
		require.NoError(t, os.WriteFile(path, []byte("[ci]\ncomment_min_confidence = "+text+"\n"), 0o600))
		_, err := LoadGlobalFrom(path)
		require.ErrorContains(t, err, "ci.comment_min_confidence")
		cfg, err := LoadGlobalClientFrom(path)
		require.NoError(t, err)
		require.NotEmpty(t, cfg.DaemonConfigWarnings())
	}
	for _, v := range []float64{-0.1, 1.1, math.NaN(), math.Inf(1), math.Inf(-1)} {
		cfg := DefaultConfig()
		cfg.CI.CommentMinConfidence = &v
		require.NoError(t, os.WriteFile(path, []byte("# previous config\n"), 0o600))
		require.ErrorContains(t, SaveGlobalTo(path, cfg), "ci.comment_min_confidence")
		contents, err := os.ReadFile(path)
		require.NoError(t, err)
		assert.Equal(t, "# previous config\n", string(contents))
	}
	cfg := DefaultConfig()
	floor := 0.7
	cfg.CI.CommentMinConfidence = &floor
	require.NoError(t, SaveGlobalTo(path, cfg))
	loaded, err := LoadGlobalFrom(path)
	require.NoError(t, err)
	assert.Equal(t, &floor, loaded.CI.CommentMinConfidence)
}
