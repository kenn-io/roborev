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
	// Legacy switches the export to the pre-panel CI era (~2026-02 to
	// ~2026-06): completed review/range jobs with no panel run, grouped
	// per (repo, git_ref) into one wall-clock unit ("pseudopanel") when
	// two or more reviews share the ref. Rows from that era predate all
	// CI tagging, so membership is structural, and singleton reviews are
	// excluded as manual one-offs.
	// Legacy turnaround (first_attempt_at -> posted_at) measures earliest
	// job enqueue -> latest job finish and excludes comment-posting
	// latency, so it slightly undercounts the panel-era PR-perceived
	// turnaround (first poller attempt -> panel posted). Legacy cursors
	// are namespaced and are rejected if replayed with Legacy unset, and
	// vice versa.
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

// legacyUnitTimeExpr normalizes a review_jobs timestamp column (SQLite
// CURRENT_TIMESTAMP space format or RFC 3339) to sortable RFC 3339 UTC so
// MIN/MAX aggregate correctly across mixed formats.
func legacyUnitTimeExpr(col string) string {
	return "strftime('%Y-%m-%dT%H:%M:%SZ', " + col + ")"
}

// legacyGithubRepo maps a repos.name value (CI clones store the remote URL)
// to the owner/repo form used by panel-era github_repo, passing through
// values that are not GitHub remotes.
func legacyGithubRepo(name string) string {
	for _, prefix := range []string{
		"https://github.com/", "http://github.com/",
		"git@github.com:", "ssh://git@github.com/",
	} {
		if rest, ok := strings.CutPrefix(name, prefix); ok {
			return strings.TrimSuffix(rest, ".git")
		}
	}
	return name
}

// legacyHeadSHA extracts the head of a git_ref: the part after ".." (or
// "...") for range refs, the ref itself otherwise.
func legacyHeadSHA(gitRef string) string {
	if i := strings.LastIndex(gitRef, ".."); i >= 0 {
		return strings.TrimPrefix(gitRef[i+2:], ".")
	}
	return gitRef
}

