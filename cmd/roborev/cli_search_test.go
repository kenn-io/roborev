package main

import (
	"bytes"
	"encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type cliSearchDocument struct {
	Query   string `json:"query"`
	Results []struct {
		Command string `json:"command"`
		Summary string `json:"summary"`
		Usage   string `json:"usage"`
	} `json:"results"`
}

func executeCLISearch(args ...string) (int, string, string) {
	root := newRootCmd()
	var output, diagnostics bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&diagnostics)
	code := executeCLI(root, args)
	return code, output.String(), diagnostics.String()
}

func TestCLISearchDiscovery(t *testing.T) {
	assert := assert.New(t)
	isolateCLIErrorGlobals(t)
	code, output, diagnostics := executeCLISearch("--help")
	require.Equal(t, 0, code)
	assert.Empty(diagnostics)
	firstLine := strings.SplitN(output, "\n", 2)[0]
	assert.Equal(`Agents: find the right command with: roborev cli search "<what you want to do>"`, firstLine)
	_, invocation, found := strings.Cut(firstLine, "with: ")
	require.True(t, found)
	commands, err := advertisedCommands(invocation)
	require.NoError(t, err)
	require.Len(t, commands, 1)
	assert.NoError(validateHelpInvocation(commands[0]))
	code, output, diagnostics = executeCLISearch("cli")
	assert.Equal(0, code)
	assert.Contains(output, "search")
	assert.Empty(diagnostics)
}

func TestCLISearchRunsOffline(t *testing.T) {
	assert := assert.New(t)
	isolateCLIErrorGlobals(t)
	home, data, outside := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("ROBOREV_DATA_DIR", data)
	t.Chdir(outside)
	require.NoError(t, os.WriteFile(filepath.Join(data, "config.toml"), []byte("invalid = ["), 0o600))
	for _, args := range [][]string{
		{"cli", "search", "review", "--json"},
		{"cli", "search", "review", "--server", "invalid-endpoint", "--json"},
		{"cli", "search", "review", "--server", "https://reviews.example.com", "--json"},
		{"cli"},
	} {
		root := newRootCmd()
		root.PersistentPreRunE = func(*cobra.Command, []string) error {
			return errors.New("root runtime setup must not run")
		}
		var output, diagnostics bytes.Buffer
		root.SetOut(&output)
		root.SetErr(&diagnostics)
		assert.Equal(0, executeCLI(root, args), "%v: %s", args, diagnostics.String())
		assert.NotEmpty(output.String())
		assert.Empty(diagnostics.String())
	}
	entries, err := os.ReadDir(home)
	require.NoError(t, err)
	assert.Empty(entries)
	entries, err = os.ReadDir(data)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal("config.toml", entries[0].Name())
}

func TestCLISearchOutput(t *testing.T) {
	assert := assert.New(t)
	isolateCLIErrorGlobals(t)
	code, output, diagnostics := executeCLISearch("cli", "search", "review", "uncommitted", "changes", "--json")
	require.Equal(t, 0, code, diagnostics)
	assert.Empty(diagnostics)
	var document cliSearchDocument
	require.NoError(t, json.Unmarshal([]byte(output), &document))
	assert.Equal("review uncommitted changes", document.Query)
	require.NotEmpty(t, document.Results)
	assert.LessOrEqual(len(document.Results), 5)
	assert.Equal("roborev review", document.Results[0].Command)
	assert.NotEmpty(document.Results[0].Summary)
	assert.Equal("roborev review [commit | start end]", document.Results[0].Usage)
	assert.NotContains(output, `"score"`)
	assert.True(strings.HasSuffix(output, "\n"))

	code, quoted, diagnostics := executeCLISearch("cli", "search", "review uncommitted changes", "--json")
	assert.Equal(0, code, diagnostics)
	assert.Equal(output, quoted)
	code, output, diagnostics = executeCLISearch("cli", "search", "review uncommitted changes", "--limit", "1")
	assert.Equal(0, code, diagnostics)
	assert.Contains(output, "roborev review  Review a commit, commit range, or uncommitted changes\n")
	assert.Contains(output, "Run 'roborev review --help' for flags and examples.\n")
	assert.Len(strings.Split(strings.TrimSpace(output), "\n"), 2)
}

