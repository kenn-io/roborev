package storage

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgreSQL schema version - increment when schema changes
const pgSchemaVersion = 12

// pgSchemaName is the PostgreSQL schema used to isolate roborev tables
const pgSchemaName = "roborev"

//go:embed schemas/postgres_v12.sql
var pgSchemaSQL string

// pgSchemaStatements returns the individual DDL statements for schema creation.
// Parsed from the embedded SQL file.
func pgSchemaStatements() []string {
	var stmts []string
	for stmt := range strings.SplitSeq(pgSchemaSQL, ";") {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" {
			continue
		}
		// Skip pure comment lines
		lines := strings.Split(stmt, "\n")
		hasCode := false
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if line != "" && !strings.HasPrefix(line, "--") {
				hasCode = true
				break
			}
		}
		if hasCode {
			stmts = append(stmts, stmt)
		}
	}
	return stmts
}

// PgPool wraps a pgx connection pool with reconnection logic
type PgPool struct {
	pool       *pgxpool.Pool
	connString string
	config     PgPoolConfig
}

// PgPoolConfig configures the PostgreSQL connection pool
type PgPoolConfig struct {
	// ConnectTimeout is the timeout for initial connection (default: 5s)
	ConnectTimeout time.Duration
	// MaxConns is the maximum number of connections (default: 4)
	MaxConns int32
	// MinConns is the minimum number of connections (default: 0)
	MinConns int32
	// MaxConnLifetime is the maximum lifetime of a connection (default: 1h)
	MaxConnLifetime time.Duration
	// MaxConnIdleTime is the maximum idle time before closing (default: 30m)
	MaxConnIdleTime time.Duration
}

// DefaultPgPoolConfig returns sensible defaults for the connection pool
func DefaultPgPoolConfig() PgPoolConfig {
	return PgPoolConfig{
		ConnectTimeout:  5 * time.Second,
		MaxConns:        4,
		MinConns:        0,
		MaxConnLifetime: time.Hour,
		MaxConnIdleTime: 30 * time.Minute,
	}
}

// NewPgPool creates a new PostgreSQL connection pool.
// The connection string should be a PostgreSQL URL like:
// postgres://user:pass@host:port/dbname?sslmode=disable
func NewPgPool(ctx context.Context, connString string, cfg PgPoolConfig) (*PgPool, error) {
	poolCfg, err := pgxpool.ParseConfig(connString)
	if err != nil {
		return nil, fmt.Errorf("parse connection string: %w", err)
	}

	// Apply configuration
	poolCfg.MaxConns = cfg.MaxConns
	poolCfg.MinConns = cfg.MinConns
	poolCfg.MaxConnLifetime = cfg.MaxConnLifetime
	poolCfg.MaxConnIdleTime = cfg.MaxConnIdleTime

	// Set search_path to roborev schema on each connection.
	// Try setting search_path first; if schema doesn't exist, create it.
	// This avoids requiring CREATE privilege when schema already exists.
	poolCfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "SET search_path TO "+pgSchemaName)
		if err != nil {
			// Schema doesn't exist - create it and retry
			if _, createErr := conn.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS "+pgSchemaName); createErr != nil {
				return createErr
			}
			_, err = conn.Exec(ctx, "SET search_path TO "+pgSchemaName)
		}
		return err
	}

	// Create context with timeout for initial connection
	connectCtx, cancel := context.WithTimeout(ctx, cfg.ConnectTimeout)
	defer cancel()

	pool, err := pgxpool.NewWithConfig(connectCtx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("connect to postgres: %w", err)
	}

	// Verify connection
	if err := pool.Ping(connectCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}

	return &PgPool{
		pool:       pool,
		connString: connString,
		config:     cfg,
	}, nil
}

// Close closes the connection pool
func (p *PgPool) Close() {
	if p.pool != nil {
		p.pool.Close()
	}
}

// Pool returns the underlying pgxpool.Pool for direct access
func (p *PgPool) Pool() *pgxpool.Pool {
	return p.pool
}

