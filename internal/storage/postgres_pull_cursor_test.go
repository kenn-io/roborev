//go:build postgres

package storage

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
)

// seedPullHistory gives each cursor a distinct timestamp and every job a review.
func seedPullHistory(t *testing.T, pool *PgPool, n int) uuid.UUID {
	t.Helper()
	ctx := t.Context()
	machineID := uuid.New()
	require.NoError(t, pool.RegisterMachine(ctx, machineID, "remote"))
	repoID := createTestRepo(t, pool.Pool(), TestRepoOpts{})
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i, query := range []string{
		`INSERT INTO review_jobs (uuid, repo_id, git_ref, agent, status, prompt, enqueued_at, updated_at, source_machine_id)
		 SELECT md5('job-' || i)::uuid, $2, 'main', 'test', 'done', 'review prompt',
		 $4::timestamptz + i * interval '1 second', $4::timestamptz + i * interval '1 second', $3
		 FROM generate_series(1, $1::int) i`,
		`INSERT INTO reviews (uuid, job_uuid, agent, prompt, output, structured_output, job_enqueued_at, job_source_machine_id, updated_by_machine_id, created_at, updated_at)
		 SELECT md5('review-' || i)::uuid, j.uuid, 'test', '', 'No issues found.', $2::jsonb, j.enqueued_at, $3, $3, j.updated_at, j.updated_at
		 FROM generate_series(1, $1::int) i JOIN review_jobs j ON j.uuid = md5('job-' || i)::uuid`,
		`INSERT INTO responses (uuid, job_uuid, responder, response, source_machine_id, created_at, inserted_at)
		 SELECT md5('response-' || i)::uuid, md5('job-' || i)::uuid, 'test', 'comment', $3, $2::timestamptz + i * interval '1 second', $2::timestamptz + i * interval '1 second'
		 FROM generate_series(1, $1::int) i`,
		`INSERT INTO experiment_definitions SELECT 'test-experiment', 'hash', '{}', $2::timestamptz, $3::uuid, NULL FROM generate_series(1, $1::int) LIMIT 1`,
		`INSERT INTO experiment_assignments (review_unit_kind, review_unit_uuid, experiment_id, arm, subject_hash, effective_config_hash, effective_config_json, assigned_at, inserted_at, source_machine_id)
		 SELECT 'job', md5('job-' || i), 'test-experiment', 'default', 'subject', 'config', '{}', $2::timestamptz, $2::timestamptz + i * interval '1 second', $3
		 FROM generate_series(1, $1::int) i`,
	} {
		args := []any{n, base, machineID}
		switch i {
		case 0:
			args = []any{n, repoID, machineID, base}
		case 1:
			args = []any{n, string(reviewFixtureJSON("No issues found.")), machineID}
		}
		_, err := pool.pool.Exec(ctx, query, args...)
		require.NoError(t, err)
	}
	_, err := pool.pool.Exec(ctx, "ANALYZE")
	require.NoError(t, err)
	return machineID
}

func pullPlanWork(t *testing.T, raw []byte) (rows, work float64) {
	t.Helper()
	type node struct {
		Type         string  `json:"Node Type"`
		Rows         float64 `json:"Actual Rows"`
		Loops        float64 `json:"Actual Loops"`
		Filtered     float64 `json:"Rows Removed by Filter"`
		JoinFiltered float64 `json:"Rows Removed by Join Filter"`
		Rechecked    float64 `json:"Rows Removed by Index Recheck"`
		Plans        []node  `json:"Plans"`
	}
	var plans []struct{ Plan node }
	require.NoError(t, json.Unmarshal(raw, &plans))
	require.Len(t, plans, 1)
	var visit func(node)
	visit = func(n node) {
		work = max(work, (n.Rows+n.Filtered+n.JoinFiltered+n.Rechecked)*n.Loops)
		for _, child := range n.Plans {
			visit(child)
		}
	}
	visit(plans[0].Plan)
	return plans[0].Plan.Rows, work
}

func assertPullPlanBounded(t *testing.T, raw []byte) {
	t.Helper()
	rows, work := pullPlanWork(t, raw)
	assert.InDelta(t, 100, rows, 0, "probe must return a full page")
	assert.LessOrEqual(t, work, float64(500), "plan processed too many rows")
}

