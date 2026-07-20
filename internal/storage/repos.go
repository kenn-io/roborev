package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"strings"

	"github.com/uptrace/bun"
)

var repoColumns = []string{"id", "root_path", "name", "identity", "created_at"}

func (db *DB) selectRepo(ctx context.Context, where string, args ...any) (*Repo, error) {
	var row repoRow
	if err := db.bun.NewSelect().
		Model(&row).
		Column(repoColumns...).
		Where(where, args...).
		Scan(ctx); err != nil {
		return nil, err
	}
	repo := row.toModel()
	return &repo, nil
}

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

	ctx := context.Background()
	repo, err := db.selectRepo(ctx, "root_path = ?", absPath)
	if err == nil {
		// Update identity if provided and not already set
		if repoIdentity != "" && repo.Identity == "" {
			_, err = db.bun.NewUpdate().
				Model((*repoRow)(nil)).
				Set("identity = ?", repoIdentity).
				Where("id = ?", repo.ID).
				Exec(ctx)
			if err != nil {
				return nil, fmt.Errorf("update identity: %w", err)
			}
			repo.Identity = repoIdentity
		}
		return repo, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}

	// Create new with an idempotent conflict clause for concurrent inserts on
	// the same root_path. If the row already exists, re-read it.
	row := repoRow{
		RootPath: absPath,
		Name:     filepath.Base(absPath),
		Identity: optionalString(repoIdentity),
	}
	if _, err = db.bun.NewInsert().
		Model(&row).
		Column("root_path", "name", "identity").
		On("CONFLICT (root_path) DO NOTHING").
		Exec(ctx); err != nil {
		return nil, err
	}

	// Re-read to get the actual row (whether we just created it or it was
	// concurrently created by another caller).
	created, err := db.selectRepo(ctx, "root_path = ?", absPath)
	if err != nil {
		return nil, fmt.Errorf("re-read repo after insert: %w", err)
	}

	// Update identity if provided and not already set
	if repoIdentity != "" && created.Identity == "" {
		_, err = db.bun.NewUpdate().
			Model((*repoRow)(nil)).
			Set("identity = ?", repoIdentity).
			Where("id = ?", created.ID).
			Exec(ctx)
		if err != nil {
			return nil, fmt.Errorf("update identity: %w", err)
		}
		created.Identity = repoIdentity
	}

	return created, nil
}

// GetRepoByPath returns a repo by its path
func (db *DB) GetRepoByPath(rootPath string) (*Repo, error) {
	absPath, err := normalizeRepoPath(rootPath)
	if err != nil {
		return nil, err
	}

	return db.selectRepo(context.Background(), "root_path = ?", absPath)
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

	query := db.bun.NewSelect().
		TableExpr("repos AS r").
		ColumnExpr("r.name").
		ColumnExpr("r.root_path").
		ColumnExpr("COALESCE(r.identity, '') AS identity").
		ColumnExpr("COUNT(rj.id) AS count")
	if o.branch != "" {
		query = query.Join("INNER JOIN review_jobs AS rj ON rj.repo_id = r.id")
	} else {
		query = query.Join("LEFT JOIN review_jobs AS rj ON rj.repo_id = r.id")
	}

	if o.prefix != "" {
		query = query.Where("r.root_path LIKE ? || '/%' ESCAPE '!'", escapeLike(o.prefix))
	}

	if o.branch != "" {
		branchFilter := o.branch
		if o.branch == "(none)" {
			branchFilter = ""
		}
		query = query.Where("COALESCE(rj.branch, '') = ?", branchFilter)
	}

	query = query.
		GroupExpr("r.id, r.name, r.root_path").
		OrderExpr("r.name")
	if o.branch != "" {
		query = query.Having("COUNT(rj.id) > 0")
	}

	var repos []RepoWithCount
	if err := query.Scan(context.Background(), &repos); err != nil {
		return nil, 0, err
	}

	totalCount := 0
	for _, repo := range repos {
		totalCount += repo.Count
	}
	return repos, totalCount, nil
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
	var rows []struct {
		Name  string `bun:"branch_name"`
		Count int    `bun:"count"`
	}
	query := db.bun.NewSelect().
		TableExpr("review_jobs AS rj").
		ColumnExpr("COALESCE(NULLIF(rj.branch, ''), '(none)') AS branch_name").
		ColumnExpr("COUNT(*) AS count").
		GroupExpr("branch_name").
		OrderExpr("count DESC, branch_name")
	if len(repoPaths) > 0 {
		query = query.Join("INNER JOIN repos AS r ON rj.repo_id = r.id")
		if len(repoPaths) == 1 {
			query = query.Where("r.root_path = ?", repoPaths[0])
		} else {
			query = query.Where("r.root_path IN (?)", bun.List(repoPaths))
		}
	}

	result := &BranchListResult{}
	if err := query.Scan(context.Background(), &rows); err != nil {
		return nil, err
	}
	for _, row := range rows {
		result.Branches = append(result.Branches, BranchWithCount{Name: row.Name, Count: row.Count})
		result.TotalCount += row.Count
	}

	// Count actual NULL branches (not empty string or "(none)" sentinel)
	if err := db.bun.NewSelect().
		Table("review_jobs").
		ColumnExpr("COUNT(*)").
		Where("branch IS NULL").
		Scan(context.Background(), &result.NullsRemaining); err != nil {
		return nil, err
	}

	return result, nil
}

