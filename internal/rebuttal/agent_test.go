package rebuttal

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
)

// Catch unsafe-mode overrides at the subprocess boundary, including session resume.
func TestAssessmentReadOnlyWithUnsafeAgents(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires shell fixture")
	}
	previous := agent.AllowUnsafeAgents()
	agent.SetAllowUnsafeAgents(true)
	t.Cleanup(func() { agent.SetAllowUnsafeAgents(previous) })
	for _, name := range []string{"codex", "claude", "pi", "grok"} {
		t.Run(name, func(t *testing.T) {
			assert := assert.New(t)
			dir := t.TempDir()
			argsPath := filepath.Join(dir, "args")
			t.Setenv("ASSESSMENT_ARGS", argsPath)
			command := filepath.Join(dir, "agent")
			require.NoError(t, os.WriteFile(command, []byte(`#!/bin/sh
case "$*" in
 *--help*) echo '--sandbox --dangerously-skip-permissions --dangerously-bypass-approvals-and-sandbox'; exit 0;;
esac
printf '%s\n' "$@" > "$ASSESSMENT_ARGS"
exit 1
`), 0o700))
			session := "review-session"
			var a agent.Agent
			switch name {
			case "codex":
				a = agent.NewCodexAgent(command)
			case "claude":
				a = agent.NewClaudeAgent(command)
			case "pi":
				t.Setenv("PI_CODING_AGENT_DIR", dir)
				require.NoError(t, os.MkdirAll(filepath.Join(dir, "sessions", "repo"), 0o700))
				require.NoError(t, os.WriteFile(filepath.Join(dir, "sessions", "repo", "review_"+session+".jsonl"), []byte("{}\n"), 0o600))
				a = agent.NewPiAgent(command)
			case "grok":
				grok := agent.NewGrokAgent(command)
				grok.Sandbox = "off"
				a = grok
			}
			a = a.WithAgentic(true).(agent.SessionAgent).WithSessionID(session)
			_, err := RunAgentAssessment(t.Context(), a, dir, "HEAD", "assess", nil)
			require.Error(t, err) // Controlled executable exits after recording actual arguments.
			data, err := os.ReadFile(argsPath)
			require.NoError(t, err)
			args := string(data)
			assert.Contains(args, session)
			assert.NotContains(args, "--dangerously-skip-permissions\n")
			assert.NotContains(args, "--dangerously-bypass-approvals-and-sandbox\n")
			assert.NotContains(args, "--always-approve\n")
			switch name {
			case "codex":
				assert.Contains(args, "sandbox_mode=\"read-only\"")
			case "claude":
				assert.Contains(args, "--tools\nRead,Glob,Grep\n")
				assert.Contains(args, "--disallowedTools\nmcp__*\n")
				assert.Contains(args, "--permission-mode\nplan\n")
			case "pi":
				assert.Contains(args, "--tools\nread,grep,find,ls,json_output\n")
				assert.Contains(args, "--no-extensions\n")
			case "grok":
				assert.Contains(args, "--tools\nread_file,grep,list_dir\n")
				assert.Contains(args, "--sandbox\nread-only\n")
			}
			assert.True(agent.AllowUnsafeAgents())
			// A subsequent ordinary structured review still observes global unsafe mode.
			_, err = a.(agent.StructuredReviewAgent).ReviewWithSchema(t.Context(), dir, "HEAD", "review", []byte(`{}`), nil)
			require.Error(t, err)
			data, err = os.ReadFile(argsPath)
			require.NoError(t, err)
			ordinary := string(data)
			switch name {
			case "codex":
				assert.Contains(ordinary, "--dangerously-bypass-approvals-and-sandbox\n")
			case "claude":
				assert.Contains(ordinary, "--dangerously-skip-permissions\n")
			case "pi":
				assert.NotContains(ordinary, "--tools\n")
			case "grok":
				assert.Contains(ordinary, "--always-approve\n")
			}
		})
	}
}

