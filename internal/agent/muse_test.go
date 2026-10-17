package agent

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMuseBuildArgs(t *testing.T) {
	for _, tc := range []struct {
		name      string
		agentic   bool
		unsafe    bool
		reasoning ReasoningLevel
		want      []string
	}{
		{"review", false, false, ReasoningStandard, []string{"--no-foreign-personal-context", "-c", `run.toolset=["read_file","search"]`, "-c", `hooks={}`, "-c", "managed_hooks_path=null"}},
		{"agentic", true, false, ReasoningStandard, []string{"--yolo"}},
		{"unsafe", false, true, ReasoningStandard, []string{"--yolo"}},
		{"reasoning thorough", false, false, ReasoningThorough, []string{"--reasoning-effort", "high", "--no-foreign-personal-context", "-c", `run.toolset=["read_file","search"]`, "-c", `hooks={}`, "-c", "managed_hooks_path=null"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withUnsafeAgents(t, tc.unsafe)
			a := NewMuseAgent("").WithReasoning(tc.reasoning).WithAgentic(tc.agentic).(*MuseAgent)
			want := append([]string{"exec", "--no-session-log", "--contributor-mode", "off"}, tc.want...)
			want = append(want, "--prompt-file", "prompt.md")
			assert.Equal(t, want, a.buildArgs(effectiveAgentic(t.Context(), a.Agentic), "prompt.md"))
		})
	}
	a := NewMuseAgent("")
	assert.NotContains(t, a.buildArgs(false, "prompt.md"), "--trust-workspace")
	assert.Contains(t, a.CommandLine(), "--prompt-file <prompt-file>")
	assert.Contains(t, a.WithAgentic(true).(*MuseAgent).PlanningCommandLine(), "--no-foreign-personal-context")
}

func TestMuseReviewPromptAndOutput(t *testing.T) {
	skipIfWindows(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	capturePath := filepath.Join(t.TempDir(), "prompt-path")
	t.Setenv("MUSE_PROMPT_CAPTURE", capturePath)
	command := writeTempCommand(t, `#!/bin/sh
case "$1" in *etxtbsy*) exit 0;; esac
prev=""
for arg in "$@"; do
 if [ "$prev" = "--prompt-file" ]; then
  test -f "$arg" || exit 2
  test "$(cat "$arg")" = 'prompt body' || exit 3
  printf '%s' "$arg" > "$MUSE_PROMPT_CAPTURE"
 fi
 prev="$arg"
done
printf 'No issues found.\n'
printf 'status message\n' >&2
`)
	var output bytes.Buffer
	result, err := NewMuseAgent(command).Review(t.Context(), t.TempDir(), "HEAD", "prompt body", &output)
	require.NoError(t, err)
	assert := assert.New(t)
	assert.Equal("No issues found.", result)
	assert.Contains(output.String(), "No issues found.\n")
	assert.Contains(output.String(), "status message\n")
	assert.NotContains(result, "status message")
	path, err := os.ReadFile(capturePath)
	require.NoError(t, err)
	_, err = os.Stat(string(path))
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestMuseReviewIsolatesConfigAndData(t *testing.T) {
	skipIfWindows(t)
	withUnsafeAgents(t, false)
	configHome := t.TempDir()
	museConfig := filepath.Join(configHome, "muse")
	require.NoError(t, os.MkdirAll(museConfig, 0o700))
	for _, name := range []string{"auth.json", "settings.json", "trust.json", "plugins.json"} {
		require.NoError(t, os.WriteFile(filepath.Join(museConfig, name), []byte("{}"), 0o600))
	}
	t.Setenv("XDG_CONFIG_HOME", configHome)
	dataHome := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dataHome, "muse", "plugins"), 0o700))
	t.Setenv("XDG_DATA_HOME", dataHome)
	captureDir := t.TempDir()
	t.Setenv("MUSE_CONFIG_CAPTURE", captureDir)
	command := writeTempCommand(t, `#!/bin/sh
case "$1" in *etxtbsy*) exit 0;; esac
set -eu
test "$HOME" = "$USERPROFILE"
test "$XDG_CONFIG_HOME" = "$HOME/.config"
test "$XDG_DATA_HOME" = "$HOME/data"
test "$XDG_CACHE_HOME" = "$HOME/cache"
test -d "$XDG_CACHE_HOME"
printf '%s' "$HOME" > "$MUSE_CONFIG_CAPTURE/home"
printf '%s' "$XDG_CONFIG_HOME" > "$MUSE_CONFIG_CAPTURE/config-home"
printf '%s' "$XDG_DATA_HOME" > "$MUSE_CONFIG_CAPTURE/data-home"
ls -1A "$XDG_CONFIG_HOME/muse" > "$MUSE_CONFIG_CAPTURE/files"
cat "$XDG_CONFIG_HOME/muse/auth.json" > "$MUSE_CONFIG_CAPTURE/auth"
cat "$XDG_CONFIG_HOME/muse/settings.json" > "$MUSE_CONFIG_CAPTURE/settings"
ls -1A "$XDG_DATA_HOME" > "$MUSE_CONFIG_CAPTURE/data-files"
printf 'No issues found.\n'
`)
	_, err := NewMuseAgent(command).Review(t.Context(), t.TempDir(), "HEAD", "review", nil)
	require.NoError(t, err)
	home, err := os.ReadFile(filepath.Join(captureDir, "config-home"))
	require.NoError(t, err)
	require.NotEmpty(t, home)
	files, err := os.ReadFile(filepath.Join(captureDir, "files"))
	require.NoError(t, err)
	assert := assert.New(t)
	assert.NotEqual(configHome, string(home), "review must use a private config directory")
	names := strings.Fields(string(files))
	assert.Contains(names, "auth.json")
	assert.Contains(names, "settings.json")
	assert.NotContains(names, "trust.json", "review must not inherit saved workspace trust")
	assert.NotContains(names, "plugins.json", "review must not inherit user plugin configuration")
	for _, name := range []string{"auth", "settings"} {
		data, err := os.ReadFile(filepath.Join(captureDir, name))
		require.NoError(t, err)
		assert.Equal("{}", string(data))
	}
	dataPath, err := os.ReadFile(filepath.Join(captureDir, "data-home"))
	require.NoError(t, err)
	require.NotEmpty(t, dataPath)
	assert.NotEqual(dataHome, string(dataPath), "review must use a private data directory")
	dataFiles, err := os.ReadFile(filepath.Join(captureDir, "data-files"))
	require.NoError(t, err)
	assert.Empty(string(dataFiles), "installed plugins must not be copied into the private data directory")
	privateHome, err := os.ReadFile(filepath.Join(captureDir, "home"))
	require.NoError(t, err)
	require.NotEmpty(t, privateHome)
	assert.NoDirExists(string(privateHome), "per-run home must be removed after review")
	assert.NoDirExists(string(home), "per-run config directory must be removed after review")
	assert.NoDirExists(string(dataPath), "per-run data directory must be removed after review")
}

