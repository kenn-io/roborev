package agenthook

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	kitagenthook "go.kenn.io/kit/agenthook"

	reviewagent "go.kenn.io/roborev/internal/agent"
)

const (
	AgentGrok  kitagenthook.Agent = "grok"
	AgentZcode kitagenthook.Agent = "zcode"
)

var profileExecutables = map[kitagenthook.Agent][]string{
	kitagenthook.AgentClaude:  {"claude"},
	kitagenthook.AgentCodex:   {"codex"},
	kitagenthook.AgentCopilot: {"copilot"},
	kitagenthook.AgentCursor:  {"agent"},
	kitagenthook.AgentDroid:   {"droid"},
	kitagenthook.AgentGemini:  {"gemini"},
	kitagenthook.AgentHermes:  {"hermes"},
	kitagenthook.AgentQwen:    {"qwen"},
	AgentGrok:                 {"grok"},
	AgentZcode:                {"zcode"},
}

func SelectProfiles(raw string) ([]kitagenthook.Agent, error) {
	raw = strings.ToLower(strings.TrimSpace(raw))
	if raw != "" && raw != "all" {
		if agent, ok := localProfile(raw); ok {
			return []kitagenthook.Agent{agent}, nil
		}
		agent, err := kitagenthook.ParseAgent(raw)
		if err != nil {
			return nil, err
		}
		return []kitagenthook.Agent{agent}, nil
	}

	profiles := kitagenthook.Profiles()
	localAgents := localProfiles()
	if raw == "all" {
		agents := make([]kitagenthook.Agent, 0, len(profiles)+len(localAgents))
		for _, profile := range profiles {
			agents = append(agents, profile.Agent)
		}
		return append(agents, localAgents...), nil
	}

	agents := make([]kitagenthook.Agent, 0, len(profiles)+len(localAgents))
	for _, profile := range profiles {
		if profileInstalled(profile.Agent) {
			agents = append(agents, profile.Agent)
		}
	}
	for _, agent := range localAgents {
		if profileInstalled(agent) {
			agents = append(agents, agent)
		}
	}
	if len(agents) == 0 {
		return nil, fmt.Errorf("no installed coding agents detected; select one with --agent <name> or install every profile with --agent all")
	}
	return agents, nil
}

// localProfiles lists agent hook integrations this repository implements
// without a kit profile, in stable display order.
func localProfiles() []kitagenthook.Agent {
	return []kitagenthook.Agent{AgentGrok, AgentZcode}
}

func localProfile(raw string) (kitagenthook.Agent, bool) {
	for _, agent := range localProfiles() {
		if raw == string(agent) {
			return agent, true
		}
	}
	return "", false
}

// LocalProfile resolves a repository-local agent hook profile name, such as
// grok or zcode, that kit does not know about.
func LocalProfile(raw string) (kitagenthook.Agent, bool) {
	return localProfile(strings.ToLower(strings.TrimSpace(raw)))
}

// LocalProfileDisplayName returns the harness display name for a
// repository-local agent hook profile.
func LocalProfileDisplayName(agent kitagenthook.Agent) (string, bool) {
	switch agent {
	case AgentGrok:
		return "Grok Build", true
	case AgentZcode:
		return "ZCode", true
	}
	return "", false
}

func profileInstalled(agent kitagenthook.Agent) bool {
	for _, executable := range profileExecutables[agent] {
		if agent == kitagenthook.AgentCursor && executable == "agent" {
			if reviewagent.IsAvailable("cursor") {
				return true
			}
			continue
		}
		if _, err := exec.LookPath(executable); err == nil {
			return true
		}
	}
	path := ""
	var err error
	switch agent {
	case AgentGrok:
		path = DefaultGrokHooksPath()
	case AgentZcode:
		path = DefaultZcodeConfigPath()
	default:
		path, err = kitagenthook.ConfigPath(agent)
	}
	if err != nil {
		return false
	}
	info, err := os.Stat(filepath.Dir(path))
	return err == nil && info.IsDir()
}
