package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"go.kenn.io/roborev/internal/agent"
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
			names := agent.Available()
			sort.Strings(names)

			timeout := time.Duration(timeoutSecs) * time.Second
			smokePrompt := "Respond with exactly: OK"
			if largePrompt {
				smokePrompt = "Respond with exactly: OK\n" +
					strings.Repeat("// padding line\n", 2200)
			}

			// Use current directory as repo path for the smoke test
			repoPath, err := os.Getwd()
			if err != nil {
				repoPath = "."
			}

			var passed, failed, skipped int

			for _, name := range names {
				if name == "test" {
					continue
				}
				if agentFilter != "" && name != agentFilter {
					continue
				}

				if !agent.IsAvailable(name) {
					a, _ := agent.Get(name)
					cmdName := ""
					if a != nil {
						if ca, ok := a.(agent.CommandAgent); ok {
							cmdName = ca.CommandName()
						}
					}
					fmt.Printf("  - %-14s %s (not found in PATH)\n", name, cmdName)
					skipped++
					continue
				}

				a, _ := agent.GetAvailable(name)
				if a == nil {
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
