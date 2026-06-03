package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ReviewAttempt is the local CI-poller retry state for a single reviewed HEAD,
// keyed by (GithubRepo, PRNumber, HeadSHA). It is the durable source of truth
// for whether a HEAD is being reviewed and, when an AI-provider outage defers
// it, when to retry. State is one of "pending", "deferred", or "done";
// NextAttemptAt is nil while a run is in-flight or pending and set once
// deferred. Timestamps are stored as SQLite TEXT and hydrated with
// parseSQLiteTime, mirroring the ci_pr_panels scanners.
type ReviewAttempt struct {
	ID                         int64      `json:"id"`
	GithubRepo                 string     `json:"github_repo"`
	PRNumber                   int        `json:"pr_number"`
	HeadSHA                    string     `json:"head_sha"`
	Attempt                    int        `json:"attempt"`
	FirstAttemptAt             time.Time  `json:"first_attempt_at"`
	NextAttemptAt              *time.Time `json:"next_attempt_at,omitempty"`
	LastErrorClass             string     `json:"last_error_class"`
	ConsecutiveGenuineAttempts int        `json:"consecutive_genuine_attempts"`
	LastErrorExcerpt           string     `json:"last_error_excerpt"`
	LastPanelRunUUID           string     `json:"last_panel_run_uuid"`
	State                      string     `json:"state"`
	UpdatedAt                  time.Time  `json:"updated_at"`
}

// reviewAttemptColumns is the canonical SELECT column list for
// scanReviewAttempt. Kept in one place so every Get* query and the scanner
// stay in lockstep.
const reviewAttemptColumns = `id, github_repo, pr_number, head_sha, attempt,
	first_attempt_at, next_attempt_at, last_error_class,
	consecutive_genuine_attempts, last_error_excerpt, last_panel_run_uuid,
	state, updated_at`

// scanReviewAttempt hydrates a ReviewAttempt from a row selecting
// reviewAttemptColumns. The nullable next_attempt_at is scanned through
// sql.NullString and the timestamps parsed with parseSQLiteTime, mirroring
// scanCIPanel.
func scanReviewAttempt(row sqlScanner) (*ReviewAttempt, error) {
	var a ReviewAttempt
	var firstAttemptAt sql.NullString
	var nextAttemptAt sql.NullString
	var updatedAt sql.NullString
	if err := row.Scan(
		&a.ID, &a.GithubRepo, &a.PRNumber, &a.HeadSHA, &a.Attempt,
		&firstAttemptAt, &nextAttemptAt, &a.LastErrorClass,
		&a.ConsecutiveGenuineAttempts, &a.LastErrorExcerpt, &a.LastPanelRunUUID,
		&a.State, &updatedAt,
	); err != nil {
		return nil, err
	}
	if firstAttemptAt.Valid {
		a.FirstAttemptAt = parseSQLiteTime(firstAttemptAt.String)
	}
	if nextAttemptAt.Valid {
		t := parseSQLiteTime(nextAttemptAt.String)
		a.NextAttemptAt = &t
	}
	if updatedAt.Valid {
		a.UpdatedAt = parseSQLiteTime(updatedAt.String)
	}
	return &a, nil
}

// ReserveReviewAttempt idempotently reserves the attempt row for a reviewed
// HEAD. The INSERT ... ON CONFLICT DO NOTHING means the first caller for a
// (repo, pr, sha) creates the row (created=true) while a duplicate enqueue is
// a no-op (created=false); this is what makes reserve-on-enqueue safe against
// double-enqueue. The initial row is attempt=1, state='pending',
// next_attempt_at=NULL, with empty error fields and a zero genuine streak.
func (db *DB) ReserveReviewAttempt(repo string, pr int, sha string, now time.Time) (bool, error) {
	res, err := reserveReviewAttemptExec(context.Background(), db, repo, pr, sha, now)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("reserve review attempt rows: %w", err)
	}
	return n == 1, nil
}

