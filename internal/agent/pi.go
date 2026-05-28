package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"go.kenn.io/roborev/internal/config"
)

// PiAgent runs code reviews using the pi CLI
type PiAgent struct {
	Command             string         // The pi command to run (default: "pi")
	Model               string         // Model to use (provider/model format or just model)
	Provider            string         // Explicit provider (optional)
	Reasoning           ReasoningLevel // Reasoning level
	Agentic             bool           // Agentic mode
	SessionID           string         // Existing session ID to resume
	JSONSchemaExtension string         // Pi extension source for classifier schema output
}

// NewPiAgent creates a new pi agent
func NewPiAgent(command string) *PiAgent {
	if command == "" {
		command = "pi"
	}
	return &PiAgent{
		Command:             command,
		Reasoning:           ReasoningStandard,
		JSONSchemaExtension: config.DefaultPiJSONSchemaExtension,
	}
}

func (a *PiAgent) clone(opts ...agentCloneOption) *PiAgent {
	cfg := newAgentCloneConfig(
		a.Command,
		a.Model,
		a.Reasoning,
		a.Agentic,
		a.SessionID,
		opts...,
	)
	return &PiAgent{
		Command:             cfg.Command,
		Model:               cfg.Model,
		Provider:            a.Provider,
		Reasoning:           cfg.Reasoning,
		Agentic:             cfg.Agentic,
		SessionID:           cfg.SessionID,
		JSONSchemaExtension: a.JSONSchemaExtension,
	}
}

func (a *PiAgent) Name() string {
	return "pi"
}

// WithReasoning returns a copy of the agent configured with the specified reasoning level.
func (a *PiAgent) WithReasoning(level ReasoningLevel) Agent {
	return a.clone(withClonedReasoning(level))
}

// WithAgentic returns a copy of the agent configured for agentic mode.
func (a *PiAgent) WithAgentic(agentic bool) Agent {
	return a.clone(withClonedAgentic(agentic))
}

// WithModel returns a copy of the agent configured to use the specified model.
func (a *PiAgent) WithModel(model string) Agent {
	if model == "" {
		return a
	}
	return a.clone(withClonedModel(model))
}

// WithProvider returns a copy of the agent configured to use the specified provider.
func (a *PiAgent) WithProvider(provider string) Agent {
	if provider == "" {
		return a
	}
	cloned := a.clone()
	cloned.Provider = provider
	return cloned
}

// WithSessionID returns a copy of the agent configured to resume a prior session.
func (a *PiAgent) WithSessionID(sessionID string) Agent {
	return a.clone(withClonedSessionID(sessionID))
}

func (a *PiAgent) CommandName() string {
	return a.Command
}

func (a *PiAgent) CommandLine() string {
	args := a.buildArgs("")
	return a.Command + " " + strings.Join(args, " ")
}

func (a *PiAgent) buildArgs(sessionPath string) []string {
	args := []string{"-p", "--mode", "json"}
	if sessionPath != "" {
		args = append(args, "--session", sessionPath)
	}
	if a.Provider != "" {
		args = append(args, "--provider", a.Provider)
	}
	if a.Model != "" {
		args = append(args, "--model", a.Model)
	}
	if level := a.thinkingLevel(); level != "" {
		args = append(args, "--thinking", level)
	}
	return args
}

func (a *PiAgent) thinkingLevel() string {
	switch a.Reasoning {
	case ReasoningMaximum, ReasoningThorough:
		return "high"
	case ReasoningFast:
		return "low"
	default: // Standard
		return "medium"
	}
}

func (a *PiAgent) classifyArgs(promptPath, outputPath string, schema json.RawMessage) []string {
	args := []string{
		"--no-session",
		"--no-extensions",
		"--no-builtin-tools",
		"--no-skills",
		"--no-prompt-templates",
		"--no-themes",
		"--no-context-files",
		"--extension", a.jsonSchemaExtension(),
		"--json-schema", string(schema),
		"--json-output", outputPath,
		"--json-fallback", "none",
		"-p",
	}
	if a.Provider != "" {
		args = append(args, "--provider", a.Provider)
	}
	if a.Model != "" {
		args = append(args, "--model", a.Model)
	}
	if level := a.thinkingLevel(); level != "" {
		args = append(args, "--thinking", level)
	}
	return append(args,
		"@"+promptPath,
		"Classify according to the attached instructions and write the result with the structured JSON output tool.",
	)
}

func (a *PiAgent) jsonSchemaExtension() string {
	if ext := strings.TrimSpace(a.JSONSchemaExtension); ext != "" {
		return ext
	}
	return config.DefaultPiJSONSchemaExtension
}

