package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
)

// ClaudeAgent runs code reviews using Claude Code CLI
type ClaudeAgent struct {
	Command   string         // The claude command to run (default: "claude")
	Model     string         // Model to use (e.g., "opus", "sonnet", or full name)
	Reasoning ReasoningLevel // Reasoning level mapped to --effort flag
	Agentic   bool           // Whether agentic mode is enabled (allow file edits)
	SessionID string         // Existing session ID to resume
}

const (
	claudeDangerousFlag = "--dangerously-skip-permissions"
	claudeEffortFlag    = "--effort"
)

var (
	claudeDangerousSupport sync.Map
	claudeEffortSupport    sync.Map
	claudeToolsSupport     sync.Map
)

// NewClaudeAgent creates a new Claude Code agent
func NewClaudeAgent(command string) *ClaudeAgent {
	if command == "" {
		command = "claude"
	}
	return &ClaudeAgent{Command: command, Reasoning: ReasoningStandard}
}

func (a *ClaudeAgent) clone(opts ...agentCloneOption) *ClaudeAgent {
	cfg := newAgentCloneConfig(
		a.Command,
		a.Model,
		a.Reasoning,
		a.Agentic,
		a.SessionID,
		opts...,
	)
	return &ClaudeAgent{
		Command:   cfg.Command,
		Model:     cfg.Model,
		Reasoning: cfg.Reasoning,
		Agentic:   cfg.Agentic,
		SessionID: cfg.SessionID,
	}
}

// WithReasoning returns a copy of the agent with the specified reasoning level.
func (a *ClaudeAgent) WithReasoning(level ReasoningLevel) Agent {
	return a.clone(withClonedReasoning(level))
}

// WithAgentic returns a copy of the agent configured for agentic mode.
func (a *ClaudeAgent) WithAgentic(agentic bool) Agent {
	return a.clone(withClonedAgentic(agentic))
}

// WithModel returns a copy of the agent configured to use the specified model.
func (a *ClaudeAgent) WithModel(model string) Agent {
	if model == "" {
		return a
	}
	return a.clone(withClonedModel(model))
}

// WithSessionID returns a copy of the agent configured to resume a prior session.
func (a *ClaudeAgent) WithSessionID(sessionID string) Agent {
	return a.clone(withClonedSessionID(sessionID))
}

// claudeEffort maps ReasoningLevel to Claude Code's --effort flag values
func (a *ClaudeAgent) claudeEffort() string {
	switch a.Reasoning {
	case ReasoningMaximum:
		return "max"
	case ReasoningThorough:
		return "high"
	case ReasoningMedium:
		return "medium"
	case ReasoningFast:
		return "low"
	default:
		return "" // use claude default (standard = no override)
	}
}

func (a *ClaudeAgent) Name() string {
	return "claude-code"
}

func (a *ClaudeAgent) CommandName() string {
	return a.Command
}

func (a *ClaudeAgent) CommandLine() string {
	agenticMode := a.Agentic || AllowUnsafeAgents()
	args := a.buildArgs(agenticMode, true)
	return a.Command + " " + strings.Join(args, " ")
}

