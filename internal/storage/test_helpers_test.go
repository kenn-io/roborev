package storage

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

// MigrationTestEnv manages the environment for schema migration tests.
type MigrationTestEnv struct {
	t    *testing.T
	ctx  context.Context
	pool *pgxpool.Pool
}

func NewMigrationTestEnv(t *testing.T) *MigrationTestEnv {
	t.Helper()
	connString := getTestPostgresURL(t)
	cfg, err := pgxpool.ParseConfig(connString)
	require.NoError(t, err, "Failed to parse config")
	p, err := pgxpool.NewWithConfig(t.Context(), cfg)
	require.NoError(t, err, "Failed to create pool")
	t.Cleanup(func() { p.Close() })

	return &MigrationTestEnv{
		t:    t,
		ctx:  t.Context(),
		pool: p,
	}
}

func (e *MigrationTestEnv) DropSchema(schema string) {
	e.t.Helper()
	_, err := e.pool.Exec(e.ctx, "DROP SCHEMA IF EXISTS "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
	require.NoError(e.t, err, "Failed to drop schema %s", schema)
}

func (e *MigrationTestEnv) DropTable(schema, table string) {
	e.t.Helper()
	_, err := e.pool.Exec(e.ctx, "DROP TABLE IF EXISTS "+pgx.Identifier{schema, table}.Sanitize())
	require.NoError(e.t, err, "Failed to drop table %s.%s", schema, table)
}

func (e *MigrationTestEnv) CleanupDropSchema(schema string) {
	e.t.Helper()
	e.t.Cleanup(func() {
		// Use background context: t.Context() is canceled by
		// the time Cleanup runs.
		ctx, cancel := context.WithTimeout(
			context.Background(), 5*time.Second,
		)
		defer cancel()
		_, err := e.pool.Exec(ctx, "DROP SCHEMA IF EXISTS "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		require.NoError(e.t, err, "Failed to cleanup schema %s", schema)
	})
}

func (e *MigrationTestEnv) CleanupDropTable(schema, table string) {
	e.t.Helper()
	e.t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(
			context.Background(), 5*time.Second,
		)
		defer cancel()
		_, err := e.pool.Exec(ctx, "DROP TABLE IF EXISTS "+pgx.Identifier{schema, table}.Sanitize())
		require.NoError(e.t, err, "Failed to cleanup table %s.%s", schema, table)
	})
}

func (e *MigrationTestEnv) SkipIfTableInSchema(schema, table string) {
	e.t.Helper()
	var exists bool
	err := e.pool.QueryRow(e.ctx, `
		SELECT EXISTS(
			SELECT 1 FROM information_schema.tables
			WHERE table_schema = $1 AND table_name = $2
		)
	`, schema, table).Scan(&exists)
	require.NoError(e.t, err, "Failed to check %s.%s", schema, table)
	if exists {
		e.t.Skipf("Skipping: %s.%s already exists", schema, table)
	}
}

func (e *MigrationTestEnv) Exec(sql string, args ...any) {
	e.t.Helper()
	_, err := e.pool.Exec(e.ctx, sql, args...)
	require.NoError(e.t, err, "Failed to execute SQL\nSQL: %s", sql)
}

func (e *MigrationTestEnv) QueryRow(sql string, args ...any) pgx.Row {
	e.t.Helper()
	return e.pool.QueryRow(e.ctx, sql, args...)
}

// Data Factory Helpers

type TestRepoOpts struct {
	Identity string
}

func (opts *TestRepoOpts) applyDefaults() {
	if opts.Identity == "" {
		opts.Identity = "https://github.com/test/repo-" + uuid.NewString() + ".git"
	}
}

func createTestRepo(t *testing.T, pool *pgxpool.Pool, opts TestRepoOpts) int64 {
	t.Helper()
	opts.applyDefaults()

	var id int64
	err := pool.QueryRow(t.Context(), `
		INSERT INTO repos (identity)
		VALUES ($1)
		ON CONFLICT (identity) DO UPDATE SET identity = EXCLUDED.identity
		RETURNING id
	`, opts.Identity).Scan(&id)
	require.NoError(t, err, "Failed to create repo %s", opts.Identity)
	return id
}

type TestCommitOpts struct {
	RepoID    int64
	SHA       string
	Author    string
	Subject   string
	Timestamp time.Time
}

