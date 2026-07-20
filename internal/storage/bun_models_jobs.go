package storage

import (
	"database/sql"

	"github.com/uptrace/bun"
)

var sqliteJobColumns = []string{
	"id",
	"repo_id",
	"commit_id",
	"git_ref",
	"branch",
	"ci_base_branch",
	"session_id",
	"agent",
	"model",
	"provider",
	"requested_model",
	"requested_provider",
	"reasoning",
	"job_type",
	"review_type",
	"patch_id",
	"status",
	"agentic",
	"agent_invoked",
	"enqueued_at",
	"started_at",
	"finished_at",
	"retry_not_before",
	"worker_id",
	"error",
	"prompt",
	"retry_count",
	"diff_content",
	"dirty_files",
	"output_prefix",
	"skip_reason",
	"source",
	"parent_job_id",
	"patch",
	"worktree_path",
	"command_line",
	"prompt_prebuilt",
	"min_severity",
	"backup_agent",
	"backup_model",
	"panel_run_uuid",
	"panel_role",
	"panel_name",
	"panel_member_name",
	"panel_member_index",
	"panel_member_config_json",
	"claim_blocked",
	"token_usage",
	"uuid",
	"source_machine_id",
	"updated_at",
	"synced_at",
}

// postgresJobColumns is the synchronized PostgreSQL projection, not every
// physical schema column. Local checkout and scheduling state stays in SQLite.
var postgresJobColumns = []string{
	"uuid",
	"repo_id",
	"commit_id",
	"git_ref",
	"session_id",
	"agent",
	"model",
	"provider",
	"requested_model",
	"requested_provider",
	"reasoning",
	"job_type",
	"review_type",
	"patch_id",
	"status",
	"agentic",
	"agent_invoked",
	"enqueued_at",
	"started_at",
	"finished_at",
	"prompt",
	"diff_content",
	"dirty_files",
	"error",
	"token_usage",
	"worktree_path",
	"min_severity",
	"backup_agent",
	"backup_model",
	"source",
	"panel_run_uuid",
	"panel_role",
	"panel_name",
	"panel_member_name",
	"panel_member_index",
	"panel_member_config_json",
	"source_machine_id",
	"updated_at",
}

var sqliteJobInsertColumns = []string{
	"repo_id",
	"commit_id",
	"git_ref",
	"branch",
	"ci_base_branch",
	"session_id",
	"agent",
	"model",
	"provider",
	"requested_model",
	"requested_provider",
	"reasoning",
	"status",
	"job_type",
	"review_type",
	"patch_id",
	"diff_content",
	"dirty_files",
	"prompt",
	"agentic",
	"prompt_prebuilt",
	"output_prefix",
	"parent_job_id",
	"uuid",
	"source_machine_id",
	"updated_at",
	"worktree_path",
	"min_severity",
	"backup_agent",
	"backup_model",
	"panel_run_uuid",
	"panel_role",
	"panel_name",
	"panel_member_name",
	"panel_member_index",
	"panel_member_config_json",
	"claim_blocked",
	"source",
}

var sqliteJobListColumns = []string{
	"j.id",
	"j.repo_id",
	"j.commit_id",
	"j.git_ref",
	"j.branch",
	"j.ci_base_branch",
	"j.session_id",
	"j.agent",
	"j.reasoning",
	"j.status",
	"j.enqueued_at",
	"j.started_at",
	"j.finished_at",
	"j.worker_id",
	"j.error",
	"j.retry_count",
	"COALESCE(j.agentic, 0) AS agentic",
	"COALESCE(j.prompt_prebuilt, 0) AS prompt_prebuilt",
	"j.source_machine_id",
	"j.uuid",
	"j.model",
	"COALESCE(j.job_type, '') AS job_type",
	"COALESCE(j.review_type, '') AS review_type",
	"j.patch_id",
	"j.output_prefix",
	"j.parent_job_id",
	"j.provider",
	"j.requested_model",
	"j.requested_provider",
	"j.token_usage",
	"j.worktree_path",
	"j.command_line",
	"j.dirty_files",
	"COALESCE(j.min_severity, '') AS min_severity",
	"COALESCE(j.backup_agent, '') AS backup_agent",
	"COALESCE(j.backup_model, '') AS backup_model",
	"j.skip_reason",
	"j.source",
	"j.panel_run_uuid",
	"j.panel_role",
	"j.panel_name",
	"j.panel_member_name",
	"j.panel_member_index",
	"j.panel_member_config_json",
	"COALESCE(j.claim_blocked, 0) AS claim_blocked",
	"r.root_path AS repo_path",
	"r.name AS repo_name",
	"c.subject AS commit_subject",
	"rv.closed AS review_closed",
	"rv.output AS review_output",
	"rv.verdict_bool AS review_verdict_bool",
}

