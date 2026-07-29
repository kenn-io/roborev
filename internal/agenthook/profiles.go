package agenthook

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	kitagenthook "go.kenn.io/kit/agenthook"
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
}

func SelectProfiles(raw string) ([]kitagenthook.Agent, error) {
	raw = strings.ToLower(strings.TrimSpace(raw))
	if raw != "" && raw != "all" {
		agent, err := kitagenthook.ParseAgent(raw)
		if err != nil {
			return nil, err
		}
		return []kitagenthook.Agent{agent}, nil
	}

	profiles := kitagenthook.Profiles()
	if raw == "all" {
		agents := make([]kitagenthook.Agent, 0, len(profiles))
		for _, profile := range profiles {
			agents = append(agents, profile.Agent)
		}
		return agents, nil
	}

	agents := make([]kitagenthook.Agent, 0, len(profiles))
	for _, profile := range profiles {
		if profileInstalled(profile.Agent) {
			agents = append(agents, profile.Agent)
		}
	}
	if len(agents) == 0 {
		return nil, fmt.Errorf("no installed coding agents detected; select one with --agent <name> or install every profile with --agent all")
	}
	return agents, nil
}

func profileInstalled(agent kitagenthook.Agent) bool {
	for _, executable := range profileExecutables[agent] {
		if _, err := exec.LookPath(executable); err == nil {
			return true
		}
	}
	path, err := kitagenthook.ConfigPath(agent)
	if err != nil {
		return false
	}
	info, err := os.Stat(filepath.Dir(path))
	return err == nil && info.IsDir()
}
