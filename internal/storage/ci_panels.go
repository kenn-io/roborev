package storage

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"time"
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

// ciPanelColumns is the canonical SELECT column list for scanCIPanel. Kept in
// one place so every Get* query and the scanner stay in lockstep.
const ciPanelColumns = `id, github_repo, pr_number, head_sha, panel_run_uuid,
	synthesis_job_id, created_at, posting_claimed_at, posted_at, retired_at,
	outcome, first_attempt_at, attempt_count, synthesis_agent, synthesis_model,
	allow_stale_post`

// scanCIPanel hydrates a CIPanel from a row selecting ciPanelColumns. Nullable
// columns are scanned through sql.Null* and the timestamps parsed with
// parseSQLiteTime, mirroring how review_jobs nullable timestamps are hydrated
// in applyReviewJobScan.
func scanCIPanel(row sqlScanner) (*CIPanel, error) {
	var p CIPanel
	var synthesisJobID sql.NullInt64
	var createdAt sql.NullString
	var postingClaimedAt sql.NullString
	var postedAt sql.NullString
	var retiredAt sql.NullString
	var outcome sql.NullString
	var firstAttemptAt sql.NullString
	var attemptCount sql.NullInt64
	var synthesisAgent sql.NullString
	var synthesisModel sql.NullString
	if err := row.Scan(
		&p.ID, &p.GithubRepo, &p.PRNumber, &p.HeadSHA, &p.PanelRunUUID,
		&synthesisJobID, &createdAt, &postingClaimedAt, &postedAt, &retiredAt,
		&outcome, &firstAttemptAt, &attemptCount, &synthesisAgent, &synthesisModel,
		&p.AllowStalePost,
	); err != nil {
		return nil, err
	}
	if synthesisJobID.Valid {
		p.SynthesisJobID = &synthesisJobID.Int64
	}
	if createdAt.Valid {
		p.CreatedAt = parseSQLiteTime(createdAt.String)
	}
	if postingClaimedAt.Valid {
		t := parseSQLiteTime(postingClaimedAt.String)
		p.PostingClaimedAt = &t
	}
	if postedAt.Valid {
		t := parseSQLiteTime(postedAt.String)
		p.PostedAt = &t
	}
	if retiredAt.Valid {
		t := parseSQLiteTime(retiredAt.String)
		p.RetiredAt = &t
	}
	if outcome.Valid {
		p.Outcome = &outcome.String
	}
	if firstAttemptAt.Valid {
		t := parseSQLiteTime(firstAttemptAt.String)
		p.FirstAttemptAt = &t
	}
	if attemptCount.Valid {
		p.AttemptCount = &attemptCount.Int64
	}
	if synthesisAgent.Valid {
		p.SynthesisAgent = &synthesisAgent.String
	}
	if synthesisModel.Valid {
		p.SynthesisModel = &synthesisModel.String
	}
	return &p, nil
}

// GetCIPanelByPRSHA returns the panel mapping for a PR at a specific HEAD SHA.
// Returns sql.ErrNoRows when no mapping exists.
func (db *DB) GetCIPanelByPRSHA(githubRepo string, prNumber int, headSHA string) (*CIPanel, error) {
	row := db.QueryRow(`SELECT `+ciPanelColumns+`
		FROM ci_pr_panels
		WHERE github_repo = ? AND pr_number = ? AND head_sha = ?`,
		githubRepo, prNumber, headSHA)
	return scanCIPanel(row)
}

// GetActiveCIPanelByPRSHA returns the non-retired panel mapping for a PR at a
// specific HEAD SHA. Posted rows are still active for this lookup: a posted
// same-HEAD mapping means the head was already reviewed and must not be
// throttled back to pending.
func (db *DB) GetActiveCIPanelByPRSHA(githubRepo string, prNumber int, headSHA string) (*CIPanel, error) {
	row := db.QueryRow(`SELECT `+ciPanelColumns+`
		FROM ci_pr_panels
		WHERE github_repo = ? AND pr_number = ? AND head_sha = ? AND retired_at IS NULL`,
		githubRepo, prNumber, headSHA)
	return scanCIPanel(row)
}

// GetCIPanelBySynthesisJobID returns the panel mapping whose run is finalized
// by the given synthesis job. Returns sql.ErrNoRows when no mapping exists.
func (db *DB) GetCIPanelBySynthesisJobID(jobID int64) (*CIPanel, error) {
	row := db.QueryRow(`SELECT `+ciPanelColumns+`
		FROM ci_pr_panels
		WHERE synthesis_job_id = ?`, jobID)
	return scanCIPanel(row)
}

