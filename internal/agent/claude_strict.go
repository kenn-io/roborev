package agent

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"

	"go.kenn.io/roborev/internal/config"
)

// WithStrictReview returns an isolated review configuration. The policy and
// all later clones belong to this invocation, never to the registered agent.
func (a *ClaudeAgent) WithStrictReview(policy config.ReviewToolPolicy, mcpConfigPath string) Agent {
	clone := a.clone(withClonedAgentic(false), withClonedSessionID(""))
	clone.strictPolicy = policy.Clone()
	clone.strictMCPConfigPath = mcpConfigPath
	return clone
}

func (a *ClaudeAgent) buildStrictArgs(includeEffort bool) []string {
	args := []string{
		"-p", "--verbose", "--output-format", "stream-json",
		"--restricted", "--tools", "Read,Glob,Grep", "--strict-mcp-config", "--mcp-config", a.strictMCPConfigPath,
		"--permission-mode", "dontAsk", "--permission-prompts", "none", "--allowedTools", "Read,Glob,Grep",
		"--no-session-persistence", "--disable-slash-commands",
	}
	// Strict sessions never resume, even if a caller configured an old ID.
	return append(args, a.clone(withClonedSessionID("")).buildModelSessionArgs(includeEffort)...)
}

var claudeStrictStripKeys = []string{
	"CLAUDE_CODE_COORDINATOR_MODE", "CLAUDE_CODE_CHILD_SESSION", "CLAUDE_CODE_SESSION_ID", "CLAUDE_CODE_SESSION_ATTENDED", "CLAUDE_PID",
	"CLAUDE_CODE_MESSAGING_SOCKET", "CLAUDE_CODE_MESSAGING_TOKEN", "CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS",
	"CLAUDE_CODE_ENTRYPOINT", "CLAUDE_CODE_EXECPATH", "CLAUDE_CODE_ENABLE_AWAY_SUMMARY", "CLAUDE_CODE_DISABLE_AUTO_MEMORY",
	"ENABLE_TOOL_SEARCH", "ENABLE_LSP_TOOL", "MAX_MCP_OUTPUT_TOKENS", "CLAUDE_EFFORT",
}

func buildStrictClaudeEnv(base []string, model, baseURL string) ([]string, error) {
	env, err := buildClaudeEnv(base, model, baseURL)
	if err != nil {
		return nil, err
	}
	return filterEnv(env, claudeStrictStripKeys...), nil
}

var claudeStrictSupport capabilityCache

func claudeSupportsStrictReview(ctx context.Context, command string, schema, effort bool) error {
	required := []string{"--restricted", "--tools", "--strict-mcp-config", "--mcp-config", "--permission-mode", "--permission-prompts", "--allowedTools", "--no-session-persistence", "--disable-slash-commands"}
	// The cache records the complete recipe, including both optional modes, so
	// plain calls cannot accidentally qualify a later schema/effort invocation.
	key := capabilityCacheKey(command)
	if cached, ok := claudeStrictSupport.Load(key); ok && cached.(bool) {
		return nil
	}
	cmd := exec.CommandContext(ctx, command, "--help")
	configureCapabilityProbe(ctx, cmd)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("check claude --help: %w", err)
	}
	flags := strings.Fields(string(output))
	for i := range flags {
		flags[i] = strings.TrimSuffix(flags[i], ",")
	}
	complete := slices.Contains(flags, "--json-schema") && slices.Contains(flags, "--effort")
	if schema {
		required = append(required, "--json-schema")
	}
	if effort {
		required = append(required, "--effort")
	}
	for _, flag := range required {
		if !slices.Contains(flags, flag) {
			return fmt.Errorf("%w: claude strict reviews require %s; upgrade claude", ErrReviewToolPolicy, flag)
		}
	}
	if complete {
		claudeStrictSupport.Store(key, true)
	}
	return nil
}

// claudeStrictInit is native startup evidence, not model-authored text.
// Task-specific grants are checked against this same evidence for both output
// modes; a valid-looking result cannot compensate for missing capabilities.
type claudeStrictInit struct {
	PermissionMode string   `json:"permissionMode"`
	Tools          []string `json:"tools"`
	MCPServers     []struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	} `json:"mcp_servers"`
}

