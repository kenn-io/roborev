package daemon

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"uuid"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/roborev/internal/mcpserver"
	"go.kenn.io/roborev/internal/searchindex"
	"go.kenn.io/roborev/internal/storage"
)

// mcpBackend serves MCP tools from the daemon's own handlers so the HTTP
// transport mounted on the API listener shares one database connection and
// one set of read semantics with the CLI-facing API.
type mcpBackend struct {
	server *Server
}

func (s *Server) mcpBackend() mcpserver.Backend {
	return mcpBackend{server: s}
}

func (b mcpBackend) Status(ctx context.Context) (*storage.DaemonStatus, error) {
	out, err := b.server.humaGetStatus(ctx, &GetStatusInput{})
	if err != nil {
		return nil, mcpError(err)
	}
	return &out.Body, nil
}

func (b mcpBackend) ListRepos(ctx context.Context, q mcpserver.ReposQuery) ([]storage.RepoWithCount, error) {
	out, err := b.server.humaListRepos(ctx, &ListReposInput{Prefix: q.Prefix, Branch: q.Branch})
	if err != nil {
		return nil, mcpError(err)
	}
	return out.Body.Repos, nil
}

func (b mcpBackend) ListBranches(ctx context.Context, repoPath string) ([]storage.BranchWithCount, error) {
	out, err := b.server.humaListBranches(ctx, &ListBranchesInput{Repo: []string{repoPath}})
	if err != nil {
		return nil, mcpError(err)
	}
	return out.Body.Branches, nil
}

func (b mcpBackend) ListJobs(ctx context.Context, q mcpserver.JobsQuery) (mcpserver.JobsPage, error) {
	// Mirror the query defaults Huma applies when a parameter is absent so the
	// in-process path lists exactly like GET /api/jobs.
	input := &ListJobsInput{
		ID:              -1,
		GitRef:          q.GitRef,
		Status:          q.Status,
		Branch:          q.Branch,
		JobType:         q.JobType,
		OmitPrompt:      "true",
		IncludeFindings: "true",
		Limit:           limitNotProvided,
		Offset:          -1,
		Before:          -1,
		Cursor:          q.Cursor,
	}
	if q.ID > 0 {
		input.ID = q.ID
	}
	if q.RepoPath != "" {
		input.Repo = []string{q.RepoPath}
	}
	if q.Closed != nil {
		input.Closed = strconv.FormatBool(*q.Closed)
	}
	if q.Limit > 0 {
		input.Limit = q.Limit
	}
	out, err := b.server.humaListJobs(ctx, input)
	if err != nil {
		return mcpserver.JobsPage{}, mcpError(err)
	}
	page := mcpserver.JobsPage{Jobs: out.Body.Jobs, HasMore: out.Body.HasMore}
	if out.Body.NextCursor != nil {
		page.NextCursor = *out.Body.NextCursor
	}
	return page, nil
}

func (b mcpBackend) GetReview(ctx context.Context, ref mcpserver.ReviewRef) (*storage.Review, error) {
	input := &GetReviewInput{JobID: -1, SHA: ref.SHA}
	if ref.JobID > 0 {
		input.JobID = ref.JobID
		input.SHA = ""
	}
	out, err := b.server.humaGetReview(ctx, input)
	if err != nil {
		return nil, mcpError(err)
	}
	return out.Body, nil
}

func (b mcpBackend) ListComments(ctx context.Context, ref mcpserver.CommentRef) ([]storage.Response, error) {
	input := &ListCommentsInput{JobID: -1, CommitID: -1, SHA: ref.SHA}
	switch {
	case ref.JobID > 0:
		input.JobID = ref.JobID
		input.SHA = ""
	case ref.CommitID > 0:
		input.CommitID = ref.CommitID
		input.SHA = ""
	}
	out, err := b.server.humaListComments(ctx, input)
	if err != nil {
		return nil, mcpError(err)
	}
	return out.Body.Responses, nil
}

func (b mcpBackend) GetJobOutput(_ context.Context, jobID int64) (mcpserver.JobOutput, error) {
	snapshot, err := b.server.jobOutputSnapshot(jobID)
	if err != nil {
		return mcpserver.JobOutput{}, mcpError(err)
	}
	lines := make([]mcpserver.OutputLine, 0, len(snapshot.Lines))
	for _, line := range snapshot.Lines {
		lines = append(lines, mcpserver.OutputLine{
			Timestamp: line.Timestamp,
			Text:      line.Text,
			Type:      line.Type,
		})
	}
	return mcpserver.JobOutput{
		JobID:   snapshot.JobID,
		Status:  snapshot.Status,
		Lines:   lines,
		HasMore: snapshot.HasMore,
	}, nil
}

