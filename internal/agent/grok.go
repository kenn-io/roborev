package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

const (
	defaultGrokName    = "grok"
	defaultGrokCommand = "grok"

	// grokReviewTools is the positive built-in allowlist for non-agentic
	// review (Claude Read/Glob/Grep analogue). Tool IDs are model-facing
	// names from the default Grok Build toolset (xai-org/grok-build).
	//
	// This allowlist alone is not sufficient: Grok retains always-on MCP
	// meta-tools (search_tool/use_tool) even when --tools is set. Pair it
	// with grokMutatingDisallowedTools, --sandbox read-only, --no-subagents,
	// and --disable-web-search (see appendGrokReviewSafetyArgs).
	grokReviewTools = "read_file,grep,list_dir"

	grokOutputFormatStreamingJSON = "streaming-json"
	grokSandboxReadOnly           = "read-only"
)

// Auditable pin for the Grok Build tool registry used by classification and
// non-agentic review denylists. Re-check when upgrading the recommended CLI.
//
//   - grokToolsCLIVersion: first field of `grok --version`
//   - grokToolsBinaryIdentity: short hash printed by that binary (not a public
//     git object; informational only)
//   - grokToolsRepoRev: public xai-org/grok-build commit inspected for the
//     registry (crates/codegen/xai-grok-tools, headless --tools flags)
//   - grokToolsMonorepoRev: SOURCE_REV at grokToolsRepoRev (internal monorepo
//     commit the public mirror was synced from; see upstream README)
const (
	grokToolsCLIVersion     = "0.2.118"
	grokToolsBinaryIdentity = "1e1687c1cf6a"
	grokToolsRepoRev        = "a4221165824e5b1f5c4c10b7459f65e78dd6448d"
	grokToolsMonorepoRev    = "8d69c91f02bcacf01e98d5aebbf2f92547c45738"
)

func init() {
	// Keep audit pins live in production code so residual-drift revalidation
	// has a concrete, non-optional target (empty pins panic at process start).
	if grokToolsCLIVersion == "" ||
		grokToolsBinaryIdentity == "" ||
		grokToolsRepoRev == "" ||
		grokToolsMonorepoRev == "" {
		panic("grok tool registry audit pins must be non-empty")
	}
}

// grokClassifyToolsSeed is a non-empty --tools allowlist entry used only for
// classification. Empty --tools is unsafe on Grok (parse_comma_list("") → no
// override). The seed is also present in the denylist so deny wins and the
// effective toolset is empty after both flags apply.
const grokClassifyToolsSeed = "read_file"

// grokDefaultToolNames lists model-facing tool IDs from the default Grok Build
// toolset (xai-org/grok-build registry + MCP meta). Used as a single source of
// truth for fail-closed classification and residual mutating-tool denial.
//
// Source of truth: crates/codegen/xai-grok-tools ToolId::new registrations and
// headless CLI flags --tools / --disallowed-tools, validated against
// grokToolsCLIVersion / grokToolsRepoRev / grokToolsMonorepoRev.
//
// Important Grok semantics (do not reintroduce these mistakes):
//
//   - parse_comma_list("") → None → no tool override (empty --tools is NOT
//     deny-all; the default toolset remains available).
//   - A non-empty --tools allowlist still retains SearchTool/UseTool kinds
//     (MCP meta) on the main session unless they are also denied.
//   - When both --tools and --disallowed-tools are set, the denylist wins for
//     overlapping names (seed + MCP meta + known tools).
//   - Session --disallowed-tools is authoritative over project agent config.
//   - Residual drift risk remains if Grok adds tools/aliases; this is
//     structural isolation, not absolute parity with Claude's deny-all.
var grokDefaultToolNames = []string{
	// Shell / terminal
	"run_terminal_cmd",
	"run_terminal_command",
	"bash",
	"kill_terminal_command",
	"get_terminal_command_output",
	// Filesystem read / write / edit
	"read_file",
	"read",
	"list_dir",
	"grep",
	"grep_files",
	"glob",
	"search_replace",
	"write",
	"edit",
	"apply_patch",
	"hashline_read",
	"hashline_grep",
	"hashline_edit",
	"lsp",
	// Subagents / background tasks
	"task",
	"get_task_output",
	"wait_tasks",
	"kill_task",
	"monitor",
	// Schedulers
	"scheduler_create",
	"scheduler_delete",
	"scheduler_list",
	// Memory
	"memory_search",
	"memory_get",
	// Workflow / goal mutation
	"workflow",
	"update_goal",
	"todo_write",
	"todowrite",
	"skill",
	// Plan mode
	"enter_plan_mode",
	"exit_plan_mode",
	// Ask-user
	"ask_user_question",
	// Web
	"web_search",
	"web_fetch",
	// Media generation
	"image_gen",
	"image_edit",
	"image_to_video",
	"reference_to_video",
	// MCP meta (always retained by allowlist alone)
	"search_tool",
	"use_tool",
}

