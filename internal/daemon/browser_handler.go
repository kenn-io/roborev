package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/roborev/internal/storage"
)

type routeKey struct {
	method string
	path   string
}

type routePolicy int

const (
	publicRoute routePolicy = iota
	authenticatedRoute
	mutationRoute
)

var browserAPIRoutes = map[routeKey]routePolicy{
	{http.MethodGet, "/api/ping"}:                 publicRoute,
	{http.MethodGet, "/api/status"}:               authenticatedRoute,
	{http.MethodGet, "/api/jobs"}:                 authenticatedRoute,
	{http.MethodGet, "/api/review"}:               authenticatedRoute,
	{http.MethodGet, "/api/comments"}:             authenticatedRoute,
	{http.MethodGet, "/api/repos"}:                authenticatedRoute,
	{http.MethodGet, "/api/branches"}:             authenticatedRoute,
	{http.MethodGet, "/api/job/output"}:           authenticatedRoute,
	{http.MethodGet, "/api/stream/events"}:        authenticatedRoute,
	{http.MethodGet, "/api/ui/review-projection"}: authenticatedRoute,
	{http.MethodGet, "/api/ui/analytics"}:         authenticatedRoute,
	{http.MethodPost, "/api/job/cancel"}:          mutationRoute,
	{http.MethodPost, "/api/job/rerun"}:           mutationRoute,
	{http.MethodPost, "/api/review/close"}:        mutationRoute,
	{http.MethodPost, "/api/comment"}:             mutationRoute,
}

type browserPrincipalContextKey struct{}

func BrowserPrincipalFromContext(ctx context.Context) (BrowserPrincipal, bool) {
	principal, found := ctx.Value(browserPrincipalContextKey{}).(BrowserPrincipal)
	return principal, found
}

func (s *Server) newBrowserHandler(
	core http.Handler,
	static http.Handler,
	policy BrowserPolicy,
	sessions *BrowserSessionManager,
	basePath string,
) (http.Handler, error) {
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if err := policy.ValidateHost(request); err != nil {
			writeBrowserError(w, http.StatusBadRequest, "invalid_host")
			return
		}
		normalizedPath, redirect, err := normalizeBrowserPath(request.URL.Path, basePath)
		if err != nil {
			http.NotFound(w, request)
			return
		}
		if redirect {
			location := joinBrowserPath(basePath, "/")
			if request.URL.RawQuery != "" {
				location += "?" + request.URL.RawQuery
			}
			w.Header().Set("Location", location)
			w.WriteHeader(http.StatusPermanentRedirect)
			return
		}
		if normalizedPath != request.URL.Path {
			request = request.Clone(request.Context())
			request.URL.Path = normalizedPath
			request.URL.RawPath = ""
		}
		if strings.HasPrefix(request.URL.Path, "/api/ui/session") {
			handleBrowserSession(w, request, policy, sessions)
			return
		}
		if strings.HasPrefix(request.URL.Path, "/api/") || strings.HasPrefix(request.URL.Path, "/debug/") || isBrowserOpenAPIPath(request.URL.Path) {
			route, found := browserAPIRoutes[routeKey{request.Method, request.URL.Path}]
			if !found {
				http.NotFound(w, request)
				return
			}
			if route == publicRoute {
				core.ServeHTTP(w, request)
				return
			}
			principal, ambient, tab, err := authenticateBrowserRequest(request, sessions)
			if err != nil {
				writeBrowserError(w, http.StatusUnauthorized, "web_session_required")
				return
			}
			if route == mutationRoute {
				if err := policy.ValidateOrigin(request); err != nil {
					writeBrowserError(w, http.StatusForbidden, "invalid_origin")
					return
				}
				if err := sessions.CheckCSRF(tab, request.Header.Get(WebCSRFHeader)); err != nil {
					writeBrowserError(w, http.StatusForbidden, "csrf_invalid")
					return
				}
			}
			requestContext := request.Context()
			if isAuthenticatedBrowserStream(request) {
				streamContext, cancel, lifetimeErr := browserStreamContext(
					requestContext, sessions, ambient, tab,
				)
				if lifetimeErr != nil {
					writeBrowserError(w, http.StatusUnauthorized, "web_session_required")
					return
				}
				defer cancel()
				requestContext = streamContext
			}
			w.Header().Set("Cache-Control", "private, no-store")
			w.Header().Add("Vary", "Cookie")
			w.Header().Add("Vary", WebSessionHeader)
			authenticated := request.WithContext(context.WithValue(
				requestContext, browserPrincipalContextKey{}, principal,
			))
			switch request.URL.Path {
			case "/api/jobs":
				serveBrowserJobs(w, authenticated, core)
			case "/api/review":
				serveBrowserReview(w, authenticated, core)
			default:
				core.ServeHTTP(w, authenticated)
			}
			return
		}
		static.ServeHTTP(w, request)
	}), nil
}

