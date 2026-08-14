package storage

// GetLatestLogicalReviewJob returns the newest user-facing review for an exact
// repository path and optional exact branch. Panel members and non-review jobs
// are excluded so contextual consumers see the same canonical review identity
// as the native Reviews workspace.
func (db *DB) GetLatestLogicalReviewJob(repoPath, branch string) (*ReviewJob, error) {
	query := `
		SELECT j.id
		FROM review_jobs j
		JOIN repos r ON r.id = j.repo_id
		WHERE r.root_path = ?
		  AND j.job_type IN ('review', 'range', 'dirty', 'synthesis', 'compact')
		  AND COALESCE(j.panel_role, '') != 'member'`
	args := []any{normalizeRepoPathBestEffort(repoPath)}
	if branch != "" {
		query += " AND j.branch = ?"
		args = append(args, branch)
	}
	query += " ORDER BY " + sqliteNormalizedTimestampExpr("j.enqueued_at") + " DESC, j.id DESC LIMIT 1"

	var jobID int64
	if err := db.QueryRow(query, args...).Scan(&jobID); err != nil {
		return nil, err
	}
	job, err := db.GetJobByID(jobID)
	if err != nil {
		return nil, err
	}
	return job, nil
}
