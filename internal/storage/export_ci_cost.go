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
	ciCostCursorVersion    = 1
	ciCostDefaultPageLimit = 500
	ciCostMaxPageLimit     = 5000
)

// ExportCICostOptions bounds one page of the job-level CI cost export.
type ExportCICostOptions struct {
	Since  time.Time
	Until  time.Time
	Cursor string
	Limit  int
	Legacy bool
}

// ExportCICostPage is one bounded page of cost-eligible CI jobs.
type ExportCICostPage struct {
	Jobs           []ExportCICostJob `json:"jobs"`
	Truncated      bool              `json:"truncated"`
	NextCursor     *string           `json:"next_cursor"`
	EffectiveSince time.Time         `json:"-"`
	EffectiveUntil time.Time         `json:"-"`
}

// ExportCICostJob is one CI job where an agent ran. CostUSD is nil when the
// job has no usable recorded price; a pointer to zero is a priced free run.
type ExportCICostJob struct {
	JobUUID    string   `json:"job_uuid"`
	FinishedAt string   `json:"finished_at"`
	Agent      string   `json:"agent"`
	Model      *string  `json:"model"`
	Provider   *string  `json:"provider"`
	Role       string   `json:"role"`
	Status     string   `json:"status"`
	CostUSD    *float64 `json:"cost_usd"`
}

type ciCostCursor struct {
	Version    int        `json:"version"`
	DatabaseID string     `json:"database_id"`
	FinishedAt string     `json:"finished_at"`
	JobID      int64      `json:"job_id"`
	Legacy     bool       `json:"legacy,omitempty"`
	Since      *time.Time `json:"since,omitempty"`
	Until      *time.Time `json:"until,omitempty"`
}

// ErrExportCICostCursorModeMismatch is returned when a cursor minted for one
// CI cost mode is replayed against the other.
var ErrExportCICostCursorModeMismatch = errors.New("ci cost cursor mode mismatch")

// ExportCICosts returns one bounded page of cost-eligible CI jobs ordered by
// (finished_at, id). A fresh export rescans the requested window, so callers
// can safely pick up late pricing through idempotent ingestion. Legacy mode
// reconstructs the frozen pre-panel CI units.
func (db *DB) ExportCICosts(opts ExportCICostOptions) (ExportCICostPage, error) {
	switch {
	case opts.Limit <= 0:
		opts.Limit = ciCostDefaultPageLimit
	case opts.Limit > ciCostMaxPageLimit:
		opts.Limit = ciCostMaxPageLimit
	}

	cursor, err := db.resolveCICostCursor(opts.Cursor, opts.Legacy)
	if err != nil {
		return ExportCICostPage{}, err
	}
	if cursor != nil && cursor.Since != nil {
		opts.Since = cursor.Since.UTC()
	}
	if cursor != nil && cursor.Until != nil {
		opts.Until = cursor.Until.UTC()
	}
	var page ExportCICostPage
	if opts.Legacy {
		page, err = db.exportLegacyCICosts(opts, cursor)
	} else {
		page, err = db.exportRegularCICosts(opts, cursor)
	}
	if err != nil {
		return ExportCICostPage{}, err
	}
	page.EffectiveSince = opts.Since
	page.EffectiveUntil = opts.Until
	return page, nil
}

func (db *DB) exportRegularCICosts(opts ExportCICostOptions, cursor *ciCostCursor) (ExportCICostPage, error) {
	finishedExpr := sqliteNormalizedTimestampExpr("j.finished_at")
	conditions := []string{regularCICostOwnershipCondition, costEligible}
	args := []any{JobSourceCI}
	conditions, args = appendCICostBounds(conditions, args, finishedExpr, opts, cursor)
	args = append(args, opts.Limit+1)

	query := `
		SELECT j.id, j.finished_at, j.uuid, j.agent, j.model, j.provider,
		       CASE WHEN j.panel_role = '` + PanelRoleSynthesis + `'
		            THEN '` + PanelRoleSynthesis + `' ELSE '` + PanelRoleMember + `' END,
		       j.status, j.token_usage
		FROM review_jobs j
		WHERE ` + strings.Join(conditions, " AND ") + `
		ORDER BY ` + finishedExpr + ` ASC, j.id ASC
		LIMIT ?`
	return db.queryCICostPage(query, args, opts.Limit, false, opts.Since, opts.Until)
}

const regularCICostOwnershipCondition = `(j.source = ? OR EXISTS (
	SELECT 1 FROM ci_pr_panels p WHERE p.panel_run_uuid = j.panel_run_uuid
))`

// legacyCostEligible supplements costEligible's modern invocation evidence.
// Pre-panel rows predate agent_invoked, but a done review/range job proves the
// review agent completed successfully. Failed and canceled rows still require
// the marker or usage evidence because they may have stopped before invocation.
// Callers also apply legacyUnitJobConditions, which limits this inference to
// structurally identified pre-panel review/range jobs.
const legacyCostEligible = "j.started_at IS NOT NULL AND j.finished_at IS NOT NULL " +
	"AND (j.status = 'done' OR j.agent_invoked = 1 OR (" + agentRanByUsage + "))"

