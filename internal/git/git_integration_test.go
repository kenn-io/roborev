//go:build integration

package git

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetMainRepoRoot(t *testing.T) {
	t.Run("regular repo returns same as GetRepoRoot", func(t *testing.T) {
		repo := NewTestRepo(t)

		mainRoot, err := GetMainRepoRoot(repo.Dir)
		if err != nil {
			require.Condition(t, func() bool {
				return false
			}, "GetMainRepoRoot failed: %v", err)
		}

		repoRoot, err := GetRepoRoot(repo.Dir)
		if err != nil {
			require.Condition(t, func() bool {
				return false
			}, "GetRepoRoot failed: %v", err)
		}

		if mainRoot != repoRoot {
			assert.Condition(t, func() bool {
				return false
			}, "GetMainRepoRoot returned %s, expected %s (same as GetRepoRoot)", mainRoot, repoRoot)
		}
	})

	t.Run("worktree returns main repo root", func(t *testing.T) {
		repo := NewTestRepo(t)
		repo.CommitFile("file.txt", "content", "initial")

		wt := repo.AddWorktree("worktree-branch")

		// GetRepoRoot from worktree returns the worktree path
		worktreeRoot, err := GetRepoRoot(wt.Dir)
		if err != nil {
			require.Condition(t, func() bool {
				return false
			}, "GetRepoRoot on worktree failed: %v", err)
		}

		mainRepoRoot, err := GetRepoRoot(repo.Dir)
		if err != nil {
			require.Condition(t, func() bool {
				return false
			}, "GetRepoRoot on main repo failed: %v", err)
		}

		if worktreeRoot == mainRepoRoot {
			t.Skip("worktree root equals main repo root - older git version")
		}

		mainRoot, err := GetMainRepoRoot(wt.Dir)
		if err != nil {
			require.Condition(t, func() bool {
				return false
			}, "GetMainRepoRoot on worktree failed: %v", err)
		}

		if mainRoot != mainRepoRoot {
			assert.Condition(t, func() bool {
				return false
			}, "GetMainRepoRoot on worktree returned %s, expected %s", mainRoot, mainRepoRoot)
		}
	})

	t.Run("non-repo returns error", func(t *testing.T) {
		nonRepo := t.TempDir()
		_, err := GetMainRepoRoot(nonRepo)
		if err == nil {
			assert.Condition(t, func() bool {
				return false
			}, "expected error for non-repo")
		}
	})

	t.Run("submodule stays distinct from parent", func(t *testing.T) {
		parentRepo := NewTestRepo(t)
		parentRepo.CommitFile("parent.txt", "parent", "parent initial")

		subSource := NewTestRepo(t)
		subSource.CommitFile("sub.txt", "sub", "sub initial")

		// Add submodule to parent
		parentRepo.AddSubmodule(t, subSource.Dir, "mysub")

		submoduleDir := filepath.Join(parentRepo.Dir, "mysub")
		submoduleDirResolved, _ := filepath.EvalSymlinks(submoduleDir)
		if submoduleDirResolved == "" {
			submoduleDirResolved = submoduleDir
		}

		subRoot, err := GetMainRepoRoot(submoduleDir)
		if err != nil {
			require.Condition(t, func() bool {
				return false
			}, "GetMainRepoRoot on submodule failed: %v", err)
		}

		parentRoot, err := GetMainRepoRoot(parentRepo.Dir)
		if err != nil {
			require.Condition(t, func() bool {
				return false
			}, "GetMainRepoRoot on parent failed: %v", err)
		}

		if subRoot == parentRoot {
			assert.Condition(t, func() bool {
				return false
			}, "submodule root should be distinct from parent: sub=%s parent=%s", subRoot, parentRoot)
		}

		subRootResolved, _ := filepath.EvalSymlinks(subRoot)
		if subRootResolved == "" {
			subRootResolved = subRoot
		}
		if subRootResolved != submoduleDirResolved {
			assert.Condition(t, func() bool {
				return false
			}, "GetMainRepoRoot on submodule returned %s, expected %s", subRoot, submoduleDir)
		}
	})

	t.Run("worktree from submodule returns submodule root", func(t *testing.T) {
		parentRepo := NewTestRepo(t)
		parentRepo.CommitFile("parent.txt", "parent", "parent initial")

		subSource := NewTestRepo(t)
		subSource.CommitFile("sub.txt", "sub", "sub initial")

		// Add submodule to parent
		parentRepo.AddSubmodule(t, subSource.Dir, "mysub")
		parentRepo.CommitAll("add submodule")

		submoduleDir := filepath.Join(parentRepo.Dir, "mysub")

		// Create a worktree from within the submodule
		subRepo := &TestRepo{T: t, Dir: submoduleDir}
		wt := subRepo.AddWorktree("sub-wt-branch")
		worktreeDir := wt.Dir

		wtRoot, err := GetMainRepoRoot(worktreeDir)
		if err != nil {
			require.Condition(t, func() bool {
				return false
			}, "GetMainRepoRoot on submodule worktree failed: %v", err)
		}

		subRoot, err := GetMainRepoRoot(submoduleDir)
		if err != nil {
			require.Condition(t, func() bool {
				return false
			}, "GetMainRepoRoot on submodule failed: %v", err)
		}

		if wtRoot != subRoot {
			assert.Condition(t, func() bool {
				return false
			}, "worktree from submodule should return submodule root: wt=%s sub=%s", wtRoot, subRoot)
		}

		parentRoot, err := GetMainRepoRoot(parentRepo.Dir)
		if err != nil {
			require.Condition(t, func() bool {
				return false
			}, "GetMainRepoRoot on parent failed: %v", err)
		}

		if wtRoot == parentRoot {
			assert.Condition(t, func() bool {
				return false
			}, "worktree from submodule should NOT return parent root: wt=%s parent=%s", wtRoot, parentRoot)
		}

		if info, err := os.Stat(wtRoot); err != nil || !info.IsDir() {
			assert.Condition(t, func() bool {
				return false
			}, "GetMainRepoRoot returned invalid path: %s", wtRoot)
		}
	})

	t.Run("worktree HEAD resolves to worktree branch", func(t *testing.T) {
		mainRepo := NewTestRepo(t)
		mainRepo.CommitFile("file.txt", "v1", "commit1")

		mainHead, err := ResolveSHA(mainRepo.Dir, "HEAD")
		if err != nil {
			require.Condition(t, func() bool {
				return false
			}, "ResolveSHA main HEAD failed: %v", err)
		}

		wt := mainRepo.AddWorktree("wt-branch")

		// Make a new commit in the worktree
		wt.CommitFile("file.txt", "v2", "commit2")

		wtHead, err := ResolveSHA(wt.Dir, "HEAD")
		if err != nil {
			require.Condition(t, func() bool {
				return false
			}, "ResolveSHA worktree HEAD failed: %v", err)
		}

		if wtHead == mainHead {
			assert.Condition(t, func() bool {
				return false
			}, "worktree HEAD should differ from main HEAD after new commit")
		}

		mainHeadAgain, err := ResolveSHA(mainRepo.Dir, "HEAD")
		if err != nil {
			require.Condition(t, func() bool {
				return false
			}, "ResolveSHA main HEAD again failed: %v", err)
		}
		if mainHeadAgain != mainHead {
			assert.Condition(t, func() bool {
				return false
			}, "main HEAD changed unexpectedly: was %s, now %s", mainHead, mainHeadAgain)
		}

		mainRoot, _ := GetMainRepoRoot(mainRepo.Dir)
		wtRoot, _ := GetMainRepoRoot(wt.Dir)
		if mainRoot != wtRoot {
			assert.Condition(t, func() bool {
				return false
			}, "GetMainRepoRoot should return same root: main=%s wt=%s", mainRoot, wtRoot)
		}
	})
}

