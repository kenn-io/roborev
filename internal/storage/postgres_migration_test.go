//go:build postgres

package storage

import (
	"context"
	_ "embed"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

//go:embed schemas/postgres_v11.sql
var postgresV11Schema string

//go:embed schemas/postgres_v13.sql
var postgresV13Schema string

//go:embed schemas/postgres_v14.sql
var postgresV14Schema string

// openTestPgPoolRawAtVersion bootstraps a fresh Postgres test pool at the
// given older schema version by running only the corresponding embedded
// schema file and seeding schema_version. It deliberately does NOT call
// EnsureSchema, so a subsequent openTestPgPool(t) call triggers the upgrade
// path under test.
func openTestPgPoolRawAtVersion(t *testing.T, version int) *PgPool {
	t.Helper()
	connString := getTestPostgresURL(t)
	ctx := t.Context()

	pool, err := NewPgPool(ctx, connString, DefaultPgPoolConfig())
	require.NoError(t, err, "Failed to connect: %v")
	t.Cleanup(func() { pool.Close() })

	// Wipe any pre-existing roborev schema so we start clean.
	_, err = pool.pool.Exec(ctx, `DROP SCHEMA IF EXISTS roborev CASCADE`)
	require.NoError(t, err, "drop existing roborev schema")

	schemas := map[int]string{
		11: postgresV11Schema,
		13: postgresV13Schema,
		14: postgresV14Schema,
	}
	schemaSQL, ok := schemas[version]
	require.Truef(t, ok, "openTestPgPoolRawAtVersion: no embedded schema for version %d", version)

	for _, stmt := range strings.Split(schemaSQL, ";") {
		s := strings.TrimSpace(stmt)
		if s == "" {
			continue
		}
		// Skip pure comment-only statements
		isComment := true
		for _, line := range strings.Split(s, "\n") {
			if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "--") {
				isComment = false
				break
			}
		}
		if isComment {
			continue
		}
		_, err = pool.pool.Exec(ctx, s)
		require.NoError(t, err, "execute statement: %s", s)
	}

	_, err = pool.pool.Exec(ctx,
		`INSERT INTO roborev.schema_version (version) VALUES ($1) ON CONFLICT (version) DO NOTHING`, version)
	require.NoError(t, err, "seed schema_version")

	return pool
}

// pgxPool returns the underlying pgxpool.Pool for low-level access in tests.
func pgxPool(p *PgPool) *pgxpool.Pool { return p.pool }

func TestPostgresMigration_SkipReasonAndClassify(t *testing.T) {
	// Bootstrap at v11 (pre skip_reason/source) so this exercises the v12
	// migration it is named for, independent of the current schema version.
	oldPool := openTestPgPoolRawAtVersion(t, 11)
	ctx := context.Background()

	var repoID int
	require.NoError(t, pgxPool(oldPool).QueryRow(ctx,
		`INSERT INTO roborev.repos (identity) VALUES ($1) RETURNING id`,
		"git@example.com:owner/test-repo.git").Scan(&repoID))
	jobUUID := uuid.New().String()
	_, err := pgxPool(oldPool).Exec(ctx, `
		INSERT INTO roborev.review_jobs
		  (uuid, repo_id, git_ref, agent, status, enqueued_at, source_machine_id)
		VALUES ($1, $2, 'abc', 'test', 'done', NOW(), $3)
	`, jobUUID, repoID, uuid.New().String())
	require.NoError(t, err)
	_, err = pgxPool(oldPool).Exec(ctx, `
		INSERT INTO roborev.responses
		  (uuid, job_uuid, responder, response, source_machine_id, created_at)
		VALUES ($1, $2, 'human', 'legacy response', $3, NOW())
	`, uuid.New().String(), jobUUID, uuid.New().String())
	require.NoError(t, err)

	pg := openTestPgPool(t)
	defer pg.Close()

	var n int
	require.NoError(t, pgxPool(pg).QueryRow(ctx,
		`SELECT COUNT(*) FROM roborev.review_jobs WHERE git_ref = 'abc'`).Scan(&n))
	require.Equal(t, 1, n)
	require.NoError(t, pgxPool(pg).QueryRow(ctx,
		`SELECT COUNT(*) FROM roborev.responses WHERE inserted_at IS NOT NULL`).Scan(&n))
	require.Equal(t, 1, n)

	machineID := uuid.New().String()
	_, err = pgxPool(pg).Exec(ctx, `
		INSERT INTO roborev.review_jobs
		  (uuid, repo_id, git_ref, agent, job_type, review_type, status, enqueued_at,
		   source_machine_id, skip_reason, source)
		VALUES ($1, $2, 'after', 'test', 'review', 'design', 'skipped', NOW(), $3, 'trivial', 'auto_design')
	`, uuid.New().String(), repoID, machineID)
	require.NoError(t, err)
}

