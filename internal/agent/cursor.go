package agent

import (
	"context"
	"io"
	"os"
	"strings"
)

// CursorAgent runs code reviews using the Cursor agent CLI
type CursorAgent struct {
	Command   string         // The agent command to run (default: "agent")
	Model     string         // Model to use
	Reasoning ReasoningLevel // Reasoning level
	Agentic   bool           // Whether agentic mode is enabled
}

// NewCursorAgent creates a new Cursor agent
func NewCursorAgent(command string) *CursorAgent {
	if command == "" {
		command = "agent"
	}
	return &CursorAgent{Command: command, Reasoning: ReasoningStandard}
}

func (a *CursorAgent) clone(opts ...agentCloneOption) *CursorAgent {
	cfg := newAgentCloneConfig(
		a.Command,
		a.Model,
		a.Reasoning,
		a.Agentic,
		"",
		opts...,
	)
	return &CursorAgent{
		Command:   cfg.Command,
		Model:     cfg.Model,
		Reasoning: cfg.Reasoning,
		Agentic:   cfg.Agentic,
	}
}

// WithReasoning returns a copy with the reasoning level stored.
// The agent CLI has no reasoning flag; callers can map reasoning to model selection instead.
func (a *CursorAgent) WithReasoning(level ReasoningLevel) Agent {
	return a.clone(withClonedReasoning(level))
}

func (a *CursorAgent) WithAgentic(agentic bool) Agent {
	return a.clone(withClonedAgentic(agentic))
}

func (a *CursorAgent) WithModel(model string) Agent {
	if model == "" {
		return a
	}
	return a.clone(withClonedModel(model))
}

func (a *CursorAgent) Name() string {
	return "cursor"
}

func (a *CursorAgent) CommandName() string {
	return a.Command
}

func (a *CursorAgent) CommandLine() string {
	agenticMode := a.Agentic || AllowUnsafeAgents()
	return a.commandLine(agenticMode)
}

// PlanningCommandLine returns Cursor's representative read-only planning command.
func (a *CursorAgent) PlanningCommandLine() string {
	return a.commandLine(false)
}

func (a *CursorAgent) commandLine(agenticMode bool) string {
	args := a.buildArgs(agenticMode)
	return a.Command + " " + strings.Join(args, " ")
}

func (a *CursorAgent) buildArgs(agenticMode bool) []string {
	// -p enables non-interactive print mode (like Claude Code's -p flag)
	args := []string{"-p", "--output-format", "stream-json"}

	// Default to "auto" which lets the agent CLI pick a model based on the task
	model := a.Model
	if model == "" {
		model = "auto"
	}
	args = append(args, "--model", model)

	if agenticMode {
		args = append(args, "--force")
	} else {
		args = append(args, "--mode", "plan")
	}

	return args
}

func (a *CursorAgent) Review(ctx context.Context, repoPath, commitSHA, prompt string, output io.Writer) (string, error) {
	agenticMode := effectiveAgentic(ctx, a.Agentic)

	args := a.buildArgs(agenticMode)

	runResult, runErr := runStreamingCLI(ctx, streamingCLISpec{
		Name:    "cursor agent",
		Command: a.Command,
		Args:    args,
		Dir:     repoPath,
		Env:     os.Environ(),
		Stdin:   strings.NewReader(prompt),
		Output:  output,
		Parse: func(r io.Reader, sw *syncWriter) (string, error) {
			if sw == nil {
				return a.parseStreamJSON(r, nil)
			}
			return a.parseStreamJSON(r, sw)
		},
	})
	if runErr != nil {
		return "", runErr
	}

	if runResult.WaitErr != nil {
		return "", formatStreamingCLIWaitError("cursor agent", runResult, runResult.Stderr)
	}

	if runResult.ParseErr != nil {
		return "", runResult.ParseErr
	}

	if runResult.Result == "" {
		return "No review output generated", nil
	}

	return runResult.Result, nil
}

func (a *CursorAgent) parseStreamJSON(r io.Reader, output io.Writer) (string, error) {
	return parseStreamJSON(r, output)
}

func init() {
	Register(NewCursorAgent(""))
}
