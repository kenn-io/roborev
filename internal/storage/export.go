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
	"unicode/utf8"
)

type ExportProfile string

const (
	ExportProfileContent  ExportProfile = "content"
	ExportProfileMetadata ExportProfile = "metadata"

	exportContentMaxBytes   = 1 << 20
	exportDefaultPageLimit  = 500
	exportMaxPageLimit      = 5000
	exportStringMaxBytes    = 4096
	exportTruncationMarker  = "...[truncated]"
	exportReviewStatusDone  = "done"
	exportReviewVerdictPass = "pass"
	exportReviewVerdictFail = "fail"
)

type ExportReviewsOptions struct {
	Profile    ExportProfile
	Since      time.Time
	Until      time.Time
	Cursor     string
	ClosedOnly bool
	Repo       string
	Project    string
	Limit      int
}

type ExportReviewsPage struct {
	Reviews    []ExportReview `json:"reviews"`
	Truncated  bool           `json:"truncated"`
	NextCursor *string        `json:"next_cursor"`
}

type ExportReview struct {
	ReviewID    string           `json:"review_id"`
	Status      string           `json:"status"`
	Verdict     string           `json:"verdict"`
	CreatedAt   string           `json:"created_at"`
	CompletedAt string           `json:"completed_at"`
	DurationMS  *int64           `json:"duration_ms"`
	Project     string           `json:"project"`
	Repo        string           `json:"repo"`
	Branch      *string          `json:"branch"`
	CommitSHA   *string          `json:"commit_sha"`
	PRNumber    *int64           `json:"pr_number"`
	PRURL       *string          `json:"pr_url"`
	Agent       string           `json:"agent"`
	Model       *string          `json:"model"`
	Cost        ExportReviewCost `json:"cost"`
	Content     *string          `json:"content"`
	Subagents   []ExportSubagent `json:"subagents"`
}

type ExportSubagent struct {
	ReviewID    string           `json:"review_id"`
	Name        string           `json:"name"`
	Agent       string           `json:"agent"`
	Model       *string          `json:"model"`
	ReviewType  *string          `json:"review_type"`
	Verdict     string           `json:"verdict"`
	CompletedAt string           `json:"completed_at"`
	DurationMS  *int64           `json:"duration_ms"`
	Cost        ExportReviewCost `json:"cost"`
	Content     *string          `json:"content"`
}

type ExportReviewCost struct {
	TokensIn  *int64   `json:"tokens_in"`
	TokensOut *int64   `json:"tokens_out"`
	USD       *float64 `json:"usd"`
}

type exportCursor struct {
	CompletedAt string `json:"completed_at"`
	ReviewID    string `json:"review_id"`
}

type exportReviewRow struct {
	reviewID      string
	verdictBool   int64
	reviewCreated string
	output        sql.NullString
	status        string
	enqueuedAt    string
	startedAt     sql.NullString
	finishedAt    sql.NullString
	agent         string
	model         sql.NullString
	gitRef        string
	jobType       string
	branch        sql.NullString
	ciBaseBranch  sql.NullString
	panelRunUUID  sql.NullString
	panelRole     sql.NullString
	tokenUsage    sql.NullString
	project       string
	repoIdentity  sql.NullString
	commitSHA     sql.NullString
	ciGitHubRepo  sql.NullString
	ciPRNumber    sql.NullInt64
	ciHeadSHA     sql.NullString
}

// ExportReviews returns one bounded page of completed review export rows.
func (db *DB) ExportReviews(opts ExportReviewsOptions) (ExportReviewsPage, error) {
	if opts.Profile == "" {
		opts.Profile = ExportProfileContent
	}
	if opts.Profile != ExportProfileContent && opts.Profile != ExportProfileMetadata {
		return ExportReviewsPage{}, fmt.Errorf("unsupported export profile %q", opts.Profile)
	}
	switch {
	case opts.Limit <= 0:
		opts.Limit = exportDefaultPageLimit
	case opts.Limit > exportMaxPageLimit:
		opts.Limit = exportMaxPageLimit
	}

	cursor, err := decodeExportCursor(opts.Cursor)
	if err != nil {
		return ExportReviewsPage{}, err
	}

	rows, err := db.queryExportReviewRows(opts, cursor)
	if err != nil {
		return ExportReviewsPage{}, err
	}
	defer rows.Close()

	pageLimit := opts.Limit
	page := ExportReviewsPage{Reviews: []ExportReview{}}
	for rows.Next() {
		row, err := scanExportReviewRow(rows)
		if err != nil {
			return ExportReviewsPage{}, err
		}
		if pageLimit > 0 && len(page.Reviews) == pageLimit {
			page.Truncated = true
			break
		}
		review := row.toExportReview(opts.Profile)
		if row.panelRole.String == PanelRoleSynthesis && row.panelRunUUID.String != "" {
			review.Subagents, err = db.exportSubagents(row.panelRunUUID.String, opts.Profile)
			if err != nil {
				return ExportReviewsPage{}, err
			}
		}
		page.Reviews = append(page.Reviews, review)
	}
	if err := rows.Err(); err != nil {
		return ExportReviewsPage{}, err
	}
	if page.Truncated && len(page.Reviews) > 0 {
		last := page.Reviews[len(page.Reviews)-1]
		nextCursor := encodeExportCursor(last.CompletedAt, last.ReviewID)
		if nextCursor != "" {
			page.NextCursor = &nextCursor
		}
	}
	return page, nil
}

