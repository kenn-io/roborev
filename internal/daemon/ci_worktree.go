package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	gitworktree "go.kenn.io/kit/git/worktree"

	"go.kenn.io/roborev/internal/config"
)

const (
	ciWorktreeDirName    = "ci-worktrees"
	ciWorktreePrefix     = "roborev-ci-"
	ciWorktreeRepoMarker = "roborev-ci-parent"
)

func ciWorktreeParentDir() string {
	return filepath.Join(config.DataDir(), ciWorktreeDirName)
}

func writeCIWorktreeMarker(worktreeDir, repoPath string) error {
	repoPath = strings.TrimSpace(repoPath)
	if repoPath == "" {
		return errors.New("repo path must not be empty")
	}
	markerPath, err := ciWorktreeMarkerPath(worktreeDir)
	if err != nil {
		return err
	}
	return os.WriteFile(
		markerPath,
		[]byte(canonicalPath(repoPath)+"\n"),
		0o600,
	)
}

func readCIWorktreeMarker(worktreeDir string) (string, error) {
	markerPath, err := ciWorktreeMarkerPath(worktreeDir)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(markerPath)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

func ciWorktreeMarkerPath(worktreeDir string) (string, error) {
	gitDir, err := linkedWorktreeGitDir(worktreeDir)
	if err != nil {
		return "", err
	}
	return filepath.Join(gitDir, ciWorktreeRepoMarker), nil
}

func linkedWorktreeGitDir(worktreeDir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(worktreeDir, ".git"))
	if err != nil {
		return "", err
	}
	line := strings.TrimSpace(string(data))
	const prefix = "gitdir:"
	if !strings.HasPrefix(line, prefix) {
		return "", fmt.Errorf("worktree %s has unsupported .git file", worktreeDir)
	}
	gitDir := strings.TrimSpace(strings.TrimPrefix(line, prefix))
	if gitDir == "" {
		return "", fmt.Errorf("worktree %s has empty gitdir", worktreeDir)
	}
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(worktreeDir, gitDir)
	}
	return filepath.Clean(gitDir), nil
}

func repoPathFromLinkedWorktree(worktreeDir string) (string, error) {
	gitDir, err := linkedWorktreeGitDir(worktreeDir)
	if err != nil {
		return "", err
	}
	commonGitDir := filepath.Dir(filepath.Dir(gitDir))
	if filepath.Base(commonGitDir) != ".git" {
		return "", fmt.Errorf("worktree %s has unsupported common git dir %s", worktreeDir, commonGitDir)
	}
	return filepath.Dir(commonGitDir), nil
}

func cleanupStaleCIWorktrees(ctx context.Context) error {
	parentDir := ciWorktreeParentDir()
	entries, err := os.ReadDir(parentDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read CI worktree parent: %w", err)
	}

	var errs []error
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), ciWorktreePrefix) {
			continue
		}
		dir := filepath.Join(parentDir, entry.Name())
		if err := removeStaleCIWorktree(ctx, dir); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func removeStaleCIWorktree(ctx context.Context, worktreeDir string) error {
	repoPath, err := readCIWorktreeMarker(worktreeDir)
	if err != nil || repoPath == "" {
		repoPath, err = repoPathFromLinkedWorktree(worktreeDir)
		if err != nil || repoPath == "" {
			return removeCIWorktreeDir(worktreeDir)
		}
	}
	if _, err := os.Stat(repoPath); err != nil {
		return removeCIWorktreeDir(worktreeDir)
	}

	unlock := lockGitMetadata(repoPath)
	defer unlock()

	wt := &gitworktree.Worktree{Dir: worktreeDir, Repo: repoPath}
	closeErr := wt.Close(ctx)
	pruneErr := pruneGitWorktrees(ctx, repoPath)
	if closeErr != nil {
		if _, statErr := os.Stat(worktreeDir); statErr == nil {
			return fmt.Errorf("remove stale CI worktree %s: %w", worktreeDir, closeErr)
		}
	}
	if pruneErr != nil {
		return fmt.Errorf("prune stale CI worktree metadata for %s: %w", repoPath, pruneErr)
	}
	return nil
}

func removeCIWorktreeDir(worktreeDir string) error {
	if err := os.RemoveAll(worktreeDir); err != nil {
		return fmt.Errorf("remove stale CI worktree directory %s: %w", worktreeDir, err)
	}
	return nil
}

func pruneGitWorktrees(ctx context.Context, repoPath string) error {
	cmd := exec.CommandContext(ctx, "git", "-C", repoPath, "worktree", "prune")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%w: %s", err, string(out))
	}
	return nil
}
