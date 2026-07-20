package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/uptrace/bun"

	"go.kenn.io/roborev/internal/agent"
)

// GetReviewByJobID finds a review by its job ID
func (db *DB) GetReviewByJobID(jobID int64) (*Review, error) {
	ctx := context.Background()
	tx, err := db.bun.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	var reviewDB reviewRow
	if err := db.bun.NewSelect().
		Model(&reviewDB).
		Conn(tx).
		Column(
			"id", "job_id", "agent", "prompt", "output", "created_at", "closed",
			"uuid", "updated_at", "updated_by_machine_id", "synced_at", "verdict_bool",
		).
		Where("job_id = ?", jobID).
		Scan(ctx); err != nil {
		return nil, err
	}
	review := reviewDB.toModel()

	var jobDB jobHydrationRow
	query := db.bun.NewSelect().
		Conn(tx).
		TableExpr("review_jobs AS j").
		Join("JOIN repos AS rp ON rp.id = j.repo_id").
		Join("LEFT JOIN commits AS c ON c.id = j.commit_id").
		Where("j.id = ?", jobID)
	query = addJobSelectColumns(query, sqliteReviewJobColumns)
	if err := query.Scan(ctx, &jobDB); err != nil {
		return nil, err
	}
	job := jobDB.toModel()
	var verdict sql.NullInt64
	if review.VerdictBool != nil {
		verdict = sql.NullInt64{Int64: int64(*review.VerdictBool), Valid: true}
	}
	applyJobVerdict(&job, verdict, review.Output)

	review.Job = &job
	if err := tx.Commit(); err != nil {
		return nil, err
	}

	return &review, nil
}

// GetReviewByCommitSHA finds the review for a commit SHA (searches git_ref
// field). It first resolves the latest review-producing JOB for the ref (newest
// enqueued first), then returns that job's review. The first query is restricted
// to canonical SHA-review job types (review/range/dirty/synthesis/compact) so a
// newer job that merely inherits the ref but produces no SHA review — a fix or
// task job (fix jobs copy the parent's git_ref) — cannot shadow a real review.
// Panel member jobs are also excluded so SHA resolution lands on the synthesis
// (canonical) job, never an individual reviewer — members are reached explicitly
// by job id (GetReviewByJobID). When the latest qualifying job has no review row
// yet (e.g. a queued/running/failed synthesis), this returns sql.ErrNoRows — the
// "no review yet" signal callers already handle — instead of a stale older
// review.
func (db *DB) GetReviewByCommitSHA(sha string) (*Review, error) {
	var jobID int64
	err := db.bun.NewSelect().
		TableExpr("review_jobs AS j").
		ColumnExpr("j.id").
		Where("j.git_ref = ?", sha).
		Where("j.job_type IN ('review','range','dirty','synthesis','compact')").
		Where("COALESCE(j.panel_role, '') != 'member'").
		OrderExpr("j.enqueued_at DESC, j.id DESC").
		Limit(1).
		Scan(context.Background(), &jobID)
	if err != nil {
		return nil, err
	}
	return db.GetReviewByJobID(jobID)
}

// GetAllReviewsForGitRef returns all reviews for a git ref (commit SHA or range) for re-review context
func (db *DB) GetAllReviewsForGitRef(gitRef string) ([]Review, error) {
	var rows []reviewRow
	if err := db.bun.NewSelect().
		Model(&rows).
		Column("rv.id", "rv.job_id", "rv.agent", "rv.prompt", "rv.output", "rv.created_at", "rv.closed").
		Join("JOIN review_jobs AS j ON j.id = rv.job_id").
		Where("j.git_ref = ?", gitRef).
		Where("COALESCE(j.panel_role, '') != 'member'").
		OrderExpr("rv.created_at ASC").
		Scan(context.Background()); err != nil {
		return nil, err
	}

	var reviews []Review
	for _, row := range rows {
		reviews = append(reviews, row.toModel())
	}
	return reviews, nil
}

// GetRecentReviewsForRepo returns the N most recent reviews for a repo
func (db *DB) GetRecentReviewsForRepo(repoID int64, limit int) ([]Review, error) {
	if limit == 0 {
		return nil, nil
	}
	var rows []reviewRow
	if err := db.bun.NewSelect().
		Model(&rows).
		Column("rv.id", "rv.job_id", "rv.agent", "rv.prompt", "rv.output", "rv.created_at", "rv.closed").
		Join("JOIN review_jobs AS j ON j.id = rv.job_id").
		Where("j.repo_id = ?", repoID).
		OrderExpr("rv.created_at DESC").
		Limit(limit).
		Scan(context.Background()); err != nil {
		return nil, err
	}

	var reviews []Review
	for _, row := range rows {
		reviews = append(reviews, row.toModel())
	}
	return reviews, nil
}

