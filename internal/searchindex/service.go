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

	"go.kenn.io/kit/search/hybrid"
	"go.kenn.io/kit/search/rrf"
	"go.kenn.io/kit/vector"

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
	GetSearchReviews(context.Context, []string) (map[string]storage.SearchReviewSource, error)
	GetRepoByID(int64) (*storage.Repo, error)
}

type searchRuntime interface {
	Health() HealthSnapshot
	Wake()
}

// Service ranks reviews from the disposable sidecar and loads the canonical
// review data for the page it returns from reviews.db.
type Service struct {
	store    canonicalSearchStore
	index    *Index
	embedder Embedder
	runtime  searchRuntime
}

// NewService constructs a transport-neutral review search service.
func NewService(store canonicalSearchStore, index *Index, embedder Embedder, runtime searchRuntime) *Service {
	if observer, ok := runtime.(embeddingObserver); ok && embedder != nil {
		embedder = observedEmbedder{Embedder: embedder, observer: observer}
	}
	return &Service{store: store, index: index, embedder: embedder, runtime: runtime}
}

const reciprocalRankConstant = 60

// pageGroup is one ranked panel group. Members are in preference order: the
// page shows the first one that is still a live, matching canonical review.
type pageGroup struct {
	Members []legMember
	// Fused groups carry their reciprocal-rank score and the evidence from
	// every leg. A single-leg group reports its member's own score.
	Fused   bool
	Score   float64
	Matches []string
	// LexicalExcerpts holds FTS snippets by document key, preferred over a
	// semantic chunk excerpt for a document both legs found.
	LexicalExcerpts map[string]string
}

// semanticQuery is the encoded query and the generation that serves it.
type semanticQuery struct {
	key    string
	vector vector.Vector
}

// Search runs the effective retrieval mode and loads the returned page from
// the canonical store.
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
		result.Hits, err = service.searchLexical(ctx, params, filters, target)
		if err != nil {
			return SearchResult{}, err
		}
	case ModeSemantic:
		hits, ceiling, err := service.searchSemantic(ctx, params, filters, target)
		if err != nil {
			return SearchResult{}, err
		}
		if semanticPageBounded(len(hits), params.Limit, ceiling) {
			return SearchResult{}, &ModeError{Status: 503, Reason: ReasonSemanticCeiling}
		}
		result.Hits = hits
		result.Partial = result.Partial || ceiling
	case ModeHybrid:
		hits, ceiling, err := service.searchHybrid(ctx, params, filters, target)
		if err != nil {
			if _, unavailable := errors.AsType[*ModeError](err); !unavailable || params.Mode != ModeAuto {
				return SearchResult{}, err
			}
			result.Mode = ModeLexical
			result.Degraded = true
			result.DegradedReason = service.semanticReason(err)
			result.Hits, err = service.searchLexical(ctx, params, filters, target)
			if err != nil {
				return SearchResult{}, err
			}
			break
		}
		bounded := semanticPageBounded(len(hits), params.Limit, ceiling)
		if bounded && params.Mode != ModeAuto {
			return SearchResult{}, &ModeError{Status: 503, Reason: ReasonSemanticCeiling}
		}
		result.Hits = hits
		result.Partial = result.Partial || ceiling
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
		if service.health().EmbeddingsConfigured {
			reason := service.semanticReason(nil)
			if requested == ModeAuto {
				return ModeLexical, true, reason, nil
			}
			return "", false, "", &ModeError{Status: 503, Reason: reason}
		}
		if requested == ModeAuto {
			return ModeLexical, false, "", nil
		}
		return "", false, "", &ModeError{Status: 400, Reason: ReasonEmbeddingsUnconfigured}
	}
	_, available, err := service.index.ServingGeneration(ctx, service.embedder.Space())
	if err != nil {
		if requested == ModeAuto {
			return ModeLexical, true, service.semanticReason(err), nil
		}
		return "", false, "", service.unavailableError(err)
	}
	if !available {
		if requested == ModeAuto {
			return ModeLexical, true, service.semanticReason(nil), nil
		}
		return "", false, "", &ModeError{Status: 503, Reason: service.semanticReason(nil)}
	}
	if requested == ModeSemantic {
		return ModeSemantic, false, "", nil
	}
	return ModeHybrid, false, "", nil
}

func (service *Service) searchLexical(
	ctx context.Context, params SearchParams, filters SearchFilters, target int,
) ([]SearchHit, error) {
	groups, err := service.index.SearchLexical(ctx, params.Query, filters, target)
	if err != nil {
		return nil, err
	}
	return service.page(ctx, singleLegPage(groups), filters, params.Limit)
}

