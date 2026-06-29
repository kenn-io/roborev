package storage

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/roborev/internal/config"
)

// Sync state keys
const (
	SyncStateMachineID        = "machine_id"
	SyncStateLastJobCursor    = "last_job_cursor"    // ID of last synced job
	SyncStateLastReviewCursor = "last_review_cursor" // Composite cursor for reviews (updated_at,id)
	SyncStateLastResponseID   = "last_response_id"   // inserted_at/id cursor of last synced response
	SyncStateSyncTargetID     = "sync_target_id"     // Database ID of last synced Postgres
)

// GetSyncState retrieves a value from the sync_state table.
// Returns empty string if key doesn't exist.
func (db *DB) GetSyncState(key string) (string, error) {
	var value string
	err := db.QueryRow(`SELECT value FROM sync_state WHERE key = ?`, key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("get sync state %s: %w", key, err)
	}
	return value, nil
}

// SetSyncState sets a value in the sync_state table (upsert).
func (db *DB) SetSyncState(key, value string) error {
	_, err := db.Exec(`
		INSERT INTO sync_state (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value
	`, key, value)
	if err != nil {
		return fmt.Errorf("set sync state %s: %w", key, err)
	}
	return nil
}

// GetOrCreateSyncStateValue returns a durable key/value entry, creating it when absent.
// Empty stored values are treated as missing and replaced.
func (db *DB) GetOrCreateSyncStateValue(key string, create func() (string, error)) (string, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return "", errors.New("sync state key is required")
	}
	if create == nil {
		return "", errors.New("sync state create func is required")
	}

	value, err := db.GetSyncState(key)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(value) != "" {
		return value, nil
	}

	created, err := create()
	if err != nil {
		return "", err
	}
	created = strings.TrimSpace(created)
	if created == "" {
		return "", errors.New("created sync state value is required")
	}

	if value == "" {
		_, err = db.Exec(`
			INSERT OR IGNORE INTO sync_state (key, value) VALUES (?, ?)
		`, key, created)
	} else {
		_, err = db.Exec(`UPDATE sync_state SET value = ? WHERE key = ?`, created, key)
	}
	if err != nil {
		return "", fmt.Errorf("create sync state %s: %w", key, err)
	}

	value, err = db.GetSyncState(key)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(value) == "" {
		if err := db.SetSyncState(key, created); err != nil {
			return "", err
		}
		return created, nil
	}
	return value, nil
}

// GetMachineID returns this machine's unique identifier, creating one if it doesn't exist.
// Uses INSERT OR IGNORE + SELECT to ensure concurrency-safe behavior.
// Treats empty values as missing and regenerates.
func (db *DB) GetMachineID() (string, error) {
	// Try to insert a new ID, ignoring if one already exists
	newID := GenerateUUID()
	_, err := db.Exec(`
		INSERT OR IGNORE INTO sync_state (key, value) VALUES (?, ?)
	`, SyncStateMachineID, newID)
	if err != nil {
		return "", fmt.Errorf("insert machine ID: %w", err)
	}

	// Always select the stored value (either ours or a concurrent caller's)
	var id string
	err = db.QueryRow(`SELECT value FROM sync_state WHERE key = ?`, SyncStateMachineID).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("get machine ID: %w", err)
	}

	// Treat empty value as missing (could happen from manual edits or past bugs)
	if id == "" {
		_, err = db.Exec(`UPDATE sync_state SET value = ? WHERE key = ?`, newID, SyncStateMachineID)
		if err != nil {
			return "", fmt.Errorf("update empty machine ID: %w", err)
		}
		return newID, nil
	}
	return id, nil
}

// BackfillSourceMachineID sets source_machine_id on existing rows that don't have one.
// This should be called when sync is first enabled.
func (db *DB) BackfillSourceMachineID() error {
	machineID, err := db.GetMachineID()
	if err != nil {
		return err
	}

	// Backfill review_jobs
	_, err = db.Exec(`UPDATE review_jobs SET source_machine_id = ? WHERE source_machine_id IS NULL`, machineID)
	if err != nil {
		return fmt.Errorf("backfill review_jobs source_machine_id: %w", err)
	}

	// Backfill reviews (updated_by_machine_id)
	_, err = db.Exec(`UPDATE reviews SET updated_by_machine_id = ? WHERE updated_by_machine_id IS NULL`, machineID)
	if err != nil {
		return fmt.Errorf("backfill reviews updated_by_machine_id: %w", err)
	}

	// Backfill responses
	_, err = db.Exec(`UPDATE responses SET source_machine_id = ? WHERE source_machine_id IS NULL`, machineID)
	if err != nil {
		return fmt.Errorf("backfill responses source_machine_id: %w", err)
	}

	return nil
}

