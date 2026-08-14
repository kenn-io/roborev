package storage

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
)

const (
	AnalyticsSchemaVersion  = 1
	MaxAnalyticsTimeBuckets = 1000
)

var ErrAnalyticsRangeTooLarge = errors.New("analytics range has too many time buckets")

type AnalyticsBucket string

const (
	AnalyticsBucketHour  AnalyticsBucket = "hour"
	AnalyticsBucketDay   AnalyticsBucket = "day"
	AnalyticsBucketWeek  AnalyticsBucket = "week"
	AnalyticsBucketMonth AnalyticsBucket = "month"
)

type AnalyticsOptions struct {
	Since    time.Time
	Until    time.Time
	Projects []string
	Sources  []string
	Agents   []string
	Models   []string
	Bucket   AnalyticsBucket
}

type AnalyticsFilters struct {
	Since    *time.Time      `json:"since,omitempty"`
	Until    time.Time       `json:"until"`
	Projects []string        `json:"projects"`
	Sources  []string        `json:"sources"`
	Agents   []string        `json:"agents"`
	Models   []string        `json:"models"`
	Bucket   AnalyticsBucket `json:"bucket"`
}

type AnalyticsReviewStats struct {
	Total        int     `json:"total"`
	Done         int     `json:"done"`
	Failed       int     `json:"failed"`
	Canceled     int     `json:"canceled"`
	Skipped      int     `json:"skipped"`
	FailureRate  float64 `json:"failure_rate"`
	RunErrors    int     `json:"run_errors"`
	RunErrorRate float64 `json:"run_error_rate"`
}

type AnalyticsVerdictStats struct {
	Passed      int     `json:"passed"`
	FailOpen    int     `json:"fail_open"`
	FailClosed  int     `json:"fail_closed"`
	Rated       int     `json:"rated"`
	FailureRate float64 `json:"failure_rate"`
}

type AnalyticsPercentiles struct {
	P50Secs float64 `json:"p50_secs"`
	P90Secs float64 `json:"p90_secs"`
	P99Secs float64 `json:"p99_secs"`
}

type AnalyticsAttemptStats struct {
	Eligible int                  `json:"eligible"`
	Duration AnalyticsPercentiles `json:"duration"`
}

type AnalyticsCostStats struct {
	TotalUSD         float64 `json:"total_usd"`
	EligibleAttempts int     `json:"eligible_attempts"`
	PricedAttempts   int     `json:"priced_attempts"`
	Coverage         float64 `json:"coverage"`
	Complete         bool    `json:"complete"`
}

type AnalyticsSummary struct {
	Reviews       AnalyticsReviewStats  `json:"reviews"`
	Verdicts      AnalyticsVerdictStats `json:"verdicts"`
	ReviewLatency AnalyticsPercentiles  `json:"review_latency"`
	Attempts      AnalyticsAttemptStats `json:"attempts"`
	Cost          AnalyticsCostStats    `json:"cost"`
}

type AnalyticsTimeBucket struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
	AnalyticsSummary
}

type AnalyticsProjectRow struct {
	Project string `json:"project"`
	AnalyticsSummary
}

type AnalyticsDimensionRow struct {
	Value string `json:"value"`
	AnalyticsSummary
}

type AnalyticsFilterOptions struct {
	Projects []string `json:"projects"`
	Sources  []string `json:"sources"`
	Agents   []string `json:"agents"`
	Models   []string `json:"models"`
}

type AnalyticsSnapshot struct {
	SchemaVersion int                     `json:"schema_version"`
	Filters       AnalyticsFilters        `json:"filters"`
	Summary       AnalyticsSummary        `json:"summary"`
	TimeSeries    []AnalyticsTimeBucket   `json:"time_series"`
	Projects      []AnalyticsProjectRow   `json:"projects"`
	Sources       []AnalyticsDimensionRow `json:"sources"`
	Agents        []AnalyticsDimensionRow `json:"agents"`
	Models        []AnalyticsDimensionRow `json:"models"`
	Options       AnalyticsFilterOptions  `json:"options"`
}

type analyticsRow struct {
	project         string
	source          string
	agent           string
	model           string
	jobType         string
	panelRole       string
	status          JobStatus
	finishedAt      time.Time
	reviewDuration  float64
	attemptDuration float64
	verdict         sql.NullInt64
	closed          bool
	eligible        bool
	priced          bool
	costUSD         float64
}

