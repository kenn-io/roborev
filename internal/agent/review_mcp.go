package agent

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"go.kenn.io/roborev/internal/config"
)

// prepareReviewMCPConfig selects native definitions using the current operator
// ceiling. Callers supply freshly loaded global config on each attempt and
// defer cleanup across the entire invocation, including cancellation. checkoutPath
// is the actual agent working directory, which can differ from process cwd.
func prepareReviewMCPConfig(p config.ReviewToolPolicy, cfg *config.Config, checkoutPath string) (path string, cleanup func(), err error) {
	noop := func() {}
	policy, err := config.CanonicalReviewToolPolicy(&p)
	if err != nil {
		return "", noop, fmt.Errorf("%w: %w", ErrReviewToolPolicy, err)
	}
	selected := make(map[string]jsontext.Value)
	allowed := make(map[string]bool)
	if cfg != nil {
		for _, pair := range cfg.ReviewMCPAllowedTools {
			allowed[pair] = true
		}
	}
	for server, tools := range policy.MCP {
		for _, tool := range tools {
			if !allowed[server+"/"+tool] {
				return "", noop, fmt.Errorf("%w: review MCP tool %q exceeds the operator ceiling", ErrReviewToolPolicy, server+"/"+tool)
			}
		}
		if len(tools) > 0 {
			selected[server] = nil
		}
	}
	if len(selected) > 0 {
		if cfg == nil || cfg.ReviewMCPConfig == "" {
			return "", noop, fmt.Errorf("%w: review_mcp_config is required for MCP grants", ErrReviewToolPolicy)
		}
		// A checkout-relative file could let a repository shadow the operator's
		// server definitions. Config carries no source directory to rebase it on.
		if !filepath.IsAbs(cfg.ReviewMCPConfig) {
			return "", noop, fmt.Errorf("%w: review MCP config %q must be an absolute path", ErrReviewToolPolicy, cfg.ReviewMCPConfig)
		}
		data, err := os.ReadFile(cfg.ReviewMCPConfig)
		if err != nil {
			return "", noop, fmt.Errorf("%w: cannot read review MCP config %q", ErrReviewToolPolicy, cfg.ReviewMCPConfig)
		}
		var native struct {
			Servers map[string]jsontext.Value `json:"mcpServers"`
		}
		if err := json.Unmarshal(data, &native); err != nil || native.Servers == nil {
			// Decoder errors can contain values from secret-bearing definitions.
			return "", noop, fmt.Errorf("%w: malformed review MCP config %q", ErrReviewToolPolicy, cfg.ReviewMCPConfig)
		}
		for server := range selected {
			definition, ok := native.Servers[server]
			if !ok {
				return "", noop, fmt.Errorf("%w: review MCP config %q has no server %q", ErrReviewToolPolicy, cfg.ReviewMCPConfig, server)
			}
			if !validReviewMCPDefinition(definition) {
				return "", noop, fmt.Errorf("%w: invalid server %q in review MCP config %q", ErrReviewToolPolicy, server, cfg.ReviewMCPConfig)
			}
			selected[server] = definition
		}
	}
	data, err := json.Marshal(struct {
		Servers map[string]jsontext.Value `json:"mcpServers"`
	}{selected})
	if err != nil {
		return "", noop, fmt.Errorf("%w: cannot encode selected review MCP config", ErrReviewToolPolicy)
	}
	dir, err := newPrivateReviewConfigDir(checkoutPath)
	if err != nil {
		return "", noop, err
	}
	cleanup = func() { _ = os.RemoveAll(dir) }
	path = filepath.Join(dir, "mcp.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		cleanup()
		return "", noop, fmt.Errorf("%w: cannot write selected review MCP config", ErrReviewToolPolicy)
	}
	return path, cleanup, nil
}

// Validate native field shapes without round-tripping the definition through a
// reduced schema. Unknown native fields survive selection unchanged.
func validReviewMCPDefinition(definition jsontext.Value) bool {
	var fields map[string]jsontext.Value
	if err := json.Unmarshal(definition, &fields); err != nil || fields == nil {
		return false
	}
	for name, raw := range fields {
		switch name {
		case "type", "command", "url":
			if raw.Kind() != '"' {
				return false
			}
		case "args":
			var args []any
			if err := json.Unmarshal(raw, &args); err != nil || args == nil {
				return false
			}
			for _, arg := range args {
				if _, ok := arg.(string); !ok {
					return false
				}
			}
		case "env", "headers":
			var values map[string]any
			if err := json.Unmarshal(raw, &values); err != nil || values == nil {
				return false
			}
			for _, value := range values {
				if _, ok := value.(string); !ok {
					return false
				}
			}
		}
	}
	var endpoint struct {
		Type    string `json:"type"`
		Command string `json:"command"`
		URL     string `json:"url"`
	}
	if err := json.Unmarshal(definition, &endpoint); err != nil {
		return false
	}
	switch endpoint.Type {
	case "", "stdio":
		return endpoint.Command != "" && endpoint.URL == ""
	case "http", "streamable-http", "sse", "ws":
		return endpoint.URL != ""
	default:
		return false
	}
}

// nativeReviewToolNames maps validated logical grants to exact native names.
func nativeReviewToolNames(p config.ReviewToolPolicy) []string {
	names := []string{}
	for server, tools := range p.MCP {
		for _, tool := range tools {
			names = append(names, "mcp__"+server+"__"+tool)
		}
	}
	slices.Sort(names)
	return slices.Compact(names)
}
