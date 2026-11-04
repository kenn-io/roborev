package searchindex

import (
	"context"
	"errors"
	"fmt"
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

func TestReconcilerPullWriteRefreshesOnlyPulledReview(t *testing.T) {
	var readCounts []int
	for _, reviews := range []int{1000, 10000} {
		t.Run(fmt.Sprintf("reviews=%d", reviews), func(t *testing.T) {
			db, r := seedReconcilerHistory(t, reviews, 0)
			store := &observedSearchStore{DB: db}
			r.store = store
			r.config.SweepInterval = time.Hour
			r.nextSweep = r.config.Now().Add(time.Hour)
			fullReads := store.fullReads.Load()
			jobID := int64(reviews + 1)
			_, err := db.ExecContext(t.Context(), `
				INSERT INTO review_jobs (id, repo_id, git_ref, agent, status, job_type)
				VALUES (?, 1, 'main', 'test', 'done', 'review');
				INSERT INTO reviews (job_id, agent, output)
				VALUES (?, 'test', 'pulleduniqueword');`, jobID, jobID)
			require.NoError(t, err)
			r.WakeChanges()
			for more := true; more; {
				more, err = r.reconcileTurn(t.Context())
				require.NoError(t, err)
			}
			assert.Equal(t, fullReads, store.fullReads.Load())
			assert.Equal(t, []int64{jobID}, store.jobReads)
			var got int64
			require.NoError(t, r.index.db.QueryRowContext(t.Context(), `SELECT job_id FROM review_mirror JOIN review_fts USING (doc_key) WHERE review_fts MATCH 'pulleduniqueword'`).Scan(&got))
			assert.Equal(t, jobID, got)
			readCounts = append(readCounts, len(store.jobReads))
		})
	}
	require.Len(t, readCounts, 2)
	assert.Equal(t, readCounts[0], readCounts[1])
}

func TestReconcilerPullWriteDuringFullScan(t *testing.T) {
	r, store, index := incrementalFixture(t, ReconcilerConfig{MirrorPageSize: 1, SweepInterval: time.Hour})
	more, err := r.reconcileTurn(t.Context())
	require.NoError(t, err)
	require.True(t, more)
	// The scan has already read this review, so only the log can refresh it.
	_, err = store.ExecContext(t.Context(), `UPDATE reviews SET output = 'pulledduringfullscan' WHERE job_id = 1`)
	require.NoError(t, err)
	r.WakeChanges()
	for more {
		more, err = r.reconcileTurn(t.Context())
		require.NoError(t, err)
	}
	assert.Equal(t, int64(3), store.fullReads.Load())
	assert.Equal(t, []int64{1}, store.jobReads)
	var key string
	require.NoError(t, index.db.QueryRowContext(t.Context(), `SELECT doc_key FROM review_fts WHERE review_fts MATCH 'pulledduringfullscan'`).Scan(&key))
	assert.Equal(t, "local:1", key)
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
		r, store, _ := incrementalFixture(t, ReconcilerConfig{SweepInterval: time.Minute})
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
		cancel()
		assert.ErrorIs(t, <-done, context.Canceled)
	})
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

func TestReconcilerSweepDrainsBoundedPages(t *testing.T) {
	t.Run("repo", func(t *testing.T) {
		now := time.Now()
		r, store, index := incrementalFixture(t, ReconcilerConfig{MirrorPageSize: 1, Now: func() time.Time { return now }})
		for more := true; more; {
			var err error
			more, err = r.reconcileTurn(t.Context())
			require.NoError(t, err)
		}
		assert.Equal(t, int64(3), store.fullReads.Load())
		result, err := store.ExecContext(t.Context(), `UPDATE repos SET name = 'renamed' WHERE id = 1`)
		require.NoError(t, err)
		changed, err := result.RowsAffected()
		require.NoError(t, err)
		require.Equal(t, int64(1), changed)
		now = now.Add(2 * r.config.SweepInterval)
		more, err := r.reconcileTurn(t.Context())
		require.NoError(t, err)
		require.True(t, more)
		assert.False(t, r.Health().MirrorComplete)
		assert.Nil(t, r.Health().MirrorBacklog)
		for more {
			before := len(store.jobReads)
			more, err = r.reconcileTurn(t.Context())
			require.NoError(t, err)
			assert.LessOrEqual(t, len(store.jobReads)-before, 1)
		}
		var name string
		require.NoError(t, index.db.QueryRowContext(t.Context(), `SELECT repo_name FROM review_mirror WHERE job_id = 1`).Scan(&name))
		assert.Equal(t, "renamed", name)
		assert.Equal(t, int64(3), store.fullReads.Load())
		assert.True(t, r.Health().MirrorComplete)
		require.NotNil(t, r.Health().MirrorBacklog)
		assert.Zero(t, *r.Health().MirrorBacklog)
	})
}
