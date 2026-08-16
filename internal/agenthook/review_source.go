package agenthook

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"time"

	kitdaemon "go.kenn.io/kit/daemon"

	"go.kenn.io/roborev/internal/storage"
)

// ReviewSource supplies the tracked repository and review metadata needed by
// the Agent Hook state machine.
type ReviewSource interface {
	ResolveTrackedRepo(
		ctx context.Context, path, branch string,
	) (TrackedRepoResolution, bool)
	ListOpenReviewJobs(
		ctx context.Context, repoRoot, branch string,
	) ([]storage.ReviewJob, bool)
}

// NewHTTPReviewSource creates a source backed by an already-known regular
// daemon endpoint. It never discovers or starts a daemon.
func NewHTTPReviewSource(addr string) ReviewSource {
	ep, err := kitdaemon.ParseEndpoint(addr, kitdaemon.ParseEndpointOptions{
		TCPPolicy: kitdaemon.RequireLoopback,
	})
	if err != nil {
		return httpReviewSource{}
	}
	return httpReviewSource{
		baseURL: ep.BaseURL(),
		client: ep.HTTPClient(kitdaemon.HTTPClientOptions{
			Timeout:               2 * time.Second,
			ResponseHeaderTimeout: 2 * time.Second,
			DisableKeepAlives:     true,
		}),
	}
}

type httpReviewSource struct {
	baseURL string
	client  *http.Client
}

type jobsResponse struct {
	Jobs []storage.ReviewJob `json:"jobs"`
}

func (s httpReviewSource) ResolveTrackedRepo(
	ctx context.Context, path, branch string,
) (TrackedRepoResolution, bool) {
	if s.baseURL == "" || s.client == nil {
		return TrackedRepoResolution{}, false
	}
	values := url.Values{}
	values.Set("path", path)
	values.Set("branch", branch)
	req, err := http.NewRequestWithContext(
		ctx, http.MethodGet,
		s.baseURL+"/api/repos/resolve?"+values.Encode(), nil,
	)
	if err != nil {
		return TrackedRepoResolution{}, false
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return TrackedRepoResolution{}, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return TrackedRepoResolution{}, false
	}
	var out struct {
		Tracked *bool `json:"tracked"`
		Repo    *struct {
			RootPath              string     `json:"root_path"`
			Identity              string     `json:"identity"`
			Name                  string     `json:"name"`
			AgentHookSnoozedUntil *time.Time `json:"agent_hook_snoozed_until,omitempty"`
		} `json:"repo,omitempty"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.Tracked == nil {
		return TrackedRepoResolution{}, false
	}
	resolved := TrackedRepoResolution{Tracked: *out.Tracked}
	if out.Repo != nil {
		resolved.RootPath = out.Repo.RootPath
		resolved.Identity = out.Repo.Identity
		resolved.Name = out.Repo.Name
		if out.Repo.AgentHookSnoozedUntil != nil {
			resolved.SnoozedUntil = *out.Repo.AgentHookSnoozedUntil
		}
	}
	return resolved, true
}

func (s httpReviewSource) ListOpenReviewJobs(
	ctx context.Context, repoRoot, branch string,
) ([]storage.ReviewJob, bool) {
	if s.baseURL == "" || s.client == nil {
		return nil, false
	}
	values := url.Values{}
	values.Set("repo", repoRoot)
	if branch != "" {
		values.Set("branch", branch)
		values.Set("branch_include_empty", "true")
	}
	values.Set("status", "done")
	values.Set("closed", "false")
	values.Set("limit", "10000")
	values.Set("omit_prompt", "true")
	req, err := http.NewRequestWithContext(
		ctx, http.MethodGet, s.baseURL+"/api/jobs?"+values.Encode(), nil,
	)
	if err != nil {
		return nil, false
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, false
	}
	var out jobsResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, false
	}
	return out.Jobs, true
}
