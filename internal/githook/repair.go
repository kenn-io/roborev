package githook

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	gitrepo "go.kenn.io/kit/git/repo"
)

// RepairRepoHooks rewrites roborev-managed hooks in repoPath so they invoke
// binaryPath. Only hook files containing roborev marker comments are
// modified; unmanaged hooks are left alone. Returns whether any managed
// hooks were found. A repoPath that is not a git repository is not an
// error: it reports (false, nil) so callers can sweep registered repos
// that may have been deleted.
func RepairRepoHooks(ctx context.Context, repoPath, binaryPath string) (bool, error) {
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
		if err := InstallWithOptions(hooksDir, hookName, InstallOptions{
			BinaryPath: binaryPath,
		}); err != nil {
			errs = append(errs, err)
		}
	}

	return found, errors.Join(errs...)
}

// HookBinaryStale reports whether the repo's named hook is roborev-managed
// but bakes a binary path other than binaryPath. Read-only: callers that
// cannot rewrite the hook (e.g. the daemon when hooks live inside the
// working tree) use this to detect and warn about stale hooks that
// NeedsUpgrade misses because the version marker is current.
func HookBinaryStale(ctx context.Context, repoPath, hookName, binaryPath string) bool {
	hooksDir, err := gitrepo.HooksPath(ctx, repoPath)
	if err != nil {
		return false
	}
	content, err := os.ReadFile(filepath.Join(hooksDir, hookName))
	if err != nil {
		return false
	}
	s := string(content)
	if !strings.Contains(strings.ToLower(s), "# roborev "+hookName+" hook") {
		return false
	}
	return !hookUsesBinary(s, binaryPath)
}

// HooksDirInsideWorktree reports whether the repo's effective hooks
// directory resolves inside the working tree but outside the git dir
// (e.g. core.hooksPath = .githooks). Such directories may hold tracked
// files, so background processes must not write to them.
func HooksDirInsideWorktree(ctx context.Context, repoPath string) (bool, error) {
	root, err := gitrepo.Root(ctx, repoPath)
	if err != nil {
		return false, fmt.Errorf("resolve repo root: %w", err)
	}
	hooksDir, err := gitrepo.HooksPath(ctx, root)
	if err != nil {
		return false, fmt.Errorf("get hooks path: %w", err)
	}
	gitDir, err := gitrepo.GitDir(ctx, root)
	if err != nil {
		return false, fmt.Errorf("get git dir: %w", err)
	}
	if isPathWithin(hooksDir, gitDir) {
		return false, nil
	}
	return isPathWithin(hooksDir, root), nil
}

// isPathWithin reports whether path is dir or inside dir. Both paths must
// be absolute; comparison is lexical (no symlink resolution).
func isPathWithin(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
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
