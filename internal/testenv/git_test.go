package testenv

import (
	"os"
	"os/exec"
	"path/filepath"
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
	for _, key := range []string{"MISE_DATA_DIR", "MISE_CACHE_DIR", "MISE_STATE_DIR"} {
		t.Setenv(key, "")
		require.NoError(t, os.Unsetenv(key))
	}

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

func TestConfigureGitForTestsStandalone(t *testing.T) {
	if os.Getenv("ROBOREV_TEST_STANDALONE") == "1" {
		restore, err := configureGitForTests(t.TempDir())
		require.NoError(t, err)
		restore()
		return
	}
	exe, err := os.Executable()
	require.NoError(t, err)
	t.Setenv("PATH", "")
	t.Setenv("ROBOREV_TEST_STANDALONE", "1")
	cmd := exec.CommandContext(t.Context(), exe, "-test.run=^TestConfigureGitForTestsStandalone$")
	cmd.Dir = t.TempDir()
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", out)
}

func TestConfigureGitForTestsMissingHome(t *testing.T) {
	if os.Getenv("ROBOREV_TEST_MISSING_HOME") == "1" {
		restore, err := configureGitForTests(t.TempDir())
		require.NoError(t, err)
		restore()
		return
	}
	exe, err := os.Executable()
	require.NoError(t, err)
	t.Setenv("ROBOREV_TEST_MISSING_HOME", "1")
	for _, key := range []string{"HOME", "USERPROFILE", "XDG_CONFIG_HOME", "XDG_CACHE_HOME"} {
		t.Setenv(key, "")
		require.NoError(t, os.Unsetenv(key))
	}
	cmd := exec.CommandContext(t.Context(), exe, "-test.run=^TestConfigureGitForTestsMissingHome$")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", out)
}

func TestConfigureGitForTestsGoEnv(t *testing.T) {
	goBin, err := exec.LookPath("go")
	require.NoError(t, err)
	for _, mode := range []string{"default", "long line", "off", "toolchain defaults"} {
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
			if mode == "off" {
				goenv = "off"
			}
			if mode == "toolchain defaults" {
				root := t.TempDir()
				config := "GOPATH=" + filepath.Join(t.TempDir(), "go") + "\nGOCACHE=" + filepath.Join(t.TempDir(), "cache") + "\n"
				require.NoError(t, os.WriteFile(filepath.Join(root, "go.env"), []byte(config), 0o600))
				t.Setenv("GOROOT", root)
				t.Setenv("GOTOOLCHAIN", "local")
				goenv = "off"
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

func TestConfigureGitForTestsMiseShim(t *testing.T) {
	shim := os.Getenv("ROBOREV_TEST_MISE_GO_SHIM")
	if shim == "" {
		t.Skip("no mise Go shim specified")
	}
	t.Setenv("MISE_OFFLINE", "1")
	t.Setenv("MISE_AUTO_INSTALL", "0")
	read := func(args ...string) string {
		cmd := exec.CommandContext(t.Context(), shim, args...)
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s", out)
		return string(out)
	}
	version, root := read("version"), read("env", "GOROOT")
	dir := t.TempDir()
	restore, err := configureGitForTests(dir)
	require.NoError(t, err)
	defer restore()
	assert.Equal(t, version, read("version"))
	assert.Equal(t, root, read("env", "GOROOT"))
	_, err = os.Stat(filepath.Join(dir, ".local", "share", "mise", "installs"))
	assert.ErrorIs(t, err, os.ErrNotExist)
}