// prepareSemantic encodes the query and resolves the serving generation.
// Every failure is a semantic-unavailable mode error.
func (service *Service) prepareSemantic(ctx context.Context, query string) (semanticQuery, error) {
	queryCtx, cancel := context.WithTimeout(ctx, queryEmbeddingTimeout)
	encoded, err := vector.EncodeOne(queryCtx, encodeQueries(service.embedder), query)
	cancel()
	if err != nil {
		return semanticQuery{}, service.unavailableError(err)
	}
	serving, ok, err := service.index.ServingGeneration(ctx, service.embedder.Space())
	if err != nil {
		return semanticQuery{}, service.unavailableError(err)
	}
	if !ok {
		return semanticQuery{}, &ModeError{Status: 503, Reason: service.semanticReason(nil)}
	}
	return semanticQuery{key: serving.Key, vector: encoded}, nil
}

// searchSemantic widens the candidate window once, to the deep ceiling, when
// the first window cannot fill the page. The bool reports that eligible
// neighbors may remain beyond that ceiling.
func (service *Service) searchSemantic(
	ctx context.Context, params SearchParams, filters SearchFilters, target int,
) ([]SearchHit, bool, error) {
	query, err := service.prepareSemantic(ctx, params.Query)
	if err != nil {
		return nil, false, err
	}
	groups, err := service.index.SearchSemantic(ctx, query.key, query.vector, target, filters)
	if err != nil {
		return nil, false, service.unavailableError(err)
	}
	deep := len(groups) < target
	ceiling := false
	if deep {
		if groups, ceiling, err = service.searchSemanticDeep(ctx, query, filters); err != nil {
			return nil, false, err
		}
	}
	hits, err := service.page(ctx, singleLegPage(groups), filters, params.Limit)
	if err != nil || deep || len(hits) >= params.Limit {
		return hits, ceiling, err
	}
	if groups, ceiling, err = service.searchSemanticDeep(ctx, query, filters); err != nil {
		return nil, false, err
	}
	hits, err = service.page(ctx, singleLegPage(groups), filters, params.Limit)
	return hits, ceiling, err
}

func (service *Service) searchSemanticDeep(
	ctx context.Context, query semanticQuery, filters SearchFilters,
) ([]legGroup, bool, error) {
	probeScore, hasProbe, err := service.index.SemanticProbe(ctx, query.key, query.vector, semanticDeepLimit)
	if err != nil {
		return nil, false, service.unavailableError(err)
	}
	groups, err := service.index.SearchSemantic(ctx, query.key, query.vector, semanticDeepLimit, filters)
	if err != nil {
		return nil, false, service.unavailableError(err)
	}
	return groups, semanticCeilingReached(probeScore, hasProbe), nil
}

// searchHybrid fuses the lexical and semantic legs with kit's grouped
// reciprocal rank fusion, widening the semantic window like searchSemantic.
func (service *Service) searchHybrid(
	ctx context.Context, params SearchParams, filters SearchFilters, target int,
) ([]SearchHit, bool, error) {
	query, err := service.prepareSemantic(ctx, params.Query)
	if err != nil {
		return nil, false, err
	}
	groups, semanticGroups, err := service.fuse(ctx, params.Query, query, filters, target, target)
	if err != nil {
		return nil, false, err
	}
	deep := semanticGroups < target
	ceiling := false
	if deep {
		if groups, ceiling, err = service.fuseDeep(ctx, params.Query, query, filters, target); err != nil {
			return nil, false, err
		}
	}
	hits, err := service.page(ctx, groups, filters, params.Limit)
	if err != nil || deep || len(hits) >= params.Limit {
		return hits, ceiling, err
	}
	if groups, ceiling, err = service.fuseDeep(ctx, params.Query, query, filters, target); err != nil {
		return nil, false, err
	}
	hits, err = service.page(ctx, groups, filters, params.Limit)
	return hits, ceiling, err
}

func (service *Service) fuseDeep(
	ctx context.Context, text string, query semanticQuery, filters SearchFilters, target int,
) ([]pageGroup, bool, error) {
	probeScore, hasProbe, err := service.index.SemanticProbe(ctx, query.key, query.vector, semanticDeepLimit)
	if err != nil {
		return nil, false, service.unavailableError(err)
	}
	groups, _, err := service.fuse(ctx, text, query, filters, target, semanticDeepLimit)
	return groups, semanticCeilingReached(probeScore, hasProbe), err
}

