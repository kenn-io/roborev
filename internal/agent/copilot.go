package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os/exec"
	"strings"
	"sync"
)

var (
	copilotAllowAllToolsSupport      sync.Map
	copilotStreamOffSupport          sync.Map
	copilotJSONOutputSupport         sync.Map
	copilotDisableBuiltInMCPsSupport sync.Map
)

var errNoCopilotJSON = errors.New("no valid copilot JSON events parsed from output")

// copilotSupportsAllowAllTools checks whether the copilot binary supports
// the --allow-all-tools flag needed for non-interactive tool approval.
// Results are cached per command path.
func copilotSupportsAllowAllTools(ctx context.Context, command string) (bool, error) {
	if cached, ok := copilotAllowAllToolsSupport.Load(command); ok {
		return cached.(bool), nil
	}
	cmd := exec.CommandContext(ctx, command, "--help")
	configureCapabilityProbe(cmd)
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
	configureCapabilityProbe(cmd)
	output, err := cmd.CombinedOutput()
	supported := strings.Contains(string(output), "--stream")
	if err != nil && !supported {
		return false, fmt.Errorf("check %s --help: %w: %s", command, err, output)
	}
	copilotStreamOffSupport.Store(command, supported)
	return supported, nil
}

func copilotSupportsJSONOutput(ctx context.Context, command string) (bool, error) {
	if cached, ok := copilotJSONOutputSupport.Load(command); ok {
		return cached.(bool), nil
	}
	cmd := exec.CommandContext(ctx, command, "--help")
	configureCapabilityProbe(cmd)
	output, err := cmd.CombinedOutput()
	supported := strings.Contains(string(output), "--output-format")
	if err != nil && !supported {
		return false, fmt.Errorf("check %s --help: %w: %s", command, err, output)
	}
	copilotJSONOutputSupport.Store(command, supported)
	return supported, nil
}

func copilotSupportsDisableBuiltInMCPs(ctx context.Context, command string) (bool, error) {
	if cached, ok := copilotDisableBuiltInMCPsSupport.Load(command); ok {
		return cached.(bool), nil
	}
	cmd := exec.CommandContext(ctx, command, "--help")
	configureCapabilityProbe(cmd)
	output, err := cmd.CombinedOutput()
	supported := strings.Contains(string(output), "--disable-builtin-mcps")
	if err != nil && !supported {
		return false, fmt.Errorf("check %s --help: %w: %s", command, err, output)
	}
	copilotDisableBuiltInMCPsSupport.Store(command, supported)
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
	return a.commandArgs(agenticMode, true, true, true, true)
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
	args := a.commandArgs(agenticMode, false, false, false, false)
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

	supportsJSONOutput, err := copilotSupportsJSONOutput(ctx, a.Command)
	if err != nil {
		log.Printf("copilot: cannot detect --output-format support: %v", err)
	}

	supportsDisableBuiltInMCPs, err := copilotSupportsDisableBuiltInMCPs(ctx, a.Command)
	if err != nil {
		log.Printf("copilot: cannot detect --disable-builtin-mcps support: %v", err)
	}

	args := a.commandArgs(
		agenticMode,
		supportsAllowAllTools,
		supportsStreamOff,
		supportsJSONOutput,
		supportsDisableBuiltInMCPs,
	)

	cmd := exec.CommandContext(ctx, a.Command, args...)
	cmd.Stdin = strings.NewReader(prompt)
	cmd.Dir = repoPath
	tracker := configureSubprocess(cmd)

	var stdout, stderr bytes.Buffer
	if sw := newSyncWriter(output); sw != nil {
		if supportsJSONOutput {
			cmd.Stdout = &stdout
		} else {
			cmd.Stdout = io.MultiWriter(&stdout, sw)
		}
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
	if supportsJSONOutput {
		if sessionCapture, ok := output.(*SessionCaptureWriter); ok {
			sessionCapture.capture([]byte(lineTerminated(result)))
		}
		parsed, parseErr := parseCopilotJSON(strings.NewReader(result))
		if parseErr != nil && !errors.Is(parseErr, errNoCopilotJSON) {
			return "", parseErr
		}
		if parsed != "" {
			writeCopilotReviewOutput(output, parsed)
			return parsed, nil
		}
		if errors.Is(parseErr, errNoCopilotJSON) {
			writeCopilotReviewOutput(output, result)
		}
	}
	if len(result) == 0 {
		return "No review output generated", nil
	}

	return result, nil
}

func lineTerminated(text string) string {
	if text == "" || strings.HasSuffix(text, "\n") {
		return text
	}
	return text + "\n"
}

func writeCopilotReviewOutput(output io.Writer, text string) {
	if text == "" {
		return
	}
	sw := newSyncWriter(output)
	if sw == nil {
		return
	}
	_, _ = sw.Write([]byte(lineTerminated(text)))
}

func (a *CopilotAgent) commandArgs(
	agenticMode, includePermissions, includeStreamOff, includeJSONOutput, includeDisableBuiltInMCPs bool,
) []string {
	args := []string{}
	if includePermissions {
		args = append(args, "-s", "--allow-all-tools")
	}
	if includeStreamOff {
		args = append(args, "--stream", "off")
	}
	if includeJSONOutput {
		args = append(args, "--output-format", "json")
	}
	if includeDisableBuiltInMCPs && !agenticMode {
		args = append(args, "--disable-builtin-mcps")
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

type copilotEvent struct {
	Type string `json:"type"`
	Data struct {
		MessageID    string            `json:"messageId,omitempty"`
		Content      string            `json:"content,omitempty"`
		ToolCalls    []json.RawMessage `json:"toolCalls,omitempty"`
		ToolRequests []json.RawMessage `json:"toolRequests,omitempty"`
	} `json:"data,omitempty"`
}

func parseCopilotJSON(r io.Reader) (string, error) {
	var validEventsParsed bool
	assistantMessages := newTrailingReviewText()

	err := scanStreamJSONLines(r, nil, func(line string) error {
		var ev copilotEvent
		if jsonErr := json.Unmarshal([]byte(line), &ev); jsonErr != nil {
			return nil
		}
		if ev.Type == "" {
			return nil
		}
		validEventsParsed = true

		if ev.Type == "assistant.tool_call" ||
			ev.Type == "assistant.tool_result" ||
			strings.HasPrefix(ev.Type, "tool.") {
			assistantMessages.ResetAfterTool()
		}

		if ev.Type == "assistant.message" {
			if len(ev.Data.ToolCalls) > 0 || len(ev.Data.ToolRequests) > 0 {
				assistantMessages.ResetAfterTool()
				return nil
			}
			if ev.Data.Content != "" {
				assistantMessages.AddWithID(ev.Data.MessageID, ev.Data.Content)
			}
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if !validEventsParsed {
		return "", errNoCopilotJSON
	}
	return assistantMessages.Join("\n"), nil
}

func init() {
	Register(NewCopilotAgent(""))
}
