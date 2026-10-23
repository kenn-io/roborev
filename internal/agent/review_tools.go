package agent

import (
	"errors"
	"fmt"
	"strings"

	"go.kenn.io/roborev/internal/config"
)

// ErrReviewToolPolicy marks configuration/capability errors that must not be
// treated as a native synthesis failure eligible for raw-output fallback.
var ErrReviewToolPolicy = errors.New("invalid review tool policy")

// StrictReviewAgent is a separate capability from read-only planning or
// structured output. Implementations confine native review tools to the policy.
type StrictReviewAgent interface {
	Agent
	WithStrictReview(policy config.ReviewToolPolicy, mcpConfigPath string) Agent
}

// ValidateReviewToolSelection checks the actual adapter selected for this
// attempt. Strict requests must never downgrade to ordinary adapter behavior.
func ValidateReviewToolSelection(p *config.ReviewToolPolicy, a Agent, explicitAgentic bool) error {
	if p == nil {
		return nil
	}
	if _, err := config.CanonicalReviewToolPolicy(p); err != nil {
		return fmt.Errorf("%w: %w", ErrReviewToolPolicy, err)
	}
	if explicitAgentic {
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

// ValidateReviewToolBackup checks a distinct configured backup even when its
// executable is currently unavailable, matching structured review validation.
func ValidateReviewToolBackup(p *config.ReviewToolPolicy, resolution WorkflowConfig, selectedAgent string) error {
	if p == nil {
		return nil
	}
	if _, err := config.CanonicalReviewToolPolicy(p); err != nil {
		return fmt.Errorf("%w: %w", ErrReviewToolPolicy, err)
	}
	name := strings.TrimSpace(resolution.BackupAgent)
	if name == "" || resolution.AgentMatches(selectedAgent, name) {
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
