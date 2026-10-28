package storage

import (
	"database/sql/driver"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"modernc.org/sqlite"
)

// JobSortColumn names a column the job listing can be ordered by. The columns
// match the sortable columns of the web Reviews table.
type JobSortColumn string

const (
	JobSortID         JobSortColumn = "id"
	JobSortRepo       JobSortColumn = "repo"
	JobSortAgent      JobSortColumn = "agent"
	JobSortReviewType JobSortColumn = "review_type"
	JobSortStatus     JobSortColumn = "status"
	JobSortVerdict    JobSortColumn = "verdict"
	JobSortClosed     JobSortColumn = "closed"
	JobSortElapsed    JobSortColumn = "elapsed"
	JobSortCost       JobSortColumn = "cost"
	JobSortEnqueuedAt JobSortColumn = "enqueued_at"
)

// ErrInvalidSortPosition reports a resume position whose key count does not
// match the listing's sort column.
var ErrInvalidSortPosition = errors.New("sort position does not match the sort column")

// ParseJobSortColumn returns the sort column with the given name. An empty
// name selects the default enqueue-time order.
func ParseJobSortColumn(name string) (JobSortColumn, error) {
	switch column := JobSortColumn(name); column {
	case "":
		return JobSortEnqueuedAt, nil
	case JobSortID, JobSortRepo, JobSortAgent, JobSortReviewType, JobSortStatus,
		JobSortVerdict, JobSortClosed, JobSortElapsed, JobSortCost, JobSortEnqueuedAt:
		return column, nil
	default:
		return "", fmt.Errorf("unknown sort column %q", name)
	}
}

// JobSort orders a job listing by one column, breaking ties by job ID in the
// same direction. The zero value is the default order: newest enqueue time
// first.
type JobSort struct {
	Column    JobSortColumn
	Ascending bool
	// AsOf is the time an unfinished job's elapsed time runs to. Every page of
	// one elapsed-time listing must use the same AsOf, or running jobs move
	// between pages as their elapsed time grows.
	AsOf time.Time
}

// JobSortPosition is where a sorted listing resumes: the sort key values and
// ID of the last job on the previous page, as ListJobs reported them in
// ReviewJob.SortKey and ReviewJob.ID.
type JobSortPosition struct {
	Keys  []any
	JobID int64
}

// WithSort orders the listing. Without it, jobs are listed newest first.
func WithSort(sort JobSort) ListJobsOption {
	return func(o *listJobsOptions) { o.sort = sort }
}

// WithSortAfter resumes the listing strictly after the given position in its
// sort order.
func WithSortAfter(position JobSortPosition) ListJobsOption {
	return func(o *listJobsOptions) { o.sortAfter = &position }
}

// jobSortKeys returns the SQL expressions a listing sorts by before the job
// ID, and the arguments those expressions bind, in order. Expressions are
// non-NULL so row-value comparisons against a resume position stay defined.
// The enqueue time is the exception: it must match the expression of
// idx_review_jobs_enqueued_position for the default listing to read the index
// in order, and it is NULL only for timestamp text SQLite cannot parse.
// The expressions reproduce what the web table showed when it sorted rows
// itself, so the server order matches the displayed values.
func jobSortKeys(sort JobSort, includeFindings bool) ([]string, []any) {
	switch sort.Column {
	case JobSortID:
		return nil, nil
	case JobSortRepo:
		return []string{"COALESCE(r.name, '')", "COALESCE(j.branch, '')"}, nil
	case JobSortAgent:
		return []string{"COALESCE(j.agent, '')"}, nil
	case JobSortReviewType:
		return []string{jobReviewTypeLabelExpr}, nil
	case JobSortStatus:
		return []string{"COALESCE(j.status, '')"}, nil
	case JobSortVerdict:
		return []string{jobVerdictSortExpr(includeFindings)}, nil
	case JobSortClosed:
		return []string{"COALESCE(rv.closed, -1)"}, nil
	case JobSortElapsed:
		asOf := sort.AsOf.UTC().Format(time.DateTime)
		return []string{jobElapsedSortExpr}, slices.Repeat([]any{asOf}, strings.Count(jobElapsedSortExpr, "?"))
	case JobSortCost:
		return []string{jobCostSortExpr}, nil
	default:
		return []string{sqliteNormalizedTimestampExpr("j.enqueued_at")}, nil
	}
}

// jobPanelParentCond matches a panel synthesis row, whose elapsed time and
// cost cover its members.
const jobPanelParentCond = "j.panel_role = 'synthesis' AND NULLIF(j.panel_run_uuid, '') IS NOT NULL"

const jobPanelMembersFrom = "FROM review_jobs m WHERE m.panel_run_uuid = j.panel_run_uuid AND m.panel_role = 'member'"

