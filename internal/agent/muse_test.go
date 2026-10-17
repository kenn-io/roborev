package agent

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
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
	}{
		{"review", false, false, ReasoningStandard},
		{"agentic", true, false, ReasoningStandard},
		{"unsafe", false, true, ReasoningStandard},
		{"reasoning thorough", false, false, ReasoningThorough},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withUnsafeAgents(t, tc.unsafe)
			a := NewMuseAgent("").WithReasoning(tc.reasoning).WithAgentic(tc.agentic).(*MuseAgent)
			args := a.buildArgs(effectiveAgentic(t.Context(), a.Agentic), "prompt.md")
			assert := assert.New(t)
			assert.Contains(strings.Join(args, " "), "--prompt-file prompt.md")
			if tc.agentic || tc.unsafe {
				assert.Contains(args, "--yolo")
				assert.NotContains(args, `run.toolset=["read_file","search"]`)
			} else {
				assert.NotContains(args, "--yolo")
				assert.Contains(args, "--no-foreign-personal-context")
				for _, override := range []string{`run.toolset=["read_file","search"]`, "hooks={}", "managed_hooks_path=null"} {
					assert.Contains(strings.Join(args, " "), "-c "+override)
				}
			}
			if tc.reasoning == ReasoningThorough {
				assert.Contains(strings.Join(args, " "), "--reasoning-effort high")
			}
		})
	}
	a := NewMuseAgent("")
	assert.Contains(t, a.CommandLine(), "--prompt-file <prompt-file>")
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
	for _, tc := range []struct{ source, key string }{
		{"xdg", "mcpServers"},
		{"xdg", "mcp_servers"},
		{"home", "mcpServers"},
		{"home", "mcp_servers"},
	} {
		t.Run(tc.source+"/"+tc.key, func(t *testing.T) {
			configHome := t.TempDir()
			if tc.source == "home" {
				home := t.TempDir()
				configHome = filepath.Join(home, ".config")
				t.Setenv("HOME", home)
				t.Setenv("XDG_CONFIG_HOME", "")
			} else {
				t.Setenv("XDG_CONFIG_HOME", configHome)
			}
			museConfig := filepath.Join(configHome, "muse")
			require.NoError(t, os.MkdirAll(museConfig, 0o700))
			for name, data := range map[string]string{
				"auth.json":     `{"token":"example"}`,
				"settings.json": fmt.Sprintf(museSettingsWithMCP, tc.key),
				"trust.json":    "{}",
				"plugins.json":  "{}",
			} {
				require.NoError(t, os.WriteFile(filepath.Join(museConfig, name), []byte(data), 0o600))
			}
			dataHome := t.TempDir()
			require.NoError(t, os.MkdirAll(filepath.Join(dataHome, "muse", "plugins"), 0o700))
			t.Setenv("XDG_DATA_HOME", dataHome)
			capture := filepath.Join(t.TempDir(), "settings.json")
			t.Setenv("MUSE_SETTINGS_CAPTURE", capture)
			command := writeTempCommand(t, `#!/bin/sh
case "$1" in *etxtbsy*) exit 0;; esac
set -eu
test "$HOME" = "$USERPROFILE"
test "$XDG_CONFIG_HOME" = "$HOME/.config"
test "$XDG_DATA_HOME" = "$HOME/data"
test "$XDG_CACHE_HOME" = "$HOME/cache"
test -d "$XDG_CACHE_HOME"
case "$HOME" in "$TMPDIR"/*) ;; *) exit 1;; esac
test "$(cat "$XDG_CONFIG_HOME/muse/auth.json")" = '{"token":"example"}'
cp "$XDG_CONFIG_HOME/muse/settings.json" "$MUSE_SETTINGS_CAPTURE"
test ! -e "$XDG_CONFIG_HOME/muse/trust.json"
test ! -e "$XDG_CONFIG_HOME/muse/plugins.json"
test -z "$(ls -1A "$XDG_DATA_HOME")"
printf 'No issues found.\n'
`)
			repoPath := t.TempDir()
			tmp := t.TempDir()
			t.Setenv("TMPDIR", tmp)
			_, err := NewMuseAgent(command).Review(t.Context(), repoPath, "HEAD", "review", nil)
			require.NoError(t, err)
			files, err := os.ReadDir(tmp)
			require.NoError(t, err)
			assert.Empty(t, files, "review must remove its prompt and private home")
			settings, err := os.ReadFile(capture)
			require.NoError(t, err)
			assert.JSONEq(t, `{"schema_version":1,"model":"muse-spark-1.3","hooks":{}}`, string(settings))
		})
	}
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
	repo := t.TempDir()
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	_, err := NewMuseAgent(cli.CmdPath).Review(t.Context(), repo, "HEAD", "review", nil)
	require.ErrorContains(t, err, "muse failed:")
	files, err := os.ReadDir(tmp)
	require.NoError(t, err)
	assert.Empty(t, files, "failed run must remove its prompt and private home")
}

