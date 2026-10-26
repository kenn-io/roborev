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
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"go.kenn.io/roborev/internal/config"
)

// claudeStrictNativeTools are the only built-in tools a strict review loads.
var claudeStrictNativeTools = []string{"Glob", "Grep", "Read"}

// claudeStrictFlags must all appear in `claude --help` before a strict review
// runs. An older CLI would ignore the restrictions instead of failing.
var claudeStrictFlags = []string{
	"--restricted", "--tools", "--allowedTools", "--strict-mcp-config", "--mcp-config",
	"--permission-mode", "--permission-prompts", "--no-session-persistence", "--disable-slash-commands",
}

// claudeHelpFlagCache maps a capabilityProbeKey to the flag set parsed from
// that executable's `--help` output.
var claudeHelpFlagCache sync.Map

// WithStrictReview returns a copy that runs reviews in a fresh, locked-down
// Claude session limited to policy. mcpConfigPath is the only MCP config the
// session loads.
func (a *ClaudeAgent) WithStrictReview(policy config.ReviewToolPolicy, mcpConfigPath string) Agent {
	clone := a.clone()
	clone.strictPolicy = policy.Clone()
	clone.strictMCPConfigPath = mcpConfigPath
	clone.strictSettingsPath = ""
	clone.Agentic = false
	clone.SessionID = ""
	return clone
}

// StrictReviewMCPConfig returns the MCP config path of a strict review, or ""
// when the agent was not prepared with WithStrictReview.
func (a *ClaudeAgent) StrictReviewMCPConfig() string {
	if a.strictPolicy == nil {
		return ""
	}
	return a.strictMCPConfigPath
}

// strictAllowedTools lists the tools a strict session may load and use.
func (a *ClaudeAgent) strictAllowedTools() []string {
	return slices.Clone(claudeStrictNativeTools)
}

func (a *ClaudeAgent) buildStrictArgs(includeEffort bool) []string {
	tools := strings.Join(a.strictAllowedTools(), ",")
	args := []string{
		"-p", "--verbose", "--output-format", "stream-json",
		// --restricted skips user, project and local settings, so hooks,
		// plugins and permission rules from them cannot add tools.
		"--restricted", "--tools", tools, "--allowedTools", tools,
		"--strict-mcp-config", "--mcp-config", a.strictMCPConfigPath,
		"--permission-mode", "dontAsk", "--permission-prompts", "none",
		"--no-session-persistence", "--disable-slash-commands",
	}
	if a.strictSettingsPath != "" {
		// Only the credential and provider keys copied from user settings.
		args = append(args, "--settings", a.strictSettingsPath)
	}
	model, _, err := parseModel(a.Model)
	if err != nil {
		model = a.Model
	}
	if model != "" {
		args = append(args, "--model", model)
	}
	if effort := reasoningEffort(a.Reasoning); includeEffort && effort != "" {
		args = append(args, claudeEffortFlag, effort)
	}
	return args
}

// claudeStrictSettingsKeys are the user settings a strict review keeps:
// credentials and provider selection, which --restricted would otherwise drop.
var claudeStrictSettingsKeys = []string{
	"env", "apiKeyHelper", "awsAuthRefresh", "awsCredentialExport", "gcpAuthRefresh",
	"forceLoginMethod", "forceLoginOrgUUID", "modelOverrides",
}

// writeClaudeStrictSettings copies claudeStrictSettingsKeys from the user
// settings file into dir/settings.json and returns its path. It never reads
// project or local settings, which the reviewed repository controls. It
// returns "" when the user file is missing, invalid, or has none of the keys.
func writeClaudeStrictSettings(dir string) (string, error) {
	configDir := os.Getenv("CLAUDE_CONFIG_DIR")
	if configDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", nil
		}
		configDir = filepath.Join(home, ".claude")
	}
	data, err := os.ReadFile(filepath.Join(configDir, "settings.json"))
	if err != nil {
		return "", nil
	}
	var user map[string]jsontext.Value
	if json.Unmarshal(data, &user) != nil {
		return "", nil
	}
	kept := make(map[string]jsontext.Value)
	for _, key := range claudeStrictSettingsKeys {
		if value, ok := user[key]; ok {
			kept[key] = value
		}
	}
	if len(kept) == 0 {
		return "", nil
	}
	out, err := json.Marshal(kept)
	if err != nil {
		return "", fmt.Errorf("encode strict claude settings: %w", err)
	}
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, out, 0o600); err != nil {
		return "", fmt.Errorf("write strict claude settings: %w", err)
	}
	return path, nil
}

func claudeHelpFlags(ctx context.Context, command string) (map[string]bool, error) {
	key := capabilityCacheKey(command)
	if key.path != "" {
		if cached, ok := claudeHelpFlagCache.Load(key); ok {
			return cached.(map[string]bool), nil
		}
	}
	cmd := exec.CommandContext(ctx, command, "--help")
	configureCapabilityProbe(ctx, cmd)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("check %s --help: %w", command, err)
	}
	flags := make(map[string]bool)
	for field := range strings.FieldsSeq(string(output)) {
		if strings.HasPrefix(field, "--") {
			flags[strings.TrimRight(field, ",")] = true
		}
	}
	if key.path != "" {
		claudeHelpFlagCache.Store(key, flags)
	}
	return flags, nil
}

type claudeStrictInit struct {
	PermissionMode string   `json:"permissionMode"`
	Tools          []string `json:"tools"`
	MCPServers     []struct {
		Name string `json:"name"`
	} `json:"mcp_servers"`
}

