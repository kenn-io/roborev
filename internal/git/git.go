package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// normalizeMSYSPath converts MSYS-style paths (e.g., /c/Users/...) to Windows paths (C:\Users\...).
// On non-Windows systems, it just applies filepath.FromSlash.
func normalizeMSYSPath(path string) string {
	path = strings.TrimSpace(path)
	// On Windows, MSYS paths like /c/Users/... need to be converted to C:\Users\...
	// Regular paths like C:/Users/... just need slash conversion
	if runtime.GOOS == "windows" && len(path) >= 3 && path[0] == '/' {
		// Check for MSYS-style drive letter: /c/ or /C/
		if (path[1] >= 'a' && path[1] <= 'z' || path[1] >= 'A' && path[1] <= 'Z') && path[2] == '/' {
			// Convert /c/... to C:/...
			path = strings.ToUpper(string(path[1])) + ":" + path[2:]
		}
	}
	return filepath.FromSlash(path)
}

// CommitInfo holds metadata about a commit
type CommitInfo struct {
	SHA       string
	Author    string
	Subject   string
	Body      string // Full commit message body (excluding subject)
	Timestamp time.Time
}

// GetCommitInfo retrieves commit metadata
func GetCommitInfo(repoPath, sha string) (*CommitInfo, error) {
	return GetCommitInfoCtx(context.Background(), repoPath, sha)
}

// GetCommitInfoCtx is GetCommitInfo with a cancellable context.
func GetCommitInfoCtx(ctx context.Context, repoPath, sha string) (*CommitInfo, error) {
	// Use record separator (ASCII 30) to delimit fields - won't appear in commit messages
	const rs = "\x1e"
	cmd := exec.CommandContext(ctx, "git", "log", "-1", "--format=%H"+rs+"%an"+rs+"%s"+rs+"%aI"+rs+"%b", sha)
	cmd.Dir = repoPath

	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git log: %w", err)
	}

	parts := strings.SplitN(strings.TrimSuffix(string(out), "\n"), rs, 5)
	if len(parts) < 4 {
		return nil, fmt.Errorf("unexpected git log output: %s", out)
	}

	ts, err := time.Parse(time.RFC3339, parts[3])
	if err != nil {
		ts = time.Now() // Fallback
	}

	var body string
	if len(parts) >= 5 {
		body = strings.TrimSpace(parts[4])
	}

	return &CommitInfo{
		SHA:       parts[0],
		Author:    parts[1],
		Subject:   parts[2],
		Body:      body,
		Timestamp: ts,
	}, nil
}

// GetCurrentBranch returns the current branch name, or empty string if detached HEAD.
// Uses symbolic-ref (without --short) and strips refs/heads/ directly, because both
// rev-parse --abbrev-ref and symbolic-ref --short can return "heads/branch" when the
// name is ambiguous with another ref (remote tracking branch, tag, etc.).
func GetCurrentBranch(repoPath string) string {
	cmd := exec.Command("git", "symbolic-ref", "HEAD")
	cmd.Dir = repoPath

	out, err := cmd.Output()
	if err != nil {
		// Detached HEAD or not a git repo
		return ""
	}

	ref := strings.TrimSpace(string(out))
	return strings.TrimPrefix(ref, "refs/heads/")
}

// LocalBranchName strips the "origin/" prefix from a branch name if present.
// This normalizes branch names for comparison since GetDefaultBranch may return
// "origin/main" while GetCurrentBranch returns "main".
func LocalBranchName(branch string) string {
	return strings.TrimPrefix(branch, "origin/")
}

// IgnorePatternForDir returns the root-anchored ignore pattern for dir and a
// probe path that can be passed to git check-ignore.
func IgnorePatternForDir(repoPath, dir string) (pattern string, probe string, err error) {
	rel, err := filepath.Rel(repoPath, dir)
	if err != nil {
		return "", "", err
	}
	rel = filepath.Clean(rel)
	if rel == "." || !filepath.IsLocal(rel) {
		return "", "", fmt.Errorf("ignore directory must be under the repo root: %s", dir)
	}
	slashRel := filepath.ToSlash(rel)
	return "/" + slashRel + "/", filepath.ToSlash(filepath.Join(rel, ".roborev-ignore-check")), nil
}

// CheckIgnoreNoIndex reports whether path is ignored by git without requiring
// the path to exist or be tracked in the index.
func CheckIgnoreNoIndex(repoPath, path string) (bool, error) {
	cmd := exec.Command("git", "-C", repoPath, "check-ignore", "--quiet", "--no-index", path)
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return false, err
}

// EnsureLocalExcludePattern appends pattern to .git/info/exclude when the repo
// does not already ignore a path via tracked ignore files.
func EnsureLocalExcludePattern(repoPath, pattern string) error {
	excludePath, err := infoExcludePath(repoPath)
	if err != nil {
		return err
	}
	return AppendIgnorePatternFile(excludePath, pattern)
}

// HasTrackedFilesUnder reports whether git tracks any file at or under path.
func HasTrackedFilesUnder(repoPath, path string) (bool, error) {
	rel, err := filepath.Rel(repoPath, path)
	if err != nil {
		return false, err
	}
	rel = filepath.Clean(rel)
	if rel == "." || !filepath.IsLocal(rel) {
		return false, fmt.Errorf("path must be under the repo root: %s", path)
	}
	cmd := exec.Command("git", "-C", repoPath, "ls-files", "--", filepath.ToSlash(rel))
	out, err := cmd.Output()
	if err != nil {
		return false, fmt.Errorf("git ls-files: %w", err)
	}
	return strings.TrimSpace(string(out)) != "", nil
}

// EnsureNoTrackedFilesUnder rejects paths that already contain tracked files.
func EnsureNoTrackedFilesUnder(repoPath, path string) error {
	hasTrackedFiles, err := HasTrackedFilesUnder(repoPath, path)
	if err != nil {
		return err
	}
	if hasTrackedFiles {
		return fmt.Errorf("snapshot_dir must not contain tracked files: %s", path)
	}
	return nil
}

// ValidateRepoLocalPathNoSymlinks rejects repo-local paths whose existing path
// components contain symlinks or resolve outside the repository root.
func ValidateRepoLocalPathNoSymlinks(repoPath, path string) error {
	absRepo, err := filepath.Abs(repoPath)
	if err != nil {
		return err
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(absRepo, absPath)
	if err != nil {
		return err
	}
	rel = filepath.Clean(rel)
	if rel == "." || !filepath.IsLocal(rel) {
		return fmt.Errorf("path must be under the repo root: %s", path)
	}
	resolvedRepo, err := filepath.EvalSymlinks(absRepo)
	if err != nil {
		return fmt.Errorf("resolve repo root: %w", err)
	}
	current := absRepo
	for part := range strings.SplitSeq(rel, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("snapshot_dir must not contain symlinks: %s", current)
		}
		resolvedCurrent, err := filepath.EvalSymlinks(current)
		if err != nil {
			return err
		}
		resolvedRel, err := filepath.Rel(resolvedRepo, resolvedCurrent)
		if err != nil {
			return err
		}
		if resolvedRel == "." || !filepath.IsLocal(resolvedRel) {
			return fmt.Errorf("snapshot_dir must stay under the repo root: %s", path)
		}
	}
	return nil
}

func infoExcludePath(repoPath string) (string, error) {
	cmd := exec.Command("git", "-C", repoPath, "rev-parse", "--git-path", "info/exclude")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse info/exclude: %w", err)
	}
	path := strings.TrimSpace(string(out))
	if path == "" {
		return "", fmt.Errorf("git rev-parse info/exclude returned an empty path")
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(repoPath, path)
	}
	return path, nil
}

// AppendIgnorePatternFile appends roborev's snapshot ignore pattern to an
// ignore file, refusing symlinks and non-regular files.
func AppendIgnorePatternFile(path, pattern string) error {
	var prefix string
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s is a symlink; refusing to update it", path)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%s is not a regular file; refusing to update it", path)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
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

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
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