func (db *DB) queryExportReviewRows(opts ExportReviewsOptions, cursor *exportCursor) (*sql.Rows, error) {
	outputExpr := "NULL"
	if opts.Profile == ExportProfileContent {
		outputExpr = "rv.output"
	}
	completedExpr := sqliteNormalizedTimestampExpr("rv.created_at")
	var conditions []string
	args := make([]any, 0)
	conditions = append(conditions,
		"j.status = 'done'",
		"COALESCE(j.job_type, 'review') IN ('review','range','dirty','synthesis')",
		"COALESCE(j.panel_role, '') != 'member'",
		"rv.verdict_bool IS NOT NULL",
	)
	if !opts.Since.IsZero() {
		conditions = append(conditions, completedExpr+" >= datetime(?)")
		args = append(args, opts.Since.UTC().Format(time.RFC3339))
	}
	if !opts.Until.IsZero() {
		conditions = append(conditions, completedExpr+" < datetime(?)")
		args = append(args, opts.Until.UTC().Format(time.RFC3339))
	}
	if opts.ClosedOnly {
		conditions = append(conditions, "rv.closed = 1")
	}
	if opts.Repo != "" {
		conditions = append(conditions, "COALESCE(NULLIF(TRIM(rp.identity), ''), rp.name) = ?")
		args = append(args, opts.Repo)
	}
	if opts.Project != "" {
		conditions = append(conditions, "rp.name = ?")
		args = append(args, opts.Project)
	}
	if cursor != nil {
		conditions = append(conditions, "("+completedExpr+" > datetime(?) OR ("+completedExpr+" = datetime(?) AND rv.uuid > ?))")
		args = append(args, cursor.CompletedAt, cursor.CompletedAt, cursor.ReviewID)
	}

	limitClause := ""
	if opts.Limit > 0 {
		limitClause = " LIMIT ?"
		args = append(args, opts.Limit+1)
	}

	query := `
		SELECT rv.uuid, rv.verdict_bool, rv.created_at, ` + outputExpr + `,
		       j.status, j.enqueued_at, j.started_at, j.finished_at, j.agent, j.model,
		       j.git_ref, COALESCE(j.job_type, 'review'), j.branch, j.ci_base_branch,
		       j.panel_run_uuid, j.panel_role, j.token_usage,
		       rp.name, rp.identity, c.sha,
		       cp.github_repo, cp.pr_number, cp.head_sha
		FROM reviews rv
		JOIN review_jobs j ON j.id = rv.job_id
		JOIN repos rp ON rp.id = j.repo_id
		LEFT JOIN commits c ON c.id = j.commit_id
		LEFT JOIN ci_pr_panels cp ON cp.panel_run_uuid = j.panel_run_uuid
		WHERE ` + strings.Join(conditions, " AND ") + `
		ORDER BY ` + completedExpr + ` ASC, rv.uuid ASC` + limitClause
	return db.Query(query, args...)
}

func scanExportReviewRow(rows *sql.Rows) (exportReviewRow, error) {
	var row exportReviewRow
	err := rows.Scan(
		&row.reviewID,
		&row.verdictBool,
		&row.reviewCreated,
		&row.output,
		&row.status,
		&row.enqueuedAt,
		&row.startedAt,
		&row.finishedAt,
		&row.agent,
		&row.model,
		&row.gitRef,
		&row.jobType,
		&row.branch,
		&row.ciBaseBranch,
		&row.panelRunUUID,
		&row.panelRole,
		&row.tokenUsage,
		&row.project,
		&row.repoIdentity,
		&row.commitSHA,
		&row.ciGitHubRepo,
		&row.ciPRNumber,
		&row.ciHeadSHA,
	)
	return row, err
}

