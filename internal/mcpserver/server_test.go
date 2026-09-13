package mcpserver

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
)

type fakeBackend struct {
	statusFn       func(context.Context) (*storage.DaemonStatus, error)
	listReposFn    func(context.Context, ReposQuery) ([]storage.RepoWithCount, error)
	listBranchesFn func(context.Context, string) ([]storage.BranchWithCount, error)
	listJobsFn     func(context.Context, JobsQuery) (JobsPage, error)
	getReviewFn    func(context.Context, ReviewRef) (*storage.Review, error)
	listCommentsFn func(context.Context, CommentRef) ([]storage.Response, error)
	getJobOutputFn func(context.Context, int64) (JobOutput, error)
}

func (f *fakeBackend) Status(ctx context.Context) (*storage.DaemonStatus, error) {
	if f.statusFn == nil {
		return &storage.DaemonStatus{}, nil
	}
	return f.statusFn(ctx)
}

func (f *fakeBackend) ListRepos(ctx context.Context, q ReposQuery) ([]storage.RepoWithCount, error) {
	if f.listReposFn == nil {
		return nil, nil
	}
	return f.listReposFn(ctx, q)
}

func (f *fakeBackend) ListBranches(ctx context.Context, repoPath string) ([]storage.BranchWithCount, error) {
	if f.listBranchesFn == nil {
		return nil, nil
	}
	return f.listBranchesFn(ctx, repoPath)
}

func (f *fakeBackend) ListJobs(ctx context.Context, q JobsQuery) (JobsPage, error) {
	if f.listJobsFn == nil {
		return JobsPage{}, nil
	}
	return f.listJobsFn(ctx, q)
}

func (f *fakeBackend) GetReview(ctx context.Context, ref ReviewRef) (*storage.Review, error) {
	if f.getReviewFn == nil {
		return nil, NewError(ErrorCodeNotFound, "review not found")
	}
	return f.getReviewFn(ctx, ref)
}

func (f *fakeBackend) ListComments(ctx context.Context, ref CommentRef) ([]storage.Response, error) {
	if f.listCommentsFn == nil {
		return nil, nil
	}
	return f.listCommentsFn(ctx, ref)
}

func (f *fakeBackend) GetJobOutput(ctx context.Context, jobID int64) (JobOutput, error) {
	if f.getJobOutputFn == nil {
		return JobOutput{}, nil
	}
	return f.getJobOutputFn(ctx, jobID)
}

// connectSession runs the server over an in-memory transport and returns a
// connected client session.
func connectSession(t *testing.T, backend Backend) *mcp.ClientSession {
	t.Helper()
	server := New(backend, "test")
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx, serverTransport) }()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = session.Close()
		cancel()
		<-done
	})
	return session
}

func callTool(t *testing.T, session *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: args})
	require.NoError(t, err)
	return result
}

func decodeText(t *testing.T, result *mcp.CallToolResult, out any) {
	t.Helper()
	require.NotEmpty(t, result.Content)
	text, ok := result.Content[0].(*mcp.TextContent)
	require.True(t, ok, "expected text content, got %T", result.Content[0])
	require.NoError(t, json.Unmarshal([]byte(text.Text), out))
}

func TestRegisteredToolsAndGuidanceAreReadOnly(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	session := connectSession(t, &fakeBackend{})

	tools, err := session.ListTools(t.Context(), nil)
	require.NoError(err)
	var names []string
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
	}
	slices.Sort(names)
	assert.Equal([]string{
		"roborev_get_job_output",
		"roborev_get_review",
		"roborev_list_branches",
		"roborev_list_comments",
		"roborev_list_jobs",
		"roborev_list_repos",
		"roborev_status",
	}, names)

	resources, err := session.ListResources(t.Context(), nil)
	require.NoError(err)
	require.Len(resources.Resources, 1)
	assert.Equal(guidanceResourceURI, resources.Resources[0].URI)
	read, err := session.ReadResource(t.Context(), &mcp.ReadResourceParams{URI: guidanceResourceURI})
	require.NoError(err)
	require.Len(read.Contents, 1)
	assert.Contains(read.Contents[0].Text, "roborev_get_review")

	_, err = session.ReadResource(t.Context(), &mcp.ReadResourceParams{URI: "roborev://mcp/missing"})
	assert.Error(err)
}

