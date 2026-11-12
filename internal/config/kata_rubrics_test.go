package config

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestKataRubricsMode(t *testing.T) {
	for _, mode := range []string{"", "off", "current"} {
		cfg := DefaultConfig()
		cfg.KataRubrics.Mode = mode
		require.NoError(t, cfg.Validate())
	}
	require.ErrorContains(t, (&Config{KataRubrics: KataRubricsConfig{Mode: "open"}}).Validate(), "kata_rubrics.mode")
	require.ErrorContains(t, (&RepoConfig{KataRubrics: KataRubricsConfig{Mode: "current"}}).Validate(), "kata_rubrics.mode")
	require.NoError(t, (&RepoConfig{KataRubrics: KataRubricsConfig{Mode: "off"}}).Validate())
	assert.Equal(t, "off", ResolveKataRubricsMode(nil, nil))
	assert.Equal(t, "current", ResolveKataRubricsMode(nil, &Config{KataRubrics: KataRubricsConfig{Mode: "current"}}))
	assert.Equal(t, "off", ResolveKataRubricsMode(&RepoConfig{KataRubrics: KataRubricsConfig{Mode: "off"}}, &Config{KataRubrics: KataRubricsConfig{Mode: "current"}}))
}

func TestLoadRepoKataRubricsRejectsNonOptOut(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, ".roborev.toml")
	for _, mode := range []string{"", "current", "open"} {
		require.NoError(t, os.WriteFile(file, []byte("[kata_rubrics]\nmode = "+strconv.Quote(mode)+"\n"), 0o600))
		_, err := LoadRepoConfig(dir)
		require.ErrorContains(t, err, "kata_rubrics.mode")
	}
	require.NoError(t, os.WriteFile(file, []byte("[kata_rubrics]\nmode = 'off'\n"), 0o600))
	_, err := LoadRepoConfig(dir)
	require.NoError(t, err)
}

func TestLoadGlobalKataRubricsRejectsInvalidModes(t *testing.T) {
	file := filepath.Join(t.TempDir(), "config.toml")
	for _, mode := range []string{"", "open"} {
		require.NoError(t, os.WriteFile(file, []byte("[kata_rubrics]\nmode = "+strconv.Quote(mode)+"\n"), 0o600))
		_, err := LoadGlobalFrom(file)
		require.ErrorContains(t, err, "kata_rubrics.mode")
	}
}

func TestKataRubricsConfigSetAndRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name   string
		global bool
		mode   string
		valid  bool
	}{
		{"global current", true, "current", true}, {"global off", true, "off", true},
		{"global empty", true, "", false}, {"global invalid", true, "open", false},
		{"local off", false, "off", true}, {"local current", false, "current", false},
		{"local empty", false, "", false}, {"local invalid", false, "open", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var cfg any = &RepoConfig{}
			if tc.global {
				cfg = DefaultConfig()
			}
			err := SetConfigValue(cfg, "kata_rubrics.mode", tc.mode)
			if tc.valid {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, "kata_rubrics.mode")
			}
		})
	}
	dir := t.TempDir()
	path := filepath.Join(dir, ".roborev.toml")
	require.NoError(t, SaveRepoConfigToWithExplicitKeys(path, &RepoConfig{Agent: "test"}, "agent"))
	cfg, err := LoadRepoConfig(dir)
	require.NoError(t, err)
	assert.Equal(t, "", cfg.KataRubrics.Mode)
	assert.Equal(t, "current", ResolveKataRubricsMode(cfg, &Config{KataRubrics: KataRubricsConfig{Mode: "current"}}))
	require.Error(t, SaveRepoConfigToWithExplicitKeys(path, cfg, "kata_rubrics.mode"))
}

func TestLoadRepoKataRubricsFromRef(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, ".roborev.toml", "[kata_rubrics]\nmode = ''\n")
	execGit(t, dir, "init")
	execGit(t, dir, "config", "user.email", "test@example.com")
	execGit(t, dir, "config", "user.name", "Test")
	execGit(t, dir, "add", ".roborev.toml")
	execGit(t, dir, "commit", "-m", "config")
	_, err := LoadRepoConfigFromRef(dir, "HEAD")
	require.ErrorContains(t, err, "kata_rubrics.mode")
}
