package storage

import (
	_ "embed"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

//go:embed schemas/postgres_v1.sql
var postgresV1Schema string

const defaultTestMachineID = "11111111-1111-1111-1111-111111111111"

func TestDefaultPgPoolConfig(t *testing.T) {
	cfg := DefaultPgPoolConfig()

	assert.Equal(t, 5*time.Second, cfg.ConnectTimeout)
	assert.EqualValues(t, 4, cfg.MaxConns)
	assert.EqualValues(t, 0, cfg.MinConns)
	assert.Equal(t, time.Hour, cfg.MaxConnLifetime)
	assert.Equal(t, 30*time.Minute, cfg.MaxConnIdleTime)
}

func TestPgSchemaStatementsContainsRequiredTables(t *testing.T) {
	requiredStatements := []string{
		"CREATE SCHEMA IF NOT EXISTS roborev",
		"CREATE TABLE IF NOT EXISTS roborev.schema_version",
		"CREATE TABLE IF NOT EXISTS roborev.machines",
		"CREATE TABLE IF NOT EXISTS roborev.repos",
		"CREATE TABLE IF NOT EXISTS roborev.commits",
		"CREATE TABLE IF NOT EXISTS roborev.review_jobs",
		"CREATE TABLE IF NOT EXISTS roborev.reviews",
		"CREATE TABLE IF NOT EXISTS roborev.responses",
	}

	// Join all statements to search across the actual executed schema
	allStatements := strings.Join(pgSchemaStatements(), "\n")

	for _, required := range requiredStatements {
		assert.Contains(t, allStatements, required)
	}
}

func TestPgSchemaStatementsContainsRequiredIndexes(t *testing.T) {
	requiredIndexes := []string{
		"idx_review_jobs_source",
		"idx_review_jobs_updated",
		"idx_reviews_job_uuid",
		"idx_reviews_updated",
		"idx_responses_job_uuid",
		"idx_responses_id",
	}

	// Join all statements to search across the actual executed schema
	allStatements := strings.Join(pgSchemaStatements(), "\n")

	for _, idx := range requiredIndexes {
		assert.Contains(t, allStatements, idx)
	}
}

func TestPgSchemaStatementsSplitsCleanly(t *testing.T) {
	assert := assert.New(t)
	stmts := pgSchemaStatements()

	// Exactly one statement should create the attempts table. A literal
	// semicolon in the preceding comment would fragment the comment and
	// either drop or duplicate this statement.
	attemptsCount := 0
	for _, stmt := range stmts {
		if strings.Contains(stmt, "CREATE TABLE IF NOT EXISTS roborev.ci_pr_review_attempts") {
			attemptsCount++
		}
	}
	assert.Equal(1, attemptsCount,
		"expected exactly one ci_pr_review_attempts CREATE TABLE statement")

	// No returned statement may be a leaked prose fragment. Every real
	// statement in this schema starts with an uppercase SQL keyword (CREATE,
	// ALTER, ...). A comment fragmented by an inline semicolon would leave a
	// chunk whose first non-comment line is lowercase prose (e.g. "state is
	// one of ..."). This catches semicolon-in-comment fragmentation for any
	// table, not just ci_pr_review_attempts.
	for _, stmt := range stmts {
		first := firstNonCommentLine(stmt)
		require.NotEmpty(t, first, "statement had no code line: %q", stmt)
		c := first[0]
		assert.False(c >= 'a' && c <= 'z',
			"statement starts with lowercase prose (leaked comment fragment): %q", stmt)
	}
}

// firstNonCommentLine returns the first line of stmt, trimmed, that is neither
// blank nor a SQL line comment (a line starting with "--").
func firstNonCommentLine(stmt string) string {
	for line := range strings.SplitSeq(stmt, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "--") {
			continue
		}
		return line
	}
	return ""
}

// Integration tests require a live PostgreSQL instance.
// Run with: TEST_POSTGRES_URL=postgres://... go test -run Integration

func TestIntegration_PullReviewsFiltersByKnownJobs(t *testing.T) {
	pool := openTestPgPool(t)
	ctx := t.Context()

	// Clean up test data - use valid UUIDs
	machineID := uuid.NewString()
	otherMachineID := uuid.NewString()
	jobUUID1 := uuid.NewString()
	jobUUID2 := uuid.NewString()
	reviewUUID1 := uuid.NewString()
	reviewUUID2 := uuid.NewString()

	var repoIDs []int64
	defer func() {
		cleanupTestData(t, pool, machineID, otherMachineID, repoIDs, []string{jobUUID1, jobUUID2})
	}()

	// Register both machines
	if err := pool.RegisterMachine(ctx, machineID, "test"); err != nil {
		require.NoError(t, err, "RegisterMachine failed: %v")
	}
	if err := pool.RegisterMachine(ctx, otherMachineID, "other"); err != nil {
		require.NoError(t, err, "RegisterMachine (other) failed: %v")
	}

	// Create a repo using the helper
	repoIdentity := "test-repo-" + time.Now().Format("20060102150405")
	repoID := createTestRepo(t, pool.Pool(), TestRepoOpts{Identity: repoIdentity})
	repoIDs = append(repoIDs, repoID)

	// Create a commit using the helper
	commitID := createTestCommit(t, pool.Pool(), TestCommitOpts{RepoID: repoID, SHA: "abc123456789"})

	// Create two jobs with different UUIDs using explicit timestamps
	createTestJob(t, pool.pool, TestJobOpts{
		UUID:            jobUUID1,
		RepoID:          repoID,
		CommitID:        commitID,
		SourceMachineID: machineID,
	})
	createTestJob(t, pool.pool, TestJobOpts{
		UUID:            jobUUID2,
		RepoID:          repoID,
		CommitID:        commitID,
		SourceMachineID: machineID,
	})

	baseTime := time.Now().Truncate(time.Millisecond)
	// Create reviews with explicit timestamps to ensure ordering
	createTestReview(t, pool.pool, TestReviewOpts{
		UUID:               reviewUUID1,
		JobUUID:            jobUUID1,
		UpdatedByMachineID: otherMachineID,
		CreatedAt:          baseTime,
		UpdatedAt:          baseTime,
	})

	createTestReview(t, pool.pool, TestReviewOpts{
		UUID:               reviewUUID2,
		JobUUID:            jobUUID2,
		UpdatedByMachineID: otherMachineID,
		CreatedAt:          baseTime.Add(100 * time.Millisecond), // Explicitly later
		UpdatedAt:          baseTime.Add(100 * time.Millisecond),
	})

	t.Run("empty knownJobUUIDs returns empty and preserves cursor", func(t *testing.T) {
		reviews, newCursor, err := pool.PullReviews(ctx, machineID, []string{}, "", 100)
		require.NoError(t, err, "PullReviews failed: %v")

		assert.Empty(t, reviews)
		assert.Empty(t, newCursor)
	})

	t.Run("filters to only known job UUIDs", func(t *testing.T) {
		// Only request reviews for job1
		reviews, _, err := pool.PullReviews(ctx, machineID, []string{jobUUID1}, "", 100)
		require.NoError(t, err, "PullReviews failed: %v")

		assert.Len(t, reviews, 1)
		assert.Equal(t, reviews[0].JobUUID, jobUUID1)
	})

	t.Run("cursor does not skip reviews for later-known jobs", func(t *testing.T) {
		// First pull with only job1 known - gets review1, advances cursor
		reviews1, cursor1, err := pool.PullReviews(ctx, machineID, []string{jobUUID1}, "", 100)
		require.NoError(t, err, "First PullReviews failed: %v")

		assert.Len(t, reviews1, 1)

		// Second pull with both jobs known - should still get review2
		// even though cursor advanced past review1's timestamp
		reviews2, _, err := pool.PullReviews(ctx, machineID, []string{jobUUID1, jobUUID2}, cursor1, 100)
		require.NoError(t, err, "Second PullReviews failed: %v")

		assert.Len(t, reviews2, 1)
		assert.Equal(t, reviews2[0].JobUUID, jobUUID2)
	})
}

