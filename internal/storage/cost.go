package storage

import (
	"strings"
	"time"
)

// CostAggregate is the approximate agent spend for a scope. It is partial by
// nature: JobsWithCost <= JobsTotal because only some agents report cost.
type CostAggregate struct {
	TotalUSD     float64 `json:"total_usd"`
	JobsWithCost int     `json:"jobs_with_cost"`
	JobsTotal    int     `json:"jobs_total"`
	Complete     bool    `json:"complete"` // JobsTotal > 0 && JobsWithCost == JobsTotal
}

// CostOptions scopes a cost aggregate. The zero value selects all repos, all
// branches, and all time.
type CostOptions struct {
	RepoPaths   []string  // empty = all repos; multiple = OR over repos.root_path
	Branch      string    // exact branch; ignored when BranchEmpty is true
	BranchEmpty bool      // true = only jobs with empty/NULL branch
	Since       time.Time // zero = all time; else enqueued_at >= Since
}

// hasCost is the SQL predicate for a row carrying a recorded dollar cost. It
// gates the priced numerator (jobs_with_cost) and total_usd. Requires
// review_jobs aliased as "j".
const hasCost = "json_valid(j.token_usage) AND json_extract(j.token_usage, '$.has_cost')"

// agentRanByUsage is the fallback agent-ran signal: a token_usage blob that
// records real consumption — a cost flag, output tokens, peak context, or a
// dollar figure. Only an agent run can produce any of these, so it backs
// eligibility for rows whose agent_invoked marker is absent: rows from before
// the column existed, rows backfilled from token_usage, or remote rows synced
// before the marker was added. Requires review_jobs aliased as "j".
const agentRanByUsage = "json_valid(j.token_usage) AND (" +
	"json_extract(j.token_usage, '$.has_cost') " +
	"OR json_extract(j.token_usage, '$.total_output_tokens') > 0 " +
	"OR json_extract(j.token_usage, '$.peak_context_tokens') > 0 " +
	"OR json_extract(j.token_usage, '$.cost_usd') > 0)"

// costEligible is the shared eligibility predicate: a terminal job where an
// agent actually ran. It gates total_usd, jobs_with_cost, and jobs_total so
// coverage cannot exceed 100%. Requires review_jobs aliased as "j".
//
// "An agent ran" is the agent_invoked marker — the worker sets it immediately
// before the agent call, after all pre-agent gates, and it syncs across
// machines — or a token_usage blob proving consumption (agentRanByUsage), which
// covers rows predating the marker. Terminal rows that never run an agent (panel
// synthesis passthrough/all-failed/all-passed, or a job that failed a pre-agent
// gate) carry neither signal and stay out of the denominator, so coverage is not
// dragged below 100% by rows that could never report cost.
const costEligible = "j.started_at IS NOT NULL AND j.finished_at IS NOT NULL " +
	"AND j.status != 'skipped' AND (j.agent_invoked = 1 OR (" + agentRanByUsage + "))"

// GetCostAggregate computes approximate agent spend for the given scope on a
// fresh read.
func (db *DB) GetCostAggregate(opts CostOptions) (CostAggregate, error) {
	return costAggregate(db, opts)
}

// costAggregate computes approximate agent spend against any querier, so callers
// can share a read snapshot (e.g. GetSummary's transaction). Panel member rows
// are included — spend is per-row, not row-count.
func costAggregate(q querier, opts CostOptions) (CostAggregate, error) {
	var conditions []string
	var args []any

	if len(opts.RepoPaths) > 0 {
		placeholders := make([]string, len(opts.RepoPaths))
		for i, p := range opts.RepoPaths {
			placeholders[i] = "?"
			args = append(args, p)
		}
		conditions = append(conditions, "r.root_path IN ("+strings.Join(placeholders, ",")+")")
	}
	if opts.BranchEmpty {
		conditions = append(conditions, "(j.branch = '' OR j.branch IS NULL)")
	} else if opts.Branch != "" {
		conditions = append(conditions, "j.branch = ?")
		args = append(args, opts.Branch)
	}
	if !opts.Since.IsZero() {
		conditions = append(conditions, "datetime(j.enqueued_at) >= datetime(?)")
		args = append(args, opts.Since.UTC().Format("2006-01-02 15:04:05"))
	}

	where := ""
	if len(conditions) > 0 {
		where = "WHERE " + strings.Join(conditions, " AND ")
	}

	query := `
		SELECT
			COALESCE(SUM(CASE WHEN ` + costEligible + ` THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN ` + costEligible + `
				AND ` + hasCost + ` THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN ` + costEligible + `
				AND ` + hasCost + `
				THEN json_extract(j.token_usage, '$.cost_usd') ELSE 0 END), 0)
		FROM review_jobs j
		JOIN repos r ON r.id = j.repo_id
		` + where

	var c CostAggregate
	if err := q.QueryRow(query, args...).Scan(&c.JobsTotal, &c.JobsWithCost, &c.TotalUSD); err != nil {
		return CostAggregate{}, err
	}
	c.Complete = c.JobsTotal > 0 && c.JobsWithCost == c.JobsTotal
	return c, nil
}