func isAuthenticatedBrowserStream(request *http.Request) bool {
	return request.URL.Path == "/api/stream/events" ||
		(request.URL.Path == "/api/job/output" && request.URL.Query().Get("stream") == "1")
}

func browserStreamContext(
	parent context.Context,
	sessions *BrowserSessionManager,
	ambient, tab string,
) (context.Context, context.CancelFunc, error) {
	revoked, remaining, err := sessions.sessionLifetime(ambient, tab)
	if err != nil {
		return nil, nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	timer := time.AfterFunc(remaining, cancel)
	go func() {
		select {
		case <-revoked:
			cancel()
		case <-ctx.Done():
		}
		timer.Stop()
	}()
	return ctx, cancel, nil
}

// browserReviewJob is an explicit presentation allowlist. Storage jobs carry
// execution metadata such as agent command lines, reusable session IDs,
// resolved panel configuration, worker ownership, and checkout paths. Those
// fields are useful to the loopback CLI API but must never cross the browser
// listener boundary merely because a field was added to storage.ReviewJob.
type browserReviewJob struct {
	ID               int64                 `json:"id"`
	RepoID           int64                 `json:"repo_id"`
	CommitID         *int64                `json:"commit_id,omitempty"`
	GitRef           string                `json:"git_ref"`
	Branch           string                `json:"branch,omitempty"`
	Agent            string                `json:"agent"`
	Model            string                `json:"model,omitempty"`
	Provider         string                `json:"provider,omitempty"`
	Reasoning        string                `json:"reasoning,omitempty"`
	JobType          string                `json:"job_type"`
	Status           storage.JobStatus     `json:"status"`
	EnqueuedAt       time.Time             `json:"enqueued_at"`
	StartedAt        *time.Time            `json:"started_at,omitempty"`
	FinishedAt       *time.Time            `json:"finished_at,omitempty"`
	Prompt           string                `json:"prompt,omitempty"`
	RetryCount       int                   `json:"retry_count"`
	Agentic          bool                  `json:"agentic"`
	PromptPrebuilt   bool                  `json:"prompt_prebuilt"`
	ReviewType       string                `json:"review_type,omitempty"`
	PatchID          string                `json:"patch_id,omitempty"`
	OutputPrefix     string                `json:"output_prefix,omitempty"`
	SkipReason       string                `json:"skip_reason,omitempty"`
	Source           string                `json:"source,omitempty"`
	ParentJobID      *int64                `json:"parent_job_id,omitempty"`
	MinSeverity      string                `json:"min_severity,omitempty"`
	PanelRunUUID     string                `json:"panel_run_uuid,omitempty"`
	PanelRole        string                `json:"panel_role,omitempty"`
	PanelName        string                `json:"panel_name,omitempty"`
	PanelMemberName  string                `json:"panel_member_name,omitempty"`
	PanelMemberIndex int                   `json:"panel_member_index,omitempty"`
	TokenUsage       string                `json:"token_usage,omitempty"`
	UUID             string                `json:"uuid,omitempty"`
	RepoPath         string                `json:"repo_path,omitempty"`
	RepoName         string                `json:"repo_name,omitempty"`
	CommitSubject    string                `json:"commit_subject,omitempty"`
	Closed           *bool                 `json:"closed,omitempty"`
	Verdict          *string               `json:"verdict,omitempty"`
	PanelSummary     *storage.PanelSummary `json:"panel_summary,omitempty"`
}

// browserTokenUsage is the numeric presentation subset of persisted token
// usage. The storage envelope also carries resumable agent identifiers and
// collection cursors, which must remain confined to the loopback API.
type browserTokenUsage struct {
	InputTokens         *int64   `json:"input_tokens,omitempty"`
	CachedInputTokens   *int64   `json:"cached_input_tokens,omitempty"`
	CacheCreationTokens *int64   `json:"cache_creation_tokens,omitempty"`
	OutputTokens        *int64   `json:"total_output_tokens,omitempty"`
	PeakContextTokens   *int64   `json:"peak_context_tokens,omitempty"`
	CostUSD             *float64 `json:"cost_usd,omitempty"`
	HasCost             bool     `json:"has_cost,omitempty"`
}

type browserJobsResponse struct {
	Jobs          []browserReviewJob `json:"jobs"`
	HasMore       bool               `json:"has_more"`
	NextCursor    *string            `json:"next_cursor"`
	Stats         *storage.JobStats  `json:"stats,omitempty"`
	FilteredStats *storage.JobStats  `json:"filtered_stats,omitempty"`
}

type browserReviewResponse struct {
	ID          int64             `json:"id"`
	JobID       int64             `json:"job_id"`
	Agent       string            `json:"agent"`
	Prompt      string            `json:"prompt"`
	Output      string            `json:"output"`
	CreatedAt   time.Time         `json:"created_at"`
	Closed      bool              `json:"closed"`
	VerdictBool *int              `json:"verdict_bool,omitempty"`
	Job         *browserReviewJob `json:"job,omitempty"`
}

func projectBrowserReviewJob(job storage.ReviewJob) browserReviewJob {
	return browserReviewJob{
		ID: job.ID, RepoID: job.RepoID, CommitID: job.CommitID,
		GitRef: job.GitRef, Branch: job.Branch, Agent: job.Agent,
		Model: job.Model, Provider: job.Provider, Reasoning: job.Reasoning,
		JobType: job.JobType, Status: job.Status, EnqueuedAt: job.EnqueuedAt,
		StartedAt: job.StartedAt, FinishedAt: job.FinishedAt, Prompt: job.Prompt,
		RetryCount: job.RetryCount, Agentic: job.Agentic,
		PromptPrebuilt: job.PromptPrebuilt, ReviewType: job.ReviewType,
		PatchID: job.PatchID, OutputPrefix: job.OutputPrefix,
		SkipReason: job.SkipReason, Source: job.Source, ParentJobID: job.ParentJobID,
		MinSeverity: job.MinSeverity, PanelRunUUID: job.PanelRunUUID,
		PanelRole: job.PanelRole, PanelName: job.PanelName,
		PanelMemberName: job.PanelMemberName, PanelMemberIndex: job.PanelMemberIndex,
		TokenUsage: projectBrowserTokenUsage(job.TokenUsage), UUID: job.UUID,
		RepoPath: job.RepoPath,
		RepoName: job.RepoName, CommitSubject: job.CommitSubject, Closed: job.Closed,
		Verdict: job.Verdict, PanelSummary: job.PanelSummary,
	}
}

func projectBrowserTokenUsage(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return ""
	}
	var usage browserTokenUsage
	if err := json.Unmarshal([]byte(raw), &usage); err != nil {
		return ""
	}
	if usage.InputTokens == nil && usage.CachedInputTokens == nil &&
		usage.CacheCreationTokens == nil && usage.OutputTokens == nil &&
		usage.PeakContextTokens == nil && usage.CostUSD == nil && !usage.HasCost {
		return ""
	}
	projected, err := json.Marshal(usage)
	if err != nil {
		return ""
	}
	return string(projected)
}

