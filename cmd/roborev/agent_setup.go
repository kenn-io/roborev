package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"
	kitagenthook "go.kenn.io/kit/agenthook"
	"go.kenn.io/kit/agentsetup"
	"go.kenn.io/kit/consolewindow"

	"go.kenn.io/roborev/internal/agenthook"
	"go.kenn.io/roborev/internal/githook"
	"go.kenn.io/roborev/internal/mcpconfig"
	"go.kenn.io/roborev/internal/skills"
)

func agentSetupCmd() *cobra.Command {
	var cmd *cobra.Command
	var mcp bool
	setup := agentsetup.Options{SetupCommand: "roborev agent setup", Items: func(name string) ([]agentsetup.Item, error) {
		if name != "claude" && name != "codex" {
			return nil, errors.New("setup covers Claude and Codex")
		}
		check, _ := cmd.Flags().GetBool("check")
		printOnly, _ := cmd.Flags().GetBool("print")
		var server *string
		if cmd.Flags().Changed("server") && !check {
			server = &serverAddr
		}
		if printOnly {
			binary, err := githook.ResolveRoborevPath("")
			if err != nil {
				return nil, err
			}
			address := ""
			if server != nil {
				address = *server
			}
			return []agentsetup.Item{agentsetup.MCP(name, "roborev", mcpconfig.Server(mcpconfig.Options{Executable: binary.Path, Server: address}))}, nil
		}
		var selected *bool
		if cmd.Flags().Changed("mcp") && !check {
			selected = &mcp
		}
		items, _, err := agentSetupItems(name, selected, server)
		if err != nil {
			return []agentsetup.Item{{Name: "setup", Check: func(context.Context) (agentsetup.Status, error) { return agentsetup.Status{}, err }}}, nil
		}
		return items, nil
	}}
	cmd = agentsetup.NewCommand(setup)
	cmd.Example = "  roborev agent setup --check"
	cmd.Flags().BoolVar(&mcp, "mcp", false, "register MCP and use MCP skills")
	run := cmd.RunE
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		err := run(cmd, args)
		if errors.Is(err, agentsetup.ErrNotReady) {
			return &exitError{code: 1, cause: err, silent: true}
		}
		return err
	}
	return cmd
}

// agentReadiness checks one agent in the mode its hooks already use and
// returns the row that most needs attention with one detail per row.
func agentReadiness(ctx context.Context, name string) (agentsetup.Row, []string) {
	report, _ := runAgentSetup(ctx, []string{name}, nil, true, nil)
	worst := agentsetup.Row{State: agentsetup.StateOK}
	var details []string
	for _, row := range report.Rows {
		detail := fmt.Sprintf("%s: %s", row.Piece, row.State)
		if row.Reason != "" {
			detail += ": " + row.Reason
		}
		if row.NextStep != "" {
			detail += "; " + row.NextStep
		}
		details = append(details, detail)
		if worst.State == agentsetup.StateOK && row.State != agentsetup.StateOK || row.State == agentsetup.StateFailed && worst.State != agentsetup.StateFailed {
			worst = row
		}
	}
	return worst, details
}

// runAgentSetup installs or checks each agent. A nil mcp keeps the mode each
// agent already uses, and an agent whose config cannot be read reports a failed
// row without stopping the others.
func runAgentSetup(ctx context.Context, names []string, mcp *bool, check bool, server *string) (agentsetup.Report, error) {
	report, err := agentsetup.Run(ctx, agentsetup.Options{Agents: names, CheckOnly: check, SetupCommand: "roborev agent setup", Items: func(name string) ([]agentsetup.Item, error) {
		items, _, err := agentSetupItems(name, mcp, server)
		if err != nil {
			items = []agentsetup.Item{{Name: "setup", Check: func(context.Context) (agentsetup.Status, error) { return agentsetup.Status{}, err }}}
		}
		return items, nil
	}})
	return report, err
}

// agentSetupItems keeps the installed hook's executable, server, and custom
// arguments unless setup selects a change. An explicit server reaches both the
// hook and MCP, and a generated hook's missing absolute executable is replaced.
func agentSetupItems(name string, mcp *bool, server *string) ([]agentsetup.Item, bool, error) {
	agent := kitagenthook.Agent(name)
	binary, err := githook.ResolveRoborevPath("")
	if err != nil {
		return nil, false, err
	}
	installed, found, err := agenthook.InstalledOptions(agent)
	if err != nil {
		return nil, false, err
	}
	opts := agenthook.InstallOptions{Executable: binary.Path, Timeout: 10 * time.Second}
	opts = installedSetupOptions(opts, installed, found, binary.Path)
	registration, registered := mcpconfig.Registration(mcpconfig.Options{Agent: skills.Agent(name)})
	if opts.RoborevServerAddr == "" && registered {
		opts.RoborevServerAddr = registration.DaemonAddress()
	}
	selected := opts.MCP
	if mcp != nil {
		selected = *mcp
	}
	if server != nil {
		opts.RoborevServerAddr = *server
	}
	opts.Agent, opts.MCP = name, selected
	hooks, err := setupHookOptions(agent, opts)
	if err != nil {
		return nil, selected, err
	}
	path, err := kitagenthook.ConfigPath(agent)
	if err != nil {
		return nil, selected, err
	}
	mixed := false
	if found {
		command, specs, different, err := agenthook.InstalledCommand(agent)
		if err != nil {
			return nil, selected, err
		}
		mixed = different
		if !mixed {
			command, err = agenthook.RewriteCommand(command, agent, opts, binary.Path)
			if err != nil {
				return nil, selected, err
			}
			for _, required := range hooks.Hooks {
				if !slices.ContainsFunc(specs, func(spec kitagenthook.Hook) bool { return spec.Event == required.Event }) {
					specs = append(specs, required)
				}
			}
			hooks.Hooks = specs
			hooks.Command, hooks.CommandWindows, hooks.Executable, hooks.Arguments = command.Native, command.Windows, "", nil
		}
	}
	hookItem := agentsetup.Hooks(name, hooks)
	if mixed {
		hookItem.Install = nil
		hookItem.Check = mixedHookStatus
	}
	items := []agentsetup.Item{hookItem, {
		Name:    "skills",
		Install: func(context.Context) error { return agenthook.InstallSkills(agent, path, selected) },
		Check: func(context.Context) (agentsetup.Status, error) {
			status, _ := skills.StatusForAgentMode(skills.Agent(agent), &selected)
			states := slices.Collect(maps.Values(status.Skills))
			switch {
			case !status.Available || len(states) == 0 || slices.Contains(states, skills.SkillMissing):
				return agentsetup.Status{State: agentsetup.StateMissing, Reason: "Bundled skills are missing"}, nil
			case slices.Contains(states, skills.SkillOutdated):
				return agentsetup.Status{State: agentsetup.StateOutdated, Reason: "Bundled skills are outdated"}, nil
			}
			return agentsetup.Status{State: agentsetup.StateOK, Reason: "Bundled skills are current"}, nil
		},
	}}
	if mixed && (selected || registered) {
		items = append(items, agentsetup.Item{Name: "mcp roborev", Check: mixedHookStatus})
	} else if selected {
		item, err := agentMCPItem(name, opts.Executable, opts.RoborevServerAddr, registration, registered)
		if err != nil {
			return nil, selected, err
		}
		items = append(items, item)
	}
	return items, selected, nil
}

