package main

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/testenv"
)

func TestUsageErrorPreservesCause(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		message := hegel.Draw(ht, hegel.Text())
		cause := errors.New(message)
		cmd := &cobra.Command{Use: "list"}
		err := usageErr(cmd, cause)
		var exit *exitError
		require.ErrorAs(ht, err, &exit)
		assert.Equal(ht, 2, exit.code)
		assert.ErrorIs(ht, err, cause)
		assert.Equal(ht, message, err.Error())
		assert.True(ht, cmd.SilenceUsage)
	})
}

func TestRootFlagErrorDoesNotDumpUsage(t *testing.T) {
	isolateCLIErrorGlobals(t)
	root := newRootCmd()
	var out, stderr strings.Builder
	root.SetOut(&out)
	root.SetErr(&stderr)
	root.SetArgs([]string{"list", "--bogus"})
	_, err := root.ExecuteC()
	require.Error(t, err)
	var exit *exitError
	require.ErrorAs(t, err, &exit)
	assert.Equal(t, 2, exit.code)
	assert.Empty(t, out.String())
	assert.NotContains(t, stderr.String(), "Usage:")
}

// Empty messages are valid causes too (Hegel's minimal counterexample).
func TestUsageErrorEmptyMessage(t *testing.T) {
	cause := errors.New("")
	err := usageErr(&cobra.Command{Use: "list"}, cause)
	var exit *exitError
	require.ErrorAs(t, err, &exit)
	assert.Equal(t, 2, exit.code)
	assert.Equal(t, "", err.Error())
	assert.ErrorIs(t, err, cause)
}

func TestCLIUsageRecovery(t *testing.T) {
	isolateCLIErrorGlobals(t)
	testenv.SetDataDir(t)
	repo := NewGitTestRepo(t)
	repo.CommitFile("example.txt", "fixture", "fixture")
	t.Chdir(repo.Dir)
	for _, tc := range []struct {
		args []string
		code int
		help string
	}{
		{[]string{"bogus"}, 2, "roborev --help"},
		{[]string{"daemon", "bogus"}, 2, "roborev daemon --help"},
		{[]string{"help", "bogus"}, 2, "roborev --help"},
		{[]string{"help", "config", "bogus"}, 2, "roborev config --help"},
		{[]string{"fix", "--list", "--plan"}, 2, "roborev fix --help"},
		{[]string{"list", "--bogus"}, 2, "roborev list --help"},
		{[]string{"completion", "bash", "extra"}, 2, "roborev completion bash --help"},
		{[]string{"list", "--limit", "many"}, 2, "roborev list --help"},
		{[]string{"config", "set", "foo"}, 2, "roborev config set --help"},
		{[]string{"signing-init"}, 2, "roborev signing-init --help"},
		{[]string{"snooze", "off", "--duration", "1h"}, 2, "roborev snooze --help"},
		{[]string{"review", "--branch", "--dirty"}, 2, "roborev review --help"},
		{[]string{"fix", "--list", "--batch"}, 2, "roborev fix --help"},
		{[]string{"wait", "--sha", "HEAD", "42"}, 2, "roborev wait --help"},
		{[]string{"search"}, 2, "roborev search --help"},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			var out, stderr strings.Builder
			root := newRootCmd()
			root.SetOut(&out)
			root.SetErr(&stderr)
			code := executeCLI(root, tc.args)
			assert.Equal(t, tc.code, code)
			assert.Empty(t, out.String())
			assert.Contains(t, stderr.String(), "hint:")
			assert.Contains(t, stderr.String(), tc.help)
			assert.NotContains(t, stderr.String(), "Usage:")
		})
	}
}

func TestCLIJSONErrorRoundTrip(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		message := hegel.Draw(ht, hegel.Text())
		hint := hegel.Draw(ht, hegel.Text())
		cmd := &cobra.Command{Use: "search"}
		cmd.Flags().Bool("json", false, "")
		var stderr strings.Builder
		cmd.SetErr(&stderr)
		code := renderCLIError(cmd, []string{"--json"}, withHint(errors.New(message), hint))
		var got struct {
			Error struct {
				Message  string `json:"message"`
				Hint     string `json:"hint"`
				Code     string `json:"code"`
				ExitCode int    `json:"exit_code"`
			} `json:"error"`
		}
		require.NoError(ht, json.Unmarshal([]byte(stderr.String()), &got))
		assert.Equal(ht, message, got.Error.Message)
		if hint != "" {
			assert.Equal(ht, hint, got.Error.Hint)
		}
		assert.Equal(ht, "runtime", got.Error.Code)
		assert.Equal(ht, 1, code)
		assert.Equal(ht, code, got.Error.ExitCode)
	})
}

