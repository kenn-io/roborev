package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"go.kenn.io/roborev/internal/config"
)

// SchemaAgent is an optional Agent capability. Implementers return a single
// JSON document conforming to the given JSON Schema, via the underlying CLI's
// native structured-output mechanism (not via prompt nagging).
type SchemaAgent interface {
	Agent

	// ClassifyWithSchema runs one agent turn constrained by `schema` and
	// returns the raw JSON result. `out` receives progress/log lines but not
	// the structured result itself.
	ClassifyWithSchema(
		ctx context.Context,
		repoPath, gitRef, prompt string,
		schema json.RawMessage,
		out io.Writer,
	) (json.RawMessage, error)
}

// StructuredReviewAgent is an optional Agent capability for reviews whose
// final result is constrained by a JSON Schema. Unlike SchemaAgent's
// classification turn, this mode retains the normal read-only repository
// tools needed to inspect code.
type StructuredReviewAgent interface {
	Agent

	ReviewWithSchema(
		ctx context.Context,
		repoPath, gitRef, prompt string,
		schema json.RawMessage,
		out io.Writer,
	) (json.RawMessage, error)
}

func IsStructuredReviewAgent(a Agent) bool {
	_, ok := a.(StructuredReviewAgent)
	return ok
}

// ValidateStructuredReviewSelection rejects a resolved agent that cannot run
// a schema-constrained custom review. Built-in review types use prose output
// and accept every Agent implementation.
func ValidateStructuredReviewSelection(reviewType string, a Agent) error {
	if config.IsBuiltInReviewType(reviewType) || IsStructuredReviewAgent(a) {
		return nil
	}
	return fmt.Errorf(
		"agent %q does not support schema-constrained reviews", a.Name(),
	)
}

// ValidateStructuredReviewBackup rejects a distinct configured backup that
// cannot run a schema-constrained custom review. Availability is deliberately
// not checked: workers resolve workflow backups again at failover time, so a
// configured backup that is unavailable during enqueue may still be selected
// later.
func ValidateStructuredReviewBackup(
	reviewType string,
	resolution WorkflowConfig,
	selectedAgent string,
) error {
	backupName := strings.TrimSpace(resolution.BackupAgent)
	if config.IsBuiltInReviewType(reviewType) || backupName == "" ||
		resolution.AgentMatches(selectedAgent, backupName) {
		return nil
	}

	var backup Agent
	var err error
	if isConfiguredACPAgentNameFromConfig(
		backupName, resolution.GlobalConfig, resolution.RepoConfig,
	) {
		backup, err = configuredACPAgentFromConfig(
			backupName, resolution.RepoConfig, resolution.GlobalConfig,
		)
	} else {
		backup, err = Get(backupName)
	}
	if err != nil {
		return fmt.Errorf("resolve backup agent %q: %w", backupName, err)
	}
	if err := ValidateStructuredReviewSelection(reviewType, backup); err != nil {
		return fmt.Errorf("invalid backup agent: %w", err)
	}
	return nil
}

// IsSchemaAgent reports whether a is a SchemaAgent.
func IsSchemaAgent(a Agent) bool {
	_, ok := a.(SchemaAgent)
	return ok
}

// GetAvailableSchemaExactWithConfig resolves exactly the requested
// schema-capable agent and checks availability using the same config-aware
// command override rules as normal review execution.
func GetAvailableSchemaExactWithConfig(name string, cfg *config.Config) (SchemaAgent, error) {
	canonical := resolveAlias(name)
	registryMu.RLock()
	a, ok := registry[canonical]
	registryMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("classifier %q not registered", name)
	}
	if !IsSchemaAgent(a) {
		return nil, fmt.Errorf("classify_agent %q is not a SchemaAgent", name)
	}
	if !isAvailableWithConfig(canonical, cfg) {
		return nil, fmt.Errorf("classifier %q not installed (CLI not on PATH)", name)
	}
	resolved := applyAvailableCommand(a, cfg)
	sa, ok := resolved.(SchemaAgent)
	if !ok {
		return nil, fmt.Errorf("classify_agent %q lost SchemaAgent capability after command resolution", name)
	}
	return sa, nil
}

// GetAvailableSchemaWithConfig resolves an installed schema-capable agent,
// trying the preferred agent first, then configured backups, then roborev's
// normal fallback order. Non-schema agents are skipped during fallback.
func GetAvailableSchemaWithConfig(preferred string, cfg *config.Config, backups ...string) (SchemaAgent, error) {
	preferred = resolveAlias(preferred)
	if preferred != "" {
		if sa, err := GetAvailableSchemaExactWithConfig(preferred, cfg); err == nil {
			return sa, nil
		}
	}

	for _, backup := range backups {
		backup = resolveAlias(backup)
		if backup == "" || backup == preferred {
			continue
		}
		if sa, err := GetAvailableSchemaExactWithConfig(backup, cfg); err == nil {
			return sa, nil
		}
	}

	for _, name := range fallbackAgentOrder {
		if name == preferred {
			continue
		}
		if sa, err := GetAvailableSchemaExactWithConfig(name, cfg); err == nil {
			return sa, nil
		}
	}

	names := availableSchemaAgentNames()
	if len(names) == 0 {
		names = []string{"claude-code"}
	}
	return nil, fmt.Errorf(
		"no schema-capable classifier agents available (install one of: %s)",
		strings.Join(names, ", "),
	)
}

func availableSchemaAgentNames() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	names := make([]string, 0, len(registry))
	for _, name := range fallbackAgentOrder {
		a, ok := registry[name]
		if ok && IsSchemaAgent(a) {
			names = append(names, name)
		}
	}
	return names
}

// ValidateClassifyAgent errors when the named agent isn't registered or isn't
// a SchemaAgent. Canonicalizes aliases (e.g. "claude" -> "claude-code")
// before lookup so config values that mirror the rest of roborev's
// agent-selection code (which accepts aliases) aren't rejected here.
// Registered with config at init() time.
func ValidateClassifyAgent(name string) error {
	canonical := resolveAlias(name)
	registryMu.RLock()
	a, ok := registry[canonical]
	if !ok {
		registryMu.RUnlock()
		return fmt.Errorf("unknown agent %q", name)
	}
	if IsSchemaAgent(a) {
		registryMu.RUnlock()
		return nil
	}
	var valid []string
	for n, r := range registry {
		if IsSchemaAgent(r) {
			valid = append(valid, n)
		}
	}
	registryMu.RUnlock()
	return fmt.Errorf(
		"agent %q does not support structured output (classify_agent must be one of: %v)",
		name, valid)
}

func init() {
	config.RegisterClassifyAgentValidator(ValidateClassifyAgent)
}
