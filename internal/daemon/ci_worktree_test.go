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

func TestStaleCIWorktreeDirs_MissingParentReturnsNotExist(t *testing.T) {
	_, err := staleCIWorktreeDirs(filepath.Join(t.TempDir(), "does-not-exist"))
	assert.ErrorIs(t, err, os.ErrNotExist)
}
