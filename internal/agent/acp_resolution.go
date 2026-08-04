package agent

import (
	"fmt"
	"os/exec"
	"sort"
	"strings"

	"go.kenn.io/roborev/internal/agentname"
	"go.kenn.io/roborev/internal/config"
)

// ACPConfigName returns the configuration key represented by an agent name.
// Named ACP agents have exactly one identity: acp.<name>.
func ACPConfigName(name string) string {
	if configName, ok := agentname.ACPConfigName(name); ok {
		return configName
	}
	return ""
}

func namedACPAgentName(configName string) string {
	configName = strings.TrimSpace(configName)
	if configName == "" {
		return ""
	}
	return agentname.NamedACP(configName)
}

// StorageNameFromConfig returns the durable database identity for an agent.
// Named ACP entries are namespaced so stored jobs never confuse a configured
// entry with a built-in agent identity.
func StorageNameFromConfig(
	name string,
	repoCfg *config.RepoConfig,
	cfg *config.Config,
) string {
	name = strings.TrimSpace(name)
	if configName := ACPConfigName(name); configName != "" &&
		isConfiguredACPAgentNameFromConfig(name, cfg, repoCfg) {
		return namedACPAgentName(configName)
	}
	return name
}

func defaultACPAgentConfig() *config.ACPAgentConfig {
	return &config.ACPAgentConfig{
		Command:         defaultACPCommand,
		Args:            []string{},
		ReadOnlyMode:    defaultACPReadOnlyMode,
		AutoApproveMode: defaultACPAutoApproveMode,
		Mode:            defaultACPReadOnlyMode,
		Model:           "",
		Timeout:         defaultACPTimeoutSeconds,
	}
}

func isConfiguredACPAgentName(name string, cfg *config.Config, repoPath string) bool {
	var repoCfg *config.RepoConfig
	if strings.TrimSpace(repoPath) != "" {
		repoCfg, _ = config.LoadRepoConfig(repoPath)
	}
	return isConfiguredACPAgentNameFromConfig(name, cfg, repoCfg)
}

func isConfiguredACPAgentNameFromConfig(name string, cfg *config.Config, repoCfg *config.RepoConfig) bool {
	rawName := ACPConfigName(name)
	if rawName == "" {
		return false
	}
	_, ok := config.ResolveACPAgentConfigFromConfig(rawName, repoCfg, cfg)
	return ok
}

func configuredACPAgent(name, repoPath string, cfg *config.Config) (*ACPAgent, error) {
	var repoCfg *config.RepoConfig
	if strings.TrimSpace(repoPath) != "" {
		repoCfg, _ = config.LoadRepoConfig(repoPath)
	}
	return configuredACPAgentFromConfig(name, repoCfg, cfg)
}

func configuredACPAgentFromConfig(
	name string,
	repoCfg *config.RepoConfig,
	cfg *config.Config,
) (*ACPAgent, error) {
	configName := ACPConfigName(name)
	acpCfg, ok := config.ResolveACPAgentConfigFromConfig(configName, repoCfg, cfg)
	if !ok {
		return nil, fmt.Errorf("ACP agent %q is not configured", configName)
	}
	return configuredACPAgentWithConfig(configName, &acpCfg)
}

func configuredACPAgentWithConfig(name string, acpCfg *config.ACPAgentConfig) (*ACPAgent, error) {
	name = strings.TrimSpace(name)
	candidate := config.ACPAgentConfig{}
	if acpCfg != nil {
		candidate = *acpCfg
	}
	if err := config.ValidateACPAgentConfig(name, candidate); err != nil {
		return nil, err
	}
	return NewACPAgentFromConfig(namedACPAgentName(name), &candidate), nil
}

// resolveAvailableBackupWithConfig returns the first backup agent whose
// command resolves to an available binary. Named ACP backups resolve through
// their own configuration entry.
func resolveAvailableBackupWithConfig(
	preferred string,
	backups []string,
	repoCfg *config.RepoConfig,
	cfg *config.Config,
) (Agent, bool, error) {
	for _, backup := range backups {
		raw := strings.TrimSpace(backup)
		if raw == "" {
			continue
		}
		if isConfiguredACPAgentNameFromConfig(raw, cfg, repoCfg) {
			acpAgent, err := configuredACPAgentFromConfig(raw, repoCfg, cfg)
			if err != nil {
				return nil, false, err
			}
			if _, err := exec.LookPath(acpAgent.CommandName()); err == nil {
				return acpAgent, true, nil
			}
			continue
		}
		backup = resolveAlias(raw)
		if backup == preferred {
			continue
		}
		registryMu.RLock()
		_, inReg := registry[backup]
		registryMu.RUnlock()
		if inReg && isAvailableWithConfig(backup, cfg) {
			agent, _ := Get(backup)
			return applyAvailableCommand(agent, cfg), true, nil
		}
	}
	return nil, false, nil
}

