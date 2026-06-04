package telemetry

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
)

func openTestDB(t *testing.T) *storage.DB {
	t.Helper()
	db, err := storage.Open(filepath.Join(t.TempDir(), "reviews.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	return db
}