// EnsureSchema creates the schema if it doesn't exist and checks version.
// If legacy tables exist in the public schema, they are migrated to roborev.
func (p *PgPool) EnsureSchema(ctx context.Context) error {
	// Migrate legacy tables from public schema if they exist
	if err := p.migrateLegacyTables(ctx); err != nil {
		return fmt.Errorf("migrate legacy tables: %w", err)
	}

	// Execute each schema statement individually since pgx prepared
	// statement mode doesn't support multi-statement execution
	for _, stmt := range pgSchemaStatements() {
		if _, err := p.pool.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("create schema: %w", err)
		}
	}

	// Check/insert schema version using ON CONFLICT to handle concurrent initializers
	var currentVersion int
	err := p.pool.QueryRow(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&currentVersion)
	if err != nil {
		return fmt.Errorf("check schema version: %w", err)
	}

	if currentVersion == 0 {
		// First time - insert version with ON CONFLICT to handle races
		_, err = p.pool.Exec(ctx, `INSERT INTO schema_version (version) VALUES ($1) ON CONFLICT (version) DO NOTHING`, pgSchemaVersion)
		if err != nil {
			return fmt.Errorf("insert schema version: %w", err)
		}
		// Create indexes not in base schema (to support upgrades from older versions)
		_, err = p.pool.Exec(ctx, `CREATE INDEX IF NOT EXISTS idx_review_jobs_branch ON review_jobs(branch)`)
		if err != nil {
			return fmt.Errorf("create branch index: %w", err)
		}
		_, err = p.pool.Exec(ctx, `CREATE INDEX IF NOT EXISTS idx_review_jobs_job_type ON review_jobs(job_type)`)
		if err != nil {
			return fmt.Errorf("create job_type index: %w", err)
		}
		_, err = p.pool.Exec(ctx, `CREATE INDEX IF NOT EXISTS idx_review_jobs_patch_id ON review_jobs(patch_id)`)
		if err != nil {
			return fmt.Errorf("create patch_id index: %w", err)
		}
	} else if currentVersion > pgSchemaVersion {
		return fmt.Errorf("database schema version %d is newer than supported version %d", currentVersion, pgSchemaVersion)
	} else if currentVersion < pgSchemaVersion {
		// Run migrations
		if currentVersion < 2 {
			// Migration 1->2: Add model column to review_jobs
			_, err = p.pool.Exec(ctx, `ALTER TABLE review_jobs ADD COLUMN IF NOT EXISTS model TEXT`)
			if err != nil {
				return fmt.Errorf("migrate to v2 (add model column): %w", err)
			}
		}
		if currentVersion < 3 {
			// Migration 2->3: Add branch column to review_jobs
			_, err = p.pool.Exec(ctx, `ALTER TABLE review_jobs ADD COLUMN IF NOT EXISTS branch TEXT`)
			if err != nil {
				return fmt.Errorf("migrate to v3 (add branch column): %w", err)
			}
			// Add index for branch filtering
			_, err = p.pool.Exec(ctx, `CREATE INDEX IF NOT EXISTS idx_review_jobs_branch ON review_jobs(branch)`)
			if err != nil {
				return fmt.Errorf("migrate to v3 (add branch index): %w", err)
			}
		}
		if currentVersion < 4 {
			// Migration 3->4: Add job_type column to review_jobs
			_, err = p.pool.Exec(ctx, `ALTER TABLE review_jobs ADD COLUMN IF NOT EXISTS job_type TEXT NOT NULL DEFAULT 'review'`)
			if err != nil {
				return fmt.Errorf("migrate to v4 (add job_type column): %w", err)
			}
			// Backfill job_type for existing rows
			_, err = p.pool.Exec(ctx, `UPDATE review_jobs SET job_type = 'dirty' WHERE (git_ref = 'dirty' OR diff_content IS NOT NULL) AND job_type = 'review'`)
			if err != nil {
				return fmt.Errorf("migrate to v4 (backfill dirty): %w", err)
			}
			_, err = p.pool.Exec(ctx, `UPDATE review_jobs SET job_type = 'range' WHERE git_ref LIKE '%..%' AND commit_id IS NULL AND job_type = 'review'`)
			if err != nil {
				return fmt.Errorf("migrate to v4 (backfill range): %w", err)
			}
			_, err = p.pool.Exec(ctx, `UPDATE review_jobs SET job_type = 'task' WHERE commit_id IS NULL AND diff_content IS NULL AND git_ref != 'dirty' AND git_ref NOT LIKE '%..%' AND git_ref != '' AND job_type = 'review'`)
			if err != nil {
				return fmt.Errorf("migrate to v4 (backfill task): %w", err)
			}
			// Add index for job_type filtering
			_, err = p.pool.Exec(ctx, `CREATE INDEX IF NOT EXISTS idx_review_jobs_job_type ON review_jobs(job_type)`)
			if err != nil {
				return fmt.Errorf("migrate to v4 (add job_type index): %w", err)
			}
			// Add review_type column
			_, err = p.pool.Exec(ctx, `ALTER TABLE review_jobs ADD COLUMN IF NOT EXISTS review_type TEXT NOT NULL DEFAULT ''`)
			if err != nil {
				return fmt.Errorf("migrate to v4 (add review_type column): %w", err)
			}
		}
		if currentVersion < 5 {
			// Migration 4->5: Add patch_id column to review_jobs
			_, err = p.pool.Exec(ctx, `ALTER TABLE review_jobs ADD COLUMN IF NOT EXISTS patch_id TEXT`)
			if err != nil {
				return fmt.Errorf("migrate to v5 (add patch_id column): %w", err)
			}
			_, err = p.pool.Exec(ctx, `CREATE INDEX IF NOT EXISTS idx_review_jobs_patch_id ON review_jobs(patch_id)`)
			if err != nil {
				return fmt.Errorf("migrate to v5 (add patch_id index): %w", err)
			}
		}
		if currentVersion < 6 {
			// Migration 5->6: Rename addressed to closed in reviews.
			// Idempotent: skip if addressed column doesn't exist
			// (fresh installs create the table with closed directly).
			_, err = p.pool.Exec(ctx, `
				DO $$ BEGIN
					IF EXISTS (
						SELECT 1 FROM information_schema.columns
						WHERE table_schema = 'roborev'
						AND table_name = 'reviews'
						AND column_name = 'addressed'
					) THEN
						ALTER TABLE reviews
							RENAME COLUMN addressed TO closed;
					END IF;
				END $$`)
			if err != nil {
				return fmt.Errorf("migrate to v6 (rename addressed to closed): %w", err)
			}
		}
		if currentVersion < 7 {
			_, err = p.pool.Exec(ctx, `ALTER TABLE review_jobs ADD COLUMN IF NOT EXISTS session_id TEXT`)
			if err != nil {
				return fmt.Errorf("migrate to v7 (add session_id column): %w", err)
			}
		}
		if currentVersion < 8 {
			_, err = p.pool.Exec(ctx, `ALTER TABLE review_jobs ADD COLUMN IF NOT EXISTS token_usage TEXT`)
			if err != nil {
				return fmt.Errorf("migrate to v8 (add token_usage column): %w", err)
			}
		}
		if currentVersion < 9 {
			_, err = p.pool.Exec(ctx, `ALTER TABLE review_jobs ADD COLUMN IF NOT EXISTS worktree_path TEXT`)
			if err != nil {
				return fmt.Errorf("migrate to v9 (add worktree_path column): %w", err)
			}
		}
		if currentVersion < 10 {
			_, err = p.pool.Exec(ctx, `ALTER TABLE review_jobs ADD COLUMN IF NOT EXISTS provider TEXT`)
			if err != nil {
				return fmt.Errorf("migrate to v10 (add provider column): %w", err)
			}
			_, err = p.pool.Exec(ctx, `ALTER TABLE review_jobs ADD COLUMN IF NOT EXISTS requested_model TEXT`)
			if err != nil {
				return fmt.Errorf("migrate to v10 (add requested_model column): %w", err)
			}
			_, err = p.pool.Exec(ctx, `ALTER TABLE review_jobs ADD COLUMN IF NOT EXISTS requested_provider TEXT`)
			if err != nil {
				return fmt.Errorf("migrate to v10 (add requested_provider column): %w", err)
			}
		}
		if currentVersion < 11 {
			_, err = p.pool.Exec(ctx, `ALTER TABLE review_jobs ADD COLUMN IF NOT EXISTS min_severity TEXT NOT NULL DEFAULT ''`)
			if err != nil {
				return fmt.Errorf("v11 migration (min_severity): %w", err)
			}
		}
		if currentVersion < 12 {
			// Auto design review support — skip_reason, source columns, and
			// dedup indexes. (job_type has no CHECK constraint; 'classify'
			// is accepted as-is.)
			if _, err = p.pool.Exec(ctx, `ALTER TABLE review_jobs ADD COLUMN IF NOT EXISTS skip_reason TEXT`); err != nil {
				return fmt.Errorf("v12 migration (add skip_reason): %w", err)
			}
			if _, err = p.pool.Exec(ctx, `ALTER TABLE review_jobs ADD COLUMN IF NOT EXISTS source TEXT`); err != nil {
				return fmt.Errorf("v12 migration (add source): %w", err)
			}
			if _, err = p.pool.Exec(ctx, `ALTER TABLE review_jobs DROP CONSTRAINT IF EXISTS review_jobs_status_check`); err != nil {
				return fmt.Errorf("v12 migration (drop status check): %w", err)
			}
			if _, err = p.pool.Exec(ctx, `ALTER TABLE review_jobs ADD CONSTRAINT review_jobs_status_check
				CHECK (status IN ('queued','running','done','failed','canceled','applied','rebased','skipped'))`); err != nil {
				return fmt.Errorf("v12 migration (add status check): %w", err)
			}
			if _, err = p.pool.Exec(ctx, `CREATE UNIQUE INDEX IF NOT EXISTS idx_review_jobs_auto_design_dedup
				ON review_jobs(repo_id, commit_id, review_type)
				WHERE source = 'auto_design'`); err != nil {
				return fmt.Errorf("v12 migration (add dedup index): %w", err)
			}
			if _, err = p.pool.Exec(ctx, `CREATE UNIQUE INDEX IF NOT EXISTS idx_review_jobs_auto_design_dedup_ref
				ON review_jobs(repo_id, git_ref, review_type)
				WHERE source = 'auto_design' AND commit_id IS NULL`); err != nil {
				return fmt.Errorf("v12 migration (add dedup ref index): %w", err)
			}
		}
		// Update version
		_, err = p.pool.Exec(ctx, `INSERT INTO schema_version (version) VALUES ($1) ON CONFLICT (version) DO NOTHING`, pgSchemaVersion)
		if err != nil {
			return fmt.Errorf("update schema version: %w", err)
		}
	}

	// Auto-design dedup indexes are created unconditionally (with IF
	// NOT EXISTS) on every startup so a DB that was interrupted
	// between the schema_version insert and a prior attempt to create
	// these indexes still self-heals — otherwise currentVersion==12
	// would skip both the fresh-init block and the v12 migration on
	// the next run and the uniqueness guarantee would be permanently
	// lost.
	if _, err := p.pool.Exec(ctx, `CREATE UNIQUE INDEX IF NOT EXISTS idx_review_jobs_auto_design_dedup
		ON review_jobs(repo_id, commit_id, review_type)
		WHERE source = 'auto_design'`); err != nil {
		return fmt.Errorf("ensure auto-design dedup index: %w", err)
	}
	// Widen the dedup_ref index to cover ALL auto_design rows (not
	// just commitless). The narrow form lets a (NULL, ref) row coexist
	// with a (resolved_id, ref) row for the same git_ref because
	// SQL's NULL != NULL semantics defeat the (repo_id, commit_id,
	// review_type) index. Widening enforces the cross-case dedup at
	// the storage layer. If duplicates already exist, fall back to
	// the narrow form so the daemon still starts.
	var dupes int
	if err := p.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM (
			SELECT 1 FROM review_jobs WHERE source = 'auto_design'
			GROUP BY repo_id, git_ref, review_type HAVING COUNT(*) > 1
		) t
	`).Scan(&dupes); err != nil {
		return fmt.Errorf("count auto-design duplicates: %w", err)
	}
	if dupes == 0 {
		if _, err := p.pool.Exec(ctx, `DROP INDEX IF EXISTS idx_review_jobs_auto_design_dedup_ref`); err != nil {
			return fmt.Errorf("drop narrow auto-design dedup ref index: %w", err)
		}
		if _, err := p.pool.Exec(ctx, `CREATE UNIQUE INDEX IF NOT EXISTS idx_review_jobs_auto_design_dedup_ref
			ON review_jobs(repo_id, git_ref, review_type)
			WHERE source = 'auto_design'`); err != nil {
			return fmt.Errorf("ensure wider auto-design dedup ref index: %w", err)
		}
	} else {
		log.Printf("auto-design (postgres): %d duplicate (repo, git_ref, review_type) groups exist; "+
			"keeping narrow dedup_ref index until cleaned up", dupes)
		if _, err := p.pool.Exec(ctx, `CREATE UNIQUE INDEX IF NOT EXISTS idx_review_jobs_auto_design_dedup_ref
			ON review_jobs(repo_id, git_ref, review_type)
			WHERE source = 'auto_design' AND commit_id IS NULL`); err != nil {
			return fmt.Errorf("ensure narrow auto-design dedup ref index: %w", err)
		}
	}

	return nil
}

// GetDatabaseID returns the unique ID for this Postgres database.
// Creates one if it doesn't exist. This ID is used to detect when
// a client is syncing to a different database than before.
func (p *PgPool) GetDatabaseID(ctx context.Context) (string, error) {
	var id string
	err := p.pool.QueryRow(ctx, `SELECT value FROM sync_metadata WHERE key = 'database_id'`).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("query database_id: %w", err)
	}

	// Generate new ID - use ON CONFLICT to handle concurrent creation
	newID := GenerateUUID()
	_, err = p.pool.Exec(ctx, `
		INSERT INTO sync_metadata (key, value) VALUES ('database_id', $1)
		ON CONFLICT (key) DO NOTHING
	`, newID)
	if err != nil {
		return "", fmt.Errorf("insert database_id: %w", err)
	}

	// Re-read in case another process inserted first
	err = p.pool.QueryRow(ctx, `SELECT value FROM sync_metadata WHERE key = 'database_id'`).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("re-read database_id: %w", err)
	}
	return id, nil
}

// pgLegacyTables lists tables that may exist in public schema from older installations
var pgLegacyTables = []string{
	"responses",
	"reviews",
	"review_jobs",
	"commits",
	"repos",
	"machines",
	"schema_version",
}

// migrateLegacyTables moves roborev tables from public schema to roborev schema.
// Handles concurrent execution and partial migration states gracefully.
func (p *PgPool) migrateLegacyTables(ctx context.Context) error {
	// Check if any legacy tables exist in public schema
	var hasLegacy bool
	err := p.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM information_schema.tables
			WHERE table_schema = 'public' AND table_name = 'schema_version'
		)
	`).Scan(&hasLegacy)
	if err != nil {
		return fmt.Errorf("check legacy tables: %w", err)
	}

	if !hasLegacy {
		return nil
	}

	// Ensure target schema exists before moving tables into it.
	// AfterConnect's SET search_path doesn't fail for missing schemas,
	// so the schema may not have been created yet.
	if _, err := p.pool.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS "+pgSchemaName); err != nil {
		return fmt.Errorf("create target schema: %w", err)
	}

	// Migrate tables in dependency order (reverse of pgLegacyTables)
	for _, table := range pgLegacyTables {
		// Check if table exists in public and not in roborev
		var existsInPublic, existsInRoborev bool
		err := p.pool.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM information_schema.tables
				WHERE table_schema = 'public' AND table_name = $1
			)
		`, table).Scan(&existsInPublic)
		if err != nil {
			return fmt.Errorf("check table %s in public: %w", table, err)
		}
		if !existsInPublic {
			continue
		}

		err = p.pool.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM information_schema.tables
				WHERE table_schema = $1 AND table_name = $2
			)
		`, pgSchemaName, table).Scan(&existsInRoborev)
		if err != nil {
			return fmt.Errorf("check table %s in roborev: %w", table, err)
		}

		if existsInRoborev {
			// Table exists in both schemas - this could mean data loss if rows remain in public
			var publicCount, roborevCount int64
			if err := p.pool.QueryRow(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM public.%s`, table)).Scan(&publicCount); err != nil {
				// Handle concurrent drop - treat as empty/gone
				if pgErr, ok := isPgError(err); ok && pgErr == "42P01" {
					continue
				}
				return fmt.Errorf("count rows in public.%s: %w", table, err)
			}
			if err := p.pool.QueryRow(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM %s.%s`, pgSchemaName, table)).Scan(&roborevCount); err != nil {
				// roborev table disappeared - if public still has data, try to move it
				if pgErr, ok := isPgError(err); ok && pgErr == "42P01" {
					if publicCount > 0 {
						// Fall through to move logic below by not continuing
						existsInRoborev = false
					} else {
						// public is empty, roborev gone - nothing to do
						continue
					}
				} else {
					return fmt.Errorf("count rows in %s.%s: %w", pgSchemaName, table, err)
				}
			}
			if existsInRoborev {
				if publicCount > 0 {
					return fmt.Errorf("table %s exists in both public (%d rows) and %s (%d rows) schemas; "+
						"manual reconciliation required - migrate data from public.%s to %s.%s then DROP TABLE public.%s",
						table, publicCount, pgSchemaName, roborevCount, table, pgSchemaName, table, table)
				}
				// public table is empty, safe to drop it
				if _, err := p.pool.Exec(ctx, fmt.Sprintf(`DROP TABLE public.%s`, table)); err != nil {
					// Ignore if already dropped by concurrent process
					if pgErr, ok := isPgError(err); ok && pgErr == "42P01" {
						continue
					}
					return fmt.Errorf("drop empty public.%s: %w", table, err)
				}
				continue
			}
		}

		// Move table to roborev schema
		_, err = p.pool.Exec(ctx, fmt.Sprintf(
			`ALTER TABLE public.%s SET SCHEMA %s`,
			table, pgSchemaName,
		))
		if err != nil {
			// Ignore "relation does not exist" (42P01) - table was moved by concurrent process
			// Ignore "relation already exists" (42P07) - table appeared in roborev concurrently
			if pgErr, ok := isPgError(err); ok && (pgErr == "42P01" || pgErr == "42P07") {
				continue
			}
			return fmt.Errorf("migrate table %s: %w", table, err)
		}
	}

	return nil
}

