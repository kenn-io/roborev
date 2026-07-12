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
// deferred retries).
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
}

// ExportCIMetrics returns one bounded page of finalized CI panel records
// ordered by (posted_at, id) ascending, with the same opaque-cursor and
// database_id-reset contract as ExportReviews.
func (db *DB) ExportCIMetrics(opts ExportCIMetricsOptions) (ExportCIMetricsPage, error) {
	switch {
	case opts.Limit <= 0:
		opts.Limit = ciMetricsDefaultPageLimit
	case opts.Limit > ciMetricsMaxPageLimit:
		opts.Limit = ciMetricsMaxPageLimit
	}

	cursor, err := db.resolveCIMetricsCursor(opts.Cursor)
	if err != nil {
		return ExportCIMetricsPage{}, err
	}

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
		       cp.panel_run_uuid, sj.agent, sj.model
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
			return ExportCIMetricsPage{}, fmt.Errorf("scan ci metrics row: %w", err)
		}
		if len(pending) == opts.Limit {
			page.Truncated = true
			break
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
		next := encodeCIMetricsCursor(databaseID, lastPosted, lastID)
		if next != "" {
			page.NextCursor = &next
		}
	}
	return page, nil
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

func encodeCIMetricsCursor(databaseID, postedAt string, panelID int64) string {
	if databaseID == "" || postedAt == "" || panelID <= 0 {
		return ""
	}
	data, err := json.Marshal(ciMetricsCursor{
		Version:    ciMetricsCursorVersion,
		DatabaseID: databaseID,
		PostedAt:   postedAt,
		PanelID:    panelID,
	})
	if err != nil {
		log.Printf("storage: warning: encode ci metrics cursor: %v", err)
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(data)
}

func (db *DB) resolveCIMetricsCursor(cursor string) (*ciMetricsCursor, error) {
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
	var count int
	err = db.QueryRow(`
		SELECT COUNT(1) FROM ci_pr_panels cp
		WHERE cp.id = ? AND cp.posted_at IS NOT NULL
		  AND `+sqliteNormalizedTimestampExpr("cp.posted_at")+` = datetime(?)`,
		decoded.PanelID, decoded.PostedAt).Scan(&count)
	if err != nil {
		return nil, fmt.Errorf("validate ci metrics cursor: %w", err)
	}
	if count == 0 {
		return nil, fmt.Errorf("%w: posted_at %q panel_id %d",
			ErrExportCursorNotFound, decoded.PostedAt, decoded.PanelID)
	}
	return &decoded, nil
}
