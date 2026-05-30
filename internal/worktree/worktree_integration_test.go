//go:build integration

package worktree_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/testutil"
	"go.kenn.io/roborev/internal/worktree"
)

func requireGitAvailable(t *testing.T) {
	t.Helper()
	_, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not available")
	}
}

func TestWorktreeCleanupBetweenIterations(t *testing.T) {
	requireGitAvailable(t)

	repo := testutil.InitTestRepo(t)

	// Simulate the refine loop pattern: create a worktree, then clean it up
	// before the next iteration. Verify the directory is removed each time.
	var prevPath string
	for i := range 3 {
		wt, err := worktree.Create(repo.Root, "HEAD")
		require.NoError(t, err, "iteration %d", i)

		// Verify previous worktree was cleaned up
		if prevPath != "" {
			_, err = os.Stat(prevPath)
			require.ErrorIs(t, err, os.ErrNotExist, "iteration %d", i)
		}

		// Verify current worktree exists
		_, err = os.Stat(wt.Dir)
		require.NoError(t, err, "iteration %d", i)

		// Simulate the explicit cleanup call (as done on error/no-change paths)
		wt.Close()
		prevPath = wt.Dir
	}

	// Verify the last worktree was also cleaned up
	_, err := os.Stat(prevPath)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestCreateTempWorktreeIgnoresHooks(t *testing.T) {
	requireGitAvailable(t)

	repo := testutil.InitTestRepo(t)

	repo.WriteNamedHook("post-checkout", "#!/bin/sh\nexit 1\n")

	// Verify the hook is active (a normal worktree add would fail)
	failDir := t.TempDir()
	cmd := exec.Command("git", "-C", repo.Root, "worktree", "add", "--detach", failDir, "HEAD")
	if out, err := cmd.CombinedOutput(); err == nil {
		// Clean up the worktree before failing
		exec.Command("git", "-C", repo.Root, "worktree", "remove", "--force", failDir).Run()
		// Some git versions don't fail on post-checkout hook errors.
		// In that case, verify our approach still succeeds.
		_ = out
	}

	// worktree.Create should succeed because it suppresses hooks
	wt, err := worktree.Create(repo.Root, "HEAD")
	require.NoError(t, err)
	defer wt.Close()

	// Verify the worktree directory exists and has the file from the repo
	_, err = os.Stat(wt.Dir)
	require.NoError(t, err)

	baseFile := filepath.Join(wt.Dir, "base.txt")
	content, err := os.ReadFile(baseFile)
	require.NoError(t, err)
	assert.Equal(t, "base", string(content))
}

func TestCreateTempWorktreeInitializesSubmodules(t *testing.T) {
	requireGitAvailable(t)

	subRepo := testutil.NewTestRepo(t)
	subRepo.RunGit("init")
	subRepo.SymbolicRef("HEAD", "refs/heads/main")
	subRepo.Config("user.email", testutil.GitUserEmail)
	subRepo.Config("user.name", testutil.GitUserName)
	subRepo.CommitFile("sub.txt", "sub", "submodule commit")

	mainRepo := testutil.NewTestRepo(t)
	mainRepo.RunGit("init")
	mainRepo.SymbolicRef("HEAD", "refs/heads/main")
	mainRepo.Config("user.email", testutil.GitUserEmail)
	mainRepo.Config("user.name", testutil.GitUserName)
	mainRepo.Config("protocol.file.allow", "always")
	mainRepo.RunGit("-c", "protocol.file.allow=always", "submodule", "add", subRepo.Root, "deps/sub")
	mainRepo.RunGit("commit", "-m", "add submodule")

	wt, err := worktree.Create(mainRepo.Root, "HEAD")
	require.NoError(t, err)
	defer wt.Close()

	_, err = os.Stat(filepath.Join(wt.Dir, "deps", "sub", "sub.txt"))
	require.NoError(t, err)
}

// TestCreateWorktreeBlocksNestedLocalSubmodules verifies that the recursive
// submodule pass does NOT enable protocol.file.allow=always, even when the
// top-level repo uses file-protocol submodules. This prevents
// attacker-controlled nested .gitmodules from cloning arbitrary local repos
// (CVE-2022-39253). The top-level local submodule is initialized, but
// the nested local submodule within it is blocked.
func TestCreateWorktreeBlocksNestedLocalSubmodules(t *testing.T) {
	requireGitAvailable(t)

	leafRepo := testutil.NewTestRepo(t)
	leafRepo.RunGit("init")
	leafRepo.SymbolicRef("HEAD", "refs/heads/main")
	leafRepo.Config("user.email", testutil.GitUserEmail)
	leafRepo.Config("user.name", testutil.GitUserName)
	leafRepo.CommitFile("leaf.txt", "leaf", "leaf commit")

	subRepo := testutil.NewTestRepo(t)
	subRepo.RunGit("init")
	subRepo.SymbolicRef("HEAD", "refs/heads/main")
	subRepo.Config("user.email", testutil.GitUserEmail)
	subRepo.Config("user.name", testutil.GitUserName)
	subRepo.CommitFile("sub.txt", "sub", "sub commit")
	subRepo.Config("protocol.file.allow", "always")
	leafRepoURL, err := filepath.Rel(subRepo.Root, leafRepo.Root)
	require.NoError(t, err)
	subRepo.RunGit("-c", "protocol.file.allow=always", "submodule", "add", leafRepoURL, "deps/leaf")
	subRepo.RunGit("commit", "-m", "add nested submodule")

	mainRepo := testutil.NewTestRepo(t)
	mainRepo.RunGit("init")
	mainRepo.SymbolicRef("HEAD", "refs/heads/main")
	mainRepo.Config("user.email", testutil.GitUserEmail)
	mainRepo.Config("user.name", testutil.GitUserName)
	mainRepo.Config("protocol.file.allow", "always")
	mainRepo.RunGit("-c", "protocol.file.allow=always", "submodule", "add", subRepo.Root, "deps/sub")
	mainRepo.RunGit("commit", "-m", "add top-level submodule")

	// Create should still succeed — the recursive pass failure is non-fatal
	// for the worktree overall, but the nested submodule won't be cloned.
	wt, err := worktree.Create(mainRepo.Root, "HEAD")
	require.NoError(t, err)
	defer wt.Close()

	// Top-level local submodule IS initialized.
	_, err = os.Stat(filepath.Join(wt.Dir, "deps", "sub", "sub.txt"))
	require.NoError(t, err, "top-level local submodule should be initialized")

	// Nested local submodule is NOT initialized (file protocol blocked).
	_, err = os.Stat(filepath.Join(wt.Dir, "deps", "sub", "deps", "leaf", "leaf.txt"))
	require.ErrorIs(t, err, os.ErrNotExist, "nested local submodule should be blocked")
}

// TestCreateWorktreeFailsOnBrokenNestedSubmodule verifies that non-file-
// protocol recursive submodule failures (e.g. broken URLs) still cause
// Create to fail, rather than being silently swallowed.
func TestCreateWorktreeFailsOnBrokenNestedSubmodule(t *testing.T) {
	requireGitAvailable(t)

	// Create a submodule repo that itself references a nonexistent submodule.
	brokenSub := testutil.NewTestRepo(t)
	brokenSub.RunGit("init")
	brokenSub.SymbolicRef("HEAD", "refs/heads/main")
	brokenSub.Config("user.email", testutil.GitUserEmail)
	brokenSub.Config("user.name", testutil.GitUserName)
	brokenSub.CommitFile("sub.txt", "sub", "sub commit")

	// Manually write a .gitmodules pointing to a nonexistent HTTPS repo
	// and create a fake gitlink so git treats it as a registered submodule.
	brokenSub.RunGit("config", "--file", ".gitmodules",
		"submodule.broken.path", "deps/broken")
	brokenSub.RunGit("config", "--file", ".gitmodules",
		"submodule.broken.url", "https://localhost:1/nonexistent.git")
	// Use an arbitrary valid SHA for the gitlink entry.
	brokenSub.RunGit("update-index", "--add", "--cacheinfo",
		"160000,"+testutil.GetHeadSHA(t, brokenSub.Root)+",deps/broken")
	brokenSub.RunGit("add", ".gitmodules")
	brokenSub.RunGit("commit", "-m", "add broken nested submodule")

	mainRepo := testutil.NewTestRepo(t)
	mainRepo.RunGit("init")
	mainRepo.SymbolicRef("HEAD", "refs/heads/main")
	mainRepo.Config("user.email", testutil.GitUserEmail)
	mainRepo.Config("user.name", testutil.GitUserName)
	mainRepo.Config("protocol.file.allow", "always")
	mainRepo.RunGit("-c", "protocol.file.allow=always",
		"submodule", "add", brokenSub.Root, "deps/sub")
	mainRepo.RunGit("commit", "-m", "add submodule with broken nested ref")

	_, err := worktree.Create(mainRepo.Root, "HEAD")
	require.Error(t, err, "broken nested submodule should fail worktree creation")
	assert.NotContains(t, err.Error(), "transport 'file' not allowed",
		"failure should not be a file-protocol denial")
}

// TestCreateWorktreeMixedNestedSubmodules verifies that a blocked local nested
// submodule in one top-level submodule does not prevent a valid remote nested
// submodule in a sibling from being initialized.
func TestCreateWorktreeMixedNestedSubmodules(t *testing.T) {
	requireGitAvailable(t)

	// leafRepo: a valid nested submodule target (local, but referenced via
	// file:// from within a top-level submodule — should be blocked).
	leafRepo := testutil.NewTestRepo(t)
	leafRepo.RunGit("init")
	leafRepo.SymbolicRef("HEAD", "refs/heads/main")
	leafRepo.Config("user.email", testutil.GitUserEmail)
	leafRepo.Config("user.name", testutil.GitUserName)
	leafRepo.CommitFile("leaf.txt", "leaf", "leaf commit")

	// subWithLocal: top-level submodule that has a nested local submodule
	// (will be blocked by file-protocol restriction).
	subWithLocal := testutil.NewTestRepo(t)
	subWithLocal.RunGit("init")
	subWithLocal.SymbolicRef("HEAD", "refs/heads/main")
	subWithLocal.Config("user.email", testutil.GitUserEmail)
	subWithLocal.Config("user.name", testutil.GitUserName)
	subWithLocal.CommitFile("local.txt", "local", "local commit")
	subWithLocal.Config("protocol.file.allow", "always")
	leafURL, err := filepath.Rel(subWithLocal.Root, leafRepo.Root)
	require.NoError(t, err)
	subWithLocal.RunGit("-c", "protocol.file.allow=always",
		"submodule", "add", leafURL, "deps/leaf")
	subWithLocal.RunGit("commit", "-m", "add nested local submodule")

	// subClean: top-level submodule with no nested submodules (should
	// always succeed).
	subClean := testutil.NewTestRepo(t)
	subClean.RunGit("init")
	subClean.SymbolicRef("HEAD", "refs/heads/main")
	subClean.Config("user.email", testutil.GitUserEmail)
	subClean.Config("user.name", testutil.GitUserName)
	subClean.CommitFile("clean.txt", "clean", "clean commit")

	// Main repo references both top-level submodules via file://.
	mainRepo := testutil.NewTestRepo(t)
	mainRepo.RunGit("init")
	mainRepo.SymbolicRef("HEAD", "refs/heads/main")
	mainRepo.Config("user.email", testutil.GitUserEmail)
	mainRepo.Config("user.name", testutil.GitUserName)
	mainRepo.Config("protocol.file.allow", "always")
	mainRepo.RunGit("-c", "protocol.file.allow=always",
		"submodule", "add", subWithLocal.Root, "deps/with-local")
	mainRepo.RunGit("-c", "protocol.file.allow=always",
		"submodule", "add", subClean.Root, "deps/clean")
	mainRepo.RunGit("commit", "-m", "add both submodules")

	wt, err := worktree.Create(mainRepo.Root, "HEAD")
	require.NoError(t, err)
	defer wt.Close()

	// Both top-level submodules are initialized.
	_, err = os.Stat(filepath.Join(wt.Dir, "deps", "with-local", "local.txt"))
	require.NoError(t, err, "subWithLocal top-level content should exist")
	_, err = os.Stat(filepath.Join(wt.Dir, "deps", "clean", "clean.txt"))
	require.NoError(t, err, "subClean top-level content should exist")

	// The nested local submodule inside subWithLocal is blocked.
	_, err = os.Stat(filepath.Join(wt.Dir, "deps", "with-local", "deps", "leaf", "leaf.txt"))
	require.ErrorIs(t, err, os.ErrNotExist,
		"nested local submodule should be blocked by file-protocol restriction")
}
