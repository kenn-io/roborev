package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/uptrace/bun"
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

func scanReviewAttempts(query *bun.SelectQuery) ([]ReviewAttempt, error) {
	var rows []ciReviewAttemptRow
	if err := query.Scan(context.Background(), &rows); err != nil {
		return nil, err
	}
	var attempts []ReviewAttempt
	for _, row := range rows {
		attempts = append(attempts, row.toModel())
	}
	return attempts, nil
}

// ReserveReviewAttempt idempotently reserves the attempt row for a reviewed
// HEAD. The INSERT ... ON CONFLICT DO NOTHING means the first caller for a
// (repo, pr, sha) creates the row (created=true) while a duplicate enqueue is
// a no-op (created=false); this is what makes reserve-on-enqueue safe against
// double-enqueue. The initial row is attempt=1, state='pending',
// next_attempt_at=NULL, with empty error fields and a zero genuine streak.
func (db *DB) ReserveReviewAttempt(repo string, pr int, sha string, now time.Time) (bool, error) {
	res, err := db.reserveReviewAttemptExec(context.Background(), db, repo, pr, sha, now)
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
func (db *DB) reserveReviewAttemptTx(ctx context.Context, exec execer, repo string, pr int, sha string, now time.Time) error {
	_, err := db.reserveReviewAttemptExec(ctx, exec, repo, pr, sha, now)
	return err
}

// reserveReviewAttemptExec runs the idempotent attempt-row INSERT against any
// execer (the pooled *DB or a transaction connection), keeping the SQL in one
// place for both ReserveReviewAttempt and reserveReviewAttemptTx.
func (db *DB) reserveReviewAttemptExec(ctx context.Context, exec execer, repo string, pr int, sha string, now time.Time) (sql.Result, error) {
	row := ciReviewAttemptRow{
		GithubRepo: repo, PRNumber: pr, HeadSHA: sha, Attempt: 1,
		FirstAttemptAt: dbTimeFromValue(now), State: "pending", UpdatedAt: dbTimeFromValue(now),
	}
	insert := db.bun.NewInsert().Model(&row).
		Column("github_repo", "pr_number", "head_sha", "attempt", "first_attempt_at",
			"next_attempt_at", "last_error_class", "consecutive_genuine_attempts",
			"last_error_excerpt", "last_panel_run_uuid", "state", "updated_at").
		On("CONFLICT (github_repo, pr_number, head_sha) DO NOTHING")
	res, err := insert.Conn(exec).Exec(ctx)
	if err != nil {
		return nil, fmt.Errorf("reserve review attempt: %w", err)
	}
	return res, nil
}

// GetReviewAttempt returns the attempt row for a (repo, pr, sha), or nil when
// no row exists.
func (db *DB) GetReviewAttempt(repo string, pr int, sha string) (*ReviewAttempt, error) {
	var row ciReviewAttemptRow
	err := db.bun.NewSelect().Model(&row).Column(sqliteReviewAttemptColumns...).
		Where("github_repo = ?", repo).Where("pr_number = ?", pr).Where("head_sha = ?", sha).
		Scan(context.Background())
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get review attempt: %w", err)
	}
	a := row.toModel()
	return &a, nil
}

// DeferReviewAttempt marks a HEAD's attempt deferred until nextAttemptAt,
// recording the classified error so the retry sweep and give-up logic can act
// on it. bumpGenuine increments consecutive_genuine_attempts (a genuine
// failure that recurs) or resets it to zero (a transient outage), matching the
// give-up threshold semantics.
func (db *DB) DeferReviewAttempt(repo string, pr int, sha, errClass, excerpt, lastRunUUID string,
	nextAttemptAt time.Time, bumpGenuine bool,
) error {
	_, err := db.bun.NewUpdate().Model((*ciReviewAttemptRow)(nil)).
		Set("state = 'deferred'").Set("next_attempt_at = ?", dbTimeFromValue(nextAttemptAt)).
		Set("last_error_class = ?", errClass).Set("last_error_excerpt = ?", excerpt).
		Set("last_panel_run_uuid = ?", lastRunUUID).
		Set("consecutive_genuine_attempts = CASE WHEN ? THEN consecutive_genuine_attempts + 1 ELSE 0 END", bumpGenuine).
		Set("updated_at = ?", dbTimeFromValue(time.Now())).
		Where("github_repo = ?", repo).Where("pr_number = ?", pr).Where("head_sha = ?", sha).
		Exec(context.Background())
	if err != nil {
		return fmt.Errorf("defer review attempt: %w", err)
	}
	return nil
}

