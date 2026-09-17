package searchindex

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/vector"
	"go.kenn.io/kit/vector/sqlitevec"

	"go.kenn.io/roborev/internal/searchdoc"
	"go.kenn.io/roborev/internal/storage"
)

func TestMirrorRefreshReplacesMirrorAndFTSInOneTransaction(t *testing.T) {
	ctx := context.Background()
	index, err := Open(ctx, filepath.Join(t.TempDir(), "reviews.search.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, index.Close()) })

	// A constraint on the replacement FTS-shaped table gives the second
	// write a deterministic failure after the mirror upsert has run.
	_, err = index.db.ExecContext(ctx, `
		DROP TABLE review_fts;
		CREATE TABLE review_fts (
			doc_key TEXT,
			content TEXT CHECK (instr(content, 'reject') = 0),
			identifiers TEXT
		);`)
	require.NoError(t, err)

	original := testDocument(1, "accepted content")
	seen := map[string]struct{}{}
	changed, err := index.RefreshMirrorPage(ctx, []searchdoc.Document{original}, seen)
	require.NoError(t, err)
	assert.Equal(t, 1, changed)
	assert.Contains(t, seen, original.DocKey)

	changed, err = index.RefreshMirrorPage(ctx, []searchdoc.Document{original}, seen)
	require.NoError(t, err)
	assert.Zero(t, changed)

	replacement := testDocument(1, "reject replacement")
	_, err = index.RefreshMirrorPage(ctx, []searchdoc.Document{replacement}, seen)
	require.Error(t, err)

	var mirrorContent, ftsContent string
	err = index.db.QueryRowContext(ctx,
		`SELECT content FROM review_mirror WHERE doc_key = ?`, original.DocKey).Scan(&mirrorContent)
	require.NoError(t, err)
	err = index.db.QueryRowContext(ctx,
		`SELECT content FROM review_fts WHERE doc_key = ?`, original.DocKey).Scan(&ftsContent)
	require.NoError(t, err)
	assert.Equal(t, original.Content, mirrorContent)
	assert.Equal(t, original.Content, ftsContent)
}

func TestMirrorRefreshUpdatesLocalMetadataWithoutInvalidatingVectors(t *testing.T) {
	ctx := context.Background()
	index, err := Open(ctx, filepath.Join(t.TempDir(), "reviews.search.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, index.Close()) })

	doc := testDocument(1, "stable content")
	_, err = index.RefreshMirrorPage(ctx, []searchdoc.Document{doc}, nil)
	require.NoError(t, err)
	require.NoError(t, index.vectors.EnsureGeneration(ctx, "gen-1",
		vector.Generation{Model: "test", Dimensions: 2}, sqlitevec.StateActive))
	pending, err := index.vectors.PendingForGeneration(ctx, "gen-1", 10)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	require.NoError(t, index.vectors.SaveVectors(ctx, "gen-1", doc.DocKey, pending[0].Revision,
		[]vector.ChunkVector{{ChunkIndex: 0, Vector: vector.Vector{1, 0}}}))

	updated := doc
	updated.Source.Branch = "release"
	updated.Identifiers += "\nrelease"
	changed, err := index.RefreshMirrorPage(ctx, []searchdoc.Document{updated}, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, changed)

	pending, err = index.vectors.PendingForGeneration(ctx, "gen-1", 10)
	require.NoError(t, err)
	assert.Empty(t, pending)
	var branch, embedGen string
	err = index.db.QueryRowContext(ctx,
		`SELECT branch, embed_gen FROM review_mirror WHERE doc_key = ?`, doc.DocKey).Scan(&branch, &embedGen)
	require.NoError(t, err)
	assert.Equal(t, "release", branch)
	assert.Equal(t, "gen-1", embedGen)
	var ftsIdentifiers string
	err = index.db.QueryRowContext(ctx,
		`SELECT identifiers FROM review_fts WHERE doc_key = ?`, doc.DocKey).Scan(&ftsIdentifiers)
	require.NoError(t, err)
	assert.Equal(t, updated.Identifiers, ftsIdentifiers)
}

func TestMirrorDeleteMissingRemovesFTSAndEveryVectorGeneration(t *testing.T) {
	ctx := context.Background()
	index, err := Open(ctx, filepath.Join(t.TempDir(), "reviews.search.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, index.Close()) })

	docs := []searchdoc.Document{testDocument(1, "first"), testDocument(2, "second")}
	_, err = index.RefreshMirrorPage(ctx, docs, nil)
	require.NoError(t, err)
	for _, generation := range []string{"gen-1", "gen-2"} {
		require.NoError(t, index.vectors.EnsureGeneration(ctx, generation,
			vector.Generation{Model: generation, Dimensions: 2}, sqlitevec.StateActive))
		pending, pendingErr := index.vectors.PendingForGeneration(ctx, generation, 10)
		require.NoError(t, pendingErr)
		for _, item := range pending {
			require.NoError(t, index.vectors.SaveVectors(ctx, generation, item.Doc, item.Revision,
				[]vector.ChunkVector{{ChunkIndex: 0, Vector: vector.Vector{1, 0}}}))
		}
	}

	seen := map[string]struct{}{docs[1].DocKey: {}}
	changed, err := index.DeleteMissing(ctx, seen)
	require.NoError(t, err)
	assert.Equal(t, 1, changed)

	wantKept := map[string]int{
		"review_mirror":         1,
		"review_fts":            1,
		"review_vectors_chunks": 2,
		"review_vectors_stamps": 2,
	}
	for table, want := range wantKept {
		assert.Equal(t, 0, countRowsForDoc(t, index.db, table, docs[0].DocKey), table)
		assert.Equal(t, want, countRowsForDoc(t, index.db, table, docs[1].DocKey), table)
	}
	for _, ordinal := range []int{1, 2} {
		var vectors int
		err = index.db.QueryRowContext(ctx,
			fmt.Sprintf(`SELECT count(*) FROM review_vectors_v%d`, ordinal)).Scan(&vectors)
		require.NoError(t, err)
		assert.Equal(t, 1, vectors)
	}

	changed, err = index.DeleteMissing(ctx, seen)
	require.NoError(t, err)
	assert.Zero(t, changed)
}

func testDocument(id int64, output string) searchdoc.Document {
	return searchdoc.Render(storage.SearchReviewSource{
		ReviewID:   id,
		JobID:      100 + id,
		RepoID:     7,
		ReviewUUID: fmt.Sprintf("00000000-0000-4000-8000-%012d", id),
		JobUUID:    fmt.Sprintf("10000000-0000-4000-8000-%012d", id),
		RepoName:   "example/repo",
		Branch:     "main",
		GitRef:     "HEAD",
		CommitSHA:  fmt.Sprintf("%040d", id),
		ReviewType: "commit",
		PanelRole:  "primary",
		Agent:      "test",
		Verdict:    "pass",
		FinishedAt: time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC),
		Output:     output,
	})
}

func countRowsForDoc(t *testing.T, db *sql.DB, table, docKey string) int {
	t.Helper()
	var count int
	err := db.QueryRowContext(context.Background(),
		fmt.Sprintf(`SELECT count(*) FROM %s WHERE doc_key = ?`, table), docKey).Scan(&count)
	require.NoError(t, err)
	return count
}
