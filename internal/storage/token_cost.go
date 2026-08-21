package storage

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// TokenCostCandidate is the minimal persisted state needed to recover a late
// cost from the configured usage provider.
type TokenCostCandidate struct {
	JobID        int64
	SessionID    string
	Agent        string
	TokenUsage   string
	StartedAtRaw string
}

// TokenUsageLogCandidate is the persisted state needed to recover a missing
// session from a terminal job's JSONL log.
type TokenUsageLogCandidate struct {
	JobID        int64
	TokenUsage   string
	StartedAt    time.Time
	StartedAtRaw string
}

const tokenCostCandidatePredicate = costEligible + `
	AND NOT (` + hasCost + `)
	AND ` + reconciliationAttemptReleased + `
	AND COALESCE(j.session_resumed, 0) = 0
	AND COALESCE(j.session_id, '') != ''`

const tokenUsageLogCandidatePredicate = costTerminal + `
	AND NOT (` + hasCost + `)
	AND ` + reconciliationAttemptReleased + `
	AND COALESCE(j.session_id, '') = ''`

const reconciliationAttemptReleased = `(
	j.status != 'canceled' OR COALESCE(j.worker_id, '') = ''
)`

const uniqueStartedSessionPredicate = `NOT EXISTS (
		SELECT 1
		FROM review_jobs other
		WHERE other.id != j.id
		  AND other.source_machine_id = j.source_machine_id
		  AND other.started_at IS NOT NULL
		  AND other.session_id IS NOT NULL
		  AND other.session_id != ''
		  AND other.session_id = j.session_id
	) AND NOT EXISTS (
		SELECT 1
		FROM review_job_session_history history
		WHERE history.source_machine_id = j.source_machine_id
		  AND history.session_id = j.session_id
		  AND (history.job_uuid != j.uuid OR history.started_at != j.started_at)
	)`

// ListTokenCostCandidates returns a bounded page ordered by job ID. afterJobID
// is exclusive, so callers can retain a cursor while successfully priced rows
// disappear from the candidate set. A non-zero startedAfter excludes attempts
// that started earlier: the daemon's periodic reconciliation passes one so
// permanently unpriceable history (deleted sessions) does not stay in the scan
// forever, while the manual backfill passes zero to reach every job.
func (db *DB) ListTokenCostCandidates(
	afterJobID int64, limit int, startedAfter time.Time,
) ([]TokenCostCandidate, error) {
	if limit <= 0 {
		return nil, nil
	}
	machineID, err := db.GetMachineID()
	if err != nil {
		return nil, fmt.Errorf("get machine ID: %w", err)
	}
	startedCutoff := ""
	if !startedAfter.IsZero() {
		startedCutoff = formatSQLiteTimestamp(startedAfter)
	}
	rows, err := db.Query(`
		SELECT j.id, j.session_id, j.agent, COALESCE(j.token_usage, ''),
		       j.started_at
		FROM review_jobs j
		WHERE j.id > ?
		  AND j.source_machine_id = ?
		  AND (? = '' OR `+sqliteNormalizedTimestampExpr("j.started_at")+` >= ?)
		  AND `+tokenCostCandidatePredicate+`
		  AND `+uniqueStartedSessionPredicate+`
		ORDER BY j.id
		LIMIT ?`, afterJobID, machineID, startedCutoff, startedCutoff, limit)
	if err != nil {
		return nil, fmt.Errorf("list token cost candidates: %w", err)
	}
	defer rows.Close()

	var candidates []TokenCostCandidate
	for rows.Next() {
		var candidate TokenCostCandidate
		if err := rows.Scan(
			&candidate.JobID,
			&candidate.SessionID,
			&candidate.Agent,
			&candidate.TokenUsage,
			&candidate.StartedAtRaw,
		); err != nil {
			return nil, fmt.Errorf("scan token cost candidate: %w", err)
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate token cost candidates: %w", err)
	}
	return candidates, nil
}

// GetTokenCostCandidate returns jobID only while it remains eligible for a
// late-price lookup. A nil result means the job is no longer a candidate.
func (db *DB) GetTokenCostCandidate(jobID int64) (*TokenCostCandidate, error) {
	machineID, err := db.GetMachineID()
	if err != nil {
		return nil, fmt.Errorf("get machine ID: %w", err)
	}
	var candidate TokenCostCandidate
	err = db.QueryRow(`
		SELECT j.id, j.session_id, j.agent, COALESCE(j.token_usage, ''),
		       j.started_at
		FROM review_jobs j
		WHERE j.id = ?
		  AND j.source_machine_id = ?
		  AND `+tokenCostCandidatePredicate+`
		  AND `+uniqueStartedSessionPredicate,
		jobID, machineID,
	).Scan(
		&candidate.JobID,
		&candidate.SessionID,
		&candidate.Agent,
		&candidate.TokenUsage,
		&candidate.StartedAtRaw,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get token cost candidate: %w", err)
	}
	return &candidate, nil
}

// ListTokenUsageLogCandidates returns terminal missing-cost jobs whose session
// ID was not persisted. Their per-job JSONL log may still recover the session
// and make them eligible for normal provider reconciliation. A non-zero
// startedAfter excludes attempts that started earlier, bounding the daemon's
// periodic scan the same way ListTokenCostCandidates is bounded; pass zero to
// reach every job.
func (db *DB) ListTokenUsageLogCandidates(
	afterJobID int64, limit int, startedAfter time.Time,
) ([]TokenUsageLogCandidate, error) {
	if limit <= 0 {
		return nil, nil
	}
	machineID, err := db.GetMachineID()
	if err != nil {
		return nil, fmt.Errorf("get machine ID: %w", err)
	}
	startedCutoff := ""
	if !startedAfter.IsZero() {
		startedCutoff = formatSQLiteTimestamp(startedAfter)
	}
	rows, err := db.Query(`
		SELECT j.id, COALESCE(j.token_usage, ''), j.started_at
		FROM review_jobs j
		WHERE j.id > ?
		  AND j.source_machine_id = ?
		  AND (? = '' OR `+sqliteNormalizedTimestampExpr("j.started_at")+` >= ?)
		  AND `+tokenUsageLogCandidatePredicate+`
		ORDER BY j.id
		LIMIT ?`, afterJobID, machineID, startedCutoff, startedCutoff, limit)
	if err != nil {
		return nil, fmt.Errorf("list token usage log candidates: %w", err)
	}
	defer rows.Close()

	var candidates []TokenUsageLogCandidate
	for rows.Next() {
		var candidate TokenUsageLogCandidate
		var startedAt string
		if err := rows.Scan(
			&candidate.JobID,
			&candidate.TokenUsage,
			&startedAt,
		); err != nil {
			return nil, fmt.Errorf("scan token usage log candidate: %w", err)
		}
		candidate.StartedAt = parseSQLiteTime(startedAt)
		candidate.StartedAtRaw = startedAt
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate token usage log candidates: %w", err)
	}
	return candidates, nil
}
