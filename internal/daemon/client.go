package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"time"

	gitrepo "go.kenn.io/kit/git/repo"

	"go.kenn.io/roborev/internal/storage"
)

// Client provides an interface for interacting with the roborev daemon.
// This abstraction allows for easy mocking in tests.
type Client interface {
	// GetReviewBySHA retrieves a review by commit SHA
	GetReviewBySHA(sha string) (*storage.Review, error)

	// GetReviewByJobID retrieves a review by job ID
	GetReviewByJobID(jobID int64) (*storage.Review, error)

	// MarkReviewClosed marks a review as closed by job ID
	MarkReviewClosed(jobID int64) error

	// AddComment adds a comment to a job
	AddComment(jobID int64, commenter, comment string) error

	// EnqueueReview enqueues a review job and returns the job ID
	EnqueueReview(repoPath, gitRef, agentName string) (int64, error)

	// WaitForReview waits for a job to complete and returns the review
	WaitForReview(jobID int64) (*storage.Review, error)

	// FindJobForCommit finds a job for a specific commit in a repo
	FindJobForCommit(ctx context.Context, repoPath, sha string) (*storage.ReviewJob, error)

	// FindPendingJobForRef finds a queued or running job for any git ref
	FindPendingJobForRef(ctx context.Context, repoPath, gitRef string) (*storage.ReviewJob, error)

	// GetCommentsForJob fetches comments for a job
	GetCommentsForJob(jobID int64) ([]storage.Response, error)

	// Remap updates git_ref for jobs whose commits were rewritten
	Remap(req RemapRequest) (*RemapResult, error)
}

// DefaultPollInterval is the default polling interval for WaitForReview.
// Tests can override this to speed up polling-based tests.
var DefaultPollInterval = 2 * time.Second

// HTTPClient is the default HTTP-based implementation of Client
type HTTPClient struct {
	baseURL      string
	httpClient   *http.Client
	pollInterval time.Duration
}

// NewHTTPClient creates a new HTTP daemon client
func NewHTTPClient(ep DaemonEndpoint) *HTTPClient {
	return &HTTPClient{
		baseURL:      ep.BaseURL(),
		httpClient:   ep.HTTPClient(10 * time.Second),
		pollInterval: DefaultPollInterval,
	}
}

// NewHTTPClientFromRuntime creates an HTTP client using daemon runtime info
func NewHTTPClientFromRuntime() (*HTTPClient, error) {
	var lastErr error
	for range 5 {
		info, err := GetAnyRunningDaemon()
		if err == nil {
			return NewHTTPClient(info.Endpoint()), nil
		}
		lastErr = err
		time.Sleep(100 * time.Millisecond)
	}
	return nil, fmt.Errorf("daemon not running: %w", lastErr)
}

// SetPollInterval sets the polling interval for WaitForReview
func (c *HTTPClient) SetPollInterval(interval time.Duration) {
	c.pollInterval = interval
}

func (c *HTTPClient) getReview(url string) (*storage.Review, error) {
	resp, err := c.httpClient.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("server returned %s", resp.Status)
	}

	var review storage.Review
	if err := json.NewDecoder(resp.Body).Decode(&review); err != nil {
		return nil, err
	}
	return &review, nil
}

func (c *HTTPClient) GetReviewBySHA(sha string) (*storage.Review, error) {
	return c.getReview(fmt.Sprintf("%s/api/review?sha=%s", c.baseURL, sha))
}

func (c *HTTPClient) GetReviewByJobID(jobID int64) (*storage.Review, error) {
	return c.getReview(fmt.Sprintf("%s/api/review?job_id=%d", c.baseURL, jobID))
}

func (c *HTTPClient) MarkReviewClosed(jobID int64) error {
	reqBody, _ := json.Marshal(map[string]any{
		"job_id": jobID,
		"closed": true,
	})

	resp, err := c.httpClient.Post(c.baseURL+"/api/review/close", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("mark closed: %s: %s", resp.Status, body)
	}

	return nil
}

func (c *HTTPClient) AddComment(jobID int64, commenter, comment string) error {
	reqBody, _ := json.Marshal(map[string]any{
		"job_id":    jobID,
		"commenter": commenter,
		"comment":   comment,
	})

	resp, err := c.httpClient.Post(c.baseURL+"/api/comment", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("add comment: %s: %s", resp.Status, body)
	}

	return nil
}