// RenameRepo updates the display name of a repo identified by its path or current name
func (db *DB) RenameRepo(identifier, newName string) (int64, error) {
	ctx := context.Background()
	// Try to match by root_path first (absolute or relative), then by name
	absPath, pathErr := normalizeRepoPath(identifier)

	// Try path match first
	if pathErr == nil {
		result, err := db.bun.NewUpdate().
			Model((*repoRow)(nil)).
			Set("name = ?", newName).
			Where("root_path = ?", absPath).
			Exec(ctx)
		if err != nil {
			return 0, err
		}
		affected, _ := result.RowsAffected()
		if affected > 0 {
			return affected, nil
		}
	}

	// Try name match
	result, err := db.bun.NewUpdate().
		Model((*repoRow)(nil)).
		Set("name = ?", newName).
		Where("name = ?", identifier).
		Exec(ctx)
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
	ctx := context.Background()
	var existingID int64
	err = db.bun.NewSelect().
		Table("repos").
		Column("id").
		Where("root_path = ?", absPath).
		Scan(ctx, &existingID)
	if err == nil && existingID != repoID {
		return ErrRepoPathConflict
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("check path conflict: %w", err)
	}

	query := db.bun.NewUpdate().
		Model((*repoRow)(nil)).
		Set("root_path = ?", absPath).
		Where("id = ?", repoID)
	if newIdentity != "" {
		query = query.Set("identity = ?", newIdentity)
	}
	if _, err = query.Exec(ctx); err != nil {
		return fmt.Errorf("update repo path: %w", err)
	}
	return nil
}

// ListRepos returns all repos in the database
func (db *DB) ListRepos() ([]Repo, error) {
	var rows []repoRow
	if err := db.bun.NewSelect().
		Model(&rows).
		Column("id", "root_path", "name", "created_at").
		OrderExpr("name").
		Scan(context.Background()); err != nil {
		return nil, err
	}

	var repos []Repo
	for _, row := range rows {
		repos = append(repos, row.toModel())
	}
	return repos, nil
}

func (db *DB) ListReposWithIdentity() ([]Repo, error) {
	var rows []repoRow
	if err := db.bun.NewSelect().
		Model(&rows).
		Column("id", "root_path", "name", "created_at", "identity").
		Where("identity IS NOT NULL").
		Where("identity != ''").
		Scan(context.Background()); err != nil {
		return nil, err
	}
	repos := make([]Repo, 0, len(rows))
	for _, row := range rows {
		repos = append(repos, row.toModel())
	}
	return repos, nil
}

