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
	"uuid"
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
	// Legacy switches the export to the frozen pre-panel CI era: terminal
	// (done, failed, or canceled) review/range jobs with no panel run,
	// enqueued before the database's first panel activity, grouped per
	// (repo, git_ref) into one wall-clock unit ("pseudopanel") when two or
	// more terminal jobs share the ref and at least one succeeded (the
	// batch flow posted whatever output was available once every member
	// reached a terminal state). Singleton reviews are excluded as manual
	// one-offs; a database with no panel activity exports no legacy rows.
	// Legacy turnaround (earliest enqueue -> latest finish) excludes
	// comment-posting latency, unlike panel-era turnaround. Legacy cursors
	// are namespaced and rejected if replayed with Legacy unset, and vice
	// versa.
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
	GithubRepo     string                 `json:"github_repo"`
	PRNumber       int64                  `json:"pr_number"`
	HeadSHA        string                 `json:"head_sha"`
	PanelCreatedAt string                 `json:"panel_created_at"`
	PostedAt       string                 `json:"posted_at"`
	FirstAttemptAt *string                `json:"first_attempt_at"`
	AttemptCount   *int64                 `json:"attempt_count"`
	Outcome        string                 `json:"outcome"`
	SynthesisAgent *string                `json:"synthesis_agent"`
	SynthesisModel *string                `json:"synthesis_model"`
	Jobs           []ExportCIPanelJob     `json:"jobs"`
	Experiments    []ExperimentAssignment `json:"experiments"`
}

// ExportCIPanelJob is one member or synthesis job of an exported panel.
type ExportCIPanelJob struct {
	JobUUID             uuid.UUID  `json:"job_uuid" format:"uuid"`
	Role                string     `json:"role"`
	Agent               string     `json:"agent"`
	Model               *string    `json:"model"`
	Provider            *string    `json:"provider"`
	Status              string     `json:"status"`
	StartedAt           *string    `json:"started_at"`
	FinishedAt          *string    `json:"finished_at"`
	ResumeSourceJobUUID *uuid.UUID `json:"resume_source_job_uuid" format:"uuid" nullable:"true"`
}

