package main

import (
	"context"
	"errors"
	"fmt"
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
		items, err := agentSetupItems(name, selected, server, false)
		if err != nil {
			return []agentsetup.Item{{Name: "setup", Check: func(context.Context) (agentsetup.Status, error) { return agentsetup.Status{}, err }}}, nil
		}
		return items, nil
	}}
	cmd = agentsetup.NewCommand(setup)
	cmd.Long = "Install hooks and skills for Claude Code and Codex. Use --mcp to register the server and MCP skills. Existing connection settings are preserved."
	cmd.Example = "  roborev agent setup --check"
	cmd.Flags().BoolVar(&mcp, "mcp", false, "register MCP and use MCP skills")
	run := cmd.RunE
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		check, _ := cmd.Flags().GetBool("check")
		if check && (cmd.Flags().Changed("server") || cmd.Flags().Changed("mcp")) {
			return errors.New("--check cannot be combined with --server or --mcp")
		}
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
func agentReadiness(ctx context.Context, name string, hooksOnly bool) (agentsetup.Row, []string) {
	report, _ := agentsetup.Run(ctx, agentsetup.Options{Agents: []string{name}, CheckOnly: true, SetupCommand: "roborev agent setup", Items: func(name string) ([]agentsetup.Item, error) {
		items, err := agentSetupItems(name, nil, nil, hooksOnly)
		if err != nil {
			items = []agentsetup.Item{{Name: "setup", Check: func(context.Context) (agentsetup.Status, error) { return agentsetup.Status{}, err }}}
		}
		return items, nil
	}})
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

// agentSetupItems keeps the installed hook's executable, server, and custom
// arguments unless setup selects a change. An explicit server reaches both the
// hook and MCP, and a generated hook's missing absolute executable is replaced.
func agentSetupItems(name string, mcp *bool, server *string, hooksOnly bool) ([]agentsetup.Item, error) {
	agent := kitagenthook.Agent(name)
	binary, err := githook.ResolveRoborevPath("")
	if err != nil {
		return nil, err
	}
	command, specs, mixed, err := agenthook.InstalledCommand(agent)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	opts := agenthook.InstallOptions{Executable: binary.Path, Timeout: 10 * time.Second}
	found := len(specs) > 0
	if found {
		installed, err := agenthook.CommandOptions(command.Native)
		if err != nil {
			return nil, err
		}
		opts.Executable, opts.MCP, opts.RoborevServerAddr = installed.Executable, installed.MCP, installed.RoborevServerAddr
	}
	installedMode := opts.MCP
	installedExecutable := opts.Executable
	selected := installedMode
	if mcp != nil {
		selected = *mcp
	}
	if server != nil {
		opts.RoborevServerAddr = *server
	}
	opts.Agent, opts.MCP = name, selected
	hooks, err := agenthook.KitInstallOptions(agent, opts)
	if err != nil {
		return nil, err
	}
	path, err := kitagenthook.ConfigPath(agent)
	if err != nil {
		return nil, err
	}
	if found && !mixed {
		command, err = agenthook.RewriteCommand(command, agent, opts, binary.Path)
		if err != nil {
			return nil, err
		}
		updated, err := agenthook.CommandOptions(command.Native)
		if err != nil {
			return nil, err
		}
		opts.Executable = updated.Executable
		hooks.Command, hooks.CommandWindows, hooks.Executable, hooks.Arguments = command.Native, command.Windows, "", nil
	}
	hookItem := agentsetup.Hooks(name, hooks)
	if mixed {
		hookItem.Install = nil
		hookItem.Check = mixedHookStatus
	}
	if hooksOnly {
		return []agentsetup.Item{hookItem}, nil
	}
	var registration mcpconfig.RegisteredServer
	var registered bool
	if selected && !mixed {
		registration, registered, err = mcpconfig.Registration(mcpconfig.Options{Agent: skills.Agent(name)})
		if err != nil {
			return nil, err
		}
	}
	skillItem := agentsetup.Item{
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
	}
	if mixed {
		skillItem.Install = nil
		if selected != installedMode {
			skillItem.Check = mixedHookStatus
		}
	}
	var dependencyErr error
	if !mixed {
		installHook := hookItem.Install
		hookItem.Install = func(ctx context.Context) error {
			if dependencyErr != nil {
				return fmt.Errorf("MCP setup failed: %w", dependencyErr)
			}
			dependencyErr = installHook(ctx)
			return dependencyErr
		}
		installSkills := skillItem.Install
		skillItem.Install = func(ctx context.Context) error {
			if dependencyErr != nil {
				return fmt.Errorf("integration setup failed: %w", dependencyErr)
			}
			return installSkills(ctx)
		}
	}
	items := []agentsetup.Item{hookItem, skillItem}
	if mixed && (selected || registered) {
		items = append(items, agentsetup.Item{Name: "mcp roborev", Check: mixedHookStatus})
	} else if selected {
		address := opts.RoborevServerAddr
		if registered && server == nil {
			address = ""
		}
		if registered && opts.Executable != installedExecutable && registration.Command == installedExecutable && filepath.IsAbs(registration.Command) {
			if i := slices.Index(registration.Args, "mcp"); i >= 0 && i+1 < len(registration.Args) && registration.Args[i+1] == "serve" {
				registration.Command = opts.Executable
			}
		}
		item, err := agentMCPItem(name, opts.Executable, address, registration, registered)
		if err != nil {
			return nil, err
		}
		installMCP := item.Install
		item.Install = func(ctx context.Context) error {
			dependencyErr = installMCP(ctx)
			return dependencyErr
		}
		items = append([]agentsetup.Item{item}, items...)
	}
	return items, nil
}

func mixedHookStatus(context.Context) (agentsetup.Status, error) {
	return agentsetup.Status{State: agentsetup.StateUnknown, Reason: "Hook commands or settings need inspection", NextStep: "Inspect the hooks in the agent"}, nil
}

// agentMCPItem registers the roborev MCP server, or repairs the connection the
// user already has so its transport and other settings survive.
func agentMCPItem(name, executable, address string, server mcpconfig.RegisteredServer, registered bool) (agentsetup.Item, error) {
	opts := mcpconfig.Options{Agent: skills.Agent(name)}
	if !registered {
		server = mcpconfig.RegisteredServer{Command: executable, Args: mcpconfig.Server(mcpconfig.Options{Executable: executable, Server: address}).Args}
	}
	if address != "" && server.DaemonAddress() != address {
		var err error
		if server, err = server.WithServer(address); err != nil {
			return agentsetup.Item{}, err
		}
	}
	item := agentsetup.MCP(name, "roborev", server.Server())
	if registered {
		item.Install = func(context.Context) error { return mcpconfig.Repair(opts, server) }
	}
	check := item.Check
	item.Check = func(ctx context.Context) (agentsetup.Status, error) {
		unknown := agentsetup.Status{State: agentsetup.StateUnknown, Reason: "The preserved MCP settings need inspection", NextStep: "Inspect the MCP server in the agent"}
		if server.HeadersHelper != "" {
			return unknown, nil
		}
		status, err := check(ctx)
		if err != nil {
			return status, err
		}
		if status.State == agentsetup.StateOutdated {
			current, found, err := mcpconfig.Registration(opts)
			if err != nil {
				return status, err
			}
			if found && reflect.DeepEqual(current.Server(), server.Server()) {
				return unknown, nil
			}
		}
		if status.State == agentsetup.StateOK && !isRoborevMCP(server) {
			next := "Confirm the server provides RoboRev tools in the agent"
			if server.URL != "" {
				next = "Run roborev mcp status and use an advertised URL, or confirm the remote server's tools in the agent"
			}
			return agentsetup.Status{State: agentsetup.StateUnknown, Reason: "Cannot verify that the server provides RoboRev tools", NextStep: next}, nil
		}
		return status, nil
	}
	return item, nil
}

func isRoborevMCP(server mcpconfig.RegisteredServer) bool {
	if server.URL != "" {
		listeners, err := discoverMCPListeners(2 * time.Second)
		return err == nil && slices.ContainsFunc(listeners, func(listener mcpListenerStatus) bool { return listener.URL == server.URL })
	}
	name := filepath.Base(server.Command)
	if !strings.EqualFold(name, "roborev") && !strings.EqualFold(name, "roborev.exe") {
		binary, err := githook.ResolveRoborevPath("")
		if err != nil || server.Command != binary.Path {
			return false
		}
	}
	for i := 1; i < len(server.Args); i++ {
		if server.Args[i-1] == "mcp" && server.Args[i] == "serve" {
			return true
		}
	}
	return false
}
