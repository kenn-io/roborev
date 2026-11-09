package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
	"uuid"
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

// DueTokenCostCandidate is a token cost candidate whose periodic lookup is
// due, with the lookup state recorded for its current attempt.
type DueTokenCostCandidate struct {
	TokenCostCandidate
	FinishedAt time.Time
	// LookupMisses counts earlier periodic lookups of this attempt that did
	// not price it.
	LookupMisses int
	// LastNoUsageSessionID names the provider session that the previous
	// lookup found with no token or cost data. It is empty when the previous
	// lookup ended any other way.
	LastNoUsageSessionID string
}

// TokenCostLookup is the periodic reconciler's state for one job attempt.
// StartedAt and SessionID identify the attempt; a rerun starts a new one.
type TokenCostLookup struct {
	JobID     int64
	StartedAt string
	SessionID string
	Misses    int
	// NoUsageSessionID names the provider session this lookup found with no
	// token or cost data, or is empty when the lookup ended any other way.
	NoUsageSessionID string
	// NextCheckAt is when the next lookup is due. Zero stops periodic
	// lookups for this attempt.
	NextCheckAt time.Time
}

const tokenCostCandidateColumns = `j.id, j.session_id, j.agent, COALESCE(j.token_usage, ''), j.started_at`

// tokenCostLookupJoin matches a lookup row only to the attempt it recorded.
const tokenCostLookupJoin = `
		LEFT JOIN token_cost_lookups l
		  ON l.job_id = j.id
		 AND l.started_at = j.started_at
		 AND l.session_id = j.session_id`

