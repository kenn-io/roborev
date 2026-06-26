package droidhook

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.kenn.io/roborev/internal/agenthook"
)

// DroidRunner is the roborev subcommand suffix baked into Factory Droid hook
// commands. It is also the stale-command sentinel: a Droid install replaces any
// prior command hook that invokes this runner, regardless of the roborev
// binary path baked in by an earlier install. It is deliberately disjoint from
// agenthook's "agent-hook run" so a Droid install never clobbers a Codex/Claude
// entry and vice versa.
const DroidRunner = "droid-hook run"

// ExecuteMatcher is the Factory Droid tool name for shell commands. PreToolUse
// and PostToolUse hooks match it to track turns and commits, mirroring the
// Codex/Claude "Bash" matcher.
const ExecuteMatcher = "Execute"

type InstallOptions struct {
	Command    string
	ConfigPath string
	Scope      string
	Timeout    time.Duration
	DryRun     bool
}

type DumpOptions struct {
	Command    string
	ConfigPath string
	Scope      string
	Timeout    time.Duration
}

// DroidSpecs returns the hook entries roborev installs for Factory Droid:
// PreToolUse and PostToolUse on the Execute tool (turn and commit tracking) and
// a matcher-less Stop (failed-review blocking). All three carry the same
// timeout and mirror the Codex/Claude agent-hook install so behavior is on par.
func DroidSpecs(command string, timeout time.Duration) []agenthook.InstallSpec {
	secs := int(timeout.Seconds())
	return []agenthook.InstallSpec{
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

// RunInstall writes roborev's Factory Droid hook entries into the hooks.json at
// the resolved scope, collapsing any prior roborev droid-hook command into a
// single up-to-date entry. A non-empty opts.Command is used verbatim; an empty
// one falls back to roborev binary auto-resolution with the droid-hook runner
// suffix. It does not start the daemon; the run command starts it on demand.
func RunInstall(opts InstallOptions, stdout io.Writer) error {
	scope, err := normalizeScope(opts.Scope)
	if err != nil {
		return err
	}
	if opts.Timeout < 0 {
		return fmt.Errorf("timeout must be >= 0")
	}
	command, _, err := resolveDroidCommand(opts.Command)
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
	changed, err := agenthook.InstallSpecs(path, DroidSpecs(command, opts.Timeout), DroidRunner, opts.DryRun)
	if err != nil {
		return err
	}
	printInstallResult(stdout, scope, path, changed, opts.DryRun)
	return nil
}

// RunDump prints the Factory Droid hook config that RunInstall would write to
// the resolved scope, merged into any existing hooks.json there, as JSON on
// stdout. It never writes files.
func RunDump(opts DumpOptions, stdout io.Writer) error {
	scope, err := normalizeScope(opts.Scope)
	if err != nil {
		return err
	}
	if opts.Timeout < 0 {
		return fmt.Errorf("timeout must be >= 0")
	}
	command, _, err := resolveDroidCommand(opts.Command)
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
	root, _, _, err := agenthook.PlanSpecs(path, DroidSpecs(command, opts.Timeout), DroidRunner)
	if err != nil {
		return err
	}
	body, err := agenthook.MarshalJSONConfig(root)
	if err != nil {
		return fmt.Errorf("encode %s: %w", path, err)
	}
	_, err = stdout.Write(body)
	return err
}

// resolveDroidCommand resolves the hook command for Factory Droid. A non-empty
// command is used verbatim; an empty command falls back to the roborev binary
// auto-resolution with the droid-hook runner suffix.
func resolveDroidCommand(command string) (string, string, error) {
	return agenthook.ResolveHookCommandWithRunner(command, "", DroidRunner)
}

func normalizeScope(scope string) (string, error) {
	scope = strings.ToLower(strings.TrimSpace(scope))
	if scope == "" {
		return "user", nil
	}
	if scope == "user" || scope == "project" {
		return scope, nil
	}
	return "", fmt.Errorf("scope must be user or project")
}

// DefaultDroidHooksPath returns the Factory Droid hooks.json path for a scope:
// "~/.factory/hooks.json" for user scope, ".factory/hooks.json"
// (project-relative) for project scope.
func DefaultDroidHooksPath(scope string) string {
	if strings.ToLower(scope) == "project" {
		return ".factory/hooks.json"
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".factory", "hooks.json")
}

func printInstallResult(stdout io.Writer, scope, path string, changed, dryRun bool) {
	switch {
	case dryRun && changed:
		fmt.Fprintf(stdout, "would update Factory Droid hooks (%s) in %s\n", scope, path)
	case dryRun:
		fmt.Fprintf(stdout, "Factory Droid hooks (%s) already installed in %s\n", scope, path)
	case changed:
		fmt.Fprintf(stdout, "installed Factory Droid hooks (%s) in %s\n", scope, path)
	default:
		fmt.Fprintf(stdout, "Factory Droid hooks (%s) already installed in %s\n", scope, path)
	}
}
