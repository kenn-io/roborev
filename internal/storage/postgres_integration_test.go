//go:build postgres

package storage

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
)

// getIntegrationPostgresURL returns the postgres URL for integration tests.
// Set via TEST_POSTGRES_URL environment variable or use default from docker-compose.test.yml
func getIntegrationPostgresURL() string {
	if url := os.Getenv("TEST_POSTGRES_URL"); url != "" {
		return url
	}
	return "postgres://roborev_test:roborev_test_password@localhost:5433/roborev_test"
}

// integrationEnv manages Postgres connection, schema, and temp directory for integration tests.
type integrationEnv struct {
	T      *testing.T
	Ctx    context.Context
	cancel context.CancelFunc
	Pool   *PgPool
	TmpDir string
	pgURL  string
}

// newIntegrationEnv creates a test environment: connects to Postgres, wipes and recreates the schema,
// and provides a temp directory for SQLite databases.
func newIntegrationEnv(t *testing.T, timeout time.Duration) *integrationEnv {
	t.Helper()
	url := getIntegrationPostgresURL()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)

	pool, err := NewPgPool(ctx, url, DefaultPgPoolConfig())
	if err != nil {
		cancel()
		require.Condition(t, func() bool {
			return false
		}, "Failed to connect to postgres: %v (is docker-compose running?)", err)
	}

	_, _ = pool.pool.Exec(ctx, "DROP SCHEMA IF EXISTS roborev CASCADE")
	if err := pool.EnsureSchema(ctx); err != nil {
		pool.Close()
		cancel()
		require.Condition(t, func() bool {
			return false
		}, "EnsureSchema failed: %v", err)
	}

	env := &integrationEnv{
		T:      t,
		Ctx:    ctx,
		cancel: cancel,
		Pool:   pool,
		TmpDir: t.TempDir(),
		pgURL:  url,
	}
	t.Cleanup(func() {
		pool.Close()
		cancel()
	})
	return env
}

func (e *integrationEnv) waitForLocalJobs(t *testing.T, db *DB, expected int, timeout time.Duration) {
	t.Helper()
	waitCondition(t, timeout, fmt.Sprintf("local job count %d", expected), func() (bool, error) {
		jobs, err := db.ListJobs("", "", 1000, 0)
		if err != nil {
			return false, err
		}
		return len(jobs) >= expected, nil
	})
}

// openDB creates a new SQLite database in the temp directory.
func (e *integrationEnv) openDB(name string) *DB {
	e.T.Helper()
	db, err := Open(filepath.Join(e.TmpDir, name))
	if err != nil {
		e.T.Fatalf("Failed to open SQLite %s: %v", name, err)
	}
	e.T.Cleanup(func() { db.Close() })
	return db
}

type testNode struct {
	DB     *DB
	Repo   *Repo
	Worker *SyncWorker
}

func (e *integrationEnv) setupNode(name, repoIdentity, syncInterval string) testNode {
	e.T.Helper()
	db := e.openDB(name + ".db")
	repo, err := db.GetOrCreateRepo(filepath.Join(e.TmpDir, "repo_"+name), repoIdentity)
	if err != nil {
		e.T.Fatalf("%s: GetOrCreateRepo failed: %v", name, err)
	}
	worker := startSyncWorker(e.T, db, e.pgURL, name, syncInterval)
	return testNode{DB: db, Repo: repo, Worker: worker}
}

// validPgTables is the allowlist of tables that may be queried by test helpers.
var validPgTables = map[string]bool{
	"machines":    true,
	"repos":       true,
	"commits":     true,
	"review_jobs": true,
	"reviews":     true,
	"responses":   true,
}

// assertPgCount asserts the row count of a table in Postgres.
// The table parameter is validated against an allowlist to prevent SQL injection.
func (e *integrationEnv) assertPgCount(table string, expected int) {
	e.T.Helper()
	if !validPgTables[table] {
		e.T.Fatalf("assertPgCount: invalid table name %q", table)
	}
	var count int
	err := e.Pool.pool.QueryRow(e.Ctx, fmt.Sprintf("SELECT COUNT(*) FROM roborev.%s", table)).Scan(&count)
	if err != nil {
		e.T.Fatalf("Failed to query postgres %s count: %v", table, err)
	}
	if count != expected {
		e.T.Errorf("Expected %d rows in %s, got %d", expected, table, count)
	}
}

// assertPgCountWhere asserts the row count with a WHERE clause.
// The table parameter is validated against an allowlist to prevent SQL injection.
func (e *integrationEnv) assertPgCountWhere(table, where string, args []interface{}, expected int) {
	e.T.Helper()
	if !validPgTables[table] {
		e.T.Fatalf("assertPgCountWhere: invalid table name %q", table)
	}
	var count int
	query := fmt.Sprintf("SELECT COUNT(*) FROM roborev.%s WHERE %s", table, where)
	err := e.Pool.pool.QueryRow(e.Ctx, query, args...).Scan(&count)
	if err != nil {
		e.T.Fatalf("Failed to query postgres %s: %v", table, err)
	}
	if count != expected {
		e.T.Errorf("Expected %d rows in %s WHERE %s, got %d", expected, table, where, count)
	}
}

// pgQueryString returns a single string value from a Postgres query.
func (e *integrationEnv) pgQueryString(query string, args ...interface{}) string {
	e.T.Helper()
	var val string
	if err := e.Pool.pool.QueryRow(e.Ctx, query, args...).Scan(&val); err != nil {
		e.T.Fatalf("pgQueryString failed: %v", err)
	}
	return val
}

// tryCreateCompletedReview creates a repo, commit, enqueues a job, marks it running, and completes it.
// Returns the job, review, and any error. Safe to call from goroutines (does not call t.Fatalf).
func tryCreateCompletedReview(db *DB, repoID int64, sha, author, subject, prompt, output string) (*ReviewJob, *Review, error) {
	commit, err := db.GetOrCreateCommit(repoID, sha, author, subject, time.Now())
	if err != nil {
		return nil, nil, fmt.Errorf("GetOrCreateCommit failed: %w", err)
	}
	job, err := db.EnqueueJob(EnqueueOpts{RepoID: repoID, CommitID: commit.ID, GitRef: sha, Agent: "test"})
	if err != nil {
		return nil, nil, fmt.Errorf("EnqueueJob failed: %w", err)
	}
	if _, err := db.Exec(`UPDATE review_jobs SET status = 'running', started_at = datetime('now') WHERE id = ?`, job.ID); err != nil {
		return nil, nil, fmt.Errorf("failed to set job running: %w", err)
	}
	if err := db.CompleteJob(job.ID, "test", prompt, output); err != nil {
		return nil, nil, fmt.Errorf("CompleteJob failed: %w", err)
	}
	review, err := db.GetReviewByJobID(job.ID)
	if err != nil {
		return nil, nil, fmt.Errorf("GetReviewByJobID failed: %w", err)
	}
	// Re-fetch job to get UUID
	jobs, err := db.ListJobs("", "", 1000, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("ListJobs failed: %w", err)
	}
	for _, j := range jobs {
		if j.ID == job.ID {
			job = &j
			break
		}
	}
	return job, review, nil
}

