package daemon

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/githook"
	"go.kenn.io/roborev/internal/testutil"
)

func writeFakeBinary(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755))
	return path
}

func TestRepairRepoHooksAtStartup(t *testing.T) {
	t.Parallel()

	t.Run("rewrites stale managed hook to current binary", func(t *testing.T) {
		t.Parallel()
		repo := testutil.NewTestRepo(t)
		oldBinary := writeFakeBinary(t, "roborev")
		newBinary := writeFakeBinary(t, "roborev")
		repo.WriteHook(githook.GeneratePostCommitWithBinary(oldBinary))

		repairRepoHooksAtStartup(t.Context(), repo.Root, newBinary)

		content, err := os.ReadFile(repo.GetHookPath("post-commit"))
		require.NoError(t, err)
		assert.Contains(t, string(content), newBinary)
		assert.NotContains(t, string(content), oldBinary)
	})

	t.Run("never writes hooks dir inside worktree", func(t *testing.T) {
		t.Parallel()
		repo := testutil.NewTestRepo(t)
		oldBinary := writeFakeBinary(t, "roborev")
		hooksDir := filepath.Join(repo.Root, ".githooks")
		require.NoError(t, os.MkdirAll(hooksDir, 0o755))
		repo.Run("config", "core.hooksPath", ".githooks")
		stale := githook.GeneratePostCommitWithBinary(oldBinary)
		hookPath := filepath.Join(hooksDir, "post-commit")
		require.NoError(t, os.WriteFile(hookPath, []byte(stale), 0o755))

		repairRepoHooksAtStartup(t.Context(), repo.Root, writeFakeBinary(t, "roborev"))

		content, err := os.ReadFile(hookPath)
		require.NoError(t, err)
		assert.Equal(t, stale, string(content), "daemon must not modify hooks inside the working tree")
	})

	t.Run("leaves unmanaged hooks alone", func(t *testing.T) {
		t.Parallel()
		repo := testutil.NewTestRepo(t)
		custom := "#!/bin/sh\necho custom\n"
		repo.WriteHook(custom)

		repairRepoHooksAtStartup(t.Context(), repo.Root, writeFakeBinary(t, "roborev"))

		content, err := os.ReadFile(repo.GetHookPath("post-commit"))
		require.NoError(t, err)
		assert.Equal(t, custom, string(content))
	})

	t.Run("deleted repo is a no-op", func(t *testing.T) {
		t.Parallel()
		gone := filepath.Join(t.TempDir(), "gone")
		repairRepoHooksAtStartup(t.Context(), gone, writeFakeBinary(t, "roborev"))
	})
}
