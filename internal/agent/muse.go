package agent

import (
	"context"
	"io"
	"strings"
)

// MuseAgent runs Meta's Muse Code CLI in headless mode.
type MuseAgent struct {
	Command   string
	Model     string
	Reasoning ReasoningLevel
	Agentic   bool
}

func NewMuseAgent(command string) *MuseAgent {
	if command == "" {
		command = "muse"
	}
	return &MuseAgent{Command: command, Reasoning: ReasoningStandard}
}

func (a *MuseAgent) clone(opts ...agentCloneOption) *MuseAgent {
	cfg := newAgentCloneConfig(a.Command, a.Model, a.Reasoning, a.Agentic, "", opts...)
	return &MuseAgent{Command: cfg.Command, Model: cfg.Model, Reasoning: cfg.Reasoning, Agentic: cfg.Agentic}
}

func (a *MuseAgent) WithReasoning(level ReasoningLevel) Agent {
	return a.clone(withClonedReasoning(level))
}

func (a *MuseAgent) WithAgentic(agentic bool) Agent {
	return a.clone(withClonedAgentic(agentic))
}

func (a *MuseAgent) WithModel(model string) Agent {
	if model == "" {
		return a
	}
	return a.clone(withClonedModel(model))
}

func (a *MuseAgent) Name() string        { return "muse" }
func (a *MuseAgent) CommandName() string { return a.Command }

func (a *MuseAgent) buildArgs(agentic bool, promptFile string) []string {
	args := []string{"exec", "--no-session-log", "--contributor-mode", "off"}
	if a.Model != "" {
		args = append(args, "--model", a.Model)
	}
	if effort := reasoningEffort(a.Reasoning); effort != "" {
		args = append(args, "--reasoning-effort", effort)
	}
	if agentic {
		args = append(args, "--yolo")
	} else {
		args = append(args, "-c", `run.toolset=["read_file","search"]`, "-c", `hooks={}`)
	}
	return append(args, "--prompt-file", promptFile)
}

func (a *MuseAgent) CommandLine() string {
	return a.Command + " " + strings.Join(a.buildArgs(a.Agentic || AllowUnsafeAgents(), "<prompt-file>"), " ")
}

func (a *MuseAgent) PlanningCommandLine() string {
	return a.Command + " " + strings.Join(a.buildArgs(false, "<prompt-file>"), " ")
}

func (a *MuseAgent) Review(ctx context.Context, repoPath, commitSHA, prompt string, output io.Writer) (string, error) {
	// Muse requires a real file for prompts that exceed argv limits.
	promptPath, cleanup, err := createPromptFile("muse", prompt)
	if err != nil {
		return "", err
	}
	defer cleanup()
	result, err := runStreamingCLI(ctx, streamingCLISpec{
		Name: "muse", Command: a.Command,
		Args: a.buildArgs(effectiveAgentic(ctx, a.Agentic), promptPath),
		Dir:  repoPath, Output: output, StreamStderr: true, DrainStdout: true,
		Parse: parseAntigravityOutput,
	})
	if err != nil {
		return "", err
	}
	if result.WaitErr != nil {
		return "", formatStreamingCLIWaitError("muse", result, result.Stderr)
	}
	if result.ParseErr != nil {
		return result.Result, result.ParseErr
	}
	if result.Result == "" {
		return "No review output generated", nil
	}
	return result.Result, nil
}

func init() { Register(NewMuseAgent("")) }