// parseModel splits a model spec of the form "<model>@<base_url>" into its
// components. The split happens at the first "@" whose suffix starts with
// http:// or https:// (so proxy URLs embedded after the model are recognized
// while leaving the rest of the spec intact).
//
// Rejections (return error):
//   - Proxy URL containing userinfo (user:pass@host) — these leak into
//     child-process env, /proc, and error messages. Operators must use
//     ROBOREV_CLAUDE_PROXY_TOKEN for proxy auth.
//   - Trailing bare "@" with no URL suffix (e.g. "sonnet@") — malformed
//     input that previously fell through to native routing silently.
//   - Leading "@http(s)://" with no model — proxy mode must pin tier
//     aliases to a concrete model name.
//   - Bare "http(s)://" URL with no "<model>@" prefix — same reason.
//   - Leading "@" with non-URL suffix (e.g. "@foo") — would pass through to
//     Claude as `--model @foo` and produce a confusing downstream error.
func parseModel(spec string) (model, baseURL string, err error) {
	if strings.HasPrefix(spec, "@http://") || strings.HasPrefix(spec, "@https://") {
		return "", "", fmt.Errorf("model spec %q has proxy URL but no model; use '<model>@%s'", spec, spec[1:])
	}
	if strings.HasPrefix(spec, "http://") || strings.HasPrefix(spec, "https://") {
		return "", "", fmt.Errorf("model spec %q is a bare proxy URL; use '<model>@%s'", spec, spec)
	}
	if strings.HasPrefix(spec, "@") {
		return "", "", fmt.Errorf("model spec %q starts with '@'; model name must come before the '@<base_url>' suffix", spec)
	}
	for i := 1; i < len(spec); i++ {
		if spec[i] != '@' {
			continue
		}
		suffix := spec[i+1:]
		if strings.HasPrefix(suffix, "http://") || strings.HasPrefix(suffix, "https://") {
			if err := validateProxyURL(suffix); err != nil {
				return "", "", err
			}
			model := spec[:i]
			// Reject specs like "foo@httpx://bar@http://host" where the model
			// component itself contains a URL-like substring — these indicate
			// a malformed multi-URL spec that would otherwise silently pass
			// a nonsensical --model argument to Claude.
			if strings.Contains(model, "://") {
				return "", "", fmt.Errorf("model spec %q has URL-like substring in model component %q; only one '@<base_url>' suffix is allowed", spec, model)
			}
			return model, suffix, nil
		}
	}
	if strings.HasSuffix(spec, "@") {
		return "", "", fmt.Errorf("model spec %q has trailing '@' — remove it or append a proxy URL as '<model>@http(s)://<host>'", spec)
	}
	return spec, "", nil
}

