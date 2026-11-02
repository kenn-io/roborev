package daemon

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
)

func assertPanelRetireReason(t *testing.T, db *storage.DB, id int64, want string) {
	t.Helper()
	var reason sql.NullString
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT retire_reason FROM ci_pr_panels WHERE id = ?`, id).Scan(&reason))
	assert.Equal(t, sql.NullString{String: want, Valid: want != ""}, reason)
}