// IsOnBaseBranch returns true if currentBranch is equivalent to base for the
// purpose of "already on the base branch" guardrails. Bare local names
// ("main") match directly. Slash-containing bases are classified by
// namespace: an unambiguous remote-tracking ref (refs/remotes/<base> exists,
// refs/heads/<base> does not) is stripped of its configured-remote prefix
// before comparison; an unambiguous local branch (only refs/heads/<base>
// exists) is compared verbatim; and an ambiguous name (both exist) is
// refused so neither the pathological local "origin/main" nor a shadowed
// remote-tracking ref is treated as "already on base".
func IsOnBaseBranch(repoPath, currentBranch, base string) bool {
	if currentBranch == "" {
		return false
	}
	if !strings.Contains(base, "/") {
		return currentBranch == base
	}
	remoteExists := refExists(repoPath, "refs/remotes/"+base)
	localExists := refExists(repoPath, "refs/heads/"+base)
	if !remoteExists && !localExists {
		return false
	}
	if localExists && !remoteExists {
		return currentBranch == base
	}
	if localExists && remoteExists {
		return false
	}
	// remoteExists && !localExists — unambiguously remote-tracking.
	stripped := stripRemotePrefix(repoPath, base)
	if stripped == base {
		// The prefix did not match any configured remote name; bail out
		// rather than matching against an unstripped slashy name.
		return false
	}
	return currentBranch == stripped
}

// UpstreamIsTrunk reports whether ref's @{upstream} points to the
// repository's trunk. Only remote-tracking upstreams can be trunk: a
// local-branch upstream (configured via branch.<name>.remote = ".") is
// rejected even if its short name coincidentally matches the default
// branch. For unambiguous remote-tracking upstreams, the branch part
// after stripping the configured remote prefix must exactly match the
// branch part of GetDefaultBranch. Returns false if ref has no upstream
// configured or the default branch cannot be detected.
func UpstreamIsTrunk(repoPath, ref string) bool {
	cfg, ok := readUpstreamConfig(repoPath, ref)
	if !ok {
		return false
	}
	if !strings.HasPrefix(cfg.qualified, "refs/remotes/") {
		return false
	}
	defaultBranch, err := GetDefaultBranch(repoPath)
	if err != nil {
		return false
	}
	return stripRemotePrefix(repoPath, cfg.short) == stripRemotePrefix(repoPath, defaultBranch)
}

// stripRemotePrefix removes the longest configured-remote prefix from ref.
// "origin/main" → "main", "company/fork/main" → "main" (when "company/fork" is
// a remote), "origin/team/main" → "team/main" (when only "origin" is a remote
// — "origin/team" is not), and bare "main" → "main".
func stripRemotePrefix(repoPath, ref string) string {
	if !strings.Contains(ref, "/") {
		return ref
	}
	remotes, err := listRemotes(repoPath)
	if err != nil {
		return ref
	}
	// Longest match first so multi-slash remote names (e.g. "company/fork") are
	// tested before shorter prefixes that happen to overlap.
	sort.Slice(remotes, func(i, j int) bool { return len(remotes[i]) > len(remotes[j]) })
	for _, r := range remotes {
		if stripped, ok := strings.CutPrefix(ref, r+"/"); ok {
			return stripped
		}
	}
	return ref
}

// refExists reports whether the given fully-qualified ref resolves in the repo.
func refExists(repoPath, fullRef string) bool {
	cmd := exec.Command("git", "rev-parse", "--verify", "--quiet", fullRef)
	cmd.Dir = repoPath
	return cmd.Run() == nil
}

// listRemotes returns the names of configured remotes (e.g. ["origin", "upstream"]).
// Uses strings.Fields so CRLF-terminated output on Windows doesn't leave stray
// "\r" on remote names (which would break prefix matching downstream).
func listRemotes(repoPath string) ([]string, error) {
	cmd := exec.Command("git", "remote")
	cmd.Dir = repoPath
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git remote: %w", err)
	}
	return strings.Fields(string(out)), nil
}

// GetDiff returns the full diff for a commit, excluding generated
// files like lock files. Extra exclude patterns (filenames or globs)
// are appended to the built-in exclusion list.
func GetDiff(
	repoPath, sha string, extraExcludes ...string,
) (string, error) {
	return GetDiffCtx(context.Background(), repoPath, sha, extraExcludes...)
}

// GetDiffCtx is GetDiff with a cancellable context.
func GetDiffCtx(
	ctx context.Context, repoPath, sha string, extraExcludes ...string,
) (string, error) {
	args := []string{"show", sha, "--format=", "--"}
	args = append(args, ReviewPathspecArgs(extraExcludes...)...)

	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = repoPath

	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git show: %w", err)
	}

	return string(out), nil
}

// GetDiffLimited returns up to maxBytes of a commit diff and reports whether the
// output was truncated before the full diff was read.
func GetDiffLimited(
	repoPath, sha string, maxBytes int, extraExcludes ...string,
) (string, bool, error) {
	return GetDiffLimitedCtx(context.Background(), repoPath, sha, maxBytes, extraExcludes...)
}

// GetDiffLimitedCtx is GetDiffLimited with a cancellable context.
func GetDiffLimitedCtx(
	ctx context.Context, repoPath, sha string, maxBytes int, extraExcludes ...string,
) (string, bool, error) {
	args := []string{"show", sha, "--format=", "--"}
	args = append(args, ReviewPathspecArgs(extraExcludes...)...)
	return captureGitOutputLimited(ctx, repoPath, maxBytes, args...)
}

// GetFilesChanged returns the list of files changed in a commit
func GetFilesChanged(
	repoPath, sha string,
) ([]string, error) {
	return GetFilesChangedCtx(context.Background(), repoPath, sha)
}

// GetFilesChangedCtx is GetFilesChanged with a cancellable context.
func GetFilesChangedCtx(
	ctx context.Context, repoPath, sha string,
) ([]string, error) {
	cmd := exec.CommandContext(ctx, "git", "diff-tree", "--no-commit-id", "--name-only", "-r", sha)
	cmd.Dir = repoPath

	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git diff-tree: %w", err)
	}

	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	var files []string
	for _, line := range lines {
		if line != "" {
			files = append(files, line)
		}
	}

	return files, nil
}

// GetStat returns the stat summary for a commit
func GetStat(repoPath, sha string) (string, error) {
	cmd := exec.Command("git", "show", "--stat", sha, "--format=")
	cmd.Dir = repoPath

	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git show --stat: %w", err)
	}

	return string(out), nil
}

// IsUnbornHead returns true if the repository has an unborn HEAD (no commits yet).
// Returns false if HEAD points to a valid commit, if the path is not a git repo,
// or if HEAD is corrupt (e.g., ref pointing to a missing object).
func IsUnbornHead(repoPath string) bool {
	// Unborn HEAD = symbolic ref exists but the target branch ref doesn't.
	// Step 1: HEAD must be a symbolic ref (e.g., refs/heads/main)
	cmd := exec.Command("git", "symbolic-ref", "-q", "HEAD")
	cmd.Dir = repoPath
	out, err := cmd.Output()
	if err != nil {
		return false // not a symbolic ref or not a git repo
	}
	ref := strings.TrimSpace(string(out))
	if ref == "" {
		return false
	}
	// Step 2: "rev-parse --verify <ref>" fails only when the ref doesn't
	// exist at all (unborn). For corrupt refs (file exists but points to a
	// missing object), rev-parse still succeeds and returns the raw SHA.
	cmd = exec.Command("git", "rev-parse", "--verify", ref)
	cmd.Dir = repoPath
	return cmd.Run() != nil
}

// ResolveSHA resolves a ref (like HEAD) to a full SHA
func ResolveSHA(repoPath, ref string) (string, error) {
	return ResolveSHACtx(context.Background(), repoPath, ref)
}

// ResolveSHACtx is ResolveSHA with a cancellable context.
func ResolveSHACtx(ctx context.Context, repoPath, ref string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "rev-parse", ref)
	cmd.Dir = repoPath

	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse: %w", err)
	}

	return strings.TrimSpace(string(out)), nil
}

// IsAncestor checks if ancestor is an ancestor of descendant.
// Returns (true, nil) if ancestor is reachable from descendant via the commit graph.
// Returns (false, nil) if ancestor is not an ancestor (git exits with status 1).
// Returns (false, error) for git errors (e.g., bad object, repo issues).
func IsAncestor(repoPath, ancestor, descendant string) (bool, error) {
	cmd := exec.Command("git", "merge-base", "--is-ancestor", ancestor, descendant)
	cmd.Dir = repoPath
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	// Exit code 1 means "not ancestor", which is not an error
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil
	}
	// Any other error (exit code 128, etc.) is a real git error
	return false, fmt.Errorf("git merge-base --is-ancestor: %w", err)
}

