package mcpserver

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"go.kenn.io/roborev/internal/storage"
)

const (
	defaultJobLimit = 50
	maxJobLimit     = 500
	maxOutputLines  = 2000
)

type statusInput struct{}

// statusOutput is the subset of the daemon status that is useful to an
// agent. It avoids types (UUIDs, nested snooze rows) whose generated JSON
// schema does not match their JSON encoding.
type statusOutput struct {
	Version        string `json:"version"`
	QueuedJobs     int    `json:"queued_jobs"`
	RunningJobs    int    `json:"running_jobs"`
	CompletedJobs  int    `json:"completed_jobs"`
	FailedJobs     int    `json:"failed_jobs"`
	CanceledJobs   int    `json:"canceled_jobs"`
	AppliedJobs    int    `json:"applied_jobs"`
	RebasedJobs    int    `json:"rebased_jobs"`
	SkippedJobs    int    `json:"skipped_jobs"`
	ActiveWorkers  int    `json:"active_workers"`
	MaxWorkers     int    `json:"max_workers"`
	QueuePaused    bool   `json:"queue_paused"`
	UpdateDraining bool   `json:"update_draining"`
	MachineID      string `json:"machine_id,omitempty"`
}

type listReposInput struct {
	Prefix string `json:"prefix,omitempty" jsonschema:"only repositories whose root path starts with this prefix"`
	Branch string `json:"branch,omitempty" jsonschema:"only repositories with jobs on this branch"`
}

type listReposOutput struct {
	Repos []storage.RepoWithCount `json:"repos"`
}

type listBranchesInput struct {
	RepoPath string `json:"repo_path" jsonschema:"repository root_path from roborev_list_repos"`
}

type listBranchesOutput struct {
	Branches []storage.BranchWithCount `json:"branches"`
}

type listJobsInput struct {
	RepoPath string `json:"repo_path,omitempty" jsonschema:"repository root_path from roborev_list_repos"`
	Branch   string `json:"branch,omitempty" jsonschema:"branch name at enqueue time"`
	Status   string `json:"status,omitempty" jsonschema:"queued, running, done, failed, canceled, applied, rebased, or skipped"`
	JobType  string `json:"job_type,omitempty" jsonschema:"review, range, dirty, task, compact, or fix"`
	Closed   *bool  `json:"closed,omitempty" jsonschema:"true for addressed reviews, false for open reviews"`
	Limit    int    `json:"limit,omitempty" jsonschema:"maximum rows to return; default 50, max 500"`
	Cursor   string `json:"cursor,omitempty" jsonschema:"next_cursor from a previous page"`
}

type jobRow struct {
	WebURL        string                 `json:"web_url,omitempty" jsonschema:"browser URL for this review on its owning daemon; use this URL when linking to the review"`
	ID            int64                  `json:"id"`
	UUID          string                 `json:"uuid,omitempty"`
	RepoPath      string                 `json:"repo_path,omitempty"`
	RepoName      string                 `json:"repo_name,omitempty"`
	GitRef        string                 `json:"git_ref"`
	Branch        string                 `json:"branch,omitempty"`
	CommitSubject string                 `json:"commit_subject,omitempty"`
	Agent         string                 `json:"agent"`
	Model         string                 `json:"model,omitempty"`
	JobType       string                 `json:"job_type"`
	ReviewType    string                 `json:"review_type,omitempty"`
	Status        string                 `json:"status"`
	Verdict       string                 `json:"verdict,omitempty"`
	Closed        *bool                  `json:"closed,omitempty"`
	FindingCounts *storage.FindingCounts `json:"finding_counts,omitempty"`
	Error         string                 `json:"error,omitempty"`
	EnqueuedAt    time.Time              `json:"enqueued_at"`
	StartedAt     *time.Time             `json:"started_at,omitempty"`
	FinishedAt    *time.Time             `json:"finished_at,omitempty"`
	ParentJobID   *int64                 `json:"parent_job_id,omitempty"`
}

type listJobsOutput struct {
	Jobs       []jobRow `json:"jobs"`
	HasMore    bool     `json:"has_more"`
	NextCursor string   `json:"next_cursor,omitempty"`
}

type reviewRefInput struct {
	JobID int64  `json:"job_id,omitempty" jsonschema:"review job id; takes precedence over sha"`
	SHA   string `json:"sha,omitempty" jsonschema:"commit SHA whose latest review to return"`
}

type reviewOutput struct {
	WebURL        string                 `json:"web_url,omitempty" jsonschema:"browser URL for this review on its owning daemon; use this URL when linking to the review"`
	ID            int64                  `json:"id"`
	JobID         int64                  `json:"job_id"`
	Agent         string                 `json:"agent"`
	Verdict       string                 `json:"verdict,omitempty"`
	Closed        bool                   `json:"closed"`
	FindingCounts *storage.FindingCounts `json:"finding_counts,omitempty"`
	CreatedAt     time.Time              `json:"created_at"`
	Output        string                 `json:"output"`
	Job           *jobRow                `json:"job,omitempty"`
}