// validateProxyURL rejects proxy URLs that would leak credentials via the
// child-process environment. The parsed URL must not contain userinfo; http://
// is only permitted for loopback hosts so real credentials aren't forwarded
// over plaintext to remote endpoints.
func validateProxyURL(raw string) error {
	u, err := url.ParseRequestURI(raw)
	if err != nil {
		return fmt.Errorf("invalid proxy URL %q: %w", raw, err)
	}
	if u.Host == "" {
		return fmt.Errorf("proxy URL %q has no host", raw)
	}
	if u.User != nil {
		return fmt.Errorf("proxy URL must not embed credentials; set ROBOREV_CLAUDE_PROXY_TOKEN instead")
	}
	// Reject fragments — they have no server-side meaning for HTTP requests
	// and indicate operator error. Belt-and-suspenders: also scan the raw
	// string in case ParseRequestURI's fragment handling changes.
	if u.Fragment != "" || strings.Contains(raw, "#") {
		return fmt.Errorf("proxy URL %q must not contain a fragment", raw)
	}
	if u.Scheme == "http" && !isLoopbackHost(u.Hostname()) {
		return fmt.Errorf("proxy URL %q uses http:// with a non-loopback host; use https:// or a loopback address", raw)
	}
	return nil
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

func (a *ClaudeAgent) buildArgs(agenticMode, includeEffort bool) []string {
	sessionID := sanitizedResumeSessionID(a.SessionID)
	// Always use stdin piping + stream-json for non-interactive execution
	// (following claude-code-action pattern from Anthropic)
	args := []string{"-p", "--verbose", "--output-format", "stream-json"}

	// buildArgs is also called from CommandLine() for display; on parse error
	// fall back to the raw configured model so operators see what they typed
	// in logs. Review() re-parses and surfaces the error before execution.
	model, _, parseErr := parseModel(a.Model)
	if parseErr != nil {
		model = a.Model
	}
	if model != "" {
		args = append(args, "--model", model)
	}
	if sessionID != "" {
		args = append(args, "--resume", sessionID)
	}

	if includeEffort {
		if effort := a.claudeEffort(); effort != "" {
			args = append(args, claudeEffortFlag, effort)
		}
	}

	if agenticMode {
		// Agentic mode: Claude can use tools and make file changes
		args = append(args, claudeDangerousFlag)
		args = append(args, "--allowedTools", "Edit,MultiEdit,Write,Read,Glob,Grep,Bash")
	} else {
		// Review mode: read-only tools only (no Bash to prevent arbitrary command execution)
		args = append(args, "--allowedTools", "Read,Glob,Grep")
	}
	return args
}

func claudeSupportsDangerousFlag(ctx context.Context, command string) (bool, error) {
	if cached, ok := claudeDangerousSupport.Load(command); ok {
		return cached.(bool), nil
	}
	cmd := exec.CommandContext(ctx, command, "--help")
	configureCapabilityProbe(cmd)
	output, err := cmd.CombinedOutput()
	supported := strings.Contains(string(output), claudeDangerousFlag)
	if err != nil && !supported {
		return false, fmt.Errorf("check %s --help: %w: %s", command, err, output)
	}
	claudeDangerousSupport.Store(command, supported)
	return supported, nil
}

func claudeSupportsEffortFlag(ctx context.Context, command string) bool {
	if cached, ok := claudeEffortSupport.Load(command); ok {
		return cached.(bool)
	}
	cmd := exec.CommandContext(ctx, command, "--help")
	configureCapabilityProbe(cmd)
	output, _ := cmd.CombinedOutput()
	supported := strings.Contains(string(output), claudeEffortFlag)
	claudeEffortSupport.Store(command, supported)
	return supported
}

// claudeSupportsToolsFlag reports whether the installed claude binary
// recognizes the `--tools` flag (used by ClassifyWithSchema with `""`
// as a deny-all). Older versions do not, and silently passing the flag
// to a binary that ignores it would leave file/shell tools enabled
// against untrusted commit text. Errors are treated as "unsupported"
// so the caller fails closed.
func claudeSupportsToolsFlag(ctx context.Context, command string) bool {
	if cached, ok := claudeToolsSupport.Load(command); ok {
		return cached.(bool)
	}
	cmd := exec.CommandContext(ctx, command, "--help")
	configureCapabilityProbe(cmd)
	output, err := cmd.CombinedOutput()
	if err != nil {
		// Don't cache transient failures.
		return false
	}
	supported := strings.Contains(string(output), "--tools")
	claudeToolsSupport.Store(command, supported)
	return supported
}

func (a *ClaudeAgent) Review(ctx context.Context, repoPath, commitSHA, prompt string, output io.Writer) (string, error) {
	model, baseURL, err := parseModel(a.Model)
	if err != nil {
		return "", err
	}

	// Use agentic mode if either per-job setting or global setting enables it
	agenticMode := a.Agentic || AllowUnsafeAgents()

	if agenticMode {
		supported, err := claudeSupportsDangerousFlag(ctx, a.Command)
		if err != nil {
			return "", err
		}
		if !supported {
			return "", fmt.Errorf("claude does not support %s; upgrade claude or disable allow_unsafe_agents", claudeDangerousFlag)
		}
	}

	// Only pass --effort if the installed Claude Code supports it
	includeEffort := a.claudeEffort() != "" && claudeSupportsEffortFlag(ctx, a.Command)

	// Build args - always uses stdin piping + stream-json for non-interactive execution
	args := a.buildArgs(agenticMode, includeEffort)

	cmd := exec.CommandContext(ctx, a.Command, args...)
	cmd.Dir = repoPath
	env, err := buildClaudeEnv(cmd.Environ(), model, baseURL)
	if err != nil {
		return "", err
	}

	runResult, runErr := runStreamingCLI(ctx, streamingCLISpec{
		Name:    "claude",
		Command: a.Command,
		Args:    args,
		Dir:     repoPath,
		Env:     env,
		Stdin:   strings.NewReader(prompt),
		Output:  output,
		Parse: func(r io.Reader, sw *syncWriter) (string, error) {
			if sw == nil {
				return parseStreamJSON(r, nil)
			}
			return parseStreamJSON(r, sw)
		},
	})
	if runErr != nil {
		return "", runErr
	}

	if runResult.WaitErr != nil {
		return "", formatDetailedCLIWaitError(runResult, detailedCLIWaitErrorOptions{
			AgentName:     a.Name(),
			Stderr:        runResult.Stderr,
			PartialOutput: runResult.Result,
		})
	}

	if runResult.ParseErr != nil {
		return "", runResult.ParseErr
	}

	if runResult.Result == "" {
		return "No review output generated", nil
	}

	return runResult.Result, nil
}

// claudeStreamMessage represents a message in Claude's stream-json output format
type claudeStreamMessage struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype,omitempty"`
	IsError bool   `json:"is_error,omitempty"`
	Message struct {
		Content json.RawMessage `json:"content,omitempty"`
	} `json:"message,omitempty"`
	Result string `json:"result,omitempty"`
	Error  struct {
		Message string `json:"message,omitempty"`
	} `json:"error,omitempty"`
}