func (c *HTTPClient) EnqueueReview(repoPath, gitRef, agentName string) (int64, error) {
	reqBody, _ := json.Marshal(EnqueueRequest{
		RepoPath: repoPath,
		GitRef:   gitRef,
		Agent:    agentName,
	})

	resp, err := c.httpClient.Post(c.baseURL+"/api/enqueue", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return 0, fmt.Errorf("enqueue failed: %s", body)
	}

	var job storage.ReviewJob
	if err := json.NewDecoder(resp.Body).Decode(&job); err != nil {
		return 0, err
	}

	return job.ID, nil
}

func (c *HTTPClient) getJobByID(jobID int64) (*storage.ReviewJob, error) {
	resp, err := c.httpClient.Get(fmt.Sprintf("%s/api/jobs?id=%d", c.baseURL, jobID))
	if err != nil {
		return nil, fmt.Errorf("polling job %d: %w", jobID, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("polling job %d: server returned %s", jobID, resp.Status)
	}

	var result struct {
		Jobs []storage.ReviewJob `json:"jobs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("polling job %d: decode error: %w", jobID, err)
	}
	if len(result.Jobs) == 0 {
		return nil, fmt.Errorf("job %d not found", jobID)
	}
	return &result.Jobs[0], nil
}

func (c *HTTPClient) WaitForReview(jobID int64) (*storage.Review, error) {
	missingReviewAttempts := 0
	for {
		job, err := c.getJobByID(jobID)
		if err != nil {
			return nil, err
		}
		switch job.Status {
		case storage.JobStatusDone:
			review, err := c.GetReviewByJobID(jobID)
			if err != nil {
				return nil, err
			}
			if review != nil {
				return review, nil
			}
			missingReviewAttempts++
			if missingReviewAttempts > 5 {
				return nil, fmt.Errorf("review for job %d not found", jobID)
			}
		case storage.JobStatusFailed:
			return nil, fmt.Errorf("job %d failed: %s", jobID, job.Error)
		case storage.JobStatusCanceled:
			return nil, fmt.Errorf("job %d was canceled", jobID)
		}

		time.Sleep(c.pollInterval)
	}
}

func (c *HTTPClient) FindJobForCommit(ctx context.Context, repoPath, sha string) (*storage.ReviewJob, error) {
	// Normalize repo path to main repo root to handle worktrees consistently.
	// The daemon stores jobs using the main repo root, so we need to match that.
	normalizedRepo := repoPath
	if mainRoot, err := gitrepo.MainRoot(ctx, repoPath); err == nil {
		normalizedRepo = mainRoot
	}
	// Also resolve symlinks and make absolute
	if resolved, err := filepath.EvalSymlinks(normalizedRepo); err == nil {
		normalizedRepo = resolved
	}
	if abs, err := filepath.Abs(normalizedRepo); err == nil {
		normalizedRepo = abs
	}

	// Query by git_ref and repo to avoid matching jobs from different repos
	queryURL := fmt.Sprintf("%s/api/jobs?git_ref=%s&repo=%s&limit=1",
		c.baseURL, url.QueryEscape(sha), url.QueryEscape(normalizedRepo))

	resp, err := c.httpClient.Get(queryURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("query for %s: server returned %s", sha, resp.Status)
	}

	var result struct {
		Jobs []storage.ReviewJob `json:"jobs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("query for %s: decode error: %w", sha, err)
	}

	if len(result.Jobs) > 0 {
		return &result.Jobs[0], nil
	}

	// Fallback: if repo filter yielded no results, try git_ref only.
	// This handles worktrees where daemon stores the main repo root path
	// but the caller uses the worktree path.
	fallbackURL := fmt.Sprintf("%s/api/jobs?git_ref=%s&limit=100", c.baseURL, url.QueryEscape(sha))
	fallbackResp, err := c.httpClient.Get(fallbackURL)
	if err != nil {
		return nil, fmt.Errorf("fallback query for %s: %w", sha, err)
	}
	defer fallbackResp.Body.Close()

	if fallbackResp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fallback query for %s: server returned %s", sha, fallbackResp.Status)
	}

	var fallbackResult struct {
		Jobs []storage.ReviewJob `json:"jobs"`
	}
	if err := json.NewDecoder(fallbackResp.Body).Decode(&fallbackResult); err != nil {
		return nil, fmt.Errorf("fallback query for %s: decode error: %w", sha, err)
	}

	// Filter client-side: find a job whose repo path matches when normalized
	for i := range fallbackResult.Jobs {
		job := &fallbackResult.Jobs[i]
		jobRepo := job.RepoPath
		// Skip empty or relative paths to avoid false matches
		if jobRepo == "" || !filepath.IsAbs(jobRepo) {
			continue
		}
		if resolved, err := filepath.EvalSymlinks(jobRepo); err == nil {
			jobRepo = resolved
		}
		if jobRepo == normalizedRepo {
			return job, nil
		}
	}

	return nil, nil
}

