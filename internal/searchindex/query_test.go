package searchindex

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/vector"

	"go.kenn.io/roborev/internal/searchdoc"
	"go.kenn.io/roborev/internal/storage"
)

func TestLiteralFTSQueryQuotesEveryWhitespaceDelimitedSegment(t *testing.T) {
	tests := map[string]string{
		"foo/bar.go:42":        `"foo/bar.go:42"`,
		"retry-safe":           `"retry-safe"`,
		`say "hello"`:          `"say" """hello"""`,
		"OR NEAR NOT":          `"OR" "NEAR" "NOT"`,
		"foo ---":              `"foo" "---"`,
		"  alpha\t beta\n ":    `"alpha" "beta"`,
		"--- / ::: ":           "",
		"\t\n ":                "",
		"界/面.go:42 retry-safe": `"界/面.go:42" "retry-safe"`,
	}
	for input, want := range tests {
		t.Run(fmt.Sprintf("%q", input), func(t *testing.T) {
			assert.Equal(t, want, literalFTSQuery(input))
		})
	}
}

func TestCandidateTargetHasFloorMultiplierAndCeiling(t *testing.T) {
	assert.Equal(t, 50, candidateTarget(1))
	assert.Equal(t, 60, candidateTarget(20))
	assert.Equal(t, 200, candidateTarget(100))
	assert.Equal(t, 200, candidateTarget(1000))
}

func TestSearchLexicalTreatsPathsHyphensQuotesAndKeywordsLiterally(t *testing.T) {
	ctx := context.Background()
	index := openQueryTestIndex(t)
	docs := []searchdoc.Document{
		queryTestDocument(1, "panel-path", "Finding: high foo/bar.go:42\nProblem: retry-safe parser failed", queryDocOptions{}),
		queryTestDocument(2, "panel-quote", `Problem: say "hello" before returning`, queryDocOptions{}),
		queryTestDocument(3, "panel-keyword", "Problem: OR NEAR NOT are ordinary words", queryDocOptions{}),
	}
	_, err := index.RefreshMirrorPage(ctx, docs, nil)
	require.NoError(t, err)

	for query, wantDoc := range map[string]string{
		"foo/bar.go:42": docs[0].DocKey,
		"retry-safe":    docs[0].DocKey,
		`"hello"`:       docs[1].DocKey,
		"OR NEAR NOT":   docs[2].DocKey,
	} {
		t.Run(query, func(t *testing.T) {
			hits, err := index.SearchLexical(ctx, query, SearchFilters{}, 20)
			require.NoError(t, err)
			require.NotEmpty(t, hits)
			assert.Equal(t, wantDoc, hits[0].DocKey)
		})
	}

	for _, query := range []string{"", " \t\n ", "--- / :::"} {
		hits, err := index.SearchLexical(ctx, query, SearchFilters{}, 20)
		require.NoError(t, err)
		assert.Empty(t, hits)
	}
}

func TestSearchLexicalRanksIdentifiersAndSHAPrefixesBeforeBM25(t *testing.T) {
	ctx := context.Background()
	index := openQueryTestIndex(t)
	shaA := "abcdef0123456789abcdef0123456789abcdef01"
	shaB := "abcdef0999999999999999999999999999999999"
	docs := []searchdoc.Document{
		queryTestDocument(1, "group-bm25", strings.Repeat("abcdef0123456789abcdef0123456789abcdef01 ", 8), queryDocOptions{
			commitSHA: "1111111111111111111111111111111111111111",
			finished:  time.Date(2026, 9, 15, 14, 0, 0, 0, time.UTC),
		}),
		queryTestDocument(2, "group-a", "ordinary review prose", queryDocOptions{
			commitSHA: shaA,
			finished:  time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC),
		}),
		queryTestDocument(3, "group-b", "ordinary review prose", queryDocOptions{
			commitSHA: shaB,
			finished:  time.Date(2026, 9, 15, 13, 0, 0, 0, time.UTC),
		}),
	}
	_, err := index.RefreshMirrorPage(ctx, docs, nil)
	require.NoError(t, err)

	full, err := index.SearchLexical(ctx, shaA, SearchFilters{}, 10)
	require.NoError(t, err)
	require.NotEmpty(t, full)
	assert.Equal(t, docs[1].DocKey, full[0].DocKey)
	assert.Contains(t, full[0].MatchedIn, MatchIdentifier)

	unique, err := index.SearchLexical(ctx, "abcdef01", SearchFilters{}, 10)
	require.NoError(t, err)
	require.Len(t, unique, 1)
	assert.Equal(t, docs[1].DocKey, unique[0].DocKey)

	ambiguous, err := index.SearchLexical(ctx, "abcdef0", SearchFilters{}, 10)
	require.NoError(t, err)
	require.Len(t, ambiguous, 2)
	assert.Equal(t, []string{docs[2].DocKey, docs[1].DocKey}, []string{ambiguous[0].DocKey, ambiguous[1].DocKey})
	assert.ElementsMatch(t, []string{MatchIdentifier}, ambiguous[0].MatchedIn)
}

