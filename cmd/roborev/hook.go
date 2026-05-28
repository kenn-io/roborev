package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"go.kenn.io/roborev/internal/git"
	"go.kenn.io/roborev/internal/githook"
)

func installHookCmd() *cobra.Command {
	var force bool

	cmd := &cobra.Command{
		Use:   "install-hook",
		Short: "Install post-commit hook in current repository",
		RunE: func(cmd *cobra.Command, args []string) error {
			root, err := git.GetRepoRoot(".")
			if err != nil {
				return fmt.Errorf("not a git repository: %w", err)
			}

			if err := git.EnsureAbsoluteHooksPath(root); err != nil {
				return fmt.Errorf("normalize hooks path: %w", err)
			}
			hooksDir, err := git.GetHooksPath(root)
			if err != nil {
				return fmt.Errorf("get hooks path: %w", err)
			}

			if err := os.MkdirAll(hooksDir, 0o755); err != nil {
				return fmt.Errorf("create hooks directory: %w", err)
			}

			return githook.InstallAll(hooksDir, force)
		},
	}

	cmd.Flags().BoolVar(&force, "force", false, "overwrite existing hook")

	return cmd
}

func uninstallHookCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "uninstall-hook",
		Short: "Remove roborev hooks from current repository",
		RunE: func(cmd *cobra.Command, args []string) error {
			root, err := git.GetRepoRoot(".")
			if err != nil {
				return fmt.Errorf("not a git repository: %w", err)
			}

			hooksDir, err := git.GetHooksPath(root)
			if err != nil {
				return fmt.Errorf("get hooks path: %w", err)
			}

			for _, hookName := range []string{
				"post-commit", "post-rewrite",
			} {
				if err := githook.Uninstall(
					filepath.Join(hooksDir, hookName),
				); err != nil {
					return err
				}
			}

			return nil
		},
	}
}