var sqliteJobDetailColumns = []string{
	"j.id",
	"j.repo_id",
	"j.commit_id",
	"j.git_ref",
	"j.branch",
	"j.ci_base_branch",
	"j.session_id",
	"j.agent",
	"j.reasoning",
	"j.status",
	"j.enqueued_at",
	"j.started_at",
	"j.finished_at",
	"j.worker_id",
	"j.error",
	"j.retry_count",
	"COALESCE(j.agentic, 0) AS agentic",
	"j.model",
	"j.provider",
	"j.requested_model",
	"j.requested_provider",
	"COALESCE(j.job_type, '') AS job_type",
	"COALESCE(j.review_type, '') AS review_type",
	"j.patch_id",
	"j.output_prefix",
	"j.parent_job_id",
	"j.patch",
	"j.token_usage",
	"j.dirty_files",
	"j.worktree_path",
	"j.command_line",
	"COALESCE(j.min_severity, '') AS min_severity",
	"COALESCE(j.backup_agent, '') AS backup_agent",
	"COALESCE(j.backup_model, '') AS backup_model",
	"j.skip_reason",
	"j.source",
	"j.panel_run_uuid",
	"j.panel_role",
	"j.panel_name",
	"j.panel_member_name",
	"j.panel_member_index",
	"j.panel_member_config_json",
	"COALESCE(j.claim_blocked, 0) AS claim_blocked",
	"r.root_path AS repo_path",
	"r.name AS repo_name",
	"c.subject AS commit_subject",
}

var sqliteJobClaimColumns = []string{
	"j.id",
	"j.repo_id",
	"j.commit_id",
	"j.git_ref",
	"j.branch",
	"j.ci_base_branch",
	"j.session_id",
	"j.agent",
	"j.model",
	"j.provider",
	"j.requested_model",
	"j.requested_provider",
	"j.reasoning",
	"j.status",
	"j.enqueued_at",
	"j.diff_content",
	"j.dirty_files",
	"j.prompt",
	"COALESCE(j.agentic, 0) AS agentic",
	"COALESCE(j.prompt_prebuilt, 0) AS prompt_prebuilt",
	"COALESCE(j.job_type, '') AS job_type",
	"COALESCE(j.review_type, '') AS review_type",
	"j.output_prefix",
	"j.patch_id",
	"j.parent_job_id",
	"j.worktree_path",
	"j.command_line",
	"COALESCE(j.min_severity, '') AS min_severity",
	"COALESCE(j.backup_agent, '') AS backup_agent",
	"COALESCE(j.backup_model, '') AS backup_model",
	"j.panel_run_uuid",
	"j.panel_role",
	"j.panel_name",
	"j.panel_member_name",
	"j.panel_member_index",
	"j.panel_member_config_json",
	"COALESCE(j.claim_blocked, 0) AS claim_blocked",
	"j.source",
	"j.retry_count",
	"j.uuid",
	"r.root_path AS repo_path",
	"r.name AS repo_name",
	"c.subject AS commit_subject",
}

