package searchindex

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

type observedSearchStore struct {
	*storage.DB
	fullReads atomic.Int64
	jobReads  []int64
	afterRead func()
	readErr   error
}

func (s *observedSearchStore) ListSearchDocuments(ctx context.Context, after int64, limit int) ([]storage.SearchReviewSource, error) {
	s.fullReads.Add(1)
	return s.DB.ListSearchDocuments(ctx, after, limit)
}

func (s *observedSearchStore) GetSearchDocumentForJob(ctx context.Context, id int64) (*storage.SearchReviewSource, error) {
	s.jobReads = append(s.jobReads, id)
	if s.readErr != nil {
		return nil, s.readErr
	}
	source, err := s.DB.GetSearchDocumentForJob(ctx, id)
	if s.afterRead != nil {
		s.afterRead()
	}
	return source, err
}

func incrementalFixture(t *testing.T, config ReconcilerConfig) (*Reconciler, *observedSearchStore, *Index) {
	t.Helper()
	db := testutil.OpenTestDB(t)
	_, err := db.ExecContext(t.Context(), `
		INSERT INTO repos (id, root_path, name) VALUES (1, '/synthetic/repo', 'repo');
		INSERT INTO review_jobs (id, repo_id, git_ref, agent, status, job_type)
		VALUES (1, 1, 'main', 'test', 'done', 'review'),
		       (2, 1, 'main', 'test', 'done', 'review');
		INSERT INTO reviews (id, job_id, agent, output)
		VALUES (1, 1, 'test', 'first original'),
		       (2, 2, 'test', 'second original');`)
	require.NoError(t, err)
	store := &observedSearchStore{DB: db}
	index := openGenerationTestIndex(t)
	return NewReconciler(store, index, nil, config), store, index
}

func TestReconcilerJobUpdatesDoNotReadUnrelatedHistory(t *testing.T) {
	r, store, index := incrementalFixture(t, ReconcilerConfig{})
	_, err := r.reconcileTurn(t.Context())
	require.NoError(t, err)
	r.embedder = &reconcilerEmbedder{space: testSpace("test", 2), batchSize: 2}
	_, err = r.reconcileTurn(t.Context())
	require.NoError(t, err)

	_, err = store.AddCommentToJob(1, "author", "incrementalcomment")
	require.NoError(t, err)
	require.NoError(t, store.MarkReviewClosedByJobID(1, true))
	for range 10 {
		r.WakeJob(1)
	}
	_, err = r.reconcileTurn(t.Context())
	require.NoError(t, err)
	assert.Equal(t, int64(1), store.fullReads.Load())
	assert.Equal(t, []int64{1}, store.jobReads)
	var key string
	require.NoError(t, index.db.QueryRowContext(t.Context(), `SELECT doc_key FROM review_fts WHERE review_fts MATCH 'incrementalcomment'`).Scan(&key))
	assert.Equal(t, "local:1", key)
	var closed bool
	require.NoError(t, index.db.QueryRowContext(t.Context(), `SELECT closed FROM review_mirror WHERE doc_key = 'local:1'`).Scan(&closed))
	assert.True(t, closed)
	assert.Equal(t, int64(2), r.Health().Indexed)

	// A sync-assigned UUID replaces the old local key, including its vectors.
	_, err = store.ExecContext(t.Context(), `UPDATE reviews SET uuid = '00000000-0000-4000-8000-000000000001' WHERE job_id = 1`)
	require.NoError(t, err)
	r.WakeJob(1)
	_, err = r.reconcileTurn(t.Context())
	require.NoError(t, err)
	for _, table := range []string{"review_mirror", "review_fts", "review_vectors_chunks", "review_vectors_stamps"} {
		assert.Zero(t, countRowsForDoc(t, index.db, table, "local:1"), table)
		assert.Equal(t, 1, countRowsForDoc(t, index.db, table, "local:2"), table)
		assert.Equal(t, 1, countRowsForDoc(t, index.db, table, "00000000-0000-4000-8000-000000000001"), table)
	}

	_, err = store.ExecContext(t.Context(), `DELETE FROM reviews WHERE job_id = 1`)
	require.NoError(t, err)
	r.WakeJob(1)
	_, err = r.reconcileTurn(t.Context())
	require.NoError(t, err)
	for _, table := range []string{"review_mirror", "review_fts", "review_vectors_chunks", "review_vectors_stamps"} {
		assert.Zero(t, countRowsForDoc(t, index.db, table, "00000000-0000-4000-8000-000000000001"), table)
		assert.Equal(t, 1, countRowsForDoc(t, index.db, table, "local:2"), table)
	}
	assert.Equal(t, int64(1), r.Health().Indexed)
	assert.Equal(t, int64(1), store.fullReads.Load())
}

