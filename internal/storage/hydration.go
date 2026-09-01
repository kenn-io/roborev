package storage

import (
	"database/sql"
	"encoding/json"
	"uuid"
)

type sqlScanner interface {
	Scan(dest ...any) error
}

type reviewJobScanFields struct {
	EnqueuedAt        string
	StartedAt         sql.NullString
	FinishedAt        sql.NullString
	WorkerID          sql.NullString
	Error             sql.NullString
	Prompt            sql.NullString
	SourceMachineID   sql.Null[uuid.UUID]
	UUID              sql.Null[uuid.UUID]
	Model             sql.NullString
	Provider          sql.NullString
	RequestedModel    sql.NullString
	RequestedProvider sql.NullString
	Branch            sql.NullString
	CIBaseBranch      sql.NullString
	SessionID         sql.NullString
	ResumeSourceUUID  sql.Null[uuid.UUID]
	CommitID          sql.NullInt64
	CommitSubject     sql.NullString
	JobType           sql.NullString
	ReviewType        sql.NullString
	PatchID           sql.NullString
	ParentJobID       sql.NullInt64
	Patch             sql.NullString
	DiffContent       sql.NullString
	DirtyFiles        sql.NullString
	OutputPrefix      sql.NullString
	CommandLine       sql.NullString
	TokenUsage        sql.NullString
	Agentic           int
	PromptPrebuilt    int
	Closed            sql.NullInt64
	WorktreePath      string
	MinSeverity       string
	BackupAgent       string
	BackupModel       string
	PanelRunUUID      sql.Null[uuid.UUID]
	PanelRole         sql.NullString
	PanelName         sql.NullString
	PanelMemberName   sql.NullString
	PanelMemberIndex  sql.NullInt64
	PanelMemberConfig sql.NullString
	ClaimBlocked      int
	SkipReason        sql.NullString
	Source            sql.NullString
}

func applyReviewJobScan(job *ReviewJob, fields reviewJobScanFields) {
	if fields.CommitID.Valid {
		job.CommitID = &fields.CommitID.Int64
	}
	if fields.CommitSubject.Valid {
		job.CommitSubject = fields.CommitSubject.String
	}
	if fields.Branch.Valid {
		job.Branch = fields.Branch.String
	}
	if fields.CIBaseBranch.Valid {
		job.CIBaseBranch = fields.CIBaseBranch.String
	}
	if fields.SessionID.Valid {
		job.SessionID = fields.SessionID.String
	}
	if fields.ResumeSourceUUID.Valid {
		job.ResumeSourceJobUUID = &fields.ResumeSourceUUID.V
	}
	if fields.Model.Valid {
		job.Model = fields.Model.String
	}
	if fields.Provider.Valid {
		job.Provider = fields.Provider.String
	}
	if fields.RequestedModel.Valid {
		job.RequestedModel = fields.RequestedModel.String
	}
	if fields.RequestedProvider.Valid {
		job.RequestedProvider = fields.RequestedProvider.String
	}
	if fields.JobType.Valid {
		job.JobType = fields.JobType.String
	}
	if fields.ReviewType.Valid {
		job.ReviewType = fields.ReviewType.String
	}
	if fields.PatchID.Valid {
		job.PatchID = fields.PatchID.String
	}
	if fields.ParentJobID.Valid {
		job.ParentJobID = &fields.ParentJobID.Int64
	}
	if fields.Patch.Valid {
		job.Patch = &fields.Patch.String
	}
	if fields.DiffContent.Valid {
		job.DiffContent = &fields.DiffContent.String
	}
	if fields.DirtyFiles.Valid {
		job.DirtyFiles = decodeDirtyFiles(fields.DirtyFiles.String)
	}
	if fields.OutputPrefix.Valid {
		job.OutputPrefix = fields.OutputPrefix.String
	}
	if fields.Prompt.Valid {
		job.Prompt = fields.Prompt.String
	}
	if fields.WorkerID.Valid {
		job.WorkerID = fields.WorkerID.String
	}
	if fields.Error.Valid {
		job.Error = fields.Error.String
	}
	if fields.SourceMachineID.Valid {
		job.SourceMachineID = &fields.SourceMachineID.V
	}
	if fields.UUID.Valid {
		job.UUID = &fields.UUID.V
	}
	if fields.CommandLine.Valid {
		job.CommandLine = fields.CommandLine.String
	}
	if fields.TokenUsage.Valid {
		job.TokenUsage = fields.TokenUsage.String
	}
	job.Agentic = fields.Agentic != 0
	job.PromptPrebuilt = fields.PromptPrebuilt != 0
	if fields.EnqueuedAt != "" {
		job.EnqueuedAt = parseSQLiteTime(fields.EnqueuedAt)
	}
	if fields.StartedAt.Valid {
		t := parseSQLiteTime(fields.StartedAt.String)
		job.StartedAt = &t
		job.StartedAtRaw = fields.StartedAt.String
	}
	if fields.FinishedAt.Valid {
		t := parseSQLiteTime(fields.FinishedAt.String)
		job.FinishedAt = &t
	}
	if fields.Closed.Valid {
		closed := fields.Closed.Int64 != 0
		job.Closed = &closed
	}
	job.WorktreePath = fields.WorktreePath
	job.MinSeverity = fields.MinSeverity
	job.BackupAgent = fields.BackupAgent
	job.BackupModel = fields.BackupModel
	if fields.PanelRunUUID.Valid {
		job.PanelRunUUID = &fields.PanelRunUUID.V
	}
	if fields.PanelRole.Valid {
		job.PanelRole = fields.PanelRole.String
	}
	if fields.PanelName.Valid {
		job.PanelName = fields.PanelName.String
	}
	if fields.PanelMemberName.Valid {
		job.PanelMemberName = fields.PanelMemberName.String
	}
	if fields.PanelMemberIndex.Valid {
		job.PanelMemberIndex = int(fields.PanelMemberIndex.Int64)
	}
	if fields.PanelMemberConfig.Valid {
		job.PanelMemberConfigJSON = fields.PanelMemberConfig.String
	}
	job.ClaimBlocked = fields.ClaimBlocked != 0
	if fields.SkipReason.Valid {
		job.SkipReason = fields.SkipReason.String
	}
	if fields.Source.Valid {
		job.Source = fields.Source.String
	}
}

