package mcpserver

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"uuid"

	"go.kenn.io/roborev/internal/storage"
	roborevclient "go.kenn.io/roborev/pkg/client"
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
	if err := b.getJSON(func(api *roborevclient.Client) (*http.Response, error) { return api.GetStatusRaw(ctx) }, &status); err != nil {
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
	if err := b.getJSON(func(api *roborevclient.Client) (*http.Response, error) {
		return api.ListReposRaw(ctx, nil, roborevclient.WithQuery(params))
	}, &body); err != nil {
		return nil, err
	}
	return body.Repos, nil
}

func (b *HTTPBackend) ListBranches(ctx context.Context, repoPath string) ([]storage.BranchWithCount, error) {
	params := url.Values{"repo": {repoPath}}
	var body struct {
		Branches []storage.BranchWithCount `json:"branches"`
	}
	if err := b.getJSON(func(api *roborevclient.Client) (*http.Response, error) {
		return api.ListBranchesRaw(ctx, nil, roborevclient.WithQuery(params))
	}, &body); err != nil {
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
	if err := b.getJSON(func(api *roborevclient.Client) (*http.Response, error) {
		return api.ListJobsRaw(ctx, nil, roborevclient.WithQuery(params))
	}, &body); err != nil {
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
	if err := b.getJSON(func(api *roborevclient.Client) (*http.Response, error) {
		return api.GetReviewRaw(ctx, nil, roborevclient.WithQuery(params))
	}, &review); err != nil {
		return nil, err
	}
	return &review, nil
}

func (b *HTTPBackend) ListComments(ctx context.Context, ref CommentRef) ([]storage.Response, error) {
	var body struct {
		Responses []storage.Response `json:"responses"`
	}
	if err := b.getJSON(func(api *roborevclient.Client) (*http.Response, error) {
		return api.ListCommentsRaw(ctx, nil, roborevclient.WithQuery(commentRefParams(ref)))
	}, &body); err != nil {
		return nil, err
	}
	return body.Responses, nil
}

func (b *HTTPBackend) GetJobOutput(ctx context.Context, jobID int64) (JobOutput, error) {
	params := url.Values{"job_id": {strconv.FormatInt(jobID, 10)}}
	var output JobOutput
	if err := b.getJSON(func(api *roborevclient.Client) (*http.Response, error) {
		return api.GetJobOutputRaw(ctx, nil, roborevclient.WithQuery(params))
	}, &output); err != nil {
		return JobOutput{}, err
	}
	return output, nil
}

// Search reads completed review history through the daemon's search endpoint.
func (b *HTTPBackend) Search(ctx context.Context, q SearchQuery) (storage.SearchResponse, error) {
	params := url.Values{"q": {q.Query}}
	if q.Mode != "" {
		params.Set("mode", q.Mode)
	}
	if q.Repo != "" {
		params.Set("repo", q.Repo)
	}
	if q.Branch != "" {
		params.Set("branch", q.Branch)
	}
	if q.Since != "" {
		params.Set("since", q.Since)
	}
	if q.Verdict != "" {
		params.Set("verdict", q.Verdict)
	}
	if q.State != "" {
		params.Set("state", q.State)
	}
	if q.Limit > 0 {
		params.Set("limit", strconv.Itoa(q.Limit))
	}

	api, err := roborevclient.NewWithHTTPClient(b.baseURL, b.client)
	if err != nil {
		return storage.SearchResponse{}, NewError(ErrorCodeInternal, "build review search request")
	}
	resp, err := api.SearchReviewsRaw(ctx, nil, roborevclient.WithQuery(params))
	if err != nil {
		return storage.SearchResponse{}, NewError(ErrorCodeUnavailable, "roborev daemon is unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return storage.SearchResponse{}, searchHTTPStatusError(resp, q.Mode)
	}

	var body jsontext.Value
	decoder := jsontext.NewDecoder(resp.Body)
	if err := json.UnmarshalDecode(decoder, &body); err != nil {
		return storage.SearchResponse{}, NewError(ErrorCodeInternal, "invalid response from roborev daemon")
	}
	var trailing any
	if err := json.UnmarshalDecode(decoder, &trailing); !errors.Is(err, io.EOF) {
		return storage.SearchResponse{}, NewError(ErrorCodeInternal, "invalid response from roborev daemon")
	}
	if !validSearchResponseJSON(body) {
		return storage.SearchResponse{}, NewError(ErrorCodeInternal, "invalid response from roborev daemon")
	}
	var result storage.SearchResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return storage.SearchResponse{}, NewError(ErrorCodeInternal, "invalid response from roborev daemon")
	}
	if result.Query == "" || result.Mode == "" {
		return storage.SearchResponse{}, NewError(ErrorCodeInternal, "invalid response from roborev daemon")
	}
	if result.Hits == nil {
		result.Hits = []storage.SearchHit{}
	}
	return result, nil
}

func validSearchResponseJSON(body jsontext.Value) bool {
	var response map[string]jsontext.Value
	if err := json.Unmarshal(body, &response); err != nil || response == nil {
		return false
	}
	if !hasNonNullJSONMembers(response,
		"query", "mode", "degraded", "bounded", "partial", "coverage",
	) {
		return false
	}
	hitsJSON, ok := response["hits"]
	if !ok {
		return false
	}

	var coverage map[string]jsontext.Value
	if err := json.Unmarshal(response["coverage"], &coverage); err != nil ||
		!hasNonNullJSONMembers(coverage,
			"mirror_complete", "embeddings_configured", "vector_state", "embedding_backlog", "skipped",
		) {
		return false
	}
	if bytes.Equal(bytes.TrimSpace(hitsJSON), []byte("null")) {
		return true
	}
	var hits []jsontext.Value
	if err := json.Unmarshal(hitsJSON, &hits); err != nil {
		return false
	}
	for _, hitJSON := range hits {
		var hit map[string]jsontext.Value
		if err := json.Unmarshal(hitJSON, &hit); err != nil ||
			!hasNonNullJSONMembers(hit,
				"job_id", "review_id", "repo_name", "repo_path", "git_ref", "review_type", "agent",
				"closed", "finished_at", "score", "matched_in", "excerpt",
			) {
			return false
		}
	}
	return true
}

func hasNonNullJSONMembers(object map[string]jsontext.Value, members ...string) bool {
	if object == nil {
		return false
	}
	for _, member := range members {
		value, ok := object[member]
		if !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return false
		}
	}
	return true
}

func searchHTTPStatusError(resp *http.Response, mode string) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
	var problem struct {
		Detail string `json:"detail"`
		Error  string `json:"error"`
	}
	_ = json.Unmarshal(body, &problem)
	reason := problem.Detail
	if reason == "" {
		reason = problem.Error
	}
	if mode == "semantic" || mode == "hybrid" {
		switch reason {
		case "embeddings are not configured":
			return NewError(ErrorCodeInvalidArgument, reason)
		case "semantic search is unavailable", "semantic candidate ceiling exhausted":
			return NewError(ErrorCodeUnavailable, reason)
		}
	}

	switch resp.StatusCode {
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return NewError(ErrorCodeInvalidArgument, "search request is invalid")
	case http.StatusBadGateway, http.StatusGatewayTimeout:
		return NewError(ErrorCodeUnavailable, "roborev daemon is unavailable")
	case http.StatusServiceUnavailable:
		return NewError(ErrorCodeUnavailable, "review search is unavailable")
	default:
		if mode == "semantic" || mode == "hybrid" {
			return NewError(ErrorCodeUnavailable, "review search is unavailable")
		}
		return NewError(ErrorCodeInternal, "review search failed")
	}
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

func (b *HTTPBackend) getJSON(call func(*roborevclient.Client) (*http.Response, error), out any) error {
	api, err := roborevclient.NewWithHTTPClient(b.baseURL, b.client)
	if err != nil {
		return NewError(ErrorCodeInternal, fmt.Sprintf("build daemon client: %v", err))
	}
	resp, err := call(api)
	if err != nil {
		return NewError(ErrorCodeUnavailable, fmt.Sprintf("roborev daemon request failed: %v", err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return httpStatusError(resp)
	}
	if err := json.UnmarshalRead(resp.Body, out); err != nil {
		return NewError(ErrorCodeInternal, fmt.Sprintf("decode daemon response: %v", err))
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

func (b *HTTPBackend) AddComment(ctx context.Context, in AddCommentInput) (*storage.Response, error) {
	body, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	var out storage.Response
	err = b.getJSON(func(api *roborevclient.Client) (*http.Response, error) {
		return api.AddCommentRaw(ctx, nil, roborevclient.WithBody(body))
	}, &out)
	return &out, err
}

func (b *HTTPBackend) CloseReview(ctx context.Context, jobID int64) error {
	body, err := json.Marshal(struct {
		JobID  int64 `json:"job_id"`
		Closed bool  `json:"closed"`
	}{jobID, true})
	if err != nil {
		return err
	}
	var out successOutput
	return b.getJSON(func(api *roborevclient.Client) (*http.Response, error) {
		return api.CloseReviewRaw(ctx, nil, roborevclient.WithBody(body))
	}, &out)
}

func (b *HTTPBackend) Snooze(ctx context.Context, in SnoozeInput) (SnoozeOutput, error) {
	body, err := json.Marshal(in)
	if err != nil {
		return SnoozeOutput{}, err
	}
	var out SnoozeOutput
	err = b.getJSON(func(api *roborevclient.Client) (*http.Response, error) {
		return api.SetAgentHookSnoozeRaw(ctx, nil, roborevclient.WithBody(body))
	}, &out)
	return out, err
}

func (b *HTTPBackend) CompleteFix(ctx context.Context, id uuid.UUID) error {
	body, err := json.Marshal(completeFixInput{FixSessionID: id})
	if err != nil {
		return err
	}
	var out struct {
		OK bool `json:"ok"`
	}
	return b.getJSON(func(api *roborevclient.Client) (*http.Response, error) {
		return api.CompleteAgentHookFixRaw(ctx, nil, roborevclient.WithBody(body))
	}, &out)
}
