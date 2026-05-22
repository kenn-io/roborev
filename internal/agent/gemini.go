package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
)

// errNoStreamJSON indicates no valid stream-json events were parsed.
// Stream-json output is required; this error means the Gemini CLI may need to be upgraded.
var errNoStreamJSON = errors.New("no valid stream-json events parsed from output")

// maxStderrLen is the maximum number of bytes of stderr to include in error messages.
const maxStderrLen = 1024

// truncateStderr truncates stderr output to a reasonable size for error messages.
func truncateStderr(stderr string) string {
	if len(stderr) <= maxStderrLen {
		return stderr
	}
	return stderr[:maxStderrLen] + "... (truncated)"
}

// defaultGeminiModel is the built-in default that may be auto-retried
// without -m if Google retires the model name.
const defaultGeminiModel = "gemini-3.1-pro-preview"

// GeminiAgent runs code reviews using the Gemini CLI
type GeminiAgent struct {
	Command   string         // The gemini command to run (default: "gemini")
	Model     string         // Model to use (e.g., "gemini-3.1-pro-preview")
	Reasoning ReasoningLevel // Reasoning level (for future support)
	Agentic   bool           // Whether agentic mode is enabled (allow file edits)
}

// NewGeminiAgent creates a new Gemini agent
func NewGeminiAgent(command string) *GeminiAgent {
	if command == "" {
		command = "gemini"
	}
	return &GeminiAgent{Command: command, Model: defaultGeminiModel, Reasoning: ReasoningStandard}
}

func (a *GeminiAgent) clone(opts ...agentCloneOption) *GeminiAgent {
	cfg := newAgentCloneConfig(
		a.Command,
		a.Model,
		a.Reasoning,
		a.Agentic,
		"",
		opts...,
	)
	return &GeminiAgent{
		Command:   cfg.Command,
		Model:     cfg.Model,
		Reasoning: cfg.Reasoning,
		Agentic:   cfg.Agentic,
	}
}

// WithReasoning returns a copy of the agent with the model preserved (reasoning not yet supported).
func (a *GeminiAgent) WithReasoning(level ReasoningLevel) Agent {
	return a.clone(withClonedReasoning(level))
}

// WithAgentic returns a copy of the agent configured for agentic mode.
func (a *GeminiAgent) WithAgentic(agentic bool) Agent {
	return a.clone(withClonedAgentic(agentic))
}

// WithModel returns a copy of the agent configured to use the specified model.
func (a *GeminiAgent) WithModel(model string) Agent {
	if model == "" {
		return a
	}
	return a.clone(withClonedModel(model))
}

func (a *GeminiAgent) Name() string {
	return "gemini"
}

func (a *GeminiAgent) CommandName() string {
	return a.Command
}

func (a *GeminiAgent) CommandLine() string {
	agenticMode := a.Agentic || AllowUnsafeAgents()
	args := a.buildArgs(agenticMode)
	return a.Command + " " + strings.Join(args, " ")
}

func (a *GeminiAgent) buildArgs(agenticMode bool) []string {
	return a.buildArgsWithModel(a.Model, agenticMode)
}

func (a *GeminiAgent) Review(ctx context.Context, repoPath, commitSHA, prompt string, output io.Writer) (string, error) {
	agenticMode := a.Agentic || AllowUnsafeAgents()
	args := a.buildArgs(agenticMode)

	result, stderrStr, err := a.runGemini(ctx, repoPath, prompt, args, output)
	if err != nil && a.Model == defaultGeminiModel && isModelNotFoundError(stderrStr) {
		// Built-in default model may be stale (Google renames
		// frequently). Retry without -m to let the Gemini CLI use
		// its own default. Non-default models (set via WithModel /
		// config) fail fast so config errors are surfaced.
		log.Printf("gemini: model %q not found, retrying without -m flag", a.Model)
		noModelArgs := a.buildArgsWithModel("", agenticMode)
		result, _, err = a.runGemini(ctx, repoPath, prompt, noModelArgs, output)
	}
	return result, err
}

// buildArgsWithModel builds CLI args with an explicit model override
// (empty string omits the -m flag entirely).
func (a *GeminiAgent) buildArgsWithModel(model string, agenticMode bool) []string {
	args := []string{"--output-format", "stream-json"}

	if model != "" {
		args = append(args, "-m", model)
	}

	if agenticMode {
		args = append(args, "--approval-mode", "yolo")
	} else {
		args = append(args, "--approval-mode", "plan")
	}

	return args
}