// GetRepoByID returns a repo by its ID
func (db *DB) GetRepoByID(id int64) (*Repo, error) {
	return db.selectRepo(context.Background(), "id = ?", id)
}

// GetRepoByName returns a repo by its display name
func (db *DB) GetRepoByName(name string) (*Repo, error) {
	return db.selectRepo(context.Background(), "name = ?", name)
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
	var statusRows []struct {
		Status JobStatus `bun:"status"`
		Count  int       `bun:"count"`
	}
	if err := db.bun.NewSelect().
		Table("review_jobs").
		Column("status").
		ColumnExpr("COUNT(*) AS count").
		Where("repo_id = ?", repoID).
		GroupExpr("status").
		Scan(context.Background(), &statusRows); err != nil {
		return nil, err
	}

	for _, row := range statusRows {
		stats.TotalJobs += row.Count
		switch row.Status {
		case JobStatusQueued:
			stats.QueuedJobs = row.Count
		case JobStatusRunning:
			stats.RunningJobs = row.Count
		case JobStatusDone:
			stats.CompletedJobs = row.Count
		case JobStatusFailed:
			stats.FailedJobs = row.Count
		}
	}

	// Get review verdict counts (P/F from output)
	// Closed/open counts preserve the legacy prompt-job exclusion.
	var reviewRows []struct {
		Output      string  `bun:"output"`
		Closed      bool    `bun:"closed"`
		VerdictBool *int64  `bun:"verdict_bool"`
		CommitID    *int64  `bun:"commit_id"`
		GitRef      string  `bun:"git_ref"`
		JobType     *string `bun:"job_type"`
	}
	if err := db.bun.NewSelect().
		TableExpr("reviews AS r").
		ColumnExpr("r.output").
		ColumnExpr("r.closed").
		ColumnExpr("r.verdict_bool").
		ColumnExpr("rj.commit_id").
		ColumnExpr("rj.git_ref").
		ColumnExpr("rj.job_type").
		Join("JOIN review_jobs AS rj ON r.job_id = rj.id").
		Where("rj.repo_id = ?", repoID).
		Scan(context.Background(), &reviewRows); err != nil {
		return nil, err
	}

	for _, row := range reviewRows {
		job := ReviewJob{
			CommitID: row.CommitID,
			GitRef:   row.GitRef,
			JobType:  stringValue(row.JobType),
		}

		if job.CommitID == nil && job.GitRef == "prompt" {
			continue
		}

		var verdictBool sql.NullInt64
		if row.VerdictBool != nil {
			verdictBool = sql.NullInt64{Int64: *row.VerdictBool, Valid: true}
		}
		applyJobVerdict(&job, verdictBool, row.Output)
		if job.Verdict != nil {
			if *job.Verdict == verdictPass {
				stats.PassedReviews++
			} else {
				stats.FailedReviews++
			}
		}

		if row.Closed {
			stats.ClosedReviews++
		} else {
			stats.OpenReviews++
		}
	}

	return stats, nil
}

// ErrRepoHasJobs is returned when trying to delete a repo with jobs without cascade
var ErrRepoHasJobs = errors.New("repository has existing jobs; use cascade to delete them")