// reserveReviewAttemptTx reserves the attempt row inside a caller-owned
// transaction (used by CreateCIPanelRun so the panel run and its retry-attempt
// row are created atomically). The INSERT is idempotent
// (ON CONFLICT DO NOTHING), so re-enqueuing an existing (pending) attempt is a
// no-op rather than resetting its state. RowsAffected is intentionally ignored:
// the caller has already established it owns this (repo, pr, sha) via the panel
// reservation, so whether the row was new or pre-existing does not change the
// outcome.
func reserveReviewAttemptTx(ctx context.Context, exec execer, repo string, pr int, sha string, now time.Time) error {
	_, err := reserveReviewAttemptExec(ctx, exec, repo, pr, sha, now)
	return err
}

// reserveReviewAttemptExec runs the idempotent attempt-row INSERT against any
// execer (the pooled *DB or a transaction connection), keeping the SQL in one
// place for both ReserveReviewAttempt and reserveReviewAttemptTx.
func reserveReviewAttemptExec(ctx context.Context, exec execer, repo string, pr int, sha string, now time.Time) (sql.Result, error) {
	ts := now.Format(time.RFC3339)
	res, err := exec.ExecContext(ctx, `
		INSERT INTO ci_pr_review_attempts
			(github_repo, pr_number, head_sha, attempt, first_attempt_at,
			 next_attempt_at, last_error_class, consecutive_genuine_attempts,
			 last_error_excerpt, last_panel_run_uuid, state, updated_at)
		VALUES (?, ?, ?, 1, ?, NULL, '', 0, '', '', 'pending', ?)
		ON CONFLICT(github_repo, pr_number, head_sha) DO NOTHING`,
		repo, pr, sha, ts, ts)
	if err != nil {
		return nil, fmt.Errorf("reserve review attempt: %w", err)
	}
	return res, nil
}

// GetReviewAttempt returns the attempt row for a (repo, pr, sha), or nil when
// no row exists.
func (db *DB) GetReviewAttempt(repo string, pr int, sha string) (*ReviewAttempt, error) {
	row := db.QueryRow(`SELECT `+reviewAttemptColumns+`
		FROM ci_pr_review_attempts
		WHERE github_repo = ? AND pr_number = ? AND head_sha = ?`,
		repo, pr, sha)
	a, err := scanReviewAttempt(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get review attempt: %w", err)
	}
	return a, nil
}

// DeferReviewAttempt marks a HEAD's attempt deferred until nextAttemptAt,
// recording the classified error so the retry sweep and give-up logic can act
// on it. bumpGenuine increments consecutive_genuine_attempts (a genuine
// failure that recurs) or resets it to zero (a transient outage), matching the
// give-up threshold semantics.
func (db *DB) DeferReviewAttempt(repo string, pr int, sha, errClass, excerpt, lastRunUUID string,
	nextAttemptAt time.Time, bumpGenuine bool,
) error {
	_, err := db.Exec(`
		UPDATE ci_pr_review_attempts
		SET state = 'deferred',
		    next_attempt_at = ?,
		    last_error_class = ?,
		    last_error_excerpt = ?,
		    last_panel_run_uuid = ?,
		    consecutive_genuine_attempts =
		        CASE WHEN ? THEN consecutive_genuine_attempts + 1 ELSE 0 END,
		    updated_at = ?
		WHERE github_repo = ? AND pr_number = ? AND head_sha = ?`,
		nextAttemptAt.Format(time.RFC3339), errClass, excerpt, lastRunUUID,
		bumpGenuine, time.Now().Format(time.RFC3339),
		repo, pr, sha)
	if err != nil {
		return fmt.Errorf("defer review attempt: %w", err)
	}
	return nil
}

