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
	for name, imported := range map[string]int{"local": 0, "shared pages then misses": 3*sharedLookupPage + 4} {
		t.Run(name, func(t *testing.T) {
			index := openGenerationTestIndex(t)
			recorder := recordFillCursors(t, index)
			sources := makeSearchSources(imported + 12)
			embedder := &reconcilerEmbedder{space: testSpace("model", 2), batchSize: 1}
			r := NewReconciler(&reconcilerStore{sources: sources}, index, embedder, ReconcilerConfig{})
			if imported > 0 {
				shared := newMemorySharedVectors()
				space, err := sharedSpace(embedder.space)
				require.NoError(t, err)
				shared.rows[space] = make(map[storage.SearchVectorKey][]storage.SearchVectorChunk)
				for _, source := range sources[:imported] {
					doc := searchdoc.Render(source)
					shared.rows[space][sharedKey(source.ReviewUUID, doc.ContentHash)] = []storage.SearchVectorChunk{{Index: 0, Vector: []float32{1, 0}}}
				}
				r.ShareVectors(shared)
			}
			for {
				more, err := r.refreshMirror(t.Context())
				require.NoError(t, err)
				if !more {
					break
				}
			}
			wantCursors := []string{"", searchdoc.Render(sources[imported+3]).DocKey, searchdoc.Render(sources[imported+7]).DocKey}
			if imported > 0 {
				wantCursors[0] = searchdoc.Render(sources[3*sharedLookupPage-1]).DocKey
				wantCursors = append([]string{
					"",
					searchdoc.Render(sources[sharedLookupPage-1]).DocKey,
					searchdoc.Render(sources[2*sharedLookupPage-1]).DocKey,
				}, wantCursors...)
			}
			for turn, wantAfter := range wantCursors {
				recorder.after = nil
				_, err := r.reconcileTurn(t.Context())
				require.NoError(t, err)
				assert.Equal(t, []string{wantAfter}, recorder.after)
				if imported > 0 && turn < 3 {
					assert.Empty(t, embedder.calls, "shared pages skip the provider")
					assert.Equal(t, int64((turn+1)*sharedLookupPage), r.Health().Embedded, "each turn imports one shared page")
				}
			}
			assert.Zero(t, r.Health().EmbeddingBacklog)
			assert.Equal(t, kitKey(t, embedder.space), r.Health().ActiveGeneration)
			if imported > 0 {
				sources[0].Output = "edited shared review"
				r.WakeJob(sources[0].JobID)
				for range 2 {
					_, err := r.reconcileTurn(t.Context())
					require.NoError(t, err)
				}
				assert.Len(t, embedder.calls, 13, "the edited shared review is embedded after the cursor wraps")
				assert.Zero(t, r.Health().EmbeddingBacklog)
			}
		})
	}
}

func TestReconcilerFillCursorRetriesAndWraps(t *testing.T) {
	ctx := t.Context()
	index := openGenerationTestIndex(t)
	recorder := recordFillCursors(t, index)
	store := &reconcilerStore{sources: makeSearchSources(12)}
	embedder := &reconcilerEmbedder{space: testSpace("model", 2), batchSize: 1}
	r := NewReconciler(store, index, embedder, ReconcilerConfig{})
	_, err := r.reconcileTurn(ctx)
	require.NoError(t, err)
	providerErr := errors.New("provider unavailable")
	embedder.embed = func(context.Context, []string) ([][]float32, error) { return nil, providerErr }
	_, err = r.reconcileTurn(ctx)
	require.ErrorIs(t, err, providerErr)
	embedder.embed = nil
	recorder.after = nil
	_, err = r.reconcileTurn(ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{searchdoc.Render(store.sources[3]).DocKey}, recorder.after, "the retry resumes before the unfinished reviews")
	assert.Equal(t, int64(8), r.Health().Embedded)

	store.sources[0].Output = "edited review"
	r.WakeJob(store.sources[0].JobID)
	for range 3 {
		_, err = r.reconcileTurn(ctx)
		require.NoError(t, err)
	}
	assert.Zero(t, r.Health().EmbeddingBacklog, "the edited review behind the cursor is re-embedded after the scan wraps")
	assert.Equal(t, kitKey(t, embedder.space), r.Health().ActiveGeneration)
}
