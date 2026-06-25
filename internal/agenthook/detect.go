package agenthook

import (
	"encoding/json"
	"os"
	"slices"
)

// Installed reports whether the agent harness config at path contains a
// roborev agent-hook command. A missing file is not an error (returns false).
// Read-only: it never modifies the config.
func Installed(path string) (bool, error) {
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
	return jsonContainsRoborevHook(root), nil
}

// jsonContainsRoborevHook walks an arbitrary decoded JSON value looking for a
// string that is a roborev agent-hook command. This is schema-agnostic, so it
// works for both Claude (settings.json) and Codex (hooks.json) shapes.
func jsonContainsRoborevHook(v any) bool {
	switch t := v.(type) {
	case string:
		return isRoborevAgentHookCommand(t)
	case []any:
		if slices.ContainsFunc(t, jsonContainsRoborevHook) {
			return true
		}
	case map[string]any:
		for _, e := range t {
			if jsonContainsRoborevHook(e) {
				return true
			}
		}
	}
	return false
}
