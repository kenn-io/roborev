package storage

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpenBunHandleSharesSQLiteDatabase(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "reviews.db"))
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, db.Close())
	})

	repo, err := db.GetOrCreateRepo(filepath.Join(t.TempDir(), "repo"))
	require.NoError(t, err)

	var count int
	err = db.bun.NewRaw("SELECT COUNT(*) FROM repos WHERE id = ?", repo.ID).
		Scan(t.Context(), &count)
	require.NoError(t, err)
	assert.Equal(t, 1, count)
}
