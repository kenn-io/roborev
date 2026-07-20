package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/uptrace/bun"
)

// Panel terminal outcomes, persisted once at finalization. A NULL outcome
// means the row was finalized before outcome persistence existed; exports
// surface it as PanelOutcomeUnknown.
const (
	PanelOutcomeReviewPosted   = "review_posted"
	PanelOutcomeNoReviewPosted = "no_review_posted"
	PanelOutcomeGiveupPosted   = "giveup_posted"
	PanelOutcomeAbandoned      = "abandoned"
	PanelOutcomeUnknown        = "unknown"

	// PanelOutcomeLegacyReview marks rows exported from the frozen pre-panel
	// ci_pr_reviews table (ExportCIMetricsOptions.Legacy). It is never
	// persisted to ci_pr_panels; ExportCIMetrics stamps it onto legacy rows
	// at export time.
	PanelOutcomeLegacyReview = "legacy_review"
)

// CIPanel maps a PR HEAD (github_repo, pr_number, head_sha) to the subagent
// panel run that reviews it and to that run's synthesis job. It is the
// panel-based successor to CIPRBatch: instead of tracking a matrix of jobs
// with completion counters, a panel run owns its own synthesis gating, so the
// CI mapping only needs to remember which run covers which HEAD and when its
// PR comment was claimed/posted.
type CIPanel struct {
	ID               int64      `json:"id"`
	GithubRepo       string     `json:"github_repo"`
	PRNumber         int        `json:"pr_number"`
	HeadSHA          string     `json:"head_sha"`
	PanelRunUUID     string     `json:"panel_run_uuid"`
	SynthesisJobID   *int64     `json:"synthesis_job_id,omitempty"`
	CreatedAt        time.Time  `json:"created_at"`
	PostingClaimedAt *time.Time `json:"posting_claimed_at,omitempty"`
	PostedAt         *time.Time `json:"posted_at,omitempty"`
	RetiredAt        *time.Time `json:"retired_at,omitempty"`
	Outcome          *string    `json:"outcome,omitempty"`
	FirstAttemptAt   *time.Time `json:"first_attempt_at,omitempty"`
	AttemptCount     *int64     `json:"attempt_count,omitempty"`
	SynthesisAgent   *string    `json:"synthesis_agent,omitempty"`
	SynthesisModel   *string    `json:"synthesis_model,omitempty"`

	// AllowStalePost permits the panel's review to post even after the PR
	// HEAD advances past the reviewed HEAD. Set by quiet-hours-only
	// deferrals, which retain the in-flight panel as an interval snapshot
	// instead of superseding it.
	AllowStalePost bool `json:"allow_stale_post,omitempty"`
}

func (db *DB) selectCIPanel(where string, args ...any) (*CIPanel, error) {
	var row ciPanelRow
	if err := db.bun.NewSelect().
		Model(&row).
		Column(sqliteCIPanelColumns...).
		Where(where, args...).
		Scan(context.Background()); err != nil {
		return nil, err
	}
	panel := row.toModel()
	return &panel, nil
}

func scanCIPanels(query *bun.SelectQuery) ([]CIPanel, error) {
	var rows []ciPanelRow
	if err := query.Scan(context.Background(), &rows); err != nil {
		return nil, err
	}
	var panels []CIPanel
	for _, row := range rows {
		panels = append(panels, row.toModel())
	}
	return panels, nil
}

// GetCIPanelByPRSHA returns the panel mapping for a PR at a specific HEAD SHA.
// Returns sql.ErrNoRows when no mapping exists.
func (db *DB) GetCIPanelByPRSHA(githubRepo string, prNumber int, headSHA string) (*CIPanel, error) {
	return db.selectCIPanel(
		"github_repo = ? AND pr_number = ? AND head_sha = ?",
		githubRepo, prNumber, headSHA,
	)
}

// GetActiveCIPanelByPRSHA returns the non-retired panel mapping for a PR at a
// specific HEAD SHA. Posted rows are still active for this lookup: a posted
// same-HEAD mapping means the head was already reviewed and must not be
// throttled back to pending.
func (db *DB) GetActiveCIPanelByPRSHA(githubRepo string, prNumber int, headSHA string) (*CIPanel, error) {
	return db.selectCIPanel(
		"github_repo = ? AND pr_number = ? AND head_sha = ? AND retired_at IS NULL",
		githubRepo, prNumber, headSHA,
	)
}

