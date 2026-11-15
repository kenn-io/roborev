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
	"strings"
	"time"

	"golang.org/x/mod/semver"

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

// SessionCLI identifies which Gemini-compatible executable owns a session.
func (a *GeminiAgent) SessionCLI() string {
	return commandBaseName(a.Command)
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
	return a.commandLine(false)
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
	capabilities := a.capabilities(ctx)
	if a.SessionID != "" && !capabilities.resume {
		if resetErr := resetSessionCapture(output); resetErr != nil {
			return "", resetErr
		}
		a = a.WithSessionID("").(*GeminiAgent)
	}
	args := a.buildArgsWithModel(a.Model, agenticMode)
	result, stderrStr, turnStarted, err := a.runGemini(ctx, repoPath, prompt, args, capabilities, output)
	modelNotFound := err != nil && !turnStarted && a.Model == defaultGeminiModel && isModelNotFoundError(stderrStr)
	if a.SessionID != "" && ctx.Err() == nil && !turnStarted &&
		(err != nil || result == "No review output generated") && !modelNotFound {
		if resetErr := resetSessionCapture(output); resetErr != nil {
			return "", errors.Join(err, resetErr)
		}
		log.Printf("gemini: resumed session failed (%v), retrying in a fresh session", err)
		a = a.WithSessionID("").(*GeminiAgent)
		args = a.buildArgsWithModel(a.Model, agenticMode)
		result, stderrStr, turnStarted, err = a.runGemini(ctx, repoPath, prompt, args, capabilities, output)
	}
	if err != nil && !turnStarted && a.Model == defaultGeminiModel && isModelNotFoundError(stderrStr) {
		// Built-in default model may be stale (Google renames
		// frequently). Retry without -m to let the Gemini CLI use
		// its own default. Non-default models (set via WithModel /
		// config) fail fast so config errors are surfaced.
		if resetErr := resetUnlessResuming(output, a.SessionID); resetErr != nil {
			return "", errors.Join(err, resetErr)
		}
		log.Printf("gemini: model %q not found, retrying without -m flag", a.Model)
		noModelArgs := a.buildArgsWithModel("", agenticMode)
		result, stderrStr, turnStarted, err = a.runGemini(ctx, repoPath, prompt, noModelArgs, capabilities, output)
		if a.SessionID != "" && ctx.Err() == nil && !turnStarted &&
			(err != nil || result == "No review output generated") {
			if resetErr := resetSessionCapture(output); resetErr != nil {
				return "", errors.Join(err, resetErr)
			}
			a = a.WithSessionID("").(*GeminiAgent)
			noModelArgs = a.buildArgsWithModel("", agenticMode)
			result, _, _, err = a.runGemini(ctx, repoPath, prompt, noModelArgs, capabilities, output)
		}
	}
	return result, err
}

// resetUnlessResuming clears captured session state abandoned by a failed
// attempt, unless the retry resumes the same non-empty session ID.
func resetUnlessResuming(output io.Writer, sessionID string) error {
	if sessionID != "" {
		return nil
	}
	return resetSessionCapture(output)
}

func resetSessionCapture(output io.Writer) error {
	if resetter, ok := output.(interface{ ResetSessionID() error }); ok {
		return resetter.ResetSessionID()
	}
	return nil
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

	if a.SessionID != "" {
		args = append(args, "--resume", a.SessionID)
	}

	return args
}

type geminiCLICapabilities struct {
	streamJSON bool
	resume     bool
	promptFlag bool
}

const (
	geminiResumeVersion = "0.20.0" // newer releases accept resumed prompts on stdin
)

// capabilities gates new CLI flags by the installed version. An undetectable
// version leaves them off.
func (a *GeminiAgent) capabilities(ctx context.Context) geminiCLICapabilities {
	version := cliVersion(ctx, a.Command)
	if a.usesAntigravity() {
		return geminiCLICapabilities{
			streamJSON: versionAtLeast(version, antigravityStreamJSONVersion),
			resume:     versionAtLeast(version, antigravityStreamJSONVersion),
			// An undetectable version keeps the established --prompt contract.
			promptFlag: !versionAtLeast(version, "0") || antigravityVersionUsesPromptFlag(version),
		}
	}
	return geminiCLICapabilities{resume: versionAtLeast(version, geminiResumeVersion)}
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

	return args
}

