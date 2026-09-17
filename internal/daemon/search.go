package daemon

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/roborev/internal/searchindex"
	"go.kenn.io/roborev/internal/storage"
)

const maxSearchQueryRunes = 2000

// errSearchRepoNotFound reports that a search repo identifier matches no
// repository known to the daemon.
var errSearchRepoNotFound = errors.New("repository not found")

// errSearchRepoLookup reports that a repository lookup failed before it
// could determine whether the identifier exists; the storage cause is
// wrapped alongside it.
var errSearchRepoLookup = errors.New("resolve repository")

type reviewSearcher interface {
	Search(context.Context, searchindex.SearchParams) (searchindex.SearchResult, error)
}

type searchReconciler interface {
	Run(context.Context) error
	Wake()
	Health() searchindex.HealthSnapshot
}

// WithSearch wires the review search service and its background reconciler
// into the daemon lifecycle.
func WithSearch(service *searchindex.Service, reconciler *searchindex.Reconciler) ServerOption {
	return func(server *Server) {
		server.search = service
		server.searchReconciler = reconciler
	}
}

func (s *Server) startSearch(ctx context.Context) {
	s.searchMu.Lock()
	if s.searchReconciler == nil || s.searchStopped || s.searchCancel != nil {
		s.searchMu.Unlock()
		return
	}

	searchCtx, cancel := context.WithCancel(ctx)
	subscriberID, events := s.broadcaster.Subscribe("")
	s.searchCancel = cancel
	s.searchSubscriberID = subscriberID
	s.searchWG.Add(2)
	go func() {
		defer s.searchWG.Done()
		_ = s.searchReconciler.Run(searchCtx)
	}()
	go func() {
		defer s.searchWG.Done()
		for {
			select {
			case <-searchCtx.Done():
				return
			case event, ok := <-events:
				if !ok {
					return
				}
				if isSearchWakeEvent(event.Type) {
					s.searchReconciler.Wake()
				}
			}
		}
	}()
	s.searchMu.Unlock()
	if s.activityLog != nil {
		s.activityLog.Log("search.started", "search", "search reconciliation started", nil)
	}
}

func isSearchWakeEvent(eventType string) bool {
	switch eventType {
	case "review.completed", "review.closed", "review.reopened", "review.remapped", "review.commented":
		return true
	default:
		return false
	}
}

func (s *Server) stopSearch() {
	s.searchMu.Lock()
	s.searchStopped = true
	cancel := s.searchCancel
	subscriberID := s.searchSubscriberID
	s.searchCancel = nil
	s.searchSubscriberID = 0
	s.searchMu.Unlock()

	if cancel != nil {
		cancel()
		if subscriberID != 0 {
			s.broadcaster.Unsubscribe(subscriberID)
		}
	}
	s.searchWG.Wait()
	if cancel != nil && s.activityLog != nil {
		s.activityLog.Log("search.stopped", "search", "search reconciliation stopped", nil)
	}
}

func (s *Server) humaSearchReviews(
	ctx context.Context, input *SearchInput,
) (*SearchOutput, error) {
	if s.search == nil {
		return nil, huma.Error503ServiceUnavailable("search is unavailable")
	}

	params, err := s.searchParams(input)
	if err != nil {
		switch {
		case errors.Is(err, errSearchRepoNotFound):
			return nil, huma.Error400BadRequest(errSearchRepoNotFound.Error())
		case errors.Is(err, errSearchRepoLookup):
			if s.errorLog != nil {
				s.errorLog.LogError(
					"server", fmt.Sprintf("search repository lookup failed: %v", err), 0,
				)
			}
			return nil, huma.Error500InternalServerError("repository lookup failed")
		}
		return nil, huma.Error400BadRequest(err.Error())
	}
	result, err := s.search.Search(ctx, params)
	if err != nil {
		if modeErr, ok := errors.AsType[*searchindex.ModeError](err); ok {
			status := http.StatusServiceUnavailable
			if modeErr.Status == http.StatusBadRequest {
				status = http.StatusBadRequest
			}
			return nil, huma.NewError(status, modeErr.Reason)
		}
		return nil, huma.Error500InternalServerError("search failed")
	}

	response := searchResponseFromResult(result)
	if response.Hits == nil {
		response.Hits = []SearchHit{}
	}
	return &SearchOutput{Body: response}, nil
}

func (s *Server) searchParams(input *SearchInput) (searchindex.SearchParams, error) {
	query := strings.TrimSpace(input.Query)
	if query == "" {
		return searchindex.SearchParams{}, fmt.Errorf("q is required")
	}
	if utf8.RuneCountInString(query) > maxSearchQueryRunes {
		return searchindex.SearchParams{}, fmt.Errorf("q must be at most %d runes", maxSearchQueryRunes)
	}

	mode := searchindex.SearchMode(input.Mode)
	if mode == "" {
		mode = searchindex.ModeAuto
	}
	switch mode {
	case searchindex.ModeAuto, searchindex.ModeLexical, searchindex.ModeHybrid, searchindex.ModeSemantic:
	default:
		return searchindex.SearchParams{}, fmt.Errorf("invalid mode")
	}

	state := input.State
	if state == "" {
		state = searchindex.StateAll
	}
	switch state {
	case searchindex.StateAll, searchindex.StateOpen, searchindex.StateClosed:
	default:
		return searchindex.SearchParams{}, fmt.Errorf("invalid state")
	}
	if input.Verdict != "" && input.Verdict != "pass" && input.Verdict != "fail" {
		return searchindex.SearchParams{}, fmt.Errorf("invalid verdict")
	}

	limit := 20
	if input.Limit != "" {
		parsed, err := strconv.Atoi(string(input.Limit))
		if err != nil {
			return searchindex.SearchParams{}, fmt.Errorf("limit must be between 1 and 100")
		}
		limit = parsed
	}
	if limit < 1 || limit > 100 {
		return searchindex.SearchParams{}, fmt.Errorf("limit must be between 1 and 100")
	}

	since, err := parseSearchSince(input.Since, s.searchNow())
	if err != nil {
		return searchindex.SearchParams{}, err
	}
	repoID, err := s.resolveSearchRepo(input.Repo)
	if err != nil {
		return searchindex.SearchParams{}, err
	}
	return searchindex.SearchParams{
		Query: query, Mode: mode, RepoID: repoID, Branch: input.Branch,
		Since: since, Verdict: input.Verdict, State: state, Limit: limit,
	}, nil
}