func (opts *TestCommitOpts) applyDefaults() {
	if opts.SHA == "" {
		opts.SHA = uuid.NewString()
	}
	if opts.Author == "" {
		opts.Author = "Test Author"
	}
	if opts.Subject == "" {
		opts.Subject = "Test Subject"
	}
	if opts.Timestamp.IsZero() {
		opts.Timestamp = time.Now()
	}
}

func createTestCommit(t *testing.T, pool *pgxpool.Pool, opts TestCommitOpts) int64 {
	t.Helper()
	opts.applyDefaults()

	var id int64
	err := pool.QueryRow(t.Context(), `
		INSERT INTO commits (repo_id, sha, author, subject, timestamp)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (repo_id, sha) DO UPDATE SET author = EXCLUDED.author
		RETURNING id
	`, opts.RepoID, opts.SHA, opts.Author, opts.Subject, opts.Timestamp).Scan(&id)
	require.NoError(t, err, "Failed to create commit %s", opts.SHA)
	return id
}

type TestJobOpts struct {
	UUID            string
	RepoID          int64
	CommitID        int64
	GitRef          string
	Agent           string
	Status          string
	SourceMachineID string
	EnqueuedAt      time.Time
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

func (opts *TestJobOpts) applyDefaults() {
	if opts.UUID == "" {
		opts.UUID = uuid.NewString()
	}
	if opts.GitRef == "" {
		opts.GitRef = "HEAD"
	}
	if opts.Agent == "" {
		opts.Agent = "test"
	}
	if opts.Status == "" {
		opts.Status = "done"
	}
	if opts.SourceMachineID == "" {
		opts.SourceMachineID = uuid.NewString()
	}

	now := time.Now()
	if opts.EnqueuedAt.IsZero() {
		opts.EnqueuedAt = now
	}
	if opts.CreatedAt.IsZero() {
		opts.CreatedAt = now
	}
	if opts.UpdatedAt.IsZero() {
		opts.UpdatedAt = now
	}
}

func createTestJob(t *testing.T, pool *pgxpool.Pool, opts TestJobOpts) {
	t.Helper()
	opts.applyDefaults()

	_, err := pool.Exec(t.Context(), `
		INSERT INTO review_jobs (uuid, repo_id, commit_id, git_ref, agent, status, source_machine_id, enqueued_at, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
	`, opts.UUID, opts.RepoID, opts.CommitID, opts.GitRef, opts.Agent, opts.Status, opts.SourceMachineID, opts.EnqueuedAt, opts.CreatedAt, opts.UpdatedAt)
	require.NoError(t, err, "Failed to create job %s", opts.UUID)
}

type TestReviewOpts struct {
	UUID               string
	JobUUID            string
	Agent              string
	Prompt             string
	Output             string
	Closed             bool
	CreatedAt          time.Time
	UpdatedAt          time.Time
	UpdatedByMachineID string
}

func (opts *TestReviewOpts) applyDefaults() {
	if opts.UUID == "" {
		opts.UUID = uuid.NewString()
	}
	if opts.Agent == "" {
		opts.Agent = "test"
	}
	now := time.Now()
	if opts.CreatedAt.IsZero() {
		opts.CreatedAt = now
	}
	if opts.UpdatedAt.IsZero() {
		opts.UpdatedAt = now
	}
	if opts.UpdatedByMachineID == "" {
		opts.UpdatedByMachineID = uuid.NewString()
	}
}

func createTestReview(t *testing.T, pool *pgxpool.Pool, opts TestReviewOpts) {
	t.Helper()
	opts.applyDefaults()

	_, err := pool.Exec(t.Context(), `
		INSERT INTO reviews (uuid, job_uuid, agent, prompt, output, closed, created_at, updated_at, updated_by_machine_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
	`, opts.UUID, opts.JobUUID, opts.Agent, opts.Prompt, opts.Output, opts.Closed, opts.CreatedAt, opts.UpdatedAt, opts.UpdatedByMachineID)
	require.NoError(t, err, "Failed to create review %s", opts.UUID)
}

func hasExecutableCode(stmt string) bool {
	for line := range strings.SplitSeq(stmt, "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "--") {
			return true
		}
	}
	return false
}

func parseSQLStatements(sql string) []string {
	var stmts []string
	for stmt := range strings.SplitSeq(sql, ";") {
		stmt = strings.TrimSpace(stmt)
		if stmt != "" && hasExecutableCode(stmt) {
			stmts = append(stmts, stmt)
		}
	}
	return stmts
}
