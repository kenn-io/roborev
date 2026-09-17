package searchindex

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"html"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/vector"

	"go.kenn.io/roborev/internal/embedding"
	"go.kenn.io/roborev/internal/searchdoc"
	"go.kenn.io/roborev/internal/storage"
)

func TestServiceSearchModesAndCoverage(t *testing.T) {
	ctx := context.Background()
	index := openQueryTestIndex(t)
	doc := queryTestDocument(1, "group", "literal needle", queryDocOptions{})
	_, err := index.RefreshMirrorPage(ctx, []searchdoc.Document{doc}, nil)
	require.NoError(t, err)
	store := newServiceStore(doc)
	runtime := &serviceRuntime{health: HealthSnapshot{MirrorComplete: true, VectorState: "unconfigured"}}
	service := NewService(store, index, nil, runtime)

	for _, mode := range []SearchMode{ModeHybrid, ModeSemantic} {
		_, err := service.Search(ctx, SearchParams{Query: "needle", Mode: mode, Limit: 10})
		var modeErr *ModeError
		require.ErrorAs(t, err, &modeErr)
		assert.Equal(t, 400, modeErr.Status)
		assert.Equal(t, ReasonEmbeddingsUnconfigured, modeErr.Reason)
	}

	result, err := service.Search(ctx, SearchParams{Query: "needle", Mode: ModeAuto, Limit: 10})
	require.NoError(t, err)
	assert.Equal(t, ModeLexical, result.Mode)
	assert.False(t, result.Degraded)
	assert.Empty(t, result.DegradedReason)
	assert.False(t, result.Partial)
	assert.Equal(t, SearchCoverage{
		MirrorComplete: true, EmbeddingsConfigured: false, VectorState: VectorDisabled,
	}, result.Coverage)
	require.Len(t, result.Hits, 1)
	assert.Equal(t, []string{MatchLexical}, result.Hits[0].MatchedIn)
	assert.NotZero(t, result.Hits[0].Score)

	lexical, err := service.Search(ctx, SearchParams{Query: "needle", Mode: ModeLexical, Limit: 10})
	require.NoError(t, err)
	assert.Equal(t, ModeLexical, lexical.Mode)
	assert.False(t, lexical.Degraded)
}

func TestServiceReportsUnavailableVectorLegAndAutoDegrades(t *testing.T) {
	ctx := context.Background()
	index := openQueryTestIndex(t)
	doc := queryTestDocument(1, "group", "literal needle", queryDocOptions{})
	_, err := index.RefreshMirrorPage(ctx, []searchdoc.Document{doc}, nil)
	require.NoError(t, err)
	embedder := &serviceEmbedder{model: vector.Generation{Model: "test", Dimensions: 2}, query: vector.Vector{1, 0}}
	runtime := &serviceRuntime{health: HealthSnapshot{
		MirrorComplete: true, EmbeddingsConfigured: true, VectorState: "building", EmbeddingBacklog: 4,
	}}
	service := NewService(newServiceStore(doc), index, embedder, runtime)

	for _, mode := range []SearchMode{ModeHybrid, ModeSemantic} {
		_, err := service.Search(ctx, SearchParams{Query: "needle", Mode: mode, Limit: 10})
		var modeErr *ModeError
		require.ErrorAs(t, err, &modeErr)
		assert.Equal(t, 503, modeErr.Status)
		assert.Equal(t, ReasonSemanticUnavailable, modeErr.Reason)
	}
	auto, err := service.Search(ctx, SearchParams{Query: "needle", Mode: ModeAuto, Limit: 10})
	require.NoError(t, err)
	assert.Equal(t, ModeLexical, auto.Mode)
	assert.True(t, auto.Degraded)
	assert.Equal(t, ReasonSemanticUnavailable, auto.DegradedReason)
	assert.Equal(t, VectorBuilding, auto.Coverage.VectorState)
	assert.Equal(t, int64(4), auto.Coverage.EmbeddingBacklog)
}

