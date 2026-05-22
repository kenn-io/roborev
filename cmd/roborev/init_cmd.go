package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/git"
	"go.kenn.io/roborev/internal/githook"
)

func initCmd() *cobra.Command {
	var agent string
	var noDaemon bool

	cmd := &cobra.Command{
		Use:   "init",
		Short: "Initialize roborev in current repository",
		Long: `Initialize roborev with a single command:
  - Creates ~/.roborev/ global config directory
  - Creates .roborev.toml in repo (if --agent specified)
  - Installs post-commit hook
  - Starts the daemon (unless --no-daemon)`,
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Println("Initializing roborev...")

			// 1. Ensure we're in a git repo
			root, err := git.GetRepoRoot(".")
			if err != nil {
				return fmt.Errorf("not a git repository - run this from inside a git repo")
			}

			// 2. Create config directory and default config
			configDir := config.DataDir()
			if err := os.MkdirAll(configDir, 0755); err != nil {
				return fmt.Errorf("create config dir: %w", err)
			}

			configPath := config.GlobalConfigPath()
			if _, err := os.Stat(configPath); os.IsNotExist(err) {
				cfg := config.DefaultConfig()
				if agent != "" {
					cfg.DefaultAgent = agent
				}
				if err := config.SaveGlobal(cfg); err != nil {
					return fmt.Errorf("save config: %w", err)
				}
				fmt.Printf("  Created config at %s\n", configPath)
			} else {
				fmt.Printf("  Config already exists at %s\n", configPath)
			}

			// 3. Create per-repo config if agent specified
			repoConfigPath := filepath.Join(root, ".roborev.toml")
			if agent != "" {
				if _, err := os.Stat(repoConfigPath); os.IsNotExist(err) {
					repoConfig := &config.RepoConfig{Agent: agent}
					if err := config.SaveRepoConfigTo(repoConfigPath, repoConfig); err != nil {
						return fmt.Errorf("create repo config: %w", err)
					}
					fmt.Printf("  Created %s\n", repoConfigPath)
				}
			}

			// 4. Ensure the repo-local snapshot directory stays untracked.
			if err := ensureSnapshotDirIgnored(root); err != nil {
				return fmt.Errorf("ensure snapshot dir gitignored: %w", err)
			}

			// 5. Install hooks (post-commit + post-rewrite)
			if err := git.EnsureAbsoluteHooksPath(root); err != nil {
				return fmt.Errorf("normalize hooks path: %w", err)
			}
			hooksDir, err := git.GetHooksPath(root)
			if err != nil {
				return fmt.Errorf("get hooks path: %w", err)
			}
			if err := os.MkdirAll(hooksDir, 0755); err != nil {
				return fmt.Errorf("create hooks directory: %w", err)
			}
			if err := githook.InstallAll(hooksDir, false); err != nil {
				if githook.HasRealErrors(err) {
					return fmt.Errorf("install hooks: %w", err)
				}
				fmt.Printf("  Warning: %v\n", err)
			}

			// 6. Start daemon (or just register if --no-daemon)
			var initIncomplete bool
			if noDaemon {
				// Try to register with an already-running daemon, but don't start one
				if err := registerRepo(root); err != nil {
					initIncomplete = true
					if isTransportError(err) {
						fmt.Println("  Daemon not running (use 'roborev daemon start' or systemctl)")
					} else {
						fmt.Printf("  Warning: failed to register repo: %v\n", err)
					}
				} else {
					fmt.Println("  Repo registered with running daemon")
				}
			} else if err := ensureDaemon(); err != nil {
				initIncomplete = true
				fmt.Printf("  Warning: %v\n", err)
				fmt.Println("  Run 'roborev daemon start' to start manually")
			} else {
				fmt.Println("  Daemon is running")
				if err := registerRepo(root); err != nil {
					initIncomplete = true
					fmt.Printf("  Warning: failed to register repo: %v\n", err)
				} else {
					fmt.Println("  Repo registered")
				}
			}

			// 7. Success message
			fmt.Println()
			if initIncomplete {
				fmt.Println("Setup incomplete: repo was not registered with the daemon.")
				fmt.Println("Start the daemon and run 'roborev init' again, or register manually.")
			} else {
				fmt.Println("Ready! Every commit will now be automatically reviewed.")
			}
			fmt.Println()
			fmt.Println("Commands:")
			fmt.Println("  roborev status      - view queue and daemon status")
			fmt.Println("  roborev show HEAD   - view review for a commit")
			fmt.Println("  roborev tui         - interactive terminal UI")

			return nil
		},
	}

	cmd.Flags().StringVar(&agent, "agent", "", "default agent (codex, claude-code, gemini, copilot, opencode, cursor, kiro, kilo)")
	cmd.Flags().BoolVar(&noDaemon, "no-daemon", false, "skip auto-starting daemon (useful with systemd/launchd)")
	registerAgentCompletion(cmd)

	cmd.AddCommand(ghActionCmd())

	return cmd
}

func ensureSnapshotDirIgnored(root string) error {
	snapshotDir, err := config.ResolveSnapshotDir(root)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(root, snapshotDir)
	if err != nil {
		return err
	}
	rel = filepath.Clean(rel)
	pattern := "/" + filepath.ToSlash(rel) + "/"
	probe := filepath.ToSlash(filepath.Join(rel, ".roborev-ignore-check"))
	// Respect broader existing rules, e.g. tmp/ or var/, before appending
	// roborev's explicit snapshot directory entry.
	ignored, err := gitCheckIgnoreNoIndex(root, probe)
	if err != nil {
		return err
	}
	if ignored {
		return nil
	}
	return appendGitignoreEntry(filepath.Join(root, ".gitignore"), pattern)
}

func gitCheckIgnoreNoIndex(root, path string) (bool, error) {
	cmd := exec.Command("git", "-C", root, "check-ignore", "--quiet", "--no-index", path)
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return false, err
}

func appendGitignoreEntry(path, pattern string) error {
	var prefix string
	if data, err := os.ReadFile(path); err == nil {
		text := string(data)
		for line := range strings.SplitSeq(text, "\n") {
			if strings.TrimSpace(line) == pattern {
				return nil
			}
		}
		if len(text) > 0 && !strings.HasSuffix(text, "\n") {
			prefix = "\n"
		}
	} else if !os.IsNotExist(err) {
		return err
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintf(f, "%s# roborev snapshots\n%s\n", prefix, pattern)
	return err
}
