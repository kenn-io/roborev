package agent

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/gofrs/flock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMuseBuildArgs(t *testing.T) {
	for _, tc := range []struct {
		name      string
		agentic   bool
		reasoning ReasoningLevel
	}{
		{"review", false, ReasoningStandard},
		{"agentic", true, ReasoningStandard},
		{"reasoning thorough", false, ReasoningThorough},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withUnsafeAgents(t, false)
			a := NewMuseAgent("").WithReasoning(tc.reasoning).WithAgentic(tc.agentic).(*MuseAgent)
			args := a.buildArgs(a.Agentic, "prompt.md")
			assert := assert.New(t)
			assert.Contains(strings.Join(args, " "), "--prompt-file prompt.md")
			if tc.agentic {
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
	command := writeTempCommand(t, `#!/bin/sh
case "$1" in *etxtbsy*) exit 0;; esac
prev=""
for arg in "$@"; do
 if [ "$prev" = "--prompt-file" ]; then
  test -f "$arg" || exit 2
  test "$(cat "$arg")" = 'prompt body' || exit 3
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
}

func TestMuseReviewIsolatesConfigAndData(t *testing.T) {
	skipIfWindows(t)
	withUnsafeAgents(t, false)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("MUSE_ORIGINAL_HOME", home)
	command := writeTempCommand(t, `#!/bin/sh
case "$1" in *etxtbsy*) exit 0;; esac
set -eu
test "$HOME" != "$MUSE_ORIGINAL_HOME"
test "$HOME" = "$USERPROFILE"
test "$XDG_CONFIG_HOME" = "$HOME/.config"
test "$XDG_DATA_HOME" = "$HOME/data"
test "$XDG_CACHE_HOME" = "$HOME/cache"
prev=""
prompt=""
for arg in "$@"; do
 if [ "$prev" = "--prompt-file" ]; then prompt="$arg"; fi
 prev="$arg"
done
test -f "$prompt"
printf 'No issues found.\n'
`)
	repo := t.TempDir()
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	result, err := NewMuseAgent(command).Review(t.Context(), repo, "HEAD", "review", nil)
	require.NoError(t, err)
	assert.Equal(t, "No issues found.", result)
	files, err := os.ReadDir(tmp)
	require.NoError(t, err)
	assert.Empty(t, files, "review must remove its prompt and private home")
}

func TestPrepareMuseRunIsolatesConfigAndData(t *testing.T) {
	skipIfWindows(t)
	// auth names the change to the private login; user is the user's
	// auth.json token afterward.
	for _, tc := range []struct{ source, key, auth, user string }{
		{"xdg", "mcpServers", "example", "example"},
		{"xdg", "mcp_servers", "example", "example"},
		{"home", "mcpServers", "example", "example"},
		{"home", "mcp_servers", "example", "example"},
		{"xdg", "mcpServers", "renewed", "renewed"},
		{"xdg", "mcpServers", "concurrent", "concurrent"},
		{"xdg", "mcpServers", "locked", "example"},
		{"xdg", "mcpServers", "unwritable", "example"},
	} {
		t.Run(tc.source+"/"+tc.key+"/"+tc.auth, func(t *testing.T) {
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
				// Muse creates its lock file beside auth.json.
				".auth.json.lock": "",
			} {
				require.NoError(t, os.WriteFile(filepath.Join(museConfig, name), []byte(data), 0o600))
			}
			lockPath := filepath.Join(museConfig, ".auth.json.lock")
			lock := flock.New(lockPath, flock.SetPermissions(0o600))
			if tc.auth == "locked" {
				locked, err := lock.TryLock()
				require.NoError(t, err)
				require.True(t, locked)
			}
			t.Cleanup(func() { _ = lock.Close() })
			if tc.auth == "unwritable" {
				if os.Geteuid() == 0 {
					t.Skip("root can write to a read-only directory")
				}
				require.NoError(t, os.Chmod(museConfig, 0o500))
				t.Cleanup(func() { _ = os.Chmod(museConfig, 0o700) })
			}
			var logs bytes.Buffer
			prevLog := log.Writer()
			log.SetOutput(&logs)
			t.Cleanup(func() { log.SetOutput(prevLog) })
			dataHome := t.TempDir()
			require.NoError(t, os.MkdirAll(filepath.Join(dataHome, "muse", "plugins"), 0o700))
			t.Setenv("XDG_DATA_HOME", dataHome)
			tmp := t.TempDir()
			t.Setenv("TMPDIR", tmp)
			_, env, cleanup, err := prepareMuseRun("review", false)
			require.NoError(t, err)
			privateEnv := make(map[string]string)
			for _, entry := range env {
				key, value, _ := strings.Cut(entry, "=")
				privateEnv[key] = value
			}
			privateConfig := filepath.Join(privateEnv["XDG_CONFIG_HOME"], "muse")
			if tc.auth == "example" {
				assert := assert.New(t)
				home := privateEnv["HOME"]
				assert.Equal(home, privateEnv["USERPROFILE"])
				assert.Equal(filepath.Join(home, ".config"), privateEnv["XDG_CONFIG_HOME"])
				assert.Equal(filepath.Join(home, "data"), privateEnv["XDG_DATA_HOME"])
				assert.Equal(filepath.Join(home, "cache"), privateEnv["XDG_CACHE_HOME"])
				assert.DirExists(privateEnv["XDG_CACHE_HOME"])
				assert.Equal(tmp, filepath.Dir(home))
				assert.NoFileExists(filepath.Join(privateConfig, "trust.json"))
				assert.NoFileExists(filepath.Join(privateConfig, "plugins.json"))
				files, err := os.ReadDir(privateEnv["XDG_DATA_HOME"])
				require.NoError(t, err)
				assert.Empty(files)
				settings, err := os.ReadFile(filepath.Join(privateConfig, "settings.json"))
				require.NoError(t, err)
				assert.JSONEq(`{"schema_version":1,"model":"muse-spark-1.3","hooks":{}}`, string(settings))
				auth, err := os.ReadFile(filepath.Join(privateConfig, "auth.json"))
				require.NoError(t, err)
				assert.JSONEq(`{"token":"example"}`, string(auth))
			} else {
				require.NoError(t, os.WriteFile(filepath.Join(privateConfig, "auth.json"), []byte(`{"token":"renewed"}`), 0o600))
				if tc.auth == "concurrent" {
					require.NoError(t, os.WriteFile(filepath.Join(museConfig, "auth.json"), []byte(`{"token":"concurrent"}`), 0o600))
				}
			}
			synctest.Test(t, func(t *testing.T) { cleanup() })
			if tc.auth == "locked" || tc.auth == "unwritable" {
				assert.Contains(t, logs.String(), "muse: could not save renewed login")
			} else {
				assert.Empty(t, logs.String())
			}
			auth, err := os.ReadFile(filepath.Join(museConfig, "auth.json"))
			require.NoError(t, err)
			assert.JSONEq(t, fmt.Sprintf(`{"token":%q}`, tc.user), string(auth))
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
	// Agentic runs inherit config even when non-agentic setup cannot copy it.
	configHome := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(configHome, "muse"), []byte("not a directory"), 0o600))
	t.Setenv("XDG_CONFIG_HOME", configHome)
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
	for _, mode := range []string{"review", "plan"} {
		t.Run(mode, func(t *testing.T) {
			repo := newPlanTestRepo(t)
			config := t.TempDir()
			require.NoError(t, os.Mkdir(filepath.Join(config, "muse"), 0o700))
			require.NoError(t, os.WriteFile(filepath.Join(config, "muse", "settings.json"), []byte(fmt.Sprintf(museSettingsWithMCP, "mcpServers")), 0o600))
			t.Setenv("XDG_CONFIG_HOME", config)
			t.Setenv("MUSE_BINARY", muse)
			// Muse's runtime temp directory must sit outside the planning worktree's ancestor.
			t.Setenv("MUSE_RUNTIME_TMP", t.TempDir())
			command := writeTempCommand(t, `#!/bin/sh
case "$1" in *etxtbsy*) exit 0;; esac
export TMPDIR="$MUSE_RUNTIME_TMP"
exec "$MUSE_BINARY" "$@" --provider echo
`)
			tmp := t.TempDir()
			t.Setenv("TMPDIR", tmp)
			a := NewMuseAgent(command)
			var result string
			var output bytes.Buffer
			if mode == "plan" {
				result, err = RunPlan(t.Context(), a, repo, "HEAD", "contract prompt", &output, nil)
			} else {
				result, err = a.Review(t.Context(), repo, "HEAD", "contract prompt", &output)
			}
			require.NoError(t, err, "native Muse output: %s", output.String())
			assert.Equal(t, "echo: contract prompt", result)
		})
	}
}
