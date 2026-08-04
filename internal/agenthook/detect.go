package agenthook

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
)

// Installed reports whether the agent harness config at path contains a
// roborev agent-hook command. A missing file is not an error (returns false).
// Read-only: it never modifies the config.
func Installed(path string) (bool, error) {
	return installedWithRunner(path, agentHookRunner)
}

// InstalledForAgent reports whether path contains a roborev hook configured
// for agent. Agent-specific runners must be matched explicitly so a Grok or
// Droid hook is not mistaken for a plain Codex/Claude hook (or vice versa).
func InstalledForAgent(path, agent string) (bool, error) {
	var runner string
	switch agent {
	case "codex", "claude", "claude-code":
		runner = agentHookRunner
	case "droid":
		runner = droidAgentHookRunner
	case "grok":
		runner = grokAgentHookRunner
	default:
		return false, fmt.Errorf("unsupported agent %q", agent)
	}
	return installedWithRunner(path, runner)
}

func installedWithRunner(path, runner string) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	var root any
	if err := json.Unmarshal(data, &root); err != nil {
		return false, err
	}
	return jsonContainsRoborevHook(root, runner), nil
}

// jsonContainsRoborevHook walks an arbitrary decoded JSON value looking for a
// string that is a roborev agent-hook command. This is schema-agnostic, so it
// works for both Claude (settings.json) and Codex (hooks.json) shapes.
func jsonContainsRoborevHook(v any, runner string) bool {
	switch t := v.(type) {
	case string:
		return isRoborevHookCommand(t, runner)
	case []any:
		if slices.ContainsFunc(t, func(value any) bool {
			return jsonContainsRoborevHook(value, runner)
		}) {
			return true
		}
	case map[string]any:
		for _, e := range t {
			if jsonContainsRoborevHook(e, runner) {
				return true
			}
		}
	}
	return false
}