func TestSearchLexicalAppliesEveryMirrorFilterBeforeLimit(t *testing.T) {
	ctx := context.Background()
	index := openQueryTestIndex(t)
	cutoff := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	docs := make([]searchdoc.Document, 0, 61)
	for i := range 60 {
		docs = append(docs, queryTestDocument(int64(i+1), fmt.Sprintf("excluded-%d", i), "needle needle needle", queryDocOptions{
			repoID:   99,
			branch:   "other",
			verdict:  "fail",
			closed:   true,
			finished: cutoff.Add(-time.Hour),
		}))
	}
	wanted := queryTestDocument(100, "wanted", "needle", queryDocOptions{
		repoID:   7,
		branch:   "feature/search",
		verdict:  "pass",
		finished: cutoff.Add(time.Hour),
	})
	docs = append(docs, wanted)
	_, err := index.RefreshMirrorPage(ctx, docs, nil)
	require.NoError(t, err)

	hits, err := index.SearchLexical(ctx, "needle", SearchFilters{
		RepoID: 7, Branch: "feature/search", Since: &cutoff, Verdict: "pass", State: StateOpen,
	}, 1)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	assert.Equal(t, wanted.DocKey, hits[0].DocKey)
}

func TestQueryWithProbeReturnsOneHitBeyondTheDeepCeiling(t *testing.T) {
	ctx := context.Background()
	index := openQueryTestIndex(t)
	doc := queryTestDocument(1, "probe-group", "semantic chunks", queryDocOptions{})
	_, err := index.RefreshMirrorPage(ctx, []searchdoc.Document{doc}, nil)
	require.NoError(t, err)
	model := vector.Generation{Model: "probe", Dimensions: 2}
	key, err := index.EnsureGeneration(ctx, model)
	require.NoError(t, err)
	pending, err := index.PendingGeneration(ctx, key, 1)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	vectors := make([]vector.ChunkVector, 1002)
	for i := range vectors {
		vectors[i] = vector.ChunkVector{ChunkIndex: i, Vector: vector.Vector{1, 0}}
	}
	require.NoError(t, index.SaveGenerationVectors(ctx, key, pending[0], vectors))
	require.NoError(t, index.ActivateGeneration(ctx, key))

	hits, probe, err := index.QueryWithProbe(ctx, key, vector.Vector{1, 0}, semanticDeepLimit)
	require.NoError(t, err)
	assert.Len(t, hits, semanticDeepLimit)
	require.NotNil(t, probe)
	assert.InDelta(t, 1, probe.Score, 0.0001)
	hitChunks := make([]int, len(hits))
	for i, hit := range hits {
		hitChunks[i] = hit.ChunkIndex
	}
	assert.NotContains(t, hitChunks, probe.ChunkIndex)
}

