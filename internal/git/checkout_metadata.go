package git

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
)

// CheckoutMetadata describes the repository state needed to identify a
// checkout without spawning git.
type CheckoutMetadata struct {
	WorktreeRoot string
	GitDir       string
	CommonDir    string
	Head         string
	Branch       string
}

// ReadCheckoutMetadata reads checkout identity with go-git. It supports plain
// repositories and linked worktrees, including their separate git and common
// directories.
func ReadCheckoutMetadata(repoPath string) (CheckoutMetadata, error) {
	metadata, unborn, err := readCheckoutMetadata(repoPath)
	if err != nil {
		return CheckoutMetadata{}, err
	}
	if unborn {
		return CheckoutMetadata{}, fmt.Errorf("resolve HEAD: branch %s has no commits: %w",
			metadata.Branch, plumbing.ErrReferenceNotFound)
	}
	return metadata, nil
}

// ReadUnbornCheckoutMetadata reads a checkout whose HEAD names a branch with
// no commits yet, such as a repository before its first commit. Head is
// empty. It fails when HEAD resolves to a commit.
func ReadUnbornCheckoutMetadata(repoPath string) (CheckoutMetadata, error) {
	metadata, unborn, err := readCheckoutMetadata(repoPath)
	if err != nil {
		return CheckoutMetadata{}, err
	}
	if !unborn {
		return CheckoutMetadata{}, errors.New("HEAD resolves to a commit")
	}
	return metadata, nil
}

// readCheckoutMetadata reports unborn when HEAD names a branch whose ref does
// not exist; the returned metadata then has an empty Head.
func readCheckoutMetadata(repoPath string) (metadata CheckoutMetadata, unborn bool, err error) {
	// Open linked worktree and common-dir storage separately. go-git's
	// EnableDotGitCommonDir path leaves the commondir file locked on Windows.
	repo, err := gogit.PlainOpenWithOptions(repoPath, &gogit.PlainOpenOptions{
		DetectDotGit: true,
	})
	if err != nil {
		return CheckoutMetadata{}, false, fmt.Errorf("open repository: %w", err)
	}
	defer closeCheckoutRepository(repo, &err)
	worktree, err := repo.Worktree()
	if err != nil {
		return CheckoutMetadata{}, false, fmt.Errorf("open worktree: %w", err)
	}
	root := filepath.Clean(worktree.Filesystem.Root())
	gitDir, commonDir, err := checkoutGitDirs(root)
	if err != nil {
		return CheckoutMetadata{}, false, err
	}
	head, err := repo.Reference(plumbing.HEAD, false)
	if err != nil {
		return CheckoutMetadata{}, false, fmt.Errorf("read HEAD: %w", err)
	}
	branch := ""
	if head.Type() == plumbing.SymbolicReference {
		if head.Target().IsBranch() {
			branch = head.Target().Short()
		}
		refRepo := repo
		if gitDir != commonDir {
			refRepo, err = gogit.PlainOpen(commonDir)
			if err != nil {
				return CheckoutMetadata{}, false, fmt.Errorf("open common repository: %w", err)
			}
			defer closeCheckoutRepository(refRepo, &err)
		}
		head, err = refRepo.Reference(head.Target(), true)
		if errors.Is(err, plumbing.ErrReferenceNotFound) && branch != "" {
			return CheckoutMetadata{
				WorktreeRoot: root,
				GitDir:       gitDir,
				CommonDir:    commonDir,
				Branch:       branch,
			}, true, nil
		}
		if err != nil {
			return CheckoutMetadata{}, false, fmt.Errorf("resolve HEAD: %w", err)
		}
	}
	return CheckoutMetadata{
		WorktreeRoot: root,
		GitDir:       gitDir,
		CommonDir:    commonDir,
		Head:         head.Hash().String(),
		Branch:       branch,
	}, false, nil
}

func closeCheckoutRepository(repo *gogit.Repository, readErr *error) {
	closer, ok := repo.Storer.(interface{ Close() error })
	if !ok {
		return
	}
	if err := closer.Close(); err != nil && *readErr == nil {
		*readErr = fmt.Errorf("close repository: %w", err)
	}
}

func checkoutGitDirs(worktreeRoot string) (string, string, error) {
	dotGit := filepath.Join(worktreeRoot, ".git")
	info, err := os.Stat(dotGit)
	if err != nil {
		return "", "", fmt.Errorf("stat git directory: %w", err)
	}
	if info.IsDir() {
		return dotGit, dotGit, nil
	}

	data, err := os.ReadFile(dotGit)
	if err != nil {
		return "", "", fmt.Errorf("read git directory file: %w", err)
	}
	gitDirText, ok := strings.CutPrefix(strings.TrimSpace(string(data)), "gitdir:")
	if !ok || strings.TrimSpace(gitDirText) == "" {
		return "", "", fmt.Errorf("invalid git directory file %s", dotGit)
	}
	gitDir := filepath.Clean(strings.TrimSpace(gitDirText))
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(worktreeRoot, gitDir)
	}
	commonDir := gitDir
	data, err = os.ReadFile(filepath.Join(gitDir, "commondir"))
	switch {
	case err == nil:
		commonDir = filepath.Clean(strings.TrimSpace(string(data)))
		if commonDir == "." || commonDir == "" {
			commonDir = gitDir
		} else if !filepath.IsAbs(commonDir) {
			commonDir = filepath.Join(gitDir, commonDir)
		}
	case os.IsNotExist(err):
	default:
		return "", "", fmt.Errorf("read common git directory: %w", err)
	}
	return filepath.Clean(gitDir), filepath.Clean(commonDir), nil
}
