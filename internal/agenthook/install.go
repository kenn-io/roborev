package agenthook

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	kitagenthook "go.kenn.io/kit/agenthook"
	kitdaemon "go.kenn.io/kit/daemon"

	"go.kenn.io/roborev/internal/agentconfig"
	"go.kenn.io/roborev/internal/mcpconfig"
	"go.kenn.io/roborev/internal/skills"
)

const (
	agentHookRunner    = "agent-hook run"
	RegistrationSource = "roborev-agent-hook"
	agentHookMarker    = "--source=" + RegistrationSource
)

type InstallOptions struct {
	MCP               bool
	MCPTransport      string
	MCPURL            string
	RoborevServerAddr string
	Agent             string
	Executable        string
	Command           string
	ConfigPath        string
	Timeout           time.Duration
	DryRun            bool
}

func resolveMCPDaemon(enabled bool, transport, url, server string) (string, error) {
	if !enabled && (url != "" || (transport != "" && transport != "stdio")) {
		return "", fmt.Errorf("MCP transport options require --mcp")
	}
	if enabled {
		if err := mcpconfig.Validate(mcpconfig.Options{Transport: transport, URL: url}); err != nil {
			return "", err
		}
	}
	if transport == "http" {
		address, err := mcpconfig.DaemonAddress(url)
		if err != nil {
			return "", err
		}
		if server != "" {
			return "", fmt.Errorf("--mcp-url selects the hook daemon; omit --roborev-server for HTTP MCP")
		}
		if _, err := kitdaemon.ParseEndpoint(address, kitdaemon.ParseEndpointOptions{TCPPolicy: kitdaemon.RequireLoopback}); err != nil {
			return "", fmt.Errorf("MCP hook daemon: %w", err)
		}
		server = address
	}
	return server, nil
}

func RunInstall(opts InstallOptions, stdout io.Writer) error {
	address, err := resolveMCPDaemon(opts.MCP, opts.MCPTransport, opts.MCPURL, opts.RoborevServerAddr)
	if err != nil {
		return err
	}
	opts.RoborevServerAddr = address
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
		result, runErr := runInstall(agent, opts, stdout)
		if runErr != nil {
			errs = append(errs, profileError(agent, opts.ConfigPath, runErr))
			continue
		}
		printInstallResult(stdout, result, opts.DryRun)
	}
	return errors.Join(errs...)
}

func RunDump(opts InstallOptions, stdout io.Writer) error {
	address, err := resolveMCPDaemon(opts.MCP, opts.MCPTransport, opts.MCPURL, opts.RoborevServerAddr)
	if err != nil {
		return err
	}
	opts.RoborevServerAddr = address

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

	result, err := planNativeHooks(agent, opts)
	if err != nil {
		return profileError(agent, opts.ConfigPath, err)
	}
	_, err = stdout.Write(result.Data)
	return err
}

func planNativeHooks(agent kitagenthook.Agent, opts InstallOptions) (kitagenthook.Result, error) {
	if agent == AgentGrok {
		return planGrokInstall(opts)
	}
	kitOpts, err := validatedKitInstallOptions(agent, opts)
	if err != nil {
		return kitagenthook.Result{}, err
	}
	return kitagenthook.PlanInstall(agent, kitOpts)
}