func (c *HTTPClient) FindPendingJobForRef(ctx context.Context, repoPath, gitRef string) (*storage.ReviewJob, error) {
	// Normalize repo path to main repo root
	normalizedRepo := repoPath
	if mainRoot, err := gitrepo.MainRoot(ctx, repoPath); err == nil {
		normalizedRepo = mainRoot
	}
	if resolved, err := filepath.EvalSymlinks(normalizedRepo); err == nil {
		normalizedRepo = resolved
	}
	if abs, err := filepath.Abs(normalizedRepo); err == nil {
		normalizedRepo = abs
	}

	// Use server-side status filtering to find pending jobs.
	// Query for queued first, then running - this avoids pagination issues.
	for _, status := range []string{"queued", "running"} {
		queryURL := fmt.Sprintf("%s/api/jobs?git_ref=%s&repo=%s&status=%s&limit=1",
			c.baseURL, url.QueryEscape(gitRef), url.QueryEscape(normalizedRepo), status)

		resp, err := c.httpClient.Get(queryURL)
		if err != nil {
			return nil, err
		}

		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return nil, fmt.Errorf("query for %s: server returned %s", gitRef, resp.Status)
		}

		var result struct {
			Jobs []storage.ReviewJob `json:"jobs"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			resp.Body.Close()
			return nil, fmt.Errorf("query for %s: decode error: %w", gitRef, err)
		}
		resp.Body.Close()

		if len(result.Jobs) > 0 {
			return &result.Jobs[0], nil
		}
	}

	return nil, nil
}

func (c *HTTPClient) GetCommentsForJob(jobID int64) ([]storage.Response, error) {
	resp, err := c.httpClient.Get(fmt.Sprintf("%s/api/comments?job_id=%d", c.baseURL, jobID))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch responses: %s", resp.Status)
	}

	var result struct {
		Responses []storage.Response `json:"responses"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	return result.Responses, nil
}

// GetAllCommentsForJob fetches comments for a job, merging legacy
// commit-based comments via storage.MergeResponses. When commitID > 0,
// fetches legacy by commit ID. Otherwise, if gitRef looks like a SHA,
// fetches by SHA. Callers may pre-validate gitRef via git.LooksLikeSHA.
func (c *HTTPClient) GetAllCommentsForJob(jobID, commitID int64, gitRef string) ([]storage.Response, error) {
	responses, err := c.GetCommentsForJob(jobID)
	if err != nil {
		return nil, err
	}

	// Also fetch legacy commit-based comments.
	// Prefer commit_id (unambiguous), fall back to SHA only when
	// gitRef looks like a hex commit SHA (not a task label).
	commitID, gitRef = legacyCommentLookupTarget(commitID, gitRef)
	var legacyURL string
	if commitID > 0 {
		legacyURL = fmt.Sprintf("%s/api/comments?commit_id=%d", c.baseURL, commitID)
	} else if gitRef != "" {
		legacyURL = fmt.Sprintf("%s/api/comments?sha=%s", c.baseURL, gitRef)
	}
	if legacyURL != "" {
		legacyResp, err := c.httpClient.Get(legacyURL)
		if err == nil {
			defer legacyResp.Body.Close()
			if legacyResp.StatusCode == http.StatusOK {
				var result struct {
					Responses []storage.Response `json:"responses"`
				}
				if json.NewDecoder(legacyResp.Body).Decode(&result) == nil {
					responses = storage.MergeResponses(responses, result.Responses)
				}
			}
		}
	}

	return responses, nil
}

func legacyCommentLookupTarget(commitID int64, gitRef string) (int64, string) {
	var commitIDPtr *int64
	if commitID > 0 {
		commitIDPtr = &commitID
	}
	return storage.ReviewJob{
		CommitID: commitIDPtr,
		GitRef:   gitRef,
	}.LegacyCommentLookupTarget()
}

// RemapResult is the response from POST /api/remap.
type RemapResult struct {
	Remapped int `json:"remapped"`
	Skipped  int `json:"skipped"`
}

// Remap sends rewritten commit mappings to the daemon so that
// review jobs are updated to point at the new SHAs.
func (c *HTTPClient) Remap(req RemapRequest) (*RemapResult, error) {
	reqBody, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	resp, err := c.httpClient.Post(
		c.baseURL+"/api/remap", "application/json",
		bytes.NewReader(reqBody),
	)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("remap: %s: %s", resp.Status, body)
	}

	var result RemapResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return &result, nil
}