// isPgError checks if err is a PostgreSQL error and returns its SQLSTATE code.
// Uses errors.As to unwrap wrapped errors.
func isPgError(err error) (string, bool) {
	if err == nil {
		return "", false
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code, true
	}
	return "", false
}

// Ping checks if the connection is alive
func (p *PgPool) Ping(ctx context.Context) error {
	return p.pool.Ping(ctx)
}

// RegisterMachine registers or updates this machine in the machines table
func (p *PgPool) RegisterMachine(ctx context.Context, machineID, name string) error {
	_, err := p.pool.Exec(ctx, `
		INSERT INTO machines (machine_id, name, last_seen_at)
		VALUES ($1, $2, NOW())
		ON CONFLICT (machine_id) DO UPDATE SET
			name = COALESCE(EXCLUDED.name, machines.name),
			last_seen_at = NOW()
	`, machineID, name)
	if err != nil {
		return fmt.Errorf("register machine: %w", err)
	}
	return nil
}

// GetOrCreateRepo finds or creates a repo by identity, returns the PostgreSQL ID
func (p *PgPool) GetOrCreateRepo(ctx context.Context, identity string) (int64, error) {
	var id int64
	err := p.pool.QueryRow(ctx, `
		INSERT INTO repos (identity)
		VALUES ($1)
		ON CONFLICT (identity) DO UPDATE SET identity = EXCLUDED.identity
		RETURNING id
	`, identity).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("get or create repo: %w", err)
	}
	return id, nil
}