func runInstall(agent kitagenthook.Agent, opts InstallOptions, stdout io.Writer) (kitagenthook.Result, error) {
	planned, err := planNativeHooks(agent, opts)
	if err != nil {
		return kitagenthook.Result{}, err
	}
	if opts.MCP {
		dir := ""
		if opts.ConfigPath != "" && agent != kitagenthook.AgentClaude {
			dir = filepath.Dir(opts.ConfigPath)
			if (agent == AgentGrok || agent == kitagenthook.AgentCopilot) && strings.EqualFold(filepath.Base(dir), "hooks") {
				dir = filepath.Dir(dir)
			}
		}
		executable := opts.Executable
		if opts.Command != "" {
			fields, err := splitHookCommand(opts.Command)
			if err != nil {
				return kitagenthook.Result{}, err
			}
			executable = fields[0] // planNativeHooks has validated the command.
		}
		mcpOpts := mcpconfig.Options{
			Agent: skills.Agent(agent), ConfigDir: dir, Executable: executable,
			Transport: opts.MCPTransport, URL: opts.MCPURL, Server: opts.RoborevServerAddr, DryRun: true,
		}
		path, err := mcpconfig.ConfigPath(mcpOpts)
		if err != nil {
			return kitagenthook.Result{}, err
		}
		shared := filepath.Clean(path) == filepath.Clean(planned.ConfigPath)
		var mcpResult mcpconfig.Result
		if shared {
			// Kit owns hook parsing and returns bytes. Merge that result once,
			// without re-reading or planning the existing MCP configuration.
			data, err := mcpconfig.Merge(planned.Data, mcpOpts)
			if err != nil {
				return kitagenthook.Result{}, err
			}
			planned.Changed = planned.Changed || !bytes.Equal(planned.Data, data)
			planned.Data = data
			mcpResult = mcpconfig.Result{Path: path, Data: data, Changed: planned.Changed}
		} else {
			mcpResult, err = mcpconfig.Install(mcpOpts)
			if err != nil {
				return kitagenthook.Result{}, err
			}
		}
		if opts.DryRun {
			fmt.Fprintf(stdout, "MCP configuration in %s (changed: %t):\n%s\n", mcpResult.Path, mcpResult.Changed, mcpResult.Data)
		} else if !shared && mcpResult.Changed {
			if err := agentconfig.Write(mcpResult.Path, mcpResult.Data); err != nil {
				return kitagenthook.Result{}, err
			}
			fmt.Fprintf(stdout, "installed MCP configuration in %s\n", mcpResult.Path)
		}
	}
	if opts.DryRun {
		fmt.Fprintf(stdout, "would install or update bundled %s skills in %s (MCP: %t)\n", agent, agentHookSkillsDir(agent, planned.ConfigPath), opts.MCP)
		return planned, nil
	}
	if err := installAgentHookSkills(agent, planned.ConfigPath, opts.MCP); err != nil {
		return kitagenthook.Result{}, err
	}
	if !planned.Changed {
		return planned, nil
	}
	if err := agentconfig.Write(planned.ConfigPath, planned.Data); err != nil {
		return planned, err
	}
	return planned, nil
}

func installAgentHookSkills(agent kitagenthook.Agent, configPath string, mcp bool) error {
	skillAgent := skills.Agent(agent)

	if _, err := skills.InstallToPath(skillAgent, agentHookSkillsDir(agent, configPath), &mcp); err != nil {
		return fmt.Errorf("install bundled %s skills: %w", skillAgent, err)
	}
	return nil
}

func agentHookSkillsDir(agent kitagenthook.Agent, configPath string) string {
	configDir := filepath.Dir(configPath)
	if (agent == AgentGrok || agent == kitagenthook.AgentCopilot) && strings.EqualFold(filepath.Base(configDir), "hooks") {
		configDir = filepath.Dir(configDir)
	}
	return filepath.Join(configDir, "skills")
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
	kitOpts := kitInstallOptions(agent, opts)
	if opts.RoborevServerAddr != "" && opts.Command != "" {
		args, err := kitagenthook.BuildCommand("--roborev-server", opts.RoborevServerAddr)
		if err != nil {
			return kitagenthook.InstallOptions{}, err
		}
		kitOpts.Command += " " + args.Native
	}
	if agent == kitagenthook.AgentClaude && opts.Command == "" {
		commands, err := kitagenthook.BuildCommand(kitOpts.Executable, kitOpts.Arguments...)
		if err != nil {
			return kitagenthook.InstallOptions{}, err
		}
		kitOpts.Executable = ""
		kitOpts.Arguments = nil
		kitOpts.Command = commands.POSIX
	}
	return kitOpts, nil
}

func kitInstallOptions(agent kitagenthook.Agent, opts InstallOptions) kitagenthook.InstallOptions {
	command := strings.TrimSpace(opts.Command)
	if command != "" {
		command += " " + agentHookMarker
		if opts.MCP {
			command += " --mcp"
		}
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
	if opts.RoborevServerAddr != "" && opts.Command == "" {
		kitOpts.Arguments = append(kitOpts.Arguments, "--roborev-server", opts.RoborevServerAddr)
	}

	if opts.MCP && opts.Command == "" {
		kitOpts.Arguments = append(kitOpts.Arguments, "--mcp")
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
