package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"slices"
	"time"
	"uuid"
)

// CIBranchCursor is the newest default-branch commit the CI poller has
// accounted for in one repository. Commits after it still need a review.
type CIBranchCursor struct {
	GithubRepo string
	Branch     string
	HeadSHA    string
}

// CIBranchReview maps one landed default-branch commit to the panel run that
// reviews it and to the commit status that reports the result. A commit that
// could not be reviewed has no run and carries its final status directly.
type CIBranchReview struct {
	ID               int64
	GithubRepo       string
	Branch           string
	SHA              string
	PanelRunUUID     *uuid.UUID
	SynthesisJobID   *int64
	FinalState       string
	FinalDescription string
}

func migrateCIBranchReviews(ctx context.Context, db *DB) error {
	for _, statement := range []string{
		`CREATE TABLE IF NOT EXISTS ci_branch_cursors (
  github_repo TEXT PRIMARY KEY,
  branch TEXT NOT NULL,
  head_sha TEXT NOT NULL,
  updated_at TEXT NOT NULL
)`,
		`CREATE TABLE IF NOT EXISTS ci_branch_reviews (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  github_repo TEXT NOT NULL,
  branch TEXT NOT NULL,
  sha TEXT NOT NULL,
  panel_run_uuid TEXT,
  synthesis_job_id INTEGER,
  final_state TEXT NOT NULL DEFAULT '',
  final_description TEXT NOT NULL DEFAULT '',
  status_claimed_at TEXT,
  status_sent_at TEXT,
  created_at TEXT NOT NULL,
  UNIQUE(github_repo, sha)
)`,
		`CREATE INDEX IF NOT EXISTS idx_ci_branch_reviews_synthesis ON ci_branch_reviews(synthesis_job_id)`,
		// Every synthesis job that ever reviewed a commit, so an explicit rerun
		// of an earlier run still finds its commit after a newer rerun.
		`CREATE TABLE IF NOT EXISTS ci_branch_review_runs (
  synthesis_job_id INTEGER PRIMARY KEY,
  review_id INTEGER NOT NULL REFERENCES ci_branch_reviews(id)
)`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}

// GetCIBranchCursor returns nil when the repository has no cursor yet.
func (db *DB) GetCIBranchCursor(ctx context.Context, githubRepo string) (*CIBranchCursor, error) {
	cursor := CIBranchCursor{GithubRepo: githubRepo}
	err := db.QueryRowContext(ctx, `SELECT branch, head_sha FROM ci_branch_cursors WHERE github_repo = ?`,
		githubRepo).Scan(&cursor.Branch, &cursor.HeadSHA)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &cursor, nil
}

// SetCIBranchCursor records that every commit up to headSHA is accounted for,
// without reviewing anything. The poller uses it for the starting point, after
// a history rewrite, and for commits that reviews are configured to skip.
func (db *DB) SetCIBranchCursor(ctx context.Context, githubRepo, branch, headSHA string) error {
	return setCIBranchCursor(ctx, db, githubRepo, branch, headSHA, time.Now())
}

// DeleteCIBranchCursor forgets a repository's review position, so the next
// enabled poll starts from the branch head again.
func (db *DB) DeleteCIBranchCursor(ctx context.Context, githubRepo string) error {
	_, err := db.ExecContext(ctx, `DELETE FROM ci_branch_cursors WHERE github_repo = ?`, githubRepo)
	return err
}

// DeleteCIBranchCursorsExcept forgets the review position of every repository
// not in polled, so a repository that returns to the poller starts from its
// branch head instead of reviewing what landed while it was not polled.
func (db *DB) DeleteCIBranchCursorsExcept(ctx context.Context, polled []string) error {
	rows, err := db.QueryContext(ctx, `SELECT github_repo FROM ci_branch_cursors`)
	if err != nil {
		return err
	}
	defer rows.Close()
	var dropped []string
	for rows.Next() {
		var repo string
		if err := rows.Scan(&repo); err != nil {
			return err
		}
		if !slices.Contains(polled, repo) {
			dropped = append(dropped, repo)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, repo := range dropped {
		if err := db.DeleteCIBranchCursor(ctx, repo); err != nil {
			return err
		}
	}
	return nil
}

func setCIBranchCursor(ctx context.Context, exec execer, githubRepo, branch, headSHA string, now time.Time) error {
	_, err := exec.ExecContext(ctx, `INSERT INTO ci_branch_cursors (github_repo, branch, head_sha, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(github_repo) DO UPDATE SET branch = excluded.branch,
		  head_sha = excluded.head_sha, updated_at = excluded.updated_at`,
		githubRepo, branch, headSHA, now.UTC().Format(time.RFC3339))
	return err
}

// CreateCIBranchReviewRun enqueues the panel run for one landed commit and
// advances the repository cursor to that commit in the same transaction, so a
// crash cannot review a commit twice or skip it. created is false when the
// commit already has a review; the cursor still advances.
func (db *DB) CreateCIBranchReviewRun(ctx context.Context, githubRepo, branch, sha string,
	members []EnqueueOpts, synthesis EnqueueOpts,
) (bool, *ReviewJob, error) {
	machineID, _ := db.GetMachineID() // writes on a pooled conn; must precede BEGIN
	now := time.Now()
	conn, err := db.Conn(ctx)
	if err != nil {
		return false, nil, err
	}
	defer conn.Close()
	if err := beginImmediate(ctx, conn); err != nil {
		return false, nil, err
	}
	committed := false
	defer func() {
		if !committed {
			if err := rollbackConn(conn); err != nil {
				log.Printf("CreateCIBranchReviewRun: rollback failed: %v", err)
			}
		}
	}()

	runUUID := uuid.New()
	res, err := conn.ExecContext(ctx, `INSERT OR IGNORE INTO ci_branch_reviews
		(github_repo, branch, sha, panel_run_uuid, created_at) VALUES (?, ?, ?, ?, ?)`,
		githubRepo, branch, sha, runUUID, now.UTC().Format(time.RFC3339))
	if err != nil {
		return false, nil, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, nil, err
	}
	var members2 []*ReviewJob
	var synth *ReviewJob
	if n == 1 {
		for i := range members {
			members[i].PanelRunUUID = &runUUID
			members[i].Source = JobSourceCIBranch
		}
		synthesis.PanelRunUUID = &runUUID
		synthesis.Source = JobSourceCIBranch
		members2, synth, err = db.enqueuePanelRunTx(ctx, conn, members, synthesis, machineID, now)
		if err != nil {
			return false, nil, err
		}
		if _, err := conn.ExecContext(ctx, `UPDATE ci_branch_reviews SET synthesis_job_id = ?
			WHERE panel_run_uuid = ?`, synth.ID, runUUID); err != nil {
			return false, nil, err
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO ci_branch_review_runs (synthesis_job_id, review_id)
			SELECT ?, id FROM ci_branch_reviews WHERE panel_run_uuid = ?`, synth.ID, runUUID); err != nil {
			return false, nil, err
		}
	}
	if err := setCIBranchCursor(ctx, conn, githubRepo, branch, sha, now); err != nil {
		return false, nil, err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return false, nil, err
	}
	committed = true
	if n == 0 {
		return false, nil, nil
	}
	if err := db.attachPanelExperimentAssignments(members2, synth); err != nil {
		return false, nil, err
	}
	return true, synth, nil
}

// RecordCIBranchCommitOutcome accounts for a landed commit that cannot be
// reviewed, such as one whose review configuration is invalid. The commit
// receives its final status and the cursor moves past it.
func (db *DB) RecordCIBranchCommitOutcome(ctx context.Context, githubRepo, branch, sha, state, description string) error {
	now := time.Now()
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := beginImmediate(ctx, conn); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			if err := rollbackConn(conn); err != nil {
				log.Printf("RecordCIBranchCommitOutcome: rollback failed: %v", err)
			}
		}
	}()
	if _, err := conn.ExecContext(ctx, `INSERT OR IGNORE INTO ci_branch_reviews
		(github_repo, branch, sha, final_state, final_description, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		githubRepo, branch, sha, state, description, now.UTC().Format(time.RFC3339)); err != nil {
		return err
	}
	if err := setCIBranchCursor(ctx, conn, githubRepo, branch, sha, now); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return err
	}
	committed = true
	return nil
}

const ciBranchReviewColumns = `id, github_repo, branch, sha, panel_run_uuid, synthesis_job_id, final_state, final_description`

func scanCIBranchReview(row interface{ Scan(...any) error }) (CIBranchReview, error) {
	var review CIBranchReview
	var run sql.Null[uuid.UUID]
	var synth sql.NullInt64
	if err := row.Scan(&review.ID, &review.GithubRepo, &review.Branch, &review.SHA, &run, &synth,
		&review.FinalState, &review.FinalDescription); err != nil {
		return CIBranchReview{}, err
	}
	if run.Valid {
		review.PanelRunUUID = &run.V
	}
	if synth.Valid {
		review.SynthesisJobID = &synth.Int64
	}
	return review, nil
}

// GetCIBranchReviewBySynthesisJobID returns sql.ErrNoRows for jobs that do not
// review a landed default-branch commit.
func (db *DB) GetCIBranchReviewBySynthesisJobID(ctx context.Context, jobID int64) (*CIBranchReview, error) {
	review, err := scanCIBranchReview(db.QueryRowContext(ctx, `SELECT `+ciBranchReviewColumns+`
		FROM ci_branch_reviews WHERE synthesis_job_id = ?`, jobID))
	if err != nil {
		return nil, err
	}
	return &review, nil
}

// GetCIBranchReview returns sql.ErrNoRows when the commit has no review row.
func (db *DB) GetCIBranchReview(ctx context.Context, githubRepo, sha string) (*CIBranchReview, error) {
	review, err := scanCIBranchReview(db.QueryRowContext(ctx, `SELECT `+ciBranchReviewColumns+`
		FROM ci_branch_reviews WHERE github_repo = ? AND sha = ?`, githubRepo, sha))
	if err != nil {
		return nil, err
	}
	return &review, nil
}

// SetCIBranchReviewFinalStatus records the result of review's run to publish
// once. A later call for the same run leaves the first result unchanged, and
// a run that a rerun has replaced records nothing.
func (db *DB) SetCIBranchReviewFinalStatus(ctx context.Context, review CIBranchReview, state, description string) error {
	_, err := db.ExecContext(ctx, `UPDATE ci_branch_reviews SET final_state = ?, final_description = ?
		WHERE id = ? AND synthesis_job_id IS ? AND final_state = ''`,
		state, description, review.ID, nullableJobID(review.SynthesisJobID))
	return err
}

// ErrCIBranchReviewReplaced means a rerun replaced the run whose result was
// being published, so the publication no longer belongs to it.
var ErrCIBranchReviewReplaced = errors.New("default-branch review run was replaced by a rerun")

func nullableJobID(id *int64) sql.NullInt64 {
	if id == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: *id, Valid: true}
}

// ListCIBranchReviewsAwaitingResult returns reviews whose synthesis job has
// finished but whose result has not been recorded, for example because the
// completion event was lost in a restart.
func (db *DB) ListCIBranchReviewsAwaitingResult(ctx context.Context, githubRepo string) ([]CIBranchReview, error) {
	return db.listCIBranchReviews(ctx, `SELECT b.id, b.github_repo, b.branch, b.sha, b.panel_run_uuid,
		b.synthesis_job_id, b.final_state, b.final_description
		FROM ci_branch_reviews b JOIN review_jobs j ON j.id = b.synthesis_job_id
		WHERE b.github_repo = ? AND b.final_state = '' AND j.status IN ('done', 'failed', 'canceled')
		ORDER BY b.id`, githubRepo)
}

// ListCIBranchReviewStatuses returns recorded results whose publication is
// still open: neither published nor given up on.
func (db *DB) ListCIBranchReviewStatuses(ctx context.Context, githubRepo string) ([]CIBranchReview, error) {
	return db.listCIBranchReviews(ctx, `SELECT `+ciBranchReviewColumns+` FROM ci_branch_reviews
		WHERE github_repo = ? AND final_state != '' AND status_sent_at IS NULL ORDER BY id`, githubRepo)
}

func (db *DB) listCIBranchReviews(ctx context.Context, query string, args ...any) ([]CIBranchReview, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var reviews []CIBranchReview
	for rows.Next() {
		review, err := scanCIBranchReview(rows)
		if err != nil {
			return nil, err
		}
		reviews = append(reviews, review)
	}
	return reviews, rows.Err()
}

// ClaimCIBranchReviewStatus leases review's unpublished result to one
// publisher. A crashed publisher's lease becomes claimable after staleWindow.
// The claim fails when a rerun has replaced the run that produced the result.
func (db *DB) ClaimCIBranchReviewStatus(ctx context.Context, review CIBranchReview, staleWindow time.Duration) (bool, error) {
	res, err := db.ExecContext(ctx, `UPDATE ci_branch_reviews SET status_claimed_at = datetime('now')
		WHERE id = ? AND synthesis_job_id IS ? AND final_state != '' AND status_sent_at IS NULL
		  AND (status_claimed_at IS NULL OR datetime(status_claimed_at) < datetime('now', ?))`,
		review.ID, nullableJobID(review.SynthesisJobID), fmt.Sprintf("-%d seconds", int64(staleWindow.Seconds())))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// FinishCIBranchReviewStatus releases the publication lease. settled closes
// the publication: the status was published, or GitHub will never accept it,
// so status_sent_at also marks a publication that was given up on. An unsettled
// result stays listed for another attempt. It returns
// ErrCIBranchReviewReplaced when a rerun took over the review after its lease
// went stale, leaving the rerun's publication untouched.
func (db *DB) FinishCIBranchReviewStatus(ctx context.Context, review CIBranchReview, settled bool) error {
	query := `UPDATE ci_branch_reviews SET status_claimed_at = NULL
		WHERE id = ? AND synthesis_job_id IS ? AND status_sent_at IS NULL`
	if settled {
		query = `UPDATE ci_branch_reviews SET status_sent_at = datetime('now'), status_claimed_at = NULL
		WHERE id = ? AND synthesis_job_id IS ? AND status_sent_at IS NULL`
	}
	res, err := db.ExecContext(ctx, query, review.ID, nullableJobID(review.SynthesisJobID))
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrCIBranchReviewReplaced
	}
	return nil
}

// GetTimedOutCIBranchReviewRuns returns the panel runs of unfinished
// default-branch reviews with a member running longer than maxAge, for the
// same batch-timeout handling as pull request panels.
func (db *DB) GetTimedOutCIBranchReviewRuns(ctx context.Context, githubRepo string, maxAge time.Duration) ([]uuid.UUID, error) {
	rows, err := db.QueryContext(ctx, `SELECT b.panel_run_uuid FROM ci_branch_reviews b
		WHERE b.github_repo = ? AND b.final_state = '' AND b.panel_run_uuid IS NOT NULL
		  AND EXISTS (
		      SELECT 1 FROM review_jobs j
		      WHERE j.panel_run_uuid = b.panel_run_uuid
		        AND j.panel_role = 'member'
		        AND j.status = 'running'
		        AND j.started_at IS NOT NULL
		        AND datetime(j.started_at) < datetime('now', ?)
		  )
		ORDER BY b.id`, githubRepo, fmt.Sprintf("-%d seconds", int64(maxAge.Seconds())))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var runs []uuid.UUID
	for rows.Next() {
		var run uuid.UUID
		if err := rows.Scan(&run); err != nil {
			return nil, err
		}
		runs = append(runs, run)
	}
	return runs, rows.Err()
}

// rerunCIBranchReviewTx moves a default-branch review's status publication to
// an explicit rerun of any of its earlier runs, in the same transaction as the
// new jobs, so the rerun's result replaces the published one. found is false
// when the source job does not belong to a default-branch review.
//
// Like a pull request panel, the review stays with its current run while that
// run's result is unpublished: only a rerun of the current run itself can take
// over, and never while its result is being published.
func rerunCIBranchReviewTx(ctx context.Context, conn *sql.Conn, sourceJobID int64, synth *ReviewJob, now time.Time) (bool, error) {
	var id int64
	var current sql.NullInt64
	var claimedAt, sentAt sql.NullString
	err := conn.QueryRowContext(ctx, `SELECT b.id, b.synthesis_job_id, b.status_claimed_at, b.status_sent_at
		FROM ci_branch_review_runs r JOIN ci_branch_reviews b ON b.id = r.review_id
		WHERE r.synthesis_job_id = ?`, sourceJobID).Scan(&id, &current, &claimedAt, &sentAt)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !sentAt.Valid {
		if claimedAt.Valid && !parseSQLiteTime(claimedAt.String).Before(now.Add(-PanelPostingStaleWindow)) {
			return true, ErrCIPanelActive
		}
		if !current.Valid || current.Int64 != sourceJobID {
			return true, ErrCIPanelActive
		}
	}
	if _, err := conn.ExecContext(ctx, `UPDATE ci_branch_reviews
		SET panel_run_uuid = ?, synthesis_job_id = ?, final_state = '', final_description = '',
		    status_claimed_at = NULL, status_sent_at = NULL
		WHERE id = ?`, synth.PanelRunUUID, synth.ID, id); err != nil {
		return true, err
	}
	_, err = conn.ExecContext(ctx, `INSERT INTO ci_branch_review_runs (synthesis_job_id, review_id)
		VALUES (?, ?)`, synth.ID, id)
	return true, err
}
