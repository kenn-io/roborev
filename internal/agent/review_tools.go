package agent

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"go.kenn.io/roborev/internal/config"
)

// ErrReviewToolPolicy marks a review tool policy that is invalid or that the
// selected agent cannot enforce.
var ErrReviewToolPolicy = errors.New("invalid review tool policy")

// StrictReviewAgent is implemented by adapters that can confine a review to
// read-only native tools and the exact MCP tools named in the policy.
type StrictReviewAgent interface {
	Agent
	WithStrictReview(policy config.ReviewToolPolicy, mcp ReviewMCP) Agent
}

// ReviewMCP is the MCP access prepared for one strict review.
type ReviewMCP struct {
	ConfigPath string   // private MCP config file that lists only Servers
	Servers    []string // selected server names, sorted
	Tools      []string // exact native tool names the review may call, sorted
}

// ValidateReviewToolSelection checks that agent a can run a review under
// policy p. A strict policy must never fall back to an unrestricted run.
func ValidateReviewToolSelection(p *config.ReviewToolPolicy, a Agent, agentic bool) error {
	if p == nil {
		return nil
	}
	if _, err := config.CanonicalReviewToolPolicy(p); err != nil {
		return fmt.Errorf("%w: %w", ErrReviewToolPolicy, err)
	}
	if agentic {
		return fmt.Errorf("%w: strict review tools cannot be combined with agentic mode", ErrReviewToolPolicy)
	}
	if a == nil {
		return fmt.Errorf("%w: strict review tools require a selected agent", ErrReviewToolPolicy)
	}
	if _, ok := a.(StrictReviewAgent); !ok {
		return fmt.Errorf("%w: agent %q does not support strict review tools", ErrReviewToolPolicy, a.Name())
	}
	return nil
}

// ValidateReviewToolBackup checks a distinct configured backup agent, even
// when it is not installed right now, since failover may select it later.
func ValidateReviewToolBackup(p *config.ReviewToolPolicy, resolution WorkflowConfig, selectedAgent string) error {
	name := strings.TrimSpace(resolution.BackupAgent)
	if p == nil || name == "" || resolution.AgentMatches(selectedAgent, name) {
		return nil
	}
	var backup Agent
	var err error
	if isConfiguredACPAgentNameFromConfig(name, resolution.GlobalConfig, resolution.RepoConfig) {
		backup, err = configuredACPAgentFromConfig(name, resolution.RepoConfig, resolution.GlobalConfig)
	} else {
		backup, err = Get(name)
	}
	if err != nil {
		return fmt.Errorf("%w: resolve backup agent %q: %w", ErrReviewToolPolicy, name, err)
	}
	if err := ValidateReviewToolSelection(p, backup, false); err != nil {
		return fmt.Errorf("invalid backup agent: %w", err)
	}
	return nil
}

// PrepareReviewToolPolicy returns the agent to run a review under policy p and
// a cleanup func the caller must run once the review ends, on every path. A
// strict policy gets a private MCP config holding only its granted servers,
// checked against cfg's current operator ceiling, a private copy of the
// Claude user settings' credential keys, and a fresh session. A nil policy
// returns a unchanged.
func PrepareReviewToolPolicy(a Agent, p *config.ReviewToolPolicy, cfg *config.Config) (Agent, func(), error) {
	noop := func() {}
	if p == nil {
		return a, noop, nil
	}
	if err := ValidateReviewToolSelection(p, a, false); err != nil {
		return nil, noop, err
	}
	policy, err := config.CanonicalReviewToolPolicy(p)
	if err != nil {
		return nil, noop, fmt.Errorf("%w: %w", ErrReviewToolPolicy, err)
	}
	selection, err := selectReviewMCP(cfg, policy)
	if err != nil {
		return nil, noop, err
	}
	mcp, cleanup, err := writeReviewMCPConfig(selection)
	if err != nil {
		return nil, noop, err
	}
	strict := a.(StrictReviewAgent).WithStrictReview(*policy, mcp)
	if c, ok := strict.(*ClaudeAgent); ok {
		settings, err := writeClaudeStrictSettings(filepath.Dir(mcp.ConfigPath))
		if err != nil {
			cleanup()
			return nil, noop, err
		}
		c.strictSettingsPath = settings
	}
	return strict, cleanup, nil
}

