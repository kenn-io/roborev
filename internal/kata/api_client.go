package kata

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	gen "go.kenn.io/kata/pkg/client/generated"
	"golang.org/x/mod/semver"

	"go.kenn.io/roborev/internal/config"
)

const (
	// MinAPISchemaVersion is the oldest Kata API schema the connection accepts.
	MinAPISchemaVersion = "0.21.0"
	// PaginationAPISchemaVersion is the first schema that honors a list cursor
	// (Kata v0.19.0's cmd/kata/api_compat.go, apiVersionDiscoveryPagination).
	// An older server ignores the cursor and would repeat the first page.
	PaginationAPISchemaVersion = "0.27.0"
	// WriteActor is the actor roborev names on every write. An authenticated
	// credential's own actor overrides it, and a static token needs it.
	WriteActor = "roborev"

	// apiDefaultListLimit matches Kata v0.19.0's `kata list` default
	// (cmd/kata/list.go:306).
	apiDefaultListLimit = 200
)

// ServerInfo is what the server reports about itself.
type ServerInfo struct {
	Version          string
	APISchemaVersion string
}

// APIOption adjusts an APIClient.
type APIOption func(*apiSettings)

type apiSettings struct {
	timeout    time.Duration
	httpClient *http.Client
}

// WithTimeout sets the per-request deadline.
func WithTimeout(d time.Duration) APIOption { return func(s *apiSettings) { s.timeout = d } }

// WithHTTPClient supplies the underlying HTTP client (for TLS trust in tests).
// Its timeout and redirect policy are overridden.
func WithHTTPClient(c *http.Client) APIOption { return func(s *apiSettings) { s.httpClient = c } }

// APIClient implements Client over Kata's HTTP API, using the configured
// connection. Only the daemon builds one: it holds the credential. It resolves
// a workspace's project the way the kata CLI does and falls back to the
// configured default project.
type APIClient struct {
	tr             *transport
	workdir        string
	defaultProject string

	mu       sync.Mutex
	info     *ServerInfo
	projects map[string]int64
}

var _ Client = (*APIClient)(nil)

type apiIssueListPage struct {
	Complete   bool           `json:"complete"`
	Issues     jsontext.Value `json:"issues"`
	NextCursor *string        `json:"next_cursor,omitempty"`
	Total      *int64         `json:"total,omitempty"`
	Truncated  bool           `json:"truncated"`
}

func (p apiIssueListPage) decodeIssues() ([]gen.IssueOut, error) {
	if p.Issues.Kind() != jsontext.KindBeginArray {
		return nil, fmt.Errorf("%w: the issue list has no issues array", ErrInvalidResponse)
	}
	var issues []gen.IssueOut
	if err := json.Unmarshal(p.Issues, &issues); err != nil {
		return nil, fmt.Errorf("%w: could not decode the issue list: %w", ErrInvalidResponse, err)
	}
	return issues, nil
}

// NewAPIClient builds a client for cfg. It reads the credential and checks the
// endpoint but does not contact Kata, so construction is cheap and a
// misconfiguration surfaces here, never as a silent fall back to the CLI.
func NewAPIClient(workdir string, cfg config.KataIntegrationConfig, opts ...APIOption) (*APIClient, error) {
	if !cfg.Enabled {
		return nil, errors.New("kata: the API connection is not enabled")
	}
	endpoint, err := cfg.ParsedEndpoint()
	if err != nil {
		return nil, err
	}
	key, err := cfg.ResolveAPIKey()
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrAuthenticationRequired, err)
	}
	var settings apiSettings
	for _, opt := range opts {
		opt(&settings)
	}
	tr, err := newTransport(transportOptions{
		Endpoint: endpoint, APIKey: key, Timeout: settings.timeout, HTTPClient: settings.httpClient,
	})
	if err != nil {
		return nil, err
	}
	return &APIClient{
		tr: tr, workdir: workdir,
		defaultProject: strings.TrimSpace(cfg.DefaultProject),
	}, nil
}

// SecurityNote describes a reduced guarantee on this platform, or "".
func (c *APIClient) SecurityNote() string { return c.tr.securityNote }

