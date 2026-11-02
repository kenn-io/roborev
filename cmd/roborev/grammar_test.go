package main

import (
	"bytes"
	"encoding/json/v2"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
)

func grammarCommand(t *testing.T, root *cobra.Command, path string) *cobra.Command {
	t.Helper()
	cmd, rest, err := root.Find(strings.Fields(path))
	require.NoError(t, err)
	require.Empty(t, rest)
	require.Equal(t, "roborev "+path, cmd.CommandPath())
	return cmd
}

func TestCommandGrammarCompatibility(t *testing.T) {
	isolateCLIErrorGlobals(t)
	root := newRootCmd()
	for _, move := range commandMoves {
		t.Run(move.legacy, func(t *testing.T) {
			legacy := grammarCommand(t, root, move.legacy)
			canonical := grammarCommand(t, root, move.canonical)
			assert.True(t, legacy.Hidden)
			assert.Equal(t, move.legacy == "backfill-verdicts", canonical.Hidden)
			var compare func(*cobra.Command, *cobra.Command)
			compare = func(old, current *cobra.Command) {
				_, oldSynopsis, _ := strings.Cut(old.Use, " ")
				_, newSynopsis, _ := strings.Cut(current.Use, " ")
				assert.Equal(t, oldSynopsis, newSynopsis)
				old.Flags().VisitAll(func(flag *pflag.Flag) {
					actual := current.Flags().Lookup(flag.Name)
					require.NotNil(t, actual, current.CommandPath()+" --"+flag.Name)
					assert.Equal(t, flag.DefValue, actual.DefValue)
					assert.Equal(t, flag.Value.Type(), actual.Value.Type())
					assert.Equal(t, flag.Annotations, actual.Annotations)
					assert.Equal(t, flag.NoOptDefVal, actual.NoOptDefVal)
					assert.Equal(t, flag.Shorthand, actual.Shorthand)
					_, oldCompletion := old.GetFlagCompletionFunc(flag.Name)
					_, currentCompletion := current.GetFlagCompletionFunc(flag.Name)
					assert.Equal(t, oldCompletion, currentCompletion, flag.Name)
				})
				assert.Equal(t, stableCommandPath(old), stableCommandPath(current))
				assert.Equal(t, isCLIHook(old), isCLIHook(current))
				assert.Equal(t, cliUseCommands[stableCommandPath(old)], cliUseCommands[stableCommandPath(current)])
				for _, child := range old.Commands() {
					replacement := directCommand(current, child.Name())
					require.NotNil(t, replacement, child.CommandPath())
					compare(child, replacement)
				}
			}
			compare(legacy, canonical)
		})
	}
	canonical := grammarCommand(t, root, "job list")
	require.NoError(t, canonical.ParseFlags([]string{"--limit", "7"}))
	legacy := grammarCommand(t, root, "list")
	assert.False(t, legacy.Flags().Changed("limit"))
	assert.NotEqual(t, canonical.Flags().Lookup("limit").Value.String(), legacy.Flags().Lookup("limit").Value.String())
}

func TestCommandGrammarHelpAndCompletion(t *testing.T) {
	isolateCLIErrorGlobals(t)
	for _, args := range [][]string{{"--help"}, {"__complete", ""}} {
		root := newRootCmd()
		var output, diagnostics bytes.Buffer
		root.SetOut(&output)
		root.SetErr(&diagnostics)
		assert.Equal(t, 0, executeCLI(root, args))
		for _, name := range []string{"job", "hook", "agent", "report", "storage", "signing"} {
			assert.Contains(t, output.String(), name)
		}
		for line := range strings.SplitSeq(output.String(), "\n") {
			words := strings.Fields(line)
			if len(words) > 0 {
				for _, move := range commandMoves {
					assert.NotEqual(t, move.legacy, words[0], "legacy command advertised: %s", line)
				}
			}
		}
	}
	root := newRootCmd()
	assert.True(t, grammarCommand(t, root, "respond").Hidden)
	assert.Same(t, grammarCommand(t, root, "job list"), findGrammarAlias(t, root, "job ls"))
	assert.Same(t, grammarCommand(t, root, "job close"), findGrammarAlias(t, root, "job resolve"))
	root = newRootCmd()
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&output)
	assert.Equal(t, 0, executeCLI(root, []string{"__complete", "agent", "hook", ""}))
	assert.Contains(t, output.String(), "snooze")
	assert.Contains(t, output.String(), "install")
}

func findGrammarAlias(t *testing.T, root *cobra.Command, path string) *cobra.Command {
	t.Helper()
	cmd, rest, err := root.Find(strings.Fields(path))
	require.NoError(t, err)
	require.Empty(t, rest)
	return cmd
}

