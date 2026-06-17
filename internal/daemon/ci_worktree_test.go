package daemon

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCIWorktreeRepoDir_NestsByRepoBasename(t *testing.T) {
	got := ciWorktreeRepoDir("/srv/clones/acme/widget")
	assert.Equal(t, "widget", filepath.Base(got))
	assert.Equal(t, ciWorktreeParentDir(), filepath.Dir(got))
}

func TestCIWorktreeRepoDir_FlatFallbackForEmptyRepo(t *testing.T) {
	assert.Equal(t, ciWorktreeParentDir(), ciWorktreeRepoDir(""))
	assert.Equal(t, ciWorktreeParentDir(), ciWorktreeRepoDir("/"))
	assert.Equal(t, ciWorktreeParentDir(), ciWorktreeRepoDir("   "))
}

func TestStaleCIWorktreeDirs_HandlesNestedAndLegacyLayouts(t *testing.T) {
	parent := t.TempDir()
	// Legacy flat layout left by older daemons.
	legacy := filepath.Join(parent, ciWorktreePrefix+"1-aaa")
	require.NoError(t, os.MkdirAll(legacy, 0o755))
	// Current repo-nested layout.
	nested := filepath.Join(parent, "widget", ciWorktreePrefix+"2-bbb")
	require.NoError(t, os.MkdirAll(nested, 0o755))
	// Dirs that must be ignored: a non-worktree child of a repo dir, and a
	// repo dir holding no CI worktrees.
	require.NoError(t, os.MkdirAll(filepath.Join(parent, "widget", "not-a-worktree"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(parent, "empty-repo"), 0o755))

	dirs, err := staleCIWorktreeDirs(parent)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{legacy, nested}, dirs)
}

func TestStaleCIWorktreeDirs_RepoNamedLikeWorktreePrefix(t *testing.T) {
	parent := t.TempDir()
	// A repository whose basename starts with the CI worktree prefix produces
	// a repo-named parent that also matches the prefix. Its nested worktrees
	// must be collected individually, never the parent itself, which would be
	// deleted wholesale without closing the worktrees underneath it.
	repoParent := filepath.Join(parent, ciWorktreePrefix+"tools")
	nested := filepath.Join(repoParent, ciWorktreePrefix+"3-ccc")
	require.NoError(t, os.MkdirAll(nested, 0o755))

	dirs, err := staleCIWorktreeDirs(parent)
	require.NoError(t, err)
	assert.Equal(t, []string{nested}, dirs)
	assert.NotContains(t, dirs, repoParent)
}

func TestStaleCIWorktreeDirs_LinkedWorktreeNotRecursed(t *testing.T) {
	parent := t.TempDir()
	// A real legacy flat worktree (a .git link file) whose checkout happens to
	// contain a directory matching the worktree prefix. The worktree itself
	// must be returned for cleanup, not the inner directory.
	worktree := filepath.Join(parent, ciWorktreePrefix+"4-ddd")
	require.NoError(t, os.MkdirAll(filepath.Join(worktree, ciWorktreePrefix+"inner"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(worktree, ".git"),
		[]byte("gitdir: /some/repo/.git/worktrees/x\n"),
		0o600,
	))

	dirs, err := staleCIWorktreeDirs(parent)
	require.NoError(t, err)
	assert.Equal(t, []string{worktree}, dirs)
}

func TestStaleCIWorktreeDirs_MissingParentReturnsNotExist(t *testing.T) {
	_, err := staleCIWorktreeDirs(filepath.Join(t.TempDir(), "does-not-exist"))
	assert.ErrorIs(t, err, os.ErrNotExist)
}