func parseSearchSince(raw string, now time.Time) (*time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	if duration, err := time.ParseDuration(raw); err == nil && duration > 0 {
		since := now.Add(-duration)
		return &since, nil
	}
	timestamp, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return nil, fmt.Errorf("since must be a Go duration or RFC3339 timestamp")
	}
	return &timestamp, nil
}

func (s *Server) resolveSearchRepo(identifier string) (int64, error) {
	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		return 0, nil
	}
	repo, err := s.db.FindRepo(identifier)
	if errors.Is(err, sql.ErrNoRows) {
		repo, err = s.db.GetRepoByIdentity(identifier)
	}
	if err != nil {
		return 0, fmt.Errorf("%w: %w", errSearchRepoLookup, err)
	}
	if repo == nil {
		return 0, errSearchRepoNotFound
	}
	return repo.ID, nil
}

func searchResponseFromResult(result searchindex.SearchResult) SearchResponse {
	hits := make([]SearchHit, 0, len(result.Hits))
	for _, hit := range result.Hits {
		hits = append(hits, SearchHit{
			JobID: hit.JobID, JobUUID: hit.JobUUID, ReviewID: hit.ReviewID,
			ReviewUUID: hit.ReviewUUID, RepoName: hit.RepoName, RepoPath: hit.RepoPath,
			GitRef: hit.GitRef, CommitSHA: hit.CommitSHA, CommitSubject: hit.CommitSubject,
			Branch: hit.Branch, ReviewType: hit.ReviewType, PanelRole: hit.PanelRole,
			Agent: hit.Agent, Verdict: hit.Verdict, Closed: hit.Closed,
			FinishedAt: hit.FinishedAt, Score: hit.Score,
			MatchedIn: append([]string(nil), hit.MatchedIn...), Excerpt: hit.Excerpt,
		})
	}
	return SearchResponse{
		Query: result.Query, Mode: string(result.Mode), Degraded: result.Degraded,
		DegradedReason: result.DegradedReason, Bounded: result.Bounded,
		BoundedReason: result.BoundedReason, Partial: result.Partial,
		Coverage: SearchCoverage{
			MirrorComplete:       result.Coverage.MirrorComplete,
			MirrorBacklog:        result.Coverage.MirrorBacklog,
			EmbeddingsConfigured: result.Coverage.EmbeddingsConfigured,
			VectorState:          result.Coverage.VectorState,
			EmbeddingBacklog:     result.Coverage.EmbeddingBacklog,
			Skipped:              result.Coverage.Skipped,
		},
		Hits: hits,
	}
}

func searchHealthFromSnapshot(snapshot searchindex.HealthSnapshot) *storage.SearchHealth {
	return &storage.SearchHealth{
		Indexed: snapshot.Indexed, MirrorComplete: snapshot.MirrorComplete,
		MirrorBacklog:        snapshot.MirrorBacklog,
		EmbeddingsConfigured: snapshot.EmbeddingsConfigured,
		Embedded:             snapshot.Embedded, Skipped: snapshot.Skipped,
		EmbeddingBacklog: snapshot.EmbeddingBacklog,
		VectorState:      publicSearchVectorState(snapshot),
		ActiveGeneration: snapshot.ActiveGeneration,
		LastSuccessAt:    snapshot.LastSuccessAt, LastProgressAt: snapshot.LastProgressAt,
		RatePerSecond: snapshot.RatePerSecond, ETASeconds: snapshot.ETASeconds,
		LastError: snapshot.LastError, LastErrorStatus: snapshot.LastErrorStatus,
	}
}

func publicSearchVectorState(snapshot searchindex.HealthSnapshot) string {
	if !snapshot.EmbeddingsConfigured || snapshot.VectorState == "unconfigured" {
		return searchindex.VectorDisabled
	}
	if snapshot.VectorState == "building" && snapshot.Generation != "" && snapshot.ActiveGeneration != "" &&
		snapshot.Generation != snapshot.ActiveGeneration {
		return searchindex.VectorReplacing
	}
	switch snapshot.VectorState {
	case "ready":
		if snapshot.ActiveGeneration == "" {
			return searchindex.VectorUnavailable
		}
		return searchindex.VectorActive
	case "building":
		return searchindex.VectorBuilding
	case "error":
		return searchindex.VectorUnavailable
	default:
		return searchindex.VectorUnavailable
	}
}