// validateClaudeStrictInit checks Claude's own startup report: the session
// must load exactly wantTools, no MCP servers, and the non-prompting mode.
func validateClaudeStrictInit(line []byte, wantTools []string) error {
	var init claudeStrictInit
	if err := json.Unmarshal(line, &init); err != nil {
		return fmt.Errorf("%w: malformed claude init event: %w", ErrReviewToolPolicy, err)
	}
	if init.PermissionMode != "dontAsk" {
		return fmt.Errorf("%w: claude started in permission mode %q, want dontAsk", ErrReviewToolPolicy, init.PermissionMode)
	}
	got := slices.Sorted(slices.Values(init.Tools))
	want := slices.Sorted(slices.Values(wantTools))
	if !slices.Equal(got, want) {
		return fmt.Errorf("%w: claude started with tools %v, want %v", ErrReviewToolPolicy, got, want)
	}
	if len(init.MCPServers) != 0 {
		return fmt.Errorf("%w: claude started with MCP server %q", ErrReviewToolPolicy, init.MCPServers[0].Name)
	}
	return nil
}

// parseClaudeStrictStream checks the init event before any model output and
// stops at the first violation. It then parses the stream like an ordinary
// review, or a schema review when schema is set.
func parseClaudeStrictStream(r io.Reader, out io.Writer, wantTools []string, schema bool) (string, error) {
	var stream bytes.Buffer
	initialized := false
	err := scanStreamJSONLines(r, out, func(line string) error {
		stream.WriteString(line)
		stream.WriteByte('\n')
		if initialized {
			return nil
		}
		var msg struct {
			Type    string `json:"type"`
			Subtype string `json:"subtype"`
		}
		if json.Unmarshal([]byte(line), &msg) != nil {
			return nil
		}
		switch {
		case msg.Type == "system" && msg.Subtype == "init":
			initialized = true
			return validateClaudeStrictInit([]byte(line), wantTools)
		case msg.Type == "assistant" || msg.Type == "result" || msg.Type == "tool_use" || msg.Type == "tool_result":
			return fmt.Errorf("%w: claude sent %s output before its init event", ErrReviewToolPolicy, msg.Type)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if !initialized {
		return "", errors.New("claude did not report its startup tools")
	}
	if schema {
		result, err := parseClaudeClassifyStream(bytes.NewReader(stream.Bytes()))
		if err != nil {
			// Keep provider errors such as quota limits, like ordinary reviews.
			if _, streamErr := parseStreamJSON(bytes.NewReader(stream.Bytes()), nil); streamErr != nil {
				return "", streamErr
			}
		}
		return string(result), err
	}
	return parseStreamJSON(&stream, nil)
}

// parseClaudeStrictStreamOrStop calls stop as soon as the stream breaks the
// policy, so Claude does not keep running after a bad startup report.
func parseClaudeStrictStreamOrStop(r io.Reader, out io.Writer, wantTools []string, schema bool, stop func()) (string, error) {
	parsed, err := parseClaudeStrictStream(r, out, wantTools, schema)
	if errors.Is(err, ErrReviewToolPolicy) {
		stop()
	}
	return parsed, err
}

func (a *ClaudeAgent) strictReview(ctx context.Context, repoPath, prompt string, schema jsontext.Value, out io.Writer) (string, error) {
	if a.strictMCPConfigPath == "" {
		return "", fmt.Errorf("%w: strict claude review has no MCP config", ErrReviewToolPolicy)
	}
	model, baseURL, err := parseModel(a.Model)
	if err != nil {
		return "", err
	}
	flags, err := claudeHelpFlags(ctx, a.Command)
	if err != nil {
		return "", err
	}
	hasSchema := len(schema) != 0
	required := claudeStrictFlags
	if hasSchema {
		required = append(slices.Clone(required), "--json-schema")
	}
	if a.strictSettingsPath != "" {
		required = append(slices.Clone(required), "--settings")
	}
	for _, flag := range required {
		if !flags[flag] {
			return "", fmt.Errorf("%w: strict reviews need a Claude Code CLI with %s; upgrade claude", ErrReviewToolPolicy, flag)
		}
	}
	args := a.buildStrictArgs(reasoningEffort(a.Reasoning) != "" && flags[claudeEffortFlag])
	wantTools := a.strictAllowedTools()
	if hasSchema {
		args = append(args, "--json-schema", string(schema))
		wantTools = append(wantTools, "StructuredOutput")
	}
	env, err := buildClaudeEnv(os.Environ(), model, baseURL)
	if err != nil {
		return "", err
	}

	// Stop Claude as soon as its startup report breaks the policy.
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	result, err := runStreamingCLI(runCtx, streamingCLISpec{
		Name: "claude", Command: a.Command, Args: args, Dir: repoPath, Env: env,
		Stdin: strings.NewReader(prompt), Output: out,
		Parse: func(r io.Reader, sw *syncWriter) (string, error) {
			var w io.Writer
			if sw != nil {
				w = sw
			}
			return parseClaudeStrictStreamOrStop(r, w, wantTools, hasSchema, cancel)
		},
	})
	if errors.Is(result.ParseErr, ErrReviewToolPolicy) {
		return "", result.ParseErr
	}
	if err != nil {
		return "", err
	}
	if result.WaitErr != nil {
		return "", formatDetailedCLIWaitError(result, detailedCLIWaitErrorOptions{AgentName: a.Name(), Stderr: result.Stderr})
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