func (b mcpBackend) Search(
	ctx context.Context, query mcpserver.SearchQuery,
) (storage.SearchResponse, error) {
	if b.server.search == nil {
		return storage.SearchResponse{}, mcpserver.NewError(
			mcpserver.ErrorCodeUnavailable, "review search is unavailable")
	}
	input := &SearchInput{
		Query: query.Query, Mode: query.Mode, Repo: query.Repo, Branch: query.Branch,
		Since: query.Since, Verdict: query.Verdict, State: query.State,
		Limit: searchLimit(strconv.Itoa(query.Limit)),
	}
	params, err := b.server.searchParams(input)
	if err != nil {
		message := "search request is invalid"
		if errors.Is(err, errSearchRepoNotFound) {
			message = errSearchRepoNotFound.Error()
		}
		return storage.SearchResponse{}, mcpserver.NewError(mcpserver.ErrorCodeInvalidArgument, message)
	}
	result, err := b.server.search.Search(ctx, params)
	if err != nil {
		if modeErr, ok := errors.AsType[*searchindex.ModeError](err); ok {
			switch modeErr.Reason {
			case searchindex.ReasonEmbeddingsUnconfigured:
				return storage.SearchResponse{}, mcpserver.NewError(
					mcpserver.ErrorCodeInvalidArgument, modeErr.Reason)
			case searchindex.ReasonSemanticUnavailable, searchindex.ReasonSemanticCeiling:
				return storage.SearchResponse{}, mcpserver.NewError(
					mcpserver.ErrorCodeUnavailable, modeErr.Reason)
			}
		}
		return storage.SearchResponse{}, mcpserver.NewError(
			mcpserver.ErrorCodeInternal, "review search failed")
	}
	response := searchResponseFromResult(result)
	if response.Hits == nil {
		response.Hits = []storage.SearchHit{}
	}
	return response, nil
}

// mcpError maps Huma status errors onto the stable MCP error codes.
func mcpError(err error) error {
	statusErr, ok := errors.AsType[huma.StatusError](err)
	if !ok {
		return mcpserver.NewError(mcpserver.ErrorCodeInternal, err.Error())
	}
	message := statusErr.Error()
	if model, ok := errors.AsType[*huma.ErrorModel](err); ok && model.Detail != "" {
		message = model.Detail
	}
	switch statusErr.GetStatus() {
	case http.StatusNotFound:
		return mcpserver.NewError(mcpserver.ErrorCodeNotFound, message)
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return mcpserver.NewError(mcpserver.ErrorCodeInvalidArgument, message)
	case http.StatusServiceUnavailable, http.StatusConflict:
		return mcpserver.NewError(mcpserver.ErrorCodeUnavailable, message)
	default:
		return mcpserver.NewError(mcpserver.ErrorCodeInternal, message)
	}
}

func (b mcpBackend) AddComment(ctx context.Context, in mcpserver.AddCommentInput) (*storage.Response, error) {
	out, err := b.server.humaAddComment(ctx, &AddCommentInput{Body: AddCommentRequest{JobID: in.JobID, Commenter: in.Commenter, Comment: in.Comment}})
	if err != nil {
		return nil, mcpError(err)
	}
	return out.Body, nil
}

func (b mcpBackend) CloseReview(ctx context.Context, jobID int64) error {
	_, err := b.server.humaCloseReview(ctx, &CloseReviewInput{Body: CloseReviewRequest{JobID: jobID, Closed: true}})
	if err != nil {
		return mcpError(err)
	}
	return nil
}

func (b mcpBackend) Snooze(ctx context.Context, in mcpserver.SnoozeInput) (mcpserver.SnoozeOutput, error) {
	out, err := b.server.humaSetAgentHookSnooze(ctx, &AgentHookSnoozeInput{Body: AgentHookSnoozeRequest{RepoPath: in.RepoPath, WorktreePath: in.WorktreePath, Branch: in.Branch, Enabled: in.Enabled, SnoozedUntil: in.SnoozedUntil}})
	if err != nil {
		return mcpserver.SnoozeOutput{}, mcpError(err)
	}
	return mcpserver.SnoozeOutput{Snoozed: out.Body.Snoozed, SnoozedUntil: out.Body.SnoozedUntil}, nil
}

func (b mcpBackend) CompleteFix(ctx context.Context, id uuid.UUID) error {
	_, err := b.server.humaAgentHookFixDone(ctx, &AgentHookFixDoneInput{Body: AgentHookFixDoneRequest{FixSessionID: id}})
	if err != nil {
		return mcpError(err)
	}
	return nil
}