var sqliteReviewJobColumns = []string{
	"j.id",
	"j.repo_id",
	"j.commit_id",
	"j.git_ref",
	"j.branch",
	"j.ci_base_branch",
	"j.session_id",
	"j.agent",
	"j.reasoning",
	"j.status",
	"j.enqueued_at",
	"j.started_at",
	"j.finished_at",
	"j.worker_id",
	"j.error",
	"j.model",
	"j.provider",
	"j.requested_model",
	"j.requested_provider",
	"COALESCE(j.job_type, '') AS job_type",
	"COALESCE(j.review_type, '') AS review_type",
	"j.patch_id",
	"j.token_usage",
	"COALESCE(j.min_severity, '') AS min_severity",
	"COALESCE(j.backup_agent, '') AS backup_agent",
	"COALESCE(j.backup_model, '') AS backup_model",
	"j.panel_run_uuid",
	"j.panel_role",
	"j.panel_name",
	"j.panel_member_name",
	"j.panel_member_index",
	"j.panel_member_config_json",
	"COALESCE(j.claim_blocked, 0) AS claim_blocked",
	"rp.root_path AS repo_path",
	"rp.name AS repo_name",
	"c.subject AS commit_subject",
}

var sqliteBatchReviewJobColumns = []string{
	"j.id",
	"j.repo_id",
	"j.commit_id",
	"j.git_ref",
	"j.branch",
	"j.ci_base_branch",
	"j.session_id",
	"j.agent",
	"j.reasoning",
	"j.status",
	"j.enqueued_at",
	"j.started_at",
	"j.finished_at",
	"j.worker_id",
	"j.error",
	"COALESCE(j.agentic, 0) AS agentic",
	"j.model",
	"COALESCE(j.job_type, '') AS job_type",
	"COALESCE(j.review_type, '') AS review_type",
	"COALESCE(j.min_severity, '') AS min_severity",
	"COALESCE(j.backup_agent, '') AS backup_agent",
	"COALESCE(j.backup_model, '') AS backup_model",
	"j.panel_run_uuid",
	"j.panel_role",
	"j.panel_name",
	"j.panel_member_name",
	"j.panel_member_index",
	"j.panel_member_config_json",
	"COALESCE(j.claim_blocked, 0) AS claim_blocked",
	"r.root_path AS repo_path",
	"r.name AS repo_name",
	"c.subject AS commit_subject",
}

type jobRow struct {
	bun.BaseModel `bun:"table:review_jobs,alias:j"`

	ID                    int64       `bun:"id,pk,autoincrement"`
	RepoID                int64       `bun:"repo_id"`
	CommitID              *int64      `bun:"commit_id"`
	GitRef                string      `bun:"git_ref"`
	Branch                *string     `bun:"branch"`
	CIBaseBranch          *string     `bun:"ci_base_branch"`
	SessionID             *string     `bun:"session_id"`
	Agent                 string      `bun:"agent"`
	Model                 *string     `bun:"model"`
	Provider              *string     `bun:"provider"`
	RequestedModel        *string     `bun:"requested_model"`
	RequestedProvider     *string     `bun:"requested_provider"`
	Reasoning             *string     `bun:"reasoning"`
	JobType               string      `bun:"job_type"`
	ReviewType            string      `bun:"review_type"`
	PatchID               *string     `bun:"patch_id"`
	Status                JobStatus   `bun:"status"`
	Agentic               bool        `bun:"agentic"`
	AgentInvoked          bool        `bun:"agent_invoked"`
	EnqueuedAt            dbTime      `bun:"enqueued_at"`
	StartedAt             dbTime      `bun:"started_at"`
	FinishedAt            dbTime      `bun:"finished_at"`
	RetryNotBefore        dbRetryTime `bun:"retry_not_before"`
	WorkerID              *string     `bun:"worker_id"`
	Error                 *string     `bun:"error"`
	Prompt                *string     `bun:"prompt"`
	RetryCount            int         `bun:"retry_count"`
	DiffContent           *string     `bun:"diff_content"`
	DirtyFiles            *string     `bun:"dirty_files"`
	OutputPrefix          *string     `bun:"output_prefix"`
	SkipReason            *string     `bun:"skip_reason"`
	Source                *string     `bun:"source"`
	ParentJobID           *int64      `bun:"parent_job_id"`
	Patch                 *string     `bun:"patch"`
	WorktreePath          *string     `bun:"worktree_path"`
	CommandLine           *string     `bun:"command_line"`
	PromptPrebuilt        bool        `bun:"prompt_prebuilt"`
	MinSeverity           string      `bun:"min_severity"`
	BackupAgent           string      `bun:"backup_agent"`
	BackupModel           string      `bun:"backup_model"`
	PanelRunUUID          *string     `bun:"panel_run_uuid"`
	PanelRole             *string     `bun:"panel_role"`
	PanelName             *string     `bun:"panel_name"`
	PanelMemberName       *string     `bun:"panel_member_name"`
	PanelMemberIndex      *int        `bun:"panel_member_index"`
	PanelMemberConfigJSON *string     `bun:"panel_member_config_json"`
	ClaimBlocked          bool        `bun:"claim_blocked"`
	TokenUsage            *string     `bun:"token_usage"`
	UUID                  *string     `bun:"uuid"`
	SourceMachineID       *string     `bun:"source_machine_id"`
	CreatedAt             dbTime      `bun:"created_at"`
	UpdatedAt             dbTime      `bun:"updated_at"`
	SyncedAt              dbTime      `bun:"synced_at"`
}