// extractContentText extracts only the text that appears after the last tool-use
// block in a Claude message content field. Content can be a plain string or an
// array of content blocks (e.g. [{"type":"text","text":"..."}]).
func extractContentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	// Try string first (simple format)
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	// Try array of content blocks (real Claude Code format)
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		texts := newTrailingReviewText()
		for _, b := range blocks {
			switch b.Type {
			case "text":
				texts.Add(b.Text)
			case "tool_use", "tool_result":
				texts.ResetAfterTool()
			}
		}
		return texts.Join("\n")
	}
	return ""
}

// parseStreamJSON parses Claude's stream-json output and extracts the final result.
// Uses bufio.Reader.ReadString to read lines without buffer size limits.
// On success, returns (result, nil). On failure, returns (partialOutput, error)
// where partialOutput contains any assistant messages collected before the error.
func parseStreamJSON(r io.Reader, output io.Writer) (string, error) {
	var lastResult string
	assistantMessages := newTrailingReviewText()
	var errorMessages []string
	var validEventsParsed bool

	err := scanStreamJSONLines(r, output, func(line string) error {
		var msg claudeStreamMessage
		if jsonErr := json.Unmarshal([]byte(line), &msg); jsonErr == nil {
			validEventsParsed = true

			if msg.Type == "assistant" {
				if text := extractContentText(msg.Message.Content); text != "" {
					assistantMessages.Add(text)
				}
			}
			if msg.Type == "tool_use" || msg.Type == "tool_result" {
				assistantMessages.ResetAfterTool()
			}

			if msg.Type == "result" {
				if msg.IsError {
					errMsg := "review returned error"
					if msg.Error.Message != "" {
						errMsg = msg.Error.Message
					} else if msg.Result != "" {
						errMsg = msg.Result
					}
					errorMessages = append(errorMessages, errMsg)
				} else if msg.Result != "" {
					lastResult = msg.Result
				}
			}

			if msg.Type == "error" && msg.Error.Message != "" {
				errorMessages = append(errorMessages, msg.Error.Message)
			}
		}
		return nil
	})
	if err != nil {
		return "", err
	}

	// Error if we didn't parse any valid events
	if !validEventsParsed {
		return "", fmt.Errorf("no valid stream-json events parsed from output")
	}

	// Build partial output for error context
	partial := assistantMessages.Join("\n")

	// If error events were received but we got no result, report them with any partial output
	if len(errorMessages) > 0 && lastResult == "" {
		return partial, fmt.Errorf("stream errors: %s", strings.Join(errorMessages, "; "))
	}

	// Prefer the result field if present, otherwise join assistant messages
	if lastResult != "" {
		return lastResult, nil
	}
	if result := assistantMessages.Join("\n"); result != "" {
		return result, nil
	}

	return "", nil
}

// filterEnv returns a copy of env with the specified key removed
func filterEnv(env []string, keys ...string) []string {
	result := make([]string, 0, len(env))
	for _, e := range env {
		k, _, _ := strings.Cut(e, "=")
		strip := slices.Contains(keys, k)
		if !strip {
			result = append(result, e)
		}
	}
	return result
}

// claudeStripKeys lists every Anthropic-related env var that roborev
// strips from the inherited environment before launching Claude. Keeping
// this in one place means Review and ClassifyWithSchema cannot drift on
// what they sanitize.
var claudeStripKeys = []string{
	"ANTHROPIC_API_KEY",
	"CLAUDECODE",
	"ANTHROPIC_BASE_URL",
	"ANTHROPIC_AUTH_TOKEN",
	"ANTHROPIC_DEFAULT_OPUS_MODEL",
	"ANTHROPIC_DEFAULT_SONNET_MODEL",
	"ANTHROPIC_DEFAULT_HAIKU_MODEL",
	"CLAUDE_CODE_SUBAGENT_MODEL",
}