func TestCLIErrorModeSelectors(t *testing.T) {
	for _, tc := range []struct {
		args []string
		json bool
	}{
		{[]string{"--bogus", "--json"}, true},
		{[]string{"--json", "--bogus"}, true},
		{[]string{"--json", "--json=false"}, false},
		{[]string{"--json=false", "--json"}, true},
		{[]string{"--json=true"}, true},
		{[]string{"--json=invalid"}, false},
		{[]string{"--", "--json"}, false},
		{[]string{"--prompt", "--json"}, false},
		{[]string{"-p", "--json"}, false},
		{[]string{"-vp--json"}, false},
		{[]string{"--prompt=--json"}, false},
		{[]string{"--prompt", "data", "--json"}, true},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			cmd := &cobra.Command{Use: "example"}
			cmd.Flags().Bool("json", false, "")
			cmd.Flags().BoolP("verbose", "v", false, "")
			cmd.Flags().StringP("prompt", "p", "", "")
			assert.Equal(t, tc.json, cliBoolFlag(cmd, tc.args, "json"))
		})
	}
}

func TestCLIErrorSilenceAndFormats(t *testing.T) {
	for _, tc := range []struct {
		name   string
		args   []string
		err    error
		code   int
		want   string
		absent string
	}{
		{"runtime", nil, errors.New("failed"), 1, "hint: run 'example --help'", "Usage:"},
		{"agent", []string{"--agent"}, errors.New("failed"), 1, "Hint:", "hint:"},
		{"quiet", []string{"--quiet"}, invalidValue(errors.New("invalid")), 4, "Error: invalid", "hint:"},
		{"verdict", nil, &exitError{code: 1}, 1, "", "Error:"},
		{"diagnostics already printed", []string{"--json"}, &exitError{code: 1, silent: true}, 1, "", "error"},
		{"export reset", nil, &exitError{code: 3, cause: errors.New("reset cursor")}, 3, "Error: reset cursor", "Usage:"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := &cobra.Command{Use: "example"}
			cmd.Flags().Bool("json", false, "")
			cmd.Flags().Bool("agent", false, "")
			cmd.Flags().Bool("quiet", false, "")
			var stderr strings.Builder
			cmd.SetErr(&stderr)
			assert.Equal(t, tc.code, renderCLIError(cmd, tc.args, tc.err))
			assert.Contains(t, stderr.String(), tc.want)
			assert.NotContains(t, stderr.String(), tc.absent)
		})
	}
	t.Run("provider agent is not output mode", func(t *testing.T) {
		cmd := &cobra.Command{Use: "review"}
		cmd.Flags().String("agent", "", "")
		var stderr strings.Builder
		cmd.SetErr(&stderr)
		assert.Equal(t, 1, renderCLIError(cmd, []string{"--agent", "test"}, errors.New("failed")))
		assert.Contains(t, stderr.String(), "hint:")
		assert.NotContains(t, stderr.String(), "Hint:")
	})
}

func TestCLIJSONBuffersDiagnostics(t *testing.T) {
	isolateCLIErrorGlobals(t)
	testenv.SetDataDir(t)
	for _, fail := range []bool{true, false} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			root := newRootCmd()
			cmd, _, err := root.Find([]string{"version"})
			require.NoError(t, err)
			cmd.RunE = func(*cobra.Command, []string) error {
				fmt.Fprintln(lifecycleOut, "startup progress")
				fmt.Fprintln(cliWarningOut, "configuration warning")
				if fail {
					return errors.New("failed")
				}
				return nil
			}
			var out, stderr strings.Builder
			root.SetOut(&out)
			root.SetErr(&stderr)
			code := executeCLI(root, []string{"version", "--json"})
			assert.Empty(t, out.String())
			if fail {
				assert.Equal(t, 1, code)
				assert.JSONEq(t, `{"error":{"code":"runtime","message":"failed","hint":"run 'roborev version --help' for usage and examples","exit_code":1}}`, stderr.String())
			} else {
				assert.Zero(t, code)
				assert.Equal(t, "startup progress\nconfiguration warning\n", stderr.String())
			}
		})
	}
}