// Hide the optional structured capability to exercise the ordinary fallback.
type proseOnlyAssessmentAgent struct{ agent.ReadOnlyReviewAgent }

func TestAssessmentReadOnlyFallback(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires shell fixture")
	}
	previous := agent.AllowUnsafeAgents()
	agent.SetAllowUnsafeAgents(true)
	t.Cleanup(func() { agent.SetAllowUnsafeAgents(previous) })
	dir := t.TempDir()
	argsPath := filepath.Join(dir, "args")
	t.Setenv("ASSESSMENT_ARGS", argsPath)
	command := filepath.Join(dir, "agent")
	require.NoError(t, os.WriteFile(command, []byte(`#!/bin/sh
printf '%s\n' "$@" > "$ASSESSMENT_ARGS"
printf '%s\n' '{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"{\"outcome\":\"confirmed\",\"explanation\":\"checked\"}"}]}}'
`), 0o700))
	a := proseOnlyAssessmentAgent{agent.NewPiAgent(command).WithAgentic(true).(agent.ReadOnlyReviewAgent)}
	got, err := RunAgentAssessment(t.Context(), a, dir, "HEAD", "assess", nil)
	require.NoError(t, err)
	assert.JSONEq(t, `{"outcome":"confirmed","explanation":"checked"}`, got)
	data, err := os.ReadFile(argsPath)
	require.NoError(t, err)
	assert.Contains(t, string(data), "--tools\nread,grep,find,ls\n")
	assert.Contains(t, string(data), "--no-extensions\n")
	assert.True(t, agent.AllowUnsafeAgents())
}

func TestAssessmentRejectsUnsupportedReadOnlyAgent(t *testing.T) {
	called := false
	a := &agent.FakeAgent{NameStr: "unsupported", ReviewFn: func(context.Context, string, string, string, io.Writer) (string, error) {
		called = true
		return "", nil
	}}
	_, err := RunAgentAssessment(t.Context(), a, t.TempDir(), "HEAD", "assess", nil)
	require.ErrorContains(t, err, "cannot enforce read-only assessments")
	assert.False(t, called)
}

func TestAssessmentStructuredOutputReadOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires shell fixture")
	}
	previous := agent.AllowUnsafeAgents()
	agent.SetAllowUnsafeAgents(true)
	t.Cleanup(func() { agent.SetAllowUnsafeAgents(previous) })
	for _, name := range []string{"codex", "claude", "pi", "grok"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			script := "#!/bin/sh\ncase \"$*\" in *--help*) echo '--sandbox'; exit 0;; esac\n"
			var a agent.Agent
			command := filepath.Join(dir, "agent")
			switch name {
			case "codex":
				a = agent.NewCodexAgent(command)
				script += `printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"{\"outcome\":\"confirmed\",\"explanation\":\"checked\"}"}}'`
			case "claude":
				a = agent.NewClaudeAgent(command)
				script += `printf '%s\n' '{"type":"result","subtype":"success","result":"{\"outcome\":\"confirmed\",\"explanation\":\"checked\"}"}'`
			case "grok":
				a = agent.NewGrokAgent(command)
				script += `printf '%s\n' '{"structuredOutput":{"outcome":"confirmed","explanation":"checked"}}'`
			case "pi":
				a = agent.NewPiAgent(command)
				script += `previous=""
for arg in "$@"; do
 if [ "$previous" = --json-output ]; then
  printf '%s\n' '{"outcome":"confirmed","explanation":"checked"}' > "$arg"
 fi
 previous="$arg"
done`
			}
			require.NoError(t, os.WriteFile(command, []byte(script+"\n"), 0o700))
			got, err := RunAgentAssessment(t.Context(), a, dir, "HEAD", "assess", nil)
			require.NoError(t, err)
			assert.JSONEq(t, `{"outcome":"confirmed","explanation":"checked"}`, got)
			assert.True(t, agent.AllowUnsafeAgents())
		})
	}
}