func TestListJobsAppliesLimitsAndTrimsRows(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	closed := true
	verdict := "F"
	var got JobsQuery
	backend := &fakeBackend{listJobsFn: func(_ context.Context, q JobsQuery) (JobsPage, error) {
		got = q
		next := "cursor-2"
		return JobsPage{
			Jobs: []storage.ReviewJob{{
				ID:            7,
				RepoPath:      "/repo",
				GitRef:        "abc123",
				Branch:        "main",
				Agent:         "codex",
				JobType:       storage.JobTypeReview,
				Status:        storage.JobStatusDone,
				Prompt:        "must not leak",
				Verdict:       &verdict,
				Closed:        &closed,
				FindingCounts: &storage.FindingCounts{High: 1},
				EnqueuedAt:    time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
			}},
			HasMore:    true,
			NextCursor: next,
		}, nil
	}}
	session := connectSession(t, backend)

	result := callTool(t, session, "roborev_list_jobs", map[string]any{
		"repo_path": " /repo ", "branch": "main", "closed": false, "limit": 5000,
	})
	require.False(result.IsError)
	assert.Equal("/repo", got.RepoPath)
	assert.Equal("main", got.Branch)
	require.NotNil(got.Closed)
	assert.False(*got.Closed)
	assert.Equal(maxJobLimit, got.Limit)

	var out listJobsOutput
	decodeText(t, result, &out)
	require.Len(out.Jobs, 1)
	row := out.Jobs[0]
	assert.Equal(int64(7), row.ID)
	assert.Equal("fail", row.Verdict)
	assert.Equal("done", row.Status)
	require.NotNil(row.FindingCounts)
	assert.Equal(1, row.FindingCounts.High)
	assert.True(out.HasMore)
	assert.Equal("cursor-2", out.NextCursor)

	text := result.Content[0].(*mcp.TextContent).Text
	assert.NotContains(text, "must not leak")
	assert.NotContains(text, "prompt")

	callTool(t, session, "roborev_list_jobs", map[string]any{})
	assert.Equal(defaultJobLimit, got.Limit)
}

func TestGetReviewDerivesVerdictAndRequiresRef(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var got ReviewRef
	passed := 1
	backend := &fakeBackend{getReviewFn: func(_ context.Context, ref ReviewRef) (*storage.Review, error) {
		got = ref
		return &storage.Review{
			ID: 3, JobID: 9, Agent: "claude-code", Output: "LGTM",
			Prompt: "hidden prompt", VerdictBool: &passed,
		}, nil
	}}
	session := connectSession(t, backend)

	result := callTool(t, session, "roborev_get_review", map[string]any{"job_id": 9})
	require.False(result.IsError)
	assert.Equal(ReviewRef{JobID: 9}, got)
	var out reviewOutput
	decodeText(t, result, &out)
	assert.Equal(int64(9), out.JobID)
	assert.Equal("pass", out.Verdict)
	assert.Equal("LGTM", out.Output)
	assert.NotContains(result.Content[0].(*mcp.TextContent).Text, "hidden prompt")

	callTool(t, session, "roborev_get_review", map[string]any{"sha": " deadbeef "})
	assert.Equal(ReviewRef{SHA: "deadbeef"}, got)

	result = callTool(t, session, "roborev_get_review", map[string]any{})
	require.True(result.IsError)
	var failure toolErrorOutput
	decodeText(t, result, &failure)
	assert.Equal(ErrorCodeInvalidArgument, failure.Error.Code)
}

