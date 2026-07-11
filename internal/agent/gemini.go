package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os/exec"
	"path"
	"runtime"
	"strconv"
	"strings"
	"time"
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
	Command       string         // The gemini-compatible command to run (default: "gemini"; "agy" is preferred at resolution time)
	Model         string         // Model to use (e.g., "gemini-3.1-pro-preview")
	ModelExplicit bool           // Whether Model came from WithModel/config rather than the built-in default
	CommandAuto   bool           // Whether Command was selected from compatible command candidates
	Reasoning     ReasoningLevel // Reasoning level (for future support)
	Agentic       bool           // Whether agentic mode is enabled (allow file edits)
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
		Command:       cfg.Command,
		Model:         cfg.Model,
		ModelExplicit: a.ModelExplicit,
		CommandAuto:   a.CommandAuto,
		Reasoning:     cfg.Reasoning,
		Agentic:       cfg.Agentic,
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
	clone := a.clone(withClonedModel(model))
	clone.ModelExplicit = true
	return clone
}

func (a *GeminiAgent) Name() string {
	return "gemini"
}

func (a *GeminiAgent) CommandName() string {
	return a.Command
}

func (a *GeminiAgent) CommandNames() []string {
	if a.Command == "gemini" {
		return []string{"agy", "gemini"}
	}
	return []string{a.Command}
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
	if a.usesAntigravity() && a.ModelExplicit {
		return "", fmt.Errorf("antigravity CLI does not support explicit Gemini model selection; remove the model override or configure gemini_cmd to the legacy gemini CLI")
	}

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
	if a.usesAntigravity() {
		return a.buildAntigravityArgs(agenticMode)
	}

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

func (a *GeminiAgent) usesAntigravity() bool {
	return commandBaseName(a.Command) == "agy"
}

func commandBaseName(command string) string {
	base := strings.ToLower(path.Base(strings.ReplaceAll(command, "\\", "/")))
	base = strings.TrimSuffix(base, ".exe")
	return base
}

func (a *GeminiAgent) buildAntigravityArgs(agenticMode bool) []string {
	// These are the flags common to both print-mode contracts. runAntigravity
	// adds the prompt-carrying flag (a bare --print for old agy that reads
	// stdin, or --prompt <text> for agy >= 1.1.1) after detecting the version,
	// so no --print is emitted here where it would swallow --print-timeout.
	args := []string{"--print-timeout", "30m"}

	if agenticMode {
		args = append(args, "--dangerously-skip-permissions")
	} else {
		args = append(args, "--sandbox")
	}

	return args
}

// runGemini executes the Gemini CLI with the given args and returns
// the review result, captured stderr, and any error.
func (a *GeminiAgent) runGemini(ctx context.Context, repoPath, prompt string, args []string, output io.Writer) (string, string, error) {
	if a.usesAntigravity() {
		return a.runAntigravity(ctx, repoPath, prompt, args, output)
	}

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

// antigravityPromptFlagVersion is the agy release where print mode began taking
// the prompt as the value of --prompt (an alias of --print/-p) and stopped
// reading it from stdin. Older agy builds read the prompt from stdin with a
// bare --print. agy is an unstable CLI target, so the invocation is gated on
// the reported version rather than assuming one contract.
const (
	antigravityPromptFlagVersion   = "1.1.1"
	antigravityVersionProbeTimeout = 10 * time.Second
)

func (a *GeminiAgent) runAntigravity(ctx context.Context, repoPath, prompt string, args []string, output io.Writer) (string, string, error) {
	// Choose the prompt-carrying flag by agy version: >= 1.1.1 takes the prompt
	// as the value of --prompt (stdin is ignored); older agy reads it from
	// stdin with a bare --print. A bare --print on new agy would swallow the
	// following --print-timeout token as the prompt, so the two forms must not
	// be mixed.
	trimmedPrompt := strings.TrimRight(prompt, "\n")
	var finalArgs []string
	var stdin io.Reader
	if antigravityPromptViaFlag(ctx, a.Command) {
		// This contract carries the prompt in argv (agy has no stdin/file
		// prompt input here), so bound its length to fail with a clear error
		// rather than an opaque exec failure. The ceiling is platform-specific
		// (see antigravityMaxPromptArgLen).
		if size, limit := antigravityPromptArgSize(trimmedPrompt), antigravityMaxPromptArgLen(); size > limit {
			return "", "", fmt.Errorf("prompt too large for antigravity argv (size %d, max %d on %s)", size, limit, runtime.GOOS)
		}
		finalArgs = append(append([]string(nil), args...), "--prompt", trimmedPrompt)
	} else {
		finalArgs = append([]string{"--print"}, args...)
		stdin = strings.NewReader(trimmedPrompt + "\n")
	}

	runResult, runErr := runStreamingCLI(ctx, streamingCLISpec{
		Name:         "antigravity",
		Command:      a.Command,
		Args:         finalArgs,
		Dir:          repoPath,
		Stdin:        stdin,
		Output:       output,
		StreamStderr: true,
		Parse: func(r io.Reader, sw *syncWriter) (string, error) {
			return parseAntigravityOutput(r, sw)
		},
	})
	if runErr != nil {
		return "", "", runErr
	}

	if runResult.WaitErr != nil {
		return "", runResult.Stderr, formatStreamingCLIWaitError("antigravity", runResult, truncateStderr(runResult.Stderr))
	}

	if runResult.ParseErr != nil {
		return "", runResult.Stderr, runResult.ParseErr
	}

	if runResult.Result != "" {
		return runResult.Result, runResult.Stderr, nil
	}

	return "No review output generated", runResult.Stderr, nil
}

// antigravityPromptViaFlag reports whether the installed agy expects the prompt
// as a --prompt flag value (agy >= 1.1.1) rather than on stdin (agy <= 1.1.0).
// It shells out to `agy --version`; the `agy version` subcommand needs a TTY
// and fails in pipes/subprocesses. Any detection failure (missing binary,
// timeout, unparseable output) defaults to the current flag contract, since the
// stdin form only exists in old agy builds.
func antigravityPromptViaFlag(ctx context.Context, command string) bool {
	// Cap the probe: `agy --version` returns instantly, but agy's sibling
	// `agy version` subcommand hangs without a TTY, so don't let a wedged probe
	// consume the whole review timeout.
	vctx, cancel := context.WithTimeout(ctx, antigravityVersionProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(vctx, command, "--version")
	// Run the probe from a stable cwd and without a console window, and avoid
	// inheriting a deleted daemon working directory (a bad cwd otherwise makes
	// the probe fail and mis-default a legacy agy to the --prompt contract).
	configureCapabilityProbe(cmd)
	out, err := cmd.Output()
	if err != nil {
		log.Printf("antigravity: could not read agy version (%v); assuming the --prompt flag contract", err)
		return true
	}
	return antigravityVersionUsesPromptFlag(string(out))
}

// antigravityPromptArgSize measures the prompt against the platform's
// command-line limit: UTF-16 code units on Windows (what CreateProcess counts,
// counting supplementary-plane runes as a surrogate pair), bytes elsewhere.
func antigravityPromptArgSize(prompt string) int {
	if runtime.GOOS != "windows" {
		return len(prompt)
	}
	return utf16CodeUnits(prompt)
}

// utf16CodeUnits counts the UTF-16 code units in s, counting supplementary-plane
// runes (> U+FFFF) as a surrogate pair.
func utf16CodeUnits(s string) int {
	units := 0
	for _, r := range s {
		if r > 0xFFFF {
			units += 2
		} else {
			units++
		}
	}
	return units
}

// antigravityMaxPromptArgLen is the ceiling for antigravityPromptArgSize when
// the prompt is passed in argv, the only channel agy print mode offers. The
// limits differ sharply by OS: Windows caps the whole command line at 32767
// UTF-16 units, Linux caps a single argument at MAX_ARG_STRLEN (128 KiB), and
// macOS only bounds total argv+env (~1 MiB). The default prompt cap (200 KiB)
// exceeds the Linux and Windows ceilings, so a large diff fails with a clear
// error instead of an opaque one.
func antigravityMaxPromptArgLen() int {
	switch runtime.GOOS {
	case "windows":
		// Half of the 32767-unit command-line cap, which survives worst-case
		// quote/backslash escaping (it can nearly double a quoted argument) and
		// reserves room for the executable path and the other flags.
		return 15000
	case "linux":
		return 120 * 1024 // under MAX_ARG_STRLEN (128 KiB) per argument
	default:
		return maxPromptArgLen // macOS/other: bounded by total ARG_MAX
	}
}

// antigravityVersionUsesPromptFlag reports whether agy's `--version` output
// indicates the --prompt flag contract (>= 1.1.1). It scans for the first
// dotted version token, so decorated output ("agy version 1.1.0", a trailing
// build hash) still resolves. Output with no parseable version defaults to
// true, the current contract.
func antigravityVersionUsesPromptFlag(versionOutput string) bool {
	for tok := range strings.FieldsSeq(versionOutput) {
		if !strings.Contains(tok, ".") {
			continue
		}
		if cmp, ok := compareDotVersion(tok, antigravityPromptFlagVersion); ok {
			return cmp >= 0
		}
	}
	return true
}

// compareDotVersion compares two dotted numeric versions (major.minor.patch,
// with an optional leading 'v' and any -prerelease/+build suffix ignored). It
// returns -1, 0, or 1, and ok=false if either side has no parseable version.
func compareDotVersion(a, b string) (int, bool) {
	av, aok := parseDotVersion(a)
	bv, bok := parseDotVersion(b)
	if !aok || !bok {
		return 0, false
	}
	for i := range av {
		switch {
		case av[i] < bv[i]:
			return -1, true
		case av[i] > bv[i]:
			return 1, true
		}
	}
	return 0, true
}

// parseDotVersion parses a single major[.minor[.patch]] numeric version token,
// tolerating a 'v' prefix and dropping any -prerelease/+build metadata.
func parseDotVersion(v string) ([3]int, bool) {
	var out [3]int
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	if v == "" {
		return out, false
	}
	parts := strings.Split(v, ".")
	if len(parts) > 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

func parseAntigravityOutput(r io.Reader, sw *syncWriter) (string, error) {
	var buf strings.Builder
	if sw == nil {
		_, err := io.Copy(&buf, r)
		return strings.TrimSpace(buf.String()), err
	}
	_, err := io.Copy(io.MultiWriter(&buf, sw), r)
	return strings.TrimSpace(buf.String()), err
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
