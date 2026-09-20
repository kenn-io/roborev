package searchindex

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"html"
	"slices"
	"strings"
	"time"
	"unicode"

	"go.kenn.io/kit/vector"

	"go.kenn.io/roborev/internal/searchdoc"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/streamfmt"
)

const (
	queryEmbeddingTimeout = 3 * time.Second
	excerptRuneLimit      = 600

	MatchIdentifier = "identifier"
	MatchLexical    = "lexical"
	MatchSemantic   = "semantic"

	StateAll    = "all"
	StateOpen   = "open"
	StateClosed = "closed"

	VectorDisabled    = "disabled"
	VectorBuilding    = "building"
	VectorActive      = "active"
	VectorReplacing   = "replacing"
	VectorUnavailable = "unavailable"

	ReasonEmbeddingsUnconfigured = "embeddings are not configured"
	ReasonSemanticUnavailable    = "semantic search is unavailable"
	ReasonSemanticCeiling        = semanticProbeReason
)

// SearchMode selects the retrieval legs used for review search.
type SearchMode string

const (
	ModeAuto     SearchMode = "auto"
	ModeLexical  SearchMode = "lexical"
	ModeHybrid   SearchMode = "hybrid"
	ModeSemantic SearchMode = "semantic"
)

// SearchParams contains validated search text and canonical filters.
type SearchParams struct {
	Query   string
	Mode    SearchMode
	RepoID  int64
	Branch  string
	Since   *time.Time
	Verdict string
	State   string
	Limit   int
}

// SearchResult is the transport-neutral search response.
type SearchResult struct {
	Query          string         `json:"query"`
	Mode           SearchMode     `json:"mode"`
	Degraded       bool           `json:"degraded"`
	DegradedReason string         `json:"degraded_reason,omitempty"`
	Bounded        bool           `json:"bounded"`
	BoundedReason  string         `json:"bounded_reason,omitempty"`
	Partial        bool           `json:"partial"`
	Coverage       SearchCoverage `json:"coverage"`
	Hits           []SearchHit    `json:"hits"`
}

// SearchCoverage reports point-in-time mirror and embedding completeness.
type SearchCoverage struct {
	MirrorComplete       bool   `json:"mirror_complete"`
	MirrorBacklog        *int64 `json:"mirror_backlog,omitempty"`
	EmbeddingsConfigured bool   `json:"embeddings_configured"`
	VectorState          string `json:"vector_state"`
	EmbeddingBacklog     int64  `json:"embedding_backlog"`
	Skipped              int64  `json:"skipped"`
}

// SearchHit identifies the canonical review member chosen for one group.
type SearchHit struct {
	JobID         int64     `json:"job_id"`
	JobUUID       string    `json:"job_uuid,omitempty"`
	ReviewID      int64     `json:"review_id"`
	ReviewUUID    string    `json:"review_uuid,omitempty"`
	RepoName      string    `json:"repo_name"`
	RepoPath      string    `json:"repo_path"`
	GitRef        string    `json:"git_ref"`
	CommitSHA     string    `json:"commit_sha,omitempty"`
	CommitSubject string    `json:"commit_subject,omitempty"`
	Branch        string    `json:"branch,omitempty"`
	ReviewType    string    `json:"review_type"`
	PanelRole     string    `json:"panel_role,omitempty"`
	Agent         string    `json:"agent"`
	Verdict       string    `json:"verdict,omitempty"`
	Closed        bool      `json:"closed"`
	FinishedAt    time.Time `json:"finished_at"`
	Score         float64   `json:"score"`
	MatchedIn     []string  `json:"matched_in"`
	Excerpt       string    `json:"excerpt"`
}

// ModeError maps a requested search mode failure to an HTTP-compatible status.
type ModeError struct {
	Status int
	Reason string
	Err    error
}

func (e *ModeError) Error() string { return e.Reason }
func (e *ModeError) Unwrap() error { return e.Err }

type canonicalSearchStore interface {
	GetSearchDocument(context.Context, string) (*storage.SearchReviewSource, error)
	GetRepoByID(int64) (*storage.Repo, error)
}