func getTestPostgresURL(t *testing.T) string {
	t.Helper()
	connString := ""
	// Check common env vars
	for _, envVar := range []string{"TEST_POSTGRES_URL", "POSTGRES_URL", "DATABASE_URL"} {
		if v := lookupEnv(envVar); v != "" {
			connString = v
			break
		}
	}
	if connString == "" {
		t.Skip("No PostgreSQL URL set (TEST_POSTGRES_URL, POSTGRES_URL, or DATABASE_URL)")
	}
	return connString
}

func lookupEnv(key string) string {
	return os.Getenv(key)
}

// openTestPgPool connects to the test Postgres, ensures schema, and registers cleanup.
func openTestPgPool(t *testing.T) *PgPool {
	t.Helper()
	connString := getTestPostgresURL(t)
	ctx := t.Context()
	pool, err := NewPgPool(ctx, connString, DefaultPgPoolConfig())
	require.NoError(t, err, "Failed to connect: %v")

	t.Cleanup(func() { pool.Close() })
	if err := pool.EnsureSchema(ctx); err != nil {
		require.NoError(t, err, "EnsureSchema failed: %v")
	}
	return pool
}

func cleanupTestData(t *testing.T, pool *PgPool, machineID, otherMachineID string, repoIDs []int64, jobUUIDs []string) {
	t.Helper()
	ctx := t.Context()
	// Clean up in reverse dependency order using tracked UUIDs
	// Delete responses and reviews by job_uuid since that's tracked
	for _, jobUUID := range jobUUIDs {
		pool.pool.Exec(ctx, `DELETE FROM responses WHERE job_uuid = $1`, jobUUID)
		pool.pool.Exec(ctx, `DELETE FROM reviews WHERE job_uuid = $1`, jobUUID)
	}
	pool.pool.Exec(ctx, `DELETE FROM review_jobs WHERE source_machine_id = $1`, machineID)
	pool.pool.Exec(ctx, `DELETE FROM commits WHERE repo_id = ANY($1)`, repoIDs)
	pool.pool.Exec(ctx, `DELETE FROM repos WHERE id = ANY($1)`, repoIDs)
	pool.pool.Exec(ctx, `DELETE FROM machines WHERE machine_id = $1`, machineID)
	pool.pool.Exec(ctx, `DELETE FROM machines WHERE machine_id = $1`, otherMachineID)
}

func TestIntegration_EnsureSchema_AutoInitializesVersion(t *testing.T) {
	// This test verifies that EnsureSchema auto-initializes when schema_version table is empty
	pool := openTestPgPool(t)
	ctx := t.Context()

	// Clear schema_version to simulate empty table
	_, _ = pool.pool.Exec(ctx, `DELETE FROM schema_version`)

	// EnsureSchema should succeed and auto-initialize
	if err := pool.EnsureSchema(ctx); err != nil {
		require.NoError(t, err, "EnsureSchema failed: %v")
	}

	// Verify version was inserted
	var version int
	if err := pool.pool.QueryRow(ctx, `SELECT MAX(version) FROM schema_version`).Scan(&version); err != nil {
		require.NoError(t, err, "Failed to query version: %v")
	}
	assert.Equal(t, pgSchemaVersion, version)
}

func TestIntegration_EnsureSchema_RejectsNewerVersion(t *testing.T) {
	// This test verifies that EnsureSchema returns error when schema version is newer than supported
	pool := openTestPgPool(t)
	ctx := t.Context()

	// Insert a newer version
	futureVersion := pgSchemaVersion + 10
	_, err := pool.pool.Exec(ctx, `INSERT INTO schema_version (version) VALUES ($1) ON CONFLICT (version) DO NOTHING`, futureVersion)
	require.NoError(t, err, "Failed to insert future version: %v")

	defer func() {
		// Clean up - remove future version
		pool.pool.Exec(ctx, `DELETE FROM schema_version WHERE version = $1`, futureVersion)
	}()

	// EnsureSchema should fail with clear error
	err = pool.EnsureSchema(ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "newer than supported")
}

