package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	gitrepo "go.kenn.io/kit/git/repo"

	"go.kenn.io/roborev/internal/githook"
	"go.kenn.io/roborev/internal/storage"
)

type repairHookOptions struct {
	current    bool
	registered bool
	binary     string
	out        io.Writer
}

func installHookRepairCmd() *cobra.Command {
	opts := repairHookOptions{out: os.Stdout}

	cmd := &cobra.Command{
		Use:   "repair",
		Short: "Repair existing roborev-managed git hooks",
		RunE: func(cmd *cobra.Command, args []string) error {
			if !opts.registered {
				opts.current = true
			}
			return repairHooks(cmd.Context(), opts)
		},
	}

	cmd.Flags().BoolVar(&opts.registered, "registered", false, "repair hooks in all registered repositories")
	cmd.Flags().StringVar(&opts.binary, "binary", "", "roborev binary path to bake into git hooks")

	return cmd
}

func repairHooks(ctx context.Context, opts repairHookOptions) error {
	out := opts.out
	if out == nil {
		out = io.Discard
	}

	resolution, err := githook.ResolveRoborevPath(opts.binary)
	if err != nil {
		return fmt.Errorf("resolve hook binary: %w", err)
	}
	if resolution.Notice != "" {
		fmt.Fprintln(out, resolution.Notice)
	}

	roots, err := hookRepairRoots(ctx, opts)
	if err != nil {
		return err
	}
	if len(roots) == 0 {
		fmt.Fprintln(out, "No repositories to repair")
		return nil
	}

	var reconciled int
	var warnings []error
	for _, root := range roots {
		found, err := repairManagedRepoHooks(ctx, root, resolution.Path)
		if err != nil {
			warnings = append(warnings, fmt.Errorf("%s: %w", root, err))
			continue
		}
		if found {
			reconciled++
		}
	}

	for _, warning := range warnings {
		fmt.Fprintf(out, "Warning: failed to repair hooks for %v\n", warning)
	}
	if len(warnings) > 0 {
		return errors.Join(warnings...)
	}
	if reconciled == 0 {
		fmt.Fprintln(out, "No managed hooks found")
		return nil
	}
	fmt.Fprintf(out, "Repaired hooks in %d repo(s)\n", reconciled)
	return nil
}

func hookRepairRoots(ctx context.Context, opts repairHookOptions) ([]string, error) {
	seen := map[string]struct{}{}
	var roots []string
	add := func(root string) {
		if root == "" {
			return
		}
		clean := filepath.Clean(root)
		if _, ok := seen[clean]; ok {
			return
		}
		seen[clean] = struct{}{}
		roots = append(roots, clean)
	}

	if opts.current {
		if root, err := gitrepo.Root(ctx, "."); err == nil {
			add(root)
		}
	}

	if opts.registered {
		registered, err := registeredHookRepos()
		if err != nil {
			return nil, err
		}
		for _, root := range registered {
			add(root)
		}
	}

	return roots, nil
}

func registeredHookRepos() ([]string, error) {
	dbPath := storage.DefaultDBPath()
	if _, err := os.Stat(dbPath); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("stat repo database: %w", err)
	}

	db, err := storage.Open(dbPath)
	if err != nil {
		return nil, fmt.Errorf("open repo database: %w", err)
	}
	defer db.Close()

	repos, err := db.ListRepos()
	if err != nil {
		return nil, fmt.Errorf("list repos: %w", err)
	}

	roots := make([]string, 0, len(repos))
	for _, repo := range repos {
		roots = append(roots, repo.RootPath)
	}
	return roots, nil
}

func repairManagedRepoHooks(ctx context.Context, repoPath, binaryPath string) (bool, error) {
	root, err := gitrepo.Root(ctx, repoPath)
	if err != nil {
		return false, nil
	}
	hooksDir, err := gitrepo.HooksPath(ctx, root)
	if err != nil {
		return false, fmt.Errorf("get hooks path: %w", err)
	}

	var found bool
	var errs []error
	for _, hookName := range []string{"post-commit", "post-rewrite"} {
		managed, err := hookFileHasRoborevMarker(filepath.Join(hooksDir, hookName), hookName)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s hook: %w", hookName, err))
			continue
		}
		if !managed {
			continue
		}
		found = true
		if err := githook.InstallWithOptions(hooksDir, hookName, githook.InstallOptions{
			BinaryPath: binaryPath,
		}); err != nil {
			errs = append(errs, err)
		}
	}

	return found, errors.Join(errs...)
}

func hookFileHasRoborevMarker(path, hookName string) (bool, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return strings.Contains(
		strings.ToLower(string(content)),
		"# roborev "+hookName+" hook",
	), nil
}