func validateClaudeStrictInit(raw []byte, schema bool) error {
	var init claudeStrictInit
	if err := json.Unmarshal(raw, &init); err != nil {
		return fmt.Errorf("malformed init evidence")
	}
	if init.PermissionMode != "dontAsk" {
		return fmt.Errorf("permission mode must be dontAsk")
	}
	for _, tool := range []string{"Read", "Glob", "Grep"} {
		if !slices.Contains(init.Tools, tool) {
			return fmt.Errorf("required tool %s is unavailable", tool)
		}
	}
	for _, tool := range init.Tools {
		if tool == "Read" || tool == "Glob" || tool == "Grep" || (schema && tool == "StructuredOutput") {
			continue
		}
		return fmt.Errorf("unexpected native tool %q", tool)
	}
	if init.MCPServers == nil {
		return fmt.Errorf("missing MCP server evidence")
	}
	if len(init.MCPServers) != 0 {
		return fmt.Errorf("unexpected MCP server")
	}
	return nil
}

func parseClaudeStrictStream(r io.Reader, out io.Writer, schema bool) (string, error) {
	var stream bytes.Buffer
	var initialized bool
	var startupErr error
	err := scanStreamJSONLines(r, out, func(line string) error {
		stream.WriteString(line)
		stream.WriteByte('\n')
		var msg struct {
			Type    string `json:"type"`
			Subtype string `json:"subtype"`
		}
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			return nil
		}
		if msg.Type == "system" && msg.Subtype == "init" {
			if err := validateClaudeStrictInit([]byte(line), schema); err != nil {
				startupErr = err
			}
			initialized = true
		} else if !initialized && (msg.Type == "assistant" || msg.Type == "result" || msg.Type == "tool_use" || msg.Type == "tool_result") {
			startupErr = fmt.Errorf("output preceded init evidence")
		}
		// Continue draining on rejection so the child cannot block on its stdout.
		return nil
	})
	if err != nil {
		return "", err
	}
	if !initialized {
		startupErr = fmt.Errorf("missing init evidence")
	}
	parsed, parseErr := parseStreamJSON(bytes.NewReader(stream.Bytes()), nil)
	if startupErr != nil {
		return "", errors.Join(fmt.Errorf("%w: strict Claude startup: %w", ErrReviewToolPolicy, startupErr), parseErr)
	}
	if parseErr != nil {
		return "", parseErr
	}
	if schema {
		result, err := parseClaudeClassifyStream(&stream)
		return string(result), err
	}
	return parsed, nil
}

func (a *ClaudeAgent) strictReview(ctx context.Context, repoPath, prompt string, schema jsontext.Value, out io.Writer) (string, error) {
	policy, err := config.CanonicalReviewToolPolicy(a.strictPolicy)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrReviewToolPolicy, err)
	}
	if a.Agentic {
		return "", fmt.Errorf("%w: strict review tools cannot be combined with agentic mode", ErrReviewToolPolicy)
	}
	if len(policy.MCP) != 0 {
		return "", fmt.Errorf("%w: strict MCP grants are not supported yet", ErrReviewToolPolicy)
	}
	if a.strictMCPConfigPath == "" {
		return "", fmt.Errorf("%w: strict review requires a prepared MCP config", ErrReviewToolPolicy)
	}
	model, baseURL, err := parseModel(a.Model)
	if err != nil {
		return "", err
	}
	hasSchema := len(schema) != 0
	includeEffort := reasoningEffort(a.Reasoning) != ""
	if err := claudeSupportsStrictReview(ctx, a.Command, hasSchema, includeEffort); err != nil {
		return "", err
	}
	args := a.buildStrictArgs(includeEffort)
	if hasSchema {
		args = append(args, "--json-schema", string(schema))
	}
	env, err := buildStrictClaudeEnv(os.Environ(), model, baseURL)
	if err != nil {
		return "", err
	}
	result, err := runStreamingCLI(ctx, streamingCLISpec{
		Name: "claude", Command: a.Command, Args: args, Dir: repoPath, Env: env, Stdin: strings.NewReader(prompt), Output: out,
		Parse: func(r io.Reader, sw *syncWriter) (string, error) { return parseClaudeStrictStream(r, sw, hasSchema) },
	})
	if err != nil {
		return "", err
	}
	if result.WaitErr != nil {
		return "", formatStreamingCLIWaitError(a.Name(), result, result.Stderr)
	}
	if result.ParseErr != nil {
		return "", result.ParseErr
	}
	if result.Result == "" && !hasSchema {
		return "No review output generated", nil
	}
	return result.Result, nil
}

var _ StrictReviewAgent = (*ClaudeAgent)(nil)