func TestServiceCoverageMarksAnIncompleteMirrorPartial(t *testing.T) {
	ctx := context.Background()
	index := openQueryTestIndex(t)
	doc := queryTestDocument(1, "group", "needle", queryDocOptions{})
	_, err := index.RefreshMirrorPage(ctx, []searchdoc.Document{doc}, nil)
	require.NoError(t, err)
	backlog := int64(7)
	service := NewService(newServiceStore(doc), index, nil, &serviceRuntime{health: HealthSnapshot{
		MirrorComplete: false, MirrorBacklog: &backlog, VectorState: "unconfigured",
	}})

	result, err := service.Search(ctx, SearchParams{Query: "needle", Mode: ModeLexical, Limit: 10})
	require.NoError(t, err)
	assert.True(t, result.Partial)
	assert.False(t, result.Coverage.MirrorComplete)
	require.NotNil(t, result.Coverage.MirrorBacklog)
	assert.Equal(t, backlog, *result.Coverage.MirrorBacklog)
}

func TestServiceSemanticUsesCosineFloorAndVersionedChunkExcerpt(t *testing.T) {
	ctx := context.Background()
	index := openQueryTestIndex(t)
	longOutput := strings.Repeat("x", 2100) + " semantic-target \x1b[31mcolored\x1b[0m \x01end"
	matching := queryTestDocument(1, "match", longOutput, queryDocOptions{})
	belowFloor := queryTestDocument(2, "below", "unrelated", queryDocOptions{})
	_, err := index.RefreshMirrorPage(ctx, []searchdoc.Document{matching, belowFloor}, nil)
	require.NoError(t, err)
	model := vector.Generation{Model: "test", Dimensions: 2}
	seedActiveGeneration(t, index, model, map[string][]vector.ChunkVector{
		matching.DocKey: {
			{ChunkIndex: 0, Vector: vector.Vector{0, 1}},
			{ChunkIndex: 1, Vector: vector.Vector{1, 0}},
		},
		belowFloor.DocKey: {{ChunkIndex: 0, Vector: vector.Vector{0.29, 0.957026}}},
	})
	runtime := activeServiceRuntime(model)
	service := NewService(newServiceStore(matching, belowFloor), index,
		&serviceEmbedder{model: model, query: vector.Vector{1, 0}}, runtime)

	result, err := service.Search(ctx, SearchParams{Query: "paraphrase", Mode: ModeSemantic, Limit: 10})
	require.NoError(t, err)
	require.Len(t, result.Hits, 1)
	hit := result.Hits[0]
	assert.Equal(t, matching.DocKey, hit.ReviewUUID)
	assert.Equal(t, []string{MatchSemantic}, hit.MatchedIn)
	assert.InDelta(t, 1, hit.Score, 0.0001)
	assert.Contains(t, hit.Excerpt, "semantic-target")
	assert.NotContains(t, hit.Excerpt, "\x1b")
	assert.NotContains(t, hit.Excerpt, "\x01")
	assert.LessOrEqual(t, utf8.RuneCountInString(hit.Excerpt), excerptRuneLimit)
}

func TestServiceLexicalExcerptIsEscapedSanitizedAndRuneBounded(t *testing.T) {
	ctx := context.Background()
	index := openQueryTestIndex(t)
	output := "<script>alert(1)</script> needle \x1b[31mcolored\x1b[0m \x02 " + strings.Repeat("tail ", 300)
	doc := queryTestDocument(1, "group", output, queryDocOptions{})
	_, err := index.RefreshMirrorPage(ctx, []searchdoc.Document{doc}, nil)
	require.NoError(t, err)
	service := NewService(newServiceStore(doc), index, nil,
		&serviceRuntime{health: HealthSnapshot{MirrorComplete: true, VectorState: "unconfigured"}})

	result, err := service.Search(ctx, SearchParams{Query: "needle", Mode: ModeLexical, Limit: 1})
	require.NoError(t, err)
	require.Len(t, result.Hits, 1)
	excerpt := result.Hits[0].Excerpt
	assert.NotContains(t, excerpt, "<script>")
	assert.Contains(t, excerpt, "&lt;script&gt;")
	assert.NotContains(t, excerpt, "\x1b")
	assert.NotContains(t, excerpt, "\x02")
	assert.LessOrEqual(t, lexicalExcerptContentRunes(excerpt), excerptRuneLimit)
}