// GetOrCreateCommit finds or creates a commit, returns the PostgreSQL ID
func (p *PgPool) GetOrCreateCommit(ctx context.Context, repoID int64, sha, author, subject string, timestamp time.Time) (int64, error) {
	var id int64
	err := p.pool.QueryRow(ctx, `
		INSERT INTO commits (repo_id, sha, author, subject, timestamp)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (repo_id, sha) DO UPDATE SET sha = EXCLUDED.sha
		RETURNING id
	`, repoID, sha, author, subject, timestamp).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("get or create commit: %w", err)
	}
	return id, nil
}

// Tx runs a function within a transaction
func (p *PgPool) Tx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() {
		if err := tx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			return
		}
	}()

	if err := fn(tx); err != nil {
		return err
	}

	return tx.Commit(ctx)
}

// UpsertJob inserts or updates a job in PostgreSQL
func (p *PgPool) UpsertJob(ctx context.Context, j SyncableJob, pgRepoID int64, pgCommitID *int64) error {
	_, err := p.pool.Exec(ctx, `
		INSERT INTO review_jobs (
			uuid, repo_id, commit_id, git_ref, session_id, agent, model, provider, requested_model, requested_provider, reasoning, job_type, review_type, patch_id, status, agentic,
			enqueued_at, started_at, finished_at, prompt, diff_content, error, token_usage,
			worktree_path, min_severity, source_machine_id, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26, NOW())
		ON CONFLICT (uuid) DO UPDATE SET
			status = EXCLUDED.status,
			finished_at = EXCLUDED.finished_at,
			error = EXCLUDED.error,
			model = EXCLUDED.model,
			provider = EXCLUDED.provider,
			requested_model = EXCLUDED.requested_model,
			requested_provider = EXCLUDED.requested_provider,
			git_ref = EXCLUDED.git_ref,
			session_id = COALESCE(EXCLUDED.session_id, review_jobs.session_id),
			commit_id = EXCLUDED.commit_id,
			patch_id = EXCLUDED.patch_id,
			token_usage = COALESCE(EXCLUDED.token_usage, review_jobs.token_usage),
			worktree_path = COALESCE(EXCLUDED.worktree_path, review_jobs.worktree_path),
			min_severity = EXCLUDED.min_severity,
			updated_at = NOW()
	`, j.UUID, pgRepoID, pgCommitID, j.GitRef, nullString(j.SessionID), j.Agent, nullString(j.Model), nullString(j.Provider), nullString(j.RequestedModel), nullString(j.RequestedProvider), nullString(j.Reasoning),
		defaultStr(j.JobType, "review"), j.ReviewType, nullString(j.PatchID), j.Status, j.Agentic, j.EnqueuedAt, j.StartedAt, j.FinishedAt,
		nullString(j.Prompt), j.DiffContent, nullString(j.Error), nullString(j.TokenUsage), nullString(j.WorktreePath), normalizeMinSeverityForWrite(j.MinSeverity), j.SourceMachineID)
	return err
}