type analyticsAccumulator struct {
	summary          AnalyticsSummary
	reviewDurations  []float64
	attemptDurations []float64
}

// GetAnalytics returns one coherent analytics snapshot from a SQLite read
// transaction. Unlike GetCostAggregate, the time cut is finished_at because
// this view accounts for work completed inside the selected window.
func (db *DB) GetAnalytics(opts AnalyticsOptions) (*AnalyticsSnapshot, error) {
	if !validAnalyticsBucket(opts.Bucket) {
		return nil, fmt.Errorf("invalid analytics bucket %q", opts.Bucket)
	}
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := queryAnalyticsRows(tx, opts)
	if err != nil {
		return nil, err
	}
	snapshot, err := aggregateAnalytics(rows, opts)
	if err != nil {
		return nil, err
	}
	options, err := queryAnalyticsFilterOptions(tx, opts.Since, opts.Until)
	if err != nil {
		return nil, err
	}
	snapshot.Options = options
	return snapshot, nil
}

func queryAnalyticsRows(q querier, opts AnalyticsOptions) ([]analyticsRow, error) {
	conditions := []string{"j.finished_at IS NOT NULL"}
	args := []any{}
	if !opts.Since.IsZero() {
		conditions = append(conditions, "julianday(j.finished_at) >= julianday(?)")
		args = append(args, analyticsTime(opts.Since))
	}
	if !opts.Until.IsZero() {
		conditions = append(conditions, "julianday(j.finished_at) < julianday(?)")
		args = append(args, analyticsTime(opts.Until))
	}
	appendAnalyticsInFilter(&conditions, &args, "r.name", opts.Projects)
	appendAnalyticsInFilter(&conditions, &args, "COALESCE(j.source, '')", opts.Sources)

	query := `
		SELECT r.name, COALESCE(j.source, ''), COALESCE(j.agent, ''), COALESCE(j.model, ''),
		       COALESCE(NULLIF(j.job_type, ''), 'review'), COALESCE(j.panel_role, ''),
		       j.status, j.finished_at,
		       COALESCE(CAST((julianday(j.finished_at) - julianday(j.enqueued_at)) * 86400 AS REAL), 0),
		       COALESCE(CAST((julianday(j.finished_at) - julianday(j.started_at)) * 86400 AS REAL), 0),
		       rv.verdict_bool, COALESCE(rv.closed, 0),
		       CASE WHEN ` + costEligible + ` THEN 1 ELSE 0 END,
		       CASE WHEN ` + costEligible + ` AND ` + hasCost + ` THEN 1 ELSE 0 END,
		       CASE WHEN ` + costEligible + ` AND ` + hasCost + `
		            THEN json_extract(j.token_usage, '$.cost_usd') ELSE 0 END
		FROM review_jobs j
		JOIN repos r ON r.id = j.repo_id
		LEFT JOIN reviews rv ON rv.job_id = j.id
		WHERE ` + strings.Join(conditions, " AND ") + `
		ORDER BY datetime(j.finished_at), j.id`

	sqlRows, err := q.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer sqlRows.Close()
	result := []analyticsRow{}
	for sqlRows.Next() {
		var row analyticsRow
		var finished string
		if err := sqlRows.Scan(
			&row.project, &row.source, &row.agent, &row.model, &row.jobType,
			&row.panelRole, &row.status, &finished, &row.reviewDuration,
			&row.attemptDuration, &row.verdict, &row.closed, &row.eligible,
			&row.priced, &row.costUSD,
		); err != nil {
			return nil, err
		}
		row.finishedAt = parseSQLiteTime(finished).UTC()
		result = append(result, row)
	}
	return result, sqlRows.Err()
}