// fuse runs both legs through hybrid.RunGroups and returns the fused groups
// in rank order, plus how many groups the semantic leg found.
func (service *Service) fuse(
	ctx context.Context, text string, query semanticQuery, filters SearchFilters,
	lexicalGroups, semanticCandidates int,
) ([]pageGroup, int, error) {
	lexicalLeg, err := service.index.lexicalLeg(text, filters, lexicalGroups)
	if err != nil {
		return nil, 0, err
	}
	semanticLeg, err := service.index.semanticLeg(ctx, query.key, query.vector, semanticCandidates, filters)
	if err != nil {
		return nil, 0, service.unavailableError(err)
	}
	fused, err := hybrid.RunGroups(ctx, service.index.db, reciprocalRankConstant, []hybrid.GroupLeg[string, legMember]{
		{Name: MatchLexical, Weight: 1, Query: lexicalLeg, Scan: scanLexicalRow},
		{Name: MatchSemantic, Weight: 1, Query: semanticLeg, Scan: scanSemanticRow},
	})
	if err != nil {
		return nil, 0, service.unavailableError(err)
	}
	semanticGroups := 0
	groups := make([]pageGroup, 0, len(fused.Hits))
	for _, hit := range fused.Hits {
		group := fusedPageGroup(hit)
		if slices.ContainsFunc(hit.Contributions, func(c rrf.Contribution) bool { return c.Leg == MatchSemantic }) {
			semanticGroups++
		}
		groups = append(groups, group)
	}
	// Equal fused scores keep canonical tie order: newer review, then higher
	// job, then document key, judged on each group's representative.
	slices.SortStableFunc(groups, func(left, right pageGroup) int {
		if left.Score != right.Score {
			if left.Score > right.Score {
				return -1
			}
			return 1
		}
		return compareMemberTie(left.Members[0], right.Members[0])
	})
	return groups, semanticGroups, nil
}

// fusedPageGroup orders a fused group's members: the leg that ranked the
// group best goes first, lexical on a tie, each leg keeping its own order.
func fusedPageGroup(hit rrf.GroupHit[string, legMember]) pageGroup {
	legRank := make(map[string]int, len(hit.Contributions))
	for _, contribution := range hit.Contributions {
		legRank[contribution.Leg] = contribution.Rank
	}
	legs := []string{MatchLexical, MatchSemantic}
	if rank, ok := legRank[MatchSemantic]; ok {
		if lexicalRank, lexicalOK := legRank[MatchLexical]; !lexicalOK || rank < lexicalRank {
			legs = []string{MatchSemantic, MatchLexical}
		}
	}
	group := pageGroup{Fused: true, Score: hit.Score, LexicalExcerpts: map[string]string{}}
	var matches []string
	for _, leg := range legs {
		first := true
		for _, alternate := range hit.Alternates {
			if alternate.Leg != leg {
				continue
			}
			group.Members = append(group.Members, alternate.Member)
			if first {
				matches = append(matches, alternate.Member.MatchedIn()...)
				first = false
			}
			if leg == MatchLexical {
				group.LexicalExcerpts[alternate.Member.DocKey] = alternate.Member.Excerpt
			}
		}
	}
	group.Matches = stableMatches(matches)
	return group
}

func singleLegPage(groups []legGroup) []pageGroup {
	page := make([]pageGroup, len(groups))
	for i, group := range groups {
		page[i] = pageGroup{Members: group.Members}
	}
	return page
}

func compareMemberTie(left, right legMember) int {
	if comparison := strings.Compare(right.FinishedKey, left.FinishedKey); comparison != 0 {
		return comparison
	}
	if left.JobID != right.JobID {
		if left.JobID > right.JobID {
			return -1
		}
		return 1
	}
	return strings.Compare(left.DocKey, right.DocKey)
}

func stableMatches(matches []string) []string {
	ordered := make([]string, 0, 3)
	for _, match := range []string{MatchIdentifier, MatchLexical, MatchSemantic} {
		if slices.Contains(matches, match) {
			ordered = append(ordered, match)
		}
	}
	return ordered
}