// grokClassifyDisallowedTools is the CSV deny list for ClassifyWithSchema.
// Combined with --sandbox, --no-subagents, --disable-web-search, and
// --max-turns 1 for layered fail-closed classification.
var grokClassifyDisallowedTools = strings.Join(grokDefaultToolNames, ",")

// grokMutatingDisallowedTools denies every default tool that is not in the
// non-agentic review allowlist, plus MCP meta-tools that survive --tools.
// Computed once from grokDefaultToolNames so names stay centralized.
var grokMutatingDisallowedTools = strings.Join(grokMutatingToolNames(), ",")

func grokMutatingToolNames() []string {
	allowed := map[string]struct{}{
		"read_file": {},
		"grep":      {},
		"list_dir":  {},
	}
	out := make([]string, 0, len(grokDefaultToolNames))
	for _, name := range grokDefaultToolNames {
		if _, ok := allowed[name]; ok {
			continue
		}
		out = append(out, name)
	}
	return out
}

// errNoGrokJSON indicates no valid grok streaming-json events were parsed.
var errNoGrokJSON = errors.New("no valid grok streaming-json events parsed from output")

// GrokAgent runs code reviews using xAI Grok Build (`grok` CLI) in headless
// mode — the same one-shot CommandAgent pattern as Claude Code and Codex.
//
// Non-agentic review safety layers (not absolute "all tools disabled"):
//
//	--sandbox read-only
//	--tools read_file,grep,list_dir          (positive allowlist)
//	--disallowed-tools <mutating+MCP meta>   (closes MCP residual)
//	--no-subagents
//	--disable-web-search
//
// Agentic (fix / allow_unsafe_agents):
//
//	--always-approve  (Claude --dangerously-skip-permissions analogue)
//
// Classification (SchemaAgent) is structurally isolated (not Claude deny-all
// equivalence): non-empty --tools seed + denylist of seed+defaults+MCP meta,
// one turn, no always-approve — see classifyArgs / appendGrokClassifySafetyArgs.
//
// Official install: https://x.ai/cli (see also github.com/xai-org/grok-build).
// Auth is handled by the Grok CLI (grok login / XAI_API_KEY), not roborev.
type GrokAgent struct {
	Command   string         // The grok command to run (default: "grok")
	Model     string         // Model ID (e.g. "grok-4.5")
	Reasoning ReasoningLevel // Mapped onto --reasoning-effort
	Agentic   bool           // Whether agentic mode is enabled (allow file edits)
	SessionID string         // Existing session ID to resume via --resume
}

// NewGrokAgent creates a Grok Build agent with standard reasoning.
func NewGrokAgent(command string) *GrokAgent {
	if command == "" {
		command = defaultGrokCommand
	}
	return &GrokAgent{Command: command, Reasoning: ReasoningStandard}
}

func (a *GrokAgent) clone(opts ...agentCloneOption) *GrokAgent {
	cfg := newAgentCloneConfig(
		a.Command,
		a.Model,
		a.Reasoning,
		a.Agentic,
		a.SessionID,
		opts...,
	)
	return &GrokAgent{
		Command:   cfg.Command,
		Model:     cfg.Model,
		Reasoning: cfg.Reasoning,
		Agentic:   cfg.Agentic,
		SessionID: cfg.SessionID,
	}
}

// WithReasoning returns a copy of the agent with the specified reasoning level.
func (a *GrokAgent) WithReasoning(level ReasoningLevel) Agent {
	return a.clone(withClonedReasoning(level))
}

// WithAgentic returns a copy of the agent configured for agentic mode.
func (a *GrokAgent) WithAgentic(agentic bool) Agent {
	return a.clone(withClonedAgentic(agentic))
}

// WithModel returns a copy of the agent configured to use the specified model.
func (a *GrokAgent) WithModel(model string) Agent {
	if model == "" {
		return a
	}
	return a.clone(withClonedModel(model))
}

