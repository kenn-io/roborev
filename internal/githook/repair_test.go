package githook

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/testutil"
)

func writeExecutableFile(t *testing.T, path string) string {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755))
	return path
}

func TestRepairRepoHooks(t *testing.T) {
	t.Parallel()

	t.Run("rewrites managed hooks to new binary", func(t *testing.T) {
		t.Parallel()
		repo := testutil.NewTestRepo(t)
		oldBinary := writeExecutableFile(t, filepath.Join(t.TempDir(), "roborev"))
		newBinary := writeExecutableFile(t, filepath.Join(t.TempDir(), "roborev"))
		repo.WriteHook(GeneratePostCommitWithBinary(oldBinary))
		repo.WriteNamedHook(hookPostRewrite, GeneratePostRewriteWithBinary(oldBinary))

		found, err := RepairRepoHooks(t.Context(), repo.Root, newBinary)
		require.NoError(t, err)
		assert.True(t, found)

		for _, name := range []string{hookPostCommit, hookPostRewrite} {
			content, err := os.ReadFile(repo.GetHookPath(name))
			require.NoError(t, err)
			// The hook bakes the path via %q, so compare the quoted form
			// (on Windows the raw path differs by backslash escaping).
			assert.Contains(t, string(content), fmt.Sprintf("ROBOREV=%q", newBinary), "%s should point at new binary", name)
			assert.NotContains(t, string(content), fmt.Sprintf("ROBOREV=%q", oldBinary), "%s should not point at old binary", name)
		}
	})

	t.Run("leaves unmanaged hooks alone", func(t *testing.T) {
		t.Parallel()
		repo := testutil.NewTestRepo(t)
		custom := "#!/bin/sh\necho custom\n"
		repo.WriteHook(custom)

		found, err := RepairRepoHooks(t.Context(), repo.Root, writeExecutableFile(t, filepath.Join(t.TempDir(), "roborev")))
		require.NoError(t, err)
		assert.False(t, found)

		content, err := os.ReadFile(repo.GetHookPath(hookPostCommit))
		require.NoError(t, err)
		assert.Equal(t, custom, string(content))
	})

	t.Run("no hooks installed", func(t *testing.T) {
		t.Parallel()
		repo := testutil.NewTestRepo(t)

		found, err := RepairRepoHooks(t.Context(), repo.Root, writeExecutableFile(t, filepath.Join(t.TempDir(), "roborev")))
		require.NoError(t, err)
		assert.False(t, found)
		assert.NoFileExists(t, repo.GetHookPath(hookPostCommit))
	})

	t.Run("not a git repo", func(t *testing.T) {
		t.Parallel()
		found, err := RepairRepoHooks(t.Context(), t.TempDir(), writeExecutableFile(t, filepath.Join(t.TempDir(), "roborev")))
		require.NoError(t, err)
		assert.False(t, found)
	})

	t.Run("repairs one hook when the other is unmanaged", func(t *testing.T) {
		t.Parallel()
		repo := testutil.NewTestRepo(t)
		oldBinary := writeExecutableFile(t, filepath.Join(t.TempDir(), "roborev"))
		newBinary := writeExecutableFile(t, filepath.Join(t.TempDir(), "roborev"))
		repo.WriteHook(GeneratePostCommitWithBinary(oldBinary))
		custom := "#!/bin/sh\necho custom rewrite\n"
		repo.WriteNamedHook(hookPostRewrite, custom)

		found, err := RepairRepoHooks(t.Context(), repo.Root, newBinary)
		require.NoError(t, err)
		assert.True(t, found)

		postCommit, err := os.ReadFile(repo.GetHookPath(hookPostCommit))
		require.NoError(t, err)
		assert.Contains(t, string(postCommit), fmt.Sprintf("ROBOREV=%q", newBinary))

		postRewrite, err := os.ReadFile(repo.GetHookPath(hookPostRewrite))
		require.NoError(t, err)
		assert.Equal(t, custom, string(postRewrite))
	})
}

