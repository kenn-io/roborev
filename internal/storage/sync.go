package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/uptrace/bun"

	"go.kenn.io/roborev/internal/config"
)

// Sync state keys
const (
	SyncStateMachineID        = "machine_id"
	SyncStateLastJobCursor    = "last_job_cursor"    // ID of last synced job
	SyncStateLastReviewCursor = "last_review_cursor" // Composite cursor for reviews (updated_at,id)
	SyncStateLastResponseID   = "last_response_id"   // inserted_at/id cursor of last synced response
	SyncStateSyncTargetID     = "sync_target_id"     // Database ID of last synced Postgres
	SyncStateDatabaseID       = "database_id"        // Stable identity of this local SQLite database
)

// GetSyncState retrieves a value from the sync_state table.
// Returns empty string if key doesn't exist.
func (db *DB) GetSyncState(key string) (string, error) {
	var row syncStateRow
	err := db.bun.NewSelect().
		Model(&row).
		Column("value").
		Where("key = ?", key).
		Scan(context.Background())
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("get sync state %s: %w", key, err)
	}
	return row.Value, nil
}

// SetSyncState sets a value in the sync_state table (upsert).
func (db *DB) SetSyncState(key, value string) error {
	if err := upsertKeyValue(context.Background(), db.bun, syncStateTable, key, value); err != nil {
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

	row := syncStateRow{Key: key, Value: created}
	if _, err = db.bun.NewInsert().
		Model(&row).
		Column("key", "value").
		On("CONFLICT (key) DO NOTHING").
		Exec(context.Background()); err != nil {
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
// Uses ON CONFLICT DO NOTHING + SELECT to ensure concurrency-safe behavior.
// Treats empty values as missing and regenerates.
func (db *DB) GetMachineID() (string, error) {
	// Try to insert a new ID, ignoring if one already exists
	newID := GenerateUUID()
	row := syncStateRow{Key: SyncStateMachineID, Value: newID}
	_, err := db.bun.NewInsert().
		Model(&row).
		Column("key", "value").
		On("CONFLICT (key) DO NOTHING").
		Exec(context.Background())
	if err != nil {
		return "", fmt.Errorf("insert machine ID: %w", err)
	}

	// Always select the stored value (either ours or a concurrent caller's)
	err = db.bun.NewSelect().
		Model(&row).
		Column("value").
		Where("key = ?", SyncStateMachineID).
		Scan(context.Background())
	if err != nil {
		return "", fmt.Errorf("get machine ID: %w", err)
	}

	// Treat empty value as missing (could happen from manual edits or past bugs)
	if row.Value == "" {
		_, err = db.bun.NewUpdate().
			Model((*syncStateRow)(nil)).
			Set("value = ?", newID).
			Where("key = ?", SyncStateMachineID).
			Exec(context.Background())
		if err != nil {
			return "", fmt.Errorf("update empty machine ID: %w", err)
		}
		return newID, nil
	}
	return row.Value, nil
}

// GetDatabaseID returns this local database's unique identifier, creating one
// if it doesn't exist. It changes only when the SQLite database is recreated.
func (db *DB) GetDatabaseID() (string, error) {
	id, err := db.GetOrCreateSyncStateValue(SyncStateDatabaseID, func() (string, error) {
		return GenerateUUID(), nil
	})
	if err != nil {
		return "", fmt.Errorf("get database ID: %w", err)
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
	_, err = db.bun.NewUpdate().Model((*jobRow)(nil)).Set("source_machine_id = ?", machineID).
		Where("source_machine_id IS NULL").Exec(context.Background())
	if err != nil {
		return fmt.Errorf("backfill review_jobs source_machine_id: %w", err)
	}

	// Backfill reviews (updated_by_machine_id)
	_, err = db.bun.NewUpdate().Model((*reviewRow)(nil)).Set("updated_by_machine_id = ?", machineID).
		Where("updated_by_machine_id IS NULL").Exec(context.Background())
	if err != nil {
		return fmt.Errorf("backfill reviews updated_by_machine_id: %w", err)
	}

	// Backfill responses
	_, err = db.bun.NewUpdate().Model((*responseRow)(nil)).Set("source_machine_id = ?", machineID).
		Where("source_machine_id IS NULL").Exec(context.Background())
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
	if _, err := db.bun.NewUpdate().Model((*jobRow)(nil)).Set("synced_at = NULL").Where("1 = 1").Exec(context.Background()); err != nil {
		return fmt.Errorf("clear review_jobs synced_at: %w", err)
	}
	// Clear synced_at on reviews
	if _, err := db.bun.NewUpdate().Model((*reviewRow)(nil)).Set("synced_at = NULL").Where("1 = 1").Exec(context.Background()); err != nil {
		return fmt.Errorf("clear reviews synced_at: %w", err)
	}
	// Clear synced_at on responses
	if _, err := db.bun.NewUpdate().Model((*responseRow)(nil)).Set("synced_at = NULL").Where("1 = 1").Exec(context.Background()); err != nil {
		return fmt.Errorf("clear responses synced_at: %w", err)
	}
	return nil
}

// BackfillRepoIdentities computes and sets identity for repos that don't have one.
// Uses config.ResolveRepoIdentity to ensure consistency with new repo creation.
// Returns the number of repos backfilled.
func (db *DB) BackfillRepoIdentities() (int, error) {
	var rows []repoRow
	if err := db.bun.NewSelect().
		Model(&rows).
		Column("id", "root_path").
		Where("identity IS NULL OR identity = ''").
		Scan(context.Background()); err != nil {
		return 0, fmt.Errorf("query repos without identity: %w", err)
	}

	backfilled := 0
	for _, row := range rows {
		// Use the same resolver as new repo creation to ensure consistency
		identity := config.ResolveRepoIdentity(row.RootPath, nil)
		if identity == "" {
			// Shouldn't happen since ResolveRepoIdentity always returns something,
			// but skip if it does
			continue
		}

		if err := db.SetRepoIdentity(row.ID, identity); err != nil {
			// May fail due to duplicate identity - skip
			continue
		}
		backfilled++
	}

	return backfilled, nil
}

// SetRepoIdentity sets the identity for a repo.
func (db *DB) SetRepoIdentity(repoID int64, identity string) error {
	_, err := db.bun.NewUpdate().
		Model((*repoRow)(nil)).
		Set("identity = ?", identity).
		Where("id = ?", repoID).
		Exec(context.Background())
	if err != nil {
		return fmt.Errorf("set repo identity: %w", err)
	}
	return nil
}

// GetRepoByIdentity finds a repo by its identity.
// Returns nil if not found, error if duplicates exist.
func (db *DB) GetRepoByIdentity(identity string) (*Repo, error) {
	var rows []repoRow
	if err := db.bun.NewSelect().
		Model(&rows).
		Column(repoColumns...).
		Where("identity = ?", identity).
		Scan(context.Background()); err != nil {
		return nil, fmt.Errorf("query repo by identity: %w", err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	if len(rows) > 1 {
		return nil, fmt.Errorf("multiple repos found with identity %q", identity)
	}
	repo := rows[0].toModel()
	return &repo, nil
}

// GetRepoByIdentityCaseInsensitive is like GetRepoByIdentity but uses
// case-insensitive comparison. Used by the CI poller since GitHub
// owner/repo names are case-insensitive.
// Excludes sync placeholders (root_path == identity) which don't have
// a real local checkout.
func (db *DB) GetRepoByIdentityCaseInsensitive(identity string) (*Repo, error) {
	var rows []repoRow
	if err := db.bun.NewSelect().
		Model(&rows).
		Column(repoColumns...).
		Where("LOWER(identity) = LOWER(?)", identity).
		Where("root_path != identity").
		Scan(context.Background()); err != nil {
		return nil, fmt.Errorf("query repo by identity (ci): %w", err)
	}

	matches := make([]Repo, 0, len(rows))
	for _, row := range rows {
		matches = append(matches, row.toModel())
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

type syncableJobRow struct {
	ID                    int64   `bun:"id"`
	UUID                  string  `bun:"uuid"`
	RepoID                int64   `bun:"repo_id"`
	RepoIdentity          string  `bun:"repo_identity"`
	CommitID              *int64  `bun:"commit_id"`
	CommitSHA             string  `bun:"commit_sha"`
	CommitAuthor          string  `bun:"commit_author"`
	CommitSubject         string  `bun:"commit_subject"`
	CommitTimestampRaw    string  `bun:"commit_timestamp_raw"`
	GitRef                string  `bun:"git_ref"`
	SessionID             string  `bun:"session_id"`
	Agent                 string  `bun:"agent"`
	Model                 string  `bun:"model"`
	Provider              string  `bun:"provider"`
	RequestedModel        string  `bun:"requested_model"`
	RequestedProvider     string  `bun:"requested_provider"`
	Reasoning             string  `bun:"reasoning"`
	JobType               string  `bun:"job_type"`
	ReviewType            string  `bun:"review_type"`
	PatchID               string  `bun:"patch_id"`
	Status                string  `bun:"status"`
	Agentic               bool    `bun:"agentic"`
	AgentInvoked          bool    `bun:"agent_invoked"`
	EnqueuedAtRaw         string  `bun:"enqueued_at_raw"`
	StartedAtRaw          string  `bun:"started_at_raw"`
	FinishedAtRaw         string  `bun:"finished_at_raw"`
	Prompt                string  `bun:"prompt"`
	DiffContent           *string `bun:"diff_content"`
	DirtyFiles            *string `bun:"dirty_files"`
	Error                 string  `bun:"error"`
	TokenUsage            string  `bun:"token_usage"`
	WorktreePath          string  `bun:"worktree_path"`
	Source                string  `bun:"source"`
	MinSeverity           string  `bun:"min_severity"`
	BackupAgent           string  `bun:"backup_agent"`
	BackupModel           string  `bun:"backup_model"`
	PanelRunUUID          string  `bun:"panel_run_uuid"`
	PanelRole             string  `bun:"panel_role"`
	PanelName             string  `bun:"panel_name"`
	PanelMemberName       string  `bun:"panel_member_name"`
	PanelMemberIndex      int     `bun:"panel_member_index"`
	PanelMemberConfigJSON string  `bun:"panel_member_config_json"`
	SourceMachineID       string  `bun:"source_machine_id"`
	UpdatedAtRaw          string  `bun:"updated_at_raw"`
}

func (row syncableJobRow) toModel() SyncableJob {
	job := SyncableJob{
		ID: row.ID, UUID: row.UUID, RepoID: row.RepoID, RepoIdentity: row.RepoIdentity,
		CommitID: cloneInt64Pointer(row.CommitID), CommitSHA: row.CommitSHA,
		CommitAuthor: row.CommitAuthor, CommitSubject: row.CommitSubject,
		GitRef: row.GitRef, SessionID: row.SessionID, Agent: row.Agent, Model: row.Model,
		Provider: row.Provider, RequestedModel: row.RequestedModel,
		RequestedProvider: row.RequestedProvider, Reasoning: row.Reasoning,
		JobType: row.JobType, ReviewType: row.ReviewType, PatchID: row.PatchID,
		Status: row.Status, Agentic: row.Agentic, AgentInvoked: row.AgentInvoked,
		EnqueuedAt: parseSQLiteTime(row.EnqueuedAtRaw), Prompt: row.Prompt,
		DiffContent: cloneStringPointer(row.DiffContent), Error: row.Error,
		TokenUsage: row.TokenUsage, WorktreePath: row.WorktreePath, Source: row.Source,
		MinSeverity: row.MinSeverity, BackupAgent: row.BackupAgent, BackupModel: row.BackupModel,
		PanelRunUUID: row.PanelRunUUID, PanelRole: row.PanelRole, PanelName: row.PanelName,
		PanelMemberName: row.PanelMemberName, PanelMemberIndex: row.PanelMemberIndex,
		PanelMemberConfigJSON: row.PanelMemberConfigJSON, SourceMachineID: row.SourceMachineID,
		UpdatedAt: parseSQLiteTime(row.UpdatedAtRaw), UpdatedAtRaw: row.UpdatedAtRaw,
		StartedAtRaw: row.StartedAtRaw, FinishedAtRaw: row.FinishedAtRaw,
	}
	if row.CommitTimestampRaw != "" {
		job.CommitTimestamp = parseSQLiteTime(row.CommitTimestampRaw)
	}
	if row.StartedAtRaw != "" {
		startedAt := parseSQLiteTime(row.StartedAtRaw)
		if !startedAt.IsZero() {
			job.StartedAt = &startedAt
		}
	}
	if row.FinishedAtRaw != "" {
		finishedAt := parseSQLiteTime(row.FinishedAtRaw)
		if !finishedAt.IsZero() {
			job.FinishedAt = &finishedAt
		}
	}
	if row.DirtyFiles != nil {
		job.DirtyFiles = decodeDirtyFiles(*row.DirtyFiles)
	}
	return job
}

// GetJobsToSync returns terminal jobs that need to be pushed to PostgreSQL.
// These are jobs created locally that haven't been synced or were updated since last sync.
func (db *DB) GetJobsToSync(machineID string, limit int) ([]SyncableJob, error) {
	if limit <= 0 {
		return nil, nil
	}
	var rows []syncableJobRow
	err := db.bun.NewSelect().TableExpr("review_jobs AS j").
		ColumnExpr("j.id AS id").ColumnExpr("j.uuid AS uuid").ColumnExpr("j.repo_id AS repo_id").
		ColumnExpr("COALESCE(r.identity, '') AS repo_identity").ColumnExpr("j.commit_id AS commit_id").
		ColumnExpr("COALESCE(c.sha, '') AS commit_sha").ColumnExpr("COALESCE(c.author, '') AS commit_author").
		ColumnExpr("COALESCE(c.subject, '') AS commit_subject").
		ColumnExpr("COALESCE(c.timestamp, '') AS commit_timestamp_raw").
		ColumnExpr("j.git_ref AS git_ref").ColumnExpr("COALESCE(j.session_id, '') AS session_id").
		ColumnExpr("j.agent AS agent").ColumnExpr("COALESCE(j.model, '') AS model").
		ColumnExpr("COALESCE(j.provider, '') AS provider").
		ColumnExpr("COALESCE(j.requested_model, '') AS requested_model").
		ColumnExpr("COALESCE(j.requested_provider, '') AS requested_provider").
		ColumnExpr("COALESCE(j.reasoning, '') AS reasoning").
		ColumnExpr("COALESCE(j.job_type, 'review') AS job_type").
		ColumnExpr("COALESCE(j.review_type, '') AS review_type").
		ColumnExpr("COALESCE(j.patch_id, '') AS patch_id").ColumnExpr("j.status AS status").
		ColumnExpr("j.agentic AS agentic").ColumnExpr("j.agent_invoked AS agent_invoked").
		ColumnExpr("j.enqueued_at AS enqueued_at_raw").
		ColumnExpr("COALESCE(j.started_at, '') AS started_at_raw").
		ColumnExpr("COALESCE(j.finished_at, '') AS finished_at_raw").
		ColumnExpr("COALESCE(j.prompt, '') AS prompt").ColumnExpr("j.diff_content AS diff_content").
		ColumnExpr("j.dirty_files AS dirty_files").ColumnExpr("COALESCE(j.error, '') AS error").
		ColumnExpr("COALESCE(j.token_usage, '') AS token_usage").
		ColumnExpr("COALESCE(j.worktree_path, '') AS worktree_path").
		ColumnExpr("COALESCE(j.source, '') AS source").
		ColumnExpr("COALESCE(j.min_severity, '') AS min_severity").
		ColumnExpr("COALESCE(j.backup_agent, '') AS backup_agent").
		ColumnExpr("COALESCE(j.backup_model, '') AS backup_model").
		ColumnExpr("COALESCE(j.panel_run_uuid, '') AS panel_run_uuid").
		ColumnExpr("COALESCE(j.panel_role, '') AS panel_role").
		ColumnExpr("COALESCE(j.panel_name, '') AS panel_name").
		ColumnExpr("COALESCE(j.panel_member_name, '') AS panel_member_name").
		ColumnExpr("COALESCE(j.panel_member_index, 0) AS panel_member_index").
		ColumnExpr("COALESCE(j.panel_member_config_json, '') AS panel_member_config_json").
		ColumnExpr("j.source_machine_id AS source_machine_id").ColumnExpr("j.updated_at AS updated_at_raw").
		Join("JOIN repos AS r ON j.repo_id = r.id").Join("LEFT JOIN commits AS c ON j.commit_id = c.id").
		Where("j.status IN ('done', 'failed', 'canceled', 'skipped')").
		Where("j.source_machine_id = ?", machineID).Where("j.uuid IS NOT NULL").
		Where("j.synced_at IS NULL OR "+sqliteNormalizedTimestampExpr("j.updated_at")+" > "+sqliteNormalizedTimestampExpr("j.synced_at")).
		OrderExpr("j.id").Limit(limit).Scan(context.Background(), &rows)
	if err != nil {
		return nil, fmt.Errorf("query jobs to sync: %w", err)
	}

	var jobs []SyncableJob
	for _, row := range rows {
		jobs = append(jobs, row.toModel())
	}
	return jobs, nil
}

// MarkJobSynced updates the synced_at timestamp for a job
func (db *DB) MarkJobSynced(jobID int64) error {
	now := dbTimeFromValue(time.Now())
	_, err := db.bun.NewUpdate().Model((*jobRow)(nil)).Set("synced_at = ?", now).
		Where("id = ?", jobID).Exec(context.Background())
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
	now := dbTimeFromValue(time.Now())
	tx, err := db.bun.BeginTx(context.Background(), nil)
	if err != nil {
		return fmt.Errorf("begin mark jobs synced: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, m := range marks {
		if _, err := db.bun.NewUpdate().Model((*jobRow)(nil)).Conn(tx).
			Set("synced_at = ?", now).Where("id = ?", m.ID).Where("updated_at = ?", m.UpdatedAt).
			Where("COALESCE(token_usage, '') = ?", m.TokenUsage).Where("status = ?", m.Status).
			Where("COALESCE(session_id, '') = ?", m.SessionID).Where("agent_invoked = ?", m.AgentInvoked).
			Where("agent = ?", m.Agent).Where("COALESCE(model, '') = ?", m.Model).
			Where("COALESCE(provider, '') = ?", m.Provider).Where("COALESCE(error, '') = ?", m.Error).
			Where("COALESCE(started_at, '') = ?", m.StartedAtRaw).
			Where("COALESCE(finished_at, '') = ?", m.FinishedAtRaw).
			Exec(context.Background()); err != nil {
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

type syncableReviewRow struct {
	ID                 int64  `bun:"id"`
	UUID               string `bun:"uuid"`
	JobID              int64  `bun:"job_id"`
	JobUUID            string `bun:"job_uuid"`
	Agent              string `bun:"agent"`
	Prompt             string `bun:"prompt"`
	Output             string `bun:"output"`
	Closed             bool   `bun:"closed"`
	UpdatedByMachineID string `bun:"updated_by_machine_id"`
	CreatedAtRaw       string `bun:"created_at_raw"`
	UpdatedAtRaw       string `bun:"updated_at_raw"`
}

// GetReviewsToSync returns reviews modified locally that need to be pushed.
// Only returns reviews whose parent job has already been synced.
func (db *DB) GetReviewsToSync(machineID string, limit int) ([]SyncableReview, error) {
	if limit <= 0 {
		return nil, nil
	}
	var rows []syncableReviewRow
	err := db.bun.NewSelect().TableExpr("reviews AS r").
		ColumnExpr("r.id AS id").ColumnExpr("r.uuid AS uuid").ColumnExpr("r.job_id AS job_id").
		ColumnExpr("j.uuid AS job_uuid").ColumnExpr("r.agent AS agent").
		ColumnExpr("r.prompt AS prompt").ColumnExpr("r.output AS output").ColumnExpr("r.closed AS closed").
		ColumnExpr("r.updated_by_machine_id AS updated_by_machine_id").
		ColumnExpr("r.created_at AS created_at_raw").ColumnExpr("r.updated_at AS updated_at_raw").
		Join("JOIN review_jobs AS j ON r.job_id = j.id").
		Where("r.updated_by_machine_id = ?", machineID).Where("r.uuid IS NOT NULL").
		Where("j.uuid IS NOT NULL").Where("j.synced_at IS NOT NULL").
		Where("r.synced_at IS NULL OR "+sqliteNormalizedTimestampExpr("r.updated_at")+" > "+sqliteNormalizedTimestampExpr("r.synced_at")).
		OrderExpr("r.id").Limit(limit).Scan(context.Background(), &rows)
	if err != nil {
		return nil, fmt.Errorf("query reviews to sync: %w", err)
	}

	var reviews []SyncableReview
	for _, row := range rows {
		reviews = append(reviews, SyncableReview{
			ID: row.ID, UUID: row.UUID, JobID: row.JobID, JobUUID: row.JobUUID,
			Agent: row.Agent, Prompt: row.Prompt, Output: row.Output, Closed: row.Closed,
			UpdatedByMachineID: row.UpdatedByMachineID,
			CreatedAt:          parseSQLiteTime(row.CreatedAtRaw), UpdatedAt: parseSQLiteTime(row.UpdatedAtRaw),
		})
	}
	return reviews, nil
}

// MarkReviewSynced updates the synced_at timestamp for a review
func (db *DB) MarkReviewSynced(reviewID int64) error {
	now := dbTimeFromValue(time.Now())
	_, err := db.bun.NewUpdate().Model((*reviewRow)(nil)).Set("synced_at = ?", now).
		Where("id = ?", reviewID).Exec(context.Background())
	return err
}

// MarkReviewsSynced updates the synced_at timestamp for multiple reviews
func (db *DB) MarkReviewsSynced(reviewIDs []int64) error {
	if len(reviewIDs) == 0 {
		return nil
	}
	now := dbTimeFromValue(time.Now())
	_, err := db.bun.NewUpdate().Model((*reviewRow)(nil)).Set("synced_at = ?", now).
		Where("id IN (?)", bun.List(reviewIDs)).Exec(context.Background())
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

type syncableResponseRow struct {
	ID              int64  `bun:"id"`
	UUID            string `bun:"uuid"`
	JobID           *int64 `bun:"job_id"`
	JobUUID         string `bun:"job_uuid"`
	Responder       string `bun:"responder"`
	Response        string `bun:"response"`
	SourceMachineID string `bun:"source_machine_id"`
	CreatedAtRaw    string `bun:"created_at_raw"`
}

// GetCommentsToSync returns comments created locally that need to be pushed.
// Only returns comments whose parent job has already been synced.
func (db *DB) GetCommentsToSync(machineID string, limit int) ([]SyncableResponse, error) {
	if limit <= 0 {
		return nil, nil
	}
	var rows []syncableResponseRow
	err := db.bun.NewSelect().TableExpr("responses AS r").
		ColumnExpr("r.id AS id").ColumnExpr("r.uuid AS uuid").ColumnExpr("r.job_id AS job_id").
		ColumnExpr("j.uuid AS job_uuid").ColumnExpr("r.responder AS responder").
		ColumnExpr("r.response AS response").ColumnExpr("r.source_machine_id AS source_machine_id").
		ColumnExpr("r.created_at AS created_at_raw").
		Join("JOIN review_jobs AS j ON r.job_id = j.id").
		Where("r.source_machine_id = ?", machineID).Where("r.uuid IS NOT NULL").
		Where("j.uuid IS NOT NULL").Where("r.synced_at IS NULL").Where("j.synced_at IS NOT NULL").
		OrderExpr("r.id").Limit(limit).Scan(context.Background(), &rows)
	if err != nil {
		return nil, fmt.Errorf("query responses to sync: %w", err)
	}

	var responses []SyncableResponse
	for _, row := range rows {
		response := SyncableResponse{
			ID: row.ID, UUID: row.UUID, JobUUID: row.JobUUID, Responder: row.Responder,
			Response: row.Response, SourceMachineID: row.SourceMachineID,
			CreatedAt: parseSQLiteTime(row.CreatedAtRaw),
		}
		if row.JobID != nil {
			response.JobID = *row.JobID
		}
		responses = append(responses, response)
	}
	return responses, nil
}

// MarkCommentSynced updates the synced_at timestamp for a comment
func (db *DB) MarkCommentSynced(responseID int64) error {
	now := dbTimeFromValue(time.Now())
	_, err := db.bun.NewUpdate().Model((*responseRow)(nil)).Set("synced_at = ?", now).
		Where("id = ?", responseID).Exec(context.Background())
	return err
}

// MarkCommentsSynced updates the synced_at timestamp for multiple comments
func (db *DB) MarkCommentsSynced(responseIDs []int64) error {
	if len(responseIDs) == 0 {
		return nil
	}
	now := dbTimeFromValue(time.Now())
	_, err := db.bun.NewUpdate().Model((*responseRow)(nil)).Set("synced_at = ?", now).
		Where("id IN (?)", bun.List(responseIDs)).Exec(context.Background())
	return err
}

// UpsertPulledJob inserts or updates a job from PostgreSQL into SQLite.
// Sets synced_at to prevent re-pushing. Requires repo to exist.
func (db *DB) UpsertPulledJob(j PulledJob, repoID int64, commitID *int64) error {
	now := dbTimeFromValue(time.Now())
	dirtyFilesJSON, err := encodeDirtyFiles(j.DirtyFiles)
	if err != nil {
		return err
	}
	panelMemberIndex := j.PanelMemberIndex
	reasoning := j.Reasoning
	row := jobRow{
		UUID: optionalString(j.UUID), RepoID: repoID, CommitID: commitID, GitRef: j.GitRef,
		SessionID: optionalString(j.SessionID), Agent: j.Agent, Model: optionalString(j.Model),
		Provider: optionalString(j.Provider), RequestedModel: optionalString(j.RequestedModel),
		RequestedProvider: optionalString(j.RequestedProvider), Reasoning: &reasoning,
		JobType: j.JobType, ReviewType: j.ReviewType, PatchID: optionalString(j.PatchID),
		Status: JobStatus(j.Status), Agentic: j.Agentic, AgentInvoked: j.AgentInvoked,
		EnqueuedAt: dbTimeFromValue(j.EnqueuedAt), StartedAt: dbTimeFromPointer(j.StartedAt),
		FinishedAt: dbTimeFromPointer(j.FinishedAt), Prompt: optionalString(j.Prompt),
		DiffContent: cloneStringPointer(j.DiffContent), DirtyFiles: optionalString(dirtyFilesJSON),
		Error: optionalString(j.Error), TokenUsage: optionalString(j.TokenUsage),
		WorktreePath: optionalString(j.WorktreePath), Source: optionalString(j.Source),
		MinSeverity: normalizeMinSeverityForWrite(j.MinSeverity), BackupAgent: j.BackupAgent, BackupModel: j.BackupModel,
		PanelRunUUID: optionalString(j.PanelRunUUID), PanelRole: optionalString(j.PanelRole),
		PanelName: optionalString(j.PanelName), PanelMemberName: optionalString(j.PanelMemberName),
		PanelMemberIndex: &panelMemberIndex, PanelMemberConfigJSON: optionalString(j.PanelMemberConfigJSON),
		SourceMachineID: optionalString(j.SourceMachineID), UpdatedAt: dbTimeFromValue(j.UpdatedAt), SyncedAt: now,
	}
	_, err = db.bun.NewInsert().Model(&row).
		Column("uuid", "repo_id", "commit_id", "git_ref", "session_id", "agent", "model", "provider",
			"requested_model", "requested_provider", "reasoning", "job_type", "review_type", "patch_id", "status",
			"agentic", "agent_invoked", "enqueued_at", "started_at", "finished_at", "prompt", "diff_content",
			"dirty_files", "error", "token_usage", "worktree_path", "source", "min_severity", "backup_agent",
			"backup_model", "panel_run_uuid", "panel_role", "panel_name", "panel_member_name", "panel_member_index",
			"panel_member_config_json", "source_machine_id", "updated_at", "synced_at").
		On("CONFLICT (uuid) DO UPDATE").Set("status = excluded.status").
		Set("finished_at = excluded.finished_at").Set("error = excluded.error").
		Set("model = excluded.model").Set("provider = excluded.provider").
		Set("requested_model = excluded.requested_model").Set("requested_provider = excluded.requested_provider").
		Set("git_ref = excluded.git_ref").
		Set("session_id = CASE WHEN excluded.status IN ('done', 'failed', 'canceled', 'skipped', 'applied', 'rebased') THEN excluded.session_id ELSE COALESCE(excluded.session_id, j.session_id) END").
		Set("commit_id = excluded.commit_id").Set("patch_id = excluded.patch_id").
		Set("dirty_files = COALESCE(excluded.dirty_files, j.dirty_files)").
		Set("token_usage = CASE WHEN excluded.status IN ('done', 'failed', 'canceled', 'skipped', 'applied', 'rebased') THEN excluded.token_usage ELSE COALESCE(excluded.token_usage, j.token_usage) END").
		Set("agent_invoked = CASE WHEN excluded.status IN ('done', 'failed', 'canceled', 'skipped', 'applied', 'rebased') THEN excluded.agent_invoked ELSE (j.agent_invoked OR excluded.agent_invoked) END").
		Set("worktree_path = COALESCE(excluded.worktree_path, j.worktree_path)").
		Set("source = COALESCE(excluded.source, j.source)").Set("min_severity = excluded.min_severity").
		Set("backup_agent = excluded.backup_agent").Set("backup_model = excluded.backup_model").
		Set("panel_run_uuid = excluded.panel_run_uuid").Set("panel_role = excluded.panel_role").
		Set("panel_name = excluded.panel_name").Set("panel_member_name = excluded.panel_member_name").
		Set("panel_member_index = excluded.panel_member_index").
		Set("panel_member_config_json = excluded.panel_member_config_json").
		Set("updated_at = excluded.updated_at").Set("synced_at = ?", now).
		Where("j.status NOT IN ('applied', 'rebased') OR " + sqliteNormalizedTimestampExpr("j.updated_at") + " < " + sqliteNormalizedTimestampExpr("excluded.updated_at")).
		Exec(context.Background())
	return err
}

// UpsertPulledReview inserts or updates a review from PostgreSQL into SQLite.
func (db *DB) UpsertPulledReview(r PulledReview) error {
	// First, find the job_id by uuid
	var jobID int64
	err := db.bun.NewSelect().Model((*jobRow)(nil)).Column("id").Where("uuid = ?", r.JobUUID).
		Scan(context.Background(), &jobID)
	if errors.Is(err, sql.ErrNoRows) {
		// Job doesn't exist locally - skip this review (orphaned)
		return nil
	}
	if err != nil {
		return fmt.Errorf("find job for review: %w", err)
	}

	now := dbTimeFromValue(time.Now())
	var verdictBool *int
	if r.Output != "" {
		verdict := verdictToBool(ParseVerdict(r.Output))
		verdictBool = &verdict
	}
	row := reviewRow{
		UUID: &r.UUID, JobID: &jobID, Agent: r.Agent, Prompt: r.Prompt, Output: r.Output,
		Closed: r.Closed, VerdictBool: verdictBool, UpdatedByMachineID: &r.UpdatedByMachineID,
		CreatedAt: dbTimeFromValue(r.CreatedAt), UpdatedAt: dbTimeFromValue(r.UpdatedAt), SyncedAt: now,
	}
	_, err = db.bun.NewInsert().Model(&row).
		Column("uuid", "job_id", "agent", "prompt", "output", "closed", "verdict_bool",
			"updated_by_machine_id", "created_at", "updated_at", "synced_at").
		On("CONFLICT (uuid) DO UPDATE").Set("closed = excluded.closed").
		Set("verdict_bool = COALESCE(rv.verdict_bool, excluded.verdict_bool)").
		Set("updated_by_machine_id = excluded.updated_by_machine_id").
		Set("updated_at = excluded.updated_at").Set("synced_at = ?", now).
		Where(sqliteNormalizedTimestampExpr("rv.updated_at") + " < " + sqliteNormalizedTimestampExpr("excluded.updated_at")).
		Exec(context.Background())
	return err
}

// UpsertPulledResponse inserts a response from PostgreSQL into SQLite.
func (db *DB) UpsertPulledResponse(r PulledResponse) error {
	// First, find the job_id by uuid
	var jobID int64
	err := db.bun.NewSelect().Model((*jobRow)(nil)).Column("id").Where("uuid = ?", r.JobUUID).
		Scan(context.Background(), &jobID)
	if errors.Is(err, sql.ErrNoRows) {
		// Job doesn't exist locally - skip this response (orphaned)
		return nil
	}
	if err != nil {
		return fmt.Errorf("find job for response: %w", err)
	}

	row := responseRow{
		UUID: &r.UUID, JobID: &jobID, Responder: r.Responder, Response: r.Response,
		SourceMachineID: &r.SourceMachineID, CreatedAt: dbTimeFromValue(r.CreatedAt),
		SyncedAt: dbTimeFromValue(time.Now()),
	}
	_, err = db.bun.NewInsert().Model(&row).
		Column("uuid", "job_id", "responder", "response", "source_machine_id", "created_at", "synced_at").
		On("CONFLICT (uuid) DO NOTHING").Exec(context.Background())
	return err
}

// GetKnownJobUUIDs returns UUIDs of all jobs that have a UUID.
// Used to filter reviews when pulling from PostgreSQL.
func (db *DB) GetKnownJobUUIDs() ([]string, error) {
	var uuids []string
	if err := db.bun.NewSelect().Model((*jobRow)(nil)).Column("uuid").Where("uuid IS NOT NULL").
		Scan(context.Background(), &uuids); err != nil {
		return nil, fmt.Errorf("query job UUIDs: %w", err)
	}
	return uuids, nil
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
	ctx := context.Background()
	var repoIDs []int64
	if err := db.bun.NewSelect().
		Table("repos").
		Column("id").
		Where("identity = ?", identity).
		Where("root_path != ?", identity).
		Scan(ctx, &repoIDs); err != nil {
		return 0, fmt.Errorf("find repos by identity: %w", err)
	}

	// If exactly one local repo exists, always use it (even if placeholder exists)
	if len(repoIDs) == 1 {
		return repoIDs[0], nil
	}

	// 0 or 2+ local repos - look for existing placeholder
	var placeholderID int64
	err := db.bun.NewSelect().
		Table("repos").
		Column("id").
		Where("root_path = ?", identity).
		Where("identity = ?", identity).
		Scan(ctx, &placeholderID)
	if err == nil {
		return placeholderID, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("find placeholder repo: %w", err)
	}

	// No placeholder exists - create one
	// Use extracted repo name for display, but root_path stays as identity to mark it as a placeholder
	row := repoRow{
		RootPath: identity,
		Name:     ExtractRepoNameFromIdentity(identity),
		Identity: optionalString(identity),
	}
	if _, err := db.bun.NewInsert().
		Model(&row).
		Column("root_path", "name", "identity").
		On("CONFLICT (root_path) DO NOTHING").
		Exec(ctx); err != nil {
		return 0, fmt.Errorf("create placeholder repo: %w", err)
	}
	if err := db.bun.NewSelect().
		Table("repos").
		Column("id").
		Where("root_path = ?", identity).
		Where("identity = ?", identity).
		Scan(ctx, &placeholderID); err != nil {
		return 0, fmt.Errorf("read placeholder repo: %w", err)
	}
	return placeholderID, nil
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
	commit, err := db.GetOrCreateCommit(repoID, sha, author, subject, timestamp)
	if err != nil {
		return 0, fmt.Errorf("create commit: %w", err)
	}
	return commit.ID, nil
}