// tryCreateCompletedReviewWithoutCommit creates a job and completes it without an underlying commit.
func tryCreateCompletedReviewWithoutCommit(db *DB, repoID int64) (*ReviewJob, error) {
	job, err := db.EnqueueJob(EnqueueOpts{RepoID: repoID, CommitID: 0, GitRef: "HEAD", Agent: "test"})
	if err != nil {
		return nil, fmt.Errorf("EnqueueJob failed: %w", err)
	}
	if _, err := db.Exec(`UPDATE review_jobs SET status = 'running', started_at = datetime('now') WHERE id = ?`, job.ID); err != nil {
		return nil, fmt.Errorf("failed to set job running: %w", err)
	}
	if err := db.CompleteJob(job.ID, "test", "prompt", "output"); err != nil {
		return nil, fmt.Errorf("CompleteJob failed: %w", err)
	}
	return job, nil
}

// createCompletedReview creates a repo, commit, enqueues a job, marks it running, and completes it.
// Returns the job and review. Must NOT be called from a goroutine (uses t.Fatalf).
func createCompletedReview(t *testing.T, db *DB, repoID int64, sha, author, subject, prompt, output string) (*ReviewJob, *Review) {
	t.Helper()
	job, review, err := tryCreateCompletedReview(db, repoID, sha, author, subject, prompt, output)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "createCompletedReview failed: %v", err)
	}
	return job, review
}

// startSyncWorker creates a SyncWorker with the given config, starts it, waits for connection,
// and registers cleanup. Returns the worker.
func startSyncWorker(t *testing.T, db *DB, pgURL, machineName, interval string) *SyncWorker {
	t.Helper()
	if interval == "" {
		interval = "100ms"
	}
	cfg := config.SyncConfig{
		Enabled:        true,
		PostgresURL:    pgURL,
		Interval:       interval,
		MachineName:    machineName,
		ConnectTimeout: "5s",
	}
	worker := NewSyncWorker(db, cfg)
	if err := worker.Start(); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "SyncWorker.Start failed for %s: %v", machineName, err)
	}
	t.Cleanup(func() { worker.Stop() })

	if err := waitForSyncWorkerConnection(worker, 10*time.Second); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "Failed to connect for %s: %v", machineName, err)
	}
	return worker
}

// waitForSyncWorkerConnection waits for the SyncWorker to establish a postgres connection
func waitForSyncWorkerConnection(worker *SyncWorker, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		_, err := worker.SyncNow()
		if err == nil {
			return nil
		}
		if err.Error() != "not connected to PostgreSQL" {
			return err
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("timeout waiting for sync worker connection")
}

// startSyncWorkerNoSync starts a SyncWorker with initial sync
// disabled and waits for its background goroutine to establish
// a Postgres connection. Uses HealthCheck to poll connection
// status. With skipInitialSync set, no data is pushed or pulled
// until the first ticker tick. Use this when testing
// ticker-driven sync behavior.
func startSyncWorkerNoSync(
	t *testing.T,
	db *DB,
	pgURL, machineName, interval string,
) *SyncWorker {
	t.Helper()
	if interval == "" {
		interval = "100ms"
	}
	cfg := config.SyncConfig{
		Enabled:        true,
		PostgresURL:    pgURL,
		Interval:       interval,
		MachineName:    machineName,
		ConnectTimeout: "5s",
	}
	worker := NewSyncWorker(db, cfg)
	if err := worker.SetSkipInitialSync(true); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "SetSkipInitialSync failed for %s: %v", machineName, err)
	}
	if err := worker.Start(); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "SyncWorker.Start failed for %s: %v",
			machineName, err)
	}
	t.Cleanup(func() { worker.Stop() })

	// Poll HealthCheck until connected (no sync triggered)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		healthy, _ := worker.HealthCheck()
		if healthy {
			return worker
		}
		time.Sleep(50 * time.Millisecond)
	}
	require.Condition(t, func() bool {
		return false
	}, "Timeout waiting for %s to connect via HealthCheck",
		machineName)

	return nil
}

// createBatchReviews creates N completed reviews and returns their UUIDs.
func createBatchReviews(t *testing.T, db *DB, repoID int64, count int, shaPrefix, author, subjectPrefix string) []string {
	t.Helper()
	var uuids []string
	for i := 0; i < count; i++ {
		sha := fmt.Sprintf("%s_%02d", shaPrefix, i)
		subject := fmt.Sprintf("%s %d", subjectPrefix, i)
		job, _ := createCompletedReview(t, db, repoID, sha, author, subject, "prompt", fmt.Sprintf("Review %s", sha))
		uuids = append(uuids, job.UUID)
	}
	return uuids
}

// waitCondition waits for a condition to be true or times out.
func waitCondition(t *testing.T, timeout time.Duration, msg string, condition func() (bool, error)) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		ok, err := condition()
		if err != nil {
			lastErr = err
		}
		if ok {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	if lastErr != nil {
		require.Condition(t, func() bool {
			return false
		}, "Timeout waiting for: %s (last error: %v)", msg, lastErr)
	}
	require.Condition(t, func() bool {
		return false
	}, "Timeout waiting for: %s", msg)
}

func jobUUIDSet(jobs []ReviewJob) map[string]bool {
	uuids := make(map[string]bool, len(jobs))
	for _, job := range jobs {
		uuids[job.UUID] = true
	}
	return uuids
}

func requireLocalJobByUUID(t *testing.T, db *DB, uuid string) ReviewJob {
	t.Helper()
	jobs, err := db.ListJobs("", "", 1000, 0)
	require.NoError(t, err)
	var found *ReviewJob
	for _, job := range jobs {
		if job.UUID == uuid {
			j := job
			found = &j
			break
		}
	}
	require.NotNil(t, found, "job UUID %s not found", uuid)
	return *found
}

// TestIntegration_MigrationV6Idempotent verifies the v5→v6 migration
// (addressed→closed rename) succeeds when the reviews table already
// has the closed column. This happens when legacy schema_version
// exists at an old version but data tables were created fresh.
func TestIntegration_MigrationV6Idempotent(t *testing.T) {
	url := getIntegrationPostgresURL()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := NewPgPool(ctx, url, DefaultPgPoolConfig())
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "Failed to connect: %v", err)
	}
	defer pool.Close()

	// Start clean
	_, _ = pool.pool.Exec(ctx, "DROP SCHEMA IF EXISTS roborev CASCADE")

	// Create schema and run EnsureSchema to get fresh tables
	// (with closed column, not addressed)
	if err := pool.EnsureSchema(ctx); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "Initial EnsureSchema failed: %v", err)
	}

	// Simulate legacy state: downgrade version to 5 so the
	// v5→v6 migration will run again, but the table already
	// has closed (not addressed).
	_, err = pool.pool.Exec(ctx, `DELETE FROM schema_version`)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "Failed to clear schema_version: %v", err)
	}
	_, err = pool.pool.Exec(ctx, `INSERT INTO schema_version (version) VALUES (5)`)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "Failed to insert version 5: %v", err)
	}

	// This should succeed — the migration must be idempotent
	if err := pool.EnsureSchema(ctx); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "EnsureSchema with v5→v6 on fresh table failed: %v", err)
	}

	// Verify the column is named closed
	var colName string
	err = pool.pool.QueryRow(ctx, `
		SELECT column_name FROM information_schema.columns
		WHERE table_schema = 'roborev'
		AND table_name = 'reviews'
		AND column_name = 'closed'
	`).Scan(&colName)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "closed column not found: %v", err)
	}
}

