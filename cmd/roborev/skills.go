package main

import (
	"fmt"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"go.kenn.io/roborev/internal/skills"
)

func skillsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "skills",
		Short: "Manage AI agent skills",
		Long:  "Install and manage roborev skills for AI agents (Claude Code, Codex, Factory Droid, Grok Build)",
		RunE: func(cmd *cobra.Command, args []string) error {
			available, err := skills.ListSkills()
			if err != nil {
				return fmt.Errorf("list skills: %w", err)
			}
			if len(available) == 0 {
				fmt.Println("No skills available.")
				return nil
			}

			statuses := skills.Status()

			// Build a map of agent -> skill -> state for quick lookup
			type agentLabel struct {
				agent  skills.Agent
				label  string
				prefix string
			}
			agents := []agentLabel{
				{skills.AgentClaude, "Claude Code", "/"},
				{skills.AgentCodex, "Codex", "$"},
				{skills.AgentDroid, "Factory Droid", "/"},
				{skills.AgentGrok, "Grok Build", "/"},
			}

			fmt.Println("Skills:")
			for _, s := range available {
				fmt.Printf("\n  %s\n", s.Name)
				if s.Description != "" {
					fmt.Printf("  %s\n", s.Description)
				}

				for _, a := range agents {
					if !slices.Contains(s.SupportedAgents, a.agent) {
						continue
					}

					var as *skills.AgentStatus
					for i := range statuses {
						if statuses[i].Agent == a.agent {
							as = &statuses[i]
							break
						}
					}

					state := skills.SkillMissing
					if as != nil {
						state = as.Skills[s.DirName]
					}

					var badge string
					switch state {
					case skills.SkillCurrent:
						badge = "installed"
					case skills.SkillOutdated:
						badge = "outdated"
					case skills.SkillMissing:
						if as != nil && !as.Available {
							badge = "no agent"
						} else {
							badge = "not installed"
						}
					}

					fmt.Printf("    %s %-12s  %s%s\n", a.label, "("+badge+")", a.prefix, s.Name)
				}
			}

			// Determine if any action is needed
			var needsInstall, needsUpdate bool
			for _, as := range statuses {
				if !as.Available {
					continue
				}
				for _, state := range as.Skills {
					if state == skills.SkillMissing {
						needsInstall = true
					}
					if state == skills.SkillOutdated {
						needsUpdate = true
					}
				}
			}

			if needsInstall || needsUpdate {
				fmt.Printf("\nRun 'roborev skills install' to install or update.\n")
			}

			return nil
		},
	}

	var installPath string
	var installAgent string
	installCmd := &cobra.Command{
		Use:   "install",
		Short: "Install roborev skills for AI agents",
		Long: `Install roborev skills to your AI agent configuration directories.

Skills are installed for agents whose config directories exist:
  - Claude Code: ~/.claude/skills/ (or $CLAUDE_CONFIG_DIR/skills/ if set)
  - Codex: ~/.codex/skills/ (or $CODEX_HOME/skills/ if set)
  - Factory Droid: ~/.factory/skills/
  - Grok Build: ~/.grok/skills/ (or $GROK_HOME/skills/ if set)

Use --path to install directly into a custom final skills directory. Custom
installs use the Claude variant by default; use --agent to select codex, droid, or grok.

This command is idempotent - running it multiple times is safe.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			pathChanged := cmd.Flags().Changed("path")
			agentChanged := cmd.Flags().Changed("agent")
			if agentChanged && !pathChanged {
				return fmt.Errorf("--agent requires --path")
			}

			var results []skills.InstallResult
			if pathChanged {
				if strings.TrimSpace(installPath) == "" {
					return fmt.Errorf("--path cannot be empty")
				}
				result, err := skills.InstallToPath(skills.Agent(installAgent), installPath)
				if err != nil {
					return err
				}
				results = []skills.InstallResult{result}
			} else {
				var err error
				results, err = skills.Install()
				if err != nil {
					return err
				}
			}

			// formatSkills formats skill names with the correct invocation prefix per agent
			// Claude uses /skill-name, Codex uses $skill-name
			formatSkills := func(agent skills.Agent, skillNames []string) string {
				prefix := "/"
				if agent == skills.AgentCodex {
					prefix = "$"
				}
				formatted := make([]string, len(skillNames))
				for i, name := range skillNames {
					formatted[i] = prefix + name
				}
				return strings.Join(formatted, ", ")
			}

			anyInstalled := false
			var installedAgents []skills.Agent
			for _, result := range results {
				if result.Skipped {
					fmt.Printf("%s: skipped (no %s directory)\n", result.Agent, result.ConfigDir)
					continue
				}

				if len(result.Installed) > 0 {
					anyInstalled = true
					installedAgents = append(installedAgents, result.Agent)
					fmt.Printf("%s: installed %s\n", result.Agent, formatSkills(result.Agent, result.Installed))
				}
				if len(result.Updated) > 0 {
					anyInstalled = true
					if len(result.Installed) == 0 {
						installedAgents = append(installedAgents, result.Agent)
					}
					fmt.Printf("%s: updated %s\n", result.Agent, formatSkills(result.Agent, result.Updated))
				}
			}

			if !anyInstalled {
				fmt.Println("\nNo agents found. Install Claude Code, Codex, Factory Droid, or Grok Build first, then run this command.")
			} else {
				fmt.Println("\nSkills installed! Try:")
				for _, agent := range installedAgents {
					switch agent {
					case skills.AgentClaude:
						fmt.Println("  Claude Code: /roborev-review, /roborev-review-branch, /roborev-design-review, /roborev-design-review-branch, /roborev-fix, /roborev-respond")
					case skills.AgentCodex:
						fmt.Println("  Codex: $roborev-review, $roborev-review-branch, $roborev-design-review, $roborev-design-review-branch, $roborev-fix, $roborev-respond")
					case skills.AgentDroid:
						fmt.Println("  Factory Droid: /roborev-review, /roborev-review-branch, /roborev-design-review, /roborev-design-review-branch, /roborev-lookahead-review, /roborev-lookahead-review-branch, /roborev-fix, /roborev-respond")
					case skills.AgentGrok:
						fmt.Println("  Grok Build: /roborev-review, /roborev-review-branch, /roborev-design-review, /roborev-design-review-branch, /roborev-lookahead-review, /roborev-lookahead-review-branch, /roborev-fix, /roborev-refine, /roborev-respond")
					}
				}
			}

			return nil
		},
	}

	installCmd.Flags().StringVar(&installPath, "path", "", "install directly into this final skills directory")
	installCmd.Flags().StringVar(&installAgent, "agent", string(skills.AgentClaude), "skill variant for --path (claude, codex, droid, or grok)")

	updateCmd := &cobra.Command{
		Use:   "update",
		Short: "Update roborev skills for agents that have them installed",
		Long: `Update roborev skills only for agents that already have them installed.

Unlike 'install', this command does NOT install skills for new agents -
it only updates existing installations. Used by 'roborev update'.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			results, err := skills.Update()
			if err != nil {
				return err
			}

			if len(results) == 0 {
				fmt.Println("No skills to update (none installed)")
				return nil
			}

			for _, result := range results {
				if len(result.Updated) > 0 {
					fmt.Printf("%s: updated %d skill(s)\n", result.Agent, len(result.Updated))
				}
				if len(result.Installed) > 0 {
					// This can happen if user had one skill but not the other
					fmt.Printf("%s: installed %d skill(s)\n", result.Agent, len(result.Installed))
				}
			}

			return nil
		},
	}

	cmd.AddCommand(installCmd)
	cmd.AddCommand(updateCmd)
	return cmd
}
