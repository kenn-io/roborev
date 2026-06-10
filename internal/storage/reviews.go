package storage

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"go.kenn.io/roborev/internal/agent"
)

// GetReviewByJobID finds a review by its job ID
func (db *DB) GetReviewByJobID(jobID int64) (*Review, error) {
	var r Review
	var reviewFields reviewScanFields
	var job ReviewJob
	var jobFields reviewJobScanFields
	err := db.QueryRow(`
		SELECT rv.id, rv.job_id, rv.agent, rv.prompt, rv.output, rv.created_at, rv.closed, rv.uuid, rv.verdict_bool,
		       j.id, j.repo_id, j.commit_id, j.git_ref, j.branch, j.ci_base_branch, j.session_id, j.agent, j.reasoning, j.status, j.enqueued_at,
		       j.started_at, j.finished_at, j.worker_id, j.error, j.model, j.provider, j.requested_model, j.requested_provider, j.job_type, j.review_type, j.patch_id,
		       rp.root_path, rp.name, c.subject, j.token_usage, COALESCE(j.min_severity, ''), COALESCE(j.backup_agent, ''), COALESCE(j.backup_model, ''),
		       COALESCE(j.panel_run_uuid, ''), COALESCE(j.panel_role, ''), COALESCE(j.panel_name, ''), COALESCE(j.panel_member_name, ''), j.panel_member_index, COALESCE(j.panel_member_config_json, ''), COALESCE(j.claim_blocked, 0)
		FROM reviews rv
		JOIN review_jobs j ON j.id = rv.job_id
		JOIN repos rp ON rp.id = j.repo_id
		LEFT JOIN commits c ON c.id = j.commit_id
		WHERE rv.job_id = ?
	`, jobID).Scan(&r.ID, &r.JobID, &r.Agent, &r.Prompt, &r.Output, &reviewFields.CreatedAt, &reviewFields.Closed, &reviewFields.UUID, &reviewFields.VerdictBool,
		&job.ID, &job.RepoID, &jobFields.CommitID, &job.GitRef, &jobFields.Branch, &jobFields.CIBaseBranch, &jobFields.SessionID, &job.Agent, &job.Reasoning, &job.Status, &jobFields.EnqueuedAt,
		&jobFields.StartedAt, &jobFields.FinishedAt, &jobFields.WorkerID, &jobFields.Error, &jobFields.Model, &jobFields.Provider, &jobFields.RequestedModel, &jobFields.RequestedProvider, &jobFields.JobType, &jobFields.ReviewType, &jobFields.PatchID,
		&job.RepoPath, &job.RepoName, &jobFields.CommitSubject, &jobFields.TokenUsage, &jobFields.MinSeverity, &jobFields.BackupAgent, &jobFields.BackupModel,
		&jobFields.PanelRunUUID, &jobFields.PanelRole, &jobFields.PanelName, &jobFields.PanelMemberName, &jobFields.PanelMemberIndex, &jobFields.PanelMemberConfig, &jobFields.ClaimBlocked)
	if err != nil {
		return nil, err
	}
	applyReviewScan(&r, reviewFields)
	applyReviewJobScan(&job, jobFields)
	applyJobVerdict(&job, reviewFields.VerdictBool, r.Output)

	r.Job = &job

	return &r, nil
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
	err := db.QueryRow(`
		SELECT j.id
		FROM review_jobs j
		WHERE j.git_ref = ?
		  AND j.job_type IN ('review','range','dirty','synthesis','compact')
		  AND COALESCE(j.panel_role, '') != 'member'
		ORDER BY j.enqueued_at DESC, j.id DESC
		LIMIT 1
	`, sha).Scan(&jobID)
	if err != nil {
		return nil, err
	}
	return db.GetReviewByJobID(jobID)
}

