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
)

// seedPullHistory gives each cursor a distinct timestamp and every job a review.
func seedPullHistory(t *testing.T, pool *PgPool, n int) uuid.UUID {
	t.Helper()
	ctx := t.Context()
	machineID := uuid.New()
	require.NoError(t, pool.RegisterMachine(ctx, machineID, "remote"))
	repoID := createTestRepo(t, pool.Pool(), TestRepoOpts{})
	for i, query := range []string{
		`INSERT INTO review_jobs (uuid, repo_id, git_ref, agent, status, prompt, enqueued_at, updated_at, source_machine_id)
		 SELECT md5('job-' || i)::uuid, $2, 'main', 'test', 'done', 'review prompt',
		 '2026-01-01'::timestamptz + i * interval '1 second', '2026-01-01'::timestamptz + i * interval '1 second', $3
		 FROM generate_series(1, $1::int) i`,
		`INSERT INTO reviews (uuid, job_uuid, agent, prompt, output, structured_output, job_enqueued_at, job_source_machine_id, updated_by_machine_id, created_at, updated_at)
		 SELECT md5('review-' || i)::uuid, j.uuid, 'test', '', 'No issues found.', $2::jsonb, j.enqueued_at, $3, $3, j.updated_at, j.updated_at
		 FROM generate_series(1, $1::int) i JOIN review_jobs j ON j.uuid = md5('job-' || i)::uuid`,
		`INSERT INTO responses (uuid, job_uuid, responder, response, source_machine_id, created_at, inserted_at)
		 SELECT md5('response-' || i)::uuid, md5('job-' || i)::uuid, 'test', 'comment', $3, $2::timestamptz + i * interval '1 second', $2::timestamptz + i * interval '1 second'
		 FROM generate_series(1, $1::int) i`,
		`INSERT INTO experiment_definitions SELECT 'test-experiment', 'hash', '{}', $2::timestamptz, $3::uuid, NULL FROM generate_series(1, $1::int) LIMIT 1`,
		`INSERT INTO experiment_assignments (review_unit_kind, review_unit_uuid, experiment_id, arm, subject_hash, effective_config_hash, effective_config_json, assigned_at, inserted_at, source_machine_id)
		 SELECT 'job', md5('job-' || i), 'test-experiment', 'control', 'subject', 'config', '{}', $2::timestamptz, $2::timestamptz + i * interval '1 second', $3
		 FROM generate_series(1, $1::int) i`,
	} {
		var arg any = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		switch i {
		case 0:
			arg = repoID
		case 1:
			arg = string(reviewFixtureJSON("No issues found."))
		}
		_, err := pool.pool.Exec(ctx, query, n, arg, machineID)
		require.NoError(t, err)
	}
	_, err := pool.pool.Exec(ctx, "ANALYZE")
	require.NoError(t, err)
	return machineID
}

func TestIntegration_PullCursorPlans(t *testing.T) { //nolint:paralleltest // shares the PostgreSQL schema
	pool := newIntegrationEnv(t).Pool
	seedPullHistory(t, pool, 10000)
	ctx := t.Context()
	tx, err := pool.pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { require.NoError(t, tx.Rollback(context.WithoutCancel(ctx))) }()
	_, err = tx.Exec(ctx, "SET LOCAL plan_cache_mode = force_generic_plan")
	require.NoError(t, err)
	var cursorTime time.Time
	var cursorID int64
	require.NoError(t, tx.QueryRow(ctx, "SELECT updated_at, id FROM review_jobs ORDER BY updated_at, id OFFSET 1000 LIMIT 1").Scan(&cursorTime, &cursorID))
	for _, tc := range []struct{ name, query string }{
		{"jobs", pgPullJobsQuery},
		{"reviews", pgPullReviewsQuery},
		{"responses", pgPullResponsesQuery},
		{"assignments", pgPullExperimentAssignmentsQuery},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tx.Exec(ctx, "PREPARE pull_"+tc.name+" AS "+tc.query)
			require.NoError(t, err)
			args := fmt.Sprintf("'%s', '%s', %d, 100", uuid.New(), cursorTime.Format(time.RFC3339Nano), cursorID)
			var raw []byte
			require.NoError(t, tx.QueryRow(ctx, "EXPLAIN (ANALYZE, FORMAT JSON) EXECUTE pull_"+tc.name+"("+args+")").Scan(&raw))
			type node struct {
				Type  string  `json:"Node Type"`
				Rows  float64 `json:"Actual Rows"`
				Loops float64 `json:"Actual Loops"`
				Plans []node  `json:"Plans"`
			}
			var plans []struct{ Plan node }
			require.NoError(t, json.Unmarshal(raw, &plans))
			require.Len(t, plans, 1)
			var visit func(node)
			visit = func(n node) {
				assert.LessOrEqual(t, n.Rows*n.Loops, float64(500), "%s processed too many rows", n.Type)
				for _, child := range n.Plans {
					visit(child)
				}
			}
			visit(plans[0].Plan)
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
	assert.NotEqual(cursor, next)
}
