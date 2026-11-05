package searchindex

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

// Adding UUIDs starts a new measurement series for the shared benchmarks.
func seedCanonicalReconcilerHistory(tb testing.TB, reviews, promptSize int) *storage.DB {
	tb.Helper()
	db := testutil.OpenTestDB(tb)
	_, err := db.ExecContext(tb.Context(), `
		BEGIN;
		INSERT INTO repos (id, root_path, name) VALUES (1, '/synthetic/repo', 'repo');
		INSERT INTO commits (id, repo_id, sha, author, subject, timestamp)
		VALUES (1, 1, 'abcdef1234567890', 'author', 'subject', CURRENT_TIMESTAMP);
		WITH RECURSIVE ids(id) AS (VALUES(1) UNION ALL SELECT id + 1 FROM ids WHERE id < :reviews)
		INSERT INTO review_jobs (id, repo_id, commit_id, git_ref, agent, status, job_type, uuid)
		SELECT id, 1, 1, 'abcdef1234567890', 'test', 'done', 'review',
		       printf('00000000-0000-4000-8000-%012d', id) FROM ids;
		INSERT INTO job_content (job_id, prompt)
		SELECT id, zstd_compress(printf('%*s', :prompt_size, 'prompt')) FROM review_jobs WHERE :prompt_size > 0;
		INSERT INTO reviews (id, job_id, agent, output, uuid)
		SELECT id, id, 'test', 'synthetic review output', uuid FROM review_jobs;
		INSERT INTO responses (job_id, responder, response)
		SELECT id, 'author', 'synthetic comment' FROM review_jobs WHERE id % 2 = 0;
		COMMIT;`, sql.Named("reviews", reviews), sql.Named("prompt_size", promptSize))
	require.NoError(tb, err)
	return db
}
