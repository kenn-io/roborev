package agent

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// DroidAgent runs code reviews using Factory's Droid CLI
type DroidAgent struct {
	Command   string         // The droid command to run (default: "droid")
	Reasoning ReasoningLevel // Reasoning level for the agent
	Agentic   bool           // Whether agentic mode is enabled (allow file edits)
}

// NewDroidAgent creates a new Droid agent with standard reasoning
func NewDroidAgent(command string) *DroidAgent {
	if command == "" {
		command = "droid"
	}
	return &DroidAgent{Command: command, Reasoning: ReasoningStandard}
}

func (a *DroidAgent) clone(opts ...agentCloneOption) *DroidAgent {
	cfg := newAgentCloneConfig(
		a.Command,
		"",
		a.Reasoning,
		a.Agentic,
		"",
		opts...,
	)
	return &DroidAgent{
		Command:   cfg.Command,
		Reasoning: cfg.Reasoning,
		Agentic:   cfg.Agentic,
	}
}

// WithReasoning returns a copy of the agent with the specified reasoning level
func (a *DroidAgent) WithReasoning(level ReasoningLevel) Agent {
	return a.clone(withClonedReasoning(level))
}

// WithAgentic returns a copy of the agent configured for agentic mode.
func (a *DroidAgent) WithAgentic(agentic bool) Agent {
	return a.clone(withClonedAgentic(agentic))
}

// WithModel returns the agent unchanged (model selection not supported for droid).
func (a *DroidAgent) WithModel(model string) Agent {
	return a
}

// droidReasoningEffort maps ReasoningLevel to droid-specific effort values
func (a *DroidAgent) droidReasoningEffort() string {
	switch a.Reasoning {
	case ReasoningMaximum, ReasoningThorough, ReasoningHigh:
		return "high"
	case ReasoningMedium:
		return "medium"
	case ReasoningFast, ReasoningLow:
		return "low"
	default:
		return "" // use droid default
	}
}

func (a *DroidAgent) Name() string {
	return "droid"
}

func (a *DroidAgent) CommandName() string {
	return a.Command
}

func (a *DroidAgent) CommandLine() string {
	agenticMode := a.Agentic || AllowUnsafeAgents()
	args := a.buildArgs(agenticMode)
	return a.Command + " " + strings.Join(args, " ")
}

func (a *DroidAgent) buildArgs(agenticMode bool) []string {
	args := []string{"exec"}

	// Set autonomy level based on agentic mode
	if agenticMode {
		args = append(args, "--auto", "medium")
	} else {
		args = append(args, "--auto", "low")
	}

	// Set reasoning effort if specified
	if effort := a.droidReasoningEffort(); effort != "" {
		args = append(args, "--reasoning-effort", effort)
	}

	return args
}

func (a *DroidAgent) Review(ctx context.Context, repoPath, commitSHA, prompt string, output io.Writer) (string, error) {
	// Use agentic mode if either per-job setting or global setting enables it
	agenticMode := a.Agentic || AllowUnsafeAgents()

	args := a.buildArgs(agenticMode)

	cmd := exec.CommandContext(ctx, a.Command, args...)
	cmd.Dir = repoPath
	cmd.Stdin = strings.NewReader(prompt)
	tracker := configureSubprocess(cmd)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	if sw := newSyncWriter(output); sw != nil {
		cmd.Stderr = io.MultiWriter(&stderr, sw)
	} else {
		cmd.Stderr = &stderr
	}

	if err := cmd.Run(); err != nil {
		if ctxErr := contextProcessError(ctx, tracker, err, nil); ctxErr != nil {
			return "", ctxErr
		}
		return "", fmt.Errorf("droid failed: %w\nstderr: %s", err, stderr.String())
	}

	result := stdout.String()
	if len(result) == 0 {
		return "No review output generated", nil
	}

	return result, nil
}

func init() {
	Register(NewDroidAgent(""))
}