// GetRepoRoot returns the root directory of the git repository
func GetRepoRoot(path string) (string, error) {
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	cmd.Dir = path

	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse --show-toplevel: %w", err)
	}

	// Git on Windows can return MSYS-style paths (/c/Users/...) or forward-slash paths (C:/...).
	// Convert to native Windows paths for consistency with Go's filepath.
	return normalizeMSYSPath(string(out)), nil
}

// ValidateWorktreeForRepo checks that worktreePath is a git checkout
// whose main repository root matches repoRoot. Returns true if the
// worktree is valid for the given repo. Returns false (without error)
// if the path doesn't exist, isn't a git repo, or belongs to a
// different repository.
func ValidateWorktreeForRepo(worktreePath, repoRoot string) bool {
	// Verify the path is itself a checkout root (not a subdirectory).
	toplevel, err := GetRepoRoot(worktreePath)
	if err != nil {
		return false
	}
	if cleanEvalPath(toplevel) != cleanEvalPath(worktreePath) {
		return false
	}
	// Verify the checkout belongs to the same main repository.
	mainRoot, err := GetMainRepoRoot(worktreePath)
	if err != nil {
		return false
	}
	return cleanEvalPath(mainRoot) == cleanEvalPath(repoRoot)
}

// cleanEvalPath resolves symlinks and cleans the path for comparison.
// Falls back to filepath.Clean if symlink resolution fails.
func cleanEvalPath(p string) string {
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	return filepath.Clean(p)
}

// ResolveGitDir returns the absolute path to the .git directory for a
// repository, correctly handling worktrees and MSYS-style paths on
// Windows.
func ResolveGitDir(repoPath string) (string, error) {
	cmd := exec.Command("git", "rev-parse", "--git-dir")
	cmd.Dir = repoPath
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse --git-dir: %w", err)
	}
	gitDir := normalizeMSYSPath(string(out))
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(repoPath, gitDir)
	}
	return filepath.Clean(gitDir), nil
}

// GetMainRepoRoot returns the main repository root, resolving through worktrees.
// For a regular repository or submodule, this returns the same as GetRepoRoot.
// For a worktree, this returns the main repository's root path.
func GetMainRepoRoot(path string) (string, error) {
	// Get both --git-dir and --git-common-dir to detect worktrees
	// For regular repos: both return ".git" (or absolute path)
	// For submodules: both return the same path (e.g., "../.git/modules/sub")
	// For worktrees: --git-dir returns worktree-specific dir, --git-common-dir returns main repo's .git
	gitDirCmd := exec.Command("git", "rev-parse", "--git-dir")
	gitDirCmd.Dir = path
	gitDirOut, err := gitDirCmd.Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse --git-dir: %w", err)
	}
	gitDir := normalizeMSYSPath(string(gitDirOut))

	commonDirCmd := exec.Command("git", "rev-parse", "--git-common-dir")
	commonDirCmd.Dir = path
	commonDirOut, err := commonDirCmd.Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse --git-common-dir: %w", err)
	}
	commonDir := normalizeMSYSPath(string(commonDirOut))

	// Make paths absolute for comparison
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(path, gitDir)
	}
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(path, commonDir)
	}
	gitDir = filepath.Clean(gitDir)
	commonDir = filepath.Clean(commonDir)

	// Only apply worktree resolution if git-dir differs from common-dir
	// This ensures submodules (where both are the same) use GetRepoRoot
	if gitDir != commonDir {
		// This is a worktree. For regular worktrees, commonDir ends with ".git"
		// and the main repo is its parent. For submodule worktrees, commonDir
		// is inside .git/modules/ and we need to read the core.worktree config.
		//
		// Some workspace managers create linked worktrees from a bare common
		// repository (for example, commonDir=/path/to/repo.git). In that layout
		// there is no main working tree, so the current checkout root is the
		// stable repo-local path to register and use for config resolution.
		bareCmd := exec.Command("git", "config", "--file", filepath.Join(commonDir, "config"), "--bool", "core.bare")
		if out, err := bareCmd.Output(); err == nil && strings.TrimSpace(string(out)) == "true" {
			return GetRepoRoot(path)
		}

		if filepath.Base(commonDir) == ".git" {
			// Regular worktree - parent of .git is the repo root
			return filepath.Dir(commonDir), nil
		}

		// Submodule worktree - read core.worktree from config
		cmd := exec.Command("git", "config", "--file", filepath.Join(commonDir, "config"), "core.worktree")
		out, err := cmd.Output()
		if err != nil {
			return "", fmt.Errorf("git config core.worktree for submodule worktree: %w", err)
		}
		worktree := normalizeMSYSPath(string(out))
		if !filepath.IsAbs(worktree) {
			worktree = filepath.Join(commonDir, worktree)
		}
		return filepath.Clean(worktree), nil
	}

	// Regular repo or submodule - use standard resolution
	return GetRepoRoot(path)
}

// ReadFile reads a file at a specific commit
func ReadFile(repoPath, sha, filePath string) ([]byte, error) {
	cmd := exec.Command("git", "show", fmt.Sprintf("%s:%s", sha, filePath))
	cmd.Dir = repoPath

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git show %s:%s: %s", sha, filePath, stderr.String())
	}

	return stdout.Bytes(), nil
}

// GetParentCommits returns the N commits before the given commit (not including it)
// Returns commits in reverse chronological order (most recent parent first)
func GetParentCommits(repoPath, sha string, count int) ([]string, error) {
	return GetParentCommitsCtx(context.Background(), repoPath, sha, count)
}

// GetParentCommitsCtx is GetParentCommits with a cancellable context.
func GetParentCommitsCtx(ctx context.Context, repoPath, sha string, count int) ([]string, error) {
	// Use git log to get parent commits, skipping the commit itself
	cmd := exec.CommandContext(ctx, "git", "log", "--format=%H", "-n", fmt.Sprintf("%d", count), "--skip=1", sha)
	cmd.Dir = repoPath

	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git log: %w", err)
	}

	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	var commits []string
	for _, line := range lines {
		if line != "" {
			commits = append(commits, line)
		}
	}

	return commits, nil
}

// GetCommitParents returns the direct parent commits for sha.
func GetCommitParents(repoPath, sha string) ([]string, error) {
	cmd := exec.Command("git", "show", "-s", "--format=%P", sha)
	cmd.Dir = repoPath

	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git show parents: %w", err)
	}

	return strings.Fields(string(out)), nil
}

// IsRange returns true if the ref is a range (contains "..")
func IsRange(ref string) bool {
	return strings.Contains(ref, "..")
}

// ParseRange splits a range ref into start and end
func ParseRange(ref string) (start, end string, ok bool) {
	parts := strings.SplitN(ref, "..", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// GetRangeCommits returns all commits in a range (oldest first)
func GetRangeCommits(repoPath, rangeRef string) ([]string, error) {
	return GetRangeCommitsCtx(context.Background(), repoPath, rangeRef)
}

// GetRangeCommitsCtx is GetRangeCommits with a cancellable context.
func GetRangeCommitsCtx(ctx context.Context, repoPath, rangeRef string) ([]string, error) {
	cmd := exec.CommandContext(ctx, "git", "log", "--format=%H", "--reverse", rangeRef)
	cmd.Dir = repoPath

	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git log range: %w", err)
	}

	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	var commits []string
	for _, line := range lines {
		if line != "" {
			commits = append(commits, line)
		}
	}

	return commits, nil
}

// GetRangeDiff returns the combined diff for a range, excluding
// generated files like lock files. Extra exclude patterns (filenames
// or globs) are appended to the built-in exclusion list.
func GetRangeDiff(
	repoPath, rangeRef string, extraExcludes ...string,
) (string, error) {
	return GetRangeDiffCtx(context.Background(), repoPath, rangeRef, extraExcludes...)
}

// GetRangeDiffCtx is GetRangeDiff with a cancellable context.
func GetRangeDiffCtx(
	ctx context.Context, repoPath, rangeRef string, extraExcludes ...string,
) (string, error) {
	args := []string{"diff", rangeRef, "--"}
	args = append(args, ReviewPathspecArgs(extraExcludes...)...)

	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = repoPath

	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git diff range: %w", err)
	}

	return string(out), nil
}

