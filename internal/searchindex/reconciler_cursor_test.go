package searchindex

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/vector/sqlitevec"

	"go.kenn.io/roborev/internal/searchdoc"
	"go.kenn.io/roborev/internal/storage"
)

// cursorConnector records bound cursors while executing the real SQLite queries.
type cursorConnector struct {
	base  driver.Driver
	path  string
	after []string
}

func (c *cursorConnector) Connect(context.Context) (driver.Conn, error) {
	conn, err := c.base.Open(c.path)
	if err != nil {
		return nil, err
	}
	return &cursorConn{Conn: conn, recorder: c}, nil
}

func (c *cursorConnector) Driver() driver.Driver { return c.base }

type cursorConn struct {
	driver.Conn
	recorder *cursorConnector
}

func (c *cursorConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	return c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
}

func (c *cursorConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if strings.Contains(query, "FROM review_mirror d") && strings.Contains(query, "WHERE NOT") {
		after := ""
		if len(args) == 3 {
			after = args[1].Value.(string)
		}
		c.recorder.after = append(c.recorder.after, after)
	}
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
}

func recordFillCursors(t *testing.T, index *Index) *cursorConnector {
	t.Helper()
	var path string
	require.NoError(t, index.db.QueryRowContext(t.Context(), "SELECT file FROM pragma_database_list WHERE name = 'main'").Scan(&path))
	recorder := &cursorConnector{base: index.db.Driver(), path: path}
	db := sql.OpenDB(recorder)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	var err error
	index.vectors, err = sqlitevec.New[string, string](t.Context(), db, sqlitevec.Schema{
		DocsTable: "review_mirror", IDColumn: "doc_key", ContentColumn: "content",
		EmbedGenColumn: "embed_gen", VectorsPrefix: "review_vectors", RevisionColumn: "content_hash",
	})
	require.NoError(t, err)
	return recorder
}

func TestReconcilerResumesFillAfterPreviousTurn(t *testing.T) {
	for _, tc := range []struct {
		name     string
		sharing  bool
		imported int
	}{
		{name: "local"},
		{name: "shared misses", sharing: true},
		{name: "shared page then misses", sharing: true, imported: 256},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			index := openGenerationTestIndex(t)
			recorder := recordFillCursors(t, index)
			sources := makeSearchSources(tc.imported + 12)
			embedder := &reconcilerEmbedder{space: testSpace("model", 2), batchSize: 1}
			r := NewReconciler(&reconcilerStore{sources: sources}, index, embedder, ReconcilerConfig{})
			if tc.sharing {
				shared := newMemorySharedVectors()
				space, err := sharedSpace(embedder.space)
				require.NoError(t, err)
				shared.rows[space] = make(map[storage.SearchVectorKey][]storage.SearchVectorChunk)
				for _, source := range sources[:tc.imported] {
					doc := searchdoc.Render(source)
					shared.rows[space][sharedKey(source.ReviewUUID, doc.ContentHash)] = []storage.SearchVectorChunk{{Index: 0, Vector: []float32{1, 0}}}
				}
				r.ShareVectors(shared)
			}
			for turn, wantAfter := range []string{"", searchdoc.Render(sources[tc.imported+3]).DocKey, searchdoc.Render(sources[tc.imported+7]).DocKey} {
				recorder.after = nil
				_, err := r.reconcileTurn(t.Context())
				require.NoError(t, err)
				wantScans := []string{wantAfter}
				if turn == 0 && tc.imported > 0 {
					wantScans = append(wantScans, searchdoc.Render(sources[tc.imported-1]).DocKey)
				}
				assert.Equal(wantScans, recorder.after)
				assert.Equal(int64(8-4*turn), r.Health().EmbeddingBacklog)
			}
			assert.Equal(12, embedder.callCount())
			assert.Equal(int64(tc.imported+12), r.Health().Embedded)
			assert.Equal(kitKey(t, embedder.space), r.Health().ActiveGeneration)
		})
	}
}

func TestReconcilerFillCursorRetriesAndWraps(t *testing.T) {
	assert := assert.New(t)
	ctx := t.Context()
	index := openGenerationTestIndex(t)
	recorder := recordFillCursors(t, index)
	store := &reconcilerStore{sources: makeSearchSources(13)}
	embedder := &reconcilerEmbedder{space: testSpace("model", 2), batchSize: 1}
	r := NewReconciler(store, index, embedder, ReconcilerConfig{})
	_, err := r.reconcileTurn(ctx)
	require.NoError(t, err)
	fourth := searchdoc.Render(store.sources[3]).DocKey
	providerErr := errors.New("provider unavailable")
	calls := 0
	embedder.embed = func(context.Context, []string) ([][]float32, error) {
		calls++
		if calls == 2 {
			return nil, providerErr
		}
		return [][]float32{{1, 0}}, nil
	}
	recorder.after = nil
	_, err = r.reconcileTurn(ctx)
	require.ErrorIs(t, err, providerErr)
	assert.Equal([]string{fourth}, recorder.after)
	assert.Equal(int64(5), r.Health().Embedded)
	embedder.embed = nil
	recorder.after = nil
	_, err = r.reconcileTurn(ctx)
	require.NoError(t, err)
	assert.Equal([]string{fourth}, recorder.after, "retry starts before the unfinished documents")
	assert.Equal(int64(9), r.Health().Embedded)

	store.sources[0].Output = "edited review"
	r.WakeJob(store.sources[0].JobID)
	_, err = r.reconcileTurn(ctx)
	require.NoError(t, err)
	assert.Equal(int64(1), r.Health().EmbeddingBacklog, "the edited review is behind the cursor")
	assert.Empty(r.Health().ActiveGeneration)
	// Exhaust the scan, then wrap to pick up the edited review.
	for range 2 {
		_, err = r.reconcileTurn(ctx)
		require.NoError(t, err)
	}
	assert.Zero(r.Health().EmbeddingBacklog)
	assert.Equal(kitKey(t, embedder.space), r.Health().ActiveGeneration)
	assert.Equal(15, embedder.callCount(), "one failed call and one re-embedding, with all thirteen reviews covered")

	embedder.space = testSpace("replacement", 2)
	recorder.after = nil
	_, err = r.reconcileTurn(ctx)
	require.NoError(t, err)
	assert.Equal([]string{""}, recorder.after, "a replacement generation starts at the first review")
	assert.Equal(int64(4), r.Health().Embedded)
	assert.Equal(int64(9), r.Health().EmbeddingBacklog)
}

func TestReconcilerFillCursorAllowsKitRestart(t *testing.T) {
	assert := assert.New(t)
	index := openGenerationTestIndex(t)
	recorder := recordFillCursors(t, index)
	store := &reconcilerStore{sources: makeSearchSources(7)}
	embedder := &reconcilerEmbedder{space: testSpace("model", 2), batchSize: 1}
	r := NewReconciler(store, index, embedder, ReconcilerConfig{})
	_, err := r.reconcileTurn(t.Context())
	require.NoError(t, err)
	store.sources[0].Output = "edited review"
	r.WakeJob(store.sources[0].JobID)
	recorder.after = nil
	_, err = r.reconcileTurn(t.Context())
	require.NoError(t, err)
	assert.Equal([]string{searchdoc.Render(store.sources[3]).DocKey, searchdoc.Render(store.sources[6]).DocKey, ""}, recorder.after)
	assert.Zero(r.Health().EmbeddingBacklog)
	assert.Equal(8, embedder.callCount())
	assert.Equal(kitKey(t, embedder.space), r.Health().ActiveGeneration)
}