func TestQueryWithProbeObservesRawCeilingBeforeFreshnessJoin(t *testing.T) {
	ctx := context.Background()
	index := openQueryTestIndex(t)
	stale := queryTestDocument(1, "stale", "stale raw chunks", queryDocOptions{})
	fresh := queryTestDocument(2, "fresh", "fresh result beyond ceiling", queryDocOptions{})
	_, err := index.RefreshMirrorPage(ctx, []searchdoc.Document{stale, fresh}, nil)
	require.NoError(t, err)
	model := vector.Generation{Model: "raw-probe", Dimensions: 2}
	key, err := index.EnsureGeneration(ctx, model)
	require.NoError(t, err)
	pending, err := index.PendingGeneration(ctx, key, 2)
	require.NoError(t, err)
	require.Len(t, pending, 2)
	for _, item := range pending {
		if item.Doc == stale.DocKey {
			vectors := make([]vector.ChunkVector, semanticDeepLimit+1)
			for i := range vectors {
				vectors[i] = vector.ChunkVector{ChunkIndex: i, Vector: vector.Vector{1, 0}}
			}
			require.NoError(t, index.SaveGenerationVectors(ctx, key, item, vectors))
			continue
		}
		require.Equal(t, fresh.DocKey, item.Doc)
		require.NoError(t, index.SaveGenerationVectors(ctx, key, item,
			[]vector.ChunkVector{{ChunkIndex: 0, Vector: vector.Vector{0.8, 0.6}}}))
	}
	require.NoError(t, index.ActivateGeneration(ctx, key))

	stale.Source.Output = "changed after embedding"
	stale = searchdoc.Render(stale.Source)
	_, err = index.RefreshMirrorPage(ctx, []searchdoc.Document{stale}, nil)
	require.NoError(t, err)
	beyond, err := index.QueryGeneration(ctx, key, vector.Vector{1, 0}, semanticDeepLimit+2)
	require.NoError(t, err)
	require.Len(t, beyond, 1, "the fresh above-floor hit exists just beyond the stale raw window")
	assert.Equal(t, fresh.DocKey, beyond[0].Doc)
	assert.GreaterOrEqual(t, float64(beyond[0].Score), semanticCosineFloor)

	hits, probe, err := index.QueryWithProbe(ctx, key, vector.Vector{1, 0}, semanticDeepLimit)
	require.NoError(t, err)
	assert.Empty(t, hits, "stale raw hits must not become candidates")
	require.NotNil(t, probe, "freshness joins must not erase the raw hit at the ceiling probe")
	assert.GreaterOrEqual(t, float64(probe.Score), semanticCosineFloor)
}

func openQueryTestIndex(t *testing.T) *Index {
	t.Helper()
	index, err := Open(context.Background(), filepath.Join(t.TempDir(), "reviews.search.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, index.Close()) })
	return index
}

type queryDocOptions struct {
	repoID    int64
	repoName  string
	branch    string
	commitSHA string
	verdict   string
	closed    bool
	finished  time.Time
	jobID     int64
}

func queryTestDocument(id int64, group, output string, options queryDocOptions) searchdoc.Document {
	if options.repoID == 0 {
		options.repoID = 7
	}
	if options.repoName == "" {
		options.repoName = "example/repo"
	}
	if options.branch == "" {
		options.branch = "main"
	}
	if options.commitSHA == "" {
		options.commitSHA = fmt.Sprintf("%040x", id)
	}
	if options.verdict == "" {
		options.verdict = "pass"
	}
	if options.finished.IsZero() {
		options.finished = time.Date(2026, 9, 15, 12, 0, 0, int(id), time.UTC)
	}
	if options.jobID == 0 {
		options.jobID = 100 + id
	}
	return searchdoc.Render(storage.SearchReviewSource{
		ReviewID: id, JobID: options.jobID, RepoID: options.repoID,
		ReviewUUID:   fmt.Sprintf("00000000-0000-4000-8000-%012d", id),
		JobUUID:      fmt.Sprintf("10000000-0000-4000-8000-%012d", id),
		PanelRunUUID: group,
		RepoName:     options.repoName, Branch: options.branch,
		GitRef: options.commitSHA, CommitSHA: options.commitSHA,
		ReviewType: "commit", PanelRole: "member", Agent: "test",
		Verdict: options.verdict, Closed: options.closed, FinishedAt: options.finished,
		Output: output,
	})
}
