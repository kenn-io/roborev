package agent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
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

// PrepareReviewToolPolicy applies policy after all adapter, model and session
// selection. The caller owns cleanup across the complete invocation, including
// failures. Operator authority is supplied live, never read from a job snapshot.
// checkoutPath is the actual agent working directory, not the config source.
func PrepareReviewToolPolicy(a Agent, p *config.ReviewToolPolicy, cfg *config.Config, checkoutPath string, explicitAgentic bool) (Agent, func(), error) {
	noop := func() {}
	if p == nil {
		return a, noop, nil
	}
	if err := ValidateReviewToolSelection(p, a, explicitAgentic); err != nil {
		return nil, noop, err
	}
	policy, err := config.CanonicalReviewToolPolicy(p)
	if err != nil {
		return nil, noop, fmt.Errorf("%w: %w", ErrReviewToolPolicy, err)
	}
	// Selected-server preparation and native startup enforcement must land
	// together before MCP requests can execute.
	if len(policy.MCP) != 0 {
		return nil, noop, fmt.Errorf("%w: strict MCP grants are not supported yet", ErrReviewToolPolicy)
	}
	dir, err := newPrivateReviewConfigDir(checkoutPath)
	if err != nil {
		return nil, noop, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	path := filepath.Join(dir, "mcp.json")
	if err := os.WriteFile(path, []byte(`{"mcpServers":{}}`), 0o600); err != nil {
		cleanup()
		return nil, noop, fmt.Errorf("%w: cannot write review MCP config", ErrReviewToolPolicy)
	}
	if session, ok := a.(SessionAgent); ok {
		a = session.WithSessionID("")
	}
	strict, ok := a.(StrictReviewAgent)
	if !ok {
		cleanup()
		return nil, noop, fmt.Errorf("%w: agent lost strict capability after session isolation", ErrReviewToolPolicy)
	}
	return strict.WithStrictReview(*policy, path), cleanup, nil
}

// newPrivateReviewConfigDir resolves both directory roots before creating any
// artifacts. Never trust TMPDIR to place private operator configuration outside
// the actual checkout. An empty checkout uses the process cwd, like exec.Cmd.Dir.
// Errors deliberately omit filesystem paths and their underlying OS diagnostics.
func newPrivateReviewConfigDir(checkoutPath string) (string, error) {
	checkout, err := filepath.Abs(checkoutPath)
	if err != nil {
		return "", fmt.Errorf("%w: cannot resolve review checkout", ErrReviewToolPolicy)
	}
	checkout, err = filepath.EvalSymlinks(checkout)
	if err != nil {
		return "", fmt.Errorf("%w: cannot resolve review checkout", ErrReviewToolPolicy)
	}
	info, err := os.Stat(checkout)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("%w: review checkout must be a directory", ErrReviewToolPolicy)
	}
	tempRoot := os.TempDir()
	if !filepath.IsAbs(tempRoot) {
		return "", fmt.Errorf("%w: review config temporary root must be absolute", ErrReviewToolPolicy)
	}
	tempRoot, err = filepath.EvalSymlinks(tempRoot)
	if err != nil {
		return "", fmt.Errorf("%w: cannot resolve review config temporary root", ErrReviewToolPolicy)
	}
	outsideCheckout := func(path string) bool {
		rel, err := filepath.Rel(checkout, path)
		if err == nil && filepath.IsLocal(rel) {
			return false
		}
		// EvalSymlinks does not normalize case on case-insensitive volumes.
		// Compare directory identity too, so alternate spellings cannot hide
		// that the temporary root is inside the checkout.
		for {
			ancestor, err := os.Stat(path)
			if err != nil || os.SameFile(info, ancestor) {
				return false
			}
			parent := filepath.Dir(path)
			if parent == path {
				return true
			}
			path = parent
		}
	}
	if !outsideCheckout(tempRoot) {
		return "", fmt.Errorf("%w: review config temporary root must be outside the checkout", ErrReviewToolPolicy)
	}
	dir, err := os.MkdirTemp(tempRoot, "roborev-review-mcp-")
	if err != nil {
		return "", fmt.Errorf("%w: cannot create private review config directory", ErrReviewToolPolicy)
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil || !outsideCheckout(resolved) {
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("%w: private review config directory must be outside the checkout", ErrReviewToolPolicy)
	}
	return resolved, nil
}
