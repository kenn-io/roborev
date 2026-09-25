package storage

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFindReposByIdentity(t *testing.T) {
	assert := assert.New(t)
	db := openTestDB(t)
	const id = "https://example.com/org/project.git"

	// With no local checkout, sync creates a placeholder (root_path == identity).
	_, err := db.GetOrCreateRepoByIdentity(id)
	require.NoError(t, err)
	none, err := db.FindReposByIdentity(id)
	require.NoError(t, err)
	assert.Empty(none)

	b, err := db.GetOrCreateRepo("/srv/b/project", id)
	require.NoError(t, err)
	a, err := db.GetOrCreateRepo("/srv/a/project", id)
	require.NoError(t, err)
	_, err = db.GetOrCreateRepo("/srv/other", "https://example.com/org/other.git")
	require.NoError(t, err)

	repos, err := db.FindReposByIdentity(id)
	require.NoError(t, err)
	require.Len(t, repos, 2)
	assert.Equal(a.RootPath, repos[0].RootPath)
	assert.Equal(b.RootPath, repos[1].RootPath)
	assert.Equal(id, repos[0].Identity)
}
