package storage

import (
	"context"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWriteGateCanceledWaiterDoesNotBlockNextWrite(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		db := openTestDB(t)
		defer db.Close()
		tx, err := db.BeginTx(t.Context(), nil)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()
		_, err = tx.ExecContext(t.Context(), `INSERT INTO sync_state (key, value) VALUES ('holder', 'rolled back')`)
		require.NoError(t, err)

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		canceled := make(chan error, 1)
		go func() {
			_, err := db.ExecContext(ctx, `INSERT INTO sync_state (key, value) VALUES ('canceled', 'absent')`)
			canceled <- err
		}()
		synctest.Wait()
		next := make(chan error, 1)
		go func() {
			_, err := db.ExecContext(t.Context(), `INSERT INTO sync_state (key, value) VALUES ('next', 'committed')`)
			next <- err
		}()
		synctest.Wait()
		cancel()
		require.ErrorIs(t, <-canceled, context.Canceled)
		require.NoError(t, tx.Rollback())
		require.NoError(t, <-next)
		var count int
		require.NoError(t, db.QueryRowContext(t.Context(), `SELECT count(*) FROM sync_state WHERE key IN ('holder', 'canceled')`).Scan(&count))
		assert.Equal(t, 0, count)
	})
}

func TestWriteGateReleasesAfterFailedBeginOrRollback(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		db := openTestDB(t)
		defer db.Close()
		conn, err := db.Conn(t.Context())
		require.NoError(t, err)
		defer conn.Close()
		require.NoError(t, beginImmediate(t.Context(), conn))
		_, err = conn.ExecContext(t.Context(), `INSERT INTO sync_state (key, value) VALUES ('rollback', 'absent')`)
		require.NoError(t, err)
		require.NoError(t, rollbackConn(conn))
		// Invalid SQL fails after acquiring the gate. Keep the connection
		// open to prove the error itself releases it.
		_, err = conn.ExecContext(t.Context(), "BEGIN IMMEDIATE invalid")
		require.Error(t, err)

		// A leaked gate makes this wait out the fake-clock write timeout.
		_, err = db.ExecContext(t.Context(), `INSERT INTO sync_state (key, value) VALUES ('next', 'committed')`)
		require.NoError(t, err)
	})
}