// reviewMCPSelection is the MCP access of one strict review: the server
// definitions its private config lists and the native tool names it may call.
type reviewMCPSelection struct {
	servers map[string]jsontext.Value
	tools   []string
}

// selectReviewMCP checks each grant of a canonical policy against the
// operator's review_mcp_allowed_tools and copies only the granted servers'
// definitions from review_mcp_config. Without grants it reads no file.
func selectReviewMCP(cfg *config.Config, policy *config.ReviewToolPolicy) (reviewMCPSelection, error) {
	sel := reviewMCPSelection{servers: map[string]jsontext.Value{}, tools: []string{}}
	if len(policy.MCP) == 0 {
		return sel, nil
	}
	var ceiling []string
	if cfg != nil {
		ceiling = cfg.ReviewMCPAllowedTools
	}
	for server, tools := range policy.MCP {
		for _, tool := range tools {
			if !slices.Contains(ceiling, server+"/"+tool) {
				return sel, fmt.Errorf("%w: MCP tool %q is not in review_mcp_allowed_tools", ErrReviewToolPolicy, server+"/"+tool)
			}
			sel.tools = append(sel.tools, "mcp__"+server+"__"+tool)
		}
	}
	slices.Sort(sel.tools)

	path := cfg.ReviewMCPConfig
	if !filepath.IsAbs(path) {
		return sel, fmt.Errorf("%w: MCP grants need review_mcp_config set to an absolute path, got %q", ErrReviewToolPolicy, path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return sel, fmt.Errorf("%w: read review_mcp_config: %w", ErrReviewToolPolicy, err)
	}
	var file struct {
		Servers map[string]jsontext.Value `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		return sel, fmt.Errorf("%w: parse review_mcp_config %s: %w", ErrReviewToolPolicy, path, err)
	}
	for server := range policy.MCP {
		definition, ok := file.Servers[server]
		if !ok {
			return sel, fmt.Errorf("%w: review_mcp_config %s has no server %q", ErrReviewToolPolicy, path, server)
		}
		sel.servers[server] = definition
	}
	return sel, nil
}

// writeReviewMCPConfig writes the selected servers to a new private temp
// directory in Claude's native {"mcpServers": {...}} format.
func writeReviewMCPConfig(sel reviewMCPSelection) (ReviewMCP, func(), error) {
	data, err := json.Marshal(struct {
		Servers map[string]jsontext.Value `json:"mcpServers"`
	}{sel.servers})
	if err != nil {
		return ReviewMCP{}, nil, fmt.Errorf("encode review MCP config: %w", err)
	}
	dir, err := os.MkdirTemp("", "roborev-review-mcp-")
	if err != nil {
		return ReviewMCP{}, nil, fmt.Errorf("create review MCP config: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	path := filepath.Join(dir, "mcp.json")
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		cleanup()
		return ReviewMCP{}, nil, fmt.Errorf("write review MCP config: %w", err)
	}
	return ReviewMCP{
		ConfigPath: path,
		Servers:    slices.Sorted(maps.Keys(sel.servers)),
		Tools:      slices.Clone(sel.tools),
	}, cleanup, nil
}

// CheckStrictReviewPrepared fails unless a came from PrepareReviewToolPolicy,
// so a strict review never runs with an adapter's ordinary permissions.
func CheckStrictReviewPrepared(p *config.ReviewToolPolicy, a Agent) error {
	if p == nil {
		return nil
	}
	if s, ok := a.(interface{ StrictReviewMCPConfig() string }); ok && s.StrictReviewMCPConfig() != "" {
		return nil
	}
	return fmt.Errorf("%w: agent %q was not prepared for a strict review", ErrReviewToolPolicy, a.Name())
}
