package storage

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	bunschema "github.com/uptrace/bun/schema"
)

// PostgreSQL schema version - increment when schema changes
const pgSchemaVersion = 17

// pgSchemaName is the PostgreSQL schema used to isolate roborev tables
const pgSchemaName = "roborev"

//go:embed schemas/postgres_v17.sql
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
	bun        *bun.DB
	connString string
	config     PgPoolConfig
}

func newPostgresBunDB(db *sql.DB) *bun.DB {
	return bun.NewDB(db, pgdialect.New())
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
	sqldb := stdlib.OpenDBFromPool(pool)

	return &PgPool{
		pool:       pool,
		bun:        newPostgresBunDB(sqldb),
		connString: connString,
		config:     cfg,
	}, nil
}

// Close closes the connection pool
func (p *PgPool) Close() {
	if p.bun != nil {
		if err := p.bun.Close(); err != nil {
			log.Printf("close postgres Bun wrapper: %v", err)
		}
	}
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
		_, err = p.pool.Exec(ctx, `CREATE INDEX IF NOT EXISTS idx_review_jobs_panel ON review_jobs(panel_run_uuid, panel_role, panel_member_index)`)
		if err != nil {
			return fmt.Errorf("create panel index: %w", err)
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
		if currentVersion < 13 {
			if _, err = p.pool.Exec(ctx, `ALTER TABLE review_jobs ADD COLUMN IF NOT EXISTS retry_not_before TIMESTAMP WITH TIME ZONE`); err != nil {
				return fmt.Errorf("v13 migration (add retry_not_before): %w", err)
			}
		}
		if currentVersion < 14 {
			if _, err = p.pool.Exec(ctx, `ALTER TABLE responses ADD COLUMN IF NOT EXISTS inserted_at TIMESTAMP WITH TIME ZONE`); err != nil {
				return fmt.Errorf("v14 migration (add inserted_at): %w", err)
			}
			if _, err = p.pool.Exec(ctx, `UPDATE responses SET inserted_at = created_at WHERE inserted_at IS NULL`); err != nil {
				return fmt.Errorf("v14 migration (backfill inserted_at): %w", err)
			}
			if _, err = p.pool.Exec(ctx, `ALTER TABLE responses ALTER COLUMN inserted_at SET DEFAULT clock_timestamp()`); err != nil {
				return fmt.Errorf("v14 migration (set inserted_at default): %w", err)
			}
			if _, err = p.pool.Exec(ctx, `ALTER TABLE responses ALTER COLUMN inserted_at SET NOT NULL`); err != nil {
				return fmt.Errorf("v14 migration (set inserted_at not null): %w", err)
			}
			if _, err = p.pool.Exec(ctx, `CREATE INDEX IF NOT EXISTS idx_responses_inserted ON responses(inserted_at)`); err != nil {
				return fmt.Errorf("v14 migration (add inserted_at index): %w", err)
			}
		}
		if currentVersion < 15 {
			// Panel columns + job-level failover override (backup_agent,
			// backup_model): the branch's schema work as a single migration
			// on top of main's v14 (inserted_at).
			for _, stmt := range []string{
				`ALTER TABLE review_jobs ADD COLUMN IF NOT EXISTS panel_run_uuid TEXT`,
				`ALTER TABLE review_jobs ADD COLUMN IF NOT EXISTS panel_role TEXT`,
				`ALTER TABLE review_jobs ADD COLUMN IF NOT EXISTS panel_name TEXT`,
				`ALTER TABLE review_jobs ADD COLUMN IF NOT EXISTS panel_member_name TEXT`,
				`ALTER TABLE review_jobs ADD COLUMN IF NOT EXISTS panel_member_index INTEGER`,
				`ALTER TABLE review_jobs ADD COLUMN IF NOT EXISTS panel_member_config_json TEXT`,
				`CREATE INDEX IF NOT EXISTS idx_review_jobs_panel ON review_jobs(panel_run_uuid, panel_role, panel_member_index)`,
				`ALTER TABLE review_jobs ADD COLUMN IF NOT EXISTS backup_agent TEXT NOT NULL DEFAULT ''`,
				`ALTER TABLE review_jobs ADD COLUMN IF NOT EXISTS backup_model TEXT NOT NULL DEFAULT ''`,
			} {
				if _, err = p.pool.Exec(ctx, stmt); err != nil {
					return fmt.Errorf("v15 migration (panel + backup columns): %w", err)
				}
			}
		}
		if currentVersion < 16 {
			if _, err = p.pool.Exec(ctx, `ALTER TABLE review_jobs ADD COLUMN IF NOT EXISTS dirty_files TEXT`); err != nil {
				return fmt.Errorf("v16 migration (add dirty_files): %w", err)
			}
		}
		if currentVersion < 17 {
			// agent_invoked: authoritative, synced "an agent ran" signal for cost
			// eligibility. Rows that predate the column keep the default FALSE and
			// are not backfilled — a historical run that recorded token usage is
			// still counted via the token_usage fallback in costEligible.
			if _, err = p.pool.Exec(ctx, `ALTER TABLE review_jobs ADD COLUMN IF NOT EXISTS agent_invoked BOOLEAN NOT NULL DEFAULT FALSE`); err != nil {
				return fmt.Errorf("v17 migration (add agent_invoked): %w", err)
			}
		}
		// Update version
		_, err = p.pool.Exec(ctx, `INSERT INTO schema_version (version) VALUES ($1) ON CONFLICT (version) DO NOTHING`, pgSchemaVersion)
		if err != nil {
			return fmt.Errorf("update schema version: %w", err)
		}
	}

	if _, err := p.pool.Exec(ctx, `CREATE INDEX IF NOT EXISTS idx_responses_inserted ON responses(inserted_at)`); err != nil {
		return fmt.Errorf("ensure response inserted_at index: %w", err)
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
	err := p.bun.NewSelect().Model((*pgSyncMetadataRow)(nil)).Column("value").
		Where("key = ?", "database_id").Scan(ctx, &id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("query database_id: %w", err)
	}

	// Generate new ID - use ON CONFLICT to handle concurrent creation
	newID := GenerateUUID()
	row := pgSyncMetadataRow{Key: "database_id", Value: newID}
	_, err = p.bun.NewInsert().Model(&row).Column("key", "value").
		On("CONFLICT (key) DO NOTHING").Exec(ctx)
	if err != nil {
		return "", fmt.Errorf("insert database_id: %w", err)
	}

	// Re-read in case another process inserted first
	err = p.bun.NewSelect().Model((*pgSyncMetadataRow)(nil)).Column("value").
		Where("key = ?", "database_id").Scan(ctx, &id)
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
	row := pgMachineRow{MachineID: machineID, Name: name}
	_, err := p.bun.NewInsert().Model(&row).Column("machine_id", "name").
		Value("last_seen_at", "NOW()").
		On("CONFLICT (machine_id) DO UPDATE").
		Set("name = COALESCE(EXCLUDED.name, m.name)").
		Set("last_seen_at = NOW()").Exec(ctx)
	if err != nil {
		return fmt.Errorf("register machine: %w", err)
	}
	return nil
}

// GetOrCreateRepo finds or creates a repo by identity, returns the PostgreSQL ID
func (p *PgPool) GetOrCreateRepo(ctx context.Context, identity string) (int64, error) {
	row := repoRow{Identity: &identity}
	err := p.bun.NewInsert().Model(&row).Column("identity").
		On("CONFLICT (identity) DO UPDATE").Set("identity = EXCLUDED.identity").
		Returning("id").Scan(ctx)
	if err != nil {
		return 0, fmt.Errorf("get or create repo: %w", err)
	}
	return row.ID, nil
}

// GetOrCreateCommit finds or creates a commit, returns the PostgreSQL ID
func (p *PgPool) GetOrCreateCommit(ctx context.Context, repoID int64, sha, author, subject string, timestamp time.Time) (int64, error) {
	row := commitRow{
		RepoID: repoID, SHA: sha, Author: author, Subject: subject,
		Timestamp: dbTimeFromValue(timestamp),
	}
	err := p.bun.NewInsert().Model(&row).
		Column("repo_id", "sha", "author", "subject", "timestamp").
		On("CONFLICT (repo_id, sha) DO UPDATE").Set("sha = EXCLUDED.sha").
		Returning("id").Scan(ctx)
	if err != nil {
		return 0, fmt.Errorf("get or create commit: %w", err)
	}
	return row.ID, nil
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
	query, err := p.newJobUpsert(j, pgRepoID, pgCommitID)
	if err != nil {
		return err
	}
	_, err = query.Exec(ctx)
	return err
}

func (p *PgPool) newJobUpsert(j SyncableJob, pgRepoID int64, pgCommitID *int64) (*bun.InsertQuery, error) {
	dirtyFilesJSON, err := encodeDirtyFiles(j.DirtyFiles)
	if err != nil {
		return nil, err
	}
	panelMemberIndex := j.PanelMemberIndex
	row := jobRow{
		UUID: optionalString(j.UUID), RepoID: pgRepoID, CommitID: pgCommitID, GitRef: j.GitRef,
		SessionID: optionalString(j.SessionID), Agent: j.Agent, Model: optionalString(j.Model),
		Provider: optionalString(j.Provider), RequestedModel: optionalString(j.RequestedModel),
		RequestedProvider: optionalString(j.RequestedProvider), Reasoning: optionalString(j.Reasoning),
		JobType: defaultStr(j.JobType, "review"), ReviewType: j.ReviewType, PatchID: optionalString(j.PatchID),
		Status: JobStatus(j.Status), Agentic: j.Agentic, AgentInvoked: j.AgentInvoked,
		EnqueuedAt: dbTimeFromValue(j.EnqueuedAt), StartedAt: dbTimeFromPointer(j.StartedAt),
		FinishedAt: dbTimeFromPointer(j.FinishedAt), Prompt: optionalString(sanitizePostgresText(j.Prompt)),
		DiffContent: sanitizePostgresTextPointer(j.DiffContent), DirtyFiles: optionalString(dirtyFilesJSON),
		Error: optionalString(sanitizePostgresText(j.Error)), TokenUsage: optionalString(j.TokenUsage),
		WorktreePath: optionalString(j.WorktreePath), Source: optionalString(j.Source),
		MinSeverity: normalizeMinSeverityForWrite(j.MinSeverity), BackupAgent: j.BackupAgent, BackupModel: j.BackupModel,
		PanelRunUUID: optionalString(j.PanelRunUUID), PanelRole: optionalString(j.PanelRole),
		PanelName: optionalString(j.PanelName), PanelMemberName: optionalString(j.PanelMemberName),
		PanelMemberIndex: &panelMemberIndex, PanelMemberConfigJSON: optionalString(j.PanelMemberConfigJSON),
		SourceMachineID: optionalString(j.SourceMachineID),
	}
	query := p.bun.NewInsert().Model(&row).
		Column("uuid", "repo_id", "commit_id", "git_ref", "session_id", "agent", "model", "provider",
			"requested_model", "requested_provider", "reasoning", "job_type", "review_type", "patch_id",
			"status", "agentic", "enqueued_at", "started_at", "finished_at", "prompt", "diff_content",
			"dirty_files", "error", "token_usage", "worktree_path", "source", "min_severity", "panel_run_uuid",
			"panel_role", "panel_name", "panel_member_name", "panel_member_index", "panel_member_config_json",
			"source_machine_id", "backup_agent", "backup_model", "agent_invoked").
		Value("updated_at", "clock_timestamp()").
		On("CONFLICT (uuid) DO UPDATE").
		Set("status = EXCLUDED.status").Set("finished_at = EXCLUDED.finished_at").
		Set("error = EXCLUDED.error").Set("model = EXCLUDED.model").Set("provider = EXCLUDED.provider").
		Set("requested_model = EXCLUDED.requested_model").Set("requested_provider = EXCLUDED.requested_provider").
		Set("git_ref = EXCLUDED.git_ref").
		Set("session_id = CASE WHEN EXCLUDED.status IN ('done', 'failed', 'canceled', 'skipped', 'applied', 'rebased') THEN EXCLUDED.session_id ELSE COALESCE(EXCLUDED.session_id, j.session_id) END").
		Set("commit_id = EXCLUDED.commit_id").Set("patch_id = EXCLUDED.patch_id").
		Set("dirty_files = COALESCE(EXCLUDED.dirty_files, j.dirty_files)").
		Set("token_usage = CASE WHEN EXCLUDED.status IN ('done', 'failed', 'canceled', 'skipped', 'applied', 'rebased') THEN EXCLUDED.token_usage ELSE COALESCE(EXCLUDED.token_usage, j.token_usage) END").
		Set("agent_invoked = CASE WHEN EXCLUDED.status IN ('done', 'failed', 'canceled', 'skipped', 'applied', 'rebased') THEN EXCLUDED.agent_invoked ELSE (j.agent_invoked OR EXCLUDED.agent_invoked) END").
		Set("worktree_path = COALESCE(EXCLUDED.worktree_path, j.worktree_path)").
		Set("source = COALESCE(EXCLUDED.source, j.source)").
		Set("min_severity = EXCLUDED.min_severity").Set("backup_agent = EXCLUDED.backup_agent").
		Set("backup_model = EXCLUDED.backup_model").Set("panel_run_uuid = EXCLUDED.panel_run_uuid").
		Set("panel_role = EXCLUDED.panel_role").Set("panel_name = EXCLUDED.panel_name").
		Set("panel_member_name = EXCLUDED.panel_member_name").
		Set("panel_member_index = EXCLUDED.panel_member_index").
		Set("panel_member_config_json = EXCLUDED.panel_member_config_json").
		Set("updated_at = clock_timestamp()")
	return query, nil
}

// UpsertReview inserts or updates a review in PostgreSQL
func (p *PgPool) UpsertReview(ctx context.Context, r SyncableReview) error {
	_, err := p.newReviewUpsert(r).Exec(ctx)
	return err
}

func (p *PgPool) newReviewUpsert(r SyncableReview) *bun.InsertQuery {
	row := reviewRow{
		UUID: &r.UUID, JobUUID: &r.JobUUID, Agent: r.Agent, Prompt: r.Prompt,
		Output: r.Output, Closed: r.Closed, UpdatedByMachineID: &r.UpdatedByMachineID,
		CreatedAt: dbTimeFromValue(r.CreatedAt),
	}
	return p.bun.NewInsert().Model(&row).
		Column("uuid", "job_uuid", "agent", "prompt", "output", "closed", "updated_by_machine_id", "created_at").
		Value("updated_at", "clock_timestamp()").
		On("CONFLICT (uuid) DO UPDATE").
		Set("closed = EXCLUDED.closed").
		Set("updated_by_machine_id = EXCLUDED.updated_by_machine_id").
		Set("updated_at = clock_timestamp()")
}

// InsertResponse inserts a response in PostgreSQL (append-only, no updates)
func (p *PgPool) InsertResponse(ctx context.Context, r SyncableResponse) error {
	_, err := p.newResponseInsert(r).Exec(ctx)
	return err
}

func (p *PgPool) newResponseInsert(r SyncableResponse) *bun.InsertQuery {
	row := responseRow{
		UUID: &r.UUID, JobUUID: &r.JobUUID, Responder: r.Responder, Response: r.Response,
		SourceMachineID: &r.SourceMachineID, CreatedAt: dbTimeFromValue(r.CreatedAt),
	}
	return p.bun.NewInsert().Model(&row).
		Column("uuid", "job_uuid", "responder", "response", "source_machine_id", "created_at").
		On("CONFLICT (uuid) DO NOTHING")
}

// PulledJob represents a job pulled from PostgreSQL
type PulledJob struct {
	UUID                  string
	RepoIdentity          string
	CommitSHA             string
	CommitAuthor          string
	CommitSubject         string
	CommitTimestamp       time.Time
	GitRef                string
	SessionID             string
	Agent                 string
	Model                 string
	Provider              string
	RequestedModel        string
	RequestedProvider     string
	Reasoning             string
	JobType               string
	ReviewType            string
	PatchID               string
	Status                string
	Agentic               bool
	AgentInvoked          bool
	EnqueuedAt            time.Time
	StartedAt             *time.Time
	FinishedAt            *time.Time
	Prompt                string
	DiffContent           *string
	DirtyFiles            []string
	Error                 string
	TokenUsage            string
	WorktreePath          string
	Source                string
	MinSeverity           string
	BackupAgent           string
	BackupModel           string
	PanelRunUUID          string
	PanelRole             string
	PanelName             string
	PanelMemberName       string
	PanelMemberIndex      int
	PanelMemberConfigJSON string
	SourceMachineID       string
	UpdatedAt             time.Time
}

type pulledJobRow struct {
	UUID                  string  `bun:"uuid"`
	RepoIdentity          string  `bun:"repo_identity"`
	CommitSHA             string  `bun:"commit_sha"`
	CommitAuthor          string  `bun:"commit_author"`
	CommitSubject         string  `bun:"commit_subject"`
	CommitTimestamp       dbTime  `bun:"commit_timestamp"`
	GitRef                string  `bun:"git_ref"`
	SessionID             string  `bun:"session_id"`
	Agent                 string  `bun:"agent"`
	Model                 string  `bun:"model"`
	Provider              string  `bun:"provider"`
	RequestedModel        string  `bun:"requested_model"`
	RequestedProvider     string  `bun:"requested_provider"`
	Reasoning             string  `bun:"reasoning"`
	JobType               string  `bun:"job_type"`
	ReviewType            string  `bun:"review_type"`
	PatchID               string  `bun:"patch_id"`
	Status                string  `bun:"status"`
	Agentic               bool    `bun:"agentic"`
	AgentInvoked          bool    `bun:"agent_invoked"`
	EnqueuedAt            dbTime  `bun:"enqueued_at"`
	StartedAt             dbTime  `bun:"started_at"`
	FinishedAt            dbTime  `bun:"finished_at"`
	Prompt                string  `bun:"prompt"`
	DiffContent           *string `bun:"diff_content"`
	DirtyFiles            *string `bun:"dirty_files"`
	Error                 string  `bun:"error"`
	TokenUsage            string  `bun:"token_usage"`
	WorktreePath          string  `bun:"worktree_path"`
	Source                string  `bun:"source"`
	MinSeverity           string  `bun:"min_severity"`
	BackupAgent           string  `bun:"backup_agent"`
	BackupModel           string  `bun:"backup_model"`
	PanelRunUUID          string  `bun:"panel_run_uuid"`
	PanelRole             string  `bun:"panel_role"`
	PanelName             string  `bun:"panel_name"`
	PanelMemberName       string  `bun:"panel_member_name"`
	PanelMemberIndex      int     `bun:"panel_member_index"`
	PanelMemberConfigJSON string  `bun:"panel_member_config_json"`
	SourceMachineID       string  `bun:"source_machine_id"`
	UpdatedAt             dbTime  `bun:"updated_at"`
	CursorID              int64   `bun:"cursor_id"`
}

func (row pulledJobRow) toModel() PulledJob {
	job := PulledJob{
		UUID: row.UUID, RepoIdentity: row.RepoIdentity,
		CommitSHA: row.CommitSHA, CommitAuthor: row.CommitAuthor,
		CommitSubject: row.CommitSubject, CommitTimestamp: row.CommitTimestamp.Time,
		GitRef: row.GitRef, SessionID: row.SessionID, Agent: row.Agent,
		Model: row.Model, Provider: row.Provider, RequestedModel: row.RequestedModel,
		RequestedProvider: row.RequestedProvider, Reasoning: row.Reasoning,
		JobType: row.JobType, ReviewType: row.ReviewType, PatchID: row.PatchID,
		Status: row.Status, Agentic: row.Agentic, AgentInvoked: row.AgentInvoked,
		EnqueuedAt: row.EnqueuedAt.Time, Prompt: row.Prompt,
		DiffContent: cloneStringPointer(row.DiffContent), Error: row.Error,
		TokenUsage: row.TokenUsage, WorktreePath: row.WorktreePath, Source: row.Source,
		MinSeverity: row.MinSeverity, BackupAgent: row.BackupAgent, BackupModel: row.BackupModel,
		PanelRunUUID: row.PanelRunUUID, PanelRole: row.PanelRole, PanelName: row.PanelName,
		PanelMemberName: row.PanelMemberName, PanelMemberIndex: row.PanelMemberIndex,
		PanelMemberConfigJSON: row.PanelMemberConfigJSON,
		SourceMachineID:       row.SourceMachineID, UpdatedAt: row.UpdatedAt.Time,
	}
	job.StartedAt = row.StartedAt.pointer()
	job.FinishedAt = row.FinishedAt.pointer()
	if row.DirtyFiles != nil {
		job.DirtyFiles = decodeDirtyFiles(*row.DirtyFiles)
	}
	return job
}

// PullJobs fetches jobs from PostgreSQL updated after the given cursor.
// Cursor format: "updated_at id" (space-separated) or empty for first pull.
// Returns jobs not from the given machineID (to avoid echo).
func (p *PgPool) PullJobs(ctx context.Context, excludeMachineID string, cursor string, limit int) ([]PulledJob, string, error) {
	if limit <= 0 {
		return nil, cursor, nil
	}
	var cursorTime time.Time
	var cursorID int64

	if cursor != "" {
		var ts string
		_, err := fmt.Sscanf(cursor, "%s %d", &ts, &cursorID)
		if err == nil {
			cursorTime, _ = time.Parse(time.RFC3339Nano, ts)
		}
	}

	var rows []pulledJobRow
	err := p.bun.NewSelect().TableExpr("review_jobs AS j").
		ColumnExpr("j.uuid AS uuid").ColumnExpr("r.identity AS repo_identity").
		ColumnExpr("COALESCE(c.sha, '') AS commit_sha").
		ColumnExpr("COALESCE(c.author, '') AS commit_author").
		ColumnExpr("COALESCE(c.subject, '') AS commit_subject").
		ColumnExpr("COALESCE(c.timestamp, '1970-01-01'::timestamptz) AS commit_timestamp").
		ColumnExpr("j.git_ref AS git_ref").ColumnExpr("COALESCE(j.session_id, '') AS session_id").
		ColumnExpr("j.agent AS agent").ColumnExpr("COALESCE(j.model, '') AS model").
		ColumnExpr("COALESCE(j.provider, '') AS provider").
		ColumnExpr("COALESCE(j.requested_model, '') AS requested_model").
		ColumnExpr("COALESCE(j.requested_provider, '') AS requested_provider").
		ColumnExpr("COALESCE(j.reasoning, '') AS reasoning").
		ColumnExpr("COALESCE(j.job_type, 'review') AS job_type").
		ColumnExpr("COALESCE(j.review_type, '') AS review_type").
		ColumnExpr("COALESCE(j.patch_id, '') AS patch_id").
		ColumnExpr("j.status AS status").ColumnExpr("j.agentic AS agentic").
		ColumnExpr("COALESCE(j.agent_invoked, FALSE) AS agent_invoked").
		ColumnExpr("j.enqueued_at AS enqueued_at").ColumnExpr("j.started_at AS started_at").
		ColumnExpr("j.finished_at AS finished_at").ColumnExpr("COALESCE(j.prompt, '') AS prompt").
		ColumnExpr("j.diff_content AS diff_content").ColumnExpr("j.dirty_files AS dirty_files").
		ColumnExpr("COALESCE(j.error, '') AS error").ColumnExpr("COALESCE(j.token_usage, '') AS token_usage").
		ColumnExpr("COALESCE(j.worktree_path, '') AS worktree_path").
		ColumnExpr("COALESCE(j.source, '') AS source").
		ColumnExpr("COALESCE(j.min_severity, '') AS min_severity").
		ColumnExpr("COALESCE(j.backup_agent, '') AS backup_agent").
		ColumnExpr("COALESCE(j.backup_model, '') AS backup_model").
		ColumnExpr("COALESCE(j.panel_run_uuid, '') AS panel_run_uuid").
		ColumnExpr("COALESCE(j.panel_role, '') AS panel_role").
		ColumnExpr("COALESCE(j.panel_name, '') AS panel_name").
		ColumnExpr("COALESCE(j.panel_member_name, '') AS panel_member_name").
		ColumnExpr("COALESCE(j.panel_member_index, 0) AS panel_member_index").
		ColumnExpr("COALESCE(j.panel_member_config_json, '') AS panel_member_config_json").
		ColumnExpr("j.source_machine_id AS source_machine_id").
		ColumnExpr("j.updated_at AS updated_at").ColumnExpr("j.id AS cursor_id").
		Join("JOIN repos AS r ON j.repo_id = r.id").
		Join("LEFT JOIN commits AS c ON j.commit_id = c.id").
		Where("j.source_machine_id IS NULL OR j.source_machine_id != ?", excludeMachineID).
		Where("j.updated_at > ? OR (j.updated_at = ? AND j.id > ?)", cursorTime, cursorTime, cursorID).
		OrderExpr("j.updated_at, j.id").Limit(limit).Scan(ctx, &rows)
	if err != nil {
		return nil, cursor, fmt.Errorf("query jobs: %w", err)
	}

	var jobs []PulledJob
	for _, row := range rows {
		jobs = append(jobs, row.toModel())
	}

	newCursor := cursor
	if len(rows) > 0 {
		last := rows[len(rows)-1]
		newCursor = fmt.Sprintf("%s %d", last.UpdatedAt.Time.Format(time.RFC3339Nano), last.CursorID)
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

type pulledReviewRow struct {
	UUID               string `bun:"uuid"`
	JobUUID            string `bun:"job_uuid"`
	Agent              string `bun:"agent"`
	Prompt             string `bun:"prompt"`
	Output             string `bun:"output"`
	Closed             bool   `bun:"closed"`
	UpdatedByMachineID string `bun:"updated_by_machine_id"`
	CreatedAt          dbTime `bun:"created_at"`
	UpdatedAt          dbTime `bun:"updated_at"`
	CursorID           int64  `bun:"cursor_id"`
}

// PullReviews fetches reviews from PostgreSQL updated after the given cursor.
// Only fetches reviews for jobs in knownJobUUIDs to avoid cursor advancement past unknown jobs.
func (p *PgPool) PullReviews(ctx context.Context, excludeMachineID string, knownJobUUIDs []string, cursor string, limit int) ([]PulledReview, string, error) {
	if limit <= 0 {
		return nil, cursor, nil
	}
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

	var rows []pulledReviewRow
	err := p.bun.NewSelect().TableExpr("reviews AS r").
		ColumnExpr("r.uuid AS uuid").ColumnExpr("r.job_uuid AS job_uuid").
		ColumnExpr("r.agent AS agent").ColumnExpr("r.prompt AS prompt").
		ColumnExpr("r.output AS output").ColumnExpr("r.closed AS closed").
		ColumnExpr("r.updated_by_machine_id AS updated_by_machine_id").
		ColumnExpr("r.created_at AS created_at").ColumnExpr("r.updated_at AS updated_at").
		ColumnExpr("r.id AS cursor_id").
		Where("r.updated_by_machine_id IS NULL OR r.updated_by_machine_id != ?", excludeMachineID).
		Where("r.job_uuid IN (?)", bun.List(knownJobUUIDs)).
		Where("r.updated_at > ? OR (r.updated_at = ? AND r.id > ?)", cursorTime, cursorTime, cursorID).
		OrderExpr("r.updated_at, r.id").Limit(limit).Scan(ctx, &rows)
	if err != nil {
		return nil, cursor, fmt.Errorf("query reviews: %w", err)
	}

	var reviews []PulledReview
	for _, row := range rows {
		reviews = append(reviews, PulledReview{
			UUID: row.UUID, JobUUID: row.JobUUID, Agent: row.Agent, Prompt: row.Prompt,
			Output: row.Output, Closed: row.Closed, UpdatedByMachineID: row.UpdatedByMachineID,
			CreatedAt: row.CreatedAt.Time, UpdatedAt: row.UpdatedAt.Time,
		})
	}

	newCursor := cursor
	if len(rows) > 0 {
		last := rows[len(rows)-1]
		newCursor = fmt.Sprintf("%s %d", last.UpdatedAt.Time.Format(time.RFC3339Nano), last.CursorID)
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
	InsertedAt      time.Time
}

type pulledResponseRow struct {
	UUID            string `bun:"uuid"`
	JobUUID         string `bun:"job_uuid"`
	Responder       string `bun:"responder"`
	Response        string `bun:"response"`
	SourceMachineID string `bun:"source_machine_id"`
	CreatedAt       dbTime `bun:"created_at"`
	InsertedAt      dbTime `bun:"inserted_at"`
	CursorID        int64  `bun:"cursor_id"`
}

// PullResponses fetches responses from PostgreSQL inserted after the given cursor.
// Cursor format: "inserted_at id" (space-separated) or empty for first pull.
func (p *PgPool) PullResponses(ctx context.Context, excludeMachineID string, cursor string, limit int) ([]PulledResponse, string, error) {
	if limit <= 0 {
		return nil, cursor, nil
	}
	var cursorTime time.Time
	var cursorID int64
	if cursor != "" {
		var ok bool
		cursorTime, cursorID, ok = parseTimestampIDCursor(cursor)
		if !ok {
			cursor = ""
		}
	}

	var rows []pulledResponseRow
	err := p.bun.NewSelect().TableExpr("responses AS r").
		ColumnExpr("r.uuid AS uuid").ColumnExpr("r.job_uuid AS job_uuid").
		ColumnExpr("r.responder AS responder").ColumnExpr("r.response AS response").
		ColumnExpr("r.source_machine_id AS source_machine_id").
		ColumnExpr("r.created_at AS created_at").ColumnExpr("r.inserted_at AS inserted_at").
		ColumnExpr("r.id AS cursor_id").
		Where("r.source_machine_id IS NULL OR r.source_machine_id != ?", excludeMachineID).
		Where("r.inserted_at > ? OR (r.inserted_at = ? AND r.id > ?)", cursorTime, cursorTime, cursorID).
		OrderExpr("r.inserted_at, r.id").Limit(limit).Scan(ctx, &rows)
	if err != nil {
		return nil, cursor, fmt.Errorf("query responses: %w", err)
	}

	var responses []PulledResponse
	for _, row := range rows {
		responses = append(responses, PulledResponse{
			UUID: row.UUID, JobUUID: row.JobUUID, Responder: row.Responder,
			Response: row.Response, SourceMachineID: row.SourceMachineID,
			CreatedAt: row.CreatedAt.Time, InsertedAt: row.InsertedAt.Time,
		})
	}

	newCursor := cursor
	if len(rows) > 0 {
		last := rows[len(rows)-1]
		newCursor = formatTimestampIDCursor(last.InsertedAt.Time, last.CursorID)
	}

	return responses, newCursor, nil
}

// nullString returns nil if s is empty, otherwise returns s
func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func sanitizePostgresText(s string) string {
	return strings.ReplaceAll(strings.ToValidUTF8(s, "\uFFFD"), "\x00", "\uFFFD")
}

func sanitizePostgresTextPointer(s *string) *string {
	if s == nil {
		return nil
	}
	sanitized := sanitizePostgresText(*s)
	return &sanitized
}

// defaultStr returns s if non-empty, otherwise returns the default.
// Used for NOT NULL columns that should never be nil.
func defaultStr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func (p *PgPool) queueBunQuery(batch *pgx.Batch, query bunschema.QueryAppender) error {
	queryBytes, err := query.AppendQuery(bunschema.NewQueryGen(p.bun.Dialect()), nil)
	if err != nil {
		return err
	}
	batch.Queue(string(queryBytes))
	return nil
}

// BatchUpsertReviews inserts or updates multiple reviews in a single batch operation.
// Returns a boolean slice indicating success/failure for each item at the corresponding index.
func (p *PgPool) BatchUpsertReviews(ctx context.Context, reviews []SyncableReview) ([]bool, error) {
	if len(reviews) == 0 {
		return nil, nil
	}

	batch := &pgx.Batch{}
	for _, r := range reviews {
		if err := p.queueBunQuery(batch, p.newReviewUpsert(r)); err != nil {
			return nil, fmt.Errorf("build review upsert: %w", err)
		}
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
		if err := p.queueBunQuery(batch, p.newResponseInsert(r)); err != nil {
			return nil, fmt.Errorf("build response insert: %w", err)
		}
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

	success, err := p.batchUpsertJobs(ctx, jobs)
	if err == nil || len(jobs) == 1 {
		return success, err
	}
	return p.upsertJobsIndividually(ctx, jobs)
}

func (p *PgPool) batchUpsertJobs(ctx context.Context, jobs []JobWithPgIDs) ([]bool, error) {
	batch := &pgx.Batch{}
	for _, jw := range jobs {
		if err := p.queueJobUpsert(batch, jw); err != nil {
			return nil, err
		}
	}

	br := p.pool.SendBatch(ctx, batch)

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
	if err := br.Close(); err != nil && firstErr == nil {
		firstErr = err
	}

	return success, firstErr
}

func (p *PgPool) upsertJobsIndividually(ctx context.Context, jobs []JobWithPgIDs) ([]bool, error) {
	success := make([]bool, len(jobs))
	var firstErr error
	for i, jw := range jobs {
		batch := &pgx.Batch{}
		if err := p.queueJobUpsert(batch, jw); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		br := p.pool.SendBatch(ctx, batch)
		_, execErr := br.Exec()
		closeErr := br.Close()
		if execErr != nil {
			if firstErr == nil {
				firstErr = execErr
			}
			continue
		}
		if closeErr != nil {
			if firstErr == nil {
				firstErr = closeErr
			}
			continue
		}
		success[i] = true
	}
	return success, firstErr
}

func (p *PgPool) queueJobUpsert(batch *pgx.Batch, jw JobWithPgIDs) error {
	query, err := p.newJobUpsert(jw.Job, jw.PgRepoID, jw.PgCommitID)
	if err != nil {
		return err
	}
	return p.queueBunQuery(batch, query)
}
