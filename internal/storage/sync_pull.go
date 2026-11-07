package storage

import (
	"context"
	"database/sql"
	"time"
)

// A 10,000-job first pull with foreground writes every 250 ms measured
// enqueue p99 at 116 ms with 5 ms batches, versus 55 to 67 ms on main.
// Yield after 500 us plus the current row and commit to preserve foreground latency.
const pullTxBudget = 500 * time.Microsecond

// pullSession reuses one connection and its compiled statements across pages.
// All writes in an open transaction, including parent creation, use this conn.
type pullSession struct {
	db      *DB
	conn    *sql.Conn
	stmts   map[string]*sql.Stmt
	started time.Time
	budget  time.Duration
	jobs    bool
	changed bool
	cancels []func()
	notify  func()
}

func (db *DB) newPullSession(ctx context.Context) (*pullSession, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	return &pullSession{db: db, conn: conn, stmts: make(map[string]*sql.Stmt), budget: pullTxBudget}, nil
}

// pullOne runs one row in its own transaction for callers outside the sync worker.
func pullOne[T any](db *DB, jobs bool, row func(context.Context, *pullSession) (T, error)) (T, error) {
	// This API predates caller contexts; workers pass their context directly.
	ctx := context.Background()
	var zero T
	s, err := db.newPullSession(ctx)
	if err != nil {
		return zero, err
	}
	defer s.close()
	if err := s.begin(ctx, jobs); err != nil {
		return zero, err
	}
	v, err := row(ctx, s)
	if err != nil {
		return zero, err
	}
	return v, s.commit(ctx)
}

func (s *pullSession) close() {
	s.rollback()
	for _, stmt := range s.stmts {
		_ = stmt.Close()
	}
	_ = s.conn.Close()
}

func (s *pullSession) begin(ctx context.Context, jobs bool) error {
	if !s.started.IsZero() {
		return nil
	}
	// Attempt changes always lock attempts before entering the write gate.
	if jobs {
		if err := s.db.attempts.mu.LockContext(ctx); err != nil {
			return err
		}
	}
	if err := beginImmediate(ctx, s.conn); err != nil {
		if jobs {
			s.db.attempts.mu.Unlock()
		}
		return err
	}
	s.jobs = jobs
	s.started = time.Now()
	return nil
}

func (s *pullSession) reset() {
	if s.jobs {
		s.db.attempts.mu.Unlock()
	}
	s.jobs = false
	s.started = time.Time{}
	s.changed = false
	s.cancels = nil
}

func (s *pullSession) rollback() {
	if !s.started.IsZero() {
		_ = rollbackConn(s.conn)
		s.reset()
	}
}

func (s *pullSession) commit(ctx context.Context) error {
	if s.started.IsZero() {
		return nil
	}
	if _, err := s.conn.ExecContext(ctx, "COMMIT"); err != nil {
		s.rollback()
		return err
	}
	for _, cancel := range s.cancels {
		cancel()
	}
	changed := s.changed
	s.reset()
	if changed && s.notify != nil {
		s.notify()
	}
	return nil
}

func (s *pullSession) finishRow(ctx context.Context, changed bool, err error) error {
	if err != nil {
		s.rollback()
		return err
	}
	s.changed = s.changed || changed
	if time.Since(s.started) >= s.budget {
		return s.commit(ctx)
	}
	return nil
}

func (s *pullSession) prepare(ctx context.Context, query string) (*sql.Stmt, error) {
	if stmt := s.stmts[query]; stmt != nil {
		return stmt, nil
	}
	stmt, err := s.conn.PrepareContext(ctx, query)
	if err == nil {
		s.stmts[query] = stmt
	}
	return stmt, err
}

func (s *pullSession) exec(ctx context.Context, query string, args ...any) (sql.Result, error) {
	stmt, err := s.prepare(ctx, query)
	if err != nil {
		return nil, err
	}
	return stmt.ExecContext(ctx, args...)
}

func (s *pullSession) query(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	stmt, err := s.prepare(ctx, query)
	if err != nil {
		return nil, err
	}
	return stmt.QueryContext(ctx, args...)
}

func (s *pullSession) queryRow(ctx context.Context, query string, args ...any) *sql.Row {
	if stmt, err := s.prepare(ctx, query); err == nil {
		return stmt.QueryRowContext(ctx, args...)
	}
	// Unprepared, the same query reports its failure from Scan.
	return s.conn.QueryRowContext(ctx, query, args...)
}