func TestIntegration_SyncFullCycle(t *testing.T) {
	env := newIntegrationEnv(t, 30*time.Second)
	db := env.openDB("test.db")

	repo, err := db.GetOrCreateRepo(env.TmpDir, "git@github.com:test/integration.git")
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "GetOrCreateRepo failed: %v", err)
	}

	_, review := createCompletedReview(t, db, repo.ID, "abc123def456", "Test Author", "Test subject", "Test prompt", "Test review output")

	worker := startSyncWorker(t, db, env.pgURL, "integration-test", "1s")
	_ = worker

	env.assertPgCount("repos", 1)
	env.assertPgCount("review_jobs", 1)
	env.assertPgCount("reviews", 1)

	pgReviewUUID := env.pgQueryString("SELECT uuid FROM roborev.reviews")
	if pgReviewUUID != review.UUID {
		assert.Condition(t, func() bool {
			return false
		}, "Review UUID mismatch: postgres=%s, local=%s", pgReviewUUID, review.UUID)
	}
}

func TestIntegration_SyncMultipleRepos(t *testing.T) {
	env := newIntegrationEnv(t, 30*time.Second)
	db := env.openDB("test.db")

	repo1, _ := db.GetOrCreateRepo(filepath.Join(env.TmpDir, "repo1"), "git@github.com:test/repo1.git")
	repo2, _ := db.GetOrCreateRepo(filepath.Join(env.TmpDir, "repo2"), "git@github.com:test/repo2.git")

	sameSHA := "deadbeef12345678"
	createCompletedReview(t, db, repo1.ID, sameSHA, "Author", "Subject", "prompt", "output")
	createCompletedReview(t, db, repo2.ID, sameSHA, "Author", "Subject", "prompt", "output")

	startSyncWorker(t, db, env.pgURL, "multi-repo-test", "1s")

	env.assertPgCount("repos", 2)
	env.assertPgCountWhere("commits", "sha = $1", []interface{}{sameSHA}, 2)
}

func TestIntegration_PullFromRemote(t *testing.T) {
	env := newIntegrationEnv(t, 30*time.Second)

	// Insert data directly into postgres (simulating another machine's sync)
	remoteMachineUUID := "11111111-1111-1111-1111-111111111111"
	_, err := env.Pool.pool.Exec(env.Ctx, `
		INSERT INTO roborev.machines (machine_id, name, last_seen_at)
		VALUES ($1, 'remote-test', NOW())
	`, remoteMachineUUID)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "Failed to insert machine: %v", err)
	}

	_, err = env.Pool.pool.Exec(env.Ctx, `
		INSERT INTO roborev.repos (identity, created_at)
		VALUES ('git@github.com:test/pull-test.git', NOW())
	`)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "Failed to insert repo: %v", err)
	}

	var pgRepoID int64
	env.Pool.pool.QueryRow(env.Ctx, "SELECT id FROM roborev.repos WHERE identity = $1", "git@github.com:test/pull-test.git").Scan(&pgRepoID)

	remoteJobUUID := "22222222-2222-2222-2222-222222222222"
	_, err = env.Pool.pool.Exec(env.Ctx, `
		INSERT INTO roborev.review_jobs (uuid, repo_id, git_ref, status, agent, reasoning, job_type, review_type, source_machine_id, enqueued_at, created_at, updated_at)
		VALUES ($1, $2, 'main', 'done', 'test', '', 'review', '', $3, NOW(), NOW(), NOW())
	`, remoteJobUUID, pgRepoID, remoteMachineUUID)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "Failed to insert job: %v", err)
	}

	db := env.openDB("test.db")
	worker := startSyncWorker(t, db, env.pgURL, "local-test", "1s")

	if _, err := worker.SyncNow(); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "SyncNow failed: %v", err)
	}

	jobs, err := db.ListJobs("", "", 100, 0)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "ListJobs failed: %v", err)
	}
	if len(jobs) != 1 {
		assert.Condition(t, func() bool {
			return false
		}, "Expected 1 job in local DB, got %d", len(jobs))
	}
	if len(jobs) > 0 && jobs[0].UUID != remoteJobUUID {
		assert.Condition(t, func() bool {
			return false
		}, "Expected job UUID '%s', got %s", remoteJobUUID, jobs[0].UUID)
	}
}

func TestIntegration_SyncPullsLateVisibleJobBeforeCursor(t *testing.T) {
	env := newIntegrationEnv(t, 30*time.Second)

	repoIdentity := "git@github.com:test/late-visible.git"
	nodeA := env.setupNode("late-visible-a", repoIdentity, "1h")
	nodeB := env.setupNode("late-visible-b", repoIdentity, "1h")

	createCompletedReview(t, nodeB.DB, nodeB.Repo.ID, "late_01", "Bob", "First", "prompt", "output")
	createCompletedReview(t, nodeB.DB, nodeB.Repo.ID, "late_02", "Bob", "Second", "prompt", "output")
	_, err := nodeB.Worker.SyncNow()
	require.NoError(t, err)
	_, err = nodeA.Worker.SyncNow()
	require.NoError(t, err)

	cursor, err := nodeA.DB.GetSyncState(SyncStateLastJobCursor)
	require.NoError(t, err)
	cursorTime, _, ok := parseTimestampIDCursor(cursor)
	require.True(t, ok)

	machineB, err := nodeB.DB.GetMachineID()
	require.NoError(t, err)
	pgRepoID, err := env.Pool.GetOrCreateRepo(env.Ctx, repoIdentity)
	require.NoError(t, err)
	lateUUID := "11111111-1111-1111-1111-111111111111"
	_, err = env.Pool.pool.Exec(env.Ctx, `
		INSERT INTO roborev.review_jobs (
			uuid, repo_id, git_ref, agent, job_type, review_type, status,
			enqueued_at, source_machine_id, updated_at
		) VALUES ($1, $2, $3, 'test', 'review', '', 'done', $4, $5, $6)
	`, lateUUID, pgRepoID, "late_stale", cursorTime.Add(-10*time.Second), machineB, cursorTime.Add(-10*time.Second))
	require.NoError(t, err)

	_, err = nodeA.Worker.SyncNow()
	require.NoError(t, err)

	jobs, err := nodeA.DB.ListJobs("", "", 1000, 0)
	require.NoError(t, err)
	assert.Contains(t, jobUUIDSet(jobs), lateUUID)
}