// UpsertReview inserts or updates a review in PostgreSQL
func (p *PgPool) UpsertReview(ctx context.Context, r SyncableReview) error {
	_, err := p.pool.Exec(ctx, `
		INSERT INTO reviews (
			uuid, job_uuid, agent, prompt, output, closed,
			updated_by_machine_id, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NOW())
		ON CONFLICT (uuid) DO UPDATE SET
			closed = EXCLUDED.closed,
			updated_by_machine_id = EXCLUDED.updated_by_machine_id,
			updated_at = NOW()
	`, r.UUID, r.JobUUID, r.Agent, r.Prompt, r.Output, r.Closed,
		r.UpdatedByMachineID, r.CreatedAt)
	return err
}

// InsertResponse inserts a response in PostgreSQL (append-only, no updates)
func (p *PgPool) InsertResponse(ctx context.Context, r SyncableResponse) error {
	_, err := p.pool.Exec(ctx, `
		INSERT INTO responses (
			uuid, job_uuid, responder, response, source_machine_id, created_at
		) VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (uuid) DO NOTHING
	`, r.UUID, r.JobUUID, r.Responder, r.Response, r.SourceMachineID, r.CreatedAt)
	return err
}

// PulledJob represents a job pulled from PostgreSQL
type PulledJob struct {
	UUID              string
	RepoIdentity      string
	CommitSHA         string
	CommitAuthor      string
	CommitSubject     string
	CommitTimestamp   time.Time
	GitRef            string
	SessionID         string
	Agent             string
	Model             string
	Provider          string
	RequestedModel    string
	RequestedProvider string
	Reasoning         string
	JobType           string
	ReviewType        string
	PatchID           string
	Status            string
	Agentic           bool
	EnqueuedAt        time.Time
	StartedAt         *time.Time
	FinishedAt        *time.Time
	Prompt            string
	DiffContent       *string
	Error             string
	TokenUsage        string
	WorktreePath      string
	MinSeverity       string
	SourceMachineID   string
	UpdatedAt         time.Time
}