func TestCLIInvalidValuesAndRecovery(t *testing.T) {
	isolateCLIErrorGlobals(t)
	testenv.SetDataDir(t)
	t.Chdir(t.TempDir())
	require.NoError(t, os.WriteFile(config.GlobalConfigPath(), []byte("[remote_client]\nexternal_url='https://reviews.example.com'\n"), 0o600))
	for _, tc := range []struct {
		args []string
		code int
		hint string
	}{
		{[]string{"list", "--server", "invalid"}, 4, "roborev list --help"},
		{[]string{"cancel", "abc"}, 4, "roborev list"},
		{[]string{"fix", "abc"}, 4, "roborev fix --help"},
		{[]string{"fix", "42abc"}, 4, "roborev fix --help"},
		{[]string{"fix", "--", "-1"}, 4, "roborev fix --help"},
		{[]string{"cancel", "0"}, 4, "roborev list"},
		{[]string{"close", "abc"}, 4, "roborev list"},
		{[]string{"wait", "--job", "abc"}, 4, "roborev wait --help"},
		{[]string{"--server", "https://reviews.example.com", "wait", "--job", "abc"}, 4, "roborev wait --help"},
		{[]string{"comment", "--job", "abc", "message"}, 4, "roborev comment --help"},
		{[]string{"comment", "--job", "0", "message"}, 4, "roborev comment --help"},
		{[]string{"show", "--job", "abc"}, 4, "roborev show --help"},
		{[]string{"show"}, 4, "roborev show --job"},
		{[]string{"wait", "--sha", "HEAD"}, 4, "commit"},
		{[]string{"config", "get", "not_a_key"}, 4, "roborev config list"},
		{[]string{"analyze", "--show-prompt", "invalid"}, 4, "roborev analyze --help"},
		{[]string{"snooze", "invalid"}, 4, "roborev snooze --help"},
		{[]string{"log", "abc"}, 4, "roborev log --help"},
		{[]string{"ui", "abc"}, 4, "roborev ui --help"},
		{[]string{"search", "query", "--verdict", "other"}, 4, "roborev search --help"},
		{[]string{"search", "query", "--since", "7d"}, 4, "roborev search --help"},
		{[]string{"search", "query", "--limit", "0"}, 4, "roborev search --help"},
		{[]string{"search", "query", "--open", "--closed"}, 2, "roborev search --help"},
		{[]string{"export", "reviews", "--profile", "invalid"}, 4, "roborev export reviews --help"},
		{[]string{"export", "reviews", "--cursor", "opaque", "--since", "2026-01-01"}, 2, "roborev export reviews --help"},
		{[]string{"init", "--no-daemon"}, 4, "git init"},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			root := newRootCmd()
			var out, stderr strings.Builder
			root.SetOut(&out)
			root.SetErr(&stderr)
			stdout := captureStdout(t, func() { assert.Equal(t, tc.code, executeCLI(root, tc.args)) })
			assert.Empty(t, stdout)
			assert.Empty(t, out.String())
			assert.Contains(t, stderr.String(), tc.hint)
			assert.Contains(t, stderr.String(), "hint:")
		})
	}
}

func TestCLIDaemonRecoveryHints(t *testing.T) {
	isolateCLIErrorGlobals(t)
	testenv.SetDataDir(t)
	oldGet, oldStart, oldCleanup := getAnyRunningDaemon, startDaemonForEnsure, cleanupZombieDaemons
	t.Cleanup(func() { getAnyRunningDaemon, startDaemonForEnsure, cleanupZombieDaemons = oldGet, oldStart, oldCleanup })
	localFailure := errors.New("synthetic startup failure")
	starts := 0
	getAnyRunningDaemon = func() (*daemon.RuntimeInfo, error) { return nil, os.ErrNotExist }
	startDaemonForEnsure = func() error { starts++; return localFailure }
	cleanupZombieDaemons = func(daemon.DaemonEndpoint) int { return 0 }
	for _, explicit := range []bool{false, true} {
		t.Run(fmt.Sprint(explicit), func(t *testing.T) {
			args := []string{"list", "--json"}
			if explicit {
				server := httptest.NewServer(http.NotFoundHandler())
				server.Close()
				args = append(args, "--server", server.URL)
			}
			before := starts
			root := newRootCmd()
			var out, stderr strings.Builder
			root.SetOut(&out)
			root.SetErr(&stderr)
			assert.Equal(t, 1, executeCLI(root, args))
			assert.Empty(t, out.String())
			var envelope struct {
				Error struct {
					Code    string `json:"code"`
					Message string `json:"message"`
					Hint    string `json:"hint"`
				} `json:"error"`
			}
			require.NoError(t, json.Unmarshal([]byte(stderr.String()), &envelope))
			assert.Equal(t, "daemon_unreachable", envelope.Error.Code)
			if explicit {
				assert.Equal(t, before, starts)
				assert.Contains(t, envelope.Error.Hint, "--server")
				assert.NotContains(t, envelope.Error.Message, "not running")
			} else {
				assert.Equal(t, before+1, starts)
				assert.Contains(t, envelope.Error.Hint, "roborev daemon start")
				assert.Contains(t, envelope.Error.Hint, "roborev doctor")
				assert.Contains(t, envelope.Error.Message, localFailure.Error())
			}
		})
	}
}

