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
		{"review", false, false, ReasoningStandard, []string{"-c", `run.toolset=["read_file","search"]`, "-c", `hooks={}`, "-c", "managed_hooks_path=null"}},
		{"agentic", true, false, ReasoningStandard, []string{"--yolo"}},
		{"unsafe", false, true, ReasoningStandard, []string{"--yolo"}},
		{"reasoning thorough", false, false, ReasoningThorough, []string{"--reasoning-effort", "high", "-c", `run.toolset=["read_file","search"]`, "-c", `hooks={}`, "-c", "managed_hooks_path=null"}},
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
}

func TestMuseReviewPromptAndOutput(t *testing.T) {
	skipIfWindows(t)
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

func TestMuseReviewIsolatesSavedWorkspaceTrust(t *testing.T) {
	skipIfWindows(t)
	withUnsafeAgents(t, false)
	configHome := t.TempDir()
	museConfig := filepath.Join(configHome, "muse")
	require.NoError(t, os.MkdirAll(museConfig, 0o700))
	for _, name := range []string{"auth.json", "settings.json", "trust.json"} {
		require.NoError(t, os.WriteFile(filepath.Join(museConfig, name), []byte("{}"), 0o600))
	}
	t.Setenv("XDG_CONFIG_HOME", configHome)
	captureDir := t.TempDir()
	t.Setenv("MUSE_CONFIG_CAPTURE", captureDir)
	command := writeTempCommand(t, `#!/bin/sh
case "$1" in *etxtbsy*) exit 0;; esac
set -eu
printf '%s' "$XDG_CONFIG_HOME" > "$MUSE_CONFIG_CAPTURE/config-home"
ls -1A "$XDG_CONFIG_HOME/muse" > "$MUSE_CONFIG_CAPTURE/files"
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
	assert.NoDirExists(string(home), "per-run config directory must be removed after review")
}

func TestMuseReviewFailure(t *testing.T) {
	cli := mockAgentCLI(t, MockCLIOpts{
		StderrLines: []string{"run ended with Failed: unknown model"},
		ExitCode:    1,
	})
	_, err := NewMuseAgent(cli.CmdPath).Review(t.Context(), t.TempDir(), "HEAD", "review", nil)
	require.ErrorContains(t, err, "muse failed:")
}

func TestMuseReviewEmptyOutput(t *testing.T) {
	cli := mockAgentCLI(t, MockCLIOpts{StdoutLines: []string{"  "}})
	result, err := NewMuseAgent(cli.CmdPath).Review(t.Context(), t.TempDir(), "HEAD", "review", nil)
	require.NoError(t, err)
	assert.Equal(t, "No review output generated", result)
}