// runGemini returns the review result, captured stderr, whether the turn
// started, and any error from the CLI.
func (a *GeminiAgent) runGemini(ctx context.Context, repoPath, prompt string, args []string, capabilities geminiCLICapabilities, output io.Writer) (string, string, bool, error) {
	if a.usesAntigravity() {
		return a.runAntigravity(ctx, repoPath, prompt, args, capabilities, output)
	}

	var turnStarted bool
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
			turnStarted = parsed.turnStarted
			return parsed.result, err
		},
	})
	if runErr != nil {
		return "", "", turnStarted, runErr
	}

	if runResult.WaitErr != nil {
		return "", runResult.Stderr, turnStarted, formatStreamingCLIWaitError("gemini", runResult, runResult.Stderr)
	}

	if runResult.ParseErr != nil {
		if errors.Is(runResult.ParseErr, errNoStreamJSON) {
			return "", runResult.Stderr, turnStarted, fmt.Errorf("gemini CLI must support --output-format stream-json; upgrade to latest version\nstderr: %s: %w", runResult.Stderr, errNoStreamJSON)
		}
		return "", runResult.Stderr, turnStarted, runResult.ParseErr
	}

	if runResult.Result != "" {
		return runResult.Result, runResult.Stderr, turnStarted, nil
	}

	return "No review output generated", runResult.Stderr, turnStarted, nil
}

// antigravityPromptFlagVersion is the agy release where print mode began taking
// the prompt as the value of --prompt (an alias of --print/-p) and stopped
// reading it from stdin. Older agy builds read the prompt from stdin with a
// bare --print. agy is an unstable CLI target, so the invocation is gated on
// the reported version rather than assuming one contract.
const (
	antigravityPromptFlagVersion   = "1.1.1"
	antigravityStreamJSONVersion   = "1.1.8" // stream-json and --conversation
	antigravityVersionProbeTimeout = 10 * time.Second
)