// GetRangeDiffLimited returns up to maxBytes of a range diff and reports
// whether the output was truncated before the full diff was read.
func GetRangeDiffLimited(
	repoPath, rangeRef string, maxBytes int, extraExcludes ...string,
) (string, bool, error) {
	return GetRangeDiffLimitedCtx(context.Background(), repoPath, rangeRef, maxBytes, extraExcludes...)
}

// GetRangeDiffLimitedCtx is GetRangeDiffLimited with a cancellable context.
func GetRangeDiffLimitedCtx(
	ctx context.Context, repoPath, rangeRef string, maxBytes int, extraExcludes ...string,
) (string, bool, error) {
	args := []string{"diff", rangeRef, "--"}
	args = append(args, ReviewPathspecArgs(extraExcludes...)...)
	return captureGitOutputLimited(ctx, repoPath, maxBytes, args...)
}

// HasUncommittedChanges returns true if there are uncommitted changes (staged, unstaged, or untracked files)
func HasUncommittedChanges(repoPath string) (bool, error) {
	// Check for staged or unstaged changes to tracked files
	cmd := exec.Command("git", "status", "--porcelain")
	cmd.Dir = repoPath

	out, err := cmd.Output()
	if err != nil {
		return false, fmt.Errorf("git status: %w", err)
	}

	return len(strings.TrimSpace(string(out))) > 0, nil
}

// GetDirtyFilesChanged returns changed file names for staged, unstaged, and
// untracked working-tree changes before review diff exclusions are applied.
func GetDirtyFilesChanged(repoPath string) ([]string, error) {
	cmd := exec.Command("git", "status", "--porcelain=v1", "-uall")
	cmd.Dir = repoPath

	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git status: %w", err)
	}

	seen := make(map[string]struct{})
	for raw := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		path := strings.TrimSpace(line[2:])
		if before, after, ok := strings.Cut(path, " -> "); ok {
			seen[normalizeStatusPath(before)] = struct{}{}
			seen[normalizeStatusPath(after)] = struct{}{}
			continue
		}
		seen[normalizeStatusPath(path)] = struct{}{}
	}

	files := make([]string, 0, len(seen))
	for file := range seen {
		if file != "" {
			files = append(files, file)
		}
	}
	slices.Sort(files)
	return files, nil
}

func normalizeStatusPath(path string) string {
	path = strings.TrimSpace(path)
	path = strings.Trim(path, `"`)
	return path
}

// EmptyTreeSHA is the SHA of an empty tree in git, used for diffing against
// the root commit or repos with no commits.
const EmptyTreeSHA = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

// GetDirtyDiff returns a diff of all uncommitted changes including
// untracked files. The diff includes both tracked file changes (via
// git diff HEAD) and untracked files formatted as new-file diff
// entries. Excludes generated files like lock files. Extra exclude
// patterns (filenames or globs) are appended to the built-in list.
func GetDirtyDiff(
	repoPath string, extraExcludes ...string,
) (string, error) {
	var result strings.Builder

	extra := FormatExcludeArgs(extraExcludes)

	// Build diff args with exclusions
	diffArgs := func(baseArgs ...string) []string {
		args := append(baseArgs, "--")
		args = append(args, ".")
		args = append(args, excludedPathPatterns...)
		args = append(args, extra...)
		return args
	}

	// 1. Get diff of tracked files (staged + unstaged)
	cmd := exec.Command("git", diffArgs("diff", "HEAD")...)
	cmd.Dir = repoPath

	out, err := cmd.Output()
	if err != nil {
		// If HEAD doesn't exist (no commits yet), we need to combine:
		// - git diff --cached <empty-tree>: shows staged files (index vs empty)
		// - git diff: shows unstaged changes (working tree vs index)
		// This covers the edge case where a file is staged but then removed from working tree

		// Get staged changes vs empty tree
		cmd = exec.Command("git", diffArgs("diff", "--cached", EmptyTreeSHA)...)
		cmd.Dir = repoPath
		stagedOut, err := cmd.Output()
		if err != nil {
			return "", fmt.Errorf("git diff --cached: %w", err)
		}
		if len(stagedOut) > 0 {
			result.Write(stagedOut)
		}

		// Get unstaged changes (working tree vs index)
		cmd = exec.Command("git", diffArgs("diff")...)
		cmd.Dir = repoPath
		unstagedOut, err := cmd.Output()
		if err != nil {
			return "", fmt.Errorf("git diff: %w", err)
		}
		if len(unstagedOut) > 0 {
			result.Write(unstagedOut)
		}
	} else {
		if len(out) > 0 {
			result.Write(out)
		}
	}

	// 2. Get list of untracked files, applying the same pathspec
	// excludes so filtering is consistent with the tracked diff.
	lsArgs := []string{"ls-files", "--others", "--exclude-standard", "--"}
	lsArgs = append(lsArgs, ".")
	lsArgs = append(lsArgs, excludedPathPatterns...)
	// Suppress a developer's UNTRACKED local kata override from dirty
	// reviews. A tracked .kata.local.toml is deliberately not excluded
	// anywhere (it steers kata binding resolution, so committing or
	// modifying it must stay visible to review).
	lsArgs = append(lsArgs, ":(exclude,glob)**/.kata.local.toml")
	lsArgs = append(lsArgs, extra...)
	cmd = exec.Command("git", lsArgs...)
	cmd.Dir = repoPath

	untrackedOut, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git ls-files: %w", err)
	}

	// 3. For each untracked file, create a diff-style "new file" entry
	untrackedFiles := strings.SplitSeq(strings.TrimSpace(string(untrackedOut)), "\n")
	for file := range untrackedFiles {
		if file == "" {
			continue
		}

		// Read file content
		filePath := filepath.Join(repoPath, file)
		content, err := os.ReadFile(filePath)
		if err != nil {
			// Skip files we can't read (permissions, etc.)
			continue
		}

		// Check if file is binary
		if isBinaryContent(content) {
			fmt.Fprintf(&result, "diff --git a/%s b/%s\n", file, file)
			result.WriteString("new file mode 100644\n")
			result.WriteString("Binary file (not shown)\n")
			continue
		}

		// Format as diff "new file" entry
		fmt.Fprintf(&result, "diff --git a/%s b/%s\n", file, file)
		result.WriteString("new file mode 100644\n")
		result.WriteString("--- /dev/null\n")
		fmt.Fprintf(&result, "+++ b/%s\n", file)

		lines := strings.Split(string(content), "\n")
		// Add line count header
		lineCount := len(lines)
		if lineCount > 0 && lines[lineCount-1] == "" {
			lineCount-- // Don't count trailing empty line from split
		}
		fmt.Fprintf(&result, "@@ -0,0 +1,%d @@\n", lineCount)

		// Add each line with + prefix
		for i, line := range lines {
			if i == len(lines)-1 && line == "" {
				// Skip trailing empty line from split
				continue
			}
			result.WriteString("+")
			result.WriteString(line)
			result.WriteString("\n")
		}
	}

	return result.String(), nil
}

// excludedPathPatterns contains pathspec patterns for generated or mechanical
// files that add noise to code reviews. Prompt construction summarizes omitted
// dependency metadata separately so reviewers can still verify manifest/lockfile
// consistency without inlining large lockfile bodies.
// Uses :(exclude,glob)**/ form so patterns match at any directory depth.
// Directory patterns use :(exclude) without glob since git recognizes them as trees.
var excludedPathPatterns = []string{
	// JavaScript / Node
	":(exclude,glob)**/package-lock.json",
	":(exclude,glob)**/yarn.lock",
	":(exclude,glob)**/pnpm-lock.yaml",
	":(exclude,glob)**/bun.lockb",
	":(exclude,glob)**/bun.lock",
	// Python
	":(exclude,glob)**/uv.lock",
	":(exclude,glob)**/poetry.lock",
	":(exclude,glob)**/Pipfile.lock",
	":(exclude,glob)**/pdm.lock",
	// Go
	":(exclude,glob)**/go.sum",
	// Rust
	":(exclude,glob)**/Cargo.lock",
	":(exclude,glob)**/cargo.lock", // lowercase for case-insensitive filesystems
	// Ruby
	":(exclude,glob)**/Gemfile.lock",
	// PHP
	":(exclude,glob)**/composer.lock",
	// .NET
	":(exclude,glob)**/packages.lock.json",
	// Dart / Flutter
	":(exclude,glob)**/pubspec.lock",
	// Elixir
	":(exclude,glob)**/mix.lock",
	// Swift
	":(exclude,glob)**/Package.resolved",
	// iOS / macOS
	":(exclude,glob)**/Podfile.lock",
	// Nix
	":(exclude,glob)**/flake.lock",
	":(exclude,glob)**/.beads/**",
	":(exclude,glob)**/.gocache/**",
	":(exclude,glob)**/.cache/**",
}

