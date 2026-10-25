package agent

import (
	"errors"
	"fmt"
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
