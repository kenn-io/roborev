package searchindex

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

// Compare event refreshes with a full scan on 8,000 reviews and 4,000 comments.
// The full scan includes 500 MiB of prompts. Run with CGO_ENABLED=0 to measure
// the same SQLite driver used by release builds.
func BenchmarkReconcilerReviewEvent(b *testing.B) {
	for _, mode := range []string{"full", "job"} {
		b.Run(mode, func(b *testing.B) {
			promptSize := 0
			if mode == "full" {
				promptSize = 65536
			}
			db, r := seedReconcilerHistory(b, 8000, promptSize)
			benchmarkReconcilerReviewEvent(b, db, r, mode == "job")
		})
	}
}

func BenchmarkReconcilerReviewEventHistory(b *testing.B) {
	for _, reviews := range []int{1000, 4000, 16000} {
		b.Run(fmt.Sprintf("reviews=%d", reviews), func(b *testing.B) {
			db, r := seedReconcilerHistory(b, reviews, 0)
			benchmarkReconcilerReviewEvent(b, db, r, true)
		})
	}
}

func benchmarkReconcilerReviewEvent(b *testing.B, db *storage.DB, r *Reconciler, job bool) {
	b.Helper()
	for b.Loop() {
		_, err := db.ExecContext(b.Context(), `UPDATE reviews SET closed = NOT closed WHERE job_id = 1`)
		require.NoError(b, err)
		if job {
			r.WakeJob(1)
		} else {
			r.Wake()
		}
		for more := true; more; {
			more, err = r.reconcileTurn(b.Context())
			require.NoError(b, err)
		}
	}
}

func seedReconcilerHistory(b *testing.B, reviews, promptSize int) (*storage.DB, *Reconciler) {
	b.Helper()
	db := testutil.OpenTestDB(b)
	_, err := db.ExecContext(b.Context(), `
		BEGIN;
		INSERT INTO repos (id, root_path, name) VALUES (1, '/synthetic/repo', 'repo');
		INSERT INTO commits (id, repo_id, sha, author, subject, timestamp)
		VALUES (1, 1, 'abcdef1234567890', 'author', 'subject', CURRENT_TIMESTAMP);
		WITH RECURSIVE ids(id) AS (VALUES(1) UNION ALL SELECT id + 1 FROM ids WHERE id < :reviews)
		INSERT INTO review_jobs (id, repo_id, commit_id, git_ref, agent, status, job_type)
		SELECT id, 1, 1, 'abcdef1234567890', 'test', 'done', 'review' FROM ids;
		INSERT INTO job_content (job_id, prompt)
		SELECT id, zstd_compress(printf('%*s', :prompt_size, 'prompt')) FROM review_jobs WHERE :prompt_size > 0;
		INSERT INTO reviews (id, job_id, agent, output)
		SELECT id, id, 'test', 'synthetic review output' FROM review_jobs;
		INSERT INTO responses (job_id, responder, response)
		SELECT id, 'author', 'synthetic comment' FROM review_jobs WHERE id % 2 = 0;
		COMMIT;`, sql.Named("reviews", reviews), sql.Named("prompt_size", promptSize))
	require.NoError(b, err)
	index, err := Open(b.Context(), filepath.Join(b.TempDir(), "search.db"))
	require.NoError(b, err)
	b.Cleanup(func() { require.NoError(b, index.Close()) })
	r := NewReconciler(db, index, nil, ReconcilerConfig{})
	for more := true; more; {
		more, err = r.reconcileTurn(b.Context())
		require.NoError(b, err)
	}
	return db, r
}
