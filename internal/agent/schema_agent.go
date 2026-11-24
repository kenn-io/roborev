package agent

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
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
		schema jsontext.Value,
		out io.Writer,
	) (jsontext.Value, error)
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
		schema jsontext.Value,
		out io.Writer,
	) (jsontext.Value, error)
}

// structuredReviewGate is implemented by a StructuredReviewAgent whose schema
// support depends on the CLI it resolved to, such as Gemini, which has it only
// when it runs agy.
type structuredReviewGate interface {
	StructuredReviewSupported() bool
}

// AsStructuredReviewAgent returns a as a StructuredReviewAgent when it can run
// schema-constrained reviews with its resolved command.
func AsStructuredReviewAgent(a Agent) (StructuredReviewAgent, bool) {
	structured, ok := a.(StructuredReviewAgent)
	if !ok {
		return nil, false
	}
	if gate, ok := structured.(structuredReviewGate); ok && !gate.StructuredReviewSupported() {
		return nil, false
	}
	return structured, true
}

func IsStructuredReviewAgent(a Agent) bool {
	_, ok := AsStructuredReviewAgent(a)
	return ok
}

// SupportsStructuredReview reports whether the registered agent with this
// name (or alias) returns schema-constrained review output. Unknown names
// report false. Review output still must satisfy the JSON document model.
func SupportsStructuredReview(name string) bool {
	a, err := Get(name)
	return err == nil && IsStructuredReviewAgent(a)
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

	backup, err := structuredReviewCandidate(
		backupName, resolution.RepoConfig, resolution.GlobalConfig,
	)
	if err != nil {
		return fmt.Errorf("resolve backup agent %q: %w", backupName, err)
	}
	if err := ValidateStructuredReviewSelection(reviewType, backup); err != nil {
		return fmt.Errorf("invalid backup agent: %w", err)
	}
	return nil
}

// structuredReviewCandidate returns the agent a name would run as, without
// requiring it to be installed. An installed agent carries the command it
// resolved to, which decides schema support for Gemini (agy versus the legacy
// gemini CLI); an uninstalled one is checked as registered.
func structuredReviewCandidate(
	name string, repoCfg *config.RepoConfig, globalCfg *config.Config,
) (Agent, error) {
	if available, err := GetAvailableExactWithConfigFromConfig(repoCfg, name, globalCfg); err == nil {
		return available, nil
	}
	if isConfiguredACPAgentNameFromConfig(name, globalCfg, repoCfg) {
		return configuredACPAgentFromConfig(name, repoCfg, globalCfg)
	}
	return Get(name)
}

// ValidateStructuredReviewConfig rejects configuration that puts a custom
// review type on an agent that cannot run schema-constrained reviews. It checks
// each custom review type's own agent and each panel member, applying the
// selection and backup checks that enqueue applies, so a configuration that
// passes here does not fail later in the post-commit hook. Panels that do not
// resolve are left to the panel validation that reports them.
func ValidateStructuredReviewConfig(
	repoCfg *config.RepoConfig, globalCfg *config.Config,
) error {
	merged := config.MergeReviewConfigFromConfig(repoCfg, globalCfg)
	var errs []error
	for _, name := range slices.Sorted(maps.Keys(merged.Types)) {
		reasoning, err := config.ResolveReviewReasoningForTypeFromConfig(
			"", repoCfg, globalCfg, name,
		)
		if err != nil {
			continue
		}
		if err := validateStructuredReviewWorkflow(
			"", name, reasoning, repoCfg, globalCfg,
		); err != nil {
			errs = append(errs, fmt.Errorf("review type %q: %w", name, err))
		}
	}
	for _, panel := range slices.Sorted(maps.Keys(merged.Panels)) {
		members, _, err := config.ResolveCIPanel(panel, repoCfg, globalCfg)
		if err != nil {
			continue
		}
		for _, m := range members {
			if err := validateStructuredReviewWorkflow(
				m.Agent, m.ReviewType, m.Reasoning, repoCfg, globalCfg,
			); err != nil {
				errs = append(errs, fmt.Errorf(
					"panel %q member %q: %w", panel, m.Name, err,
				))
			}
		}
	}
	return errors.Join(errs...)
}

func validateStructuredReviewWorkflow(
	agentName, reviewType, reasoning string,
	repoCfg *config.RepoConfig, globalCfg *config.Config,
) error {
	if config.IsBuiltInReviewType(reviewType) {
		return nil
	}
	resolution, err := ResolveWorkflowConfigFromConfig(
		agentName, repoCfg, globalCfg,
		config.WorkflowForReviewType(reviewType), reasoning,
	)
	if err != nil {
		return err
	}
	selected, err := structuredReviewCandidate(
		resolution.PreferredAgent, repoCfg, globalCfg,
	)
	if err != nil {
		return fmt.Errorf("resolve agent %q: %w", resolution.PreferredAgent, err)
	}
	if err := ValidateStructuredReviewSelection(reviewType, selected); err != nil {
		return err
	}
	return ValidateStructuredReviewBackup(
		reviewType, resolution,
		StorageNameFromConfig(selected.Name(), repoCfg, globalCfg),
	)
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
// a SchemaAgent. Canonicalizes aliases (e.g.
// "claude" -> "claude-code") before lookup so config values that mirror the
// rest of roborev's agent-selection code (which accepts aliases) aren't
// rejected here.
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