// runGemini executes the Gemini CLI with the given args and returns
// the review result, captured stderr, and any error.
func (a *GeminiAgent) runGemini(ctx context.Context, repoPath, prompt string, args []string, output io.Writer) (string, string, error) {
	runResult, runErr := runStreamingCLI(ctx, streamingCLISpec{
		Name:         "gemini",
		Command:      a.Command,
		Args:         args,
		Dir:          repoPath,
		Stdin:        strings.NewReader(prompt),
		Output:       output,
		StreamStderr: true,
		Parse: func(r io.Reader, sw *syncWriter) (string, error) {
			parsed, err := a.parseStreamJSON(r, sw)
			return parsed.result, err
		},
	})
	if runErr != nil {
		return "", "", runErr
	}

	if runResult.WaitErr != nil {
		return "", runResult.Stderr, formatStreamingCLIWaitError("gemini", runResult, truncateStderr(runResult.Stderr))
	}

	if runResult.ParseErr != nil {
		if errors.Is(runResult.ParseErr, errNoStreamJSON) {
			return "", runResult.Stderr, fmt.Errorf("gemini CLI must support --output-format stream-json; upgrade to latest version\nstderr: %s: %w", truncateStderr(runResult.Stderr), errNoStreamJSON)
		}
		return "", runResult.Stderr, runResult.ParseErr
	}

	if runResult.Result != "" {
		return runResult.Result, runResult.Stderr, nil
	}

	return "No review output generated", runResult.Stderr, nil
}

// isModelNotFoundError returns true if stderr indicates the requested
// model does not exist. Google's API returns 404 with "model not found"
// or "is not found" messages when a model name is invalid or retired.
func isModelNotFoundError(stderr string) bool {
	lower := strings.ToLower(stderr)
	return strings.Contains(lower, "model") &&
		(strings.Contains(lower, "not found") ||
			strings.Contains(lower, "is not found") ||
			strings.Contains(lower, "not_found"))
}

// geminiStreamMessage represents a message in Gemini's stream-json output format
type geminiStreamMessage struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype,omitempty"`
	// Top-level fields for "message" type events
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
	Delta   bool   `json:"delta,omitempty"`
	// Nested message field (older format / Claude Code compatibility)
	Message struct {
		Content string `json:"content,omitempty"`
	} `json:"message,omitempty"`
	// Result field for "result" type events
	Result string `json:"result,omitempty"`
}

// parseResult contains the parsed result from stream-json output
type parseResult struct {
	result string // The extracted result text
}

// parseStreamJSON parses Gemini's stream-json output and extracts the final result.
// Returns parseResult with the extracted content, or error on I/O or parse failure.
// The sw parameter is the shared sync writer for thread-safe output (may be nil).
func (a *GeminiAgent) parseStreamJSON(r io.Reader, sw *syncWriter) (parseResult, error) {
	var lastResult string
	assistantMessages := newTrailingReviewText()
	var validEventsParsed bool

	err := scanStreamJSONLines(r, sw, func(trimmed string) error {
		var msg geminiStreamMessage
		if jsonErr := json.Unmarshal([]byte(trimmed), &msg); jsonErr == nil {
			validEventsParsed = true

			if msg.Type == "message" && msg.Role == "assistant" && msg.Content != "" {
				assistantMessages.Add(msg.Content)
			}
			if msg.Type == "assistant" && msg.Message.Content != "" {
				assistantMessages.Add(msg.Message.Content)
			}
			if msg.Type == "tool" || msg.Type == "tool_result" {
				assistantMessages.ResetAfterTool()
			}

			if msg.Type == "result" && msg.Result != "" {
				lastResult = msg.Result
			}
		}
		return nil
	})
	if err != nil {
		return parseResult{}, err
	}

	// If no valid events were parsed, return error
	if !validEventsParsed {
		return parseResult{}, errNoStreamJSON
	}

	// Prefer the result field if present, otherwise join assistant messages
	if lastResult != "" {
		return parseResult{result: lastResult}, nil
	}
	if result := assistantMessages.Join("\n"); result != "" {
		return parseResult{result: result}, nil
	}

	return parseResult{}, nil
}

func init() {
	Register(NewGeminiAgent(""))
}
