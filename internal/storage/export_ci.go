package storage

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"
)

const (
	ciMetricsCursorVersion    = 1
	ciMetricsDefaultPageLimit = 500
	ciMetricsMaxPageLimit     = 5000
)

// ExportCIMetricsOptions bounds one page of the CI metrics export.
type ExportCIMetricsOptions struct {
	Since  time.Time
	Until  time.Time
	Cursor string
	Limit  int
	// Legacy switches the export to the frozen pre-panel ci_pr_reviews era
	// (~2026-02 to ~2026-06), one row per reviewed PR head, before panels
	// existed. Legacy turnaround (first_attempt_at -> posted_at) measures
	// job enqueue time -> ci_pr_reviews record time; it is NOT comparable
	// to panel-era turnaround (first poller attempt -> panel posted), so
	// callers must not mix the two eras in one turnaround-time analysis.
	// Legacy cursors are namespaced and are rejected if replayed with
	// Legacy unset, and vice versa.
	Legacy bool
}

// ExportCIMetricsPage is one bounded page of finalized CI panel records.
type ExportCIMetricsPage struct {
	Panels     []ExportCIPanel `json:"panels"`
	Truncated  bool            `json:"truncated"`
	NextCursor *string         `json:"next_cursor"`
}

// ExportCIPanel is one finalized CI panel run. Outcome is never empty:
// rows finalized before outcome persistence existed export as "unknown",
// and FirstAttemptAt/AttemptCount stay null for them (there is
// deliberately no fallback to panel_created_at, which undercounts
// deferred retries). SynthesisAgent/SynthesisModel are self-contained
// snapshots taken at finalization (MarkPanelPosted), so they survive
// deletion of the underlying review_jobs rows (e.g. cascade repo delete).
// Jobs, by contrast, reflects the currently retained review_jobs rows for
// the panel run and may be empty for panels whose repo was cascade-deleted.
type ExportCIPanel struct {
	GithubRepo     string             `json:"github_repo"`
	PRNumber       int64              `json:"pr_number"`
	HeadSHA        string             `json:"head_sha"`
	PanelCreatedAt string             `json:"panel_created_at"`
	PostedAt       string             `json:"posted_at"`
	FirstAttemptAt *string            `json:"first_attempt_at"`
	AttemptCount   *int64             `json:"attempt_count"`
	Outcome        string             `json:"outcome"`
	SynthesisAgent *string            `json:"synthesis_agent"`
	SynthesisModel *string            `json:"synthesis_model"`
	Jobs           []ExportCIPanelJob `json:"jobs"`
}

// ExportCIPanelJob is one member or synthesis job of an exported panel.
type ExportCIPanelJob struct {
	JobUUID    string  `json:"job_uuid"`
	Role       string  `json:"role"`
	Agent      string  `json:"agent"`
	Model      *string `json:"model"`
	Provider   *string `json:"provider"`
	Status     string  `json:"status"`
	StartedAt  *string `json:"started_at"`
	FinishedAt *string `json:"finished_at"`
}

type ciMetricsCursor struct {
	Version    int    `json:"version"`
	DatabaseID string `json:"database_id"`
	PostedAt   string `json:"posted_at"`
	PanelID    int64  `json:"panel_id"`
	// Legacy namespaces the cursor to ExportCIMetricsOptions.Legacy so a
	// panel-era cursor can never be replayed against the legacy export (or
	// vice versa): the two eras page over different tables and ids, and
	// silently mixing them would skip or repeat rows.
	Legacy bool `json:"legacy,omitempty"`
}

// ErrExportCIMetricsCursorModeMismatch is returned when a cursor minted for
// one ExportCIMetrics mode (legacy vs. panel) is replayed against the other.
var ErrExportCIMetricsCursorModeMismatch = errors.New("ci metrics cursor mode mismatch")

