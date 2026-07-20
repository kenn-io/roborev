package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"
	"unicode"

	"github.com/uptrace/bun"
)

// retryNotBeforeLayout is a fixed-width timestamp layout used for the
// retry_not_before column. Two reasons it differs from RFC3339Nano:
//   - The 9-digit padded fractional seconds avoid the RFC3339Nano quirk
//     of stripping trailing zeros (".5" vs ".500000000"), which would
//     break lexicographic SQL comparison around fractional widths.
//   - Callers must format in UTC (see retryNotBeforeAt). Mixing local
//     offsets would break comparison during DST fall-back, where the
//     same local clock time repeats with different UTC offsets.
const retryNotBeforeLayout = "2006-01-02T15:04:05.000000000Z07:00"

// retryNotBeforeAt returns t formatted for the retry_not_before column.
// Always normalizes to UTC so DST fall-back can't produce two ordered-
// differently-but-equal local strings.
func retryNotBeforeAt(t time.Time) string {
	return t.UTC().Format(retryNotBeforeLayout)
}

// parseSQLiteTime parses a time string from SQLite which may be in different formats.
// Handles RFC3339 (what we write), SQLite datetime('now') format, and timezone variants.
// Returns zero time for empty strings. Logs a warning for non-empty unrecognized formats
// to surface driver/schema issues instead of silently producing zero times.
func parseSQLiteTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	// Try RFC3339 first (what we write for started_at, finished_at)
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	// Try SQLite datetime format (from datetime('now'))
	if t, err := time.Parse("2006-01-02 15:04:05", s); err == nil {
		return t
	}
	// Try with timezone
	if t, err := time.Parse("2006-01-02T15:04:05Z07:00", s); err == nil {
		return t
	}
	log.Printf("storage: warning: unrecognized time format %q", s)
	return time.Time{}
}

// EnqueueOpts contains options for creating any type of review job.
// The job type is inferred from which fields are set (in priority order):
//   - Prompt != "" → "task" (custom prompt job)
//   - DiffContent != "" or DirtyFiles != empty → "dirty" (uncommitted changes)
//   - CommitID > 0 → "review" (single commit)
//   - otherwise → "range" (commit range)
type EnqueueOpts struct {
	RepoID            int64
	CommitID          int64  // >0 for single-commit reviews
	GitRef            string // SHA, "start..end" range, or "dirty"
	Branch            string
	CIBaseBranch      string // PR base branch for CI jobs (event/hook matching only); Branch stays empty
	SessionID         string
	Agent             string
	Model             string // Effective model for this run
	Provider          string // Effective provider for this run (e.g. "anthropic", "openai")
	RequestedModel    string // Explicitly requested model; empty means reevaluate on rerun
	RequestedProvider string // Explicitly requested provider; empty means reevaluate on rerun
	Reasoning         string
	ReviewType        string // e.g. "security" — changes which system prompt is used
	PatchID           string // Stable patch-id for rebase tracking
	DiffContent       string // For dirty reviews (captured at enqueue time)
	DirtyFiles        []string
	Prompt            string // For task jobs (pre-stored prompt)
	OutputPrefix      string // Prefix to prepend to review output
	Agentic           bool   // Allow file edits and command execution
	PromptPrebuilt    bool   // Prompt is prebuilt and should be used as-is by the worker
	Label             string // Display label in TUI for task jobs (default: "prompt")
	JobType           string // Explicit job type (review/range/dirty/task/compact/fix); inferred if empty
	Source            string // Automation source (empty = explicit/user row)
	ParentJobID       int64  // Parent job being fixed (for fix jobs)
	WorktreePath      string // Worktree checkout path (empty = use main repo root)
	MinSeverity       string // Minimum severity filter (canonical: critical/high/medium/low or empty)
	// Job-level failover override (F7): preferred over the workflow-resolved
	// backup agent/model when the worker fails this job over to a backup.
	BackupAgent string
	BackupModel string
	// Panel relation (subagent review panels).
	PanelRunUUID          string // Groups member + synthesis jobs of one run
	PanelRole             string // "", "member", or "synthesis"
	PanelName             string // Config panel name that produced the run
	PanelMemberName       string // Subagent name for a member
	PanelMemberIndex      int    // Stable order of a member within the run
	PanelMemberConfigJSON string // Resolved member spec JSON (reproducibility)
	ClaimBlocked          bool   // Local-only gate: ClaimJob must not claim while set
}

// execer is satisfied by *DB (it embeds *sql.DB), *sql.Conn, and *sql.Tx.
// It lets Bun execute against either the pooled DB or a caller-owned
// transaction connection without rendering query strings.
type execer interface {
	bun.IConn
}

// EnqueueJob creates a new review job. The job type is inferred from opts.
func (db *DB) EnqueueJob(opts EnqueueOpts) (*ReviewJob, error) {
	uid := GenerateUUID()
	machineID, _ := db.GetMachineID()
	now := time.Now()
	return db.insertJobTx(context.Background(), db, opts, uid, machineID, now)
}

// insertJobTx inserts one review_jobs row via exec and returns the built
// ReviewJob. The caller supplies uid/machineID/now so a multi-row panel
// transaction shares one timestamp/machine id and assigns one uuid per row.
func (db *DB) insertJobTx(ctx context.Context, exec execer, opts EnqueueOpts, uid, machineID string, now time.Time) (*ReviewJob, error) {
	reasoning := opts.Reasoning
	if reasoning == "" {
		reasoning = "thorough"
	}

	// Determine job type from fields (use explicit type if provided)
	var jobType string
	if opts.JobType != "" {
		jobType = opts.JobType
	} else {
		switch {
		case opts.Prompt != "":
			jobType = JobTypeTask
		case opts.DiffContent != "" || len(opts.DirtyFiles) > 0:
			jobType = JobTypeDirty
		case opts.CommitID > 0:
			jobType = JobTypeReview
		default:
			jobType = JobTypeRange
		}
	}

	// For task jobs, use Label as git_ref display value
	gitRef := opts.GitRef
	if jobType == JobTypeTask {
		if opts.Label != "" {
			gitRef = opts.Label
		} else if gitRef == "" {
			gitRef = "prompt"
		}
	}

	job := &ReviewJob{
		RepoID:                opts.RepoID,
		GitRef:                gitRef,
		Branch:                opts.Branch,
		CIBaseBranch:          opts.CIBaseBranch,
		SessionID:             opts.SessionID,
		Agent:                 opts.Agent,
		Model:                 opts.Model,
		Provider:              opts.Provider,
		RequestedModel:        opts.RequestedModel,
		RequestedProvider:     opts.RequestedProvider,
		Reasoning:             reasoning,
		JobType:               jobType,
		ReviewType:            opts.ReviewType,
		PatchID:               opts.PatchID,
		DirtyFiles:            append([]string(nil), opts.DirtyFiles...),
		Status:                JobStatusQueued,
		EnqueuedAt:            now,
		Prompt:                opts.Prompt,
		Agentic:               opts.Agentic,
		PromptPrebuilt:        opts.PromptPrebuilt,
		OutputPrefix:          opts.OutputPrefix,
		UUID:                  uid,
		SourceMachineID:       machineID,
		UpdatedAt:             &now,
		WorktreePath:          opts.WorktreePath,
		MinSeverity:           normalizeMinSeverityForWrite(opts.MinSeverity),
		BackupAgent:           opts.BackupAgent,
		BackupModel:           opts.BackupModel,
		PanelRunUUID:          opts.PanelRunUUID,
		PanelRole:             opts.PanelRole,
		PanelName:             opts.PanelName,
		PanelMemberName:       opts.PanelMemberName,
		PanelMemberIndex:      opts.PanelMemberIndex,
		PanelMemberConfigJSON: opts.PanelMemberConfigJSON,
		ClaimBlocked:          opts.ClaimBlocked,
		Source:                opts.Source,
	}
	if opts.ParentJobID > 0 {
		job.ParentJobID = &opts.ParentJobID
	}
	if opts.CommitID > 0 {
		job.CommitID = &opts.CommitID
	}
	if opts.DiffContent != "" {
		job.DiffContent = &opts.DiffContent
	}

	row := jobRowForInsert(*job)
	insert := db.bun.NewInsert().
		Model(&row).
		Column(sqliteJobInsertColumns...).
		Returning("id")
	err := insert.Conn(exec).Scan(ctx)
	if err != nil {
		return nil, err
	}
	job.ID = row.ID
	return job, nil
}

// EnqueuePanelRun atomically inserts the member jobs then the gated synthesis
// job in a single BEGIN IMMEDIATE transaction. It enforces the panel-run
// invariants itself — members are stored with panel_role=member and the
// synthesis with job_type=synthesis, panel_role=synthesis, claim_blocked=1 — so
// a caller that forgets the gate still cannot let the synthesis row be claimed
// before its members run. On any insert error the whole run rolls back and no
// rows persist.
func (db *DB) EnqueuePanelRun(members []EnqueueOpts, synthesis EnqueueOpts) ([]*ReviewJob, *ReviewJob, error) {
	machineID, _ := db.GetMachineID()
	now := time.Now()

	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer conn.Close()

	// Raw SQL allowlist: SQLite transaction mode. BEGIN IMMEDIATE guarantees
	// the entire panel run is inserted under one write lock.
	if _, err := db.bun.NewRaw("BEGIN IMMEDIATE").Conn(conn).Exec(ctx); err != nil {
		return nil, nil, err
	}
	committed := false
	defer func() {
		if !committed {
			if _, err := db.bun.NewRaw("ROLLBACK").Conn(conn).Exec(ctx); err != nil {
				log.Printf("jobs EnqueuePanelRun: rollback failed: %v", err)
			}
		}
	}()

	memberJobs, synthJob, err := db.enqueuePanelRunTx(ctx, conn, members, synthesis, machineID, now)
	if err != nil {
		return nil, nil, err
	}

	if _, err := db.bun.NewRaw("COMMIT").Conn(conn).Exec(ctx); err != nil {
		return nil, nil, err
	}
	committed = true
	return memberJobs, synthJob, nil
}

