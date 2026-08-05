package agenthook

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode"

	kitagenthook "go.kenn.io/kit/agenthook"
)

const (
	agentHookRunner = "agent-hook run"
	agentHookMarker = "--source=roborev-agent-hook"
)

type InstallOptions struct {
	Agent      string
	Executable string
	Command    string
	ConfigPath string
	Timeout    time.Duration
	DryRun     bool
}

type DumpOptions struct {
	Agent      string
	Executable string
	Command    string
	ConfigPath string
	Timeout    time.Duration
}

func RunInstall(opts InstallOptions, stdout io.Writer) error {
	if opts.Timeout < 0 {
		return fmt.Errorf("timeout must be >= 0")
	}
	explicit := strings.TrimSpace(opts.Agent) != "" && !strings.EqualFold(strings.TrimSpace(opts.Agent), "all")
	if opts.ConfigPath != "" && !explicit {
		return fmt.Errorf("--config requires one explicit agent")
	}
	if opts.Command != "" && !explicit {
		return fmt.Errorf("--command requires one explicit agent")
	}
	agents, err := SelectProfiles(opts.Agent)
	if err != nil {
		return err
	}

	var errs []error
	for _, agent := range agents {
		result, runErr := runInstall(agent, opts)
		if runErr != nil {
			errs = append(errs, profileError(agent, opts.ConfigPath, runErr))
			continue
		}
		printInstallResult(stdout, result, opts.DryRun)
	}
	return errors.Join(errs...)
}

func RunDump(opts DumpOptions, stdout io.Writer) error {
	if opts.Timeout < 0 {
		return fmt.Errorf("timeout must be >= 0")
	}
	raw := strings.TrimSpace(opts.Agent)
	if raw == "" || strings.EqualFold(raw, "all") {
		return fmt.Errorf("dump requires one explicit agent")
	}
	agent := AgentGrok
	if !strings.EqualFold(raw, string(AgentGrok)) {
		var err error
		agent, err = kitagenthook.ParseAgent(raw)
		if err != nil {
			return err
		}
	}
	installOpts := InstallOptions{
		Agent:      raw,
		Executable: opts.Executable,
		Command:    opts.Command,
		ConfigPath: opts.ConfigPath,
		Timeout:    opts.Timeout,
		DryRun:     true,
	}
	if agent == AgentGrok {
		result, err := planGrokInstall(installOpts)
		if err != nil {
			return profileError(agent, opts.ConfigPath, err)
		}
		_, err = stdout.Write(result.Data)
		return err
	}
	kitOpts, err := validatedKitInstallOptions(agent, installOpts)
	if err != nil {
		return profileError(agent, opts.ConfigPath, err)
	}
	result, err := kitagenthook.PlanInstall(agent, kitOpts)
	if err != nil {
		return profileError(agent, opts.ConfigPath, err)
	}
	result, err = planLegacyHookMigration(agent, result)
	if err != nil {
		return profileError(agent, opts.ConfigPath, err)
	}
	_, err = stdout.Write(result.Data)
	return err
}

func runInstall(agent kitagenthook.Agent, opts InstallOptions) (kitagenthook.Result, error) {
	if agent == AgentGrok {
		return runGrokInstall(opts)
	}
	kitOpts, err := validatedKitInstallOptions(agent, opts)
	if err != nil {
		return kitagenthook.Result{}, err
	}
	planned, err := kitagenthook.PlanInstall(agent, kitOpts)
	if err != nil {
		return kitagenthook.Result{}, err
	}
	planned, err = planLegacyHookMigration(agent, planned)
	if err != nil {
		return kitagenthook.Result{}, err
	}
	if opts.DryRun {
		return planned, nil
	}
	if !planned.Changed {
		return planned, nil
	}
	if err := commitAgentHookConfig(planned.ConfigPath, planned.Data); err != nil {
		return kitagenthook.Result{}, err
	}
	return planned, nil
}

func validatedKitInstallOptions(
	agent kitagenthook.Agent,
	opts InstallOptions,
) (kitagenthook.InstallOptions, error) {
	if opts.Command != "" {
		selected, err := commandAgent(opts.Command)
		if err != nil {
			return kitagenthook.InstallOptions{}, err
		}
		if selected != agent {
			return kitagenthook.InstallOptions{}, fmt.Errorf("hook command selects %s, not %s", selected, agent)
		}
	}
	if agent == kitagenthook.AgentDroid {
		path := opts.ConfigPath
		if path == "" {
			var err error
			path, err = kitagenthook.ConfigPath(agent)
			if err != nil {
				return kitagenthook.InstallOptions{}, err
			}
		}
		if err := validateDroidHooksPath(path); err != nil {
			return kitagenthook.InstallOptions{}, err
		}
	}
	return kitInstallOptions(agent, opts), nil
}