// jobReviewTypeLabelExpr is the text of the table's type column: the review
// type ("panel" for a synthesis row, "default" for a plain review), prefixed
// with the job type for jobs other than plain reviews and syntheses.
var jobReviewTypeLabelExpr = func() string {
	label := "CASE WHEN j.panel_role = 'synthesis' THEN 'panel'" +
		" WHEN COALESCE(j.review_type, '') IN ('', 'default', 'general', 'review') THEN 'default'" +
		" ELSE j.review_type END"
	return "CASE WHEN COALESCE(j.job_type, '') IN ('" + JobTypeReview + "', '" + JobTypeSynthesis + "') THEN " + label +
		" ELSE COALESCE(j.job_type, '') || ' ' || char(183) || ' ' || " + label + " END"
}()

// jobVerdictSortExpr is the verdict ListJobs reports ('P', 'F', or ” for
// none), following applyJobVerdict. Free-form job types have no stored
// verdict, so their review text is parsed as the listing does.
func jobVerdictSortExpr(includeFindings bool) string {
	// The listing loads a legacy row's diff only with finding counts, and
	// IsTaskJob treats a loaded diff as a review.
	diffAbsent := "1"
	if includeFindings {
		diffAbsent = "jc.diff_content IS NULL"
	}
	isTask := "CASE WHEN COALESCE(j.job_type, '') != '' THEN j.job_type IN ('" + JobTypeTask + "', '" + JobTypeInsights + "')" +
		" ELSE j.commit_id IS NULL AND " + diffAbsent +
		" AND COALESCE(j.git_ref, '') NOT IN ('', 'dirty') AND instr(j.git_ref, '..') = 0 END"
	reviewDocumentTypes := []string{"", JobTypeReview, JobTypeRange, JobTypeDirty, JobTypeSynthesis, JobTypeCompact, JobTypeGoalReview}
	return "CASE" +
		" WHEN rv.verdict_bool IS NULL AND COALESCE(rv.output, '') = '' THEN ''" +
		" WHEN COALESCE(j.error, '') != '' THEN ''" +
		" WHEN " + isTask + " THEN ''" +
		" WHEN rv.verdict_bool = 1 THEN 'P'" +
		" WHEN rv.verdict_bool IS NOT NULL THEN 'F'" +
		" WHEN COALESCE(j.job_type, '') IN ('" + strings.Join(reviewDocumentTypes, "', '") + "') THEN ''" +
		" ELSE roborev_parse_verdict(rv.output) END"
}

// jobElapsedSortExpr is a job's run time in whole seconds as of the bound
// AsOf time, or -1 for a job that had not started by then. Every "?" binds
// AsOf. Measuring at AsOf keeps a job in place when it starts or finishes
// between pages: its real finish time would otherwise move it past the cursor
// and out of every later page. A panel synthesis row starts with its earliest
// member.
var jobElapsedSortExpr = func() string {
	startOf := func(column string) string {
		started := "julianday(" + sqliteNormalizedTimestampExpr(column) + ")"
		return "CASE WHEN " + started + " <= julianday(?) THEN " + started + " END"
	}
	ownStart := startOf("j.started_at")
	memberStart := "(SELECT MIN(" + startOf("m.started_at") + ") " + jobPanelMembersFrom + ")"
	start := "CASE WHEN " + jobPanelParentCond +
		" THEN COALESCE(MIN(" + ownStart + ", " + memberStart + "), " + ownStart + ", " + memberStart + ")" +
		" ELSE " + ownStart + " END"
	end := "MIN(COALESCE(julianday(" + sqliteNormalizedTimestampExpr("j.finished_at") + "), julianday(?)), julianday(?))"
	return "COALESCE(MAX(0, CAST(ROUND((" + end + " - (" + start + ")) * 86400) AS INTEGER)), -1)"
}()

// jobCostSortExpr is a job's reported USD cost, or -1 when it reports none. A
// panel synthesis row adds its members' costs.
var jobCostSortExpr = func() string {
	cost := func(alias string) string {
		usage := alias + ".token_usage"
		return "CASE WHEN json_valid(" + usage + ") THEN CASE WHEN json_type(" + usage + ", '$.has_cost') = 'true'" +
			" AND json_type(" + usage + ", '$.cost_usd') IN ('integer', 'real')" +
			" THEN json_extract(" + usage + ", '$.cost_usd') END END"
	}
	own := cost("j")
	members := "(SELECT SUM(" + cost("m") + ") " + jobPanelMembersFrom + ")"
	return "CASE WHEN " + jobPanelParentCond +
		" THEN COALESCE((" + own + ") + " + members + ", " + own + ", " + members + ", -1)" +
		" ELSE COALESCE(" + own + ", -1) END"
}()

func sqlParseVerdict(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
	switch v := args[0].(type) {
	case nil:
		return "", nil
	case string:
		return string(ParseVerdict(v)), nil
	case []byte:
		return string(ParseVerdict(string(v))), nil
	default:
		return nil, fmt.Errorf("roborev_parse_verdict: unsupported argument type %T", v)
	}
}