// enqueuePanelRunTx inserts members then synthesis via exec (caller owns the
// transaction). Returns on the first insert error so the caller can roll back.
// Each row gets a fresh uuid while sharing one machineID/now.
func (db *DB) enqueuePanelRunTx(ctx context.Context, exec execer, members []EnqueueOpts, synthesis EnqueueOpts, machineID string, now time.Time) ([]*ReviewJob, *ReviewJob, error) {
	memberJobs := make([]*ReviewJob, 0, len(members))
	for _, m := range members {
		// m is a loop copy; enforcing the role here does not mutate the
		// caller's slice. Members are members regardless of what the caller set.
		m.PanelRole = PanelRoleMember
		job, err := db.insertJobTx(ctx, exec, m, GenerateUUID(), machineID, now)
		if err != nil {
			return nil, nil, fmt.Errorf("insert panel member %d: %w", m.PanelMemberIndex, err)
		}
		memberJobs = append(memberJobs, job)
	}

	// Enforce the synthesis gate (synthesis is a value-copy parameter, so this
	// does not mutate the caller). A synthesis row that is not claim_blocked
	// could be claimed and run before its members produce reviews.
	synthesis.JobType = JobTypeSynthesis
	synthesis.PanelRole = PanelRoleSynthesis
	synthesis.ClaimBlocked = true
	synthJob, err := db.insertJobTx(ctx, exec, synthesis, GenerateUUID(), machineID, now)
	if err != nil {
		return nil, nil, fmt.Errorf("insert panel synthesis: %w", err)
	}
	return memberJobs, synthJob, nil
}

// ClaimJob atomically claims the next queued job for a worker.
// Jobs whose retry_not_before is in the future are skipped so the retry
// backoff applies regardless of which worker happened to fail the prior
// attempt.
func (db *DB) ClaimJob(workerID string) (*ReviewJob, error) {
	now := time.Now()
	nowStr := dbTimeFromValue(now)
	// retry_not_before is stored UTC + fixed-width nano (see
	// retryNotBeforeLayout) so the SQL comparison stays monotonic with
	// time. Format the comparison value the same way.
	nowNano := retryNotBeforeAt(now)

	// Raw SQL allowlist: guarded atomic state transition. The candidate select,
	// pause gate, and claim update must remain one statement so two workers
	// cannot claim the same row.
	result, err := db.bun.NewRaw(`
		UPDATE review_jobs
		SET status = 'running', worker_id = ?, started_at = ?, updated_at = ?
		WHERE id = (
			SELECT id FROM review_jobs
			WHERE status = 'queued'
			  AND claim_blocked = 0
			  AND (retry_not_before IS NULL OR retry_not_before <= ?)
			ORDER BY enqueued_at, id
			LIMIT 1
		)
		AND NOT EXISTS (
			SELECT 1 FROM daemon_state
			WHERE key = ? AND value IN ('true', '1')
		)
	`, workerID, nowStr, nowStr, nowNano, queuePausedStateKey).
		Exec(context.Background())
	if err != nil {
		return nil, err
	}

	// Check if we claimed anything
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}
	if rowsAffected == 0 {
		return nil, nil // No jobs available
	}

	// Now fetch the job we just claimed.
	var row jobHydrationRow
	query := db.bun.NewSelect().
		TableExpr("review_jobs AS j").
		Join("JOIN repos AS r ON r.id = j.repo_id").
		Join("LEFT JOIN commits AS c ON c.id = j.commit_id").
		Where("j.worker_id = ?", workerID).
		Where("j.status = ?", JobStatusRunning).
		OrderExpr("j.started_at DESC").
		Limit(1)
	query = addJobSelectColumns(query, sqliteJobClaimColumns)
	if err := query.Scan(context.Background(), &row); err != nil {
		return nil, err
	}
	job := row.toModel()
	job.Status = JobStatusRunning
	job.WorkerID = workerID
	job.StartedAt = &now
	return &job, nil
}

// SaveJobPrompt stores the prompt for a running job
func (db *DB) SaveJobPrompt(jobID int64, prompt string) error {
	_, err := db.bun.NewUpdate().
		Model((*jobRow)(nil)).
		Set("prompt = ?", prompt).
		Where("id = ?", jobID).
		Exec(context.Background())
	return err
}

// MarkJobAgentInvoked records that an agent was actually invoked for this
// attempt and stores the command line that was executed. The worker calls it
// immediately before the agent runs — after all pre-agent gates (prompt size,
// worktree creation) — so jobs that fail a gate are never marked. agent_invoked
// is the authoritative, synced "an agent ran" signal for cost eligibility (see
// costEligible); command_line stays a local display/debug field. Attempt resets
// clear both. Set fresh on each run.
//
// The update is scoped to the current attempt (status='running' and the given
// workerID), so a stale worker unwinding after a cancel/retry cannot stamp the
// marker onto a row a new attempt now owns — that would wrongly make the
// terminal row cost-eligible. Mirrors SaveJobSessionID.
func (db *DB) MarkJobAgentInvoked(jobID int64, workerID, cmdLine string) error {
	_, err := db.bun.NewUpdate().
		Model((*jobRow)(nil)).
		Set("command_line = ?", cmdLine).
		Set("agent_invoked = 1").
		Where("id = ?", jobID).
		Where("status = ?", JobStatusRunning).
		Where("worker_id = ?", workerID).
		Exec(context.Background())
	return err
}

// SaveJobSessionID stores the captured agent session ID for a job.
// The first captured ID wins so repeated lifecycle events do not
// overwrite it. The update is scoped to the current execution
// attempt: it requires status='running' and the given workerID so
// that a stale worker unwinding after cancel/retry cannot overwrite
// a session ID that belongs to a new attempt of the same job.
func (db *DB) SaveJobSessionID(
	jobID int64, workerID, sessionID string,
) error {
	if sessionID == "" {
		return nil
	}
	now := dbTimeFromValue(time.Now())
	_, err := db.bun.NewUpdate().
		Model((*jobRow)(nil)).
		Set("session_id = ?", sessionID).
		Set("updated_at = ?", now).
		Where("id = ?", jobID).
		Where("status = ?", JobStatusRunning).
		Where("worker_id = ?", workerID).
		Where("session_id IS NULL OR session_id = ''").
		Exec(context.Background())
	return err
}

// SaveJobPatch stores the generated patch for a completed fix job
func (db *DB) SaveJobPatch(jobID int64, patch string) error {
	_, err := db.bun.NewUpdate().
		Model((*jobRow)(nil)).
		Set("patch = ?", patch).
		Where("id = ?", jobID).
		Exec(context.Background())
	return err
}

// SaveJobTokenUsage stores a JSON blob of token consumption data, scoped to
// the attempt that produced it. The write only lands if the row still carries
// sessionID, so a delayed usage fetch from a prior attempt cannot stamp its
// cost onto a row that was re-enqueued (and cleared) and is now running or
// done under a different session.
//
// It also clears synced_at to invalidate the push cursor directly. A job is
// marked terminal before its cost is captured, so this write can land in the
// same RFC3339 second as a prior sync mark; updated_at vs synced_at is only
// second-precise, so the cursor would compare equal and never re-select the
// row. A NULL synced_at always re-selects it, so the cost reaches PostgreSQL.
func (db *DB) SaveJobTokenUsage(jobID int64, sessionID, tokenUsageJSON string) error {
	if tokenUsageJSON == "" {
		return nil
	}
	now := dbTimeFromValue(time.Now())
	_, err := db.bun.NewUpdate().
		Model((*jobRow)(nil)).
		Set("token_usage = ?", tokenUsageJSON).
		Set("updated_at = ?", now).
		Set("synced_at = NULL").
		Where("id = ?", jobID).
		Where("session_id = ?", sessionID).
		Exec(context.Background())
	return err
}

// BackfillJobTokenUsage stores recovered token usage for a terminal job.
// Unlike SaveJobTokenUsage, this path runs after the producing worker is gone,
// so it scopes the update to a terminal row and preserves any different
// existing session_id to avoid stamping usage onto an unrelated attempt.
func (db *DB) BackfillJobTokenUsage(jobID int64, sessionID, tokenUsageJSON string) error {
	if tokenUsageJSON == "" {
		return nil
	}
	now := dbTimeFromValue(time.Now())
	_, err := db.bun.NewUpdate().
		Model((*jobRow)(nil)).
		Set("token_usage = ?", tokenUsageJSON).
		Set(`session_id = CASE
			WHEN ? != '' AND (session_id IS NULL OR session_id = '') THEN ?
			ELSE session_id
		END`, sessionID, sessionID).
		Set("updated_at = ?", now).
		Set("synced_at = NULL").
		Where("id = ?", jobID).
		Where("status IN ('done', 'applied', 'rebased', 'failed', 'canceled', 'skipped')").
		Where("session_id IS NULL OR session_id = '' OR session_id = ?", sessionID).
		Exec(context.Background())
	return err
}