func appendCICostBounds(
	conditions []string, args []any, finishedExpr string,
	opts ExportCICostOptions, cursor *ciCostCursor,
) ([]string, []any) {
	if !opts.Since.IsZero() {
		conditions = append(conditions, finishedExpr+" >= datetime(?)")
		args = append(args, opts.Since.UTC().Format(time.RFC3339))
	}
	if !opts.Until.IsZero() {
		conditions = append(conditions, finishedExpr+" < datetime(?)")
		args = append(args, opts.Until.UTC().Format(time.RFC3339))
	}
	if cursor != nil {
		conditions = append(conditions,
			"("+finishedExpr+" > datetime(?) OR ("+finishedExpr+" = datetime(?) AND j.id > ?))")
		args = append(args, cursor.FinishedAt, cursor.FinishedAt, cursor.JobID)
	}
	return conditions, args
}

func (db *DB) exportLegacyCICosts(opts ExportCICostOptions, cursor *ciCostCursor) (ExportCICostPage, error) {
	eraEnd, err := db.legacyPanelEraEnd()
	if err != nil {
		return ExportCICostPage{}, err
	}
	if eraEnd == "" {
		return ExportCICostPage{Jobs: []ExportCICostJob{}}, nil
	}

	finishedExpr := sqliteNormalizedTimestampExpr("j.finished_at")
	conditions := []string{
		legacyUnitJobConditions,
		legacyUnitTimeExpr("j.enqueued_at") + " <= w.window_end",
		legacyCostEligible,
	}
	args := []any{eraEnd, eraEnd, eraEnd}
	conditions, args = appendCICostBounds(conditions, args, finishedExpr, opts, cursor)
	args = append(args, opts.Limit+1)

	query := `
		WITH ` + legacyCICostValidUnitsCTE() + `
		SELECT j.id, j.finished_at, j.uuid, j.agent, j.model, j.provider,
		       'review', j.status, j.token_usage
		FROM review_jobs j
		JOIN valid_units v ON v.repo_id = j.repo_id AND v.git_ref = j.git_ref
		JOIN unit_windows w ON w.repo_id = j.repo_id AND w.git_ref = j.git_ref
		WHERE ` + strings.Join(conditions, " AND ") + `
		ORDER BY ` + finishedExpr + ` ASC, j.id ASC
		LIMIT ?`
	return db.queryCICostPage(query, args, opts.Limit, true, opts.Since, opts.Until)
}

// legacyCICostValidUnitsCTE reuses the same membership and adjacency rules as
// the CI metrics legacy export. Its first two parameters are the panel-era end:
// one for unit_windows and one for the grouped valid_units population.
func legacyCICostValidUnitsCTE() string {
	return legacyUnitWindowCTE + `,
		valid_units AS MATERIALIZED (
			SELECT j.repo_id, j.git_ref
			FROM review_jobs j
			JOIN unit_windows w ON w.repo_id = j.repo_id AND w.git_ref = j.git_ref
			WHERE ` + legacyUnitJobConditions + `
			  AND ` + legacyUnitTimeExpr("j.enqueued_at") + ` <= w.window_end
			GROUP BY j.repo_id, j.git_ref
			HAVING COUNT(*) >= 2
			   AND SUM(CASE WHEN j.status = '` + string(JobStatusDone) + `' THEN 1 ELSE 0 END) >= 1
		)`
}

func (db *DB) queryCICostPage(
	query string, args []any, limit int, legacy bool, since, until time.Time,
) (ExportCICostPage, error) {
	rows, err := db.Query(query, args...)
	if err != nil {
		return ExportCICostPage{}, fmt.Errorf("query ci cost export: %w", err)
	}
	defer rows.Close()

	page := ExportCICostPage{Jobs: []ExportCICostJob{}}
	var lastID int64
	var lastFinishedAt string
	for rows.Next() {
		id, finishedAt, job, err := scanCICostRow(rows)
		if err != nil {
			return ExportCICostPage{}, err
		}
		if len(page.Jobs) == limit {
			page.Truncated = true
			break
		}
		page.Jobs = append(page.Jobs, job)
		lastID = id
		lastFinishedAt = finishedAt
	}
	if err := rows.Err(); err != nil {
		return ExportCICostPage{}, err
	}

	if len(page.Jobs) > 0 {
		databaseID, err := db.GetDatabaseID()
		if err != nil {
			return ExportCICostPage{}, err
		}
		next := encodeCICostCursor(databaseID, lastFinishedAt, lastID, legacy, since, until)
		if next != "" {
			page.NextCursor = &next
		}
	}
	return page, nil
}

