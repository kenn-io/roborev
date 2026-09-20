package searchindex

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/vector"
	"go.kenn.io/kit/vector/sqlitevec"

	"go.kenn.io/roborev/internal/searchdoc"
)

func TestSidecarCapabilitiesAndPath(t *testing.T) {
	assert.Equal(t, "/data/reviews.search.db", PathFor("/data/reviews.db"))
	assert.Equal(t, "/data/reviews.sqlite.search.db", PathFor("/data/reviews.sqlite"))

	ctx := context.Background()
	index, err := Open(ctx, filepath.Join(t.TempDir(), "reviews.search.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, index.Close()) })

	_, err = index.db.ExecContext(ctx, `CREATE VIRTUAL TABLE capability_fts USING fts5(body)`)
	require.NoError(t, err)
	_, err = index.db.ExecContext(ctx, `INSERT INTO capability_fts(body) VALUES ('portable lexical search')`)
	require.NoError(t, err)
	var lexical string
	err = index.db.QueryRowContext(ctx,
		`SELECT body FROM capability_fts WHERE capability_fts MATCH 'lexical'`).Scan(&lexical)
	require.NoError(t, err)
	assert.Equal(t, "portable lexical search", lexical)

	_, err = index.db.ExecContext(ctx, `CREATE VIRTUAL TABLE capability_vec USING vec0(embedding float[2])`)
	require.NoError(t, err)
	_, err = index.db.ExecContext(ctx,
		`INSERT INTO capability_vec(rowid, embedding) VALUES (1, vec_f32(?))`, `[1,0]`)
	require.NoError(t, err)
	var rowID int64
	err = index.db.QueryRowContext(ctx, `
		SELECT rowid FROM capability_vec
		WHERE embedding MATCH vec_f32(?) ORDER BY distance LIMIT 1`, `[1,0]`).Scan(&rowID)
	require.NoError(t, err)
	assert.Equal(t, int64(1), rowID)
}

func TestFTSAndVectorSmokeUsesContentHashRevision(t *testing.T) {
	ctx := context.Background()
	index, err := Open(ctx, filepath.Join(t.TempDir(), "reviews.search.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, index.Close()) })

	doc := testDocument(1, "alpha search term")
	changed, err := index.RefreshMirrorPage(ctx, []searchdoc.Document{doc}, map[string]struct{}{})
	require.NoError(t, err)
	assert.Equal(t, 1, changed)

	var ftsKey string
	err = index.db.QueryRowContext(ctx,
		`SELECT doc_key FROM review_fts WHERE review_fts MATCH 'alpha'`).Scan(&ftsKey)
	require.NoError(t, err)
	assert.Equal(t, doc.DocKey, ftsKey)

	require.NoError(t, index.vectors.EnsureGeneration(ctx, "gen-1",
		vector.Generation{Model: "test", Dimensions: 2}, sqlitevec.StateActive))
	pending, err := index.vectors.PendingForGeneration(ctx, "gen-1", 10)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, doc.ContentHash, pending[0].Revision)
	require.NoError(t, index.vectors.SaveVectors(ctx, "gen-1", doc.DocKey, pending[0].Revision,
		[]vector.ChunkVector{{ChunkIndex: 0, Vector: vector.Vector{1, 0}}}))

	hits, err := index.vectors.QueryGeneration(ctx, "gen-1", vector.Vector{1, 0}, 10)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	assert.Equal(t, doc.DocKey, hits[0].Doc)

	updated := testDocument(1, "beta replacement")
	_, err = index.RefreshMirrorPage(ctx, []searchdoc.Document{updated}, map[string]struct{}{})
	require.NoError(t, err)
	err = index.vectors.SaveVectors(ctx, "gen-1", doc.DocKey, pending[0].Revision,
		[]vector.ChunkVector{{ChunkIndex: 0, Vector: vector.Vector{1, 0}}})
	require.ErrorIs(t, err, vector.ErrStale)

	hits, err = index.vectors.QueryGeneration(ctx, "gen-1", vector.Vector{1, 0}, 10)
	require.NoError(t, err)
	assert.Empty(t, hits)
	pending, err = index.vectors.PendingForGeneration(ctx, "gen-1", 10)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, updated.ContentHash, pending[0].Revision)
}

func TestRecoveryRecreatesSchemaMismatchAndCleansJournalFiles(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "reviews.search.db")
	index, err := Open(ctx, path)
	require.NoError(t, err)
	require.NoError(t, index.Close())

	db, err := sql.Open(sidecarDriver, path)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `
		UPDATE search_meta SET value = 'old' WHERE key = 'schema_version';
		CREATE TABLE obsolete_sidecar_data(value TEXT);`)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	staleJournal := []byte("stale journal")
	require.NoError(t, os.WriteFile(path+"-wal", staleJournal, 0o600))
	require.NoError(t, os.WriteFile(path+"-shm", staleJournal, 0o600))

	index, err = Open(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, index.Close()) })

	var version string
	err = index.db.QueryRowContext(ctx,
		`SELECT value FROM search_meta WHERE key = 'schema_version'`).Scan(&version)
	require.NoError(t, err)
	assert.Equal(t, sidecarSchemaVersion, version)

	var obsolete int
	err = index.db.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE name = 'obsolete_sidecar_data'`).Scan(&obsolete)
	require.NoError(t, err)
	assert.Zero(t, obsolete)
	assertJournalMarkerRemoved(t, path+"-wal", staleJournal)
	assertJournalMarkerRemoved(t, path+"-shm", staleJournal)
}

func TestRecoveryRecreatesStructurallyIncompleteSidecar(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "reviews.search.db")
	index, err := Open(ctx, path)
	require.NoError(t, err)
	require.NoError(t, index.Close())

	db, err := sql.Open(sidecarDriver, path)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `
		DROP TABLE review_mirror;
		CREATE TABLE obsolete_sidecar_data(value TEXT);`)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	index, err = Open(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, index.Close()) })

	var obsolete int
	err = index.db.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE name = 'obsolete_sidecar_data'`).Scan(&obsolete)
	require.NoError(t, err)
	assert.Zero(t, obsolete)
}