// ExportCIMetrics returns one bounded page of finalized CI panel records (or,
// with Legacy set, frozen pre-panel ci_pr_reviews records) ordered by
// (posted_at, id) ascending, with the same opaque-cursor and
// database_id-reset contract as ExportReviews.
func (db *DB) ExportCIMetrics(opts ExportCIMetricsOptions) (ExportCIMetricsPage, error) {
	switch {
	case opts.Limit <= 0:
		opts.Limit = ciMetricsDefaultPageLimit
	case opts.Limit > ciMetricsMaxPageLimit:
		opts.Limit = ciMetricsMaxPageLimit
	}

	cursor, err := db.resolveCIMetricsCursor(opts.Cursor, opts.Legacy)
	if err != nil {
		return ExportCIMetricsPage{}, err
	}
	if opts.Legacy {
		return db.exportCIMetricsLegacy(opts, cursor)
	}
	return db.exportCIMetricsPanels(opts, cursor)
}

// exportCIMetricsPanels is the ci_pr_panels-backed page builder used by
// ExportCIMetrics when Legacy is unset.
func (db *DB) exportCIMetricsPanels(opts ExportCIMetricsOptions, cursor *ciMetricsCursor) (ExportCIMetricsPage, error) {
	postedExpr := sqliteNormalizedTimestampExpr("cp.posted_at")
	conditions := []string{"cp.posted_at IS NOT NULL"}
	args := make([]any, 0)
	if !opts.Since.IsZero() {
		conditions = append(conditions, postedExpr+" >= datetime(?)")
		args = append(args, opts.Since.UTC().Format(time.RFC3339))
	}
	if !opts.Until.IsZero() {
		conditions = append(conditions, postedExpr+" < datetime(?)")
		args = append(args, opts.Until.UTC().Format(time.RFC3339))
	}
	if cursor != nil {
		conditions = append(conditions,
			"("+postedExpr+" > datetime(?) OR ("+postedExpr+" = datetime(?) AND cp.id > ?))")
		args = append(args, cursor.PostedAt, cursor.PostedAt, cursor.PanelID)
	}
	args = append(args, opts.Limit+1)

	query := `
		SELECT cp.id, cp.github_repo, cp.pr_number, cp.head_sha,
		       cp.created_at, cp.posted_at, cp.first_attempt_at,
		       cp.attempt_count, COALESCE(cp.outcome, '` + PanelOutcomeUnknown + `'),
		       cp.panel_run_uuid,
		       COALESCE(NULLIF(cp.synthesis_agent, ''), sj.agent),
		       COALESCE(NULLIF(cp.synthesis_model, ''), sj.model)
		FROM ci_pr_panels cp
		LEFT JOIN review_jobs sj ON sj.id = cp.synthesis_job_id
		WHERE ` + strings.Join(conditions, " AND ") + `
		ORDER BY ` + postedExpr + ` ASC, cp.id ASC
		LIMIT ?`
	rows, err := db.Query(query, args...)
	if err != nil {
		return ExportCIMetricsPage{}, fmt.Errorf("query ci metrics export: %w", err)
	}
	defer rows.Close()

	page := ExportCIMetricsPage{Panels: []ExportCIPanel{}}
	var lastID int64
	var lastPosted string
	type pendingPanel struct {
		panel   ExportCIPanel
		runUUID string
	}
	var pending []pendingPanel
	for rows.Next() {
		id, panel, runUUID, err := scanCIMetricsRow(rows)
		if err != nil {
			return ExportCIMetricsPage{}, err
		}
		if len(pending) == opts.Limit {
			page.Truncated = true
			break
		}
		pending = append(pending, pendingPanel{panel: panel, runUUID: runUUID})
		lastID = id
		lastPosted = panel.PostedAt
	}
	if err := rows.Err(); err != nil {
		return ExportCIMetricsPage{}, err
	}

	for _, item := range pending {
		jobs, err := db.exportCIPanelJobs(item.runUUID)
		if err != nil {
			return ExportCIMetricsPage{}, err
		}
		item.panel.Jobs = jobs
		page.Panels = append(page.Panels, item.panel)
	}

	if len(page.Panels) > 0 {
		databaseID, err := db.GetDatabaseID()
		if err != nil {
			return ExportCIMetricsPage{}, err
		}
		next := encodeCIMetricsCursor(databaseID, lastPosted, lastID, false)
		if next != "" {
			page.NextCursor = &next
		}
	}
	return page, nil
}

