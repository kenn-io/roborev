package agenthook

import (
	"encoding/json/jsontext"
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

	"go.kenn.io/roborev/internal/agentconfig"
)

func Installed(agent kitagenthook.Agent, path string) (bool, error) {
	result, err := kitagenthook.PlanUninstall(agent, path, agentHookMarker)
	if err != nil {
		return false, err
	}
	return result.Changed, nil
}

// InstalledOptions reads the choices in the agent's RoboRev-owned hooks.
// Command is set only when the hook has arguments setup does not generate.
func InstalledOptions(agent kitagenthook.Agent) (InstallOptions, bool, error) {
	path, err := kitagenthook.ConfigPath(agent)
	if err != nil {
		return InstallOptions{}, false, err
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return InstallOptions{}, false, nil
	}
	var root any
	if err == nil {
		err = json.Unmarshal(data, &root)
	}
	if err != nil {
		return InstallOptions{}, false, err
	}
	generated := []string{"agent-hook", "run", "--agent", string(agent), agentHookMarker}
	var result InstallOptions
	found := false
	for _, hook := range ownedHooks(root, agent) {
		command := hook["command"].(string)
		if !strings.Contains(command, agentHookMarker) {
			continue
		}
		args, _ := splitHookCommand(command) // ownedHooks has parsed it
		next := InstallOptions{Executable: args[0]}
		if timeout, ok := hook["timeout"].(float64); ok {
			next.Timeout = time.Duration(timeout) * time.Second
		}
		var extra []string
		next.MCP, next.RoborevServerAddr, _, err = hookArgs(args[1:])
		if err != nil {
			return InstallOptions{}, false, fmt.Errorf("invalid --mcp value in %s: %w", path, err)
		}
		_, _, extra, _ = hookArgs(args[min(len(args), 6):])
		custom := !slices.Equal(args[1:min(len(args), 6)], generated) || len(extra) > 0
		if custom {
			next.Command = withoutHookMarker(command)
		}
		if !found {
			result, found = next, true
		} else if result.RoborevServerAddr == "" {
			result.RoborevServerAddr = next.RoborevServerAddr
		}
	}
	return result, found, nil
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

func withoutHookMarker(command string) string {
	for _, marker := range []string{" '" + agentHookMarker + "'", " \"" + agentHookMarker + "\"", " " + agentHookMarker} {
		if before, after, found := strings.Cut(command, marker); found {
			args, _ := splitHookCommand(after)
			_, _, extra, _ := hookArgs(args)
			if len(extra) > 0 {
				quoted, err := kitagenthook.BuildCommand(extra[0], extra[1:]...)
				if err == nil {
					before += " " + quoted.POSIX
				}
			}
			args, err := splitHookCommand(before)
			if err == nil {
				filtered := slices.DeleteFunc(args, func(arg string) bool {
					return arg == "--mcp" || strings.HasPrefix(arg, "--mcp=")
				})
				if len(filtered) != len(args) {
					if rebuilt, err := kitagenthook.BuildCommand(filtered[0], filtered[1:]...); err == nil {
						return rebuilt.POSIX
					}
				}
			}
			return before
		}
	}
	return command
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

// UpdateInstalled changes the connection flags in each owned command while
// preserving the rest of the agent's hook configuration.
func UpdateInstalled(agent kitagenthook.Agent, opts InstallOptions, executable string) error {
	path, err := kitagenthook.ConfigPath(agent)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var root any
	if err := json.Unmarshal(data, &root); err != nil {
		return err
	}
	changed := false
	for _, hook := range ownedHooks(root, agent) {
		command := hook["command"].(string)
		args, err := splitHookCommand(command)
		if err != nil {
			return err
		}
		mcp, server, rest, err := hookArgs(args[1:])
		if err != nil {
			return err
		}
		moved := false
		generated := []string{"agent-hook", "run", "--agent", string(agent), agentHookMarker}
		if len(rest) == len(generated) && slices.Equal(rest, generated) && filepath.IsAbs(args[0]) {
			if _, err := os.Stat(args[0]); os.IsNotExist(err) {
				args[0], moved = executable, true
			}
		}
		if mcp == opts.MCP && server == opts.RoborevServerAddr && !moved {
			continue
		}
		if opts.RoborevServerAddr != "" {
			rest = append(rest, "--roborev-server", opts.RoborevServerAddr)
		}
		if opts.MCP {
			rest = append(rest, "--mcp")
		}
		built, err := kitagenthook.BuildCommand(args[0], rest...)
		if err != nil {
			return err
		}
		hook["command"] = built.POSIX
		if _, ok := hook["commandWindows"]; ok {
			hook["commandWindows"] = built.Windows
		}
		changed = true
	}
	// Kit supplies the standard registrations for any missing events.
	kitOpts, err := KitInstallOptions(agent, opts)
	if err != nil {
		return err
	}
	owned := ownedHooks(root, agent)
	if len(owned) > 0 {
		kitOpts.Executable, kitOpts.Arguments = "", nil
		kitOpts.Command = owned[0]["command"].(string)
		kitOpts.CommandWindows, _ = owned[0]["commandWindows"].(string)
	}
	planned, err := kitagenthook.PlanInstall(agent, kitOpts)
	if err != nil {
		return err
	}
	var desired map[string]any
	if err := json.Unmarshal(planned.Data, &desired); err != nil {
		return err
	}
	object, _ := root.(map[string]any)
	events, _ := object["hooks"].(map[string]any)
	plannedEvents, _ := desired["hooks"].(map[string]any)
	for _, spec := range kitOpts.Hooks {
		name := string(spec.Event)
		if len(ownedHooks(events[name], agent)) > 0 {
			continue
		}
		groups, _ := events[name].([]any)
		plannedGroups, _ := plannedEvents[name].([]any)
		for _, group := range plannedGroups {
			if len(ownedHooks(group, agent)) > 0 {
				groups = append(groups, group)
				changed = true
			}
		}
		events[name] = groups
	}
	if !changed {
		return nil
	}
	data, err = json.Marshal(root, jsontext.WithIndent("  "))
	if err != nil {
		return err
	}
	return agentconfig.Write(path, append(data, '\n'))
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