func TestSanitizeLexicalExcerptTruncatesContentBeforeExpandingMarkers(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "match ending at content boundary",
			raw:  strings.Repeat("a", 599) + "\ue000b\ue001tail",
			want: strings.Repeat("a", 599) + "<mark>b</mark>",
		},
		{
			name: "content boundary inside match",
			raw:  "\ue000" + strings.Repeat("界", 601) + "\ue001",
			want: "<mark>" + strings.Repeat("界", 600) + "</mark>",
		},
		{
			name: "controls ANSI and HTML remain sanitized",
			raw:  "\ue000" + strings.Repeat("x", 590) + "\x1b[31m<script>\x02\ue001tail",
			want: "<mark>" + strings.Repeat("x", 590) + "&lt;script&gt;</mark>ta",
		},
		{
			name: "unbalanced private markers are normalized",
			raw:  "\ue001x\ue000" + strings.Repeat("y", 700),
			want: "x<mark>" + strings.Repeat("y", 599) + "</mark>",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			excerpt := sanitizeLexicalExcerpt(tc.raw)
			assert.Equal(t, tc.want, excerpt)
			assert.Equal(t, excerptRuneLimit, lexicalExcerptContentRunes(excerpt))
			assert.Equal(t, strings.Count(excerpt, "<mark>"), strings.Count(excerpt, "</mark>"))
			assert.NotContains(t, excerpt, "\ue000")
			assert.NotContains(t, excerpt, "\ue001")
			assert.NotContains(t, excerpt, "\x1b")
			assert.NotContains(t, excerpt, "\x02")
			assert.NotContains(t, excerpt, "<script>")
		})
	}
}

func lexicalExcerptContentRunes(excerpt string) int {
	withoutMarkers := strings.ReplaceAll(excerpt, "<mark>", "")
	withoutMarkers = strings.ReplaceAll(withoutMarkers, "</mark>", "")
	return utf8.RuneCountInString(html.UnescapeString(withoutMarkers))
}

func TestServiceHybridGroupsPanelMembersBeforeFusionAndFiltersMembers(t *testing.T) {
	ctx := context.Background()
	index := openQueryTestIndex(t)
	panel := "00000000-0000-4000-8000-000000009999"
	lexicalMember := queryTestDocument(1, panel, "retry-safe lexical winner", queryDocOptions{verdict: "pass"})
	semanticMember := queryTestDocument(2, panel, "wording independent match", queryDocOptions{verdict: "pass"})
	synthesis := queryTestDocument(3, panel, "retry-safe synthesis", queryDocOptions{verdict: "fail", closed: true})
	synthesis.Source.PanelRole = storage.PanelRoleSynthesis
	synthesis = searchdoc.Render(synthesis.Source)
	_, err := index.RefreshMirrorPage(ctx, []searchdoc.Document{lexicalMember, semanticMember, synthesis}, nil)
	require.NoError(t, err)
	model := vector.Generation{Model: "test", Dimensions: 2}
	seedActiveGeneration(t, index, model, map[string][]vector.ChunkVector{
		lexicalMember.DocKey:  {{ChunkIndex: 0, Vector: vector.Vector{0, 1}}},
		semanticMember.DocKey: {{ChunkIndex: 0, Vector: vector.Vector{1, 0}}},
		synthesis.DocKey:      {{ChunkIndex: 0, Vector: vector.Vector{0, 1}}},
	})
	service := NewService(newServiceStore(lexicalMember, semanticMember, synthesis), index,
		&serviceEmbedder{model: model, query: vector.Vector{1, 0}}, activeServiceRuntime(model))

	result, err := service.Search(ctx, SearchParams{
		Query: "retry-safe", Mode: ModeHybrid, Limit: 10, Verdict: "pass", State: StateOpen,
	})
	require.NoError(t, err)
	require.Len(t, result.Hits, 1)
	hit := result.Hits[0]
	assert.Equal(t, lexicalMember.Source.JobID, hit.JobID)
	assert.Equal(t, lexicalMember.Source.ReviewID, hit.ReviewID)
	assert.Equal(t, "pass", hit.Verdict)
	assert.False(t, hit.Closed)
	assert.Equal(t, []string{MatchLexical, MatchSemantic}, hit.MatchedIn)
	assert.InDelta(t, 2.0/61.0, hit.Score, 1e-12)
}