// exportCIMetricsLegacy is the ci_pr_reviews-backed page builder used by
// ExportCIMetrics when Legacy is set. Each row joins its linked review_jobs
// row (job_id) and is shaped into the same ExportCIPanel struct: outcome is
// always PanelOutcomeLegacyReview, attempt_count is always nil (legacy rows
// predate retry-attempt tracking), and posted_at/first_attempt_at/
// panel_created_at are derived from cr.created_at and j.enqueued_at rather
// than from a real panel lifecycle. Jobs always contains exactly the one
// linked job, tagged role "review".
func (db *DB) exportCIMetricsLegacy(opts ExportCIMetricsOptions, cursor *ciMetricsCursor) (ExportCIMetricsPage, error) {
	createdExpr := sqliteNormalizedTimestampExpr("cr.created_at")
	conditions := []string{"cr.created_at IS NOT NULL"}
	args := make([]any, 0)
	if !opts.Since.IsZero() {
		conditions = append(conditions, createdExpr+" >= datetime(?)")
		args = append(args, opts.Since.UTC().Format(time.RFC3339))
	}
	if !opts.Until.IsZero() {
		conditions = append(conditions, createdExpr+" < datetime(?)")
		args = append(args, opts.Until.UTC().Format(time.RFC3339))
	}
	if cursor != nil {
		conditions = append(conditions,
			"("+createdExpr+" > datetime(?) OR ("+createdExpr+" = datetime(?) AND cr.id > ?))")
		args = append(args, cursor.PostedAt, cursor.PostedAt, cursor.PanelID)
	}
	args = append(args, opts.Limit+1)

	query := `
		SELECT cr.id, cr.github_repo, cr.pr_number, cr.head_sha, cr.created_at,
		       j.uuid, j.enqueued_at, j.agent, j.model, j.provider, j.status,
		       j.started_at, j.finished_at
		FROM ci_pr_reviews cr
		JOIN review_jobs j ON j.id = cr.job_id
		WHERE ` + strings.Join(conditions, " AND ") + `
		ORDER BY ` + createdExpr + ` ASC, cr.id ASC
		LIMIT ?`
	rows, err := db.Query(query, args...)
	if err != nil {
		return ExportCIMetricsPage{}, fmt.Errorf("query legacy ci metrics export: %w", err)
	}
	defer rows.Close()

	page := ExportCIMetricsPage{Panels: []ExportCIPanel{}}
	var lastID int64
	var lastPosted string
	for rows.Next() {
		id, panel, err := scanLegacyCIMetricsRow(rows)
		if err != nil {
			return ExportCIMetricsPage{}, err
		}
		if len(page.Panels) == opts.Limit {
			page.Truncated = true
			break
		}
		page.Panels = append(page.Panels, panel)
		lastID = id
		lastPosted = panel.PostedAt
	}
	if err := rows.Err(); err != nil {
		return ExportCIMetricsPage{}, err
	}

	if len(page.Panels) > 0 {
		databaseID, err := db.GetDatabaseID()
		if err != nil {
			return ExportCIMetricsPage{}, err
		}
		next := encodeCIMetricsCursor(databaseID, lastPosted, lastID, true)
		if next != "" {
			page.NextCursor = &next
		}
	}
	return page, nil
}

