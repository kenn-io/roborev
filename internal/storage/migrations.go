package storage

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"
)

// SQLite schema changes are numbered migrations recorded in
// schema_migrations. Rules:
//
//   - Append a new migration with the next version. Never edit, renumber,
//     or remove a migration that has reached main; fix mistakes with a new
//     migration.
//   - A migration is recorded only after it finishes, so an interrupted
//     migration runs again on the next start. Write it to be safe to rerun.
//   - A migration may manage its own transactions, for example to work in
//     batches or to run VACUUM, which SQLite does not allow in a transaction.
//
// The steps Open runs after the migrations on every start are not numbered:
// the auto-design dedup index widening, which waits until duplicate rows are
// gone, and the data steps (legacy review conversion, job ID preservation,
// verdict backfill).

// schemaMigration is one numbered step of the SQLite schema.
type schemaMigration struct {
	version int
	name    string
	apply   func(ctx context.Context, db *DB) error
}

// schemaMigrations lists every SQLite schema migration in version order.
var schemaMigrations = []schemaMigration{
	{1, "baseline", migrateBaseline},
}

// ErrSchemaTooNew reports a database written by a newer roborev release.
var ErrSchemaTooNew = errors.New("database schema is newer than this roborev release")

// migrateBaseline brings a database that predates numbered migrations to the
// schema of the release that introduced them. A new database takes the same
// path. Its steps check the current schema before each change, so it also
// accepts every older layout. It is frozen: later schema changes are new
// migrations, not edits to schema, migrate(), or the helpers migrate() calls.
func migrateBaseline(_ context.Context, db *DB) error {
	if _, err := db.Exec(schema); err != nil {
		return fmt.Errorf("initialize schema: %w", err)
	}
	return db.migrate()
}

// runSchemaMigrations applies every migration newer than the database's
// recorded version, in order.
func (db *DB) runSchemaMigrations(ctx context.Context, migrations []schemaMigration) error {
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY,
		name TEXT NOT NULL,
		applied_at TEXT NOT NULL
	)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}
	var current int
	if err := db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&current); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	latest := migrations[len(migrations)-1].version
	if current > latest {
		return fmt.Errorf("%w: the database is at schema version %d and this release supports up to %d; upgrade roborev",
			ErrSchemaTooNew, current, latest)
	}
	for _, m := range migrations {
		if m.version <= current {
			continue
		}
		log.Printf("Database migration %d: %s", m.version, m.name)
		if err := m.apply(ctx, db); err != nil {
			return fmt.Errorf("migration %d (%s): %w", m.version, m.name, err)
		}
		if _, err := db.ExecContext(ctx,
			`INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)`,
			m.version, m.name, time.Now().UTC().Format(time.RFC3339)); err != nil {
			return fmt.Errorf("record migration %d: %w", m.version, err)
		}
	}
	return nil
}