// isAvailableWithConfig checks whether the named agent can be resolved
// to an executable command, considering config command overrides. If a
// config override points to an available binary, the agent is considered
// available even when the default command isn't in PATH.
//
// Overrides use the same identity validation as default commands (e.g.
// cursor_cmd must not resolve to Grok's agent alias).
func isAvailableWithConfig(name string, cfg *config.Config) bool {
	name = resolveAlias(name)
	registryMu.RLock()
	a, ok := registry[name]
	registryMu.RUnlock()
	if !ok {
		return false
	}
	ca, ok := a.(CommandAgent)
	if !ok {
		return true // non-command agents (e.g. test) are always available
	}
	// Check the configured command first — it takes priority.
	if override := commandOverrideForAgent(name, cfg); override != "" {
		return availableCommandForAgent(ca, override)
	}
	// Fall back to the default (hardcoded) command.
	return firstAvailableCommand(ca) != ""
}

// GetPreferredOrBackupWithConfig resolves an available workflow agent while
// honoring runtime ACP config and command overrides. Unlike GetAvailable, it is
// strict: it only considers the preferred agent and explicitly configured
// backups, never the package-wide hardcoded fallback chain.
func GetPreferredOrBackupWithConfig(
	repoPath string,
	preferred string,
	cfg *config.Config,
	backups ...string,
) (Agent, error) {
	var repoCfg *config.RepoConfig
	if strings.TrimSpace(repoPath) != "" {
		repoCfg, _ = config.LoadRepoConfig(repoPath)
	}
	return GetPreferredOrBackupWithConfigFromConfig(
		repoCfg, preferred, cfg, backups...,
	)
}

// GetPreferredOrBackupWithConfigFromConfig is the config-taking core of
// GetPreferredOrBackupWithConfig; it never reads repo config from disk.
func GetPreferredOrBackupWithConfigFromConfig(
	repoCfg *config.RepoConfig,
	preferred string,
	cfg *config.Config,
	backups ...string,
) (Agent, error) {
	rawPreferred := strings.TrimSpace(preferred)
	preferred = resolveAlias(rawPreferred)

	if isConfiguredACPAgentNameFromConfig(rawPreferred, cfg, repoCfg) {
		acpAgent, err := configuredACPAgentFromConfig(rawPreferred, repoCfg, cfg)
		if err != nil {
			return nil, err
		}
		if _, err := exec.LookPath(acpAgent.CommandName()); err == nil {
			return acpAgent, nil
		}
		backup, ok, err := resolveAvailableBackupWithConfig("", backups, repoCfg, cfg)
		if err != nil {
			return nil, err
		}
		if ok {
			return backup, nil
		}
		return nil, unavailablePreferredBackupError(rawPreferred, backups)
	}

	if preferred != "" {
		registryMu.RLock()
		_, knownAgent := registry[preferred]
		registryMu.RUnlock()
		if !knownAgent {
			known := AvailableNamesFromConfig(repoCfg, cfg)
			return nil, &UnknownAgentError{Name: preferred, Known: known}
		}
		if isAvailableWithConfig(preferred, cfg) {
			a, _ := Get(preferred)
			return applyAvailableCommand(a, cfg), nil
		}
	}

	backup, ok, err := resolveAvailableBackupWithConfig(preferred, backups, repoCfg, cfg)
	if err != nil {
		return nil, err
	}
	if ok {
		return backup, nil
	}

	return nil, unavailablePreferredBackupError(preferred, backups)
}

// AvailableNamesFromConfig returns built-in and effective named ACP agents.
func AvailableNamesFromConfig(repoCfg *config.RepoConfig, cfg *config.Config) []string {
	known := Available()
	for name := range config.ResolveACPAgentConfigsFromConfig(repoCfg, cfg) {
		known = append(known, namedACPAgentName(name))
	}
	sort.Strings(known)
	return known
}

func unavailablePreferredBackupError(preferred string, backups []string) error {
	return fmt.Errorf(
		"no configured agent available (preferred: %q, backups: %s)\nYou may need to run 'roborev daemon restart' from a shell that has access to your agents",
		preferred,
		strings.Join(nonEmptyResolvedAgentNames(backups), ", "),
	)
}