func TestIntegration_SyncLookbackDoesNotRevertAppliedFixJob(t *testing.T) {
	t.Setenv(syncCursorLookbackEnv, "1h")
	env := newIntegrationEnv(t, 30*time.Second)

	repoIdentity := "git@github.com:test/lookback-applied.git"
	nodeA := env.setupNode("lookback-applied-a", repoIdentity, "1h")
	nodeB := env.setupNode("lookback-applied-b", repoIdentity, "1h")

	commit, err := nodeB.DB.GetOrCreateCommit(nodeB.Repo.ID, "fix_01", "Bob", "Fix patch", time.Now())
	require.NoError(t, err)
	fixJob, err := nodeB.DB.EnqueueJob(EnqueueOpts{
		RepoID:   nodeB.Repo.ID,
		CommitID: commit.ID,
		GitRef:   "fix_01",
		Agent:    "test",
		JobType:  JobTypeFix,
	})
	require.NoError(t, err)
	_, err = nodeB.DB.Exec(`UPDATE review_jobs SET status = 'running', started_at = datetime('now') WHERE id = ?`, fixJob.ID)
	require.NoError(t, err)
	require.NoError(t, nodeB.DB.CompleteJob(fixJob.ID, "test", "prompt", "patch output"))

	_, err = nodeB.Worker.SyncNow()
	require.NoError(t, err)
	_, err = nodeA.Worker.SyncNow()
	require.NoError(t, err)

	localFixJob := requireLocalJobByUUID(t, nodeA.DB, fixJob.UUID)
	assert.Equal(t, JobStatusDone, localFixJob.Status)

	require.NoError(t, nodeA.DB.MarkJobApplied(localFixJob.ID))

	_, err = nodeA.Worker.SyncNow()
	require.NoError(t, err)

	localFixJob = requireLocalJobByUUID(t, nodeA.DB, fixJob.UUID)
	assert.Equal(t, JobStatusApplied, localFixJob.Status)
}

func TestIntegration_SyncPullsLateVisibleResponseBeforeCursor(t *testing.T) {
	env := newIntegrationEnv(t, 30*time.Second)

	repoIdentity := "git@github.com:test/late-response.git"
	nodeA := env.setupNode("late-response-a", repoIdentity, "1h")
	nodeB := env.setupNode("late-response-b", repoIdentity, "1h")

	jobB, _ := createCompletedReview(t, nodeB.DB, nodeB.Repo.ID, "resp_01", "Bob", "First", "prompt", "output")
	_, err := nodeB.Worker.SyncNow()
	require.NoError(t, err)
	_, err = nodeA.Worker.SyncNow()
	require.NoError(t, err)

	machineB, err := nodeB.DB.GetMachineID()
	require.NoError(t, err)

	tx, err := env.Pool.pool.Begin(env.Ctx)
	require.NoError(t, err)
	defer tx.Rollback(env.Ctx)

	lateUUID := "33333333-3333-3333-3333-333333333333"
	_, err = tx.Exec(env.Ctx, `
		INSERT INTO roborev.responses (uuid, job_uuid, responder, response, source_machine_id, created_at)
		VALUES ($1, $2, 'human', 'late response', $3, clock_timestamp())
	`, lateUUID, jobB.UUID, machineB)
	require.NoError(t, err)

	highUUID := "44444444-4444-4444-4444-444444444444"
	_, err = env.Pool.pool.Exec(env.Ctx, `
		INSERT INTO roborev.responses (uuid, job_uuid, responder, response, source_machine_id, created_at)
		VALUES ($1, $2, 'human', 'high response', $3, clock_timestamp())
	`, highUUID, jobB.UUID, machineB)
	require.NoError(t, err)

	_, err = nodeA.Worker.SyncNow()
	require.NoError(t, err)

	err = tx.Commit(env.Ctx)
	require.NoError(t, err)

	_, err = nodeA.Worker.SyncNow()
	require.NoError(t, err)

	rows, err := nodeA.DB.Query(`SELECT uuid FROM responses WHERE uuid IN (?, ?)`, lateUUID, highUUID)
	require.NoError(t, err)
	defer rows.Close()

	uuids := map[string]bool{}
	for rows.Next() {
		var uuid string
		require.NoError(t, rows.Scan(&uuid))
		uuids[uuid] = true
	}
	require.NoError(t, rows.Err())
	assert.Contains(t, uuids, highUUID)
	assert.Contains(t, uuids, lateUUID)
}

func TestIntegration_FinalPush(t *testing.T) {
	env := newIntegrationEnv(t, 30*time.Second)
	db := env.openDB("test.db")

	repo, _ := db.GetOrCreateRepo(env.TmpDir, "git@github.com:test/finalpush.git")

	// Create 150 jobs without commits to match previous test behavior
	// (replacing createBatchReviews which forces commits)
	for i := 0; i < 150; i++ {
		if _, err := tryCreateCompletedReviewWithoutCommit(db, repo.ID); err != nil {
			require.Condition(t, func() bool {
				return false
			}, "tryCreateCompletedReviewWithoutCommit failed: %v", err)
		}
	}

	worker := startSyncWorker(t, db, env.pgURL, "finalpush-test", "1h")

	if err := worker.FinalPush(); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "FinalPush failed: %v", err)
	}

	env.assertPgCount("review_jobs", 150)
	env.assertPgCountWhere(
		"review_jobs", "commit_id IS NULL", nil, 150,
	)
	env.assertPgCount("commits", 0)
	env.assertPgCount("reviews", 150)
}

func TestIntegration_FinalPush_NoCommit(t *testing.T) {
	env := newIntegrationEnv(t, 30*time.Second)
	db := env.openDB("test.db")

	repo, err := db.GetOrCreateRepo(env.TmpDir, "git@github.com:test/finalpush-nocommit.git")
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "GetOrCreateRepo failed: %v", err)
	}

	// Create a job without a commit (CommitID=0)
	_, err = tryCreateCompletedReviewWithoutCommit(db, repo.ID)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "tryCreateCompletedReviewWithoutCommit failed: %v", err)
	}

	worker := startSyncWorker(t, db, env.pgURL, "finalpush-nocommit-test", "1h")

	if err := worker.FinalPush(); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "FinalPush failed: %v", err)
	}

	env.assertPgCount("review_jobs", 1)
	env.assertPgCountWhere(
		"review_jobs", "commit_id IS NULL", nil, 1,
	)
	env.assertPgCount("reviews", 1)
}

func TestIntegration_SchemaCreation(t *testing.T) {
	env := newIntegrationEnv(t, 30*time.Second)

	tables := []string{"machines", "repos", "commits", "review_jobs", "reviews", "responses"}
	for _, table := range tables {
		var exists bool
		err := env.Pool.pool.QueryRow(env.Ctx, `
			SELECT EXISTS (
				SELECT FROM information_schema.tables
				WHERE table_schema = 'roborev' AND table_name = $1
			)
		`, table).Scan(&exists)
		if err != nil {
			require.Condition(t, func() bool {
				return false
			}, "Failed to check table %s: %v", table, err)
		}
		if !exists {
			assert.Condition(t, func() bool {
				return false
			}, "Expected table roborev.%s to exist", table)
		}
	}
}