func TestIntegration_PullCursorPlans(t *testing.T) { //nolint:paralleltest // shares the PostgreSQL schema
	pool := newIntegrationEnv(t).Pool
	seedPullHistory(t, pool, 10000)
	ctx := t.Context()
	for _, query := range []string{
		`INSERT INTO commits (repo_id, sha, author, subject, timestamp) SELECT repo_id, uuid::text, 'test', 'commit', enqueued_at FROM review_jobs`,
		`UPDATE review_jobs j SET commit_id = c.id FROM commits c WHERE c.sha = j.uuid::text`,
		`ANALYZE`,
	} {
		_, err := pool.pool.Exec(ctx, query)
		require.NoError(t, err)
	}
	tx, err := pool.pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { require.NoError(t, tx.Rollback(context.WithoutCancel(ctx))) }()
	_, err = tx.Exec(ctx, "SET LOCAL plan_cache_mode = force_generic_plan")
	require.NoError(t, err)
	for _, tc := range []struct{ name, table, column, query string }{
		{"jobs", "review_jobs", "updated_at", pgPullJobsQuery},
		{"reviews", "reviews", "updated_at", pgPullReviewsQuery},
		{"responses", "responses", "inserted_at", pgPullResponsesQuery},
		{"assignments", "experiment_assignments", "inserted_at", pgPullExperimentAssignmentsQuery},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tx.Exec(ctx, "PREPARE pull_"+tc.name+" AS "+tc.query)
			require.NoError(t, err)
			var cursorTime time.Time
			var cursorID int64
			cursorQuery := fmt.Sprintf("SELECT %s, id FROM %s ORDER BY %s, id OFFSET 1000 LIMIT 1", tc.column, tc.table, tc.column)
			require.NoError(t, tx.QueryRow(ctx, cursorQuery).Scan(&cursorTime, &cursorID))
			args := fmt.Sprintf("'%s', '%s', %d, 100", uuid.New(), cursorTime.Format(time.RFC3339Nano), cursorID)
			var raw []byte
			require.NoError(t, tx.QueryRow(ctx, "EXPLAIN (ANALYZE, FORMAT JSON) EXECUTE pull_"+tc.name+"("+args+")").Scan(&raw))
			assertPullPlanBounded(t, raw)
		})
	}
}

func TestIntegration_PullReviewsPagesPastUnknownJobs(t *testing.T) { //nolint:paralleltest // shares the PostgreSQL schema
	assert := assert.New(t)
	pool := newIntegrationEnv(t).Pool
	seedPullHistory(t, pool, 8)
	ctx := t.Context()
	jobs, _, err := pool.PullJobs(ctx, uuid.New(), "", 100)
	require.NoError(t, err)
	require.Len(t, jobs, 8)
	known := map[uuid.UUID]struct{}{jobs[0].UUID: {}, jobs[6].UUID: {}}
	reviews, cursor, err := pool.PullReviews(ctx, uuid.New(), known, "", 2)
	require.NoError(t, err)
	require.Len(t, reviews, 2)
	assert.Equal(jobs[0].UUID, reviews[0].JobUUID)
	assert.Equal(jobs[6].UUID, reviews[1].JobUUID)
	var updated time.Time
	var id int64
	require.NoError(t, pool.pool.QueryRow(ctx, "SELECT updated_at, id FROM reviews WHERE uuid = $1", reviews[1].UUID).Scan(&updated, &id))
	assert.Equal(formatTimestampIDCursor(updated, id), cursor)

	// A short result still ends at the last known parent, before the tail.
	reviews, shortCursor, err := pool.PullReviews(ctx, uuid.New(), known, "", 3)
	require.NoError(t, err)
	assert.Len(reviews, 2)
	assert.Equal(cursor, shortCursor)
	reviews, unchanged, err := pool.PullReviews(ctx, uuid.New(), known, cursor, 2)
	require.NoError(t, err)
	assert.Empty(reviews)
	assert.Equal(cursor, unchanged)

	known[jobs[7].UUID] = struct{}{}
	reviews, next, err := pool.PullReviews(ctx, uuid.New(), known, cursor, 2)
	require.NoError(t, err)
	require.Len(t, reviews, 1)
	assert.Equal(jobs[7].UUID, reviews[0].JobUUID)
	require.NoError(t, pool.pool.QueryRow(ctx, "SELECT updated_at, id FROM reviews WHERE uuid = $1", reviews[0].UUID).Scan(&updated, &id))
	assert.Equal(formatTimestampIDCursor(updated, id), next)
}