func TestReconcilerKeepsJobWakesDuringScansAndRefreshes(t *testing.T) {
	r, store, index := incrementalFixture(t, ReconcilerConfig{MirrorPageSize: 1})
	more, err := r.reconcileTurn(t.Context())
	require.NoError(t, err)
	require.True(t, more)
	_, err = store.ExecContext(t.Context(), `UPDATE reviews SET output = 'during scan' WHERE job_id = 1`)
	require.NoError(t, err)
	r.WakeJob(1)
	for range 4 {
		_, err = r.reconcileTurn(t.Context())
		require.NoError(t, err)
	}
	assert.Equal(t, []int64{1}, store.jobReads)
	var content string
	require.NoError(t, index.db.QueryRowContext(t.Context(), `SELECT content FROM review_mirror WHERE doc_key = 'local:1'`).Scan(&content))
	assert.Contains(t, content, "during scan")

	store.afterRead = func() {
		_, err := store.ExecContext(t.Context(), `UPDATE reviews SET output = 'during refresh' WHERE job_id = 1`)
		require.NoError(t, err)
		r.WakeJob(1)
		store.afterRead = nil
	}
	r.WakeJob(1)
	more, err = r.reconcileTurn(t.Context())
	require.NoError(t, err)
	assert.True(t, more)
	_, err = r.reconcileTurn(t.Context())
	require.NoError(t, err)
	require.NoError(t, index.db.QueryRowContext(t.Context(), `SELECT content FROM review_mirror WHERE doc_key = 'local:1'`).Scan(&content))
	assert.Contains(t, content, "during refresh")
	assert.Equal(t, int64(3), store.fullReads.Load())

	store.readErr = errors.New("canonical read failed")
	r.WakeJob(2)
	_, err = r.reconcileTurn(t.Context())
	require.ErrorIs(t, err, store.readErr)
	store.readErr = nil
	_, err = r.reconcileTurn(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []int64{1, 1, 1, 2, 2}, store.jobReads)
}

func TestReconcilerJobWakesDoNotPostponeSafetySweep(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r, store, index := incrementalFixture(t, ReconcilerConfig{SweepInterval: time.Minute})
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- r.Run(ctx) }()
		synctest.Wait()
		assert.Equal(t, int64(1), store.fullReads.Load())
		// A change without an event must still be found by the safety sweep.
		_, err := store.ExecContext(t.Context(), `UPDATE reviews SET output = 'sweptchange' WHERE job_id = 2`)
		require.NoError(t, err)
		for range 3 {
			time.Sleep(15 * time.Second)
			r.WakeJob(1)
			synctest.Wait()
			assert.Equal(t, int64(1), store.fullReads.Load())
		}
		time.Sleep(15 * time.Second)
		synctest.Wait()
		assert.Equal(t, int64(1), store.fullReads.Load())
		assert.Contains(t, store.jobReads, int64(2))
		var key string
		require.NoError(t, index.db.QueryRowContext(t.Context(), `SELECT doc_key FROM review_fts WHERE review_fts MATCH 'sweptchange'`).Scan(&key))
		assert.Equal(t, "local:2", key)
		cancel()
		assert.ErrorIs(t, <-done, context.Canceled)
	})
}