func TestIntegration_Multiplayer(t *testing.T) {
	env := newIntegrationEnv(t, 60*time.Second)

	sharedRepoIdentity := "git@github.com:test/multiplayer-repo.git"

	nodeA := env.setupNode("machine-a", sharedRepoIdentity, "100ms")
	nodeB := env.setupNode("machine-b", sharedRepoIdentity, "100ms")

	dbA, dbB := nodeA.DB, nodeB.DB
	workerA, workerB := nodeA.Worker, nodeB.Worker

	jobA, reviewA := createCompletedReview(t, dbA, nodeA.Repo.ID, "aaaa1111", "Alice", "Feature A", "prompt A", "Review from Machine A")
	jobB, reviewB := createCompletedReview(t, dbB, nodeB.Repo.ID, "bbbb2222", "Bob", "Feature B", "prompt B", "Review from Machine B")

	// Sync to push, then sync again to pull each other's data
	if _, err := workerA.SyncNow(); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "Machine A: SyncNow failed: %v", err)
	}
	if _, err := workerB.SyncNow(); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "Machine B: SyncNow failed: %v", err)
	}
	if _, err := workerA.SyncNow(); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "Machine A: Second SyncNow failed: %v", err)
	}
	if _, err := workerB.SyncNow(); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "Machine B: Second SyncNow failed: %v", err)
	}

	env.waitForLocalJobs(t, dbA, 2, 10*time.Second)
	env.waitForLocalJobs(t, dbB, 2, 10*time.Second)

	env.assertPgCount("review_jobs", 2)
	env.assertPgCount("reviews", 2)

	// Verify Machine A can see Machine B's review
	jobsA, err := dbA.ListJobs("", "", 100, 0)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "Machine A: ListJobs failed: %v", err)
	}
	if len(jobsA) != 2 {
		assert.Condition(t, func() bool {
			return false
		}, "Machine A should see 2 jobs (own + pulled), got %d", len(jobsA))
	}

	var foundBinA bool
	for _, j := range jobsA {
		if j.UUID == jobB.UUID {
			foundBinA = true
			break
		}
	}
	if !foundBinA {
		assert.Condition(t, func() bool {
			return false
		}, "Machine A should have pulled Machine B's job")
	}

	// Verify Machine B can see Machine A's review
	jobsB, err := dbB.ListJobs("", "", 100, 0)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "Machine B: ListJobs failed: %v", err)
	}
	if len(jobsB) != 2 {
		assert.Condition(t, func() bool {
			return false
		}, "Machine B should see 2 jobs (own + pulled), got %d", len(jobsB))
	}

	var foundAinB bool
	for _, j := range jobsB {
		if j.UUID == jobA.UUID {
			foundAinB = true
			break
		}
	}
	if !foundAinB {
		assert.Condition(t, func() bool {
			return false
		}, "Machine B should have pulled Machine A's job")
	}

	// Verify review content was pulled correctly
	reviewBinA, err := dbA.GetReviewByCommitSHA("bbbb2222")
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "Machine A: GetReviewByCommitSHA for B's commit failed: %v", err)
	}
	if reviewBinA.UUID != reviewB.UUID {
		assert.Condition(t, func() bool {
			return false
		}, "Machine A: pulled review UUID mismatch: got %s, want %s", reviewBinA.UUID, reviewB.UUID)
	}

	reviewAinB, err := dbB.GetReviewByCommitSHA("aaaa1111")
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "Machine B: GetReviewByCommitSHA for A's commit failed: %v", err)
	}
	if reviewAinB.UUID != reviewA.UUID {
		assert.Condition(t, func() bool {
			return false
		}, "Machine B: pulled review UUID mismatch: got %s, want %s", reviewAinB.UUID, reviewA.UUID)
	}

	t.Log("Multiplayer sync verified: both machines can see each other's reviews")
}

func TestIntegration_MultiplayerSameCommit(t *testing.T) {
	env := newIntegrationEnv(t, 60*time.Second)

	sharedRepoIdentity := "git@github.com:test/same-commit-repo.git"
	sharedCommitSHA := "cccc3333"

	nodeA := env.setupNode("machine-a", sharedRepoIdentity, "100ms")
	nodeB := env.setupNode("machine-b", sharedRepoIdentity, "100ms")
	dbA, dbB := nodeA.DB, nodeB.DB
	workerA, workerB := nodeA.Worker, nodeB.Worker

	jobA, reviewA := createCompletedReview(t, dbA, nodeA.Repo.ID, sharedCommitSHA, "Charlie", "Shared commit", "prompt", "Machine A's review of shared commit")
	jobB, reviewB := createCompletedReview(t, dbB, nodeB.Repo.ID, sharedCommitSHA, "Charlie", "Shared commit", "prompt", "Machine B's review of shared commit")

	if jobA.UUID == jobB.UUID {
		require.Condition(t, func() bool {
			return false
		}, "Jobs from different machines should have different UUIDs")
	}

	// Sync both, then pull each other's data
	if _, err := workerA.SyncNow(); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "Machine A: SyncNow failed: %v", err)
	}
	if _, err := workerB.SyncNow(); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "Machine B: SyncNow failed: %v", err)
	}
	if _, err := workerA.SyncNow(); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "Machine A: Second SyncNow failed: %v", err)
	}
	if _, err := workerB.SyncNow(); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "Machine B: Second SyncNow failed: %v", err)
	}

	env.waitForLocalJobs(t, dbA, 2, 10*time.Second)
	env.waitForLocalJobs(t, dbB, 2, 10*time.Second)

	env.assertPgCount("review_jobs", 2)
	env.assertPgCount("reviews", 2)

	// Both machines should now have both jobs
	jobsA, err := dbA.ListJobs("", "", 100, 0)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "Machine A: ListJobs failed: %v", err)
	}
	jobsB, err := dbB.ListJobs("", "", 100, 0)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "Machine B: ListJobs failed: %v", err)
	}

	if len(jobsA) != 2 {
		assert.Condition(t, func() bool {
			return false
		}, "Machine A should have 2 jobs, got %d", len(jobsA))
	}
	if len(jobsB) != 2 {
		assert.Condition(t, func() bool {
			return false
		}, "Machine B should have 2 jobs, got %d", len(jobsB))
	}

	// Verify both job UUIDs are present in each machine's database
	jobsAMap := make(map[string]bool)
	for _, j := range jobsA {
		jobsAMap[j.UUID] = true
	}
	if !jobsAMap[jobA.UUID] || !jobsAMap[jobB.UUID] {
		assert.Condition(t, func() bool {
			return false
		}, "Machine A missing expected job UUIDs: has A=%v, has B=%v", jobsAMap[jobA.UUID], jobsAMap[jobB.UUID])
	}

	jobsBMap := make(map[string]bool)
	for _, j := range jobsB {
		jobsBMap[j.UUID] = true
	}
	if !jobsBMap[jobA.UUID] || !jobsBMap[jobB.UUID] {
		assert.Condition(t, func() bool {
			return false
		}, "Machine B missing expected job UUIDs: has A=%v, has B=%v", jobsBMap[jobA.UUID], jobsBMap[jobB.UUID])
	}

	// Verify reviews are present on both machines
	reviewAonA, err := dbA.GetReviewByJobID(jobA.ID)
	if err != nil {
		t.Logf("Machine A: local review A query by job ID: %v (expected for pulled jobs)", err)
	} else if reviewAonA.UUID != reviewA.UUID {
		assert.Condition(t, func() bool {
			return false
		}, "Machine A: review A UUID mismatch")
	}

	var jobBIDonA int64
	for _, j := range jobsA {
		if j.UUID == jobB.UUID {
			jobBIDonA = j.ID
			break
		}
	}
	if jobBIDonA == 0 {
		assert.Condition(t, func() bool {
			return false
		}, "Machine A: could not find job B by UUID")
	} else {
		reviewBonA, err := dbA.GetReviewByJobID(jobBIDonA)
		if err != nil {
			assert.Condition(t, func() bool {
				return false
			}, "Machine A: failed to get review B: %v", err)
		} else if reviewBonA.UUID != reviewB.UUID {
			assert.Condition(t, func() bool {
				return false
			}, "Machine A: review B UUID mismatch: got %s, want %s", reviewBonA.UUID, reviewB.UUID)
		}
	}

	var jobAIDonB int64
	for _, j := range jobsB {
		if j.UUID == jobA.UUID {
			jobAIDonB = j.ID
			break
		}
	}
	if jobAIDonB == 0 {
		assert.Condition(t, func() bool {
			return false
		}, "Machine B: could not find job A by UUID")
	} else {
		reviewAonB, err := dbB.GetReviewByJobID(jobAIDonB)
		if err != nil {
			assert.Condition(t, func() bool {
				return false
			}, "Machine B: failed to get review A: %v", err)
		} else if reviewAonB.UUID != reviewA.UUID {
			assert.Condition(t, func() bool {
				return false
			}, "Machine B: review A UUID mismatch: got %s, want %s", reviewAonB.UUID, reviewA.UUID)
		}
	}

	reviewBonB, err := dbB.GetReviewByJobID(jobB.ID)
	if err != nil {
		t.Logf("Machine B: local review B query by job ID: %v (expected for pulled jobs)", err)
	} else if reviewBonB.UUID != reviewB.UUID {
		assert.Condition(t, func() bool {
			return false
		}, "Machine B: review B UUID mismatch")
	}

	t.Log("Same-commit multiplayer verified: both reviews preserved with unique UUIDs")
}

