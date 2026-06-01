package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	gitrepo "go.kenn.io/kit/git/repo"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/storage"
)

// resolveRepoIdentifier resolves a path-like identifier to its git repo root.
// If the identifier is ".", "..", or an existing path on disk, it will try to
// resolve it to the git repository root. This allows display names like "org/project"
// to be treated as names rather than paths.
func resolveRepoIdentifier(ctx context.Context, identifier string) string {
	// Special cases that are always paths
	if identifier == "." || identifier == ".." ||
		strings.HasPrefix(identifier, "./") ||
		strings.HasPrefix(identifier, "../") {
		return resolvePathToGitRoot(ctx, identifier)
	}

	// Check if it's an absolute path (works on both Unix and Windows)
	if filepath.IsAbs(identifier) {
		return resolvePathToGitRoot(ctx, identifier)
	}

	// For identifiers containing path separators (/ or \), check if they exist on disk.
	// This allows names like "org/project" to be treated as names, not paths.
	// Since explicit path prefixes (./, ../, absolute) are handled above, these are
	// ambiguous - only treat as path if the path actually exists and is accessible.
	if strings.ContainsAny(identifier, "/\\") {
		if _, err := os.Stat(identifier); err == nil {
			// Path exists on disk, treat as path
			return resolvePathToGitRoot(ctx, identifier)
		}
		// Path doesn't exist or isn't accessible (permission denied, etc.)
		// Treat as a name since user didn't use explicit path syntax
		return identifier
	}

	// No path separators, treat as a name
	return identifier
}

// resolvePathToGitRoot resolves a filesystem path to its git repository root.
// If not in a git repo, returns the absolute path.
func resolvePathToGitRoot(ctx context.Context, path string) string {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return path
	}

	repoRoot, err := gitrepo.Root(ctx, absPath)
	if err != nil {
		// Not a git repo or git error, fall back to absolute path
		return absPath
	}

	return repoRoot
}

// resolveRepoFlag resolves a --repo flag value to the main repo root.
// An empty or "." value resolves to the current directory's repo.
// Used by tui and review commands that accept --repo with NoOptDefVal=".".
func resolveRepoFlag(ctx context.Context, path string) (string, error) {
	if path == "" || path == "." {
		path = "."
	}
	root, err := gitrepo.MainRoot(ctx, path)
	if err != nil || root == "" {
		return "", fmt.Errorf("not inside a git repository")
	}
	return root, nil
}

// resolveBranchFlag resolves a --branch flag value to a branch name.
// "HEAD" (the NoOptDefVal) resolves to the current branch. Any other value
// is returned as-is. Used by tui and review commands that accept --branch
// with NoOptDefVal="HEAD".
func resolveBranchFlag(ctx context.Context, value, repoPath string) (string, error) {
	if value == "HEAD" {
		branch := gitrepo.CurrentBranch(ctx, repoPath)
		if branch == "" {
			return "", fmt.Errorf("could not detect current branch")
		}
		return branch, nil
	}
	return value, nil
}

func repoCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "repo",
		Short: "Manage repositories in the roborev database",
		Long: `Manage repositories tracked by roborev.

Subcommands:
  list    - List all repositories with their review counts
  show    - Show details about a specific repository
  rename  - Rename a repository's display name
  move    - Update a repository's root path (after directory rename)
  delete  - Remove a repository from tracking
  merge   - Merge reviews from one repository into another
`,
	}

	cmd.AddCommand(repoListCmd())
	cmd.AddCommand(repoShowCmd())
	cmd.AddCommand(repoRenameCmd())
	cmd.AddCommand(repoMoveCmd())
	cmd.AddCommand(repoDeleteCmd())
	cmd.AddCommand(repoMergeCmd())

	return cmd
}

func repoListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List all repositories",
		Long: `List all repositories tracked by roborev with their review counts.

Shows the display name, path, and number of reviews for each repository.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			dbPath := storage.DefaultDBPath()
			if dbPath == "" {
				return fmt.Errorf("cannot determine database path")
			}

			db, err := storage.Open(dbPath)
			if err != nil {
				return fmt.Errorf("open database: %w", err)
			}
			defer db.Close()

			repos, total, err := db.ListReposWithReviewCounts()
			if err != nil {
				return fmt.Errorf("list repos: %w", err)
			}

			if len(repos) == 0 {
				fmt.Println("No repositories found")
				return nil
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintf(w, "NAME\tPATH\tREVIEWS\n")
			for _, r := range repos {
				fmt.Fprintf(w, "%s\t%s\t%d\n", r.Name, r.RootPath, r.Count)
			}
			w.Flush()

			fmt.Printf("\nTotal: %d repositories, %d reviews\n", len(repos), total)
			return nil
		},
	}
}

func repoShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <path-or-name>",
		Short: "Show details about a repository",
		Long: `Show detailed information about a repository including stats.