func TestBackendErrorsCarryStableCodes(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	backend := &fakeBackend{listReposFn: func(context.Context, ReposQuery) ([]storage.RepoWithCount, error) {
		return nil, NewError(ErrorCodeUnavailable, "daemon is restarting")
	}}
	session := connectSession(t, backend)

	result := callTool(t, session, "roborev_list_repos", map[string]any{})
	require.True(result.IsError)
	var failure toolErrorOutput
	decodeText(t, result, &failure)
	assert.Equal(ErrorCodeUnavailable, failure.Error.Code)
	assert.Contains(failure.Error.Message, "daemon is restarting")
}

func TestListBranchesRequiresRepoPath(t *testing.T) {
	session := connectSession(t, &fakeBackend{})
	result := callTool(t, session, "roborev_list_branches", map[string]any{"repo_path": "  "})
	require.True(t, result.IsError)
	var failure toolErrorOutput
	decodeText(t, result, &failure)
	assert.Equal(t, ErrorCodeInvalidArgument, failure.Error.Code)
}

func TestGetJobOutputTailsLines(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	backend := &fakeBackend{getJobOutputFn: func(_ context.Context, jobID int64) (JobOutput, error) {
		lines := make([]OutputLine, 0, 5)
		for i := range 5 {
			lines = append(lines, OutputLine{Text: string(rune('a' + i)), Type: "text"})
		}
		return JobOutput{JobID: jobID, Status: "running", Lines: lines, HasMore: true}, nil
	}}
	session := connectSession(t, backend)

	result := callTool(t, session, "roborev_get_job_output", map[string]any{"job_id": 4, "tail": 2})
	require.False(result.IsError)
	var out jobOutputResult
	decodeText(t, result, &out)
	assert.Equal(int64(4), out.JobID)
	assert.True(out.Truncated)
	assert.True(out.HasMore)
	require.Len(out.Lines, 2)
	assert.Equal("d", out.Lines[0].Text)
	assert.Equal("e", out.Lines[1].Text)

	result = callTool(t, session, "roborev_get_job_output", map[string]any{"job_id": 0})
	assert.True(result.IsError)
}

func TestGetReviewDerivesFindingCountsFromStructuredOutput(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	failed := 0
	backend := &fakeBackend{getReviewFn: func(context.Context, ReviewRef) (*storage.Review, error) {
		return &storage.Review{
			ID: 1, JobID: 2, Agent: "codex", Output: "findings", VerdictBool: &failed,
			StructuredOutput: storage.StructuredOutput{
				"schema_version": 2, "summary": "review", "verdict": "fail",
				"findings": []any{
					map[string]any{"severity": "high", "problem": "p", "fix": "f", "location": nil},
					map[string]any{"severity": "low", "problem": "p", "fix": "f", "location": nil},
					map[string]any{"severity": "low", "problem": "p", "fix": "f", "location": nil},
				},
			},
		}, nil
	}}
	session := connectSession(t, backend)

	result := callTool(t, session, "roborev_get_review", map[string]any{"job_id": 2})
	require.False(result.IsError)
	var out reviewOutput
	decodeText(t, result, &out)
	assert.Equal("fail", out.Verdict)
	require.NotNil(out.FindingCounts)
	assert.Equal(1, out.FindingCounts.High)
	assert.Equal(2, out.FindingCounts.Low)
}

