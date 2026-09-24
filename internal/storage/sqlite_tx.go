package storage

import (
	"context"
	"database/sql"
	"database/sql/driver"
)

// beginImmediate starts a write transaction on a dedicated connection. A
// BEGIN interrupted by context cancellation can leave SQLite inside the
// transaction while reporting an error, and database/sql would return that
// connection to the pool still holding the write lock. Every later BEGIN on
// it then fails with "cannot start a transaction within a transaction" and
// every other writer waits on the lock, so the connection is discarded.
func beginImmediate(ctx context.Context, conn *sql.Conn) error {
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		discardConn(conn)
		return err
	}
	return nil
}

// rollbackConn ends a transaction opened by beginImmediate. It ignores the
// caller's context because a canceled ROLLBACK would leave the transaction
// open on a pooled connection; if the rollback still fails, the connection
// is discarded for the same reason.
func rollbackConn(conn *sql.Conn) error {
	if _, err := conn.ExecContext(context.Background(), "ROLLBACK"); err != nil {
		discardConn(conn)
		return err
	}
	return nil
}

// discardConn keeps conn out of the pool once the caller closes it.
func discardConn(conn *sql.Conn) {
	_ = conn.Raw(func(any) error { return driver.ErrBadConn })
}
