package agent

import (
	"context"
	"encoding/json"
	"io"
	"regexp"
	"strings"
	"unicode"
)

// OpenCodeAgent runs code reviews using the OpenCode CLI
type OpenCodeAgent struct {
	Command   string         // The opencode command to run (default: "opencode")
	Model     string         // Model to use (provider/model format, e.g., "anthropic/claude-sonnet-4-20250514")
	Reasoning ReasoningLevel // Reasoning level (for future support)
	Agentic   bool           // Whether agentic mode is enabled (OpenCode auto-approves in non-interactive mode)
	SessionID string         // Existing session ID to resume
}

// NewOpenCodeAgent creates a new OpenCode agent
func NewOpenCodeAgent(command string) *OpenCodeAgent {
	if command == "" {
		command = "opencode"
	}
	return &OpenCodeAgent{Command: command, Reasoning: ReasoningStandard}
}

func (a *OpenCodeAgent) clone(opts ...agentCloneOption) *OpenCodeAgent {
	cfg := newAgentCloneConfig(
		a.Command,
		a.Model,
		a.Reasoning,
		a.Agentic,
		a.SessionID,
		opts...,
	)
	return &OpenCodeAgent{
		Command:   cfg.Command,
		Model:     cfg.Model,
		Reasoning: cfg.Reasoning,
		Agentic:   cfg.Agentic,
		SessionID: cfg.SessionID,
	}
}

// WithReasoning returns a copy of the agent with the model preserved (reasoning not yet supported).
func (a *OpenCodeAgent) WithReasoning(level ReasoningLevel) Agent {
	return a.clone(withClonedReasoning(level))
}

// WithAgentic returns a copy of the agent configured for agentic mode.
// Note: OpenCode's `run` command auto-approves all permissions in non-interactive mode,
// so agentic mode is effectively always enabled when running through roborev.
func (a *OpenCodeAgent) WithAgentic(agentic bool) Agent {
	return a.clone(withClonedAgentic(agentic))
}

// WithModel returns a copy of the agent configured to use the specified model.
func (a *OpenCodeAgent) WithModel(model string) Agent {
	if model == "" {
		return a
	}
	return a.clone(withClonedModel(model))
}

// WithSessionID returns a copy of the agent configured to resume a prior session.
func (a *OpenCodeAgent) WithSessionID(sessionID string) Agent {
	return a.clone(withClonedSessionID(sessionID))
}

func (a *OpenCodeAgent) Name() string {
	return "opencode"
}

func (a *OpenCodeAgent) CommandName() string {
	return a.Command
}

func (a *OpenCodeAgent) buildArgs() []string {
	args := []string{"run", "--format", "json"}
	if a.SessionID != "" {
		args = append(args, "--session", a.SessionID)
	}
	if a.Model != "" {
		args = append(args, "--model", a.Model)
	}
	return args
}

func (a *OpenCodeAgent) CommandLine() string {
	args := a.buildArgs()
	return a.Command + " " + strings.Join(args, " ")
}

func (a *OpenCodeAgent) Review(
	ctx context.Context,
	repoPath, commitSHA, prompt string,
	output io.Writer,
) (string, error) {
	args := a.buildArgs()

	runResult, runErr := runStreamingCLI(ctx, streamingCLISpec{
		Name:    "opencode",
		Command: a.Command,
		Args:    args,
		Dir:     repoPath,
		Stdin:   strings.NewReader(prompt),
		Output:  output,
		// opencode prints sqlite-migration progress to stderr
		// on every invocation, drowning the live log with
		// noise. Skip stderr streaming; the full stderr is
		// still captured by runStreamingCLI and surfaced via
		// formatDetailedCLIWaitError on non-zero exit.
		StreamStderr: false,
		DrainStdout:  true,
		Parse:        parseOpenCodeJSON,
	})
	if runErr != nil {
		return "", runErr
	}

	if runResult.WaitErr != nil {
		return "", formatDetailedCLIWaitError(runResult, detailedCLIWaitErrorOptions{
			AgentName:     "opencode",
			Stderr:        runResult.Stderr,
			PartialOutput: runResult.Result,
		})
	}

	if runResult.ParseErr != nil {
		return runResult.Result, runResult.ParseErr
	}

	if runResult.Result == "" {
		return "No review output generated", nil
	}
	return runResult.Result, nil
}

// opencodeEvent represents a top-level JSONL event from opencode --format json.
type opencodeEvent struct {
	Type string          `json:"type"`
	Part json.RawMessage `json:"part,omitempty"`
}

// opencodePart represents the nested part payload in opencode events.
type opencodePart struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// parseOpenCodeJSON reads JSONL from opencode --format json, streams
// raw lines to the output writer (for live rendering + log capture),
// and extracts the final result text from "text" events.
func parseOpenCodeJSON(
	r io.Reader, sw *syncWriter,
) (string, error) {
	textParts := newTrailingReviewText()

	err := scanStreamJSONLines(r, sw, func(line string) error {
		var ev opencodeEvent
		if json.Unmarshal([]byte(line), &ev) == nil && ev.Part != nil {
			var part opencodePart
			if json.Unmarshal(ev.Part, &part) == nil && part.Type != "" {
				if part.Type == "tool" {
					textParts.ResetAfterTool()
				}
				if part.Type == "text" && part.Text != "" {
					textParts.Add(stripTerminalControls(part.Text))
				}
			}
		}
		return nil
	})
	if err != nil {
		return textParts.Join(""), err
	}

	return textParts.Join(""), nil
}

// ansiPattern matches ANSI CSI and OSC escape sequences.
var ansiPattern = regexp.MustCompile(
	`\x1b\[[0-9;?]*[a-zA-Z]` +
		`|\x1b\]([^\x07\x1b]|\x1b[^\\])*(\x07|\x1b\\)`,
)

// stripTerminalControls removes ANSI escape sequences and
// non-printable control characters from s while preserving
// newlines and tabs. Used to sanitize agent output at ingestion
// before it is stored or rendered.
func stripTerminalControls(s string) string {
	s = ansiPattern.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r == '\t' || r == '\n' || !unicode.IsControl(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func init() {
	Register(NewOpenCodeAgent(""))
}