func aggregateAnalytics(rows []analyticsRow, opts AnalyticsOptions) (*AnalyticsSnapshot, error) {
	filters := AnalyticsFilters{
		Until: opts.Until.UTC(), Projects: sortedUnique(opts.Projects),
		Sources: sortedUnique(opts.Sources), Agents: sortedUnique(opts.Agents),
		Models: sortedUnique(opts.Models), Bucket: opts.Bucket,
	}
	if !opts.Since.IsZero() {
		since := opts.Since.UTC()
		filters.Since = &since
	}
	snapshot := &AnalyticsSnapshot{
		SchemaVersion: AnalyticsSchemaVersion, Filters: filters,
		TimeSeries: []AnalyticsTimeBucket{}, Projects: []AnalyticsProjectRow{},
		Sources: []AnalyticsDimensionRow{}, Agents: []AnalyticsDimensionRow{},
		Models: []AnalyticsDimensionRow{}, Options: AnalyticsFilterOptions{
			Projects: []string{}, Sources: []string{}, Agents: []string{}, Models: []string{},
		},
	}
	total := &analyticsAccumulator{}
	projects := map[string]*analyticsAccumulator{}
	sources := map[string]*analyticsAccumulator{}
	agents := map[string]*analyticsAccumulator{}
	models := map[string]*analyticsAccumulator{}
	buckets := map[time.Time]*analyticsAccumulator{}

	for _, row := range rows {
		logicalReview := isLogicalReview(row)
		eligibleAttempt := row.eligible && matchesAnalyticsAttemptFilters(row, opts)
		if !logicalReview && !eligibleAttempt {
			continue
		}
		project := analyticsAccumulatorFor(projects, row.project)
		source := analyticsAccumulatorFor(sources, row.source)
		bucket := analyticsAccumulatorForTime(buckets, analyticsBucketStart(row.finishedAt, opts.Bucket))
		if logicalReview {
			for _, acc := range []*analyticsAccumulator{total, project, source, bucket} {
				acc.addReview(row)
			}
		}
		if eligibleAttempt {
			agent := analyticsAccumulatorFor(agents, row.agent)
			model := analyticsAccumulatorFor(models, row.model)
			for _, acc := range []*analyticsAccumulator{total, project, source, agent, model, bucket} {
				acc.addAttempt(row)
			}
		}
	}

	snapshot.Summary = total.finish()
	for key, acc := range projects {
		snapshot.Projects = append(snapshot.Projects, AnalyticsProjectRow{Project: key, AnalyticsSummary: acc.finish()})
	}
	sort.Slice(snapshot.Projects, func(i, j int) bool {
		if snapshot.Projects[i].Reviews.Total != snapshot.Projects[j].Reviews.Total {
			return snapshot.Projects[i].Reviews.Total > snapshot.Projects[j].Reviews.Total
		}
		return snapshot.Projects[i].Project < snapshot.Projects[j].Project
	})
	snapshot.Sources = finishAnalyticsDimensions(sources)
	snapshot.Agents = finishAnalyticsDimensions(agents)
	snapshot.Models = finishAnalyticsDimensions(models)
	starts := make([]time.Time, 0, len(buckets))
	for start := range buckets {
		starts = append(starts, start)
	}
	sort.Slice(starts, func(i, j int) bool { return starts[i].Before(starts[j]) })
	seriesStart := time.Time{}
	if !opts.Since.IsZero() {
		seriesStart = analyticsBucketStart(opts.Since, opts.Bucket)
	} else if len(starts) > 0 {
		seriesStart = starts[0]
	}
	seriesUntil := opts.Until.UTC()
	if seriesUntil.IsZero() && len(starts) > 0 {
		seriesUntil = analyticsBucketEnd(starts[len(starts)-1], opts.Bucket)
	}
	for start := seriesStart; !start.IsZero() && start.Before(seriesUntil); start = analyticsBucketEnd(start, opts.Bucket) {
		if len(snapshot.TimeSeries) >= MaxAnalyticsTimeBuckets {
			return nil, ErrAnalyticsRangeTooLarge
		}
		acc := buckets[start]
		if acc == nil {
			acc = &analyticsAccumulator{}
		}
		snapshot.TimeSeries = append(snapshot.TimeSeries, AnalyticsTimeBucket{
			Start: start, End: analyticsBucketEnd(start, opts.Bucket), AnalyticsSummary: acc.finish(),
		})
	}
	return snapshot, nil
}

