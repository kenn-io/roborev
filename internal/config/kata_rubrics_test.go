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