// page loads up to limit hits from the canonical store in group order. A group
// shows its first member that is still a live review matching filters; when
// none is, the sidecar is behind reviews.db and the reconciler is woken.
func (service *Service) page(
	ctx context.Context, groups []pageGroup, filters SearchFilters, limit int,
) ([]SearchHit, error) {
	hits := make([]SearchHit, 0, min(limit, len(groups)))
	repoPaths := map[int64]string{}
	stale := false
	// Load groups in candidate-target batches: stale groups are rare, so the
	// first batch usually fills the page with one canonical query.
	batchSize := candidateTarget(limit)
	for next := 0; next < len(groups) && len(hits) < limit; {
		batch := groups[next:min(next+batchSize, len(groups))]
		next += len(batch)
		var keys []string
		for _, group := range batch {
			for _, member := range group.Members {
				keys = append(keys, member.DocKey)
			}
		}
		sources, err := service.store.GetSearchReviews(ctx, keys)
		if err != nil {
			return nil, fmt.Errorf("load search reviews: %w", err)
		}
		type choice struct {
			group  pageGroup
			member legMember
			source storage.SearchReviewSource
		}
		var chosen []choice
		var chunkDocs []string
		for _, group := range batch {
			found := false
			for _, member := range group.Members {
				source, ok := sources[member.DocKey]
				if !ok || !sourceMatchesFilters(source, filters) {
					stale = true
					continue
				}
				chosen = append(chosen, choice{group: group, member: member, source: source})
				if _, lexical := group.LexicalExcerpts[member.DocKey]; member.Semantic && !lexical {
					chunkDocs = append(chunkDocs, member.DocKey)
				}
				found = true
				break
			}
			if !found {
				stale = true
			}
		}
		contents, err := service.index.mirrorContents(ctx, chunkDocs)
		if err != nil {
			return nil, err
		}
		for _, item := range chosen {
			if len(hits) == limit {
				break
			}
			repoPath, ok := repoPaths[item.source.RepoID]
			if !ok {
				repo, err := service.store.GetRepoByID(item.source.RepoID)
				if errors.Is(err, sql.ErrNoRows) {
					stale = true
					continue
				}
				if err != nil {
					return nil, fmt.Errorf("load search repository: %w", err)
				}
				repoPath = repo.RootPath
				repoPaths[item.source.RepoID] = repoPath
			}
			hits = append(hits, searchHit(item.group, item.member, item.source, repoPath, contents))
		}
	}
	if stale {
		service.wake()
	}
	return hits, nil
}

func searchHit(
	group pageGroup, member legMember, source storage.SearchReviewSource, repoPath string,
	contents map[string]string,
) SearchHit {
	hit := SearchHit{
		JobID: source.JobID, JobUUID: source.JobUUID,
		ReviewID: source.ReviewID, ReviewUUID: source.ReviewUUID,
		RepoName: source.RepoName, RepoPath: repoPath,
		GitRef: source.GitRef, CommitSHA: source.CommitSHA,
		CommitSubject: source.CommitSubject, Branch: source.Branch,
		ReviewType: source.ReviewType, PanelRole: source.PanelRole,
		Agent: source.Agent, Verdict: source.Verdict, Closed: source.Closed,
		FinishedAt: source.FinishedAt, Score: member.Score, MatchedIn: member.MatchedIn(),
	}
	if group.Fused {
		hit.Score = group.Score
		hit.MatchedIn = group.Matches
	}
	switch excerpt, lexical := group.LexicalExcerpts[member.DocKey]; {
	case lexical && excerpt != "":
		hit.Excerpt = sanitizeLexicalExcerpt(excerpt)
	case member.Semantic:
		chunks := vector.Split(contents[member.DocKey], vector.SplitOptions{
			MaxRunes: searchChunkRunes, Overlap: searchChunkOverlap,
		})
		if member.ChunkIndex >= 0 && member.ChunkIndex < len(chunks) {
			hit.Excerpt = sanitizePlainExcerpt(chunks[member.ChunkIndex].Text)
		}
	default:
		hit.Excerpt = sanitizeLexicalExcerpt(member.Excerpt)
	}
	return hit
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

func semanticCeilingReached(probeScore float32, hasProbe bool) bool {
	return hasProbe && float64(probeScore) >= semanticCosineFloor
}

func semanticPageBounded(resultCount, requestedLimit int, ceilingReached bool) bool {
	return resultCount < requestedLimit && ceilingReached
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

func (service *Service) semanticReason(err error) string {
	if reason := authenticationReason(err); reason != "" {
		return reason
	}
	if reason := service.health().CredentialReason; reason != "" {
		return reason
	}
	return ReasonSemanticUnavailable
}

func (service *Service) unavailableError(err error) error {
	if _, ok := errors.AsType[*ModeError](err); ok {
		return err
	}
	return &ModeError{Status: 503, Reason: service.semanticReason(err), Err: err}
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
		EmbeddingsConfigured: health.EmbeddingsConfigured,
		EmbeddingBacklog:     health.EmbeddingBacklog, Skipped: health.Skipped,
		VectorState: VectorDisabled,
	}
	if service.embedder == nil {
		if health.EmbeddingsConfigured {
			coverage.VectorState = VectorUnavailable
		}
		coverage.EmbeddingBacklog = 0
		return coverage
	}
	_, available, err := service.index.ServingGeneration(ctx, service.embedder.Space())
	if err == nil && available {
		coverage.VectorState = VectorActive
		return coverage
	}
	_, hasActive, activeErr := service.index.ActiveGeneration(ctx)
	if err == nil && activeErr == nil && hasActive {
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