type ciMetricsCursor struct {
	Version    int       `json:"version"`
	DatabaseID uuid.UUID `json:"database_id"`
	PostedAt   string    `json:"posted_at"`
	PanelID    int64     `json:"panel_id"`
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
		runUUID uuid.UUID
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
		item.panel.Experiments, err = db.GetExperimentAssignments(ReviewUnitPanel, item.runUUID)
		if err != nil {
			return ExportCIMetricsPage{}, err
		}
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

// legacyGithubRepo maps a repo's remote identity (repos.identity, a GitHub
// remote URL for CI clones; the export falls back to repos.name only when
// identity is unset) to the owner/repo form used by panel-era github_repo,
// passing through values that are not GitHub remotes.
func legacyGithubRepo(identity string) string {
	for _, prefix := range []string{
		"https://github.com/", "http://github.com/",
		"git@github.com:", "ssh://git@github.com/",
	} {
		if rest, ok := strings.CutPrefix(identity, prefix); ok {
			return strings.TrimSuffix(rest, ".git")
		}
	}
	return identity
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
// independent reviews of the same range before any CI tagging existed, and
// the tables that linked them to PRs have no surviving production rows, so
// a pseudopanel is identified structurally per the Legacy option doc. Each
// group collapses into one wall-clock unit: panel_created_at/
// first_attempt_at is the group's earliest enqueue, posted_at its latest
// finish (the batch posted only after every member was terminal, so a
// failed or canceled sibling's finish counts), outcome is PanelOutcomeLegacyReview,
// pr_number is 0 (the PR linkage is unrecoverable), and head_sha is the
// range head. Jobs lists the group's terminal jobs tagged role "review";
// synthesis fields stay nil.
func (db *DB) exportCIMetricsLegacy(opts ExportCIMetricsOptions, cursor *ciMetricsCursor) (ExportCIMetricsPage, error) {
	eraEnd, err := db.legacyPanelEraEnd()
	if err != nil {
		return ExportCIMetricsPage{}, err
	}
	if eraEnd == "" {
		// No panel activity ever: there is no bounded pre-panel era to
		// export from.
		return ExportCIMetricsPage{Panels: []ExportCIPanel{}}, nil
	}

	postedExpr := "MAX(" + legacyUnitTimeExpr("j.finished_at") + ")"
	having := []string{}
	args := []any{eraEnd, eraEnd}
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
	having = append(having, "COUNT(*) >= 2",
		"SUM(CASE WHEN j.status = 'done' THEN 1 ELSE 0 END) >= 1")
	args = append(args, opts.Limit+1)

	query := `
		WITH ` + legacyUnitWindowCTE + `
		SELECT MIN(j.id), j.repo_id,
		       COALESCE(NULLIF(TRIM(r.identity), ''), r.name), j.git_ref,
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
			u            legacyUnit
			repoIdentity string
			gitRef       string
			first        sql.NullString
			posted       sql.NullString
		)
		if err := rows.Scan(&u.unitID, &u.repoID, &repoIdentity, &gitRef, &first, &posted); err != nil {
			return ExportCIMetricsPage{}, fmt.Errorf("scan legacy ci metrics unit: %w", err)
		}
		if len(units) == opts.Limit {
			truncated = true
			break
		}
		u.panel.GithubRepo = legacyGithubRepo(repoIdentity)
		u.panel.HeadSHA = legacyHeadSHA(gitRef)
		u.panel.Outcome = PanelOutcomeLegacyReview
		u.panel.PostedAt = posted.String
		if first.Valid {
			v := first.String
			u.panel.FirstAttemptAt = &v
			u.panel.PanelCreatedAt = v
		}
		u.panel.Jobs, err = db.legacyUnitJobs(u.repoID, gitRef, eraEnd)
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

// legacyPanelEraEnd returns the timestamp of the database's first panel
// activity (earliest panel-tagged job enqueue or ci_pr_panels row) in
// normalized RFC 3339 UTC, or "" when there is none. It is the upper bound
// of the pre-panel era: only jobs enqueued strictly before it can belong to
// a legacy pseudopanel, so post-panel reviews do not form new legacy units.
// Recomputed per export call; the --legacy export is a one-time backfill, so
// this runs against complete data and is never a live, drifting feed.
func (db *DB) legacyPanelEraEnd() (string, error) {
	var end sql.NullString
	err := db.QueryRow(`
		SELECT MIN(t) FROM (
			SELECT MIN(strftime('%Y-%m-%dT%H:%M:%SZ', j.enqueued_at)) AS t
			FROM review_jobs j
			WHERE j.panel_run_uuid IS NOT NULL AND j.panel_run_uuid != ''
			UNION ALL
			SELECT MIN(strftime('%Y-%m-%dT%H:%M:%SZ', cp.created_at))
			FROM ci_pr_panels cp
		) WHERE t IS NOT NULL`).Scan(&end)
	if err != nil {
		return "", fmt.Errorf("query legacy panel era end: %w", err)
	}
	return end.String, nil
}

// legacyUnitJobConditionsFor builds the shared WHERE fragment selecting jobs
// that can belong to a legacy pseudopanel unit under the given table alias:
// terminal (done, failed, or canceled) review/range jobs with no panel run,
// enqueued before the pre-panel era end (one bound parameter, see
// legacyPanelEraEnd). Failed and canceled jobs count as members because the
// batch flow treated both as terminal and then posted whatever output was
// available; the unit query separately requires at least one done job per
// group. The finished_at guard is what keeps the panel migration's own
// cleanup out: it canceled leftover in-flight batch jobs without stamping
// finished_at, and those batches never posted. Pre-panel rows carry no CI
// tagging, so membership is structural (the unit query additionally requires
// groups of two or more).
func legacyUnitJobConditionsFor(alias string) string {
	return `(` + alias + `.panel_run_uuid IS NULL OR ` + alias + `.panel_run_uuid = '')
		  AND ` + alias + `.job_type IN ('review', 'range')
		  AND ` + alias + `.status IN ('done', 'failed', 'canceled')
		  AND ` + alias + `.finished_at IS NOT NULL
		  AND ` + legacyUnitTimeExpr(alias+".enqueued_at") + ` < ?`
}

// legacyUnitJobConditions is legacyUnitJobConditionsFor for the default
// alias "j". Binds one parameter: the pre-panel era end.
var legacyUnitJobConditions = legacyUnitJobConditionsFor("j")

// legacyUnitWindowCTE bounds a unit to ADJACENT jobs: only jobs enqueued
// within an hour of the ref's first enqueue belong to the pseudopanel, so a
// manual re-review of the same ref days later cannot stretch the unit's
// wall clock (observed in production: one ref re-reviewed 12 days later
// inflated its turnaround to 290 hours). Pseudopanel members enqueue within
// seconds of each other, so the hour window is generous for real units.
// Binds one parameter: the pre-panel era end.
var legacyUnitWindowCTE = `unit_windows AS MATERIALIZED (
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
// window end (first enqueue + 1 hour) as a scalar subquery over three bound
// parameters (repo_id, git_ref, era end). Per-unit callers use this instead
// of legacyUnitWindowCTE: the CTE aggregates every unit in the table, which
// is fine once per page but pathological when repeated for each of the
// page's units.
var legacyUnitWindowEndSubquery = `(
		SELECT strftime('%Y-%m-%dT%H:%M:%SZ',
		                datetime(MIN(strftime('%Y-%m-%dT%H:%M:%SZ', jj.enqueued_at)), '+1 hour'))
		FROM review_jobs jj
		WHERE jj.repo_id = ? AND jj.git_ref = ?
		  AND ` + legacyUnitJobConditionsFor("jj") + `)`

// legacyUnitJobs returns the terminal jobs of one legacy (repo, git_ref)
// unit in id order — bounded to the adjacency window and pre-panel era like
// the unit query — shaped as ExportCIPanelJob rows tagged role "review".
func (db *DB) legacyUnitJobs(repoID int64, gitRef, eraEnd string) ([]ExportCIPanelJob, error) {
	rows, err := db.Query(`
		SELECT j.uuid, j.agent, j.model, j.provider, j.status,
		       `+legacyUnitTimeExpr("j.started_at")+`,
		       `+legacyUnitTimeExpr("j.finished_at")+`
		FROM review_jobs j
		WHERE j.repo_id = ? AND j.git_ref = ?
		  AND `+legacyUnitJobConditions+`
		  AND `+legacyUnitTimeExpr("j.enqueued_at")+` <= `+legacyUnitWindowEndSubquery+`
		ORDER BY j.id ASC`,
		repoID, gitRef, eraEnd, repoID, gitRef, eraEnd)
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
func scanCIMetricsRow(rows *sql.Rows) (int64, ExportCIPanel, uuid.UUID, error) {
	var (
		id             int64
		panel          ExportCIPanel
		createdAt      sql.NullString
		postedAt       sql.NullString
		firstAttemptAt sql.NullString
		attemptCount   sql.NullInt64
		runUUID        uuid.UUID
		synthAgent     sql.NullString
		synthModel     sql.NullString
	)
	if err := rows.Scan(&id, &panel.GithubRepo, &panel.PRNumber, &panel.HeadSHA,
		&createdAt, &postedAt, &firstAttemptAt, &attemptCount, &panel.Outcome,
		&runUUID, &synthAgent, &synthModel); err != nil {
		return 0, ExportCIPanel{}, uuid.Nil(), fmt.Errorf("scan ci metrics row: %w", err)
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

func (db *DB) exportCIPanelJobs(panelRunUUID uuid.UUID) ([]ExportCIPanelJob, error) {
	rows, err := db.Query(`
		SELECT j.uuid, COALESCE(j.panel_role, ''), j.agent, j.model,
		       j.provider, j.status, j.started_at, j.finished_at, j.resume_source_job_uuid
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
			job          ExportCIPanelJob
			model        sql.NullString
			provider     sql.NullString
			startedAt    sql.NullString
			finishedAt   sql.NullString
			resumeSource sql.Null[uuid.UUID]
		)
		if err := rows.Scan(&job.JobUUID, &job.Role, &job.Agent, &model,
			&provider, &job.Status, &startedAt, &finishedAt, &resumeSource); err != nil {
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
		if resumeSource.Valid {
			job.ResumeSourceJobUUID = &resumeSource.V
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}

func encodeCIMetricsCursor(databaseID uuid.UUID, postedAt string, panelID int64, legacy bool) string {
	if databaseID == uuid.Nil() || postedAt == "" || panelID <= 0 {
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
	if decoded.DatabaseID == uuid.Nil() || decoded.PostedAt == "" || decoded.PanelID <= 0 {
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
	existsArgs := []any{decoded.PanelID, decoded.PostedAt}
	if legacy {
		// A legacy cursor names a pseudopanel unit by its MIN(j.id) anchor
		// job and the unit's latest finish; re-derive that unit's group and
		// check both still hold. The era end bounds all three membership
		// fragments; when the database has no panel activity ("" era end),
		// nothing matches and the cursor is rejected as not found.
		eraEnd, err := db.legacyPanelEraEnd()
		if err != nil {
			return nil, err
		}
		existsQuery = `
			SELECT COUNT(1) FROM review_jobs a
			WHERE a.id = ?
			  AND ` + legacyUnitJobConditionsFor("a") + `
			  AND (SELECT MAX(` + legacyUnitTimeExpr("j.finished_at") + `)
			       FROM review_jobs j
			       WHERE j.repo_id = a.repo_id AND j.git_ref = a.git_ref
			         AND ` + legacyUnitJobConditions + `
			         AND ` + legacyUnitTimeExpr("j.enqueued_at") + ` <= (
			           SELECT strftime('%Y-%m-%dT%H:%M:%SZ',
			                    datetime(MIN(strftime('%Y-%m-%dT%H:%M:%SZ', jj.enqueued_at)), '+1 hour'))
			           FROM review_jobs jj
			           WHERE jj.repo_id = a.repo_id AND jj.git_ref = a.git_ref
			             AND ` + legacyUnitJobConditionsFor("jj") + `)) = ?`
		existsArgs = []any{decoded.PanelID, eraEnd, eraEnd, eraEnd, decoded.PostedAt}
	}
	var count int
	if err := db.QueryRow(existsQuery, existsArgs...).Scan(&count); err != nil {
		return nil, fmt.Errorf("validate ci metrics cursor: %w", err)
	}
	if count == 0 {
		return nil, fmt.Errorf("%w: posted_at %q panel_id %d",
			ErrExportCursorNotFound, decoded.PostedAt, decoded.PanelID)
	}
	return &decoded, nil
}
