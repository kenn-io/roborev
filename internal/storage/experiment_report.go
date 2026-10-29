package storage

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"go.kenn.io/roborev/internal/agentname"
	"go.kenn.io/roborev/pkg/structuredreview"
)

// ErrInvalidExperimentReportOptions distinguishes invalid cohorts from missing experiments.
var ErrInvalidExperimentReportOptions = errors.New("invalid experiment report options")

// ExperimentReportOptions selects enrollment cohorts, not historical result snapshots.
// A bound is omitted only when its time is zero and its Provided flag is false.
// Zero MinSubjects uses the default of four per arm.
type ExperimentReportOptions struct {
	Since         time.Time
	Until         time.Time
	SinceProvided bool
	UntilProvided bool
	MinSubjects   int
}

type ExperimentReport struct {
	ID             string                       `json:"id"`
	DefinitionHash string                       `json:"definition_hash"`
	Ratio          *float64                     `json:"ratio"`
	AsOf           time.Time                    `json:"as_of"`
	Since          *time.Time                   `json:"since"`
	Until          *time.Time                   `json:"until"`
	MinSubjects    int                          `json:"min_subjects"`
	Default        ExperimentArmReport          `json:"default"`
	Experiment     ExperimentArmReport          `json:"experiment"`
	Integrity      ExperimentIntegrity          `json:"integrity"`
	Allocation     ExperimentAllocation         `json:"allocation"`
	Labels         ExperimentLabelCoverage      `json:"labels"`
	Metrics        []ExperimentMetricComparison `json:"metrics"`
	Method         ExperimentReportMethod       `json:"method"`
	Success        *ExperimentSuccessResult     `json:"success,omitempty"`
	definitionJSON string
}

// ExperimentReportMethod describes exploratory subject-label permutation tests
// and percentile intervals from a within-arm subject bootstrap. Bootstrap
// intervals always use Resamples draws, even when permutations are exact.
type ExperimentReportMethod struct {
	Resamples    int     `json:"resamples"`
	Confidence   float64 `json:"confidence"`
	Exploratory  bool    `json:"exploratory"`
	Adjusted     bool    `json:"adjusted"`
	OutcomeBasis string  `json:"outcome_basis"`
}

type ExperimentArmReport struct {
	CanonicalStatuses        ExperimentCanonicalStatuses `json:"canonical_statuses"`
	Subjects                 int                         `json:"subjects"`
	ReviewUnits              int                         `json:"review_units"`
	TerminalUnits            int                         `json:"terminal_units"`
	PendingUnits             int                         `json:"pending_units"`
	FailedUnits              int                         `json:"failed_units"`
	CanceledUnits            int                         `json:"canceled_units"`
	SkippedUnits             int                         `json:"skipped_units"`
	AttributionCompleteUnits int                         `json:"attribution_complete_units"`
	OutputUnits              int                         `json:"output_units"`
	Cost                     ExperimentCostCoverage      `json:"cost"`
	CostliestUnits           []ExperimentCostliestUnit   `json:"costliest_units"`
}

// ExperimentCanonicalStatuses counts the sole output job of each review unit.
// Component failures are separately included in FailedUnits and CanceledUnits.
type ExperimentCanonicalStatuses struct {
	Done     int `json:"done"`
	Applied  int `json:"applied"`
	Rebased  int `json:"rebased"`
	Failed   int `json:"failed"`
	Canceled int `json:"canceled"`
	Skipped  int `json:"skipped"`
	Queued   int `json:"queued"`
	Running  int `json:"running"`
	Missing  int `json:"missing"`
}

type ExperimentCostCoverage struct {
	EligibleComponents int      `json:"eligible_components"`
	PricedComponents   int      `json:"priced_components"`
	CompleteUnits      int      `json:"complete_units"`
	RecordedUSD        *float64 `json:"recorded_usd"`
	Complete           bool     `json:"complete"`
	Reason             string   `json:"reason,omitempty"`
}

type ExperimentCostliestUnit struct {
	Kind        string                         `json:"kind"`
	UUID        string                         `json:"uuid"`
	RecordedUSD float64                        `json:"recorded_usd"`
	Complete    bool                           `json:"complete"`
	Components  []ExperimentComponentReference `json:"components"`
}

