package agenthook

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	kitagenthook "go.kenn.io/kit/agenthook"
)

// legacyHookCommands finds registrations written by the pre-kit installer.
// Only the three profiles that installer supported have a migration path, and
// only direct roborev invocations are recognizable as owned commands. Remove
// this migration after v0.66 ships; v0.64 introduces the marker and v0.65 keeps
// one skipped-release upgrade window.
func legacyHookCommands(agent kitagenthook.Agent, path string) ([]string, error) {
	switch agent {
	case kitagenthook.AgentClaude, kitagenthook.AgentCodex, kitagenthook.AgentDroid:
	default:
		return nil, nil
	}

	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read agent hook config %s: %w", path, err)
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return nil, nil
	}

	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		return nil, fmt.Errorf("decode agent hook config %s: %w", path, err)
	}
	commands := map[string]struct{}{}
	collectLegacyHookCommands(root["hooks"], agent, commands)
	result := make([]string, 0, len(commands))
	for command := range commands {
		result = append(result, command)
	}
	sort.Strings(result)
	return result, nil
}

func collectLegacyHookCommands(value any, agent kitagenthook.Agent, commands map[string]struct{}) {
	switch typed := value.(type) {
	case map[string]any:
		if command, ok := typed["command"].(string); ok && isLegacyHookCommand(agent, command) {
			commands[command] = struct{}{}
		}
		for _, child := range typed {
			collectLegacyHookCommands(child, agent, commands)
		}
	case []any:
		for _, child := range typed {
			collectLegacyHookCommands(child, agent, commands)
		}
	}
}

func isLegacyHookCommand(agent kitagenthook.Agent, command string) bool {
	if strings.Contains(command, agentHookMarker) {
		return false
	}
	fields, err := splitHookCommand(command)
	if err != nil || len(fields) < 3 || fields[1] != "agent-hook" || fields[2] != "run" {
		return false
	}
	executable := fields[0]
	if separator := strings.LastIndexAny(executable, `/\`); separator >= 0 {
		executable = executable[separator+1:]
	}
	executable = strings.TrimSuffix(strings.ToLower(executable), ".exe")
	if executable != "roborev" {
		return false
	}

	selectedAgent := ""
	for i := 3; i < len(fields); i++ {
		switch {
		case fields[i] == "--agent":
			if i+1 >= len(fields) || selectedAgent != "" {
				return false
			}
			i++
			selectedAgent = fields[i]
		case strings.HasPrefix(fields[i], "--agent="):
			if selectedAgent != "" {
				return false
			}
			selectedAgent = strings.TrimPrefix(fields[i], "--agent=")
		}
	}

	switch agent {
	case kitagenthook.AgentClaude, kitagenthook.AgentCodex:
		return selectedAgent == ""
	case kitagenthook.AgentDroid:
		return strings.EqualFold(selectedAgent, string(kitagenthook.AgentDroid))
	default:
		return false
	}
}
