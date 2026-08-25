package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
)

// CodexAgent runs code reviews using the Codex CLI
type CodexAgent struct {
	Command                   string         // The codex command to run (default: "codex")
	Model                     string         // Model to use (e.g., "o3", "o4-mini")
	Reasoning                 ReasoningLevel // Reasoning level for the agent
	Agentic                   bool           // Whether agentic mode is enabled (allow file edits)
	SessionID                 string         // Existing session/thread ID to resume
	SuppressSkillInstructions bool           // Whether to suppress Codex skill instructions
	IgnoreUserConfig          bool           // Whether to pass --ignore-user-config
	ConfigOverrides           []string       // Extra `-c key=value` overrides injected from roborev config
}

const (
	codexDangerousFlag         = "--dangerously-bypass-approvals-and-sandbox"
	codexAutoApproveFlag       = "--full-auto"
	codexIgnoreUserConfigFlag  = "--ignore-user-config"
	codexDisableSkillsConfig   = "skills.include_instructions=false"
	codexReadOnlySandboxConfig = `sandbox_mode="read-only"`
)

var (
	codexDangerousSupport        sync.Map
	codexAutoApproveSupport      sync.Map
	codexIgnoreUserConfigSupport sync.Map
)

// errNoCodexJSON indicates no valid codex --json events were parsed.
var errNoCodexJSON = errors.New("no valid codex --json events parsed from output")

// errCodexStreamFailed indicates codex emitted a failure event in the JSON stream.
var errCodexStreamFailed = errors.New("codex stream reported failure")

// NewCodexAgent creates a new Codex agent with standard reasoning
func NewCodexAgent(command string) *CodexAgent {
	if command == "" {
		command = "codex"
	}
	return &CodexAgent{Command: command, Reasoning: ReasoningStandard}
}

func (a *CodexAgent) clone(opts ...agentCloneOption) *CodexAgent {
	cfg := newAgentCloneConfig(
		a.Command,
		a.Model,
		a.Reasoning,
		a.Agentic,
		a.SessionID,
		opts...,
	)
	return &CodexAgent{
		Command:                   cfg.Command,
		Model:                     cfg.Model,
		Reasoning:                 cfg.Reasoning,
		Agentic:                   cfg.Agentic,
		SessionID:                 cfg.SessionID,
		SuppressSkillInstructions: a.SuppressSkillInstructions,
		IgnoreUserConfig:          a.IgnoreUserConfig,
		ConfigOverrides:           a.ConfigOverrides,
	}
}

// WithReasoning returns a copy of the agent with the specified reasoning level
func (a *CodexAgent) WithReasoning(level ReasoningLevel) Agent {
	return a.clone(withClonedReasoning(level))
}

// WithAgentic returns a copy of the agent configured for agentic mode.
func (a *CodexAgent) WithAgentic(agentic bool) Agent {
	return a.clone(withClonedAgentic(agentic))
}

// WithModel returns a copy of the agent configured to use the specified model.
func (a *CodexAgent) WithModel(model string) Agent {
	if model == "" {
		return a
	}
	return a.clone(withClonedModel(model))
}

// WithSessionID returns a copy of the agent configured to resume a prior session.
func (a *CodexAgent) WithSessionID(sessionID string) Agent {
	return a.clone(withClonedSessionID(sessionID))
}

// WithCodexSkillsDisabled returns a copy of agent with Codex skill instructions
// suppressed when the agent is Codex.
func WithCodexSkillsDisabled(a Agent, disabled bool) Agent {
	codexAgent, ok := a.(*CodexAgent)
	if !ok {
		return a
	}
	clone := *codexAgent
	clone.SuppressSkillInstructions = disabled
	return &clone
}

// WithCodexUserConfigIgnored returns a copy of agent configured to ignore the
// Codex user config when the agent is Codex.
func WithCodexUserConfigIgnored(a Agent, ignored bool) Agent {
	codexAgent, ok := a.(*CodexAgent)
	if !ok {
		return a
	}
	clone := *codexAgent
	clone.IgnoreUserConfig = ignored
	return &clone
}