func (row exportReviewRow) toExportReview(profile ExportProfile) ExportReview {
	completed := parseSQLiteTime(row.reviewCreated)
	repo := row.project
	if row.repoIdentity.Valid && strings.TrimSpace(row.repoIdentity.String) != "" {
		repo = row.repoIdentity.String
	}
	review := ExportReview{
		ReviewID:    capExportString(row.reviewID),
		Status:      capExportString(row.status),
		Verdict:     exportVerdict(row.verdictBool),
		CreatedAt:   formatExportTime(parseSQLiteTime(row.enqueuedAt)),
		CompletedAt: formatExportTime(completed),
		DurationMS:  exportDurationMS(row.startedAt, row.finishedAt),
		Project:     capExportString(row.project),
		Repo:        capExportString(repo),
		Branch:      stringPtrNonEmpty(firstNonEmpty(row.branch, row.ciBaseBranch)),
		CommitSHA:   stringPtrNonEmpty(row.exportCommitSHA()),
		PRNumber:    int64PtrValid(row.ciPRNumber),
		Agent:       capExportString(row.agent),
		Model:       stringPtrNonEmpty(nullStringValue(row.model)),
		Cost:        parseExportCost(row.tokenUsage),
		Subagents:   []ExportSubagent{},
	}
	if review.PRNumber != nil && row.ciGitHubRepo.Valid && row.ciGitHubRepo.String != "" {
		review.PRURL = stringPtrNonEmpty("https://github.com/" + capExportString(row.ciGitHubRepo.String) + "/pull/" + fmt.Sprint(*review.PRNumber))
	}
	if profile == ExportProfileContent && row.output.Valid {
		review.Content = exportContentPtr(row.output.String)
	}
	return review
}

func (row exportReviewRow) exportCommitSHA() string {
	if row.ciHeadSHA.Valid && isSHALike(row.ciHeadSHA.String) {
		rangeEnd := rangeEndSHA(row.gitRef)
		if rangeEnd != "" && rangeEnd != row.ciHeadSHA.String {
			log.Printf("storage: export synthesis head_sha mismatch for review %s: range end %s, ci head %s",
				row.reviewID, rangeEnd, row.ciHeadSHA.String)
		}
		return row.ciHeadSHA.String
	}
	switch row.jobType {
	case JobTypeReview:
		return nullStringValue(row.commitSHA)
	case JobTypeRange, JobTypeSynthesis:
		return rangeEndSHA(row.gitRef)
	default:
		return ""
	}
}