// CloseIdleConnections releases this client's idle HTTP connections.
func (c *APIClient) CloseIdleConnections() {
	if c != nil && c.tr != nil {
		c.tr.closeIdleConnections()
	}
}

// Binding returns the workspace's bound project, or the configured default
// project when the workspace has no binding. A binding file that is present
// but unreadable stops the lookup. It never contacts Kata.
func (c *APIClient) Binding(context.Context) (Binding, error) {
	b, err := workspaceBinding(c.workdir)
	if err == nil {
		return b, nil
	}
	if errors.Is(err, ErrNoBinding) && c.defaultProject != "" {
		return Binding{Project: c.defaultProject}, nil
	}
	return Binding{}, err
}

func compatibleAPISchema(reported string) bool { return schemaAtLeast(reported, MinAPISchemaVersion) }

// schemaAtLeast compares a reported "MAJOR.MINOR.PATCH" (a leading v is
// allowed) with a minimum. Anything that is not a full semantic version fails.
func schemaAtLeast(reported, minimum string) bool {
	core := strings.TrimPrefix(strings.TrimSpace(reported), "v")
	version := "v" + core
	if !semver.IsValid(version) || strings.Count(strings.SplitN(core, "-", 2)[0], ".") != 2 {
		return false
	}
	return semver.Compare(version, "v"+minimum) >= 0
}

// Info returns compatible server information, caching a successful health
// check on this client. A server that does not implement the health endpoint,
// or reports a schema older than MinAPISchemaVersion, is incompatible and
// nothing else is sent to it.
func (c *APIClient) Info(ctx context.Context) (ServerInfo, error) {
	c.mu.Lock()
	cached := c.info
	c.mu.Unlock()
	if cached != nil {
		return *cached, nil
	}
	return c.refreshInfo(ctx)
}

// refreshInfo checks health even when this client already cached a compatible
// result. Connection evaluation uses it to detect a server that stopped or
// began rejecting requests after an earlier successful check.
func (c *APIClient) refreshInfo(ctx context.Context) (ServerInfo, error) {
	var health gen.HealthResponseBody
	if _, err := c.tr.do(ctx, http.MethodGet, "/api/v1/health", nil, nil, nil, &health, http.StatusOK); err != nil {
		if errors.Is(err, ErrNotFound) {
			return ServerInfo{}, ErrIncompatible
		}
		return ServerInfo{}, err
	}
	info := ServerInfo{Version: health.Version}
	if health.APISchemaVersion != nil {
		info.APISchemaVersion = *health.APISchemaVersion
	}
	if !compatibleAPISchema(info.APISchemaVersion) {
		return info, ErrIncompatible
	}
	c.mu.Lock()
	c.info = &info
	c.mu.Unlock()
	return info, nil
}

// projectID resolves an active project by name. It refreshes a cached project
// list when the requested project is missing.
func (c *APIClient) projectID(ctx context.Context, name string) (int64, error) {
	if name == "" {
		return 0, ErrNoBinding
	}
	c.mu.Lock()
	cached := c.projects
	c.mu.Unlock()
	if cached == nil {
		var err error
		cached, err = c.refreshProjects(ctx)
		if err != nil {
			return 0, err
		}
	} else if _, ok := cached[name]; !ok {
		var err error
		cached, err = c.refreshProjects(ctx)
		if err != nil {
			return 0, err
		}
	}
	id, ok := cached[name]
	if !ok {
		return 0, fmt.Errorf("%w: %q", ErrProjectNotFound, name)
	}
	return id, nil
}

func (c *APIClient) refreshProjects(ctx context.Context) (map[string]int64, error) {
	var list gen.ListProjectsResponseBody
	if _, err := c.tr.do(ctx, http.MethodGet, "/api/v1/projects", nil, nil, nil, &list, http.StatusOK); err != nil {
		return nil, err
	}
	projects := make(map[string]int64, len(list.Projects))
	for _, project := range list.Projects {
		if project.Active && project.DeletedAt == nil {
			projects[project.Name] = project.ID
		}
	}
	c.mu.Lock()
	c.projects = projects
	c.mu.Unlock()
	return projects, nil
}

