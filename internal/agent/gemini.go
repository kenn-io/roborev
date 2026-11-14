package agent

import (
	"context"
	"encoding/json/v2"
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

	"go.kenn.io/roborev/internal/procutil"
)

// errNoStreamJSON indicates no valid stream-json events were parsed.
// Stream-json output is required; this error means the Gemini CLI may need to be upgraded.
var errNoStreamJSON = errors.New("no valid stream-json events parsed from output")

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
	SessionID     string         // Existing session ID to resume
}

var _ SessionAgent = (*GeminiAgent)(nil)

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
		a.SessionID,
		opts...,
	)
	return &GeminiAgent{
		Command:       cfg.Command,
		Model:         cfg.Model,
		ModelExplicit: a.ModelExplicit,
		CommandAuto:   a.CommandAuto,
		Reasoning:     cfg.Reasoning,
		Agentic:       cfg.Agentic,
		SessionID:     cfg.SessionID,
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

// WithSessionID returns a copy of the agent configured to resume a prior session.
func (a *GeminiAgent) WithSessionID(sessionID string) Agent {
	return a.clone(withClonedSessionID(sessionID))
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
	return a.commandLine(agenticMode)
}

// PlanningCommandLine returns Gemini's representative read-only planning command.
func (a *GeminiAgent) PlanningCommandLine() string {
	return a.WithSessionID("").(*GeminiAgent).commandLine(false)
}

func (a *GeminiAgent) commandLine(agenticMode bool) string {
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

	agenticMode := effectiveAgentic(ctx, a.Agentic)
	capabilities := geminiCLICapabilities{}
	if a.usesAntigravity() {
		capabilities = geminiCLICapabilities{streamJSON: true, resume: true, plan: true}
	} else {
		capabilities = geminiCapabilitiesFor(geminiVersionOutput(ctx, a.Command))
	}
	runAgent := a
	if !a.usesAntigravity() && !capabilities.resume {
		if resetter, ok := output.(interface{ ResetSessionID() error }); ok {
			if resetErr := resetter.ResetSessionID(); resetErr != nil {
				return "", fmt.Errorf("clear unsupported Gemini session before run: %w", resetErr)
			}
		}
		runAgent = a.WithSessionID("").(*GeminiAgent)
	}
	args := runAgent.buildArgsWithCapabilities(runAgent.Model, agenticMode, capabilities)
	result, stderrStr, err := runAgent.runGemini(ctx, repoPath, prompt, args, capabilities.streamJSON, output)
	resumeFailed := !agenticMode && (err != nil || result == "No review output generated")
	if resumeFailed && runAgent.SessionID != "" && ctx.Err() == nil {
		if resetter, ok := output.(interface{ ResetSessionID() error }); ok {
			if resetErr := resetter.ResetSessionID(); resetErr != nil {
				return "", fmt.Errorf("reset failed session before fresh run: %w", resetErr)
			}
		}
		if err != nil {
			log.Printf("gemini: resuming session failed, retrying in a fresh session: %v", err)
		} else {
			log.Printf("gemini: resumed session produced no review output, retrying in a fresh session")
		}
		runAgent = a.WithSessionID("").(*GeminiAgent)
		result, stderrStr, err = runAgent.runGemini(
			ctx,
			repoPath,
			prompt,
			runAgent.buildArgsWithCapabilities(runAgent.Model, agenticMode, capabilities),
			capabilities.streamJSON,
			output,
		)
	}
	if err != nil && runAgent.Model == defaultGeminiModel && isModelNotFoundError(stderrStr) && ctx.Err() == nil {
		if resetter, ok := output.(interface{ ResetSessionID() error }); ok {
			if resetErr := resetter.ResetSessionID(); resetErr != nil {
				return "", fmt.Errorf("reset failed model session before fallback: %w", resetErr)
			}
		}
		// Built-in default model may be stale (Google renames
		// frequently). Retry without -m to let the Gemini CLI use
		// its own default. Non-default models (set via WithModel /
		// config) fail fast so config errors are surfaced.
		log.Printf("gemini: model %q not found, retrying without -m flag", runAgent.Model)
		noModelArgs := runAgent.buildArgsWithCapabilities("", agenticMode, capabilities)
		result, _, err = runAgent.runGemini(ctx, repoPath, prompt, noModelArgs, capabilities.streamJSON, output)
	}
	if !a.usesAntigravity() && !capabilities.resume {
		if resetter, ok := output.(interface{ ResetSessionID() error }); ok {
			if resetErr := resetter.ResetSessionID(); resetErr != nil {
				return "", errors.Join(err, fmt.Errorf("clear unsupported Gemini session after run: %w", resetErr))
			}
		}
	}
	return result, err
}

// buildArgsWithModel builds CLI args with an explicit model override
// (empty string omits the -m flag entirely).
func (a *GeminiAgent) buildArgsWithModel(model string, agenticMode bool) []string {
	return a.buildArgsWithCapabilities(model, agenticMode, geminiCLICapabilities{
		streamJSON: true,
		resume:     true,
		plan:       true,
	})
}

func (a *GeminiAgent) buildArgsWithCapabilities(
	model string,
	agenticMode bool,
	capabilities geminiCLICapabilities,
) []string {
	if a.usesAntigravity() {
		return a.buildAntigravityArgs(agenticMode)
	}

	var args []string
	if capabilities.streamJSON {
		args = append(args, "--output-format", "stream-json")
	}

	if model != "" {
		args = append(args, "-m", model)
	}

	if agenticMode {
		args = append(args, "--approval-mode", "yolo")
	} else if capabilities.plan {
		args = append(args, "--approval-mode", "plan")
	}

	if capabilities.resume && a.SessionID != "" && !strings.HasPrefix(a.SessionID, "-") {
		args = append(args, "--resume", a.SessionID)
	}

	return args
}

type geminiCLICapabilities struct {
	streamJSON bool
	resume     bool
	plan       bool
}

const (
	geminiStreamJSONVersion   = "0.11.0"
	geminiResumeVersion       = "0.15.0"
	geminiPlanModeVersion     = "0.34.0"
	geminiVersionProbeTimeout = 10 * time.Second
)

func geminiCapabilitiesFor(versionOutput string) geminiCLICapabilities {
	streamJSON, _ := versionAtLeast(versionOutput, geminiStreamJSONVersion)
	resume, _ := versionAtLeast(versionOutput, geminiResumeVersion)
	plan, _ := versionAtLeast(versionOutput, geminiPlanModeVersion)
	return geminiCLICapabilities{streamJSON: streamJSON, resume: resume, plan: plan}
}

func geminiVersionOutput(ctx context.Context, command string) string {
	vctx, cancel := context.WithTimeout(ctx, geminiVersionProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(vctx, command, "--version")
	configureCapabilityProbe(ctx, cmd)
	out, err := cmd.Output()
	if err != nil {
		log.Printf("gemini: could not read CLI version (%v)", procutil.WithStderr(err))
		return ""
	}
	return string(out)
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
	}
	// Non-agentic print-mode reviews omit --sandbox: agy's sandbox permission
	// gate rejects `pwd` (and similar read-only workspace probes) in headless
	// print mode. Reviews still do not get --dangerously-skip-permissions.

	if a.SessionID != "" && !strings.HasPrefix(a.SessionID, "-") {
		args = append(args, "--conversation", a.SessionID)
	}

	return args
}

// runGemini executes the Gemini CLI with the given args and returns
// the review result, captured stderr, and any error.
func (a *GeminiAgent) runGemini(
	ctx context.Context,
	repoPath, prompt string,
	args []string,
	streamJSON bool,
	output io.Writer,
) (string, string, error) {
	if a.usesAntigravity() {
		return a.runAntigravity(ctx, repoPath, prompt, args, output)
	}

	parseOutput := parsePlainTextOutput
	if streamJSON {
		parseOutput = func(r io.Reader, sw *syncWriter) (string, error) {
			parsed, err := a.parseStreamJSON(r, sw)
			return parsed.result, err
		}
	}
	runResult, runErr := runStreamingCLI(ctx, streamingCLISpec{
		Name:         "gemini",
		Command:      a.Command,
		Args:         args,
		Dir:          repoPath,
		Stdin:        strings.NewReader(prompt),
		Output:       output,
		StreamStderr: true,
		Parse:        parseOutput,
	})
	if runErr != nil {
		return "", "", runErr
	}

	if runResult.WaitErr != nil {
		return "", runResult.Stderr, formatStreamingCLIWaitError("gemini", runResult, runResult.Stderr)
	}

	if runResult.ParseErr != nil {
		if errors.Is(runResult.ParseErr, errNoStreamJSON) {
			return "", runResult.Stderr, fmt.Errorf("gemini CLI must support --output-format stream-json; upgrade to latest version\nstderr: %s: %w", runResult.Stderr, errNoStreamJSON)
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
	antigravityPromptFlagVersion = "1.1.1"
	// Antigravity added --output-format stream-json in 1.1.8.
	antigravityStreamJSONVersion   = "1.1.8"
	antigravityVersionProbeTimeout = 10 * time.Second
)

func (a *GeminiAgent) runAntigravity(ctx context.Context, repoPath, prompt string, args []string, output io.Writer) (string, string, error) {
	// Headless print mode soft-denies tools that need a confirmation. Merge
	// read_file(*) and inspect command() allows into the official settings
	// file before launch so reviews emit output. Agentic runs already pass
	// --dangerously-skip-permissions and do not need this.
	if !a.Agentic {
		if err := ensureAntigravityReviewSettings(ctx); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return "", "", ctxErr
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return "", "", err
	}

	// Antigravity tools need an explicit workspace root. cmd.Dir controls the
	// process cwd, but the CLI resolves its tool workspace separately and can
	// fall back to its scratch directory for ephemeral worktrees.
	args = append(append([]string(nil), args...), "--add-dir", repoPath)
	version := antigravityVersionOutput(ctx, a.Command)
	if err := ctx.Err(); err != nil {
		return "", "", err
	}
	streamJSON := antigravityVersionSupports(version, antigravityStreamJSONVersion)
	if streamJSON {
		args = append([]string{"--output-format", "stream-json"}, args...)
	}

	// Choose the prompt-carrying flag by agy version: >= 1.1.1 takes the prompt
	// as the value of --prompt (stdin is ignored when a prompt flag is present);
	// older agy reads it from stdin with a bare --print. A bare --print on new
	// agy would swallow the following --print-timeout token as the prompt, so
	// the two forms must not be mixed.
	//
	// Exception: when the prompt exceeds the platform argv cap, omit every
	// prompt flag (--prompt/--print/-p). New agy still reads a non-TTY stdin
	// as the prompt if no prompt flag is passed (antigravity-cli#582).
	trimmedPrompt := strings.TrimRight(prompt, "\n")
	var finalArgs []string
	var stdin io.Reader
	if antigravityVersionUsesPromptFlag(version) {
		if size, limit := antigravityPromptArgSize(trimmedPrompt), antigravityMaxPromptArgLen(); size > limit {
			finalArgs = append([]string(nil), args...)
			stdin = strings.NewReader(trimmedPrompt + "\n")
		} else {
			finalArgs = append(append([]string(nil), args...), "--prompt", trimmedPrompt)
		}
	} else {
		finalArgs = append([]string{"--print"}, args...)
		stdin = strings.NewReader(trimmedPrompt + "\n")
	}

	parseOutput := parsePlainTextOutput
	if streamJSON {
		parseOutput = a.parseAntigravityStreamJSON
	}
	runResult, runErr := runStreamingCLI(ctx, streamingCLISpec{
		Name:         "antigravity",
		Command:      a.Command,
		Args:         finalArgs,
		Dir:          repoPath,
		Stdin:        stdin,
		Output:       output,
		StreamStderr: true,
		Parse:        parseOutput,
	})
	if runErr != nil {
		return "", "", runErr
	}

	if runResult.WaitErr != nil {
		return "", runResult.Stderr, formatStreamingCLIWaitError("antigravity", runResult, runResult.Stderr)
	}

	if runResult.ParseErr != nil {
		return "", runResult.Stderr, runResult.ParseErr
	}

	if runResult.Result != "" {
		return runResult.Result, runResult.Stderr, nil
	}

	// Agentic jobs keep the shared non-fatal placeholder: fix jobs
	// legitimately emit no text (they are judged by their worktree patch,
	// and erroring here would discard valid edits before capture). This
	// gates on workflow intent, not agenticMode: allow_unsafe_agents
	// changes tool permissions, not what a review must produce.
	if a.Agentic {
		return "No review output generated", runResult.Stderr, nil
	}

	// A clean exit with no output is a failure, not an empty review: agy >=
	// 1.1.3 soft-denies tools that need a permission confirmation in headless
	// print mode and exits 0 having produced nothing. Returning an error lets
	// the worker retry and fail over instead of recording an empty review.
	msg := "antigravity produced no review output"
	if strings.Contains(runResult.Stderr, "permissions.allow") {
		msg += "; add read_file(*) to permissions.allow in agy's settings.json"
	}
	if s := runResult.Stderr; s != "" {
		msg += "\nstderr: " + s
	}
	return "", runResult.Stderr, errors.New(msg)
}

// antigravityVersionOutput probes with --version. The `agy version` subcommand
// needs a TTY and fails in pipes, so it cannot be used here.
func antigravityVersionOutput(ctx context.Context, command string) string {
	// Cap the probe: `agy --version` returns instantly, but agy's sibling
	// `agy version` subcommand hangs without a TTY, so don't let a wedged probe
	// consume the whole review timeout.
	vctx, cancel := context.WithTimeout(ctx, antigravityVersionProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(vctx, command, "--version")
	// Run the probe from a stable cwd and without a console window, and avoid
	// inheriting a deleted daemon working directory (a bad cwd otherwise makes
	// the probe fail and mis-default a legacy agy to the --prompt contract).
	configureCapabilityProbe(ctx, cmd)
	out, err := cmd.Output()
	if err != nil {
		log.Printf("antigravity: could not read agy version (%v)", procutil.WithStderr(err))
		return ""
	}
	return string(out)
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
// the prompt is passed in argv. Prompts larger than this are delivered on
// stdin without a --prompt/--print/-p flag: new agy still reads non-TTY stdin
// when no prompt flag is present (antigravity-cli#582). The limits differ
// sharply by OS: Windows caps the whole command line at 32767 UTF-16 units,
// Linux caps a single argument at MAX_ARG_STRLEN (128 KiB), and macOS only
// bounds total argv+env (~1 MiB). The default prompt cap (200 KiB) exceeds
// the Linux and Windows ceilings.
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
	return antigravityVersionAtLeast(versionOutput, antigravityPromptFlagVersion, true)
}

func antigravityVersionSupports(versionOutput, minimum string) bool {
	return antigravityVersionAtLeast(versionOutput, minimum, false)
}

func antigravityVersionAtLeast(versionOutput, minimum string, unknown bool) bool {
	supported, known := versionAtLeast(versionOutput, minimum)
	if !known {
		return unknown
	}
	return supported
}

func versionAtLeast(versionOutput, minimum string) (bool, bool) {
	for tok := range strings.FieldsSeq(versionOutput) {
		if !strings.Contains(tok, ".") {
			continue
		}
		if cmp, ok := compareDotVersion(tok, minimum); ok {
			return cmp >= 0, true
		}
	}
	return false, false
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

// geminiStreamMessage represents the documented Gemini CLI stream events.
type geminiStreamMessage struct {
	Type    string `json:"type"`
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
	Status  string `json:"status,omitempty"`
	Delta   bool   `json:"delta,omitempty"`
}

// parseResult contains the parsed result from stream-json output
type parseResult struct {
	result string // The extracted result text
}

// parseStreamJSON parses Gemini's stream-json output and extracts the final result.
// Returns parseResult with the extracted content, or error on I/O or parse failure.
// The sw parameter is the shared sync writer for thread-safe output (may be nil).
func (a *GeminiAgent) parseStreamJSON(r io.Reader, sw *syncWriter) (parseResult, error) {
	assistantMessages := newTrailingReviewText()
	var resultSeen bool

	err := scanStreamJSONLines(r, sw, func(trimmed string) error {
		var msg geminiStreamMessage
		if json.Unmarshal([]byte(trimmed), &msg) != nil {
			return nil
		}
		switch msg.Type {
		case "message":
			if msg.Role == "assistant" && msg.Content != "" {
				if msg.Delta {
					assistantMessages.AppendWithID("assistant", msg.Content)
				} else {
					assistantMessages.AddWithID("assistant", msg.Content)
				}
			}
		case "tool_use", "tool_result":
			assistantMessages.ResetAfterTool()
		case "result":
			resultSeen = true
			if !strings.EqualFold(msg.Status, "success") {
				return fmt.Errorf("gemini CLI ended with status %q", msg.Status)
			}
		}
		return nil
	})
	if err != nil {
		return parseResult{}, err
	}

	if !resultSeen {
		return parseResult{}, errNoStreamJSON
	}

	if result := assistantMessages.Join("\n"); result != "" {
		return parseResult{result: result}, nil
	}

	return parseResult{}, nil
}

type antigravityStreamEvent struct {
	Event          string `json:"event"`
	ConversationID string `json:"conversation_id,omitempty"`
	Result         struct {
		Status         string `json:"status"`
		Response       string `json:"response"`
		Error          string `json:"error,omitempty"`
		FinishedReason string `json:"finished_reason,omitempty"`
	} `json:"result,omitempty"`
}

func (a *GeminiAgent) parseAntigravityStreamJSON(r io.Reader, sw *syncWriter) (string, error) {
	var response string
	var resultSeen bool
	err := scanStreamJSONLines(r, sw, func(line string) error {
		var event antigravityStreamEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			return nil
		}
		if event.Event != "result" {
			return nil
		}
		resultSeen = true
		if event.Result.FinishedReason != "" {
			return fmt.Errorf("antigravity result was incomplete: %s", event.Result.FinishedReason)
		}
		if event.Result.Status != "SUCCESS" {
			return fmt.Errorf("antigravity ended with status %q: %s", event.Result.Status, event.Result.Error)
		}
		response = event.Result.Response
		return nil
	})
	if err != nil {
		return "", err
	}
	if !resultSeen {
		return "", errNoStreamJSON
	}
	return strings.TrimSpace(response), nil
}

func init() {
	Register(NewGeminiAgent(""))
}
