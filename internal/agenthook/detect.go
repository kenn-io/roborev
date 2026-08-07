package agenthook

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"

	kitagenthook "go.kenn.io/kit/agenthook"
)

func Installed(agent kitagenthook.Agent, path string) (bool, error) {
	result, err := kitagenthook.PlanUninstall(agent, path, agentHookMarker)
	if err != nil {
		return false, err
	}
	return result.Changed, nil
}

func InstalledForAgent(path, agent string) (bool, error) {
	if strings.EqualFold(strings.TrimSpace(agent), string(AgentGrok)) {
		body, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		var root any
		if err := json.Unmarshal(body, &root); err != nil {
			return false, err
		}
		return containsGrokHook(root), nil
	}
	profile, err := kitagenthook.ParseAgent(agent)
	if err != nil {
		return false, fmt.Errorf("unsupported agent %q", agent)
	}
	return Installed(profile, path)
}

func containsGrokHook(value any) bool {
	switch typed := value.(type) {
	case []any:
		return slices.ContainsFunc(typed, containsGrokHook)
	case map[string]any:
		if command, ok := typed["command"].(string); ok && isGrokHookCommand(command) {
			return true
		}
		for _, child := range typed {
			if containsGrokHook(child) {
				return true
			}
		}
	}
	return false
}

func isGrokHookCommand(command string) bool {
	agent, err := commandAgent(command)
	return err == nil && agent == AgentGrok
}
