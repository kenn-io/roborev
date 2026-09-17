package searchindex

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
)

const sidecarSchemaVersion = "1"

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
  embed_gen TEXT
);

CREATE VIRTUAL TABLE review_fts USING fts5(
  doc_key UNINDEXED,
  content,
  identifiers,
  tokenize = 'unicode61'
);

INSERT INTO search_meta(key, value) VALUES ('schema_version', '1');
`

type columnDefinition struct {
	name       string
	columnType string
	notNull    int
	primaryKey int
}

type tableDefinition struct {
	name      string
	columns   []columnDefinition
	uniqueKey []string
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
	{
		name: "review_vectors_generations",
		columns: []columnDefinition{
			{name: "ordinal", columnType: "INTEGER", primaryKey: 1},
			{name: "gen_key"},
			{name: "fingerprint", columnType: "TEXT", notNull: 1},
			{name: "dimension", columnType: "INTEGER", notNull: 1},
			{name: "state", columnType: "TEXT", notNull: 1},
		},
		uniqueKey: []string{"gen_key"},
	},
	{
		name: "review_vectors_chunks",
		columns: []columnDefinition{
			{name: "ordinal", columnType: "INTEGER", notNull: 1, primaryKey: 1},
			{name: "doc_key", notNull: 1, primaryKey: 2},
			{name: "chunk_index", columnType: "INTEGER", notNull: 1, primaryKey: 3},
			{name: "vec_rowid", columnType: "INTEGER", notNull: 1},
		},
	},
	{
		name: "review_vectors_stamps",
		columns: []columnDefinition{
			{name: "ordinal", columnType: "INTEGER", notNull: 1, primaryKey: 1},
			{name: "doc_key", notNull: 1, primaryKey: 2},
			{name: "revision"},
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
		if len(expected.uniqueKey) > 0 {
			hasKey, err := hasUniqueKey(ctx, db, expected.name, expected.uniqueKey)
			if err != nil {
				return fmt.Errorf("inspect search sidecar key on %s: %w", expected.name, err)
			}
			if !hasKey {
				return &schemaMismatchError{reason: fmt.Sprintf("table %s lacks unique key %v", expected.name, expected.uniqueKey)}
			}
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
	return validateGenerationTables(ctx, db)
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

func hasUniqueKey(ctx context.Context, db *sql.DB, table string, expected []string) (bool, error) {
	rows, err := db.QueryContext(ctx, fmt.Sprintf(`PRAGMA index_list(%s)`, table))
	if err != nil {
		return false, err
	}
	var uniqueIndexes []string
	for rows.Next() {
		var sequence, unique, partial int
		var name, origin string
		if err := rows.Scan(&sequence, &name, &unique, &origin, &partial); err != nil {
			_ = rows.Close()
			return false, err
		}
		if unique == 1 && partial == 0 {
			uniqueIndexes = append(uniqueIndexes, name)
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return false, err
	}
	if err := rows.Close(); err != nil {
		return false, err
	}
	for _, index := range uniqueIndexes {
		columns, err := indexColumns(ctx, db, index)
		if err != nil {
			return false, err
		}
		if slices.Equal(columns, expected) {
			return true, nil
		}
	}
	return false, nil
}

func indexColumns(ctx context.Context, db *sql.DB, index string) ([]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT name FROM pragma_index_info(?) ORDER BY seqno`, index)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var columns []string
	for rows.Next() {
		var column string
		if err := rows.Scan(&column); err != nil {
			return nil, err
		}
		columns = append(columns, column)
	}
	return columns, rows.Err()
}

func validateGenerationTables(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx,
		`SELECT ordinal, dimension FROM review_vectors_generations ORDER BY ordinal`)
	if err != nil {
		return fmt.Errorf("list search vector generations: %w", err)
	}
	type generation struct {
		ordinal   int64
		dimension int
	}
	var generations []generation
	for rows.Next() {
		var item generation
		if err := rows.Scan(&item.ordinal, &item.dimension); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan search vector generation: %w", err)
		}
		generations = append(generations, item)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("scan search vector generations: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close search vector generations: %w", err)
	}

	for _, item := range generations {
		name := fmt.Sprintf("review_vectors_v%d", item.ordinal)
		definition, found, err := readTableSQL(ctx, db, name)
		if err != nil {
			return fmt.Errorf("read search vector table %s: %w", name, err)
		}
		want := fmt.Sprintf("createvirtualtable%susingvec0(embeddingfloat[%d]distance_metric=cosine)", name, item.dimension)
		if item.dimension <= 0 || !found || compactSQL(definition) != want {
			return &schemaMismatchError{reason: fmt.Sprintf("generation %d lacks its %d-dimensional vec0 table", item.ordinal, item.dimension)}
		}
	}
	return nil
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