// ClearAllSyncedAt clears all synced_at timestamps in the database.
// This is used when syncing to a new Postgres database to ensure
// all data gets re-synced.
func (db *DB) ClearAllSyncedAt() error {
	// Clear synced_at on review_jobs
	if _, err := db.Exec(`UPDATE review_jobs SET synced_at = NULL`); err != nil {
		return fmt.Errorf("clear review_jobs synced_at: %w", err)
	}
	// Clear synced_at on reviews
	if _, err := db.Exec(`UPDATE reviews SET synced_at = NULL`); err != nil {
		return fmt.Errorf("clear reviews synced_at: %w", err)
	}
	// Clear synced_at on responses
	if _, err := db.Exec(`UPDATE responses SET synced_at = NULL`); err != nil {
		return fmt.Errorf("clear responses synced_at: %w", err)
	}
	return nil
}

// BackfillRepoIdentities computes and sets identity for repos that don't have one.
// Uses config.ResolveRepoIdentity to ensure consistency with new repo creation.
// Returns the number of repos backfilled.
func (db *DB) BackfillRepoIdentities() (int, error) {
	// Get repos without identity
	rows, err := db.Query(`SELECT id, root_path FROM repos WHERE identity IS NULL OR identity = ''`)
	if err != nil {
		return 0, fmt.Errorf("query repos without identity: %w", err)
	}
	defer rows.Close()

	type repoInfo struct {
		id   int64
		path string
	}
	var repos []repoInfo
	for rows.Next() {
		var r repoInfo
		if err := rows.Scan(&r.id, &r.path); err != nil {
			return 0, fmt.Errorf("scan repo: %w", err)
		}
		repos = append(repos, r)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	backfilled := 0
	for _, r := range repos {
		// Use the same resolver as new repo creation to ensure consistency
		identity := config.ResolveRepoIdentity(r.path, nil)
		if identity == "" {
			// Shouldn't happen since ResolveRepoIdentity always returns something,
			// but skip if it does
			continue
		}

		if err := db.SetRepoIdentity(r.id, identity); err != nil {
			// May fail due to duplicate identity - skip
			continue
		}
		backfilled++
	}

	return backfilled, nil
}

// SetRepoIdentity sets the identity for a repo.
func (db *DB) SetRepoIdentity(repoID int64, identity string) error {
	_, err := db.Exec(`UPDATE repos SET identity = ? WHERE id = ?`, identity, repoID)
	if err != nil {
		return fmt.Errorf("set repo identity: %w", err)
	}
	return nil
}

// GetRepoByIdentity finds a repo by its identity.
// Returns nil if not found, error if duplicates exist.
func (db *DB) GetRepoByIdentity(identity string) (*Repo, error) {
	rows, err := db.Query(`
		SELECT id, root_path, name, created_at, identity
		FROM repos WHERE identity = ?
	`, identity)
	if err != nil {
		return nil, fmt.Errorf("query repo by identity: %w", err)
	}
	defer rows.Close()

	var r Repo
	var count int
	for rows.Next() {
		count++
		if count > 1 {
			return nil, fmt.Errorf("multiple repos found with identity %q", identity)
		}
		var createdAt string
		var identityVal sql.NullString
		if err := rows.Scan(&r.ID, &r.RootPath, &r.Name, &createdAt, &identityVal); err != nil {
			return nil, fmt.Errorf("scan repo: %w", err)
		}
		r.CreatedAt = parseSQLiteTime(createdAt)
		if identityVal.Valid {
			r.Identity = identityVal.String
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("get repo by identity: %w", err)
	}
	if count == 0 {
		return nil, nil
	}
	return &r, nil
}

// GetRepoByIdentityCaseInsensitive is like GetRepoByIdentity but uses
// case-insensitive comparison. Used by the CI poller since GitHub
// owner/repo names are case-insensitive.
// Excludes sync placeholders (root_path == identity) which don't have
// a real local checkout.
func (db *DB) GetRepoByIdentityCaseInsensitive(identity string) (*Repo, error) {
	rows, err := db.Query(`
		SELECT id, root_path, name, created_at, identity
		FROM repos WHERE LOWER(identity) = LOWER(?) AND root_path != identity
	`, identity)
	if err != nil {
		return nil, fmt.Errorf("query repo by identity (ci): %w", err)
	}
	defer rows.Close()

	var matches []Repo
	for rows.Next() {
		var r Repo
		var createdAt string
		var identityVal sql.NullString
		if err := rows.Scan(&r.ID, &r.RootPath, &r.Name, &createdAt, &identityVal); err != nil {
			return nil, fmt.Errorf("scan repo: %w", err)
		}
		r.CreatedAt = parseSQLiteTime(createdAt)
		if identityVal.Valid {
			r.Identity = identityVal.String
		}
		matches = append(matches, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("get repo by identity (ci): %w", err)
	}
	if len(matches) == 0 {
		return nil, nil
	}
	if len(matches) == 1 {
		return &matches[0], nil
	}
	return PreferAutoClone(matches), nil
}

// PreferAutoClone picks the best repo from multiple matches.
// It prefers auto-clones (root_path under {DataDir}/clones/) since CI
// manages those and they won't have dirty working tree state.
// If no auto-clone is found, it returns the most recently created repo.
// Sync placeholders (root_path == identity) are skipped defensively.
func PreferAutoClone(repos []Repo) *Repo {
	// Filter out sync placeholders that don't have a real checkout.
	filtered := repos[:0:0]
	for _, r := range repos {
		if r.RootPath != r.Identity {
			filtered = append(filtered, r)
		}
	}
	if len(filtered) == 0 {
		// All entries are placeholders — return first original match
		// so callers can handle it (findLocalRepo skips placeholders).
		return &repos[0]
	}
	repos = filtered

	if len(repos) == 1 {
		return &repos[0]
	}

	clonesPrefix := config.DataDir() + "/clones/"
	for i := range repos {
		if strings.HasPrefix(repos[i].RootPath, clonesPrefix) {
			return &repos[i]
		}
	}
	// No auto-clone found — return most recently created.
	best := &repos[0]
	for i := 1; i < len(repos); i++ {
		if repos[i].CreatedAt.After(best.CreatedAt) {
			best = &repos[i]
		}
	}
	return best
}

// SyncableJob contains job data needed for sync
type SyncableJob struct {
	ID                    int64
	UUID                  string
	RepoID                int64
	RepoIdentity          string
	CommitID              *int64
	CommitSHA             string
	CommitAuthor          string
	CommitSubject         string
	CommitTimestamp       time.Time
	GitRef                string
	SessionID             string
	Agent                 string
	Model                 string
	Provider              string
	RequestedModel        string
	RequestedProvider     string
	Reasoning             string
	JobType               string
	ReviewType            string
	PatchID               string
	Status                string
	Agentic               bool
	AgentInvoked          bool
	EnqueuedAt            time.Time
	StartedAt             *time.Time
	FinishedAt            *time.Time
	Prompt                string
	DiffContent           *string
	DirtyFiles            []string
	Error                 string
	TokenUsage            string
	WorktreePath          string
	Source                string
	MinSeverity           string
	BackupAgent           string
	BackupModel           string
	PanelRunUUID          string
	PanelRole             string
	PanelName             string
	PanelMemberName       string
	PanelMemberIndex      int
	PanelMemberConfigJSON string
	SourceMachineID       string
	UpdatedAt             time.Time
	UpdatedAtRaw          string
	StartedAtRaw          string
	FinishedAtRaw         string
}

// GetJobsToSync returns terminal jobs that need to be pushed to PostgreSQL.
// These are jobs created locally that haven't been synced or were updated since last sync.
func (db *DB) GetJobsToSync(machineID string, limit int) ([]SyncableJob, error) {
	rows, err := db.Query(`
		SELECT
			j.id, j.uuid, j.repo_id, COALESCE(r.identity, ''),
			j.commit_id, COALESCE(c.sha, ''), COALESCE(c.author, ''), COALESCE(c.subject, ''), COALESCE(c.timestamp, ''),
			j.git_ref, COALESCE(j.session_id, ''), j.agent, COALESCE(j.model, ''), COALESCE(j.provider, ''), COALESCE(j.requested_model, ''), COALESCE(j.requested_provider, ''), COALESCE(j.reasoning, ''), COALESCE(j.job_type, 'review'), COALESCE(j.review_type, ''), COALESCE(j.patch_id, ''), j.status, j.agentic, j.agent_invoked,
			j.enqueued_at, COALESCE(j.started_at, ''), COALESCE(j.finished_at, ''),
			COALESCE(j.prompt, ''), j.diff_content, j.dirty_files, COALESCE(j.error, ''), COALESCE(j.token_usage, ''),
			COALESCE(j.worktree_path, ''), COALESCE(j.source, ''), COALESCE(j.min_severity, ''), COALESCE(j.backup_agent, ''), COALESCE(j.backup_model, ''),
			COALESCE(j.panel_run_uuid, ''), COALESCE(j.panel_role, ''), COALESCE(j.panel_name, ''), COALESCE(j.panel_member_name, ''), COALESCE(j.panel_member_index, 0), COALESCE(j.panel_member_config_json, ''),
			j.source_machine_id, j.updated_at
		FROM review_jobs j
		JOIN repos r ON j.repo_id = r.id
		LEFT JOIN commits c ON j.commit_id = c.id
		WHERE j.status IN ('done', 'failed', 'canceled', 'skipped')
		AND j.source_machine_id = ?
		AND j.uuid IS NOT NULL
		AND (j.synced_at IS NULL OR `+sqliteNormalizedTimestampExpr("j.updated_at")+` > `+sqliteNormalizedTimestampExpr("j.synced_at")+`)
		ORDER BY j.id
		LIMIT ?
	`, machineID, limit)
	if err != nil {
		return nil, fmt.Errorf("query jobs to sync: %w", err)
	}
	defer rows.Close()

	var jobs []SyncableJob
	for rows.Next() {
		var j SyncableJob
		var enqueuedAt, startedAt, finishedAt, commitTimestamp, updatedAt string
		var commitID sql.NullInt64
		var diffContent sql.NullString
		var dirtyFiles sql.NullString

		err := rows.Scan(
			&j.ID, &j.UUID, &j.RepoID, &j.RepoIdentity,
			&commitID, &j.CommitSHA, &j.CommitAuthor, &j.CommitSubject, &commitTimestamp,
			&j.GitRef, &j.SessionID, &j.Agent, &j.Model, &j.Provider, &j.RequestedModel, &j.RequestedProvider, &j.Reasoning, &j.JobType, &j.ReviewType, &j.PatchID, &j.Status, &j.Agentic, &j.AgentInvoked,
			&enqueuedAt, &startedAt, &finishedAt,
			&j.Prompt, &diffContent, &dirtyFiles, &j.Error, &j.TokenUsage,
			&j.WorktreePath, &j.Source, &j.MinSeverity, &j.BackupAgent, &j.BackupModel,
			&j.PanelRunUUID, &j.PanelRole, &j.PanelName, &j.PanelMemberName, &j.PanelMemberIndex, &j.PanelMemberConfigJSON,
			&j.SourceMachineID, &updatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("scan job: %w", err)
		}

		if commitID.Valid {
			j.CommitID = &commitID.Int64
		}
		if diffContent.Valid {
			j.DiffContent = &diffContent.String
		}
		if dirtyFiles.Valid {
			j.DirtyFiles = decodeDirtyFiles(dirtyFiles.String)
		}
		j.EnqueuedAt = parseSQLiteTime(enqueuedAt)
		if startedAt != "" {
			t := parseSQLiteTime(startedAt)
			if !t.IsZero() {
				j.StartedAt = &t
			}
		}
		j.StartedAtRaw = startedAt
		if finishedAt != "" {
			t := parseSQLiteTime(finishedAt)
			if !t.IsZero() {
				j.FinishedAt = &t
			}
		}
		j.FinishedAtRaw = finishedAt
		if commitTimestamp != "" {
			j.CommitTimestamp = parseSQLiteTime(commitTimestamp)
		}
		j.UpdatedAt = parseSQLiteTime(updatedAt)
		j.UpdatedAtRaw = updatedAt

		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}

// MarkJobSynced updates the synced_at timestamp for a job
func (db *DB) MarkJobSynced(jobID int64) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := db.Exec(`UPDATE review_jobs SET synced_at = ? WHERE id = ?`, now, jobID)
	return err
}

// JobSyncMark identifies a pushed job by the snapshot fields that distinguish
// the exact terminal attempt that was pushed. MarkJobsSynced restores synced_at
// only when the row still matches all of them.
type JobSyncMark struct {
	ID            int64
	UpdatedAt     string // raw updated_at string from the pushed snapshot
	TokenUsage    string // token_usage from the pushed snapshot ("" when NULL)
	Status        string // status from the pushed snapshot (always terminal)
	SessionID     string // session_id from the pushed snapshot ("" when NULL)
	AgentInvoked  bool   // agent_invoked from the pushed snapshot
	Agent         string // agent from the pushed snapshot
	Model         string // model from the pushed snapshot ("" when NULL)
	Provider      string // provider from the pushed snapshot ("" when NULL)
	Error         string // error from the pushed snapshot ("" when NULL)
	StartedAtRaw  string // raw started_at string from the pushed snapshot ("" when NULL)
	FinishedAtRaw string // raw finished_at string from the pushed snapshot ("" when NULL)
}

// NewJobSyncMark captures the snapshot fields MarkJobsSynced compares to confirm
// a row still matches what was pushed. Keeping the field set in one place keeps
// the push loop and the WHERE clause in agreement.
func NewJobSyncMark(j SyncableJob) JobSyncMark {
	return JobSyncMark{
		ID:            j.ID,
		UpdatedAt:     j.UpdatedAtRaw,
		TokenUsage:    j.TokenUsage,
		Status:        j.Status,
		SessionID:     j.SessionID,
		AgentInvoked:  j.AgentInvoked,
		Agent:         j.Agent,
		Model:         j.Model,
		Provider:      j.Provider,
		Error:         j.Error,
		StartedAtRaw:  j.StartedAtRaw,
		FinishedAtRaw: j.FinishedAtRaw,
	}
}

// MarkJobsSynced advances synced_at only for jobs whose pushed snapshot still
// matches the current row. Any change since the snapshot leaves the row eligible
// for the next push instead of stranding it behind an advanced cursor; a missed
// match only costs a redundant re-push next cycle, which is safe.
//
// The guard compares fields that distinguish the pushed terminal attempt:
//
//   - updated_at and token_usage: a job is marked terminal before its token usage
//     is captured, and both writes use second precision, so a capture in the same
//     second leaves updated_at byte-identical while token_usage changes from NULL
//     to the cost. A capture that lands after this mark is handled at the source:
//     SaveJobTokenUsage clears synced_at so the row re-selects regardless.
//   - status, session_id, agent_invoked: an attempt reset (ReenqueueJob, RetryJob,
//     FailoverJob, ResetStaleJobs, PromoteClassifyToDesignReview) clears cost
//     metadata and synced_at in the same second. updated_at and token_usage alone
//     can still match the snapshot (e.g. an unpriced row re-enqueued in the same
//     second leaves both unchanged), so without these the stale mark would
//     overwrite the reset's synced_at = NULL and strand the cleared-cost state.
//     status moves off the terminal push set on every reset; session_id and
//     agent_invoked further pin the attempt against a same-second re-completion.
//   - agent, model, provider, error, started_at, finished_at: for sessionless,
//     unpriced attempts, the fields above can all match again after a reset plus
//     same-second terminal re-completion. These attempt metadata fields keep a
//     stale pushed snapshot from marking the new terminal attempt synced.
//
// All compared fields are stable on a terminal row that was not reset, so the
// tighter guard never wrongly skips an unchanged row.
func (db *DB) MarkJobsSynced(marks []JobSyncMark) error {
	if len(marks) == 0 {
		return nil
	}
	now := time.Now().UTC().Format(time.RFC3339)
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin mark jobs synced: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.Prepare(
		`UPDATE review_jobs SET synced_at = ?
		 WHERE id = ? AND updated_at = ? AND COALESCE(token_usage, '') = ?
		   AND status = ? AND COALESCE(session_id, '') = ? AND agent_invoked = ?
		   AND agent = ? AND COALESCE(model, '') = ? AND COALESCE(provider, '') = ?
		   AND COALESCE(error, '') = ? AND COALESCE(started_at, '') = ?
		   AND COALESCE(finished_at, '') = ?`)
	if err != nil {
		return fmt.Errorf("prepare mark jobs synced: %w", err)
	}
	defer stmt.Close()

	for _, m := range marks {
		invoked := 0
		if m.AgentInvoked {
			invoked = 1
		}
		if _, err := stmt.Exec(
			now, m.ID, m.UpdatedAt, m.TokenUsage, m.Status, m.SessionID, invoked,
			m.Agent, m.Model, m.Provider, m.Error, m.StartedAtRaw, m.FinishedAtRaw,
		); err != nil {
			return fmt.Errorf("mark job %d synced: %w", m.ID, err)
		}
	}
	return tx.Commit()
}

// SyncableReview contains review data needed for sync
type SyncableReview struct {
	ID                 int64
	UUID               string
	JobID              int64
	JobUUID            string
	Agent              string
	Prompt             string
	Output             string
	Closed             bool
	UpdatedByMachineID string
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// GetReviewsToSync returns reviews modified locally that need to be pushed.
// Only returns reviews whose parent job has already been synced.
func (db *DB) GetReviewsToSync(machineID string, limit int) ([]SyncableReview, error) {
	rows, err := db.Query(`
		SELECT
			r.id, r.uuid, r.job_id, j.uuid,
			r.agent, r.prompt, r.output, r.closed,
			r.updated_by_machine_id, r.created_at, r.updated_at
		FROM reviews r
		JOIN review_jobs j ON r.job_id = j.id
		WHERE r.updated_by_machine_id = ?
		AND r.uuid IS NOT NULL
		AND j.uuid IS NOT NULL
		AND j.synced_at IS NOT NULL
		AND (r.synced_at IS NULL OR `+sqliteNormalizedTimestampExpr("r.updated_at")+` > `+sqliteNormalizedTimestampExpr("r.synced_at")+`)
		ORDER BY r.id
		LIMIT ?
	`, machineID, limit)
	if err != nil {
		return nil, fmt.Errorf("query reviews to sync: %w", err)
	}
	defer rows.Close()

	var reviews []SyncableReview
	for rows.Next() {
		var r SyncableReview
		var createdAt, updatedAt string

		err := rows.Scan(
			&r.ID, &r.UUID, &r.JobID, &r.JobUUID,
			&r.Agent, &r.Prompt, &r.Output, &r.Closed,
			&r.UpdatedByMachineID, &createdAt, &updatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("scan review: %w", err)
		}

		r.CreatedAt = parseSQLiteTime(createdAt)
		r.UpdatedAt = parseSQLiteTime(updatedAt)
		reviews = append(reviews, r)
	}
	return reviews, rows.Err()
}

// MarkReviewSynced updates the synced_at timestamp for a review
func (db *DB) MarkReviewSynced(reviewID int64) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := db.Exec(`UPDATE reviews SET synced_at = ? WHERE id = ?`, now, reviewID)
	return err
}

// MarkReviewsSynced updates the synced_at timestamp for multiple reviews
func (db *DB) MarkReviewsSynced(reviewIDs []int64) error {
	if len(reviewIDs) == 0 {
		return nil
	}
	now := time.Now().UTC().Format(time.RFC3339)
	placeholders := make([]string, len(reviewIDs))
	args := make([]any, len(reviewIDs)+1)
	args[0] = now
	for i, id := range reviewIDs {
		placeholders[i] = "?"
		args[i+1] = id
	}
	query := fmt.Sprintf(`UPDATE reviews SET synced_at = ? WHERE id IN (%s)`,
		strings.Join(placeholders, ","))
	_, err := db.Exec(query, args...)
	return err
}

// SyncableResponse contains response data needed for sync
type SyncableResponse struct {
	ID              int64
	UUID            string
	JobID           int64
	JobUUID         string
	Responder       string
	Response        string
	SourceMachineID string
	CreatedAt       time.Time
}

// GetCommentsToSync returns comments created locally that need to be pushed.
// Only returns comments whose parent job has already been synced.
func (db *DB) GetCommentsToSync(machineID string, limit int) ([]SyncableResponse, error) {
	rows, err := db.Query(`
		SELECT
			r.id, r.uuid, r.job_id, j.uuid,
			r.responder, r.response, r.source_machine_id, r.created_at
		FROM responses r
		JOIN review_jobs j ON r.job_id = j.id
		WHERE r.source_machine_id = ?
		AND r.uuid IS NOT NULL
		AND j.uuid IS NOT NULL
		AND r.synced_at IS NULL
		AND j.synced_at IS NOT NULL
		ORDER BY r.id
		LIMIT ?
	`, machineID, limit)
	if err != nil {
		return nil, fmt.Errorf("query responses to sync: %w", err)
	}
	defer rows.Close()

	var responses []SyncableResponse
	for rows.Next() {
		var r SyncableResponse
		var createdAt string
		var jobID sql.NullInt64

		err := rows.Scan(
			&r.ID, &r.UUID, &jobID, &r.JobUUID,
			&r.Responder, &r.Response, &r.SourceMachineID, &createdAt,
		)
		if err != nil {
			return nil, fmt.Errorf("scan response: %w", err)
		}

		if jobID.Valid {
			r.JobID = jobID.Int64
		}
		r.CreatedAt = parseSQLiteTime(createdAt)
		responses = append(responses, r)
	}
	return responses, rows.Err()
}

// MarkCommentSynced updates the synced_at timestamp for a comment
func (db *DB) MarkCommentSynced(responseID int64) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := db.Exec(`UPDATE responses SET synced_at = ? WHERE id = ?`, now, responseID)
	return err
}

// MarkCommentsSynced updates the synced_at timestamp for multiple comments
func (db *DB) MarkCommentsSynced(responseIDs []int64) error {
	if len(responseIDs) == 0 {
		return nil
	}
	now := time.Now().UTC().Format(time.RFC3339)
	placeholders := make([]string, len(responseIDs))
	args := make([]any, len(responseIDs)+1)
	args[0] = now
	for i, id := range responseIDs {
		placeholders[i] = "?"
		args[i+1] = id
	}
	query := fmt.Sprintf(`UPDATE responses SET synced_at = ? WHERE id IN (%s)`,
		strings.Join(placeholders, ","))
	_, err := db.Exec(query, args...)
	return err
}

// UpsertPulledJob inserts or updates a job from PostgreSQL into SQLite.
// Sets synced_at to prevent re-pushing. Requires repo to exist.
func (db *DB) UpsertPulledJob(j PulledJob, repoID int64, commitID *int64) error {
	now := time.Now().UTC().Format(time.RFC3339)
	dirtyFilesJSON, err := encodeDirtyFiles(j.DirtyFiles)
	if err != nil {
		return err
	}
	_, err = db.Exec(`
		INSERT INTO review_jobs (
			uuid, repo_id, commit_id, git_ref, session_id, agent, model, provider, requested_model, requested_provider, reasoning, job_type, review_type, patch_id, status, agentic, agent_invoked,
			enqueued_at, started_at, finished_at, prompt, diff_content, dirty_files, error, token_usage,
			worktree_path, source, min_severity, backup_agent, backup_model,
			panel_run_uuid, panel_role, panel_name, panel_member_name, panel_member_index, panel_member_config_json,
			source_machine_id, updated_at, synced_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(uuid) DO UPDATE SET
			status = excluded.status,
			finished_at = excluded.finished_at,
			error = excluded.error,
			model = excluded.model,
			provider = excluded.provider,
			requested_model = excluded.requested_model,
			requested_provider = excluded.requested_provider,
			git_ref = excluded.git_ref,
			session_id = CASE WHEN excluded.status IN ('done', 'failed', 'canceled', 'skipped', 'applied', 'rebased') THEN excluded.session_id ELSE COALESCE(excluded.session_id, review_jobs.session_id) END,
			commit_id = excluded.commit_id,
			patch_id = excluded.patch_id,
			dirty_files = COALESCE(excluded.dirty_files, review_jobs.dirty_files),
			token_usage = CASE WHEN excluded.status IN ('done', 'failed', 'canceled', 'skipped', 'applied', 'rebased') THEN excluded.token_usage ELSE COALESCE(excluded.token_usage, review_jobs.token_usage) END,
			agent_invoked = CASE WHEN excluded.status IN ('done', 'failed', 'canceled', 'skipped', 'applied', 'rebased') THEN excluded.agent_invoked ELSE (review_jobs.agent_invoked OR excluded.agent_invoked) END,
			worktree_path = COALESCE(excluded.worktree_path, review_jobs.worktree_path),
			source = COALESCE(excluded.source, review_jobs.source),
			min_severity = excluded.min_severity,
			backup_agent = excluded.backup_agent,
			backup_model = excluded.backup_model,
			panel_run_uuid = excluded.panel_run_uuid,
			panel_role = excluded.panel_role,
			panel_name = excluded.panel_name,
			panel_member_name = excluded.panel_member_name,
			panel_member_index = excluded.panel_member_index,
			panel_member_config_json = excluded.panel_member_config_json,
			updated_at = excluded.updated_at,
			synced_at = ?
			WHERE review_jobs.status NOT IN ('applied', 'rebased')
			OR `+sqliteNormalizedTimestampExpr("review_jobs.updated_at")+` < `+sqliteNormalizedTimestampExpr("excluded.updated_at")+`
	`, j.UUID, repoID, commitID, j.GitRef, nullStr(j.SessionID), j.Agent, nullStr(j.Model), nullStr(j.Provider), nullStr(j.RequestedModel), nullStr(j.RequestedProvider), j.Reasoning, j.JobType,
		j.ReviewType, nullStr(j.PatchID), j.Status, j.Agentic, j.AgentInvoked, j.EnqueuedAt.Format(time.RFC3339),
		nullTimeStr(j.StartedAt), nullTimeStr(j.FinishedAt),
		nullStr(j.Prompt), j.DiffContent, nullStr(dirtyFilesJSON), nullStr(j.Error), nullStr(j.TokenUsage),
		nullStr(j.WorktreePath), nullStr(j.Source), normalizeMinSeverityForWrite(j.MinSeverity), j.BackupAgent, j.BackupModel,
		nullStr(j.PanelRunUUID), nullStr(j.PanelRole), nullStr(j.PanelName), nullStr(j.PanelMemberName), j.PanelMemberIndex, nullStr(j.PanelMemberConfigJSON),
		j.SourceMachineID, j.UpdatedAt.Format(time.RFC3339), now, now)
	return err
}

// UpsertPulledReview inserts or updates a review from PostgreSQL into SQLite.
func (db *DB) UpsertPulledReview(r PulledReview) error {
	// First, find the job_id by uuid
	var jobID int64
	err := db.QueryRow(`SELECT id FROM review_jobs WHERE uuid = ?`, r.JobUUID).Scan(&jobID)
	if errors.Is(err, sql.ErrNoRows) {
		// Job doesn't exist locally - skip this review (orphaned)
		return nil
	}
	if err != nil {
		return fmt.Errorf("find job for review: %w", err)
	}

	now := time.Now().UTC().Format(time.RFC3339)
	var verdictBool any
	if r.Output != "" {
		verdictBool = verdictToBool(ParseVerdict(r.Output))
	}
	_, err = db.Exec(`
		INSERT INTO reviews (
			uuid, job_id, agent, prompt, output, closed,
			verdict_bool, updated_by_machine_id, created_at, updated_at, synced_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(uuid) DO UPDATE SET
			closed = excluded.closed,
			verdict_bool = COALESCE(reviews.verdict_bool, excluded.verdict_bool),
			updated_by_machine_id = excluded.updated_by_machine_id,
			updated_at = excluded.updated_at,
			synced_at = ?
			WHERE `+sqliteNormalizedTimestampExpr("reviews.updated_at")+` < `+sqliteNormalizedTimestampExpr("excluded.updated_at")+`
	`, r.UUID, jobID, r.Agent, r.Prompt, r.Output, r.Closed,
		verdictBool,
		r.UpdatedByMachineID, r.CreatedAt.Format(time.RFC3339), r.UpdatedAt.Format(time.RFC3339), now, now)
	return err
}

// UpsertPulledResponse inserts a response from PostgreSQL into SQLite.
func (db *DB) UpsertPulledResponse(r PulledResponse) error {
	// First, find the job_id by uuid
	var jobID int64
	err := db.QueryRow(`SELECT id FROM review_jobs WHERE uuid = ?`, r.JobUUID).Scan(&jobID)
	if errors.Is(err, sql.ErrNoRows) {
		// Job doesn't exist locally - skip this response (orphaned)
		return nil
	}
	if err != nil {
		return fmt.Errorf("find job for response: %w", err)
	}

	now := time.Now().UTC().Format(time.RFC3339)
	_, err = db.Exec(`
		INSERT INTO responses (
			uuid, job_id, responder, response, source_machine_id, created_at, synced_at
		) VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(uuid) DO NOTHING
	`, r.UUID, jobID, r.Responder, r.Response, r.SourceMachineID, r.CreatedAt.Format(time.RFC3339), now)
	return err
}

// GetKnownJobUUIDs returns UUIDs of all jobs that have a UUID.
// Used to filter reviews when pulling from PostgreSQL.
func (db *DB) GetKnownJobUUIDs() ([]string, error) {
	rows, err := db.Query(`SELECT uuid FROM review_jobs WHERE uuid IS NOT NULL`)
	if err != nil {
		return nil, fmt.Errorf("query job UUIDs: %w", err)
	}
	defer rows.Close()

	var uuids []string
	for rows.Next() {
		var uuid string
		if err := rows.Scan(&uuid); err != nil {
			return nil, fmt.Errorf("scan UUID: %w", err)
		}
		uuids = append(uuids, uuid)
	}
	return uuids, rows.Err()
}

// GetOrCreateRepoByIdentity finds or creates a repo for syncing by identity.
// The logic is:
//  1. If exactly one local repo has this identity, use it (always preferred)
//  2. If a placeholder repo exists (root_path == identity), use it
//  3. If 0 or 2+ local repos have this identity, create a placeholder
//
// This ensures synced jobs attach to the right repo:
//   - Single clone: jobs attach directly to the local repo
//   - Multiple clones: jobs attach to a neutral placeholder
//   - No local clone: placeholder serves as a sync-only repo
//
// Note: Single local repos are always preferred, even if a placeholder exists
// from a previous sync (e.g., when there were 0 or 2+ clones before).
func (db *DB) GetOrCreateRepoByIdentity(identity string) (int64, error) {
	// First, check for local repos with this identity
	// (excluding placeholders where root_path == identity)
	rows, err := db.Query(`SELECT id FROM repos WHERE identity = ? AND root_path != ?`, identity, identity)
	if err != nil {
		return 0, fmt.Errorf("find repos by identity: %w", err)
	}
	defer rows.Close()

	var repoIDs []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return 0, fmt.Errorf("scan repo id: %w", err)
		}
		repoIDs = append(repoIDs, id)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterate repos: %w", err)
	}

	// If exactly one local repo exists, always use it (even if placeholder exists)
	if len(repoIDs) == 1 {
		return repoIDs[0], nil
	}

	// 0 or 2+ local repos - look for existing placeholder
	var placeholderID int64
	err = db.QueryRow(`SELECT id FROM repos WHERE root_path = ? AND identity = ?`, identity, identity).Scan(&placeholderID)
	if err == nil {
		return placeholderID, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("find placeholder repo: %w", err)
	}

	// No placeholder exists - create one
	// Use extracted repo name for display, but root_path stays as identity to mark it as a placeholder
	displayName := ExtractRepoNameFromIdentity(identity)
	result, err := db.Exec(`
		INSERT INTO repos (root_path, name, identity)
		VALUES (?, ?, ?)
	`, identity, displayName, identity)
	if err != nil {
		return 0, fmt.Errorf("create placeholder repo: %w", err)
	}
	return result.LastInsertId()
}

