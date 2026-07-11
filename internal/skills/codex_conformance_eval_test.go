//go:build codexeval

package skills

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func parseCodexCommands(r io.Reader) ([]string, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	var commands []string
	for line := 1; scanner.Scan(); line++ {
		var event struct {
			Type string `json:"type"`
			Item struct {
				Type    string `json:"type"`
				Command string `json:"command"`
			} `json:"item"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			return nil, fmt.Errorf("parse Codex JSONL line %d: %w", line, err)
		}
		if event.Type == "item.completed" && event.Item.Type == "command_execution" {
			commands = append(commands, event.Item.Command)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read Codex JSONL: %w", err)
	}
	return commands, nil
}

func containsRoborevBranchReviewWorkflow(commands []string) bool {
	for _, command := range commands {
		if commandContainsRoborevBranchReview(command) {
			return true
		}
	}
	return false
}

func containsRoborevWorkflowInvocation(commands []string) bool {
	for _, command := range commands {
		if commandContainsRoborevInvocation(command) {
			return true
		}
	}
	return false
}

func commandContainsRoborevInvocation(command string) bool {
	tokens, ok := shellWords(command)
	if !ok {
		return false
	}
	for start := 0; start < len(tokens); {
		end := start
		for end < len(tokens) && !isShellSeparator(tokens[end]) {
			end++
		}
		if simpleCommandContainsRoborevInvocation(tokens[start:end]) {
			return true
		}
		start = end + 1
	}
	return false
}

func simpleCommandContainsRoborevInvocation(tokens []string) bool {
	for len(tokens) > 0 && strings.Contains(tokens[0], "=") && !strings.HasPrefix(tokens[0], "=") {
		tokens = tokens[1:]
	}
	if len(tokens) == 0 {
		return false
	}
	executable := filepath.Base(tokens[0])
	if (executable == "zsh" || executable == "sh" || executable == "bash") && len(tokens) >= 3 && strings.Contains(tokens[1], "c") {
		return commandContainsRoborevInvocation(tokens[2])
	}
	return executable == "roborev" && len(tokens) >= 2
}

func commandContainsRoborevBranchReview(command string) bool {
	tokens, ok := shellWords(command)
	if !ok {
		return false
	}
	for start := 0; start < len(tokens); {
		end := start
		for end < len(tokens) && !isShellSeparator(tokens[end]) {
			end++
		}
		if simpleCommandContainsWorkflow(tokens[start:end]) {
			return true
		}
		start = end + 1
	}
	return false
}

func simpleCommandContainsWorkflow(tokens []string) bool {
	for len(tokens) > 0 && strings.Contains(tokens[0], "=") && !strings.HasPrefix(tokens[0], "=") {
		tokens = tokens[1:]
	}
	if len(tokens) == 0 {
		return false
	}
	executable := filepath.Base(tokens[0])
	if (executable == "zsh" || executable == "sh" || executable == "bash") && len(tokens) >= 3 && strings.Contains(tokens[1], "c") {
		return commandContainsRoborevBranchReview(tokens[2])
	}
	if executable != "roborev" || len(tokens) < 4 || tokens[1] != "review" {
		return false
	}
	branch := -1
	for i := 2; i < len(tokens); i++ {
		if tokens[i] == "--branch" {
			branch = i
		}
		if tokens[i] == "--wait" && branch >= 0 && branch < i {
			return true
		}
	}
	return false
}

func isShellSeparator(token string) bool {
	return token == "&&" || token == "||" || token == ";" || token == "|"
}

func shellWords(command string) ([]string, bool) {
	var words []string
	var word strings.Builder
	quote := rune(0)
	escaped := false
	flush := func() {
		if word.Len() > 0 {
			words = append(words, word.String())
			word.Reset()
		}
	}
	for _, r := range command {
		if escaped {
			word.WriteRune(r)
			escaped = false
			continue
		}
		if r == '\\' && quote != '\'' {
			escaped = true
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
			continue
		}
		if r == ' ' || r == '\t' || r == '\n' {
			flush()
			continue
		}
		if r == ';' || r == '|' || r == '&' {
			flush()
			separator := string(r)
			if len(words) > 0 && words[len(words)-1] == separator && r != ';' {
				words[len(words)-1] += separator
			} else {
				words = append(words, separator)
			}
			continue
		}
		word.WriteRune(r)
	}
	if quote != 0 || escaped {
		return nil, false
	}
	flush()
	return words, true
}

func TestParseCodexCommands(t *testing.T) {
	tests := []struct {
		name    string
		jsonl   string
		want    []string
		wantErr string
	}{
		{
			name: "completed command executions only",
			jsonl: strings.Join([]string{
				`{"type":"item.started","item":{"type":"command_execution","command":"ignored-started"}}`,
				`{"type":"item.completed","item":{"type":"agent_message","text":"ignored-message"}}`,
				`{"type":"item.completed","item":{"type":"command_execution","command":"roborev review --branch --wait"}}`,
			}, "\n"),
			want: []string{"roborev review --branch --wait"},
		},
		{
			name:    "malformed JSONL",
			jsonl:   "not-json\n",
			wantErr: "parse Codex JSONL line 1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseCodexCommands(strings.NewReader(tt.jsonl))
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestContainsRoborevBranchReviewWorkflow(t *testing.T) {
	tests := []struct {
		name     string
		commands []string
		want     bool
	}{
		{name: "direct", commands: []string{"roborev review --branch --wait"}, want: true},
		{name: "direct with later options", commands: []string{"roborev review --branch --wait --type security"}, want: true},
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
			assert.Equal(t, tt.want, containsRoborevBranchReviewWorkflow(tt.commands))
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
		{name: "command lookup", commands: []string{"command -v roborev"}},
		{name: "ripgrep mention", commands: []string{`rg 'roborev fix' README.md`}},
		{name: "printf mention", commands: []string{`printf '%s\n' 'roborev status'`}},
		{name: "prose mention", commands: []string{"The command is roborev fix 42"}},
		{name: "bare executable", commands: []string{"roborev"}},
		{name: "unrelated command", commands: []string{"git status"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, containsRoborevWorkflowInvocation(tt.commands))
		})
	}
}

func TestCodexSkillExplicitInvocation(t *testing.T) {
	if os.Getenv("ROBOREV_RUN_CODEX_SKILL_EVAL") != "1" {
		t.Skip("set ROBOREV_RUN_CODEX_SKILL_EVAL=1 to run the live Codex skill evaluation")
	}

	codexPath, err := exec.LookPath("codex")
	require.NoError(t, prerequisiteError(err, "Codex executable unavailable"), "live Codex skill eval prerequisite")

	isolatedHome := t.TempDir()
	isolatedCodexHome := filepath.Join(isolatedHome, ".codex")
	require.NoError(t, os.MkdirAll(isolatedCodexHome, 0o700))
	copyCodexAuthentication(t, isolatedCodexHome)
	t.Setenv("HOME", isolatedHome)
	t.Setenv("USERPROFILE", isolatedHome)
	t.Setenv("CODEX_HOME", isolatedCodexHome)

	spec, ok := lookupAgent(AgentCodex)
	require.True(t, ok)
	result, err := installAgent(spec)
	require.NoError(t, err)
	require.False(t, result.Skipped)

	repoDir := createCodexEvalRepo(t, isolatedHome)
	stubDir := createRoborevStub(t)
	t.Setenv("PATH", stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))

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
				commands := runCodexSkillEval(t, codexPath, model, repoDir, tc.prompt)
				safeCommands := sanitizeEvalCommands(commands, isolatedHome, repoDir, stubDir, authenticatedCodexHome)
				if tc.wantInvocation {
					gotWorkflow := containsRoborevBranchReviewWorkflow(commands)
					require.True(t, gotWorkflow, "explicit skill did not execute ordered roborev review --branch --wait workflow; commands=%q", safeCommands)
					t.Logf("model=%s case=%s ordered_workflow=%t", model, tc.name, gotWorkflow)
				} else {
					gotInvocation := containsRoborevWorkflowInvocation(commands)
					assert.False(t, gotInvocation, "implicit prompt executed a roborev command; commands=%q", safeCommands)
					t.Logf("model=%s case=%s roborev_invocation=%t", model, tc.name, gotInvocation)
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

func createCodexEvalRepo(t *testing.T, home string) string {
	t.Helper()
	repoDir := t.TempDir()
	runFixtureCommand(t, home, repoDir, "git", "init", "-b", "main")
	runFixtureCommand(t, home, repoDir, "git", "config", "user.name", "Eval User")
	runFixtureCommand(t, home, repoDir, "git", "config", "user.email", "eval@example.com")
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, "example.txt"), []byte("base\n"), 0o644))
	runFixtureCommand(t, home, repoDir, "git", "add", "example.txt")
	runFixtureCommand(t, home, repoDir, "git", "commit", "-m", "initial fixture")
	runFixtureCommand(t, home, repoDir, "git", "switch", "-c", "eval-topic")
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, "example.txt"), []byte("base\nreviewable change\n"), 0o644))
	runFixtureCommand(t, home, repoDir, "git", "add", "example.txt")
	runFixtureCommand(t, home, repoDir, "git", "commit", "-m", "add reviewable change")
	return repoDir
}

func runFixtureCommand(t *testing.T, home, dir, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "HOME="+home, "USERPROFILE="+home)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	require.NoError(t, cmd.Run(), "create isolated git evaluation fixture")
}

func createRoborevStub(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	stub := filepath.Join(dir, "roborev")
	contents := "#!/bin/sh\nprintf '%s\\n' ROBOREV_STUB_EXECUTED\n"
	require.NoError(t, os.WriteFile(stub, []byte(contents), 0o700))
	return dir
}

func runCodexSkillEval(t *testing.T, codexPath, model, repoDir, prompt string) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, codexPath,
		"-a", "never",
		"exec", "--json", "--ephemeral", "--ignore-user-config", "--ignore-rules",
		"-s", "read-only", "-m", model, "-C", repoDir, prompt,
	)
	cmd.Env = os.Environ()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	require.False(t, errors.Is(ctx.Err(), context.DeadlineExceeded), "Codex skill eval timed out for model %s", model)
	require.NoError(t, err, "Codex skill eval process failed for model %s (stderr withheld)", model)
	commands, err := parseCodexCommands(&stdout)
	require.NoError(t, err, "parse Codex skill eval output for model %s", model)
	return commands
}

func sanitizeEvalCommands(commands []string, privatePaths ...string) []string {
	sanitized := append([]string(nil), commands...)
	for i := range sanitized {
		for _, privatePath := range privatePaths {
			if privatePath != "" {
				sanitized[i] = strings.ReplaceAll(sanitized[i], privatePath, "<isolated-path>")
			}
		}
	}
	return sanitized
}
