package searchindex

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"

	"go.kenn.io/kit/vector"
)

const (
	semanticDeepLimit   = 1000
	semanticCosineFloor = 0.30
	semanticProbeReason = "semantic candidate ceiling exhausted"
)

// SearchFilters are applied first to mirrored candidates and again to the
// canonical documents used to hydrate results.
type SearchFilters struct {
	RepoID  int64
	Branch  string
	Since   *time.Time
	Verdict string
	State   string
}

type rankedCandidate struct {
	DocKey        string
	GroupKey      string
	ReviewID      int64
	ReviewUUID    string
	JobID         int64
	JobUUID       string
	RepoID        int64
	RepoName      string
	Branch        string
	GitRef        string
	CommitSHA     string
	FinishedAt    time.Time
	Verdict       string
	Closed        bool
	PanelRole     string
	Content       string
	ContentHash   string
	ChunkIndex    int
	Rank          int
	Score         float64
	MatchedIn     []string
	Excerpt       string
	identifier    bool
	repoPath      string
	commitSubject string
	reviewType    string
	agent         string
}

func candidateTarget(limit int) int {
	return min(max(limit*3, 50), 200)
}

func literalFTSQuery(query string) string {
	words := strings.Fields(strings.TrimSpace(query))
	if !slices.ContainsFunc(words, containsSearchToken) {
		return ""
	}
	quoted := make([]string, 0, len(words))
	for _, word := range words {
		quoted = append(quoted, `"`+strings.ReplaceAll(word, `"`, `""`)+`"`)
	}
	return strings.Join(quoted, " ")
}

func containsSearchToken(value string) bool {
	return strings.IndexFunc(value, func(r rune) bool {
		return unicode.IsLetter(r) || unicode.IsNumber(r)
	}) >= 0
}

// SearchLexical returns at most limit panel-grouped lexical candidates.
// Exact identifiers and full or short commit SHA matches precede FTS rank.
func (index *Index) SearchLexical(
	ctx context.Context, query string, filters SearchFilters, limit int,
) ([]rankedCandidate, error) {
	query = strings.TrimSpace(query)
	if query == "" || limit <= 0 {
		return nil, nil
	}

	exact, err := index.searchExactIdentifiers(ctx, query, filters, limit)
	if err != nil {
		return nil, err
	}
	var lexical []rankedCandidate
	if ftsQuery := literalFTSQuery(query); ftsQuery != "" {
		lexical, err = index.searchFTS(ctx, ftsQuery, filters, limit)
		if err != nil {
			return nil, err
		}
	}

	byGroup := make(map[string]rankedCandidate, len(exact)+len(lexical))
	for _, candidate := range lexical {
		byGroup[candidate.GroupKey] = candidate
	}
	for _, candidate := range exact {
		if previous, ok := byGroup[candidate.GroupKey]; ok {
			candidate.MatchedIn = stableMatches(append(candidate.MatchedIn, previous.MatchedIn...))
			if candidate.Excerpt == "" {
				candidate.Excerpt = previous.Excerpt
			}
		}
		byGroup[candidate.GroupKey] = candidate
	}

	merged := make([]rankedCandidate, 0, len(byGroup))
	for _, candidate := range byGroup {
		merged = append(merged, candidate)
	}
	slices.SortFunc(merged, compareLexicalCandidates)
	if len(merged) > limit {
		merged = merged[:limit]
	}
	for i := range merged {
		merged[i].Rank = i + 1
	}
	return merged, nil
}

