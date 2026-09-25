package githook

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	gitcmd "go.kenn.io/kit/git/cmd"
	gitrepo "go.kenn.io/kit/git/repo"
)

// runner shells out through kit's defensive git runner so inherited git
// environment variables do not affect repository discovery.
var runner = gitcmd.New()

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
	for _, hookName := range []string{"post-commit", "post-rewrite", "pre-push"} {
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

// HooksInsideGitDir reports whether the repo's effective hooks directory and
// managed hook symlink targets lie inside its git or common git directory — the
// layout roborev's own hook installs use, including linked worktrees. Any
// other location (core.hooksPath into a working tree, an external shared
// hooks directory) may hold tracked or user-managed files, so background
// processes and automatic CLI maintenance must only write hooks when this
// reports true. Explicit installation and repair may opt into other locations.
func HooksInsideGitDir(ctx context.Context, repoPath string) (bool, error) {
	hooksDir, err := gitrepo.HooksPath(ctx, repoPath)
	if err != nil {
		return false, fmt.Errorf("get hooks path: %w", err)
	}
	gitDir, err := gitrepo.GitDir(ctx, repoPath)
	if err != nil {
		return false, fmt.Errorf("get git dir: %w", err)
	}
	// Git reports some paths physically (--git-path resolves symlinks) and
	// others logically, so canonicalize before comparing.
	gitDir = canonicalizePath(gitDir)
	commonDir, err := gitCommonDir(ctx, repoPath)
	if err != nil {
		return false, fmt.Errorf("get git common dir: %w", err)
	}
	commonDir = canonicalizePath(commonDir)
	inside := func(path string) bool {
		return isPathWithin(path, gitDir) || isPathWithin(path, commonDir)
	}
	if !inside(canonicalizePath(hooksDir)) {
		return false, nil
	}
	for _, name := range []string{"post-commit", "post-rewrite", "pre-push"} {
		path := filepath.Join(hooksDir, name)
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return false, fmt.Errorf("inspect %s hook: %w", name, err)
		}
		if info.Mode()&os.ModeSymlink == 0 {
			continue
		}
		resolved, err := filepath.EvalSymlinks(path)
		// A dangling link can create an outside file during companion install.
		// Leave unresolved links to explicit maintenance as well.
		if err != nil || !inside(resolved) {
			return false, nil
		}
	}
	return true, nil
}

// canonicalizePath resolves symlinks in the longest existing prefix of path
// and rejoins the remainder, so paths that do not fully exist yet (for
// example a hooks dir that was never created) still canonicalize.
func canonicalizePath(path string) string {
	remainder := ""
	for current := path; ; {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			return filepath.Join(resolved, remainder)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return path
		}
		remainder = filepath.Join(filepath.Base(current), remainder)
		current = parent
	}
}

// gitCommonDir returns the absolute path of the repository's common git
// directory, resolving relative rev-parse output the same way kit's
// gitrepo.GitDir does.
func gitCommonDir(ctx context.Context, repoPath string) (string, error) {
	out, err := runner.Output(ctx, repoPath, "rev-parse", "--git-common-dir")
	if err != nil {
		return "", fmt.Errorf("git rev-parse --git-common-dir: %w", err)
	}
	dir := gitrepo.NormalizePath(string(out))
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(repoPath, dir)
	}
	return filepath.Clean(dir), nil
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