type searchRuntime interface {
	Health() HealthSnapshot
	Wake()
}

// Service coordinates retrieval against the disposable sidecar and canonical
// hydration against reviews.db.
type Service struct {
	store    canonicalSearchStore
	index    *Index
	embedder Embedder
	runtime  searchRuntime
}

// NewService constructs a transport-neutral review search service.
func NewService(store canonicalSearchStore, index *Index, embedder Embedder, runtime searchRuntime) *Service {
	return &Service{store: store, index: index, embedder: embedder, runtime: runtime}
}

type semanticLegResult struct {
	Candidates     []rankedCandidate
	Probe          *vector.Hit[string]
	Query          vector.Vector
	GenerationKey  string
	Deep           bool
	CeilingReached bool
}

// Search runs the effective retrieval mode and hydrates every result from the
// canonical store before returning it.
func (service *Service) Search(ctx context.Context, params SearchParams) (SearchResult, error) {
	params = normalizeSearchParams(params)
	health := service.health()
	result := SearchResult{
		Query: params.Query, Coverage: service.coverage(ctx, health),
	}
	result.Partial = !result.Coverage.MirrorComplete
	filters := filtersForParams(params)

	effective, degraded, reason, err := service.resolveMode(ctx, params.Mode)
	if err != nil {
		return SearchResult{}, err
	}
	result.Mode = effective
	result.Degraded = degraded
	result.DegradedReason = reason

	target := candidateTarget(params.Limit)
	switch effective {
	case ModeLexical:
		candidates, err := service.index.SearchLexical(ctx, params.Query, filters, target)
		if err != nil {
			return SearchResult{}, err
		}
		candidates, err = service.hydrate(ctx, candidates, filters)
		if err != nil {
			return SearchResult{}, err
		}
		result.Hits = service.toHits(candidates, params.Limit)
	case ModeSemantic:
		semantic, err := service.searchSemantic(ctx, params.Query, filters, target)
		if err != nil {
			return SearchResult{}, service.unavailableError(err)
		}
		semantic, unavailable, err := service.hydrateAndRefillSemantic(ctx, semantic, filters, target)
		if err != nil {
			if unavailable {
				return SearchResult{}, service.unavailableError(err)
			}
			return SearchResult{}, err
		}
		if semanticPageBounded(len(semantic.Candidates), params.Limit, semantic.CeilingReached) {
			return SearchResult{}, &ModeError{Status: 503, Reason: ReasonSemanticCeiling}
		}
		result.Hits = service.toHits(semantic.Candidates, params.Limit)
		result.Partial = result.Partial || semantic.CeilingReached
	case ModeHybrid:
		lexical, semantic, lexicalErr, semanticErr := runHybridLegs(ctx,
			func(ctx context.Context) ([]rankedCandidate, error) {
				return service.index.SearchLexical(ctx, params.Query, filters, target)
			},
			func(ctx context.Context) (semanticLegResult, error) {
				return service.searchSemantic(ctx, params.Query, filters, target)
			})
		if lexicalErr != nil {
			return SearchResult{}, lexicalErr
		}
		if semanticErr != nil {
			if params.Mode != ModeAuto {
				return SearchResult{}, service.unavailableError(semanticErr)
			}
			result.Mode = ModeLexical
			result.Degraded = true
			result.DegradedReason = ReasonSemanticUnavailable
			lexical, err = service.hydrate(ctx, lexical, filters)
			if err != nil {
				return SearchResult{}, err
			}
			result.Hits = service.toHits(lexical, params.Limit)
			break
		}
		lexical, err = service.hydrate(ctx, lexical, filters)
		if err != nil {
			return SearchResult{}, err
		}
		semantic, unavailable, err := service.hydrateAndRefillSemantic(ctx, semantic, filters, target)
		if err != nil {
			if unavailable {
				if params.Mode != ModeAuto {
					return SearchResult{}, service.unavailableError(err)
				}
				result.Mode = ModeLexical
				result.Degraded = true
				result.DegradedReason = ReasonSemanticUnavailable
				result.Hits = service.toHits(lexical, params.Limit)
				break
			}
			return SearchResult{}, err
		}
		bounded := semanticPageBounded(len(semantic.Candidates), params.Limit, semantic.CeilingReached)
		if bounded && params.Mode != ModeAuto {
			return SearchResult{}, &ModeError{Status: 503, Reason: ReasonSemanticCeiling}
		}
		merged := mergeGroupRRF(lexical, semantic.Candidates, params.Limit)
		preferLexicalExcerpts(merged, lexical)
		result.Hits = service.toHits(merged, params.Limit)
		result.Partial = result.Partial || semantic.CeilingReached
		if bounded {
			result.Bounded = true
			result.BoundedReason = ReasonSemanticCeiling
			result.Degraded = true
			result.DegradedReason = ReasonSemanticCeiling
			result.Partial = true
		}
	default:
		return SearchResult{}, &ModeError{Status: 400, Reason: "invalid search mode"}
	}

	if (result.Mode == ModeSemantic || result.Mode == ModeHybrid) && result.Coverage.EmbeddingBacklog > 0 {
		result.Partial = true
	}
	if result.Hits == nil {
		result.Hits = []SearchHit{}
	}
	return result, nil
}

