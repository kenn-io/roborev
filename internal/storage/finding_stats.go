package storage

import (
	"database/sql"
	"encoding/json/jsontext"
	"fmt"
	"sort"
	"strings"
	"time"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/rebuttal"
	"go.kenn.io/roborev/pkg/structuredreview"
)

// DefaultFindingMinSamples is a display threshold, never a read/execution cap.
// Adapted from github/lever at f859d0a, automations/precision-review/
// improver-prompt.md's five-example evidence rule.
const DefaultFindingMinSamples = 5

// FindingOutcomeCounts keeps raw denominators even when rates are unavailable.
// Positive means agent-confirmed, not proof that a fix was applied.
type FindingOutcomeCounts struct {
	Findings       int      `json:"findings"`
	Labeled        int      `json:"labeled"`
	Positive       int      `json:"positive"`
	Negative       int      `json:"negative"`
	Excluded       int      `json:"excluded"`
	Unlabeled      int      `json:"unlabeled"`
	ActedOnRate    *float64 `json:"acted_on_rate"`
	RateState      string   `json:"rate_state"`
	FailingReviews int      `json:"failing_reviews"`
	ClosedReviews  int      `json:"closed_reviews"`
	ResolutionRate *float64 `json:"resolution_rate"`
}

// FindingStatsRow describes one stable grouping cell. Severity spend overlaps
// across cells; other groupings include units with zero structured findings.
type FindingStatsRow struct {
	Key string `json:"key"`
	FindingOutcomeCounts
	Reviews           int           `json:"reviews"`
	StructuredReviews int           `json:"structured_reviews"`
	Cost              CostAggregate `json:"cost"`
	CostPerPositive   *float64      `json:"cost_per_positive"`
}

// FindingEvidence identifies an assessment within a review version. Index is
// zero-based and is meaningful only with ReviewUUID; it is not a persistent ID.
type FindingEvidence struct {
	JobID           int64
	ReviewUUID      string
	Index           int
	Finding         structuredreview.Finding
	Outcome         string // confirmed, refuted, superseded, or unlabeled
	Tier            string // R for validated rebuttals; empty otherwise
	AssessmentJobID int64
	AssessmentUUID  string
	Explanation     string
}

// RebuttalCoverage reports why retained assessments did not become labels.
type RebuttalCoverage struct {
	Valid      int `json:"valid"`
	Unbound    int `json:"unbound"`
	Stale      int `json:"stale"`
	Ambiguous  int `json:"ambiguous"`
	Invalid    int `json:"invalid"`
	Ineligible int `json:"ineligible"`
}

// FindingStats is a completion-window snapshot of logical code reviews.
// Evidence is reusable by insights without exporting full review text in summary.
type FindingStats struct {
	Members           *MemberStats                    `json:"members,omitempty"`
	Since             time.Time                       `json:"since"`
	AsOf              time.Time                       `json:"as_of"`
	RepoPath          string                          `json:"repo_path,omitempty"`
	Branch            string                          `json:"branch,omitempty"`
	TimeBasis         string                          `json:"time_basis"`
	GroupBy           string                          `json:"group_by"`
	MinSamples        int                             `json:"min_samples"`
	LogicalReviews    int                             `json:"logical_reviews"`
	StructuredReviews int                             `json:"structured_reviews"`
	MissingDocuments  int                             `json:"missing_documents"`
	InvalidDocuments  int                             `json:"invalid_documents"`
	LegacyDocuments   int                             `json:"legacy_documents"`
	UnableDocuments   int                             `json:"unable_documents"`
	Rebuttals         RebuttalCoverage                `json:"rebuttals"`
	TierCoverage      map[string]FindingOutcomeCounts `json:"tier_coverage"`
	TierLabeled       map[string]int                  `json:"tier_labeled"`
	UnavailableTiers  []string                        `json:"unavailable_tiers"`
	Total             FindingOutcomeCounts            `json:"total"`
	Cost              CostAggregate                   `json:"cost"`
	Rows              []FindingStatsRow               `json:"rows"`
	Evidence          []FindingEvidence               `json:"-"`
}

// ValidateFindingOptions is shared by storage and HTTP/CLI boundaries.
func ValidateFindingOptions(enabled bool, group string, minimum int) error {
	if !enabled && (group != "" || minimum != 0) {
		return fmt.Errorf("grouping and minimum samples require findings")
	}
	if minimum < 0 {
		return fmt.Errorf("minimum samples must be nonnegative")
	}
	switch group {
	case "", "severity", "review-type", "agent", "model", "arm", "member":
		return nil
	}
	return fmt.Errorf("invalid findings grouping %q", group)
}