func TestIntegration_EnsureSchema_FreshDatabase(t *testing.T) {
	// This test verifies that a fresh database (no roborev schema) can be initialized
	connString := getTestPostgresURL(t)
	ctx := t.Context()

	// First, check if schema exists
	env := NewMigrationTestEnv(t)
	var schemaExists bool
	if err := env.QueryRow(`SELECT EXISTS(SELECT 1 FROM information_schema.schemata WHERE schema_name = 'roborev')`).Scan(&schemaExists); err != nil {
		require.NoError(t, err, "Failed to check if schema exists: %v")
	}

	if schemaExists {
		// Don't actually drop if it has data - just verify the bootstrap works
		pool := openTestPgPool(t)

		// Verify schema_version table is accessible
		var version int
		err := pool.pool.QueryRow(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&version)
		require.NoError(t, err, "Failed to query schema_version: %v")

		t.Logf("Schema version: %d", version)

		// Verify branch index exists (created by migration or fresh install)
		var indexExists bool
		err = pool.pool.QueryRow(ctx, `
			SELECT EXISTS(
				SELECT 1 FROM pg_indexes
				WHERE schemaname = 'roborev' AND indexname = 'idx_review_jobs_branch'
			)
		`).Scan(&indexExists)
		require.NoError(t, err, "Failed to check branch index: %v")

		assert.True(t, indexExists, "Expected idx_review_jobs_branch to exist after EnsureSchema")
	} else {
		env.DropSchema("roborev")
		env.CleanupDropSchema("roborev")

		// Fresh database - NewPgPool should succeed with AfterConnect bootstrap
		pool, err := NewPgPool(ctx, connString, DefaultPgPoolConfig())
		require.NoError(t, err, "Failed to connect on fresh database: %v")

		t.Cleanup(func() { pool.Close() })

		if err := pool.EnsureSchema(ctx); err != nil {
			require.NoError(t, err, "EnsureSchema failed on fresh database: %v")
		}

		// Verify tables were created in roborev schema
		var tableCount int
		err = pool.pool.QueryRow(ctx, `
			SELECT COUNT(*) FROM information_schema.tables
			WHERE table_schema = 'roborev'
		`).Scan(&tableCount)
		require.NoError(t, err, "Failed to count tables: %v")

		assert.GreaterOrEqual(t, tableCount, 5)

		// Verify branch index was created for fresh install
		var indexExists bool
		err = pool.pool.QueryRow(ctx, `
			SELECT EXISTS(
				SELECT 1 FROM pg_indexes
				WHERE schemaname = 'roborev' AND indexname = 'idx_review_jobs_branch'
			)
		`).Scan(&indexExists)
		require.NoError(t, err, "Failed to check branch index: %v")

		assert.True(t, indexExists, "Expected idx_review_jobs_branch to exist on fresh install")
	}
}

func setupMigrationEnv(t *testing.T) *MigrationTestEnv {
	env := NewMigrationTestEnv(t)
	// Clean up after test
	env.CleanupDropSchema("roborev")
	env.CleanupDropTable("public", "schema_version")
	env.CleanupDropTable("public", "repos")

	env.DropTable("public", "repos")
	env.DropTable("public", "schema_version")
	env.DropSchema("roborev")
	return env
}

func countSuccesses(success []bool) int {
	count := 0
	for _, ok := range success {
		if ok {
			count++
		}
	}
	return count
}

func TestIntegration_EnsureSchema_MigratesLegacyTables(t *testing.T) {
	// This test verifies that tables in public schema are migrated to roborev
	ctx := t.Context()

	env := setupMigrationEnv(t)
	env.SkipIfTableInSchema("roborev", "schema_version")
	env.SkipIfTableInSchema("public", "schema_version")

	// Create legacy table in public schema
	env.Exec(`CREATE TABLE IF NOT EXISTS public.schema_version (version INTEGER PRIMARY KEY)`)
	env.Exec(`INSERT INTO public.schema_version (version) VALUES (1) ON CONFLICT DO NOTHING`)

	// Now connect with the normal pool and run EnsureSchema
	connString := getTestPostgresURL(t)
	pool, err := NewPgPool(ctx, connString, DefaultPgPoolConfig())
	require.NoError(t, err, "Failed to connect: %v")

	t.Cleanup(func() { pool.Close() })

	if err := pool.EnsureSchema(ctx); err != nil {
		require.NoError(t, err, "EnsureSchema failed: %v")
	}

	// Verify legacy table was migrated (no longer in public)
	var publicExists bool
	if err := pool.pool.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM information_schema.tables
			WHERE table_schema = 'public' AND table_name = 'schema_version'
		)
	`).Scan(&publicExists); err != nil {
		require.NoError(t, err, "Failed to check if public.schema_version exists: %v")
	}
	assert.False(t, publicExists)

	// Verify data is accessible in roborev schema
	var version int
	err = pool.pool.QueryRow(ctx, `SELECT version FROM schema_version`).Scan(&version)
	require.NoError(t, err, "Failed to query migrated data: %v")

	assert.Equal(t, 1, version)
}

func TestIntegration_EnsureSchema_MigratesMultipleTablesAndMixedState(t *testing.T) {
	// This test verifies migration with multiple tables in public and mixed state
	// (some tables already in roborev, some in public)
	ctx := t.Context()

	env := setupMigrationEnv(t)

	env.SkipIfTableInSchema("roborev", "schema_version")
	env.SkipIfTableInSchema("public", "schema_version")

	// Create roborev schema for mixed state test
	env.Exec(`CREATE SCHEMA IF NOT EXISTS roborev`)

	// Create legacy tables in public schema (simulating old installation)
	env.Exec(`CREATE TABLE IF NOT EXISTS public.schema_version (version INTEGER PRIMARY KEY)`)
	env.Exec(`INSERT INTO public.schema_version (version) VALUES (1) ON CONFLICT DO NOTHING`)

	// Create repos table in public (second legacy table)
	env.Exec(`CREATE TABLE IF NOT EXISTS public.repos (id SERIAL PRIMARY KEY, identity TEXT UNIQUE NOT NULL)`)
	env.Exec(`INSERT INTO public.repos (identity) VALUES ('test-repo-legacy') ON CONFLICT DO NOTHING`)

	// Create machines table directly in roborev (simulating partial migration)
	env.Exec(`CREATE TABLE IF NOT EXISTS roborev.machines (id SERIAL PRIMARY KEY, machine_id UUID UNIQUE NOT NULL, name TEXT)`)

	// Now connect with the normal pool and run EnsureSchema
	connString := getTestPostgresURL(t)
	pool, err := NewPgPool(ctx, connString, DefaultPgPoolConfig())
	require.NoError(t, err, "Failed to connect: %v")

	defer pool.Close()

	if err := pool.EnsureSchema(ctx); err != nil {
		require.NoError(t, err, "EnsureSchema failed: %v")
	}

	// Verify public tables gone
	var exists bool
	if err := pool.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM information_schema.tables WHERE table_schema='public' AND table_name='schema_version')`).Scan(&exists); err != nil {
		require.NoError(t, err, "Failed to check public.schema_version: %v")
	}
	assert.False(t, exists)
	if err := pool.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM information_schema.tables WHERE table_schema='public' AND table_name='repos')`).Scan(&exists); err != nil {
		require.NoError(t, err, "Failed to check public.repos: %v")
	}
	assert.False(t, exists)

	// Verify roborev.machines exists
	if err := pool.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM information_schema.tables WHERE table_schema='roborev' AND table_name='machines')`).Scan(&exists); err != nil {
		require.NoError(t, err, "Failed to check roborev.machines: %v")
	}
	assert.True(t, exists)

	// Verify data is accessible
	var version int
	err = pool.pool.QueryRow(ctx, `SELECT version FROM schema_version`).Scan(&version)
	require.NoError(t, err, "Failed to query migrated schema_version: %v")

	assert.Equal(t, 1, version)

	var repoIdentity string
	err = pool.pool.QueryRow(ctx, `SELECT identity FROM repos WHERE identity = 'test-repo-legacy'`).Scan(&repoIdentity)
	require.NoError(t, err, "Failed to query migrated repo: %v")

	assert.Equal(t, "test-repo-legacy", repoIdentity)
}