// FormatExcludeArgs converts user-provided exclude patterns (filenames
// or globs) into git pathspec arguments. Plain names without path
// separators get both **/name (file match) and **/name/** (directory
// subtree) so they work whether the name is a file or directory.
// Leading-slash patterns (/vendor) are root-anchored — no **/
// prefix. Patterns containing "/" are passed through as-is.
// FormatExcludeArgs converts user-provided exclude patterns into git
// pathspec arguments suitable for appending after "--".
func FormatExcludeArgs(patterns []string) []string {
	if len(patterns) == 0 {
		return nil
	}
	args := make([]string, 0, len(patterns))
	for _, raw := range patterns {
		p := strings.TrimSpace(raw)
		p = strings.TrimRight(p, "/")
		if p == "" {
			continue
		}

		// Leading slash = root-anchored. Strip the slash for
		// pathspec but don't add **/ prefix.
		rooted := strings.HasPrefix(p, "/")
		p = strings.TrimLeft(p, "/")
		if p == "" {
			continue
		}

		if rooted || strings.Contains(p, "/") {
			args = append(args,
				":(exclude,glob)"+p,
				":(exclude,glob)"+p+"/**",
			)
		} else {
			args = append(args,
				":(exclude,glob)**/"+p,
				":(exclude,glob)**/"+p+"/**",
			)
		}
	}
	return args
}

// ReviewPathspecArgs returns the git pathspec arguments roborev uses for review
// diffs: the repo root plus built-in and configured exclusions.
func ReviewPathspecArgs(extraExcludes ...string) []string {
	args := []string{"."}
	args = append(args, excludedPathPatterns...)
	args = append(args, FormatExcludeArgs(extraExcludes)...)
	return args
}

func captureGitOutputLimited(ctx context.Context, repoPath string, maxBytes int, args ...string) (string, bool, error) {
	if maxBytes <= 0 {
		return "", true, nil
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = repoPath

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", false, fmt.Errorf("create git stdout pipe: %w", err)
	}

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return "", false, fmt.Errorf("start git command: %w", err)
	}

	var out bytes.Buffer
	buf := make([]byte, 32*1024)
	truncated := false

	for {
		n, readErr := stdout.Read(buf)
		if n > 0 {
			remaining := maxBytes - out.Len()
			if remaining <= 0 {
				truncated = true
				cancel()
				break
			}
			if n > remaining {
				out.Write(buf[:remaining])
				truncated = true
				cancel()
				break
			}
			out.Write(buf[:n])
		}

		if readErr == nil {
			continue
		}
		if readErr == io.EOF {
			break
		}
		cancel()
		_ = cmd.Wait()
		return "", false, fmt.Errorf("read git output: %w", readErr)
	}

	// Drain remaining stdout so the process can exit cleanly.
	// On Windows, a killed process may still block if its stdout
	// pipe buffer is full and no reader is consuming it, which
	// prevents Wait from returning.
	if truncated {
		_, _ = io.Copy(io.Discard, stdout)
	}

	waitErr := cmd.Wait()
	if truncated {
		result := out.Bytes()
		// Trim at most 3 trailing bytes that form an incomplete
		// UTF-8 rune left by the byte-boundary truncation. Interior
		// invalid bytes are preserved and get FFFD replacement below.
		for i := 0; i < utf8.UTFMax-1 && len(result) > 0; i++ {
			r, size := utf8.DecodeLastRune(result)
			if r != utf8.RuneError || size != 1 {
				break
			}
			result = result[:len(result)-1]
		}
		return sanitizeToValidUTF8(result), true, nil
	}
	if waitErr != nil {
		return "", false, fmt.Errorf("git command failed: %w: %s", waitErr, strings.TrimSpace(stderr.String()))
	}

	return out.String(), false, nil
}

func sanitizeToValidUTF8(b []byte) string {
	return string(bytes.ToValidUTF8(b, []byte("\uFFFD")))
}

// isBinaryContent checks if content appears to be binary (contains null bytes in first 8KB)
func isBinaryContent(content []byte) bool {
	// Check first 8KB for null bytes
	checkLen := min(len(content), 8192)
	for i := range checkLen {
		if content[i] == 0 {
			return true
		}
	}
	return false
}

// GetRangeFilesChanged returns the list of files changed in a range (e.g. "mergeBase..HEAD").
func GetRangeFilesChanged(
	repoPath, rangeRef string,
) ([]string, error) {
	return GetRangeFilesChangedCtx(context.Background(), repoPath, rangeRef)
}

// GetRangeFilesChangedCtx is GetRangeFilesChanged with a cancellable context.
func GetRangeFilesChangedCtx(
	ctx context.Context, repoPath, rangeRef string,
) ([]string, error) {
	cmd := exec.CommandContext(ctx, "git", "diff", "--name-only", rangeRef)
	cmd.Dir = repoPath

	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git diff --name-only: %w", err)
	}

	text := strings.TrimSpace(string(out))
	if text == "" {
		return nil, nil
	}

	lines := strings.Split(text, "\n")
	var files []string
	for _, line := range lines {
		if line != "" {
			files = append(files, line)
		}
	}

	return files, nil
}

// GetRangeStart returns the start commit (first parent before range) for context lookup
func GetRangeStart(repoPath, rangeRef string) (string, error) {
	return GetRangeStartCtx(context.Background(), repoPath, rangeRef)
}

// GetRangeStartCtx is GetRangeStart with a cancellable context.
func GetRangeStartCtx(ctx context.Context, repoPath, rangeRef string) (string, error) {
	start, _, ok := ParseRange(rangeRef)
	if !ok {
		return "", fmt.Errorf("invalid range: %s", rangeRef)
	}

	// Resolve the start ref
	return ResolveSHACtx(ctx, repoPath, start)
}

// IsRebaseInProgress returns true if a rebase operation is in progress
func IsRebaseInProgress(repoPath string) bool {
	cmd := exec.Command("git", "rev-parse", "--git-dir")
	cmd.Dir = repoPath

	out, err := cmd.Output()
	if err != nil {
		return false
	}

	gitDir := strings.TrimSpace(string(out))
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(repoPath, gitDir)
	}

	// Check for rebase-merge (interactive rebase) or rebase-apply (git am, regular rebase)
	for _, dir := range []string{"rebase-merge", "rebase-apply"} {
		if info, err := os.Stat(filepath.Join(gitDir, dir)); err == nil && info.IsDir() {
			return true
		}
	}

	return false
}

