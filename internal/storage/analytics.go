package storage

import (
	"database/sql"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"time"
)

const AnalyticsSchemaVersion = 1

type AnalyticsBucket string

const (
	AnalyticsBucketHour  AnalyticsBucket = "hour"
	AnalyticsBucketDay   AnalyticsBucket = "day"
	AnalyticsBucketWeek  AnalyticsBucket = "week"
	AnalyticsBucketMonth AnalyticsBucket = "month"
)

// AnalyticsSplit names the dimension that breaks the time series into one
// series per value.
type AnalyticsSplit string

const (
	AnalyticsSplitAgent   AnalyticsSplit = "agent"
	AnalyticsSplitModel   AnalyticsSplit = "model"
	AnalyticsSplitProject AnalyticsSplit = "project"
	AnalyticsSplitSource  AnalyticsSplit = "source"
)

type AnalyticsOptions struct {
	Since    time.Time
	Until    time.Time
	Projects []string
	Sources  []string
	Agents   []string
	Models   []string
	Bucket   AnalyticsBucket
	// Split, when set, adds one time series per value of that dimension.
	Split AnalyticsSplit
}

type AnalyticsFilters struct {
	Since    *time.Time      `json:"since,omitempty"`
	Until    time.Time       `json:"until"`
	Projects []string        `json:"projects"`
	Sources  []string        `json:"sources"`
	Agents   []string        `json:"agents"`
	Models   []string        `json:"models"`
	Bucket   AnalyticsBucket `json:"bucket"`
	Split    AnalyticsSplit  `json:"split,omitempty"`
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

// AnalyticsSplitSeries is the time series for one value of the split
// dimension. Rows are attributed by their own agent, model, project, or
// source, so logical reviews and attempts each count under their own value.
type AnalyticsSplitSeries struct {
	Value      string                `json:"value"`
	Summary    AnalyticsSummary      `json:"summary"`
	TimeSeries []AnalyticsTimeBucket `json:"time_series"`
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
	SplitSeries   []AnalyticsSplitSeries  `json:"split_series"`
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
	if opts.Split != "" && !ValidAnalyticsSplit(opts.Split) {
		return nil, fmt.Errorf("invalid analytics split %q", opts.Split)
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
	return aggregateAnalytics(rows, opts)
}

// analyticsRowsQuery selects every finished job in the time window. Project
// and source filters are applied in Go so the same rows also supply the filter
// choices, which must list every value in the window. Every review_jobs column
// read here must stay in idx_review_jobs_analytics: they are stored after the
// large prompt column, and reading them from the table walks its overflow
// pages.
func analyticsRowsQuery(opts AnalyticsOptions) (string, []any) {
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
	return `
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
		ORDER BY datetime(j.finished_at), j.id`, args
}

func queryAnalyticsRows(q querier, opts AnalyticsOptions) ([]analyticsRow, error) {
	query, args := analyticsRowsQuery(opts)
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
		Models: sortedUnique(opts.Models), Bucket: opts.Bucket, Split: opts.Split,
	}
	if !opts.Since.IsZero() {
		since := opts.Since.UTC()
		filters.Since = &since
	}
	snapshot := &AnalyticsSnapshot{
		SchemaVersion: AnalyticsSchemaVersion, Filters: filters,
		Projects: []AnalyticsProjectRow{},
		Sources:  []AnalyticsDimensionRow{}, Agents: []AnalyticsDimensionRow{},
		Models: []AnalyticsDimensionRow{}, SplitSeries: []AnalyticsSplitSeries{},
	}
	total := &analyticsAccumulator{}
	projects := map[string]*analyticsAccumulator{}
	sources := map[string]*analyticsAccumulator{}
	agents := map[string]*analyticsAccumulator{}
	models := map[string]*analyticsAccumulator{}
	buckets := map[time.Time]*analyticsAccumulator{}
	splitTotals := map[string]*analyticsAccumulator{}
	splitBuckets := map[string]map[time.Time]*analyticsAccumulator{}
	optionProjects := map[string]struct{}{}
	optionSources := map[string]struct{}{}
	optionAgents := map[string]struct{}{}
	optionModels := map[string]struct{}{}

	for _, row := range rows {
		optionProjects[row.project] = struct{}{}
		optionSources[row.source] = struct{}{}
		if row.agent != "" {
			optionAgents[row.agent] = struct{}{}
		}
		if row.model != "" {
			optionModels[row.model] = struct{}{}
		}
		if !containsAnalyticsValue(opts.Projects, row.project) || !containsAnalyticsValue(opts.Sources, row.source) {
			continue
		}
		logicalReview := isLogicalReview(row)
		eligibleAttempt := row.eligible && matchesAnalyticsAttemptFilters(row, opts)
		if !logicalReview && !eligibleAttempt {
			continue
		}
		project := analyticsAccumulatorFor(projects, row.project)
		source := analyticsAccumulatorFor(sources, row.source)
		bucketStart := analyticsBucketStart(row.finishedAt, opts.Bucket)
		bucket := analyticsAccumulatorForTime(buckets, bucketStart)
		reviewAccs := []*analyticsAccumulator{total, project, source, bucket}
		attemptAccs := []*analyticsAccumulator{total, project, source, bucket}
		if opts.Split != "" {
			key := analyticsSplitValue(row, opts.Split)
			if splitBuckets[key] == nil {
				splitBuckets[key] = map[time.Time]*analyticsAccumulator{}
			}
			splitTotal := analyticsAccumulatorFor(splitTotals, key)
			splitBucket := analyticsAccumulatorForTime(splitBuckets[key], bucketStart)
			reviewAccs = append(reviewAccs, splitTotal, splitBucket)
			attemptAccs = append(attemptAccs, splitTotal, splitBucket)
		}
		if logicalReview {
			for _, acc := range reviewAccs {
				acc.addReview(row)
			}
		}
		if eligibleAttempt {
			attemptAccs = append(attemptAccs,
				analyticsAccumulatorFor(agents, row.agent), analyticsAccumulatorFor(models, row.model))
			for _, acc := range attemptAccs {
				acc.addAttempt(row)
			}
		}
	}

	snapshot.Options = AnalyticsFilterOptions{
		Projects: sortedAnalyticsOptions(optionProjects), Sources: sortedAnalyticsOptions(optionSources),
		Agents: sortedAnalyticsOptions(optionAgents), Models: sortedAnalyticsOptions(optionModels),
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
	seriesStart, seriesUntil := analyticsSeriesBounds(buckets, opts)
	snapshot.TimeSeries = analyticsTimeSeries(buckets, seriesStart, seriesUntil, opts.Bucket)
	for _, row := range finishAnalyticsDimensions(splitTotals) {
		snapshot.SplitSeries = append(snapshot.SplitSeries, AnalyticsSplitSeries{
			Value: row.Value, Summary: row.AnalyticsSummary,
			TimeSeries: analyticsTimeSeries(splitBuckets[row.Value], seriesStart, seriesUntil, opts.Bucket),
		})
	}
	return snapshot, nil
}

// analyticsSeriesBounds returns the first bucket start and the exclusive end
// of the time series: the requested window when bounded, otherwise the span
// of buckets that hold data.
func analyticsSeriesBounds(
	buckets map[time.Time]*analyticsAccumulator, opts AnalyticsOptions,
) (time.Time, time.Time) {
	starts := slices.SortedFunc(maps.Keys(buckets), time.Time.Compare)
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
	return seriesStart, seriesUntil
}

// analyticsTimeSeries emits one bucket per period in [start, until), filling
// periods without data with empty summaries.
func analyticsTimeSeries(
	buckets map[time.Time]*analyticsAccumulator, start, until time.Time, bucket AnalyticsBucket,
) []AnalyticsTimeBucket {
	count := 0
	for at := start; !at.IsZero() && at.Before(until); at = analyticsBucketEnd(at, bucket) {
		count++
	}
	series := make([]AnalyticsTimeBucket, 0, count)
	for ; !start.IsZero() && start.Before(until); start = analyticsBucketEnd(start, bucket) {
		acc := buckets[start]
		if acc == nil {
			acc = &analyticsAccumulator{}
		}
		series = append(series, AnalyticsTimeBucket{
			Start: start, End: analyticsBucketEnd(start, bucket), AnalyticsSummary: acc.finish(),
		})
	}
	return series
}

func analyticsSplitValue(row analyticsRow, split AnalyticsSplit) string {
	switch split {
	case AnalyticsSplitAgent:
		return row.agent
	case AnalyticsSplitModel:
		return row.model
	case AnalyticsSplitProject:
		return row.project
	case AnalyticsSplitSource:
		return row.source
	default:
		return ""
	}
}

// ValidAnalyticsSplit reports whether split names a supported dimension.
func ValidAnalyticsSplit(split AnalyticsSplit) bool {
	switch split {
	case AnalyticsSplitAgent, AnalyticsSplitModel, AnalyticsSplitProject, AnalyticsSplitSource:
		return true
	default:
		return false
	}
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
	sorted := slices.Clone(values)
	slices.Sort(sorted)
	return AnalyticsPercentiles{
		P50Secs: percentileSorted(sorted, 0.50),
		P90Secs: percentileSorted(sorted, 0.90),
		P99Secs: percentileSorted(sorted, 0.99),
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

// sortedAnalyticsOptions returns a non-nil slice so empty choices encode as
// JSON arrays.
func sortedAnalyticsOptions(values map[string]struct{}) []string {
	result := slices.AppendSeq(make([]string, 0, len(values)), maps.Keys(values))
	slices.Sort(result)
	return result
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
