//go:build codexeval

package skills

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type codexCommandEvent struct {
	Command          string
	AggregatedOutput string
	ExitCode         *int
	Status           string
}

func parseCodexCommandEvents(r io.Reader) ([]codexCommandEvent, error) {
	decoder := json.NewDecoder(r)
	var commands []codexCommandEvent
	for record := 1; ; record++ {
		var event struct {
			Type string `json:"type"`
			Item struct {
				Type             string `json:"type"`
				Command          string `json:"command"`
				AggregatedOutput string `json:"aggregated_output"`
				ExitCode         *int   `json:"exit_code"`
				Status           string `json:"status"`
			} `json:"item"`
		}
		if err := decoder.Decode(&event); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, fmt.Errorf("parse Codex JSON event %d: %w", record, err)
		}
		if event.Type == "item.completed" && event.Item.Type == "command_execution" {
			commands = append(commands, codexCommandEvent{
				Command:          event.Item.Command,
				AggregatedOutput: event.Item.AggregatedOutput,
				ExitCode:         event.Item.ExitCode,
				Status:           event.Item.Status,
			})
		}
	}
	return commands, nil
}

func containsSuccessfulRoborevBranchReview(events []codexCommandEvent, marker string) (bool, error) {
	for _, event := range events {
		workflow, err := commandContainsRoborevBranchReview(event.Command)
		if err != nil {
			return false, err
		}
		if workflow && event.ExitCode != nil && *event.ExitCode == 0 &&
			event.Status == "completed" && containsExactOutputLine(event.AggregatedOutput, marker) {
			return true, nil
		}
	}
	return false, nil
}

func containsStubExecution(events []codexCommandEvent, marker string) bool {
	for _, event := range events {
		if containsExactOutputLine(event.AggregatedOutput, marker) {
			return true
		}
	}
	return false
}

func containsForbiddenRoborevExecution(events []codexCommandEvent, marker string) (bool, error) {
	if containsStubExecution(events, marker) {
		return true, nil
	}
	commands := make([]string, len(events))
	for i, event := range events {
		commands[i] = event.Command
	}
	return containsRoborevWorkflowInvocation(commands)
}

func containsExactOutputLine(output, marker string) bool {
	for _, line := range strings.Split(output, "\n") {
		if strings.TrimSuffix(line, "\r") == marker {
			return true
		}
	}
	return false
}

func containsRoborevBranchReviewWorkflow(commands []string) (bool, error) {
	for _, command := range commands {
		match, err := commandContainsRoborevBranchReview(command)
		if err != nil {
			return false, err
		}
		if match {
			return true, nil
		}
	}
	return false, nil
}

func containsRoborevWorkflowInvocation(commands []string) (bool, error) {
	for _, command := range commands {
		match, err := commandContainsRoborevInvocation(command)
		if err != nil {
			return false, err
		}
		if match {
			return true, nil
		}
	}
	return false, nil
}

func commandContainsRoborevInvocation(command string) (bool, error) {
	tokens, err := shellWords(command)
	if err != nil {
		return false, fmt.Errorf("classify command: %w", err)
	}
	for start := 0; start < len(tokens); {
		end := start
		for end < len(tokens) && !isShellSeparator(tokens[end]) {
			end++
		}
		match, err := simpleCommandMatches(tokens[start:end], func(tokens []string) bool {
			return len(tokens) > 0 && filepath.Base(tokens[0]) == "roborev"
		})
		if err != nil {
			return false, fmt.Errorf("classify command: %w", err)
		}
		if match {
			return true, nil
		}
		start = end + 1
	}
	return false, nil
}

func commandContainsRoborevBranchReview(command string) (bool, error) {
	tokens, err := shellWords(command)
	if err != nil {
		return false, fmt.Errorf("classify command: %w", err)
	}
	for start := 0; start < len(tokens); {
		end := start
		for end < len(tokens) && !isShellSeparator(tokens[end]) {
			end++
		}
		match, err := simpleCommandMatches(tokens[start:end], roborevBranchReviewTokens)
		if err != nil {
			return false, fmt.Errorf("classify command: %w", err)
		}
		if match {
			return true, nil
		}
		start = end + 1
	}
	return false, nil
}