func (db *DB) CountReviews() (int, error) {
	return db.bun.NewSelect().Model((*reviewRow)(nil)).Count(context.Background())
}

// FindReusableSessionCandidates returns recent completed jobs with reusable
// sessions for the same repo, branch, agent, and review type, newest first.
func (db *DB) FindReusableSessionCandidates(
	repoID int64, branch, agent, reviewType, worktreePath string, limit int,
) ([]ReviewJob, error) {
	if repoID == 0 || branch == "" || agent == "" {
		return nil, nil
	}
	if reviewType == "" {
		reviewType = "default"
	}
	if limit <= 0 {
		jobs, _, err := db.scanReusableSessionCandidates(
			repoID, branch, agent, reviewType, worktreePath, 0, 0, 0,
		)
		return jobs, err
	}

	batchSize := max(limit*2, 20)

	var jobs []ReviewJob
	for offset := 0; len(jobs) < limit; offset += batchSize {
		batch, scanned, err := db.scanReusableSessionCandidates(
			repoID, branch, agent, reviewType, worktreePath,
			batchSize, offset, limit-len(jobs),
		)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, batch...)
		if scanned < batchSize {
			break
		}
	}
	return jobs, nil
}

// FindReusableSessionCandidate returns the newest reusable session candidate.
func (db *DB) FindReusableSessionCandidate(
	repoID int64, branch, agent, reviewType, worktreePath string,
) (*ReviewJob, error) {
	jobs, err := db.FindReusableSessionCandidates(repoID, branch, agent, reviewType, worktreePath, 1)
	if err != nil {
		return nil, err
	}
	if len(jobs) == 0 {
		return nil, nil
	}
	return &jobs[0], nil
}

func (db *DB) scanReusableSessionCandidates(
	repoID int64,
	branch string,
	agentName string,
	reviewType string,
	worktreePath string,
	limit int,
	offset int,
	remaining int,
) ([]ReviewJob, int, error) {
	var rows []struct {
		ID        int64   `bun:"id"`
		GitRef    string  `bun:"git_ref"`
		SessionID *string `bun:"session_id"`
		CommitSHA string  `bun:"commit_sha"`
	}
	query := db.bun.NewSelect().
		TableExpr("review_jobs AS j").
		ColumnExpr("j.id").
		ColumnExpr("j.git_ref").
		ColumnExpr("j.session_id").
		ColumnExpr("COALESCE(c.sha, '') AS commit_sha").
		Join("LEFT JOIN commits AS c ON c.id = j.commit_id").
		Where("j.repo_id = ?", repoID).
		Where("j.branch = ?", branch).
		Where("j.agent = ?", agentName).
		Where("j.status = ?", JobStatusDone).
		Where("COALESCE(NULLIF(j.job_type, ''), 'review') IN ('review', 'range', 'dirty')").
		Where("COALESCE(j.panel_role, '') = ''").
		Where("j.session_id IS NOT NULL").
		Where("j.session_id <> ''").
		Where("COALESCE(NULLIF(j.review_type, ''), 'default') = ?", reviewType).
		Where("COALESCE(j.worktree_path, '') = ?", worktreePath).
		OrderExpr("julianday(COALESCE(j.finished_at, j.updated_at, j.enqueued_at)) DESC, j.id DESC")
	if limit > 0 {
		query = query.Limit(limit).Offset(offset)
	}
	if err := query.Scan(context.Background(), &rows); err != nil {
		return nil, 0, err
	}

	var jobs []ReviewJob
	for _, row := range rows {
		target := reusableSessionCandidateTarget(row.GitRef, row.CommitSHA)
		if row.SessionID == nil || !agent.IsValidResumeSessionID(*row.SessionID) || target == "" {
			continue
		}
		job := ReviewJob{
			ID:                    row.ID,
			GitRef:                row.GitRef,
			SessionID:             *row.SessionID,
			ReusableSessionTarget: target,
		}
		jobs = append(jobs, job)
		if remaining > 0 && len(jobs) >= remaining {
			break
		}
	}
	return jobs, len(rows), nil
}

func reusableSessionCandidateTarget(gitRef, commitSHA string) string {
	gitRef = strings.TrimSpace(gitRef)
	if gitRef == "" {
		return ""
	}
	if gitRef == "dirty" {
		return strings.TrimSpace(commitSHA)
	}
	if strings.Contains(gitRef, "..") {
		parts := strings.SplitN(gitRef, "..", 2)
		return strings.TrimSpace(parts[1])
	}
	return gitRef
}