type ExperimentComponentReference struct {
	ProviderSessionIDs []string `json:"provider_session_ids,omitempty"`
	JobID              int64    `json:"job_id"`
	JobUUID            string   `json:"job_uuid"`
	Role               string   `json:"role,omitempty"`
	Agent              string   `json:"agent"`
	Provider           string   `json:"provider,omitempty"`
	SessionID          string   `json:"session_id,omitempty"`
}

type ExperimentIntegrity struct {
	Complete            bool   `json:"complete"`
	ConflictingSubjects int    `json:"conflicting_subjects"`
	IncompleteUnits     int    `json:"incomplete_units"`
	Reason              string `json:"reason,omitempty"`
}

type ExperimentAllocation struct {
	DefaultSubjects      int      `json:"default_subjects"`
	ExperimentalSubjects int      `json:"experimental_subjects"`
	ExpectedDefault      *float64 `json:"expected_default"`
	ExpectedExperimental *float64 `json:"expected_experimental"`
	ChiSquare            *float64 `json:"chi_square"`
	PValue               *float64 `json:"p_value"`
	Approximate          bool     `json:"approximate"`
	Warning              string   `json:"warning,omitempty"`
}

type ExperimentLabelCoverage struct {
	Available      bool                           `json:"available"`
	Coverage       *float64                       `json:"coverage"`
	Reason         string                         `json:"reason"`
	RefutedReviews []ExperimentComponentReference `json:"refuted_reviews"`
}

// ExperimentMetricValue weights subjects equally after aggregating their units.
// Duration metrics use each subject's review-duration percentile; other metrics
// use its mean. Rates are fractions, and costs are USD. Coverage counts only
// the units eligible for that particular metric.
type ExperimentMetricValue struct {
	Value         *float64 `json:"value"`
	Subjects      int      `json:"subjects"`
	Units         int      `json:"units"`
	ExpectedUnits int      `json:"expected_units"`
	Coverage      *float64 `json:"coverage"`
	Complete      bool     `json:"complete"`
}

type ExperimentInterval struct {
	Low  float64 `json:"low"`
	High float64 `json:"high"`
}

// ExperimentMetricComparison reports experiment minus default. Approximate
// describes permutation p-values and randomization thresholds; bootstrap
// intervals are always resampled. No multiplicity adjustment is applied.
type ExperimentMetricComparison struct {
	Metric                 string                `json:"metric"`
	Default                ExperimentMetricValue `json:"default"`
	Experiment             ExperimentMetricValue `json:"experiment"`
	Delta                  *float64              `json:"delta"`
	Interval               *ExperimentInterval   `json:"interval"`
	PValue                 *float64              `json:"p_value"`
	RandomizationThreshold *float64              `json:"randomization_threshold"`
	Approximate            bool                  `json:"approximate"`
	Permutations           int                   `json:"permutations"`
	State                  string                `json:"state"`
	Reason                 string                `json:"reason,omitempty"`
}

// findingOutcomes leaves room for a provider with durable finding and review identities.
// A parent job ID alone cannot deduplicate labels or identify a replaced review.
type findingOutcomes interface {
	coverage() ExperimentLabelCoverage
}
type rebuttalFindingOutcomes struct{}

func (rebuttalFindingOutcomes) coverage() ExperimentLabelCoverage {
	return ExperimentLabelCoverage{Reason: "Rebuttals have no durable finding identity or original-review version; finding labels and refuted-review lists are unavailable."}
}

type experimentReportUnit struct {
	kind, uuid, arm, subject, plan string
	jobs                           []experimentReportJob
	attributed, terminal           bool
	canonical                      int
	values                         map[string]float64
	expected                       map[string]bool
	cost                           ExperimentCostCoverage
}

type experimentReportJob struct {
	ref                                                                                          ExperimentComponentReference
	status, member, structured, jobType, reviewType, analysisType, source, jobError, minSeverity string
	memberIndex                                                                                  int
	nonVoting, invoked, eligible, priced                                                         bool
	started, finished                                                                            time.Time
	finishedWholeSecond                                                                          bool
	dollars                                                                                      float64
}