type commentRow struct {
	ID        int64     `json:"id"`
	Responder string    `json:"responder"`
	Response  string    `json:"response"`
	Source    string    `json:"source,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

type listCommentsOutput struct {
	Comments []commentRow `json:"comments"`
}

type jobOutputInput struct {
	JobID int64 `json:"job_id" jsonschema:"job id"`
	Tail  int   `json:"tail,omitempty" jsonschema:"return only the last N lines; default and maximum 2000"`
}

type jobOutputResult struct {
	JobID  int64        `json:"job_id"`
	Status string       `json:"status"`
	Lines  []OutputLine `json:"lines"`
	// AvailableLines is the size of the daemon's retained snapshot, not the
	// number of lines the agent produced: running output is bounded by the
	// daemon buffer and persisted logs keep only their tail.
	AvailableLines int  `json:"available_lines"`
	Truncated      bool `json:"truncated" jsonschema:"true when this response omits lines from the daemon snapshot"`
	HasMore        bool `json:"has_more"`
}

func (s *Server) registerTools() {
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "roborev_status",
		Description: "Report roborev daemon version, queue counts, and worker usage. " +
			"Call this first to confirm the daemon is reachable.",
	}, wrapTool(s.status))
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "roborev_list_repos",
		Description: "List repositories tracked by roborev with their review job counts. " +
			"Use the returned root_path as repo_path in other tools.",
	}, wrapTool(s.listRepos))
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "roborev_list_branches",
		Description: "List branches with review job counts for one tracked repository.",
	}, wrapTool(s.listBranches))
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "roborev_list_jobs",
		Description: "List review jobs (metadata only, newest first) filtered by repository, " +
			"branch, status, job type, or closed state. Use next_cursor to page.",
	}, wrapTool(s.listJobs))
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "roborev_get_review",
		Description: "Return the full review output, verdict, and finding counts for a job id " +
			"or the latest review of a commit SHA.",
	}, wrapTool(s.getReview))
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "roborev_list_comments",
		Description: "List developer responses attached to a review by job id or commit SHA.",
	}, wrapTool(s.listComments))
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "roborev_get_job_output",
		Description: "Return the last lines of the agent's streamed output for a job, at most 2000. " +
			"available_lines is the size of the daemon's retained snapshot, which is itself bounded, " +
			"so earlier output of very long jobs may be gone. Useful for running or failed jobs; " +
			"completed reviews are better read with roborev_get_review.",
	}, wrapTool(s.getJobOutput))
}

type toolError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type toolErrorOutput struct {
	Error toolError `json:"error"`
}

// wrapTool adapts a plain handler into the SDK signature, converting errors
// into tool results that carry a stable error code.
func wrapTool[In, Out any](
	fn func(context.Context, In) (Out, error),
) mcp.ToolHandlerFor[In, Out] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in In) (*mcp.CallToolResult, Out, error) {
		out, err := fn(ctx, in)
		if err == nil {
			return nil, out, nil
		}
		payload := toolErrorOutput{Error: toolError{Code: errorCode(err), Message: err.Error()}}
		content, marshalErr := json.Marshal(payload)
		if marshalErr != nil {
			return nil, out, fmt.Errorf("marshal MCP tool error: %w", marshalErr)
		}
		result := &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: string(content)}},
		}
		result.SetError(err)
		return result, out, nil
	}
}

func (s *Server) status(ctx context.Context, _ statusInput) (statusOutput, error) {
	status, err := s.backend.Status(ctx)
	if err != nil {
		return statusOutput{}, err
	}
	out := statusOutput{
		Version:        status.Version,
		QueuedJobs:     status.QueuedJobs,
		RunningJobs:    status.RunningJobs,
		CompletedJobs:  status.CompletedJobs,
		FailedJobs:     status.FailedJobs,
		CanceledJobs:   status.CanceledJobs,
		AppliedJobs:    status.AppliedJobs,
		RebasedJobs:    status.RebasedJobs,
		SkippedJobs:    status.SkippedJobs,
		ActiveWorkers:  status.ActiveWorkers,
		MaxWorkers:     status.MaxWorkers,
		QueuePaused:    status.QueuePaused,
		UpdateDraining: status.UpdateDraining,
	}
	if status.MachineID != nil {
		out.MachineID = status.MachineID.String()
	}
	return out, nil
}

func (s *Server) listRepos(ctx context.Context, in listReposInput) (listReposOutput, error) {
	repos, err := s.backend.ListRepos(ctx, ReposQuery{
		Prefix: strings.TrimSpace(in.Prefix),
		Branch: strings.TrimSpace(in.Branch),
	})
	if err != nil {
		return listReposOutput{}, err
	}
	if repos == nil {
		repos = []storage.RepoWithCount{}
	}
	return listReposOutput{Repos: repos}, nil
}

func (s *Server) listBranches(ctx context.Context, in listBranchesInput) (listBranchesOutput, error) {
	repoPath := strings.TrimSpace(in.RepoPath)
	if repoPath == "" {
		return listBranchesOutput{}, NewError(ErrorCodeInvalidArgument, "repo_path is required")
	}
	branches, err := s.backend.ListBranches(ctx, repoPath)
	if err != nil {
		return listBranchesOutput{}, err
	}
	if branches == nil {
		branches = []storage.BranchWithCount{}
	}
	return listBranchesOutput{Branches: branches}, nil
}

func (s *Server) listJobs(ctx context.Context, in listJobsInput) (listJobsOutput, error) {
	limit := in.Limit
	switch {
	case limit <= 0:
		limit = defaultJobLimit
	case limit > maxJobLimit:
		limit = maxJobLimit
	}
	page, err := s.backend.ListJobs(ctx, JobsQuery{
		RepoPath: strings.TrimSpace(in.RepoPath),
		Branch:   strings.TrimSpace(in.Branch),
		Status:   strings.TrimSpace(in.Status),
		JobType:  strings.TrimSpace(in.JobType),
		Closed:   in.Closed,
		Limit:    limit,
		Cursor:   strings.TrimSpace(in.Cursor),
	})
	if err != nil {
		return listJobsOutput{}, err
	}
	out := listJobsOutput{
		Jobs:       make([]jobRow, 0, len(page.Jobs)),
		HasMore:    page.HasMore,
		NextCursor: page.NextCursor,
	}
	for i := range page.Jobs {
		out.Jobs = append(out.Jobs, newJobRow(&page.Jobs[i]))
	}
	return out, nil
}

func (s *Server) getReview(ctx context.Context, in reviewRefInput) (reviewOutput, error) {
	ref, err := in.ref()
	if err != nil {
		return reviewOutput{}, err
	}
	review, err := s.backend.GetReview(ctx, ref)
	if err != nil {
		return reviewOutput{}, err
	}
	out := reviewOutput{
		WebURL:    review.WebURL,
		ID:        review.ID,
		JobID:     review.JobID,
		Agent:     review.Agent,
		Closed:    review.Closed,
		CreatedAt: review.CreatedAt,
		Output:    review.Output,
	}
	if review.Job != nil {
		row := newJobRow(review.Job)
		out.Job = &row
		out.Verdict = row.Verdict
		out.FindingCounts = row.FindingCounts
	}
	if out.FindingCounts == nil {
		out.FindingCounts = findingCountsFromStructured(review.StructuredOutput)
	}
	if out.Verdict == "" && review.VerdictBool != nil {
		if *review.VerdictBool == 1 {
			out.Verdict = "pass"
		} else {
			out.Verdict = "fail"
		}
	}
	return out, nil
}

func (s *Server) listComments(ctx context.Context, in reviewRefInput) (listCommentsOutput, error) {
	ref, err := in.ref()
	if err != nil {
		return listCommentsOutput{}, err
	}
	responses, err := s.allComments(ctx, ref)
	if err != nil {
		return listCommentsOutput{}, err
	}
	out := listCommentsOutput{Comments: make([]commentRow, 0, len(responses))}
	for _, r := range responses {
		out.Comments = append(out.Comments, commentRow{
			ID:        r.ID,
			Responder: r.Responder,
			Response:  r.Response,
			Source:    r.Source,
			CreatedAt: r.CreatedAt,
		})
	}
	return out, nil
}

func (s *Server) getJobOutput(ctx context.Context, in jobOutputInput) (jobOutputResult, error) {
	if in.JobID <= 0 {
		return jobOutputResult{}, NewError(ErrorCodeInvalidArgument, "job_id is required")
	}
	output, err := s.backend.GetJobOutput(ctx, in.JobID)
	if err != nil {
		return jobOutputResult{}, err
	}
	lines := output.Lines
	total := len(lines)
	keep := maxOutputLines
	if in.Tail > 0 && in.Tail < keep {
		keep = in.Tail
	}
	truncated := false
	if len(lines) > keep {
		lines = lines[len(lines)-keep:]
		truncated = true
	}
	if lines == nil {
		lines = []OutputLine{}
	}
	return jobOutputResult{
		JobID:          output.JobID,
		Status:         output.Status,
		Lines:          lines,
		AvailableLines: total,
		Truncated:      truncated,
		HasMore:        output.HasMore,
	}, nil
}

// allComments merges job-linked comments with legacy commit-linked comments
// for the same review job, so callers see the full conversation regardless
// of which linkage each comment used. Linkage comes from the job row rather
// than the review, so an archived review awaiting conversion still yields
// its comments. Backend failures propagate; only a missing job falls back to
// the direct lookup, where legacy commit comments may still exist.
func (s *Server) allComments(ctx context.Context, ref ReviewRef) ([]storage.Response, error) {
	job, err := s.resolveJob(ctx, ref)
	if err != nil {
		return nil, err
	}
	if job == nil {
		return s.backend.ListComments(ctx, CommentRef{JobID: ref.JobID, SHA: ref.SHA})
	}
	responses, err := s.backend.ListComments(ctx, CommentRef{JobID: job.ID})
	if err != nil {
		return nil, err
	}
	commitID, sha := job.LegacyCommentLookupTarget()
	if commitID == 0 && sha == "" {
		return responses, nil
	}
	legacy, err := s.backend.ListComments(ctx, CommentRef{CommitID: commitID, SHA: sha})
	if err != nil {
		if errorCode(err) == ErrorCodeNotFound {
			return responses, nil
		}
		return nil, err
	}
	return storage.MergeResponses(responses, legacy), nil
}

// resolveJob finds the job a review reference points at: the job itself by
// ID, or the newest canonical review job for a commit SHA. Fix, task, and
// panel member jobs can share a reviewed SHA and are skipped, matching the
// storage layer's SHA review lookup. It returns nil without error when no
// job matches.
func (s *Server) resolveJob(ctx context.Context, ref ReviewRef) (*storage.ReviewJob, error) {
	if ref.JobID > 0 {
		page, err := s.backend.ListJobs(ctx, JobsQuery{ID: ref.JobID, Limit: 1})
		if err != nil {
			return nil, err
		}
		if len(page.Jobs) == 0 {
			return nil, nil
		}
		return &page.Jobs[0], nil
	}
	query := JobsQuery{GitRef: ref.SHA, Limit: defaultJobLimit}
	for {
		page, err := s.backend.ListJobs(ctx, query)
		if err != nil {
			return nil, err
		}
		for i := range page.Jobs {
			if isCanonicalReviewJob(&page.Jobs[i]) {
				return &page.Jobs[i], nil
			}
		}
		if !page.HasMore || page.NextCursor == "" {
			return nil, nil
		}
		query.Cursor = page.NextCursor
	}
}

// isCanonicalReviewJob follows ReviewJob's review classification, including
// its legacy empty-type fallback, while also accepting compact and synthesis
// jobs that produce canonical review documents.
func isCanonicalReviewJob(job *storage.ReviewJob) bool {
	if job.PanelRole == storage.PanelRoleMember {
		return false
	}
	return job.IsReviewJob() || job.JobType == storage.JobTypeCompact ||
		job.IsSynthesisJob()
}

// findingCountsFromStructured derives severity counts from the stored
// structured review document when the job projection did not carry them.
func findingCountsFromStructured(structured storage.StructuredOutput) *storage.FindingCounts {
	if len(structured) == 0 {
		return nil
	}
	raw, err := json.Marshal(structured)
	if err != nil {
		return nil
	}
	text := string(raw)
	return storage.ReviewFindingCounts(&text)
}

func (in reviewRefInput) ref() (ReviewRef, error) {
	sha := strings.TrimSpace(in.SHA)
	if in.JobID <= 0 && sha == "" {
		return ReviewRef{}, NewError(ErrorCodeInvalidArgument, "job_id or sha is required")
	}
	return ReviewRef{JobID: in.JobID, SHA: sha}, nil
}

func newJobRow(job *storage.ReviewJob) jobRow {
	row := jobRow{
		WebURL:        job.WebURL,
		ID:            job.ID,
		RepoPath:      job.RepoPath,
		RepoName:      job.RepoName,
		GitRef:        job.GitRef,
		Branch:        job.Branch,
		CommitSubject: job.CommitSubject,
		Agent:         job.Agent,
		Model:         job.Model,
		JobType:       job.JobType,
		ReviewType:    job.ReviewType,
		Status:        string(job.Status),
		Verdict:       verdictLabel(job.Verdict),
		Closed:        job.Closed,
		FindingCounts: job.FindingCounts,
		Error:         job.Error,
		EnqueuedAt:    job.EnqueuedAt,
		StartedAt:     job.StartedAt,
		FinishedAt:    job.FinishedAt,
		ParentJobID:   job.ParentJobID,
	}
	if job.UUID != nil {
		row.UUID = job.UUID.String()
	}
	return row
}

func verdictLabel(verdict *string) string {
	if verdict == nil {
		return ""
	}
	switch storage.Verdict(*verdict) {
	case storage.VerdictPass:
		return "pass"
	case storage.VerdictFail:
		return "fail"
	default:
		return *verdict
	}
}