func TestCLISearchQueryWhitespace(t *testing.T) {
	isolateCLIErrorGlobals(t)
	code, output, diagnostics := executeCLISearch("cli", "search", "  zzqxvnonexistent  ", "--json")
	assert.Equal(t, 0, code, diagnostics)
	assert.JSONEq(t, `{"query":"zzqxvnonexistent","results":[]}`, output)
}

func TestCLISearchNoResults(t *testing.T) {
	assert := assert.New(t)
	isolateCLIErrorGlobals(t)
	code, output, diagnostics := executeCLISearch("cli", "search", "zzqxvnonexistent", "--json")
	assert.Equal(0, code, diagnostics)
	assert.JSONEq(`{"query":"zzqxvnonexistent","results":[]}`, output)
	code, output, diagnostics = executeCLISearch("cli", "search", "zzqxvnonexistent")
	assert.Equal(0, code, diagnostics)
	assert.Contains(output, "Run 'roborev --help' for flags and examples.")
}

func TestCLISearchUsageErrors(t *testing.T) {
	assert := assert.New(t)
	isolateCLIErrorGlobals(t)
	for _, args := range [][]string{
		{}, {" "}, {"review", "--limit", "0"}, {"review", "--limit", "21"},
		{"review", "--limit", "-1"}, {"review", "--limit", "many"}, {"review", "--missing"},
	} {
		for _, jsonMode := range []bool{false, true} {
			command := append([]string{"cli", "search"}, args...)
			if jsonMode {
				command = append(command, "--json")
			}
			code, output, diagnostics := executeCLISearch(command...)
			assert.Equal(2, code, "%v: %s", command, diagnostics)
			assert.Empty(output)
			assert.Contains(diagnostics, "roborev cli search --help")
			if jsonMode {
				var envelope struct {
					Error struct {
						Code     string `json:"code"`
						ExitCode int    `json:"exit_code"`
					} `json:"error"`
				}
				require.NoError(t, json.Unmarshal([]byte(diagnostics), &envelope))
				assert.Equal("usage", envelope.Error.Code)
				assert.Equal(2, envelope.Error.ExitCode)
			}
		}
	}
}

var cliSearchTasks = []struct{ query, command string }{
	{"review uncommitted changes", "roborev review"},
	{"show recent failed review jobs", "roborev job list"},
	{"read findings for a commit", "roborev job show"},
	{"add a note to a review", "roborev job comment"},
	{"log a note about a review", "roborev job comment"},
	{"mark a review resolved", "roborev job close"},
	{"cancel a queued job", "roborev job cancel"},
	{"wait for a review to finish", "roborev job wait"},
	{"search review history for a race", "roborev job search"},
	{"show agent output log", "roborev job log"},
	{"pause the review queue", "roborev daemon pause"},
	{"resume the review queue", "roborev daemon resume"},
	{"check available providers", "roborev agent check"},
	{"get a configuration value", "roborev config get"},
	{"report review spending", "roborev report cost"},
	{"fix review findings", "roborev fix"},
}

func assertCLISearchResults(t *testing.T, document cliSearchDocument) {
	assert := assert.New(t)
	t.Helper()
	root := newRootCmd()
	for _, result := range document.Results {
		assert.NotContains([]string{
			"roborev tui", "roborev ui", "roborev daemon run", "roborev mcp serve",
			"roborev post-commit", "roborev agent hook run", "roborev agent hook fix-done",
			"roborev job", "roborev config", "roborev agent hook", "roborev daemon",
			"roborev agent", "roborev report", "roborev hook", "roborev storage",
			"roborev signing", "roborev export", "roborev sync", "roborev repo",
			"roborev ci", "roborev mcp",
		}, result.Command)
		cmd, rest, err := root.Find(strings.Fields(strings.TrimPrefix(result.Command, "roborev ")))
		require.NoError(t, err)
		assert.Empty(rest)
		assert.Equal(result.Command, cmd.CommandPath())
		assert.True(cmd.Runnable(), result.Command)
		for current := cmd; current != nil; current = current.Parent() {
			assert.False(current.Hidden, result.Command)
			assert.Empty(current.Deprecated, result.Command)
			assert.NotContains([]string{"help", "completion", "cli"}, current.Name())
		}
	}
}