// exportCIMetricsLegacy is the review_jobs-backed page builder used by
// ExportCIMetrics when Legacy is set. The pre-panel CI era enqueued
// independent reviews of the same range — typically two agents x two review
// types per PR head ("pseudopanels") — before any CI tagging existed
// (source='ci' and ci_base_branch both postdate those rows), and the tables
// that linked them to PRs (ci_pr_batches, then ci_pr_reviews) have no
// surviving production rows. A pseudopanel is therefore identified
// structurally: two or more completed review/range jobs sharing
// (repo, git_ref) with no panel run. Each such group collapses into one
// wall-clock unit: panel_created_at/first_attempt_at is the group's
// earliest enqueue, posted_at its latest finish, outcome is always
// PanelOutcomeLegacyReview, pr_number is 0 (the PR linkage is
// unrecoverable), and head_sha is the range head. Jobs lists the group's
// completed jobs tagged role "review"; synthesis_agent/synthesis_model stay
// nil (pseudopanels had no synthesis). Singleton reviews are excluded: a
// lone job on a ref is a manual one-off, not a pseudopanel.
func (db *DB) exportCIMetricsLegacy(opts ExportCIMetricsOptions, cursor *ciMetricsCursor) (ExportCIMetricsPage, error) {
	postedExpr := "MAX(" + legacyUnitTimeExpr("j.finished_at") + ")"
	having := []string{}
	args := make([]any, 0)
	if !opts.Since.IsZero() {
		having = append(having, postedExpr+" >= ?")
		args = append(args, opts.Since.UTC().Format(time.RFC3339))
	}
	if !opts.Until.IsZero() {
		having = append(having, postedExpr+" < ?")
		args = append(args, opts.Until.UTC().Format(time.RFC3339))
	}
	if cursor != nil {
		having = append(having,
			"("+postedExpr+" > ? OR ("+postedExpr+" = ? AND MIN(j.id) > ?))")
		args = append(args, cursor.PostedAt, cursor.PostedAt, cursor.PanelID)
	}
	having = append(having, "COUNT(*) >= 2")
	args = append(args, opts.Limit+1)

	query := `
		WITH ` + legacyUnitWindowCTE + `
		SELECT MIN(j.id), j.repo_id, r.name, j.git_ref,
		       MIN(` + legacyUnitTimeExpr("j.enqueued_at") + `),
		       ` + postedExpr + `
		FROM review_jobs j
		JOIN repos r ON r.id = j.repo_id
		JOIN unit_windows w ON w.repo_id = j.repo_id AND w.git_ref = j.git_ref
		WHERE ` + legacyUnitJobConditions + `
		  AND ` + legacyUnitTimeExpr("j.enqueued_at") + ` <= w.window_end
		GROUP BY j.repo_id, j.git_ref
		HAVING ` + strings.Join(having, " AND ") + `
		ORDER BY 6 ASC, 1 ASC
		LIMIT ?`
	rows, err := db.Query(query, args...)
	if err != nil {
		return ExportCIMetricsPage{}, fmt.Errorf("query legacy ci metrics export: %w", err)
	}
	defer rows.Close()

	type legacyUnit struct {
		unitID int64
		repoID int64
		panel  ExportCIPanel
	}
	units := []legacyUnit{}
	truncated := false
	for rows.Next() {
		var (
			u        legacyUnit
			repoName string
			gitRef   string
			first    sql.NullString
			posted   sql.NullString
		)
		if err := rows.Scan(&u.unitID, &u.repoID, &repoName, &gitRef, &first, &posted); err != nil {
			return ExportCIMetricsPage{}, fmt.Errorf("scan legacy ci metrics unit: %w", err)
		}
		if len(units) == opts.Limit {
			truncated = true
			break
		}
		u.panel.GithubRepo = legacyGithubRepo(repoName)
		u.panel.HeadSHA = legacyHeadSHA(gitRef)
		u.panel.Outcome = PanelOutcomeLegacyReview
		u.panel.PostedAt = posted.String
		if first.Valid {
			v := first.String
			u.panel.FirstAttemptAt = &v
			u.panel.PanelCreatedAt = v
		}
		u.panel.Jobs, err = db.legacyUnitJobs(u.repoID, gitRef)
		if err != nil {
			return ExportCIMetricsPage{}, err
		}
		units = append(units, u)
	}
	if err := rows.Err(); err != nil {
		return ExportCIMetricsPage{}, err
	}

	page := ExportCIMetricsPage{Panels: make([]ExportCIPanel, 0, len(units)), Truncated: truncated}
	for _, u := range units {
		page.Panels = append(page.Panels, u.panel)
	}
	if len(units) > 0 {
		databaseID, err := db.GetDatabaseID()
		if err != nil {
			return ExportCIMetricsPage{}, err
		}
		last := units[len(units)-1]
		next := encodeCIMetricsCursor(databaseID, last.panel.PostedAt, last.unitID, true)
		if next != "" {
			page.NextCursor = &next
		}
	}
	return page, nil
}

// legacyUnitJobConditions is the shared WHERE fragment selecting jobs that
// can belong to a legacy pseudopanel unit: completed review/range jobs with
// no panel run. Pre-panel rows carry no CI tagging, so membership is
// structural (the unit query additionally requires groups of two or more).
const legacyUnitJobConditions = `(j.panel_run_uuid IS NULL OR j.panel_run_uuid = '')
		  AND j.job_type IN ('review', 'range')
		  AND j.status = 'done' AND j.finished_at IS NOT NULL`

// legacyUnitWindowCTE bounds a unit to ADJACENT jobs: only jobs enqueued
// within an hour of the ref's first enqueue belong to the pseudopanel, so a
// manual re-review of the same ref days later cannot stretch the unit's
// wall clock (observed in production: one ref re-reviewed 12 days later
// inflated its turnaround to 290 hours). Pseudopanel members enqueue within
// seconds of each other, so the hour window is generous for real units.
const legacyUnitWindowCTE = `unit_windows AS MATERIALIZED (
			SELECT j.repo_id, j.git_ref,
			       strftime('%Y-%m-%dT%H:%M:%SZ',
			                datetime(MIN(` + legacyUnitTimeExprConst + `), '+1 hour')) AS window_end
			FROM review_jobs j
			WHERE ` + legacyUnitJobConditions + `
			GROUP BY j.repo_id, j.git_ref
		)`