func serveBrowserJobs(w http.ResponseWriter, request *http.Request, core http.Handler) {
	serveProjectedBrowserJSON(w, request, core, func(data []byte) (any, error) {
		var source struct {
			Jobs          []storage.ReviewJob `json:"jobs"`
			HasMore       bool                `json:"has_more"`
			NextCursor    *string             `json:"next_cursor"`
			Stats         *storage.JobStats   `json:"stats,omitempty"`
			FilteredStats *storage.JobStats   `json:"filtered_stats,omitempty"`
		}
		if err := json.Unmarshal(data, &source); err != nil {
			return nil, err
		}
		jobs := make([]browserReviewJob, len(source.Jobs))
		for i := range source.Jobs {
			jobs[i] = projectBrowserReviewJob(source.Jobs[i])
		}
		return browserJobsResponse{
			Jobs: jobs, HasMore: source.HasMore, NextCursor: source.NextCursor,
			Stats: source.Stats, FilteredStats: source.FilteredStats,
		}, nil
	})
}

func serveBrowserReview(w http.ResponseWriter, request *http.Request, core http.Handler) {
	serveProjectedBrowserJSON(w, request, core, func(data []byte) (any, error) {
		var source *storage.Review
		if err := json.Unmarshal(data, &source); err != nil {
			return nil, err
		}
		if source == nil {
			return nil, nil
		}
		projected := browserReviewResponse{
			ID: source.ID, JobID: source.JobID, Agent: source.Agent,
			Prompt: source.Prompt, Output: source.Output, CreatedAt: source.CreatedAt,
			Closed: source.Closed, VerdictBool: source.VerdictBool,
		}
		if source.Job != nil {
			job := projectBrowserReviewJob(*source.Job)
			projected.Job = &job
		}
		return projected, nil
	})
}

