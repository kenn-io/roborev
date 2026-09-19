package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"go.kenn.io/roborev/internal/githook"
	"go.kenn.io/roborev/internal/mcpconfig"
	"go.kenn.io/roborev/internal/skills"
)

func mcpInstallCmd() *cobra.Command {
	var agent, binary string
	opts := mcpconfig.Options{Transport: "stdio"}
	cmd := &cobra.Command{
		Use: "install", Short: "Install roborev MCP configuration for coding agents", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			selected := strings.ToLower(strings.TrimSpace(agent))
			if opts.ConfigPath != "" && (selected == "" || selected == "all") {
				return fmt.Errorf("--config requires one --agent")
			}
			if err := mcpconfig.Validate(opts); err != nil {
				return err
			}
			if opts.Transport == "stdio" {
				resolved, err := githook.ResolveRoborevPath(binary)
				if err != nil {
					return err
				}
				opts.Executable = resolved.Path
			}
			opts.Server = serverAddr
			agents := skills.Agents()
			if selected != "" && selected != "all" {
				agents = []skills.Agent{skills.Agent(selected)}
			}
			var errs []error
			for _, a := range agents {
				if selected == "" {
					dir, err := skills.ConfigDir(a)
					if err != nil {
						return err
					}
					info, err := os.Stat(dir)
					if os.IsNotExist(err) {
						continue
					}
					if err != nil {
						return err
					}
					if !info.IsDir() {
						continue
					}
				}
				opts.Agent = a
				result, err := mcpconfig.Install(opts)
				if err != nil {
					errs = append(errs, fmt.Errorf("%s: %w", a, err))
					continue
				}
				if opts.DryRun {
					fmt.Fprintf(cmd.OutOrStdout(), "%s: %s\n%s\n", a, result.Path, result.Data)
				} else {
					fmt.Fprintf(cmd.OutOrStdout(), "%s: MCP configured in %s\n", a, result.Path)
				}
			}
			return errors.Join(errs...)
		},
	}
	cmd.Flags().StringVar(&agent, "agent", "", "agent to configure; default detects config directories, all selects every agent")
	cmd.Flags().StringVar(&opts.ConfigPath, "config", "", "MCP config file for one selected agent")
	cmd.Flags().StringVar(&opts.Transport, "transport", "stdio", "MCP transport: stdio or http")
	cmd.Flags().StringVar(&opts.URL, "url", "", "existing daemon /mcp endpoint for HTTP transport")
	cmd.Flags().StringVar(&binary, "binary", "", "installed roborev executable for stdio")
	cmd.Flags().BoolVar(&opts.DryRun, "dry-run", false, "print merged configuration without writing")
	return cmd
}