The argument can be either the repository path or its display name.
When given a path inside a repository, it resolves to the repo root.

Examples:
  roborev repo show my-project
  roborev repo show /path/to/project
  roborev repo show .
`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			identifier := resolveRepoIdentifier(cmd.Context(), args[0])

			dbPath := storage.DefaultDBPath()
			if dbPath == "" {
				return fmt.Errorf("cannot determine database path")
			}

			db, err := storage.Open(dbPath)
			if err != nil {
				return fmt.Errorf("open database: %w", err)
			}
			defer db.Close()

			repo, err := db.FindRepo(identifier)
			if err != nil {
				return fmt.Errorf("repository not found: %s", identifier)
			}

			stats, err := db.GetRepoStats(repo.ID)
			if err != nil {
				return fmt.Errorf("get stats: %w", err)
			}

			fmt.Printf("Repository: %s\n", stats.Repo.Name)
			fmt.Printf("Path:       %s\n", stats.Repo.RootPath)
			fmt.Printf("Created:    %s\n", stats.Repo.CreatedAt.Format("2006-01-02 15:04:05"))
			fmt.Println()
			fmt.Printf("Jobs:       %d total\n", stats.TotalJobs)
			if stats.QueuedJobs > 0 {
				fmt.Printf("  Queued:   %d\n", stats.QueuedJobs)
			}
			if stats.RunningJobs > 0 {
				fmt.Printf("  Running:  %d\n", stats.RunningJobs)
			}
			fmt.Printf("  Done:     %d\n", stats.CompletedJobs)
			if stats.FailedJobs > 0 {
				fmt.Printf("  Failed:   %d\n", stats.FailedJobs)
			}
			fmt.Println()
			fmt.Printf("Reviews:    %d total\n", stats.ClosedReviews+stats.OpenReviews)
			fmt.Printf("  Passed:      %d\n", stats.PassedReviews)
			fmt.Printf("  Failed:      %d\n", stats.FailedReviews)
			fmt.Printf("  Closed:      %d\n", stats.ClosedReviews)
			fmt.Printf("  Open:        %d\n", stats.OpenReviews)

			return nil
		},
	}
}

func repoRenameCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rename <path-or-name> <new-name>",
		Short: "Rename a repository's display name",
		Long: `Rename a repository's display name in the roborev database.

This changes the name stored in the database, which is shown in the TUI
and CLI output. It does NOT affect the filesystem or git repository.

NOTE: This is different from the display_name setting in .roborev.toml.
The database name is set once when a repo is first tracked (from the
directory name). Use this command to change it after the fact.

