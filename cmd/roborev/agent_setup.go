package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
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
			return unknownAgentItems(), nil
		}
		check, _ := cmd.Flags().GetBool("check")
		printOnly, _ := cmd.Flags().GetBool("print")
		if check && (cmd.Flags().Changed("server") || cmd.Flags().Changed("mcp")) {
			return nil, errors.New("--check cannot be combined with --server or --mcp")
		}
		if printOnly {
			binary, err := githook.ResolveRoborevPath("")
			if err != nil {
				return nil, err
			}
			address := ""
			if cmd.Flags().Changed("server") {
				address = serverAddr
			}
			return []agentsetup.Item{agentsetup.MCP(name, "roborev", mcpconfig.Server(mcpconfig.Options{Executable: binary.Path, Server: address}))}, nil
		}
		var selected *bool
		if cmd.Flags().Changed("mcp") && !check {
			selected = &mcp
		}
		var server *string
		if cmd.Flags().Changed("server") && !check {
			server = &serverAddr
		}
		return agentSetupItems(name, selected, server, false)
	}}
	cmd = agentsetup.NewCommand(setup)
	cmd.Long = "Install hooks and skills for Claude Code and Codex. Use --mcp to register the server and MCP skills."
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

func unknownAgentItems() []agentsetup.Item {
	check := func(context.Context) (agentsetup.Status, error) {
		return agentsetup.Status{State: agentsetup.StateUnknown, Reason: "Setup checks cover Claude Code and Codex", NextStep: "Run roborev agent setup --agent claude or --agent codex"}, nil
	}
	return []agentsetup.Item{{Name: "setup", Check: check}}
}

// agentReadiness checks an agent in its installed skill mode. Quickstart asks
// about hooks only; doctor also checks skills and MCP when enabled.
func agentReadiness(ctx context.Context, name string, hooksOnly bool) (agentsetup.Row, []string) {
	if name != "claude" && name != "codex" {
		row := agentsetup.Row{Agent: name, Piece: "setup", State: agentsetup.StateUnknown, Reason: "Setup checks cover Claude Code and Codex", NextStep: "Run roborev agent setup --agent claude or --agent codex"}
		return row, []string{"setup: unknown: Setup checks cover Claude Code and Codex"}
	}
	report, _ := agentsetup.Run(ctx, agentsetup.Options{Agents: []string{name}, CheckOnly: true, SetupCommand: "roborev agent setup", Items: func(name string) ([]agentsetup.Item, error) {
		return agentSetupItems(name, nil, nil, hooksOnly)
	}})
	return readinessSummary(report.Rows)
}

func readinessSeverity(state agentsetup.State) int {
	switch state {
	case agentsetup.StateOK:
		return 0
	case agentsetup.StateFailed:
		return 3
	case agentsetup.StateUnknown:
		return 1
	default:
		return 2
	}
}

func agentSetupItems(name string, mcp *bool, server *string, hooksOnly bool) ([]agentsetup.Item, error) {
	agent := kitagenthook.Agent(name)
	binary, err := githook.ResolveRoborevPath("")
	if err != nil {
		return nil, err
	}
	selected := false
	if status, ok := skills.StatusForAgent(skills.Agent(name)); ok {
		selected = status.MCP
	}
	if mcp != nil {
		selected = *mcp
	}
	opts := agenthook.InstallOptions{Agent: name, Executable: binary.Path, MCP: selected, Timeout: 10 * time.Second}
	if server != nil {
		opts.RoborevServerAddr = *server
	}
	hooks, err := agenthook.KitInstallOptions(agent, opts)
	if err != nil {
		return nil, err
	}
	hookItem := agentsetup.Hooks(name, hooks)
	if mcp == nil && hooksOnly {
		checkHook := hookItem.Check
		hookItem.Check = func(ctx context.Context) (agentsetup.Status, error) {
			status, err := checkHook(ctx)
			if err != nil || status.State != agentsetup.StateOutdated {
				return status, err
			}
			alternate := opts
			alternate.MCP = !selected
			alternateHooks, err := agenthook.KitInstallOptions(agent, alternate)
			if err != nil {
				return status, err
			}
			return agentsetup.Hooks(name, alternateHooks).Check(ctx)
		}
	}
	items := []agentsetup.Item{hookItem}
	if hooksOnly {
		return items, nil
	}
	path, err := kitagenthook.ConfigPath(agent)
	if err != nil {
		return nil, err
	}
	items = append(items, agentsetup.Item{
		Name:    "bundled skills",
		Install: func(context.Context) error { return agenthook.InstallSkills(agent, path, selected) },
		Check: func(context.Context) (agentsetup.Status, error) {
			status, _ := skills.StatusForAgentMode(skills.Agent(agent), &selected)
			states := slices.Collect(maps.Values(status.Skills))
			switch {
			case !status.Available || len(states) == 0 || slices.Contains(states, skills.SkillMissing):
				return agentsetup.Status{State: agentsetup.StateMissing}, nil
			case slices.Contains(states, skills.SkillOutdated):
				return agentsetup.Status{State: agentsetup.StateOutdated}, nil
			default:
				return agentsetup.Status{State: agentsetup.StateOK}, nil
			}
		},
	})
	if selected {
		address := ""
		if server != nil {
			address = *server
		}
		mcpItem := agentsetup.MCP(name, "roborev", mcpconfig.Server(mcpconfig.Options{Executable: binary.Path, Server: address}))
		installMCP := mcpItem.Install
		var mcpErr error
		mcpItem.Install = func(ctx context.Context) error {
			mcpErr = installMCP(ctx)
			return mcpErr
		}
		for i := range items {
			install := items[i].Install
			items[i].Install = func(ctx context.Context) error {
				if mcpErr != nil {
					return fmt.Errorf("MCP setup failed: %w", mcpErr)
				}
				return install(ctx)
			}
		}
		items = append([]agentsetup.Item{mcpItem}, items...)
	}
	return items, nil
}

func readinessSummary(rows []agentsetup.Row) (agentsetup.Row, []string) {
	worst := agentsetup.Row{State: agentsetup.StateOK}
	for _, row := range rows {
		if readinessSeverity(row.State) > readinessSeverity(worst.State) {
			worst = row
		}
	}
	var details []string
	for _, row := range rows {
		detail := fmt.Sprintf("%s: %s", row.Piece, row.State)
		if row.Reason != "" {
			detail += ": " + row.Reason
		}
		if row.NextStep != "" && row.NextStep != worst.NextStep {
			detail += "; " + row.NextStep
		}
		details = append(details, detail)
	}
	return worst, details
}
