package git

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gitworktree "go.kenn.io/kit/git/worktree"
)

func TestPatchRoundTrip(t *testing.T) {
	assert := assert.New(t)
	repo := NewTestRepoWithCommit(t)
	repo.Run("config", "core.autocrlf", "false")
	binaryPath := filepath.Join(repo.Dir, "binary.dat")
	require.NoError(t, os.WriteFile(binaryPath, []byte("before\x00data"), 0o644))
	repo.Run("add", "binary.dat")
	repo.Run("commit", "-m", "add binary")
	require.NoError(t, os.WriteFile(binaryPath, []byte("after\x00data"), 0o644))
	require.NoError(t, os.Remove(filepath.Join(repo.Dir, "initial.txt")))
	repo.WriteFile("new.txt", "new file\n")
	repo.Run("add", "-A")
	patch := repo.Run("diff", "--cached", "--binary") + "\n"
	repo.Run("reset", "--hard", "HEAD")

	// Inherited hook environment must not redirect the patch to another repo.
	leaked := NewTestRepoWithCommit(t)
	t.Setenv("GIT_DIR", filepath.Join(leaked.Dir, ".git"))
	t.Setenv("GIT_WORK_TREE", leaked.Dir)
	t.Setenv("GIT_INDEX_FILE", filepath.Join(leaked.Dir, ".git", "index"))
	require.NoError(t, CheckPatch(t.Context(), repo.Dir, patch))
	content, err := os.ReadFile(binaryPath)
	require.NoError(t, err)
	assert.Equal("before\x00data", string(content))
	require.NoError(t, ApplyPatch(t.Context(), repo.Dir, patch))
	content, err = os.ReadFile(binaryPath)
	require.NoError(t, err)
	assert.Equal("after\x00data", string(content))
	assert.NoFileExists(filepath.Join(repo.Dir, "initial.txt"))
	content, err = os.ReadFile(filepath.Join(repo.Dir, "new.txt"))
	require.NoError(t, err)
	assert.Equal("new file\n", string(content))
	assert.FileExists(filepath.Join(leaked.Dir, "initial.txt"))
	assert.NoFileExists(filepath.Join(leaked.Dir, "new.txt"))
}

func TestPatchErrors(t *testing.T) {
	assert := assert.New(t)
	repo := NewTestRepoWithCommit(t)
	repo.WriteFile("initial.txt", "changed content")
	patch := repo.Run("diff", "--binary") + "\n"
	// The file already contains the change, so applying it again conflicts.
	err := CheckPatch(t.Context(), repo.Dir, patch)
	var conflict *gitworktree.PatchConflictError
	require.ErrorAs(t, err, &conflict)
	assert.Contains(conflict.Detail, "initial.txt")
	require.Error(t, ApplyPatch(t.Context(), repo.Dir, patch))

	err = CheckPatch(t.Context(), repo.Dir, "invalid patch\n")
	require.Error(t, err)
	_, isConflict := errors.AsType[*gitworktree.PatchConflictError](err)
	assert.False(isConflict, "invalid patches must not trigger a rebase")

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, CheckPatch(ctx, repo.Dir, patch), context.Canceled)
	require.ErrorIs(t, ApplyPatch(ctx, repo.Dir, patch), context.Canceled)
	assert.NoError(CheckPatch(ctx, repo.Dir, ""))
	assert.NoError(ApplyPatch(ctx, repo.Dir, ""))
}
