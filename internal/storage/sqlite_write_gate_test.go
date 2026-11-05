package storage

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

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
		var value string
		require.NoError(t, db.QueryRowContext(t.Context(), `SELECT value FROM sync_state WHERE key = 'next'`).Scan(&value))
		assert.Equal(t, "committed", value)
		var count int
		require.NoError(t, db.QueryRowContext(t.Context(), `SELECT count(*) FROM sync_state WHERE key IN ('holder', 'canceled')`).Scan(&count))
		assert.Equal(t, 0, count)
	})
}

func TestWriteGateReleasesAfterFailedBeginOrRollback(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"failed-begin", "sql-rollback", "tx-rollback"} {
		t.Run(kind, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				db := openTestDB(t)
				defer db.Close()
				if kind == "tx-rollback" {
					tx, err := db.BeginTx(t.Context(), nil)
					require.NoError(t, err)
					defer func() { _ = tx.Rollback() }()
					_, err = tx.ExecContext(t.Context(), `INSERT INTO sync_state (key, value) VALUES ('rollback', 'absent')`)
					require.NoError(t, err)
					require.NoError(t, tx.Rollback())
				} else {
					conn, err := db.Conn(t.Context())
					require.NoError(t, err)
					defer conn.Close()
					defer func() { _ = rollbackConn(conn) }()
					if kind == "failed-begin" {
						// Invalid SQL fails after acquiring the gate. Keep the
						// connection open to prove the error itself releases it.
						_, err = conn.ExecContext(t.Context(), "BEGIN IMMEDIATE invalid")
						require.Error(t, err)
					} else {
						require.NoError(t, beginImmediate(t.Context(), conn))
						_, err = conn.ExecContext(t.Context(), `INSERT INTO sync_state (key, value) VALUES ('rollback', 'absent')`)
						require.NoError(t, err)
						require.NoError(t, rollbackConn(conn))
					}
				}
				ctx, cancel := context.WithTimeout(t.Context(), time.Second)
				defer cancel()
				_, err := db.ExecContext(ctx, `INSERT INTO sync_state (key, value) VALUES ('next', 'committed')`)
				require.NoError(t, err)
				var value string
				require.NoError(t, db.QueryRowContext(t.Context(), `SELECT value FROM sync_state WHERE key = 'next'`).Scan(&value))
				assert.Equal(t, "committed", value)
				var count int
				require.NoError(t, db.QueryRowContext(t.Context(), `SELECT count(*) FROM sync_state WHERE key = 'rollback'`).Scan(&count))
				assert.Equal(t, 0, count)
			})
		})
	}
}