func (a *GeminiAgent) runAntigravity(ctx context.Context, repoPath, prompt string, args []string, capabilities geminiCLICapabilities, output io.Writer) (string, string, bool, error) {
	// Headless print mode soft-denies tools that need a confirmation. Merge
	// read_file(*) and inspect command() allows into the official settings
	// file before launch so reviews emit output. Agentic runs already pass
	// --dangerously-skip-permissions and do not need this.
	if !a.Agentic {
		if err := ensureAntigravityReviewSettings(ctx); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return "", "", false, ctxErr
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return "", "", false, err
	}

	// Antigravity tools need an explicit workspace root. cmd.Dir controls the
	// process cwd, but the CLI resolves its tool workspace separately and can
	// fall back to its scratch directory for ephemeral worktrees.
	args = append(append([]string(nil), args...), "--add-dir", repoPath)
	parse := parsePlainTextOutput
	var turnStarted bool
	if a.SessionID != "" {
		args = append(args, "--conversation", a.SessionID)
	}
	if capabilities.streamJSON {
		args = append(args, "--output-format", "stream-json")
		parse = func(r io.Reader, sw *syncWriter) (string, error) {
			result, started, err := parseAntigravityStreamJSON(r, sw)
			turnStarted = started
			return result, err
		}
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
	if capabilities.promptFlag {
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

	runResult, runErr := runStreamingCLI(ctx, streamingCLISpec{
		Name:         "antigravity",
		Command:      a.Command,
		Args:         finalArgs,
		Dir:          repoPath,
		Stdin:        stdin,
		Output:       output,
		StreamStderr: true,
		Parse:        parse,
	})
	if runErr != nil {
		return "", "", turnStarted, runErr
	}

	if runResult.WaitErr != nil {
		return "", runResult.Stderr, turnStarted, formatStreamingCLIWaitError("antigravity", runResult, runResult.Stderr)
	}

	if runResult.ParseErr != nil {
		return "", runResult.Stderr, turnStarted, runResult.ParseErr
	}

	if runResult.Result != "" {
		return runResult.Result, runResult.Stderr, turnStarted, nil
	}

	// Agentic jobs keep the shared non-fatal placeholder: fix jobs
	// legitimately emit no text (they are judged by their worktree patch,
	// and erroring here would discard valid edits before capture). This
	// gates on workflow intent, not agenticMode: allow_unsafe_agents
	// changes tool permissions, not what a review must produce.
	if a.Agentic {
		return "No review output generated", runResult.Stderr, turnStarted, nil
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
	return "", runResult.Stderr, turnStarted, errors.New(msg)
}

var errAntigravityPrintTimeout = errors.New("antigravity print timed out; response is partial")

// cliVersion returns `--version` output, or "" when detection fails; agy's
// `agy version` subcommand needs a TTY and fails in pipes/subprocesses.
func cliVersion(ctx context.Context, command string) string {
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
		log.Printf("gemini: could not read %s version (%v)", command, procutil.WithStderr(err))
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
// build hash) still resolves. Unparseable output is false.
func antigravityVersionUsesPromptFlag(versionOutput string) bool {
	return versionAtLeast(versionOutput, antigravityPromptFlagVersion)
}

// versionAtLeast reports whether the first dotted version token in
// versionOutput is >= minimum. Versions follow semver ordering, so a
// prerelease ("0.34.0-preview.1") sorts below its final release. A leading 'v'
// and two-part versions ("1.2") are accepted; unparseable output is false.
func versionAtLeast(versionOutput, minimum string) bool {
	for tok := range strings.FieldsSeq(versionOutput) {
		v := "v" + strings.TrimPrefix(tok, "v")
		if strings.Contains(tok, ".") && semver.IsValid(v) {
			return semver.Compare(v, "v"+strings.TrimPrefix(minimum, "v")) >= 0
		}
	}
	return false
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
	result      string // The extracted result text
	turnStarted bool
}

// parseStreamJSON parses Gemini's stream-json output and extracts its result
// and whether the stream shows that a turn started.
// The sw parameter is the shared sync writer for thread-safe output (may be nil).
func (a *GeminiAgent) parseStreamJSON(r io.Reader, sw *syncWriter) (parseResult, error) {
	var lastResult string
	assistantMessages := newTrailingReviewText()
	var validEventsParsed bool
	var turnStarted bool

	err := scanStreamJSONLines(r, sw, func(trimmed string) error {
		var msg geminiStreamMessage
		if jsonErr := json.Unmarshal([]byte(trimmed), &msg); jsonErr == nil {
			validEventsParsed = true
			turnStarted = turnStarted || (msg.Type != "" && msg.Type != "init" && (msg.Type != "message" || msg.Role != "user"))

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
		return parseResult{turnStarted: turnStarted}, err
	}

	// If no valid events were parsed, return error
	if !validEventsParsed {
		return parseResult{turnStarted: turnStarted}, errNoStreamJSON
	}

	// Prefer the result field if present, otherwise join assistant messages
	if lastResult != "" {
		return parseResult{result: lastResult, turnStarted: turnStarted}, nil
	}
	if result := assistantMessages.Join("\n"); result != "" {
		return parseResult{result: result, turnStarted: turnStarted}, nil
	}
	return parseResult{turnStarted: turnStarted}, nil
}

type antigravityStreamEvent struct {
	Event  string                `json:"event"`
	Result antigravityResultData `json:"result"`
}

type antigravityResultData struct {
	Status         string `json:"status"`
	Response       string `json:"response"`
	Error          string `json:"error"`
	FinishedReason string `json:"finished_reason"`
	NumTurns       int    `json:"num_turns"`
}

func (r antigravityResultData) startedTurn() bool {
	return r.Status == "SUCCESS" || strings.TrimSpace(r.Response) != "" || r.NumTurns > 0
}

// parseAntigravityStreamJSON reads the documented result event and reports
// whether a non-init event shows that the turn started.
func parseAntigravityStreamJSON(r io.Reader, sw *syncWriter) (string, bool, error) {
	var final *antigravityStreamEvent
	var turnStarted bool
	err := scanStreamJSONLines(r, sw, func(line string) error {
		var event antigravityStreamEvent
		if json.Unmarshal([]byte(line), &event) == nil && event.Event != "" {
			switch event.Event {
			case "init":
			case "result":
				turnStarted = turnStarted || event.Result.startedTurn()
				if final == nil {
					final = &event
				}
			default:
				turnStarted = true
			}
		}
		return nil
	})
	if err != nil {
		return "", turnStarted, err
	}
	result, err := final.reviewResult()
	return result, turnStarted, err
}

func (event *antigravityStreamEvent) reviewResult() (string, error) {
	if event == nil {
		return "", errNoStreamJSON
	}
	if event.Result.Status != "SUCCESS" {
		return "", fmt.Errorf("antigravity ended with status %q: %s", event.Result.Status, event.Result.Error)
	}
	if event.Result.FinishedReason == "print_timeout" {
		return "", errAntigravityPrintTimeout
	}
	return strings.TrimSpace(event.Result.Response), nil
}

func init() {
	Register(NewGeminiAgent(""))
}