// scanLegacyCIMetricsRow scans one ci_pr_reviews-joined-review_jobs export
// row into an ExportCIPanel, mirroring scanCIMetricsRow's null-handling for
// the panel-era query.
func scanLegacyCIMetricsRow(rows *sql.Rows) (int64, ExportCIPanel, error) {
	var (
		id         int64
		panel      ExportCIPanel
		createdAt  sql.NullString
		jobUUID    string
		enqueuedAt sql.NullString
		agent      string
		model      sql.NullString
		provider   sql.NullString
		status     string
		startedAt  sql.NullString
		finishedAt sql.NullString
	)
	if err := rows.Scan(&id, &panel.GithubRepo, &panel.PRNumber, &panel.HeadSHA, &createdAt,
		&jobUUID, &enqueuedAt, &agent, &model, &provider, &status, &startedAt, &finishedAt); err != nil {
		return 0, ExportCIPanel{}, fmt.Errorf("scan legacy ci metrics row: %w", err)
	}
	panel.Outcome = PanelOutcomeLegacyReview
	if createdAt.Valid {
		panel.PostedAt = formatExportTime(parseSQLiteTime(createdAt.String))
	}
	if enqueuedAt.Valid {
		v := formatExportTime(parseSQLiteTime(enqueuedAt.String))
		panel.FirstAttemptAt = &v
		panel.PanelCreatedAt = v
	}
	if agent != "" {
		panel.SynthesisAgent = &agent
	}
	if model.Valid && model.String != "" {
		panel.SynthesisModel = &model.String
	}

	job := ExportCIPanelJob{JobUUID: jobUUID, Role: "review", Agent: agent, Status: status}
	if model.Valid && model.String != "" {
		job.Model = &model.String
	}
	if provider.Valid && provider.String != "" {
		job.Provider = &provider.String
	}
	if startedAt.Valid {
		v := formatExportTime(parseSQLiteTime(startedAt.String))
		job.StartedAt = &v
	}
	if finishedAt.Valid {
		v := formatExportTime(parseSQLiteTime(finishedAt.String))
		job.FinishedAt = &v
	}
	panel.Jobs = []ExportCIPanelJob{job}
	return id, panel, nil
}

// scanCIMetricsRow scans one ci_pr_panels export row and maps its nullable
// SQL columns onto an ExportCIPanel. It returns the panel's database id and
// panel_run_uuid alongside the populated panel so the caller can page and
// join review_jobs without re-touching sql.Null* locals.
func scanCIMetricsRow(rows *sql.Rows) (int64, ExportCIPanel, string, error) {
	var (
		id             int64
		panel          ExportCIPanel
		createdAt      sql.NullString
		postedAt       sql.NullString
		firstAttemptAt sql.NullString
		attemptCount   sql.NullInt64
		runUUID        string
		synthAgent     sql.NullString
		synthModel     sql.NullString
	)
	if err := rows.Scan(&id, &panel.GithubRepo, &panel.PRNumber, &panel.HeadSHA,
		&createdAt, &postedAt, &firstAttemptAt, &attemptCount, &panel.Outcome,
		&runUUID, &synthAgent, &synthModel); err != nil {
		return 0, ExportCIPanel{}, "", fmt.Errorf("scan ci metrics row: %w", err)
	}
	if createdAt.Valid {
		panel.PanelCreatedAt = formatExportTime(parseSQLiteTime(createdAt.String))
	}
	if postedAt.Valid {
		panel.PostedAt = formatExportTime(parseSQLiteTime(postedAt.String))
	}
	if firstAttemptAt.Valid {
		v := formatExportTime(parseSQLiteTime(firstAttemptAt.String))
		panel.FirstAttemptAt = &v
	}
	if attemptCount.Valid {
		panel.AttemptCount = &attemptCount.Int64
	}
	if synthAgent.Valid && synthAgent.String != "" {
		panel.SynthesisAgent = &synthAgent.String
	}
	if synthModel.Valid && synthModel.String != "" {
		panel.SynthesisModel = &synthModel.String
	}
	return id, panel, runUUID, nil
}

