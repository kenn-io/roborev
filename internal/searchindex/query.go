package searchindex

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"
	"unicode"

	"go.kenn.io/kit/search/lexical"
	"go.kenn.io/kit/search/sqlitefts"
	"go.kenn.io/kit/search/sqlquery"
	"go.kenn.io/kit/vector"
	"go.kenn.io/kit/vector/sqlitevec"
)

const (
	semanticDeepLimit   = 1000
	semanticCosineFloor = 0.30
	semanticProbeReason = "semantic candidate ceiling exhausted"
)

// SearchFilters restrict every retrieval leg to matching mirrored reviews.
type SearchFilters struct {
	RepoID  int64
	Branch  string
	Since   *time.Time
	Verdict string
	State   string
}

// legMember is one review a retrieval leg returned. Legs order members best
// first within each panel group, and groups best first.
type legMember struct {
	DocKey     string
	Score      float64
	ChunkIndex int
	Identifier bool
	Lexical    bool
	Semantic   bool
	Excerpt    string
	// FinishedKey and JobID break score ties: newer, then higher job, then key.
	// FinishedKey is Unix seconds.
	FinishedKey float64
	JobID       int64
}

// MatchedIn names the retrieval evidence for this member.
func (member legMember) MatchedIn() []string {
	var matches []string
	if member.Identifier {
		matches = append(matches, MatchIdentifier)
	}
	if member.Lexical {
		matches = append(matches, MatchLexical)
	}
	if member.Semantic {
		matches = append(matches, MatchSemantic)
	}
	return matches
}

// legGroup is one panel group and the members a leg found for it.
type legGroup struct {
	Key     string
	Members []legMember
}

func candidateTarget(limit int) int {
	return min(max(limit*3, 50), 200)
}

// literalFTSQuery quotes every word so FTS5 syntax in the query stays literal.
// A query with no letter or digit has no unicode61 token and returns "".
func literalFTSQuery(query string) string {
	if !slices.ContainsFunc(strings.Fields(query), containsSearchToken) {
		return ""
	}
	prepared, err := lexical.Literal().PrepareLiteral(query)
	if err != nil {
		return ""
	}
	return prepared.Match
}

func containsSearchToken(value string) bool {
	return strings.IndexFunc(value, func(r rune) bool {
		return unicode.IsLetter(r) || unicode.IsNumber(r)
	}) >= 0
}

// finishedTime is the numeric finish time of a stored review: Unix seconds
// with millisecond precision, from SQLite's own parser. A missing time is 0,
// so it sorts oldest.
func finishedTime(expression string) string {
	return "COALESCE(unixepoch(" + expression + ", 'subsec'), 0)"
}

// filterPredicate restricts a leg's source rows, aliased d, to filters.
func filterPredicate(filters SearchFilters) sqlquery.Predicate {
	var clauses []string
	var args []any
	if filters.RepoID != 0 {
		clauses = append(clauses, "d.repo_id = ?")
		args = append(args, filters.RepoID)
	}
	if filters.Branch != "" {
		clauses = append(clauses, "d.branch = ?")
		args = append(args, filters.Branch)
	}
	if filters.Since != nil {
		clauses = append(clauses, "unixepoch(d.finished_at, 'subsec') >= unixepoch(?, 'subsec')")
		args = append(args, filters.Since.UTC().Format(time.RFC3339Nano))
	}
	if filters.Verdict != "" {
		clauses = append(clauses, "d.verdict = ?")
		args = append(args, filters.Verdict)
	}
	switch filters.State {
	case StateOpen:
		clauses = append(clauses, "d.closed = 0")
	case StateClosed:
		clauses = append(clauses, "d.closed = 1")
	}
	if len(clauses) == 0 {
		return sqlquery.Predicate{SQL: "1"}
	}
	return sqlquery.Predicate{SQL: strings.Join(clauses, " AND "), Args: args}
}

var reviewFTS = func() sqlitefts.Helper {
	helper, err := sqlitefts.New(
		sqlitefts.WithIndexTable("review_fts"), sqlitefts.WithIndexKey("doc_key"),
		sqlitefts.WithSourceTable("review_mirror"), sqlitefts.WithSourceKey("doc_key"),
	)
	if err != nil {
		panic(err)
	}
	return helper
}()