func migrateTokenCostLookups(ctx context.Context, db *DB) error {
	_, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS token_cost_lookups (
  job_id INTEGER PRIMARY KEY REFERENCES review_jobs(id) ON DELETE CASCADE,
  started_at TEXT NOT NULL,
  session_id TEXT NOT NULL,
  misses INTEGER NOT NULL,
  no_usage_session_id TEXT NOT NULL,
  next_check_at TEXT
 )`)
	return err
}

func listTokenCostCandidatesQuery(afterJobID int64, limit int, startedAfter time.Time, machineID uuid.UUID) (string, []any) {
	return listTokenCandidatesQuery(
		tokenCostCandidateColumns, "",
		tokenCostCandidatePredicate+` AND `+uniqueStartedSessionPredicate, nil,
		afterJobID, limit, startedAfter, machineID,
	)
}

func listTokenCandidatesQuery(projection, join, predicate string, predicateArgs []any, afterJobID int64, limit int, startedAfter time.Time, machineID uuid.UUID) (string, []any) {
	startedPredicate := ""
	args := []any{afterJobID, machineID}
	if !startedAfter.IsZero() {
		startedPredicate = " AND " + sqliteNormalizedTimestampExpr("j.started_at") + " >= ?"
		args = append(args, formatSQLiteTimestamp(startedAfter))
	}
	args = append(args, predicateArgs...)
	return `
		SELECT ` + projection + `
		FROM review_jobs j` + join + `
		WHERE j.id > ?
		  AND j.source_machine_id = ?` + startedPredicate + `
		  AND ` + predicate + `
		ORDER BY j.id
		LIMIT ?`, append(args, limit)
}

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
	query, args := listTokenCostCandidatesQuery(afterJobID, limit, startedAfter, machineID)
	rows, err := db.Query(query, args...)
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

func listDueTokenCostCandidatesQuery(afterJobID int64, limit int, startedAfter, now time.Time, machineID uuid.UUID) (string, []any) {
	return listTokenCandidatesQuery(
		tokenCostCandidateColumns+`, j.finished_at,
		COALESCE(l.misses, 0), COALESCE(l.no_usage_session_id, '')`,
		tokenCostLookupJoin,
		tokenCostCandidatePredicate+` AND `+uniqueStartedSessionPredicate+`
		  AND (l.job_id IS NULL OR l.next_check_at <= ?)`,
		[]any{formatSQLiteTimestamp(now)},
		afterJobID, limit, startedAfter, machineID,
	)
}

// ListDueTokenCostCandidates is ListTokenCostCandidates for the daemon's
// periodic reconciler. It leaves out attempts whose next lookup comes after
// now, and attempts whose lookups have stopped.
func (db *DB) ListDueTokenCostCandidates(
	ctx context.Context, afterJobID int64, limit int, startedAfter, now time.Time,
) ([]DueTokenCostCandidate, error) {
	if limit <= 0 {
		return nil, nil
	}
	machineID, err := db.GetMachineID()
	if err != nil {
		return nil, fmt.Errorf("get machine ID: %w", err)
	}
	query, args := listDueTokenCostCandidatesQuery(afterJobID, limit, startedAfter, now, machineID)
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list due token cost candidates: %w", err)
	}
	defer rows.Close()

	var candidates []DueTokenCostCandidate
	for rows.Next() {
		var candidate DueTokenCostCandidate
		var finishedAt string
		if err := rows.Scan(
			&candidate.JobID,
			&candidate.SessionID,
			&candidate.Agent,
			&candidate.TokenUsage,
			&candidate.StartedAtRaw,
			&finishedAt,
			&candidate.LookupMisses,
			&candidate.LastNoUsageSessionID,
		); err != nil {
			return nil, fmt.Errorf("scan due token cost candidate: %w", err)
		}
		candidate.FinishedAt = parseSQLiteTime(finishedAt)
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate due token cost candidates: %w", err)
	}
	return candidates, nil
}

// SaveTokenCostLookup records the periodic reconciler's state for a job
// attempt, replacing any earlier state for the job.
func (db *DB) SaveTokenCostLookup(ctx context.Context, lookup TokenCostLookup) error {
	var nextCheckAt any
	if !lookup.NextCheckAt.IsZero() {
		nextCheckAt = formatSQLiteTimestamp(lookup.NextCheckAt)
	}
	_, err := db.ExecContext(ctx, `
		INSERT INTO token_cost_lookups
		  (job_id, started_at, session_id, misses, no_usage_session_id, next_check_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(job_id) DO UPDATE SET
		  started_at = excluded.started_at,
		  session_id = excluded.session_id,
		  misses = excluded.misses,
		  no_usage_session_id = excluded.no_usage_session_id,
		  next_check_at = excluded.next_check_at`,
		lookup.JobID, lookup.StartedAt, lookup.SessionID,
		lookup.Misses, lookup.NoUsageSessionID, nextCheckAt,
	)
	if err != nil {
		return fmt.Errorf("save token cost lookup: %w", err)
	}
	return nil
}

// PruneTokenCostLookups deletes lookup state that no longer gates a lookup:
// the job was priced or started a new attempt, or its attempt started
// before startedAfter and has left the periodic scan.
func (db *DB) PruneTokenCostLookups(ctx context.Context, startedAfter time.Time) error {
	_, err := db.ExecContext(ctx, `
		DELETE FROM token_cost_lookups
		WHERE NOT EXISTS (
			SELECT 1
			FROM review_jobs j
			WHERE j.id = token_cost_lookups.job_id
			  AND j.started_at = token_cost_lookups.started_at
			  AND j.session_id = token_cost_lookups.session_id
			  AND NOT (`+hasCost+`)
			  AND `+sqliteNormalizedTimestampExpr("j.started_at")+` >= ?
		)`, formatSQLiteTimestamp(startedAfter))
	if err != nil {
		return fmt.Errorf("prune token cost lookups: %w", err)
	}
	return nil
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

func listTokenUsageLogCandidatesQuery(afterJobID int64, limit int, startedAfter time.Time, machineID uuid.UUID) (string, []any) {
	return listTokenCandidatesQuery(
		`j.id, COALESCE(j.token_usage, ''), j.started_at`, "",
		tokenUsageLogCandidatePredicate, nil,
		afterJobID, limit, startedAfter, machineID,
	)
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
	query, args := listTokenUsageLogCandidatesQuery(afterJobID, limit, startedAfter, machineID)
	rows, err := db.Query(query, args...)
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