// PullJobs fetches jobs from PostgreSQL updated after the given cursor.
// Cursor format: "updated_at id" (space-separated) or empty for first pull.
// Returns jobs not from the given machineID (to avoid echo).
func (p *PgPool) PullJobs(ctx context.Context, excludeMachineID string, cursor string, limit int) ([]PulledJob, string, error) {
	var cursorTime time.Time
	var cursorID int64

	if cursor != "" {
		var ts string
		_, err := fmt.Sscanf(cursor, "%s %d", &ts, &cursorID)
		if err == nil {
			cursorTime, _ = time.Parse(time.RFC3339Nano, ts)
		}
	}

	rows, err := p.pool.Query(ctx, `
		SELECT
			j.uuid, r.identity, COALESCE(c.sha, ''), COALESCE(c.author, ''), COALESCE(c.subject, ''), COALESCE(c.timestamp, '1970-01-01'::timestamptz),
			j.git_ref, COALESCE(j.session_id, ''), j.agent, COALESCE(j.model, ''), COALESCE(j.provider, ''), COALESCE(j.requested_model, ''), COALESCE(j.requested_provider, ''), COALESCE(j.reasoning, ''), COALESCE(j.job_type, 'review'), COALESCE(j.review_type, ''), COALESCE(j.patch_id, ''), j.status, j.agentic,
			j.enqueued_at, j.started_at, j.finished_at,
			COALESCE(j.prompt, ''), j.diff_content, COALESCE(j.error, ''), COALESCE(j.token_usage, ''),
			COALESCE(j.worktree_path, ''), COALESCE(j.min_severity, ''), j.source_machine_id, j.updated_at, j.id
		FROM review_jobs j
		JOIN repos r ON j.repo_id = r.id
		LEFT JOIN commits c ON j.commit_id = c.id
		WHERE (j.source_machine_id IS NULL OR j.source_machine_id != $1)
		AND (j.updated_at > $2 OR (j.updated_at = $2 AND j.id > $3))
		ORDER BY j.updated_at, j.id
		LIMIT $4
	`, excludeMachineID, cursorTime, cursorID, limit)
	if err != nil {
		return nil, cursor, fmt.Errorf("query jobs: %w", err)
	}
	defer rows.Close()

	var jobs []PulledJob
	var lastUpdatedAt time.Time
	var lastID int64

	for rows.Next() {
		var j PulledJob
		var diffContent *string

		err := rows.Scan(
			&j.UUID, &j.RepoIdentity, &j.CommitSHA, &j.CommitAuthor, &j.CommitSubject, &j.CommitTimestamp,
			&j.GitRef, &j.SessionID, &j.Agent, &j.Model, &j.Provider, &j.RequestedModel, &j.RequestedProvider, &j.Reasoning, &j.JobType, &j.ReviewType, &j.PatchID, &j.Status, &j.Agentic,
			&j.EnqueuedAt, &j.StartedAt, &j.FinishedAt,
			&j.Prompt, &diffContent, &j.Error, &j.TokenUsage,
			&j.WorktreePath, &j.MinSeverity, &j.SourceMachineID, &j.UpdatedAt, &lastID,
		)
		if err != nil {
			return nil, cursor, fmt.Errorf("scan job: %w", err)
		}

		j.DiffContent = diffContent
		lastUpdatedAt = j.UpdatedAt
		jobs = append(jobs, j)
	}

	if err := rows.Err(); err != nil {
		return nil, cursor, fmt.Errorf("rows error: %w", err)
	}

	// Update cursor if we got results
	newCursor := cursor
	if len(jobs) > 0 {
		newCursor = fmt.Sprintf("%s %d", lastUpdatedAt.Format(time.RFC3339Nano), lastID)
	}

	return jobs, newCursor, nil
}