type jobHydrationRow struct {
	jobRow
	RepoPath          string  `bun:"repo_path"`
	RepoName          string  `bun:"repo_name"`
	CommitSubject     *string `bun:"commit_subject"`
	ReviewClosed      *bool   `bun:"review_closed"`
	ReviewOutput      *string `bun:"review_output"`
	ReviewVerdictBool *int64  `bun:"review_verdict_bool"`
}

func (row jobHydrationRow) toModel() ReviewJob {
	var job ReviewJob
	row.applyToModel(&job)
	job.RepoPath = row.RepoPath
	job.RepoName = row.RepoName
	job.CommitSubject = stringValue(row.CommitSubject)
	job.Closed = row.ReviewClosed
	if row.ReviewOutput != nil {
		var verdict sql.NullInt64
		if row.ReviewVerdictBool != nil {
			verdict = sql.NullInt64{
				Int64: *row.ReviewVerdictBool,
				Valid: true,
			}
		}
		applyJobVerdict(&job, verdict, *row.ReviewOutput)
	}
	return job
}

// jobRowFromModel maps fields owned by the public ReviewJob model. Callers
// must use operation-specific column lists; model-wide updates are forbidden
// because local runtime fields such as agent_invoked and retry_not_before are
// intentionally not represented by ReviewJob.
func jobRowFromModel(job ReviewJob) jobRow {
	dirtyFiles, _ := encodeDirtyFiles(job.DirtyFiles)
	row := jobRow{
		ID:                    job.ID,
		RepoID:                job.RepoID,
		CommitID:              cloneInt64Pointer(job.CommitID),
		GitRef:                job.GitRef,
		Branch:                optionalString(job.Branch),
		CIBaseBranch:          optionalString(job.CIBaseBranch),
		SessionID:             optionalString(job.SessionID),
		Agent:                 job.Agent,
		Model:                 optionalString(job.Model),
		Provider:              optionalString(job.Provider),
		RequestedModel:        optionalString(job.RequestedModel),
		RequestedProvider:     optionalString(job.RequestedProvider),
		Reasoning:             optionalString(job.Reasoning),
		JobType:               job.JobType,
		ReviewType:            job.ReviewType,
		PatchID:               optionalString(job.PatchID),
		Status:                job.Status,
		Agentic:               job.Agentic,
		EnqueuedAt:            dbTimeFromValue(job.EnqueuedAt),
		StartedAt:             dbTimeFromPointer(job.StartedAt),
		FinishedAt:            dbTimeFromPointer(job.FinishedAt),
		WorkerID:              optionalString(job.WorkerID),
		Error:                 optionalString(job.Error),
		Prompt:                optionalString(job.Prompt),
		RetryCount:            job.RetryCount,
		DiffContent:           cloneStringPointer(job.DiffContent),
		DirtyFiles:            optionalString(dirtyFiles),
		OutputPrefix:          optionalString(job.OutputPrefix),
		SkipReason:            optionalString(job.SkipReason),
		Source:                optionalString(job.Source),
		ParentJobID:           cloneInt64Pointer(job.ParentJobID),
		Patch:                 cloneStringPointer(job.Patch),
		WorktreePath:          optionalString(job.WorktreePath),
		CommandLine:           optionalString(job.CommandLine),
		PromptPrebuilt:        job.PromptPrebuilt,
		MinSeverity:           job.MinSeverity,
		BackupAgent:           job.BackupAgent,
		BackupModel:           job.BackupModel,
		PanelRunUUID:          optionalString(job.PanelRunUUID),
		PanelRole:             optionalString(job.PanelRole),
		PanelName:             optionalString(job.PanelName),
		PanelMemberName:       optionalString(job.PanelMemberName),
		PanelMemberIndex:      optionalInt(job.PanelMemberIndex),
		PanelMemberConfigJSON: optionalString(job.PanelMemberConfigJSON),
		ClaimBlocked:          job.ClaimBlocked,
		TokenUsage:            optionalString(job.TokenUsage),
		UUID:                  optionalString(job.UUID),
		SourceMachineID:       optionalString(job.SourceMachineID),
		UpdatedAt:             dbTimeFromPointer(job.UpdatedAt),
		SyncedAt:              dbTimeFromPointer(job.SyncedAt),
	}
	return row
}