func simpleCommandMatches(tokens []string, matcher func([]string) bool) (bool, error) {
	for len(tokens) > 0 && isShellAssignment(tokens[0]) {
		tokens = tokens[1:]
	}
	if len(tokens) == 0 {
		return false, nil
	}
	executable := filepath.Base(tokens[0])
	if executable == "zsh" || executable == "sh" || executable == "bash" {
		commandString, ok, err := shellCommandString(tokens[1:])
		if err != nil || !ok {
			return false, err
		}
		inner, err := shellWords(commandString)
		if err != nil {
			return false, err
		}
		return tokenListMatches(inner, matcher)
	}
	if executable == "command" {
		if len(tokens) >= 2 && (tokens[1] == "-v" || tokens[1] == "-V") {
			return false, nil
		}
		i := 1
		for i < len(tokens) && (tokens[i] == "-p" || tokens[i] == "--") {
			i++
		}
		return simpleCommandMatches(tokens[i:], matcher)
	}
	if executable == "env" {
		i := 1
		for i < len(tokens) {
			switch {
			case tokens[i] == "--", tokens[i] == "-i", tokens[i] == "--ignore-environment":
				i++
			case tokens[i] == "-u" || tokens[i] == "--unset" || tokens[i] == "-C" || tokens[i] == "--chdir":
				i += 2
			case strings.HasPrefix(tokens[i], "--unset=") || strings.HasPrefix(tokens[i], "--chdir="):
				i++
			case isShellAssignment(tokens[i]):
				i++
			default:
				return simpleCommandMatches(tokens[i:], matcher)
			}
		}
		return false, nil
	}
	if executable == "exec" {
		i := 1
		for i < len(tokens) {
			switch tokens[i] {
			case "--", "-c", "-l":
				i++
			case "-a":
				i += 2
			default:
				return simpleCommandMatches(tokens[i:], matcher)
			}
		}
		return false, nil
	}
	return matcher(tokens), nil
}

func tokenListMatches(tokens []string, matcher func([]string) bool) (bool, error) {
	for start := 0; start < len(tokens); {
		end := start
		for end < len(tokens) && !isShellSeparator(tokens[end]) {
			end++
		}
		match, err := simpleCommandMatches(tokens[start:end], matcher)
		if err != nil || match {
			return match, err
		}
		start = end + 1
	}
	return false, nil
}

func roborevBranchReviewTokens(tokens []string) bool {
	return len(tokens) == 4 && tokens[0] == "roborev" &&
		tokens[1] == "review" && tokens[2] == "--branch" && tokens[3] == "--wait"
}

func shellCommandString(tokens []string) (string, bool, error) {
	for i := 0; i < len(tokens); i++ {
		token := tokens[i]
		if token == "--login" || token == "-l" {
			continue
		}
		if token == "-c" || shortShellOptionHasCommandFlag(token) {
			if i+1 >= len(tokens) {
				return "", false, errors.New("shell command flag has no command string")
			}
			return tokens[i+1], true, nil
		}
		if !strings.HasPrefix(token, "-") {
			return "", false, nil
		}
	}
	return "", false, nil
}

func shortShellOptionHasCommandFlag(token string) bool {
	return token == "-lc" || token == "-cl"
}