func nonEmptyResolvedAgentNames(names []string) []string {
	out := make([]string, 0, len(names))
	for _, name := range names {
		if s := strings.TrimSpace(name); s != "" {
			out = append(out, resolveAlias(s))
		}
	}
	return out
}

// GetAvailableWithConfig resolves an available agent while honoring named
// runtime ACP configuration and applying its command, mode, and model at
// resolution time instead of package-init time.
// It also applies command overrides for other agents (codex, claude, cursor, pi).
//
// The repoPath parameter is used to resolve repo-level ACP configuration,
// which takes precedence over global ACP configuration.
//
// Optional backup agent names are tried after the preferred agent but
// before the hardcoded fallback chain (see GetAvailable).
func GetAvailableWithConfig(repoPath string, preferred string, cfg *config.Config, backups ...string) (Agent, error) {
	var repoCfg *config.RepoConfig
	if strings.TrimSpace(repoPath) != "" {
		repoCfg, _ = config.LoadRepoConfig(repoPath)
	}
	return GetAvailableWithConfigFromConfig(repoCfg, preferred, cfg, backups...)
}

// GetAvailableExactWithConfig resolves exactly the requested agent while
// honoring command overrides and configured ACP names. Unlike
// GetAvailableWithConfig, it never falls through to backup agents or the global
// fallback chain.
func GetAvailableExactWithConfig(repoPath string, name string, cfg *config.Config) (Agent, error) {
	var repoCfg *config.RepoConfig
	if strings.TrimSpace(repoPath) != "" {
		repoCfg, _ = config.LoadRepoConfig(repoPath)
	}
	return GetAvailableExactWithConfigFromConfig(repoCfg, name, cfg)
}

// GetAvailableExactWithConfigFromConfig is like GetAvailableExactWithConfig,
// but uses an already-loaded repo config.
func GetAvailableExactWithConfigFromConfig(repoCfg *config.RepoConfig, name string, cfg *config.Config) (Agent, error) {
	rawName := strings.TrimSpace(name)
	if rawName == "" {
		return nil, fmt.Errorf("empty agent name")
	}

	if isConfiguredACPAgentNameFromConfig(rawName, cfg, repoCfg) {
		configured, err := configuredACPAgentFromConfig(rawName, repoCfg, cfg)
		if err != nil {
			return nil, err
		}
		if _, err := exec.LookPath(configured.CommandName()); err != nil {
			return nil, fmt.Errorf("agent %q command %q unavailable: %w", rawName, configured.CommandName(), err)
		}
		return configured, nil
	}

	canonical := resolveAlias(rawName)
	a, err := Get(canonical)
	if err != nil {
		return nil, &UnknownAgentError{
			Name:  canonical,
			Known: AvailableNamesFromConfig(repoCfg, cfg),
		}
	}

	if ca, ok := a.(CommandAgent); ok {
		if override := commandOverrideForAgent(canonical, cfg); override != "" {
			if !availableCommandForAgent(ca, override) {
				return nil, fmt.Errorf("agent %q command %q unavailable", canonical, override)
			}
			return applyAgentConfigOverrides(applyCommandOverrides(a, cfg), cfg), nil
		}
		if firstAvailableCommand(ca) == "" {
			return nil, fmt.Errorf("agent %q unavailable", canonical)
		}
		return applyAgentConfigOverrides(applyResolvedCommand(a), cfg), nil
	}

	return applyAgentConfigOverrides(a, cfg), nil
}