func setupBranchOriginTest(t *testing.T) (*TestRepo, *TestRepo) {
	t.Helper()
	bareRepo := NewBareTestRepo(t)
	bareRepo.Run("symbolic-ref", "HEAD", "refs/heads/main")

	seedRepo := NewTestRepo(t)
	seedRepo.Run("checkout", "-b", "main")
	seedRepo.CommitFile("file.txt", "base", "initial")
	seedRepo.Run("remote", "add", "origin", bareRepo.Dir)
	seedRepo.Run("push", "-u", "origin", "main")

	return bareRepo, seedRepo
}

func TestGetDefaultBranchOriginHead(t *testing.T) {
	t.Run("missing local branch uses origin ref", func(t *testing.T) {
		bareRepo, _ := setupBranchOriginTest(t)
		clone := CloneTestRepo(t, bareRepo.Dir)
		clone.Run("remote", "set-head", "origin", "-a")
		clone.Run("checkout", "--detach")
		clone.Run("branch", "-D", "main")

		branch, err := GetDefaultBranch(clone.Dir)
		if err != nil {
			require.Condition(t, func() bool {
				return false
			}, "GetDefaultBranch failed: %v", err)
		}
		if branch != "origin/main" {
			require.Condition(t, func() bool {
				return false
			}, "expected origin/main, got %s", branch)
		}
	})

	t.Run("stale local branch uses origin ref", func(t *testing.T) {
		bareRepo, seedRepo := setupBranchOriginTest(t)
		clone := CloneTestRepo(t, bareRepo.Dir)
		clone.Run("remote", "set-head", "origin", "-a")

		seedRepo.CommitFile("file2.txt", "new", "update")
		seedRepo.Run("push")
		clone.Run("fetch", "origin")

		branch, err := GetDefaultBranch(clone.Dir)
		if err != nil {
			require.Condition(t, func() bool {
				return false
			}, "GetDefaultBranch failed: %v", err)
		}
		if branch != "origin/main" {
			require.Condition(t, func() bool {
				return false
			}, "expected origin/main, got %s", branch)
		}
	})

	t.Run("origin/HEAD points to missing remote ref, falls back to local branch", func(t *testing.T) {
		bareRepo, _ := setupBranchOriginTest(t)
		clone := CloneTestRepo(t, bareRepo.Dir)
		clone.Run("remote", "set-head", "origin", "-a")

		// Delete the remote-tracking branch while keeping origin/HEAD symbolic ref intact
		clone.Run("update-ref", "-d", "refs/remotes/origin/main")

		// Local main branch should still exist
		localBranchOut := clone.Run("rev-parse", "--verify", "main")
		if localBranchOut == "" {
			require.Condition(t, func() bool {
				return false
			}, "expected local main branch to exist")
		}

		branch, err := GetDefaultBranch(clone.Dir)
		if err != nil {
			require.Condition(t, func() bool {
				return false
			}, "GetDefaultBranch failed: %v", err)
		}
		if branch != "main" {
			require.Condition(t, func() bool {
				return false
			}, "expected main (local branch fallback), got %s", branch)
		}
	})
}
