package agenthook

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.kenn.io/roborev/internal/githook"
)

type InstallOptions struct {
	Agent            string
	Command          string
	CodexConfigPath  string
	ClaudeConfigPath string
	Timeout          time.Duration
	DryRun           bool
}

type DumpOptions struct {
	Agent      string
	Command    string
	ConfigPath string
	Timeout    time.Duration
}

type installSpec struct {
	Event          string
	Matcher        string
	Command        string
	Timeout        int
	IncludeTimeout bool
}

func RunInstall(opts InstallOptions, stdout io.Writer) error {
	agent := strings.ToLower(strings.TrimSpace(opts.Agent))
	if agent == "" {
		agent = "all"
	}
	if agent != "all" && agent != "codex" && agent != "claude" {
		return fmt.Errorf("agent must be codex, claude, or all")
	}
	if opts.Timeout < 0 {
		return fmt.Errorf("timeout must be >= 0")
	}
	command, err := resolveInstallCommand(opts.Command)
	if err != nil {
		return err
	}

	if agent == "all" || agent == "codex" {
		changed, err := installSpecs(opts.CodexConfigPath, codexSpecs(command, opts.Timeout), opts.DryRun)
		if err != nil {
			return err
		}
		printInstallResult(stdout, "Codex", opts.CodexConfigPath, changed, opts.DryRun)
	}
	if agent == "all" || agent == "claude" {
		changed, err := installSpecs(opts.ClaudeConfigPath, claudeSpecs(command), opts.DryRun)
		if err != nil {
			return err
		}
		printInstallResult(stdout, "Claude", opts.ClaudeConfigPath, changed, opts.DryRun)
	}
	return nil
}

func RunDump(opts DumpOptions, stdout io.Writer) error {
	agent := strings.ToLower(strings.TrimSpace(opts.Agent))
	if opts.Timeout < 0 {
		return fmt.Errorf("timeout must be >= 0")
	}
	command, err := resolveInstallCommand(opts.Command)
	if err != nil {
		return err
	}

	path := opts.ConfigPath
	var specs []installSpec
	switch agent {
	case "codex":
		if path == "" {
			path = DefaultCodexHooksPath()
		}
		specs = codexSpecs(command, opts.Timeout)
	case "claude":
		if path == "" {
			path = DefaultClaudeSettingsPath()
		}
		specs = claudeSpecs(command)
	default:
		return fmt.Errorf("agent must be codex or claude")
	}

	root, _, _, err := planSpecs(path, specs)
	if err != nil {
		return err
	}
	body, err := marshalJSONConfig(root)
	if err != nil {
		return fmt.Errorf("encode %s: %w", path, err)
	}
	_, err = stdout.Write(body)
	return err
}

func resolveInstallCommand(command string) (string, error) {
	command = strings.TrimSpace(command)
	if command == "" {
		return defaultInstallCommand()
	}
	return command, nil
}

func printInstallResult(stdout io.Writer, name, path string, changed, dryRun bool) {
	switch {
	case dryRun && changed:
		fmt.Fprintf(stdout, "would update %s agent hooks in %s\n", name, path)
	case dryRun:
		fmt.Fprintf(stdout, "%s agent hooks already installed in %s\n", name, path)
	case changed:
		fmt.Fprintf(stdout, "installed %s agent hooks in %s\n", name, path)
	default:
		fmt.Fprintf(stdout, "%s agent hooks already installed in %s\n", name, path)
	}
}

func codexSpecs(command string, timeout time.Duration) []installSpec {
	secs := int(timeout.Seconds())
	return []installSpec{
		{
			Event:          "PreToolUse",
			Matcher:        "^Bash$",
			Command:        command,
			Timeout:        secs,
			IncludeTimeout: true,
		},
		{
			Event:          "PostToolUse",
			Matcher:        "^Bash$",
			Command:        command,
			Timeout:        secs,
			IncludeTimeout: true,
		},
		{
			Event:          "Stop",
			Command:        command,
			Timeout:        secs,
			IncludeTimeout: true,
		},
	}
}

func claudeSpecs(command string) []installSpec {
	return []installSpec{
		{
			Event:   "PreToolUse",
			Matcher: "Bash",
			Command: command,
		},
		{
			Event:   "PostToolUse",
			Matcher: "Bash",
			Command: command,
		},
		{
			Event:   "Stop",
			Command: command,
		},
	}
}

