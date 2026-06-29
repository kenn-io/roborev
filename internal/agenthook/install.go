package agenthook

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	gitpkg "go.kenn.io/roborev/internal/git"
	"go.kenn.io/roborev/internal/githook"
)

// agentHookRunner is the roborev subcommand suffix baked into Codex/Claude hook
// commands. It also serves as the stale-command sentinel: an install replaces
// any prior command hook that invokes this runner, regardless of the roborev
// binary path baked in by an earlier install.
const agentHookRunner = "agent-hook run"

// droidAgentHookRunner selects the Factory Droid profile through the shared
// agent-hook runtime. It is deliberately more specific than agentHookRunner so a
// Droid install never clobbers plain Codex/Claude hook entries and vice versa.
const droidAgentHookRunner = "agent-hook run --agent droid"

// ExecuteMatcher is the Factory Droid tool name for shell commands. PreToolUse
// and PostToolUse hooks match it to track turns and commits, mirroring the
// Codex/Claude Bash matcher.
const ExecuteMatcher = "Execute"

var droidPathCaseInsensitive = runtime.GOOS == "windows"

type InstallOptions struct {
	Agent            string
	Command          string
	ConfigPath       string
	CodexConfigPath  string
	ClaudeConfigPath string
	Scope            string
	Timeout          time.Duration
	DryRun           bool
}

type DumpOptions struct {
	Agent      string
	Command    string
	ConfigPath string
	Scope      string
	Timeout    time.Duration
}

// InstallSpec describes a single hook entry to install: the harness event, an
// optional tool-name matcher, the command to run, and an optional timeout. It is
// shared by every integration that reuses this package's JSON hook format
// (Codex, Claude, and Factory Droid all use the same shape).
type InstallSpec struct {
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
	if agent != "all" && agent != "codex" && agent != "claude" && agent != "droid" {
		return fmt.Errorf("agent must be codex, claude, droid, or all")
	}
	if opts.Timeout < 0 {
		return fmt.Errorf("timeout must be >= 0")
	}
	command, err := resolveInstallCommand(agent, opts.Command)
	if err != nil {
		return err
	}
	if agent == "all" && opts.ConfigPath != "" {
		return fmt.Errorf("--config is only supported when installing a single agent")
	}

	if agent == "all" || agent == "codex" {
		path := opts.CodexConfigPath
		if agent == "codex" && opts.ConfigPath != "" {
			path = opts.ConfigPath
		}
		changed, err := InstallSpecs(path, codexSpecs(command, opts.Timeout), agentHookRunner, opts.DryRun)
		if err != nil {
			return err
		}
		printInstallResult(stdout, "Codex", path, changed, opts.DryRun)
	}
	if agent == "all" || agent == "claude" {
		path := opts.ClaudeConfigPath
		if agent == "claude" && opts.ConfigPath != "" {
			path = opts.ConfigPath
		}
		changed, err := InstallSpecs(path, claudeSpecs(command), agentHookRunner, opts.DryRun)
		if err != nil {
			return err
		}
		printInstallResult(stdout, "Claude", path, changed, opts.DryRun)
	}
	if agent == "droid" {
		scope, err := normalizeDroidScope(opts.Scope)
		if err != nil {
			return err
		}
		path := opts.ConfigPath
		if path == "" {
			path = DefaultDroidHooksPath(scope)
		}
		if path == "" {
			return fmt.Errorf("could not resolve Factory Droid hooks path for scope %q", scope)
		}
		if err := validateDroidHooksPath(path); err != nil {
			return err
		}
		changed, err := InstallSpecs(path, droidSpecs(command, opts.Timeout), droidAgentHookRunner, opts.DryRun)
		if err != nil {
			return err
		}
		printInstallResult(stdout, "Factory Droid", path, changed, opts.DryRun)
	}
	return nil
}