// buildClaudeEnv returns the env Claude should run with, given the
// process baseEnv (cmd.Environ()), the resolved model, and an optional
// proxy baseURL. Strips inherited Anthropic routing vars so roborev owns
// the routing decision: native mode uses Anthropic defaults, proxy mode
// injects a validated block. Never forwards ANTHROPIC_API_KEY to a
// third-party proxy.
func buildClaudeEnv(baseEnv []string, model, baseURL string) ([]string, error) {
	env := filterEnv(baseEnv, claudeStripKeys...)
	if baseURL != "" {
		// Route Claude Code to an OpenAI/Anthropic-compatible proxy
		// (Ollama, LiteLLM, etc.). Pin all tier aliases to the same
		// model so Claude's internal tier-switching stays on the proxy
		// target. Proxy auth is opt-in via ROBOREV_CLAUDE_PROXY_TOKEN.
		// Trim whitespace; reject embedded control characters.
		authToken := strings.TrimSpace(os.Getenv("ROBOREV_CLAUDE_PROXY_TOKEN"))
		if strings.ContainsAny(authToken, "\n\r\x00") {
			return nil, fmt.Errorf("ROBOREV_CLAUDE_PROXY_TOKEN must not contain control characters (newline, carriage return, or NUL)")
		}
		if authToken == "" {
			authToken = "proxy"
		}
		env = append(env,
			"ANTHROPIC_BASE_URL="+baseURL,
			"ANTHROPIC_AUTH_TOKEN="+authToken,
			"ANTHROPIC_DEFAULT_OPUS_MODEL="+model,
			"ANTHROPIC_DEFAULT_SONNET_MODEL="+model,
			"ANTHROPIC_DEFAULT_HAIKU_MODEL="+model,
			"CLAUDE_CODE_SUBAGENT_MODEL="+model,
		)
	} else if apiKey := AnthropicAPIKey(); apiKey != "" {
		env = append(env, "ANTHROPIC_API_KEY="+apiKey)
	}
	env = append(env, "CLAUDE_NO_SOUND=1")
	return env, nil
}

func init() {
	Register(NewClaudeAgent(""))
}

// classifyArgs builds the argv for a schema-constrained one-shot classify call.
func (a *ClaudeAgent) classifyArgs(schema json.RawMessage) []string {
	// Classify is a routing decision over commit messages and diffs from
	// shared repos — never trust that input. Disable ALL tools (including
	// Read/Glob/Grep) so a prompt-injected commit cannot exfiltrate
	// secrets via the JSON `reason` field by reading ~/.ssh, ~/.roborev,
	// or any other local file. The schema constraint pins output to
	// {design_review, reason} regardless. Never pass
	// --dangerously-skip-permissions.
	args := []string{
		"-p", "--output-format", "stream-json", "--verbose",
		"--json-schema", string(schema),
		"--tools", "",
	}
	// parseModel returns model + optional baseURL; we only need the model
	// name on the CLI here. baseURL is wired via env in ClassifyWithSchema.
	model, _, parseErr := parseModel(a.Model)
	if parseErr != nil {
		model = a.Model
	}
	if model != "" {
		args = append(args, "--model", model)
	}
	if eff := a.claudeEffort(); eff != "" {
		args = append(args, claudeEffortFlag, eff)
	}
	return args
}

type claudeStructuredOutputBlock struct {
	Type   string          `json:"type"`
	Name   string          `json:"name,omitempty"`
	Input  json.RawMessage `json:"input,omitempty"`
	Caller struct {
		Type string `json:"type,omitempty"`
	} `json:"caller,omitempty"`
}

