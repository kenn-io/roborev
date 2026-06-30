package git

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// EnqueueMetadataReader serves the cheap git metadata reads needed while
// freezing an enqueue target. Implementations must not compute patch-id or
// main-repo-root; those stay on their existing subprocess paths.
type EnqueueMetadataReader interface {
	Root() (string, error)
	CurrentBranch() string
	Resolve(ref string) (string, error)
	CommitInfo(ref string) (*CommitInfo, error)
	RangeCommits(rangeRef string) ([]string, error)
}

var openGoGitRepository = func(path string) (*gogit.Repository, error) {
	return gogit.PlainOpenWithOptions(path, &gogit.PlainOpenOptions{
		DetectDotGit:          true,
		EnableDotGitCommonDir: true,
	})
}

// OpenEnqueueMetadataReader uses go-git when it can open the repository, and
// falls back to the existing subprocess helpers for repositories go-git cannot
// read yet, such as reftable, SHA-256, or partial clones with missing objects.
func OpenEnqueueMetadataReader(ctx context.Context, repoPath string) EnqueueMetadataReader {
	fallback := subprocessEnqueueMetadataReader{ctx: ctx, repoPath: repoPath}
	if hasDotGitFileAncestor(repoPath) {
		return fallback
	}
	repo, err := openGoGitRepository(repoPath)
	if err != nil {
		return fallback
	}
	return &goGitEnqueueMetadataReader{repo: repo, fallback: fallback}
}

func hasDotGitFileAncestor(path string) bool {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	for {
		info, err := os.Stat(filepath.Join(abs, ".git"))
		if err == nil {
			return !info.IsDir()
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			return false
		}
		abs = parent
	}
}

type subprocessEnqueueMetadataReader struct {
	ctx      context.Context
	repoPath string
}

func (r subprocessEnqueueMetadataReader) Root() (string, error) {
	return GetRepoRoot(r.repoPath)
}

func (r subprocessEnqueueMetadataReader) CurrentBranch() string {
	return GetCurrentBranch(r.repoPath)
}

func (r subprocessEnqueueMetadataReader) Resolve(ref string) (string, error) {
	return ResolveSHACtx(r.ctx, r.repoPath, ref)
}

func (r subprocessEnqueueMetadataReader) CommitInfo(ref string) (*CommitInfo, error) {
	return GetCommitInfoCtx(r.ctx, r.repoPath, ref)
}

func (r subprocessEnqueueMetadataReader) RangeCommits(rangeRef string) ([]string, error) {
	return GetRangeCommitsCtx(r.ctx, r.repoPath, rangeRef)
}

type goGitEnqueueMetadataReader struct {
	mu       sync.Mutex
	repo     *gogit.Repository
	fallback subprocessEnqueueMetadataReader
	disabled bool
}

func (r *goGitEnqueueMetadataReader) fallbackDisabled() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.disabled
}

func (r *goGitEnqueueMetadataReader) disableGoGit() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.disabled = true
}

func (r *goGitEnqueueMetadataReader) Root() (string, error) {
	if r.fallbackDisabled() {
		return r.fallback.Root()
	}
	wt, err := r.repo.Worktree()
	if err != nil {
		r.disableGoGit()
		return r.fallback.Root()
	}
	return filepath.Clean(wt.Filesystem.Root()), nil
}

func (r *goGitEnqueueMetadataReader) CurrentBranch() string {
	if r.fallbackDisabled() {
		return r.fallback.CurrentBranch()
	}
	head, err := r.repo.Head()
	if err != nil || !head.Name().IsBranch() {
		if err != nil {
			r.disableGoGit()
			return r.fallback.CurrentBranch()
		}
		return ""
	}
	return head.Name().Short()
}

func (r *goGitEnqueueMetadataReader) Resolve(ref string) (string, error) {
	if r.fallbackDisabled() {
		return r.fallback.Resolve(ref)
	}
	hash, err := r.repo.ResolveRevision(plumbing.Revision(ref))
	if err != nil {
		r.disableGoGit()
		return r.fallback.Resolve(ref)
	}
	return hash.String(), nil
}

func (r *goGitEnqueueMetadataReader) CommitInfo(ref string) (*CommitInfo, error) {
	if r.fallbackDisabled() {
		return r.fallback.CommitInfo(ref)
	}
	hash, err := r.repo.ResolveRevision(plumbing.Revision(ref))
	if err != nil {
		r.disableGoGit()
		return r.fallback.CommitInfo(ref)
	}
	commit, err := r.repo.CommitObject(*hash)
	if err != nil {
		r.disableGoGit()
		return r.fallback.CommitInfo(ref)
	}
	return commitInfoFromGoGit(commit), nil
}

func commitInfoFromGoGit(commit *object.Commit) *CommitInfo {
	subject, body := splitCommitMessage(commit.Message)
	timestamp := commit.Author.When
	if timestamp.IsZero() {
		timestamp = time.Now()
	}
	return &CommitInfo{
		SHA:       commit.Hash.String(),
		Author:    commit.Author.Name,
		Subject:   subject,
		Body:      body,
		Timestamp: timestamp,
	}
}

func splitCommitMessage(message string) (string, string) {
	message = strings.TrimRight(message, "\r\n")
	subject, body, _ := strings.Cut(message, "\n")
	return subject, strings.TrimSpace(body)
}

func (r *goGitEnqueueMetadataReader) RangeCommits(rangeRef string) ([]string, error) {
	return r.fallback.RangeCommits(rangeRef)
}