type browserResponseCapture struct {
	header http.Header
	body   bytes.Buffer
	status int
}

func newBrowserResponseCapture() *browserResponseCapture {
	return &browserResponseCapture{header: make(http.Header)}
}

func (capture *browserResponseCapture) Header() http.Header { return capture.header }

func (capture *browserResponseCapture) WriteHeader(status int) {
	if capture.status == 0 {
		capture.status = status
	}
}

func (capture *browserResponseCapture) Write(data []byte) (int, error) {
	if capture.status == 0 {
		capture.status = http.StatusOK
	}
	return capture.body.Write(data)
}

func serveProjectedBrowserJSON(
	w http.ResponseWriter,
	request *http.Request,
	core http.Handler,
	project func([]byte) (any, error),
) {
	capture := newBrowserResponseCapture()
	core.ServeHTTP(capture, request)
	status := capture.status
	if status == 0 {
		status = http.StatusOK
	}
	if status < http.StatusOK || status >= http.StatusMultipleChoices {
		copyBrowserResponse(w, capture.header, status, capture.body.Bytes())
		return
	}
	value, err := project(capture.body.Bytes())
	if err != nil {
		writeBrowserError(w, http.StatusInternalServerError, "browser_response_invalid")
		return
	}
	copyBrowserHeaders(w.Header(), capture.header)
	w.Header().Del("Content-Length")
	writeBrowserJSON(w, status, value)
}