When to use rename vs move vs merge:
  - Use RENAME when you have ONE repo entry and want a different name
  - Use MOVE when you renamed/moved the directory on disk
  - Use MERGE when you have TWO repo entries that should be combined
    (e.g., after renaming a directory, you'll have both old and new entries)

The first argument can be either:
  - The repository path (absolute or relative, resolves to repo root)
  - The current database name

Examples:
  # Give a friendlier name to the current directory's repo
  roborev repo rename . my-project

  # Rename by current database name
  roborev repo rename old-name new-name

  # Rename by path
  roborev repo rename /path/to/project better-name
`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			identifier := resolveRepoIdentifier(cmd.Context(), args[0])
			newName := args[1]

			if newName == "" {
				return fmt.Errorf("new name cannot be empty")
			}

			dbPath := storage.DefaultDBPath()
			if dbPath == "" {
				return fmt.Errorf("cannot determine database path")
			}

			db, err := storage.Open(dbPath)
			if err != nil {
				return fmt.Errorf("open database: %w", err)
			}
			defer db.Close()

			affected, err := db.RenameRepo(identifier, newName)
			if err != nil {
				return fmt.Errorf("rename repo: %w", err)
			}

			if affected == 0 {
				return fmt.Errorf("no repository found matching %q", identifier)
			}

			fmt.Printf("Renamed repository to %q\n", newName)

			// If the renamed repo's stored root_path no longer exists on disk,
			// hint the user about `repo move` so they know how to recover.
			if repo, err := db.GetRepoByName(newName); err == nil {
				if _, statErr := os.Stat(repo.RootPath); errors.Is(statErr, os.ErrNotExist) {
					fmt.Fprintf(os.Stderr,
						"\nNote: %s no longer exists on disk.\n"+
							"If the directory was moved, run:\n"+
							"  roborev repo move %q <new-path>\n",
						repo.RootPath, newName)
				}
			}
			return nil
		},
	}
}

func repoMoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "move <name-or-path> <new-path>",
		Short: "Update a repository's root path",
		Long: `Update the root_path stored for a repository in the roborev database.

Use this when a repository's directory has been renamed or moved on disk.
After moving, jobs continue to be associated with the same repo entry,
so reviews remain visible.

When to use move vs rename vs merge:
  - Use MOVE when the directory was renamed/moved on disk
  - Use RENAME to change the display name only
  - Use MERGE when you have TWO repo entries that should be combined

The first argument can be either:
  - The current database name
  - The current (or stale) repository path
The second argument is the new path. "." resolves to the current
directory's git repository root.

Examples:
  # After 'mv old-project new-project', update the database from inside the new directory
  roborev repo move old-project .

  # Specify an explicit new path
  roborev repo move my-repo /Users/me/code/my-repo

  # Identify the source by its old (now stale) path
  roborev repo move /old/path /new/path
`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			identifier := resolveRepoIdentifier(cmd.Context(), args[0])
			newPathArg := args[1]

			newPath, err := resolveMoveTargetPath(cmd.Context(), newPathArg)
			if err != nil {
				return err
			}

			dbPath := storage.DefaultDBPath()
			if dbPath == "" {
				return fmt.Errorf("cannot determine database path")
			}

			db, err := storage.Open(dbPath)
			if err != nil {
				return fmt.Errorf("open database: %w", err)
			}
			defer db.Close()

			repo, err := db.FindRepo(identifier)
			if err != nil {
				return fmt.Errorf("repository not found: %s", identifier)
			}

			// Recompute identity for the new location. For repos with a git remote
			// the identity stays stable; for local-only repos it changes to
			// local://<new-path>.
			newIdentity := config.ResolveRepoIdentity(newPath, nil)

			if err := db.MoveRepo(repo.ID, newPath, newIdentity); err != nil {
				if errors.Is(err, storage.ErrRepoPathConflict) {
					return fmt.Errorf("another repository is already at %s; consider 'roborev repo merge' to combine them", newPath)
				}
				return fmt.Errorf("move repo: %w", err)
			}

			fmt.Printf("Moved repository %q to %s\n", repo.Name, newPath)
			return nil
		},
	}
}

// resolveMoveTargetPath resolves the second argument to roborev repo move.
// It accepts:
//   - "." or relative paths starting with ./ or ../
//   - absolute paths
//   - bare identifiers (treated as relative to cwd)
//
// If the resolved path is inside a git repository, the repo root is returned.
func resolveMoveTargetPath(ctx context.Context, p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", fmt.Errorf("resolve path: %w", err)
	}
	if root, err := gitrepo.Root(ctx, abs); err == nil && root != "" {
		return filepath.ToSlash(root), nil
	}
	return filepath.ToSlash(abs), nil
}

func repoDeleteCmd() *cobra.Command {
	var cascade bool
	var yes bool

	cmd := &cobra.Command{
		Use:   "delete <path-or-name>",
		Short: "Remove a repository from tracking",
		Long: `Remove a repository from the roborev database.

By default, this only removes the repository entry. Use --cascade to also
delete all associated jobs, reviews, and responses.

The argument can be either the repository path or its display name.
When given a path inside a repository, it resolves to the repo root.

Examples:
  roborev repo delete old-project
  roborev repo delete --cascade /path/to/deleted-project
  roborev repo delete --cascade --yes old-project
`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			identifier := resolveRepoIdentifier(cmd.Context(), args[0])

			dbPath := storage.DefaultDBPath()
			if dbPath == "" {
				return fmt.Errorf("cannot determine database path")
			}

			db, err := storage.Open(dbPath)
			if err != nil {
				return fmt.Errorf("open database: %w", err)
			}
			defer db.Close()

			repo, err := db.FindRepo(identifier)
			if err != nil {
				return fmt.Errorf("repository not found: %s", identifier)
			}

			// Get stats to show what will be deleted
			stats, err := db.GetRepoStats(repo.ID)
			if err != nil {
				return fmt.Errorf("get stats: %w", err)
			}

			// Confirm deletion
			if !yes {
				fmt.Printf("Repository: %s (%s)\n", repo.Name, repo.RootPath)
				fmt.Printf("Jobs: %d\n", stats.TotalJobs)
				if cascade {
					fmt.Printf("\nThis will delete the repository AND all %d jobs/reviews.\n", stats.TotalJobs)
				} else if stats.TotalJobs > 0 {
					fmt.Printf("\nThis repository has %d jobs. Use --cascade to delete them too.\n", stats.TotalJobs)
					fmt.Println("Without --cascade, deletion will fail if jobs exist.")
				}
				fmt.Print("\nProceed? [y/N] ")
				var response string
				_, _ = fmt.Scanln(&response)
				if response != "y" && response != "Y" && response != "yes" {
					fmt.Println("Cancelled")
					return nil
				}
			}

			if err := db.DeleteRepo(repo.ID, cascade); err != nil {
				if errors.Is(err, storage.ErrRepoHasJobs) {
					return fmt.Errorf("cannot delete repository with existing jobs (use --cascade)")
				}
				return fmt.Errorf("delete repo: %w", err)
			}

			if cascade {
				fmt.Printf("Deleted repository %q and %d jobs\n", repo.Name, stats.TotalJobs)
			} else {
				fmt.Printf("Deleted repository %q\n", repo.Name)
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&cascade, "cascade", false, "also delete all jobs, reviews, and responses")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "skip confirmation prompt")

	return cmd
}

func repoMergeCmd() *cobra.Command {
	var yes bool

	cmd := &cobra.Command{
		Use:   "merge <source> <target>",
		Short: "Merge reviews from one repository into another",
		Long: `Merge all reviews from one repository into another.

This MOVES all jobs/reviews from the source repo into the target repo,
then DELETES the source repo entry. The target repo's name is preserved.

Common use case - directory was renamed:
  When you rename a directory (e.g., "old-project" -> "new-project"),
  roborev creates a new repo entry for the new path. You end up with:
    - "old-project" (orphaned, path no longer exists, has old reviews)
    - "new-project" (active, has new reviews)

  Use merge to combine them:
    roborev repo merge old-project new-project

  Result: "new-project" now has all reviews from both entries.

When to use merge vs rename:
  - Use MERGE when you have TWO repo entries that should be combined
  - Use RENAME when you have ONE repo entry and just want a different name

Arguments can be either repository paths or database names.
When given a path inside a repository, it resolves to the repo root.
Use 'roborev repo list' to see all tracked repositories.

Examples:
  # After renaming directory from old-project to new-project
  roborev repo merge old-project new-project

  # Consolidate repos tracked under different paths (symlinks, etc.)
  roborev repo merge /old/path /current/path

  # Skip confirmation prompt
  roborev repo merge --yes old-name new-name
`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			sourceIdent := resolveRepoIdentifier(cmd.Context(), args[0])
			targetIdent := resolveRepoIdentifier(cmd.Context(), args[1])

			dbPath := storage.DefaultDBPath()
			if dbPath == "" {
				return fmt.Errorf("cannot determine database path")
			}

			db, err := storage.Open(dbPath)
			if err != nil {
				return fmt.Errorf("open database: %w", err)
			}
			defer db.Close()

			source, err := db.FindRepo(sourceIdent)
			if err != nil {
				return fmt.Errorf("source repository not found: %s", sourceIdent)
			}

			target, err := db.FindRepo(targetIdent)
			if err != nil {
				return fmt.Errorf("target repository not found: %s", targetIdent)
			}

			if source.ID == target.ID {
				return fmt.Errorf("source and target are the same repository")
			}

			// Get stats
			sourceStats, err := db.GetRepoStats(source.ID)
			if err != nil {
				return fmt.Errorf("get source stats: %w", err)
			}

			// Confirm
			if !yes {
				fmt.Printf("Source: %s (%d jobs)\n", source.Name, sourceStats.TotalJobs)
				fmt.Printf("Target: %s\n", target.Name)
				fmt.Printf("\nThis will move %d jobs to %q and delete %q.\n",
					sourceStats.TotalJobs, target.Name, source.Name)
				fmt.Print("\nProceed? [y/N] ")
				var response string
				_, _ = fmt.Scanln(&response)
				if response != "y" && response != "Y" && response != "yes" {
					fmt.Println("Cancelled")
					return nil
				}
			}

			moved, err := db.MergeRepos(source.ID, target.ID)
			if err != nil {
				return fmt.Errorf("merge repos: %w", err)
			}

			fmt.Printf("Merged %d jobs from %q into %q\n", moved, source.Name, target.Name)
			return nil
		},
	}

	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "skip confirmation prompt")

	return cmd
}