// issuesPath checks compatibility, resolves project (the workspace binding or
// default when empty), and returns the issues collection path for it along
// with the resolved project name.
func (c *APIClient) issuesPath(ctx context.Context, project string) (path, resolved string, err error) {
	if project == "" {
		b, err := c.Binding(ctx)
		if err != nil {
			return "", "", err
		}
		project = b.Project
	}
	if _, err := c.Info(ctx); err != nil {
		return "", "", err
	}
	id, err := c.projectID(ctx, project)
	if err != nil {
		return "", "", err
	}
	return "/api/v1/projects/" + strconv.FormatInt(id, 10) + "/issues", project, nil
}

// List returns issues filtered by status (default "open") for the bound
// project. Unlimited reads the complete set, page by page on servers that
// support cursors.
func (c *APIClient) List(ctx context.Context, opts ListOpts) ([]Issue, error) {
	path, _, err := c.issuesPath(ctx, "")
	if err != nil {
		return nil, err
	}
	status := opts.Status
	switch status {
	case "":
		status = "open"
	case "all":
		status = "" // Kata's filter for both open and closed
	}
	query := url.Values{"status": {status}}
	if !opts.Unlimited {
		query.Set("limit", strconv.Itoa(apiDefaultListLimit))
		var page apiIssueListPage
		if _, err := c.tr.do(ctx, http.MethodGet, path, query, nil, nil, &page, http.StatusOK); err != nil {
			return nil, err
		}
		issues, err := page.decodeIssues()
		if err != nil {
			return nil, err
		}
		return issuesFromAPI(issues), nil
	}
	info, err := c.Info(ctx)
	if err != nil {
		return nil, err
	}
	if !schemaAtLeast(info.APISchemaVersion, PaginationAPISchemaVersion) {
		// This server ignores a cursor but returns every issue when the limit
		// is absent, as `kata list --limit 0` relies on.
		var all apiIssueListPage
		if _, err := c.tr.do(ctx, http.MethodGet, path, query, nil, nil, &all, http.StatusOK); err != nil {
			return nil, err
		}
		issues, err := all.decodeIssues()
		if err != nil {
			return nil, err
		}
		return issuesFromAPI(issues), nil
	}
	// Cursors need a stable order and a page size: without a limit Kata returns
	// every issue in one page and no cursor. Every page is read, so the page
	// size never drops an issue.
	query.Set("sort", "oldest")
	query.Set("limit", strconv.Itoa(apiDefaultListLimit))
	var out []Issue
	seen := map[string]bool{}
	for {
		var page apiIssueListPage
		if _, err := c.tr.do(ctx, http.MethodGet, path, query, nil, nil, &page, http.StatusOK); err != nil {
			return nil, err
		}
		issues, err := page.decodeIssues()
		if err != nil {
			return nil, err
		}
		out = append(out, issuesFromAPI(issues)...)
		if page.NextCursor == nil || *page.NextCursor == "" {
			if !page.Complete {
				return nil, fmt.Errorf("%w: the issue list ended without a complete page", ErrInvalidResponse)
			}
			return out, nil
		}
		cursor := *page.NextCursor
		if seen[cursor] {
			return nil, fmt.Errorf("%w: the issue list repeated a cursor", ErrInvalidResponse)
		}
		seen[cursor] = true
		query.Set("cursor", cursor)
	}
}

func issuesFromAPI(issues []gen.IssueOut) []Issue {
	out := make([]Issue, 0, len(issues))
	for _, issue := range issues {
		out = append(out, Issue{
			ShortID: issue.ShortID, QualifiedID: issue.QualifiedID, Title: issue.Title, Body: issue.Body,
			Status: issue.Status, Labels: issue.Labels, Parent: peerFromAPI(issue.Parent),
			Blocks: peersFromAPI(issue.Blocks), BlockedBy: peersFromAPI(issue.BlockedBy), Related: peersFromAPI(issue.Related),
		})
	}
	return out
}

func peerFromAPI(peer *gen.LinkPeer) *LinkPeer {
	if peer == nil {
		return nil
	}
	return &LinkPeer{ShortID: peer.ShortID, Project: peer.Project, QualifiedID: peer.QualifiedID, Status: peer.Status}
}