func kitInstallOptions(agent kitagenthook.Agent, opts InstallOptions) kitagenthook.InstallOptions {
	command := strings.TrimSpace(opts.Command)
	if command != "" {
		command += " " + agentHookMarker
	}
	kitOpts := kitagenthook.InstallOptions{
		ConfigPath: opts.ConfigPath,
		Executable: opts.Executable,
		Command:    command,
		Marker:     agentHookMarker,
		Hooks: []kitagenthook.Hook{
			{Event: kitagenthook.EventPreToolUse, Matcher: kitagenthook.ToolBash, Timeout: opts.Timeout},
			{Event: kitagenthook.EventPostToolUse, Matcher: kitagenthook.ToolBash, Timeout: opts.Timeout},
			{Event: kitagenthook.EventStop, Timeout: opts.Timeout},
		},
	}
	if opts.Command == "" {
		kitOpts.Arguments = []string{
			"agent-hook", "run", "--agent", string(agent), agentHookMarker,
		}
	}
	return kitOpts
}

func commandAgent(command string) (kitagenthook.Agent, error) {
	fields, err := splitHookCommand(command)
	if err != nil {
		return "", err
	}
	if len(fields) < 3 || fields[1] != "agent-hook" || fields[2] != "run" {
		return "", fmt.Errorf("hook command must invoke %s", agentHookRunner)
	}
	selected := ""
	for i := 3; i < len(fields); i++ {
		field := fields[i]
		if field == "--" {
			return "", fmt.Errorf("hook command must not contain an argument terminator")
		}
		value := ""
		switch {
		case field == "--agent":
			if i+1 >= len(fields) || strings.HasPrefix(fields[i+1], "--") {
				return "", fmt.Errorf("--agent requires a value")
			}
			i++
			value = fields[i]
		case strings.HasPrefix(field, "--agent="):
			value = strings.TrimPrefix(field, "--agent=")
		default:
			continue
		}
		if value == "" {
			return "", fmt.Errorf("--agent requires a value")
		}
		if selected != "" {
			return "", fmt.Errorf("hook command must select exactly one agent")
		}
		selected = value
	}
	if selected == "" {
		return "", fmt.Errorf("hook command must select an agent")
	}
	if strings.EqualFold(selected, string(AgentGrok)) {
		return AgentGrok, nil
	}
	return kitagenthook.ParseAgent(selected)
}

func splitHookCommand(command string) ([]string, error) {
	var fields []string
	var field strings.Builder
	var quote rune
	escaped := false
	previousUnescapedDollar := false
	started := false
	flush := func() {
		if !started {
			return
		}
		fields = append(fields, field.String())
		field.Reset()
		started = false
	}

	for _, r := range strings.TrimSpace(command) {
		if unicode.IsControl(r) {
			return nil, fmt.Errorf("hook command must be a single command line")
		}
		if escaped {
			field.WriteRune(r)
			started = true
			escaped = false
			previousUnescapedDollar = false
			continue
		}
		if quote != 0 {
			switch {
			case r == quote:
				quote = 0
				previousUnescapedDollar = false
			case quote == '"' && r == '\\':
				escaped = true
				previousUnescapedDollar = false
			case quote == '"' && r == '`':
				return nil, fmt.Errorf("hook command contains unsupported shell operator %q", r)
			case quote == '"' && r == '(' && previousUnescapedDollar:
				return nil, fmt.Errorf("hook command contains unsupported shell operator %q", "$(")
			default:
				field.WriteRune(r)
				previousUnescapedDollar = quote == '"' && r == '$'
			}
			started = true
			continue
		}

		switch {
		case unicode.IsSpace(r):
			flush()
		case r == '\'' || r == '"':
			quote = r
			started = true
		case r == '\\':
			escaped = true
			started = true
		case strings.ContainsRune(";&|<>()`#", r):
			return nil, fmt.Errorf("hook command contains unsupported shell operator %q", r)
		default:
			field.WriteRune(r)
			started = true
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("hook command contains an unterminated quote")
	}
	if escaped {
		return nil, fmt.Errorf("hook command contains an incomplete escape")
	}
	flush()
	return fields, nil
}

func profileError(agent kitagenthook.Agent, configuredPath string, err error) error {
	profile, _ := kitagenthook.LookupProfile(agent)
	if agent == AgentGrok {
		profile.DisplayName = "Grok Build"
	}
	path := configuredPath
	if path == "" {
		if agent == AgentGrok {
			path = DefaultGrokHooksPath()
		} else {
			path, _ = kitagenthook.ConfigPath(agent)
		}
	}
	if path == "" {
		return fmt.Errorf("%s: %w", profile.DisplayName, err)
	}
	return fmt.Errorf("%s (%s): %w", profile.DisplayName, path, err)
}

func printInstallResult(stdout io.Writer, result kitagenthook.Result, dryRun bool) {
	profile, _ := kitagenthook.LookupProfile(result.Agent)
	if result.Agent == AgentGrok {
		profile.DisplayName = "Grok Build"
	}
	switch {
	case dryRun && result.Changed:
		fmt.Fprintf(stdout, "would update %s agent hooks in %s\n", profile.DisplayName, result.ConfigPath)
	case dryRun:
		fmt.Fprintf(stdout, "%s agent hooks already installed in %s\n", profile.DisplayName, result.ConfigPath)
	case result.Changed:
		fmt.Fprintf(stdout, "installed %s agent hooks in %s\n", profile.DisplayName, result.ConfigPath)
	default:
		fmt.Fprintf(stdout, "%s agent hooks already installed in %s\n", profile.DisplayName, result.ConfigPath)
	}
}