// ClassifyWithSchema runs a single constrained Pi invocation and returns the
// JSON document written by the pi-json-schema extension. The invocation disables
// builtin tools, extension discovery, skills, prompt templates, themes, context
// file discovery, and session persistence; only the explicit schema-output
// extension is loaded.
func (a *PiAgent) ClassifyWithSchema(
	ctx context.Context,
	repoPath, gitRef, prompt string,
	schema json.RawMessage,
	out io.Writer,
) (json.RawMessage, error) {
	tmpDir, err := os.MkdirTemp("", "roborev-pi-classify-*")
	if err != nil {
		return nil, fmt.Errorf("create temp classify dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	promptPath := filepath.Join(tmpDir, "prompt.md")
	if err := os.WriteFile(promptPath, []byte(prompt), 0o600); err != nil {
		return nil, fmt.Errorf("write classify prompt: %w", err)
	}
	outputPath := filepath.Join(tmpDir, "result.json")

	args := a.classifyArgs(promptPath, outputPath, schema)
	cmd := exec.CommandContext(ctx, a.Command, args...)
	cmd.Dir = repoPath
	tracker := configureSubprocess(cmd)

	var stdoutBuf bytes.Buffer
	var stderrBuf bytes.Buffer
	if out != nil {
		sw := newSyncWriter(out)
		cmd.Stdout = io.MultiWriter(&stdoutBuf, sw)
		cmd.Stderr = io.MultiWriter(&stderrBuf, sw)
	} else {
		cmd.Stdout = &stdoutBuf
		cmd.Stderr = &stderrBuf
	}

	if err := cmd.Run(); err != nil {
		if ctxErr := contextProcessError(ctx, tracker, err, nil); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("pi classifier failed: %w\nstderr: %s", err, strings.TrimSpace(stderrBuf.String()))
	}

	result, err := os.ReadFile(outputPath)
	if err != nil {
		return nil, fmt.Errorf("read pi classifier output: %w", err)
	}
	result = bytes.TrimSpace(result)
	if !json.Valid(result) {
		return nil, fmt.Errorf("pi classifier output is not valid JSON: %q", string(result))
	}
	return json.RawMessage(result), nil
}

func (a *PiAgent) Review(
	ctx context.Context,
	repoPath, commitSHA, prompt string,
	output io.Writer,
) (string, error) {
	// Write prompt to a temporary file to avoid command line length limits
	// and to properly handle special characters.
	tmpDir := os.TempDir()
	tmpFile, err := os.CreateTemp(tmpDir, "roborev-pi-prompt-*.md")
	if err != nil {
		return "", fmt.Errorf("create temp prompt file: %w", err)
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.WriteString(prompt); err != nil {
		tmpFile.Close()
		return "", fmt.Errorf("write prompt to temp file: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return "", fmt.Errorf("close temp prompt file: %w", err)
	}

	sessionPath := resolvePiSessionPath(sanitizedResumeSessionID(a.SessionID))
	args := a.buildArgs(sessionPath)

	// Add the prompt file as an input argument (prefixed with @)
	// Pi treats @files as context/input.
	// Since the prompt contains the instructions, we pass it as a file.
	// But pi might expect instructions as text arguments.
	// The docs say: pi [options] [@files...] [messages...]
	// If we only provide @file, pi reads it. Does it treat it as a user message or context?
	// Usually @file is context.
	// We want the prompt to be the "message".
	// But if the prompt is huge, we can't pass it as an argument.
	// If we pass @file, pi loads the file.
	// We might need to add a small trigger message like "Follow the instructions in the attached file."
	args = append(args, "@"+tmpFile.Name(), "Please follow the instructions in the attached file to review the code.")

	cmd := exec.CommandContext(ctx, a.Command, args...)
	cmd.Dir = repoPath
	tracker := configureSubprocess(cmd)

	// Capture stdout for the result
	var stdoutBuf bytes.Buffer
	var stderrBuf bytes.Buffer

	// Stream stdout to output writer if provided
	if output != nil {
		sw := newSyncWriter(output)
		cmd.Stdout = io.MultiWriter(&stdoutBuf, sw)
		cmd.Stderr = io.MultiWriter(&stderrBuf, sw)
	} else {
		cmd.Stdout = &stdoutBuf
		cmd.Stderr = &stderrBuf
	}

	if err := cmd.Run(); err != nil {
		if ctxErr := contextProcessError(ctx, tracker, err, nil); ctxErr != nil {
			return "", ctxErr
		}
		return "", fmt.Errorf("pi failed: %w\nstderr: %s", err, stderrBuf.String())
	}

	result := stdoutBuf.String()
	if result == "" {
		if stderrBuf.Len() > 0 {
			return stderrBuf.String(), nil
		}
		return "No review output generated", nil
	}

	parsed, parseErr := parsePiJSON(strings.NewReader(result))
	if parseErr != nil {
		return "", parseErr
	}
	if parsed == "" {
		if stderrBuf.Len() > 0 {
			return stderrBuf.String(), nil
		}
		return "No review output generated", nil
	}
	return parsed, nil
}

func piDataDir() string {
	if dir := os.Getenv("PI_CODING_AGENT_DIR"); dir != "" {
		return dir
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".pi", "agent")
}

func resolvePiSessionPath(sessionID string) string {
	if sessionID == "" {
		return ""
	}
	matches, err := filepath.Glob(filepath.Join(piDataDir(), "sessions", "*", "*_"+sessionID+".jsonl"))
	if err != nil || len(matches) == 0 {
		return ""
	}
	return matches[0]
}

// maxPiTokenSize is the maximum single-line size parsePiJSON
// will tolerate. 4 MB accommodates large assistant messages
// emitted as a single JSON line.
const maxPiTokenSize = 4 * 1024 * 1024

func parsePiJSON(r io.Reader) (string, error) {
	br := bufio.NewScanner(r)
	br.Buffer(make([]byte, 0, bufio.MaxScanTokenSize), maxPiTokenSize)
	var latest string
	for br.Scan() {
		line := strings.TrimSpace(br.Text())
		if line == "" {
			continue
		}

		var ev struct {
			Type    string `json:"type"`
			Message struct {
				Role    string `json:"role"`
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text,omitempty"`
				} `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		if ev.Message.Role != "assistant" {
			continue
		}
		var parts []string
		for _, item := range ev.Message.Content {
			if item.Type == "text" && item.Text != "" {
				parts = append(parts, item.Text)
			}
		}
		if len(parts) > 0 {
			latest = strings.Join(parts, "\n")
		}
	}
	if err := br.Err(); err != nil {
		return latest, fmt.Errorf("read pi stream: %w", err)
	}
	return latest, nil
}

func init() {
	Register(NewPiAgent(""))
}

var _ SchemaAgent = (*PiAgent)(nil)