// These deliberately narrow projections read the persisted daemon plan without
// exporting configuration or tying storage to daemon execution types.
type experimentReportJobPlan struct {
	Agent            string
	PanelMemberName  string
	PanelMemberIndex int
}
type experimentReportPanelPlan struct {
	Members   []experimentReportJobPlan
	Synthesis experimentReportJobPlan
}

// GetExperimentReport reads attribution and current retained attempts in one
// database snapshot. Retries can replace earlier status, timing and spend.
func (db *DB) GetExperimentReport(id string, opts ExperimentReportOptions) (*ExperimentReport, error) {
	hasSince := opts.SinceProvided || !opts.Since.IsZero()
	hasUntil := opts.UntilProvided || !opts.Until.IsZero()
	if opts.MinSubjects < 0 || (hasSince && hasUntil && opts.Since.After(opts.Until)) {
		return nil, ErrInvalidExperimentReportOptions
	}
	if opts.MinSubjects == 0 {
		opts.MinSubjects = 4
	}
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	r := &ExperimentReport{ID: id, AsOf: time.Now().UTC(), MinSubjects: opts.MinSubjects, Integrity: ExperimentIntegrity{Complete: true}, Method: ExperimentReportMethod{Resamples: experimentResamples, Confidence: .95, Exploratory: true, OutcomeBasis: "latest retained attempts"}}
	if hasSince {
		r.Since = new(opts.Since.UTC())
	}
	if hasUntil {
		r.Until = new(opts.Until.UTC())
	}
	if err = tx.QueryRow(`SELECT definition_hash,definition_json FROM experiment_definitions WHERE experiment_id=?`, id).Scan(&r.DefinitionHash, &r.definitionJSON); err != nil {
		return nil, err
	}
	var definition struct {
		Ratio *float64 `json:"ratio"`
	}
	if err = json.Unmarshal([]byte(r.definitionJSON), &definition); err == nil && definition.Ratio != nil && *definition.Ratio >= 0 && *definition.Ratio <= 1 {
		r.Ratio = definition.Ratio
	} else {
		r.Integrity.Complete = false
		r.Integrity.Reason = "Stored experiment definition has no valid allocation ratio."
	}
	rows, err := tx.Query(`SELECT review_unit_kind,review_unit_uuid,arm,subject_hash,effective_config_json,assigned_at FROM experiment_assignments WHERE experiment_id=? ORDER BY subject_hash,review_unit_kind,review_unit_uuid`, id)
	if err != nil {
		return nil, err
	}
	var units []experimentReportUnit
	for rows.Next() {
		var u experimentReportUnit
		var assigned string
		if err = rows.Scan(&u.kind, &u.uuid, &u.arm, &u.subject, &u.plan, &assigned); err != nil {
			_ = rows.Close()
			return nil, err
		}
		at := parseSQLiteTime(assigned)
		if (hasSince && at.Before(opts.Since)) || (hasUntil && !at.Before(opts.Until)) {
			continue
		}
		units = append(units, u)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, err
	}
	subjects := map[string]string{}
	conflicts := map[string]bool{}
	for i := range units {
		u := &units[i]
		if u.arm != "default" && u.arm != "experiment" {
			r.Integrity.Complete = false
			r.Integrity.IncompleteUnits++
			continue
		}
		if previous, ok := subjects[u.subject]; ok && previous != u.arm {
			conflicts[u.subject] = true
		}
		subjects[u.subject] = u.arm
		if err = loadExperimentUnit(tx, u); err != nil {
			return nil, err
		}
		aggregateExperimentUnit(u)
		if !u.attributed || u.subject == "" {
			r.Integrity.IncompleteUnits++
			r.Integrity.Complete = false
		}
	}
	r.Integrity.ConflictingSubjects = len(conflicts)
	if len(conflicts) > 0 {
		r.Integrity.Complete = false
		r.Integrity.Reason = "A subject occurs in both experiment arms."
	} else if r.Integrity.IncompleteUnits > 0 {
		r.Integrity.Reason = "Frozen component attribution is incomplete or malformed."
	}
	r.Default = aggregateExperimentArm(units, "default")
	r.Experiment = aggregateExperimentArm(units, "experiment")
	r.Allocation = experimentAllocation(r.Default.Subjects, r.Experiment.Subjects, r.Ratio)
	var labels findingOutcomes = rebuttalFindingOutcomes{}
	r.Labels = labels.coverage()
	r.Metrics = buildExperimentComparisons(units, r)
	r.Success = evaluateExperimentSuccess(r)
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return r, nil
}