// codexReasoningEffort maps ReasoningLevel to codex-specific effort values
func (a *CodexAgent) codexReasoningEffort() string {
	switch a.Reasoning {
	case ReasoningMaximum:
		if codexModelSupportsMaxReasoning(a.Model) {
			return "max"
		}
		return "xhigh"
	case ReasoningXHigh:
		return "xhigh"
	case ReasoningThorough, ReasoningHigh:
		return "high"
	case ReasoningMedium:
		return "medium"
	case ReasoningFast, ReasoningLow:
		return "low"
	case ReasoningMax:
		return "max"
	default:
		return "" // use codex default
	}
}

func codexModelSupportsMaxReasoning(model string) bool {
	switch model {
	case "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna":
		return true
	default:
		return false
	}
}

func (a *CodexAgent) Name() string {
	return "codex"
}

func (a *CodexAgent) CommandName() string {
	return a.Command
}

func (a *CodexAgent) CommandLine() string {
	agenticMode := a.Agentic || AllowUnsafeAgents()
	args := a.commandArgs(codexArgOptions{
		agenticMode:   agenticMode,
		autoApprove:   !agenticMode,
		sandboxBroken: CodexSandboxDisabled(),
		preview:       true,
	})
	return a.Command + " " + strings.Join(args, " ")
}

func (a *CodexAgent) buildArgs(
	repoPath string,
	agenticMode, autoApprove, sandboxBroken bool,
) []string {
	return a.buildArgsWithSchema(
		repoPath, agenticMode, autoApprove, sandboxBroken, "",
	)
}

func (a *CodexAgent) buildArgsWithSchema(
	repoPath string,
	agenticMode, autoApprove, sandboxBroken bool,
	schemaPath string,
) []string {
	return a.commandArgs(codexArgOptions{
		repoPath:      repoPath,
		agenticMode:   agenticMode,
		autoApprove:   autoApprove,
		sandboxBroken: sandboxBroken,
		schemaPath:    schemaPath,
	})
}

type codexArgOptions struct {
	repoPath      string
	agenticMode   bool
	autoApprove   bool
	sandboxBroken bool
	preview       bool
	schemaPath    string
}

func (a *CodexAgent) commandArgs(opts codexArgOptions) []string {
	args := []string{"exec"}
	if a.SessionID != "" {
		args = append(args, "resume")
	}
	args = append(args, "--json")
	if a.IgnoreUserConfig {
		args = append(args, codexIgnoreUserConfigFlag)
	}
	// User-provided overrides go before roborev's own -c flags so roborev's
	// safety settings (skills, sandbox, reasoning) win on any key conflict.
	for _, override := range a.ConfigOverrides {
		args = append(args, "-c", override)
	}
	if opts.agenticMode {
		args = append(args, codexDangerousFlag)
	}
	if opts.autoApprove {
		if opts.sandboxBroken {
			// --full-auto still uses bwrap internally, so we
			// need the full bypass flag on broken systems.
			args = append(args, codexDangerousFlag)
		} else if a.SessionID != "" {
			args = append(args, "-c", codexReadOnlySandboxConfig)
		} else {
			args = append(args, "--sandbox", "read-only")
		}
	}
	if a.SessionID == "" && !opts.preview {
		args = append(args, "-C", opts.repoPath)
	}
	if a.Model != "" {
		args = append(args, "-m", a.Model)
	}
	if a.SuppressSkillInstructions {
		args = append(args, "-c", codexDisableSkillsConfig)
	}
	if effort := a.codexReasoningEffort(); effort != "" {
		args = append(args, "-c", fmt.Sprintf(`model_reasoning_effort="%s"`, effort))
	}
	if opts.schemaPath != "" {
		args = append(args, "--output-schema", opts.schemaPath)
	}
	if a.SessionID != "" {
		args = append(args, a.SessionID)
	}
	if !opts.preview {
		// "-" must come after all flags to read prompt from stdin
		// This avoids Windows command line length limits (~32KB)
		args = append(args, "-")
	}
	return args
}