// WithSessionID returns a copy of the agent configured to resume a prior session.
func (a *GrokAgent) WithSessionID(sessionID string) Agent {
	return a.clone(withClonedSessionID(sessionID))
}

// grokReasoningEffort maps ReasoningLevel onto Grok's --reasoning-effort
// values (none/minimal/low/medium/high/xhigh/max). Standard leaves Grok's default.
func (a *GrokAgent) grokReasoningEffort() string {
	switch a.Reasoning {
	case ReasoningMaximum, ReasoningMax:
		return "max"
	case ReasoningXHigh:
		return "xhigh"
	case ReasoningThorough, ReasoningHigh:
		return "high"
	case ReasoningMedium:
		return "medium"
	case ReasoningFast, ReasoningLow:
		return "low"
	default:
		return ""
	}
}

func (a *GrokAgent) Name() string {
	return defaultGrokName
}

func (a *GrokAgent) CommandName() string {
	return a.Command
}

func (a *GrokAgent) CommandLine() string {
	agenticMode := a.Agentic || AllowUnsafeAgents()
	args := a.buildArgs(agenticMode, "")
	return a.Command + " " + strings.Join(args, " ")
}

// appendGrokReviewSafetyArgs adds layered non-agentic review restrictions.
// Do not call for agentic jobs (WithAgentic(true) / AllowUnsafeAgents).
func appendGrokReviewSafetyArgs(args []string) []string {
	args = append(args, "--sandbox", grokSandboxReadOnly)
	args = append(args, "--tools", grokReviewTools)
	// Deny MCP meta and all mutating defaults that can outlive the allowlist.
	args = append(args, "--disallowed-tools", grokMutatingDisallowedTools)
	args = append(args, "--no-subagents")
	args = append(args, "--disable-web-search")
	return args
}

// appendGrokClassifySafetyArgs adds layered classification isolation flags.
//
// Empty --tools is NOT used (Grok treats it as no override / fail-open).
// Instead: non-empty --tools seed, then --disallowed-tools covering the seed
// plus every known default tool and MCP meta. Denylist wins on overlap.
func appendGrokClassifySafetyArgs(args []string) []string {
	args = append(args, "--sandbox", grokSandboxReadOnly)
	args = append(args, "--tools", grokClassifyToolsSeed)
	args = append(args, "--disallowed-tools", grokClassifyDisallowedTools)
	args = append(args, "--no-subagents")
	args = append(args, "--disable-web-search")
	args = append(args, "--max-turns", "1")
	args = append(args, "--no-memory")
	args = append(args, "--no-plan")
	return args
}

// buildArgs constructs headless Grok CLI arguments.
//
// When promptFile is empty (CommandLine preview), a -p placeholder is used.
// Review writes the real prompt to a temp file and passes --prompt-file so
// large prompts never hit OS argument limits (Grok headless does not read
// the prompt from stdin).
func (a *GrokAgent) buildArgs(agenticMode bool, promptFile string) []string {
	args := []string{
		"--no-auto-update",
		"--output-format", grokOutputFormatStreamingJSON,
	}

	if model := strings.TrimSpace(a.Model); model != "" {
		args = append(args, "-m", model)
	}
	if effort := a.grokReasoningEffort(); effort != "" {
		args = append(args, "--reasoning-effort", effort)
	}
	if sessionID := sanitizedResumeSessionID(a.SessionID); sessionID != "" {
		args = append(args, "--resume", sessionID)
	}

	if agenticMode {
		// Agentic mode: Grok auto-approves tools (Claude's --dangerously-skip-permissions analogue).
		args = append(args, "--always-approve")
	} else {
		args = appendGrokReviewSafetyArgs(args)
	}

	if promptFile != "" {
		args = append(args, "--prompt-file", promptFile)
	} else {
		// Display-only: real invocations always use --prompt-file.
		args = append(args, "-p", "<prompt>")
	}
	return args
}

// Review runs a code review through Grok Build headless mode.
func (a *GrokAgent) Review(ctx context.Context, repoPath, commitSHA, prompt string, output io.Writer) (string, error) {
	return a.run(ctx, repoPath, prompt, output)
}

// Synthesize combines review outputs without wrapping them as a code-review prompt.
func (a *GrokAgent) Synthesize(ctx context.Context, prompt string, output io.Writer) (string, error) {
	return a.run(ctx, "", prompt, output)
}