func extractClaudeStructuredOutput(raw json.RawMessage) (json.RawMessage, bool, error) {
	if len(raw) == 0 {
		return nil, false, nil
	}
	var blocks []claudeStructuredOutputBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, false, nil
	}
	for _, block := range blocks {
		if block.Type != "tool_use" || block.Name != "StructuredOutput" {
			continue
		}
		if block.Caller.Type != "direct" {
			return nil, true, fmt.Errorf("claude structured output tool use has non-direct caller %q", block.Caller.Type)
		}
		if len(bytes.TrimSpace(block.Input)) == 0 || bytes.Equal(bytes.TrimSpace(block.Input), []byte("null")) {
			return nil, true, fmt.Errorf("claude structured output tool use is missing input")
		}
		return block.Input, true, nil
	}
	return nil, false, nil
}

func validateClaudeClassifyJSON(label string, raw json.RawMessage) (json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("claude %s is empty", label)
	}
	if !json.Valid(trimmed) {
		return nil, fmt.Errorf("claude %s is not valid JSON: %q", label, string(raw))
	}
	if trimmed[0] != '{' {
		return nil, fmt.Errorf("claude %s is not a JSON object: %q", label, string(trimmed))
	}
	return json.RawMessage(append([]byte(nil), trimmed...)), nil
}

// parseClaudeClassifyStream reads Claude's stream-json output and returns the
// schema-constrained classifier JSON. Claude Code versions observed at 2.1.161
// can emit JSON Schema output as a direct StructuredOutput tool-use block
// instead of the older final result field, so accept both shapes and ignore
// assistant prose.
func parseClaudeClassifyStream(r io.Reader) (json.RawMessage, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 1<<20), 1<<22)
	var final json.RawMessage
	var finalLabel string
	var found bool
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var msg claudeStreamMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			continue
		}
		if msg.Type == "assistant" {
			structured, ok, err := extractClaudeStructuredOutput(msg.Message.Content)
			if err != nil {
				return nil, err
			}
			if ok {
				final = structured
				finalLabel = "structured output"
				found = true
			}
		}
		if msg.Type == "result" && msg.Result != "" {
			final = json.RawMessage(msg.Result)
			finalLabel = "result"
			found = true
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read stream: %w", err)
	}
	if !found {
		return nil, fmt.Errorf("no result or structured output event in claude stream")
	}
	return validateClaudeClassifyJSON(finalLabel, final)
}

// ClassifyWithSchema runs a single constrained Claude Code invocation and
// returns the final JSON conforming to schema. Implements SchemaAgent.
func (a *ClaudeAgent) ClassifyWithSchema(
	ctx context.Context,
	repoPath, gitRef, prompt string,
	schema json.RawMessage,
	out io.Writer,
) (json.RawMessage, error) {
	// Refuse to run if the installed claude binary doesn't recognize
	// `--tools` — without that flag, classifyArgs's deny-all is silently
	// dropped and the model would have file/shell access against
	// untrusted commit text.
	if !claudeSupportsToolsFlag(ctx, a.Command) {
		return nil, fmt.Errorf("claude binary does not support --tools flag (required for safe classification — upgrade claude or remove it as classify_agent)")
	}
	model, baseURL, err := parseModel(a.Model)
	if err != nil {
		return nil, err
	}
	args := a.classifyArgs(schema)
	cmd := exec.CommandContext(ctx, a.Command, args...)
	configureSubprocess(cmd)
	cmd.Dir = repoPath
	env, err := buildClaudeEnv(cmd.Environ(), model, baseURL)
	if err != nil {
		return nil, err
	}
	cmd.Env = env
	cmd.Stdin = strings.NewReader(prompt)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start claude: %w", err)
	}

	buf, readErr := io.ReadAll(stdout)
	if readErr != nil {
		_ = cmd.Wait()
		return nil, fmt.Errorf("read stdout: %w", readErr)
	}
	if out != nil {
		_, _ = out.Write(buf)
	}
	if err := cmd.Wait(); err != nil {
		return nil, fmt.Errorf("claude exited: %w (stderr: %s)", err, strings.TrimSpace(stderr.String()))
	}
	return parseClaudeClassifyStream(strings.NewReader(string(buf)))
}

// Compile-time assertion that ClaudeAgent implements SchemaAgent.
var _ SchemaAgent = (*ClaudeAgent)(nil)
