package storage

import (
	"database/sql"
	"sort"
	"strings"
	"time"
)

// querier abstracts *sql.DB and *sql.Tx for summary queries.
type querier interface {
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

// Summary holds aggregate review statistics for a time window.
type Summary struct {
	Since    time.Time      `json:"since"`
	RepoPath string         `json:"repo_path,omitempty"`
	Branch   string         `json:"branch,omitempty"`
	Overview OverviewStats  `json:"overview"`
	Verdicts VerdictStats   `json:"verdicts"`
	Agents   []AgentStats   `json:"agents"`
	Duration DurationStats  `json:"duration"`
	JobTypes []JobTypeStats `json:"job_types"`
	Failures FailureStats   `json:"failures"`
	Repos    []RepoSummary  `json:"repos,omitempty"`
	Cost     CostAggregate  `json:"cost"`
}

// OverviewStats contains job counts by status.
type OverviewStats struct {
	Total    int `json:"total"`
	Queued   int `json:"queued"`
	Running  int `json:"running"`
	Done     int `json:"done"`
	Failed   int `json:"failed"`
	Canceled int `json:"canceled"`
	Applied  int `json:"applied"`
	Rebased  int `json:"rebased"`
}

// VerdictStats contains pass/fail/addressed counts for completed reviews.
type VerdictStats struct {
	Total          int     `json:"total"`
	Passed         int     `json:"passed"`
	Failed         int     `json:"failed"`
	Addressed      int     `json:"addressed"`
	PassRate       float64 `json:"pass_rate"`
	ResolutionRate float64 `json:"resolution_rate"`
}

// AgentStats contains per-agent performance metrics.
// Total counts all jobs by this agent (including task and fix jobs).
// Passed and Failed count only verdict-bearing review jobs, so
// Passed + Failed may be less than Total. PassRate is Passed/(Passed+Failed).
type AgentStats struct {
	Agent      string  `json:"agent"`
	Total      int     `json:"total"`
	Passed     int     `json:"passed"`
	Failed     int     `json:"failed"`
	Errors     int     `json:"errors"`
	PassRate   float64 `json:"pass_rate"`
	MedianSecs float64 `json:"median_duration_secs"`
}

// DurationStats contains duration percentiles in seconds.
type DurationStats struct {
	ReviewP50 float64 `json:"review_p50_secs"`
	ReviewP90 float64 `json:"review_p90_secs"`
	ReviewP99 float64 `json:"review_p99_secs"`
	QueueP50  float64 `json:"queue_p50_secs"`
	QueueP90  float64 `json:"queue_p90_secs"`
	QueueP99  float64 `json:"queue_p99_secs"`
}

// JobTypeStats contains job counts by type with fix terminal status breakdown.
type JobTypeStats struct {
	Type    string `json:"type"`
	Count   int    `json:"count"`
	Applied int    `json:"applied,omitempty"`
	Rebased int    `json:"rebased,omitempty"`
}

// RepoSummary contains per-repo summary when querying across all repos.
type RepoSummary struct {
	Name      string `json:"name"`
	Path      string `json:"path"`
	Total     int    `json:"total"`
	Passed    int    `json:"passed"`
	Failed    int    `json:"failed"`
	Addressed int    `json:"addressed"`
}

// FailureStats contains failure categorization.
type FailureStats struct {
	Total   int            `json:"total"`
	Retries int            `json:"retries"`
	Errors  map[string]int `json:"errors"`
}

// verdictJobFilter excludes job types whose verdict_bool values are meaningless.
// Task jobs produce freeform analysis and fix jobs produce code edits — neither
// returns PASS/FAIL output, so ParseVerdict results are not meaningful.
// NOTE: assumes review_jobs is aliased as "j" in the enclosing query.
const verdictJobFilter = "COALESCE(j.job_type, 'review') NOT IN ('task', 'fix')"

// SummaryOptions configures the summary query.
type SummaryOptions struct {
	RepoPath string
	Branch   string
	Since    time.Time
	AllRepos bool
}

// GetSummary computes aggregate review statistics.
// All sub-queries run inside a single read transaction for snapshot consistency.
func (db *DB) GetSummary(opts SummaryOptions) (*Summary, error) {
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	s := &Summary{
		Since:    opts.Since,
		RepoPath: opts.RepoPath,
		Branch:   opts.Branch,
	}

	sinceStr := opts.Since.UTC().Format("2006-01-02 15:04:05")

	// Build shared WHERE clause.
	// Use datetime() to normalize timestamps — synced rows may use RFC3339
	// format (with 'T' separator) while local rows use space-separated format.
	var conditions []string
	var args []any
	conditions = append(conditions, "datetime(j.enqueued_at) >= datetime(?)")
	args = append(args, sinceStr)
	if opts.RepoPath != "" {
		conditions = append(conditions, "r.root_path = ?")
		args = append(args, opts.RepoPath)
	}
	if opts.Branch != "" {
		conditions = append(conditions, "j.branch = ?")
		args = append(args, opts.Branch)
	}
	where := "WHERE " + strings.Join(conditions, " AND ")

	s.Overview, err = summaryOverview(tx, where, args)
	if err != nil {
		return nil, err
	}

	s.Verdicts, err = summaryVerdicts(tx, where, args)
	if err != nil {
		return nil, err
	}

	s.Agents, err = summaryAgents(tx, where, args)
	if err != nil {
		return nil, err
	}

	s.Duration, err = summaryDurations(tx, where, args)
	if err != nil {
		return nil, err
	}

	s.JobTypes, err = summaryJobTypes(tx, where, args)
	if err != nil {
		return nil, err
	}

	s.Failures, err = summaryFailures(tx, where, args)
	if err != nil {
		return nil, err
	}

	var costRepos []string
	if opts.RepoPath != "" {
		costRepos = []string{opts.RepoPath}
	}
	s.Cost, err = costAggregate(tx, CostOptions{
		RepoPaths: costRepos,
		Branch:    opts.Branch,
		Since:     opts.Since,
	})
	if err != nil {
		return nil, err
	}

	if opts.AllRepos || opts.RepoPath == "" {
		s.Repos, err = summaryRepos(tx, where, args)
		if err != nil {
			return nil, err
		}
	}

	return s, nil
}

func summaryOverview(q querier, where string, args []any) (OverviewStats, error) {
	query := `
		SELECT
			COALESCE(SUM(CASE WHEN j.status = 'queued' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN j.status = 'running' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN j.status = 'done' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN j.status = 'failed' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN j.status = 'canceled' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN j.status = 'applied' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN j.status = 'rebased' THEN 1 ELSE 0 END), 0),
			COUNT(*)
		FROM review_jobs j
		JOIN repos r ON r.id = j.repo_id
		` + where

	var o OverviewStats
	err := q.QueryRow(query, args...).Scan(
		&o.Queued, &o.Running, &o.Done, &o.Failed,
		&o.Canceled, &o.Applied, &o.Rebased, &o.Total,
	)
	return o, err
}

func summaryVerdicts(q querier, where string, args []any) (VerdictStats, error) {
	query := `
		SELECT
			COUNT(*),
			COALESCE(SUM(CASE WHEN rv.verdict_bool = 1 THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN rv.verdict_bool = 0 THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN rv.closed = 1 AND rv.verdict_bool = 0 THEN 1 ELSE 0 END), 0)
		FROM review_jobs j
		JOIN repos r ON r.id = j.repo_id
		JOIN reviews rv ON rv.job_id = j.id
		` + where + ` AND j.status IN ('done', 'applied', 'rebased')
			AND rv.verdict_bool IS NOT NULL
			AND ` + verdictJobFilter

	var v VerdictStats
	err := q.QueryRow(query, args...).Scan(&v.Total, &v.Passed, &v.Failed, &v.Addressed)
	if err != nil {
		return v, err
	}
	if v.Total > 0 {
		v.PassRate = float64(v.Passed) / float64(v.Total)
	}
	if v.Failed > 0 {
		v.ResolutionRate = float64(v.Addressed) / float64(v.Failed)
	}
	return v, nil
}

func summaryAgents(q querier, where string, args []any) ([]AgentStats, error) {
	query := `
		SELECT
			j.agent,
			COUNT(*) as total,
			COALESCE(SUM(CASE WHEN rv.verdict_bool = 1
				AND ` + verdictJobFilter + ` THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN rv.verdict_bool = 0
				AND ` + verdictJobFilter + ` THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN j.status = 'failed' THEN 1 ELSE 0 END), 0)
		FROM review_jobs j
		JOIN repos r ON r.id = j.repo_id
		LEFT JOIN reviews rv ON rv.job_id = j.id
		` + where + `
		GROUP BY j.agent
		ORDER BY total DESC`

	rows, err := q.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var agents []AgentStats
	for rows.Next() {
		var a AgentStats
		if err := rows.Scan(&a.Agent, &a.Total, &a.Passed, &a.Failed, &a.Errors); err != nil {
			return nil, err
		}
		reviewed := a.Passed + a.Failed
		if reviewed > 0 {
			a.PassRate = float64(a.Passed) / float64(reviewed)
		}
		agents = append(agents, a)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for i := range agents {
		median, err := agentMedianDuration(q, where, args, agents[i].Agent)
		if err != nil {
			return nil, err
		}
		agents[i].MedianSecs = median
	}

	return agents, nil
}

func agentMedianDuration(q querier, where string, args []any, agent string) (float64, error) {
	query := `
		SELECT CAST((julianday(j.finished_at) - julianday(j.started_at)) * 86400 AS REAL)
		FROM review_jobs j
		JOIN repos r ON r.id = j.repo_id
		` + where + ` AND j.agent = ? AND j.started_at IS NOT NULL AND j.finished_at IS NOT NULL
		ORDER BY 1`

	allArgs := append(append([]any{}, args...), agent)
	rows, err := q.Query(query, allArgs...)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	var durations []float64
	for rows.Next() {
		var d float64
		if err := rows.Scan(&d); err != nil {
			return 0, err
		}
		durations = append(durations, d)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	return percentile(durations, 0.5), nil
}

func summaryDurations(q querier, where string, args []any) (DurationStats, error) {
	reviewQuery := `
		SELECT CAST((julianday(j.finished_at) - julianday(j.started_at)) * 86400 AS REAL)
		FROM review_jobs j
		JOIN repos r ON r.id = j.repo_id
		` + where + ` AND j.started_at IS NOT NULL AND j.finished_at IS NOT NULL
		ORDER BY 1`

	reviewDurations, err := collectDurations(q, reviewQuery, args)
	if err != nil {
		return DurationStats{}, err
	}

	queueQuery := `
		SELECT CAST((julianday(j.started_at) - julianday(j.enqueued_at)) * 86400 AS REAL)
		FROM review_jobs j
		JOIN repos r ON r.id = j.repo_id
		` + where + ` AND j.started_at IS NOT NULL
		ORDER BY 1`

	queueDurations, err := collectDurations(q, queueQuery, args)
	if err != nil {
		return DurationStats{}, err
	}

	return DurationStats{
		ReviewP50: percentile(reviewDurations, 0.50),
		ReviewP90: percentile(reviewDurations, 0.90),
		ReviewP99: percentile(reviewDurations, 0.99),
		QueueP50:  percentile(queueDurations, 0.50),
		QueueP90:  percentile(queueDurations, 0.90),
		QueueP99:  percentile(queueDurations, 0.99),
	}, nil
}

func collectDurations(q querier, query string, args []any) ([]float64, error) {
	rows, err := q.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var durations []float64
	for rows.Next() {
		var d float64
		if err := rows.Scan(&d); err != nil {
			return nil, err
		}
		if d >= 0 {
			durations = append(durations, d)
		}
	}
	return durations, rows.Err()
}

func summaryJobTypes(q querier, where string, args []any) ([]JobTypeStats, error) {
	query := `
		SELECT
			COALESCE(NULLIF(j.job_type, ''), 'review') as jt,
			COUNT(*),
			COALESCE(SUM(CASE WHEN j.status = 'applied' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN j.status = 'rebased' THEN 1 ELSE 0 END), 0)
		FROM review_jobs j
		JOIN repos r ON r.id = j.repo_id
		` + where + `
		GROUP BY jt
		ORDER BY COUNT(*) DESC`

	rows, err := q.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var types []JobTypeStats
	for rows.Next() {
		var t JobTypeStats
		if err := rows.Scan(&t.Type, &t.Count, &t.Applied, &t.Rebased); err != nil {
			return nil, err
		}
		types = append(types, t)
	}
	return types, rows.Err()
}

func summaryRepos(q querier, where string, args []any) ([]RepoSummary, error) {
	query := `
		SELECT
			r.name,
			r.root_path,
			COUNT(*) as total,
			COALESCE(SUM(CASE WHEN rv.verdict_bool = 1
				AND ` + verdictJobFilter + ` THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN rv.verdict_bool = 0
				AND ` + verdictJobFilter + ` THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN rv.closed = 1 AND rv.verdict_bool = 0
				AND ` + verdictJobFilter + ` THEN 1 ELSE 0 END), 0)
		FROM review_jobs j
		JOIN repos r ON r.id = j.repo_id
		LEFT JOIN reviews rv ON rv.job_id = j.id
		` + where + `
		GROUP BY r.id
		ORDER BY total DESC`

	rows, err := q.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var repos []RepoSummary
	for rows.Next() {
		var rs RepoSummary
		if err := rows.Scan(&rs.Name, &rs.Path, &rs.Total, &rs.Passed, &rs.Failed, &rs.Addressed); err != nil {
			return nil, err
		}
		repos = append(repos, rs)
	}
	return repos, rows.Err()
}

func summaryFailures(q querier, where string, args []any) (FailureStats, error) {
	countQuery := `
		SELECT
			COALESCE(SUM(CASE WHEN j.status = 'failed' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN j.status = 'failed' THEN j.retry_count ELSE 0 END), 0)
		FROM review_jobs j
		JOIN repos r ON r.id = j.repo_id
		` + where

	var f FailureStats
	if err := q.QueryRow(countQuery, args...).Scan(&f.Total, &f.Retries); err != nil {
		return f, err
	}

	errQuery := `
		SELECT j.error
		FROM review_jobs j
		JOIN repos r ON r.id = j.repo_id
		` + where + ` AND j.status = 'failed' AND j.error != ''`

	rows, err := q.Query(errQuery, args...)
	if err != nil {
		return f, err
	}
	defer rows.Close()

	f.Errors = make(map[string]int)
	for rows.Next() {
		var errMsg string
		if err := rows.Scan(&errMsg); err != nil {
			return f, err
		}
		category := categorizeError(errMsg)
		f.Errors[category]++
	}
	return f, rows.Err()
}

// BackfillVerdictBool populates verdict_bool for reviews that have output
// but a NULL verdict_bool. Returns the number of rows updated.
func (db *DB) BackfillVerdictBool() (int, error) {
	rows, err := db.Query(`
		SELECT rv.id, rv.output
		FROM reviews rv
		WHERE rv.verdict_bool IS NULL AND rv.output != ''
	`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	type pending struct {
		id      int64
		verdict int
	}
	var updates []pending
	for rows.Next() {
		var id int64
		var output string
		if err := rows.Scan(&id, &output); err != nil {
			return 0, err
		}
		updates = append(updates, pending{
			id:      id,
			verdict: verdictToBool(ParseVerdict(output)),
		})
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	if len(updates) == 0 {
		return 0, nil
	}

	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.Prepare(`UPDATE reviews SET verdict_bool = ? WHERE id = ?`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()

	for _, u := range updates {
		if _, err := stmt.Exec(u.verdict, u.id); err != nil {
			return 0, err
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(updates), nil
}

// categorizeError maps error messages to categories.
func categorizeError(errMsg string) string {
	lower := strings.ToLower(errMsg)
	switch {
	case strings.Contains(lower, "quota") || strings.Contains(lower, "rate limit") || strings.Contains(lower, "429"):
		return "quota"
	case strings.Contains(lower, "timeout") || strings.Contains(lower, "deadline exceeded"):
		return "timeout"
	case strings.Contains(lower, "not found") || strings.Contains(lower, "no such file"):
		return "not_found"
	case strings.Contains(lower, "signal") || strings.Contains(lower, "killed") || strings.Contains(lower, "exit status"):
		return "crash"
	default:
		return "other"
	}
}

// percentile computes the p-th percentile using linear interpolation.
// It sorts the input slice in place. Returns 0 if the slice is empty.
func percentile(values []float64, p float64) float64 {
	n := len(values)
	if n == 0 {
		return 0
	}
	sort.Float64s(values)
	if n == 1 {
		return values[0]
	}
	rank := p * float64(n-1)
	lower := int(rank)
	upper := lower + 1
	if upper >= n {
		return values[n-1]
	}
	frac := rank - float64(lower)
	return values[lower] + frac*(values[upper]-values[lower])
}