func TestIntegration_EnsureSchema_DualSchemaWithDataErrors(t *testing.T) {
	// This test verifies that having a table in both schemas with data in public
	// causes an error requiring manual reconciliation.
	ctx := t.Context()

	env := setupMigrationEnv(t)

	// Create roborev schema with repos table
	env.Exec(`CREATE SCHEMA IF NOT EXISTS roborev`)
	env.Exec(`CREATE TABLE roborev.repos (id SERIAL PRIMARY KEY, identity TEXT UNIQUE NOT NULL)`)

	// Create public.schema_version so migrateLegacyTables detects the legacy schema
	env.Exec(`CREATE TABLE public.schema_version (version INTEGER PRIMARY KEY)`)

	// Create public.repos table with data
	env.Exec(`CREATE TABLE public.repos (id SERIAL PRIMARY KEY, identity TEXT UNIQUE NOT NULL)`)
	env.Exec(`INSERT INTO public.repos (identity) VALUES ('legacy-repo')`)

	// Now connect and try EnsureSchema - should fail
	connString := getTestPostgresURL(t)
	pool, err := NewPgPool(ctx, connString, DefaultPgPoolConfig())
	require.NoError(t, err, "Failed to connect: %v")

	defer pool.Close()

	err = pool.EnsureSchema(ctx)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "manual reconciliation required")
}

func TestIntegration_EnsureSchema_EmptyPublicTableDropped(t *testing.T) {
	// This test verifies that an empty table in public is dropped when the same
	// table exists in roborev schema.
	ctx := t.Context()

	env := setupMigrationEnv(t)

	// Create roborev schema with repos table containing data
	env.Exec(`CREATE SCHEMA IF NOT EXISTS roborev`)
	env.Exec(`CREATE TABLE roborev.repos (id SERIAL PRIMARY KEY, identity TEXT UNIQUE NOT NULL)`)
	env.Exec(`INSERT INTO roborev.repos (identity) VALUES ('new-repo')`)

	// Create public.schema_version so migrateLegacyTables detects the legacy schema
	env.Exec(`CREATE TABLE public.schema_version (version INTEGER PRIMARY KEY)`)

	// Create empty public.repos table
	env.Exec(`CREATE TABLE public.repos (id SERIAL PRIMARY KEY, identity TEXT UNIQUE NOT NULL)`)
	// Note: no data inserted - empty table

	// Now connect and run EnsureSchema - should succeed and drop empty public.repos
	connString := getTestPostgresURL(t)
	pool, err := NewPgPool(ctx, connString, DefaultPgPoolConfig())
	require.NoError(t, err, "Failed to connect: %v")

	defer pool.Close()

	err = pool.EnsureSchema(ctx)
	require.NoError(t, err, "EnsureSchema failed: %v")

	var exists bool
	if err := pool.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM information_schema.tables WHERE table_schema='public' AND table_name='repos')`).Scan(&exists); err != nil {
		require.NoError(t, err, "Failed to check public.repos: %v")
	}
	assert.False(t, exists)

	// Verify roborev.repos still exists with data
	var repoIdentity string
	err = pool.pool.QueryRow(ctx, `SELECT identity FROM roborev.repos`).Scan(&repoIdentity)
	require.NoError(t, err, "Failed to query roborev.repos: %v")

	assert.Equal(t, "new-repo", repoIdentity)
}

func TestIntegration_EnsureSchema_MigratesPublicTableWithData(t *testing.T) {
	// This test verifies that a public table with data is properly migrated
	// to roborev schema when roborev doesn't have that table yet.
	// This is the normal migration path and also what the 42P01 fallback uses.
	ctx := t.Context()

	env := setupMigrationEnv(t)

	// Create roborev schema but NOT the repos table
	env.Exec(`CREATE SCHEMA IF NOT EXISTS roborev`)

	// Create public.schema_version so migrateLegacyTables detects the legacy schema
	env.Exec(`CREATE TABLE public.schema_version (version INTEGER PRIMARY KEY)`)

	// Create public.repos table with data
	env.Exec(`CREATE TABLE public.repos (id SERIAL PRIMARY KEY, identity TEXT UNIQUE NOT NULL)`)
	env.Exec(`INSERT INTO public.repos (identity) VALUES ('migrated-repo-1'), ('migrated-repo-2')`)

	// Now connect and run EnsureSchema - should migrate public.repos to roborev.repos
	connString := getTestPostgresURL(t)
	pool, err := NewPgPool(ctx, connString, DefaultPgPoolConfig())
	require.NoError(t, err, "Failed to connect: %v")

	defer pool.Close()

	err = pool.EnsureSchema(ctx)
	require.NoError(t, err, "EnsureSchema failed: %v")

	var exists bool
	if err := pool.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM information_schema.tables WHERE table_schema='public' AND table_name='repos')`).Scan(&exists); err != nil {
		require.NoError(t, err, "Failed to check public.repos: %v")
	}
	assert.False(t, exists)

	// Verify data is accessible in roborev.repos
	var count int
	err = pool.pool.QueryRow(ctx, `SELECT COUNT(*) FROM roborev.repos WHERE identity IN ('migrated-repo-1', 'migrated-repo-2')`).Scan(&count)
	require.NoError(t, err, "Failed to count migrated repos: %v")

	assert.Equal(t, 2, count)
}

