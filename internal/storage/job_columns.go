package storage

import "database/sql"

// Job rows are read through exactly two column lists: jobSelectColumns for a
// job on its own and jobWithReviewSelectColumns for a job joined with its
// review summary. Every query that hydrates a ReviewJob uses one of them with
// the matching scan destinations, so a new job column is added in one place
// and every read path returns it.
//
// Both lists expect these table aliases:
//
//	FROM review_jobs j
//	JOIN repos r ON r.id = j.repo_id
//	LEFT JOIN commits c ON c.id = j.commit_id
//
// and jobWithReviewSelectColumns additionally expects
//
//	LEFT JOIN reviews rv ON rv.job_id = j.id

// jobTextColumns names the SQL expressions used for the large TEXT payloads of
// a job row. Callers that do not need a payload select NULL so a listing never
// pulls prompts, diffs, or patches it will not show. The scan shape is the
// same whichever expressions are used.
type jobTextColumns struct {
	Prompt string
	Diff   string
	Patch  string
}

var (
	// jobTextAll loads every payload: the worker and single-job lookups need them.
	jobTextAll = jobTextColumns{Prompt: "j.prompt", Diff: "j.diff_content", Patch: "j.patch"}
	// jobTextPrompt loads only the prompt: panel member views show it inline.
	jobTextPrompt = jobTextColumns{Prompt: "j.prompt", Diff: "NULL", Patch: "NULL"}
	// jobTextNone loads no payload: batch metadata reads.
	jobTextNone = jobTextColumns{Prompt: "NULL", Diff: "NULL", Patch: "NULL"}
)

const jobSelectColumnsBase = `
		       j.id, j.uuid, j.repo_id, j.commit_id, j.git_ref, j.branch, j.ci_base_branch, j.session_id, j.resume_source_job_uuid,
		       j.agent, j.model, j.provider, j.requested_model, j.requested_provider, j.reasoning, j.status,
		       j.enqueued_at, j.started_at, j.finished_at, j.worker_id, j.error, j.retry_count,
		       COALESCE(j.agentic, 0), COALESCE(j.prompt_prebuilt, 0), j.job_type, j.review_type, j.patch_id,
		       COALESCE(j.output_prefix, ''), j.parent_job_id, j.token_usage, COALESCE(j.worktree_path, ''), j.command_line,
		       COALESCE(j.min_severity, ''), COALESCE(j.backup_agent, ''), COALESCE(j.backup_model, ''),
		       COALESCE(j.skip_reason, ''), COALESCE(j.source, ''), j.source_machine_id,
		       NULLIF(j.panel_run_uuid, ''), COALESCE(j.panel_role, ''), COALESCE(j.panel_name, ''), COALESCE(j.panel_member_name, ''), j.panel_member_index, COALESCE(j.panel_member_config_json, ''), COALESCE(j.claim_blocked, 0), COALESCE(j.non_voting, 0),
		       r.root_path, r.name, c.subject,
		       j.dirty_files, `

// jobSelectColumns is the job-only column list. text chooses which large
// payloads are loaded.
func jobSelectColumns(text jobTextColumns) string {
	return jobSelectColumnsBase + text.Prompt + ", " + text.Diff + ", " + text.Patch
}

// jobWithReviewSelectColumns is the job column list plus the review summary
// (closed flag, verdict, and output presence) from the LEFT JOINed reviews
// row. structuredOutputExpr is the expression for the stored review JSON;
// callers that do not need finding counts pass "”".
func jobWithReviewSelectColumns(text jobTextColumns, structuredOutputExpr string) string {
	return jobSelectColumns(text) + `,
		       rv.closed,
		       CASE WHEN rv.verdict_bool IS NULL THEN rv.output ELSE '' END,
		       rv.verdict_bool,
		       CASE WHEN rv.verdict_bool IS NOT NULL THEN 1 ELSE COALESCE(rv.output != '', 0) END,
		       ` + structuredOutputExpr
}

// jobScanDestinations returns the scan targets for jobSelectColumns, in order.
func jobScanDestinations(j *ReviewJob, f *reviewJobScanFields) []any {
	return []any{
		&j.ID, &f.UUID, &j.RepoID, &f.CommitID, &j.GitRef, &f.Branch, &f.CIBaseBranch, &f.SessionID, &f.ResumeSourceUUID,
		&j.Agent, &f.Model, &f.Provider, &f.RequestedModel, &f.RequestedProvider, &j.Reasoning, &j.Status,
		&f.EnqueuedAt, &f.StartedAt, &f.FinishedAt, &f.WorkerID, &f.Error, &j.RetryCount,
		&f.Agentic, &f.PromptPrebuilt, &f.JobType, &f.ReviewType, &f.PatchID,
		&f.OutputPrefix, &f.ParentJobID, &f.TokenUsage, &f.WorktreePath, &f.CommandLine,
		&f.MinSeverity, &f.BackupAgent, &f.BackupModel,
		&f.SkipReason, &f.Source, &f.SourceMachineID,
		&f.PanelRunUUID, &f.PanelRole, &f.PanelName, &f.PanelMemberName, &f.PanelMemberIndex, &f.PanelMemberConfig, &f.ClaimBlocked, &f.NonVoting,
		&j.RepoPath, &j.RepoName, &f.CommitSubject,
		&f.DirtyFiles, &f.Prompt, &f.DiffContent, &f.Patch,
	}
}

// jobReviewScanFields receives the review summary columns appended by
// jobWithReviewSelectColumns.
type jobReviewScanFields struct {
	Closed           sql.NullInt64
	Output           sql.NullString
	VerdictBool      sql.NullInt64
	HasOutput        bool
	StructuredOutput sql.NullString
}

// jobWithReviewScanDestinations returns the scan targets for
// jobWithReviewSelectColumns, in order.
func jobWithReviewScanDestinations(j *ReviewJob, f *reviewJobScanFields, rv *jobReviewScanFields) []any {
	return append(jobScanDestinations(j, f),
		&rv.Closed, &rv.Output, &rv.VerdictBool, &rv.HasOutput, &rv.StructuredOutput)
}

// applyJobWithReviewScan hydrates a job from both scan field sets: the job
// columns, then the review summary (closed flag, verdict, and optionally the
// finding counts from the stored review JSON).
func applyJobWithReviewScan(j *ReviewJob, f reviewJobScanFields, rv jobReviewScanFields, includeFindings bool) {
	f.Closed = rv.Closed
	applyReviewJobScan(j, f)
	applyJobVerdict(j, rv.VerdictBool, rv.Output.String, rv.HasOutput)
	if includeFindings {
		applyJobFindingCounts(j, rv.StructuredOutput)
	}
}