const experimentAgentInvoked = "j.agent_invoked=1 OR (" + agentRanByUsage + ")"

func loadExperimentUnit(q querier, u *experimentReportUnit) error {
	condition := "j.uuid=?"
	if u.kind == ReviewUnitPanel {
		condition = "j.panel_run_uuid=?"
	} else if u.kind != ReviewUnitJob {
		return nil
	}
	rows, err := q.Query(`
 SELECT j.id, COALESCE(j.uuid,''), COALESCE(j.panel_role,''), CASE WHEN `+experimentAgentInvoked+` THEN COALESCE(NULLIF(r.agent,''),j.agent) ELSE j.agent END,
        COALESCE(j.provider,''), COALESCE(j.session_id,''), j.status,
        COALESCE(j.panel_member_name,''), COALESCE(j.panel_member_index,0),
        COALESCE(j.non_voting,0),
        CASE WHEN `+experimentAgentInvoked+` THEN 1 ELSE 0 END,
        CASE WHEN `+costEligible+` THEN 1 ELSE 0 END,
        CASE WHEN `+costEligible+` AND `+hasCost+` THEN 1 ELSE 0 END,
        CASE WHEN `+costEligible+` AND `+hasCost+`
             THEN json_extract(j.token_usage,'$.cost_usd') ELSE 0 END,
        COALESCE(j.started_at,''), COALESCE(j.finished_at,''),
        COALESCE(r.structured_output,''), COALESCE(j.job_type,''),
        COALESCE(j.review_type,''), COALESCE(j.analysis_type,''),
        COALESCE(j.source,''), COALESCE(j.error,''), COALESCE(j.min_severity,''),
        COALESCE(j.token_usage,'')
 FROM review_jobs j LEFT JOIN reviews r ON r.job_id=j.id
 WHERE `+condition+` ORDER BY j.id`, u.uuid)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var j experimentReportJob
		var start, finish, usage string
		if err = rows.Scan(&j.ref.JobID, &j.ref.JobUUID, &j.ref.Role, &j.ref.Agent, &j.ref.Provider, &j.ref.SessionID, &j.status, &j.member, &j.memberIndex, &j.nonVoting, &j.invoked, &j.eligible, &j.priced, &j.dollars, &start, &finish, &j.structured, &j.jobType, &j.reviewType, &j.analysisType, &j.source, &j.jobError, &j.minSeverity, &usage); err != nil {
			return err
		}
		var identifiers struct {
			ProviderSessionIDs []string `json:"provider_session_ids"`
		}
		if json.Unmarshal([]byte(usage), &identifiers) == nil {
			j.ref.ProviderSessionIDs = identifiers.ProviderSessionIDs
			slices.Sort(j.ref.ProviderSessionIDs)
			j.ref.ProviderSessionIDs = slices.Compact(j.ref.ProviderSessionIDs)
		}
		j.started = parseSQLiteTime(start)
		j.finished = parseSQLiteTime(finish)
		j.finishedWholeSecond = finish != "" && !strings.ContainsAny(finish, ".,")
		if math.IsNaN(j.dollars) || math.IsInf(j.dollars, 0) {
			j.priced = false
			j.dollars = 0
		}
		u.jobs = append(u.jobs, j)
	}
	return rows.Err()
}

func experimentTerminal(status string) bool {
	return slices.Contains([]string{"done", "applied", "rebased", "failed", "canceled", "skipped"}, status)
}