func TestIntegration_GetDatabaseID_GeneratesAndPersists(t *testing.T) {
	pool := openTestPgPool(t)
	ctx := t.Context()

	// Get the database ID - should create one if it doesn't exist
	dbID1, err := pool.GetDatabaseID(ctx)
	require.NoError(t, err, "GetDatabaseID failed: %v")

	assert.NotEmpty(t, dbID1)

	// Get it again - should return the same ID
	dbID2, err := pool.GetDatabaseID(ctx)
	require.NoError(t, err, "GetDatabaseID (second call) failed: %v")

	assert.Equal(t, dbID2, dbID1)

	// Verify it's stored in sync_metadata
	var storedID string
	err = pool.pool.QueryRow(ctx, `SELECT value FROM sync_metadata WHERE key = 'database_id'`).Scan(&storedID)
	require.NoError(t, err, "Failed to query sync_metadata: %v")

	assert.Equal(t, storedID, dbID1)

	t.Logf("Database ID: %s", dbID1)
}

func TestIntegration_NewDatabaseClearsSyncedAt(t *testing.T) {
	// This test verifies that when connecting to a different Postgres database
	// (different database_id), the SQLite synced_at timestamps get cleared.
	pool := openTestPgPool(t)
	ctx := t.Context()

	// Create a test SQLite database
	sqliteDB, err := Open(t.TempDir() + "/test.db")
	require.NoError(t, err, "Failed to open SQLite: %v")

	defer sqliteDB.Close()

	// Create test data with synced_at already set
	repo, err := sqliteDB.GetOrCreateRepo(t.TempDir())
	require.NoError(t, err, "GetOrCreateRepo failed: %v")

	commit, err := sqliteDB.GetOrCreateCommit(repo.ID, "test-sha", "Author", "Subject", time.Now())
	require.NoError(t, err, "GetOrCreateCommit failed: %v")

	job, err := sqliteDB.EnqueueJob(EnqueueOpts{RepoID: repo.ID, CommitID: commit.ID, GitRef: "test-sha", Agent: "test", Reasoning: "thorough"})
	require.NoError(t, err, "EnqueueJob failed: %v")

	_, err = sqliteDB.ClaimJob("worker")
	require.NoError(t, err, "ClaimJob failed: %v")

	err = sqliteDB.CompleteJob(job.ID, "test", "prompt", "output")
	require.NoError(t, err, "CompleteJob failed: %v")

	// Mark everything as synced to simulate previous sync
	err = sqliteDB.MarkJobSynced(job.ID)
	require.NoError(t, err, "MarkJobSynced failed: %v")

	review, err := sqliteDB.GetReviewByJobID(job.ID)
	require.NoError(t, err, "GetReviewByJobID failed: %v")

	err = sqliteDB.MarkReviewSynced(review.ID)
	require.NoError(t, err, "MarkReviewSynced failed: %v")

	// Set a fake old sync target ID (simulating we synced to a different database before)
	oldTargetID := "old-database-" + uuid.NewString()
	err = sqliteDB.SetSyncState(SyncStateSyncTargetID, oldTargetID)
	require.NoError(t, err, "SetSyncState failed: %v")

	// Verify job is currently synced
	machineID, _ := sqliteDB.GetMachineID()
	jobsToSync, _ := sqliteDB.GetJobsToSync(machineID, 100)
	assert.Empty(t, jobsToSync)

	// Now get the database ID from the actual Postgres (which is different from oldTargetID)
	dbID, err := pool.GetDatabaseID(ctx)
	require.NoError(t, err, "GetDatabaseID failed: %v")

	// Simulate what connect() does: detect new database and clear synced_at
	lastTargetID, _ := sqliteDB.GetSyncState(SyncStateSyncTargetID)
	if lastTargetID != "" && lastTargetID != dbID {
		// This is what the sync worker does
		t.Logf("Detected new database (was %s..., now %s...), clearing synced_at", lastTargetID[:8], dbID[:8])
		err = sqliteDB.ClearAllSyncedAt()
		require.NoError(t, err, "ClearAllSyncedAt failed: %v")

	}
	err = sqliteDB.SetSyncState(SyncStateSyncTargetID, dbID)
	require.NoError(t, err, "SetSyncState (new target) failed: %v")

	// Now the job should be returned for sync again
	jobsToSync, err = sqliteDB.GetJobsToSync(machineID, 100)
	require.NoError(t, err, "GetJobsToSync failed: %v")

	assert.Len(t, jobsToSync, 1)

	// Verify sync target was updated
	newTargetID, _ := sqliteDB.GetSyncState(SyncStateSyncTargetID)
	assert.Equal(t, newTargetID, dbID)
}

