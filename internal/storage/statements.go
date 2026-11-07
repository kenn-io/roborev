package storage

import (
	"context"
	"database/sql"
	"errors"
	"sync"
)

// statementCache reuses compiled SQL for one owner's lifetime. The owner
// supplies preparation on its DB or dedicated connection and closes the cache.
type statementCache struct {
	mu    sync.Mutex
	stmts map[string]*sql.Stmt
}

// stmt returns the prepared statement for query, preparing it on first use
// so that preparation happens after Open's migrations.
func (s *statementCache) stmt(ctx context.Context, prepare func(context.Context, string) (*sql.Stmt, error), query string) (*sql.Stmt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if stmt, ok := s.stmts[query]; ok {
		return stmt, nil
	}
	stmt, err := prepare(ctx, query)
	if err != nil {
		return nil, err
	}
	if s.stmts == nil {
		s.stmts = make(map[string]*sql.Stmt)
	}
	s.stmts[query] = stmt
	return stmt, nil
}

func (s *statementCache) close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var errs []error
	for _, stmt := range s.stmts {
		errs = append(errs, stmt.Close())
	}
	s.stmts = nil
	return errors.Join(errs...)
}