// PulledReview represents a review pulled from PostgreSQL
type PulledReview struct {
	UUID               string
	JobUUID            string
	Agent              string
	Prompt             string
	Output             string
	Closed             bool
	UpdatedByMachineID string
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// PullReviews fetches reviews from PostgreSQL updated after the given cursor.
// Only fetches reviews for jobs in knownJobUUIDs to avoid cursor advancement past unknown jobs.
func (p *PgPool) PullReviews(ctx context.Context, excludeMachineID string, knownJobUUIDs []string, cursor string, limit int) ([]PulledReview, string, error) {
	var cursorTime time.Time
	var cursorID int64

	if cursor != "" {
		var ts string
		_, err := fmt.Sscanf(cursor, "%s %d", &ts, &cursorID)
		if err == nil {
			cursorTime, _ = time.Parse(time.RFC3339Nano, ts)
		}
	}

	// If no known jobs, return empty (no reviews can match)
	if len(knownJobUUIDs) == 0 {
		return nil, cursor, nil
	}

	rows, err := p.pool.Query(ctx, `
		SELECT
			r.uuid, r.job_uuid, r.agent, r.prompt, r.output, r.closed,
			r.updated_by_machine_id, r.created_at, r.updated_at, r.id
		FROM reviews r
		WHERE (r.updated_by_machine_id IS NULL OR r.updated_by_machine_id != $1)
		AND r.job_uuid = ANY($2)
		AND (r.updated_at > $3 OR (r.updated_at = $3 AND r.id > $4))
		ORDER BY r.updated_at, r.id
		LIMIT $5
	`, excludeMachineID, knownJobUUIDs, cursorTime, cursorID, limit)
	if err != nil {
		return nil, cursor, fmt.Errorf("query reviews: %w", err)
	}
	defer rows.Close()

	var reviews []PulledReview
	var lastUpdatedAt time.Time
	var lastID int64

	for rows.Next() {
		var r PulledReview

		err := rows.Scan(
			&r.UUID, &r.JobUUID, &r.Agent, &r.Prompt, &r.Output, &r.Closed,
			&r.UpdatedByMachineID, &r.CreatedAt, &r.UpdatedAt, &lastID,
		)
		if err != nil {
			return nil, cursor, fmt.Errorf("scan review: %w", err)
		}

		lastUpdatedAt = r.UpdatedAt
		reviews = append(reviews, r)
	}

	if err := rows.Err(); err != nil {
		return nil, cursor, fmt.Errorf("rows error: %w", err)
	}

	newCursor := cursor
	if len(reviews) > 0 {
		newCursor = fmt.Sprintf("%s %d", lastUpdatedAt.Format(time.RFC3339Nano), lastID)
	}

	return reviews, newCursor, nil
}

// PulledResponse represents a response pulled from PostgreSQL
type PulledResponse struct {
	UUID            string
	JobUUID         string
	Responder       string
	Response        string
	SourceMachineID string
	CreatedAt       time.Time
}

// PullResponses fetches responses from PostgreSQL created after the given ID cursor.
func (p *PgPool) PullResponses(ctx context.Context, excludeMachineID string, afterID int64, limit int) ([]PulledResponse, int64, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT
			r.uuid, r.job_uuid, r.responder, r.response, r.source_machine_id, r.created_at, r.id
		FROM responses r
		WHERE (r.source_machine_id IS NULL OR r.source_machine_id != $1)
		AND r.id > $2
		ORDER BY r.id
		LIMIT $3
	`, excludeMachineID, afterID, limit)
	if err != nil {
		return nil, afterID, fmt.Errorf("query responses: %w", err)
	}
	defer rows.Close()

	var responses []PulledResponse
	var lastID = afterID

	for rows.Next() {
		var r PulledResponse

		err := rows.Scan(
			&r.UUID, &r.JobUUID, &r.Responder, &r.Response, &r.SourceMachineID, &r.CreatedAt, &lastID,
		)
		if err != nil {
			return nil, afterID, fmt.Errorf("scan response: %w", err)
		}

		responses = append(responses, r)
	}

	if err := rows.Err(); err != nil {
		return nil, afterID, fmt.Errorf("rows error: %w", err)
	}

	return responses, lastID, nil
}

// nullString returns nil if s is empty, otherwise returns s
func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// defaultStr returns s if non-empty, otherwise returns the default.
// Used for NOT NULL columns that should never be nil.
func defaultStr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// BatchUpsertReviews inserts or updates multiple reviews in a single batch operation.
// Returns a boolean slice indicating success/failure for each item at the corresponding index.
func (p *PgPool) BatchUpsertReviews(ctx context.Context, reviews []SyncableReview) ([]bool, error) {
	if len(reviews) == 0 {
		return nil, nil
	}

	batch := &pgx.Batch{}
	for _, r := range reviews {
		batch.Queue(`
			INSERT INTO reviews (
				uuid, job_uuid, agent, prompt, output, closed,
				updated_by_machine_id, created_at, updated_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NOW())
			ON CONFLICT (uuid) DO UPDATE SET
				closed = EXCLUDED.closed,
				updated_by_machine_id = EXCLUDED.updated_by_machine_id,
				updated_at = NOW()
		`, r.UUID, r.JobUUID, r.Agent, r.Prompt, r.Output, r.Closed,
			r.UpdatedByMachineID, r.CreatedAt)
	}

	br := p.pool.SendBatch(ctx, batch)
	defer br.Close()

	success := make([]bool, len(reviews))
	var firstErr error
	for i := range reviews {
		_, err := br.Exec()
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		success[i] = true
	}

	return success, firstErr
}

// BatchInsertResponses inserts multiple responses in a single batch operation.
// Returns a boolean slice indicating success/failure for each item at the corresponding index.
func (p *PgPool) BatchInsertResponses(ctx context.Context, responses []SyncableResponse) ([]bool, error) {
	if len(responses) == 0 {
		return nil, nil
	}

	batch := &pgx.Batch{}
	for _, r := range responses {
		batch.Queue(`
			INSERT INTO responses (
				uuid, job_uuid, responder, response, source_machine_id, created_at
			) VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (uuid) DO NOTHING
		`, r.UUID, r.JobUUID, r.Responder, r.Response, r.SourceMachineID, r.CreatedAt)
	}

	br := p.pool.SendBatch(ctx, batch)
	defer br.Close()

	success := make([]bool, len(responses))
	var firstErr error
	for i := range responses {
		_, err := br.Exec()
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		success[i] = true
	}

	return success, firstErr
}

// JobWithPgIDs represents a job with its resolved PostgreSQL repo and commit IDs
type JobWithPgIDs struct {
	Job        SyncableJob
	PgRepoID   int64
	PgCommitID *int64
}

// BatchUpsertJobs inserts or updates multiple jobs in a single batch operation.
// The jobs must have their PgRepoID and PgCommitID already resolved.
// Returns a boolean slice indicating success/failure for each item at the corresponding index.
func (p *PgPool) BatchUpsertJobs(ctx context.Context, jobs []JobWithPgIDs) ([]bool, error) {
	if len(jobs) == 0 {
		return nil, nil
	}

	batch := &pgx.Batch{}
	for _, jw := range jobs {
		j := jw.Job
		batch.Queue(`
			INSERT INTO review_jobs (
				uuid, repo_id, commit_id, git_ref, session_id, agent, reasoning, job_type, review_type, patch_id, status, agentic,
				enqueued_at, started_at, finished_at, prompt, diff_content, error, token_usage,
				worktree_path, min_severity, source_machine_id, updated_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, NOW())
			ON CONFLICT (uuid) DO UPDATE SET
				status = EXCLUDED.status,
				finished_at = EXCLUDED.finished_at,
				error = EXCLUDED.error,
				git_ref = EXCLUDED.git_ref,
				session_id = COALESCE(EXCLUDED.session_id, review_jobs.session_id),
				commit_id = EXCLUDED.commit_id,
				patch_id = EXCLUDED.patch_id,
				token_usage = COALESCE(EXCLUDED.token_usage, review_jobs.token_usage),
				worktree_path = COALESCE(EXCLUDED.worktree_path, review_jobs.worktree_path),
				min_severity = EXCLUDED.min_severity,
				updated_at = NOW()
		`, j.UUID, jw.PgRepoID, jw.PgCommitID, j.GitRef, nullString(j.SessionID), j.Agent, nullString(j.Reasoning),
			defaultStr(j.JobType, "review"), j.ReviewType, nullString(j.PatchID), j.Status, j.Agentic, j.EnqueuedAt, j.StartedAt, j.FinishedAt,
			nullString(j.Prompt), j.DiffContent, nullString(j.Error), nullString(j.TokenUsage), nullString(j.WorktreePath), normalizeMinSeverityForWrite(j.MinSeverity), j.SourceMachineID)
	}

	br := p.pool.SendBatch(ctx, batch)
	defer br.Close()

	success := make([]bool, len(jobs))
	var firstErr error
	for i := range jobs {
		_, err := br.Exec()
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		success[i] = true
	}

	return success, firstErr
}