func jobRowForInsert(job ReviewJob) jobRow {
	row := jobRowFromModel(job)
	if job.PanelRole == PanelRoleMember {
		row.PanelMemberIndex = &job.PanelMemberIndex
	}
	return row
}

func (row jobRow) applyToModel(job *ReviewJob) {
	job.ID = row.ID
	job.RepoID = row.RepoID
	job.CommitID = cloneInt64Pointer(row.CommitID)
	job.GitRef = row.GitRef
	job.Branch = stringValue(row.Branch)
	job.CIBaseBranch = stringValue(row.CIBaseBranch)
	job.SessionID = stringValue(row.SessionID)
	job.Agent = row.Agent
	job.Model = stringValue(row.Model)
	job.Provider = stringValue(row.Provider)
	job.RequestedModel = stringValue(row.RequestedModel)
	job.RequestedProvider = stringValue(row.RequestedProvider)
	job.Reasoning = stringValue(row.Reasoning)
	job.JobType = row.JobType
	job.ReviewType = row.ReviewType
	job.PatchID = stringValue(row.PatchID)
	job.Status = row.Status
	job.Agentic = row.Agentic
	job.EnqueuedAt = row.EnqueuedAt.Time
	job.StartedAt = row.StartedAt.pointer()
	job.FinishedAt = row.FinishedAt.pointer()
	job.WorkerID = stringValue(row.WorkerID)
	job.Error = stringValue(row.Error)
	job.Prompt = stringValue(row.Prompt)
	job.RetryCount = row.RetryCount
	job.DiffContent = cloneStringPointer(row.DiffContent)
	job.DirtyFiles = decodeDirtyFiles(stringValue(row.DirtyFiles))
	job.OutputPrefix = stringValue(row.OutputPrefix)
	job.SkipReason = stringValue(row.SkipReason)
	job.Source = stringValue(row.Source)
	job.ParentJobID = cloneInt64Pointer(row.ParentJobID)
	job.Patch = cloneStringPointer(row.Patch)
	job.WorktreePath = stringValue(row.WorktreePath)
	job.CommandLine = stringValue(row.CommandLine)
	job.PromptPrebuilt = row.PromptPrebuilt
	job.MinSeverity = row.MinSeverity
	job.BackupAgent = row.BackupAgent
	job.BackupModel = row.BackupModel
	job.PanelRunUUID = stringValue(row.PanelRunUUID)
	job.PanelRole = stringValue(row.PanelRole)
	job.PanelName = stringValue(row.PanelName)
	job.PanelMemberName = stringValue(row.PanelMemberName)
	job.PanelMemberIndex = intValue(row.PanelMemberIndex)
	job.PanelMemberConfigJSON = stringValue(row.PanelMemberConfigJSON)
	job.ClaimBlocked = row.ClaimBlocked
	job.TokenUsage = stringValue(row.TokenUsage)
	job.UUID = stringValue(row.UUID)
	job.SourceMachineID = stringValue(row.SourceMachineID)
	job.UpdatedAt = row.UpdatedAt.pointer()
	job.SyncedAt = row.SyncedAt.pointer()
}