func TestCommandGrammarWireAndOutput(t *testing.T) {
	isolateCLIErrorGlobals(t)
	const payload = "{\"type\":\"assistant\",\"text\":\"fixture\"}\n"
	var requests []string
	md := daemonFromHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.URL.RequestURI())
		assert.Equal(t, "/api/job/log", r.URL.Path)
		assert.Equal(t, "42", r.URL.Query().Get("job_id"))
		assert.Equal(t, "true", r.URL.Query().Get("raw"))
		_, _ = w.Write([]byte(payload))
	}))
	for _, path := range []string{"log", "job log"} {
		root := newRootCmd()
		var output, diagnostics bytes.Buffer
		root.SetOut(&output)
		root.SetErr(&diagnostics)
		args := append(strings.Fields(path), "--raw", "42", "--server", md.Server.URL)
		assert.Equal(t, 0, executeCLI(root, args))
		assert.Equal(t, payload, output.String()) //nolint:testifylint // Raw logs must preserve bytes, including the trailing newline.
		if path == "log" {
			assert.Equal(t, "Command \"log\" is deprecated, use 'roborev job log' instead\n", diagnostics.String())
		} else {
			assert.Empty(t, diagnostics.String())
		}
	}
	require.Len(t, requests, 2)
	assert.Equal(t, requests[0], requests[1])
}

func TestCommandGrammarErrorsAndRemote(t *testing.T) {
	isolateCLIErrorGlobals(t)
	for _, path := range []string{"job show", "show"} {
		root := newRootCmd()
		var output, diagnostics bytes.Buffer
		root.SetOut(&output)
		root.SetErr(&diagnostics)
		assert.Equal(t, 2, executeCLI(root, append(strings.Fields(path), "--json", "--missing")))
		var envelope map[string]any
		require.NoError(t, json.Unmarshal(diagnostics.Bytes(), &envelope))
		assert.Contains(t, diagnostics.String(), "roborev "+path+" --help")
		assert.Empty(t, output.String())
	}
	root := newRootCmd()
	oldAddr := serverAddr
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	require.NoError(t, os.WriteFile(config.GlobalConfigPath(), []byte("[remote_client]\nexternal_url='https://reviews.example.com/history'\n"), 0o600))
	serverAddr = "https://reviews.example.com/history"
	t.Cleanup(func() { serverAddr = oldAddr })
	for _, path := range []string{"job list", "job show", "job wait", "job stream"} {
		require.NoError(t, validateRemoteCommandFor(grammarCommand(t, root, path)))
	}
	for _, path := range []string{"job cancel", "daemon resume", "agent hook run", "hook install", "storage legacy-reviews import"} {
		require.Error(t, validateRemoteCommandFor(grammarCommand(t, root, path)))
	}
	for _, path := range []string{"agent-hook run", "agent hook run", "agent hook fix-done"} {
		var diagnostics bytes.Buffer
		cmd := grammarCommand(t, root, path)
		cmd.SetErr(&diagnostics)
		legacyCommandWarning(cmd, nil)
		assert.Empty(t, diagnostics.String(), "hook compatibility: %s", path)
	}
	var diagnostics bytes.Buffer
	wait := grammarCommand(t, root, "wait")
	wait.SetErr(&diagnostics)
	legacyCommandWarning(wait, []string{"wait", "--job", "42", "--quiet"})
	assert.Empty(t, diagnostics.String())
}

func TestCommandGrammarJSONOutput(t *testing.T) {
	isolateCLIErrorGlobals(t)
	md := daemonFromHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/review":
			assert.Equal(t, "42", r.URL.Query().Get("job_id"))
			_, _ = w.Write([]byte(`{"id":1,"job_id":42,"agent":"test","output":"fixture output","job":{"id":42,"git_ref":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","job_type":"review"}}`))
		case "/api/comments":
			_, _ = w.Write([]byte(`{"responses":[]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	var previous string
	for _, path := range []string{"show", "job show"} {
		root := newRootCmd()
		var diagnostics bytes.Buffer
		root.SetErr(&diagnostics)
		output := captureStdout(t, func() {
			args := append(strings.Fields(path), "--job", "42", "--json", "--server", md.Server.URL)
			assert.Equal(t, 0, executeCLI(root, args))
		})
		var document map[string]any
		require.NoError(t, json.Unmarshal([]byte(output), &document))
		assert.Equal(t, "fixture output", document["output"])
		if path == "show" {
			previous = output
			assert.Contains(t, diagnostics.String(), "use 'roborev job show'")
		} else {
			assert.Equal(t, previous, output)
			assert.Empty(t, diagnostics.String())
		}
	}
}

func TestCommandGrammarResumeKeepsUnpauseRoute(t *testing.T) {
	isolateCLIErrorGlobals(t)
	var called bool
	md := daemonFromHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/api/queue/unpause", r.URL.Path)
		called = true
		_, _ = w.Write([]byte(`{"queue_paused":false}`))
	}))
	root := newRootCmd()
	var diagnostics bytes.Buffer
	root.SetErr(&diagnostics)
	output := captureStdout(t, func() {
		assert.Equal(t, 0, executeCLI(root, []string{"daemon", "resume", "--server", md.Server.URL}))
	})
	assert.True(t, called)
	assert.Contains(t, output, "Queue unpaused")
	assert.Empty(t, diagnostics.String())
}