func TestIntegration_BatchUpsertJobs(t *testing.T) {
	pool := openTestPgPool(t)
	ctx := t.Context()

	// Create a test repo
	repoID := createTestRepo(t, pool.Pool(), TestRepoOpts{Identity: "https://github.com/test/batch-jobs-test.git"})

	// Create multiple jobs with prepared IDs
	var jobs []JobWithPgIDs
	for i := range 5 {
		commitID := createTestCommit(t, pool.Pool(), TestCommitOpts{RepoID: repoID, SHA: fmt.Sprintf("batch-jobs-sha-%d", i)})
		jobs = append(jobs, JobWithPgIDs{
			Job: SyncableJob{
				UUID:            uuid.NewString(),
				RepoIdentity:    "https://github.com/test/batch-jobs-test.git",
				CommitSHA:       fmt.Sprintf("batch-jobs-sha-%d", i),
				GitRef:          "test-ref",
				Agent:           "test",
				Status:          "done",
				SourceMachineID: defaultTestMachineID,
				EnqueuedAt:      time.Now(),
			},
			PgRepoID:   repoID,
			PgCommitID: &commitID,
		})
	}

	success, err := pool.BatchUpsertJobs(ctx, jobs)
	require.NoError(t, err, "BatchUpsertJobs failed: %v")

	assert.Equal(t, 5, countSuccesses(success))

	// Verify jobs exist
	var jobCount int
	err = pool.pool.QueryRow(ctx, `SELECT COUNT(*) FROM review_jobs WHERE source_machine_id = $1`, defaultTestMachineID).Scan(&jobCount)
	require.NoError(t, err, "Count query failed: %v")

	assert.GreaterOrEqual(t, jobCount, 5)

	t.Run("empty batch is no-op", func(t *testing.T) {
		success, err := pool.BatchUpsertJobs(ctx, []JobWithPgIDs{})
		require.NoError(t, err)
		assert.Nil(t, success)
	})

	t.Run("worktree_path round-trips through batch upsert and pull", func(t *testing.T) {
		wtJobUUID := uuid.NewString()
		// Use a distinct machine ID so we can exclude it when pulling
		wtMachineID := uuid.NewString()
		commitID := createTestCommit(t, pool.Pool(), TestCommitOpts{
			RepoID: repoID, SHA: "batch-wt-sha",
		})
		wtJobs := []JobWithPgIDs{{
			Job: SyncableJob{
				UUID:            wtJobUUID,
				RepoIdentity:    "https://github.com/test/batch-jobs-test.git",
				CommitSHA:       "batch-wt-sha",
				GitRef:          "test-ref",
				Agent:           "test",
				Status:          "done",
				WorktreePath:    "/worktrees/feature-x",
				SourceMachineID: wtMachineID,
				EnqueuedAt:      time.Now(),
			},
			PgRepoID:   repoID,
			PgCommitID: &commitID,
		}}

		success, err := pool.BatchUpsertJobs(ctx, wtJobs)
		require.NoError(t, err)
		assert.Equal(t, 1, countSuccesses(success))

		// Verify worktree_path directly in the database row
		var storedWT *string
		err = pool.pool.QueryRow(ctx,
			`SELECT worktree_path FROM review_jobs WHERE uuid = $1`, wtJobUUID,
		).Scan(&storedWT)
		require.NoError(t, err)
		require.NotNil(t, storedWT)
		assert.Equal(t, "/worktrees/feature-x", *storedWT)

		// Also verify PullJobs returns the field. Scope the pull cursor to just before
		// this row so the test doesn't depend on table size or test shuffle order.
		var pulledCursor string
		var rowID int64
		var updatedAt time.Time
		err = pool.pool.QueryRow(ctx,
			`SELECT updated_at, id FROM review_jobs WHERE uuid = $1`, wtJobUUID,
		).Scan(&updatedAt, &rowID)
		require.NoError(t, err)
		pulledCursor = fmt.Sprintf("%s %d", updatedAt.Format(time.RFC3339Nano), rowID-1)

		otherMachine := uuid.NewString()
		pulled, _, err := pool.PullJobs(ctx, otherMachine, pulledCursor, 100)
		require.NoError(t, err)

		var found *PulledJob
		for i := range pulled {
			if pulled[i].UUID == wtJobUUID {
				found = &pulled[i]
				break
			}
		}
		require.NotNil(t, found, "expected job %s in pull results", wtJobUUID)
		assert.Equal(t, "/worktrees/feature-x", found.WorktreePath)
	})

	t.Run("model provider fields round-trip through batch upsert and pull", func(t *testing.T) {
		jobUUID := uuid.NewString()
		machineID := uuid.NewString()
		commitID := createTestCommit(t, pool.Pool(), TestCommitOpts{
			RepoID: repoID, SHA: "batch-model-provider-sha",
		})
		jobs := []JobWithPgIDs{{
			Job: SyncableJob{
				UUID:                  jobUUID,
				RepoIdentity:          "https://github.com/test/batch-jobs-test.git",
				CommitSHA:             "batch-model-provider-sha",
				GitRef:                "batch-model-provider-sha",
				Agent:                 "codex",
				Model:                 "gpt-5.5",
				Provider:              "openai",
				RequestedModel:        "requested-model",
				RequestedProvider:     "requested-provider",
				Status:                "done",
				JobType:               JobTypeReview,
				ReviewType:            "security",
				PanelRunUUID:          "run-model-provider",
				PanelRole:             PanelRoleMember,
				PanelMemberName:       "security",
				PanelMemberIndex:      0,
				PanelMemberConfigJSON: `{"name":"security"}`,
				SourceMachineID:       machineID,
				EnqueuedAt:            time.Now(),
			},
			PgRepoID:   repoID,
			PgCommitID: &commitID,
		}}

		success, err := pool.BatchUpsertJobs(ctx, jobs)
		require.NoError(t, err)
		assert.Equal(t, 1, countSuccesses(success))

		var model, provider, requestedModel, requestedProvider *string
		err = pool.pool.QueryRow(ctx,
			`SELECT model, provider, requested_model, requested_provider FROM review_jobs WHERE uuid = $1`, jobUUID,
		).Scan(&model, &provider, &requestedModel, &requestedProvider)
		require.NoError(t, err)
		require.NotNil(t, model)
		require.NotNil(t, provider)
		require.NotNil(t, requestedModel)
		require.NotNil(t, requestedProvider)
		assert.Equal(t, "gpt-5.5", *model)
		assert.Equal(t, "openai", *provider)
		assert.Equal(t, "requested-model", *requestedModel)
		assert.Equal(t, "requested-provider", *requestedProvider)

		var updatedAt time.Time
		var rowID int64
		err = pool.pool.QueryRow(ctx,
			`SELECT updated_at, id FROM review_jobs WHERE uuid = $1`, jobUUID,
		).Scan(&updatedAt, &rowID)
		require.NoError(t, err)
		cursor := fmt.Sprintf("%s %d", updatedAt.Format(time.RFC3339Nano), rowID-1)

		pulled, _, err := pool.PullJobs(ctx, uuid.NewString(), cursor, 100)
		require.NoError(t, err)
		var found *PulledJob
		for i := range pulled {
			if pulled[i].UUID == jobUUID {
				found = &pulled[i]
				break
			}
		}
		require.NotNil(t, found, "expected job %s in pull results", jobUUID)
		assert.Equal(t, "gpt-5.5", found.Model)
		assert.Equal(t, "openai", found.Provider)
		assert.Equal(t, "requested-model", found.RequestedModel)
		assert.Equal(t, "requested-provider", found.RequestedProvider)
	})
}