func TestServiceHydrationRepeatsCanonicalLivenessFiltersAndContentHash(t *testing.T) {
	ctx := context.Background()
	cutoff := time.Date(2026, 9, 15, 11, 0, 0, 0, time.UTC)
	base := queryTestDocument(1, "group", "needle", queryDocOptions{
		repoID: 7, branch: "main", verdict: "pass", finished: cutoff.Add(time.Hour),
	})
	tests := map[string]func(*serviceStore){
		"missing canonical row": func(store *serviceStore) { delete(store.docs, base.DocKey) },
		"repository changed": func(store *serviceStore) {
			source := store.docs[base.DocKey]
			source.RepoID = 8
			store.docs[base.DocKey] = source
		},
		"branch changed": func(store *serviceStore) {
			source := store.docs[base.DocKey]
			source.Branch = "other"
			store.docs[base.DocKey] = source
		},
		"review closed": func(store *serviceStore) {
			source := store.docs[base.DocKey]
			source.Closed = true
			store.docs[base.DocKey] = source
		},
		"finished before since": func(store *serviceStore) {
			source := store.docs[base.DocKey]
			source.FinishedAt = cutoff.Add(-time.Hour)
			store.docs[base.DocKey] = source
		},
		"verdict changed": func(store *serviceStore) {
			source := store.docs[base.DocKey]
			source.Verdict = "fail"
			store.docs[base.DocKey] = source
		},
		"response changed content": func(store *serviceStore) {
			source := store.docs[base.DocKey]
			source.Responses = []storage.Response{{Responder: "reviewer", Response: "new response"}}
			store.docs[base.DocKey] = source
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			index := openQueryTestIndex(t)
			_, err := index.RefreshMirrorPage(ctx, []searchdoc.Document{base}, nil)
			require.NoError(t, err)
			store := newServiceStore(base)
			mutate(store)
			runtime := &serviceRuntime{health: HealthSnapshot{MirrorComplete: true, VectorState: "unconfigured"}}
			service := NewService(store, index, nil, runtime)
			result, err := service.Search(ctx, SearchParams{
				Query: "needle", Mode: ModeLexical, Limit: 10, RepoID: 7, Branch: "main",
				Since: &cutoff, Verdict: "pass", State: StateOpen,
			})
			require.NoError(t, err)
			assert.Empty(t, result.Hits)
			assert.Equal(t, 1, runtime.wakes)
		})
	}
}

func TestServiceKeepsPanelAlternateWhenTopCandidateIsStale(t *testing.T) {
	ctx := context.Background()
	panel := "00000000-0000-4000-8000-000000009999"
	older := queryTestDocument(1, panel, "older panel member", queryDocOptions{})
	newer := queryTestDocument(2, panel, "newer panel member", queryDocOptions{})

	t.Run("lexical", func(t *testing.T) {
		index := openQueryTestIndex(t)
		_, err := index.RefreshMirrorPage(ctx, []searchdoc.Document{older, newer}, nil)
		require.NoError(t, err)
		store := newServiceStore(older, newer)
		stale := store.docs[newer.DocKey]
		stale.Output = "canonical content changed"
		store.docs[newer.DocKey] = stale
		service := NewService(store, index, nil,
			&serviceRuntime{health: HealthSnapshot{MirrorComplete: true, VectorState: "unconfigured"}})

		result, err := service.Search(ctx, SearchParams{Query: "main", Mode: ModeLexical, Limit: 1})
		require.NoError(t, err)
		require.Len(t, result.Hits, 1)
		assert.Equal(t, older.Source.ReviewID, result.Hits[0].ReviewID)
	})

	t.Run("semantic", func(t *testing.T) {
		index := openQueryTestIndex(t)
		_, err := index.RefreshMirrorPage(ctx, []searchdoc.Document{older, newer}, nil)
		require.NoError(t, err)
		model := vector.Generation{Model: "panel-alternate", Dimensions: 2}
		seedActiveGeneration(t, index, model, map[string][]vector.ChunkVector{
			older.DocKey: {{ChunkIndex: 0, Vector: vector.Vector{0.8, 0.6}}},
			newer.DocKey: {{ChunkIndex: 0, Vector: vector.Vector{1, 0}}},
		})
		store := newServiceStore(older, newer)
		stale := store.docs[newer.DocKey]
		stale.Output = "canonical content changed"
		store.docs[newer.DocKey] = stale
		service := NewService(store, index,
			&serviceEmbedder{model: model, query: vector.Vector{1, 0}}, activeServiceRuntime(model))

		result, err := service.Search(ctx, SearchParams{
			Query: "matching meaning", Mode: ModeSemantic, Limit: 1,
		})
		require.NoError(t, err)
		require.Len(t, result.Hits, 1)
		assert.Equal(t, older.Source.ReviewID, result.Hits[0].ReviewID)
	})
}