func TestHookBinaryStale(t *testing.T) {
	t.Parallel()

	t.Run("managed hook with different binary is stale", func(t *testing.T) {
		t.Parallel()
		repo := testutil.NewTestRepo(t)
		oldBinary := writeExecutableFile(t, filepath.Join(t.TempDir(), "roborev"))
		repo.WriteHook(GeneratePostCommitWithBinary(oldBinary))
		newBinary := filepath.Join(t.TempDir(), "roborev")
		assert.True(t, HookBinaryStale(t.Context(), repo.Root, hookPostCommit, newBinary))
	})

	t.Run("managed hook with matching binary is current", func(t *testing.T) {
		t.Parallel()
		repo := testutil.NewTestRepo(t)
		binary := writeExecutableFile(t, filepath.Join(t.TempDir(), "roborev"))
		repo.WriteHook(GeneratePostCommitWithBinary(binary))
		assert.False(t, HookBinaryStale(t.Context(), repo.Root, hookPostCommit, binary))
	})

	t.Run("unmanaged hook is not stale", func(t *testing.T) {
		t.Parallel()
		repo := testutil.NewTestRepo(t)
		repo.WriteHook("#!/bin/sh\necho custom\n")
		assert.False(t, HookBinaryStale(t.Context(), repo.Root, hookPostCommit, "/new/roborev"))
	})

	t.Run("missing hook is not stale", func(t *testing.T) {
		t.Parallel()
		repo := testutil.NewTestRepo(t)
		assert.False(t, HookBinaryStale(t.Context(), repo.Root, hookPostCommit, "/new/roborev"))
	})
}

func TestHooksDirInsideGitDir(t *testing.T) {
	t.Parallel()

	t.Run("default hooks dir is inside git dir", func(t *testing.T) {
		t.Parallel()
		repo := testutil.NewTestRepo(t)
		inside, err := HooksDirInsideGitDir(t.Context(), repo.Root)
		require.NoError(t, err)
		assert.True(t, inside)
	})

	t.Run("relative hooksPath in worktree is outside", func(t *testing.T) {
		t.Parallel()
		repo := testutil.NewTestRepo(t)
		require.NoError(t, os.MkdirAll(filepath.Join(repo.Root, ".githooks"), 0o755))
		repo.Run("config", "core.hooksPath", ".githooks")
		inside, err := HooksDirInsideGitDir(t.Context(), repo.Root)
		require.NoError(t, err)
		assert.False(t, inside)
	})

	t.Run("absolute external hooksPath is outside", func(t *testing.T) {
		t.Parallel()
		repo := testutil.NewTestRepo(t)
		external := t.TempDir()
		repo.Run("config", "core.hooksPath", external)
		inside, err := HooksDirInsideGitDir(t.Context(), repo.Root)
		require.NoError(t, err)
		assert.False(t, inside)
	})

	t.Run("linked worktree default hooks dir is inside common dir", func(t *testing.T) {
		t.Parallel()
		repo := testutil.NewTestRepoWithCommit(t)
		repo.Run("branch", "hooks-wt")
		wtDir := filepath.Join(t.TempDir(), "wt")
		repo.Run("worktree", "add", wtDir, "hooks-wt")

		inside, err := HooksDirInsideGitDir(t.Context(), wtDir)
		require.NoError(t, err)
		assert.True(t, inside)
	})

	t.Run("linked worktree hooksPath in main worktree is outside", func(t *testing.T) {
		t.Parallel()
		repo := testutil.NewTestRepoWithCommit(t)
		require.NoError(t, os.MkdirAll(filepath.Join(repo.Root, ".githooks"), 0o755))
		repo.Run("config", "core.hooksPath", ".githooks")
		repo.Run("branch", "hooks-wt")
		wtDir := filepath.Join(t.TempDir(), "wt")
		repo.Run("worktree", "add", wtDir, "hooks-wt")

		inside, err := HooksDirInsideGitDir(t.Context(), wtDir)
		require.NoError(t, err)
		assert.False(t, inside, "hooks dir resolves into the main worktree and must not be daemon-writable")
	})

	t.Run("hooks dir symlinked into worktree is outside", func(t *testing.T) {
		t.Parallel()
		if runtime.GOOS == "windows" {
			t.Skip("symlink creation requires elevated privileges on Windows")
		}
		repo := testutil.NewTestRepo(t)
		target := filepath.Join(repo.Root, ".githooks")
		require.NoError(t, os.MkdirAll(target, 0o755))
		hooksDir := filepath.Join(repo.Root, ".git", "hooks")
		require.NoError(t, os.RemoveAll(hooksDir))
		require.NoError(t, os.Symlink(target, hooksDir))

		inside, err := HooksDirInsideGitDir(t.Context(), repo.Root)
		require.NoError(t, err)
		assert.False(t, inside, "symlinked hooks dir escapes the git dir into the worktree")
	})

	t.Run("not a git repo returns error", func(t *testing.T) {
		t.Parallel()
		_, err := HooksDirInsideGitDir(t.Context(), t.TempDir())
		assert.Error(t, err)
	})
}
