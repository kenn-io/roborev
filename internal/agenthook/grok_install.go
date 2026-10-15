package agenthook

import (
	"bytes"
	"errors"
	"fmt"
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

func planGrokInstall(opts InstallOptions) (kitagenthook.Result, error) {
	path := strings.TrimSpace(opts.ConfigPath)
	if path == "" {
		path = DefaultGrokHooksPath()
	}
	if path == "" {
		return kitagenthook.Result{}, errors.New("could not resolve Grok Build hooks path")
	}
	command, err := hookRunCommand(AgentGrok, opts)
	if err != nil {
		return kitagenthook.Result{}, err
	}
	root, err := readHooksConfig(path, "Grok Build")
	if err != nil {
		return kitagenthook.Result{}, err
	}
	before, err := marshalHooksConfig(root)
	if err != nil {
		return kitagenthook.Result{}, fmt.Errorf("encode existing Grok Build hook config %s: %w", path, err)
	}
	hooks, err := hooksObject(root, path, "Grok Build")
	if err != nil {
		return kitagenthook.Result{}, err
	}
	if err := removeOwnedHooks(hooks, path, "Grok Build"); err != nil {
		return kitagenthook.Result{}, err
	}
	if err := appendHookEntries(hooks, path, "Grok Build", command, opts.Timeout, []hookSpec{
		{event: "PreToolUse", matcher: GrokShellMatcher},
		{event: "PostToolUse", matcher: GrokShellMatcher},
		{event: "Stop"},
	}); err != nil {
		return kitagenthook.Result{}, err
	}
	after, err := marshalHooksConfig(root)
	if err != nil {
		return kitagenthook.Result{}, fmt.Errorf("encode Grok Build hook config %s: %w", path, err)
	}
	return kitagenthook.Result{
		Agent: AgentGrok, ConfigPath: path, Changed: !bytes.Equal(before, after), Data: after,
	}, nil
}

type hookSpec struct {
	event   string
	matcher string
}

func appendHookEntries(events map[string]any, path, display, command string, timeout time.Duration, specs []hookSpec) error {
	seconds := int(timeout / time.Second)
	for _, spec := range specs {
		handler := map[string]any{"type": "command", "command": command}
		if seconds > 0 {
			handler["timeout"] = seconds
		}
		entry := map[string]any{"hooks": []any{handler}}
		if spec.matcher != "" {
			entry["matcher"] = spec.matcher
		}
		entries, err := hookEventEntries(events, spec.event, path, display)
		if err != nil {
			return err
		}
		events[spec.event] = append(entries, entry)
	}
	return nil
}