// CompleteFixJob atomically marks a fix job as done, stores the review,
// and persists the patch in a single transaction. This prevents invalid
// states where a patch is written but the job isn't done, or vice versa.
func (db *DB) CompleteFixJob(jobID int64, agent, prompt, output, patch string) error {
	nowTime := time.Now()
	now := dbTimeFromValue(nowTime)
	machineID, _ := db.GetMachineID()
	reviewUUID := GenerateUUID()

	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	// Raw SQL allowlist: SQLite transaction mode. The guarded job transition,
	// patch write, and review insert must commit atomically.
	if _, err := db.bun.NewRaw("BEGIN IMMEDIATE").Conn(conn).Exec(ctx); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			if _, err := db.bun.NewRaw("ROLLBACK").Conn(conn).Exec(ctx); err != nil {
				log.Printf("jobs CompleteFixJob: rollback failed: %v", err)
			}
		}
	}()

	// Fetch output_prefix from job (if any)
	var prefixRow struct {
		OutputPrefix *string `bun:"output_prefix"`
	}
	err = db.bun.NewSelect().
		Conn(conn).
		Table("review_jobs").
		Column("output_prefix").
		Where("id = ?", jobID).
		Scan(ctx, &prefixRow)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}

	finalOutput := output
	if prefix := stringValue(prefixRow.OutputPrefix); prefix != "" {
		finalOutput = prefix + output
	}

	// Atomically set status=done AND patch in one UPDATE
	result, err := db.bun.NewUpdate().
		Model((*jobRow)(nil)).
		Conn(conn).
		Set("status = ?", JobStatusDone).
		Set("finished_at = ?", now).
		Set("updated_at = ?", now).
		Set("patch = ?", patch).
		Where("id = ?", jobID).
		Where("status = ?", JobStatusRunning).
		Exec(ctx)
	if err != nil {
		return err
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return nil // Job was canceled
	}

	verdictBool := verdictToBool(ParseVerdict(finalOutput))
	review := reviewRow{
		JobID:              &jobID,
		Agent:              agent,
		Prompt:             prompt,
		Output:             finalOutput,
		VerdictBool:        &verdictBool,
		UUID:               &reviewUUID,
		UpdatedByMachineID: optionalString(machineID),
		UpdatedAt:          dbTimeFromValue(nowTime),
	}
	_, err = db.bun.NewInsert().
		Model(&review).
		Conn(conn).
		Column(
			"job_id", "agent", "prompt", "output", "verdict_bool", "uuid",
			"updated_by_machine_id", "updated_at",
		).
		Exec(ctx)
	if err != nil {
		return err
	}

	_, err = db.bun.NewRaw("COMMIT").Conn(conn).Exec(ctx)
	if err != nil {
		return err
	}
	committed = true
	return nil
}

// CompleteJob marks a job as done and stores the review.
// Only updates if job is still in 'running' state (respects cancellation).
// If the job has an output_prefix, it will be prepended to the output.
func (db *DB) CompleteJob(jobID int64, agent, prompt, output string) error {
	// Get machine ID and generate UUIDs before starting transaction
	// to avoid potential lock conflicts with GetMachineID's writes
	nowTime := time.Now()
	now := dbTimeFromValue(nowTime)
	machineID, _ := db.GetMachineID()
	reviewUUID := GenerateUUID()

	// Use BEGIN IMMEDIATE to acquire write lock upfront, avoiding deadlocks
	// when concurrent goroutines (workers, sync) try to upgrade from read to write.
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	// Raw SQL allowlist: SQLite transaction mode. The guarded job transition
	// and review insert must commit atomically.
	if _, err := db.bun.NewRaw("BEGIN IMMEDIATE").Conn(conn).Exec(ctx); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			if _, err := db.bun.NewRaw("ROLLBACK").Conn(conn).Exec(ctx); err != nil {
				log.Printf("jobs CompleteJob: rollback failed: %v", err)
			}
		}
	}()

	// Fetch output_prefix from job (if any)
	var prefixRow struct {
		OutputPrefix *string `bun:"output_prefix"`
	}
	err = db.bun.NewSelect().
		Conn(conn).
		Table("review_jobs").
		Column("output_prefix").
		Where("id = ?", jobID).
		Scan(ctx, &prefixRow)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}

	// Prepend output_prefix if present
	finalOutput := output
	if prefix := stringValue(prefixRow.OutputPrefix); prefix != "" {
		finalOutput = prefix + output
	}

	// Update job status only if still running (not canceled)
	result, err := db.bun.NewUpdate().
		Model((*jobRow)(nil)).
		Conn(conn).
		Set("status = ?", JobStatusDone).
		Set("finished_at = ?", now).
		Set("updated_at = ?", now).
		Where("id = ?", jobID).
		Where("status = ?", JobStatusRunning).
		Exec(ctx)
	if err != nil {
		return err
	}

	// Check if we actually updated (job wasn't canceled)
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		// Job was canceled or in unexpected state, don't store review
		return nil
	}

	// Insert review with sync columns
	var verdictBool *int
	if finalOutput != "" {
		value := verdictToBool(ParseVerdict(finalOutput))
		verdictBool = &value
	}
	review := reviewRow{
		JobID:              &jobID,
		Agent:              agent,
		Prompt:             prompt,
		Output:             finalOutput,
		VerdictBool:        verdictBool,
		UUID:               &reviewUUID,
		UpdatedByMachineID: optionalString(machineID),
		UpdatedAt:          dbTimeFromValue(nowTime),
	}
	_, err = db.bun.NewInsert().
		Model(&review).
		Conn(conn).
		Column(
			"job_id", "agent", "prompt", "output", "verdict_bool", "uuid",
			"updated_by_machine_id", "updated_at",
		).
		Exec(ctx)
	if err != nil {
		return err
	}

	_, err = db.bun.NewRaw("COMMIT").Conn(conn).Exec(ctx)
	if err != nil {
		return err
	}
	committed = true
	return nil
}

// FailJob marks a job as failed with an error message.
// Only updates if job is still in 'running' state and owned by the given worker
// (respects cancellation and prevents stale workers from failing reclaimed jobs).
// Pass empty workerID to skip the ownership check (for admin/test callers).
// Returns true if the job was actually updated (false when ownership or status
// check prevented the update).
func (db *DB) FailJob(jobID int64, workerID string, errorMsg string) (bool, error) {
	now := dbTimeFromValue(time.Now())
	query := db.bun.NewUpdate().
		Model((*jobRow)(nil)).
		Set("status = ?", JobStatusFailed).
		Set("finished_at = ?", now).
		Set("error = ?", errorMsg).
		Set("updated_at = ?", now).
		Where("id = ?", jobID).
		Where("status = ?", JobStatusRunning)
	if workerID != "" {
		query = query.Where("worker_id = ?", workerID)
	}
	result, err := query.Exec(context.Background())
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return rows > 0, nil
}