func TestServiceRejectsStaleLexicalIdentifiersDuringHydration(t *testing.T) {
	ctx := context.Background()
	doc := queryTestDocument(1, "group", "unchanged review content", queryDocOptions{
		branch: "old-branch",
	})
	index := openQueryTestIndex(t)
	_, err := index.RefreshMirrorPage(ctx, []searchdoc.Document{doc}, nil)
	require.NoError(t, err)
	store := newServiceStore(doc)
	updated := store.docs[doc.DocKey]
	updated.Branch = "new-branch"
	store.docs[doc.DocKey] = updated
	canonical := searchdoc.Render(updated)
	require.Equal(t, doc.ContentHash, canonical.ContentHash)
	require.NotEqual(t, doc.Identifiers, canonical.Identifiers)
	runtime := &serviceRuntime{health: HealthSnapshot{MirrorComplete: true, VectorState: "unconfigured"}}
	service := NewService(store, index, nil, runtime)

	result, err := service.Search(ctx, SearchParams{
		Query: "old-branch", Mode: ModeLexical, Limit: 10,
	})
	require.NoError(t, err)
	assert.Empty(t, result.Hits)
	assert.Equal(t, 1, runtime.wakes)
}

func TestServiceSemanticCandidateCeilingIsErrorForExplicitAndBoundedForAuto(t *testing.T) {
	ctx := context.Background()
	index := openQueryTestIndex(t)
	const count = semanticDeepLimit + 1
	docs := make([]searchdoc.Document, 0, count)
	for i := range count {
		docs = append(docs, queryTestDocument(int64(i+1), "one-panel", "candidate", queryDocOptions{}))
	}
	_, err := index.RefreshMirrorPage(ctx, docs, nil)
	require.NoError(t, err)
	model := vector.Generation{Model: "ceiling", Dimensions: 2}
	vectors := make(map[string][]vector.ChunkVector, len(docs))
	for _, doc := range docs {
		vectors[doc.DocKey] = []vector.ChunkVector{{ChunkIndex: 0, Vector: vector.Vector{1, 0}}}
	}
	seedActiveGeneration(t, index, model, vectors)
	service := NewService(newServiceStore(docs...), index,
		&serviceEmbedder{model: model, query: vector.Vector{1, 0}}, activeServiceRuntime(model))

	_, err = service.Search(ctx, SearchParams{Query: "unseen paraphrase", Mode: ModeSemantic, Limit: 2})
	var modeErr *ModeError
	require.ErrorAs(t, err, &modeErr)
	assert.Equal(t, 503, modeErr.Status)
	assert.Equal(t, ReasonSemanticCeiling, modeErr.Reason)

	auto, err := service.Search(ctx, SearchParams{Query: "unseen paraphrase", Mode: ModeAuto, Limit: 2})
	require.NoError(t, err)
	require.Len(t, auto.Hits, 1)
	assert.Equal(t, ModeHybrid, auto.Mode)
	assert.True(t, auto.Bounded)
	assert.Equal(t, ReasonSemanticCeiling, auto.BoundedReason)
	assert.True(t, auto.Degraded)
	assert.Equal(t, ReasonSemanticCeiling, auto.DegradedReason)
	assert.True(t, auto.Partial)
}

