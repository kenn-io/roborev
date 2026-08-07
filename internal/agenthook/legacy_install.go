package agenthook

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	kitagenthook "go.kenn.io/kit/agenthook"
)

// planLegacyHookMigration removes registrations written by the pre-kit
// installer from kit's complete planned configuration without modifying the
// source file. Only the three profiles that installer supported have a
// migration path, and only direct roborev invocations are recognizable as
// owned commands. Remove this migration after v0.66 ships; v0.64 introduces
// the marker and v0.65 keeps one skipped-release upgrade window. See #1012.
func planLegacyHookMigration(
	agent kitagenthook.Agent,
	result kitagenthook.Result,
) (kitagenthook.Result, error) {
	switch agent {
	case kitagenthook.AgentClaude, kitagenthook.AgentCodex, kitagenthook.AgentDroid:
	default:
		return result, nil
	}

	decoder := json.NewDecoder(bytes.NewReader(result.Data))
	decoder.UseNumber()
	var root map[string]any
	if err := decoder.Decode(&root); err != nil {
		return kitagenthook.Result{}, fmt.Errorf(
			"decode planned agent hook config %s: %w", result.ConfigPath, err,
		)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		return kitagenthook.Result{}, fmt.Errorf(
			"decode planned agent hook config %s: %w", result.ConfigPath, err,
		)
	}

	changed, err := removeLegacyHookCommands(root, agent, result.ConfigPath)
	if err != nil {
		return kitagenthook.Result{}, err
	}
	if !changed {
		return result, nil
	}
	data, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return kitagenthook.Result{}, fmt.Errorf(
			"encode planned agent hook config %s: %w", result.ConfigPath, err,
		)
	}
	result.Data = append(data, '\n')
	result.Changed = true
	return result, nil
}

func removeLegacyHookCommands(
	root map[string]any,
	agent kitagenthook.Agent,
	path string,
) (bool, error) {
	rawHooks, ok := root["hooks"]
	if !ok || rawHooks == nil {
		return false, nil
	}
	hooks, ok := rawHooks.(map[string]any)
	if !ok {
		return false, fmt.Errorf("agent hook config %s field %q must be an object", path, "hooks")
	}
	changed := false
	for event, rawEntries := range hooks {
		entries, ok := rawEntries.([]any)
		if !ok {
			return false, fmt.Errorf("agent hook config %s event %q must be an array", path, event)
		}
		keptEntries := make([]any, 0, len(entries))
		for _, rawEntry := range entries {
			entry, ok := rawEntry.(map[string]any)
			if !ok {
				keptEntries = append(keptEntries, rawEntry)
				continue
			}
			rawHandlers, ok := entry["hooks"]
			if !ok || rawHandlers == nil {
				keptEntries = append(keptEntries, rawEntry)
				continue
			}
			handlers, ok := rawHandlers.([]any)
			if !ok {
				return false, fmt.Errorf(
					"agent hook config %s event %q entry hooks must be an array", path, event,
				)
			}
			keptHandlers := make([]any, 0, len(handlers))
			for _, rawHandler := range handlers {
				handler, ok := rawHandler.(map[string]any)
				command, _ := handler["command"].(string)
				if ok && isLegacyHookCommand(agent, command) {
					changed = true
					continue
				}
				keptHandlers = append(keptHandlers, rawHandler)
			}
			if len(keptHandlers) == 0 {
				continue
			}
			entry["hooks"] = keptHandlers
			keptEntries = append(keptEntries, entry)
		}
		if len(keptEntries) == 0 {
			delete(hooks, event)
		} else {
			hooks[event] = keptEntries
		}
	}
	return changed, nil
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
	case AgentGrok:
		return strings.EqualFold(selectedAgent, string(AgentGrok))
	default:
		return false
	}
}
