package storage

import (
	"database/sql"
	"errors"
	"fmt"
)

// TokenCostCandidate is the minimal persisted state needed to recover a late
// cost from the configured usage provider.
type TokenCostCandidate struct {
	JobID      int64
	SessionID  string
	Agent      string
	TokenUsage string
}

const tokenCostCandidatePredicate = costEligible + `
	AND NOT (` + hasCost + `)
	AND COALESCE(j.session_id, '') != ''`

const uniqueStartedSessionPredicate = `NOT EXISTS (
		SELECT 1
		FROM review_jobs other
		WHERE other.id != j.id
		  AND other.started_at IS NOT NULL
		  AND other.session_id = j.session_id
	)`

// ListTokenCostCandidates returns a bounded page ordered by job ID. afterJobID
// is exclusive, so callers can retain a cursor while successfully priced rows
// disappear from the candidate set.
func (db *DB) ListTokenCostCandidates(
	afterJobID int64, limit int,
) ([]TokenCostCandidate, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := db.Query(`
		WITH unique_started_sessions AS (
			SELECT session_id
			FROM review_jobs
			WHERE started_at IS NOT NULL
			  AND COALESCE(session_id, '') != ''
			GROUP BY session_id
			HAVING COUNT(*) = 1
		)
		SELECT j.id, j.session_id, j.agent, COALESCE(j.token_usage, '')
		FROM review_jobs j
		JOIN unique_started_sessions unique_session
		  ON unique_session.session_id = j.session_id
		WHERE j.id > ?
		  AND `+tokenCostCandidatePredicate+`
		ORDER BY j.id
		LIMIT ?`, afterJobID, limit)
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
	var candidate TokenCostCandidate
	err := db.QueryRow(`
		SELECT j.id, j.session_id, j.agent, COALESCE(j.token_usage, '')
		FROM review_jobs j
		WHERE j.id = ?
		  AND `+tokenCostCandidatePredicate+`
		  AND `+uniqueStartedSessionPredicate,
		jobID,
	).Scan(
		&candidate.JobID,
		&candidate.SessionID,
		&candidate.Agent,
		&candidate.TokenUsage,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get token cost candidate: %w", err)
	}
	return &candidate, nil
}