func aggregateExperimentUnit(u *experimentReportUnit) {
	u.values = map[string]float64{}
	// Enrollment fixes these denominators even when the canonical job or its
	// readable output is missing. Missing observations must gate inference.
	u.expected = map[string]bool{
		"duration_p50_seconds":         true,
		"duration_p90_seconds":         true,
		"fail_verdict_rate":            true,
		"findings_critical_per_review": true,
		"findings_high_per_review":     true,
		"findings_medium_per_review":   true,
		"findings_low_per_review":      true,
	}
	u.canonical = -1
	u.attributed = true
	u.terminal = len(u.jobs) > 0
	plans := make(map[string]experimentReportJobPlan)
	switch u.kind {
	case ReviewUnitJob:
		var plan experimentReportJobPlan
		if json.Unmarshal([]byte(u.plan), &plan) != nil || plan.Agent == "" {
			u.attributed = false
		}
		plans["job"] = plan
	case ReviewUnitPanel:
		var plan experimentReportPanelPlan
		if json.Unmarshal([]byte(u.plan), &plan) != nil || len(plan.Members) == 0 || plan.Synthesis.Agent == "" {
			u.attributed = false
		}
		plans["synthesis"] = plan.Synthesis
		for _, m := range plan.Members {
			key := fmt.Sprintf("member:%s:%d", m.PanelMemberName, m.PanelMemberIndex)
			if m.Agent == "" || m.PanelMemberName == "" {
				u.attributed = false
			}
			if _, ok := plans[key]; ok {
				u.attributed = false
			}
			plans[key] = m
		}
	default:
		u.attributed = false
	}
	matched := map[string]bool{}
	var firstVoting time.Time
	failures := 0
	invoked, deviations := 0, 0
	dollars := 0.0
	for i, j := range u.jobs {
		key := "job"
		if u.kind == ReviewUnitPanel {
			key = j.ref.Role
			if key == "member" {
				key = fmt.Sprintf("member:%s:%d", j.member, j.memberIndex)
			}
		}
		plan, ok := plans[key]
		if !ok || matched[key] {
			u.attributed = false
		}
		matched[key] = true
		if u.kind == ReviewUnitJob || j.ref.Role == "synthesis" {
			if u.canonical != -1 {
				u.attributed = false
			}
			u.canonical = i
		}
		if !experimentTerminal(j.status) {
			u.terminal = false
		}
		if j.status == "failed" || j.status == "canceled" {
			failures++
		}
		if j.invoked {
			invoked++
			if ok && plan.Agent != "" && agentname.Canonical(j.ref.Agent) != agentname.Canonical(plan.Agent) {
				deviations++
			}
		}
		if j.eligible {
			u.cost.EligibleComponents++
		}
		if j.priced {
			u.cost.PricedComponents++
			dollars += j.dollars
		}
		if (u.kind == ReviewUnitJob || (j.ref.Role == "member" && !j.nonVoting)) && !j.started.IsZero() && (firstVoting.IsZero() || j.started.Before(firstVoting)) {
			firstVoting = j.started
		}
	}
	if len(matched) != len(plans) || u.canonical < 0 {
		u.attributed = false
		u.terminal = false
	}
	// Missing expected roles make work incomplete even when synthesis is finished.
	for key := range plans {
		if !matched[key] {
			u.terminal = false
			u.attributed = false
		}
	}
	u.cost.Complete = u.attributed && u.terminal && u.cost.EligibleComponents > 0 && u.cost.EligibleComponents == u.cost.PricedComponents
	if u.cost.PricedComponents > 0 {
		u.cost.RecordedUSD = new(dollars)
	}
	u.expected["cost_per_review"] = true
	if u.cost.Complete {
		u.values["cost_per_review"] = dollars
		u.cost.CompleteUnits = 1
	}
	u.expected["job_failure_rate"] = true
	if u.terminal {
		// Each complete panel contributes its component fraction; cancellations
		// count as execution failures, while skipped jobs remain nonfailures.
		u.values["job_failure_rate"] = float64(failures) / float64(len(u.jobs))
	}
	u.expected["off_plan_rate"] = invoked > 0 || !u.attributed || !u.terminal
	// Off-plan is also a component fraction within a complete review unit.
	if invoked > 0 && u.attributed && u.terminal {
		u.values["off_plan_rate"] = float64(deviations) / float64(invoked)
	}
	if u.canonical < 0 {
		return
	}
	canonical := u.jobs[u.canonical]
	// Older terminal writes truncated to whole seconds. A same-second finish
	// that predates a precise start is a zero-duration observation at that
	// resolution; genuinely earlier finishes remain unavailable.
	if canonical.finishedWholeSecond && canonical.finished.Equal(firstVoting.Truncate(time.Second)) {
		canonical.finished = firstVoting
	}
	// A canonical result may be available while advisory work is still running.
	for _, metric := range []string{"duration_p50_seconds", "duration_p90_seconds"} {
		if !firstVoting.IsZero() && !canonical.finished.IsZero() && !canonical.finished.Before(firstVoting) {
			u.values[metric] = canonical.finished.Sub(firstVoting).Seconds()
		}
	}
	job := ReviewJob{Status: JobStatus(canonical.status), JobType: canonical.jobType, ReviewType: canonical.reviewType, AnalysisType: canonical.analysisType, Source: canonical.source, Error: canonical.jobError}
	if !job.HasFindingCountsOutput() {
		return
	}
	document, err := structuredreview.Decode(jsontext.Value(canonical.structured))
	if err != nil {
		return
	}
	counts := documentFindingCounts(document)
	if counts == nil {
		return
	}
	u.values["findings_critical_per_review"] = float64(counts.Critical)
	u.values["findings_high_per_review"] = float64(counts.High)
	u.values["findings_medium_per_review"] = float64(counts.Medium)
	u.values["findings_low_per_review"] = float64(counts.Low)
	u.values["fail_verdict_rate"] = 0
	if !document.Passed(canonical.minSeverity) {
		u.values["fail_verdict_rate"] = 1
	}
}