func mixedHookStatus(context.Context) (agentsetup.Status, error) {
	return agentsetup.Status{State: agentsetup.StateUnknown, Reason: "Hook commands differ between events", NextStep: "Inspect the hooks in the agent"}, nil
}

// installedSetupOptions keeps the selected hook settings and replaces a missing
// generated executable with the current binary.
func installedSetupOptions(opts, installed agenthook.InstallOptions, found bool, executable string) agenthook.InstallOptions {
	if !found {
		return opts
	}
	if _, err := os.Stat(installed.Executable); installed.Command == "" && filepath.IsAbs(installed.Executable) && errors.Is(err, fs.ErrNotExist) {
		installed.Executable = executable
	}
	return installed
}

func setupHookOptions(agent kitagenthook.Agent, opts agenthook.InstallOptions) (kitagenthook.InstallOptions, error) {
	if opts.Command != "" {
		opts.Executable = ""
	}
	return agenthook.KitInstallOptions(agent, opts)
}

// agentMCPItem registers the roborev MCP server, or repairs the connection the
// user already has so its transport and other settings survive.
func agentMCPItem(name, executable, address string, server mcpconfig.RegisteredServer, registered bool) (agentsetup.Item, error) {
	opts := mcpconfig.Options{Agent: skills.Agent(name)}
	if !registered {
		return agentsetup.MCP(name, "roborev", mcpconfig.Server(mcpconfig.Options{Executable: executable, Server: address})), nil
	}
	if address != "" && server.DaemonAddress() != address {
		var err error
		if server, err = server.WithServer(address); err != nil {
			return agentsetup.Item{}, err
		}
	}
	item := agentsetup.MCP(name, "roborev", server.Server())
	item.Install = func(context.Context) error { return mcpconfig.Repair(opts, server) }
	if name == "claude" {
		kitCheck := item.Check
		item.Check = func(ctx context.Context) (agentsetup.Status, error) {
			status, err := kitCheck(ctx)
			if err != nil || status.State != agentsetup.StateOutdated {
				return status, err
			}
			current, found := mcpconfig.Registration(opts)
			if !found || !reflect.DeepEqual(current, server) {
				return status, nil
			}
			return checkClaudeMCPWithHeadersHelper(ctx)
		}
	}
	return item, nil
}

// Kit reports entries with unmodeled fields as outdated. Ask Claude directly
// when the modeled fields match, keeping the configuration intact.
func checkClaudeMCPWithHeadersHelper(ctx context.Context) (agentsetup.Status, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := consolewindow.CommandContext(ctx, "claude", "mcp", "get", "--", "roborev")
	cmd.WaitDelay = 100 * time.Millisecond
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return agentsetup.Status{State: agentsetup.StateUnknown, Reason: ctx.Err().Error(), NextStep: "Retry once the server is ready"}, nil
	}
	if err != nil && len(output) == 0 {
		return agentsetup.Status{State: agentsetup.StateUnknown, Reason: err.Error(), NextStep: "Inspect the server in the agent"}, nil
	}
	fields := map[string]string{}
	for line := range strings.SplitSeq(string(output), "\n") {
		key, value, _ := strings.Cut(strings.TrimSpace(line), ":")
		fields[key] = strings.TrimSpace(value)
	}
	if strings.HasPrefix(fields["Scope"], "User config") {
		status := strings.TrimLeft(fields["Status"], "✓✔✗✘ ")
		if fields["Issue"] != "" || status == "Failed to connect" {
			return agentsetup.Status{State: agentsetup.StateFailed, Reason: strings.TrimSpace(fields["Status"] + "\n" + fields["Issue"]), NextStep: "Correct the server error and retry"}, nil
		}
		if status == "Connected" {
			return agentsetup.Status{State: agentsetup.StateOK, Reason: "Claude reports the user server connected"}, nil
		}
	}
	return agentsetup.Status{State: agentsetup.StateUnknown, Reason: strings.TrimSpace(string(output)), NextStep: "Inspect the server in the agent"}, nil
}