func copyBrowserResponse(w http.ResponseWriter, header http.Header, status int, body []byte) {
	copyBrowserHeaders(w.Header(), header)
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func copyBrowserHeaders(destination, source http.Header) {
	for key, values := range source {
		for _, value := range values {
			destination.Add(key, value)
		}
	}
}

func handleBrowserSession(w http.ResponseWriter, request *http.Request, policy BrowserPolicy, sessions *BrowserSessionManager) {
	switch {
	case request.Method == http.MethodPost && request.URL.Path == "/api/ui/session/login":
		if err := policy.ValidateOrigin(request); err != nil {
			writeBrowserError(w, http.StatusForbidden, "invalid_origin")
			return
		}
		var login WebLoginRequest
		if !readBrowserJSON(w, request, &login) {
			return
		}
		credentials, err := sessions.Login(login.Token)
		if err != nil {
			var limited *WebLoginRateLimitError
			if errors.As(err, &limited) {
				seconds := max(1, int((limited.RetryAfter+time.Second-1)/time.Second))
				w.Header().Set("Retry-After", strconv.Itoa(seconds))
				writeBrowserError(w, http.StatusTooManyRequests, "login_rate_limited")
				return
			}
			writeBrowserError(w, http.StatusUnauthorized, "web_session_required")
			return
		}
		http.SetCookie(w, sessions.Cookie(credentials.Ambient))
		writeBrowserCredentials(w, credentials)
	case request.Method == http.MethodPost && request.URL.Path == "/api/ui/session/bootstrap":
		if !browserBootstrapOriginAllowed(request, policy) {
			writeBrowserError(w, http.StatusForbidden, "invalid_origin")
			return
		}
		if !strings.HasPrefix(strings.ToLower(request.Header.Get("Content-Type")), "application/json") {
			writeBrowserError(w, http.StatusUnsupportedMediaType, "invalid_request")
			return
		}
		cookie, err := request.Cookie(sessions.CookieName())
		var credentials SessionCredentials
		if err == nil {
			credentials, err = sessions.Bootstrap(cookie.Value)
		} else if policy.AllowsLocalSession(request) {
			credentials, err = sessions.NewLocalSession()
			if err == nil {
				http.SetCookie(w, sessions.Cookie(credentials.Ambient))
			}
		} else {
			err = ErrWebSessionRequired
		}
		if err != nil {
			writeBrowserError(w, http.StatusUnauthorized, "web_session_required")
			return
		}
		writeBrowserCredentials(w, credentials)
	case request.Method == http.MethodDelete && request.URL.Path == "/api/ui/session":
		if err := policy.ValidateOrigin(request); err != nil {
			writeBrowserError(w, http.StatusForbidden, "invalid_origin")
			return
		}
		_, ambient, tab, err := authenticateBrowserRequest(request, sessions)
		if err != nil {
			writeBrowserError(w, http.StatusUnauthorized, "web_session_required")
			return
		}
		if err := sessions.CheckCSRF(tab, request.Header.Get(WebCSRFHeader)); err != nil {
			writeBrowserError(w, http.StatusForbidden, "csrf_invalid")
			return
		}
		sessions.Logout(ambient)
		http.SetCookie(w, sessions.ExpiredCookie())
		w.WriteHeader(http.StatusNoContent)
	case request.Method == http.MethodGet && request.URL.Path == "/api/ui/session":
		writeBrowserSessionStatus(w, request, policy, sessions)
	default:
		http.NotFound(w, request)
	}
}

func browserBootstrapOriginAllowed(request *http.Request, policy BrowserPolicy) bool {
	if request.Header.Get("Origin") == "" {
		return policy.AllowsLocalSession(request) &&
			!strings.EqualFold(strings.TrimSpace(request.Header.Get("Sec-Fetch-Site")), "cross-site")
	}
	if policy.ValidateOrigin(request) != nil {
		return false
	}
	return request.Header.Get("Sec-Fetch-Site") == "same-origin" &&
		request.Header.Get("Sec-Fetch-Mode") == "cors" &&
		request.Header.Get("Sec-Fetch-Dest") == "empty"
}

func authenticateBrowserRequest(request *http.Request, sessions *BrowserSessionManager) (BrowserPrincipal, string, string, error) {
	cookie, err := request.Cookie(sessions.CookieName())
	if err != nil {
		return BrowserPrincipal{}, "", "", ErrWebSessionRequired
	}
	tab := request.Header.Get(WebSessionHeader)
	principal, err := sessions.Authenticate(cookie.Value, tab)
	return principal, cookie.Value, tab, err
}

func writeBrowserSessionStatus(w http.ResponseWriter, request *http.Request, policy BrowserPolicy, sessions *BrowserSessionManager) {
	authentication := "local"
	if policy.remoteAuthEnabled {
		authentication = "token"
	}
	status := WebSessionStatus{Authentication: authentication}
	principal, ambient, tab, err := authenticateBrowserRequest(request, sessions)
	if err == nil {
		status.Authenticated = true
		capabilities := webSessionCapabilities(principal)
		status.Capabilities = &capabilities
		if expiry, expiryErr := sessions.SessionExpiry(ambient, tab); expiryErr == nil {
			status.ExpiresAt = &expiry
		}
	}
	writeBrowserJSON(w, http.StatusOK, status)
}

func writeBrowserCredentials(w http.ResponseWriter, credentials SessionCredentials) {
	writeBrowserJSON(w, http.StatusOK, WebSessionCredentials{
		Session: credentials.Tab, CSRF: credentials.CSRF, ExpiresAt: credentials.Expires,
		Capabilities: webSessionCapabilities(credentials.Principal),
	})
}

func webSessionCapabilities(principal BrowserPrincipal) WebSessionCapabilities {
	return WebSessionCapabilities{
		CancelAnyJob: principal.Local, CancelReviewJob: true, RerunJob: principal.Local,
	}
}

func readBrowserJSON(w http.ResponseWriter, request *http.Request, destination any) bool {
	if !strings.HasPrefix(strings.ToLower(request.Header.Get("Content-Type")), "application/json") {
		writeBrowserError(w, http.StatusUnsupportedMediaType, "invalid_request")
		return false
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, request.Body, 64<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		writeBrowserError(w, http.StatusBadRequest, "invalid_request")
		return false
	}
	return true
}

func writeBrowserJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "private, no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeBrowserError(w http.ResponseWriter, status int, code string) {
	writeBrowserJSON(w, status, map[string]string{"error": code})
}

func isBrowserOpenAPIPath(path string) bool {
	return path == "/openapi.json" || path == "/openapi.yaml" || strings.HasPrefix(path, "/schemas/")
}