// MarkReviewClosed marks a review as closed (or reopened) by review ID
func (db *DB) MarkReviewClosed(reviewID int64, closed bool) error {
	now := dbTimeFromValue(time.Now())
	machineID, _ := db.GetMachineID()

	result, err := db.bun.NewUpdate().
		Model((*reviewRow)(nil)).
		Set("closed = ?", closed).
		Set("updated_by_machine_id = ?", machineID).
		Set("updated_at = ?", now).
		Where("id = ?", reviewID).
		Exec(context.Background())
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// MarkReviewClosedByJobID marks a review as closed (or reopened) by job ID
func (db *DB) MarkReviewClosedByJobID(jobID int64, closed bool) error {
	now := dbTimeFromValue(time.Now())
	machineID, _ := db.GetMachineID()

	result, err := db.bun.NewUpdate().
		Model((*reviewRow)(nil)).
		Set("closed = ?", closed).
		Set("updated_by_machine_id = ?", machineID).
		Set("updated_at = ?", now).
		Where("job_id = ?", jobID).
		Exec(context.Background())
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// GetJobsWithReviewsByIDs fetches jobs and their reviews in batch for the given job IDs.
// Returns a map of job ID to JobWithReview. Jobs without reviews are included with a nil Review.
func (db *DB) GetJobsWithReviewsByIDs(jobIDs []int64) (map[int64]JobWithReview, error) {
	if len(jobIDs) == 0 {
		return nil, nil
	}

	// Fetch jobs
	var jobRows []jobHydrationRow
	jobQuery := db.bun.NewSelect().
		TableExpr("review_jobs AS j").
		Join("JOIN repos AS r ON r.id = j.repo_id").
		Join("LEFT JOIN commits AS c ON c.id = j.commit_id").
		Where("j.id IN (?)", bun.List(jobIDs))
	jobQuery = addJobSelectColumns(jobQuery, sqliteBatchReviewJobColumns)
	if err := jobQuery.Scan(context.Background(), &jobRows); err != nil {
		return nil, fmt.Errorf("query jobs: %w", err)
	}

	result := make(map[int64]JobWithReview, len(jobIDs))
	for _, row := range jobRows {
		job := row.toModel()
		result[job.ID] = JobWithReview{Job: job}
	}

	// Fetch reviews for these jobs
	var reviewRows []reviewRow
	if err := db.bun.NewSelect().
		Model(&reviewRows).
		Column("id", "job_id", "agent", "prompt", "output", "created_at", "closed", "verdict_bool").
		Where("job_id IN (?)", bun.List(jobIDs)).
		Scan(context.Background()); err != nil {
		return nil, fmt.Errorf("query reviews: %w", err)
	}

	for _, row := range reviewRows {
		review := row.toModel()
		if entry, ok := result[review.JobID]; ok {
			entry.Review = &review
			var verdict sql.NullInt64
			if review.VerdictBool != nil {
				verdict = sql.NullInt64{Int64: int64(*review.VerdictBool), Valid: true}
			}
			applyJobVerdict(&entry.Job, verdict, review.Output)
			result[review.JobID] = entry
		}
	}

	return result, nil
}

// GetReviewByID finds a review by its ID
func (db *DB) GetReviewByID(reviewID int64) (*Review, error) {
	var row reviewRow
	if err := db.bun.NewSelect().
		Model(&row).
		Column("id", "job_id", "agent", "prompt", "output", "created_at", "closed").
		Where("id = ?", reviewID).
		Scan(context.Background()); err != nil {
		return nil, err
	}
	review := row.toModel()
	return &review, nil
}

// AddComment adds a comment to a commit (legacy - use AddCommentToJob for new code)
func (db *DB) AddComment(commitID int64, responder, response string) (*Response, error) {
	uuid := GenerateUUID()
	machineID, _ := db.GetMachineID()
	now := time.Now()
	row := responseRow{
		CommitID:        &commitID,
		Responder:       responder,
		Response:        response,
		UUID:            &uuid,
		SourceMachineID: optionalString(machineID),
		CreatedAt:       dbTimeFromValue(now),
	}
	result, err := db.bun.NewInsert().
		Model(&row).
		Column("commit_id", "responder", "response", "uuid", "source_machine_id", "created_at").
		Exec(context.Background())
	if err != nil {
		return nil, err
	}

	id, _ := result.LastInsertId()
	return &Response{
		ID:              id,
		CommitID:        &commitID,
		Responder:       responder,
		Response:        response,
		CreatedAt:       now,
		UUID:            uuid,
		SourceMachineID: machineID,
	}, nil
}

// AddCommentToJob adds a comment linked to a job/review
func (db *DB) AddCommentToJob(jobID int64, responder, response string) (*Response, error) {
	// Verify job exists first to return proper 404 instead of FK violation or orphaned row
	var exists int
	err := db.bun.NewSelect().
		Table("review_jobs").
		ColumnExpr("1").
		Where("id = ?", jobID).
		Scan(context.Background(), &exists)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, sql.ErrNoRows // Job not found
		}
		return nil, err
	}

	uuid := GenerateUUID()
	machineID, _ := db.GetMachineID()
	now := time.Now()
	row := responseRow{
		JobID:           &jobID,
		Responder:       responder,
		Response:        response,
		UUID:            &uuid,
		SourceMachineID: optionalString(machineID),
		CreatedAt:       dbTimeFromValue(now),
	}
	result, err := db.bun.NewInsert().
		Model(&row).
		Column("job_id", "responder", "response", "uuid", "source_machine_id", "created_at").
		Exec(context.Background())
	if err != nil {
		return nil, err
	}

	id, _ := result.LastInsertId()
	return &Response{
		ID:              id,
		JobID:           &jobID,
		Responder:       responder,
		Response:        response,
		CreatedAt:       now,
		UUID:            uuid,
		SourceMachineID: machineID,
	}, nil
}