func codexSupportsDangerousFlag(ctx context.Context, command string, ignoreUserConfig bool) (bool, error) {
	cacheKey := codexSupportCacheKey(command, ignoreUserConfig)
	if cached, ok := codexDangerousSupport.Load(cacheKey); ok {
		return cached.(bool), nil
	}
	cmd := exec.CommandContext(ctx, command, codexExecHelpArgs(ignoreUserConfig)...)
	configureCapabilityProbe(cmd)
	output, err := cmd.CombinedOutput()
	supported := strings.Contains(string(output), codexDangerousFlag)
	if err != nil && !supported {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return false, ctxErr
		}
		return false, fmt.Errorf("check %s exec --help: %w: %s", command, err, output)
	}
	codexDangerousSupport.Store(cacheKey, supported)
	return supported, nil
}

// codexSupportsNonInteractive checks that codex supports --sandbox,
// needed for non-agentic review mode (--sandbox read-only).
func codexSupportsNonInteractive(ctx context.Context, command string, ignoreUserConfig bool) (bool, error) {
	cacheKey := codexSupportCacheKey(command, ignoreUserConfig)
	if cached, ok := codexAutoApproveSupport.Load(cacheKey); ok {
		return cached.(bool), nil
	}
	cmd := exec.CommandContext(ctx, command, codexExecHelpArgs(ignoreUserConfig)...)
	configureCapabilityProbe(cmd)
	output, err := cmd.CombinedOutput()
	supported := strings.Contains(string(output), "--sandbox")
	if err != nil && !supported {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return false, ctxErr
		}
		return false, fmt.Errorf("check %s exec --help: %w: %s", command, err, output)
	}
	codexAutoApproveSupport.Store(cacheKey, supported)
	return supported, nil
}

func codexSupportCacheKey(command string, ignoreUserConfig bool) string {
	if !ignoreUserConfig {
		return command
	}
	return command + "\x00" + codexIgnoreUserConfigFlag
}

func codexExecHelpArgs(ignoreUserConfig bool) []string {
	args := []string{"exec"}
	if ignoreUserConfig {
		args = append(args, codexIgnoreUserConfigFlag)
	}
	return append(args, "--help")
}

// codexSupportsIgnoreUserConfig checks whether codex exec supports
// --ignore-user-config, which is not available in older Codex CLIs.
// Only treats the flag as supported when codex exits cleanly with the
// flag at the exec position — falling back to plain --help would
// produce false positives if the flag is listed globally but rejected
// in the exec subcommand.
func codexSupportsIgnoreUserConfig(ctx context.Context, command string) (bool, error) {
	if cached, ok := codexIgnoreUserConfigSupport.Load(command); ok {
		return cached.(bool), nil
	}

	cmd := exec.CommandContext(ctx, command, "exec", codexIgnoreUserConfigFlag, "--help")
	configureCapabilityProbe(cmd)
	output, err := cmd.CombinedOutput()
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return false, ctxErr
		}
		codexIgnoreUserConfigSupport.Store(command, false)
		return false, nil
	}
	supported := strings.Contains(string(output), codexIgnoreUserConfigFlag)
	codexIgnoreUserConfigSupport.Store(command, supported)
	return supported, nil
}

func (a *CodexAgent) Review(ctx context.Context, repoPath, commitSHA, prompt string, output io.Writer) (string, error) {
	return a.review(ctx, repoPath, commitSHA, prompt, "", output)
}

