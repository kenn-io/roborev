package storage

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAgentActivityCorruptStateAndSaturation(t *testing.T) {
	assert := assert.New(t)
	db := openTestDB(t)
	defer db.Close()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	const day = "2026-01-02"
	for _, stored := range []string{
		`{`,
		`{"day":"2026-01-02","calls":"invalid"}`,
		`{"day":"2026-01-02","calls":101,"calls":"invalid"}`,
	} {
		require.NoError(t, db.SetSyncState(agentActivityKey, stored))
		calls, err := db.RecordAgentCall(ctx, day)
		require.NoError(t, err)
		assert.Equal(uint64(1), calls)
		value, err := db.GetSyncState(agentActivityKey)
		require.NoError(t, err)
		assert.JSONEq(`{"day":"2026-01-02","calls":1}`, value)
	}
	require.NoError(t, db.SetSyncState(agentActivityKey, `{"day":"2026-01-02","calls":100}`))
	calls, err := db.RecordAgentCall(ctx, day)
	require.NoError(t, err)
	assert.Equal(uint64(101), calls)
	_, err = db.Exec(`CREATE TABLE agent_count_writes (value TEXT); CREATE TRIGGER audit_agent_count_update AFTER UPDATE ON sync_state BEGIN INSERT INTO agent_count_writes VALUES (NEW.value); END`)
	require.NoError(t, err)
	for range 3 {
		calls, err := db.RecordAgentCall(ctx, day)
		require.NoError(t, err)
		assert.Zero(calls)
	}
	stored, err := db.GetSyncState(agentActivityKey)
	require.NoError(t, err)
	assert.JSONEq(`{"day":"2026-01-02","calls":101}`, stored)
	var writes int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM agent_count_writes`).Scan(&writes))
	assert.Zero(writes)
	calls, err = db.RecordAgentCall(ctx, "2026-01-03")
	require.NoError(t, err)
	assert.Equal(uint64(1), calls)
	stored, err = db.GetSyncState(agentActivityKey)
	require.NoError(t, err)
	assert.JSONEq(`{"day":"2026-01-03","calls":1}`, stored)
}

func TestAgentActivityDatabaseCancellation(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	db.SetMaxOpenConns(1)
	conn, err := db.Conn(t.Context())
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, err := db.RecordAgentCall(ctx, "2026-01-02")
		assert.ErrorIs(t, err, context.Canceled)
	}()
	// Wall-clock wait: database/sql waits for the held SQLite connection.
	require.Eventually(t, func() bool { return db.Stats().WaitCount > 0 }, time.Second, time.Millisecond)
	cancel()
	<-done
	require.NoError(t, conn.Close())
	stored, err := db.GetSyncState(agentActivityKey)
	require.NoError(t, err)
	assert.Empty(t, stored)
}

func TestAgentActivityBusyWriteCancellation(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	writer, err := db.Begin()
	require.NoError(t, err)
	defer func() { _ = writer.Rollback() }()
	_, err = writer.Exec(`INSERT INTO sync_state (key, value) VALUES ('writer-lock', 'held')`)
	require.NoError(t, err)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		_, err := db.RecordAgentCall(ctx, "2026-01-02")
		assert.Error(t, err)
	}()
	// Wall-clock wait: SQLite's busy writer must honor the notification deadline.
	require.Eventually(t, func() bool {
		select {
		case <-done:
			return true
		default:
			return false
		}
	}, 2*time.Second, time.Millisecond)
	conn, err := db.Conn(t.Context())
	require.NoError(t, err)
	var busyTimeout int
	require.NoError(t, conn.QueryRowContext(t.Context(), `PRAGMA busy_timeout`).Scan(&busyTimeout))
	assert.Equal(t, 30000, busyTimeout)
	require.NoError(t, conn.Close())
	require.NoError(t, writer.Rollback())
	stored, err := db.GetSyncState(agentActivityKey)
	require.NoError(t, err)
	assert.Empty(t, stored)
}

type restoreFailureConnector struct {
	driver driver.Driver
	dsn    string
}

func (c restoreFailureConnector) Driver() driver.Driver { return c.driver }

func (c restoreFailureConnector) Connect(context.Context) (driver.Conn, error) {
	conn, err := c.driver.Open(c.dsn)
	if err != nil {
		return nil, err
	}
	return &restoreFailureConn{Conn: conn}, nil
}

type restoreFailureConn struct {
	driver.Conn
	restoreFailed bool
	closed        bool
}

func (c *restoreFailureConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if query == "PRAGMA busy_timeout = 30000" {
		c.restoreFailed = true
		return nil, errors.New("timeout restore failed")
	}
	return c.Conn.(driver.ExecerContext).ExecContext(ctx, query, args)
}

func (c *restoreFailureConn) Close() error {
	c.closed = true
	return c.Conn.Close()
}

func TestAgentActivityFailedRestoreDiscardsConnection(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	var path string
	require.NoError(t, db.QueryRow(`SELECT file FROM pragma_database_list WHERE name = 'main'`).Scan(&path))
	pool := sql.OpenDB(restoreFailureConnector{driver: db.Driver(), dsn: path + "?_pragma=busy_timeout(30000)"})
	t.Cleanup(func() { require.NoError(t, pool.Close()) })
	pool.SetMaxOpenConns(1)
	db = &DB{DB: pool}
	conn, err := pool.Conn(t.Context())
	require.NoError(t, err)
	var affected *restoreFailureConn
	require.NoError(t, conn.Raw(func(raw any) error {
		affected = raw.(*restoreFailureConn)
		return nil
	}))
	require.NoError(t, conn.Close())
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	calls, err := db.RecordAgentCall(ctx, "2026-01-02")
	require.NoError(t, err)
	assert.Equal(t, uint64(1), calls)
	assert.True(t, affected.restoreFailed)
	assert.True(t, affected.closed)
	var busyTimeout int
	require.NoError(t, pool.QueryRow(`PRAGMA busy_timeout`).Scan(&busyTimeout))
	assert.Equal(t, 30000, busyTimeout)
	require.NoError(t, db.SetSyncState("ordinary-write", "saved"))
}
