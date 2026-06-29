package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	gitrepo "go.kenn.io/kit/git/repo"

	"go.kenn.io/roborev/internal/githook"
)

func installHookCmd() *cobra.Command {
	var force bool
	var hookBinary string

	cmd := &cobra.Command{
		Use:   "install-hook",
		Short: "Install post-commit hook in current repository",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			root, err := gitrepo.Root(ctx, ".")
			if err != nil {
				return fmt.Errorf("not a git repository: %w", err)
			}

			if err := gitrepo.EnsureAbsoluteHooksPath(ctx, root); err != nil {
				return fmt.Errorf("normalize hooks path: %w", err)
			}
			hooksDir, err := gitrepo.HooksPath(ctx, root)
			if err != nil {
				return fmt.Errorf("get hooks path: %w", err)
			}

			if err := os.MkdirAll(hooksDir, 0o755); err != nil {
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
	cmd.AddCommand(installHookRepairCmd())

	return cmd
}

func uninstallHookCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "uninstall-hook",
		Short: "Remove roborev hooks from current repository",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			root, err := gitrepo.Root(ctx, ".")
			if err != nil {
				return fmt.Errorf("not a git repository: %w", err)
			}

			hooksDir, err := gitrepo.HooksPath(ctx, root)
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