func (index *Index) searchExactIdentifiers(
	ctx context.Context, query string, filters SearchFilters, limit int,
) ([]rankedCandidate, error) {
	var statement strings.Builder
	statement.WriteString(`
		WITH matches AS MATERIALIZED (
			SELECT m.doc_key, m.review_id, COALESCE(m.review_uuid, '') AS review_uuid,
			       m.job_id, COALESCE(m.job_uuid, '') AS job_uuid, m.group_key,
			       m.repo_id, m.repo_name, COALESCE(m.branch, '') AS branch, m.git_ref,
			       COALESCE(m.commit_sha, '') AS commit_sha,
			       COALESCE(m.finished_at, '') AS finished_at,
			       COALESCE(m.verdict, '') AS verdict, m.closed,
			       COALESCE(m.panel_role, '') AS panel_role,
			       m.content, m.content_hash
			  FROM review_mirror m
			  JOIN review_fts f ON f.doc_key = m.doc_key
			 WHERE (
				instr(char(10) || lower(f.identifiers) || char(10),
				      char(10) || lower(?) || char(10)) > 0`)
	args := []any{query}
	if isSHAPrefix(query) {
		statement.WriteString(` OR lower(COALESCE(m.commit_sha, '')) LIKE lower(?) || '%'`)
		args = append(args, query)
	}
	statement.WriteString(")")
	appendSQLFilters(&statement, &args, "m", filters)
	statement.WriteString(`
		), ranked AS (
			SELECT matches.*,
			       row_number() OVER (
				PARTITION BY group_key
				ORDER BY finished_at DESC, job_id DESC, doc_key ASC
			       ) AS group_rank
			  FROM matches
		)
		SELECT doc_key, review_id, review_uuid, job_id, job_uuid, group_key,
		       repo_id, repo_name, branch, git_ref, commit_sha, finished_at,
		       verdict, closed, panel_role, content, content_hash
		  FROM ranked WHERE group_rank = 1
		 ORDER BY finished_at DESC, job_id DESC, doc_key ASC
		 LIMIT ?`)
	args = append(args, limit)
	rows, err := index.db.QueryContext(ctx, statement.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("search exact review identifiers: %w", err)
	}
	defer func() { _ = rows.Close() }()
	candidates, err := scanCandidates(rows)
	if err != nil {
		return nil, err
	}
	for i := range candidates {
		candidates[i].identifier = true
		candidates[i].Score = 1
		candidates[i].MatchedIn = []string{MatchIdentifier}
		candidates[i].Excerpt = candidates[i].Content
	}
	return candidates, nil
}