// RearmStuckReviewAttempt re-defers a 'pending' attempt that the crash/stuck
// reconcile found stranded — claimed by the retry sweep (state flipped to
// 'pending', attempt bumped) but then CreateCIPanelRun failed or the daemon
// crashed before any panel run existed. Unlike DeferReviewAttempt it preserves
// consecutive_genuine_attempts and the recorded error fields: a failed enqueue
// is an infrastructure hiccup, not a fresh review failure, so the genuine
// give-up streak must survive untouched (routing through DeferReviewAttempt with
// bumpGenuine=false would reset it to zero and defeat genuine give-up). The
// WHERE state='pending' guard is a compare-and-swap — if a concurrent writer
// already moved the row out of 'pending' (e.g. a late finalization), this is a
// no-op. Returns whether the row was re-armed.
func (db *DB) RearmStuckReviewAttempt(repo string, pr int, sha string, nextAttemptAt time.Time) (bool, error) {
	res, err := db.Exec(`
		UPDATE ci_pr_review_attempts
		SET state = 'deferred', next_attempt_at = ?, updated_at = ?
		WHERE github_repo = ? AND pr_number = ? AND head_sha = ?
		  AND state = 'pending'`,
		nextAttemptAt.Format(time.RFC3339), time.Now().Format(time.RFC3339),
		repo, pr, sha)
	if err != nil {
		return false, fmt.Errorf("rearm stuck review attempt: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("rearm stuck review attempt rows: %w", err)
	}
	return n == 1, nil
}

// ClaimDueReviewAttempt atomically claims a deferred HEAD whose next_attempt_at
// is due (<= now), flipping it to 'pending' and bumping attempt. The
// UPDATE ... WHERE state='deferred' AND <due> is the contention point: only the
// first updater matches the predicate, so RowsAffected()==1 identifies the
// unique winner (mirroring ClaimPanelForPosting). On a win it re-SELECTs the
// row to return the new attempt and first_attempt_at. The due comparison uses
// SQLite datetime() on both sides — like GetTimedOutPanels — so a 'T'-vs-space
// formatting mismatch can never make a row read as due.
func (db *DB) ClaimDueReviewAttempt(repo string, pr int, sha string, now time.Time) (bool, int, time.Time, error) {
	nowTS := now.Format(time.RFC3339)
	res, err := db.Exec(`
		UPDATE ci_pr_review_attempts
		SET state = 'pending', attempt = attempt + 1, next_attempt_at = NULL,
		    updated_at = ?
		WHERE github_repo = ? AND pr_number = ? AND head_sha = ?
		  AND state = 'deferred'
		  AND next_attempt_at IS NOT NULL
		  AND datetime(next_attempt_at) <= datetime(?)`,
		nowTS, repo, pr, sha, nowTS)
	if err != nil {
		return false, 0, time.Time{}, fmt.Errorf("claim due review attempt: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, 0, time.Time{}, fmt.Errorf("claim due review attempt rows: %w", err)
	}
	if n != 1 {
		return false, 0, time.Time{}, nil
	}
	a, err := db.GetReviewAttempt(repo, pr, sha)
	if err != nil {
		return false, 0, time.Time{}, err
	}
	if a == nil {
		return false, 0, time.Time{}, fmt.Errorf("claim due review attempt: row vanished after claim")
	}
	return true, a.Attempt, a.FirstAttemptAt, nil
}

// MarkReviewAttemptDone marks a HEAD's attempt terminal (state='done') so the
// retry sweep and non-terminal scans skip it.
func (db *DB) MarkReviewAttemptDone(repo string, pr int, sha string) error {
	_, err := db.Exec(`
		UPDATE ci_pr_review_attempts
		SET state = 'done', next_attempt_at = NULL, updated_at = ?
		WHERE github_repo = ? AND pr_number = ? AND head_sha = ?`,
		time.Now().Format(time.RFC3339), repo, pr, sha)
	if err != nil {
		return fmt.Errorf("mark review attempt done: %w", err)
	}
	return nil
}

// DeleteReviewAttempt removes a single attempt row.
func (db *DB) DeleteReviewAttempt(repo string, pr int, sha string) error {
	_, err := db.Exec(`DELETE FROM ci_pr_review_attempts
		WHERE github_repo = ? AND pr_number = ? AND head_sha = ?`, repo, pr, sha)
	if err != nil {
		return fmt.Errorf("delete review attempt: %w", err)
	}
	return nil
}

// DeleteReviewAttemptsForPR removes every attempt row for a PR (across HEAD
// SHAs) and returns the number deleted. Used by closed-PR cleanup.
func (db *DB) DeleteReviewAttemptsForPR(repo string, pr int) (int64, error) {
	res, err := db.Exec(`DELETE FROM ci_pr_review_attempts
		WHERE github_repo = ? AND pr_number = ?`, repo, pr)
	if err != nil {
		return 0, fmt.Errorf("delete review attempts for PR: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("delete review attempts for PR rows: %w", err)
	}
	return n, nil
}

// GetNonTerminalAttemptPRs returns the distinct (github_repo, pr_number) pairs
// that still have a non-terminal attempt (pending or deferred), so the poll
// loop can check whether those PRs are still open for closed-PR cleanup.
// Reuses PanelPRRef, the same repo+pr pair type GetPendingPanelPRs returns.
func (db *DB) GetNonTerminalAttemptPRs(repo string) ([]PanelPRRef, error) {
	rows, err := db.Query(`
		SELECT DISTINCT github_repo, pr_number
		FROM ci_pr_review_attempts
		WHERE github_repo = ? AND state IN ('pending', 'deferred')`, repo)
	if err != nil {
		return nil, fmt.Errorf("get non-terminal attempt PRs: %w", err)
	}
	defer rows.Close()

	var refs []PanelPRRef
	for rows.Next() {
		var ref PanelPRRef
		if err := rows.Scan(&ref.GithubRepo, &ref.PRNumber); err != nil {
			return nil, fmt.Errorf("scan non-terminal attempt PR: %w", err)
		}
		refs = append(refs, ref)
	}
	return refs, rows.Err()
}

// GetPendingReviewAttempts returns every attempt for a repo still in the
// 'pending' state — a HEAD that is reserved or claimed but not yet deferred or
// done. The crash/stuck reconcile uses this set to find attempts stranded in
// 'pending' with no live panel (the retry sweep only selects 'deferred', so a
// pending row whose CreateCIPanelRun failed after the claim has nothing to
// re-arm it). Repo-scoped to match the per-repo poll loop.
func (db *DB) GetPendingReviewAttempts(repo string) ([]ReviewAttempt, error) {
	rows, err := db.Query(`SELECT `+reviewAttemptColumns+`
		FROM ci_pr_review_attempts
		WHERE github_repo = ? AND state = 'pending'`, repo)
	if err != nil {
		return nil, fmt.Errorf("get pending review attempts: %w", err)
	}
	defer rows.Close()

	var attempts []ReviewAttempt
	for rows.Next() {
		a, err := scanReviewAttempt(rows)
		if err != nil {
			return nil, fmt.Errorf("scan pending review attempt: %w", err)
		}
		attempts = append(attempts, *a)
	}
	return attempts, rows.Err()
}

// GetDueReviewAttempts returns the deferred attempts whose next_attempt_at is
// due (<= now) — the retry sweep's candidate set. The due comparison uses
// SQLite datetime() arithmetic on both sides, mirroring GetTimedOutPanels, so
// a 'T'-vs-space formatting mismatch can never make a fresh row read as due.
func (db *DB) GetDueReviewAttempts(repo string, now time.Time) ([]ReviewAttempt, error) {
	rows, err := db.Query(`SELECT `+reviewAttemptColumns+`
		FROM ci_pr_review_attempts
		WHERE github_repo = ? AND state = 'deferred'
		  AND next_attempt_at IS NOT NULL
		  AND datetime(next_attempt_at) <= datetime(?)`,
		repo, now.Format(time.RFC3339))
	if err != nil {
		return nil, fmt.Errorf("get due review attempts: %w", err)
	}
	defer rows.Close()

	var attempts []ReviewAttempt
	for rows.Next() {
		a, err := scanReviewAttempt(rows)
		if err != nil {
			return nil, fmt.Errorf("scan due review attempt: %w", err)
		}
		attempts = append(attempts, *a)
	}
	return attempts, rows.Err()
}