func RunDump(opts DumpOptions, stdout io.Writer) error {
	agent := strings.ToLower(strings.TrimSpace(opts.Agent))
	if opts.Timeout < 0 {
		return fmt.Errorf("timeout must be >= 0")
	}
	command, err := resolveInstallCommand(agent, opts.Command)
	if err != nil {
		return err
	}

	path := opts.ConfigPath
	var specs []InstallSpec
	runner := agentHookRunner
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
	case "droid":
		scope, err := normalizeDroidScope(opts.Scope)
		if err != nil {
			return err
		}
		if path == "" {
			path = DefaultDroidHooksPath(scope)
		}
		if path == "" {
			return fmt.Errorf("could not resolve Factory Droid hooks path for scope %q", scope)
		}
		if err := validateDroidHooksPath(path); err != nil {
			return err
		}
		specs = droidSpecs(command, opts.Timeout)
		runner = droidAgentHookRunner
	default:
		return fmt.Errorf("agent must be codex, claude, or droid")
	}

	root, _, _, err := PlanSpecs(path, specs, runner)
	if err != nil {
		return err
	}
	body, err := MarshalJSONConfig(root)
	if err != nil {
		return fmt.Errorf("encode %s: %w", path, err)
	}
	_, err = stdout.Write(body)
	return err
}

