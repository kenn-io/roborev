package git

import (
	"fmt"
	"strings"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
)

// A handful of simple, high-frequency, read-only git operations are served by
// the pure-Go go-git library instead of shelling out to the git CLI. On Windows
// each `git` subprocess costs ~50ms (CreateProcess + git startup + antivirus
// scan of git.exe); these in-process implementations avoid the spawn entirely,
// which is the dominant cost of the test suite and of live reviews on
// Windows-on-ARM. Operations whose behavior is format- or pathspec-sensitive
// (diffs and their exclude pathspecs, worktree management, patch-id) stay on the
// git CLI, where matching git's exact output is what matters.

// openRepoGoGit opens the repository containing repoPath, walking up to find
// the .git directory so subdirectories and linked worktrees resolve the same
// way `git -C repoPath` would.
func openRepoGoGit(repoPath string) (*gogit.Repository, error) {
	return gogit.PlainOpenWithOptions(repoPath, &gogit.PlainOpenOptions{DetectDotGit: true})
}

// splitCommitMessage splits a raw commit message into git's subject (%s, the
// first line) and body (%b, everything after the first line, with the blank
// separator and surrounding whitespace trimmed).
func splitCommitMessage(msg string) (subject, body string) {
	msg = strings.ReplaceAll(msg, "\r\n", "\n")
	trimmed := strings.TrimRight(msg, "\n")
	if nl := strings.IndexByte(trimmed, '\n'); nl >= 0 {
		return strings.TrimSpace(trimmed[:nl]), strings.TrimSpace(trimmed[nl+1:])
	}
	return strings.TrimSpace(trimmed), ""
}

// GetCommitInfo retrieves commit metadata.
func GetCommitInfo(repoPath, sha string) (*CommitInfo, error) {
	repo, err := openRepoGoGit(repoPath)
	if err != nil {
		return nil, fmt.Errorf("git log: %w", err)
	}
	hash, err := repo.ResolveRevision(plumbing.Revision(sha))
	if err != nil {
		return nil, fmt.Errorf("git log: %w", err)
	}
	commit, err := repo.CommitObject(*hash)
	if err != nil {
		return nil, fmt.Errorf("git log: %w", err)
	}
	subject, body := splitCommitMessage(commit.Message)
	return &CommitInfo{
		SHA:       commit.Hash.String(),
		Author:    commit.Author.Name,
		Subject:   subject,
		Body:      body,
		Timestamp: commit.Author.When,
	}, nil
}

// IsAncestor checks if ancestor is an ancestor of descendant.
// Returns (true, nil) if ancestor is reachable from descendant via the commit graph.
// Returns (false, nil) if ancestor is not an ancestor.
// Returns (false, error) for git errors (e.g., bad object, repo issues).
func IsAncestor(repoPath, ancestor, descendant string) (bool, error) {
	repo, err := openRepoGoGit(repoPath)
	if err != nil {
		return false, fmt.Errorf("git merge-base --is-ancestor: %w", err)
	}
	ancestorHash, err := repo.ResolveRevision(plumbing.Revision(ancestor))
	if err != nil {
		return false, fmt.Errorf("git merge-base --is-ancestor: %w", err)
	}
	descendantHash, err := repo.ResolveRevision(plumbing.Revision(descendant))
	if err != nil {
		return false, fmt.Errorf("git merge-base --is-ancestor: %w", err)
	}
	ancestorCommit, err := repo.CommitObject(*ancestorHash)
	if err != nil {
		return false, fmt.Errorf("git merge-base --is-ancestor: %w", err)
	}
	descendantCommit, err := repo.CommitObject(*descendantHash)
	if err != nil {
		return false, fmt.Errorf("git merge-base --is-ancestor: %w", err)
	}
	return ancestorCommit.IsAncestor(descendantCommit)
}