func aggregateExperimentArm(units []experimentReportUnit, arm string) ExperimentArmReport {
	a := ExperimentArmReport{CostliestUnits: []ExperimentCostliestUnit{}}
	subjects := map[string]bool{}
	recorded := 0.0
	for _, u := range units {
		if u.arm != arm {
			continue
		}
		subjects[u.subject] = true
		a.ReviewUnits++
		if u.terminal {
			a.TerminalUnits++
		} else {
			a.PendingUnits++
		}
		if u.attributed {
			a.AttributionCompleteUnits++
		}
		failed, canceled := false, false
		for _, j := range u.jobs {
			failed = failed || j.status == "failed"
			canceled = canceled || j.status == "canceled"
		}
		if failed {
			a.FailedUnits++
		}
		if canceled {
			a.CanceledUnits++
		}
		if u.canonical < 0 {
			a.CanonicalStatuses.Missing++
		} else {
			switch u.jobs[u.canonical].status {
			case "done":
				a.CanonicalStatuses.Done++
			case "applied":
				a.CanonicalStatuses.Applied++
			case "rebased":
				a.CanonicalStatuses.Rebased++
			case "failed":
				a.CanonicalStatuses.Failed++
			case "canceled":
				a.CanonicalStatuses.Canceled++
			case "skipped":
				a.CanonicalStatuses.Skipped++
				a.SkippedUnits++
			case "queued":
				a.CanonicalStatuses.Queued++
			case "running":
				a.CanonicalStatuses.Running++
			}
		}
		if _, ok := u.values["findings_high_per_review"]; ok {
			a.OutputUnits++
		}
		a.Cost.EligibleComponents += u.cost.EligibleComponents
		a.Cost.PricedComponents += u.cost.PricedComponents
		a.Cost.CompleteUnits += u.cost.CompleteUnits
		if u.cost.RecordedUSD != nil {
			recorded += *u.cost.RecordedUSD
			candidate := ExperimentCostliestUnit{Kind: u.kind, UUID: u.uuid, RecordedUSD: *u.cost.RecordedUSD, Complete: u.cost.Complete, Components: []ExperimentComponentReference{}}
			for _, j := range u.jobs {
				candidate.Components = append(candidate.Components, j.ref)
			}
			if len(a.CostliestUnits) == 0 || candidate.RecordedUSD > a.CostliestUnits[0].RecordedUSD {
				a.CostliestUnits = []ExperimentCostliestUnit{candidate}
			} else if candidate.RecordedUSD == a.CostliestUnits[0].RecordedUSD {
				a.CostliestUnits = append(a.CostliestUnits, candidate)
			}
		}
	}
	a.Subjects = len(subjects)
	a.Cost.Complete = a.ReviewUnits > 0 && a.Cost.CompleteUnits == a.ReviewUnits
	if a.Cost.PricedComponents > 0 {
		a.Cost.RecordedUSD = new(recorded)
	}
	if !a.Cost.Complete {
		a.Cost.Reason = "Complete terminal components and full cost coverage are required; run backfill-tokens for missing recorded dollars."
	}
	return a
}