func (a *CodexAgent) review(
	ctx context.Context,
	repoPath, commitSHA, prompt, schemaPath string,
	output io.Writer,
) (string, error) {
	// Use agentic mode if either per-job setting or global setting enables it
	agenticMode := a.Agentic || AllowUnsafeAgents()
	runAgent := a
	if a.IgnoreUserConfig {
		supported, err := codexSupportsIgnoreUserConfig(ctx, a.Command)
		if err != nil {
			return "", err
		}
		if !supported {
			clone := *a
			clone.IgnoreUserConfig = false
			runAgent = &clone
		}
	}

	if agenticMode {
		supported, err := codexSupportsDangerousFlag(ctx, a.Command, runAgent.IgnoreUserConfig)
		if err != nil {
			return "", MarkUnavailable(err)
		}
		if !supported {
			return "", fmt.Errorf("codex does not support %s; upgrade codex or disable allow_unsafe_agents", codexDangerousFlag)
		}
	}

	// Non-agentic review mode uses --sandbox read-only for
	// non-interactive sandboxed execution.
	autoApprove := false
	if !agenticMode {
		supported, err := codexSupportsNonInteractive(ctx, a.Command, runAgent.IgnoreUserConfig)
		if err != nil {
			return "", MarkUnavailable(err)
		}
		if !supported {
			return "", fmt.Errorf("codex version too old for non-interactive execution; upgrade codex or use --agentic")
		}
		autoApprove = true
	}

	// Use codex exec with --json for JSONL streaming output
	// The prompt is piped via stdin using "-" to avoid command line length limits on Windows
	sandboxBroken := CodexSandboxDisabled()
	if sandboxBroken && autoApprove {
		log.Printf("codex: sandbox disabled via config, using %s", codexAutoApproveFlag)
	}
	args := runAgent.buildArgsWithSchema(
		repoPath, agenticMode, autoApprove, sandboxBroken, schemaPath,
	)
	stdoutDiagnostics := newCodexDiagnosticCapture()

	runResult, runErr := runStreamingCLI(ctx, streamingCLISpec{
		Name:         "codex",
		Command:      a.Command,
		Args:         args,
		Dir:          repoPath,
		Stdin:        strings.NewReader(prompt),
		Output:       output,
		StreamStderr: true,
		Parse: func(r io.Reader, sw *syncWriter) (string, error) {
			return a.parseStreamJSON(io.TeeReader(r, stdoutDiagnostics), sw)
		},
	})
	runResult.Stdout = stdoutDiagnostics.String()
	if runErr != nil {
		return "", MarkUnavailable(runErr)
	}

	if runResult.WaitErr != nil {
		if errors.Is(runResult.ParseErr, errNoCodexJSON) {
			return "", markCodexNoJSONUnavailable(
				formatCodexNoJSONWaitError(runResult), runResult.Stderr, stdoutDiagnostics,
			)
		}
		return "", formatStreamingCLIWaitError("codex", runResult, runResult.Stderr)
	}

	if runResult.ParseErr != nil {
		if errors.Is(runResult.ParseErr, errNoCodexJSON) {
			return "", markCodexNoJSONUnavailable(
				formatCodexNoJSONError(runResult), runResult.Stderr, stdoutDiagnostics,
			)
		}
		return "", runResult.ParseErr
	}

	if runResult.Result == "" {
		return "No review output generated", nil
	}

	return runResult.Result, nil
}

const codexDiagnosticClassificationChunk = 4096

type codexDiagnosticCapture struct {
	rendered       strings.Builder
	tail           string
	truncated      bool
	classification LimitClassification
}

func newCodexDiagnosticCapture() *codexDiagnosticCapture {
	return &codexDiagnosticCapture{}
}

func (c *codexDiagnosticCapture) Write(p []byte) (int, error) {
	written := len(p)
	captured := 0
	if remaining := cliWaitErrorOutputLimit - c.rendered.Len(); remaining > 0 {
		if len(p) < remaining {
			remaining = len(p)
		}
		_, _ = c.rendered.Write(p[:remaining])
		captured = remaining
	}
	if written > captured {
		c.truncated = true
	}

	for len(p) > 0 {
		chunkLen := min(len(p), codexDiagnosticClassificationChunk)
		c.classifyChunk(string(p[:chunkLen]))
		p = p[chunkLen:]
	}
	return written, nil
}