func (a *analyticsAccumulator) addReview(row analyticsRow) {
	a.summary.Reviews.Total++
	switch row.status {
	case JobStatusDone, JobStatusApplied, JobStatusRebased:
		a.summary.Reviews.Done++
		if row.reviewDuration >= 0 {
			a.reviewDurations = append(a.reviewDurations, row.reviewDuration)
		}
	case JobStatusFailed:
		a.summary.Reviews.Failed++
	case JobStatusCanceled:
		a.summary.Reviews.Canceled++
	case JobStatusSkipped:
		a.summary.Reviews.Skipped++
	}
	if !row.verdict.Valid {
		return
	}
	switch row.status {
	case JobStatusDone, JobStatusApplied, JobStatusRebased:
	default:
		return
	}
	if row.verdict.Int64 == 1 {
		a.summary.Verdicts.Passed++
	} else if row.closed {
		a.summary.Verdicts.FailClosed++
	} else {
		a.summary.Verdicts.FailOpen++
	}
}

func (a *analyticsAccumulator) addAttempt(row analyticsRow) {
	a.summary.Attempts.Eligible++
	a.summary.Cost.EligibleAttempts++
	if row.attemptDuration >= 0 {
		a.attemptDurations = append(a.attemptDurations, row.attemptDuration)
	}
	if row.priced {
		a.summary.Cost.PricedAttempts++
		a.summary.Cost.TotalUSD += row.costUSD
	}
}

func (a *analyticsAccumulator) finish() AnalyticsSummary {
	result := a.summary
	result.Reviews.RunErrors = result.Reviews.Failed
	denominator := result.Reviews.Done + result.Reviews.Failed
	if denominator > 0 {
		result.Reviews.FailureRate = float64(result.Reviews.Failed) / float64(denominator)
		result.Reviews.RunErrorRate = result.Reviews.FailureRate
	}
	result.Verdicts.Rated = result.Verdicts.Passed + result.Verdicts.FailOpen + result.Verdicts.FailClosed
	if result.Verdicts.Rated > 0 {
		failedVerdicts := result.Verdicts.FailOpen + result.Verdicts.FailClosed
		result.Verdicts.FailureRate = float64(failedVerdicts) / float64(result.Verdicts.Rated)
	}
	result.ReviewLatency = analyticsPercentiles(a.reviewDurations)
	result.Attempts.Duration = analyticsPercentiles(a.attemptDurations)
	if result.Cost.EligibleAttempts > 0 {
		result.Cost.Coverage = float64(result.Cost.PricedAttempts) / float64(result.Cost.EligibleAttempts)
		result.Cost.Complete = result.Cost.PricedAttempts == result.Cost.EligibleAttempts
	}
	return result
}

func analyticsPercentiles(values []float64) AnalyticsPercentiles {
	return AnalyticsPercentiles{
		P50Secs: percentile(append([]float64(nil), values...), 0.50),
		P90Secs: percentile(append([]float64(nil), values...), 0.90),
		P99Secs: percentile(append([]float64(nil), values...), 0.99),
	}
}

func isLogicalReview(row analyticsRow) bool {
	if row.panelRole == PanelRoleMember {
		return false
	}
	switch row.jobType {
	case JobTypeReview, JobTypeRange, JobTypeDirty, JobTypeSynthesis, JobTypeCompact:
		return row.status == JobStatusDone || row.status == JobStatusFailed ||
			row.status == JobStatusCanceled || row.status == JobStatusSkipped ||
			row.status == JobStatusApplied || row.status == JobStatusRebased
	default:
		return false
	}
}

func matchesAnalyticsAttemptFilters(row analyticsRow, opts AnalyticsOptions) bool {
	return containsAnalyticsValue(opts.Agents, row.agent) && containsAnalyticsValue(opts.Models, row.model)
}

func containsAnalyticsValue(filter []string, value string) bool {
	if len(filter) == 0 {
		return true
	}
	return slices.Contains(filter, value)
}

func analyticsAccumulatorFor(values map[string]*analyticsAccumulator, key string) *analyticsAccumulator {
	if values[key] == nil {
		values[key] = &analyticsAccumulator{}
	}
	return values[key]
}

func analyticsAccumulatorForTime(values map[time.Time]*analyticsAccumulator, key time.Time) *analyticsAccumulator {
	if values[key] == nil {
		values[key] = &analyticsAccumulator{}
	}
	return values[key]
}