func installSpecs(path string, specs []installSpec, dryRun bool) (bool, error) {
	root, mode, changed, err := planSpecs(path, specs)
	if err != nil {
		return false, err
	}
	if !changed || dryRun {
		return changed, nil
	}
	if err := writeJSONConfig(path, root, mode); err != nil {
		return false, err
	}
	return true, nil
}

func planSpecs(path string, specs []installSpec) (map[string]any, os.FileMode, bool, error) {
	if path == "" {
		return nil, 0, false, fmt.Errorf("config path is required")
	}
	root, mode, err := readJSONConfig(path)
	if err != nil {
		return nil, 0, false, err
	}
	hooks, err := configObject(root)
	if err != nil {
		return nil, 0, false, err
	}

	changed := false
	for _, spec := range specs {
		specChanged, err := ensureSpec(hooks, spec)
		if err != nil {
			return nil, 0, false, fmt.Errorf("%s hook: %w", spec.Event, err)
		}
		changed = changed || specChanged
	}
	return root, mode, changed, nil
}

func readJSONConfig(path string) (map[string]any, os.FileMode, error) {
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]any{}, 0o600, nil
	}
	if err != nil {
		return nil, 0, fmt.Errorf("read %s: %w", path, err)
	}
	mode := os.FileMode(0o600)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return map[string]any{}, mode, nil
	}
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		return nil, 0, fmt.Errorf("decode %s: %w", path, err)
	}
	if root == nil {
		root = map[string]any{}
	}
	return root, mode, nil
}

func marshalJSONConfig(root map[string]any) ([]byte, error) {
	body, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(body, '\n'), nil
}

func writeJSONConfig(path string, root map[string]any, mode os.FileMode) error {
	body, err := marshalJSONConfig(root)
	if err != nil {
		return fmt.Errorf("encode %s: %w", path, err)
	}
	writePath := path
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		writePath = resolved
	}
	dir := filepath.Dir(writePath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(writePath)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp config: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp config: %w", err)
	}
	if err := os.Chmod(tmpPath, mode); err != nil {
		return fmt.Errorf("chmod temp config: %w", err)
	}
	if err := os.Rename(tmpPath, writePath); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return nil
}

func configObject(root map[string]any) (map[string]any, error) {
	raw, ok := root["hooks"]
	if !ok || raw == nil {
		hooks := map[string]any{}
		root["hooks"] = hooks
		return hooks, nil
	}
	hooks, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("hooks must be an object")
	}
	return hooks, nil
}

func ensureSpec(hooks map[string]any, spec installSpec) (bool, error) {
	entries, err := eventEntries(hooks, spec.Event)
	if err != nil {
		return false, err
	}
	idx, err := findEntry(entries, spec.Matcher)
	if err != nil {
		return false, err
	}

	commandHook := map[string]any{
		"type":    "command",
		"command": spec.Command,
	}
	if spec.IncludeTimeout {
		commandHook["timeout"] = spec.Timeout
	}

	if idx == -1 {
		entry := map[string]any{
			"hooks": []any{commandHook},
		}
		if spec.Matcher != "" {
			entry["matcher"] = spec.Matcher
		}
		hooks[spec.Event] = append(entries, entry)
		return true, nil
	}

	entry := entries[idx].(map[string]any)
	entryHookList, err := entryHooks(entry)
	if err != nil {
		return false, err
	}
	updated, changed := upsertCommandHook(entryHookList, commandHook, spec)
	entry["hooks"] = updated
	hooks[spec.Event] = entries
	return changed, nil
}

func eventEntries(hooks map[string]any, event string) ([]any, error) {
	raw, ok := hooks[event]
	if !ok || raw == nil {
		entries := []any{}
		hooks[event] = entries
		return entries, nil
	}
	entries, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("%s must be an array", event)
	}
	return entries, nil
}

