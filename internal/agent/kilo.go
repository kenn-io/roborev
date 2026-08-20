package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// KiloAgent runs code reviews using the Kilo CLI (https://kilo.ai).
// This implementation is intentionally cloned from OpenCodeAgent rather than
// sharing a base struct, because kilo may drift from opencode in the future.
type KiloAgent struct {
	Command   string         // The kilo command to run (default: "kilo")
	Model     string         // Model to use (provider/model format, e.g., "anthropic/claude-sonnet-4-20250514")
	Reasoning ReasoningLevel // Reasoning level mapped to --variant flag
	Agentic   bool           // Whether agentic mode is enabled (uses --auto)
	SessionID string         // Existing session ID to resume
}

// NewKiloAgent creates a new Kilo agent
func NewKiloAgent(command string) *KiloAgent {
	if command == "" {
		command = "kilo"
	}
	return &KiloAgent{Command: command, Reasoning: ReasoningStandard}
}

func (a *KiloAgent) clone(opts ...agentCloneOption) *KiloAgent {
	cfg := newAgentCloneConfig(
		a.Command,
		a.Model,
		a.Reasoning,
		a.Agentic,
		a.SessionID,
		opts...,
	)
	return &KiloAgent{
		Command:   cfg.Command,
		Model:     cfg.Model,
		Reasoning: cfg.Reasoning,
		Agentic:   cfg.Agentic,
		SessionID: cfg.SessionID,
	}
}

func (a *KiloAgent) WithReasoning(level ReasoningLevel) Agent {
	return a.clone(withClonedReasoning(level))
}

func (a *KiloAgent) WithAgentic(agentic bool) Agent {
	return a.clone(withClonedAgentic(agentic))
}

func (a *KiloAgent) WithModel(model string) Agent {
	if model == "" {
		return a
	}
	return a.clone(withClonedModel(model))
}

// WithSessionID returns a copy of the agent configured to resume a prior session.
func (a *KiloAgent) WithSessionID(sessionID string) Agent {
	return a.clone(withClonedSessionID(sessionID))
}

func (a *KiloAgent) Name() string {
	return "kilo"
}

func (a *KiloAgent) CommandName() string {
	return a.Command
}

// kiloVariant maps ReasoningLevel to kilo's --variant flag values
func (a *KiloAgent) kiloVariant() string {
	switch a.Reasoning {
	case ReasoningMaximum, ReasoningThorough:
		return "high"
	case ReasoningFast:
		return "minimal"
	case ReasoningLow:
		return "low"
	case ReasoningMedium:
		return "medium"
	case ReasoningHigh:
		return "high"
	case ReasoningXHigh:
		return "xhigh"
	case ReasoningMax:
		return "max"
	default:
		return "" // use kilo default
	}
}

func (a *KiloAgent) buildArgs() []string {
	args := []string{"run", "--format", "json"}
	if a.SessionID != "" {
		args = append(args, "--session", a.SessionID)
	}
	if a.Model != "" {
		args = append(args, "--model", a.Model)
	}
	if a.Agentic || AllowUnsafeAgents() {
		args = append(args, "--auto")
	}
	if variant := a.kiloVariant(); variant != "" {
		args = append(args, "--variant", variant)
	}
	return args
}

func (a *KiloAgent) CommandLine() string {
	return a.Command + " " + strings.Join(a.buildArgs(), " ")
}

// Review runs kilo with --format json and parses the JSONL stream
// (same envelope as opencode: {"type":"...","part":{"type":"text","text":"..."}}).
func (a *KiloAgent) Review(
	ctx context.Context,
	repoPath, commitSHA, prompt string,
	output io.Writer,
) (string, error) {
	runResult, runErr := runStreamingCLI(ctx, streamingCLISpec{
		Name:          "kilo",
		Command:       a.Command,
		Args:          a.buildArgs(),
		Dir:           repoPath,
		Stdin:         strings.NewReader(prompt),
		Output:        output,
		StreamStderr:  true,
		CaptureStdout: true,
		DrainStdout:   true,
		Parse:         parseOpenCodeJSON,
	})
	if runErr != nil {
		return "", runErr
	}

	if runResult.WaitErr != nil {
		errText := stripTerminalControls(runResult.Stderr)
		return "", formatDetailedCLIWaitError(runResult, detailedCLIWaitErrorOptions{
			AgentName:      "kilo",
			Stderr:         errText,
			FallbackOutput: stripTerminalControls(runResult.Stdout),
			FallbackLabel:  "output",
			PartialOutput:  runResult.Result,
		})
	}

	if runResult.ParseErr != nil {
		return runResult.Result, runResult.ParseErr
	}

	if runResult.Result == "" {
		// No review text parsed. Surface stderr or non-JSON
		// stdout so the actual error is visible instead of
		// a generic "no output" message.
		errOut := stripTerminalControls(runResult.Stderr)
		if errOut = strings.TrimSpace(errOut); errOut != "" {
			return "", fmt.Errorf(
				"kilo produced no output: %s", errOut,
			)
		}
		if raw := strings.TrimSpace(stripTerminalControls(runResult.Stdout)); raw != "" && hasNonJSONLine(raw) {
			return "", fmt.Errorf(
				"kilo produced no output: %s", raw,
			)
		}
		return "No review output generated", nil
	}
	return runResult.Result, nil
}

// hasNonJSONLine returns true if s contains any non-empty line
// that is not valid JSON. Used to distinguish plain-text error
// output from valid JSONL with no text events.
func hasNonJSONLine(s string) bool {
	for line := range strings.SplitSeq(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !json.Valid([]byte(line)) {
			return true
		}
	}
	return false
}

func init() {
	Register(NewKiloAgent(""))
}