func TestServiceSemanticRefillsAfterCanonicalSuppression(t *testing.T) {
	ctx := context.Background()
	index := openQueryTestIndex(t)
	target := candidateTarget(1)
	stale := make([]searchdoc.Document, 0, target)
	all := make([]searchdoc.Document, 0, target+1)
	for i := range target {
		doc := queryTestDocument(int64(i+1), fmt.Sprintf("stale-%d", i), "stale candidate", queryDocOptions{})
		stale = append(stale, doc)
		all = append(all, doc)
	}
	fresh := queryTestDocument(int64(target+1), "fresh", "lower ranked fresh candidate", queryDocOptions{})
	all = append(all, fresh)
	_, err := index.RefreshMirrorPage(ctx, all, nil)
	require.NoError(t, err)
	model := vector.Generation{Model: "canonical-refill", Dimensions: 2}
	vectors := make(map[string][]vector.ChunkVector, len(all))
	for _, doc := range stale {
		vectors[doc.DocKey] = []vector.ChunkVector{{ChunkIndex: 0, Vector: vector.Vector{1, 0}}}
	}
	vectors[fresh.DocKey] = []vector.ChunkVector{{ChunkIndex: 0, Vector: vector.Vector{0.8, 0.6}}}
	seedActiveGeneration(t, index, model, vectors)
	store := newServiceStore(all...)
	for _, doc := range stale {
		source := store.docs[doc.DocKey]
		source.Output = "canonical content changed"
		store.docs[doc.DocKey] = source
	}
	runtime := activeServiceRuntime(model)
	service := NewService(store, index,
		&serviceEmbedder{model: model, query: vector.Vector{1, 0}}, runtime)

	result, err := service.Search(ctx, SearchParams{
		Query: "semantic refill", Mode: ModeSemantic, Limit: 1,
	})
	require.NoError(t, err)
	require.Len(t, result.Hits, 1)
	assert.Equal(t, fresh.Source.ReviewID, result.Hits[0].ReviewID)
	assert.GreaterOrEqual(t, runtime.wakes, target)
}

func TestServiceFullPageAtSemanticCeilingIsPartialButNotBounded(t *testing.T) {
	ctx := context.Background()
	index := openQueryTestIndex(t)
	const count = semanticDeepLimit + 1
	docs := make([]searchdoc.Document, 0, count)
	for i := range count {
		docs = append(docs, queryTestDocument(int64(i+1), "one-panel", "candidate", queryDocOptions{}))
	}
	_, err := index.RefreshMirrorPage(ctx, docs, nil)
	require.NoError(t, err)
	model := vector.Generation{Model: "full-page-ceiling", Dimensions: 2}
	vectors := make(map[string][]vector.ChunkVector, len(docs))
	for _, doc := range docs {
		vectors[doc.DocKey] = []vector.ChunkVector{{ChunkIndex: 0, Vector: vector.Vector{1, 0}}}
	}
	seedActiveGeneration(t, index, model, vectors)
	service := NewService(newServiceStore(docs...), index,
		&serviceEmbedder{model: model, query: vector.Vector{1, 0}}, activeServiceRuntime(model))

	result, err := service.Search(ctx, SearchParams{
		Query: "unseen paraphrase", Mode: ModeAuto, Limit: 1,
	})
	require.NoError(t, err)
	require.Len(t, result.Hits, 1)
	assert.False(t, result.Bounded)
	assert.Empty(t, result.BoundedReason)
	assert.True(t, result.Partial)
}

func TestSemanticCeilingRequiresShortPageAndAboveFloorProbe(t *testing.T) {
	aboveFloor := &vector.Hit[string]{Score: semanticCosineFloor}
	belowFloor := &vector.Hit[string]{Score: semanticCosineFloor - 0.001}
	assert.False(t, semanticPageBounded(2, 2, semanticCeilingReached(aboveFloor)), "a full page is not bounded")
	assert.False(t, semanticPageBounded(1, 2, semanticCeilingReached(nil)), "an absent probe proves exhaustion")
	assert.False(t, semanticPageBounded(1, 2, semanticCeilingReached(belowFloor)), "a below-floor probe proves exhaustion")
	assert.True(t, semanticPageBounded(1, 2, semanticCeilingReached(aboveFloor)))
}

