package agenthook

import (
	"encoding/json/v2"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	kitagenthook "go.kenn.io/kit/agenthook"
)

func Installed(agent kitagenthook.Agent, path string) (bool, error) {
	result, err := kitagenthook.PlanUninstall(agent, path, agentHookMarker)
	if err != nil {
		return false, err
	}
	return result.Changed, nil
}

// CommandOptions reads the executable and connection selected by a hook command.
func CommandOptions(command string) (InstallOptions, error) {
	args, err := splitHookCommand(command)
	if err != nil {
		return InstallOptions{}, err
	}
	if len(args) == 0 {
		return InstallOptions{}, fmt.Errorf("empty hook command")
	}
	mcp, server, _, err := hookArgs(args[1:])
	return InstallOptions{Executable: args[0], MCP: mcp, RoborevServerAddr: server}, err
}

func hookArgs(args []string) (mcp bool, server string, extra []string, err error) {
	for i := 0; i < len(args); i++ {
		switch arg := args[i]; {
		case arg == "--mcp":
			mcp = true
		case strings.HasPrefix(arg, "--mcp="):
			mcp, err = strconv.ParseBool(strings.TrimPrefix(arg, "--mcp="))
			if err != nil {
				return
			}
		case arg == "--roborev-server" && i+1 < len(args):
			i++
			server = args[i]
		case strings.HasPrefix(arg, "--roborev-server="):
			server = strings.TrimPrefix(arg, "--roborev-server=")
		default:
			extra = append(extra, arg)
		}
	}
	return
}

func InstalledForAgent(path, agent string) (bool, error) {
	normalized := strings.ToLower(strings.TrimSpace(agent))
	if _, ok := localProfile(normalized); ok {
		if kitagenthook.Agent(normalized) == AgentZcode {
			return zcodeInstalled(path)
		}
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
		return containsOwnedHook(root, kitagenthook.Agent(normalized)), nil
	}
	profile, err := kitagenthook.ParseAgent(agent)
	if err != nil {
		return false, fmt.Errorf("unsupported agent %q", agent)
	}
	return Installed(profile, path)
}

// zcodeInstalled reports whether roborev hooks are installed for ZCode: the
// plugin form registers hooks in a hooks/hooks.json next to the plugin
// manifest, and the legacy configuration form wrote them into the shared
// config file. Either counts so a pending migration is still detected.
func zcodeInstalled(configPath string) (bool, error) {
	if body, err := os.ReadFile(filepath.Join(zcodePluginRoot(configPath), "hooks", "hooks.json")); err == nil {
		var root any
		if err := json.Unmarshal(body, &root); err == nil && containsOwnedHook(root, AgentZcode) {
			return true, nil
		}
	}
	body, err := os.ReadFile(configPath)
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
	return containsOwnedHook(root, AgentZcode), nil
}

// containsOwnedHook scans a parsed hook configuration for a roborev-owned
// hook command selecting agent, regardless of the nesting level. Local
// profiles keep hooks at harness-specific depths (ZCode nests event arrays
// under hooks.events), so a recursive search avoids duplicating each shape.
func containsOwnedHook(value any, agent kitagenthook.Agent) bool {
	return len(ownedHooks(value, agent)) > 0
}

// ownedHooks returns each hook handler whose command selects agent.
func ownedHooks(value any, agent kitagenthook.Agent) []map[string]any {
	var hooks []map[string]any
	switch typed := value.(type) {
	case []any:
		for _, child := range typed {
			hooks = append(hooks, ownedHooks(child, agent)...)
		}
	case map[string]any:
		if command, ok := typed["command"].(string); ok && isOwnedHookCommand(command, agent) {
			hooks = append(hooks, typed)
		}
		for _, key := range slices.Sorted(maps.Keys(typed)) {
			hooks = append(hooks, ownedHooks(typed[key], agent)...)
		}
	}
	return hooks
}

func isOwnedHookCommand(command string, agent kitagenthook.Agent) bool {
	selected, err := commandAgent(command)
	return err == nil && selected == agent
}

// RewriteCommand changes the selected connection in a shared hook command.
func RewriteCommand(command kitagenthook.Commands, agent kitagenthook.Agent, opts InstallOptions, executable string) (kitagenthook.Commands, error) {
	args, err := splitHookCommand(command.Native)
	if err != nil {
		return command, err
	}
	mcp, server, rest, err := hookArgs(args[1:])
	if err != nil {
		return command, err
	}
	moved := false
	generated := []string{"agent-hook", "run", "--agent", string(agent), agentHookMarker}
	if slices.Equal(rest, generated) && filepath.IsAbs(args[0]) {
		if _, err := os.Stat(args[0]); os.IsNotExist(err) {
			args[0], moved = executable, true
		}
	}
	if mcp == opts.MCP && server == opts.RoborevServerAddr && !moved {
		return command, nil
	}
	if opts.RoborevServerAddr != "" {
		rest = append(rest, "--roborev-server", opts.RoborevServerAddr)
	}
	if opts.MCP {
		rest = append(rest, "--mcp")
	}
	updated, err := kitagenthook.BuildCommand(args[0], rest...)
	if command.Windows == "" {
		updated.Windows = ""
	}
	return updated, err
}

// InstalledCommand returns the shared command and each event's settings.
func InstalledCommand(agent kitagenthook.Agent) (kitagenthook.Commands, []kitagenthook.Hook, bool, error) {
	path, err := kitagenthook.ConfigPath(agent)
	if err != nil {
		return kitagenthook.Commands{}, nil, false, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return kitagenthook.Commands{}, nil, false, err
	}
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		return kitagenthook.Commands{}, nil, false, err
	}
	events, _ := root["hooks"].(map[string]any)
	var command kitagenthook.Commands
	var specs []kitagenthook.Hook
	for _, event := range slices.Sorted(maps.Keys(events)) {
		groups, _ := events[event].([]any)
		for _, raw := range groups {
			group, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			matcher, _ := group["matcher"].(string)
			for _, hook := range ownedHooks(group["hooks"], agent) {
				windows, _ := hook["commandWindows"].(string)
				next := kitagenthook.Commands{Native: hook["command"].(string), Windows: windows}
				if len(specs) > 0 && command != next {
					return command, specs, true, nil
				}
				command = next
				timeout, _ := hook["timeout"].(float64)
				specs = append(specs, kitagenthook.Hook{Event: kitagenthook.Event(event), Matcher: matcher, Timeout: time.Duration(timeout) * time.Second})
			}
		}
	}
	return command, specs, false, nil
}