func TestReconcilerSweepFindsChangesWithoutEvents(t *testing.T) {
	cases := []struct {
		name    string
		prepare string
		change  string
		query   string
		want    string
	}{
		{"repo rename", "", "", `SELECT repo_name FROM review_mirror WHERE job_id = 1`, "renamed"},
		{"review delete", "", `DELETE FROM reviews WHERE job_id = 1`, `SELECT CAST(count(*) AS TEXT) FROM review_fts WHERE doc_key = 'local:1'`, "0"},
		{"uuid assignment", "", `UPDATE reviews SET uuid = '00000000-0000-4000-8000-000000000001' WHERE job_id = 1`, `SELECT doc_key FROM review_fts WHERE review_fts MATCH 'first'`, "00000000-0000-4000-8000-000000000001"},
		{"commit comment", `INSERT INTO commits (id, repo_id, sha, author, subject, timestamp) VALUES (1, 1, 'main', 'author', 'subject', CURRENT_TIMESTAMP); UPDATE review_jobs SET commit_id = 1 WHERE id = 1`, `INSERT INTO responses (commit_id, responder, response) VALUES (1, 'author', 'commitcomment')`, `SELECT doc_key FROM review_fts WHERE review_fts MATCH 'commitcomment' AND doc_key = 'local:1'`, "local:1"},
		{"legacy diff clear", `UPDATE review_jobs SET job_type = '' WHERE id = 1; INSERT INTO job_content (job_id, diff_content) VALUES (1, 'diff')`, `UPDATE job_content SET diff_content = NULL WHERE job_id = 1`, `SELECT CAST(count(*) AS TEXT) FROM review_mirror WHERE job_id = 1`, "0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now()
			r, store, index := incrementalFixture(t, ReconcilerConfig{Now: func() time.Time { return now }})
			if tc.prepare != "" {
				_, err := store.ExecContext(t.Context(), tc.prepare)
				require.NoError(t, err)
			}
			_, err := r.reconcileTurn(t.Context())
			require.NoError(t, err)
			if tc.name == "repo rename" {
				_, err = store.RenameRepo("/synthetic/repo", "renamed")
			} else {
				_, err = store.ExecContext(t.Context(), tc.change)
			}
			require.NoError(t, err)
			now = now.Add(2 * r.config.SweepInterval)
			for more := true; more; {
				more, err = r.reconcileTurn(t.Context())
				require.NoError(t, err)
			}
			var got string
			require.NoError(t, index.db.QueryRowContext(t.Context(), tc.query).Scan(&got))
			assert.Equal(t, tc.want, got)
			assert.Equal(t, int64(1), store.fullReads.Load())
		})
	}
}

func TestReconcilerSweepPreservesChangesDuringScan(t *testing.T) {
	now := time.Now()
	r, store, index := incrementalFixture(t, ReconcilerConfig{MirrorPageSize: 1, Now: func() time.Time { return now }})
	_, err := r.reconcileTurn(t.Context())
	require.NoError(t, err)
	_, err = store.ExecContext(t.Context(), `UPDATE reviews SET output = 'duringscan' WHERE job_id = 1`)
	require.NoError(t, err)
	for more := true; more; {
		more, err = r.reconcileTurn(t.Context())
		require.NoError(t, err)
	}
	now = now.Add(2 * r.config.SweepInterval)
	for more := true; more; {
		more, err = r.reconcileTurn(t.Context())
		require.NoError(t, err)
	}
	var key string
	require.NoError(t, index.db.QueryRowContext(t.Context(), `SELECT doc_key FROM review_fts WHERE review_fts MATCH 'duringscan'`).Scan(&key))
	assert.Equal(t, "local:1", key)
	assert.Equal(t, int64(3), store.fullReads.Load())
}