// GetCommentsForCommit returns all comments for a commit
func (db *DB) GetCommentsForCommit(commitID int64) ([]Response, error) {
	var rows []responseRow
	if err := db.bun.NewSelect().
		Model(&rows).
		Column("id", "commit_id", "job_id", "responder", "response", "created_at").
		Where("commit_id = ?", commitID).
		OrderExpr("created_at ASC").
		Scan(context.Background()); err != nil {
		return nil, err
	}

	var responses []Response
	for _, row := range rows {
		responses = append(responses, row.toModel())
	}
	return responses, nil
}

// GetCommentsForJob returns all comments linked to a job
func (db *DB) GetCommentsForJob(jobID int64) ([]Response, error) {
	var rows []responseRow
	if err := db.bun.NewSelect().
		Model(&rows).
		Column("id", "commit_id", "job_id", "responder", "response", "created_at").
		Where("job_id = ?", jobID).
		OrderExpr("created_at ASC").
		Scan(context.Background()); err != nil {
		return nil, err
	}

	var responses []Response
	for _, row := range rows {
		responses = append(responses, row.toModel())
	}
	return responses, nil
}

// GetCommentsForCommitSHA returns all comments for a commit by SHA
func (db *DB) GetCommentsForCommitSHA(sha string) ([]Response, error) {
	commit, err := db.GetCommitBySHA(sha)
	if err != nil {
		return nil, err
	}
	return db.GetCommentsForCommit(commit.ID)
}

// GetAllCommentsForJob returns all comments for a job, merging legacy
// commit-based comments via MergeResponses. When commitID > 0, fetches
// legacy comments by commit ID. Otherwise, if fallbackSHA is non-empty,
// fetches by SHA. Callers should validate the SHA (e.g. via
// git.LooksLikeSHA) before passing it here.
func (db *DB) GetAllCommentsForJob(jobID, commitID int64, fallbackSHA string) ([]Response, error) {
	responses, err := db.GetCommentsForJob(jobID)
	if err != nil {
		return nil, err
	}

	var legacyResponses []Response
	var legacyErr error
	if commitID > 0 {
		legacyResponses, legacyErr = db.GetCommentsForCommit(commitID)
	} else if fallbackSHA != "" {
		legacyResponses, legacyErr = db.GetCommentsForCommitSHA(fallbackSHA)
	}
	if legacyErr != nil {
		return responses, fmt.Errorf("legacy comment lookup: %w", legacyErr)
	}

	return MergeResponses(responses, legacyResponses), nil
}

// MergeResponses deduplicates two Response slices by ID and returns
// a chronologically sorted result. This is used wherever job-based
// and legacy commit-based comments are merged.
func MergeResponses(primary, extra []Response) []Response {
	if len(extra) == 0 {
		return primary
	}
	seen := make(map[int64]bool, len(primary))
	for _, r := range primary {
		seen[r.ID] = true
	}
	for _, r := range extra {
		if !seen[r.ID] {
			seen[r.ID] = true
			primary = append(primary, r)
		}
	}
	sort.Slice(primary, func(i, j int) bool {
		return primary[i].CreatedAt.Before(primary[j].CreatedAt)
	})
	return primary
}
