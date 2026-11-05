package storage

import (
	"context"
	"database/sql/driver"
	"errors"
	"strings"
	"time"
	"unicode"

	"golang.org/x/sync/semaphore"
)

const sqliteWriteWait = 30 * time.Second

var errWriteGateTimeout = errors.New("sqlite write queue timed out")

// writeGate queues this database's writers in arrival order. Its wait budget
// matches busy_timeout; SQLite still handles contention from other processes.
type writeGate struct{ queue *semaphore.Weighted }

func (g *writeGate) acquire(ctx context.Context) error {
	wait, cancel := context.WithTimeout(ctx, sqliteWriteWait)
	defer cancel()
	if err := g.queue.Acquire(wait, 1); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errWriteGateTimeout
	}
	return nil
}

type writeGateConnector struct {
	driver.Connector
	gate *writeGate
}

func newWriteGateConnector(base driver.Connector) *writeGateConnector {
	return &writeGateConnector{Connector: base, gate: &writeGate{queue: semaphore.NewWeighted(1)}}
}

// sqliteGateConn lists the interfaces provided by modernc's connections.
type sqliteGateConn interface {
	driver.Conn
	driver.ConnBeginTx
	driver.ConnPrepareContext
	driver.ExecerContext
	driver.QueryerContext
	driver.Pinger
	driver.SessionResetter
	driver.Validator
}

func (c *writeGateConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &writeGateConn{sqliteGateConn: conn.(sqliteGateConn), gate: c.gate}, nil
}

type writeGateConn struct {
	sqliteGateConn
	gate          *writeGate
	holding, inTx bool
}

func (c *writeGateConn) release() {
	if c.holding {
		c.holding = false
		c.gate.queue.Release(1)
	}
}

// start uses the leading keyword, leaving reads and unrecognized statements
// alone. Deferred transactions join the queue only when they first write.
func (c *writeGateConn) start(ctx context.Context, query string) (func(error), error) {
	word, rest := leadingSQLKeyword(query)
	keyword := strings.ToUpper(word)
	write := false
	switch keyword {
	case "BEGIN":
		mode, _ := leadingSQLKeyword(rest)
		write = strings.EqualFold(mode, "IMMEDIATE")
	case "INSERT", "UPDATE", "DELETE", "REPLACE", "CREATE", "DROP", "ALTER", "VACUUM":
		write = true
	}
	acquired := write && !c.holding
	if acquired {
		if err := c.gate.acquire(ctx); err != nil {
			return nil, err
		}
		c.holding = true
	}
	return func(err error) {
		switch keyword {
		case "BEGIN":
			if err == nil {
				c.inTx = true
			} else if acquired {
				c.release()
			}
		case "COMMIT", "ROLLBACK":
			if err == nil {
				c.inTx = false
				c.release()
			}
		default:
			if acquired && !c.inTx {
				c.release()
			}
		}
	}, nil
}

func leadingSQLKeyword(query string) (string, string) {
	query = strings.TrimLeftFunc(query, unicode.IsSpace)
	if end := strings.IndexFunc(query, unicode.IsSpace); end >= 0 {
		return query[:end], query[end:]
	}
	return query, ""
}

func (c *writeGateConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	finish, err := c.start(ctx, query)
	if err != nil {
		return nil, err
	}
	result, err := c.sqliteGateConn.ExecContext(ctx, query, args)
	finish(err)
	return result, err
}

func (c *writeGateConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	finish, err := c.start(ctx, query)
	if err != nil {
		return nil, err
	}
	rows, err := c.sqliteGateConn.QueryContext(ctx, query, args)
	if err != nil {
		finish(err)
		return nil, err
	}
	return &writeGateRows{Rows: rows, finish: finish}, nil
}

func (c *writeGateConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	stmt, err := c.sqliteGateConn.PrepareContext(ctx, query)
	if err != nil {
		return nil, err
	}
	return &writeGateStmt{Stmt: stmt, conn: c, query: query}, nil
}

func (c *writeGateConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	tx, err := c.sqliteGateConn.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	c.inTx = true
	return &writeGateTx{Tx: tx, conn: c}, nil
}

func (c *writeGateConn) Close() error {
	err := c.sqliteGateConn.Close()
	c.release()
	return err
}

type writeGateTx struct {
	driver.Tx
	conn *writeGateConn
}

func (t *writeGateTx) Commit() error {
	err := t.Tx.Commit()
	t.conn.inTx = false
	t.conn.release()
	return err
}

func (t *writeGateTx) Rollback() error {
	err := t.Tx.Rollback()
	t.conn.inTx = false
	t.conn.release()
	return err
}

type writeGateStmt struct {
	driver.Stmt
	conn  *writeGateConn
	query string
}

func (s *writeGateStmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	finish, err := s.conn.start(ctx, s.query)
	if err != nil {
		return nil, err
	}
	result, err := s.Stmt.(driver.StmtExecContext).ExecContext(ctx, args)
	finish(err)
	return result, err
}

func (s *writeGateStmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	finish, err := s.conn.start(ctx, s.query)
	if err != nil {
		return nil, err
	}
	rows, err := s.Stmt.(driver.StmtQueryContext).QueryContext(ctx, args)
	if err != nil {
		finish(err)
		return nil, err
	}
	return &writeGateRows{Rows: rows, finish: finish}, nil
}

type writeGateRows struct {
	driver.Rows
	finish func(error)
}

func (r *writeGateRows) Close() error {
	err := r.Rows.Close()
	if r.finish != nil {
		r.finish(err)
		r.finish = nil
	}
	return err
}