func scanCICostRow(rows *sql.Rows) (int64, string, ExportCICostJob, error) {
	var (
		id         int64
		job        ExportCICostJob
		finishedAt sql.NullString
		model      sql.NullString
		provider   sql.NullString
		tokenUsage sql.NullString
	)
	if err := rows.Scan(&id, &finishedAt, &job.JobUUID, &job.Agent, &model,
		&provider, &job.Role, &job.Status, &tokenUsage); err != nil {
		return 0, "", ExportCICostJob{}, fmt.Errorf("scan ci cost row: %w", err)
	}
	if finishedAt.Valid {
		job.FinishedAt = formatExportTime(parseSQLiteTime(finishedAt.String))
	}
	if model.Valid && model.String != "" {
		job.Model = &model.String
	}
	if provider.Valid && provider.String != "" {
		job.Provider = &provider.String
	}
	job.CostUSD = parseExportCost(tokenUsage).USD
	return id, job.FinishedAt, job, nil
}

func encodeCICostCursor(
	databaseID, finishedAt string, jobID int64, legacy bool, since, until time.Time,
) string {
	if databaseID == "" || finishedAt == "" || jobID <= 0 {
		return ""
	}
	var normalizedSince *time.Time
	if !since.IsZero() {
		value := since.UTC()
		normalizedSince = &value
	}
	var normalizedUntil *time.Time
	if !until.IsZero() {
		value := until.UTC()
		normalizedUntil = &value
	}
	data, err := json.Marshal(ciCostCursor{
		Version: ciCostCursorVersion, DatabaseID: databaseID,
		FinishedAt: finishedAt, JobID: jobID, Legacy: legacy,
		Since: normalizedSince, Until: normalizedUntil,
	})
	if err != nil {
		log.Printf("storage: warning: encode ci cost cursor: %v", err)
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(data)
}

func (db *DB) resolveCICostCursor(cursor string, legacy bool) (*ciCostCursor, error) {
	if cursor == "" {
		return nil, nil
	}
	data, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return nil, fmt.Errorf("invalid ci cost cursor: %w", err)
	}
	var decoded ciCostCursor
	if err := json.Unmarshal(data, &decoded); err != nil {
		return nil, fmt.Errorf("invalid ci cost cursor: %w", err)
	}
	if decoded.Version != ciCostCursorVersion {
		return nil, fmt.Errorf("invalid ci cost cursor: unsupported version %d", decoded.Version)
	}
	if decoded.DatabaseID == "" || decoded.FinishedAt == "" || decoded.JobID <= 0 {
		return nil, errors.New("invalid ci cost cursor: missing fields")
	}
	if decoded.Legacy != legacy {
		return nil, fmt.Errorf("%w: cursor legacy=%v does not match requested legacy=%v",
			ErrExportCICostCursorModeMismatch, decoded.Legacy, legacy)
	}
	finishedAt, err := time.Parse(time.RFC3339Nano, decoded.FinishedAt)
	if err != nil {
		return nil, fmt.Errorf("invalid ci cost cursor timestamp: %w", err)
	}
	decoded.FinishedAt = finishedAt.UTC().Format(time.RFC3339)
	if decoded.Since != nil {
		value := decoded.Since.UTC()
		decoded.Since = &value
	}
	if decoded.Until != nil {
		value := decoded.Until.UTC()
		decoded.Until = &value
	}
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

	exists, err := db.ciCostCursorRowExists(decoded, legacy)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, fmt.Errorf("%w: finished_at %q job_id %d",
			ErrExportCursorNotFound, decoded.FinishedAt, decoded.JobID)
	}
	return &decoded, nil
}

func (db *DB) ciCostCursorRowExists(cursor ciCostCursor, legacy bool) (bool, error) {
	query := `SELECT COUNT(1) FROM review_jobs j
		WHERE j.id = ? AND ` + regularCICostOwnershipCondition + ` AND ` + costEligible + `
		  AND ` + sqliteNormalizedTimestampExpr("j.finished_at") + ` = datetime(?)`
	args := []any{cursor.JobID, JobSourceCI, cursor.FinishedAt}
	if legacy {
		eraEnd, err := db.legacyPanelEraEnd()
		if err != nil {
			return false, err
		}
		if eraEnd == "" {
			return false, nil
		}
		query = `WITH ` + legacyCICostValidUnitsCTE() + `
			SELECT COUNT(1)
			FROM review_jobs j
			JOIN valid_units v ON v.repo_id = j.repo_id AND v.git_ref = j.git_ref
			JOIN unit_windows w ON w.repo_id = j.repo_id AND w.git_ref = j.git_ref
			WHERE ` + legacyUnitJobConditions + `
			  AND ` + legacyUnitTimeExpr("j.enqueued_at") + ` <= w.window_end
			  AND ` + legacyCostEligible + `
			  AND j.id = ?
			  AND ` + sqliteNormalizedTimestampExpr("j.finished_at") + ` = datetime(?)`
		args = []any{eraEnd, eraEnd, eraEnd, cursor.JobID, cursor.FinishedAt}
	}
	var count int
	if err := db.QueryRow(query, args...).Scan(&count); err != nil {
		return false, fmt.Errorf("validate ci cost cursor: %w", err)
	}
	return count > 0, nil
}