func finishAnalyticsDimensions(values map[string]*analyticsAccumulator) []AnalyticsDimensionRow {
	result := make([]AnalyticsDimensionRow, 0, len(values))
	for key, acc := range values {
		result = append(result, AnalyticsDimensionRow{Value: key, AnalyticsSummary: acc.finish()})
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Reviews.Total != result[j].Reviews.Total {
			return result[i].Reviews.Total > result[j].Reviews.Total
		}
		if result[i].Attempts.Eligible != result[j].Attempts.Eligible {
			return result[i].Attempts.Eligible > result[j].Attempts.Eligible
		}
		return result[i].Value < result[j].Value
	})
	return result
}

func validAnalyticsBucket(bucket AnalyticsBucket) bool {
	switch bucket {
	case AnalyticsBucketHour, AnalyticsBucketDay, AnalyticsBucketWeek, AnalyticsBucketMonth:
		return true
	default:
		return false
	}
}

func analyticsBucketStart(value time.Time, bucket AnalyticsBucket) time.Time {
	value = value.UTC()
	switch bucket {
	case AnalyticsBucketHour:
		return value.Truncate(time.Hour)
	case AnalyticsBucketDay:
		return time.Date(value.Year(), value.Month(), value.Day(), 0, 0, 0, 0, time.UTC)
	case AnalyticsBucketWeek:
		day := time.Date(value.Year(), value.Month(), value.Day(), 0, 0, 0, 0, time.UTC)
		offset := (int(day.Weekday()) + 6) % 7
		return day.AddDate(0, 0, -offset)
	case AnalyticsBucketMonth:
		return time.Date(value.Year(), value.Month(), 1, 0, 0, 0, 0, time.UTC)
	default:
		return time.Time{}
	}
}

func analyticsBucketEnd(start time.Time, bucket AnalyticsBucket) time.Time {
	switch bucket {
	case AnalyticsBucketHour:
		return start.Add(time.Hour)
	case AnalyticsBucketDay:
		return start.AddDate(0, 0, 1)
	case AnalyticsBucketWeek:
		return start.AddDate(0, 0, 7)
	case AnalyticsBucketMonth:
		return start.AddDate(0, 1, 0)
	default:
		return start
	}
}

func appendAnalyticsInFilter(conditions *[]string, args *[]any, column string, values []string) {
	if len(values) == 0 {
		return
	}
	placeholders := make([]string, len(values))
	for i, value := range values {
		placeholders[i] = "?"
		*args = append(*args, value)
	}
	*conditions = append(*conditions, column+" IN ("+strings.Join(placeholders, ",")+")")
}

func analyticsTime(value time.Time) string {
	return value.UTC().Format("2006-01-02 15:04:05.999999999")
}

func sortedUnique(values []string) []string {
	seen := map[string]struct{}{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		if _, found := seen[value]; found {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func queryAnalyticsFilterOptions(q querier, since, until time.Time) (AnalyticsFilterOptions, error) {
	result := AnalyticsFilterOptions{Projects: []string{}, Sources: []string{}, Agents: []string{}, Models: []string{}}
	conditions := []string{"j.finished_at IS NOT NULL"}
	args := []any{}
	if !since.IsZero() {
		conditions = append(conditions, "julianday(j.finished_at) >= julianday(?)")
		args = append(args, analyticsTime(since))
	}
	if !until.IsZero() {
		conditions = append(conditions, "julianday(j.finished_at) < julianday(?)")
		args = append(args, analyticsTime(until))
	}
	where := " WHERE " + strings.Join(conditions, " AND ")
	queries := []struct {
		column   string
		target   *[]string
		nonEmpty bool
	}{
		{"r.name", &result.Projects, false},
		{"COALESCE(j.source, '')", &result.Sources, false},
		{"COALESCE(j.agent, '')", &result.Agents, true},
		{"COALESCE(j.model, '')", &result.Models, true},
	}
	for _, item := range queries {
		itemWhere := where
		if item.nonEmpty {
			itemWhere += " AND " + item.column + " != ''"
		}
		rows, err := q.Query(`SELECT DISTINCT `+item.column+` FROM review_jobs j JOIN repos r ON r.id = j.repo_id`+itemWhere+` ORDER BY 1`, args...)
		if err != nil {
			return result, err
		}
		for rows.Next() {
			var value string
			if err := rows.Scan(&value); err != nil {
				_ = rows.Close()
				return result, err
			}
			*item.target = append(*item.target, value)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return result, err
		}
		if err := rows.Close(); err != nil {
			return result, err
		}
	}
	return result, nil
}