// upsertCommandHook installs commandHook into list, collapsing any prior roborev
// agent-hook command hooks - including ones carrying a stale binary path from an
// earlier install - into this single hook rather than appending a duplicate
// beside them. Non-roborev hooks are left untouched. It reports whether the list
// changed.
func upsertCommandHook(list []any, commandHook map[string]any, spec installSpec) ([]any, bool) {
	updated := make([]any, 0, len(list)+1)
	placed := false
	changed := false
	for _, raw := range list {
		hook, ok := raw.(map[string]any)
		if !ok || !replaceableCommandHook(hook, spec) {
			updated = append(updated, raw)
			continue
		}
		if placed {
			changed = true // drop a duplicate roborev hook left by an earlier install
			continue
		}
		placed = true
		if commandHookCurrent(hook, spec) {
			updated = append(updated, hook)
			continue
		}
		updated = append(updated, commandHook)
		changed = true
	}
	if !placed {
		updated = append(updated, commandHook)
		changed = true
	}
	return updated, changed
}

// replaceableCommandHook reports whether an existing command hook should be
// replaced by the spec's command: either it already uses the exact command (an
// idempotent re-install) or it is a roborev agent-hook command that may carry a
// stale binary path from a prior install.
func replaceableCommandHook(hook map[string]any, spec installSpec) bool {
	if hook["type"] != "command" {
		return false
	}
	cmd, _ := hook["command"].(string)
	return cmd == spec.Command || isRoborevAgentHookCommand(cmd)
}

// commandHookCurrent reports whether hook already matches spec exactly, so it
// needs no rewrite.
func commandHookCurrent(hook map[string]any, spec installSpec) bool {
	if cmd, _ := hook["command"].(string); cmd != spec.Command {
		return false
	}
	if !spec.IncludeTimeout {
		return true
	}
	curr, ok := hook["timeout"].(float64)
	return ok && int(curr) == spec.Timeout
}

// isRoborevAgentHookCommand reports whether a hook command invokes roborev's
// agent-hook runner, regardless of binary path or quoting, so an install can
// replace command hooks that carry a stale or versioned roborev path.
func isRoborevAgentHookCommand(command string) bool {
	return strings.Contains(command, "agent-hook run") && strings.Contains(command, "roborev")
}

func findEntry(entries []any, matcher string) (int, error) {
	for i, rawEntry := range entries {
		entry, ok := rawEntry.(map[string]any)
		if !ok {
			return -1, fmt.Errorf("hook entry must be an object")
		}
		rawMatcher, hasMatcher := entry["matcher"]
		if matcher == "" && (!hasMatcher || rawMatcher == "") {
			return i, nil
		}
		if matcher != "" && rawMatcher == matcher {
			return i, nil
		}
	}
	return -1, nil
}

func entryHooks(entry map[string]any) ([]any, error) {
	raw, ok := entry["hooks"]
	if !ok || raw == nil {
		return []any{}, nil
	}
	hooks, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("entry hooks must be an array")
	}
	return hooks, nil
}

// ResolveHookCommand returns the command to install for agent hooks. With an
// empty override it resolves the roborev binary the way git hooks do - preferring
// a stable shim over a versioned or temporary install path - and returns any
// advisory notice from that resolution so callers can surface it. A non-empty
// override is used verbatim with no notice, letting callers pin an exact command.
func ResolveHookCommand(override string) (command, notice string, err error) {
	if override = strings.TrimSpace(override); override != "" {
		return override, "", nil
	}
	res, err := githook.ResolveRoborevPath("")
	if err != nil {
		return "", "", fmt.Errorf("resolve roborev binary: %w", err)
	}
	return shellQuote(res.Path) + " agent-hook run", agentHookNotice(res.Notice), nil
}

// agentHookNotice adapts a binary-resolution notice for agent-hook commands. The
// shared resolver phrases its stable-binary guidance for the git hooks' --binary
// flag; agent-hook install and dump expose --command instead, so the flag name is
// translated to avoid pointing users at a flag these commands do not have.
func agentHookNotice(notice string) string {
	return strings.ReplaceAll(notice, "--binary", "--command")
}

func defaultInstallCommand() (string, error) {
	command, _, err := ResolveHookCommand("")
	return command, err
}

func DefaultCodexHooksPath() string {
	if dir := os.Getenv("CODEX_HOME"); dir != "" {
		return filepath.Join(dir, "hooks.json")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".codex", "hooks.json")
}

func DefaultClaudeSettingsPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".claude", "settings.json")
}

func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if strings.IndexFunc(s, unsafeShellRune) == -1 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

func unsafeShellRune(r rune) bool {
	return r != '/' && r != '.' && r != '-' && r != '_' && r != '+' && r != ':' &&
		(r < '0' || r > '9') &&
		(r < 'A' || r > 'Z') &&
		(r < 'a' || r > 'z')
}