func TestCLISearchTasks(t *testing.T) {
	isolateCLIErrorGlobals(t)
	for _, task := range cliSearchTasks {
		t.Run(task.query, func(t *testing.T) {
			code, output, diagnostics := executeCLISearch("cli", "search", task.query, "--json")
			require.Equal(t, 0, code, diagnostics)
			var document cliSearchDocument
			require.NoError(t, json.Unmarshal([]byte(output), &document))
			require.NotEmpty(t, document.Results)
			assert.Equal(t, task.command, document.Results[0].Command)
			assertCLISearchResults(t, document)
		})
	}
}

func TestCLISearchEligibleParents(t *testing.T) {
	isolateCLIErrorGlobals(t)
	for _, path := range []string{"init", "hook install", "agent skills", "job log"} {
		code, output, diagnostics := executeCLISearch("cli", "search", path, "--limit", "20", "--json")
		require.Equal(t, 0, code, diagnostics)
		var document cliSearchDocument
		require.NoError(t, json.Unmarshal([]byte(output), &document))
		var commands []string
		for _, result := range document.Results {
			commands = append(commands, result.Command)
		}
		assert.Contains(t, commands, "roborev "+path)
		assertCLISearchResults(t, document)
	}
	for _, path := range []string{"tui", "ui", "daemon run", "mcp serve", "post-commit", "agent hook run", "agent hook fix-done", "job", "config", "agent hook"} {
		code, output, diagnostics := executeCLISearch("cli", "search", path, "--limit", "20", "--json")
		require.Equal(t, 0, code, diagnostics)
		var document cliSearchDocument
		require.NoError(t, json.Unmarshal([]byte(output), &document))
		assertCLISearchResults(t, document)
	}
}

func FuzzCLISearch(f *testing.F) {
	for _, query := range []string{"", " ", "review", " review ", "log a note", "provider", "--json", "résumé", "☃", "review\x00log", "review\xff"} {
		f.Add(query)
	}
	f.Fuzz(func(t *testing.T, query string) {
		assert := assert.New(t)
		isolateCLIErrorGlobals(t)
		args := []string{"cli", "search", "--json", "--", query}
		code, output, diagnostics := executeCLISearch(args...)
		again, repeated, repeatedDiagnostics := executeCLISearch(args...)
		assert.Equal(code, again)
		assert.Equal(output, repeated)
		assert.Equal(diagnostics, repeatedDiagnostics)
		if strings.TrimSpace(query) == "" {
			assert.Equal(2, code)
			assert.Empty(output)
			return
		}
		require.Equal(t, 0, code, diagnostics)
		var document cliSearchDocument
		require.NoError(t, json.Unmarshal([]byte(output), &document))
		assert.Equal(strings.ToValidUTF8(strings.TrimSpace(query), "\uFFFD"), document.Query)
		assert.NotNil(document.Results)
		assert.LessOrEqual(len(document.Results), 5)
		assertCLISearchResults(t, document)
	})
}

type failingCLISearchWriter struct{}

func (failingCLISearchWriter) Write([]byte) (int, error) {
	return 0, errors.New("output unavailable")
}

func TestCLISearchWriterError(t *testing.T) {
	isolateCLIErrorGlobals(t)
	for _, jsonMode := range []bool{false, true} {
		root := newRootCmd()
		root.SetOut(failingCLISearchWriter{})
		var diagnostics bytes.Buffer
		root.SetErr(&diagnostics)
		args := []string{"cli", "search", "review"}
		if jsonMode {
			args = append(args, "--json")
		}
		assert.Equal(t, 1, executeCLI(root, args))
		assert.Contains(t, diagnostics.String(), "output unavailable")
	}
}
