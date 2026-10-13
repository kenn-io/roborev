package agenthook

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	kitagenthook "go.kenn.io/kit/agenthook"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/version"
)

// ZcodeShellMatcher matches ZCode's shell tool events. ZCode matches tool
// events against the Bash tool name exactly like Claude Code.
const ZcodeShellMatcher = "Bash"

// zcodeMinTimeout floors the hook timeout for ZCode registrations. A real
// hook run measured 15 seconds end to end (process start, daemon discovery,
// git scope resolution, review lookup) before the 10-second default killed
// it, so shorter timeouts would disable the integration in practice.
const zcodeMinTimeout = 30 * time.Second

// DefaultZcodeConfigPath returns ZCode's user-scope configuration file, the
// shared ~/.zcode/cli/config.json that also holds MCP servers and plugin
// state.
func DefaultZcodeConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".zcode", "cli", "config.json")
}

// DefaultZcodePluginRoot returns the roborev ZCode plugin directory. The
// plugin is registered by appending this directory to plugins.dirs in
// ZCode's configuration.
func DefaultZcodePluginRoot() string {
	return filepath.Join(config.DataDir(), "zcode-plugin")
}

// planZcodeInstall registers roborev as an inline ZCode plugin. ZCode does
// not run hooks configured in configuration files on the versions tested
// (the client loads hooks only through its UI-driven hooks service), while
// plugin-contributed hooks load in the CLI process itself and enable the
// hook runner automatically, so hooks are delivered as a plugin directory:
// a manifest at .zcode-plugin/plugin.json plus hook registrations in
// hooks/hooks.json, with the plugin root appended to plugins.dirs.
func planZcodeInstall(opts InstallOptions) (kitagenthook.Result, error) {
	path := strings.TrimSpace(opts.ConfigPath)
	if path == "" {
		path = DefaultZcodeConfigPath()
	}
	if path == "" {
		return kitagenthook.Result{}, errors.New("could not resolve ZCode config path")
	}
	plan, err := planZcodePlugin(opts)
	if err != nil {
		return kitagenthook.Result{}, err
	}
	root, err := readHooksConfig(path, "ZCode")
	if err != nil {
		return kitagenthook.Result{}, err
	}
	before, err := marshalHooksConfig(root)
	if err != nil {
		return kitagenthook.Result{}, fmt.Errorf("encode existing ZCode config %s: %w", path, err)
	}
	if err := registerZcodePluginDir(root, plan.root, path); err != nil {
		return kitagenthook.Result{}, err
	}
	if err := removeLegacyZcodeHooks(root, path); err != nil {
		return kitagenthook.Result{}, err
	}
	after, err := marshalHooksConfig(root)
	if err != nil {
		return kitagenthook.Result{}, fmt.Errorf("encode ZCode config %s: %w", path, err)
	}
	changed := !bytes.Equal(before, after) || zcodePluginFilesStale(plan)
	return kitagenthook.Result{
		Agent: AgentZcode, ConfigPath: path, Changed: changed, Data: after,
	}, nil
}

type zcodePlugin struct {
	root     string
	manifest []byte
	hooks    []byte
}

func planZcodePlugin(opts InstallOptions) (zcodePlugin, error) {
	command, err := hookRunCommand(AgentZcode, opts)
	if err != nil {
		return zcodePlugin{}, err
	}
	timeout := max(opts.Timeout, zcodeMinTimeout)
	events := map[string]any{}
	if err := appendHookEntries(events, "", "ZCode", command, timeout, zcodeHookSpecs(opts.StopForm)); err != nil {
		return zcodePlugin{}, err
	}
	hooks, err := marshalHooksConfig(map[string]any{"hooks": events})
	if err != nil {
		return zcodePlugin{}, err
	}
	manifest, err := json.Marshal(map[string]any{
		"name":        "roborev",
		"version":     version.Version,
		"description": "roborev review reminders for ZCode sessions",
		"hooks":       "hooks",
	}, jsontext.WithIndent("  "), json.Deterministic(true))
	if err != nil {
		return zcodePlugin{}, err
	}
	manifest = append(manifest, '\n')
	return zcodePlugin{root: DefaultZcodePluginRoot(), manifest: manifest, hooks: hooks}, nil
}