func TestIntegration_PullJobsPagesTiedTimestamps(t *testing.T) { //nolint:paralleltest // shares the PostgreSQL schema
	assert := assert.New(t)
	pool := newIntegrationEnv(t).Pool
	seedPullHistory(t, pool, 8)
	ctx := t.Context()
	rows, err := pool.pool.Query(ctx, `SELECT uuid FROM review_jobs ORDER BY id`)
	require.NoError(t, err)
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		require.NoError(t, rows.Scan(&id))
		ids = append(ids, id)
	}
	require.NoError(t, rows.Err())
	rows.Close()
	require.Len(t, ids, 8)
	machineID := uuid.New()
	_, err = pool.pool.Exec(ctx, `UPDATE review_jobs SET updated_at = '2026-02-01'::timestamptz`)
	require.NoError(t, err)
	_, err = pool.pool.Exec(ctx, `UPDATE review_jobs SET source_machine_id = $1 WHERE uuid = $2`, machineID, ids[1])
	require.NoError(t, err)
	_, err = pool.pool.Exec(ctx, `INSERT INTO commits (repo_id, sha, author, subject, timestamp)
		SELECT repo_id, 'commit-sha', 'author', 'subject', enqueued_at FROM review_jobs WHERE uuid = $1`, ids[0])
	require.NoError(t, err)
	_, err = pool.pool.Exec(ctx, `UPDATE review_jobs SET commit_id = (SELECT id FROM commits WHERE sha = 'commit-sha') WHERE uuid = $1`, ids[0])
	require.NoError(t, err)

	var pulled []PulledJob
	cursor := ""
	for {
		page, next, err := pool.PullJobs(ctx, machineID, cursor, 3)
		require.NoError(t, err)
		pulled = append(pulled, page...)
		if len(page) < 3 {
			break
		}
		require.NotEqual(t, cursor, next)
		cursor = next
	}
	require.Len(t, pulled, 7)
	var got []uuid.UUID
	for _, job := range pulled {
		got = append(got, job.UUID)
	}
	assert.Equal([]uuid.UUID{ids[0], ids[2], ids[3], ids[4], ids[5], ids[6], ids[7]}, got)
	assert.Equal("commit-sha", pulled[0].CommitSHA)
	assert.Equal("author", pulled[0].CommitAuthor)
	assert.Empty(pulled[1].CommitSHA)
}

func TestIntegration_PullCachedPlansAfterPeerArrives(t *testing.T) { //nolint:paralleltest // shares the PostgreSQL schema
	pool := newIntegrationEnv(t).Pool
	owner := seedPullHistory(t, pool, 10000)
	ctx := t.Context()
	tx, err := pool.pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { require.NoError(t, tx.Rollback(context.WithoutCancel(ctx))) }()
	peer := uuid.New()
	for _, tc := range []struct{ name, table, query string }{
		{"responses", "responses", pgPullResponsesQuery},
		{"assignments", "experiment_assignments", pgPullExperimentAssignmentsQuery},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tx.Exec(ctx, "SET LOCAL plan_cache_mode = auto")
			require.NoError(t, err)
			_, err = tx.Exec(ctx, "PREPARE cached_"+tc.name+" AS "+tc.query)
			require.NoError(t, err)
			var cursorTime time.Time
			var cursorID int64
			require.NoError(t, tx.QueryRow(ctx, "SELECT inserted_at, id FROM "+tc.table+" ORDER BY inserted_at, id OFFSET 5100 LIMIT 1").Scan(&cursorTime, &cursorID))
			args := fmt.Sprintf("'%s', '%s', %d, 100", owner, cursorTime.Format(time.RFC3339Nano), cursorID)
			for range 6 {
				rows, err := tx.Query(ctx, "EXECUTE cached_"+tc.name+"("+args+")")
				require.NoError(t, err)
				hasRow := rows.Next()
				rows.Close()
				require.NoError(t, rows.Err())
				require.False(t, hasRow)
			}
			_, err = tx.Exec(ctx, "UPDATE "+tc.table+" SET source_machine_id = $1 WHERE id > (SELECT id FROM "+tc.table+" ORDER BY inserted_at, id OFFSET 5000 LIMIT 1)", peer)
			require.NoError(t, err)
			for _, mode := range []string{"auto", "force_generic_plan"} {
				t.Run(mode, func(t *testing.T) {
					_, err := tx.Exec(ctx, "SET LOCAL plan_cache_mode = "+mode)
					require.NoError(t, err)
					var raw []byte
					require.NoError(t, tx.QueryRow(ctx, "EXPLAIN (ANALYZE, FORMAT JSON) EXECUTE cached_"+tc.name+"("+args+")").Scan(&raw))
					assertPullPlanBounded(t, raw)
				})
			}
		})
	}
}

func TestIntegration_PullSeededAssignmentsIntoSQLite(t *testing.T) { //nolint:paralleltest // shares the PostgreSQL schema
	env := newIntegrationEnv(t)
	seedPullHistory(t, env.Pool, 8)
	db := env.openDB("pull.db")
	worker := NewSyncWorker(db, config.SyncConfig{})
	_, err := worker.pullChangesWithStats(t.Context(), env.Pool)
	require.NoError(t, err)
	var count int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM experiment_assignments WHERE arm = 'default'`).Scan(&count))
	assert.Equal(t, 8, count)
}