func TestListCommentsMergesJobAndLegacyCommitComments(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	commitID := int64(77)
	var refs []CommentRef
	backend := &fakeBackend{
		listJobsFn: func(_ context.Context, q JobsQuery) (JobsPage, error) {
			switch {
			case q.GitRef == "boom":
				return JobsPage{}, NewError(ErrorCodeInternal, "database unavailable")
			case q.ID == 9:
				return JobsPage{Jobs: []storage.ReviewJob{{
					ID: 9, JobType: storage.JobTypeReview, GitRef: "abc123", CommitID: &commitID,
				}}}, nil
			case q.GitRef == "abc123" && q.Cursor == "":
				// Fill the first page with newer fix jobs; resolution must
				// follow the cursor to find the canonical review.
				jobs := make([]storage.ReviewJob, defaultJobLimit)
				for i := range jobs {
					jobs[i] = storage.ReviewJob{ID: int64(100 - i), JobType: storage.JobTypeFix, GitRef: "abc123"}
				}
				return JobsPage{Jobs: jobs, HasMore: true, NextCursor: "next"}, nil
			case q.GitRef == "abc123" && q.Cursor == "next":
				// An empty-type legacy task and panel member must not shadow
				// the canonical review job.
				return JobsPage{Jobs: []storage.ReviewJob{
					{ID: 12, GitRef: "abc123"},
					{ID: 10, JobType: storage.JobTypeReview, PanelRole: storage.PanelRoleMember, GitRef: "abc123", CommitID: &commitID},
					{ID: 9, JobType: storage.JobTypeReview, GitRef: "abc123", CommitID: &commitID},
				}}, nil
			}
			return JobsPage{}, nil
		},
		listCommentsFn: func(_ context.Context, ref CommentRef) ([]storage.Response, error) {
			refs = append(refs, ref)
			switch {
			case ref.JobID == 9:
				return []storage.Response{
					{ID: 2, Responder: "dev", Response: "job-linked", CreatedAt: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)},
				}, nil
			case ref.CommitID == 77:
				return []storage.Response{
					{ID: 1, Responder: "dev", Response: "legacy", CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
					{ID: 2, Responder: "dev", Response: "job-linked", CreatedAt: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)},
				}, nil
			case ref.SHA == "missing":
				return []storage.Response{{ID: 5, Responder: "dev", Response: "direct"}}, nil
			}
			return nil, nil
		},
	}
	session := connectSession(t, backend)

	result := callTool(t, session, "roborev_list_comments", map[string]any{"sha": "abc123"})
	require.False(result.IsError)
	var out listCommentsOutput
	decodeText(t, result, &out)
	require.Len(out.Comments, 2, "job-linked and legacy comments merged and deduplicated")
	assert.Equal("legacy", out.Comments[0].Response)
	assert.Equal("job-linked", out.Comments[1].Response)
	assert.Equal([]CommentRef{{JobID: 9}, {CommitID: 77}}, refs)

	// Job-ID lookups resolve the same linkage without needing the review row.
	refs = nil
	result = callTool(t, session, "roborev_list_comments", map[string]any{"job_id": 9})
	require.False(result.IsError)
	decodeText(t, result, &out)
	require.Len(out.Comments, 2)
	assert.Equal([]CommentRef{{JobID: 9}, {CommitID: 77}}, refs)

	// Without a matching job, the reference is looked up directly.
	result = callTool(t, session, "roborev_list_comments", map[string]any{"sha": "missing"})
	require.False(result.IsError)
	decodeText(t, result, &out)
	require.Len(out.Comments, 1)
	assert.Equal("direct", out.Comments[0].Response)

	// An unrelated backend failure is reported, not masked as a partial thread.
	result = callTool(t, session, "roborev_list_comments", map[string]any{"sha": "boom"})
	require.True(result.IsError)
	var failure toolErrorOutput
	decodeText(t, result, &failure)
	assert.Equal(ErrorCodeInternal, failure.Error.Code)
}

func TestGetJobOutputReportsAvailableLines(t *testing.T) {
	backend := &fakeBackend{getJobOutputFn: func(_ context.Context, jobID int64) (JobOutput, error) {
		lines := make([]OutputLine, maxOutputLines+5)
		return JobOutput{JobID: jobID, Status: "done", Lines: lines}, nil
	}}
	session := connectSession(t, backend)
	result := callTool(t, session, "roborev_get_job_output", map[string]any{"job_id": 1})
	require.False(t, result.IsError)
	var out jobOutputResult
	decodeText(t, result, &out)
	assert.Equal(t, maxOutputLines+5, out.AvailableLines)
	assert.Len(t, out.Lines, maxOutputLines)
	assert.True(t, out.Truncated)
}
