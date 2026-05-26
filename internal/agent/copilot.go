package agent

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"os/exec"
	"strings"
	"sync"
)

var copilotAllowAllToolsSupport sync.Map
var copilotStreamOffSupport sync.Map

// copilotSupportsAllowAllTools checks whether the copilot binary supports
// the --allow-all-tools flag needed for non-interactive tool approval.
// Results are cached per command path.
func copilotSupportsAllowAllTools(ctx context.Context, command string) (bool, error) {
	if cached, ok := copilotAllowAllToolsSupport.Load(command); ok {
		return cached.(bool), nil
	}
	cmd := exec.CommandContext(ctx, command, "--help")
	output, err := cmd.CombinedOutput()
	supported := strings.Contains(string(output), "--allow-all-tools")
	if err != nil && !supported {
		return false, fmt.Errorf("check %s --help: %w: %s", command, err, output)
	}
	copilotAllowAllToolsSupport.Store(command, supported)
	return supported, nil
}

// copilotSupportsStreamOff checks whether the copilot binary supports
// disabling streaming output so stdout remains pipe-capturable.
// Results are cached per command path.
func copilotSupportsStreamOff(ctx context.Context, command string) (bool, error) {
	if cached, ok := copilotStreamOffSupport.Load(command); ok {
		return cached.(bool), nil
	}
	cmd := exec.CommandContext(ctx, command, "--help")
	output, err := cmd.CombinedOutput()
	supported := strings.Contains(string(output), "--stream")
	if err != nil && !supported {
		return false, fmt.Errorf("check %s --help: %w: %s", command, err, output)
	}
	copilotStreamOffSupport.Store(command, supported)
	return supported, nil
}

// copilotReviewDenyTools lists tools denied in review mode to enforce read-only
// behavior. Deny rules take precedence over --allow-all-tools in copilot's
// permission system.
var copilotReviewDenyTools = []string{
	"write",
	"shell(git push:*)",
	"shell(git commit:*)",
	"shell(git checkout:*)",
	"shell(git reset:*)",
	"shell(git rebase:*)",
	"shell(git merge:*)",
	"shell(git stash:*)",
	"shell(git clean:*)",
	"shell(rm:*)",
}

// buildArgs constructs CLI arguments for a copilot invocation.
// In review mode, destructive tools are denied. In agentic mode, all tools
// are allowed without restriction.
func (a *CopilotAgent) buildArgs(agenticMode bool) []string {
	return a.commandArgs(agenticMode, true, true)
}

// CopilotAgent runs code reviews using the GitHub Copilot CLI
type CopilotAgent struct {
	Command   string         // The copilot command to run (default: "copilot")
	Model     string         // Model to use
	Reasoning ReasoningLevel // Reasoning level (for future support)
	Agentic   bool           // Whether agentic mode is enabled (controls --deny-tool flags)
}

// NewCopilotAgent creates a new Copilot agent
func NewCopilotAgent(command string) *CopilotAgent {
	if command == "" {
		command = "copilot"
	}
	return &CopilotAgent{Command: command, Reasoning: ReasoningStandard}
}

func (a *CopilotAgent) clone(opts ...agentCloneOption) *CopilotAgent {
	cfg := newAgentCloneConfig(
		a.Command,
		a.Model,
		a.Reasoning,
		a.Agentic,
		"",
		opts...,
	)
	return &CopilotAgent{
		Command:   cfg.Command,
		Model:     cfg.Model,
		Reasoning: cfg.Reasoning,
		Agentic:   cfg.Agentic,
	}
}

// WithReasoning returns a copy of the agent with the model preserved (reasoning not yet supported).
func (a *CopilotAgent) WithReasoning(level ReasoningLevel) Agent {
	return a.clone(withClonedReasoning(level))
}

// WithAgentic returns a copy of the agent configured for agentic mode.
// In agentic mode, all tools are allowed without restriction. In review mode
// (default), destructive tools are denied via --deny-tool flags.
func (a *CopilotAgent) WithAgentic(agentic bool) Agent {
	return a.clone(withClonedAgentic(agentic))
}

// WithModel returns a copy of the agent configured to use the specified model.
func (a *CopilotAgent) WithModel(model string) Agent {
	if model == "" {
		return a
	}
	return a.clone(withClonedModel(model))
}

func (a *CopilotAgent) Name() string {
	return "copilot"
}

func (a *CopilotAgent) CommandName() string {
	return a.Command
}

func (a *CopilotAgent) CommandLine() string {
	agenticMode := a.Agentic || AllowUnsafeAgents()
	args := a.commandArgs(agenticMode, false, false)
	return a.Command + " " + strings.Join(args, " ")
}

func (a *CopilotAgent) Review(ctx context.Context, repoPath, commitSHA, prompt string, output io.Writer) (string, error) {
	agenticMode := a.Agentic || AllowUnsafeAgents()

	supportsAllowAllTools, err := copilotSupportsAllowAllTools(ctx, a.Command)
	if err != nil {
		log.Printf("copilot: cannot detect --allow-all-tools support: %v", err)
	}

	supportsStreamOff, err := copilotSupportsStreamOff(ctx, a.Command)
	if err != nil {
		log.Printf("copilot: cannot detect --stream support: %v", err)
	}

	args := a.commandArgs(agenticMode, supportsAllowAllTools, supportsStreamOff)

	cmd := exec.CommandContext(ctx, a.Command, args...)
	cmd.Stdin = strings.NewReader(prompt)
	cmd.Dir = repoPath
	tracker := configureSubprocess(cmd)

	var stdout, stderr bytes.Buffer
	if sw := newSyncWriter(output); sw != nil {
		cmd.Stdout = io.MultiWriter(&stdout, sw)
		cmd.Stderr = io.MultiWriter(&stderr, sw)
	} else {
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
	}

	if err := cmd.Run(); err != nil {
		if ctxErr := contextProcessError(ctx, tracker, err, nil); ctxErr != nil {
			return "", ctxErr
		}
		return "", fmt.Errorf("copilot failed: %w\nstderr: %s", err, stderr.String())
	}

	result := stdout.String()
	if len(result) == 0 {
		return "No review output generated", nil
	}

	return result, nil
}

func (a *CopilotAgent) commandArgs(agenticMode, includePermissions, includeStreamOff bool) []string {
	args := []string{}
	if includePermissions {
		args = append(args, "-s", "--allow-all-tools")
	}
	if includeStreamOff {
		args = append(args, "--stream", "off")
	}
	if a.Model != "" {
		args = append(args, "--model", a.Model)
	}
	if includePermissions && !agenticMode {
		for _, tool := range copilotReviewDenyTools {
			args = append(args, "--deny-tool", tool)
		}
	}
	return args
}

func init() {
	Register(NewCopilotAgent(""))
}