func TestReconcilerSweepDrainsChangesWithNoMatchingJobs(t *testing.T) {
	now := time.Now()
	r, store, index := incrementalFixture(t, ReconcilerConfig{MirrorPageSize: 1, Now: func() time.Time { return now }})
	for more := true; more; {
		var err error
		more, err = r.reconcileTurn(t.Context())
		require.NoError(t, err)
	}
	_, err := store.ExecContext(t.Context(), `
		INSERT INTO repos (id, root_path, name) VALUES (2, '/synthetic/empty', 'empty');
		UPDATE repos SET name = 'renamed' WHERE id = 2;
		UPDATE reviews SET output = 'afterempty' WHERE job_id = 1`)
	require.NoError(t, err)
	now = now.Add(2 * r.config.SweepInterval)
	for more := true; more; {
		more, err = r.reconcileTurn(t.Context())
		require.NoError(t, err)
	}
	var key string
	require.NoError(t, index.db.QueryRowContext(t.Context(), `SELECT doc_key FROM review_fts WHERE review_fts MATCH 'afterempty'`).Scan(&key))
	assert.Equal(t, "local:1", key)
	head, err := store.SearchChangesHead(t.Context())
	require.NoError(t, err)
	assert.Zero(t, head)
}

func TestReconcilerSweepReportsPendingMirrorChanges(t *testing.T) {
	now := time.Now()
	r, store, _ := incrementalFixture(t, ReconcilerConfig{MirrorPageSize: 1, Now: func() time.Time { return now }})
	for more := true; more; {
		var err error
		more, err = r.reconcileTurn(t.Context())
		require.NoError(t, err)
	}
	_, err := store.ExecContext(t.Context(), `UPDATE reviews SET output = 'changed'`)
	require.NoError(t, err)
	now = now.Add(2 * r.config.SweepInterval)
	more, err := r.reconcileTurn(t.Context())
	require.NoError(t, err)
	require.True(t, more)
	assert.False(t, r.Health().MirrorComplete)
	assert.Nil(t, r.Health().MirrorBacklog)
	for more {
		more, err = r.reconcileTurn(t.Context())
		require.NoError(t, err)
	}
	assert.True(t, r.Health().MirrorComplete)
	require.NotNil(t, r.Health().MirrorBacklog)
	assert.Zero(t, *r.Health().MirrorBacklog)
}

func TestReconcilerSweepBoundsRepoAndCommitFanout(t *testing.T) {
	for _, tc := range []struct{ name, setup, change string }{
		{"repo", "", `UPDATE repos SET name = 'renamed' WHERE id = 1`},
		{"commit", `INSERT INTO commits (id, repo_id, sha, author, subject, timestamp) VALUES (1, 1, 'main', 'author', 'subject', CURRENT_TIMESTAMP); UPDATE review_jobs SET commit_id = 1`, `INSERT INTO responses (commit_id, responder, response) VALUES (1, 'author', 'fanoutcomment')`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now()
			r, store, index := incrementalFixture(t, ReconcilerConfig{MirrorPageSize: 1, Now: func() time.Time { return now }})
			if tc.setup != "" {
				_, err := store.ExecContext(t.Context(), tc.setup)
				require.NoError(t, err)
			}
			for more := true; more; {
				var err error
				more, err = r.reconcileTurn(t.Context())
				require.NoError(t, err)
			}
			_, err := store.ExecContext(t.Context(), tc.change)
			require.NoError(t, err)
			now = now.Add(2 * r.config.SweepInterval)
			for more := true; more; {
				before := len(store.jobReads)
				more, err = r.reconcileTurn(t.Context())
				require.NoError(t, err)
				assert.LessOrEqual(t, len(store.jobReads)-before, 1)
			}
			assert.ElementsMatch(t, []int64{1, 2}, store.jobReads)
			var count int
			if tc.name == "repo" {
				err = index.db.QueryRowContext(t.Context(), `SELECT count(*) FROM review_mirror WHERE repo_name = 'renamed'`).Scan(&count)
			} else {
				err = index.db.QueryRowContext(t.Context(), `SELECT count(*) FROM review_fts WHERE review_fts MATCH 'fanoutcomment'`).Scan(&count)
			}
			require.NoError(t, err)
			assert.Equal(t, 2, count)
		})
	}
}
