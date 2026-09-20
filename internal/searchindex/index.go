// Package searchindex maintains the disposable SQLite search sidecar derived
// from canonical review records.
package searchindex

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"

	"go.kenn.io/kit/vector/sqlitevec"
)

// Index owns the lexical and vector search sidecar.
type Index struct {
	db      *sql.DB
	vectors *sqlitevec.Store[string, string]
}

// CapabilityError reports a SQLite module missing from the running binary.
// Rebuilding a valid sidecar cannot repair this error.
type CapabilityError struct {
	Module string
	Err    error
}

func (e *CapabilityError) Error() string {
	return fmt.Sprintf("search sidecar requires SQLite module %s: %v", e.Module, e.Err)
}

func (e *CapabilityError) Unwrap() error { return e.Err }

type sidecarOpener func(context.Context, string) (*Index, error)

// PathFor derives the disposable search database path from the canonical
// review database path.
func PathFor(canonicalPath string) string {
	if base, ok := strings.CutSuffix(canonicalPath, ".db"); ok {
		return base + ".search.db"
	}
	return canonicalPath + ".search.db"
}

// Open opens or creates a sidecar. Schema mismatches and confirmed SQLite
// corruption are rebuilt; runtime capability failures preserve existing data.
func Open(ctx context.Context, path string) (*Index, error) {
	return openWithRecovery(ctx, path, openOnce)
}

func openWithRecovery(ctx context.Context, path string, opener sidecarOpener) (*Index, error) {
	index, err := opener(ctx, path)
	if err == nil {
		return index, nil
	}
	var capabilityErr *CapabilityError
	var mismatchErr *schemaMismatchError
	if errors.As(err, &capabilityErr) || (!errors.As(err, &mismatchErr) && !isSQLiteCorruption(err)) {
		return nil, err
	}
	if err := removeSidecarFiles(path); err != nil {
		return nil, fmt.Errorf("remove invalid search sidecar: %w", err)
	}
	index, err = opener(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("recreate search sidecar: %w", err)
	}
	return index, nil
}

func openOnce(ctx context.Context, path string) (_ *Index, err error) {
	newFile, err := isNewSidecar(path)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open(sidecarDriver, path)
	if err != nil {
		return nil, fmt.Errorf("open search sidecar: %w", err)
	}
	db.SetMaxOpenConns(1)
	defer func() {
		if err != nil {
			_ = db.Close()
		}
	}()

	if err := db.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("ping search sidecar: %w", err)
	}
	if err := checkIntegrity(ctx, db); err != nil {
		return nil, err
	}
	if err := checkCapabilities(ctx, db); err != nil {
		return nil, err
	}
	if _, err := db.ExecContext(ctx, `PRAGMA journal_mode = WAL`); err != nil {
		return nil, fmt.Errorf("enable search sidecar WAL: %w", err)
	}

	if newFile {
		if err := createBaseSchema(ctx, db); err != nil {
			return nil, err
		}
	} else if err := validateSchema(ctx, db); err != nil {
		return nil, err
	}
	vectors, err := sqlitevec.New[string, string](ctx, db, sqlitevec.Schema{
		DocsTable:      "review_mirror",
		IDColumn:       "doc_key",
		ContentColumn:  "content",
		EmbedGenColumn: "embed_gen",
		VectorsPrefix:  "review_vectors",
		RevisionColumn: "content_hash",
	})
	if err != nil {
		if newFile {
			return nil, fmt.Errorf("bind search vector store: %w", err)
		}
		return nil, &schemaMismatchError{reason: err.Error()}
	}
	if newFile {
		if err := validateSchema(ctx, db); err != nil {
			return nil, err
		}
	}
	return &Index{db: db, vectors: vectors}, nil
}

func isNewSidecar(path string) (bool, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect search sidecar: %w", err)
	}
	return info.Size() == 0, nil
}

func checkIntegrity(ctx context.Context, db *sql.DB) error {
	var result string
	if err := db.QueryRowContext(ctx, `PRAGMA quick_check`).Scan(&result); err != nil {
		return fmt.Errorf("check search sidecar integrity: %w", err)
	}
	if result != "ok" {
		return &sqliteCorruptionError{reason: result}
	}
	return nil
}

func checkCapabilities(ctx context.Context, db *sql.DB) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("reserve search capability connection: %w", err)
	}
	defer func() { _ = conn.Close() }()

	checks := []struct {
		module string
		create string
		insert string
		query  string
		drop   string
	}{
		{
			module: "fts5",
			create: `CREATE VIRTUAL TABLE temp.__roborev_fts5_cap USING fts5(content)`,
			insert: `INSERT INTO temp.__roborev_fts5_cap(content) VALUES ('capability')`,
			query:  `SELECT count(*) FROM temp.__roborev_fts5_cap WHERE __roborev_fts5_cap MATCH 'capability'`,
			drop:   `DROP TABLE temp.__roborev_fts5_cap`,
		},
		{
			module: "vec0",
			create: `CREATE VIRTUAL TABLE temp.__roborev_vec0_cap USING vec0(embedding float[2])`,
			insert: `INSERT INTO temp.__roborev_vec0_cap(rowid, embedding) VALUES (1, vec_f32('[1,0]'))`,
			query:  `SELECT rowid FROM temp.__roborev_vec0_cap WHERE embedding MATCH vec_f32('[1,0]') ORDER BY distance LIMIT 1`,
			drop:   `DROP TABLE temp.__roborev_vec0_cap`,
		},
	}
	for _, check := range checks {
		if _, err := conn.ExecContext(ctx, check.create); err != nil {
			return capabilityFailure(check.module, err)
		}
		if _, err := conn.ExecContext(ctx, check.insert); err != nil {
			return capabilityFailure(check.module, err)
		}
		var result int
		if err := conn.QueryRowContext(ctx, check.query).Scan(&result); err != nil {
			return capabilityFailure(check.module, err)
		}
		if result != 1 {
			return fmt.Errorf("verify SQLite %s capability: round trip returned %d", check.module, result)
		}
		if _, err := conn.ExecContext(ctx, check.drop); err != nil {
			return fmt.Errorf("drop %s capability table: %w", check.module, err)
		}
	}
	return nil
}

func capabilityFailure(module string, err error) error {
	lower := strings.ToLower(err.Error())
	if strings.Contains(lower, "no such module") || strings.Contains(lower, "no such function") {
		return &CapabilityError{Module: module, Err: err}
	}
	return fmt.Errorf("verify SQLite %s capability: %w", module, err)
}

type sqliteCorruptionError struct{ reason string }

func (e *sqliteCorruptionError) Error() string { return "corrupt search sidecar: " + e.reason }

func isSQLiteCorruption(err error) bool {
	if _, ok := errors.AsType[*sqliteCorruptionError](err); ok {
		return true
	}
	var coded interface{ Code() int }
	if errors.As(err, &coded) {
		primary := coded.Code() & 0xff
		return primary == 11 || primary == 26
	}
	return false
}

func removeSidecarFiles(path string) error {
	for _, target := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove %s: %w", target, err)
		}
	}
	return nil
}

// Close closes the sidecar database.
func (index *Index) Close() error {
	return index.db.Close()
}