func runConcurrentReviewsAndSync(db *DB, repoID int64, worker *SyncWorker, prefix, author string, count int, results chan<- string, errs chan<- error, done chan<- bool) {
	go func() {
		defer func() { done <- true }()
		for i := 0; i < count; i++ {
			job, _, err := tryCreateCompletedReview(db, repoID, fmt.Sprintf("%s_%02d", prefix, i), author, fmt.Sprintf("%s concurrent %d", author, i), "prompt", fmt.Sprintf("Review %s-%d", prefix, i))
			if err != nil {
				errs <- err
				continue
			}
			results <- job.UUID
			if i%3 == 0 {
				if _, err := worker.SyncNow(); err != nil {
					errs <- fmt.Errorf("%s sync at job %d: %w", author, i, err)
				}
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
}

func TestIntegration_MultiplayerRealistic(t *testing.T) {
	env := newIntegrationEnv(t, 120*time.Second)

	sharedRepoIdentity := "git@github.com:team/shared-project.git"

	nodeA := env.setupNode("alice-laptop", sharedRepoIdentity, "1h")
	nodeB := env.setupNode("bob-desktop", sharedRepoIdentity, "1h")
	nodeC := env.setupNode("carol-workstation", sharedRepoIdentity, "1h")

	dbA, repoA, workerA := nodeA.DB, nodeA.Repo, nodeA.Worker
	dbB, repoB, workerB := nodeB.DB, nodeB.Repo, nodeB.Worker
	dbC, repoC, workerC := nodeC.DB, nodeC.Repo, nodeC.Worker

	var jobsCreatedByA, jobsCreatedByB, jobsCreatedByC []string

	syncAll := func(t *testing.T) {
		t.Helper()
		if _, err := workerA.SyncNow(); err != nil {
			require.Condition(t, func() bool {
				return false
			}, "Machine A sync failed: %v", err)
		}
		if _, err := workerB.SyncNow(); err != nil {
			require.Condition(t, func() bool {
				return false
			}, "Machine B sync failed: %v", err)
		}
		if _, err := workerC.SyncNow(); err != nil {
			require.Condition(t, func() bool {
				return false
			}, "Machine C sync failed: %v", err)
		}
	}

	t.Run("Round 1: Sequential", func(t *testing.T) {
		t.Log("Round 1: Each machine creates 10 reviews (no sync yet)")
		jobsCreatedByA = append(jobsCreatedByA, createBatchReviews(t, dbA, repoA.ID, 10, "a1", "Alice", "Alice commit")...)
		jobsCreatedByB = append(jobsCreatedByB, createBatchReviews(t, dbB, repoB.ID, 10, "b1", "Bob", "Bob commit")...)
		jobsCreatedByC = append(jobsCreatedByC, createBatchReviews(t, dbC, repoC.ID, 10, "c1", "Carol", "Carol commit")...)

		// Sync twice to push and pull
		syncAll(t)
		syncAll(t)

		env.waitForLocalJobs(t, dbA, 30, 10*time.Second)
		env.assertPgCount("review_jobs", 30)
	})

	t.Run("Round 2: Interleaved", func(t *testing.T) {
		t.Log("Round 2: Interleaved creation and syncing")
		jobsCreatedByA = append(jobsCreatedByA, createBatchReviews(t, dbA, repoA.ID, 5, "a2", "Alice", "Alice round2")...)
		if _, err := workerA.SyncNow(); err != nil {
			require.Condition(t, func() bool {
				return false
			}, "Machine A round 2: SyncNow failed: %v", err)
		}

		jobsCreatedByB = append(jobsCreatedByB, createBatchReviews(t, dbB, repoB.ID, 5, "b2", "Bob", "Bob round2")...)
		if _, err := workerB.SyncNow(); err != nil {
			require.Condition(t, func() bool {
				return false
			}, "Machine B round 2: SyncNow failed: %v", err)
		}

		jobsCreatedByC = append(jobsCreatedByC, createBatchReviews(t, dbC, repoC.ID, 5, "c2", "Carol", "Carol round2")...)
		if _, err := workerC.SyncNow(); err != nil {
			require.Condition(t, func() bool {
				return false
			}, "Machine C round 2: SyncNow failed: %v", err)
		}

		// All machines sync again
		syncAll(t)

		env.waitForLocalJobs(t, dbA, 45, 10*time.Second)
		env.assertPgCount("review_jobs", 45)
	})

	t.Run("Round 3: Concurrent", func(t *testing.T) {
		t.Log("Round 3: Concurrent creation during sync")

		jobResultsA := make(chan string, 10)
		jobResultsB := make(chan string, 10)
		jobResultsC := make(chan string, 10)
		syncErrsA := make(chan error, 4)
		syncErrsB := make(chan error, 4)
		syncErrsC := make(chan error, 4)
		done := make(chan bool, 3)

		runConcurrentReviewsAndSync(dbA, repoA.ID, workerA, "a3", "Alice", 10, jobResultsA, syncErrsA, done)
		runConcurrentReviewsAndSync(dbB, repoB.ID, workerB, "b3", "Bob", 10, jobResultsB, syncErrsB, done)
		runConcurrentReviewsAndSync(dbC, repoC.ID, workerC, "c3", "Carol", 10, jobResultsC, syncErrsC, done)

		<-done
		<-done
		<-done

		close(jobResultsA)
		close(jobResultsB)
		close(jobResultsC)
		close(syncErrsA)
		close(syncErrsB)
		close(syncErrsC)

		for uuid := range jobResultsA {
			jobsCreatedByA = append(jobsCreatedByA, uuid)
		}
		for uuid := range jobResultsB {
			jobsCreatedByB = append(jobsCreatedByB, uuid)
		}
		for uuid := range jobResultsC {
			jobsCreatedByC = append(jobsCreatedByC, uuid)
		}

		for err := range syncErrsA {
			assert.Condition(t, func() bool {
				return false
			}, "%v", err)
		}
		for err := range syncErrsB {
			assert.Condition(t, func() bool {
				return false
			}, "%v", err)
		}
		for err := range syncErrsC {
			assert.Condition(t, func() bool {
				return false
			}, "%v", err)
		}

		expectedTotal := 75

		// Workers run with a long interval in this test, so convergence
		// requires explicit repeated SyncNow calls until all nodes have
		// pulled the full set.
		waitCondition(t, 20*time.Second, fmt.Sprintf("all machines converge to %d jobs", expectedTotal), func() (bool, error) {
			if _, err := workerA.SyncNow(); err != nil {
				return false, fmt.Errorf("Machine A sync failed: %w", err)
			}
			if _, err := workerB.SyncNow(); err != nil {
				return false, fmt.Errorf("Machine B sync failed: %w", err)
			}
			if _, err := workerC.SyncNow(); err != nil {
				return false, fmt.Errorf("Machine C sync failed: %w", err)
			}

			jobsA, err := dbA.ListJobs("", "", 1000, 0)
			if err != nil {
				return false, fmt.Errorf("Machine A list jobs failed: %w", err)
			}
			jobsB, err := dbB.ListJobs("", "", 1000, 0)
			if err != nil {
				return false, fmt.Errorf("Machine B list jobs failed: %w", err)
			}
			jobsC, err := dbC.ListJobs("", "", 1000, 0)
			if err != nil {
				return false, fmt.Errorf("Machine C list jobs failed: %w", err)
			}
			return len(jobsA) >= expectedTotal &&
				len(jobsB) >= expectedTotal &&
				len(jobsC) >= expectedTotal, nil
		})

		env.assertPgCount("review_jobs", expectedTotal)

		// Verify each machine can see all jobs
		jobsA, err := dbA.ListJobs("", "", 1000, 0)
		if err != nil {
			require.Condition(t, func() bool {
				return false
			}, "Machine A: ListJobs failed: %v", err)
		}
		jobsB, err := dbB.ListJobs("", "", 1000, 0)
		if err != nil {
			require.Condition(t, func() bool {
				return false
			}, "Machine B: ListJobs failed: %v", err)
		}
		jobsC, err := dbC.ListJobs("", "", 1000, 0)
		if err != nil {
			require.Condition(t, func() bool {
				return false
			}, "Machine C: ListJobs failed: %v", err)
		}

		if len(jobsA) != expectedTotal {
			assert.Condition(t, func() bool {
				return false
			}, "Machine A should see %d jobs, got %d", expectedTotal, len(jobsA))
		}
		if len(jobsB) != expectedTotal {
			assert.Condition(t, func() bool {
				return false
			}, "Machine B should see %d jobs, got %d", expectedTotal, len(jobsB))
		}
		if len(jobsC) != expectedTotal {
			assert.Condition(t, func() bool {
				return false
			}, "Machine C should see %d jobs, got %d", expectedTotal, len(jobsC))
		}

		// Verify each machine has the others' specific jobs
		jobsAMap := make(map[string]bool)
		for _, j := range jobsA {
			jobsAMap[j.UUID] = true
		}
		jobsBMap := make(map[string]bool)
		for _, j := range jobsB {
			jobsBMap[j.UUID] = true
		}
		jobsCMap := make(map[string]bool)
		for _, j := range jobsC {
			jobsCMap[j.UUID] = true
		}

		for _, uuid := range jobsCreatedByB {
			if !jobsAMap[uuid] {
				assert.Condition(t, func() bool {
					return false
				}, "Machine A missing job %s created by B", uuid)
			}
		}
		for _, uuid := range jobsCreatedByC {
			if !jobsAMap[uuid] {
				assert.Condition(t, func() bool {
					return false
				}, "Machine A missing job %s created by C", uuid)
			}
		}
		for _, uuid := range jobsCreatedByA {
			if !jobsBMap[uuid] {
				assert.Condition(t, func() bool {
					return false
				}, "Machine B missing job %s created by A", uuid)
			}
		}
		for _, uuid := range jobsCreatedByC {
			if !jobsBMap[uuid] {
				assert.Condition(t, func() bool {
					return false
				}, "Machine B missing job %s created by C", uuid)
			}
		}
		for _, uuid := range jobsCreatedByA {
			if !jobsCMap[uuid] {
				assert.Condition(t, func() bool {
					return false
				}, "Machine C missing job %s created by A", uuid)
			}
		}
		for _, uuid := range jobsCreatedByB {
			if !jobsCMap[uuid] {
				assert.Condition(t, func() bool {
					return false
				}, "Machine C missing job %s created by B", uuid)
			}
		}
	})

	t.Logf("Realistic multiplayer test passed")
	t.Logf("  Machine A created: %d, Machine B created: %d, Machine C created: %d",
		len(jobsCreatedByA), len(jobsCreatedByB), len(jobsCreatedByC))
}

func TestIntegration_MultiplayerOfflineReconnect(t *testing.T) {
	env := newIntegrationEnv(t, 60*time.Second)

	dbA := env.openDB("machine_a.db")

	repoA, err := dbA.GetOrCreateRepo(filepath.Join(env.TmpDir, "repo_a"), "git@github.com:test/offline-repo.git")
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "Machine A: GetOrCreateRepo failed: %v", err)
	}
	createCompletedReview(t, dbA, repoA.ID, "dddd4444", "Dave", "Commit 1", "prompt", "Online review")

	// Start worker, sync, then stop (simulate going offline)
	cfgA := config.SyncConfig{
		Enabled:        true,
		PostgresURL:    env.pgURL,
		Interval:       "100ms",
		MachineName:    "machine-a",
		ConnectTimeout: "5s",
	}
	workerA := NewSyncWorker(dbA, cfgA)
	if err := workerA.Start(); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "Machine A: SyncWorker.Start failed: %v", err)
	}
	if err := waitForSyncWorkerConnection(workerA, 10*time.Second); err != nil {
		workerA.Stop()
		require.Condition(t, func() bool {
			return false
		}, "Machine A: Failed to connect: %v", err)
	}
	if _, err := workerA.SyncNow(); err != nil {
		workerA.Stop()
		require.Condition(t, func() bool {
			return false
		}, "Machine A: SyncNow failed: %v", err)
	}
	workerA.Stop()

	// Machine A creates more reviews while "offline"
	createCompletedReview(t, dbA, repoA.ID, "eeee5555", "Dave", "Commit 2", "prompt", "Offline review 1")
	createCompletedReview(t, dbA, repoA.ID, "ffff6666", "Dave", "Commit 3", "prompt", "Offline review 2")

	env.assertPgCount("review_jobs", 1)

	// Machine A reconnects
	workerA2 := startSyncWorker(t, dbA, env.pgURL, "machine-a", "100ms")
	if _, err := workerA2.SyncNow(); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "Machine A reconnect: SyncNow failed: %v", err)
	}

	env.assertPgCount("review_jobs", 3)

	// Machine B connects and should see all of Machine A's reviews
	dbB := env.openDB("machine_b.db")
	workerB := startSyncWorker(t, dbB, env.pgURL, "machine-b", "100ms")
	if _, err := workerB.SyncNow(); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "Machine B: SyncNow failed: %v", err)
	}

	env.waitForLocalJobs(t, dbB, 3, 10*time.Second)

	jobsB, err := dbB.ListJobs("", "", 100, 0)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "Machine B: ListJobs failed: %v", err)
	}
	if len(jobsB) != 3 {
		assert.Condition(t, func() bool {
			return false
		}, "Machine B should see all 3 jobs from Machine A, got %d", len(jobsB))
	}

	t.Log("Offline/reconnect verified: reviews created offline sync correctly after reconnect")
}