func TestRecoveryRecreatesSidecarWhenFTSHasOrdinaryTableShape(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "reviews.search.db")
	index, err := Open(ctx, path)
	require.NoError(t, err)
	require.NoError(t, index.Close())

	db, err := sql.Open(sidecarDriver, path)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `
		DROP TABLE review_fts;
		CREATE TABLE review_fts(doc_key TEXT, content TEXT, identifiers TEXT);
		CREATE TABLE obsolete_sidecar_data(value TEXT);`)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	index, err = Open(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, index.Close()) })

	var obsolete int
	err = index.db.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE name = 'obsolete_sidecar_data'`).Scan(&obsolete)
	require.NoError(t, err)
	assert.Zero(t, obsolete)
}

func TestRecoveryRecreatesCorruptSidecarWithoutTouchingCanonicalDatabase(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	canonicalPath := filepath.Join(dir, "reviews.db")
	canonical := []byte("canonical review data")
	require.NoError(t, os.WriteFile(canonicalPath, canonical, 0o600))

	path := PathFor(canonicalPath)
	require.NoError(t, os.WriteFile(path, []byte("not a sqlite database"), 0o600))
	index, err := Open(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, index.Close()) })

	gotCanonical, err := os.ReadFile(canonicalPath)
	require.NoError(t, err)
	assert.Equal(t, canonical, gotCanonical)
	var mirrorTable int
	err = index.db.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE name = 'review_mirror'`).Scan(&mirrorTable)
	require.NoError(t, err)
	assert.Equal(t, 1, mirrorTable)
}

