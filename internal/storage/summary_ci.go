package storage

import (
	"cmp"
	"database/sql"
	"slices"
	"strings"
)

// CISummary describes panels created in the selected summary window.
// ActivePanels + PostedPanels + RetiredUnpostedPanels equals TotalPanels.
// PostedPanels means finalized (posted_at is set), including abandoned runs
// finalized without a review comment. RetiredByReason counts all retired rows,
// including finalized rows later retired to release rerun ownership.
type CISummary struct {
	TotalPanels           int `json:"total_panels"`
	ActivePanels          int `json:"active_panels"`
	PostedPanels          int `json:"posted_panels"`
	RetiredUnpostedPanels int `json:"retired_unposted_panels"`
	// ClosedBeforePost counts retired, unposted rows with reason closed_pr.
	// Its denominator is TotalPanels, including active and unknown-history rows.
	ClosedBeforePost int            `json:"closed_before_post"`
	Outcomes         map[string]int `json:"outcomes"`
	RetiredByReason  map[string]int `json:"retired_by_reason"`
	DistinctPRs      int            `json:"distinct_prs"`
	RunsPerPRP50     float64        `json:"runs_per_pr_p50"`
	RunsPerPRP90     float64        `json:"runs_per_pr_p90"`
	RunsPerPRMax     int            `json:"runs_per_pr_max"`
	HotPRs           []CIPRSummary  `json:"hot_prs"`
	PRs              []CIPRSummary  `json:"prs"`
	// Zero TurnaroundSamples makes the turnaround percentiles unavailable.
	TurnaroundSamples   int           `json:"turnaround_samples"`
	TurnaroundP50Secs   float64       `json:"turnaround_p50_secs"`
	TurnaroundP90Secs   float64       `json:"turnaround_p90_secs"`
	Cost                CostAggregate `json:"cost"`
	RetiredUnpostedCost CostAggregate `json:"retired_unposted_cost"`
}

// CIPRSummary counts runs and recorded cost for one repository-scoped PR.
// PRs and HotPRs are ordered by descending Runs, then GithubRepo and PRNumber.
// A Cost with zero JobsWithCost is unavailable, including zero eligible jobs.
type CIPRSummary struct {
	GithubRepo string        `json:"github_repo"`
	PRNumber   int           `json:"pr_number"`
	Runs       int           `json:"runs"`
	Cost       CostAggregate `json:"cost"`
}

type ciSummaryPanel struct {
	githubRepo            string
	prNumber              int
	posted, retired       bool
	outcome, retireReason string
	turnaround            sql.NullFloat64
	cost                  CostAggregate
}