func TestIntegration_SyncNowPushesAllBatches(t *testing.T) {
	env := newIntegrationEnv(t, 30*time.Second)
	db := env.openDB("test.db")

	// Start sync worker FIRST, before creating jobs
	worker := startSyncWorker(t, db, env.pgURL, "", "1h")

	repo, err := db.GetOrCreateRepo("/test/batch-sync-repo", "batch-sync-test-identity")
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "Failed to create repo: %v", err)
	}

	numJobs := 80
	t.Logf("Creating %d jobs to test batch syncing", numJobs)
	createBatchReviews(t, db, repo.ID, numJobs, "commit", "Author", "Message")

	machineID, err := db.GetMachineID()
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "Failed to get machine ID: %v", err)
	}
	pendingJobs, err := db.GetJobsToSync(machineID, 1000)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "Failed to get pending jobs: %v", err)
	}
	t.Logf("Pending jobs before sync: %d", len(pendingJobs))
	if len(pendingJobs) < numJobs {
		require.Condition(t, func() bool {
			return false
		}, "Expected %d pending jobs, got %d", numJobs, len(pendingJobs))
	}

	t.Log("Calling SyncNow to push all batches")
	stats, err := worker.SyncNow()
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "SyncNow failed: %v", err)
	}

	t.Logf("SyncNow stats: pushed %d jobs, %d reviews, %d responses",
		stats.PushedJobs, stats.PushedReviews, stats.PushedResponses)

	if stats.PushedJobs < numJobs {
		assert.Condition(t, func() bool {
			return false
		}, "Expected SyncNow to push %d jobs, only pushed %d", numJobs, stats.PushedJobs)
	}
	if stats.PushedReviews < numJobs {
		assert.Condition(t, func() bool {
			return false
		}, "Expected SyncNow to push %d reviews, only pushed %d", numJobs, stats.PushedReviews)
	}

	var pgJobCount int
	if err := env.Pool.pool.QueryRow(env.Ctx, "SELECT COUNT(*) FROM roborev.review_jobs").Scan(&pgJobCount); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "Failed to count jobs in postgres: %v", err)
	}
	if pgJobCount < numJobs {
		assert.Condition(t, func() bool {
			return false
		}, "Expected %d jobs in postgres, got %d", numJobs, pgJobCount)
	}

	var pgReviewCount int
	if err := env.Pool.pool.QueryRow(env.Ctx, "SELECT COUNT(*) FROM roborev.reviews").Scan(&pgReviewCount); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "Failed to count reviews in postgres: %v", err)
	}
	if pgReviewCount < numJobs {
		assert.Condition(t, func() bool {
			return false
		}, "Expected %d reviews in postgres, got %d", numJobs, pgReviewCount)
	}

	pendingJobsAfter, err := db.GetJobsToSync(machineID, 1000)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "Failed to get pending jobs after sync: %v", err)
	}
	if len(pendingJobsAfter) > 0 {
		assert.Condition(t, func() bool {
			return false
		}, "Expected 0 pending jobs after sync, got %d", len(pendingJobsAfter))
	}

	t.Logf("Batch sync test passed: %d jobs and %d reviews pushed in batches of %d",
		stats.PushedJobs, stats.PushedReviews, syncBatchSize)
}