// GetAllReviewsForGitRef returns all reviews for a git ref (commit SHA or range) for re-review context
func (db *DB) GetAllReviewsForGitRef(gitRef string) ([]Review, error) {
	rows, err := db.Query(`
		SELECT rv.id, rv.job_id, rv.agent, rv.prompt, rv.output, rv.created_at, rv.closed
		FROM reviews rv
		JOIN review_jobs j ON j.id = rv.job_id
		WHERE j.git_ref = ?
		  AND COALESCE(j.panel_role, '') != 'member'
		ORDER BY rv.created_at ASC
	`, gitRef)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var reviews []Review
	for rows.Next() {
		var r Review
		var fields reviewScanFields
		if err := rows.Scan(&r.ID, &r.JobID, &r.Agent, &r.Prompt, &r.Output, &fields.CreatedAt, &fields.Closed); err != nil {
			return nil, err
		}
		applyReviewScan(&r, fields)
		reviews = append(reviews, r)
	}

	return reviews, rows.Err()
}

// GetRecentReviewsForRepo returns the N most recent reviews for a repo
func (db *DB) GetRecentReviewsForRepo(repoID int64, limit int) ([]Review, error) {
	rows, err := db.Query(`
		SELECT rv.id, rv.job_id, rv.agent, rv.prompt, rv.output, rv.created_at, rv.closed
		FROM reviews rv
		JOIN review_jobs j ON j.id = rv.job_id
		WHERE j.repo_id = ?
		ORDER BY rv.created_at DESC
		LIMIT ?
	`, repoID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var reviews []Review
	for rows.Next() {
		var r Review
		var fields reviewScanFields
		if err := rows.Scan(&r.ID, &r.JobID, &r.Agent, &r.Prompt, &r.Output, &fields.CreatedAt, &fields.Closed); err != nil {
			return nil, err
		}
		applyReviewScan(&r, fields)
		reviews = append(reviews, r)
	}

	return reviews, rows.Err()
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
	query := `
		SELECT j.id, j.git_ref, j.session_id, COALESCE(c.sha, '')
		FROM review_jobs j
		LEFT JOIN commits c ON c.id = j.commit_id
		WHERE j.repo_id = ?
		  AND j.branch = ?
		  AND j.agent = ?
		  AND j.status = 'done'
		  AND COALESCE(NULLIF(j.job_type, ''), 'review') IN ('review', 'range', 'dirty')
		  AND COALESCE(j.panel_role, '') = ''
		  AND j.session_id IS NOT NULL
		  AND j.session_id <> ''
		  AND COALESCE(NULLIF(j.review_type, ''), 'default') = ?
		  AND COALESCE(j.worktree_path, '') = ?
		ORDER BY COALESCE(j.finished_at, j.updated_at, j.enqueued_at) DESC, j.id DESC`
	baseArgs := []any{repoID, branch, agent, reviewType, worktreePath}
	if limit <= 0 {
		jobs, _, err := db.scanReusableSessionCandidates(query, baseArgs, 0)
		return jobs, err
	}

	batchSize := max(limit*2, 20)

	var jobs []ReviewJob
	for offset := 0; len(jobs) < limit; offset += batchSize {
		batchQuery := query + "\n\t\tLIMIT ? OFFSET ?"
		batchArgs := append(append([]any{}, baseArgs...), batchSize, offset)
		batch, scanned, err := db.scanReusableSessionCandidates(batchQuery, batchArgs, limit-len(jobs))
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

func (db *DB) scanReusableSessionCandidates(query string, args []any, remaining int) ([]ReviewJob, int, error) {
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var jobs []ReviewJob
	scanned := 0
	for rows.Next() {
		scanned++
		var job ReviewJob
		var sessionID sql.NullString
		var commitSHA string
		if err := rows.Scan(&job.ID, &job.GitRef, &sessionID, &commitSHA); err != nil {
			return nil, 0, err
		}
		target := reusableSessionCandidateTarget(job.GitRef, commitSHA)
		if !sessionID.Valid || !agent.IsValidResumeSessionID(sessionID.String) || target == "" {
			continue
		}
		job.SessionID = sessionID.String
		job.ReusableSessionTarget = target
		jobs = append(jobs, job)
		if remaining > 0 && len(jobs) >= remaining {
			break
		}
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return jobs, scanned, nil
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
	val := 0
	if closed {
		val = 1
	}
	now := time.Now().Format(time.RFC3339)
	machineID, _ := db.GetMachineID()

	result, err := db.Exec(`UPDATE reviews SET closed = ?, updated_by_machine_id = ?, updated_at = ? WHERE id = ?`, val, machineID, now, reviewID)
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
	val := 0
	if closed {
		val = 1
	}
	now := time.Now().Format(time.RFC3339)
	machineID, _ := db.GetMachineID()

	result, err := db.Exec(`UPDATE reviews SET closed = ?, updated_by_machine_id = ?, updated_at = ? WHERE job_id = ?`, val, machineID, now, jobID)
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

	// Build placeholders for IN clause
	placeholders := make([]string, len(jobIDs))
	args := make([]any, len(jobIDs))
	for i, id := range jobIDs {
		placeholders[i] = "?"
		args[i] = id
	}
	inClause := strings.Join(placeholders, ",")

	// Fetch jobs
	// Note: The IN clause is built dynamically, but this is safe from SQL injection.
	// The `placeholders` slice contains only "?" characters, and the `args` slice
	// contains the integer IDs, which are passed to the DB driver for parameterization.
	// This prevents user-controlled input from being part of the SQL query string itself.
	jobQuery := fmt.Sprintf(`
		SELECT j.id, j.repo_id, j.commit_id, j.git_ref, j.branch, j.ci_base_branch, j.session_id, j.agent, j.reasoning, j.status, j.enqueued_at,
		       j.started_at, j.finished_at, j.worker_id, j.error, COALESCE(j.agentic, 0),
		       r.root_path, r.name, c.subject, j.model, j.job_type, j.review_type, COALESCE(j.min_severity, ''),
		       COALESCE(j.backup_agent, ''), COALESCE(j.backup_model, ''),
		       COALESCE(j.panel_run_uuid, ''), COALESCE(j.panel_role, ''), COALESCE(j.panel_name, ''), COALESCE(j.panel_member_name, ''), j.panel_member_index, COALESCE(j.panel_member_config_json, ''), COALESCE(j.claim_blocked, 0)
		FROM review_jobs j
		JOIN repos r ON r.id = j.repo_id
		LEFT JOIN commits c ON c.id = j.commit_id
		WHERE j.id IN (%s)
	`, inClause)

	rows, err := db.Query(jobQuery, args...)
	if err != nil {
		return nil, fmt.Errorf("query jobs: %w", err)
	}
	defer rows.Close()

	result := make(map[int64]JobWithReview, len(jobIDs))
	for rows.Next() {
		var j ReviewJob
		var fields reviewJobScanFields

		if err := rows.Scan(&j.ID, &j.RepoID, &fields.CommitID, &j.GitRef, &fields.Branch, &fields.CIBaseBranch, &fields.SessionID, &j.Agent, &j.Reasoning, &j.Status, &fields.EnqueuedAt,
			&fields.StartedAt, &fields.FinishedAt, &fields.WorkerID, &fields.Error, &fields.Agentic,
			&j.RepoPath, &j.RepoName, &fields.CommitSubject, &fields.Model, &fields.JobType, &fields.ReviewType, &fields.MinSeverity,
			&fields.BackupAgent, &fields.BackupModel,
			&fields.PanelRunUUID, &fields.PanelRole, &fields.PanelName, &fields.PanelMemberName, &fields.PanelMemberIndex, &fields.PanelMemberConfig, &fields.ClaimBlocked); err != nil {
			return nil, fmt.Errorf("scan job: %w", err)
		}
		applyReviewJobScan(&j, fields)

		result[j.ID] = JobWithReview{Job: j}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate jobs: %w", err)
	}

	// Fetch reviews for these jobs
	reviewQuery := fmt.Sprintf(`
		SELECT rv.id, rv.job_id, rv.agent, rv.prompt, rv.output, rv.created_at, rv.closed, rv.verdict_bool
		FROM reviews rv
		WHERE rv.job_id IN (%s)
	`, inClause)

	reviewRows, err := db.Query(reviewQuery, args...)
	if err != nil {
		return nil, fmt.Errorf("query reviews: %w", err)
	}
	defer reviewRows.Close()

	for reviewRows.Next() {
		var r Review
		var fields reviewScanFields
		if err := reviewRows.Scan(&r.ID, &r.JobID, &r.Agent, &r.Prompt, &r.Output, &fields.CreatedAt, &fields.Closed, &fields.VerdictBool); err != nil {
			return nil, fmt.Errorf("scan review: %w", err)
		}
		applyReviewScan(&r, fields)

		if entry, ok := result[r.JobID]; ok {
			entry.Review = &r
			applyJobVerdict(&entry.Job, fields.VerdictBool, r.Output)
			result[r.JobID] = entry
		}
	}
	if err := reviewRows.Err(); err != nil {
		return nil, fmt.Errorf("iterate reviews: %w", err)
	}

	return result, nil
}

// GetReviewByID finds a review by its ID
func (db *DB) GetReviewByID(reviewID int64) (*Review, error) {
	var r Review
	var fields reviewScanFields

	err := db.QueryRow(`
		SELECT id, job_id, agent, prompt, output, created_at, closed
		FROM reviews WHERE id = ?
	`, reviewID).Scan(&r.ID, &r.JobID, &r.Agent, &r.Prompt, &r.Output, &fields.CreatedAt, &fields.Closed)
	if err != nil {
		return nil, err
	}
	applyReviewScan(&r, fields)

	return &r, nil
}

// AddComment adds a comment to a commit (legacy - use AddCommentToJob for new code)
func (db *DB) AddComment(commitID int64, responder, response string) (*Response, error) {
	uuid := GenerateUUID()
	machineID, _ := db.GetMachineID()
	now := time.Now()
	nowStr := now.Format(time.RFC3339)

	result, err := db.Exec(`INSERT INTO responses (commit_id, responder, response, uuid, source_machine_id, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		commitID, responder, response, uuid, machineID, nowStr)
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
	err := db.QueryRow(`SELECT 1 FROM review_jobs WHERE id = ?`, jobID).Scan(&exists)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, sql.ErrNoRows // Job not found
		}
		return nil, err
	}

	uuid := GenerateUUID()
	machineID, _ := db.GetMachineID()
	now := time.Now()
	nowStr := now.Format(time.RFC3339)

	result, err := db.Exec(`INSERT INTO responses (job_id, responder, response, uuid, source_machine_id, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		jobID, responder, response, uuid, machineID, nowStr)
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
	rows, err := db.Query(`
		SELECT id, commit_id, job_id, responder, response, created_at
		FROM responses
		WHERE commit_id = ?
		ORDER BY created_at ASC
	`, commitID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var responses []Response
	for rows.Next() {
		var r Response
		var createdAt string
		var commitIDNull, jobIDNull sql.NullInt64
		if err := rows.Scan(&r.ID, &commitIDNull, &jobIDNull, &r.Responder, &r.Response, &createdAt); err != nil {
			return nil, err
		}
		if commitIDNull.Valid {
			r.CommitID = &commitIDNull.Int64
		}
		if jobIDNull.Valid {
			r.JobID = &jobIDNull.Int64
		}
		r.CreatedAt = parseSQLiteTime(createdAt)
		responses = append(responses, r)
	}

	return responses, rows.Err()
}

// GetCommentsForJob returns all comments linked to a job
func (db *DB) GetCommentsForJob(jobID int64) ([]Response, error) {
	rows, err := db.Query(`
		SELECT id, commit_id, job_id, responder, response, created_at
		FROM responses
		WHERE job_id = ?
		ORDER BY created_at ASC
	`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var responses []Response
	for rows.Next() {
		var r Response
		var createdAt string
		var commitIDNull, jobIDNull sql.NullInt64
		if err := rows.Scan(&r.ID, &commitIDNull, &jobIDNull, &r.Responder, &r.Response, &createdAt); err != nil {
			return nil, err
		}
		if commitIDNull.Valid {
			r.CommitID = &commitIDNull.Int64
		}
		if jobIDNull.Valid {
			r.JobID = &jobIDNull.Int64
		}
		r.CreatedAt = parseSQLiteTime(createdAt)
		responses = append(responses, r)
	}

	return responses, rows.Err()
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
