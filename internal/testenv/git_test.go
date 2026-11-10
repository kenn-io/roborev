package testenv

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConfigureGitForTestsRestoresEnvironment(t *testing.T) {
	originalHome := t.TempDir()
	t.Setenv("HOME", originalHome)
	t.Setenv("USERPROFILE", originalHome)
	t.Setenv("GIT_CONFIG_GLOBAL", "original")
	t.Setenv("GIT_TERMINAL_PROMPT", "original")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "")
	require.NoError(t, os.Unsetenv("GIT_CONFIG_NOSYSTEM"))
	t.Setenv("XDG_CONFIG_HOME", "")
	require.NoError(t, os.Unsetenv("XDG_CONFIG_HOME"))

	dir := t.TempDir()
	restore, err := configureGitForTests(dir)
	require.NoError(t, err)
	assert.Equal(t, dir, os.Getenv("HOME"))
	assert.Equal(t, dir, os.Getenv("USERPROFILE"))
	assert.Equal(t, filepath.Join(dir, ".gitconfig"), os.Getenv("GIT_CONFIG_GLOBAL"))
	contents, err := os.ReadFile(filepath.Join(dir, ".gitconfig"))
	require.NoError(t, err)
	assert.Empty(t, contents)
	restore()

	assert.Equal(t, originalHome, os.Getenv("HOME"))
	assert.Equal(t, originalHome, os.Getenv("USERPROFILE"))
	assert.Equal(t, "original", os.Getenv("GIT_CONFIG_GLOBAL"))
	assert.Equal(t, "original", os.Getenv("GIT_TERMINAL_PROMPT"))
	_, exists := os.LookupEnv("GIT_CONFIG_NOSYSTEM")
	assert.False(t, exists)
	_, exists = os.LookupEnv("XDG_CONFIG_HOME")
	assert.False(t, exists)
}

func TestConfigureGitForTestsPreservesGoPaths(t *testing.T) {
	home := t.TempDir()
	cache := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CACHE_HOME", cache)
	for _, key := range []string{"GOPATH", "GOMODCACHE", "GOCACHE"} {
		t.Setenv(key, "")
	}
	goenv := filepath.Join(t.TempDir(), "env")
	moduleCache := filepath.Join(t.TempDir(), "modules")
	require.NoError(t, os.WriteFile(goenv, []byte("GOMODCACHE="+moduleCache+"\n"), 0o600))
	t.Setenv("GOENV", goenv)

	restore, err := configureGitForTests(t.TempDir())
	require.NoError(t, err)
	defer restore()
	assert.Equal(t, filepath.Join(home, "go"), os.Getenv("GOPATH"))
	assert.Empty(t, os.Getenv("GOMODCACHE"))
	assert.Equal(t, goenv, os.Getenv("GOENV"))
	if os.PathSeparator != '\\' {
		assert.Equal(t, filepath.Join(cache, "go-build"), os.Getenv("GOCACHE"))
	}
}