// CancelJob marks a running or queued job as canceled
func (db *DB) CancelJob(jobID int64) error {
	now := dbTimeFromValue(time.Now())
	result, err := db.bun.NewUpdate().
		Model((*jobRow)(nil)).
		Set("status = ?", JobStatusCanceled).
		Set("finished_at = ?", now).
		Set("updated_at = ?", now).
		Where("id = ?", jobID).
		Where("status IN ('queued', 'running')").
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

// MarkJobApplied transitions a fix job from done to applied.
func (db *DB) MarkJobApplied(jobID int64) error {
	now := dbTimeFromValue(time.Now())
	result, err := db.bun.NewUpdate().
		Model((*jobRow)(nil)).
		Set("status = ?", JobStatusApplied).
		Set("updated_at = ?", now).
		Where("id = ?", jobID).
		Where("status = ?", JobStatusDone).
		Where("job_type = ?", JobTypeFix).
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

// MarkJobRebased transitions a done fix job to the "rebased" terminal state.
// This indicates the patch was stale and a new rebase job was triggered.
func (db *DB) MarkJobRebased(jobID int64) error {
	now := dbTimeFromValue(time.Now())
	result, err := db.bun.NewUpdate().
		Model((*jobRow)(nil)).
		Set("status = ?", JobStatusRebased).
		Set("updated_at = ?", now).
		Where("id = ?", jobID).
		Where("status = ?", JobStatusDone).
		Where("job_type = ?", JobTypeFix).
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

type ReenqueueOpts struct {
	Model    string
	Provider string
}

// ReenqueueJob resets a completed, failed, or canceled job back to queued status.
// This allows manual re-running of jobs to get a fresh review.
// For done jobs, the existing review is deleted to avoid unique constraint violations.
func (db *DB) ReenqueueJob(jobID int64, opts ReenqueueOpts) error {
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	// Raw SQL allowlist: SQLite transaction mode. BEGIN IMMEDIATE acquires the
	// write lock before the delete/update pair so concurrent reruns cannot
	// interleave partial reset state.
	if _, err := db.bun.NewRaw("BEGIN IMMEDIATE").Conn(conn).Exec(ctx); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			if _, err := db.bun.NewRaw("ROLLBACK").Conn(conn).Exec(ctx); err != nil {
				log.Printf("jobs ReenqueueJob: rollback failed: %v", err)
			}
		}
	}()

	// Delete any existing review for this job (for done jobs being rerun)
	_, err = db.bun.NewDelete().
		Model((*reviewRow)(nil)).
		Conn(conn).
		Where("job_id = ?", jobID).
		Exec(ctx)
	if err != nil {
		return err
	}

	nowStr := dbTimeFromValue(time.Now())

	// Reset job status and replace effective execution settings with the
	// newly resolved values for this rerun. Clear prompt_prebuilt and prompt
	// only for review jobs so they rebuild from current git/config state.
	// Stored-prompt jobs (task, compact, fix, insights) keep their prompt
	// since the worker needs it and cannot regenerate it from git.
	//
	// synced_at is cleared because this attempt's cost metadata is cleared: if
	// the rerun completes unpriced in the same RFC3339 second as the prior
	// sync, the second-precise updated_at vs synced_at comparison would compare
	// equal and never re-push, leaving stale spend in PostgreSQL. A NULL
	// synced_at always re-selects the row (see SaveJobTokenUsage, which clears
	// it on the symmetric cost-write path). The other attempt resets (RetryJob,
	// FailoverJob, ResetStaleJobs, PromoteClassifyToDesignReview) clear it for
	// the same reason.
	result, err := db.bun.NewUpdate().
		Model((*jobRow)(nil)).
		Conn(conn).
		Set("status = ?", JobStatusQueued).
		Set("worker_id = NULL").
		Set("started_at = NULL").
		Set("finished_at = NULL").
		Set("error = NULL").
		Set("retry_count = 0").
		Set("patch = NULL").
		Set("session_id = NULL").
		Set("token_usage = NULL").
		Set("command_line = NULL").
		Set("agent_invoked = 0").
		Set("synced_at = NULL").
		Set("model = ?", nullString(opts.Model)).
		Set("provider = ?", nullString(opts.Provider)).
		Set("prompt_prebuilt = 0").
		Set("prompt = CASE WHEN job_type IN ('task', 'compact', 'fix', 'insights') THEN prompt ELSE NULL END").
		Set("skip_reason = NULL").
		Set("updated_at = ?", nowStr).
		Where("id = ?", jobID).
		Where("status IN ('done', 'failed', 'canceled', 'skipped')").
		Exec(ctx)
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

	_, err = db.bun.NewRaw("COMMIT").Conn(conn).Exec(ctx)
	if err != nil {
		return err
	}
	committed = true
	return nil
}

// RetryJob requeues a running job for retry if retry_count < maxRetries.
// When workerID is non-empty the update is scoped to the owning worker,
// preventing a stale/zombie worker from requeuing a reclaimed job.
// Pass empty workerID to skip the ownership check (for admin/test callers).
//
// retryBackoff, when > 0, defers the requeued job from being claimed until
// now + retryBackoff. This avoids rapid concurrent agent startups racing on
// shared agent state (notably opencode's sqlite WAL).
func (db *DB) RetryJob(jobID int64, workerID string, maxRetries int, retryBackoff time.Duration) (bool, error) {
	var notBefore any
	if retryBackoff > 0 {
		notBefore = retryNotBeforeAt(time.Now().Add(retryBackoff))
	}

	query := db.bun.NewUpdate().
		Model((*jobRow)(nil)).
		Set("status = ?", JobStatusQueued).
		Set("worker_id = NULL").
		Set("started_at = NULL").
		Set("finished_at = NULL").
		Set("error = NULL").
		Set("retry_count = retry_count + 1").
		Set("session_id = NULL").
		Set("token_usage = NULL").
		Set("command_line = NULL").
		Set("agent_invoked = 0").
		Set("synced_at = NULL").
		Set("retry_not_before = ?", notBefore).
		Where("id = ?", jobID).
		Where("retry_count < ?", maxRetries).
		Where("status = ?", JobStatusRunning)
	if workerID != "" {
		query = query.Where("worker_id = ?", workerID)
	}
	result, err := query.Exec(context.Background())
	if err != nil {
		return false, err
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return false, err
	}

	return rows > 0, nil
}

// FailoverJob atomically switches a running job to the given backup agent
// and requeues it. When backupModel is non-empty the job's model is set
// to that value; otherwise model is cleared (NULL) so the backup agent
// uses its CLI default. Returns false if the job is not in running state,
// the worker doesn't own the job, or the backup agent is the same as the
// current agent.
func (db *DB) FailoverJob(jobID int64, workerID, backupAgent, backupModel string) (bool, error) {
	if backupAgent == "" {
		return false, nil
	}
	result, err := db.bun.NewUpdate().
		Model((*jobRow)(nil)).
		Set("agent = ?", backupAgent).
		Set("model = ?", nullString(backupModel)).
		Set("retry_count = 0").
		Set("status = ?", JobStatusQueued).
		Set("worker_id = NULL").
		Set("started_at = NULL").
		Set("finished_at = NULL").
		Set("error = NULL").
		Set("session_id = NULL").
		Set("token_usage = NULL").
		Set("command_line = NULL").
		Set("agent_invoked = 0").
		Set("synced_at = NULL").
		Set("retry_not_before = NULL").
		Where("id = ?", jobID).
		Where("status = ?", JobStatusRunning).
		Where("worker_id = ?", workerID).
		Where("agent != ?", backupAgent).
		Exec(context.Background())
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	return rows > 0, err
}

// GetJobRetryCount returns the retry count for a job
func (db *DB) GetJobRetryCount(jobID int64) (int, error) {
	var count int
	err := db.bun.NewSelect().
		Table("review_jobs").
		Column("retry_count").
		Where("id = ?", jobID).
		Scan(context.Background(), &count)
	return count, err
}

// ListJobsOption configures optional filters for ListJobs.
type ListJobsOption func(*listJobsOptions)

type listJobsOptions struct {
	gitRef             string
	branch             string
	branchIncludeEmpty bool
	closed             *bool
	jobType            string
	excludeJobType     string
	hideClassifyJobs   bool
	repoPrefix         string
	repoPaths          []string
	beforeCursor       *int64
	panelRun           string
	excludePanelRole   string
	omitPrompt         bool
}

// WithGitRef filters jobs by git ref.
func WithGitRef(ref string) ListJobsOption {
	return func(o *listJobsOptions) { o.gitRef = ref }
}

// WithBranch filters jobs by exact branch name.
func WithBranch(branch string) ListJobsOption {
	return func(o *listJobsOptions) { o.branch = branch }
}

// WithBranchOrEmpty filters jobs by branch name, also including jobs
// with no branch set (empty string or NULL).
func WithBranchOrEmpty(branch string) ListJobsOption {
	return func(o *listJobsOptions) {
		o.branch = branch
		o.branchIncludeEmpty = true
	}
}

// WithClosed filters jobs by closed state (true/false).
func WithClosed(closed bool) ListJobsOption {
	return func(o *listJobsOptions) { o.closed = &closed }
}

// WithoutPrompt selects an empty string in place of the prompt column so
// metadata-only listings never read the stored prompts. Prompts embed full
// diffs, so on repos with a long review history hydrating them costs tens of
// megabytes of SQLite reads and string allocations per listing.
func WithoutPrompt() ListJobsOption {
	return func(o *listJobsOptions) { o.omitPrompt = true }
}

// WithJobType filters jobs by job_type (e.g. "fix", "review").
func WithJobType(jobType string) ListJobsOption {
	return func(o *listJobsOptions) { o.jobType = jobType }
}

// WithExcludeJobType excludes jobs of the given type.
func WithExcludeJobType(jobType string) ListJobsOption {
	return func(o *listJobsOptions) { o.excludeJobType = jobType }
}

// WithHideClassifyJobs excludes auto-design-router byproducts: rows
// with job_type='classify' (the routing decision itself) and rows
// with status='skipped' (the terminal state for skipped design
// reviews). Both clauses are scoped to source='auto_design' so a
// future pipeline that creates classify jobs or skipped jobs for a
// different reason is not silently swallowed by this filter.
func WithHideClassifyJobs() ListJobsOption {
	return func(o *listJobsOptions) { o.hideClassifyJobs = true }
}

// WithBeforeCursor filters jobs to those with ID < cursor (for cursor pagination).
func WithBeforeCursor(id int64) ListJobsOption {
	return func(o *listJobsOptions) { o.beforeCursor = &id }
}

// WithRepoPrefix filters jobs to repos whose root_path starts with the given prefix.
func WithRepoPrefix(prefix string) ListJobsOption {
	return func(o *listJobsOptions) {
		// Trim trailing slash so LIKE "prefix/%"  doesn't become "prefix//%".
		// Root prefix "/" trims to "" which disables the filter (all repos match).
		o.repoPrefix = escapeLike(normalizeRepoPathPrefix(prefix))
	}
}

// WithRepoPaths filters jobs to the given set of repo root paths via an
// IN clause. Used for display names that map to multiple repos, so the
// daemon scopes the query server-side instead of returning every job for
// the client to filter. An empty slice disables the filter.
func WithRepoPaths(paths []string) ListJobsOption {
	return func(o *listJobsOptions) { o.repoPaths = paths }
}

// WithPanelRun filters jobs to a single panel run (member + synthesis rows).
func WithPanelRun(uuid string) ListJobsOption {
	return func(o *listJobsOptions) { o.panelRun = uuid }
}

// WithExcludePanelRole excludes jobs with the given panel_role (e.g. "member"),
// so the default listing and SHA-resolution path stay parent-only — the same
// caller-driven exclusion mechanism fix jobs use via WithExcludeJobType.
func WithExcludePanelRole(role string) ListJobsOption {
	return func(o *listJobsOptions) { o.excludePanelRole = role }
}

// escapeLike escapes SQL LIKE wildcards (% and _) in a literal string.
// Uses '!' as the ESCAPE character to avoid conflicts with backslashes
// in Windows paths stored in root_path.
func escapeLike(s string) string {
	s = strings.ReplaceAll(s, "!", "!!")
	s = strings.ReplaceAll(s, "%", "!%")
	s = strings.ReplaceAll(s, "_", "!_")
	return s
}

func collectListJobsOptions(opts ...ListJobsOption) listJobsOptions {
	var o listJobsOptions
	for _, opt := range opts {
		opt(&o)
	}
	return o
}

func buildJobFilterClause(statusFilter, repoFilter string, o listJobsOptions) (string, []any) {
	var args []any
	var conditions []string

	if statusFilter != "" {
		conditions = append(conditions, "j.status = ?")
		args = append(args, statusFilter)
	}
	// Repo scoping precedence: a single positional repoFilter wins, then a
	// multi-repo IN set (display names spanning repos), then a path prefix.
	// At most one applies so the clauses never compound into an empty result.
	switch {
	case repoFilter != "":
		conditions = append(conditions, "r.root_path = ?")
		args = append(args, repoFilter)
	case len(o.repoPaths) > 0:
		placeholders := make([]string, len(o.repoPaths))
		for i, p := range o.repoPaths {
			placeholders[i] = "?"
			args = append(args, p)
		}
		conditions = append(conditions, "r.root_path IN ("+strings.Join(placeholders, ",")+")")
	case o.repoPrefix != "":
		conditions = append(conditions, "r.root_path LIKE ? || '/%' ESCAPE '!'")
		args = append(args, o.repoPrefix)
	}
	if o.gitRef != "" {
		conditions = append(conditions, "j.git_ref = ?")
		args = append(args, o.gitRef)
	}
	if o.branch != "" {
		if o.branchIncludeEmpty {
			conditions = append(conditions, "(j.branch = ? OR j.branch = '' OR j.branch IS NULL)")
		} else {
			conditions = append(conditions, "j.branch = ?")
		}
		args = append(args, o.branch)
	}
	if o.closed != nil {
		if *o.closed {
			conditions = append(conditions, "rv.closed = 1")
		} else {
			conditions = append(conditions, "(rv.closed IS NULL OR rv.closed = 0)")
		}
	}
	if o.jobType != "" {
		conditions = append(conditions, "j.job_type = ?")
		args = append(args, o.jobType)
	}
	if o.excludeJobType != "" {
		conditions = append(conditions, "j.job_type != ?")
		args = append(args, o.excludeJobType)
	}
	if o.hideClassifyJobs {
		// source is nullable, so guard the comparison with COALESCE.
		// Without it, j.source = 'auto_design' returns NULL for rows
		// where source IS NULL, NOT (TRUE AND NULL) is also NULL, and
		// the row gets silently dropped from results.
		//
		// Both predicates are scoped to source='auto_design' so a
		// future pipeline that adopts classify jobs or status='skipped'
		// for an unrelated reason isn't silently hidden.
		conditions = append(conditions,
			"NOT (COALESCE(j.source, '') = 'auto_design' AND (j.job_type = ? OR j.status = ?))")
		args = append(args,
			JobTypeClassify, string(JobStatusSkipped))
	}
	if o.panelRun != "" {
		conditions = append(conditions, "j.panel_run_uuid = ?")
		args = append(args, o.panelRun)
	}
	if o.excludePanelRole != "" {
		// panel_role is nullable, so guard with COALESCE — a NULL role
		// (non-panel job) must NOT be excluded.
		conditions = append(conditions, "COALESCE(j.panel_role, '') != ?")
		args = append(args, o.excludePanelRole)
	}
	if o.beforeCursor != nil {
		conditions = append(conditions, "j.id < ?")
		args = append(args, *o.beforeCursor)
	}

	if len(conditions) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(conditions, " AND "), args
}

func addJobSelectColumns(
	query *bun.SelectQuery, columns []string,
) *bun.SelectQuery {
	for _, column := range columns {
		query = query.ColumnExpr(column)
	}
	return query
}

func applyJobFilterClause(
	query *bun.SelectQuery,
	statusFilter string,
	repoFilter string,
	options listJobsOptions,
) *bun.SelectQuery {
	clause, args := buildJobFilterClause(statusFilter, repoFilter, options)
	if clause == "" {
		return query
	}
	return query.Where(strings.TrimPrefix(clause, " WHERE "), args...)
}

// ListJobs returns jobs with optional status, repo, branch, and closed filters.
func (db *DB) ListJobs(statusFilter string, repoFilter string, limit, offset int, opts ...ListJobsOption) ([]ReviewJob, error) {
	options := collectListJobsOptions(opts...)
	query := db.bun.NewSelect().
		TableExpr("review_jobs AS j").
		Join("JOIN repos AS r ON r.id = j.repo_id").
		Join("LEFT JOIN commits AS c ON c.id = j.commit_id").
		Join("LEFT JOIN reviews AS rv ON rv.job_id = j.id")
	query = addJobSelectColumns(query, sqliteJobListColumns)
	// Metadata-only listings select a constant instead of the prompt column,
	// so SQLite never reads the large stored prompt payload.
	if options.omitPrompt {
		query = query.ColumnExpr("'' AS prompt")
	} else {
		query = query.ColumnExpr("j.prompt")
	}
	query = applyJobFilterClause(query, statusFilter, repoFilter, options).
		OrderExpr("j.id DESC")

	if limit > 0 {
		query = query.Limit(limit)
		// OFFSET requires LIMIT in SQLite
		if offset > 0 {
			query = query.Offset(offset)
		}
	}

	var rows []jobHydrationRow
	if err := query.Scan(context.Background(), &rows); err != nil {
		return nil, err
	}

	var jobs []ReviewJob
	for _, row := range rows {
		jobs = append(jobs, row.toModel())
	}
	return jobs, nil
}

// GetJobByID returns a job by ID with joined fields
// JobStats holds aggregate counts for the queue status line.
type JobStats struct {
	Done   int `json:"done"`
	Closed int `json:"closed"`
	Open   int `json:"open"`
}

// CountJobStats returns aggregate done/closed/open counts
// using the same filter logic as ListJobs (repo, branch, closed).
func (db *DB) CountJobStats(repoFilter string, opts ...ListJobsOption) (JobStats, error) {
	var stats JobStats
	query := db.bun.NewSelect().
		TableExpr("review_jobs AS j").
		ColumnExpr("COALESCE(SUM(CASE WHEN j.status = 'done' THEN 1 ELSE 0 END), 0) AS done").
		ColumnExpr("COALESCE(SUM(CASE WHEN j.status = 'done' AND rv.closed = 1 THEN 1 ELSE 0 END), 0) AS closed").
		ColumnExpr("COALESCE(SUM(CASE WHEN j.status = 'done' AND (rv.closed IS NULL OR rv.closed = 0) THEN 1 ELSE 0 END), 0) AS open").
		Join("JOIN repos AS r ON r.id = j.repo_id").
		Join("LEFT JOIN reviews AS rv ON rv.job_id = j.id")
	query = applyJobFilterClause(
		query, "", repoFilter, collectListJobsOptions(opts...),
	)
	err := query.Scan(context.Background(), &stats)
	return stats, err
}

func (db *DB) GetJobByID(id int64) (*ReviewJob, error) {
	var row jobHydrationRow
	query := db.bun.NewSelect().
		TableExpr("review_jobs AS j").
		Join("JOIN repos AS r ON r.id = j.repo_id").
		Join("LEFT JOIN commits AS c ON c.id = j.commit_id").
		Where("j.id = ?", id)
	query = addJobSelectColumns(query, sqliteJobDetailColumns).
		ColumnExpr("j.prompt")
	if err := query.Scan(context.Background(), &row); err != nil {
		return nil, err
	}
	job := row.toModel()
	return &job, nil
}

// GetJobDiffContent returns the stored dirty-diff blob for a job (empty string
// when the job has none). Only ClaimJob hydrates DiffContent on the full job;
// this targeted getter lets callers that loaded a job through a lighter query
// (GetPanelMembers, GetJobByID) recover the frozen diff without claiming.
func (db *DB) GetJobDiffContent(jobID int64) (string, error) {
	var diff string
	err := db.bun.NewSelect().
		Table("review_jobs").
		ColumnExpr("COALESCE(diff_content, '')").
		Where("id = ?", jobID).
		Scan(context.Background(), &diff)
	if err != nil {
		return "", fmt.Errorf("get job diff content: %w", err)
	}
	return diff, nil
}

// GetJobDirtyFiles returns the stored unfiltered dirty file names for a job.
func (db *DB) GetJobDirtyFiles(jobID int64) ([]string, error) {
	var row struct {
		DirtyFiles *string `bun:"dirty_files"`
	}
	err := db.bun.NewSelect().
		Table("review_jobs").
		Column("dirty_files").
		Where("id = ?", jobID).
		Scan(context.Background(), &row)
	if err != nil {
		return nil, fmt.Errorf("get job dirty files: %w", err)
	}
	if row.DirtyFiles == nil {
		return nil, nil
	}
	return decodeDirtyFiles(*row.DirtyFiles), nil
}

// GetJobCounts returns counts of jobs by status
func (db *DB) GetJobCounts() (queued, running, done, failed, canceled, applied, rebased, skipped int, err error) {
	var rows []struct {
		Status JobStatus `bun:"status"`
		Count  int       `bun:"count"`
	}
	if err := db.bun.NewSelect().
		Table("review_jobs").
		Column("status").
		ColumnExpr("COUNT(*) AS count").
		GroupExpr("status").
		Scan(context.Background(), &rows); err != nil {
		return queued, running, done, failed, canceled, applied, rebased, skipped, err
	}

	for _, row := range rows {
		switch row.Status {
		case JobStatusQueued:
			queued = row.Count
		case JobStatusRunning:
			running = row.Count
		case JobStatusDone:
			done = row.Count
		case JobStatusFailed:
			failed = row.Count
		case JobStatusCanceled:
			canceled = row.Count
		case JobStatusApplied:
			applied = row.Count
		case JobStatusRebased:
			rebased = row.Count
		case JobStatusSkipped:
			skipped = row.Count
		}
	}
	return queued, running, done, failed, canceled, applied, rebased, skipped, nil
}

// UpdateJobBranch sets the branch field for a job that doesn't have one.
// This is used to backfill the branch when it's derived from git.
// Only updates if the current branch is NULL or empty.
// Returns the number of rows affected (0 if branch was already set or job not found, 1 if updated).
func (db *DB) UpdateJobBranch(jobID int64, branch string) (int64, error) {
	result, err := db.bun.NewUpdate().
		Model((*jobRow)(nil)).
		Set("branch = ?", branch).
		Where("id = ?", jobID).
		Where("branch IS NULL OR branch = ''").
		Exec(context.Background())
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// RemapJobGitRef updates git_ref and commit_id for jobs matching
// oldSHA in a repo, used after rebases to preserve review history.
// If a job has a stored patch_id that differs from the provided one,
// that job is skipped (the commit's content changed).
// Returns the number of rows updated.
func (db *DB) RemapJobGitRef(
	repoID int64, oldSHA, newSHA, patchID string, newCommitID int64,
) (int, error) {
	now := dbTimeFromValue(time.Now())
	result, err := db.bun.NewUpdate().
		Model((*jobRow)(nil)).
		Set("git_ref = ?", newSHA).
		Set("commit_id = ?", newCommitID).
		Set("patch_id = ?", nullString(patchID)).
		Set("updated_at = ?", now).
		Where("git_ref = ?", oldSHA).
		Where("repo_id = ?", repoID).
		Where("status != ?", JobStatusRunning).
		Where("patch_id IS NULL OR patch_id = '' OR patch_id = ?", patchID).
		Exec(context.Background())
	if err != nil {
		return 0, fmt.Errorf("remap job git_ref: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(n), nil
}

// RemapJob atomically checks for matching jobs, creates the commit
// row, and updates git_ref — all in a single transaction to prevent
// orphan commit rows or races between concurrent remaps.
func (db *DB) RemapJob(
	repoID int64, oldSHA, newSHA, patchID string,
	author, subject string, timestamp time.Time,
) (int, error) {
	ctx := context.Background()
	tx, err := db.bun.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin remap tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	var matchCount int
	err = db.bun.NewSelect().
		Conn(tx).
		Table("review_jobs").
		ColumnExpr("COUNT(*)").
		Where("git_ref = ?", oldSHA).
		Where("repo_id = ?", repoID).
		Where("status != ?", JobStatusRunning).
		Where("patch_id IS NULL OR patch_id = '' OR patch_id = ?", patchID).
		Scan(ctx, &matchCount)
	if err != nil {
		return 0, fmt.Errorf("count matching jobs: %w", err)
	}
	if matchCount == 0 {
		return 0, nil
	}

	// Create or find commit row for the new SHA
	var commitID int64
	err = db.bun.NewSelect().
		Conn(tx).
		Table("commits").
		Column("id").
		Where("repo_id = ?", repoID).
		Where("sha = ?", newSHA).
		Scan(ctx, &commitID)
	if errors.Is(err, sql.ErrNoRows) {
		row := commitRow{
			RepoID:    repoID,
			SHA:       newSHA,
			Author:    author,
			Subject:   subject,
			Timestamp: dbTimeFromValue(timestamp),
		}
		insertErr := db.bun.NewInsert().
			Model(&row).
			Conn(tx).
			Column("repo_id", "sha", "author", "subject", "timestamp").
			Returning("id").
			Scan(ctx)
		if insertErr != nil {
			return 0, fmt.Errorf("create commit: %w", insertErr)
		}
		commitID = row.ID
	} else if err != nil {
		return 0, fmt.Errorf("find commit: %w", err)
	}

	now := dbTimeFromValue(time.Now())
	result, err := db.bun.NewUpdate().
		Model((*jobRow)(nil)).
		Conn(tx).
		Set("git_ref = ?", newSHA).
		Set("commit_id = ?", commitID).
		Set("patch_id = ?", nullString(patchID)).
		Set("updated_at = ?", now).
		Where("git_ref = ?", oldSHA).
		Where("repo_id = ?", repoID).
		Where("status != ?", JobStatusRunning).
		Where("patch_id IS NULL OR patch_id = '' OR patch_id = ?", patchID).
		Exec(ctx)
	if err != nil {
		return 0, fmt.Errorf("remap job git_ref: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit remap tx: %w", err)
	}
	return int(n), nil
}

// InsertSkippedDesignJobParams describes a row inserted by InsertSkippedDesignJob.
type InsertSkippedDesignJobParams struct {
	RepoID     int64
	CommitID   int64 // 0 means no commit (range/dirty); binds as NULL
	GitRef     string
	Branch     string
	SkipReason string
}

// AutoDesignAgentSentinel is the agent name used for auto-design rows
// where no specific agent has been bound yet (skipped rows, queued
// follow-up reviews/classify jobs). The Postgres schema requires
// agent NOT NULL; downstream resolvers replace this with the real
// classify_agent / design_agent at execution time.
const AutoDesignAgentSentinel = "auto-design"

// skipReasonMaxLen caps skip_reason. The reason flows into PR comments
// and TUI cells; capping length + stripping control characters
// prevents terminal-escape injection or markdown abuse via failure
// messages built from raw stderr / model output.
const skipReasonMaxLen = 200

// sanitizeSkipReason strips control characters (folding newlines/tabs
// to spaces) and caps length. Applied at every storage entry point
// that writes to skip_reason so the column is always safe to render.
func sanitizeSkipReason(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			b.WriteRune(' ')
		case unicode.IsControl(r):
			// Drop other control characters entirely.
		default:
			b.WriteRune(r)
		}
	}
	cleaned := strings.TrimSpace(b.String())
	if skipReasonRuneCount(cleaned) > skipReasonMaxLen {
		cleaned = truncateSkipReasonRunes(cleaned, skipReasonMaxLen)
	}
	return cleaned
}

func skipReasonRuneCount(s string) int {
	n := 0
	for range s {
		n++
	}
	return n
}

func truncateSkipReasonRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	count := 0
	for i := range s {
		if count == n {
			return s[:i]
		}
		count++
	}
	return s
}

// InsertSkippedDesignJob inserts a review_job row with status=skipped,
// review_type='design', and source='auto_design'. The auto_design source
// means it participates in the partial unique dedup index; ON CONFLICT
// DO NOTHING makes this a no-op when another auto-design producer already
// recorded the outcome.
func (db *DB) InsertSkippedDesignJob(p InsertSkippedDesignJobParams) error {
	now := time.Now()
	var commitID *int64
	if p.CommitID > 0 {
		commitID = &p.CommitID
	}
	row := jobRow{
		RepoID:     p.RepoID,
		CommitID:   commitID,
		GitRef:     p.GitRef,
		Branch:     optionalString(p.Branch),
		Agent:      AutoDesignAgentSentinel,
		Status:     JobStatusSkipped,
		ReviewType: "design",
		SkipReason: optionalString(sanitizeSkipReason(p.SkipReason)),
		JobType:    JobTypeReview,
		Source:     optionalString(JobSourceAutoDesign),
		EnqueuedAt: dbTimeFromValue(now),
		FinishedAt: dbTimeFromValue(now),
		UpdatedAt:  dbTimeFromValue(now),
	}
	_, err := db.bun.NewInsert().
		Model(&row).
		Column(
			"repo_id", "commit_id", "git_ref", "branch", "agent", "status",
			"review_type", "skip_reason", "job_type", "source", "enqueued_at",
			"finished_at", "updated_at",
		).
		On("CONFLICT DO NOTHING").
		Exec(context.Background())
	if err != nil {
		return fmt.Errorf("insert skipped design row: %w", err)
	}
	return nil
}

// EnqueueAutoDesignJob creates a job tagged source='auto_design' (either a
// design review or a classify job). Returns the new row's id, or 0 if
// another producer won the race (no-op). The caller is expected to
// resolve the real execution agent/model before calling — the sentinel
// is only used when Agent is empty.
func (db *DB) EnqueueAutoDesignJob(p EnqueueOpts) (int64, error) {
	jobType := p.JobType
	if jobType == "" {
		jobType = JobTypeReview
	}
	agentName := p.Agent
	if agentName == "" {
		agentName = AutoDesignAgentSentinel
	}
	var commitID *int64
	if p.CommitID > 0 {
		commitID = &p.CommitID
	}
	now := time.Now()
	row := jobRow{
		RepoID:     p.RepoID,
		CommitID:   commitID,
		GitRef:     p.GitRef,
		Branch:     optionalString(p.Branch),
		Agent:      agentName,
		Model:      optionalString(p.Model),
		Status:     JobStatusQueued,
		JobType:    jobType,
		ReviewType: p.ReviewType,
		Source:     optionalString(JobSourceAutoDesign),
		EnqueuedAt: dbTimeFromValue(now),
		UpdatedAt:  dbTimeFromValue(now),
	}
	err := db.bun.NewInsert().
		Model(&row).
		Column(
			"repo_id", "commit_id", "git_ref", "branch", "agent", "model",
			"status", "job_type", "review_type", "source", "enqueued_at",
			"updated_at",
		).
		On("CONFLICT DO NOTHING").
		Returning("id").
		Scan(context.Background())
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return row.ID, err
}

// PromoteClassifyToDesignReview converts a classify row into a queued
// design review via UPDATE (not INSERT), so the follow-up does not collide
// with the partial unique index that already covers the classify row's slot.
//
// agent and model must resolve to a real registered agent at the moment
// of promotion: the row was inserted with the AutoDesignAgentSentinel,
// and the worker that picks it up next looks up agent.Get(job.Agent),
// which would fail on the sentinel. The caller is expected to resolve
// the design-workflow agent/model via config before calling.
//
// The WHERE clause pins this to the active execution attempt
// (status='running' AND worker_id=?). A stale worker whose job was canceled,
// reclaimed, or retried will affect zero rows and receive sql.ErrNoRows.
func (db *DB) PromoteClassifyToDesignReview(classifyJobID int64, workerID, agent, model string) error {
	res, err := db.bun.NewUpdate().
		Model((*jobRow)(nil)).
		Set("job_type = ?", JobTypeReview).
		Set("status = ?", JobStatusQueued).
		Set("agent = ?", agent).
		Set("model = ?", nullString(model)).
		Set("worker_id = NULL").
		Set("started_at = NULL").
		Set("finished_at = NULL").
		Set("session_id = NULL").
		Set("token_usage = NULL").
		Set("command_line = NULL").
		Set("agent_invoked = 0").
		Set("synced_at = NULL").
		Set("prompt = NULL").
		Set("prompt_prebuilt = 0").
		Set("error = NULL").
		Set("updated_at = ?", dbTimeFromValue(time.Now())).
		Where("id = ?", classifyJobID).
		Where("job_type = ?", JobTypeClassify).
		Where("source = ?", JobSourceAutoDesign).
		Where("status = ?", JobStatusRunning).
		Where("worker_id = ?", workerID).
		Exec(context.Background())
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// MarkClassifyAsSkippedDesign converts a classify row into a terminal
// skipped design row via UPDATE (not INSERT). Same active-attempt guard as
// PromoteClassifyToDesignReview.
//
// reason is the public-facing skip_reason (rendered in PR comments, so
// must be redacted at the caller). errorDetail is the full internal error
// text persisted to the error column for operator debugging when the
// skip is caused by a classifier failure — pass "" on the clean "no
// design review needed" path.
func (db *DB) MarkClassifyAsSkippedDesign(classifyJobID int64, workerID, reason, errorDetail string) error {
	now := dbTimeFromValue(time.Now())
	res, err := db.bun.NewUpdate().
		Model((*jobRow)(nil)).
		Set("job_type = ?", JobTypeReview).
		Set("status = ?", JobStatusSkipped).
		Set("skip_reason = ?", sanitizeSkipReason(reason)).
		Set("error = ?", nullString(errorDetail)).
		Set("finished_at = ?", now).
		Set("updated_at = ?", now).
		Where("id = ?", classifyJobID).
		Where("job_type = ?", JobTypeClassify).
		Where("source = ?", JobSourceAutoDesign).
		Where("status = ?", JobStatusRunning).
		Where("worker_id = ?", workerID).
		Exec(context.Background())
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// ListJobsByStatus returns review jobs with the given status for a repo.
// Fields populated: ID, RepoID, CommitID, GitRef, Branch, Status, JobType,
// ReviewType, SkipReason, Source, EnqueuedAt.
func (db *DB) ListJobsByStatus(repoID int64, status JobStatus) ([]ReviewJob, error) {
	var rows []jobRow
	if err := db.bun.NewSelect().
		Model(&rows).
		Column("id", "repo_id", "commit_id", "git_ref", "status", "enqueued_at").
		ColumnExpr("COALESCE(branch, '') AS branch").
		ColumnExpr("COALESCE(job_type, 'review') AS job_type").
		ColumnExpr("COALESCE(review_type, '') AS review_type").
		ColumnExpr("COALESCE(skip_reason, '') AS skip_reason").
		ColumnExpr("COALESCE(source, '') AS source").
		Where("repo_id = ?", repoID).
		Where("status = ?", status).
		OrderExpr("enqueued_at DESC").
		Scan(context.Background()); err != nil {
		return nil, err
	}
	var out []ReviewJob
	for _, row := range rows {
		var job ReviewJob
		row.applyToModel(&job)
		out = append(out, job)
	}
	return out, nil
}

// MaybeReleasePanelSynthesis clears claim_blocked on a panel run's synthesis
// job once every member job is terminal. Terminal = done/failed/canceled/
// skipped/applied/rebased. Completion is derived from member rows, not from
// maintained counters, so it cannot drift.
//
// The UPDATE is idempotent and race-safe: concurrent member completions
// either still see a non-terminal member (no-op) or all-terminal (one
// UPDATE flips the flag, the rest are no-ops). It releases even when every
// member failed or was canceled, so the synthesis job always runs and lands
// a durable review — the parent never hangs.
func (db *DB) MaybeReleasePanelSynthesis(panelRunUUID string) error {
	if panelRunUUID == "" {
		return nil
	}
	now := dbTimeFromValue(time.Now())
	// Raw SQL allowlist: guarded atomic state transition. The correlated
	// terminal-member check and synthesis release must be evaluated in the
	// same statement so concurrent member completions cannot release early.
	_, err := db.bun.NewRaw(`
		UPDATE review_jobs
		   SET claim_blocked = 0, updated_at = ?
		 WHERE panel_run_uuid = ?
		   AND panel_role = 'synthesis'
		   AND claim_blocked = 1
		   AND NOT EXISTS (
		       SELECT 1 FROM review_jobs m
		        WHERE m.panel_run_uuid = review_jobs.panel_run_uuid
		          AND m.panel_role = 'member'
		          AND m.status NOT IN ('done','failed','canceled','skipped','applied','rebased')
		   )
	`, now, panelRunUUID).
		Exec(context.Background())
	if err != nil {
		return fmt.Errorf("release panel synthesis %q: %w", panelRunUUID, err)
	}
	return nil
}

// ListStuckPanelRuns returns the panel_run_uuid of every run whose synthesis
// job is still claim_blocked even though all its member jobs are terminal.
// These are the runs the safety sweep must release. Reuses the same
// member-terminal predicate as MaybeReleasePanelSynthesis.
func (db *DB) ListStuckPanelRuns() ([]string, error) {
	var uuids []string
	if err := db.bun.NewSelect().
		TableExpr("review_jobs AS s").
		ColumnExpr("s.panel_run_uuid").
		Where("s.panel_role = 'synthesis'").
		Where("s.claim_blocked = 1").
		Where(`NOT EXISTS (
			SELECT 1 FROM review_jobs AS m
			WHERE m.panel_run_uuid = s.panel_run_uuid
			  AND m.panel_role = 'member'
			  AND m.status NOT IN ('done','failed','canceled','skipped','applied','rebased')
		)`).
		Scan(context.Background(), &uuids); err != nil {
		return nil, fmt.Errorf("query stuck panel runs: %w", err)
	}
	return uuids, nil
}

// GetPanelMembers returns the member jobs of a panel run ordered by
// panel_member_index. The synthesis row is excluded. Each job carries the
// full joined/hydrated fields (verdict applied) so callers can render member
// verdicts without a second fetch.
func (db *DB) GetPanelMembers(panelRunUUID string) ([]ReviewJob, error) {
	if panelRunUUID == "" {
		return nil, nil
	}
	var rows []jobHydrationRow
	query := db.bun.NewSelect().
		TableExpr("review_jobs AS j").
		Join("JOIN repos AS r ON r.id = j.repo_id").
		Join("LEFT JOIN commits AS c ON c.id = j.commit_id").
		Join("LEFT JOIN reviews AS rv ON rv.job_id = j.id").
		Where("j.panel_run_uuid = ?", panelRunUUID).
		Where("j.panel_role = 'member'").
		OrderExpr("j.panel_member_index, j.id")
	query = addJobSelectColumns(query, sqliteJobListColumns).
		ColumnExpr("j.prompt")
	if err := query.Scan(context.Background(), &rows); err != nil {
		return nil, fmt.Errorf("query panel members: %w", err)
	}

	var jobs []ReviewJob
	for _, row := range rows {
		jobs = append(jobs, row.toModel())
	}
	return jobs, nil
}

// GetSynthesisJob returns the synthesis (parent) job for a panel run, or
// (nil, nil) when panelRunUUID is empty or no synthesis row exists. The job
// carries the full joined/hydrated fields (verdict applied), mirroring
// GetPanelMembers.
func (db *DB) GetSynthesisJob(panelRunUUID string) (*ReviewJob, error) {
	if panelRunUUID == "" {
		return nil, nil
	}
	var row jobHydrationRow
	query := db.bun.NewSelect().
		TableExpr("review_jobs AS j").
		Join("JOIN repos AS r ON r.id = j.repo_id").
		Join("LEFT JOIN commits AS c ON c.id = j.commit_id").
		Join("LEFT JOIN reviews AS rv ON rv.job_id = j.id").
		Where("j.panel_run_uuid = ?", panelRunUUID).
		Where("j.panel_role = 'synthesis'").
		Limit(1)
	query = addJobSelectColumns(query, sqliteJobListColumns).
		ColumnExpr("j.prompt")
	if err := query.Scan(context.Background(), &row); errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, fmt.Errorf("query panel synthesis: %w", err)
	}
	job := row.toModel()
	return &job, nil
}

// GetPanelMemberReviews returns one BatchReviewResult per member of a panel
// run, joined to its review output, ordered by panel_member_index. The
// synthesis row is excluded.
func (db *DB) GetPanelMemberReviews(panelRunUUID string) ([]BatchReviewResult, error) {
	if panelRunUUID == "" {
		return nil, nil
	}
	var rows []struct {
		JobID                 int64  `bun:"job_id"`
		Agent                 string `bun:"agent"`
		ReviewType            string `bun:"review_type"`
		PanelMemberName       string `bun:"panel_member_name"`
		Output                string `bun:"output"`
		Status                string `bun:"status"`
		Error                 string `bun:"error"`
		SkipReason            string `bun:"skip_reason"`
		PanelMemberConfigJSON string `bun:"panel_member_config_json"`
		StartedAt             string `bun:"started_at"`
		FinishedAt            string `bun:"finished_at"`
		TokenUsage            string `bun:"token_usage"`
	}
	if err := db.bun.NewSelect().
		TableExpr("review_jobs AS j").
		ColumnExpr("j.id AS job_id").
		ColumnExpr("j.agent").
		ColumnExpr("j.review_type").
		ColumnExpr("COALESCE(j.panel_member_name, '') AS panel_member_name").
		ColumnExpr("COALESCE(rv.output, '') AS output").
		ColumnExpr("j.status").
		ColumnExpr("COALESCE(j.error, '') AS error").
		ColumnExpr("COALESCE(j.skip_reason, '') AS skip_reason").
		ColumnExpr("COALESCE(j.panel_member_config_json, '') AS panel_member_config_json").
		ColumnExpr("COALESCE(j.started_at, '') AS started_at").
		ColumnExpr("COALESCE(j.finished_at, '') AS finished_at").
		ColumnExpr("COALESCE(j.token_usage, '') AS token_usage").
		Join("LEFT JOIN reviews AS rv ON rv.job_id = j.id").
		Where("j.panel_run_uuid = ?", panelRunUUID).
		Where("j.panel_role = 'member'").
		OrderExpr("j.panel_member_index, j.id").
		Scan(context.Background(), &rows); err != nil {
		return nil, fmt.Errorf("query panel member reviews: %w", err)
	}

	var results []BatchReviewResult
	for _, row := range rows {
		results = append(results, BatchReviewResult{
			JobID:                 row.JobID,
			Agent:                 row.Agent,
			ReviewType:            row.ReviewType,
			PanelMemberName:       row.PanelMemberName,
			Output:                row.Output,
			Status:                row.Status,
			Error:                 row.Error,
			SkipReason:            row.SkipReason,
			PanelMemberConfigJSON: row.PanelMemberConfigJSON,
			StartedAt:             row.StartedAt,
			FinishedAt:            row.FinishedAt,
			TokenUsage:            row.TokenUsage,
		})
	}
	return results, nil
}

// PanelSummary is the per-run member breakdown rendered on a collapsed panel
// parent row. members_terminal counts the release terminal set
// (done/applied/rebased/failed/canceled/skipped); members_succeeded counts
// rows with a usable review (done/applied/rebased). A single "done" count is
// ambiguous — an all-failed panel is finished but has zero done members — so
// the terminal set is broken out explicitly.
type PanelSummary struct {
	PanelRunUUID        string     `json:"panel_run_uuid"`
	MembersTotal        int        `json:"members_total"`
	MembersTerminal     int        `json:"members_terminal"`
	MembersSucceeded    int        `json:"members_succeeded"`
	MembersFailed       int        `json:"members_failed"`
	MembersCanceled     int        `json:"members_canceled"`
	MembersSkipped      int        `json:"members_skipped"`
	MembersWithCost     int        `json:"members_with_cost,omitempty"`
	MembersCostUSD      float64    `json:"members_cost_usd,omitempty"`
	MembersCostComplete bool       `json:"members_cost_complete,omitempty"`
	FirstStartedAt      *time.Time `json:"first_started_at,omitempty"`
}

// GetPanelSummaries computes the member breakdown for each given panel run in
// one GROUP BY aggregate (no per-row N+1). Runs with no member rows are
// absent from the returned map.
func (db *DB) GetPanelSummaries(runUUIDs []string) (map[string]PanelSummary, error) {
	if len(runUUIDs) == 0 {
		return nil, nil
	}
	var rows []struct {
		PanelRunUUID     string  `bun:"panel_run_uuid"`
		MembersTotal     int     `bun:"members_total"`
		MembersTerminal  int     `bun:"members_terminal"`
		MembersSucceeded int     `bun:"members_succeeded"`
		MembersFailed    int     `bun:"members_failed"`
		MembersCanceled  int     `bun:"members_canceled"`
		MembersSkipped   int     `bun:"members_skipped"`
		MembersWithCost  int     `bun:"members_with_cost"`
		MembersCostUSD   float64 `bun:"members_cost_usd"`
		FirstStartedAt   dbTime  `bun:"first_started_at"`
	}
	if err := db.bun.NewSelect().
		Table("review_jobs").
		Column("panel_run_uuid").
		ColumnExpr("COUNT(*) AS members_total").
		ColumnExpr("COALESCE(SUM(CASE WHEN status IN ('done','applied','rebased','failed','canceled','skipped') THEN 1 ELSE 0 END), 0) AS members_terminal").
		ColumnExpr("COALESCE(SUM(CASE WHEN status IN ('done','applied','rebased') THEN 1 ELSE 0 END), 0) AS members_succeeded").
		ColumnExpr("COALESCE(SUM(CASE WHEN status = 'failed' THEN 1 ELSE 0 END), 0) AS members_failed").
		ColumnExpr("COALESCE(SUM(CASE WHEN status = 'canceled' THEN 1 ELSE 0 END), 0) AS members_canceled").
		ColumnExpr("COALESCE(SUM(CASE WHEN status = 'skipped' THEN 1 ELSE 0 END), 0) AS members_skipped").
		ColumnExpr("COALESCE(SUM(CASE WHEN json_valid(token_usage) AND json_extract(token_usage, '$.has_cost') THEN 1 ELSE 0 END), 0) AS members_with_cost").
		ColumnExpr("CAST(COALESCE(SUM(CASE WHEN json_valid(token_usage) AND json_extract(token_usage, '$.has_cost') THEN json_extract(token_usage, '$.cost_usd') ELSE 0 END), 0) AS REAL) AS members_cost_usd").
		ColumnExpr("MIN(started_at) AS first_started_at").
		Where("panel_role = 'member'").
		Where("panel_run_uuid IN (?)", bun.List(runUUIDs)).
		GroupExpr("panel_run_uuid").
		Scan(context.Background(), &rows); err != nil {
		return nil, fmt.Errorf("query panel summaries: %w", err)
	}

	out := make(map[string]PanelSummary)
	for _, row := range rows {
		summary := PanelSummary{
			PanelRunUUID:        row.PanelRunUUID,
			MembersTotal:        row.MembersTotal,
			MembersTerminal:     row.MembersTerminal,
			MembersSucceeded:    row.MembersSucceeded,
			MembersFailed:       row.MembersFailed,
			MembersCanceled:     row.MembersCanceled,
			MembersSkipped:      row.MembersSkipped,
			MembersWithCost:     row.MembersWithCost,
			MembersCostUSD:      row.MembersCostUSD,
			MembersCostComplete: row.MembersTotal > 0 && row.MembersWithCost == row.MembersTotal,
			FirstStartedAt:      row.FirstStartedAt.pointer(),
		}
		out[summary.PanelRunUUID] = summary
	}
	return out, nil
}

// HasAutoDesignSlotForCommit reports whether the auto-design dedup slot is
// already occupied for (repo_id, commit_sha). Returns true when any row
// exists with review_type='design' and source='auto_design' — covering
// queued classify jobs, queued/running/done design reviews, and skipped
// design rows.
//
// Commitless auto-design rows (commit_id IS NULL, inserted when commit
// metadata lookup failed at dispatch time) also count as occupying the
// slot when their git_ref matches the SHA. Otherwise a later dispatch
// that successfully resolves the commit would create a duplicate row
// for the same change — the partial unique index on (repo_id, commit_id,
// review_type) only catches duplicates where commit_id matches, and
// SQL's NULL != NULL semantics let (NULL, ...) coexist with (123, ...).
//
// This is a performance shortcut; the partial unique index enforces
// correctness for commit-backed inserts.
func (db *DB) HasAutoDesignSlotForCommit(repoID int64, sha string) (bool, error) {
	var n int
	err := db.bun.NewSelect().
		TableExpr("review_jobs AS rj").
		ColumnExpr("COUNT(*)").
		Join("LEFT JOIN commits AS c ON rj.commit_id = c.id").
		Where("rj.repo_id = ?", repoID).
		Where("rj.review_type = 'design'").
		Where("rj.source = 'auto_design'").
		Where("c.sha = ? OR (rj.commit_id IS NULL AND rj.git_ref = ?)", sha, sha).
		Scan(context.Background(), &n)
	if err != nil {
		return false, fmt.Errorf("query auto-design slot: %w", err)
	}
	return n > 0, nil
}

func encodeDirtyFiles(files []string) (string, error) {
	if len(files) == 0 {
		return "", nil
	}
	data, err := json.Marshal(files)
	if err != nil {
		return "", fmt.Errorf("encode dirty files: %w", err)
	}
	return string(data), nil
}

func decodeDirtyFiles(data string) []string {
	if data == "" {
		return nil
	}
	var files []string
	if err := json.Unmarshal([]byte(data), &files); err != nil {
		return nil
	}
	return files
}