func TestPrepareMuseRunHomeFallback(t *testing.T) {
	skipIfWindows(t)
	home := t.TempDir()
	configDir := filepath.Join(home, ".config", "muse")
	require.NoError(t, os.MkdirAll(configDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(configDir, "auth.json"), []byte(`{"token":"example"}`), 0o600))
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")

	promptPath, env, cleanup, err := prepareMuseRun("review", false)
	require.NoError(t, err)
	t.Cleanup(cleanup)
	var configHome string
	for _, entry := range env {
		if value, ok := strings.CutPrefix(entry, "XDG_CONFIG_HOME="); ok {
			configHome = value
		}
	}
	require.NotEmpty(t, configHome)
	data, err := os.ReadFile(filepath.Join(configHome, "muse", "auth.json"))
	require.NoError(t, err)
	assert.Equal(t, `{"token":"example"}`, string(data))
	assert.FileExists(t, promptPath)
}

func TestPrepareMuseRunHomeError(t *testing.T) {
	skipIfWindows(t)
	t.Setenv("HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)

	promptPath, env, cleanup, err := prepareMuseRun("review", false)
	require.ErrorContains(t, err, "resolve muse config home:")
	assert.Empty(t, promptPath)
	assert.Nil(t, env)
	assert.Nil(t, cleanup)
	files, err := os.ReadDir(tmp)
	require.NoError(t, err)
	assert.Empty(t, files, "failed setup must remove its prompt file")
}

func TestPrepareMuseRunAgentic(t *testing.T) {
	// Agentic runs inherit the caller's config and data directories, even when
	// the config cannot be copied by non-agentic setup.
	configHome := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(configHome, "muse"), []byte("not a directory"), 0o600))
	t.Setenv("XDG_CONFIG_HOME", configHome)
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	promptPath, env, cleanup, err := prepareMuseRun("fix", true)
	require.NoError(t, err)
	t.Cleanup(cleanup)
	assert.Nil(t, env, "a nil subprocess environment inherits the caller's environment")
	prompt, err := os.ReadFile(promptPath)
	require.NoError(t, err)
	assert.Equal(t, "fix", string(prompt))
	cleanup()
	assert.NoFileExists(t, promptPath)
}

func TestMuseReviewFailure(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cli := mockAgentCLI(t, MockCLIOpts{
		StderrLines: []string{"run ended with Failed: unknown model"},
		ExitCode:    1,
	})
	_, err := NewMuseAgent(cli.CmdPath).Review(t.Context(), t.TempDir(), "HEAD", "review", nil)
	require.ErrorContains(t, err, "muse failed:")
}

func TestMuseReviewEmptyOutput(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cli := mockAgentCLI(t, MockCLIOpts{StdoutLines: []string{"  "}})
	result, err := NewMuseAgent(cli.CmdPath).Review(t.Context(), t.TempDir(), "HEAD", "review", nil)
	require.NoError(t, err)
	assert.Equal(t, "No review output generated", result)
}