func isShellAssignment(token string) bool {
	name, _, ok := strings.Cut(token, "=")
	if !ok || name == "" {
		return false
	}
	for i, r := range name {
		if !(r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || i > 0 && r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

func isShellSeparator(token string) bool {
	return token == "&&" || token == "||" || token == ";" || token == "|" || token == "&"
}

func shellWords(command string) ([]string, error) {
	var words []string
	var word strings.Builder
	quote := rune(0)
	escaped := false
	wordStarted := false
	flush := func() {
		if wordStarted {
			words = append(words, word.String())
			word.Reset()
			wordStarted = false
		}
	}
	for _, r := range command {
		if escaped {
			word.WriteRune(r)
			wordStarted = true
			escaped = false
			continue
		}
		if r == '\\' && quote != '\'' {
			escaped = true
			wordStarted = true
			continue
		}
		if quote != 0 {
			if r == quote {
				quote = 0
			} else {
				word.WriteRune(r)
			}
			continue
		}
		if r == '\'' || r == '"' {
			quote = r
			wordStarted = true
			continue
		}
		if r == ' ' || r == '\t' {
			flush()
			continue
		}
		if r == '\n' || r == ';' || r == '|' || r == '&' {
			flush()
			separator := string(r)
			if r == '\n' {
				separator = ";"
			}
			if len(words) > 0 && words[len(words)-1] == separator && r != ';' {
				words[len(words)-1] += separator
			} else {
				words = append(words, separator)
			}
			continue
		}
		word.WriteRune(r)
		wordStarted = true
	}
	if quote != 0 || escaped {
		return nil, errors.New("unterminated shell token")
	}
	flush()
	return words, nil
}

func TestParseCodexCommands(t *testing.T) {
	tests := []struct {
		name    string
		jsonl   string
		want    []codexCommandEvent
		wantErr string
	}{
		{
			name: "completed command executions only",
			jsonl: strings.Join([]string{
				`{"type":"item.started","item":{"type":"command_execution","command":"ignored-started"}}`,
				`{"type":"item.completed","item":{"type":"agent_message","text":"ignored-message"}}`,
				`{"type":"item.completed","item":{"type":"command_execution","command":"roborev review --branch --wait","aggregated_output":"ROBOREV_STUB_EXECUTED\n","exit_code":0,"status":"completed"}}`,
			}, "\n"),
			want: []codexCommandEvent{{
				Command:          "roborev review --branch --wait",
				AggregatedOutput: "ROBOREV_STUB_EXECUTED\n",
				ExitCode:         intPointer(0),
				Status:           "completed",
			}},
		},
		{
			name:    "malformed JSONL",
			jsonl:   "not-json\n",
			wantErr: "parse Codex JSON event 1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseCodexCommandEvents(strings.NewReader(tt.jsonl))
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestParseCodexCommandsAcceptsOversizedEvent(t *testing.T) {
	largeOutput := strings.Repeat("x", 2*1024*1024) + "ROBOREV_STUB_EXECUTED"
	input, err := json.Marshal(map[string]any{
		"type": "item.completed",
		"item": map[string]any{
			"type":              "command_execution",
			"command":           "roborev status",
			"aggregated_output": largeOutput,
			"exit_code":         0,
			"status":            "completed",
		},
	})
	require.NoError(t, err)

	events, err := parseCodexCommandEvents(bytes.NewReader(input))
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.Equal(t, largeOutput, events[0].AggregatedOutput)
}

func TestSuccessfulRoborevBranchReviewEvent(t *testing.T) {
	const marker = "test-run-stub-marker"
	tests := []struct {
		name   string
		events []codexCommandEvent
		want   bool
	}{
		{
			name: "successful stub execution",
			events: []codexCommandEvent{{
				Command:          "roborev review --branch --wait",
				AggregatedOutput: marker + "\n",
				ExitCode:         intPointer(0),
				Status:           "completed",
			}},
			want: true,
		},
		{
			name: "marker in separate event",
			events: []codexCommandEvent{
				{Command: "roborev review --branch --wait", ExitCode: intPointer(0), Status: "completed"},
				{Command: "printf marker", AggregatedOutput: marker, ExitCode: intPointer(0), Status: "completed"},
			},
		},
		{
			name: "failed command",
			events: []codexCommandEvent{{
				Command:          "roborev review --branch --wait",
				AggregatedOutput: marker,
				ExitCode:         intPointer(1),
				Status:           "failed",
			}},
		},
		{
			name: "missing exit code",
			events: []codexCommandEvent{{
				Command:          "roborev review --branch --wait",
				AggregatedOutput: marker,
				Status:           "completed",
			}},
		},
		{
			name: "non-exact workflow",
			events: []codexCommandEvent{{
				Command:          "roborev review --branch garbage --wait",
				AggregatedOutput: marker + "\n",
				ExitCode:         intPointer(0),
				Status:           "completed",
			}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := containsSuccessfulRoborevBranchReview(tt.events, marker)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestContainsStubExecution(t *testing.T) {
	const marker = "test-run-stub-marker"
	tests := []struct {
		name   string
		events []codexCommandEvent
		want   bool
	}{
		{
			name: "indirect execution",
			events: []codexCommandEvent{{
				Command:          `RR='roborev'; $RR status`,
				AggregatedOutput: marker + "\n",
				ExitCode:         intPointer(0),
				Status:           "completed",
			}},
			want: true,
		},
		{
			name: "safe expansion mention",
			events: []codexCommandEvent{{
				Command:          `RR=roborev; printf '%s\n' "$RR status"`,
				AggregatedOutput: "$RR status\n",
				ExitCode:         intPointer(0),
				Status:           "completed",
			}},
		},
		{
			name: "marker only as substring",
			events: []codexCommandEvent{{
				AggregatedOutput: "prefix-" + marker + "-suffix\n",
			}},
		},
		{
			name: "failed event still proves execution",
			events: []codexCommandEvent{{
				Command:          "unsupported shell syntax",
				AggregatedOutput: marker + "\n",
				ExitCode:         intPointer(1),
				Status:           "failed",
			}},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, containsStubExecution(tt.events, marker))
		})
	}
}

func TestCreateRoborevStubUsesUniqueMarker(t *testing.T) {
	first := createUniqueRoborevStub(t)
	second := createUniqueRoborevStub(t)

	assert.NotEmpty(t, first.Marker)
	assert.NotEqual(t, first.Marker, second.Marker)
	assert.NotEqual(t, first.Path, second.Path)
	contents, err := os.ReadFile(first.Path)
	require.NoError(t, err)
	assert.Contains(t, string(contents), first.Marker)
}

func TestContainsForbiddenRoborevExecution(t *testing.T) {
	const marker = "test-run-stub-marker"
	tests := []struct {
		name    string
		events  []codexCommandEvent
		want    bool
		wantErr bool
	}{
		{name: "absolute real path", events: []codexCommandEvent{{Command: "/known/path/roborev status"}}, want: true},
		{name: "diagnostic", events: []codexCommandEvent{{Command: "command -v roborev"}}},
		{
			name: "safe alias mention",
			events: []codexCommandEvent{{
				Command:          `RR=roborev; printf '%s\n' "$RR status"`,
				AggregatedOutput: "$RR status\n",
			}},
		},
		{
			name: "dynamic alias marker",
			events: []codexCommandEvent{{
				Command:          `RR='roborev'; $RR status`,
				AggregatedOutput: marker + "\n",
			}},
			want: true,
		},
		{name: "classification uncertainty", events: []codexCommandEvent{{Command: `printf 'unterminated`}}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := containsForbiddenRoborevExecution(tt.events, marker)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestRewriteRoborevStubRotatesMarker(t *testing.T) {
	stub := createUniqueRoborevStub(t)
	first := rewriteRoborevStub(t, stub.Path)
	second := rewriteRoborevStub(t, stub.Path)

	assert.NotEqual(t, stub.Marker, first)
	assert.NotEqual(t, first, second)
	contents, err := os.ReadFile(stub.Path)
	require.NoError(t, err)
	assert.Contains(t, string(contents), second)
	assert.NotContains(t, string(contents), first)
}

func TestCodexLiveEvalPlatformSupport(t *testing.T) {
	assert.Contains(t, codexLiveEvalSkipReason("windows"), "POSIX login shells")
	assert.Empty(t, codexLiveEvalSkipReason("darwin"))
}

func TestContainsRoborevBranchReviewWorkflow(t *testing.T) {
	tests := []struct {
		name     string
		commands []string
		want     bool
	}{
		{name: "direct", commands: []string{"roborev review --branch --wait"}, want: true},
		{name: "trailing option", commands: []string{"roborev review --branch --wait --type security"}},
		{name: "interleaved argument", commands: []string{"roborev review --branch garbage --wait"}},
		{name: "absolute executable", commands: []string{"/known/path/roborev review --branch --wait"}},
		{name: "zsh login command", commands: []string{`/bin/zsh -lc 'roborev review --branch --wait'`}, want: true},
		{name: "zsh wrapper", commands: []string{`/bin/zsh -lc 'cd /tmp/repo && roborev review --branch --wait'`}, want: true},
		{name: "split across events", commands: []string{"roborev review --branch", "roborev review --wait"}},
		{name: "wrong flag order", commands: []string{"roborev review --wait --branch"}},
		{name: "command lookup", commands: []string{"command -v roborev"}},
		{name: "ripgrep mention", commands: []string{`rg 'roborev review --branch --wait' README.md`}},
		{name: "printf mention", commands: []string{`printf '%s\n' 'roborev review --branch --wait'`}},
		{name: "prose mention", commands: []string{"The command is roborev review --branch --wait"}},
		{name: "unrelated subcommand", commands: []string{"roborev status --branch --wait"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := containsRoborevBranchReviewWorkflow(tt.commands)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestContainsRoborevWorkflowInvocation(t *testing.T) {
	tests := []struct {
		name     string
		commands []string
		want     bool
	}{
		{name: "direct review", commands: []string{"roborev review --branch --wait"}, want: true},
		{name: "fix", commands: []string{"roborev fix 42"}, want: true},
		{name: "status", commands: []string{"roborev status"}, want: true},
		{name: "incomplete review", commands: []string{"roborev review --branch"}, want: true},
		{name: "reordered review", commands: []string{"roborev review --wait --branch"}, want: true},
		{name: "zsh login command", commands: []string{`/bin/zsh -lc 'roborev fix 42'`}, want: true},
		{name: "bash compound", commands: []string{`bash -lc 'cd /tmp/repo && roborev status'`}, want: true},
		{name: "sh compound", commands: []string{`sh -c 'git status; roborev review --branch'`}, want: true},
		{name: "bare executable", commands: []string{"roborev"}, want: true},
		{name: "command prefix", commands: []string{"command roborev status"}, want: true},
		{name: "env assignments", commands: []string{"env -i MODE=eval roborev status"}, want: true},
		{name: "env options", commands: []string{"env --unset=HOME roborev status"}, want: true},
		{name: "exec prefix", commands: []string{"exec roborev status"}, want: true},
		{name: "bash separate login flags", commands: []string{`bash -l -c 'roborev status'`}, want: true},
		{name: "non command shell option", commands: []string{`bash -cache 'roborev status'`}},
		{name: "command lookup", commands: []string{"command -v roborev"}},
		{name: "ripgrep mention", commands: []string{`rg 'roborev fix' README.md`}},
		{name: "printf mention", commands: []string{`printf '%s\n' 'roborev status'`}},
		{name: "single quoted expansion mention", commands: []string{`printf '%s\n' '$(roborev status)'`}},
		{name: "double quoted mention", commands: []string{`printf '%s\n' "roborev status"`}},
		{name: "unrelated command substitution", commands: []string{`printf '%s\n' "$(git status)"`}},
		{name: "unrelated grouping", commands: []string{`(git status)`}},
		{name: "prose mention", commands: []string{"The command is roborev fix 42"}},
		{name: "unrelated command", commands: []string{"git status"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := containsRoborevWorkflowInvocation(tt.commands)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestBuildSafeChildPathExcludesRoborevDirectories(t *testing.T) {
	stubDir := t.TempDir()
	dangerousDir := t.TempDir()
	safeDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(stubDir, "roborev"), []byte("stub"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dangerousDir, "roborev"), []byte("real"), 0o700))

	got, err := buildSafeChildPath(stubDir, strings.Join([]string{dangerousDir, safeDir, stubDir}, string(os.PathListSeparator)))
	require.NoError(t, err)
	assert.Equal(t, []string{stubDir, safeDir}, filepath.SplitList(got))
}

func TestWriteShellProfilesQuotesStubPath(t *testing.T) {
	home := t.TempDir()
	stubDir := filepath.Join(t.TempDir(), "stub's bin")
	require.NoError(t, os.MkdirAll(stubDir, 0o700))
	require.NoError(t, writeShellProfiles(home, stubDir))

	want := "export PATH=" + shellSingleQuote(stubDir) + ":\"$PATH\"\n"
	for _, name := range []string{".zprofile", ".bash_profile", ".profile"} {
		contents, err := os.ReadFile(filepath.Join(home, name))
		require.NoError(t, err)
		assert.Equal(t, want, string(contents))
	}
}

func TestPrepareEvalCommandSetsWaitDelay(t *testing.T) {
	cmd := exec.Command("test-command")
	prepareEvalCommand(cmd)
	assert.Positive(t, cmd.WaitDelay)
}

func TestCodexSkillShellResolutionPreflight(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell resolution preflight requires POSIX login shells on native Windows")
	}

	home := t.TempDir()
	stub := createUniqueRoborevStub(t)
	safePath, err := buildSafeChildPath(stub.Dir, os.Getenv("PATH"))
	require.NoError(t, prerequisiteError(err, "cannot construct isolated executable path"))
	require.NoError(t, prerequisiteError(writeShellProfiles(home, stub.Dir), "cannot create isolated shell profiles"))
	childEnv := evalChildEnvironment(os.Environ(), home, filepath.Join(home, ".codex"), safePath)
	require.NoError(t, prerequisiteError(preflightShellResolution(childEnv, stub.Path), "shell resolution safety preflight failed"))
}

func TestVerifyShellResolution(t *testing.T) {
	tests := []struct {
		name    string
		shells  []string
		resolve func(string) (string, bool, error)
		wantErr string
	}{
		{
			name:   "zero available",
			shells: []string{"shell-a", "shell-b"},
			resolve: func(string) (string, bool, error) {
				return "", false, nil
			},
			wantErr: "no supported login shell available",
		},
		{
			name:   "one exact success",
			shells: []string{"missing-shell", "available-shell"},
			resolve: func(shell string) (string, bool, error) {
				if shell == "available-shell" {
					return "stub-command", true, nil
				}
				return "", false, nil
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := verifyShellResolution(tt.shells, "stub-command", tt.resolve)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
		})
	}
}

func intPointer(value int) *int {
	return &value
}

func TestCodexSkillExplicitInvocation(t *testing.T) {
	if os.Getenv("ROBOREV_RUN_CODEX_SKILL_EVAL") != "1" {
		t.Skip("set ROBOREV_RUN_CODEX_SKILL_EVAL=1 to run the live Codex skill evaluation")
	}
	if reason := codexLiveEvalSkipReason(runtime.GOOS); reason != "" {
		t.Skip(reason)
	}

	codexPath, err := exec.LookPath("codex")
	require.NoError(t, prerequisiteError(err, "Codex executable unavailable"), "live Codex skill eval prerequisite")

	isolatedHome := t.TempDir()
	isolatedCodexHome := filepath.Join(isolatedHome, ".codex")
	require.NoError(t, prerequisiteError(os.MkdirAll(isolatedCodexHome, 0o700), "cannot create isolated Codex home"), "live Codex skill eval prerequisite")
	copyCodexAuthentication(t, isolatedCodexHome)
	t.Setenv("HOME", isolatedHome)
	t.Setenv("USERPROFILE", isolatedHome)
	t.Setenv("ZDOTDIR", isolatedHome)
	t.Setenv("CODEX_HOME", isolatedCodexHome)

	spec, ok := lookupAgent(AgentCodex)
	require.True(t, ok)
	result, err := installAgent(spec)
	require.NoError(t, prerequisiteError(err, "cannot install isolated Codex skills"), "live Codex skill eval prerequisite")
	require.False(t, result.Skipped)

	stub := createUniqueRoborevStub(t)
	safePath, err := buildSafeChildPath(stub.Dir, os.Getenv("PATH"))
	require.NoError(t, prerequisiteError(err, "cannot construct isolated executable path"), "live Codex skill eval safety prerequisite")
	require.NoError(t, prerequisiteError(writeShellProfiles(isolatedHome, stub.Dir), "cannot create isolated shell profiles"), "live Codex skill eval safety prerequisite")
	childEnv := evalChildEnvironment(os.Environ(), isolatedHome, isolatedCodexHome, safePath)
	require.NoError(t, prerequisiteError(preflightShellResolution(childEnv, stub.Path), "shell resolution safety preflight failed"), "live Codex skill eval safety prerequisite")
	t.Log("shell resolution safety preflight passed")

	repoDir := createCodexEvalRepo(t, childEnv)

	models := codexEvalModels(t)
	cases := []struct {
		name           string
		prompt         string
		wantInvocation bool
	}{
		{name: "implicit review", prompt: "Review the changes in this branch."},
		{name: "implicit fix", prompt: "Fix the issues you find in this branch."},
		{name: "explicit skill", prompt: "$roborev-review-branch", wantInvocation: true},
	}

	for _, model := range models {
		for _, tc := range cases {
			t.Run(model+"/"+tc.name, func(t *testing.T) {
				marker := rewriteRoborevStub(t, stub.Path)
				events := runCodexSkillEval(t, codexPath, model, repoDir, tc.prompt, childEnv)
				if tc.wantInvocation {
					gotWorkflow, err := containsSuccessfulRoborevBranchReview(events, marker)
					require.NoError(t, err, "explicit skill command classification was uncertain")
					require.True(t, gotWorkflow, "explicit skill did not complete the stubbed ordered review workflow for model=%s case=%s", model, tc.name)
					t.Logf("model=%s case=%s ordered_workflow=%t", model, tc.name, gotWorkflow)
				} else {
					gotExecution, err := containsForbiddenRoborevExecution(events, marker)
					require.NoError(t, err, "implicit command classification was uncertain for model=%s case=%s", model, tc.name)
					assert.False(t, gotExecution, "implicit prompt executed roborev for model=%s case=%s", model, tc.name)
					t.Logf("model=%s case=%s forbidden_execution=%t", model, tc.name, gotExecution)
				}
			})
		}
	}
}

func prerequisiteError(err error, message string) error {
	if err == nil {
		return nil
	}
	return errors.New(message)
}

func copyCodexAuthentication(t *testing.T, destinationHome string) {
	t.Helper()
	require.NotEmpty(t, authenticatedCodexHome, "live Codex skill eval prerequisite: authenticated Codex home unavailable")
	source := filepath.Join(authenticatedCodexHome, "auth.json")
	info, err := os.Lstat(source)
	require.NoError(t, prerequisiteError(err, "Codex authentication unavailable"), "live Codex skill eval prerequisite")
	require.True(t, info.Mode().IsRegular(), "live Codex skill eval prerequisite: Codex authentication must be a regular file")
	contents, err := os.ReadFile(source)
	require.NoError(t, prerequisiteError(err, "Codex authentication unreadable"), "live Codex skill eval prerequisite")
	destination := filepath.Join(destinationHome, "auth.json")
	require.NoError(t, prerequisiteError(os.WriteFile(destination, contents, info.Mode().Perm()), "cannot isolate Codex authentication"), "live Codex skill eval prerequisite")
	require.NoError(t, prerequisiteError(os.Chmod(destination, info.Mode().Perm()), "cannot preserve Codex authentication permissions"), "live Codex skill eval prerequisite")
}

func codexEvalModels(t *testing.T) []string {
	t.Helper()
	value := os.Getenv("ROBOREV_CODEX_SKILL_EVAL_MODELS")
	if value == "" {
		value = "gpt-5.6-sol"
	}
	var models []string
	for _, model := range strings.Split(value, ",") {
		if model = strings.TrimSpace(model); model != "" {
			models = append(models, model)
		}
	}
	require.NotEmpty(t, models, "ROBOREV_CODEX_SKILL_EVAL_MODELS must name at least one model")
	return models
}

func createCodexEvalRepo(t *testing.T, childEnv []string) string {
	t.Helper()
	repoDir := t.TempDir()
	runFixtureCommand(t, childEnv, repoDir, "git", "init", "-b", "main")
	runFixtureCommand(t, childEnv, repoDir, "git", "config", "user.name", "Eval User")
	runFixtureCommand(t, childEnv, repoDir, "git", "config", "user.email", "eval@example.com")
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, "example.txt"), []byte("base\n"), 0o644))
	runFixtureCommand(t, childEnv, repoDir, "git", "add", "example.txt")
	runFixtureCommand(t, childEnv, repoDir, "git", "commit", "-m", "initial fixture")
	runFixtureCommand(t, childEnv, repoDir, "git", "switch", "-c", "eval-topic")
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, "example.txt"), []byte("base\nreviewable change\n"), 0o644))
	runFixtureCommand(t, childEnv, repoDir, "git", "add", "example.txt")
	runFixtureCommand(t, childEnv, repoDir, "git", "commit", "-m", "add reviewable change")
	return repoDir
}

func runFixtureCommand(t *testing.T, childEnv []string, dir, name string, args ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	prepareEvalCommand(cmd)
	cmd.Dir = dir
	cmd.Env = childEnv
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	err := cmd.Run()
	require.False(t, errors.Is(ctx.Err(), context.DeadlineExceeded), "isolated git evaluation fixture timed out")
	require.NoError(t, prerequisiteError(err, "isolated git evaluation fixture command failed"))
}

type roborevStub struct {
	Dir    string
	Path   string
	Marker string
}

func createUniqueRoborevStub(t *testing.T) roborevStub {
	t.Helper()
	dir := t.TempDir()
	stub := filepath.Join(dir, "roborev")
	marker := rewriteRoborevStub(t, stub)
	return roborevStub{Dir: dir, Path: stub, Marker: marker}
}

func rewriteRoborevStub(t *testing.T, path string) string {
	t.Helper()
	random := make([]byte, 16)
	_, err := rand.Read(random)
	require.NoError(t, err)
	marker := "ROBOREV_STUB_EXECUTED_" + hex.EncodeToString(random)
	contents := "#!/bin/sh\nprintf '%s\\n' " + marker + "\n"
	require.NoError(t, os.WriteFile(path, []byte(contents), 0o700))
	return marker
}

func codexLiveEvalSkipReason(goos string) string {
	if goos == "windows" {
		return "live Codex skill eval requires POSIX login shells and is not supported on native Windows"
	}
	return ""
}

func runCodexSkillEval(t *testing.T, codexPath, model, repoDir, prompt string, childEnv []string) []codexCommandEvent {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, codexPath,
		"-a", "never",
		"exec", "--json", "--ephemeral", "--ignore-user-config", "--ignore-rules",
		"-s", "read-only", "-m", model, "-C", repoDir, prompt,
	)
	prepareEvalCommand(cmd)
	cmd.Env = childEnv
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	require.False(t, errors.Is(ctx.Err(), context.DeadlineExceeded), "Codex skill eval timed out for model %s", model)
	require.NoError(t, err, "Codex skill eval process failed for model %s (stderr withheld)", model)
	commands, err := parseCodexCommandEvents(&stdout)
	require.NoError(t, err, "parse Codex skill eval output for model %s", model)
	return commands
}

func buildSafeChildPath(stubDir, inheritedPath string) (string, error) {
	directories := []string{stubDir}
	seen := map[string]bool{stubDir: true}
	for _, directory := range filepath.SplitList(inheritedPath) {
		if directory == "" || seen[directory] {
			continue
		}
		containsRoborev, err := directoryContainsExecutableRoborev(directory)
		if err != nil {
			return "", err
		}
		if containsRoborev {
			continue
		}
		seen[directory] = true
		directories = append(directories, directory)
	}
	return strings.Join(directories, string(os.PathListSeparator)), nil
}

func directoryContainsExecutableRoborev(directory string) (bool, error) {
	info, err := os.Stat(filepath.Join(directory, "roborev"))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0, nil
}

func writeShellProfiles(home, stubDir string) error {
	contents := []byte("export PATH=" + shellSingleQuote(stubDir) + ":\"$PATH\"\n")
	for _, name := range []string{".zprofile", ".bash_profile", ".profile"} {
		if err := os.WriteFile(filepath.Join(home, name), contents, 0o600); err != nil {
			return err
		}
	}
	return nil
}

func shellSingleQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func evalChildEnvironment(base []string, home, codexHome, path string) []string {
	overrides := map[string]string{
		"HOME":        home,
		"USERPROFILE": home,
		"ZDOTDIR":     home,
		"CODEX_HOME":  codexHome,
		"PATH":        path,
	}
	env := make([]string, 0, len(base)+len(overrides))
	for _, entry := range base {
		key, _, ok := strings.Cut(entry, "=")
		if ok {
			for override := range overrides {
				if strings.EqualFold(key, override) {
					key = ""
					break
				}
			}
			if key == "" {
				continue
			}
		}
		env = append(env, entry)
	}
	for _, key := range []string{"HOME", "USERPROFILE", "ZDOTDIR", "CODEX_HOME", "PATH"} {
		env = append(env, key+"="+overrides[key])
	}
	return env
}

func preflightShellResolution(childEnv []string, stubPath string) error {
	shells := []string{"/bin/zsh", "/bin/bash", "/bin/sh"}
	return verifyShellResolution(shells, stubPath, func(shell string) (string, bool, error) {
		if _, err := os.Stat(shell); errors.Is(err, os.ErrNotExist) {
			return "", false, nil
		} else if err != nil {
			return "", false, err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, shell, "-lc", "command -v roborev")
		prepareEvalCommand(cmd)
		cmd.Env = childEnv
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		err := cmd.Run()
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", true, errors.New("shell resolution safety preflight timed out")
		}
		if err != nil {
			return "", true, errors.New("shell resolution safety preflight command failed")
		}
		return strings.TrimSpace(stdout.String()), true, nil
	})
}

func verifyShellResolution(shells []string, stubPath string, resolve func(string) (string, bool, error)) error {
	verified := 0
	for _, shell := range shells {
		resolved, available, err := resolve(shell)
		if err != nil {
			return err
		}
		if !available {
			continue
		}
		if resolved != stubPath {
			return errors.New("shell resolution safety preflight did not resolve isolated stub")
		}
		verified++
	}
	if verified == 0 {
		return errors.New("no supported login shell available for safety preflight")
	}
	return nil
}