func TestCLIJSONErrorsAcrossCommands(t *testing.T) {
	isolateCLIErrorGlobals(t)
	testenv.SetDataDir(t)
	for _, args := range [][]string{
		{"show", "--job", "42"},
		{"list"},
		{"status"},
		{"summary"},
		{"cost"},
		{"doctor"},
		{"version"},
		{"search", "query"},
		{"quickstart"},
		{"run", "task"},
		{"analyze", "complexity", "example.go"},
		{"insights"},
		{"mcp", "status"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			root := newRootCmd()
			cmd, _, err := root.Find(args)
			require.NoError(t, err)
			require.NotNil(t, cmd.Flags().Lookup("json"))
			cmd.RunE = func(*cobra.Command, []string) error { return errors.New("synthetic failure") }
			var out, stderr strings.Builder
			root.SetOut(&out)
			root.SetErr(&stderr)
			assert.Equal(t, 1, executeCLI(root, append(args, "--json")))
			assert.Empty(t, out.String())
			var envelope map[string]any
			require.NoError(t, json.Unmarshal([]byte(stderr.String()), &envelope))
			require.Contains(t, envelope, "error")
			details, ok := envelope["error"].(map[string]any)
			require.True(t, ok)
			assert.Contains(t, details, "hint")
		})
	}
}

func TestCLIJSONArbitraryBytes(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		message := string(hegel.Draw(ht, hegel.Binary(0, -1)))
		hint := string(hegel.Draw(ht, hegel.Binary(1, -1)))
		cmd := &cobra.Command{Use: "example"}
		cmd.Flags().Bool("json", false, "")
		var out strings.Builder
		cmd.SetErr(&out)
		assert.Equal(ht, 1, renderCLIError(cmd, []string{"--json"}, withHint(errors.New(message), hint)))
		var envelope struct {
			Error struct {
				Message string `json:"message"`
				Hint    string `json:"hint"`
			} `json:"error"`
		}
		require.NoError(ht, json.Unmarshal([]byte(out.String()), &envelope))
		assert.Equal(ht, strings.ToValidUTF8(message, "\uFFFD"), envelope.Error.Message)
		assert.Equal(ht, strings.ToValidUTF8(hint, "\uFFFD"), envelope.Error.Hint)
	})
}

func TestCLIJSONInvalidUTF8Hint(t *testing.T) {
	cmd := &cobra.Command{Use: "example"}
	cmd.Flags().Bool("json", false, "")
	var out strings.Builder
	cmd.SetErr(&out)
	assert.Equal(t, 1, renderCLIError(cmd, []string{"--json"}, withHint(errors.New(""), "\x80")))
	assert.JSONEq(t, `{"error":{"code":"runtime","message":"","hint":"�","exit_code":1}}`, out.String())
}

func TestCLIErrorWrapperComposition(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		message := hegel.Draw(ht, hegel.Text())
		hint := hegel.Draw(ht, hegel.Text().MinSize(1))
		code := hegel.Draw(ht, hegel.SampledFrom([]int{1, 2, 3, 4}))
		cause := errors.New(message)
		original := &exitError{code: code, cause: cause, hint: hint, kind: "specific"}
		wrapped := withHint(fmt.Errorf("context: %w", original), "generic")
		wrapped = usageErr(&cobra.Command{Use: "example"}, wrapped)
		var exit *exitError
		require.ErrorAs(ht, wrapped, &exit)
		assert.Equal(ht, code, exit.code)
		assert.Equal(ht, hint, exit.hint)
		assert.ErrorIs(ht, wrapped, cause)
		assert.Equal(ht, "context: "+message, wrapped.Error())
	})
}

func TestCLIExecutableParentValidatesServer(t *testing.T) {
	isolateCLIErrorGlobals(t)
	testenv.SetDataDir(t)
	for _, name := range []string{"init", "install-hook"} {
		for _, tc := range []struct {
			server string
			code   int
		}{
			{"https://example.invalid", 2}, {"invalid", 4},
		} {
			t.Run(name+"/"+tc.server, func(t *testing.T) {
				if strings.HasPrefix(tc.server, "https://") {
					require.NoError(t, os.WriteFile(config.GlobalConfigPath(), fmt.Appendf(nil, "[remote_client]\nexternal_url='%s'\n", tc.server), 0o600))
				}
				root := newRootCmd()
				cmd, _, err := root.Find([]string{name})
				require.NoError(t, err)
				ran := false
				cmd.RunE = func(*cobra.Command, []string) error { ran = true; return nil }
				var out, stderr strings.Builder
				root.SetOut(&out)
				root.SetErr(&stderr)
				assert.Equal(t, tc.code, executeCLI(root, []string{name, "--server", tc.server}))
				assert.False(t, ran, "validation must run before an executable parent can write local state")
				assert.Empty(t, out.String())
				assert.Contains(t, stderr.String(), name+" --help")
			})
		}
	}
}