// GetAvailableWithConfigFromConfig resolves an available agent using already
// loaded repo config, never reading repo config from the working tree.
func GetAvailableWithConfigFromConfig(repoCfg *config.RepoConfig, preferred string, cfg *config.Config, backups ...string) (Agent, error) {
	rawPreferred := strings.TrimSpace(preferred)
	preferred = resolveAlias(rawPreferred)

	if isConfiguredACPAgentNameFromConfig(rawPreferred, cfg, repoCfg) {
		acpAgent, err := configuredACPAgentFromConfig(rawPreferred, repoCfg, cfg)
		if err != nil {
			return nil, err
		}
		if _, err := exec.LookPath(acpAgent.CommandName()); err == nil {
			return acpAgent, nil
		}

		backup, ok, err := resolveAvailableBackupWithConfig("", backups, repoCfg, cfg)
		if err != nil {
			return nil, err
		}
		if ok {
			return backup, nil
		}

		// Finally fall back to config-aware auto-selection.
		return getAvailableFallbackWithConfig("", repoCfg, cfg)
	}

	// Check the preferred agent using config command overrides before
	// falling back. GetAvailable only checks the hardcoded default
	// command via IsAvailable, so a configured command (e.g.
	// claude_code_cmd = "/usr/local/bin/claude-wrapper") would be
	// missed when the default binary isn't in PATH.
	if preferred != "" && cfg != nil {
		registryMu.RLock()
		_, knownAgent := registry[preferred]
		registryMu.RUnlock()
		if !knownAgent {
			return nil, &UnknownAgentError{
				Name:  preferred,
				Known: AvailableNamesFromConfig(repoCfg, cfg),
			}
		}
		if isAvailableWithConfig(preferred, cfg) {
			a, _ := Get(preferred)
			return applyAvailableCommand(a, cfg), nil
		}
	}

	// Try backup agents with config-aware availability before the
	// fallback chain. This runs regardless of whether preferred is
	// set so that backup-only configurations (preferred="" with a
	// backup_agent) still honor *_cmd overrides.
	backup, ok, err := resolveAvailableBackupWithConfig(preferred, backups, repoCfg, cfg)
	if err != nil {
		return nil, err
	}
	if ok {
		return backup, nil
	}

	if cfg != nil {
		return getAvailableFallbackWithConfig(preferred, repoCfg, cfg)
	}

	resolved, err := GetAvailable(preferred, backups...)
	if err != nil {
		return nil, err
	}
	return applyAgentConfigOverrides(applyCommandOverrides(resolved, cfg), cfg), nil
}

func getAvailableFallbackWithConfig(preferred string, repoCfg *config.RepoConfig, cfg *config.Config) (Agent, error) {
	for _, name := range fallbackAgentOrder {
		if name == preferred {
			continue
		}
		if !isAvailableWithConfig(name, cfg) {
			continue
		}
		a, _ := Get(name)
		resolved := applyAvailableCommand(a, cfg)
		return resolved, nil
	}

	var available []string
	registryMu.RLock()
	for name := range registry {
		if name != "test" && isAvailableWithConfigFromConfig(name, repoCfg, cfg) {
			available = append(available, name)
		}
	}
	registryMu.RUnlock()

	if len(available) == 0 {
		return nil, fmt.Errorf("no agents available (install one of: %s)\nYou may need to run 'roborev daemon restart' from a shell that has access to your agents", strings.Join(installHintAgentNames(), ", "))
	}

	a, _ := Get(available[0])
	return applyAvailableCommand(a, cfg), nil
}

func isAvailableWithConfigFromConfig(name string, repoCfg *config.RepoConfig, cfg *config.Config) bool {
	rawName := strings.TrimSpace(name)
	if isConfiguredACPAgentNameFromConfig(rawName, cfg, repoCfg) {
		configured, err := configuredACPAgentFromConfig(rawName, repoCfg, cfg)
		if err != nil {
			return false
		}
		_, err = exec.LookPath(configured.CommandName())
		return err == nil
	}
	return isAvailableWithConfig(resolveAlias(rawName), cfg)
}

func applyAvailableCommand(a Agent, cfg *config.Config) Agent {
	if a == nil {
		return nil
	}
	var resolved Agent
	if commandOverrideForAgent(a.Name(), cfg) != "" {
		resolved = applyCommandOverrides(a, cfg)
	} else {
		resolved = applyResolvedCommand(a)
	}
	return applyAgentConfigOverrides(resolved, cfg)
}

func applyACPAgentConfigOverride(cfg *config.ACPAgentConfig, override *config.ACPAgentConfig) {
	if cfg == nil || override == nil {
		return
	}

	if command := strings.TrimSpace(override.Command); command != "" {
		cfg.Command = command
	}
	if len(override.Args) > 0 {
		cfg.Args = append([]string(nil), override.Args...)
	}
	if readOnlyMode := strings.TrimSpace(override.ReadOnlyMode); readOnlyMode != "" {
		cfg.ReadOnlyMode = readOnlyMode
	}
	if autoApproveMode := strings.TrimSpace(override.AutoApproveMode); autoApproveMode != "" {
		cfg.AutoApproveMode = autoApproveMode
	}
	if override.DisableModeNegotiation {
		cfg.DisableModeNegotiation = true
	}
	if cfg.DisableModeNegotiation {
		cfg.Mode = ""
	} else if mode := strings.TrimSpace(override.Mode); mode != "" {
		cfg.Mode = mode
	} else {
		// If mode is omitted, default to the effective read-only mode.
		cfg.Mode = cfg.ReadOnlyMode
	}
	if model := strings.TrimSpace(override.Model); model != "" {
		cfg.Model = model
	}
	if override.Timeout > 0 {
		cfg.Timeout = override.Timeout
	}
}

func init() {
	Register(NewACPAgent(""))
}