// DeleteRepo deletes a repo and optionally its associated data
// If cascade is true, also deletes all jobs, reviews, and responses for the repo
// If cascade is false and jobs exist, returns ErrRepoHasJobs
func (db *DB) DeleteRepo(repoID int64, cascade bool) error {
	// SQLite transaction control remains raw so BEGIN IMMEDIATE acquires the
	// write lock before the count. All reads and mutations stay on the same
	// caller-owned connection through Bun.
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	// BEGIN IMMEDIATE acquires a write lock immediately, preventing races
	if _, err := db.bun.NewRaw("BEGIN IMMEDIATE").Conn(conn).Exec(ctx); err != nil {
		return err
	}

	// Ensure rollback on error
	committed := false
	defer func() {
		if !committed {
			if _, err := db.bun.NewRaw("ROLLBACK").Conn(conn).Exec(ctx); err != nil {
				log.Printf("repos DeleteRepo: rollback failed: %v", err)
			}
		}
	}()

	// Check for existing jobs (within transaction for consistency)
	jobCount, err := db.bun.NewSelect().Conn(conn).Table("review_jobs").Where("repo_id = ?", repoID).Count(ctx)
	if err != nil {
		return err
	}

	if !cascade && jobCount > 0 {
		return ErrRepoHasJobs
	}

	if cascade {
		// Delete in correct order due to foreign keys
		// 1a. Delete responses for jobs in this repo (job_id based)
		_, err := db.bun.NewDelete().Conn(conn).Table("responses").
			Where("job_id IN (SELECT id FROM review_jobs WHERE repo_id = ?)", repoID).Exec(ctx)
		if err != nil {
			return err
		}

		// 1b. Delete responses for commits in this repo (legacy commit_id based)
		_, err = db.bun.NewDelete().Conn(conn).Table("responses").
			Where("commit_id IN (SELECT id FROM commits WHERE repo_id = ?)", repoID).Exec(ctx)
		if err != nil {
			return err
		}

		// 2. Delete reviews for jobs in this repo
		_, err = db.bun.NewDelete().Conn(conn).Table("reviews").
			Where("job_id IN (SELECT id FROM review_jobs WHERE repo_id = ?)", repoID).Exec(ctx)
		if err != nil {
			return err
		}

		// 3. Delete jobs for this repo
		_, err = db.bun.NewDelete().Conn(conn).Table("review_jobs").Where("repo_id = ?", repoID).Exec(ctx)
		if err != nil {
			return err
		}

		// 4. Delete commits for this repo
		_, err = db.bun.NewDelete().Conn(conn).Table("commits").Where("repo_id = ?", repoID).Exec(ctx)
		if err != nil {
			return err
		}
	}

	// Delete the repo itself
	result, err := db.bun.NewDelete().Conn(conn).Table("repos").Where("id = ?", repoID).Exec(ctx)
	if err != nil {
		return err
	}
	affected, _ := result.RowsAffected()
	if affected == 0 {
		return sql.ErrNoRows
	}

	if _, err := db.bun.NewRaw("COMMIT").Conn(conn).Exec(ctx); err != nil {
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

	// SQLite transaction control remains raw so BEGIN IMMEDIATE keeps repo,
	// commit, and job reassignment atomic on one Bun-managed connection.
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		return 0, err
	}
	defer conn.Close()

	if _, err := db.bun.NewRaw("BEGIN IMMEDIATE").Conn(conn).Exec(ctx); err != nil {
		return 0, err
	}

	committed := false
	defer func() {
		if !committed {
			if _, err := db.bun.NewRaw("ROLLBACK").Conn(conn).Exec(ctx); err != nil {
				log.Printf("repos MergeRepos: rollback failed: %v", err)
			}
		}
	}()

	// Move all commits from source to target
	// Note: commits.sha is UNIQUE, so this will fail if both repos have
	// commits with the same SHA (which shouldn't happen for the same git repo)
	// Commit-based responses (legacy) are tied to commit_id which remains valid
	_, err = db.bun.NewUpdate().Conn(conn).Table("commits").Set("repo_id = ?", targetRepoID).
		Where("repo_id = ?", sourceRepoID).Exec(ctx)
	if err != nil {
		return 0, err
	}

	// Move all jobs from source to target
	result, err := db.bun.NewUpdate().Conn(conn).Table("review_jobs").Set("repo_id = ?", targetRepoID).
		Where("repo_id = ?", sourceRepoID).Exec(ctx)
	if err != nil {
		return 0, err
	}
	affected, _ := result.RowsAffected()

	// Delete the source repo (now empty)
	_, err = db.bun.NewDelete().Conn(conn).Table("repos").Where("id = ?", sourceRepoID).Exec(ctx)
	if err != nil {
		return 0, err
	}

	if _, err := db.bun.NewRaw("COMMIT").Conn(conn).Exec(ctx); err != nil {
		return 0, err
	}
	committed = true

	return affected, nil
}