func TestServiceQueryEmbeddingHasIndependentThreeSecondTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		index := openQueryTestIndex(t)
		doc := queryTestDocument(1, "group", "literal needle", queryDocOptions{})
		_, err := index.RefreshMirrorPage(ctx, []searchdoc.Document{doc}, nil)
		require.NoError(t, err)
		model := vector.Generation{Model: "timeout", Dimensions: 2}
		seedActiveGeneration(t, index, model, map[string][]vector.ChunkVector{
			doc.DocKey: {{ChunkIndex: 0, Vector: vector.Vector{1, 0}}},
		})
		embedder := &serviceEmbedder{model: model, embed: func(ctx context.Context) ([]float32, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}}
		service := NewService(newServiceStore(doc), index, embedder, activeServiceRuntime(model))

		result, err := service.Search(ctx, SearchParams{Query: "needle", Mode: ModeAuto, Limit: 10})
		require.NoError(t, err)
		assert.Equal(t, ModeLexical, result.Mode)
		assert.True(t, result.Degraded)
		assert.Equal(t, ReasonSemanticUnavailable, result.DegradedReason)
	})
}

func TestHybridLegRunnerStartsLexicalAndSemanticConcurrently(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		lexicalStarted := make(chan struct{})
		semanticStarted := make(chan struct{})

		lexical, semantic, lexicalErr, semanticErr := runHybridLegs(ctx,
			func(ctx context.Context) ([]rankedCandidate, error) {
				close(lexicalStarted)
				select {
				case <-semanticStarted:
					return []rankedCandidate{{DocKey: "lexical"}}, nil
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			},
			func(ctx context.Context) (semanticLegResult, error) {
				close(semanticStarted)
				select {
				case <-lexicalStarted:
					return semanticLegResult{Candidates: []rankedCandidate{{DocKey: "semantic"}}}, nil
				case <-ctx.Done():
					return semanticLegResult{}, ctx.Err()
				}
			})

		require.NoError(t, lexicalErr)
		require.NoError(t, semanticErr)
		assert.Equal(t, "lexical", lexical[0].DocKey)
		assert.Equal(t, "semantic", semantic.Candidates[0].DocKey)
	})
}

type serviceStore struct {
	mu    sync.Mutex
	docs  map[string]storage.SearchReviewSource
	repos map[int64]*storage.Repo
}

func newServiceStore(docs ...searchdoc.Document) *serviceStore {
	store := &serviceStore{docs: make(map[string]storage.SearchReviewSource), repos: make(map[int64]*storage.Repo)}
	for _, doc := range docs {
		store.docs[doc.DocKey] = doc.Source
		store.repos[doc.Source.RepoID] = &storage.Repo{
			ID: doc.Source.RepoID, Name: doc.Source.RepoName,
			RootPath: fmt.Sprintf("/repos/%d", doc.Source.RepoID),
		}
	}
	return store
}

func (s *serviceStore) GetSearchDocument(_ context.Context, key string) (*storage.SearchReviewSource, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	doc, ok := s.docs[key]
	if !ok {
		return nil, sql.ErrNoRows
	}
	copy := doc
	return &copy, nil
}

func (s *serviceStore) GetRepoByID(id int64) (*storage.Repo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	repo, ok := s.repos[id]
	if !ok {
		return nil, sql.ErrNoRows
	}
	copy := *repo
	return &copy, nil
}

type serviceRuntime struct {
	mu     sync.Mutex
	health HealthSnapshot
	wakes  int
}

func (r *serviceRuntime) Health() HealthSnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	return cloneHealth(r.health)
}

func (r *serviceRuntime) Wake() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.wakes++
}

type serviceEmbedder struct {
	model vector.Generation
	query vector.Vector
	embed func(context.Context) ([]float32, error)
}