func TestIntegration_BatchUpsertReviews(t *testing.T) {
	pool := openTestPgPool(t)
	ctx := t.Context()

	repoID := createTestRepo(t, pool.Pool(), TestRepoOpts{Identity: "https://github.com/test/batch-reviews-test.git"})
	commitID := createTestCommit(t, pool.Pool(), TestCommitOpts{RepoID: repoID, SHA: "batch-reviews-sha"})
	jobUUID := uuid.NewString()

	createTestJob(t, pool.pool, TestJobOpts{
		UUID:            jobUUID,
		RepoID:          repoID,
		CommitID:        commitID,
		SourceMachineID: defaultTestMachineID,
	})

	reviews := []SyncableReview{
		{
			UUID:               uuid.NewString(),
			JobUUID:            jobUUID,
			Agent:              "test",
			Prompt:             "test prompt 1",
			Output:             "test output 1",
			Closed:             false,
			UpdatedByMachineID: defaultTestMachineID,
			CreatedAt:          time.Now(),
		},
		{
			UUID:               uuid.NewString(),
			JobUUID:            jobUUID,
			Agent:              "test",
			Prompt:             "test prompt 2",
			Output:             "test output 2",
			Closed:             true,
			UpdatedByMachineID: defaultTestMachineID,
			CreatedAt:          time.Now(),
		},
	}

	success, err := pool.BatchUpsertReviews(ctx, reviews)
	require.NoError(t, err, "BatchUpsertReviews failed: %v")

	assert.Equal(t, 2, countSuccesses(success))

	t.Run("empty batch is no-op", func(t *testing.T) {
		success, err := pool.BatchUpsertReviews(ctx, []SyncableReview{})
		require.NoError(t, err)
		assert.Nil(t, success)
	})

	t.Run("partial failure with invalid FK", func(t *testing.T) {
		validReviewUUID := uuid.NewString()
		reviews := []SyncableReview{
			{
				UUID:               validReviewUUID,
				JobUUID:            jobUUID, // Valid FK
				Agent:              "test",
				Prompt:             "valid review",
				Output:             "output",
				UpdatedByMachineID: defaultTestMachineID,
				CreatedAt:          time.Now(),
			},
			{
				UUID:               uuid.NewString(),
				JobUUID:            "00000000-0000-0000-0000-000000000000", // Invalid FK - will fail
				Agent:              "test",
				Prompt:             "invalid review",
				Output:             "output",
				UpdatedByMachineID: defaultTestMachineID,
				CreatedAt:          time.Now(),
			},
		}

		success, err := pool.BatchUpsertReviews(ctx, reviews)

		require.Error(t, err, "Expected error from batch with invalid FK, got nil")
		assert.Len(t, success, 2)
		assert.True(t, success[0], "Expected success[0]=true (valid FK)")
		assert.False(t, success[1], "Expected success[1]=false (invalid FK)")

		var count int
		err = pool.pool.QueryRow(ctx, `SELECT COUNT(*) FROM reviews WHERE uuid = $1`, validReviewUUID).Scan(&count)
		require.NoError(t, err, "Failed to query review: %v")

		assert.Equal(t, 0, count)
	})
}

func TestIntegration_BatchInsertResponses(t *testing.T) {
	pool := openTestPgPool(t)
	ctx := t.Context()

	repoID := createTestRepo(t, pool.Pool(), TestRepoOpts{Identity: "https://github.com/test/batch-responses-test.git"})
	commitID := createTestCommit(t, pool.Pool(), TestCommitOpts{RepoID: repoID, SHA: "batch-responses-sha"})
	jobUUID := uuid.NewString()

	createTestJob(t, pool.pool, TestJobOpts{
		UUID:            jobUUID,
		RepoID:          repoID,
		CommitID:        commitID,
		SourceMachineID: defaultTestMachineID,
	})

	responses := []SyncableResponse{
		{
			UUID:            uuid.NewString(),
			JobUUID:         jobUUID,
			Responder:       "user1",
			Response:        "response 1",
			SourceMachineID: defaultTestMachineID,
			CreatedAt:       time.Now(),
		},
		{
			UUID:            uuid.NewString(),
			JobUUID:         jobUUID,
			Responder:       "user2",
			Response:        "response 2",
			SourceMachineID: defaultTestMachineID,
			CreatedAt:       time.Now(),
		},
		{
			UUID:            uuid.NewString(),
			JobUUID:         jobUUID,
			Responder:       "agent",
			Response:        "response 3",
			SourceMachineID: defaultTestMachineID,
			CreatedAt:       time.Now(),
		},
	}

	success, err := pool.BatchInsertResponses(ctx, responses)
	require.NoError(t, err, "BatchInsertResponses failed: %v")

	assert.Equal(t, 3, countSuccesses(success))

	t.Run("empty batch is no-op", func(t *testing.T) {
		success, err := pool.BatchInsertResponses(ctx, []SyncableResponse{})
		require.NoError(t, err)
		assert.Nil(t, success)
	})

	t.Run("partial failure with invalid FK", func(t *testing.T) {
		responses := []SyncableResponse{
			{
				UUID:            uuid.NewString(),
				JobUUID:         jobUUID, // Valid FK
				Responder:       "user",
				Response:        "valid response",
				SourceMachineID: defaultTestMachineID,
				CreatedAt:       time.Now(),
			},
			{
				UUID:            uuid.NewString(),
				JobUUID:         "00000000-0000-0000-0000-000000000000", // Invalid FK
				Responder:       "user",
				Response:        "invalid response",
				SourceMachineID: defaultTestMachineID,
				CreatedAt:       time.Now(),
			},
		}

		success, err := pool.BatchInsertResponses(ctx, responses)

		require.Error(t, err, "Expected error from batch with invalid FK, got nil")
		assert.Len(t, success, 2)
		assert.True(t, success[0], "Expected success[0]=true (valid FK)")
		assert.False(t, success[1], "Expected success[1]=false (invalid FK)")
	})
}