func TestIntegration_SyncNowWithProgressAbort(t *testing.T) {
	env := newIntegrationEnv(t, 30*time.Second)
	db := env.openDB("test.db")

	worker := startSyncWorker(t, db, env.pgURL, "", "1h")

	repo, err := db.GetOrCreateRepo(
		"/test/progress-abort-repo", "progress-abort-identity",
	)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "Failed to create repo: %v", err)
	}

	// Create enough jobs to require multiple push batches
	numJobs := 80
	createBatchReviews(t, db, repo.ID, numJobs, "abort", "Author", "Message")

	// progressFn returns false on first call to simulate
	// client disconnect; sync should abort early
	calls := 0
	stats, err := worker.SyncNowWithProgress(
		func(SyncProgress) bool {
			calls++
			return false
		},
	)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "SyncNowWithProgress failed: %v", err)
	}

	if calls != 1 {
		assert.Condition(t, func() bool {
			return false
		}, "Expected progressFn called once, got %d", calls)
	}

	// Should have pushed only one batch worth, not all jobs
	if stats.PushedJobs >= numJobs {
		assert.Condition(t, func() bool {
			return false
		}, "Expected partial push (abort after 1 batch), "+
			"but pushed %d/%d jobs",
			stats.PushedJobs, numJobs)
	}

	t.Logf("Progress abort test passed: pushed %d/%d jobs "+
		"before abort (1 callback call)",
		stats.PushedJobs, numJobs)
}

func TestIntegration_TickerSync(t *testing.T) {
	env := newIntegrationEnv(t, 30*time.Second)

	dbA := env.openDB("machine_a.db")
	dbB := env.openDB("machine_b.db")

	identity := "git@github.com:test/ticker-sync.git"

	repoA, err := dbA.GetOrCreateRepo(
		filepath.Join(env.TmpDir, "repo_a"), identity,
	)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "Machine A: GetOrCreateRepo failed: %v", err)
	}

	_, err = dbB.GetOrCreateRepo(
		filepath.Join(env.TmpDir, "repo_b"), identity,
	)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "Machine B: GetOrCreateRepo failed: %v", err)
	}

	// Start both workers using startSyncWorkerNoSync, which
	// waits for connection via HealthCheck without calling
	// SyncNow — no sync operations happen during startup.
	// skipInitialSync is set, so the only sync path is the
	// periodic ticker.
	startSyncWorkerNoSync(
		t, dbA, env.pgURL, "ticker-a", "200ms",
	)
	startSyncWorkerNoSync(
		t, dbB, env.pgURL, "ticker-b", "200ms",
	)

	// Create a review on machine A AFTER both workers are
	// connected and initial sync is skipped, so it can only
	// propagate via ticker ticks.
	createCompletedReview(
		t, dbA, repoA.ID,
		"tick1111", "Alice", "First ticker commit",
		"prompt", "First ticker review",
	)

	// Wait for machine B to pull machine A's review via ticker
	waitCondition(
		t, 10*time.Second,
		"Machine B pulls A's first review via ticker",
		func() (bool, error) {
			jobs, err := dbB.ListJobs("", "", 100, 0)
			if err != nil {
				return false, err
			}
			return len(jobs) >= 1, nil
		},
	)

	env.assertPgCount("review_jobs", 1)
	env.assertPgCount("reviews", 1)

	// Verify periodic behavior — create a second review and
	// confirm it propagates in a later interval.
	createCompletedReview(
		t, dbA, repoA.ID,
		"tick2222", "Alice", "Second ticker commit",
		"prompt", "Second ticker review",
	)

	waitCondition(
		t, 10*time.Second,
		"Machine B pulls A's second review via ticker",
		func() (bool, error) {
			jobs, err := dbB.ListJobs("", "", 100, 0)
			if err != nil {
				return false, err
			}
			return len(jobs) >= 2, nil
		},
	)

	env.assertPgCount("review_jobs", 2)
	env.assertPgCount("reviews", 2)
}
