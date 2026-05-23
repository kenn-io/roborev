package agent

import (
	"os/exec"
	"strings"

	"go.kenn.io/roborev/internal/config"
)

func defaultACPAgentConfig() *config.ACPAgentConfig {
	return &config.ACPAgentConfig{
		Name:            defaultACPName,
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
	rawName := strings.TrimSpace(name)
	if rawName == defaultACPName {
		return true
	}

	// Check if there's a configured ACP name in either repo or global config
	acpCfg := config.ResolveACPAgentConfig(repoPath, cfg)
	if acpCfg == nil {
		return false
	}

	configuredName := strings.TrimSpace(acpCfg.Name)
	if rawName == "" || configuredName == "" {
		return false
	}

	// Exact match only — no alias resolution. This prevents collisions
	// where an alias like "agent" → "cursor" would incorrectly route
	// cursor requests to ACP. Callers pass rawPreferred (pre-alias) so
	// `acp.name = "claude"` matches request "claude" but not "claude-code".
	return rawName == configuredName
}

func configuredACPAgent(repoPath string, cfg *config.Config) *ACPAgent {
	acpCfg := config.ResolveACPAgentConfig(repoPath, cfg)
	resolved := NewACPAgentFromConfig(acpCfg)
	// Keep a stable canonical name in runtime state.
	resolved.agentName = defaultACPName
	return resolved
}

func resolveAvailableBackupWithConfig(preferred string, backups []string, cfg *config.Config) (Agent, bool) {
	for _, backup := range backups {
		backup = resolveAlias(backup)
		if backup == "" || backup == preferred {
			continue
		}
		registryMu.RLock()
		_, inReg := registry[backup]
		registryMu.RUnlock()
		if inReg && isAvailableWithConfig(backup, cfg) {
			agent, _ := Get(backup)
			return applyAvailableCommand(agent, cfg), true
		}
	}
	return nil, false
}

// isAvailableWithConfig checks whether the named agent can be resolved
// to an executable command, considering config command overrides. If a
// config override points to an available binary, the agent is considered
// available even when the default command isn't in PATH.
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
		if _, err := exec.LookPath(override); err == nil {
			return true
		}
	}
	// Fall back to the default (hardcoded) command.
	return firstAvailableCommand(ca) != ""
}

// GetAvailableWithConfig resolves an available agent while honoring runtime ACP config.
// It treats cfg.ACP.Name as an alias for "acp" and applies cfg.ACP command/mode/model
// at resolution time instead of package-init time.
// It also applies command overrides for other agents (codex, claude, cursor, pi).
//
// The repoPath parameter is used to resolve repo-level ACP configuration,
// which takes precedence over global ACP configuration.
//
// Optional backup agent names are tried after the preferred agent but
// before the hardcoded fallback chain (see GetAvailable).
func GetAvailableWithConfig(repoPath string, preferred string, cfg *config.Config, backups ...string) (Agent, error) {
	rawPreferred := strings.TrimSpace(preferred)
	preferred = resolveAlias(rawPreferred)

	if isConfiguredACPAgentName(rawPreferred, cfg, repoPath) {
		acpAgent := configuredACPAgent(repoPath, cfg)
		if _, err := exec.LookPath(acpAgent.CommandName()); err == nil {
			return acpAgent, nil
		}
		// ACP requested with an invalid configured command. Try canonical ACP next.
		if canonicalACP, err := Get(defaultACPName); err == nil {
			if commandAgent, ok := canonicalACP.(CommandAgent); !ok {
				return canonicalACP, nil
			} else if _, err := exec.LookPath(commandAgent.CommandName()); err == nil {
				return canonicalACP, nil
			}
		}

		// ACP unavailable — try backup agents with config-aware
		// availability so *_cmd overrides are honored.
		if backup, ok := resolveAvailableBackupWithConfig("", backups, cfg); ok {
			return backup, nil
		}

		// Finally fall back to normal auto-selection.
		return GetAvailable("", backups...)
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
			// Unknown agent — let GetAvailable produce the error.
			return GetAvailable(preferred, backups...)
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
	if backup, ok := resolveAvailableBackupWithConfig(preferred, backups, cfg); ok {
		return backup, nil
	}

	resolved, err := GetAvailable(preferred, backups...)
	if err != nil {
		return nil, err
	}
	if resolved.Name() == defaultACPName {
		configured := configuredACPAgent(repoPath, cfg)
		if _, err := exec.LookPath(configured.CommandName()); err == nil {
			return configured, nil
		}
		return resolved, nil
	}

	return applyCommandOverrides(resolved, cfg), nil
}

func applyAvailableCommand(a Agent, cfg *config.Config) Agent {
	if a == nil {
		return nil
	}
	if commandOverrideForAgent(a.Name(), cfg) != "" {
		return applyCommandOverrides(a, cfg)
	}
	return applyResolvedCommand(a)
}

func applyACPAgentConfigOverride(cfg *config.ACPAgentConfig, override *config.ACPAgentConfig) {
	if cfg == nil || override == nil {
		return
	}

	if name := strings.TrimSpace(override.Name); name != "" {
		cfg.Name = name
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