func TestIntegration_EnsureSchema_MigratesV1ToV2(t *testing.T) {
	// This test verifies that a v1 schema (without model column) gets migrated to v2
	ctx := t.Context()

	env := NewMigrationTestEnv(t)
	// Clean up after test
	env.CleanupDropSchema("roborev")

	// Drop any existing schema to start fresh - this test needs to verify v1→v2 migration
	env.DropSchema("roborev")

	// Load and execute v1 schema from embedded SQL file
	// Use helper to parse and execute statements
	for _, stmt := range parseSQLStatements(postgresV1Schema) {
		env.Exec(stmt)
	}

	// Insert a test job to verify data survives migration
	testJobUUID := uuid.NewString()
	var repoID int64
	err := env.QueryRow(`
		INSERT INTO roborev.repos (identity) VALUES ('test-repo-v1-migration') RETURNING id
	`).Scan(&repoID)
	require.NoError(t, err, "Failed to insert test repo: %v")

	env.Exec(`
		INSERT INTO roborev.review_jobs (uuid, repo_id, git_ref, agent, status, source_machine_id, enqueued_at)
		VALUES ($1, $2, 'HEAD', 'test-agent', 'done', '00000000-0000-0000-0000-000000000001', NOW())
	`, testJobUUID, repoID)

	// Now connect with the normal pool and run EnsureSchema - should migrate v1→v2
	connString := getTestPostgresURL(t)
	pool, err := NewPgPool(ctx, connString, DefaultPgPoolConfig())
	require.NoError(t, err, "Failed to connect: %v")

	defer pool.Close()

	if err := pool.EnsureSchema(ctx); err != nil {
		require.NoError(t, err, "EnsureSchema failed: %v")
	}

	// Verify schema version advanced to 2
	var version int
	err = pool.pool.QueryRow(ctx, `SELECT MAX(version) FROM schema_version`).Scan(&version)
	require.NoError(t, err, "Failed to query schema version: %v")

	assert.Equal(t, pgSchemaVersion, version)

	// Verify model column was added
	var hasModelColumn bool
	err = pool.pool.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM information_schema.columns
			WHERE table_schema = 'roborev' AND table_name = 'review_jobs' AND column_name = 'model'
		)
	`).Scan(&hasModelColumn)
	require.NoError(t, err, "Failed to check for model column: %v")

	assert.True(t, hasModelColumn, "Expected model column to exist after v1→v2 migration")

	// Verify pre-existing job survived migration with model=NULL
	var jobAgent string
	var jobModel *string
	err = pool.pool.QueryRow(ctx, `SELECT agent, model FROM review_jobs WHERE uuid = $1`, testJobUUID).Scan(&jobAgent, &jobModel)
	require.NoError(t, err, "Failed to query test job after migration: %v")

	assert.Equal(t, "test-agent", jobAgent)
	assert.Nil(t, jobModel)
}

func TestIntegration_UpsertJob_BackfillsModel(t *testing.T) {
	// This test verifies that upserting a job with a model value backfills
	// an existing job that has NULL model (COALESCE behavior)
	pool := openTestPgPool(t)
	ctx := t.Context()

	// Create test data
	machineID := uuid.NewString()
	jobUUID := uuid.NewString()
	repoIdentity := "test-repo-backfill-" + time.Now().Format("20060102150405")

	defer func() {
		// Cleanup
		pool.pool.Exec(ctx, `DELETE FROM review_jobs WHERE uuid = $1`, jobUUID)
		pool.pool.Exec(ctx, `DELETE FROM repos WHERE identity = $1`, repoIdentity)
		pool.pool.Exec(ctx, `DELETE FROM machines WHERE machine_id = $1`, machineID)
	}()

	// Register machine and create repo
	if err := pool.RegisterMachine(ctx, machineID, "test"); err != nil {
		require.NoError(t, err, "RegisterMachine failed: %v")
	}
	repoID, err := pool.GetOrCreateRepo(ctx, repoIdentity)
	require.NoError(t, err, "GetOrCreateRepo failed: %v")

	// Insert job with NULL model directly
	_, err = pool.pool.Exec(ctx, `
		INSERT INTO review_jobs (uuid, repo_id, git_ref, agent, status, source_machine_id, enqueued_at, created_at, updated_at)
		VALUES ($1, $2, 'HEAD', 'test-agent', 'done', $3, NOW(), NOW(), NOW())
	`, jobUUID, repoID, machineID)
	require.NoError(t, err, "Failed to insert job with NULL model: %v")

	// Verify model is NULL
	var modelBefore *string
	err = pool.pool.QueryRow(ctx, `SELECT model FROM review_jobs WHERE uuid = $1`, jobUUID).Scan(&modelBefore)
	require.NoError(t, err, "Failed to query model before: %v")

	assert.Nil(t, modelBefore)

	// Upsert with a model value - should backfill
	job := SyncableJob{
		UUID:              jobUUID,
		RepoIdentity:      repoIdentity,
		GitRef:            "HEAD",
		Agent:             "test-agent",
		Model:             "gpt-4", // Now providing a model
		Provider:          "openai",
		RequestedModel:    "gpt-4",
		RequestedProvider: "openai",
		Status:            "done",
		SourceMachineID:   machineID,
		EnqueuedAt:        time.Now(),
	}
	err = pool.UpsertJob(ctx, job, repoID, nil)
	require.NoError(t, err, "UpsertJob failed: %v")

	// Verify model was backfilled.
	var modelAfter *string
	err = pool.pool.QueryRow(ctx, `SELECT model FROM review_jobs WHERE uuid = $1`, jobUUID).Scan(&modelAfter)
	require.NoError(t, err, "Failed to query model after: %v")
	assert.NotNil(t, modelAfter, "Expected model to be backfilled, but it's still NULL")
	if modelAfter != nil {
		assert.Equal(t, "gpt-4", *modelAfter)
	}

	// Upserting with empty values should clear the effective/requested fields so
	// synced reruns can propagate implicit/default state.
	job.Model = ""
	job.Provider = ""
	job.RequestedModel = ""
	job.RequestedProvider = ""
	err = pool.UpsertJob(ctx, job, repoID, nil)
	require.NoError(t, err, "UpsertJob (empty model/provider) failed: %v")

	var modelCleared, providerCleared, requestedModelCleared, requestedProviderCleared *string
	err = pool.pool.QueryRow(ctx, `SELECT model, provider, requested_model, requested_provider FROM review_jobs WHERE uuid = $1`, jobUUID).Scan(&modelCleared, &providerCleared, &requestedModelCleared, &requestedProviderCleared)
	require.NoError(t, err, "Failed to query cleared fields: %v")
	assert.Nil(t, modelCleared, "Expected empty model upsert to clear existing model")
	assert.Nil(t, providerCleared, "Expected empty provider upsert to clear existing provider")
	assert.Nil(t, requestedModelCleared, "Expected empty requested_model upsert to clear existing requested model")
	assert.Nil(t, requestedProviderCleared, "Expected empty requested_provider upsert to clear existing requested provider")
}
