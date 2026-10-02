package storage

import (
	"math"
	"time"

	"go.kenn.io/roborev/internal/tokens"
)

// GetBudgetSpend measures retained terminal-row costs completed on the UTC day.
// It intentionally does not change enqueue-time cost reporting or retain costs
// that have been replaced by an explicit rerun, retry, or failover.
func (db *DB) GetBudgetSpend(day time.Time) (CostAggregate, error) {
	day = day.UTC()
	start := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, time.UTC)
	rows, err := db.Query(`SELECT COALESCE(j.token_usage, '')
  FROM review_jobs j
  WHERE `+costEligible+`
   AND datetime(j.finished_at) >= datetime(?) AND datetime(j.finished_at) < datetime(?)`,
		start.Format(time.RFC3339), start.AddDate(0, 0, 1).Format(time.RFC3339))
	if err != nil {
		return CostAggregate{}, err
	}
	defer rows.Close()
	var result CostAggregate
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return CostAggregate{}, err
		}
		usage := tokens.ParseJSON(raw)
		result.JobsTotal++
		if usage == nil || !usage.HasCost || usage.CostUSD < 0 || math.IsNaN(usage.CostUSD) || math.IsInf(usage.CostUSD, 0) {
			continue
		}
		result.JobsWithCost++
		result.TotalUSD += usage.CostUSD
	}
	if err := rows.Err(); err != nil {
		return CostAggregate{}, err
	}
	result.Complete = result.JobsTotal > 0 && result.JobsTotal == result.JobsWithCost
	return result, nil
}

// SetJobBudgetAgent records a substitution only while the worker owns the job.
// The lock survives automatic retries and failover; explicit reruns reset it.
func (db *DB) SetJobBudgetAgent(jobID int64, workerID, name, model, fallbackAgent, fallbackModel string) (bool, error) {
	result, err := db.Exec(`UPDATE review_jobs
  SET agent=?,model=?,provider=NULL,session_id=NULL,budget_routing_locked=1,
      budget_original_agent=agent,
      budget_original_backup_agent=COALESCE(backup_agent, ''),
      budget_original_backup_model=COALESCE(backup_model, ''),
      backup_agent=CASE WHEN ?='' THEN backup_agent ELSE ? END,
      backup_model=CASE WHEN ?='' THEN backup_model ELSE ? END,
      updated_at=?,synced_at=NULL
  WHERE id=? AND status='running' AND worker_id=? AND budget_routing_locked=0`,
		name, nullString(model), fallbackAgent, fallbackAgent, fallbackAgent, fallbackModel,
		time.Now().UTC().Format(time.RFC3339), jobID, workerID)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n > 0, err
}
