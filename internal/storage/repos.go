package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"strings"
)

func normalizeRepoPath(rootPath string) (string, error) {
	if isWindowsAbsPath(rootPath) {
		return normalizeStoredRepoPath(rootPath), nil
	}

	absPath, err := filepath.Abs(rootPath)
	if err != nil {
		return "", err
	}
	return filepath.ToSlash(absPath), nil
}

func normalizeStoredRepoPath(rootPath string) string {
	if isWindowsAbsPath(rootPath) {
		return strings.ReplaceAll(rootPath, `\`, "/")
	}
	return rootPath
}

func normalizeRepoPathBestEffort(rootPath string) string {
	normalized, err := normalizeRepoPath(rootPath)
	if err == nil {
		return normalized
	}
	if isWindowsAbsPath(rootPath) {
		return strings.ReplaceAll(rootPath, `\`, "/")
	}
	return filepath.ToSlash(rootPath)
}

func normalizeRepoPathPrefix(prefix string) string {
	if prefix == "" {
		return ""
	}
	return strings.TrimRight(normalizeRepoPathBestEffort(prefix), "/")
}

func isWindowsAbsPath(p string) bool {
	return len(p) >= 3 && isASCIIAlpha(p[0]) && p[1] == ':' && (p[2] == '\\' || p[2] == '/')
}

func isASCIIAlpha(b byte) bool {
	return (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z')
}

// GetOrCreateRepo finds or creates a repo by its root path.
// If identity is provided, it will be stored; otherwise the identity field remains NULL.
func (db *DB) GetOrCreateRepo(rootPath string, identity ...string) (*Repo, error) {
	// Normalize path to forward slashes for consistent storage
	// across platforms (LIKE queries use '/' as separator).
	absPath, err := normalizeRepoPath(rootPath)
	if err != nil {
		return nil, err
	}

	// Extract optional identity
	var repoIdentity string
	if len(identity) > 0 {
		repoIdentity = identity[0]
	}

	// Try to find existing by path
	var repo Repo
	var createdAt string
	var identityNullable sql.NullString
	err = db.QueryRow(`SELECT id, root_path, name, identity, created_at FROM repos WHERE root_path = ?`, absPath).
		Scan(&repo.ID, &repo.RootPath, &repo.Name, &identityNullable, &createdAt)
	if err == nil {
		repo.Identity = identityNullable.String
		repo.CreatedAt = parseSQLiteTime(createdAt)

		// Update identity if provided and not already set
		if repoIdentity != "" && repo.Identity == "" {
			_, err = db.Exec(`UPDATE repos SET identity = ? WHERE id = ?`, repoIdentity, repo.ID)
			if err != nil {
				return nil, fmt.Errorf("update identity: %w", err)
			}
			repo.Identity = repoIdentity
		}
		return &repo, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}

	// Create new — use INSERT OR IGNORE to handle concurrent inserts on the
	// same root_path (UNIQUE constraint). If the row already exists, re-read it.
	name := filepath.Base(absPath)
	if repoIdentity != "" {
		_, err = db.Exec(`INSERT OR IGNORE INTO repos (root_path, name, identity) VALUES (?, ?, ?)`, absPath, name, repoIdentity)
	} else {
		_, err = db.Exec(`INSERT OR IGNORE INTO repos (root_path, name) VALUES (?, ?)`, absPath, name)
	}
	if err != nil {
		return nil, err
	}

	// Re-read to get the actual row (whether we just created it or it was
	// concurrently created by another caller).
	var created Repo
	var createdAtStr string
	var idNullable sql.NullString
	err = db.QueryRow(`SELECT id, root_path, name, identity, created_at FROM repos WHERE root_path = ?`, absPath).
		Scan(&created.ID, &created.RootPath, &created.Name, &idNullable, &createdAtStr)
	if err != nil {
		return nil, fmt.Errorf("re-read repo after insert: %w", err)
	}
	created.Identity = idNullable.String
	created.CreatedAt = parseSQLiteTime(createdAtStr)

	// Update identity if provided and not already set
	if repoIdentity != "" && created.Identity == "" {
		_, err = db.Exec(`UPDATE repos SET identity = ? WHERE id = ?`, repoIdentity, created.ID)
		if err != nil {
			return nil, fmt.Errorf("update identity: %w", err)
		}
		created.Identity = repoIdentity
	}

	return &created, nil
}

// GetRepoByPath returns a repo by its path
func (db *DB) GetRepoByPath(rootPath string) (*Repo, error) {
	absPath, err := normalizeRepoPath(rootPath)
	if err != nil {
		return nil, err
	}

	var repo Repo
	var createdAt string
	err = db.QueryRow(`SELECT id, root_path, name, created_at FROM repos WHERE root_path = ?`, absPath).
		Scan(&repo.ID, &repo.RootPath, &repo.Name, &createdAt)
	if err != nil {
		return nil, err
	}
	repo.CreatedAt = parseSQLiteTime(createdAt)
	return &repo, nil
}

// RepoWithCount represents a repo with its total job count
type RepoWithCount struct {
	Name     string `json:"name"`
	RootPath string `json:"root_path"`
	Identity string `json:"identity,omitempty"`
	Count    int    `json:"count"`
}

// ListReposOption configures filtering for ListReposWithReviewCounts.
type ListReposOption func(*listReposOptions)

type listReposOptions struct {
	prefix string
	branch string
}

// WithRepoPathPrefix filters repos whose root_path starts with the given prefix.
func WithRepoPathPrefix(prefix string) ListReposOption {
	return func(o *listReposOptions) {
		o.prefix = normalizeRepoPathPrefix(prefix)
	}
}

// WithRepoBranch filters repos to those having jobs on the given branch.
// Use "(none)" to filter for jobs without a branch.
func WithRepoBranch(branch string) ListReposOption {
	return func(o *listReposOptions) { o.branch = branch }
}

// ListReposWithReviewCounts returns repos with their total job counts.
// Options can filter by path prefix, branch, or both.
func (db *DB) ListReposWithReviewCounts(opts ...ListReposOption) ([]RepoWithCount, int, error) {
	var o listReposOptions
	for _, opt := range opts {
		opt(&o)
	}

	// Branch filtering requires INNER JOIN + HAVING to exclude repos
	// with no matching jobs. Without branch filter, LEFT JOIN shows all repos.
	joinType := "LEFT"
	if o.branch != "" {
		joinType = "INNER"
	}

	query := fmt.Sprintf(`
		SELECT r.name, r.root_path, COALESCE(r.identity, ''), COUNT(rj.id) as job_count
		FROM repos r
		%s JOIN review_jobs rj ON rj.repo_id = r.id
	`, joinType)

	var args []any
	var conditions []string

	if o.prefix != "" {
		conditions = append(
			conditions,
			"r.root_path LIKE ? || '/%' ESCAPE '!'",
		)
		args = append(args, escapeLike(o.prefix))
	}

	if o.branch != "" {
		branchFilter := o.branch
		if o.branch == "(none)" {
			branchFilter = ""
		}
		conditions = append(
			conditions, "COALESCE(rj.branch, '') = ?",
		)
		args = append(args, branchFilter)
	}

	if len(conditions) > 0 {
		query += " WHERE " + strings.Join(conditions, " AND ")
	}

	query += `
		GROUP BY r.id, r.name, r.root_path
	`
	if o.branch != "" {
		query += " HAVING job_count > 0"
	}
	query += " ORDER BY r.name"

	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var repos []RepoWithCount
	totalCount := 0
	for rows.Next() {
		var rc RepoWithCount
		if err := rows.Scan(&rc.Name, &rc.RootPath, &rc.Identity, &rc.Count); err != nil {
			return nil, 0, err
		}
		repos = append(repos, rc)
		totalCount += rc.Count
	}
	return repos, totalCount, rows.Err()
}

// BranchWithCount represents a branch with its total job count
type BranchWithCount struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// BranchListResult contains branches with counts and metadata
type BranchListResult struct {
	Branches       []BranchWithCount
	TotalCount     int
	NullsRemaining int // Number of jobs with NULL/empty branch (for backfill tracking)
}

// ListBranchesWithCounts returns all branches with their job counts
// If repoPaths is non-empty, filters to jobs in those repos only
func (db *DB) ListBranchesWithCounts(repoPaths []string) (*BranchListResult, error) {
	var rows *sql.Rows
	var err error

	if len(repoPaths) == 0 {
		// No repo filter - count branches across all repos
		rows, err = db.Query(`
			SELECT COALESCE(NULLIF(branch, ''), '(none)') as branch_name, COUNT(*) as job_count
			FROM review_jobs
			GROUP BY branch_name
			ORDER BY job_count DESC, branch_name
		`)
	} else if len(repoPaths) == 1 {
		// Single repo filter
		rows, err = db.Query(`
			SELECT COALESCE(NULLIF(rj.branch, ''), '(none)') as branch_name, COUNT(*) as job_count
			FROM review_jobs rj
			INNER JOIN repos r ON rj.repo_id = r.id
			WHERE r.root_path = ?
			GROUP BY branch_name
			ORDER BY job_count DESC, branch_name
		`, repoPaths[0])
	} else {
		// Multiple repo paths - build IN clause with placeholders
		placeholders := make([]string, len(repoPaths))
		args := make([]any, len(repoPaths))
		for i, p := range repoPaths {
			placeholders[i] = "?"
			args[i] = p
		}
		query := fmt.Sprintf(`
			SELECT COALESCE(NULLIF(rj.branch, ''), '(none)') as branch_name, COUNT(*) as job_count
			FROM review_jobs rj
			INNER JOIN repos r ON rj.repo_id = r.id
			WHERE r.root_path IN (%s)
			GROUP BY branch_name
			ORDER BY job_count DESC, branch_name
		`, strings.Join(placeholders, ","))
		rows, err = db.Query(query, args...)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := &BranchListResult{}
	for rows.Next() {
		var bc BranchWithCount
		if err := rows.Scan(&bc.Name, &bc.Count); err != nil {
			return nil, err
		}
		result.Branches = append(result.Branches, bc)
		result.TotalCount += bc.Count
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Count actual NULL branches (not empty string or "(none)" sentinel)
	if err := db.QueryRow("SELECT COUNT(*) FROM review_jobs WHERE branch IS NULL").Scan(&result.NullsRemaining); err != nil {
		return nil, err
	}

	return result, nil
}

// RenameRepo updates the display name of a repo identified by its path or current name
func (db *DB) RenameRepo(identifier, newName string) (int64, error) {
	// Try to match by root_path first (absolute or relative), then by name
	absPath, pathErr := normalizeRepoPath(identifier)

	// Try path match first
	if pathErr == nil {
		result, err := db.Exec(`UPDATE repos SET name = ? WHERE root_path = ?`, newName, absPath)
		if err != nil {
			return 0, err
		}
		affected, _ := result.RowsAffected()
		if affected > 0 {
			return affected, nil
		}
	}

	// Try name match
	result, err := db.Exec(`UPDATE repos SET name = ? WHERE name = ?`, newName, identifier)
	if err != nil {
		return 0, err
	}
	affected, _ := result.RowsAffected()
	return affected, nil
}

// ErrRepoPathConflict is returned when MoveRepo would create a duplicate root_path.
var ErrRepoPathConflict = errors.New("a different repository already has the target path; use 'roborev repo merge' instead")

// MoveRepo updates the root_path of an existing repo. The newPath is normalized
// to an absolute path with forward slashes. Optionally updates the identity if
// newIdentity is non-empty.
//
// Returns ErrRepoPathConflict if another repo (different ID) already has the
// target path - users should `repo merge` in that case.
func (db *DB) MoveRepo(repoID int64, newPath, newIdentity string) error {
	absPath, err := normalizeRepoPath(newPath)
	if err != nil {
		return fmt.Errorf("resolve new path: %w", err)
	}

	// Detect conflict: another repo already at this path
	var existingID int64
	err = db.QueryRow(`SELECT id FROM repos WHERE root_path = ?`, absPath).Scan(&existingID)
	if err == nil && existingID != repoID {
		return ErrRepoPathConflict
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("check path conflict: %w", err)
	}

	if newIdentity != "" {
		_, err = db.Exec(`UPDATE repos SET root_path = ?, identity = ? WHERE id = ?`, absPath, newIdentity, repoID)
	} else {
		_, err = db.Exec(`UPDATE repos SET root_path = ? WHERE id = ?`, absPath, repoID)
	}
	if err != nil {
		return fmt.Errorf("update repo path: %w", err)
	}
	return nil
}

// ListRepos returns all repos in the database
func (db *DB) ListRepos() ([]Repo, error) {
	rows, err := db.Query(`SELECT id, root_path, name, created_at FROM repos ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var repos []Repo
	for rows.Next() {
		var r Repo
		var createdAt string
		if err := rows.Scan(&r.ID, &r.RootPath, &r.Name, &createdAt); err != nil {
			return nil, err
		}
		r.CreatedAt = parseSQLiteTime(createdAt)
		repos = append(repos, r)
	}
	return repos, rows.Err()
}

// GetRepoByID returns a repo by its ID
func (db *DB) GetRepoByID(id int64) (*Repo, error) {
	var repo Repo
	var createdAt string
	err := db.QueryRow(`SELECT id, root_path, name, created_at FROM repos WHERE id = ?`, id).
		Scan(&repo.ID, &repo.RootPath, &repo.Name, &createdAt)
	if err != nil {
		return nil, err
	}
	repo.CreatedAt = parseSQLiteTime(createdAt)
	return &repo, nil
}

// GetRepoByName returns a repo by its display name
func (db *DB) GetRepoByName(name string) (*Repo, error) {
	var repo Repo
	var createdAt string
	err := db.QueryRow(`SELECT id, root_path, name, created_at FROM repos WHERE name = ?`, name).
		Scan(&repo.ID, &repo.RootPath, &repo.Name, &createdAt)
	if err != nil {
		return nil, err
	}
	repo.CreatedAt = parseSQLiteTime(createdAt)
	return &repo, nil
}

// FindRepo finds a repo by path or name (tries path first, then name)
func (db *DB) FindRepo(identifier string) (*Repo, error) {
	// Try by path first
	repo, err := db.GetRepoByPath(identifier)
	if err == nil {
		return repo, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}

	// Try by name
	repo, err = db.GetRepoByName(identifier)
	if err != nil {
		return nil, err
	}
	return repo, nil
}

// RepoStats contains statistics for a single repo
type RepoStats struct {
	Repo          *Repo
	TotalJobs     int
	QueuedJobs    int
	RunningJobs   int
	CompletedJobs int
	FailedJobs    int
	PassedReviews int
	FailedReviews int
	ClosedReviews int
	OpenReviews   int
}

// GetRepoStats returns detailed statistics for a repo
func (db *DB) GetRepoStats(repoID int64) (*RepoStats, error) {
	repo, err := db.GetRepoByID(repoID)
	if err != nil {
		return nil, err
	}

	stats := &RepoStats{Repo: repo}

	// Get job counts by status
	rows, err := db.Query(`
		SELECT status, COUNT(*) FROM review_jobs WHERE repo_id = ? GROUP BY status
	`, repoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var status string
		var count int
		if err := rows.Scan(&status, &count); err != nil {
			return nil, err
		}
		stats.TotalJobs += count
		switch JobStatus(status) {
		case JobStatusQueued:
			stats.QueuedJobs = count
		case JobStatusRunning:
			stats.RunningJobs = count
		case JobStatusDone:
			stats.CompletedJobs = count
		case JobStatusFailed:
			stats.FailedJobs = count
		}
	}

	// Get review verdict counts (P/F from output)
	// Closed/open counts preserve the legacy prompt-job exclusion.
	reviewRows, err := db.Query(`
		SELECT r.output, r.closed, r.verdict_bool, rj.commit_id, rj.git_ref, rj.job_type
		FROM reviews r
		JOIN review_jobs rj ON r.job_id = rj.id
		WHERE rj.repo_id = ?
	`, repoID)
	if err != nil {
		return nil, err
	}
	defer reviewRows.Close()

	for reviewRows.Next() {
		var output string
		var closed int
		var gitRef string
		var verdictBool sql.NullInt64
		var fields reviewJobScanFields

		if err := reviewRows.Scan(&output, &closed, &verdictBool, &fields.CommitID, &gitRef, &fields.JobType); err != nil {
			return nil, err
		}

		job := ReviewJob{GitRef: gitRef}
		applyReviewJobScan(&job, fields)

		if job.CommitID == nil && job.GitRef == "prompt" {
			continue
		}

		applyJobVerdict(&job, verdictBool, output)
		if job.Verdict != nil {
			if *job.Verdict == verdictPass {
				stats.PassedReviews++
			} else {
				stats.FailedReviews++
			}
		}

		if closed != 0 {
			stats.ClosedReviews++
		} else {
			stats.OpenReviews++
		}
	}
	if err := reviewRows.Err(); err != nil {
		return nil, err
	}

	return stats, nil
}

// ErrRepoHasJobs is returned when trying to delete a repo with jobs without cascade
var ErrRepoHasJobs = errors.New("repository has existing jobs; use cascade to delete them")

// DeleteRepo deletes a repo and optionally its associated data
// If cascade is true, also deletes all jobs, reviews, and responses for the repo
// If cascade is false and jobs exist, returns ErrRepoHasJobs
func (db *DB) DeleteRepo(repoID int64, cascade bool) error {
	// Use a dedicated connection with BEGIN IMMEDIATE for proper locking
	// This ensures no job can be enqueued between the count check and delete
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	// BEGIN IMMEDIATE acquires a write lock immediately, preventing races
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}

	// Ensure rollback on error
	committed := false
	defer func() {
		if !committed {
			if _, err := conn.ExecContext(ctx, "ROLLBACK"); err != nil {
				log.Printf("repos DeleteRepo: rollback failed: %v", err)
			}
		}
	}()

	// Check for existing jobs (within transaction for consistency)
	var jobCount int
	err = conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM review_jobs WHERE repo_id = ?`, repoID).Scan(&jobCount)
	if err != nil {
		return err
	}

	if !cascade && jobCount > 0 {
		return ErrRepoHasJobs
	}

	if cascade {
		// Delete in correct order due to foreign keys
		// 1a. Delete responses for jobs in this repo (job_id based)
		_, err := conn.ExecContext(ctx, `
			DELETE FROM responses WHERE job_id IN (
				SELECT id FROM review_jobs WHERE repo_id = ?
			)
		`, repoID)
		if err != nil {
			return err
		}

		// 1b. Delete responses for commits in this repo (legacy commit_id based)
		_, err = conn.ExecContext(ctx, `
			DELETE FROM responses WHERE commit_id IN (
				SELECT id FROM commits WHERE repo_id = ?
			)
		`, repoID)
		if err != nil {
			return err
		}

		// 2. Delete reviews for jobs in this repo
		_, err = conn.ExecContext(ctx, `
			DELETE FROM reviews WHERE job_id IN (
				SELECT id FROM review_jobs WHERE repo_id = ?
			)
		`, repoID)
		if err != nil {
			return err
		}

		// 3. Delete jobs for this repo
		_, err = conn.ExecContext(ctx, `DELETE FROM review_jobs WHERE repo_id = ?`, repoID)
		if err != nil {
			return err
		}

		// 4. Delete commits for this repo
		_, err = conn.ExecContext(ctx, `DELETE FROM commits WHERE repo_id = ?`, repoID)
		if err != nil {
			return err
		}
	}

	// Delete the repo itself
	result, err := conn.ExecContext(ctx, `DELETE FROM repos WHERE id = ?`, repoID)
	if err != nil {
		return err
	}
	affected, _ := result.RowsAffected()
	if affected == 0 {
		return sql.ErrNoRows
	}

	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return err
	}
	committed = true
	return nil
}

// MergeRepos moves all jobs and commits from sourceRepoID to targetRepoID, then deletes the source repo
func (db *DB) MergeRepos(sourceRepoID, targetRepoID int64) (int64, error) {
	if sourceRepoID == targetRepoID {
		return 0, nil
	}

	// Use a dedicated connection with BEGIN IMMEDIATE for proper locking
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		return 0, err
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return 0, err
	}

	committed := false
	defer func() {
		if !committed {
			if _, err := conn.ExecContext(ctx, "ROLLBACK"); err != nil {
				log.Printf("repos MergeRepos: rollback failed: %v", err)
			}
		}
	}()

	// Move all commits from source to target
	// Note: commits.sha is UNIQUE, so this will fail if both repos have
	// commits with the same SHA (which shouldn't happen for the same git repo)
	// Commit-based responses (legacy) are tied to commit_id which remains valid
	_, err = conn.ExecContext(ctx, `UPDATE commits SET repo_id = ? WHERE repo_id = ?`, targetRepoID, sourceRepoID)
	if err != nil {
		return 0, err
	}

	// Move all jobs from source to target
	result, err := conn.ExecContext(ctx, `UPDATE review_jobs SET repo_id = ? WHERE repo_id = ?`, targetRepoID, sourceRepoID)
	if err != nil {
		return 0, err
	}
	affected, _ := result.RowsAffected()

	// Delete the source repo (now empty)
	_, err = conn.ExecContext(ctx, `DELETE FROM repos WHERE id = ?`, sourceRepoID)
	if err != nil {
		return 0, err
	}

	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return 0, err
	}
	committed = true

	return affected, nil
}