func normalizeSearchParams(params SearchParams) SearchParams {
	params.Query = strings.TrimSpace(params.Query)
	if params.Mode == "" {
		params.Mode = ModeAuto
	}
	if params.State == "" {
		params.State = StateAll
	}
	if params.Limit <= 0 {
		params.Limit = 20
	}
	return params
}

func filtersForParams(params SearchParams) SearchFilters {
	return SearchFilters{
		RepoID: params.RepoID, Branch: params.Branch, Since: params.Since,
		Verdict: params.Verdict, State: params.State,
	}
}

func (service *Service) resolveMode(ctx context.Context, requested SearchMode) (SearchMode, bool, string, error) {
	switch requested {
	case ModeLexical:
		return ModeLexical, false, "", nil
	case ModeAuto, ModeHybrid, ModeSemantic:
	default:
		return "", false, "", &ModeError{Status: 400, Reason: "invalid search mode"}
	}
	if service.embedder == nil {
		if requested == ModeAuto {
			return ModeLexical, false, "", nil
		}
		return "", false, "", &ModeError{Status: 400, Reason: ReasonEmbeddingsUnconfigured}
	}
	available, err := service.index.GenerationAvailable(ctx, service.embedder.Generation().Fingerprint())
	if err != nil {
		if requested == ModeAuto {
			return ModeLexical, true, ReasonSemanticUnavailable, nil
		}
		return "", false, "", service.unavailableError(err)
	}
	if !available {
		if requested == ModeAuto {
			return ModeLexical, true, ReasonSemanticUnavailable, nil
		}
		return "", false, "", &ModeError{Status: 503, Reason: ReasonSemanticUnavailable}
	}
	if requested == ModeSemantic {
		return ModeSemantic, false, "", nil
	}
	return ModeHybrid, false, "", nil
}

func (service *Service) searchSemantic(
	ctx context.Context, query string, filters SearchFilters, target int,
) (semanticLegResult, error) {
	queryCtx, cancel := context.WithTimeout(ctx, queryEmbeddingTimeout)
	encoded, err := vector.EncodeBatched(queryCtx, encodeQueries(service.embedder), []vector.Chunk{{Index: 0, Text: query}})
	cancel()
	if err != nil {
		return semanticLegResult{}, err
	}
	if len(encoded) != 1 {
		return semanticLegResult{}, fmt.Errorf("query embedding returned %d vectors", len(encoded))
	}
	queryVector := encoded[0]
	key := service.embedder.Generation().Fingerprint()
	raw, err := service.index.QueryGeneration(ctx, key, queryVector, target)
	if err != nil {
		return semanticLegResult{}, err
	}
	candidates, err := service.index.semanticCandidates(ctx, raw, filters)
	if err != nil {
		return semanticLegResult{}, err
	}
	if candidateGroupCount(candidates) >= target {
		return semanticLegResult{
			Candidates: candidates, Query: queryVector, GenerationKey: key,
		}, nil
	}
	return service.searchSemanticDeep(ctx, key, queryVector, filters)
}