func (db *DB) exportCIPanelJobs(panelRunUUID string) ([]ExportCIPanelJob, error) {
	rows, err := db.Query(`
		SELECT j.uuid, COALESCE(j.panel_role, ''), j.agent, j.model,
		       j.provider, j.status, j.started_at, j.finished_at
		FROM review_jobs j
		WHERE j.panel_run_uuid = ?
		  AND COALESCE(j.panel_role, '') IN ('member','synthesis')
		ORDER BY j.id ASC`, panelRunUUID)
	if err != nil {
		return nil, fmt.Errorf("query ci panel jobs: %w", err)
	}
	defer rows.Close()

	jobs := []ExportCIPanelJob{}
	for rows.Next() {
		var (
			job        ExportCIPanelJob
			model      sql.NullString
			provider   sql.NullString
			startedAt  sql.NullString
			finishedAt sql.NullString
		)
		if err := rows.Scan(&job.JobUUID, &job.Role, &job.Agent, &model,
			&provider, &job.Status, &startedAt, &finishedAt); err != nil {
			return nil, fmt.Errorf("scan ci panel job: %w", err)
		}
		if model.Valid && model.String != "" {
			job.Model = &model.String
		}
		if provider.Valid && provider.String != "" {
			job.Provider = &provider.String
		}
		if startedAt.Valid {
			v := formatExportTime(parseSQLiteTime(startedAt.String))
			job.StartedAt = &v
		}
		if finishedAt.Valid {
			v := formatExportTime(parseSQLiteTime(finishedAt.String))
			job.FinishedAt = &v
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}

func encodeCIMetricsCursor(databaseID, postedAt string, panelID int64, legacy bool) string {
	if databaseID == "" || postedAt == "" || panelID <= 0 {
		return ""
	}
	data, err := json.Marshal(ciMetricsCursor{
		Version:    ciMetricsCursorVersion,
		DatabaseID: databaseID,
		PostedAt:   postedAt,
		PanelID:    panelID,
		Legacy:     legacy,
	})
	if err != nil {
		log.Printf("storage: warning: encode ci metrics cursor: %v", err)
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(data)
}

// resolveCIMetricsCursor decodes and validates an opaque cursor for the
// requested export mode. legacy must match the mode the cursor was minted
// for (ciMetricsCursor.Legacy); a mismatch is rejected with
// ErrExportCIMetricsCursorModeMismatch since the two modes page over
// different tables and ids.
func (db *DB) resolveCIMetricsCursor(cursor string, legacy bool) (*ciMetricsCursor, error) {
	if cursor == "" {
		return nil, nil
	}
	data, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return nil, fmt.Errorf("invalid ci metrics cursor: %w", err)
	}
	var decoded ciMetricsCursor
	if err := json.Unmarshal(data, &decoded); err != nil {
		return nil, fmt.Errorf("invalid ci metrics cursor: %w", err)
	}
	if decoded.Version != ciMetricsCursorVersion {
		return nil, fmt.Errorf("invalid ci metrics cursor: unsupported version %d", decoded.Version)
	}
	if decoded.DatabaseID == "" || decoded.PostedAt == "" || decoded.PanelID <= 0 {
		return nil, errors.New("invalid ci metrics cursor: missing fields")
	}
	if decoded.Legacy != legacy {
		return nil, fmt.Errorf("%w: cursor legacy=%v does not match requested legacy=%v",
			ErrExportCIMetricsCursorModeMismatch, decoded.Legacy, legacy)
	}
	t, err := time.Parse(time.RFC3339Nano, decoded.PostedAt)
	if err != nil {
		return nil, fmt.Errorf("invalid ci metrics cursor timestamp: %w", err)
	}
	decoded.PostedAt = t.UTC().Format(time.RFC3339)

	databaseID, err := db.GetDatabaseID()
	if err != nil {
		return nil, err
	}
	if decoded.DatabaseID != databaseID {
		return nil, fmt.Errorf(
			"%w: cursor database_id %q does not match current database_id %q",
			ErrExportCursorDatabaseMismatch, decoded.DatabaseID, databaseID,
		)
	}
	existsQuery := `
		SELECT COUNT(1) FROM ci_pr_panels cp
		WHERE cp.id = ? AND cp.posted_at IS NOT NULL
		  AND ` + sqliteNormalizedTimestampExpr("cp.posted_at") + ` = datetime(?)`
	if legacy {
		existsQuery = `
			SELECT COUNT(1) FROM ci_pr_reviews cr
			WHERE cr.id = ? AND cr.created_at IS NOT NULL
			  AND ` + sqliteNormalizedTimestampExpr("cr.created_at") + ` = datetime(?)`
	}
	var count int
	if err := db.QueryRow(existsQuery, decoded.PanelID, decoded.PostedAt).Scan(&count); err != nil {
		return nil, fmt.Errorf("validate ci metrics cursor: %w", err)
	}
	if count == 0 {
		return nil, fmt.Errorf("%w: posted_at %q panel_id %d",
			ErrExportCursorNotFound, decoded.PostedAt, decoded.PanelID)
	}
	return &decoded, nil
}