func TestMuseReviewEmptyOutput(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cli := mockAgentCLI(t, MockCLIOpts{StdoutLines: []string{"  "}})
	result, err := NewMuseAgent(cli.CmdPath).Review(t.Context(), t.TempDir(), "HEAD", "review", nil)
	require.NoError(t, err)
	assert.Equal(t, "No review output generated", result)
}

const museSettingsWithMCP = `{
 "schema_version": 1,
 "model": "muse-spark-1.3",
 "hooks": {},
 "%s": {"example": {"type": "stdio", "command": "roborev-missing-mcp-server", "enabled": true, "mode": "required"}}
}`

func TestPrepareMuseRunInvalidSettings(t *testing.T) {
	for _, tc := range []struct {
		name, settings, wantError string
	}{
		{"newer schema", `{"schema_version":2}`, "unsupported muse settings schema_version 2; expected 1"},
		{"missing schema", `{}`, "muse settings require schema_version 1"},
		{"malformed JSON", `{`, "read muse settings:"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config := t.TempDir()
			require.NoError(t, os.Mkdir(filepath.Join(config, "muse"), 0o700))
			require.NoError(t, os.WriteFile(filepath.Join(config, "muse", "settings.json"), []byte(tc.settings), 0o600))
			t.Setenv("XDG_CONFIG_HOME", config)
			tmp := t.TempDir()
			t.Setenv("TMPDIR", tmp)
			prompt, env, cleanup, err := prepareMuseRun("review", false)
			require.ErrorContains(t, err, tc.wantError)
			assert.Empty(t, prompt)
			assert.Nil(t, env)
			assert.Nil(t, cleanup)
			files, err := os.ReadDir(tmp)
			require.NoError(t, err)
			assert.Empty(t, files, "failed setup must remove its prompt and private home")
		})
	}
}

func TestMuseNativeEcho(t *testing.T) {
	skipIfWindows(t)
	muse, err := exec.LookPath("muse")
	if err != nil {
		t.Skip("muse is not on PATH")
	}
	t.Setenv("MUSE_NO_AUTO_UPDATE", "1")
	withUnsafeAgents(t, false)
	for _, tc := range []struct{ mode, key string }{
		{"review", "mcpServers"},
		{"review", "mcp_servers"},
		{"plan", "mcpServers"},
		{"plan", "mcp_servers"},
	} {
		t.Run(tc.mode+"/"+tc.key, func(t *testing.T) {
			repo := newPlanTestRepo(t)
			config := t.TempDir()
			require.NoError(t, os.Mkdir(filepath.Join(config, "muse"), 0o700))
			require.NoError(t, os.WriteFile(filepath.Join(config, "muse", "settings.json"), []byte(fmt.Sprintf(museSettingsWithMCP, tc.key)), 0o600))
			t.Setenv("XDG_CONFIG_HOME", config)
			t.Setenv("MUSE_BINARY", muse)
			capture := t.TempDir()
			t.Setenv("MUSE_CAPTURE_DIR", capture)
			// Muse's runtime temp directory must sit outside the planning worktree's ancestor.
			t.Setenv("MUSE_RUNTIME_TMP", t.TempDir())
			command := writeTempCommand(t, `#!/bin/sh
case "$1" in *etxtbsy*) exit 0;; esac
printf '%s' "$HOME" > "$MUSE_CAPTURE_DIR/home"
prev=""
for arg in "$@"; do
 if [ "$prev" = "--prompt-file" ]; then
  printf '%s' "$arg" > "$MUSE_CAPTURE_DIR/prompt"
 fi
 prev="$arg"
done
export TMPDIR="$MUSE_RUNTIME_TMP"
exec "$MUSE_BINARY" "$@" --provider echo
`)
			tmp := t.TempDir()
			t.Setenv("TMPDIR", tmp)
			a := NewMuseAgent(command)
			var result string
			var output bytes.Buffer
			if tc.mode == "plan" {
				result, err = RunPlan(t.Context(), a, repo, "HEAD", "contract prompt", &output, nil)
			} else {
				result, err = a.Review(t.Context(), repo, "HEAD", "contract prompt", &output)
			}
			require.NoError(t, err, "native Muse output: %s", output.String())
			assert.Equal(t, "echo: contract prompt", result)
			for _, name := range []string{"home", "prompt"} {
				path, err := os.ReadFile(filepath.Join(capture, name))
				require.NoError(t, err)
				assert.NoDirExists(t, string(path))
				assert.NoFileExists(t, string(path))
			}
		})
	}
}