func TestRecoveryPreservesFilesForMissingRuntimeModule(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "reviews.search.db")
	original := map[string][]byte{
		path:          []byte("valid sidecar"),
		path + "-wal": []byte("valid wal"),
		path + "-shm": []byte("valid shm"),
	}
	for name, contents := range original {
		require.NoError(t, os.WriteFile(name, contents, 0o600))
	}

	calls := 0
	index, err := openWithRecovery(ctx, path, func(context.Context, string) (*Index, error) {
		calls++
		return nil, &CapabilityError{Module: "vec0", Err: errors.New("no such module: vec0")}
	})
	require.Error(t, err)
	assert.Nil(t, index)
	assert.Equal(t, 1, calls)
	var capabilityErr *CapabilityError
	require.ErrorAs(t, err, &capabilityErr)
	assert.Equal(t, "vec0", capabilityErr.Module)
	for name, want := range original {
		got, readErr := os.ReadFile(name)
		require.NoError(t, readErr)
		assert.Equal(t, want, got)
	}
}

func TestRecoveryPreservesFilesForOperationalSchemaValidationError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reviews.search.db")
	index, err := Open(context.Background(), path)
	require.NoError(t, err)
	require.NoError(t, index.Close())

	mainContents, err := os.ReadFile(path)
	require.NoError(t, err)
	original := map[string][]byte{
		path:          mainContents,
		path + "-wal": []byte("valid wal"),
		path + "-shm": []byte("valid shm"),
	}
	require.NoError(t, os.WriteFile(path+"-wal", original[path+"-wal"], 0o600))
	require.NoError(t, os.WriteFile(path+"-shm", original[path+"-shm"], 0o600))

	db, err := sql.Open(sidecarDriver, path)
	require.NoError(t, err)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	validationErr := validateSchema(canceled, db)
	require.NoError(t, db.Close())
	require.ErrorIs(t, validationErr, context.Canceled)

	calls := 0
	index, err = openWithRecovery(context.Background(), path, func(context.Context, string) (*Index, error) {
		calls++
		return nil, validationErr
	})
	require.Error(t, err)
	assert.Nil(t, index)
	assert.Equal(t, 1, calls)
	for name, want := range original {
		got, readErr := os.ReadFile(name)
		require.NoError(t, readErr)
		assert.Equal(t, want, got)
	}
}

func TestRecoveryRecreatesSameColumnTablesMissingRequiredKeys(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "reviews.search.db")
	index, err := Open(ctx, path)
	require.NoError(t, err)
	require.NoError(t, index.Close())

	db, err := sql.Open(sidecarDriver, path)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `
		DROP TABLE review_mirror;
		CREATE TABLE review_mirror (
		  doc_key TEXT,
		  review_id INTEGER NOT NULL,
		  review_uuid TEXT,
		  job_id INTEGER NOT NULL,
		  job_uuid TEXT,
		  group_key TEXT NOT NULL,
		  repo_id INTEGER NOT NULL,
		  repo_name TEXT NOT NULL,
		  branch TEXT,
		  git_ref TEXT NOT NULL,
		  commit_sha TEXT,
		  finished_at TEXT,
		  verdict TEXT,
		  closed INTEGER NOT NULL,
		  panel_role TEXT,
		  content TEXT NOT NULL,
		  content_hash TEXT NOT NULL,
		  embed_gen TEXT
		);
		CREATE TABLE obsolete_sidecar_data(value TEXT);`)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	index, err = Open(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, index.Close()) })
	assertSidecarWasRebuilt(t, index.db)
}

func assertSidecarWasRebuilt(t *testing.T, db *sql.DB) {
	t.Helper()
	var obsolete int
	err := db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM sqlite_master WHERE name = 'obsolete_sidecar_data'`).Scan(&obsolete)
	require.NoError(t, err)
	assert.Zero(t, obsolete)
}

func assertJournalMarkerRemoved(t *testing.T, path string, marker []byte) {
	t.Helper()
	contents, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	require.NoError(t, err)
	assert.NotEqual(t, marker, contents)
}
