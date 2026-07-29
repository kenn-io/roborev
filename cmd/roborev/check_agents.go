package main

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
)

func checkAgentsCmd() *cobra.Command {
	var (
		timeoutSecs int
		agentFilter string
		largePrompt bool
	)

	cmd := &cobra.Command{
		Use:   "check-agents",
		Short: "Check which agents are available and responding",
		Long: `Check which agents are installed and can produce output.

For each agent found on PATH, runs a short smoke-test prompt with a timeout
to verify the agent is actually functional.

Examples:
  roborev check-agents                  # Check all agents
  roborev check-agents --agent codex    # Check only codex
  roborev check-agents --timeout 30     # 30 second timeout per agent
  roborev check-agents --large-prompt   # Test with 33KB+ prompt (Windows limit check)`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.LoadGlobal()
			if err != nil {
				return fmt.Errorf("load global config: %w", err)
			}

			repoCfg, repoPath, err := loadCommandRepoConfig(cmd)
			if err != nil {
				return fmt.Errorf("load repo config: %w", err)
			}

			names := agent.AvailableNamesFromConfig(repoCfg, cfg)

			timeout := time.Duration(timeoutSecs) * time.Second
			smokePrompt := "Respond with exactly: OK"
			if largePrompt {
				smokePrompt = "Respond with exactly: OK\n" +
					strings.Repeat("// padding line\n", 2200)
			}

			var passed, failed, skipped int

			for _, name := range names {
				if name == "test" {
					continue
				}
				if agentFilter != "" && name != agentFilter {
					continue
				}

				a, err := agent.GetAvailableExactWithConfigFromConfig(repoCfg, name, cfg)
				if err != nil {
					fmt.Printf("  - %-14s %s\n", name, err)
					skipped++
					continue
				}

				cmdName := ""
				if ca, ok := a.(agent.CommandAgent); ok {
					cmdName = ca.CommandName()
				}

				path, _ := exec.LookPath(cmdName)
				fmt.Printf("  ? %-14s %s (%s) ... ", name, cmdName, path)

				ctx, cancel := context.WithTimeout(context.Background(), timeout)
				result, err := a.Review(ctx, repoPath, "HEAD", smokePrompt, nil)
				cancel()

				if err != nil {
					fmt.Printf("FAIL\n")
					// Indent each line of the error for readability
					for line := range strings.SplitSeq(err.Error(), "\n") {
						line = strings.TrimSpace(line)
						if line != "" {
							fmt.Printf("    %s\n", line)
						}
					}
					failed++
				} else if strings.TrimSpace(result) == "" {
					fmt.Printf("FAIL (empty response)\n")
					failed++
				} else {
					fmt.Printf("OK (%d bytes)\n", len(result))
					passed++
				}
			}

			fmt.Printf("\n%d passed, %d failed, %d skipped\n", passed, failed, skipped)
			if failed > 0 {
				return fmt.Errorf("%d agent(s) failed health check", failed)
			}
			return nil
		},
	}

	cmd.Flags().IntVar(&timeoutSecs, "timeout", 60, "timeout in seconds per agent")
	cmd.Flags().StringVar(&agentFilter, "agent", "", "check only this agent")
	cmd.Flags().BoolVar(&largePrompt, "large-prompt", false,
		"use a 33KB+ prompt to test Windows command-line limits")

	return cmd
}