func candidateGroupCount(candidates []rankedCandidate) int {
	groups := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		groups[candidate.GroupKey] = struct{}{}
	}
	return len(groups)
}

func (service *Service) searchSemanticDeep(
	ctx context.Context, key string, query vector.Vector, filters SearchFilters,
) (semanticLegResult, error) {
	raw, probe, err := service.index.QueryWithProbe(ctx, key, query, semanticDeepLimit)
	if err != nil {
		return semanticLegResult{}, err
	}
	candidates, err := service.index.semanticCandidates(ctx, raw, filters)
	if err != nil {
		return semanticLegResult{}, err
	}
	return semanticLegResult{
		Candidates: candidates, Probe: probe, Query: query, GenerationKey: key,
		Deep: true, CeilingReached: semanticCeilingReached(probe),
	}, nil
}

// hydrateAndRefillSemantic makes the single deep-retry decision only after
// canonical liveness, filters, and content hashes have suppressed stale rows.
// The bool result identifies vector retrieval failures for mode degradation.
func (service *Service) hydrateAndRefillSemantic(
	ctx context.Context, semantic semanticLegResult, filters SearchFilters, target int,
) (semanticLegResult, bool, error) {
	hydrated, err := service.hydrate(ctx, semantic.Candidates, filters)
	if err != nil {
		return semanticLegResult{}, false, err
	}
	semantic.Candidates = hydrated
	if semantic.Deep || len(hydrated) >= target {
		return semantic, false, nil
	}
	semantic, err = service.searchSemanticDeep(
		ctx, semantic.GenerationKey, semantic.Query, filters,
	)
	if err != nil {
		return semanticLegResult{}, true, err
	}
	hydrated, err = service.hydrate(ctx, semantic.Candidates, filters)
	if err != nil {
		return semanticLegResult{}, false, err
	}
	semantic.Candidates = hydrated
	return semantic, false, nil
}

func runHybridLegs(
	ctx context.Context,
	lexicalFn func(context.Context) ([]rankedCandidate, error),
	semanticFn func(context.Context) (semanticLegResult, error),
) ([]rankedCandidate, semanticLegResult, error, error) {
	type lexicalOutput struct {
		candidates []rankedCandidate
		err        error
	}
	type semanticOutput struct {
		result semanticLegResult
		err    error
	}
	lexicalCh := make(chan lexicalOutput, 1)
	semanticCh := make(chan semanticOutput, 1)
	go func() {
		candidates, err := lexicalFn(ctx)
		lexicalCh <- lexicalOutput{candidates: candidates, err: err}
	}()
	go func() {
		result, err := semanticFn(ctx)
		semanticCh <- semanticOutput{result: result, err: err}
	}()
	lexical := <-lexicalCh
	semantic := <-semanticCh
	return lexical.candidates, semantic.result, lexical.err, semantic.err
}

func (service *Service) hydrate(
	ctx context.Context, candidates []rankedCandidate, filters SearchFilters,
) ([]rankedCandidate, error) {
	hydrated := make([]rankedCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		source, err := service.store.GetSearchDocument(ctx, candidate.DocKey)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				service.wake()
				continue
			}
			return nil, fmt.Errorf("hydrate search review: %w", err)
		}
		document := searchdoc.Render(*source)
		if document.DocKey != candidate.DocKey || document.ContentHash != candidate.ContentHash ||
			lexicalIdentifiersChanged(candidate, document) ||
			!sourceMatchesFilters(*source, filters) {
			service.wake()
			continue
		}
		repo, err := service.store.GetRepoByID(source.RepoID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				service.wake()
				continue
			}
			return nil, fmt.Errorf("hydrate search repository: %w", err)
		}
		candidate = hydrateCandidate(candidate, document, repo.RootPath)
		if hasMatch(candidate.MatchedIn, MatchSemantic) && !hasMatch(candidate.MatchedIn, MatchLexical) {
			chunks := vector.Split(document.Content, vector.SplitOptions{
				MaxRunes: searchChunkRunes, Overlap: searchChunkOverlap,
			})
			if candidate.ChunkIndex < 0 || candidate.ChunkIndex >= len(chunks) {
				service.wake()
				continue
			}
			candidate.Excerpt = sanitizePlainExcerpt(chunks[candidate.ChunkIndex].Text)
		} else {
			candidate.Excerpt = sanitizeLexicalExcerpt(candidate.Excerpt)
		}
		hydrated = append(hydrated, candidate)
	}
	return groupLegCandidates(hydrated), nil
}

