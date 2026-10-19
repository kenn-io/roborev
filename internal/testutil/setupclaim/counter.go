// Package setupclaim provides a counter for setup marker insert attempts.
package setupclaim

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"
)

// InstallCounter records setup marker inserts and returns their count.
func InstallCounter(t *testing.T, db *sql.DB) func() int {
	t.Helper()
	_, err := db.Exec(`CREATE TABLE setup_claim_attempts (value TEXT);
		CREATE TRIGGER count_setup_claim BEFORE INSERT ON sync_state
		WHEN NEW.key = 'telemetry.setup_finished'
		BEGIN INSERT INTO setup_claim_attempts VALUES (NEW.value); END`)
	require.NoError(t, err)
	return func() int {
		t.Helper()
		var attempts int
		require.NoError(t, db.QueryRow(`SELECT count(*) FROM setup_claim_attempts`).Scan(&attempts))
		return attempts
	}
}
