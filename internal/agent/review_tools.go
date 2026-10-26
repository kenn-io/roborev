package agent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	WithStrictReview(policy config.ReviewToolPolicy, mcpConfigPath string) Agent
}

// ValidateReviewToolSelection checks that agent a can run a review under
// policy p. A strict policy must never fall back to an unrestricted run.
func ValidateReviewToolSelection(p *config.ReviewToolPolicy, a Agent, agentic bool) error {
	if p == nil {
		return nil
	}
	policy, err := config.CanonicalReviewToolPolicy(p)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrReviewToolPolicy, err)
	}
	if agentic {
		return fmt.Errorf("%w: strict review tools cannot be combined with agentic mode", ErrReviewToolPolicy)
	}
	if len(policy.MCP) != 0 {
		return fmt.Errorf("%w: strict MCP grants are not supported yet", ErrReviewToolPolicy)
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
// strict policy gets a private MCP config file, a private copy of the Claude
// user settings' credential keys, and a fresh session; a nil policy returns a
// unchanged.
func PrepareReviewToolPolicy(a Agent, p *config.ReviewToolPolicy) (Agent, func(), error) {
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
	path, cleanup, err := writeReviewMCPConfig()
	if err != nil {
		return nil, noop, err
	}
	strict := a.(StrictReviewAgent).WithStrictReview(*policy, path)
	if c, ok := strict.(*ClaudeAgent); ok {
		settings, err := writeClaudeStrictSettings(filepath.Dir(path))
		if err != nil {
			cleanup()
			return nil, noop, err
		}
		c.strictSettingsPath = settings
	}
	return strict, cleanup, nil
}

// writeReviewMCPConfig writes the MCP config a strict review loads to a new
// private temp directory. It lists no servers.
func writeReviewMCPConfig() (string, func(), error) {
	dir, err := os.MkdirTemp("", "roborev-review-mcp-")
	if err != nil {
		return "", nil, fmt.Errorf("create review MCP config: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	path := filepath.Join(dir, "mcp.json")
	if err := os.WriteFile(path, []byte(`{"mcpServers":{}}`+"\n"), 0o600); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("write review MCP config: %w", err)
	}
	return path, cleanup, nil
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