func lexicalIdentifiersChanged(candidate rankedCandidate, document searchdoc.Document) bool {
	lexical := hasMatch(candidate.MatchedIn, MatchIdentifier) || hasMatch(candidate.MatchedIn, MatchLexical)
	return lexical && candidate.identifiers != document.Identifiers
}

func hydrateCandidate(candidate rankedCandidate, document searchdoc.Document, repoPath string) rankedCandidate {
	source := document.Source
	candidate.DocKey = document.DocKey
	candidate.GroupKey = document.GroupKey
	candidate.ReviewID = source.ReviewID
	candidate.ReviewUUID = source.ReviewUUID
	candidate.JobID = source.JobID
	candidate.JobUUID = source.JobUUID
	candidate.RepoID = source.RepoID
	candidate.RepoName = source.RepoName
	candidate.Branch = source.Branch
	candidate.GitRef = source.GitRef
	candidate.CommitSHA = source.CommitSHA
	candidate.FinishedAt = source.FinishedAt
	candidate.Verdict = source.Verdict
	candidate.Closed = source.Closed
	candidate.PanelRole = source.PanelRole
	candidate.Content = document.Content
	candidate.ContentHash = document.ContentHash
	candidate.identifiers = document.Identifiers
	// RepoPath and the remaining canonical-only fields are carried by the hit.
	candidate.repoPath = repoPath
	candidate.commitSubject = source.CommitSubject
	candidate.reviewType = source.ReviewType
	candidate.agent = source.Agent
	return candidate
}

func sourceMatchesFilters(source storage.SearchReviewSource, filters SearchFilters) bool {
	if filters.RepoID != 0 && source.RepoID != filters.RepoID {
		return false
	}
	if filters.Branch != "" && source.Branch != filters.Branch {
		return false
	}
	if filters.Since != nil && source.FinishedAt.Before(*filters.Since) {
		return false
	}
	if filters.Verdict != "" && source.Verdict != filters.Verdict {
		return false
	}
	if filters.State == StateOpen && source.Closed {
		return false
	}
	if filters.State == StateClosed && !source.Closed {
		return false
	}
	return true
}

func (service *Service) toHits(candidates []rankedCandidate, limit int) []SearchHit {
	if len(candidates) > limit {
		candidates = candidates[:limit]
	}
	hits := make([]SearchHit, 0, len(candidates))
	for _, candidate := range candidates {
		hits = append(hits, SearchHit{
			JobID: candidate.JobID, JobUUID: candidate.JobUUID,
			ReviewID: candidate.ReviewID, ReviewUUID: candidate.ReviewUUID,
			RepoName: candidate.RepoName, RepoPath: candidate.repoPath,
			GitRef: candidate.GitRef, CommitSHA: candidate.CommitSHA,
			CommitSubject: candidate.commitSubject, Branch: candidate.Branch,
			ReviewType: candidate.reviewType, PanelRole: candidate.PanelRole,
			Agent: candidate.agent, Verdict: candidate.Verdict, Closed: candidate.Closed,
			FinishedAt: candidate.FinishedAt, Score: candidate.Score,
			MatchedIn: candidate.MatchedIn, Excerpt: candidate.Excerpt,
		})
	}
	return hits
}

