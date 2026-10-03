package agent

import (
	"errors"
	"strings"

	"go.kenn.io/roborev/internal/config"
)

// Diagnosis reports whether one agent can run in the calling process's
// environment. The daemon and the CLI can disagree because each resolves
// commands against its own PATH.
type Diagnosis struct {
	Name      string `json:"name"`
	Available bool   `json:"available"`
	Command   string `json:"command,omitempty"`
	Path      string `json:"path,omitempty"`
	Error     string `json:"error,omitempty"`
	// Unknown is set when the name is neither a built-in agent nor a
	// configured ACP agent. Review resolution rejects such a name before
	// it tries any backup agent.
	Unknown bool `json:"unknown,omitempty"`
}

// DiagnoseAll reports availability for every built-in agent and every
// configured named ACP agent, excluding the internal test agent.
func DiagnoseAll(repoCfg *config.RepoConfig, cfg *config.Config) []Diagnosis {
	var out []Diagnosis
	for _, name := range AvailableNamesFromConfig(repoCfg, cfg) {
		if name == "test" {
			continue
		}
		out = append(out, Diagnose(repoCfg, name, cfg))
	}
	return out
}

// Diagnose reports whether the named agent resolves to a runnable command,
// honoring command overrides and named ACP configuration. It never falls
// back to another agent.
func Diagnose(repoCfg *config.RepoConfig, name string, cfg *config.Config) Diagnosis {
	d := Diagnosis{Name: strings.TrimSpace(name)}
	a, err := GetAvailableExactWithConfigFromConfig(repoCfg, d.Name, cfg)
	if err != nil {
		d.Error = err.Error()
		_, d.Unknown = errors.AsType[*UnknownAgentError](err)
		d.Command = expectedCommand(repoCfg, d.Name, cfg)
		return d
	}
	d.Available = true
	if ca, ok := a.(CommandAgent); ok {
		d.Command = ca.CommandName()
		d.Path, _ = resolveExecutable(d.Command)
	}
	return d
}

func expectedCommand(repoCfg *config.RepoConfig, name string, cfg *config.Config) string {
	if isConfiguredACPAgentNameFromConfig(name, cfg, repoCfg) {
		if configured, err := configuredACPAgentFromConfig(name, repoCfg, cfg); err == nil {
			return configured.CommandName()
		}
		return ""
	}
	if override := commandOverrideForAgent(name, cfg); override != "" {
		return override
	}
	a, err := Get(name)
	if err != nil {
		return ""
	}
	if ca, ok := a.(CommandAgent); ok {
		return ca.CommandName()
	}
	return ""
}