func (index *Index) searchFTS(
	ctx context.Context, query string, filters SearchFilters, limit int,
) ([]rankedCandidate, error) {
	var statement strings.Builder
	statement.WriteString(`
		WITH matches AS MATERIALIZED (
			SELECT m.doc_key, m.review_id, COALESCE(m.review_uuid, '') AS review_uuid,
			       m.job_id, COALESCE(m.job_uuid, '') AS job_uuid, m.group_key,
			       m.repo_id, m.repo_name, COALESCE(m.branch, '') AS branch, m.git_ref,
			       COALESCE(m.commit_sha, '') AS commit_sha,
			       COALESCE(m.finished_at, '') AS finished_at,
			       COALESCE(m.verdict, '') AS verdict, m.closed,
			       COALESCE(m.panel_role, '') AS panel_role,
			       m.content, m.content_hash, bm25(review_fts) AS lexical_score,
			       snippet(review_fts, 1, char(57344), char(57345), ' … ', 32) AS excerpt
			  FROM review_fts
			  JOIN review_mirror m ON m.doc_key = review_fts.doc_key
			 WHERE review_fts MATCH ?`)
	args := []any{query}
	appendSQLFilters(&statement, &args, "m", filters)
	statement.WriteString(`
		), ranked AS (
			SELECT matches.*,
			       row_number() OVER (
				PARTITION BY group_key
				ORDER BY lexical_score ASC, finished_at DESC, job_id DESC, doc_key ASC
			       ) AS group_rank
			  FROM matches
		)
		SELECT doc_key, review_id, review_uuid, job_id, job_uuid, group_key,
		       repo_id, repo_name, branch, git_ref, commit_sha, finished_at,
		       verdict, closed, panel_role, content, content_hash,
		       lexical_score, excerpt
		  FROM ranked WHERE group_rank = 1
		 ORDER BY lexical_score ASC, finished_at DESC, job_id DESC, doc_key ASC
		 LIMIT ?`)
	args = append(args, limit)
	rows, err := index.db.QueryContext(ctx, statement.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("search review text: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var candidates []rankedCandidate
	for rows.Next() {
		candidate, finishedAt, closed, err := scanCandidateBase(rows,
			&candidateScoreExcerptScanner{})
		if err != nil {
			return nil, fmt.Errorf("scan lexical review candidate: %w", err)
		}
		candidate.FinishedAt = parseCandidateTime(finishedAt)
		candidate.Closed = closed != 0
		candidate.Score = -candidate.Score
		candidate.MatchedIn = []string{MatchLexical}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("scan lexical review candidates: %w", err)
	}
	return candidates, nil
}

type rowScanner interface {
	Scan(...any) error
}

type candidateRows interface {
	rowScanner
	Next() bool
	Err() error
}

type candidateScoreExcerptScanner struct{}

func scanCandidateBase(scanner rowScanner, extra *candidateScoreExcerptScanner) (rankedCandidate, string, int, error) {
	var candidate rankedCandidate
	var finishedAt string
	var closed int
	destinations := []any{
		&candidate.DocKey, &candidate.ReviewID, &candidate.ReviewUUID,
		&candidate.JobID, &candidate.JobUUID, &candidate.GroupKey,
		&candidate.RepoID, &candidate.RepoName, &candidate.Branch, &candidate.GitRef,
		&candidate.CommitSHA, &finishedAt, &candidate.Verdict, &closed,
		&candidate.PanelRole, &candidate.Content, &candidate.ContentHash,
	}
	if extra != nil {
		destinations = append(destinations, &candidate.Score, &candidate.Excerpt)
	}
	err := scanner.Scan(destinations...)
	return candidate, finishedAt, closed, err
}

func scanCandidates(rows candidateRows) ([]rankedCandidate, error) {
	var candidates []rankedCandidate
	for rows.Next() {
		candidate, finishedAt, closed, err := scanCandidateBase(rows, nil)
		if err != nil {
			return nil, fmt.Errorf("scan review candidate: %w", err)
		}
		candidate.FinishedAt = parseCandidateTime(finishedAt)
		candidate.Closed = closed != 0
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("scan review candidates: %w", err)
	}
	return candidates, nil
}

func appendSQLFilters(statement *strings.Builder, args *[]any, alias string, filters SearchFilters) {
	if filters.RepoID != 0 {
		statement.WriteString(" AND " + alias + ".repo_id = ?")
		*args = append(*args, filters.RepoID)
	}
	if filters.Branch != "" {
		statement.WriteString(" AND " + alias + ".branch = ?")
		*args = append(*args, filters.Branch)
	}
	if filters.Since != nil {
		statement.WriteString(" AND " + alias + ".finished_at >= ?")
		*args = append(*args, filters.Since.UTC().Format(time.RFC3339Nano))
	}
	if filters.Verdict != "" {
		statement.WriteString(" AND " + alias + ".verdict = ?")
		*args = append(*args, filters.Verdict)
	}
	switch filters.State {
	case StateOpen:
		statement.WriteString(" AND " + alias + ".closed = 0")
	case StateClosed:
		statement.WriteString(" AND " + alias + ".closed = 1")
	}
}

func isSHAPrefix(query string) bool {
	if len(query) < 7 || len(query) > 40 {
		return false
	}
	for _, value := range query {
		if !unicode.Is(unicode.ASCII_Hex_Digit, value) {
			return false
		}
	}
	return true
}

func compareLexicalCandidates(left, right rankedCandidate) int {
	if left.identifier != right.identifier {
		if left.identifier {
			return -1
		}
		return 1
	}
	if left.Score != right.Score {
		if left.Score > right.Score {
			return -1
		}
		return 1
	}
	return compareCandidateTie(left, right)
}

func compareCandidateTie(left, right rankedCandidate) int {
	if comparison := right.FinishedAt.Compare(left.FinishedAt); comparison != 0 {
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

func parseCandidateTime(value string) time.Time {
	if value == "" {
		return time.Time{}
	}
	parsed, _ := time.Parse(time.RFC3339Nano, value)
	return parsed
}

// QueryWithProbe returns fresh hits from the first limit raw neighbors plus,
// when present, the raw hit beyond that ceiling. The probe is observed before
// the mirror and generation-freshness joins that may suppress candidate rows.
func (index *Index) QueryWithProbe(
	ctx context.Context, key string, query vector.Vector, limit int,
) ([]semanticHit, *vector.Hit[string], error) {
	if limit <= 0 {
		return nil, nil, nil
	}
	probe, err := index.queryRawProbe(ctx, key, query, limit)
	if err != nil {
		return nil, nil, err
	}
	hits, err := index.QueryGeneration(ctx, key, query, limit)
	if err != nil {
		return nil, nil, err
	}
	return hits, probe, nil
}

func (index *Index) queryRawProbe(
	ctx context.Context, key string, query vector.Vector, limit int,
) (*vector.Hit[string], error) {
	var ordinal int64
	var dimensions int
	err := index.db.QueryRowContext(ctx, `
		SELECT ordinal, dimension
		  FROM review_vectors_generations WHERE gen_key = ?`, key).Scan(&ordinal, &dimensions)
	if err != nil {
		return nil, fmt.Errorf("find raw semantic generation: %w", err)
	}
	if len(query) != dimensions {
		return nil, fmt.Errorf("query has %d dimensions, generation expects %d", len(query), dimensions)
	}
	encoded, err := json.Marshal(query)
	if err != nil {
		return nil, fmt.Errorf("serialize raw semantic query: %w", err)
	}
	statement := fmt.Sprintf(`
		SELECT distance FROM review_vectors_v%d
		 WHERE embedding MATCH vec_f32(?)
		 ORDER BY distance LIMIT ?`, ordinal)
	rows, err := index.db.QueryContext(ctx, statement, string(encoded), limit+1)
	if err != nil {
		return nil, fmt.Errorf("query raw semantic probe: %w", err)
	}
	defer func() { _ = rows.Close() }()
	position := 0
	for rows.Next() {
		var distance float64
		if err := rows.Scan(&distance); err != nil {
			return nil, fmt.Errorf("scan raw semantic probe: %w", err)
		}
		if position == limit {
			return &vector.Hit[string]{ChunkIndex: -1, Score: float32(1 - distance)}, nil
		}
		position++
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("scan raw semantic probe: %w", err)
	}
	return nil, nil
}

func (index *Index) semanticCandidates(
	ctx context.Context, hits []semanticHit, filters SearchFilters,
) ([]rankedCandidate, error) {
	bestByDocument := make(map[string]semanticHit, len(hits))
	documentOrder := make([]string, 0, len(hits))
	for _, hit := range hits {
		if float64(hit.Score) < semanticCosineFloor {
			continue
		}
		if _, found := bestByDocument[hit.Doc]; found {
			continue
		}
		bestByDocument[hit.Doc] = hit
		documentOrder = append(documentOrder, hit.Doc)
	}
	if len(documentOrder) == 0 {
		return nil, nil
	}

	candidates := make([]rankedCandidate, 0, len(documentOrder))
	for _, docKey := range documentOrder {
		candidate, found, err := index.readCandidate(ctx, docKey)
		if err != nil {
			return nil, err
		}
		hit := bestByDocument[docKey]
		if !found || candidate.ContentHash != hit.ContentHash || !candidateMatchesFilters(candidate, filters) {
			continue
		}
		// Carry the query's revision through canonical hydration as well.
		candidate.ContentHash = hit.ContentHash
		candidate.Score = float64(hit.Score)
		candidate.ChunkIndex = hit.ChunkIndex
		candidate.MatchedIn = []string{MatchSemantic}
		candidates = append(candidates, candidate)
	}
	slices.SortFunc(candidates, func(left, right rankedCandidate) int {
		if left.Score != right.Score {
			if left.Score > right.Score {
				return -1
			}
			return 1
		}
		return compareCandidateTie(left, right)
	})
	return groupLegCandidates(candidates), nil
}

func (index *Index) readCandidate(ctx context.Context, docKey string) (rankedCandidate, bool, error) {
	row := index.db.QueryRowContext(ctx, `
		SELECT doc_key, review_id, COALESCE(review_uuid, ''),
		       job_id, COALESCE(job_uuid, ''), group_key,
		       repo_id, repo_name, COALESCE(branch, ''), git_ref,
		       COALESCE(commit_sha, ''), COALESCE(finished_at, ''),
		       COALESCE(verdict, ''), closed, COALESCE(panel_role, ''),
		       content, content_hash
		  FROM review_mirror WHERE doc_key = ?`, docKey)
	candidate, finishedAt, closed, err := scanCandidateBase(row, nil)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return rankedCandidate{}, false, nil
		}
		return rankedCandidate{}, false, fmt.Errorf("read semantic review candidate: %w", err)
	}
	candidate.FinishedAt = parseCandidateTime(finishedAt)
	candidate.Closed = closed != 0
	return candidate, true, nil
}

func candidateMatchesFilters(candidate rankedCandidate, filters SearchFilters) bool {
	if filters.RepoID != 0 && candidate.RepoID != filters.RepoID {
		return false
	}
	if filters.Branch != "" && candidate.Branch != filters.Branch {
		return false
	}
	if filters.Since != nil && candidate.FinishedAt.Before(*filters.Since) {
		return false
	}
	if filters.Verdict != "" && candidate.Verdict != filters.Verdict {
		return false
	}
	if filters.State == StateOpen && candidate.Closed {
		return false
	}
	if filters.State == StateClosed && !candidate.Closed {
		return false
	}
	return true
}
