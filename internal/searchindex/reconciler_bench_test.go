package searchindex

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
)

// Compare event refreshes with a full scan on synthetic history containing
// 8,000 reviews, 4,000 comments, and 500 MiB of stored prompts. Run with
// CGO_ENABLED=0 to measure the same SQLite driver used by release builds.
func BenchmarkReconcilerReviewEvent(b *testing.B) {
	// The shared test database helper takes *testing.T, so this benchmark
	// creates its own disposable canonical database.
	db, err := storage.Open(filepath.Join(b.TempDir(), "reviews.db"))
	require.NoError(b, err)
	b.Cleanup(func() { require.NoError(b, db.Close()) })
	_, err = db.Exec(`
		BEGIN;
		INSERT INTO repos (id, root_path, name) VALUES (1, '/synthetic/repo', 'repo');
		INSERT INTO commits (id, repo_id, sha, author, subject, timestamp)
		VALUES (1, 1, 'abcdef1234567890', 'author', 'subject', CURRENT_TIMESTAMP);
		WITH RECURSIVE ids(id) AS (VALUES(1) UNION ALL SELECT id + 1 FROM ids WHERE id < 8000)
		INSERT INTO review_jobs (id, repo_id, commit_id, git_ref, agent, status, job_type)
		SELECT id, 1, 1, 'abcdef1234567890', 'test', 'done', 'review' FROM ids;
		INSERT INTO job_content (job_id, prompt)
		SELECT id, zstd_compress(printf('%32768s', 'prompt')) FROM review_jobs;
		INSERT INTO reviews (id, job_id, agent, output)
		SELECT id, id, 'test', 'synthetic review output' FROM review_jobs;
		INSERT INTO responses (job_id, responder, response)
		SELECT id, 'author', 'synthetic comment' FROM review_jobs WHERE id % 2 = 0;
		COMMIT;`)
	require.NoError(b, err)
	index, err := Open(b.Context(), filepath.Join(b.TempDir(), "search.db"))
	require.NoError(b, err)
	b.Cleanup(func() { require.NoError(b, index.Close()) })
	r := NewReconciler(db, index, nil, ReconcilerConfig{})
	for more := true; more; {
		more, err = r.reconcileTurn(b.Context())
		require.NoError(b, err)
	}
	for _, mode := range []string{"full", "job"} {
		b.Run(mode, func(b *testing.B) {
			for b.Loop() {
				_, err := db.Exec(`UPDATE reviews SET closed = NOT closed WHERE job_id = 1`)
				require.NoError(b, err)
				if mode == "job" {
					r.WakeJob(1)
				} else {
					r.Wake()
				}
				for more := true; more; {
					more, err = r.reconcileTurn(b.Context())
					require.NoError(b, err)
				}
			}
		})
	}
}