func peersFromAPI(peers []gen.LinkPeer) []LinkPeer {
	if len(peers) == 0 {
		return nil
	}
	out := make([]LinkPeer, 0, len(peers))
	for i := range peers {
		out = append(out, *peerFromAPI(&peers[i]))
	}
	return out
}

// splitRef separates an optional "project#id" prefix. A bare ref belongs to
// the empty project, which means the bound or default one.
func splitRef(ref string) (project, id string, err error) {
	id = ref
	if p, rest, qualified := strings.Cut(ref, "#"); qualified {
		if p == "" {
			return "", "", fmt.Errorf("%w: empty project in %q", ErrRequestRejected, ref)
		}
		project, id = p, rest
	}
	if err := validatePathSegment(id); err != nil {
		return "", "", err
	}
	return project, id, nil
}

// Show returns one issue by short ID, UID, or project#short ID.
func (c *APIClient) Show(ctx context.Context, ref string) (Issue, error) {
	project, id, err := splitRef(ref)
	if err != nil {
		return Issue{}, err
	}
	path, project, err := c.issuesPath(ctx, project)
	if err != nil {
		return Issue{}, err
	}
	var shown gen.ShowIssueResponseBody
	if _, err := c.tr.do(ctx, http.MethodGet, path+"/"+id, nil, nil, nil, &shown, http.StatusOK); err != nil {
		return Issue{}, err
	}
	// Kata accepts a ULID in either case and returns it in uppercase, and
	// ParseRefs lowercases every ref it finds in a commit message.
	if !strings.EqualFold(shown.Issue.ShortID, id) && !strings.EqualFold(shown.Issue.UID, id) {
		return Issue{}, fmt.Errorf("%w: Kata returned a different issue than %q", ErrInvalidResponse, id)
	}
	iss := Issue{
		ShortID: shown.Issue.ShortID, QualifiedID: project + "#" + shown.Issue.ShortID, Title: shown.Issue.Title,
		Body: shown.Issue.Body, Status: shown.Issue.Status,
	}
	edges := make([]linkEdge, 0, len(shown.Links))
	for _, link := range shown.Links {
		edges = append(edges, linkEdge{Type: link.Type, From: *peerFromAPI(&link.From), To: *peerFromAPI(&link.To)})
	}
	if err := attachLinks(&iss, iss.QualifiedID, edges); err != nil {
		return Issue{}, fmt.Errorf("%w: %w", ErrInvalidResponse, err)
	}
	for _, label := range shown.Labels {
		if label.Label != "" {
			iss.Labels = append(iss.Labels, label.Label)
		}
	}
	return iss, nil
}

// Create files an issue with the idempotency key and metadata in the request.
// The project is the request's, else the workspace binding, else the default.
// A look-alike soft block is surfaced as an error, as with the kata CLI.
func (c *APIClient) Create(ctx context.Context, req CreateReq) (CreateResult, error) {
	if strings.TrimSpace(req.Title) == "" {
		return CreateResult{}, fmt.Errorf("%w: title is required", ErrRequestRejected)
	}
	path, _, err := c.issuesPath(ctx, req.Project)
	if err != nil {
		return CreateResult{}, err
	}
	body := gen.CreateIssueRequestBody{
		Actor: new(WriteActor), Title: req.Title, Body: &req.Body, Labels: req.Labels, Metadata: req.Metadata,
	}
	if req.Priority != nil {
		body.Priority = new(int64(*req.Priority))
	}
	var header http.Header
	if req.IdempotencyKey != "" {
		header = http.Header{"Idempotency-Key": {req.IdempotencyKey}}
	}
	var created gen.MutationResponseBody
	if _, err := c.tr.do(ctx, http.MethodPost, path, nil, body, header, &created, http.StatusOK, http.StatusCreated); err != nil {
		return CreateResult{}, err
	}
	if created.Issue.ShortID == "" {
		return CreateResult{}, fmt.Errorf("%w: created issue has no short ID", ErrInvalidResponse)
	}
	return CreateResult{
		ShortID:     created.Issue.ShortID,
		Reused:      created.Reused != nil && *created.Reused,
		Attribution: Attribution{Author: created.Issue.Author},
	}, nil
}
