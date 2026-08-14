package webfixture

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
)

func TestSeedCreatesRepresentativeDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reviews.db")
	require.NoError(t, Seed(path))

	db, err := storage.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	assert := assert.New(t)
	assert.Equal(2, scalarInt(t, db.DB, `SELECT COUNT(*) FROM repos`))
	assert.GreaterOrEqual(scalarInt(t, db.DB, `SELECT COUNT(*) FROM review_jobs`), 50)
	assert.Equal(8, scalarInt(t, db.DB, `SELECT COUNT(DISTINCT status) FROM review_jobs`))
	assert.Equal(1, scalarInt(t, db.DB, `SELECT COUNT(*) FROM review_jobs WHERE job_type = 'compact'`))
	assert.Equal(1, scalarInt(t, db.DB, `SELECT COUNT(*) FROM review_jobs WHERE panel_role = 'synthesis'`))
	assert.Equal(2, scalarInt(t, db.DB, `SELECT COUNT(*) FROM review_jobs WHERE panel_role = 'member'`))
	assert.GreaterOrEqual(scalarInt(t, db.DB, `SELECT COUNT(*) FROM responses`), 2)
	assert.Equal(0, scalarInt(t, db.DB, `
		SELECT COUNT(*) FROM review_jobs
		WHERE status = 'canceled' AND finished_at IS NULL`))

	assert.Equal(1, scalarInt(t, db.DB, `
		SELECT COUNT(*) FROM review_jobs
		WHERE status IN ('done', 'failed', 'canceled', 'applied', 'rebased')
		  AND agent_invoked = 1
		  AND json_extract(token_usage, '$.has_cost') = 1
		  AND json_extract(token_usage, '$.cost_usd') > 0`))
	assert.Equal(1, scalarInt(t, db.DB, `
		SELECT COUNT(*) FROM review_jobs
		WHERE json_extract(token_usage, '$.has_cost') = 1
		  AND json_extract(token_usage, '$.cost_usd') = 0`))
	assert.Equal(1, scalarInt(t, db.DB, `
		SELECT COUNT(*) FROM review_jobs
		WHERE agent_invoked = 1 AND token_usage IS NULL`))
	cost, err := db.GetCostAggregate(storage.CostOptions{})
	require.NoError(t, err)
	assert.InEpsilon(1.25, cost.TotalUSD, 0.0001)
	assert.Equal(2, cost.JobsWithCost)
	assert.Equal(3, cost.JobsTotal)
	assert.False(cost.Complete)

	var queuedStatus string
	require.NoError(t, db.QueryRow(
		`SELECT status FROM review_jobs WHERE id = ?`, CancelJobID,
	).Scan(&queuedStatus))
	assert.Equal("queued", queuedStatus)

	var closed, verdict int
	require.NoError(t, db.QueryRow(`
		SELECT closed, verdict_bool FROM reviews WHERE job_id = ?`,
		CloseReviewJobID,
	).Scan(&closed, &verdict))
	assert.Equal(0, closed)
	assert.Equal(0, verdict)

	rows, err := db.Query(`SELECT root_path FROM repos ORDER BY id`)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rows.Close()) })
	var roots []string
	for rows.Next() {
		var root string
		require.NoError(t, rows.Scan(&root))
		roots = append(roots, root)
	}
	require.NoError(t, rows.Err())
	assert.Equal([]string{"/workspace/project-alpha", "/workspace/project-beta"}, roots)
}

func TestSeedRejectsExistingData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reviews.db")
	require.NoError(t, Seed(path))
	require.ErrorContains(t, Seed(path), "already contains fixture data")
}

func scalarInt(t *testing.T, db *sql.DB, query string) int {
	t.Helper()
	var value int
	require.NoError(t, db.QueryRow(query).Scan(&value))
	return value
}
