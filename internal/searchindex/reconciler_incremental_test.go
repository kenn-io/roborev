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
	_, err := db.Exec(`
		INSERT INTO repos (id, root_path, name) VALUES (1, '/synthetic/repo', 'repo');
		INSERT INTO review_jobs (id, repo_id, git_ref, agent, status, job_type)
		VALUES (1, 1, 'main', 'test', 'done', 'review'),
		       (2, 1, 'main', 'test', 'done', 'review');
		INSERT INTO reviews (id, job_id, agent, prompt, output)
		VALUES (1, 1, 'test', '', 'first original'),
		       (2, 2, 'test', '', 'second original');`)
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
	require.NoError(t, index.db.QueryRow(`SELECT doc_key FROM review_fts WHERE review_fts MATCH 'incrementalcomment'`).Scan(&key))
	assert.Equal(t, "local:1", key)
	var closed bool
	require.NoError(t, index.db.QueryRow(`SELECT closed FROM review_mirror WHERE doc_key = 'local:1'`).Scan(&closed))
	assert.True(t, closed)
	assert.Equal(t, int64(2), r.Health().Indexed)

	// A sync-assigned UUID replaces the old local key, including its vectors.
	_, err = store.Exec(`UPDATE reviews SET uuid = '00000000-0000-4000-8000-000000000001' WHERE job_id = 1`)
	require.NoError(t, err)
	r.WakeJob(1)
	_, err = r.reconcileTurn(t.Context())
	require.NoError(t, err)
	for _, table := range []string{"review_mirror", "review_fts", "review_vectors_chunks", "review_vectors_stamps"} {
		assert.Zero(t, countRowsForDoc(t, index.db, table, "local:1"), table)
		assert.Equal(t, 1, countRowsForDoc(t, index.db, table, "local:2"), table)
		assert.Equal(t, 1, countRowsForDoc(t, index.db, table, "00000000-0000-4000-8000-000000000001"), table)
	}

	_, err = store.Exec(`DELETE FROM reviews WHERE job_id = 1`)
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
	_, err = store.Exec(`UPDATE reviews SET output = 'during scan' WHERE job_id = 1`)
	require.NoError(t, err)
	r.WakeJob(1)
	for range 4 {
		_, err = r.reconcileTurn(t.Context())
		require.NoError(t, err)
	}
	assert.Equal(t, []int64{1}, store.jobReads)
	var content string
	require.NoError(t, index.db.QueryRow(`SELECT content FROM review_mirror WHERE doc_key = 'local:1'`).Scan(&content))
	assert.Contains(t, content, "during scan")

	store.afterRead = func() {
		_, err := store.Exec(`UPDATE reviews SET output = 'during refresh' WHERE job_id = 1`)
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
	require.NoError(t, index.db.QueryRow(`SELECT content FROM review_mirror WHERE doc_key = 'local:1'`).Scan(&content))
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
		_, err := store.Exec(`UPDATE reviews SET output = 'sweptchange' WHERE job_id = 2`)
		require.NoError(t, err)
		for range 3 {
			time.Sleep(15 * time.Second)
			r.WakeJob(1)
			synctest.Wait()
			assert.Equal(t, int64(1), store.fullReads.Load())
		}
		time.Sleep(15 * time.Second)
		synctest.Wait()
		assert.Equal(t, int64(2), store.fullReads.Load())
		var key string
		require.NoError(t, index.db.QueryRow(`SELECT doc_key FROM review_fts WHERE review_fts MATCH 'sweptchange'`).Scan(&key))
		assert.Equal(t, "local:2", key)
		cancel()
		assert.ErrorIs(t, <-done, context.Canceled)
	})
}