func optionalInt(value int) *int {
	if value == 0 {
		return nil
	}
	return &value
}

func intValue(value *int) int {
	if value == nil {
		return 0
	}
	return *value
}

type reviewRow struct {
	bun.BaseModel      `bun:"table:reviews,alias:rv"`
	ID                 int64   `bun:"id,pk,autoincrement"`
	JobID              *int64  `bun:"job_id"`
	JobUUID            *string `bun:"job_uuid"`
	Agent              string  `bun:"agent"`
	Prompt             string  `bun:"prompt"`
	Output             string  `bun:"output"`
	CreatedAt          dbTime  `bun:"created_at"`
	Closed             bool    `bun:"closed"`
	UUID               *string `bun:"uuid"`
	UpdatedAt          dbTime  `bun:"updated_at"`
	UpdatedByMachineID *string `bun:"updated_by_machine_id"`
	SyncedAt           dbTime  `bun:"synced_at"`
	VerdictBool        *int    `bun:"verdict_bool"`
}

func (row reviewRow) toModel() Review {
	review := Review{
		ID:                 row.ID,
		Agent:              row.Agent,
		Prompt:             row.Prompt,
		Output:             row.Output,
		CreatedAt:          row.CreatedAt.Time,
		Closed:             row.Closed,
		UUID:               stringValue(row.UUID),
		UpdatedAt:          row.UpdatedAt.pointer(),
		UpdatedByMachineID: stringValue(row.UpdatedByMachineID),
		SyncedAt:           row.SyncedAt.pointer(),
	}
	if row.JobID != nil {
		review.JobID = *row.JobID
	}
	if row.VerdictBool != nil {
		verdict := *row.VerdictBool
		review.VerdictBool = &verdict
	}
	return review
}

type responseRow struct {
	bun.BaseModel   `bun:"table:responses,alias:rs"`
	ID              int64   `bun:"id,pk,autoincrement"`
	CommitID        *int64  `bun:"commit_id"`
	JobID           *int64  `bun:"job_id"`
	JobUUID         *string `bun:"job_uuid"`
	Responder       string  `bun:"responder"`
	Response        string  `bun:"response"`
	CreatedAt       dbTime  `bun:"created_at"`
	UUID            *string `bun:"uuid"`
	SourceMachineID *string `bun:"source_machine_id"`
	SyncedAt        dbTime  `bun:"synced_at"`
	InsertedAt      dbTime  `bun:"inserted_at"`
}

func (row responseRow) toModel() Response {
	return Response{
		ID:              row.ID,
		CommitID:        cloneInt64Pointer(row.CommitID),
		JobID:           cloneInt64Pointer(row.JobID),
		Responder:       row.Responder,
		Response:        row.Response,
		CreatedAt:       row.CreatedAt.Time,
		UUID:            stringValue(row.UUID),
		SourceMachineID: stringValue(row.SourceMachineID),
		SyncedAt:        row.SyncedAt.pointer(),
	}
}