// summaryCI aggregates jobs per panel before joining them to the panel cohort.
// No job timestamp predicate applies: panel creation time owns this scope.
func summaryCI(q querier, opts SummaryOptions) (*CISummary, error) {
	conditions := []string{"datetime(cp.created_at) >= datetime(?)"}
	args := []any{opts.Since.UTC().Format("2006-01-02 15:04:05")}
	if opts.RepoPath != "" {
		conditions = append(conditions, "r.root_path = ?")
		args = append(args, opts.RepoPath)
	}
	if opts.Branch != "" {
		conditions = append(conditions, "syn.branch = ?")
		args = append(args, opts.Branch)
	}
	rows, err := q.Query(`WITH selected AS (
  SELECT cp.* FROM ci_pr_panels cp
  LEFT JOIN review_jobs syn ON syn.id = cp.synthesis_job_id
  LEFT JOIN repos r ON r.id = syn.repo_id
  WHERE `+strings.Join(conditions, " AND ")+`
 ), costs AS (
  SELECT j.panel_run_uuid,
   SUM(CASE WHEN `+costEligible+` THEN 1 ELSE 0 END) AS eligible,
   SUM(CASE WHEN `+costEligible+` AND `+hasCost+` THEN 1 ELSE 0 END) AS priced,
   SUM(CASE WHEN `+costEligible+` AND `+hasCost+` THEN json_extract(j.token_usage, '$.cost_usd') ELSE 0 END) AS usd
  FROM review_jobs j
  WHERE j.panel_run_uuid IN (SELECT panel_run_uuid FROM selected)
  GROUP BY j.panel_run_uuid
 )
 SELECT cp.github_repo, cp.pr_number, cp.posted_at IS NOT NULL, cp.retired_at IS NOT NULL,
  COALESCE(cp.outcome,'unknown'), COALESCE(cp.retire_reason,'unknown'),
  (julianday(cp.posted_at) - julianday(cp.first_attempt_at)) * 86400,
  COALESCE(c.eligible,0), COALESCE(c.priced,0), COALESCE(c.usd,0)
 FROM selected cp LEFT JOIN costs c ON c.panel_run_uuid = cp.panel_run_uuid`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var panels []ciSummaryPanel
	for rows.Next() {
		var p ciSummaryPanel
		if err := rows.Scan(&p.githubRepo, &p.prNumber, &p.posted, &p.retired, &p.outcome, &p.retireReason, &p.turnaround, &p.cost.JobsTotal, &p.cost.JobsWithCost, &p.cost.TotalUSD); err != nil {
			return nil, err
		}
		panels = append(panels, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return aggregateCIPanels(panels), nil
}

func aggregateCIPanels(panels []ciSummaryPanel) *CISummary {
	if len(panels) == 0 {
		return nil
	}
	s := &CISummary{TotalPanels: len(panels), Outcomes: map[string]int{}, RetiredByReason: map[string]int{}, PRs: []CIPRSummary{}, HotPRs: []CIPRSummary{}}
	type prKey struct {
		repo   string
		number int
	}
	prs := map[prKey]*CIPRSummary{}
	var turnaround []float64
	// Sort before floating point addition for reproducible aggregation regardless
	// of query order. The caller owns this temporary row slice.
	slices.SortFunc(panels, func(a, b ciSummaryPanel) int {
		if n := cmp.Compare(a.githubRepo, b.githubRepo); n != 0 {
			return n
		}
		if n := cmp.Compare(a.prNumber, b.prNumber); n != 0 {
			return n
		}
		return cmp.Compare(a.cost.TotalUSD, b.cost.TotalUSD)
	})
	for _, p := range panels {
		switch {
		case p.posted:
			s.PostedPanels++
			outcome := p.outcome
			if outcome == "" {
				outcome = "unknown"
			}
			s.Outcomes[outcome]++
		case p.retired:
			s.RetiredUnpostedPanels++
		default:
			s.ActivePanels++
		}
		if p.retired {
			reason := p.retireReason
			if reason == "" {
				reason = "unknown"
			}
			s.RetiredByReason[reason]++
			if !p.posted {
				addCICost(&s.RetiredUnpostedCost, p.cost)
				if reason == PanelRetireReasonClosedPR {
					s.ClosedBeforePost++
				}
			}
		}
		if p.posted && p.turnaround.Valid && p.turnaround.Float64 >= 0 {
			turnaround = append(turnaround, p.turnaround.Float64)
		}
		key := prKey{p.githubRepo, p.prNumber}
		if prs[key] == nil {
			prs[key] = &CIPRSummary{GithubRepo: p.githubRepo, PRNumber: p.prNumber}
		}
		prs[key].Runs++
		addCICost(&prs[key].Cost, p.cost)
		addCICost(&s.Cost, p.cost)
	}
	var runs []float64
	for _, pr := range prs {
		s.PRs = append(s.PRs, *pr)
		runs = append(runs, float64(pr.Runs))
		s.RunsPerPRMax = max(s.RunsPerPRMax, pr.Runs)
	}
	slices.SortFunc(s.PRs, func(a, b CIPRSummary) int {
		if n := cmp.Compare(b.Runs, a.Runs); n != 0 {
			return n
		}
		if n := cmp.Compare(a.GithubRepo, b.GithubRepo); n != 0 {
			return n
		}
		return cmp.Compare(a.PRNumber, b.PRNumber)
	})
	for _, pr := range s.PRs {
		// Lever's precision-review README uses >5 runs per PR:
		// https://github.com/github/lever/blob/f859d0aa2be96981157ee0480655c84877e3cd98/automations/precision-review/README.md
		// This is a reporting threshold, not a data limit.
		if pr.Runs > 5 {
			s.HotPRs = append(s.HotPRs, pr)
		}
	}
	s.DistinctPRs = len(prs)
	s.RunsPerPRP50 = percentile(runs, 0.5)
	s.RunsPerPRP90 = percentile(runs, 0.9)
	s.TurnaroundSamples = len(turnaround)
	s.TurnaroundP50Secs = percentile(turnaround, 0.5)
	s.TurnaroundP90Secs = percentile(turnaround, 0.9)
	return s
}

func addCICost(total *CostAggregate, cost CostAggregate) {
	total.TotalUSD += cost.TotalUSD
	total.JobsTotal += cost.JobsTotal
	total.JobsWithCost += cost.JobsWithCost
	total.Complete = total.JobsTotal > 0 && total.JobsWithCost == total.JobsTotal
}