// lexicalLeg returns every member of the best groupLimit panel groups that
// match query. Exact identifiers and full or short commit SHAs rank before
// FTS matches, which rank by BM25. Filters run before the group limit.
func (index *Index) lexicalLeg(query string, filters SearchFilters, groupLimit int) (sqlquery.Query, error) {
	predicate := filterPredicate(filters)
	var args []any

	// Every FTS match: the outer query limits panel groups, not rows.
	fts := `SELECT NULL AS doc_key, NULL AS score WHERE 0`
	match := literalFTSQuery(query)
	if match != "" {
		built, err := reviewFTS.Build(sqlitefts.Request{
			Match: match, SourcePredicate: predicate, CandidateLimit: math.MaxInt,
		})
		if err != nil {
			return sqlquery.Query{}, fmt.Errorf("build review text query: %w", err)
		}
		fts = built.SQL
		args = append(args, built.Args...)
	}

	identifier := `instr(char(10) || lower(d.identifiers) || char(10), char(10) || lower(?) || char(10)) > 0`
	args = append(args, query)
	if isSHAPrefix(query) {
		identifier += ` OR lower(COALESCE(d.commit_sha, '')) LIKE lower(?) || '%'`
		args = append(args, query)
	}
	args = append(args, predicate.Args...)

	order := `identifier DESC, score DESC, finished_key DESC, job_id DESC, doc_key ASC`
	excerpts := `SELECT NULL AS doc_key, NULL AS excerpt WHERE 0`
	if match != "" {
		excerpts = `SELECT review_fts.doc_key,
		       snippet(review_fts, 1, char(57344), char(57345), ' … ', 32) AS excerpt
		  FROM review_fts
		 WHERE review_fts MATCH ?
		   AND review_fts.doc_key IN (SELECT doc_key FROM members WHERE lexical AND NOT identifier)`
	}
	statement := `
		WITH fts AS MATERIALIZED (` + fts + `),
		ident AS MATERIALIZED (
			SELECT d.doc_key FROM review_mirror AS d
			 WHERE (` + identifier + `) AND (` + predicate.SQL + `)
		),
		keys AS (SELECT doc_key FROM fts UNION SELECT doc_key FROM ident),
		matches AS MATERIALIZED (
			SELECT d.doc_key, d.group_key, d.job_id,
			       ` + finishedTime("d.finished_at") + ` AS finished_key,
			       ident.doc_key IS NOT NULL AS identifier,
			       fts.doc_key IS NOT NULL AS lexical,
			       CASE WHEN ident.doc_key IS NOT NULL THEN 1.0 ELSE fts.score END AS score,
			       CASE WHEN ident.doc_key IS NOT NULL THEN d.content END AS identifier_excerpt
			  FROM keys
			  JOIN review_mirror AS d ON d.doc_key = keys.doc_key
			  LEFT JOIN fts ON fts.doc_key = keys.doc_key
			  LEFT JOIN ident ON ident.doc_key = keys.doc_key
		),
		ranked AS MATERIALIZED (
			SELECT matches.*, row_number() OVER (PARTITION BY group_key ORDER BY ` + order + `) AS member_rank
			  FROM matches
		),
		top_groups AS MATERIALIZED (
			SELECT group_key, row_number() OVER (ORDER BY ` + order + `) AS group_rank
			  FROM ranked WHERE member_rank = 1
			 ORDER BY group_rank LIMIT ?
		),
		members AS MATERIALIZED (
			SELECT ranked.*, top_groups.group_rank
			  FROM ranked JOIN top_groups ON top_groups.group_key = ranked.group_key
		),
		excerpts AS MATERIALIZED (` + excerpts + `)
		SELECT members.group_key, members.doc_key, members.score, members.identifier, members.lexical,
		       COALESCE(members.identifier_excerpt, excerpts.excerpt, ''), members.finished_key, members.job_id
		  FROM members LEFT JOIN excerpts ON excerpts.doc_key = members.doc_key
		 ORDER BY members.group_rank, members.member_rank`
	args = append(args, groupLimit)
	if match != "" {
		args = append(args, match)
	}
	return sqlquery.Query{SQL: statement, Args: args}, nil
}

func scanLexicalRow(rows *sql.Rows) (string, legMember, error) {
	var group string
	var member legMember
	err := rows.Scan(&group, &member.DocKey, &member.Score, &member.Identifier, &member.Lexical,
		&member.Excerpt, &member.FinishedKey, &member.JobID)
	return group, member, err
}

// SearchLexical runs the lexical leg alone and returns its panel groups.
func (index *Index) SearchLexical(
	ctx context.Context, query string, filters SearchFilters, groupLimit int,
) ([]legGroup, error) {
	query = strings.TrimSpace(query)
	if query == "" || groupLimit <= 0 {
		return nil, nil
	}
	statement, err := index.lexicalLeg(query, filters, groupLimit)
	if err != nil {
		return nil, err
	}
	groups, err := runLeg(ctx, index.db, statement, scanLexicalRow)
	if err != nil {
		return nil, fmt.Errorf("search review text: %w", err)
	}
	return groups, nil
}