func (c *codexDiagnosticCapture) classifyChunk(chunk string) {
	window := c.tail + chunk
	c.classification = preferLimitClassification(
		c.classification,
		ClassifyLimit("codex", window),
	)

	overlap := maxLimitRuleSubstringLength("codex") - 1
	if overlap <= 0 || len(window) <= overlap {
		c.tail = window
		return
	}
	c.tail = window[len(window)-overlap:]
}

func (c *codexDiagnosticCapture) String() string {
	if c.truncated {
		return c.rendered.String() + "..."
	}
	return c.rendered.String()
}

func (c *codexDiagnosticCapture) Classification() LimitClassification {
	return c.classification
}

func markCodexNoJSONUnavailable(
	err error,
	stderr string,
	stdoutDiagnostics *codexDiagnosticCapture,
) error {
	classification := preferLimitClassification(
		stdoutDiagnostics.Classification(),
		ClassifyLimit("codex", stderr),
	)
	return MarkUnavailable(WithLimitClassification(err, classification))
}

func maxLimitRuleSubstringLength(agentName string) int {
	maxLength := 0
	for _, rule := range defaultLimitRules {
		if limitRuleAppliesToAgent(rule, agentName) && len(rule.Substring) > maxLength {
			maxLength = len(rule.Substring)
		}
	}
	return maxLength
}

func preferLimitClassification(current, candidate LimitClassification) LimitClassification {
	priority := func(kind LimitKind) int {
		switch kind {
		case LimitKindQuota:
			return 3
		case LimitKindSession:
			return 2
		case LimitKindTransient:
			return 1
		default:
			return 0
		}
	}
	if priority(candidate.Kind) > priority(current.Kind) {
		candidate.Message = ""
		return candidate
	}
	return current
}

func formatCodexNoJSONWaitError(runResult streamingCLIResult) error {
	return fmt.Errorf(
		"codex failed: %w (parse error: %w)%s",
		runResult.WaitErr,
		errNoCodexJSON,
		codexNoJSONDiagnostics(runResult),
	)
}

func formatCodexNoJSONError(runResult streamingCLIResult) error {
	return fmt.Errorf(
		"codex CLI did not emit valid --json events; upgrade codex or check CLI compatibility: %w%s",
		errNoCodexJSON,
		codexNoJSONDiagnostics(runResult),
	)
}

func codexNoJSONDiagnostics(runResult streamingCLIResult) string {
	var detail strings.Builder
	if stderr := truncateCLIWaitErrorOutput(runResult.Stderr); stderr != "" {
		fmt.Fprintf(&detail, "\nstderr: %s", stderr)
	}
	if stdout := truncateCLIWaitErrorOutput(runResult.Stdout); stdout != "" {
		fmt.Fprintf(&detail, "\nstdout: %s", stdout)
	}
	return detail.String()
}

func (a *CodexAgent) ReviewWithSchema(
	ctx context.Context,
	repoPath, gitRef, prompt string,
	schema json.RawMessage,
	out io.Writer,
) (json.RawMessage, error) {
	schemaFile, err := os.CreateTemp("", "roborev-codex-review-schema-*.json")
	if err != nil {
		return nil, fmt.Errorf("create codex review schema: %w", err)
	}
	schemaPath := schemaFile.Name()
	defer os.Remove(schemaPath)
	if _, err := schemaFile.Write(schema); err != nil {
		schemaFile.Close()
		return nil, fmt.Errorf("write codex review schema: %w", err)
	}
	if err := schemaFile.Close(); err != nil {
		return nil, fmt.Errorf("close codex review schema: %w", err)
	}
	result, err := a.review(
		ctx, repoPath, gitRef, prompt, schemaPath, out,
	)
	if err != nil {
		return nil, err
	}
	trimmed := strings.TrimSpace(result)
	if !json.Valid([]byte(trimmed)) || !strings.HasPrefix(trimmed, "{") {
		return nil, fmt.Errorf("codex structured review output is not a JSON object")
	}
	return json.RawMessage(trimmed), nil
}

var _ StructuredReviewAgent = (*CodexAgent)(nil)