// GetFindingStats computes a consistent snapshot, normalized like GetSummary.
func (db *DB) GetFindingStats(opts SummaryOptions, groupBy string) (*FindingStats, error) {
	if opts.RepoPath != "" {
		opts.RepoPath = normalizeRepoPathBestEffort(opts.RepoPath)
	}
	if opts.AsOf.IsZero() {
		opts.AsOf = time.Now()
	}
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	return findingStats(tx, opts, groupBy)
}

type findingReview struct {
	id         int64
	repoID     int64
	uuid       string
	run        string
	reviewUUID string
	kind       string
	agent      string
	model      string
	reviewType string
	source     string
	ciBase     string
	finished   time.Time
	finishRaw  string
	enqueued   string
	closed     bool
	failed     bool
	document   sql.NullString
	doc        *structuredreview.Document
	executions []findingExecutionCost
	evidence   []FindingEvidence
	keys       []string
}

type findingExecutionCost struct {
	id   int64
	cost CostAggregate
}

func findingStats(q querier, opts SummaryOptions, groupBy string) (*FindingStats, error) {
	if err := ValidateFindingOptions(true, groupBy, opts.MinSamples); err != nil {
		return nil, err
	}
	if groupBy == "" {
		groupBy = "severity"
	}
	minimum := opts.MinSamples
	if minimum == 0 {
		minimum = DefaultFindingMinSamples
	}
	f := &FindingStats{
		Since: opts.Since, AsOf: opts.AsOf, RepoPath: opts.RepoPath, Branch: opts.Branch,
		TimeBasis: "finished_at", GroupBy: groupBy, MinSamples: minimum,
		Rows: []FindingStatsRow{}, TierLabeled: map[string]int{"R": 0},
		UnavailableTiers: []string{"K", "D", "panel R"},
		TierCoverage:     map[string]FindingOutcomeCounts{"R": {}},
	}
	conditions := `j.status='done' AND j.finished_at IS NOT NULL AND COALESCE(j.panel_role,'')!='member'
 AND (j.job_type IN ('review','range','dirty','synthesis') OR (COALESCE(j.job_type,'')='' AND (j.commit_id IS NOT NULL OR j.git_ref='dirty' OR instr(j.git_ref,'..')>0
 OR EXISTS (SELECT 1 FROM job_content legacy_content WHERE legacy_content.job_id=j.id AND legacy_content.diff_content IS NOT NULL))))
 AND julianday(j.finished_at)>=julianday(?) AND julianday(j.finished_at)<=julianday(?)`
	args := []any{opts.Since.UTC().Format(time.RFC3339Nano), opts.AsOf.UTC().Format(time.RFC3339Nano)}
	if opts.RepoPath != "" {
		conditions += " AND r.root_path=?"
		args = append(args, opts.RepoPath)
	}
	if opts.Branch != "" {
		conditions += " AND j.branch=?"
		args = append(args, opts.Branch)
	}
	rows, err := q.Query(`SELECT j.id, j.repo_id, COALESCE(j.uuid,''), COALESCE(j.panel_run_uuid,''),
 COALESCE(rv.uuid,''), j.job_type, j.agent, COALESCE(j.model,''),
 COALESCE(j.review_type,''), COALESCE(j.source,''), COALESCE(j.ci_base_branch,''),
 j.finished_at, j.enqueued_at, COALESCE(rv.closed,0),
 COALESCE(rv.verdict_bool=0,0), rv.structured_output
 FROM review_jobs j JOIN repos r ON r.id=j.repo_id LEFT JOIN reviews rv ON rv.job_id=j.id WHERE `+conditions+` ORDER BY j.id`, args...)
	if err != nil {
		return nil, err
	}
	var reviews []*findingReview
	for rows.Next() {
		r := &findingReview{}
		var finished, enqueued string
		if err = rows.Scan(&r.id, &r.repoID, &r.uuid, &r.run, &r.reviewUUID, &r.kind, &r.agent, &r.model, &r.reviewType, &r.source, &r.ciBase, &finished, &enqueued, &r.closed, &r.failed, &r.document); err != nil {
			rows.Close()
			return nil, err
		}
		r.finished = parseSQLiteTime(finished)
		r.finishRaw = finished
		r.enqueued = enqueued
		// SQLite date arithmetic rounds submillisecond times. Apply the exact
		// completion bounds in Go after the coarse SQL selection.
		if !r.finished.Before(opts.Since) && !r.finished.After(opts.AsOf) {
			reviews = append(reviews, r)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	for _, r := range reviews {
		f.LogicalReviews++
		if !r.document.Valid || r.document.String == "" {
			f.MissingDocuments++
		} else {
			doc, e := structuredreview.Decode(jsontext.Value(r.document.String))
			switch {
			case e != nil:
				f.InvalidDocuments++
			case doc.Legacy != nil:
				f.LegacyDocuments++
			case doc.UnableToReview():
				f.UnableDocuments++
			default:
				r.doc = &doc
				f.StructuredReviews++
			}
		}
		if r.doc != nil {
			for i, found := range r.doc.Findings {
				r.evidence = append(r.evidence, FindingEvidence{JobID: r.id, ReviewUUID: r.reviewUUID, Index: i, Finding: found, Outcome: "unlabeled"})
			}
		}
	}
	if err = applyFindingLabels(q, reviews, opts.AsOf, &f.Rebuttals); err != nil {
		return nil, err
	}
	cells := map[string]*FindingStatsRow{}
	totalExecutions := map[int64]bool{}
	cellExecutions := map[string]map[int64]bool{}
	for _, r := range reviews {
		r.executions, err = findingReviewCosts(q, r, opts.AsOf)
		if err != nil {
			return nil, err
		}
		addDistinctFindingCost(&f.Cost, r.executions, totalExecutions)
		if groupBy == "severity" {
			seen := map[string]bool{}
			for _, e := range r.evidence {
				seen[e.Finding.Severity] = true
			}
			for key := range seen {
				r.keys = append(r.keys, key)
			}
		} else {
			switch groupBy {
			case "agent":
				r.keys = []string{r.agent}
			case "model":
				r.keys = []string{r.model}
			case "review-type":
				key := r.reviewType
				if config.IsDefaultReviewType(key) {
					key = config.ReviewTypeDefault
				}
				r.keys = []string{key}
			case "arm":
				r.keys, err = findingArmKeys(q, r)
				if err != nil {
					return nil, err
				}
			}
		}
		addReviewCounts(&f.Total, r)
		// The R cohort is local standalone findings, not a promise that every
		// stored session can still be resumed. K/D adapters remain unavailable.
		if r.kind != JobTypeSynthesis && r.source != JobSourceCI && r.ciBase == "" {
			cohort := f.TierCoverage["R"]
			addReviewCounts(&cohort, r)
			for _, e := range r.evidence {
				addFindingOutcome(&cohort, e.Outcome)
			}
			f.TierCoverage["R"] = cohort
		}
		for _, e := range r.evidence {
			addFindingOutcome(&f.Total, e.Outcome)
			if e.Tier == "R" && (e.Outcome == "confirmed" || e.Outcome == "refuted") {
				f.TierLabeled["R"]++
			}
		}
		f.Evidence = append(f.Evidence, r.evidence...)
		for _, key := range r.keys {
			c := cells[key]
			if c == nil {
				c = &FindingStatsRow{Key: key}
				cells[key] = c
				cellExecutions[key] = map[int64]bool{}
			}
			c.Reviews++
			if r.doc != nil {
				c.StructuredReviews++
			}
			addReviewCounts(&c.FindingOutcomeCounts, r)
			addDistinctFindingCost(&c.Cost, r.executions, cellExecutions[key])
			for _, e := range r.evidence {
				if groupBy != "severity" || e.Finding.Severity == key {
					addFindingOutcome(&c.FindingOutcomeCounts, e.Outcome)
				}
			}
		}
	}
	finishFindingCounts(&f.Total, minimum)
	for tier, cohort := range f.TierCoverage {
		finishFindingCounts(&cohort, minimum)
		f.TierCoverage[tier] = cohort
	}
	finishFindingCost(&f.Cost)
	keys := make([]string, 0, len(cells))
	for key := range cells {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		c := cells[key]
		finishFindingCounts(&c.FindingOutcomeCounts, minimum)
		finishFindingCost(&c.Cost)
		if c.Positive > 0 && c.Cost.JobsWithCost > 0 {
			v := c.Cost.TotalUSD / float64(c.Positive)
			c.CostPerPositive = &v
		}
		f.Rows = append(f.Rows, *c)
	}
	if groupBy == "member" {
		f.Members, err = findingMemberStats(q, reviews, opts.AsOf)
		if err != nil {
			return nil, err
		}
	}
	return f, nil
}

func addReviewCounts(c *FindingOutcomeCounts, r *findingReview) {
	if r.failed {
		c.FailingReviews++
		if r.closed {
			c.ClosedReviews++
		}
	}
}

func addFindingOutcome(c *FindingOutcomeCounts, outcome string) {
	c.Findings++
	switch outcome {
	case "confirmed":
		c.Positive++
	case "refuted":
		c.Negative++
	case "superseded":
		c.Excluded++
	default:
		c.Unlabeled++
	}
}

func finishFindingCounts(c *FindingOutcomeCounts, minimum int) {
	c.Labeled = c.Positive + c.Negative
	c.RateState = "unavailable"
	c.ActedOnRate = nil
	if c.Labeled > 0 {
		if c.Labeled < minimum {
			c.RateState = "below_minimum"
		} else {
			v := float64(c.Positive) / float64(c.Labeled)
			c.ActedOnRate = &v
			c.RateState = "available"
		}
	}
	if c.FailingReviews > 0 {
		v := float64(c.ClosedReviews) / float64(c.FailingReviews)
		c.ResolutionRate = &v
	}
}

func addFindingCost(c *CostAggregate, other CostAggregate) {
	c.TotalUSD += other.TotalUSD
	c.JobsTotal += other.JobsTotal
	c.JobsWithCost += other.JobsWithCost
}

func finishFindingCost(c *CostAggregate) {
	c.Complete = c.JobsTotal > 0 && c.JobsWithCost == c.JobsTotal
}

func addDistinctFindingCost(c *CostAggregate, executions []findingExecutionCost, seen map[int64]bool) {
	for _, execution := range executions {
		if !seen[execution.id] {
			addFindingCost(c, execution.cost)
			seen[execution.id] = true
		}
	}
}

func findingReviewCosts(q querier, r *findingReview, asOf time.Time) ([]findingExecutionCost, error) {
	// Select retained parent/member executions through the parent cohort. This
	// includes failed invoked and advisory members without charging rebuttal tasks.
	where := "j.id=?"
	args := []any{r.id}
	if r.run != "" {
		where = "(j.id=? OR (j.panel_run_uuid=? AND j.panel_role='member'))"
		args = append(args, r.run)
	}
	rows, err := q.Query(`SELECT j.id,j.finished_at,CASE WHEN `+hasCost+` THEN 1 ELSE 0 END,CASE WHEN `+hasCost+` THEN json_extract(j.token_usage,'$.cost_usd') ELSE 0 END FROM review_jobs j WHERE `+where+` AND `+costEligible, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var executions []findingExecutionCost
	for rows.Next() {
		var execution findingExecutionCost
		var finished string
		if err = rows.Scan(&execution.id, &finished, &execution.cost.JobsWithCost, &execution.cost.TotalUSD); err != nil {
			return nil, err
		}
		if parseSQLiteTime(finished).After(asOf) {
			continue
		}
		execution.cost.JobsTotal = 1
		executions = append(executions, execution)
	}
	return executions, rows.Err()
}

func findingArmKeys(q querier, r *findingReview) ([]string, error) {
	kind, unit := ReviewUnitJob, r.uuid
	if r.run != "" {
		kind, unit = ReviewUnitPanel, r.run
	}
	rows, err := q.Query(`SELECT a.experiment_id,d.definition_hash,a.arm FROM experiment_assignments a JOIN experiment_definitions d ON d.experiment_id=a.experiment_id WHERE a.review_unit_kind=? AND a.review_unit_uuid=? ORDER BY a.experiment_id,d.definition_hash,a.arm`, kind, unit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys []string
	for rows.Next() {
		var id, hash, arm string
		if err = rows.Scan(&id, &hash, &arm); err != nil {
			return nil, err
		}
		keys = append(keys, fmt.Sprintf("%s@%s/%s", id, hash, arm))
	}
	if len(keys) == 0 {
		keys = []string{"unassigned"}
	}
	return keys, rows.Err()
}

// findingChronologyBefore compares at the precision both stored boundaries retain.
// ClaimJob uses preciseTimestampAt (fixed-width nanos), but insertJobTx and
// completeJob write RFC3339 seconds; reruns can also enqueue with nanos. Without
// a fraction, events within that second cannot be ordered. Explicit fractional
// zeroes remain precise. As-of filtering and latest-label ordering stay exact.
func findingChronologyBefore(a, b string) bool {
	first, second := parseSQLiteTime(a), parseSQLiteTime(b)
	if !strings.ContainsAny(a, ".,") || !strings.ContainsAny(b, ".,") {
		first = first.Truncate(time.Second)
		second = second.Truncate(time.Second)
	}
	return first.Before(second)
}

func applyFindingLabels(q querier, reviews []*findingReview, asOf time.Time, coverage *RebuttalCoverage) error {
	if len(reviews) == 0 {
		return nil
	}
	byParent := map[int64]*findingReview{}
	byVersion := map[string]*findingReview{}
	for _, r := range reviews {
		byParent[r.id] = r
		if r.reviewUUID != "" {
			byVersion[r.reviewUUID] = r
		}
	}
	// Scan assessments once. Sync retains the anchored review version in the
	// prompt but does not carry machine-local parent_job_id relationships.
	rows, err := q.Query(`SELECT j.id,COALESCE(j.uuid,''),j.parent_job_id,j.repo_id,j.status,j.enqueued_at,COALESCE(j.started_at,''),COALESCE(j.finished_at,''),COALESCE(zstd_decompress(jc.prompt),''),COALESCE(rv.output,'') FROM review_jobs j LEFT JOIN job_content jc ON jc.job_id=j.id LEFT JOIN reviews rv ON rv.job_id=j.id WHERE j.source='rebuttal' AND j.job_type='task' ORDER BY j.id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	latest := map[*FindingEvidence]time.Time{}
	for rows.Next() {
		var id, repoID int64
		var parent sql.NullInt64
		var uuid, status, enqueued, started, finished, prompt, output string
		if err = rows.Scan(&id, &uuid, &parent, &repoID, &status, &enqueued, &started, &finished, &prompt, &output); err != nil {
			return err
		}
		end := parseSQLiteTime(finished)
		if end.After(asOf) || parseSQLiteTime(enqueued).After(asOf) || status == "queued" || status == "running" {
			continue
		}
		selection, selectionErr := rebuttal.DecodeSelection(prompt)
		r := byParent[parent.Int64]
		if !parent.Valid {
			r = byVersion[selection.ReviewUUID]
		}
		if r == nil {
			continue
		}
		if r.kind == JobTypeSynthesis || r.source == JobSourceCI || r.ciBase != "" {
			coverage.Ineligible++
			continue
		}
		start := parseSQLiteTime(started)
		if findingChronologyBefore(r.finishRaw, r.enqueued) || status != "done" || end.IsZero() || start.IsZero() ||
			findingChronologyBefore(enqueued, r.finishRaw) || findingChronologyBefore(started, enqueued) ||
			findingChronologyBefore(finished, started) || repoID != r.repoID {
			coverage.Invalid++
			continue
		}
		if selectionErr != nil {
			coverage.Unbound++
			continue
		}
		if selection.ReviewUUID != r.reviewUUID || r.reviewUUID == "" {
			coverage.Stale++
			continue
		}
		assessment, e := rebuttal.Parse(output)
		if e != nil {
			coverage.Invalid++
			continue
		}
		if r.doc == nil {
			coverage.Invalid++
			continue
		}
		index := rebuttal.MatchFinding(selection.Finding, r.doc.Findings)
		if index < 0 {
			coverage.Ambiguous++
			continue
		}
		coverage.Valid++
		evidence := &r.evidence[index]
		previous, seen := latest[evidence]
		key, previousKey := uuid, evidence.AssessmentUUID
		if key == "" {
			key = fmt.Sprintf("%020d", id)
		}
		if previousKey == "" {
			previousKey = fmt.Sprintf("%020d", evidence.AssessmentJobID)
		}
		if seen && (end.Before(previous) || (end.Equal(previous) && (key < previousKey || (key == previousKey && id <= evidence.AssessmentJobID)))) {
			continue
		}
		latest[evidence] = end
		evidence.Outcome = assessment.Outcome
		evidence.Tier = "R"
		evidence.AssessmentJobID = id
		evidence.AssessmentUUID = uuid
		evidence.Explanation = assessment.Explanation
	}
	return rows.Err()
}