func semanticCeilingReached(probe *vector.Hit[string]) bool {
	return probe != nil && float64(probe.Score) >= semanticCosineFloor
}

func semanticPageBounded(resultCount, requestedLimit int, ceilingReached bool) bool {
	return resultCount < requestedLimit && ceilingReached
}

func preferLexicalExcerpts(merged, lexical []rankedCandidate) {
	byDocument := make(map[string]string, len(lexical))
	for _, candidate := range lexical {
		byDocument[candidate.DocKey] = candidate.Excerpt
	}
	for i := range merged {
		if hasMatch(merged[i].MatchedIn, MatchLexical) && hasMatch(merged[i].MatchedIn, MatchSemantic) {
			if excerpt, found := byDocument[merged[i].DocKey]; found {
				merged[i].Excerpt = excerpt
			}
		}
	}
}

func hasMatch(matches []string, wanted string) bool {
	return slices.Contains(matches, wanted)
}

func sanitizeLexicalExcerpt(value string) string {
	value = sanitizeControls(value)
	value = truncateMarkedExcerpt(value, excerptRuneLimit)
	value = html.EscapeString(value)
	value = strings.ReplaceAll(value, "\ue000", "<mark>")
	value = strings.ReplaceAll(value, "\ue001", "</mark>")
	return value
}

func truncateMarkedExcerpt(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	var result strings.Builder
	contentRunes := 0
	marked := false
	for _, current := range value {
		switch current {
		case '\ue000':
			if contentRunes < limit && !marked {
				result.WriteRune(current)
				marked = true
			}
		case '\ue001':
			if marked {
				result.WriteRune(current)
				marked = false
			}
		default:
			if contentRunes == limit {
				if marked {
					result.WriteRune('\ue001')
				}
				return result.String()
			}
			result.WriteRune(current)
			contentRunes++
		}
	}
	if marked {
		result.WriteRune('\ue001')
	}
	return result.String()
}

func sanitizePlainExcerpt(value string) string {
	return truncateRunes(sanitizeControls(value), excerptRuneLimit)
}

func sanitizeControls(value string) string {
	value = streamfmt.StripANSI(value)
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' || !unicode.IsControl(r) {
			return r
		}
		return -1
	}, value)
}

func truncateRunes(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}

func (service *Service) unavailableError(err error) error {
	if _, ok := errors.AsType[*ModeError](err); ok {
		return err
	}
	return &ModeError{Status: 503, Reason: ReasonSemanticUnavailable, Err: err}
}

func (service *Service) health() HealthSnapshot {
	if service.runtime == nil {
		return HealthSnapshot{EmbeddingsConfigured: service.embedder != nil}
	}
	return service.runtime.Health()
}

func (service *Service) wake() {
	if service.runtime != nil {
		service.runtime.Wake()
	}
}

func (service *Service) coverage(ctx context.Context, health HealthSnapshot) SearchCoverage {
	coverage := SearchCoverage{
		MirrorComplete: health.MirrorComplete, MirrorBacklog: health.MirrorBacklog,
		EmbeddingsConfigured: service.embedder != nil,
		EmbeddingBacklog:     health.EmbeddingBacklog, Skipped: health.Skipped,
		VectorState: VectorDisabled,
	}
	if service.embedder == nil {
		coverage.EmbeddingBacklog = 0
		return coverage
	}
	available, err := service.index.GenerationAvailable(ctx, service.embedder.Generation().Fingerprint())
	if err == nil && available {
		coverage.VectorState = VectorActive
		return coverage
	}
	active, hasActive, activeErr := service.index.ActiveGeneration(ctx)
	if activeErr == nil && hasActive && active.Fingerprint != service.embedder.Generation().Fingerprint() {
		coverage.VectorState = VectorReplacing
		return coverage
	}
	if health.VectorState == "error" || health.LastError != "" || err != nil || activeErr != nil {
		coverage.VectorState = VectorUnavailable
		return coverage
	}
	coverage.VectorState = VectorBuilding
	return coverage
}