// GetCIPanelBySynthesisJobID returns the panel mapping whose run is finalized
// by the given synthesis job. Returns sql.ErrNoRows when no mapping exists.
func (db *DB) GetCIPanelBySynthesisJobID(jobID int64) (*CIPanel, error) {
	return db.selectCIPanel("synthesis_job_id = ?", jobID)
}

// GetCIPanelByRunUUID returns the CI panel mapping for a panel run UUID.
// Returns sql.ErrNoRows when the panel run is not CI-owned.
func (db *DB) GetCIPanelByRunUUID(panelRunUUID string) (*CIPanel, error) {
	return db.selectCIPanel("panel_run_uuid = ?", panelRunUUID)
}

// CreateCIPanelRun atomically reserves the ci_pr_panels row, inserts the run's
// member + synthesis jobs (sharing one generated panel_run_uuid), and backfills
// synthesis_job_id — all in one BEGIN IMMEDIATE transaction. Returns
// created=false (and rolls back) when another caller already owns this
// (repo, pr, sha). F2, F9.
func (db *DB) CreateCIPanelRun(githubRepo string, prNumber int, headSHA string,
	members []EnqueueOpts, synthesis EnqueueOpts,
) (bool, []*ReviewJob, *ReviewJob, error) {
	machineID, _ := db.GetMachineID() // WRITES on a pooled conn — must precede BEGIN
	now := time.Now()

	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		return false, nil, nil, err
	}
	defer conn.Close()
	// Raw SQL allowlist: SQLite transaction mode. Panel reservation, attempt
	// reservation, job creation, and synthesis backfill share one write lock.
	if _, err := db.bun.NewRaw("BEGIN IMMEDIATE").Conn(conn).Exec(ctx); err != nil {
		return false, nil, nil, err
	}
	committed := false
	defer func() {
		if !committed {
			if _, err := db.bun.NewRaw("ROLLBACK").Conn(conn).Exec(ctx); err != nil {
				log.Printf("CreateCIPanelRun: rollback failed: %v", err)
			}
		}
	}()

	created, mems, syn, err := db.createCIPanelRunTx(ctx, conn, githubRepo, prNumber, headSHA, members, synthesis, machineID, now)
	if err != nil {
		return false, nil, nil, err // deferred rollback fires
	}
	if !created {
		return false, nil, nil, nil // loser: deferred rollback fires, zero jobs
	}
	if _, err := db.bun.NewRaw("COMMIT").Conn(conn).Exec(ctx); err != nil {
		return false, nil, nil, err
	}
	committed = true
	return true, mems, syn, nil
}

// createCIPanelRunTx reserves the mapping, inserts the run's jobs, and backfills
// synthesis_job_id via exec. The caller owns the transaction (BEGIN/COMMIT/
// ROLLBACK) exactly like enqueuePanelRunTx. Returns created=false (no rows
// inserted) when the INSERT OR IGNORE reservation loses to a concurrent caller.
func (db *DB) createCIPanelRunTx(ctx context.Context, exec execer, githubRepo string, prNumber int, headSHA string,
	members []EnqueueOpts, synthesis EnqueueOpts, machineID string, now time.Time,
) (bool, []*ReviewJob, *ReviewJob, error) {
	runUUID := GenerateUUID()

	deleteRetired := db.bun.NewDelete().Model((*ciPanelRow)(nil)).
		Where("github_repo = ?", githubRepo).Where("pr_number = ?", prNumber).
		Where("head_sha = ?", headSHA).Where("retired_at IS NOT NULL")
	if _, err := deleteRetired.Conn(exec).Exec(ctx); err != nil {
		return false, nil, nil, err
	}

	row := ciPanelRow{
		GithubRepo: githubRepo, PRNumber: prNumber, HeadSHA: headSHA,
		PanelRunUUID: runUUID, CreatedAt: dbTimeFromValue(now),
	}
	insert := db.bun.NewInsert().Model(&row).
		Column("github_repo", "pr_number", "head_sha", "panel_run_uuid", "created_at").
		On("CONFLICT (github_repo, pr_number, head_sha) DO NOTHING")
	res, err := insert.Conn(exec).Exec(ctx)
	if err != nil {
		return false, nil, nil, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, nil, nil, err
	}
	if n == 0 {
		return false, nil, nil, nil // another poller owns this PR/HEAD; roll back, create nothing
	}

	// Reserve the retry-attempt row in the SAME transaction so the durable
	// review state and the panel run are created atomically: a winning panel
	// reservation always yields exactly one attempt row, and a concurrent loser
	// (n==0 above) never reaches here. The INSERT is idempotent
	// (ON CONFLICT DO NOTHING) so the retry sweep's re-enqueue of an
	// already-claimed (pending) attempt is a harmless no-op rather than a clobber.
	if err := db.reserveReviewAttemptTx(ctx, exec, githubRepo, prNumber, headSHA, now); err != nil {
		return false, nil, nil, err
	}

	// F9: stamp the run uuid onto every job before enqueuePanelRunTx, which
	// enforces role/gate but NOT the run uuid.
	for i := range members {
		members[i].PanelRunUUID = runUUID
		members[i].Source = JobSourceCI
	}
	synthesis.PanelRunUUID = runUUID
	synthesis.Source = JobSourceCI

	mems, syn, err := db.enqueuePanelRunTx(ctx, exec, members, synthesis, machineID, now)
	if err != nil {
		return false, nil, nil, err
	}
	backfill := db.bun.NewUpdate().Model((*ciPanelRow)(nil)).
		Set("synthesis_job_id = ?", syn.ID).Where("panel_run_uuid = ?", runUUID)
	if _, err := backfill.Conn(exec).Exec(ctx); err != nil {
		return false, nil, nil, err
	}
	return true, mems, syn, nil
}