func TestCLIHookFailuresKeepBriefDiagnostics(t *testing.T) {
	isolateCLIErrorGlobals(t)
	testenv.SetDataDir(t)
	for _, tc := range []struct {
		args    []string
		message string
	}{
		{[]string{"agent-hook", "run", "--source", "stale", "--agent", "codex"}, "agent hook registration is outdated"},
		{[]string{"agent-hook", "fix-done", "invalid"}, "parse fix session ID"},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			root := newRootCmd()
			var out, stderr strings.Builder
			root.SetOut(&out)
			root.SetErr(&stderr)
			assert.Equal(t, 1, executeCLI(root, tc.args))
			assert.Empty(t, out.String())
			assert.Contains(t, stderr.String(), "Error: "+tc.message)
			assert.NotContains(t, stderr.String(), "hint:")
			assert.NotContains(t, stderr.String(), "Hint:")
			assert.NotContains(t, stderr.String(), "Usage:")
		})
	}
	root := newRootCmd()
	var out, stderr strings.Builder
	root.SetOut(&out)
	root.SetErr(&stderr)
	assert.Equal(t, 1, executeCLI(root, []string{"post-commit", "--bogus"}))
	assert.Empty(t, out.String())
	assert.Empty(t, stderr.String())
}

func TestCLIRemoteShowDoesNotRequireLocalCheckout(t *testing.T) {
	isolateCLIErrorGlobals(t)
	testenv.SetDataDir(t)
	t.Chdir(t.TempDir())
	require.NoError(t, os.WriteFile(config.GlobalConfigPath(), []byte("[remote_client]\nexternal_url='https://reviews.example.com'\n"), 0o600))
	root := newRootCmd()
	var out, stderr strings.Builder
	root.SetOut(&out)
	root.SetErr(&stderr)
	assert.Equal(t, 2, executeCLI(root, []string{"--server", "https://reviews.example.com", "show"}))
	assert.Empty(t, out.String())
	assert.Contains(t, stderr.String(), "remote show requires --job")
	assert.NotContains(t, stderr.String(), "git repository")
}

func TestCLIOutputSelectorsAgreeWithFlagParser(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		value := hegel.Draw(ht, hegel.Text())
		first := hegel.Draw(ht, hegel.SampledFrom([]bool{false, true}))
		last := hegel.Draw(ht, hegel.SampledFrom([]bool{false, true}))
		short := hegel.Draw(ht, hegel.SampledFrom([]bool{false, true}))
		separator := hegel.Draw(ht, hegel.SampledFrom([]bool{false, true}))
		cmd := &cobra.Command{Use: "example"}
		cmd.Flags().Bool("json", false, "")
		cmd.Flags().String("agent", "", "")
		cmd.Flags().StringP("prompt", "p", "", "")
		args := []string{"--json=" + fmt.Sprint(first), "--agent", value}
		if short {
			args = append(args, "-p", value)
		} else {
			args = append(args, "--prompt="+value)
		}
		args = append(args, "--json="+fmt.Sprint(last))
		if separator {
			args = append(args, "--", "--json="+fmt.Sprint(!last))
		}
		require.NoError(ht, cmd.ParseFlags(args))
		want, err := cmd.Flags().GetBool("json")
		require.NoError(ht, err)
		assert.Equal(ht, want, cliBoolFlag(cmd, args, "json"))
		assert.False(ht, cliBoolFlag(cmd, args, "agent"))
	})
}

// Production executes one command per process; root integration tests must
// restore the globals also used by standalone command fixtures.
func isolateCLIErrorGlobals(t *testing.T) {
	t.Helper()
	oldAddr, oldEndpoint := serverAddr, parsedServerEndpoint
	oldVerbose, oldFromSkill := verbose, fromSkill
	serverAddr, parsedServerEndpoint = "", nil
	verbose, fromSkill = false, false
	t.Cleanup(func() {
		serverAddr, parsedServerEndpoint = oldAddr, oldEndpoint
		verbose, fromSkill = oldVerbose, oldFromSkill
	})
}
