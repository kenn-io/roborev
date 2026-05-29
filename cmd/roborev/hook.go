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
	var hookBinary string

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

			if err := os.MkdirAll(hooksDir, 0755); err != nil {
				return fmt.Errorf("create hooks directory: %w", err)
			}

			binaryResolution, err := githook.ResolveRoborevPath(hookBinary)
			if err != nil {
				return fmt.Errorf("resolve hook binary: %w", err)
			}
			if binaryResolution.Notice != "" {
				fmt.Println(binaryResolution.Notice)
			}
			return githook.InstallAllWithOptions(hooksDir, githook.InstallOptions{
				Force:      force,
				BinaryPath: binaryResolution.Path,
			})
		},
	}

	cmd.Flags().BoolVar(&force, "force", false, "overwrite existing hook")
	cmd.Flags().StringVar(&hookBinary, "binary", "", "roborev binary path to bake into git hooks (for version-manager shims)")

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