// semanticLeg ranks reviews by their closest current chunk in generation key.
// The candidate limit bounds raw neighbors; filters and the cosine floor run
// after that window, as sqlite-vec requires.
func (index *Index) semanticLeg(
	ctx context.Context, key string, query vector.Vector, candidateLimit int, filters SearchFilters,
) (sqlquery.Query, error) {
	candidates, err := index.vectors.BuildCandidateQuery(ctx, index.db, key, query, sqlitevec.CandidateQuery{
		CandidateLimit:  candidateLimit,
		SourcePredicate: filterPredicate(filters),
		ExtraSourceCols: []sqlquery.Column{
			{Name: "group_key", As: "group_key"},
			{Name: "finished_at", As: "finished_at"},
			{Name: "job_id", As: "job_id"},
		},
	})
	if err != nil {
		return sqlquery.Query{}, fmt.Errorf("build semantic candidate query: %w", err)
	}
	order := `distance ASC, finished_key DESC, job_id DESC, doc_key ASC`
	statement := `
		WITH chunks AS MATERIALIZED (` + candidates.SQL + `),
		best AS MATERIALIZED (
			SELECT doc_key, group_key, job_id, chunk_index, distance,
			       ` + finishedTime("finished_at") + ` AS finished_key,
			       row_number() OVER (PARTITION BY doc_key ORDER BY distance, chunk_index) AS chunk_rank
			  FROM chunks
		),
		docs AS MATERIALIZED (
			SELECT * FROM best WHERE chunk_rank = 1 AND 1 - distance >= ?
		),
		ranked AS MATERIALIZED (
			SELECT docs.*, row_number() OVER (PARTITION BY group_key ORDER BY ` + order + `) AS member_rank
			  FROM docs
		),
		groups AS MATERIALIZED (
			SELECT group_key, row_number() OVER (ORDER BY ` + order + `) AS group_rank
			  FROM ranked WHERE member_rank = 1
		)
		SELECT ranked.group_key, ranked.doc_key, ranked.distance, ranked.chunk_index,
		       ranked.finished_key, ranked.job_id
		  FROM ranked JOIN groups ON groups.group_key = ranked.group_key
		 ORDER BY groups.group_rank, ranked.member_rank`
	return sqlquery.Query{
		SQL:       statement,
		Args:      append(candidates.Args, semanticCosineFloor),
		RawWindow: candidates.RawWindow,
	}, nil
}

func scanSemanticRow(rows *sql.Rows) (string, legMember, error) {
	var group string
	var distance float64
	member := legMember{Semantic: true}
	if err := rows.Scan(&group, &member.DocKey, &distance, &member.ChunkIndex,
		&member.FinishedKey, &member.JobID); err != nil {
		return "", legMember{}, err
	}
	score, err := sqlitevec.ScoreFromDistance(distance)
	if err != nil {
		return "", legMember{}, err
	}
	member.Score = float64(score)
	return group, member, nil
}

// SearchSemantic runs the semantic leg alone and returns its panel groups.
func (index *Index) SearchSemantic(
	ctx context.Context, key string, query vector.Vector, candidateLimit int, filters SearchFilters,
) ([]legGroup, error) {
	if candidateLimit <= 0 {
		return nil, nil
	}
	statement, err := index.semanticLeg(ctx, key, query, candidateLimit, filters)
	if err != nil {
		return nil, err
	}
	groups, err := runLeg(ctx, index.db, statement, scanSemanticRow)
	if err != nil {
		return nil, fmt.Errorf("query semantic candidates: %w", err)
	}
	return groups, nil
}

// runLeg executes one leg and groups consecutive rows by panel group.
func runLeg(
	ctx context.Context, db sqlquery.Queryer, statement sqlquery.Query,
	scan func(*sql.Rows) (string, legMember, error),
) ([]legGroup, error) {
	type row struct {
		group  string
		member legMember
	}
	rows, err := statement.All(ctx, db, func(rows *sql.Rows) (row, error) {
		group, member, err := scan(rows)
		return row{group: group, member: member}, err
	})
	if err != nil {
		return nil, err
	}
	var groups []legGroup
	for _, item := range rows {
		if len(groups) == 0 || groups[len(groups)-1].Key != item.group {
			groups = append(groups, legGroup{Key: item.group})
		}
		last := len(groups) - 1
		groups[last].Members = append(groups[last].Members, item.member)
	}
	return groups, nil
}

// SemanticProbe reports the similarity of the first raw neighbor beyond limit.
// The raw window is counted before freshness joins, so stale vectors still
// occupy it.
func (index *Index) SemanticProbe(
	ctx context.Context, key string, query vector.Vector, limit int,
) (float32, bool, error) {
	window, err := index.vectors.QueryGenerationWindow(ctx, key, query, limit)
	if err != nil {
		return 0, false, fmt.Errorf("probe semantic candidate window: %w", err)
	}
	return window.ProbeScore, window.HasProbe, nil
}

// mirrorContents returns the mirrored text of docKeys, for semantic excerpts.
func (index *Index) mirrorContents(ctx context.Context, docKeys []string) (map[string]string, error) {
	contents := make(map[string]string, len(docKeys))
	if len(docKeys) == 0 {
		return contents, nil
	}
	args := make([]any, len(docKeys))
	for i, key := range docKeys {
		args[i] = key
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(docKeys)), ",")
	rows, err := index.db.QueryContext(ctx,
		`SELECT doc_key, content FROM review_mirror WHERE doc_key IN (`+placeholders+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("read search mirror content: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var key, content string
		if err := rows.Scan(&key, &content); err != nil {
			return nil, fmt.Errorf("scan search mirror content: %w", err)
		}
		contents[key] = content
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read search mirror content: %w", err)
	}
	return contents, nil
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
