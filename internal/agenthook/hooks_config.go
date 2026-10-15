package agenthook

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	kitagenthook "go.kenn.io/kit/agenthook"
)

// Hooks in harnesses without a kit profile are stored as JSON documents that
// follow the Claude Code nested-hooks shape. These helpers read, mutate, and
// re-encode such configs while preserving unrelated keys.

func readHooksConfig(path, display string) (map[string]any, error) {
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s hook config %s: %w", display, path, err)
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return map[string]any{}, nil
	}
	decoder := jsontext.NewDecoder(bytes.NewReader(body))
	var root map[string]any
	if err := json.UnmarshalDecode(decoder, &root, json.WithUnmarshalers(json.UnmarshalFromFunc(func(dec *jsontext.Decoder, value *any) error {
		if dec.PeekKind() != '0' {
			return errors.ErrUnsupported
		}
		raw, err := dec.ReadValue()
		*value = raw.Clone()
		return err
	}))); err != nil {
		return nil, fmt.Errorf("decode %s hook config %s: %w", display, path, err)
	}
	var trailing any
	if err := json.UnmarshalDecode(decoder, &trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		return nil, fmt.Errorf("decode %s hook config %s: %w", display, path, err)
	}
	if root == nil {
		root = map[string]any{}
	}
	return root, nil
}

func marshalHooksConfig(root map[string]any) ([]byte, error) {
	body, err := json.Marshal(root, jsontext.WithIndent("  "), json.Deterministic(true))
	if err != nil {
		return nil, err
	}
	return append(body, '\n'), nil
}

func hooksObject(root map[string]any, path, display string) (map[string]any, error) {
	raw, ok := root["hooks"]
	if !ok || raw == nil {
		hooks := map[string]any{}
		root["hooks"] = hooks
		return hooks, nil
	}
	hooks, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("invalid %s hook config %s: field %q must be an object", display, path, "hooks")
	}
	return hooks, nil
}

func objectField(parent map[string]any, field, path, display string) (map[string]any, error) {
	raw, ok := parent[field]
	if !ok || raw == nil {
		object := map[string]any{}
		parent[field] = object
		return object, nil
	}
	object, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("invalid %s hook config %s: field %q must be an object", display, path, field)
	}
	return object, nil
}

func hookEventEntries(events map[string]any, event, path, display string) ([]any, error) {
	raw, ok := events[event]
	if !ok || raw == nil {
		return nil, nil
	}
	entries, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("invalid %s hook config %s: event %q must be an array", display, path, event)
	}
	return entries, nil
}

func removeOwnedHooks(events map[string]any, path, display string) error {
	for event, rawEntries := range events {
		entries, ok := rawEntries.([]any)
		if !ok {
			return fmt.Errorf("invalid %s hook config %s: event %q must be an array", display, path, event)
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
				return fmt.Errorf("invalid %s hook config %s: event %q entry hooks must be an array", display, path, event)
			}
			keptHandlers := make([]any, 0, len(handlers))
			for _, rawHandler := range handlers {
				handler, _ := rawHandler.(map[string]any)
				command, _ := handler["command"].(string)
				if strings.Contains(command, agentHookMarker) {
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
			delete(events, event)
		} else {
			events[event] = keptEntries
		}
	}
	return nil
}

func hookRunCommand(agent kitagenthook.Agent, opts InstallOptions) (string, error) {
	if command := strings.TrimSpace(opts.Command); command != "" {
		selected, err := commandAgent(command)
		if err != nil {
			return "", err
		}
		if selected != agent {
			return "", fmt.Errorf("hook command selects %s, not %s", selected, agent)
		}
		if opts.RoborevServerAddr != "" {
			args, err := kitagenthook.BuildCommand("--roborev-server", opts.RoborevServerAddr)
			if err != nil {
				return "", err
			}
			command += " " + args.Native
		}
		if opts.MCP {
			command += " --mcp"
		}
		return command + " " + agentHookMarker, nil
	}
	args := []string{"agent-hook", "run", "--agent", string(agent), agentHookMarker}
	if opts.RoborevServerAddr != "" {
		args = append(args, "--roborev-server", opts.RoborevServerAddr)
	}
	if opts.MCP {
		args = append(args, "--mcp")
	}
	commands, err := kitagenthook.BuildCommand(opts.Executable, args...)
	if err != nil {
		return "", err
	}
	return commands.Native, nil
}
