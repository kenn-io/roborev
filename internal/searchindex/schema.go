package searchindex

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
)

const sidecarSchemaVersion = "2"

const sidecarSchema = `
CREATE TABLE search_meta (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);

CREATE TABLE review_mirror (
  doc_key TEXT PRIMARY KEY,
  review_id INTEGER NOT NULL,
  review_uuid TEXT,
  job_id INTEGER NOT NULL,
  job_uuid TEXT,
  group_key TEXT NOT NULL,
  repo_id INTEGER NOT NULL,
  repo_name TEXT NOT NULL,
  branch TEXT,
  git_ref TEXT NOT NULL,
  commit_sha TEXT,
  finished_at TEXT,
  verdict TEXT,
  closed INTEGER NOT NULL,
  panel_role TEXT,
  content TEXT NOT NULL,
  content_hash TEXT NOT NULL,
  embed_gen TEXT,
  share_state INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE review_exchange (
  doc_key TEXT PRIMARY KEY,
  gen_key TEXT NOT NULL,
  content_hash TEXT NOT NULL,
  first_pending_at INTEGER NOT NULL,
  next_attempt_at INTEGER NOT NULL,
  attempts INTEGER NOT NULL,
  claim_expires_at INTEGER NOT NULL,
  origin TEXT NOT NULL,
  published_target TEXT NOT NULL
);

CREATE VIRTUAL TABLE review_fts USING fts5(
  doc_key UNINDEXED,
  content,
  identifiers,
  tokenize = 'unicode61'
);

INSERT INTO search_meta(key, value) VALUES ('schema_version', '2');
`

type columnDefinition struct {
	name       string
	columnType string
	notNull    int
	primaryKey int
}

type tableDefinition struct {
	name    string
	columns []columnDefinition
}

var expectedSchema = []tableDefinition{
	{
		name: "search_meta",
		columns: []columnDefinition{
			{name: "key", columnType: "TEXT", primaryKey: 1},
			{name: "value", columnType: "TEXT", notNull: 1},
		},
	},
	{
		name: "review_mirror",
		columns: []columnDefinition{
			{name: "doc_key", columnType: "TEXT", primaryKey: 1},
			{name: "review_id", columnType: "INTEGER", notNull: 1},
			{name: "review_uuid", columnType: "TEXT"},
			{name: "job_id", columnType: "INTEGER", notNull: 1},
			{name: "job_uuid", columnType: "TEXT"},
			{name: "group_key", columnType: "TEXT", notNull: 1},
			{name: "repo_id", columnType: "INTEGER", notNull: 1},
			{name: "repo_name", columnType: "TEXT", notNull: 1},
			{name: "branch", columnType: "TEXT"},
			{name: "git_ref", columnType: "TEXT", notNull: 1},
			{name: "commit_sha", columnType: "TEXT"},
			{name: "finished_at", columnType: "TEXT"},
			{name: "verdict", columnType: "TEXT"},
			{name: "closed", columnType: "INTEGER", notNull: 1},
			{name: "panel_role", columnType: "TEXT"},
			{name: "content", columnType: "TEXT", notNull: 1},
			{name: "content_hash", columnType: "TEXT", notNull: 1},
			{name: "embed_gen", columnType: "TEXT"},
			{name: "share_state", columnType: "INTEGER", notNull: 1},
		},
	},
	{
		name: "review_exchange",
		columns: []columnDefinition{
			{name: "doc_key", columnType: "TEXT", primaryKey: 1},
			{name: "gen_key", columnType: "TEXT", notNull: 1},
			{name: "content_hash", columnType: "TEXT", notNull: 1},
			{name: "first_pending_at", columnType: "INTEGER", notNull: 1},
			{name: "next_attempt_at", columnType: "INTEGER", notNull: 1},
			{name: "attempts", columnType: "INTEGER", notNull: 1},
			{name: "claim_expires_at", columnType: "INTEGER", notNull: 1},
			{name: "origin", columnType: "TEXT", notNull: 1},
			{name: "published_target", columnType: "TEXT", notNull: 1},
		},
	},
	{
		name: "review_fts",
		columns: []columnDefinition{
			{name: "doc_key"},
			{name: "content"},
			{name: "identifiers"},
		},
	},
}

type schemaMismatchError struct{ reason string }

func (e *schemaMismatchError) Error() string {
	return "search sidecar schema mismatch: " + e.reason
}

func createBaseSchema(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin search sidecar schema: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, sidecarSchema); err != nil {
		return fmt.Errorf("create search sidecar schema: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit search sidecar schema: %w", err)
	}
	return nil
}

func validateSchema(ctx context.Context, db *sql.DB) error {
	for _, expected := range expectedSchema {
		columns, err := tableColumns(ctx, db, expected.name)
		if err != nil {
			return fmt.Errorf("inspect search sidecar table %s: %w", expected.name, err)
		}
		if !slices.Equal(columns, expected.columns) {
			return &schemaMismatchError{reason: fmt.Sprintf("table %s has columns %+v, expected %+v", expected.name, columns, expected.columns)}
		}
	}

	var version string
	err := db.QueryRowContext(ctx,
		`SELECT value FROM search_meta WHERE key = 'schema_version'`).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		return &schemaMismatchError{reason: "schema version row is missing"}
	}
	if err != nil {
		return fmt.Errorf("read search sidecar schema version: %w", err)
	}
	if version != sidecarSchemaVersion {
		return &schemaMismatchError{reason: fmt.Sprintf("version %q, expected %q", version, sidecarSchemaVersion)}
	}

	ftsSQL, found, err := readTableSQL(ctx, db, "review_fts")
	if err != nil {
		return fmt.Errorf("read review_fts definition: %w", err)
	}
	if !found || compactSQL(ftsSQL) != "createvirtualtablereview_ftsusingfts5(doc_keyunindexed,content,identifiers,tokenize='unicode61')" {
		return &schemaMismatchError{reason: "review_fts is not the expected FTS5 virtual table"}
	}
	return nil
}

func tableColumns(ctx context.Context, db *sql.DB, table string) ([]columnDefinition, error) {
	rows, err := db.QueryContext(ctx, fmt.Sprintf(`PRAGMA table_info(%s)`, table))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var columns []columnDefinition
	for rows.Next() {
		var cid int
		var column columnDefinition
		var defaultValue any
		if err := rows.Scan(&cid, &column.name, &column.columnType, &column.notNull, &defaultValue, &column.primaryKey); err != nil {
			return nil, err
		}
		column.columnType = strings.ToUpper(column.columnType)
		columns = append(columns, column)
	}
	return columns, rows.Err()
}

func readTableSQL(ctx context.Context, db *sql.DB, table string) (string, bool, error) {
	var definition string
	err := db.QueryRowContext(ctx,
		`SELECT sql FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&definition)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return definition, true, nil
}

func compactSQL(statement string) string {
	return strings.ToLower(strings.Join(strings.Fields(statement), ""))
}