// zcodeHookSpecs selects the registered events. ZCode schedules only
// SessionStart and UserPromptSubmit on the versions tested (a full day of
// turn-phase logs plus a post-restart Stop verification with a loaded hook
// showed zero Stop, PreToolUse, and PostToolUse dispatches), so the default
// delivers reminders through those two as injected additionalContext. The
// Stop registration is kept installed anyway: it is inert until ZCode
// dispatches the event, then works immediately with the shared thresholds
// and acknowledgment state, so users need not reinstall when that lands.
// StopForm adds PreToolUse/PostToolUse for harnesses that dispatch them.
func zcodeHookSpecs(stopForm bool) []hookSpec {
	if stopForm {
		return []hookSpec{
			{event: "PreToolUse", matcher: ZcodeShellMatcher},
			{event: "PostToolUse", matcher: ZcodeShellMatcher},
			{event: "Stop"},
		}
	}
	return []hookSpec{
		{event: "UserPromptSubmit"},
		{event: "SessionStart"},
		{event: "Stop"},
	}
}

// installZcodePluginFiles writes the plugin manifest and hook registrations.
// It runs before the configuration is updated so a registered plugin always
// has its files on disk.
func installZcodePluginFiles(opts InstallOptions) error {
	plan, err := planZcodePlugin(opts)
	if err != nil {
		return err
	}
	manifestDir := filepath.Join(plan.root, ".zcode-plugin")
	if err := os.MkdirAll(manifestDir, 0o755); err != nil {
		return fmt.Errorf("create ZCode plugin manifest dir: %w", err)
	}
	if err := os.WriteFile(filepath.Join(manifestDir, "plugin.json"), plan.manifest, 0o644); err != nil {
		return fmt.Errorf("write ZCode plugin manifest: %w", err)
	}
	hooksDir := filepath.Join(plan.root, "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		return fmt.Errorf("create ZCode plugin hooks dir: %w", err)
	}
	if err := os.WriteFile(filepath.Join(hooksDir, "hooks.json"), plan.hooks, 0o644); err != nil {
		return fmt.Errorf("write ZCode plugin hooks: %w", err)
	}
	return nil
}

// zcodePluginFilesStale reports whether the plugin files on disk differ from
// the planned payload.
func zcodePluginFilesStale(plan zcodePlugin) bool {
	manifest, err := os.ReadFile(filepath.Join(plan.root, ".zcode-plugin", "plugin.json"))
	if err != nil || !bytes.Equal(manifest, plan.manifest) {
		return true
	}
	hooks, err := os.ReadFile(filepath.Join(plan.root, "hooks", "hooks.json"))
	return err != nil || !bytes.Equal(hooks, plan.hooks)
}

func registerZcodePluginDir(root map[string]any, pluginRoot, path string) error {
	plugins, err := objectField(root, "plugins", path, "ZCode")
	if err != nil {
		return err
	}
	rawDirs, ok := plugins["dirs"]
	if !ok || rawDirs == nil {
		plugins["dirs"] = []any{pluginRoot}
		return nil
	}
	dirs, ok := rawDirs.([]any)
	if !ok {
		return fmt.Errorf("invalid ZCode config %s: field %q must be an array", path, "plugins.dirs")
	}
	for _, dir := range dirs {
		if text, ok := dir.(string); ok && text == pluginRoot {
			return nil
		}
	}
	plugins["dirs"] = append(dirs, pluginRoot)
	return nil
}

// removeLegacyZcodeHooks removes roborev-owned registrations from
// hooks.events, the configuration-hook form that ZCode does not load on the
// versions tested. Unrelated hooks are preserved.
func removeLegacyZcodeHooks(root map[string]any, path string) error {
	rawHooks, ok := root["hooks"]
	if !ok || rawHooks == nil {
		return nil
	}
	hooks, ok := rawHooks.(map[string]any)
	if !ok {
		return fmt.Errorf("invalid ZCode config %s: field %q must be an object", path, "hooks")
	}
	rawEvents, ok := hooks["events"]
	if !ok || rawEvents == nil {
		return nil
	}
	events, ok := rawEvents.(map[string]any)
	if !ok {
		return fmt.Errorf("invalid ZCode config %s: field %q must be an object", path, "hooks.events")
	}
	if err := removeOwnedHooks(events, path, "ZCode"); err != nil {
		return err
	}
	if len(events) == 0 {
		delete(hooks, "events")
	}
	if len(hooks) == 0 {
		delete(root, "hooks")
	}
	return nil
}