// legacyUnitTimeExprConst mirrors legacyUnitTimeExpr("j.enqueued_at") for
// use inside const SQL fragments.
const legacyUnitTimeExprConst = `strftime('%Y-%m-%dT%H:%M:%SZ', j.enqueued_at)`

// legacyUnitWindowEndSubquery computes one (repo, git_ref) unit's adjacency
// window end (first enqueue + 1 hour) as a scalar subquery over two bound
// parameters. Per-unit callers use this instead of legacyUnitWindowCTE: the
// CTE aggregates every unit in the table, which is fine once per page but
// pathological when repeated for each of the page's units.
const legacyUnitWindowEndSubquery = `(
		SELECT strftime('%Y-%m-%dT%H:%M:%SZ',
		                datetime(MIN(strftime('%Y-%m-%dT%H:%M:%SZ', jj.enqueued_at)), '+1 hour'))
		FROM review_jobs jj
		WHERE jj.repo_id = ? AND jj.git_ref = ?
		  AND (jj.panel_run_uuid IS NULL OR jj.panel_run_uuid = '')
		  AND jj.job_type IN ('review', 'range')
		  AND jj.status = 'done' AND jj.finished_at IS NOT NULL)`

// legacyUnitJobs returns the completed jobs of one legacy (repo, git_ref)
// unit in id order — bounded to the adjacency window like the unit query —
// shaped as ExportCIPanelJob rows tagged role "review".
func (db *DB) legacyUnitJobs(repoID int64, gitRef string) ([]ExportCIPanelJob, error) {
	rows, err := db.Query(`
		SELECT j.uuid, j.agent, j.model, j.provider, j.status,
		       `+legacyUnitTimeExpr("j.started_at")+`,
		       `+legacyUnitTimeExpr("j.finished_at")+`
		FROM review_jobs j
		WHERE j.repo_id = ? AND j.git_ref = ?
		  AND `+legacyUnitJobConditions+`
		  AND `+legacyUnitTimeExpr("j.enqueued_at")+` <= `+legacyUnitWindowEndSubquery+`
		ORDER BY j.id ASC`,
		repoID, gitRef, repoID, gitRef)
	if err != nil {
		return nil, fmt.Errorf("query legacy ci unit jobs: %w", err)
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
		if err := rows.Scan(&job.JobUUID, &job.Agent, &model, &provider, &job.Status,
			&startedAt, &finishedAt); err != nil {
			return nil, fmt.Errorf("scan legacy ci unit job: %w", err)
		}
		job.Role = "review"
		if model.Valid && model.String != "" {
			job.Model = &model.String
		}
		if provider.Valid && provider.String != "" {
			job.Provider = &provider.String
		}
		if startedAt.Valid {
			job.StartedAt = &startedAt.String
		}
		if finishedAt.Valid {
			job.FinishedAt = &finishedAt.String
		}
		jobs = append(jobs, job)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return jobs, nil
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
		// A legacy cursor names a pseudopanel unit by its MIN(j.id) anchor
		// job and the unit's latest finish; re-derive that unit's group and
		// check both still hold.
		existsQuery = `
			SELECT COUNT(1) FROM review_jobs a
			WHERE a.id = ?
			  AND (a.panel_run_uuid IS NULL OR a.panel_run_uuid = '')
			  AND a.job_type IN ('review', 'range')
			  AND a.status = 'done' AND a.finished_at IS NOT NULL
			  AND (SELECT MAX(` + legacyUnitTimeExpr("j.finished_at") + `)
			       FROM review_jobs j
			       WHERE j.repo_id = a.repo_id AND j.git_ref = a.git_ref
			         AND ` + legacyUnitJobConditions + `
			         AND ` + legacyUnitTimeExpr("j.enqueued_at") + ` <= (
			           SELECT strftime('%Y-%m-%dT%H:%M:%SZ',
			                    datetime(MIN(strftime('%Y-%m-%dT%H:%M:%SZ', jj.enqueued_at)), '+1 hour'))
			           FROM review_jobs jj
			           WHERE jj.repo_id = a.repo_id AND jj.git_ref = a.git_ref
			             AND (jj.panel_run_uuid IS NULL OR jj.panel_run_uuid = '')
			             AND jj.job_type IN ('review', 'range')
			             AND jj.status = 'done' AND jj.finished_at IS NOT NULL)) = ?`
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
