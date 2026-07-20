package storage

import (
	"context"
	"time"

	"github.com/uptrace/bun"
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
	return costAggregate(db.bun, db, opts)
}

// costAggregate computes approximate agent spend against any Bun connection, so callers
// can share a read snapshot (e.g. GetSummary's transaction). Panel member rows
// are included — spend is per-row, not row-count.
func costAggregate(db *bun.DB, conn bun.IConn, opts CostOptions) (CostAggregate, error) {
	query := db.NewSelect().
		Conn(conn).
		TableExpr("review_jobs AS j").
		ColumnExpr("COALESCE(SUM(CASE WHEN " + costEligible + " THEN 1 ELSE 0 END), 0) AS jobs_total").
		ColumnExpr("COALESCE(SUM(CASE WHEN " + costEligible + " AND " + hasCost + " THEN 1 ELSE 0 END), 0) AS jobs_with_cost").
		ColumnExpr("CAST(COALESCE(SUM(CASE WHEN " + costEligible + " AND " + hasCost + " THEN json_extract(j.token_usage, '$.cost_usd') ELSE 0 END), 0) AS REAL) AS total_usd").
		Join("JOIN repos AS r ON r.id = j.repo_id")
	if len(opts.RepoPaths) > 0 {
		query = query.Where("r.root_path IN (?)", bun.List(opts.RepoPaths))
	}
	if opts.BranchEmpty {
		query = query.Where("j.branch = '' OR j.branch IS NULL")
	} else if opts.Branch != "" {
		query = query.Where("j.branch = ?", opts.Branch)
	}
	if !opts.Since.IsZero() {
		query = query.Where(
			"datetime(j.enqueued_at) >= datetime(?)",
			opts.Since.UTC().Format("2006-01-02 15:04:05"),
		)
	}

	var c CostAggregate
	if err := query.Scan(context.Background(), &c); err != nil {
		return CostAggregate{}, err
	}
	c.Complete = c.JobsTotal > 0 && c.JobsWithCost == c.JobsTotal
	return c, nil
}