// MakeTransientReviewAttemptsDue clears startup-visible transient backoff by
// moving future deferred transient attempts to now. Quota/cooldown failures are
// recorded as transient at the CI-attempt layer, so a daemon restart after the
// provider recovers should let the retry sweep run immediately. Genuine
// deterministic failures keep their scheduled backoff.
func (db *DB) MakeTransientReviewAttemptsDue(now time.Time) (int64, error) {
	nowTS := dbTimeFromValue(now)
	res, err := db.bun.NewUpdate().Model((*ciReviewAttemptRow)(nil)).
		Set("next_attempt_at = ?", nowTS).Set("updated_at = ?", nowTS).
		Where("state = 'deferred'").Where("last_error_class = 'transient'").
		Where("next_attempt_at IS NOT NULL").Where("datetime(next_attempt_at) > datetime(?)", nowTS).
		Exec(context.Background())
	if err != nil {
		return 0, fmt.Errorf("make transient review attempts due: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("make transient review attempts due rows: %w", err)
	}
	return n, nil
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
	res, err := db.bun.NewUpdate().Model((*ciReviewAttemptRow)(nil)).
		Set("state = 'deferred'").Set("next_attempt_at = ?", dbTimeFromValue(nextAttemptAt)).
		Set("updated_at = ?", dbTimeFromValue(time.Now())).
		Where("github_repo = ?", repo).Where("pr_number = ?", pr).Where("head_sha = ?", sha).
		Where("state = 'pending'").Exec(context.Background())
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
	nowTS := dbTimeFromValue(now)
	// Raw SQL allowlist: the due predicate and attempt increment form one
	// atomic claim whose RowsAffected identifies the unique winner.
	res, err := db.bun.NewRaw(`
		UPDATE ci_pr_review_attempts
		SET state = 'pending', attempt = attempt + 1, next_attempt_at = NULL,
		    updated_at = ?
		WHERE github_repo = ? AND pr_number = ? AND head_sha = ?
		  AND state = 'deferred'
		  AND next_attempt_at IS NOT NULL
		  AND datetime(next_attempt_at) <= datetime(?)`,
		nowTS, repo, pr, sha, nowTS).Exec(context.Background())
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
	_, err := db.bun.NewUpdate().Model((*ciReviewAttemptRow)(nil)).
		Set("state = 'done'").Set("next_attempt_at = NULL").
		Set("updated_at = ?", dbTimeFromValue(time.Now())).
		Where("github_repo = ?", repo).Where("pr_number = ?", pr).Where("head_sha = ?", sha).
		Exec(context.Background())
	if err != nil {
		return fmt.Errorf("mark review attempt done: %w", err)
	}
	return nil
}

// DeleteReviewAttempt removes a single attempt row.
func (db *DB) DeleteReviewAttempt(repo string, pr int, sha string) error {
	_, err := db.bun.NewDelete().Model((*ciReviewAttemptRow)(nil)).
		Where("github_repo = ?", repo).Where("pr_number = ?", pr).Where("head_sha = ?", sha).
		Exec(context.Background())
	if err != nil {
		return fmt.Errorf("delete review attempt: %w", err)
	}
	return nil
}

// DeleteReviewAttemptsForPR removes every attempt row for a PR (across HEAD
// SHAs) and returns the number deleted. Used by closed-PR cleanup.
func (db *DB) DeleteReviewAttemptsForPR(repo string, pr int) (int64, error) {
	res, err := db.bun.NewDelete().Model((*ciReviewAttemptRow)(nil)).
		Where("github_repo = ?", repo).Where("pr_number = ?", pr).Exec(context.Background())
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
	var refs []PanelPRRef
	if err := db.bun.NewSelect().Model((*ciReviewAttemptRow)(nil)).
		Column("github_repo", "pr_number").Distinct().Where("github_repo = ?", repo).
		Where("state IN ('pending', 'deferred')").Scan(context.Background(), &refs); err != nil {
		return nil, fmt.Errorf("get non-terminal attempt PRs: %w", err)
	}
	return refs, nil
}

// GetPendingReviewAttempts returns every attempt for a repo still in the
// 'pending' state — a HEAD that is reserved or claimed but not yet deferred or
// done. The crash/stuck reconcile uses this set to find attempts stranded in
// 'pending' with no live panel (the retry sweep only selects 'deferred', so a
// pending row whose CreateCIPanelRun failed after the claim has nothing to
// re-arm it). Repo-scoped to match the per-repo poll loop.
func (db *DB) GetPendingReviewAttempts(repo string) ([]ReviewAttempt, error) {
	attempts, err := scanReviewAttempts(db.bun.NewSelect().Model((*ciReviewAttemptRow)(nil)).
		Column(sqliteReviewAttemptColumns...).Where("github_repo = ?", repo).Where("state = 'pending'"))
	if err != nil {
		return nil, fmt.Errorf("get pending review attempts: %w", err)
	}
	return attempts, nil
}

// GetDueReviewAttempts returns the deferred attempts whose next_attempt_at is
// due (<= now) — the retry sweep's candidate set. The due comparison uses
// SQLite datetime() arithmetic on both sides, mirroring GetTimedOutPanels, so
// a 'T'-vs-space formatting mismatch can never make a fresh row read as due.
func (db *DB) GetDueReviewAttempts(repo string, now time.Time) ([]ReviewAttempt, error) {
	attempts, err := scanReviewAttempts(db.bun.NewSelect().Model((*ciReviewAttemptRow)(nil)).
		Column(sqliteReviewAttemptColumns...).Where("github_repo = ?", repo).
		Where("state = 'deferred'").Where("next_attempt_at IS NOT NULL").
		Where("datetime(next_attempt_at) <= datetime(?)", dbTimeFromValue(now)))
	if err != nil {
		return nil, fmt.Errorf("get due review attempts: %w", err)
	}
	return attempts, nil
}