// ExtractRepoNameFromIdentity extracts a human-readable name from a git identity.
// Examples:
//   - "git@github.com:org/repo.git" -> "repo"
//   - "https://github.com/org/my-project.git" -> "my-project"
//   - "https://github.com/org/repo" -> "repo"
//   - "" -> "unknown"
func ExtractRepoNameFromIdentity(identity string) string {
	// Handle empty identity
	if identity == "" {
		return "unknown"
	}

	// Remove trailing .git if present
	name := strings.TrimSuffix(identity, ".git")

	// Find the last path component
	// Handle both SSH (git@host:path) and HTTPS (https://host/path) formats
	if idx := strings.LastIndex(name, "/"); idx >= 0 {
		name = name[idx+1:]
	} else if idx := strings.LastIndex(name, ":"); idx >= 0 {
		// SSH format like git@github.com:org/repo - get part after last /
		afterColon := name[idx+1:]
		if slashIdx := strings.LastIndex(afterColon, "/"); slashIdx >= 0 {
			name = afterColon[slashIdx+1:]
		} else {
			name = afterColon
		}
	}

	// If we ended up with empty string, use the identity as-is
	if name == "" {
		return identity
	}
	return name
}

// GetOrCreateCommitByRepoAndSHA finds or creates a commit.
func (db *DB) GetOrCreateCommitByRepoAndSHA(repoID int64, sha, author, subject string, timestamp time.Time) (int64, error) {
	// Try to find existing
	var id int64
	err := db.QueryRow(`SELECT id FROM commits WHERE repo_id = ? AND sha = ?`, repoID, sha).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("find commit: %w", err)
	}

	// Create
	result, err := db.Exec(`
		INSERT INTO commits (repo_id, sha, author, subject, timestamp)
		VALUES (?, ?, ?, ?, ?)
	`, repoID, sha, author, subject, timestamp.Format(time.RFC3339))
	if err != nil {
		return 0, fmt.Errorf("create commit: %w", err)
	}
	return result.LastInsertId()
}

// nullStr returns nil if s is empty, otherwise returns s
func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// nullTimeStr formats a time pointer or returns nil
func nullTimeStr(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.Format(time.RFC3339)
}