// GetCIPanelByRunUUID returns the CI panel mapping for a panel run UUID.
// Returns sql.ErrNoRows when the panel run is not CI-owned.
func (db *DB) GetCIPanelByRunUUID(panelRunUUID string) (*CIPanel, error) {
	row := db.QueryRow(`SELECT `+ciPanelColumns+`
		FROM ci_pr_panels
		WHERE panel_run_uuid = ?`, panelRunUUID)
	return scanCIPanel(row)
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
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return false, nil, nil, err
	}
	committed := false
	defer func() {
		if !committed {
			if _, err := conn.ExecContext(ctx, "ROLLBACK"); err != nil {
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
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
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

	// Rows created before source was persisted are CI-owned only through this
	// mapping. Preserve that ownership before replacing a retired same-HEAD
	// mapping so historical retry costs remain exportable.
	if _, err := exec.ExecContext(ctx,
		`UPDATE review_jobs
		 SET source = ?
		 WHERE COALESCE(source, '') = ''
		   AND panel_run_uuid IN (
		       SELECT panel_run_uuid FROM ci_pr_panels
		       WHERE github_repo = ? AND pr_number = ? AND head_sha = ?
		         AND retired_at IS NOT NULL
		   )`, JobSourceCI, githubRepo, prNumber, headSHA); err != nil {
		return false, nil, nil, err
	}
	if _, err := exec.ExecContext(ctx,
		`DELETE FROM ci_pr_panels
		 WHERE github_repo = ? AND pr_number = ? AND head_sha = ? AND retired_at IS NOT NULL`,
		githubRepo, prNumber, headSHA); err != nil {
		return false, nil, nil, err
	}

	res, err := exec.ExecContext(ctx,
		`INSERT OR IGNORE INTO ci_pr_panels (github_repo, pr_number, head_sha, panel_run_uuid, created_at)
		 VALUES (?, ?, ?, ?, ?)`,
		githubRepo, prNumber, headSHA, runUUID, now.Format(time.RFC3339))
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
	if err := reserveReviewAttemptTx(ctx, exec, githubRepo, prNumber, headSHA, now); err != nil {
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
	if _, err := exec.ExecContext(ctx,
		`UPDATE ci_pr_panels SET synthesis_job_id = ? WHERE panel_run_uuid = ?`, syn.ID, runUUID); err != nil {
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
	res, err := db.Exec(`
		UPDATE ci_pr_panels SET posting_claimed_at = datetime('now')
		 WHERE id = ? AND posted_at IS NULL
		   AND retired_at IS NULL
		   AND (posting_claimed_at IS NULL
		        OR datetime(posting_claimed_at) < datetime('now', ?))`, id, staleArg)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// ReleasePanelPostClaim clears an unposted row's lease so it can be reclaimed,
// e.g. when a poster fails before posting and wants another to retry.
func (db *DB) ReleasePanelPostClaim(id int64) error {
	_, err := db.Exec(`UPDATE ci_pr_panels SET posting_claimed_at = NULL WHERE id = ? AND posted_at IS NULL AND retired_at IS NULL`, id)
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
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("mark panel posted: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.Exec(`
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
		WHERE id = ? AND posted_at IS NULL AND retired_at IS NULL`, outcome, id)
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

	now := time.Now().Format(time.RFC3339)
	if _, err := tx.Exec(`
		UPDATE ci_pr_review_attempts
		SET state = 'done', next_attempt_at = NULL, updated_at = ?
		WHERE (github_repo, pr_number, head_sha) = (
		    SELECT github_repo, pr_number, head_sha FROM ci_pr_panels WHERE id = ?)`,
		now, id); err != nil {
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
	_, err := db.Exec(`
		UPDATE ci_pr_panels
		SET retired_at = datetime('now'), posting_claimed_at = NULL
		WHERE id = ? AND posted_at IS NULL AND retired_at IS NULL`, id)
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
	res, err := db.Exec(`
		UPDATE ci_pr_panels
		SET retired_at = datetime('now'), posting_claimed_at = NULL
		WHERE id = ? AND posted_at IS NULL AND retired_at IS NULL
		  AND allow_stale_post = 0`, id)
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
	res, err := db.Exec(`
		UPDATE ci_pr_panels
		SET allow_stale_post = 1
		WHERE github_repo = ? AND pr_number = ? AND head_sha != ?
		  AND posted_at IS NULL AND retired_at IS NULL
		  AND allow_stale_post = 0`,
		githubRepo, prNumber, newHeadSHA)
	if err != nil {
		return 0, fmt.Errorf("mark panels allow_stale_post: %w", err)
	}
	return res.RowsAffected()
}

// GetActivePanelsForPR returns the un-posted, non-retired panel runs for a
// (github_repo, pr_number). Used by the supersede and closed-PR cleanup sweeps
// to find every still-active run for a PR (across HEAD SHAs).
func (db *DB) GetActivePanelsForPR(githubRepo string, prNumber int) ([]CIPanel, error) {
	rows, err := db.Query(`SELECT `+ciPanelColumns+`
		FROM ci_pr_panels
		WHERE github_repo = ? AND pr_number = ? AND posted_at IS NULL AND retired_at IS NULL`,
		githubRepo, prNumber)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var panels []CIPanel
	for rows.Next() {
		panel, err := scanCIPanel(rows)
		if err != nil {
			return nil, err
		}
		panels = append(panels, *panel)
	}
	return panels, rows.Err()
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
	rows, err := db.Query(`SELECT `+ciPanelColumns+`
		FROM ci_pr_panels
		WHERE github_repo = ? AND posted_at IS NULL
		  AND retired_at IS NULL
		  AND EXISTS (
		      SELECT 1 FROM review_jobs j
		      WHERE j.panel_run_uuid = ci_pr_panels.panel_run_uuid
		        AND j.panel_role = 'member'
		        AND j.status = 'running'
		        AND j.started_at IS NOT NULL
		        AND datetime(j.started_at) < datetime('now', ?)
		  )`,
		githubRepo, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var panels []CIPanel
	for rows.Next() {
		panel, err := scanCIPanel(rows)
		if err != nil {
			return nil, err
		}
		panels = append(panels, *panel)
	}
	return panels, rows.Err()
}

// GetUnpostedTerminalPanels returns panel rows whose synthesis job is terminal
// (done or failed) but that were never posted — the dropped-event / crash
// recovery set for the spec §10 posting reconcile.
func (db *DB) GetUnpostedTerminalPanels(githubRepo string) ([]CIPanel, error) {
	rows, err := db.Query(`
		SELECT `+ciPanelColumns+`
		FROM ci_pr_panels
		WHERE github_repo = ? AND posted_at IS NULL AND retired_at IS NULL
		  AND EXISTS (
		      SELECT 1 FROM review_jobs s
		      WHERE s.id = ci_pr_panels.synthesis_job_id
		        AND s.status IN ('done', 'failed'))`, githubRepo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var panels []CIPanel
	for rows.Next() {
		panel, err := scanCIPanel(rows)
		if err != nil {
			return nil, err
		}
		panels = append(panels, *panel)
	}
	return panels, rows.Err()
}

// GetPendingPanelPRs returns the distinct (github_repo, pr_number) pairs that
// have an un-posted panel run, so the poll loop can check whether those PRs are
// still open (closed-PR cleanup). Mirrors GetPendingBatchPRs. F13.
func (db *DB) GetPendingPanelPRs(githubRepo string) ([]PanelPRRef, error) {
	rows, err := db.Query(`
		SELECT DISTINCT github_repo, pr_number
		FROM ci_pr_panels
		WHERE github_repo = ? AND posted_at IS NULL AND retired_at IS NULL`, githubRepo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var refs []PanelPRRef
	for rows.Next() {
		var ref PanelPRRef
		if err := rows.Scan(&ref.GithubRepo, &ref.PRNumber); err != nil {
			return nil, err
		}
		refs = append(refs, ref)
	}
	return refs, rows.Err()
}

// LatestPanelTimeForPR returns the created_at of the most recent panel run for
// a PR (any HEAD SHA), or the zero time when no run exists. It is the
// panel-based successor to LatestBatchTimeForPR, used by the CI poller's
// throttle check. Timestamps are parsed with parseSQLiteTime, matching the
// other ci_pr_panels scanners.
func (db *DB) LatestPanelTimeForPR(githubRepo string, prNumber int) (time.Time, error) {
	var createdAt sql.NullString
	err := db.QueryRow(`SELECT created_at FROM ci_pr_panels
		WHERE github_repo = ? AND pr_number = ?
		ORDER BY datetime(created_at) DESC LIMIT 1`, githubRepo, prNumber).Scan(&createdAt)
	if err == sql.ErrNoRows {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	if !createdAt.Valid {
		return time.Time{}, nil
	}
	return parseSQLiteTime(createdAt.String), nil
}

// DeleteCIPanel removes a single ci_pr_panels mapping row (supersede/cleanup),
// preserving CI ownership on source-less historical jobs before the mapping is
// removed.
func (db *DB) DeleteCIPanel(id int64) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`UPDATE review_jobs
		SET source = ?
		WHERE COALESCE(source, '') = ''
		  AND panel_run_uuid = (SELECT panel_run_uuid FROM ci_pr_panels WHERE id = ?)`,
		JobSourceCI, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM ci_pr_panels WHERE id = ?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteCIPanelByRun removes the mapping row for a panel run uuid, preserving
// CI ownership on source-less historical jobs before the mapping is removed.
func (db *DB) DeleteCIPanelByRun(panelRunUUID string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`UPDATE review_jobs
		SET source = ?
		WHERE COALESCE(source, '') = '' AND panel_run_uuid = ?
		  AND EXISTS (SELECT 1 FROM ci_pr_panels WHERE panel_run_uuid = ?)`,
		JobSourceCI, panelRunUUID, panelRunUUID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM ci_pr_panels WHERE panel_run_uuid = ?`, panelRunUUID); err != nil {
		return err
	}
	return tx.Commit()
}