func (e *serviceEmbedder) Embed(ctx context.Context, kind embedding.InputKind, texts []string) ([][]float32, error) {
	if kind != embedding.InputQuery {
		return nil, errors.New("service must request query embeddings")
	}
	if len(texts) != 1 {
		return nil, fmt.Errorf("got %d query texts", len(texts))
	}
	if e.embed != nil {
		query, err := e.embed(ctx)
		if err != nil {
			return nil, err
		}
		return [][]float32{query}, nil
	}
	return [][]float32{e.query}, nil
}

func (e *serviceEmbedder) Generation() vector.Generation { return e.model }
func (e *serviceEmbedder) BatchSize() int                { return 64 }

func activeServiceRuntime(model vector.Generation) *serviceRuntime {
	return &serviceRuntime{health: HealthSnapshot{
		MirrorComplete: true, EmbeddingsConfigured: true, VectorState: "ready",
		Generation: model.Fingerprint(), ActiveGeneration: model.Fingerprint(),
	}}
}

func seedActiveGeneration(
	t *testing.T, index *Index, model vector.Generation, vectors map[string][]vector.ChunkVector,
) {
	t.Helper()
	ctx := context.Background()
	key, err := index.EnsureGeneration(ctx, model)
	require.NoError(t, err)
	pending, err := index.PendingGeneration(ctx, key, len(vectors)+1)
	require.NoError(t, err)
	require.Len(t, pending, len(vectors))
	for _, item := range pending {
		chunkVectors, ok := vectors[item.Doc]
		require.True(t, ok, "missing vectors for %s", item.Doc)
		require.NoError(t, index.SaveGenerationVectors(ctx, key, item, chunkVectors))
	}
	require.NoError(t, index.ActivateGeneration(ctx, key))
}

// The vector query and candidate construction are separate operations. Advance
// both stores between them so an old score cannot be attached to new content.
func TestSemanticCandidatesRejectRevisionChangedAfterVectorQuery(t *testing.T) {
	for _, deep := range []bool{false, true} {
		for _, reembed := range []bool{false, true} {
			t.Run(fmt.Sprintf("deep=%t/reembed=%t", deep, reembed), func(t *testing.T) {
				ctx := context.Background()
				index := openQueryTestIndex(t)
				original := queryTestDocument(1, "group", "original matching content", queryDocOptions{})
				_, err := index.RefreshMirrorPage(ctx, []searchdoc.Document{original}, nil)
				require.NoError(t, err)
				model := vector.Generation{Model: "test", Dimensions: 2}
				seedActiveGeneration(t, index, model, map[string][]vector.ChunkVector{
					original.DocKey: {{ChunkIndex: 0, Vector: vector.Vector{1, 0}}},
				})
				store := newServiceStore(original)
				service := NewService(store, index, nil, nil)
				key := model.Fingerprint()
				raw, err := index.QueryGeneration(ctx, key, vector.Vector{1, 0}, 50)
				require.NoError(t, err)
				if deep {
					var probe *vector.Hit[string]
					raw, probe, err = index.QueryWithProbe(ctx, key, vector.Vector{1, 0}, semanticDeepLimit)
					require.NoError(t, err)
					require.Nil(t, probe)
				}
				require.Len(t, raw, 1)
				require.InDelta(t, 1, raw[0].Score, 0.0001)

				updated := queryTestDocument(1, "group", "replacement unrelated content", queryDocOptions{})
				require.NotEqual(t, original.ContentHash, updated.ContentHash)
				store.docs[updated.DocKey] = updated.Source
				_, err = index.RefreshMirrorPage(ctx, []searchdoc.Document{updated}, nil)
				require.NoError(t, err)
				if reembed {
					seedActiveGeneration(t, index, model, map[string][]vector.ChunkVector{
						updated.DocKey: {{ChunkIndex: 0, Vector: vector.Vector{0, 1}}},
					})
				}

				candidates, err := index.semanticCandidates(ctx, raw, SearchFilters{})
				require.NoError(t, err)
				hydrated, err := service.hydrate(ctx, candidates, SearchFilters{})
				require.NoError(t, err)
				assert.Empty(t, service.toHits(hydrated, 10),
					"replacement content must not inherit the original vector's score")
			})
		}
	}
}