func (a *GrokAgent) run(ctx context.Context, repoPath, prompt string, output io.Writer) (string, error) {
	agenticMode := a.Agentic || AllowUnsafeAgents()

	// Grok headless does not read the prompt from stdin; use a temp file
	// like Pi to avoid OS command-line length limits.
	tmpFile, err := os.CreateTemp("", "roborev-grok-prompt-*.md")
	if err != nil {
		return "", fmt.Errorf("create temp prompt file: %w", err)
	}
	promptPath := tmpFile.Name()
	defer os.Remove(promptPath)

	if _, err := tmpFile.WriteString(prompt); err != nil {
		tmpFile.Close()
		return "", fmt.Errorf("write prompt to temp file: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return "", fmt.Errorf("close temp prompt file: %w", err)
	}

	args := a.buildArgs(agenticMode, promptPath)

	runResult, runErr := runStreamingCLI(ctx, streamingCLISpec{
		Name:    "grok",
		Command: a.Command,
		Args:    args,
		Dir:     repoPath,
		Output:  output,
		Parse: func(r io.Reader, sw *syncWriter) (string, error) {
			if sw == nil {
				return parseGrokStreamingJSON(r, nil)
			}
			return parseGrokStreamingJSON(r, sw)
		},
	})
	if runErr != nil {
		return "", runErr
	}

	if runResult.WaitErr != nil {
		return "", formatStreamingCLIWaitError("grok", runResult, runResult.Stderr)
	}

	if runResult.ParseErr != nil {
		if errors.Is(runResult.ParseErr, errNoGrokJSON) {
			return "", fmt.Errorf("grok CLI did not emit valid streaming-json events; upgrade grok or check CLI compatibility: %w", errNoGrokJSON)
		}
		return runResult.Result, runResult.ParseErr
	}

	if runResult.Result == "" {
		return "No review output generated", nil
	}

	return runResult.Result, nil
}

// grokStreamEvent is one NDJSON line from --output-format streaming-json.
// See Grok Build headless docs for the full event catalogue.
type grokStreamEvent struct {
	Type    string `json:"type"`
	Data    string `json:"data,omitempty"`
	Message string `json:"message,omitempty"`
}

// parseGrokStreamingJSON parses Grok's native streaming-json NDJSON stream
// and extracts concatenated type=text payloads. Any type=error event is a hard
// failure; collected text is returned only as diagnostic context.
func parseGrokStreamingJSON(r io.Reader, output io.Writer) (string, error) {
	reviewText := newTrailingReviewText()
	var errorMessages []string
	var validEventsParsed bool

	err := scanStreamJSONLines(r, output, func(line string) error {
		var ev grokStreamEvent
		if jsonErr := json.Unmarshal([]byte(line), &ev); jsonErr != nil {
			return nil
		}
		if ev.Type == "" {
			return nil
		}
		validEventsParsed = true

		switch ev.Type {
		case "text":
			if ev.Data != "" {
				reviewText.Add(ev.Data)
			}
		case "tool_call", "tool_call_update":
			reviewText.ResetAfterTool()
		case "error":
			msg := ev.Message
			if msg == "" {
				msg = ev.Data
			}
			if msg == "" {
				msg = "grok stream error"
			}
			errorMessages = append(errorMessages, msg)
		}
		return nil
	})
	if err != nil {
		return reviewText.Join(""), err
	}

	if !validEventsParsed {
		return "", errNoGrokJSON
	}

	result := reviewText.Join("")
	if len(errorMessages) > 0 {
		return result, fmt.Errorf("grok stream errors: %s", strings.Join(errorMessages, "; "))
	}
	return result, nil
}

// classifyArgs builds argv for a schema-constrained one-shot classify call.
// Structural isolation: non-empty tools seed + denylist (seed+defaults+MCP),
// disable subagents/web/memory/plan, cap to one turn, never always-approve.
func (a *GrokAgent) classifyArgs(schema json.RawMessage, promptFile string) []string {
	args := []string{
		"--no-auto-update",
		"--output-format", "json",
		"--json-schema", string(schema),
	}
	args = appendGrokClassifySafetyArgs(args)
	if model := strings.TrimSpace(a.Model); model != "" {
		args = append(args, "-m", model)
	}
	if effort := a.grokReasoningEffort(); effort != "" {
		args = append(args, "--reasoning-effort", effort)
	}
	args = append(args, "--prompt-file", promptFile)
	return args
}

// ClassifyWithSchema runs a single constrained Grok headless invocation and
// returns JSON conforming to schema. Implements SchemaAgent.
func (a *GrokAgent) ClassifyWithSchema(
	ctx context.Context,
	repoPath, gitRef, prompt string,
	schema json.RawMessage,
	out io.Writer,
) (json.RawMessage, error) {
	tmpFile, err := os.CreateTemp("", "roborev-grok-classify-*.md")
	if err != nil {
		return nil, fmt.Errorf("create temp classify prompt: %w", err)
	}
	promptPath := tmpFile.Name()
	defer os.Remove(promptPath)

	if _, err := tmpFile.WriteString(prompt); err != nil {
		tmpFile.Close()
		return nil, fmt.Errorf("write classify prompt: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return nil, fmt.Errorf("close classify prompt: %w", err)
	}

	args := a.classifyArgs(schema, promptPath)
	cmd := exec.CommandContext(ctx, a.Command, args...)
	cmd.Dir = repoPath
	tracker := configureSubprocess(cmd)

	var stdoutBuf, stderrBuf bytes.Buffer
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
		return nil, fmt.Errorf("grok classifier failed: %w\nstderr: %s", err, strings.TrimSpace(stderrBuf.String()))
	}

	return parseGrokClassifyJSON(stdoutBuf.Bytes())
}

// grokJSONResult is the single-object --output-format json payload.
// Grok emits camelCase structuredOutput / structuredOutputError (see
// xai-grok-pager headless reducer attach_structured_output).
type grokJSONResult struct {
	Type                  string          `json:"type"`
	Message               string          `json:"message"`
	Text                  string          `json:"text"`
	StructuredOutput      json.RawMessage `json:"structuredOutput"`
	StructuredOutputError string          `json:"structuredOutputError"`
}

func parseGrokClassifyJSON(raw []byte) (json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("grok classifier produced empty output")
	}

	// Decode exactly one JSON document, skipping leading non-JSON noise.
	decIn := trimmed
	if idx := bytes.IndexByte(trimmed, '{'); idx > 0 {
		decIn = trimmed[idx:]
	}
	dec := json.NewDecoder(bytes.NewReader(decIn))
	var result grokJSONResult
	if err := dec.Decode(&result); err != nil {
		return nil, fmt.Errorf("grok classifier output is not JSON: %w", err)
	}
	// Reject trailing JSON documents or non-whitespace junk after the first value.
	rest := bytes.TrimSpace(decIn[dec.InputOffset():])
	if len(rest) > 0 {
		return nil, fmt.Errorf("grok classifier output contains trailing JSON")
	}

	if result.Type == "error" {
		msg := result.Message
		if msg == "" {
			msg = "classifier error"
		}
		return nil, fmt.Errorf("grok classifier error: %s", msg)
	}

	if errMsg := strings.TrimSpace(result.StructuredOutputError); errMsg != "" {
		return nil, fmt.Errorf("grok structured output validation failed: %s", errMsg)
	}

	so := bytes.TrimSpace(result.StructuredOutput)
	if len(so) == 0 {
		return nil, fmt.Errorf("grok classifier output missing structuredOutput")
	}
	if bytes.Equal(so, []byte("null")) {
		return nil, fmt.Errorf("grok structuredOutput is null")
	}
	return validateGrokClassifyJSON("structuredOutput", so)
}

func validateGrokClassifyJSON(label string, raw json.RawMessage) (json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("grok %s is empty", label)
	}
	if !json.Valid(trimmed) {
		return nil, fmt.Errorf("grok %s is not valid JSON: %q", label, string(raw))
	}
	if trimmed[0] != '{' {
		return nil, fmt.Errorf("grok %s is not a JSON object: %q", label, string(trimmed))
	}
	// Exactly one JSON value; reject trailing documents inside the field.
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	var obj map[string]json.RawMessage
	if err := dec.Decode(&obj); err != nil {
		return nil, fmt.Errorf("grok %s is not a JSON object: %w", label, err)
	}
	if dec.More() {
		return nil, fmt.Errorf("grok %s contains trailing JSON", label)
	}
	return json.RawMessage(append([]byte(nil), trimmed...)), nil
}

// Compile-time assertion that GrokAgent implements SchemaAgent and SessionAgent.
var (
	_ SchemaAgent  = (*GrokAgent)(nil)
	_ SessionAgent = (*GrokAgent)(nil)
)

func init() {
	Register(NewGrokAgent(""))
}