// GetBranchName returns a human-readable branch reference for a commit.
// Returns something like "main", "feature/foo", or "main~3" depending on
// where the commit is relative to branch heads. Returns empty string on error
// or timeout (2 second limit to avoid blocking UI).
func GetBranchName(repoPath, sha string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", "name-rev", "--name-only", "--refs=refs/heads/*", sha)
	cmd.Dir = repoPath

	out, err := cmd.Output()
	if err != nil {
		return ""
	}

	name := strings.TrimSpace(string(out))
	// name-rev returns "undefined" if commit isn't reachable from any branch
	if name == "" || name == "undefined" {
		return ""
	}

	// Strip ~N or ^N suffix (e.g., "main~12" -> "main")
	// These indicate the commit is N commits behind the branch tip
	if idx := strings.IndexAny(name, "~^"); idx != -1 {
		name = name[:idx]
	}

	return name
}

// WorktreePathForBranch returns the worktree directory where branch is checked out.
// If the branch is checked out in any worktree (including the main repo), returns
// that path and true. If the branch is not checked out anywhere, returns repoPath
// and false. Returns a non-nil error if the git command fails.
// An empty branch always returns repoPath, true, nil.
func WorktreePathForBranch(repoPath, branch string) (string, bool, error) {
	if branch == "" {
		return repoPath, true, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", "-C", repoPath, "worktree", "list", "--porcelain")
	out, err := cmd.Output()
	if err != nil {
		return "", false, fmt.Errorf("git worktree list: %w", err)
	}

	// Parse porcelain output. Each worktree block is separated by blank lines.
	// Format:
	//   worktree /path/to/dir
	//   HEAD <sha>
	//   branch refs/heads/<name>
	//   [prunable ...]
	//   <blank line>
	//
	// We collect path+branch pairs, then verify the path exists before
	// returning it. This avoids returning stale/prunable worktree paths.
	type wtEntry struct {
		path, branch string
	}
	var entries []wtEntry
	var currentPath, currentBranch string
	for line := range strings.SplitSeq(string(out), "\n") {
		line = strings.TrimSpace(line)
		if path, ok := strings.CutPrefix(line, "worktree "); ok {
			currentPath = path
			currentBranch = ""
		} else if ref, ok := strings.CutPrefix(line, "branch "); ok {
			currentBranch = strings.TrimPrefix(ref, "refs/heads/")
		} else if line == "" && currentPath != "" {
			if currentBranch != "" {
				entries = append(entries, wtEntry{currentPath, currentBranch})
			}
			currentPath = ""
			currentBranch = ""
		}
	}
	// Handle last block if output doesn't end with a blank line.
	if currentPath != "" && currentBranch != "" {
		entries = append(entries, wtEntry{currentPath, currentBranch})
	}

	for _, e := range entries {
		if e.branch == branch {
			if _, err := os.Stat(e.path); err == nil {
				return e.path, true, nil
			}
			// Path doesn't exist (stale/prunable worktree) — skip it.
		}
	}
	return repoPath, false, nil
}

// EnsureAbsoluteHooksPath checks whether core.hooksPath is set
// to a relative value and, if so, resolves it to an absolute
// path and updates the git config. Relative hooks paths break
// linked worktrees because git resolves them from the worktree
// root, not the main repo root.
func EnsureAbsoluteHooksPath(repoPath string) error {
	// Read the effective value from any config level
	// (local, global, system) so we catch relative paths
	// from ~/.gitconfig too.
	cmd := exec.Command(
		"git", "config", "core.hooksPath",
	)
	cmd.Dir = repoPath
	out, err := cmd.Output()
	if err != nil {
		// Not set at any level — nothing to fix.
		return nil
	}
	raw := normalizeMSYSPath(string(out))
	if raw == "" || filepath.IsAbs(raw) || isGitTildePath(raw) {
		return nil
	}
	// Resolve against the main repo root, not the worktree
	// root, so the shared config value stays valid after a
	// linked worktree is removed.
	mainRoot, err := GetMainRepoRoot(repoPath)
	if err != nil {
		return fmt.Errorf(
			"resolve main repo root: %w", err,
		)
	}
	abs := filepath.Join(mainRoot, raw)
	set := exec.Command(
		"git", "config", "--local", "core.hooksPath", abs,
	)
	set.Dir = repoPath
	if err := set.Run(); err != nil {
		return fmt.Errorf(
			"update core.hooksPath to absolute: %w", err,
		)
	}
	return nil
}

// isGitTildePath returns true for paths that git expands via
// tilde expansion: "~", "~/path", "~user", "~user/path".
// These must not be joined to a repo root. Git calls
// getpwnam on the text between ~ and the first slash, so
// ~user must start with a valid POSIX username character
// (letter or underscore).
func isGitTildePath(s string) bool {
	if s == "" || s[0] != '~' {
		return false
	}
	if len(s) == 1 {
		return true
	}
	c := s[1]
	if c == '/' || c == filepath.Separator {
		return true
	}
	return (c >= 'a' && c <= 'z') ||
		(c >= 'A' && c <= 'Z') || c == '_'
}

// GetHooksPath returns the path to the hooks directory,
// respecting core.hooksPath. Relative paths are resolved
// against the main repository root (not the worktree root)
// so that linked worktrees share the same hooks directory.
func GetHooksPath(repoPath string) (string, error) {
	cmd := exec.Command("git", "rev-parse", "--git-path", "hooks")
	cmd.Dir = repoPath

	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf(
			"git rev-parse --git-path hooks: %w", err,
		)
	}

	hooksPath := normalizeMSYSPath(string(out))

	if !filepath.IsAbs(hooksPath) {
		// Resolve against the main repo root so linked
		// worktrees get the same hooks directory.
		root, err := GetMainRepoRoot(repoPath)
		if err != nil {
			return "", fmt.Errorf(
				"resolve main repo root for hooks path: %w",
				err,
			)
		}
		hooksPath = filepath.Join(root, hooksPath)
	}

	return hooksPath, nil
}

// commitHookNames lists the hook scripts that can reject a commit.
var commitHookNames = []string{
	"pre-commit",
	"prepare-commit-msg",
	"commit-msg",
}

// hasCommitHooks returns true if the repo has at least one
// executable commit-related hook installed.
func hasCommitHooks(repoPath string) bool {
	hooksDir, err := GetHooksPath(repoPath)
	if err != nil {
		return false
	}
	for _, name := range commitHookNames {
		p := filepath.Join(hooksDir, name)
		info, err := os.Stat(p)
		if err != nil {
			continue
		}
		if info.IsDir() {
			continue
		}
		// On Unix, check the execute bit. On Windows every
		// regular file is considered executable.
		if runtime.GOOS == "windows" || info.Mode()&0o111 != 0 {
			return true
		}
	}
	return false
}

// isHookCausingFailure checks whether a commit failure was caused
// by a hook (pre-commit, commit-msg, etc.). It combines two checks:
//  1. At least one commit hook must be installed — if there are no
//     hooks, the failure is definitionally non-hook (e.g., GPG
//     signing, object-write errors).
//  2. A hookless dry-run (`git commit --dry-run --no-verify`) must
//     succeed, confirming the commit is otherwise viable.
//
// Both conditions must hold to classify the failure as hook-caused.
func isHookCausingFailure(repoPath string) bool {
	if !hasCommitHooks(repoPath) {
		return false
	}
	cmd := exec.Command(
		"git", "-C", repoPath, "commit",
		"--dry-run", "--no-verify", "-m", "probe",
	)
	return cmd.Run() == nil
}

// GetDefaultBranch detects the default branch (from origin/HEAD, or main/master locally)
func GetDefaultBranch(repoPath string) (string, error) {
	// Prefer origin/HEAD as the authoritative source for the default branch
	cmd := exec.Command("git", "symbolic-ref", "refs/remotes/origin/HEAD")
	cmd.Dir = repoPath
	out, err := cmd.Output()
	if err == nil {
		// Returns refs/remotes/origin/main -> extract "main"
		ref := strings.TrimSpace(string(out))
		branchName := strings.TrimPrefix(ref, "refs/remotes/origin/")
		if branchName != "" {
			// Verify the remote-tracking ref exists before using it
			checkCmd := exec.Command("git", "rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+branchName)
			checkCmd.Dir = repoPath
			if checkCmd.Run() == nil {
				return "origin/" + branchName, nil
			}
			// Remote-tracking ref doesn't exist, fall back to local branch
			checkCmd = exec.Command("git", "rev-parse", "--verify", "--quiet", branchName)
			checkCmd.Dir = repoPath
			if checkCmd.Run() == nil {
				return branchName, nil
			}
		}
	}

	// Fall back to common local branch names (for repos without origin)
	for _, branch := range []string{"main", "master"} {
		cmd := exec.Command("git", "rev-parse", "--verify", "--quiet", branch)
		cmd.Dir = repoPath
		if err := cmd.Run(); err == nil {
			return branch, nil
		}
	}

	return "", fmt.Errorf("could not detect default branch (tried origin/HEAD, main, master)")
}

// UpstreamMissingError reports that a branch's @{upstream} is configured but
// the referenced ref does not resolve locally (e.g., the remote-tracking ref
// has not been fetched or was deleted). Callers should surface this to the
// user instead of silently falling back to a different base branch, which
// could select the wrong commit range in fork workflows.
type UpstreamMissingError struct {
	Ref      string // The branch whose upstream was resolved (e.g., "HEAD" or "feature").
	Upstream string // The configured upstream name (e.g., "upstream/main").
}

func (e *UpstreamMissingError) Error() string {
	return fmt.Sprintf("upstream %q for %s does not resolve locally (try 'git fetch')", e.Upstream, e.Ref)
}

// GetUpstream returns the upstream tracking branch for a ref (e.g., "upstream/main").
// Returns ("", nil) when no @{upstream} is configured, so callers can fall back
// to a default base. Returns ("", *UpstreamMissingError) when @{upstream} is
// configured but the referenced ref does not resolve locally — callers should
// surface this instead of falling back, because the fallback target may select
// the wrong commit range. Passing an empty ref is equivalent to HEAD.
func GetUpstream(repoPath, ref string) (string, error) {
	if ref == "" {
		ref = "HEAD"
	}
	cmd := exec.Command("git", "rev-parse", "--abbrev-ref", "--symbolic-full-name", ref+"@{upstream}")
	cmd.Dir = repoPath

	out, err := cmd.Output()
	if err != nil {
		// Exit code 128 covers both "no upstream configured" and "upstream
		// configured but ref not resolvable" (git varies between versions).
		// Distinguish by re-reading branch.<name>.remote/merge.
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 128 {
			if cfg, ok := readUpstreamConfig(repoPath, ref); ok {
				return "", &UpstreamMissingError{Ref: ref, Upstream: cfg.short}
			}
			return "", nil
		}
		return "", fmt.Errorf("git rev-parse @{upstream}: %w", err)
	}

	upstream := strings.TrimSpace(string(out))
	if upstream == "" {
		return "", nil
	}
	// Verify the exact namespace the tracking config points to so a lookalike
	// ref with the same short name (local branch, tag) can't shadow a missing
	// remote-tracking ref. Fall back to an unqualified check if the config
	// can't be read — e.g., for detached HEAD callers.
	if cfg, ok := readUpstreamConfig(repoPath, ref); ok {
		if !refExists(repoPath, cfg.qualified) {
			return "", &UpstreamMissingError{Ref: ref, Upstream: upstream}
		}
	} else if !refExists(repoPath, upstream) {
		return "", &UpstreamMissingError{Ref: ref, Upstream: upstream}
	}
	return upstream, nil
}

// upstreamConfig captures the resolved short name and fully-qualified ref
// implied by branch.<name>.remote and branch.<name>.merge.
type upstreamConfig struct {
	short     string // e.g. "upstream/main" or "main" for local tracking
	qualified string // e.g. "refs/remotes/upstream/main" or "refs/heads/main"
}

// readUpstreamConfig returns the upstream configuration for a ref. Returns
// (cfg, true) when branch.<name>.remote and branch.<name>.merge are set,
// indicating @{upstream} is configured even if rev-parse cannot resolve the
// ref. Returns (zero, false) otherwise.
func readUpstreamConfig(repoPath, ref string) (upstreamConfig, bool) {
	branch := branchNameForConfig(repoPath, ref)
	if branch == "" {
		return upstreamConfig{}, false
	}
	remote := readGitConfig(repoPath, "branch."+branch+".remote")
	merge := readGitConfig(repoPath, "branch."+branch+".merge")
	if remote == "" || merge == "" {
		return upstreamConfig{}, false
	}
	mergeBranch := strings.TrimPrefix(merge, "refs/heads/")
	if remote == "." {
		// Local-branch tracking writes the target verbatim.
		return upstreamConfig{
			short:     mergeBranch,
			qualified: "refs/heads/" + mergeBranch,
		}, true
	}
	if remoteValueIsURL(repoPath, remote) {
		return upstreamConfig{}, false
	}
	return upstreamConfig{
		short:     remote + "/" + mergeBranch,
		qualified: "refs/remotes/" + remote + "/" + mergeBranch,
	}, true
}

// GetBranchBase returns the base ref derived from ref's branch.<name>.base
// config. Bare names (e.g. "main") are resolved to the remote-tracking ref
// (e.g. "origin/main") that the local branch tracks when local <name> exists
// and is an ancestor of the remote-tracking ref — i.e., local <name> is just
// a stale snapshot of the trunk. Without this, merge-base(local-main, HEAD)
// walks back to the stale pointer and the resulting range pulls in commits
// already merged into the remote trunk after a rebase-onto-origin/main.
// Qualified refs (containing '/') are returned as-is so users can pin to
// e.g. "upstream/main". Passing an empty ref is equivalent to HEAD.
func GetBranchBase(repoPath, ref string) string {
	branch := branchNameForConfig(repoPath, ref)
	if branch == "" {
		return ""
	}
	raw := readGitConfig(repoPath, "branch."+branch+".base")
	return resolveConfiguredBase(repoPath, raw)
}

// resolveConfiguredBase translates a configured base name to the remote-
// tracking ref it should map to, or returns configValue unchanged when no
// translation applies. Values that already name a remote-tracking ref
// unambiguously are passed through; local branch names (including slash-
// containing names like "release/1.2") are subject to stale-ancestor
// translation; divergent local branches are also passed through.
func resolveConfiguredBase(repoPath, configValue string) string {
	if configValue == "" {
		return configValue
	}
	if isQualifiedRemoteRef(repoPath, configValue) {
		return configValue
	}
	remoteTracking := preferredRemoteTracking(repoPath, configValue)
	if remoteTracking == "" {
		return configValue
	}
	if !refExists(repoPath, "refs/heads/"+configValue) {
		return remoteTracking
	}
	isAncestor, err := IsAncestor(repoPath, configValue, remoteTracking)
	if err != nil || !isAncestor {
		return configValue
	}
	return remoteTracking
}

// isQualifiedRemoteRef reports whether value names a remote-tracking ref
// that is not shadowed by a same-named local branch. Defers to the local
// branch when both exist so stale-ancestor translation still applies, and
// so the resolved base matches git's normal ref lookup precedence
// (refs/heads beats refs/remotes).
func isQualifiedRemoteRef(repoPath, value string) bool {
	if !strings.Contains(value, "/") {
		return false
	}
	if !refExists(repoPath, "refs/remotes/"+value) {
		return false
	}
	if refExists(repoPath, "refs/heads/"+value) {
		return false
	}
	return true
}

// preferredRemoteTracking returns the remote-tracking ref short name for a
// branch name, preferring the branch's configured @{upstream} so fork
// workflows (local main → upstream/main) translate correctly, and falling
// back to origin/<name> only when no upstream is configured. Returns ""
// when no remote-tracking counterpart resolves locally; in particular:
//   - An explicitly configured upstream that does not resolve is NOT
//     silently substituted with origin/<name> — that would override the
//     user's choice of remote in fork workflows.
//   - For slash-containing names, the origin/<name> fallback only applies
//     when a local branch by that name exists. Otherwise a remote-
//     qualified value like "upstream/main" whose configured remote-
//     tracking ref isn't fetched would collapse to "origin/upstream/main",
//     silently switching to a different remote.
func preferredRemoteTracking(repoPath, name string) string {
	if cfg, ok := readUpstreamConfig(repoPath, name); ok {
		if strings.HasPrefix(cfg.qualified, "refs/remotes/") &&
			refExists(repoPath, cfg.qualified) {
			return cfg.short
		}
		return ""
	}
	if !refExists(repoPath, "refs/remotes/origin/"+name) {
		return ""
	}
	if strings.Contains(name, "/") && !refExists(repoPath, "refs/heads/"+name) {
		return ""
	}
	return "origin/" + name
}

func branchNameForConfig(repoPath, ref string) string {
	branch := ref
	if branch == "HEAD" || branch == "" {
		branch = GetCurrentBranch(repoPath)
		if branch == "" {
			return ""
		}
	} else {
		// Accept fully-qualified local branch refs (e.g., "refs/heads/feature")
		// so callers who pass a ResolveSHA-style ref still resolve to the
		// correct branch.<name>.* config keys.
		branch = strings.TrimPrefix(branch, "refs/heads/")
	}
	return branch
}

func remoteValueIsURL(repoPath, remote string) bool {
	if remoteNameExists(repoPath, remote) {
		return false
	}
	return strings.Contains(remote, "://") ||
		strings.Contains(remote, ":") ||
		strings.HasPrefix(remote, "/") ||
		strings.HasPrefix(remote, "./") ||
		strings.HasPrefix(remote, "../") ||
		strings.HasPrefix(remote, "~")
}

func remoteNameExists(repoPath, remote string) bool {
	remotes, err := listRemotes(repoPath)
	if err != nil {
		return false
	}
	return slices.Contains(remotes, remote)
}

// readGitConfig returns the value of a git config key, or "" if missing.
func readGitConfig(repoPath, key string) string {
	cmd := exec.Command("git", "config", "--get", key)
	cmd.Dir = repoPath
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// GetMergeBase returns the merge-base (common ancestor) between two refs
func GetMergeBase(repoPath, ref1, ref2 string) (string, error) {
	cmd := exec.Command("git", "merge-base", ref1, ref2)
	cmd.Dir = repoPath

	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git merge-base: %w", err)
	}

	return strings.TrimSpace(string(out)), nil
}

// GetCommitsSince returns all commits from mergeBase to HEAD (exclusive of mergeBase)
// Returns commits in chronological order (oldest first)
func GetCommitsSince(repoPath, mergeBase string) ([]string, error) {
	rangeRef := mergeBase + "..HEAD"
	return GetRangeCommits(repoPath, rangeRef)
}

// CommitError represents a failure during CreateCommit.
// Phase distinguishes "add" failures (lockfile, permissions) from
// "commit" failures (hooks, empty commit, identity issues).
// HookFailed is set by probing whether a hookless commit would
// succeed — true means a hook (pre-commit, commit-msg, etc.)
// caused the failure.
type CommitError struct {
	Phase      string // "add" or "commit"
	HookFailed bool   // true when a hook caused the failure
	Stderr     string
	Err        error
}

func (e *CommitError) Error() string {
	return fmt.Sprintf("git %s: %v: %s", e.Phase, e.Err, e.Stderr)
}

func (e *CommitError) Unwrap() error {
	return e.Err
}

// CreateCommit stages all changes and creates a commit with the given message
// Returns the SHA of the new commit
func CreateCommit(repoPath, message string) (string, error) {
	// Stage all changes (respects .gitignore)
	cmd := exec.Command("git", "add", "-A")
	cmd.Dir = repoPath
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", &CommitError{
			Phase: "add", Stderr: stderr.String(), Err: err,
		}
	}

	// Create commit
	cmd = exec.Command("git", "commit", "-m", message)
	cmd.Dir = repoPath
	stderr.Reset()
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", &CommitError{
			Phase:      "commit",
			HookFailed: isHookCausingFailure(repoPath),
			Stderr:     stderr.String(),
			Err:        err,
		}
	}

	// Get the SHA of the new commit
	sha, err := ResolveSHA(repoPath, "HEAD")
	if err != nil {
		return "", fmt.Errorf("get new commit SHA: %w", err)
	}

	return sha, nil
}

// IsWorkingTreeClean returns true if the working tree has no uncommitted or untracked changes
func IsWorkingTreeClean(repoPath string) bool {
	cmd := exec.Command("git", "-C", repoPath, "status", "--porcelain")
	output, err := cmd.Output()
	if err != nil {
		return false // Assume dirty if we can't check
	}
	return len(strings.TrimSpace(string(output))) == 0
}

// CheckoutBranch switches to the given branch in the repository.
func CheckoutBranch(repoPath, branch string) error {
	cmd := exec.Command("git", "checkout", branch)
	cmd.Dir = repoPath
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf(
			"git checkout %s: %w: %s",
			branch, err, stderr.String(),
		)
	}
	return nil
}

// ResetWorkingTree discards all uncommitted changes (staged and unstaged)
func ResetWorkingTree(repoPath string) error {
	// Reset staged changes
	resetCmd := exec.Command("git", "-C", repoPath, "reset", "--hard", "HEAD")
	if err := resetCmd.Run(); err != nil {
		return fmt.Errorf("git reset --hard: %w", err)
	}
	// Clean untracked files
	cleanCmd := exec.Command("git", "-C", repoPath, "clean", "-fd")
	if err := cleanCmd.Run(); err != nil {
		return fmt.Errorf("git clean: %w", err)
	}
	return nil
}

// GetRemoteURL returns the URL for a git remote.
// If remoteName is empty, tries "origin" first, then any other remote.
// Returns empty string if no remotes exist.
func GetRemoteURL(repoPath, remoteName string) string {
	if remoteName == "" {
		// Try origin first
		url := getRemoteURLByName(repoPath, "origin")
		if url != "" {
			return url
		}
		// Fall back to any remote
		return getAnyRemoteURL(repoPath)
	}
	return getRemoteURLByName(repoPath, remoteName)
}

func getRemoteURLByName(repoPath, name string) string {
	cmd := exec.Command("git", "remote", "get-url", name)
	cmd.Dir = repoPath
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// GetPatchID returns the stable patch-id for a commit. Patch-ids are
// content-based hashes of the diff, so two commits with the same code
// change (e.g. before and after a rebase) share the same patch-id.
// Returns "" for merge commits, empty commits, or on any error.
func GetPatchID(repoPath, sha string) string {
	show := exec.Command("git", "-c", "color.ui=false", "show", sha)
	show.Dir = repoPath

	patchID := exec.Command("git", "patch-id", "--stable")
	patchID.Dir = repoPath

	pipe, err := show.StdoutPipe()
	if err != nil {
		return ""
	}
	patchID.Stdin = pipe

	var out bytes.Buffer
	patchID.Stdout = &out

	if err := show.Start(); err != nil {
		return ""
	}
	if err := patchID.Start(); err != nil {
		pipe.Close() // unblock show if pipe buffer is full
		_ = show.Wait()
		return ""
	}

	_ = show.Wait() // only patchID's exit status matters
	if err := patchID.Wait(); err != nil {
		return ""
	}

	// Output format: "<patch-id> <commit-sha>\n"
	fields := strings.Fields(out.String())
	if len(fields) < 1 {
		return ""
	}
	return fields[0]
}

func getAnyRemoteURL(repoPath string) string {
	// List all remotes
	cmd := exec.Command("git", "remote")
	cmd.Dir = repoPath
	out, err := cmd.Output()
	if err != nil {
		return ""
	}

	remotes := strings.SplitSeq(strings.TrimSpace(string(out)), "\n")
	for remote := range remotes {
		if remote == "" {
			continue
		}
		url := getRemoteURLByName(repoPath, remote)
		if url != "" {
			return url
		}
	}
	return ""
}

// ShortRef abbreviates a git ref for display. SHA-like tokens
// (hex strings longer than 7 chars) are truncated to 7 chars.
// Range refs like "abc123def..xyz789abc" become "abc123d..xyz789a".
// Non-hex refs (branch names, task labels) pass through unchanged.
func ShortRef(ref string) string {
	if before, after, ok := strings.Cut(ref, ".."); ok {
		return shortenIfHex(before) + ".." + shortenIfHex(after)
	}
	return shortenIfHex(ref)
}

// shortenIfHex truncates s to 7 characters only if it looks like a
// hex SHA (all hex digits and longer than 7 chars). Non-hex strings
// like branch names or task labels are returned unchanged.
func shortenIfHex(s string) string {
	if len(s) > 7 && isHex(s) {
		return s[:7]
	}
	return s
}

func isHex(s string) bool {
	for _, c := range s {
		if (c < '0' || c > '9') &&
			(c < 'a' || c > 'f') &&
			(c < 'A' || c > 'F') {
			return false
		}
	}
	return len(s) > 0
}

// ShortSHA returns the first 7 characters of a SHA hash, or
// the full string if shorter. Matches git's default abbreviation.
func ShortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// LooksLikeSHA returns true if s looks like a git commit SHA (7-40 hex chars,
// case-insensitive). The 7-char minimum matches git's default abbreviation
// length and safely excludes short hex task labels like "dead" or "cafe".
func LooksLikeSHA(s string) bool {
	return len(s) >= 7 && len(s) <= 40 && isHex(s)
}
