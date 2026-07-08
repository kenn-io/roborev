package githook

import (
	"os"
	"path/filepath"
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
			assert.Contains(t, string(content), newBinary, "%s should point at new binary", name)
			assert.NotContains(t, string(content), oldBinary, "%s should not point at old binary", name)
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
		assert.Contains(t, string(postCommit), newBinary)

		postRewrite, err := os.ReadFile(repo.GetHookPath(hookPostRewrite))
		require.NoError(t, err)
		assert.Equal(t, custom, string(postRewrite))
	})
}

func TestHooksDirInsideWorktree(t *testing.T) {
	t.Parallel()

	t.Run("default hooks dir is not inside worktree", func(t *testing.T) {
		t.Parallel()
		repo := testutil.NewTestRepo(t)
		inside, err := HooksDirInsideWorktree(t.Context(), repo.Root)
		require.NoError(t, err)
		assert.False(t, inside)
	})

	t.Run("relative hooksPath inside worktree", func(t *testing.T) {
		t.Parallel()
		repo := testutil.NewTestRepo(t)
		require.NoError(t, os.MkdirAll(filepath.Join(repo.Root, ".githooks"), 0o755))
		repo.Run("config", "core.hooksPath", ".githooks")
		inside, err := HooksDirInsideWorktree(t.Context(), repo.Root)
		require.NoError(t, err)
		assert.True(t, inside)
	})

	t.Run("absolute hooksPath outside worktree", func(t *testing.T) {
		t.Parallel()
		repo := testutil.NewTestRepo(t)
		external := t.TempDir()
		repo.Run("config", "core.hooksPath", external)
		inside, err := HooksDirInsideWorktree(t.Context(), repo.Root)
		require.NoError(t, err)
		assert.False(t, inside)
	})

	t.Run("not a git repo returns error", func(t *testing.T) {
		t.Parallel()
		_, err := HooksDirInsideWorktree(t.Context(), t.TempDir())
		assert.Error(t, err)
	})
}
