package agenthook

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	kitagenthook "go.kenn.io/kit/agenthook"
)

const GrokShellMatcher = "Bash|run_terminal_command|run_terminal_cmd"

func DefaultGrokHooksPath() string {
	home := strings.TrimSpace(os.Getenv("GROK_HOME"))
	if home == "" {
		home, _ = os.UserHomeDir()
		if home != "" {
			home = filepath.Join(home, ".grok")
		}
	}
	if home == "" {
		return ""
	}
	return filepath.Join(home, "hooks", "roborev.json")
}

func runGrokInstall(opts InstallOptions) (kitagenthook.Result, error) {
	result, err := planGrokInstall(opts)
	if err != nil || opts.DryRun || !result.Changed {
		return result, err
	}
	if err := commitAgentHookConfig(result.ConfigPath, result.Data); err != nil {
		return kitagenthook.Result{}, err
	}
	return result, nil
}

func planGrokInstall(opts InstallOptions) (kitagenthook.Result, error) {
	path := strings.TrimSpace(opts.ConfigPath)
	if path == "" {
		path = DefaultGrokHooksPath()
	}
	if path == "" {
		return kitagenthook.Result{}, errors.New("could not resolve Grok Build hooks path")
	}
	command, err := grokHookCommand(opts)
	if err != nil {
		return kitagenthook.Result{}, err
	}
	root, err := readGrokConfig(path)
	if err != nil {
		return kitagenthook.Result{}, err
	}
	before, err := marshalGrokConfig(root)
	if err != nil {
		return kitagenthook.Result{}, fmt.Errorf("encode existing Grok Build hook config %s: %w", path, err)
	}
	hooks, err := grokHooksObject(root, path)
	if err != nil {
		return kitagenthook.Result{}, err
	}
	if err := removeGrokOwnedHooks(hooks, path); err != nil {
		return kitagenthook.Result{}, err
	}
	timeout := int(opts.Timeout / time.Second)
	for _, spec := range []struct {
		event   string
		matcher string
	}{
		{event: "PreToolUse", matcher: GrokShellMatcher},
		{event: "PostToolUse", matcher: GrokShellMatcher},
		{event: "Stop"},
	} {
		handler := map[string]any{"type": "command", "command": command}
		if timeout > 0 {
			handler["timeout"] = timeout
		}
		entry := map[string]any{"hooks": []any{handler}}
		if spec.matcher != "" {
			entry["matcher"] = spec.matcher
		}
		entries, err := grokEventEntries(hooks, spec.event, path)
		if err != nil {
			return kitagenthook.Result{}, err
		}
		hooks[spec.event] = append(entries, entry)
	}
	after, err := marshalGrokConfig(root)
	if err != nil {
		return kitagenthook.Result{}, fmt.Errorf("encode Grok Build hook config %s: %w", path, err)
	}
	return kitagenthook.Result{
		Agent: AgentGrok, ConfigPath: path, Changed: !bytes.Equal(before, after), Data: after,
	}, nil
}

func grokHookCommand(opts InstallOptions) (string, error) {
	if command := strings.TrimSpace(opts.Command); command != "" {
		selected, err := commandAgent(command)
		if err != nil {
			return "", err
		}
		if selected != AgentGrok {
			return "", fmt.Errorf("hook command selects %s, not %s", selected, AgentGrok)
		}
		return command + " " + agentHookMarker, nil
	}
	commands, err := kitagenthook.BuildCommand(
		opts.Executable, "agent-hook", "run", "--agent", string(AgentGrok), agentHookMarker,
	)
	if err != nil {
		return "", err
	}
	return commands.Native, nil
}

func readGrokConfig(path string) (map[string]any, error) {
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read Grok Build hook config %s: %w", path, err)
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return map[string]any{}, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var root map[string]any
	if err := decoder.Decode(&root); err != nil {
		return nil, fmt.Errorf("decode Grok Build hook config %s: %w", path, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		return nil, fmt.Errorf("decode Grok Build hook config %s: %w", path, err)
	}
	if root == nil {
		root = map[string]any{}
	}
	return root, nil
}

func marshalGrokConfig(root map[string]any) ([]byte, error) {
	body, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(body, '\n'), nil
}

func grokHooksObject(root map[string]any, path string) (map[string]any, error) {
	raw, ok := root["hooks"]
	if !ok || raw == nil {
		hooks := map[string]any{}
		root["hooks"] = hooks
		return hooks, nil
	}
	hooks, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("invalid Grok Build hook config %s: field %q must be an object", path, "hooks")
	}
	return hooks, nil
}

func grokEventEntries(hooks map[string]any, event, path string) ([]any, error) {
	raw, ok := hooks[event]
	if !ok || raw == nil {
		return nil, nil
	}
	entries, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("invalid Grok Build hook config %s: event %q must be an array", path, event)
	}
	return entries, nil
}

func removeGrokOwnedHooks(hooks map[string]any, path string) error {
	for event, rawEntries := range hooks {
		entries, ok := rawEntries.([]any)
		if !ok {
			return fmt.Errorf("invalid Grok Build hook config %s: event %q must be an array", path, event)
		}
		keptEntries := make([]any, 0, len(entries))
		for _, rawEntry := range entries {
			entry, ok := rawEntry.(map[string]any)
			rawHandlers, hasHandlers := entry["hooks"]
			if !ok || !hasHandlers || rawHandlers == nil {
				keptEntries = append(keptEntries, rawEntry)
				continue
			}
			handlers, ok := rawHandlers.([]any)
			if !ok {
				return fmt.Errorf("invalid Grok Build hook config %s: event %q entry hooks must be an array", path, event)
			}
			keptHandlers := make([]any, 0, len(handlers))
			for _, rawHandler := range handlers {
				handler, ok := rawHandler.(map[string]any)
				command, _ := handler["command"].(string)
				// Main briefly installed Grok commands before the common ownership
				// marker landed. Remove this exact direct form through v0.66 so an
				// upgrade cannot double-fire hooks. See #1012.
				if ok && (strings.Contains(command, agentHookMarker) ||
					isLegacyHookCommand(AgentGrok, command)) {
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
	return nil
}