// codexEvent represents a top-level event in codex's --json JSONL output.
type codexEvent struct {
	Type    string `json:"type"`
	Message string `json:"message,omitempty"`
	Error   struct {
		Message string `json:"message,omitempty"`
	} `json:"error,omitempty"`
	Item struct {
		ID      string `json:"id,omitempty"`
		Type    string `json:"type,omitempty"`
		Text    string `json:"text,omitempty"`
		Command string `json:"command,omitempty"`
		Status  string `json:"status,omitempty"`
	} `json:"item,omitempty"`
}

func isCodexEventType(eventType string) bool {
	return eventType == "error" ||
		strings.HasPrefix(eventType, "thread.") ||
		strings.HasPrefix(eventType, "turn.") ||
		strings.HasPrefix(eventType, "item.")
}

func isCodexToolEvent(ev codexEvent) bool {
	if ev.Type != "item.started" &&
		ev.Type != "item.updated" &&
		ev.Type != "item.completed" {
		return false
	}
	switch ev.Item.Type {
	case "command_execution", "file_change":
		return true
	default:
		return false
	}
}

func codexFailureEventError(ev codexEvent) error {
	switch ev.Type {
	case "turn.failed":
		if ev.Error.Message != "" {
			return fmt.Errorf("%w: %s", errCodexStreamFailed, ev.Error.Message)
		}
		if ev.Message != "" {
			return fmt.Errorf("%w: %s", errCodexStreamFailed, ev.Message)
		}
		return fmt.Errorf("%w: turn failed", errCodexStreamFailed)
	case "error":
		if ev.Error.Message != "" {
			return fmt.Errorf("%w: %s", errCodexStreamFailed, ev.Error.Message)
		}
		if ev.Message != "" {
			return fmt.Errorf("%w: %s", errCodexStreamFailed, ev.Message)
		}
		return fmt.Errorf("%w: stream error", errCodexStreamFailed)
	default:
		return nil
	}
}

// parseStreamJSON parses codex's --json JSONL output and extracts review text.
// Codex emits events like thread.started, turn.started, item.completed (with agent_message),
// and turn.completed. The agent_message items contain the actual review text.
func (a *CodexAgent) parseStreamJSON(r io.Reader, sw *syncWriter) (string, error) {
	var validEventsParsed bool
	agentMessages := newTrailingReviewText()
	var streamFailure error

	err := scanStreamJSONLines(r, sw, func(line string) error {
		var ev codexEvent
		if jsonErr := json.Unmarshal([]byte(line), &ev); jsonErr == nil {
			if isCodexEventType(ev.Type) {
				validEventsParsed = true

				if streamFailure == nil {
					streamFailure = codexFailureEventError(ev)
				}

				if isCodexToolEvent(ev) {
					// The persisted review is defined as the assistant text
					// after the last tool event in the stream.
					agentMessages.ResetAfterTool()
				}

				// Collect agent_message text from completed/updated items.
				// For messages with IDs, keep only the latest text per ID to avoid duplicates
				// from incremental updates while preserving first-seen order.
				if (ev.Type == "item.completed" || ev.Type == "item.updated") &&
					ev.Item.Type == "agent_message" && ev.Item.Text != "" {
					agentMessages.AddWithID(ev.Item.ID, ev.Item.Text)
				}
			}
		}
		return nil
	})
	if err != nil {
		return "", err
	}

	if !validEventsParsed {
		return "", errNoCodexJSON
	}

	if streamFailure != nil {
		return "", streamFailure
	}

	if result := agentMessages.Join("\n"); result != "" {
		return result, nil
	}

	return "", nil
}

func init() {
	Register(NewCodexAgent(""))
}

// CodexAgent does NOT implement SchemaAgent on purpose.
//
// codex's --sandbox read-only blocks writes but still allows reads, and
// codex exec has no equivalent to claude --tools "" that disables the
// shell/file tool entirely. Allowing codex to classify untrusted commit
// text would let a prompt-injected commit read local secrets and
// exfiltrate them via the JSON `reason` field.
