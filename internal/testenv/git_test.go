package testenv

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
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
	t.Setenv("MISE_CONFIG_DIR", "original-mise")
	for _, entry := range [][2]string{{"MISE_DATA_DIR", "XDG_DATA_HOME"}, {"MISE_CACHE_DIR", "XDG_CACHE_HOME"}, {"MISE_STATE_DIR", "XDG_STATE_HOME"}} {
		t.Setenv(entry[0], "")
		require.NoError(t, os.Unsetenv(entry[0]))
		t.Setenv(entry[1], t.TempDir())
	}

	dir := t.TempDir()
	restore, err := configureGitForTests(dir)
	require.NoError(t, err)
	assert.Equal(t, dir, os.Getenv("HOME"))
	assert.Equal(t, dir, os.Getenv("USERPROFILE"))
	assert.Equal(t, filepath.Join(dir, ".gitconfig"), os.Getenv("GIT_CONFIG_GLOBAL"))
	assert.Equal(t, "original-mise", os.Getenv("MISE_CONFIG_DIR"))
	for _, entry := range [][2]string{{"MISE_DATA_DIR", "XDG_DATA_HOME"}, {"MISE_CACHE_DIR", "XDG_CACHE_HOME"}, {"MISE_STATE_DIR", "XDG_STATE_HOME"}} {
		assert.Equal(t, filepath.Join(os.Getenv(entry[1]), "mise"), os.Getenv(entry[0]))
	}
	contents, err := os.ReadFile(filepath.Join(dir, ".gitconfig"))
	require.NoError(t, err)
	assert.Empty(t, contents)
	restore()

	assert.Equal(t, originalHome, os.Getenv("HOME"))
	assert.Equal(t, originalHome, os.Getenv("USERPROFILE"))
	assert.Equal(t, "original", os.Getenv("GIT_CONFIG_GLOBAL"))
	assert.Equal(t, "original", os.Getenv("GIT_TERMINAL_PROMPT"))
	assert.Equal(t, "original-mise", os.Getenv("MISE_CONFIG_DIR"))
	for _, key := range []string{"MISE_DATA_DIR", "MISE_CACHE_DIR", "MISE_STATE_DIR"} {
		_, exists := os.LookupEnv(key)
		assert.False(t, exists, key)
	}
	_, exists := os.LookupEnv("GIT_CONFIG_NOSYSTEM")
	assert.False(t, exists)
	_, exists = os.LookupEnv("XDG_CONFIG_HOME")
	assert.False(t, exists)
}

func TestConfigureGitForTestsDoesNotRunGo(t *testing.T) {
	exe, err := os.Executable()
	require.NoError(t, err)
	binary, err := os.ReadFile(exe)
	require.NoError(t, err)
	dir := t.TempDir()
	name := "go"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), binary, 0o755))
	t.Setenv("PATH", dir)
	marker := filepath.Join(t.TempDir(), "started")
	t.Setenv("ROBOREV_TEST_STALLED_GO", marker)
	t.Setenv("GOPATH", "")
	t.Setenv("GOCACHE", "")
	restore, err := configureGitForTests(t.TempDir())
	require.NoError(t, err)
	defer restore()
	_, err = os.Stat(marker)
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestConfigureGitForTestsMissingHome(t *testing.T) {
	keys := []string{"HOME", "USERPROFILE", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "GOPATH", "GOCACHE", "MISE_CONFIG_DIR", "MISE_DATA_DIR", "MISE_CACHE_DIR", "MISE_STATE_DIR"}
	for _, key := range keys {
		t.Setenv(key, "")
		require.NoError(t, os.Unsetenv(key))
	}
	restore, err := configureGitForTests(t.TempDir())
	require.NoError(t, err)
	defer restore()
	for _, key := range keys[6:] {
		value := os.Getenv(key)
		if value != "" {
			assert.True(t, filepath.IsAbs(value), key)
		}
	}
}

func TestConfigureGitForTestsGoEnv(t *testing.T) {
	goBin, err := exec.LookPath("go")
	require.NoError(t, err)
	for _, mode := range []string{"default", "long line", "toolchain defaults", "empty user value"} {
		t.Run(mode, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			t.Setenv("XDG_CACHE_HOME", t.TempDir())
			for _, key := range []string{"GOPATH", "GOMODCACHE", "GOCACHE"} {
				t.Setenv(key, "")
			}
			goenv := filepath.Join(t.TempDir(), "goenv")
			if mode == "long line" {
				config := "IGNORED=" + strings.Repeat("x", 70*1024) + "\nGOMODCACHE=" + filepath.Join(t.TempDir(), "modules") + "\nGOCACHE=" + filepath.Join(t.TempDir(), "cache") + "\n"
				require.NoError(t, os.WriteFile(goenv, []byte(config), 0o600))
			}
			if mode == "toolchain defaults" || mode == "empty user value" {
				root := t.TempDir()
				config := "GOPATH=" + filepath.Join(t.TempDir(), "go") + "\nGOCACHE=" + filepath.Join(t.TempDir(), "cache") + "\n"
				require.NoError(t, os.WriteFile(filepath.Join(root, "go.env"), []byte(config), 0o600))
				t.Setenv("GOROOT", root)
				t.Setenv("GOTOOLCHAIN", "local")
				if mode == "toolchain defaults" {
					goenv = "off"
				} else {
					require.NoError(t, os.WriteFile(goenv, []byte("GOPATH=\nGOCACHE=\n"), 0o600))
				}
			}
			t.Setenv("GOENV", goenv)
			if mode == "default" {
				require.NoError(t, os.Unsetenv("GOENV"))
			}
			read := func() string {
				cmd := exec.CommandContext(t.Context(), goBin, "env", "GOPATH", "GOMODCACHE", "GOCACHE", "GOENV")
				out, err := cmd.CombinedOutput()
				require.NoError(t, err, "%s", out)
				return string(out)
			}
			before := read()
			restore, err := configureGitForTests(t.TempDir())
			require.NoError(t, err)
			assert.Equal(t, before, read())
			restore()
			assert.Equal(t, before, read())
			value, exists := os.LookupEnv("GOENV")
			if mode == "default" {
				assert.False(t, exists)
			} else {
				assert.Equal(t, goenv, value)
			}
		})
	}
}