func TestPostgresMigration_ResponseInsertedAt(t *testing.T) {
	oldPool := openTestPgPoolRawAtVersion(t, 13)
	ctx := context.Background()

	var repoID int
	require.NoError(t, pgxPool(oldPool).QueryRow(ctx,
		`INSERT INTO roborev.repos (identity) VALUES ($1) RETURNING id`,
		"git@example.com:owner/response-migration.git").Scan(&repoID))
	jobUUID := uuid.New().String()
	_, err := pgxPool(oldPool).Exec(ctx, `
		INSERT INTO roborev.review_jobs
		  (uuid, repo_id, git_ref, agent, status, enqueued_at, source_machine_id)
		VALUES ($1, $2, 'abc', 'test', 'done', NOW(), $3)
	`, jobUUID, repoID, uuid.New().String())
	require.NoError(t, err)
	_, err = pgxPool(oldPool).Exec(ctx, `
		INSERT INTO roborev.responses
		  (uuid, job_uuid, responder, response, source_machine_id, created_at)
		VALUES ($1, $2, 'human', 'legacy response', $3, NOW())
	`, uuid.New().String(), jobUUID, uuid.New().String())
	require.NoError(t, err)

	pg := openTestPgPool(t)
	defer pg.Close()

	var n int
	require.NoError(t, pgxPool(pg).QueryRow(ctx,
		`SELECT COUNT(*) FROM roborev.responses WHERE inserted_at IS NOT NULL`).Scan(&n))
	require.Equal(t, 1, n)

	var indexExists bool
	require.NoError(t, pgxPool(pg).QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM pg_indexes
			WHERE schemaname = 'roborev' AND indexname = 'idx_responses_inserted'
		)
	`).Scan(&indexExists))
	require.True(t, indexExists)
}

// TestPostgresMigration_PanelColumns verifies that upgrading a pre-panel (v13)
// database to the current schema adds the panel columns and the panel index
// without error. Regression guard: the embedded base schema is replayed on
// every startup, so a panel index in the base schema would try to index
// columns that the v15 migration has not added yet and abort the upgrade.
func TestPostgresMigration_PanelColumns(t *testing.T) {
	oldPool := openTestPgPoolRawAtVersion(t, 13)
	ctx := context.Background()

	var repoID int
	require.NoError(t, pgxPool(oldPool).QueryRow(ctx,
		`INSERT INTO roborev.repos (identity) VALUES ($1) RETURNING id`,
		"git@example.com:owner/panel-migrate.git").Scan(&repoID))
	// A v13-shaped row has no panel columns.
	_, err := pgxPool(oldPool).Exec(ctx, `
		INSERT INTO roborev.review_jobs
		  (uuid, repo_id, git_ref, agent, status, enqueued_at, source_machine_id)
		VALUES ($1, $2, 'pre-panel', 'test', 'done', NOW(), $3)
	`, uuid.New().String(), repoID, uuid.New().String())
	require.NoError(t, err)

	// Triggers the v13->current upgrade (v14 inserted_at, then v15 panels +
	// backup). Fails here if the base-schema replay tries to index panel
	// columns before the migration adds them.
	pg := openTestPgPool(t)
	defer pg.Close()

	for _, col := range []string{
		"panel_run_uuid", "panel_role", "panel_name",
		"panel_member_name", "panel_member_index", "panel_member_config_json",
	} {
		var n int
		require.NoError(t, pgxPool(pg).QueryRow(ctx, `
			SELECT COUNT(*) FROM information_schema.columns
			WHERE table_schema = 'roborev' AND table_name = 'review_jobs' AND column_name = $1
		`, col).Scan(&n))
		require.Equal(t, 1, n, "column %s should exist after v15 upgrade", col)
	}

	var idxCount int
	require.NoError(t, pgxPool(pg).QueryRow(ctx, `
		SELECT COUNT(*) FROM pg_indexes
		WHERE schemaname = 'roborev' AND indexname = 'idx_review_jobs_panel'
	`).Scan(&idxCount))
	require.Equal(t, 1, idxCount, "idx_review_jobs_panel should exist after v15 upgrade")

	// The pre-existing row survived the upgrade.
	var n int
	require.NoError(t, pgxPool(pg).QueryRow(ctx,
		`SELECT COUNT(*) FROM roborev.review_jobs WHERE git_ref = 'pre-panel'`).Scan(&n))
	require.Equal(t, 1, n)
}

// TestPostgresMigration_BackupColumns verifies that upgrading a pre-backup
// (v14) database to the current schema adds backup_agent/backup_model (F7)
// without error and preserves existing rows.
func TestPostgresMigration_BackupColumns(t *testing.T) {
	oldPool := openTestPgPoolRawAtVersion(t, 14)
	ctx := context.Background()

	var repoID int
	require.NoError(t, pgxPool(oldPool).QueryRow(ctx,
		`INSERT INTO roborev.repos (identity) VALUES ($1) RETURNING id`,
		"git@example.com:owner/backup-migrate.git").Scan(&repoID))
	// A v14-shaped row has no backup columns.
	_, err := pgxPool(oldPool).Exec(ctx, `
		INSERT INTO roborev.review_jobs
		  (uuid, repo_id, git_ref, agent, status, enqueued_at, source_machine_id)
		VALUES ($1, $2, 'pre-backup', 'test', 'done', NOW(), $3)
	`, uuid.New().String(), repoID, uuid.New().String())
	require.NoError(t, err)

	// Triggers the v14->v15 upgrade.
	pg := openTestPgPool(t)
	defer pg.Close()

	for _, col := range []string{"backup_agent", "backup_model"} {
		var n int
		require.NoError(t, pgxPool(pg).QueryRow(ctx, `
			SELECT COUNT(*) FROM information_schema.columns
			WHERE table_schema = 'roborev' AND table_name = 'review_jobs' AND column_name = $1
		`, col).Scan(&n))
		require.Equal(t, 1, n, "column %s should exist after v15 upgrade", col)
	}

	// The pre-existing row survived and backfilled to '' (NOT NULL DEFAULT '').
	var ba, bm string
	require.NoError(t, pgxPool(pg).QueryRow(ctx,
		`SELECT backup_agent, backup_model FROM roborev.review_jobs WHERE git_ref = 'pre-backup'`,
	).Scan(&ba, &bm))
	require.Empty(t, ba)
	require.Empty(t, bm)
}