func resolveInstallCommand(agent, command string) (string, error) {
	command = strings.TrimSpace(command)
	if command == "" {
		if agent == "droid" {
			command, _, err := ResolveHookCommandWithRunner("", "", droidAgentHookRunner)
			return command, err
		}
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

func codexSpecs(command string, timeout time.Duration) []InstallSpec {
	secs := int(timeout.Seconds())
	return []InstallSpec{
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

func claudeSpecs(command string) []InstallSpec {
	return []InstallSpec{
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

func droidSpecs(command string, timeout time.Duration) []InstallSpec {
	secs := int(timeout.Seconds())
	return []InstallSpec{
		{
			Event:          "PreToolUse",
			Matcher:        ExecuteMatcher,
			Command:        command,
			Timeout:        secs,
			IncludeTimeout: true,
		},
		{
			Event:          "PostToolUse",
			Matcher:        ExecuteMatcher,
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

// InstallSpecs writes specs into the hook config at path, collapsing any prior
// roborev command hooks for runner into a single up-to-date entry. runner is the
// subcommand suffix (e.g. "agent-hook run") used to identify stale roborev
// commands from earlier installs. It reports whether the config
// changed. When dryRun is set, it computes the change without writing.
func InstallSpecs(path string, specs []InstallSpec, runner string, dryRun bool) (bool, error) {
	root, mode, changed, err := PlanSpecs(path, specs, runner)
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

// PlanSpecs reads the hook config at path and computes the merged config that
// would result from installing specs (identified by runner for stale-command
// detection) without writing. It returns the root object, its file mode, and
// whether the config would change.
func PlanSpecs(path string, specs []InstallSpec, runner string) (map[string]any, os.FileMode, bool, error) {
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
		specChanged, err := ensureSpec(hooks, spec, runner)
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

// MarshalJSONConfig encodes a hook config root as indented JSON with a trailing
// newline, suitable for writing to a hooks.json or settings.json file.
func MarshalJSONConfig(root map[string]any) ([]byte, error) {
	body, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(body, '\n'), nil
}

func writeJSONConfig(path string, root map[string]any, mode os.FileMode) error {
	body, err := MarshalJSONConfig(root)
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

func ensureSpec(hooks map[string]any, spec InstallSpec, runner string) (bool, error) {
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
	updated, changed := upsertCommandHook(entryHookList, commandHook, spec, runner)
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
// command hooks for runner - including ones carrying a stale binary path from an
// earlier install - into this single hook rather than appending a duplicate
// beside them. Non-roborev hooks are left untouched. It reports whether the list
// changed.
func upsertCommandHook(list []any, commandHook map[string]any, spec InstallSpec, runner string) ([]any, bool) {
	updated := make([]any, 0, len(list)+1)
	placed := false
	changed := false
	for _, raw := range list {
		hook, ok := raw.(map[string]any)
		if !ok || !replaceableCommandHook(hook, spec, runner) {
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
// idempotent re-install) or it is a roborev command for runner that may carry a
// stale binary path from a prior install.
func replaceableCommandHook(hook map[string]any, spec InstallSpec, runner string) bool {
	if hook["type"] != "command" {
		return false
	}
	cmd, _ := hook["command"].(string)
	return cmd == spec.Command || isRoborevHookCommand(cmd, runner)
}

// commandHookCurrent reports whether hook already matches spec exactly, so it
// needs no rewrite.
func commandHookCurrent(hook map[string]any, spec InstallSpec) bool {
	if cmd, _ := hook["command"].(string); cmd != spec.Command {
		return false
	}
	if !spec.IncludeTimeout {
		return true
	}
	curr, ok := hook["timeout"].(float64)
	return ok && int(curr) == spec.Timeout
}

// isRoborevHookCommand reports whether a hook command invokes the roborev
// runner (e.g. "agent-hook run"), regardless of binary path or quoting, so an
// install can replace command hooks that carry a stale or versioned roborev
// path. runner is the subcommand suffix to match.
func isRoborevHookCommand(command, runner string) bool {
	if !strings.Contains(command, "roborev") {
		return false
	}

	baseRunner := runner
	if runner == droidAgentHookRunner {
		baseRunner = agentHookRunner
	}

	suffix, ok := hookCommandRunnerSuffix(command, baseRunner)
	if !ok {
		return false
	}

	switch runner {
	case agentHookRunner:
		return !selectsDroidAgent(suffix)
	case droidAgentHookRunner:
		return selectsDroidAgent(suffix)
	default:
		return true
	}
}

func hookCommandRunnerSuffix(command, runner string) (string, bool) {
	idx := strings.Index(command, runner)
	if idx == -1 {
		return "", false
	}
	after := idx + len(runner)
	if after == len(command) {
		return "", true
	}
	next := command[after : after+1]
	if strings.ContainsAny(next, "\"'") {
		return "", true
	}
	if !strings.ContainsAny(next, " \t\r\n") {
		return "", false
	}
	return strings.TrimSpace(command[after:]), true
}

func selectsDroidAgent(suffix string) bool {
	fields := shellFields(suffix)
	for i, field := range fields {
		field = cleanShellToken(field)
		if field == "--agent=droid" {
			return true
		}
		if field == "--agent" && i+1 < len(fields) && cleanShellToken(fields[i+1]) == "droid" {
			return true
		}
	}
	return false
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
// The notice is translated for command-only flows (dump), which expose
// --command rather than --binary.
func ResolveHookCommand(override string) (command, notice string, err error) {
	command, notice, err = ResolveHookCommandWithRunner(override, "", agentHookRunner)
	if err != nil {
		return "", "", err
	}
	return command, TranslateBinaryNotice(notice), nil
}

// ResolveHookCommandWithBinary returns the command to install for agent hooks.
// commandOverride is used verbatim when set. binaryOverride is resolved and
// quoted before appending the agent-hook runner subcommand. The returned notice
// is raw (mentions --binary), since the install flow exposes --binary.
func ResolveHookCommandWithBinary(commandOverride, binaryOverride string) (command, notice string, err error) {
	return ResolveHookCommandWithRunner(commandOverride, binaryOverride, agentHookRunner)
}

// ResolveHookCommandWithRunner returns the command to install for a roborev hook
// integration identified by runner (e.g. "agent-hook run").
// commandOverride is used verbatim when set; otherwise binaryOverride (or an
// auto-resolved roborev binary) is quoted and suffixed with runner. The returned
// notice is raw (mentions --binary); callers that expose --command instead
// should pass it through TranslateBinaryNotice. commandOverride and
// binaryOverride are mutually exclusive.
func ResolveHookCommandWithRunner(commandOverride, binaryOverride, runner string) (command, notice string, err error) {
	commandOverride = strings.TrimSpace(commandOverride)
	binaryOverride = strings.TrimSpace(binaryOverride)
	if commandOverride != "" && binaryOverride != "" {
		return "", "", fmt.Errorf("--command and --binary cannot be used together")
	}
	if commandOverride != "" {
		return commandOverride, "", nil
	}
	return resolveHookCommandFromBinary(binaryOverride, runner)
}

func resolveHookCommandFromBinary(binaryOverride, runner string) (command, notice string, err error) {
	res, err := githook.ResolveRoborevPath(binaryOverride)
	if err != nil {
		return "", "", fmt.Errorf("resolve roborev binary: %w", err)
	}
	return shellQuote(res.Path) + " " + runner, res.Notice, nil
}

// TranslateBinaryNotice adapts a binary-resolution notice for command-only hook
// flows. The shared resolver phrases its stable-binary guidance for --binary;
// dump exposes --command instead, so the flag name is translated to avoid
// pointing users at a flag that command does not have.
func TranslateBinaryNotice(notice string) string {
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

// DefaultDroidHooksPath returns the user-scoped Factory Droid hooks.json path.
// Unsupported scopes return an empty path. HOME is checked first so tests and
// POSIX-style environments work on Windows, where os.UserHomeDir returns
// USERPROFILE instead.
func DefaultDroidHooksPath(scope string) string {
	scope = strings.ToLower(strings.TrimSpace(scope))
	if scope != "" && scope != "user" {
		return ""
	}
	home := os.Getenv("HOME")
	if home == "" {
		var err error
		home, err = os.UserHomeDir()
		if err != nil || home == "" {
			return ""
		}
	}
	return filepath.Join(home, ".factory", "hooks.json")
}

func validateDroidHooksPath(path string) error {
	if isUserDroidHooksPath(path) {
		if resolved, ok := evalExistingParentPath(path); ok && !isUserDroidHooksPath(resolved) && isProjectDroidHooksPath(resolved) {
			return fmt.Errorf("project-scoped Factory Droid hook config is not supported; use the user-scoped Factory hooks path instead")
		}
		return nil
	}
	if isProjectDroidHooksPath(path) {
		return fmt.Errorf("project-scoped Factory Droid hook config is not supported; use the user-scoped Factory hooks path instead")
	}
	if resolved, ok := evalExistingParentPath(path); ok && isProjectDroidHooksPath(resolved) {
		return fmt.Errorf("project-scoped Factory Droid hook config is not supported; use the user-scoped Factory hooks path instead")
	}
	return nil
}

func isUserDroidHooksPath(path string) bool {
	return sameCleanAbsPath(path, DefaultDroidHooksPath("user"))
}

func isProjectDroidHooksPath(path string) bool {
	path = strings.TrimSpace(path)
	if path == "" {
		return false
	}
	clean := filepath.Clean(path)
	projectRel := filepath.Join(".factory", "hooks.json")
	if sameDroidPath(clean, projectRel) {
		return true
	}
	if isTargetRepoDroidHooksPath(clean) {
		return true
	}
	if repoRoot, err := gitpkg.GetRepoRoot("."); err == nil && repoRoot != "" &&
		sameCleanAbsPath(clean, filepath.Join(repoRoot, projectRel)) {
		return true
	}
	if !filepath.IsAbs(clean) {
		return false
	}
	wd, err := os.Getwd()
	if err != nil || wd == "" {
		return false
	}
	projectAbs, err := filepath.Abs(filepath.Join(wd, projectRel))
	if err != nil {
		return false
	}
	return sameCleanAbsPath(clean, projectAbs)
}

func isTargetRepoDroidHooksPath(path string) bool {
	abs, ok := cleanAbsPath(path)
	if !ok || !sameDroidPathName(filepath.Base(abs), "hooks.json") {
		return false
	}
	factoryDir := filepath.Dir(abs)
	if !sameDroidPathName(filepath.Base(factoryDir), ".factory") {
		return false
	}
	candidateRoot := filepath.Dir(factoryDir)
	repoRoot, err := gitpkg.GetRepoRoot(candidateRoot)
	if err != nil || repoRoot == "" {
		return false
	}
	return sameCleanAbsPath(candidateRoot, repoRoot)
}

func sameCleanAbsPath(a, b string) bool {
	a = strings.TrimSpace(a)
	b = strings.TrimSpace(b)
	if a == "" || b == "" {
		return false
	}
	aAbs, okA := cleanAbsPath(a)
	bAbs, okB := cleanAbsPath(b)
	if okA && okB {
		return sameDroidPath(filepath.Clean(aAbs), filepath.Clean(bAbs))
	}
	return sameDroidPath(filepath.Clean(a), filepath.Clean(b))
}

func cleanAbsPath(path string) (string, bool) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", false
	}
	abs, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", false
	}
	return canonicalExistingPath(filepath.Clean(abs)), true
}

func canonicalExistingPath(path string) string {
	path = filepath.Clean(path)
	volume := filepath.VolumeName(path)
	rest := strings.TrimPrefix(path, volume)
	rooted := strings.HasPrefix(rest, string(filepath.Separator))
	rest = strings.TrimPrefix(rest, string(filepath.Separator))
	parts := strings.Split(rest, string(filepath.Separator))

	current := volume
	if rooted {
		current += string(filepath.Separator)
	}
	for i, part := range parts {
		if part == "" {
			continue
		}
		next := filepath.Join(current, part)
		if _, err := os.Lstat(next); err != nil {
			for _, remaining := range parts[i:] {
				if remaining != "" {
					current = filepath.Join(current, remaining)
				}
			}
			return filepath.Clean(current)
		}
		if actual := actualDirEntryName(current, part); actual != "" {
			part = actual
		}
		current = filepath.Join(current, part)
	}
	return filepath.Clean(current)
}

func actualDirEntryName(parent, name string) string {
	if parent == "" {
		parent = "."
	}
	entries, err := os.ReadDir(parent)
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		if entry.Name() == name {
			return name
		}
	}
	for _, entry := range entries {
		if strings.EqualFold(entry.Name(), name) {
			return entry.Name()
		}
	}
	return ""
}

func sameDroidPath(a, b string) bool {
	if droidPathCaseInsensitive {
		return strings.EqualFold(a, b)
	}
	return a == b
}

func sameDroidPathName(a, b string) bool {
	return strings.EqualFold(a, b)
}

// evalExistingParentPath resolves the longest existing ancestor of path and
// returns the full path with symlinks evaluated on the existing portion. This
// catches symlinked parent directories even when the final path components do
// not exist yet (e.g., hooks.json has not been created).
func evalExistingParentPath(path string) (string, bool) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", false
	}
	clean := filepath.Clean(path)
	existing := clean
	remaining := ""
	for existing != "." && existing != string(filepath.Separator) {
		if _, err := os.Lstat(existing); err == nil {
			break
		}
		remaining = filepath.Join(filepath.Base(existing), remaining)
		existing = filepath.Dir(existing)
	}
	if existing == "." || existing == string(filepath.Separator) {
		return cleanAbsPath(clean)
	}
	resolved, err := filepath.EvalSymlinks(existing)
	if err != nil {
		return cleanAbsPath(clean)
	}
	return cleanAbsPath(filepath.Join(resolved, remaining))
}

func normalizeDroidScope(scope string) (string, error) {
	scope = strings.ToLower(strings.TrimSpace(scope))
	if scope == "" {
		return "user", nil
	}
	if scope == "user" {
		return scope, nil
	}
	if scope == "project" {
		return "", fmt.Errorf("project scope is not supported for Factory Droid agent hooks; use user scope because project hooks are executable repo-local configuration")
	}
	return "", fmt.Errorf("scope must be user")
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