type reviewScanFields struct {
	CreatedAt        string
	Closed           int
	UUID             sql.Null[uuid.UUID]
	VerdictBool      sql.NullInt64
	StructuredOutput sql.NullString
}

const reviewSelectColumns = `
	rv.id, rv.job_id, rv.agent, rv.prompt, rv.output, rv.created_at,
	rv.closed, rv.uuid, rv.verdict_bool, rv.structured_output`

func reviewScanDestinations(
	review *Review,
	fields *reviewScanFields,
) []any {
	return []any{
		&review.ID,
		&review.JobID,
		&review.Agent,
		&review.Prompt,
		&review.Output,
		&fields.CreatedAt,
		&fields.Closed,
		&fields.UUID,
		&fields.VerdictBool,
		&fields.StructuredOutput,
	}
}

func scanReviewFields(
	scanner sqlScanner,
) (Review, reviewScanFields, error) {
	var review Review
	var fields reviewScanFields
	if err := scanner.Scan(reviewScanDestinations(&review, &fields)...); err != nil {
		return Review{}, reviewScanFields{}, err
	}
	applyReviewScan(&review, fields)
	return review, fields, nil
}

func scanReview(scanner sqlScanner) (Review, error) {
	review, _, err := scanReviewFields(scanner)
	return review, err
}

func applyReviewScan(review *Review, fields reviewScanFields) {
	review.CreatedAt = parseSQLiteTime(fields.CreatedAt)
	review.Closed = fields.Closed != 0
	if fields.UUID.Valid {
		review.UUID = &fields.UUID.V
	}
	if fields.StructuredOutput.Valid {
		_ = json.Unmarshal(
			[]byte(fields.StructuredOutput.String),
			&review.StructuredOutput,
		)
	}
	applyReviewVerdict(review, fields.VerdictBool)
}

func scanCommit(scanner sqlScanner) (*Commit, error) {
	var commit Commit
	var timestamp, createdAt string
	if err := scanner.Scan(
		&commit.ID,
		&commit.RepoID,
		&commit.SHA,
		&commit.Author,
		&commit.Subject,
		&timestamp,
		&createdAt,
	); err != nil {
		return nil, err
	}
	commit.Timestamp = parseSQLiteTime(timestamp)
	commit.CreatedAt = parseSQLiteTime(createdAt)
	return &commit, nil
}
