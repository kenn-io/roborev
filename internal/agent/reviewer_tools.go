package agent

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"os"
	"slices"
	"strings"
)

// ReviewerToolsServer is the MCP server name agents register the reviewer
// endpoint under. Claude exposes its tools as mcp__<server>__<tool>.
const ReviewerToolsServer = "roborev_review"

// ReviewerTools is the reviewer-only MCP endpoint one review attempt may call.
// Token is a bearer credential scoped by the daemon to that attempt.
type ReviewerTools struct {
	URL       string
	Token     string
	ToolNames []string
}

type reviewerToolsKey struct{}

// WithReviewerTools attaches the reviewer endpoint to a review run. Agents
// implementing ReviewerToolsAgent connect to it; other agents ignore it.
func WithReviewerTools(ctx context.Context, tools ReviewerTools) context.Context {
	return context.WithValue(ctx, reviewerToolsKey{}, tools)
}

func reviewerToolsFrom(ctx context.Context) (ReviewerTools, bool) {
	tools, ok := ctx.Value(reviewerToolsKey{}).(ReviewerTools)
	return tools, ok && tools.URL != "" && tools.Token != "" && !planningReadOnly(ctx)
}

// ReviewerToolsAgent is implemented by agents that give their review runs the
// reviewer endpoint from WithReviewerTools.
type ReviewerToolsAgent interface {
	Agent
	UsesReviewerTools()
}

func (a *ClaudeAgent) UsesReviewerTools() {}

func claudeReviewerToolNames(tools ReviewerTools) []string {
	names := make([]string, 0, len(tools.ToolNames))
	for _, name := range tools.ToolNames {
		names = append(names, "mcp__"+ReviewerToolsServer+"__"+name)
	}
	return names
}

// claudeReviewerArgs connects a Claude review to the reviewer endpoint only.
// --strict-mcp-config drops every user and project MCP server, and the
// reviewer tools are preapproved by exact name alongside the existing tools.
func claudeReviewerArgs(args []string, configPath string, tools ReviewerTools) []string {
	out := slices.Clone(args)
	allowed := strings.Join(claudeReviewerToolNames(tools), ",")
	for i := 0; i+1 < len(out); i++ {
		if out[i] == "--allowedTools" {
			out[i+1] += "," + allowed
		}
	}
	return append(out, "--strict-mcp-config", "--mcp-config", configPath)
}

// writeClaudeReviewerConfig writes the --mcp-config file. It is a private
// temporary file so the bearer token stays out of the process arguments.
func writeClaudeReviewerConfig(tools ReviewerTools) (string, func(), error) {
	config := map[string]any{"mcpServers": map[string]any{
		ReviewerToolsServer: map[string]any{
			"type":    "http",
			"url":     tools.URL,
			"headers": map[string]string{"Authorization": "Bearer " + tools.Token},
		},
	}}
	data, err := json.Marshal(config)
	if err != nil {
		return "", nil, fmt.Errorf("encode reviewer MCP config: %w", err)
	}
	f, err := os.CreateTemp("", "roborev-review-mcp-*.json")
	if err != nil {
		return "", nil, fmt.Errorf("create reviewer MCP config: %w", err)
	}
	cleanup := func() { _ = os.Remove(f.Name()) }
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		cleanup()
		return "", nil, fmt.Errorf("write reviewer MCP config: %w", err)
	}
	if err := f.Close(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("write reviewer MCP config: %w", err)
	}
	return f.Name(), cleanup, nil
}

// withReviewerTools adds the reviewer endpoint to Claude review args when the
// run carries one. The returned cleanup is never nil.
func (a *ClaudeAgent) withReviewerTools(ctx context.Context, args []string) ([]string, func(), error) {
	tools, ok := reviewerToolsFrom(ctx)
	if !ok {
		return args, func() {}, nil
	}
	path, cleanup, err := writeClaudeReviewerConfig(tools)
	if err != nil {
		return nil, nil, err
	}
	return claudeReviewerArgs(args, path, tools), cleanup, nil
}

// codexReviewerTokenEnv carries the reviewer token to Codex, which reads it
// through bearer_token_env_var so it stays out of the process arguments.
const codexReviewerTokenEnv = "ROBOREV_REVIEW_MCP_TOKEN"

func (a *CodexAgent) UsesReviewerTools() {}

// codexReviewerArgs registers the reviewer endpoint as a Codex MCP server
// limited to the reviewer tools. Codex calls MCP servers from its own process,
// not from the sandboxed shell, so the read-only sandbox stays unchanged. The
// flags follow --json, ahead of any session ID and the trailing "-".
func codexReviewerArgs(args []string, tools ReviewerTools) []string {
	enabled := make([]string, 0, len(tools.ToolNames))
	for _, name := range tools.ToolNames {
		enabled = append(enabled, fmt.Sprintf("%q", name))
	}
	prefix := "mcp_servers." + ReviewerToolsServer + "."
	server := []string{
		"-c", prefix + "url=" + fmt.Sprintf("%q", tools.URL),
		"-c", prefix + `bearer_token_env_var="` + codexReviewerTokenEnv + `"`,
		"-c", prefix + "enabled_tools=[" + strings.Join(enabled, ",") + "]",
	}
	insert := slices.Index(args, "--json") + 1
	return slices.Insert(slices.Clone(args), insert, server...)
}
