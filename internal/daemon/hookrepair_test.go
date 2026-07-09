package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
		// The hook bakes the path via %q, so compare the quoted form
		// (on Windows the raw path differs by backslash escaping).
		assert.Contains(t, string(content), fmt.Sprintf("ROBOREV=%q", newBinary))
		assert.NotContains(t, string(content), fmt.Sprintf("ROBOREV=%q", oldBinary))
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

	t.Run("never writes main-worktree hooks dir via linked worktree", func(t *testing.T) {
		t.Parallel()
		repo := testutil.NewTestRepoWithCommit(t)
		oldBinary := writeFakeBinary(t, "roborev")
		hooksDir := filepath.Join(repo.Root, ".githooks")
		require.NoError(t, os.MkdirAll(hooksDir, 0o755))
		repo.Run("config", "core.hooksPath", ".githooks")
		stale := githook.GeneratePostCommitWithBinary(oldBinary)
		hookPath := filepath.Join(hooksDir, "post-commit")
		require.NoError(t, os.WriteFile(hookPath, []byte(stale), 0o755))
		repo.Run("branch", "hooks-wt")
		wtDir := filepath.Join(t.TempDir(), "wt")
		repo.Run("worktree", "add", wtDir, "hooks-wt")

		repairRepoHooksAtStartup(t.Context(), wtDir, writeFakeBinary(t, "roborev"))

		content, err := os.ReadFile(hookPath)
		require.NoError(t, err)
		assert.Equal(t, stale, string(content),
			"daemon must not modify hooks that resolve into the main worktree")
	})

	t.Run("repairs common-dir hooks via linked worktree", func(t *testing.T) {
		t.Parallel()
		repo := testutil.NewTestRepoWithCommit(t)
		oldBinary := writeFakeBinary(t, "roborev")
		newBinary := writeFakeBinary(t, "roborev")
		repo.WriteHook(githook.GeneratePostCommitWithBinary(oldBinary))
		repo.Run("branch", "hooks-wt")
		wtDir := filepath.Join(t.TempDir(), "wt")
		repo.Run("worktree", "add", wtDir, "hooks-wt")

		repairRepoHooksAtStartup(t.Context(), wtDir, newBinary)

		content, err := os.ReadFile(repo.GetHookPath("post-commit"))
		require.NoError(t, err)
		assert.Contains(t, string(content), fmt.Sprintf("ROBOREV=%q", newBinary))
		assert.NotContains(t, string(content), fmt.Sprintf("ROBOREV=%q", oldBinary))
	})

	t.Run("deleted repo is a no-op", func(t *testing.T) {
		t.Parallel()
		gone := filepath.Join(t.TempDir(), "gone")
		repairRepoHooksAtStartup(t.Context(), gone, writeFakeBinary(t, "roborev"))
	})
}

func TestReadOnlyHookWarnings(t *testing.T) {
	t.Parallel()

	setupWorktreeHooks := func(t *testing.T) (*testutil.TestRepo, string) {
		t.Helper()
		repo := testutil.NewTestRepo(t)
		hooksDir := filepath.Join(repo.Root, ".githooks")
		require.NoError(t, os.MkdirAll(hooksDir, 0o755))
		repo.Run("config", "core.hooksPath", ".githooks")
		return repo, hooksDir
	}

	t.Run("stale baked binary with current marker warns", func(t *testing.T) {
		t.Parallel()
		repo, hooksDir := setupWorktreeHooks(t)
		oldBinary := writeFakeBinary(t, "roborev")
		for _, name := range []string{"post-commit", "post-rewrite"} {
			content := githook.GeneratePostCommitWithBinary(oldBinary)
			if name == "post-rewrite" {
				content = githook.GeneratePostRewriteWithBinary(oldBinary)
			}
			require.NoError(t, os.WriteFile(filepath.Join(hooksDir, name), []byte(content), 0o755))
		}

		warnings := readOnlyHookWarnings(t.Context(), repo.Root, writeFakeBinary(t, "roborev"))
		require.Len(t, warnings, 2)
		assert.Contains(t, warnings[0], "post-commit")
		assert.Contains(t, warnings[0], "stale")
		assert.Contains(t, warnings[1], "post-rewrite")
		assert.Contains(t, warnings[1], "stale")
	})

	t.Run("current hooks produce no warnings", func(t *testing.T) {
		t.Parallel()
		repo, hooksDir := setupWorktreeHooks(t)
		binary := writeFakeBinary(t, "roborev")
		require.NoError(t, os.WriteFile(
			filepath.Join(hooksDir, "post-commit"),
			[]byte(githook.GeneratePostCommitWithBinary(binary)), 0o755))
		require.NoError(t, os.WriteFile(
			filepath.Join(hooksDir, "post-rewrite"),
			[]byte(githook.GeneratePostRewriteWithBinary(binary)), 0o755))

		assert.Empty(t, readOnlyHookWarnings(t.Context(), repo.Root, binary))
	})

	t.Run("outdated marker warns without duplicate stale warning", func(t *testing.T) {
		t.Parallel()
		repo, hooksDir := setupWorktreeHooks(t)
		legacy := "#!/bin/sh\n# roborev post-commit hook v1\nroborev enqueue\n"
		require.NoError(t, os.WriteFile(filepath.Join(hooksDir, "post-commit"), []byte(legacy), 0o755))

		warnings := readOnlyHookWarnings(t.Context(), repo.Root, writeFakeBinary(t, "roborev"))
		var postCommitWarnings []string
		for _, w := range warnings {
			if strings.Contains(w, "post-commit") {
				postCommitWarnings = append(postCommitWarnings, w)
			}
		}
		require.Len(t, postCommitWarnings, 1)
		assert.Contains(t, postCommitWarnings[0], "outdated")
	})

	t.Run("missing post-rewrite warns", func(t *testing.T) {
		t.Parallel()
		repo, hooksDir := setupWorktreeHooks(t)
		binary := writeFakeBinary(t, "roborev")
		require.NoError(t, os.WriteFile(
			filepath.Join(hooksDir, "post-commit"),
			[]byte(githook.GeneratePostCommitWithBinary(binary)), 0o755))

		warnings := readOnlyHookWarnings(t.Context(), repo.Root, binary)
		require.Len(t, warnings, 1)
		assert.Contains(t, warnings[0], "post-rewrite")
	})
}
