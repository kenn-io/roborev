package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"go.kenn.io/roborev/internal/storage"
)

const maxErrorBodyBytes = 64 << 10

// HTTPBackend reads roborev data from a running daemon over its HTTP API.
// The stdio transport uses it so a separate MCP process never opens the
// daemon's database directly.
type HTTPBackend struct {
	baseURL string
	client  *http.Client
}

// NewHTTPBackend targets the daemon at baseURL using client.
func NewHTTPBackend(baseURL string, client *http.Client) *HTTPBackend {
	if client == nil {
		client = http.DefaultClient
	}
	return &HTTPBackend{baseURL: strings.TrimRight(baseURL, "/"), client: client}
}

func (b *HTTPBackend) Status(ctx context.Context) (*storage.DaemonStatus, error) {
	var status storage.DaemonStatus
	if err := b.getJSON(ctx, "/api/status", nil, &status); err != nil {
		return nil, err
	}
	return &status, nil
}

func (b *HTTPBackend) ListRepos(ctx context.Context, q ReposQuery) ([]storage.RepoWithCount, error) {
	params := url.Values{}
	if q.Prefix != "" {
		params.Set("prefix", q.Prefix)
	}
	if q.Branch != "" {
		params.Set("branch", q.Branch)
	}
	var body struct {
		Repos []storage.RepoWithCount `json:"repos"`
	}
	if err := b.getJSON(ctx, "/api/repos", params, &body); err != nil {
		return nil, err
	}
	return body.Repos, nil
}

func (b *HTTPBackend) ListBranches(ctx context.Context, repoPath string) ([]storage.BranchWithCount, error) {
	params := url.Values{"repo": {repoPath}}
	var body struct {
		Branches []storage.BranchWithCount `json:"branches"`
	}
	if err := b.getJSON(ctx, "/api/branches", params, &body); err != nil {
		return nil, err
	}
	return body.Branches, nil
}

func (b *HTTPBackend) ListJobs(ctx context.Context, q JobsQuery) (JobsPage, error) {
	params := url.Values{}
	params.Set("omit_prompt", "true")
	params.Set("include_findings", "true")
	if q.ID > 0 {
		params.Set("id", strconv.FormatInt(q.ID, 10))
	}
	if q.GitRef != "" {
		params.Set("git_ref", q.GitRef)
	}
	if q.RepoPath != "" {
		params.Set("repo", q.RepoPath)
	}
	if q.Branch != "" {
		params.Set("branch", q.Branch)
	}
	if q.Status != "" {
		params.Set("status", q.Status)
	}
	if q.JobType != "" {
		params.Set("job_type", q.JobType)
	}
	if q.Closed != nil {
		params.Set("closed", strconv.FormatBool(*q.Closed))
	}
	if q.Limit > 0 {
		params.Set("limit", strconv.Itoa(q.Limit))
	}
	if q.Cursor != "" {
		params.Set("cursor", q.Cursor)
	}
	var body struct {
		Jobs       []storage.ReviewJob `json:"jobs"`
		HasMore    bool                `json:"has_more"`
		NextCursor *string             `json:"next_cursor"`
	}
	if err := b.getJSON(ctx, "/api/jobs", params, &body); err != nil {
		return JobsPage{}, err
	}
	page := JobsPage{Jobs: body.Jobs, HasMore: body.HasMore}
	if body.NextCursor != nil {
		page.NextCursor = *body.NextCursor
	}
	return page, nil
}

func (b *HTTPBackend) GetReview(ctx context.Context, ref ReviewRef) (*storage.Review, error) {
	params := url.Values{}
	if ref.JobID > 0 {
		params.Set("job_id", strconv.FormatInt(ref.JobID, 10))
	} else if ref.SHA != "" {
		params.Set("sha", ref.SHA)
	}
	var review storage.Review
	if err := b.getJSON(ctx, "/api/review", params, &review); err != nil {
		return nil, err
	}
	return &review, nil
}

func (b *HTTPBackend) ListComments(ctx context.Context, ref CommentRef) ([]storage.Response, error) {
	var body struct {
		Responses []storage.Response `json:"responses"`
	}
	if err := b.getJSON(ctx, "/api/comments", commentRefParams(ref), &body); err != nil {
		return nil, err
	}
	return body.Responses, nil
}

func (b *HTTPBackend) GetJobOutput(ctx context.Context, jobID int64) (JobOutput, error) {
	params := url.Values{"job_id": {strconv.FormatInt(jobID, 10)}}
	var output JobOutput
	if err := b.getJSON(ctx, "/api/job/output", params, &output); err != nil {
		return JobOutput{}, err
	}
	return output, nil
}

func commentRefParams(ref CommentRef) url.Values {
	params := url.Values{}
	switch {
	case ref.JobID > 0:
		params.Set("job_id", strconv.FormatInt(ref.JobID, 10))
	case ref.CommitID > 0:
		params.Set("commit_id", strconv.FormatInt(ref.CommitID, 10))
	case ref.SHA != "":
		params.Set("sha", ref.SHA)
	}
	return params
}

func (b *HTTPBackend) getJSON(ctx context.Context, path string, params url.Values, out any) error {
	target := b.baseURL + path
	if len(params) > 0 {
		target += "?" + params.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return NewError(ErrorCodeInternal, fmt.Sprintf("build request for %s: %v", path, err))
	}
	req.Header.Set("Accept", "application/json")
	resp, err := b.client.Do(req)
	if err != nil {
		return NewError(ErrorCodeUnavailable, fmt.Sprintf("roborev daemon request failed: %v", err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return httpStatusError(resp)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return NewError(ErrorCodeInternal, fmt.Sprintf("decode %s response: %v", path, err))
	}
	return nil
}

func httpStatusError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
	message := strings.TrimSpace(string(body))
	// Huma problem responses carry "detail"; legacy handlers carry "error".
	var parsed struct {
		Detail string `json:"detail"`
		Error  string `json:"error"`
	}
	if err := json.Unmarshal(body, &parsed); err == nil {
		switch {
		case parsed.Detail != "":
			message = parsed.Detail
		case parsed.Error != "":
			message = parsed.Error
		}
	}
	if message == "" {
		message = resp.Status
	}
	code := ErrorCodeInternal
	switch resp.StatusCode {
	case http.StatusNotFound:
		code = ErrorCodeNotFound
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		code = ErrorCodeInvalidArgument
	case http.StatusServiceUnavailable, http.StatusBadGateway, http.StatusGatewayTimeout:
		code = ErrorCodeUnavailable
	}
	return NewError(code, message)
}