// ClaimPanelForPosting atomically leases the row for posting. Returns true only
// to the single caller whose UPDATE matched: posted_at is NULL and the claim is
// either unset or older than staleWindow (a crashed poster's lease is reclaimable).
// F3 — guarantees one PR comment per run.
func (db *DB) ClaimPanelForPosting(id int64, staleWindow time.Duration) (bool, error) {
	staleArg := fmt.Sprintf("-%d seconds", int64(staleWindow.Seconds()))
	// Raw SQL allowlist: guarded atomic state transition with SQLite datetime
	// arithmetic. RowsAffected identifies the unique posting-lease winner.
	res, err := db.bun.NewRaw(`
		UPDATE ci_pr_panels SET posting_claimed_at = datetime('now')
		 WHERE id = ? AND posted_at IS NULL
		   AND retired_at IS NULL
		   AND (posting_claimed_at IS NULL
		        OR datetime(posting_claimed_at) < datetime('now', ?))`, id, staleArg).
		Exec(context.Background())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// ReleasePanelPostClaim clears an unposted row's lease so it can be reclaimed,
// e.g. when a poster fails before posting and wants another to retry.
func (db *DB) ReleasePanelPostClaim(id int64) error {
	_, err := db.bun.NewUpdate().
		Model((*ciPanelRow)(nil)).
		Set("posting_claimed_at = NULL").
		Where("id = ?", id).
		Where("posted_at IS NULL").
		Where("retired_at IS NULL").
		Exec(context.Background())
	return err
}

// MarkPanelPosted finalizes the run: in one atomic transaction it finalizes
// the panel row — permanently barring further posting claims and stamping the
// terminal outcome, a snapshot of first_attempt_at/attempt from the
// operational attempt row, and a snapshot of the synthesis job's agent/model —
// then marks the HEAD's review attempt terminal (state='done', mirroring
// MarkReviewAttemptDone). The panel UPDATE is guarded with
// posted_at IS NULL AND retired_at IS NULL and its RowsAffected is checked: a
// stale posting lease that races a previous MarkPanelPosted call (or a
// concurrent retire) must not double-finalize the row or overwrite an
// already-stamped outcome, so the whole transaction is rolled back and an
// error returned instead of also marking the attempt done. Both snapshots
// matter because closed-PR cleanup later deletes attempt rows and cascade
// repo deletion deletes review_jobs rows; the panel row is the durable record
// of terminal metrics. The attempt row may already be gone (deleted by
// closed-PR cleanup); zero rows affected there is not an error.
func (db *DB) MarkPanelPosted(id int64, outcome string) error {
	tx, err := db.bun.BeginTx(context.Background(), nil)
	if err != nil {
		return fmt.Errorf("mark panel posted: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Raw SQL allowlist: guarded atomic state transition. Finalization snapshots
	// related attempt and synthesis fields in the same statement.
	res, err := db.bun.NewRaw(`
		UPDATE ci_pr_panels
		SET posted_at = datetime('now'),
		    outcome = ?,
		    first_attempt_at = (
		        SELECT a.first_attempt_at FROM ci_pr_review_attempts a
		        WHERE a.github_repo = ci_pr_panels.github_repo
		          AND a.pr_number = ci_pr_panels.pr_number
		          AND a.head_sha = ci_pr_panels.head_sha),
		    attempt_count = (
		        SELECT a.attempt FROM ci_pr_review_attempts a
		        WHERE a.github_repo = ci_pr_panels.github_repo
		          AND a.pr_number = ci_pr_panels.pr_number
		          AND a.head_sha = ci_pr_panels.head_sha),
		    synthesis_agent = (
		        SELECT j.agent FROM review_jobs j WHERE j.id = ci_pr_panels.synthesis_job_id),
		    synthesis_model = (
		        SELECT j.model FROM review_jobs j WHERE j.id = ci_pr_panels.synthesis_job_id)
		WHERE id = ? AND posted_at IS NULL AND retired_at IS NULL`, outcome, id).
		Conn(tx).
		Exec(context.Background())
	if err != nil {
		return fmt.Errorf("mark panel posted: finalize panel: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("mark panel posted: rows affected: %w", err)
	}
	if n != 1 {
		return fmt.Errorf("panel %d not finalizable: already posted, retired, or missing", id)
	}

	now := dbTimeFromValue(time.Now())
	if _, err := db.bun.NewRaw(`
		UPDATE ci_pr_review_attempts
		SET state = 'done', next_attempt_at = NULL, updated_at = ?
		WHERE (github_repo, pr_number, head_sha) = (
		    SELECT github_repo, pr_number, head_sha FROM ci_pr_panels WHERE id = ?)`,
		now, id).
		Conn(tx).
		Exec(context.Background()); err != nil {
		return fmt.Errorf("mark panel posted: mark attempt done: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("mark panel posted: commit: %w", err)
	}
	return nil
}

// MarkPanelRetired makes an abandoned panel row non-postable while retaining its
// created_at timestamp for throttle calculations.
func (db *DB) MarkPanelRetired(id int64) error {
	_, err := db.bun.NewUpdate().
		Model((*ciPanelRow)(nil)).
		Set("retired_at = datetime('now')").
		Set("posting_claimed_at = NULL").
		Where("id = ?", id).
		Where("posted_at IS NULL").
		Where("retired_at IS NULL").
		Exec(context.Background())
	return err
}

// MarkPanelRetiredIfStalePostDisallowed retires the panel only when
// allow_stale_post is still unset, as one atomic statement. This closes the
// posting-time race with MarkPanelsAllowStalePost: SQLite serializes the two
// writes, so either this retirement lands first (the marker's retired_at IS
// NULL predicate then skips the row) or the marking lands first (the
// allow_stale_post = 0 predicate here affects zero rows and the caller posts
// the retained snapshot). Returns whether the row was retired.
func (db *DB) MarkPanelRetiredIfStalePostDisallowed(id int64) (bool, error) {
	res, err := db.bun.NewUpdate().
		Model((*ciPanelRow)(nil)).
		Set("retired_at = datetime('now')").
		Set("posting_claimed_at = NULL").
		Where("id = ?", id).
		Where("posted_at IS NULL").
		Where("retired_at IS NULL").
		Where("allow_stale_post = 0").
		Exec(context.Background())
	if err != nil {
		return false, fmt.Errorf("retire panel if stale post disallowed: %w", err)
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// PanelPRRef identifies a (github_repo, pr_number) pair for panel PR lookups.
type PanelPRRef struct {
	GithubRepo string
	PRNumber   int
}

// MarkPanelsAllowStalePost flags every still-active panel run for a PR at a
// HEAD other than newHeadSHA so its review may post even after the PR HEAD
// advances. Quiet-hours-only deferrals use this to retain the in-flight
// panel as an interval snapshot instead of superseding it. Returns the
// number of rows flagged.
func (db *DB) MarkPanelsAllowStalePost(githubRepo string, prNumber int, newHeadSHA string) (int64, error) {
	res, err := db.bun.NewUpdate().
		Model((*ciPanelRow)(nil)).
		Set("allow_stale_post = 1").
		Where("github_repo = ?", githubRepo).
		Where("pr_number = ?", prNumber).
		Where("head_sha != ?", newHeadSHA).
		Where("posted_at IS NULL").
		Where("retired_at IS NULL").
		Where("allow_stale_post = 0").
		Exec(context.Background())
	if err != nil {
		return 0, fmt.Errorf("mark panels allow_stale_post: %w", err)
	}
	return res.RowsAffected()
}

// GetActivePanelsForPR returns the un-posted, non-retired panel runs for a
// (github_repo, pr_number). Used by the supersede and closed-PR cleanup sweeps
// to find every still-active run for a PR (across HEAD SHAs).
func (db *DB) GetActivePanelsForPR(githubRepo string, prNumber int) ([]CIPanel, error) {
	return scanCIPanels(db.bun.NewSelect().
		Model((*ciPanelRow)(nil)).
		Column(sqliteCIPanelColumns...).
		Where("github_repo = ?", githubRepo).
		Where("pr_number = ?", prNumber).
		Where("posted_at IS NULL").
		Where("retired_at IS NULL"))
}

// GetTimedOutPanels returns the un-posted panel runs with at least one running
// member whose own started_at is older than maxAge — the timeout sweep's
// candidate set. The cutoff uses SQLite datetime arithmetic (datetime('now', ?))
// rather than a Go-formatted timestamp compared lexically: a 'T'-vs-space
// mismatch at offset 10 would otherwise make fresh timestamps sort as timed out.
// Panel created_at remains the immutable CI throttle clock, so restart recovery
// must not use it as runtime state.
func (db *DB) GetTimedOutPanels(githubRepo string, maxAge time.Duration) ([]CIPanel, error) {
	cutoff := fmt.Sprintf("-%d seconds", int64(maxAge.Seconds()))
	return scanCIPanels(db.bun.NewSelect().
		Model((*ciPanelRow)(nil)).
		Column(sqliteCIPanelColumns...).
		Where("cp.github_repo = ?", githubRepo).
		Where("cp.posted_at IS NULL").
		Where("cp.retired_at IS NULL").
		Where(`EXISTS (
			SELECT 1 FROM review_jobs AS j
			WHERE j.panel_run_uuid = cp.panel_run_uuid
			  AND j.panel_role = 'member'
			  AND j.status = 'running'
			  AND j.started_at IS NOT NULL
			  AND datetime(j.started_at) < datetime('now', ?)
		)`, cutoff))
}

// GetUnpostedTerminalPanels returns panel rows whose synthesis job is terminal
// (done or failed) but that were never posted — the dropped-event / crash
// recovery set for the spec §10 posting reconcile.
func (db *DB) GetUnpostedTerminalPanels(githubRepo string) ([]CIPanel, error) {
	return scanCIPanels(db.bun.NewSelect().
		Model((*ciPanelRow)(nil)).
		Column(sqliteCIPanelColumns...).
		Where("cp.github_repo = ?", githubRepo).
		Where("cp.posted_at IS NULL").
		Where("cp.retired_at IS NULL").
		Where(`EXISTS (
			SELECT 1 FROM review_jobs AS s
			WHERE s.id = cp.synthesis_job_id
			  AND s.status IN ('done', 'failed')
		)`))
}

// GetPendingPanelPRs returns the distinct (github_repo, pr_number) pairs that
// have an un-posted panel run, so the poll loop can check whether those PRs are
// still open (closed-PR cleanup). Mirrors GetPendingBatchPRs. F13.
func (db *DB) GetPendingPanelPRs(githubRepo string) ([]PanelPRRef, error) {
	var refs []PanelPRRef
	if err := db.bun.NewSelect().
		Model((*ciPanelRow)(nil)).
		Column("github_repo", "pr_number").
		Distinct().
		Where("github_repo = ?", githubRepo).
		Where("posted_at IS NULL").
		Where("retired_at IS NULL").
		Scan(context.Background(), &refs); err != nil {
		return nil, err
	}
	return refs, nil
}

// LatestPanelTimeForPR returns the created_at of the most recent panel run for
// a PR (any HEAD SHA), or the zero time when no run exists. It is the
// panel-based successor to LatestBatchTimeForPR, used by the CI poller's
// throttle check. Timestamps are parsed with parseSQLiteTime, matching the
// other ci_pr_panels scanners.
func (db *DB) LatestPanelTimeForPR(githubRepo string, prNumber int) (time.Time, error) {
	var createdAt dbTime
	err := db.bun.NewSelect().
		Model((*ciPanelRow)(nil)).
		Column("created_at").
		Where("github_repo = ?", githubRepo).
		Where("pr_number = ?", prNumber).
		OrderExpr("datetime(created_at) DESC").
		Limit(1).
		Scan(context.Background(), &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	return createdAt.Time, nil
}

// DeleteCIPanel removes a single ci_pr_panels mapping row (supersede/cleanup).
func (db *DB) DeleteCIPanel(id int64) error {
	_, err := db.bun.NewDelete().
		Model((*ciPanelRow)(nil)).
		Where("id = ?", id).
		Exec(context.Background())
	return err
}

// DeleteCIPanelByRun removes the mapping row for a panel run uuid.
func (db *DB) DeleteCIPanelByRun(panelRunUUID string) error {
	_, err := db.bun.NewDelete().
		Model((*ciPanelRow)(nil)).
		Where("panel_run_uuid = ?", panelRunUUID).
		Exec(context.Background())
	return err
}