func (db *DB) exportSubagents(panelRunUUID string, profile ExportProfile) ([]ExportSubagent, error) {
	outputExpr := "NULL"
	if profile == ExportProfileContent {
		outputExpr = "rv.output"
	}
	rows, err := db.Query(`
		SELECT rv.uuid, rv.verdict_bool, rv.created_at, `+outputExpr+`,
		       j.agent, j.model, j.review_type, j.panel_member_name, j.started_at, j.finished_at,
		       j.token_usage
		FROM review_jobs j
		JOIN reviews rv ON rv.job_id = j.id
		WHERE j.panel_run_uuid = ?
		  AND j.panel_role = 'member'
		  AND j.status = 'done'
		  AND rv.verdict_bool IS NOT NULL
		ORDER BY j.panel_member_index ASC, j.id ASC
	`, panelRunUUID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []ExportSubagent{}
	for rows.Next() {
		var reviewID string
		var verdictBool int64
		var completedAt string
		var output sql.NullString
		var agentName string
		var model sql.NullString
		var reviewType sql.NullString
		var memberName sql.NullString
		var startedAt sql.NullString
		var finishedAt sql.NullString
		var tokenUsage sql.NullString
		if err := rows.Scan(&reviewID, &verdictBool, &completedAt, &output, &agentName, &model, &reviewType, &memberName, &startedAt, &finishedAt, &tokenUsage); err != nil {
			return nil, err
		}
		name := nullStringValue(memberName)
		if name == "" {
			name = agentName
		}
		sub := ExportSubagent{
			ReviewID:    capExportString(reviewID),
			Name:        capExportString(name),
			Agent:       capExportString(agentName),
			Model:       stringPtrNonEmpty(nullStringValue(model)),
			ReviewType:  stringPtrNonEmpty(nullStringValue(reviewType)),
			Verdict:     exportVerdict(verdictBool),
			CompletedAt: formatExportTime(parseSQLiteTime(completedAt)),
			DurationMS:  exportDurationMS(startedAt, finishedAt),
			Cost:        parseExportCost(tokenUsage),
		}
		if profile == ExportProfileContent && output.Valid {
			sub.Content = exportContentPtr(output.String)
		}
		out = append(out, sub)
	}
	return out, rows.Err()
}

func parseExportCost(tokenUsage sql.NullString) ExportReviewCost {
	if !tokenUsage.Valid || tokenUsage.String == "" {
		return ExportReviewCost{}
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(tokenUsage.String), &raw); err != nil {
		return ExportReviewCost{}
	}
	return ExportReviewCost{
		TokensIn:  int64PtrPositive(jsonInt64(raw, "input_tokens")),
		TokensOut: int64PtrPositive(jsonInt64(raw, "total_output_tokens")),
		USD:       float64PtrCost(raw),
	}
}

func jsonInt64(raw map[string]any, key string) int64 {
	switch v := raw[key].(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case json.Number:
		n, _ := v.Int64()
		return n
	default:
		return 0
	}
}

func float64PtrCost(raw map[string]any) *float64 {
	hasCost, ok := raw["has_cost"].(bool)
	if !ok || !hasCost {
		return nil
	}
	v, ok := raw["cost_usd"].(float64)
	if !ok {
		return nil
	}
	return &v
}

func int64PtrPositive(v int64) *int64 {
	if v <= 0 {
		return nil
	}
	return &v
}

func int64PtrValid(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	out := v.Int64
	return &out
}

func stringPtrNonEmpty(v string) *string {
	v = capExportString(strings.TrimSpace(v))
	if v == "" {
		return nil
	}
	return &v
}

func exportContentPtr(v string) *string {
	capped := capExportContent(v)
	return &capped
}

func exportDurationMS(startedAt, finishedAt sql.NullString) *int64 {
	if !startedAt.Valid || !finishedAt.Valid {
		return nil
	}
	start := parseSQLiteTime(startedAt.String)
	finished := parseSQLiteTime(finishedAt.String)
	if start.IsZero() || finished.IsZero() {
		return nil
	}
	ms := finished.Sub(start).Milliseconds()
	return &ms
}

func formatExportTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func exportVerdict(verdictBool int64) string {
	if verdictBool == 1 {
		return exportReviewVerdictPass
	}
	return exportReviewVerdictFail
}

func firstNonEmpty(values ...sql.NullString) string {
	for _, v := range values {
		if v.Valid && strings.TrimSpace(v.String) != "" {
			return v.String
		}
	}
	return ""
}

func nullStringValue(v sql.NullString) string {
	if !v.Valid {
		return ""
	}
	return v.String
}

func capExportString(s string) string {
	return capUTF8Bytes(s, exportStringMaxBytes, "")
}

func capExportContent(s string) string {
	if len(s) <= exportContentMaxBytes {
		return s
	}
	return capUTF8Bytes(s, exportContentMaxBytes, exportTruncationMarker)
}

func capUTF8Bytes(s string, maxBytes int, marker string) string {
	if maxBytes <= 0 || len(s) <= maxBytes {
		return s
	}
	limit := maxBytes
	if marker != "" {
		limit -= len(marker)
	}
	if limit < 0 {
		limit = 0
	}
	for limit > 0 && !utf8.ValidString(s[:limit]) {
		_, size := utf8.DecodeLastRuneInString(s[:limit])
		if size <= 0 {
			limit--
		} else {
			limit -= size
		}
	}
	return s[:limit] + marker
}

func rangeEndSHA(ref string) string {
	parts := strings.Split(ref, "...")
	if len(parts) == 2 && isSHALike(parts[1]) {
		return parts[1]
	}
	parts = strings.Split(ref, "..")
	if len(parts) == 2 && isSHALike(parts[1]) {
		return parts[1]
	}
	return ""
}

func isSHALike(s string) bool {
	s = strings.TrimSpace(s)
	if len(s) < 7 || len(s) > 64 {
		return false
	}
	for _, r := range s {
		if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F') {
			continue
		}
		return false
	}
	return true
}

func encodeExportCursor(completedAt, reviewID string) string {
	if completedAt == "" || reviewID == "" {
		return ""
	}
	data, err := json.Marshal(exportCursor{CompletedAt: completedAt, ReviewID: reviewID})
	if err != nil {
		log.Printf("storage: warning: encode export cursor: %v", err)
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(data)
}

func decodeExportCursor(cursor string) (*exportCursor, error) {
	if cursor == "" {
		return nil, nil
	}
	data, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return nil, fmt.Errorf("invalid export cursor: %w", err)
	}
	var decoded exportCursor
	if err := json.Unmarshal(data, &decoded); err != nil {
		return nil, fmt.Errorf("invalid export cursor: %w", err)
	}
	if decoded.CompletedAt == "" || decoded.ReviewID == "" {
		return nil, errors.New("invalid export cursor: missing fields")
	}
	t, err := time.Parse(time.RFC3339Nano, decoded.CompletedAt)
	if err != nil {
		return nil, fmt.Errorf("invalid export cursor timestamp: %w", err)
	}
	decoded.CompletedAt = t.UTC().Format(time.RFC3339)
	return &decoded, nil
}
