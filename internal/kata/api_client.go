package kata

import (
	"cmp"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
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

// SupportsReplies reports whether the server stores typed replies and reads
// comment threads.
func (s ServerInfo) SupportsReplies() bool {
	return schemaAtLeast(s.APISchemaVersion, RepliesAPISchemaVersion)
}

// APIOption adjusts an APIClient.
type APIOption func(*apiSettings)

type apiSettings struct {
	timeout     time.Duration
	httpClient  *http.Client
	noWorkspace bool
}

// WithTimeout sets the per-request deadline.
func WithTimeout(d time.Duration) APIOption { return func(s *apiSettings) { s.timeout = d } }

// WithHTTPClient supplies the underlying HTTP client (for TLS trust in tests).
// Its timeout and redirect policy are overridden.
func WithHTTPClient(c *http.Client) APIOption { return func(s *apiSettings) { s.httpClient = c } }

// WithoutWorkspace makes the client ignore binding files and the working
// directory: the project is the one a call names, else the default project. The
// daemon's proxy endpoints use it, because the daemon's own directory says
// nothing about the caller's repository.
func WithoutWorkspace() APIOption { return func(s *apiSettings) { s.noWorkspace = true } }

// APIClient implements Client over Kata's HTTP API, using the configured
// connection. Only the daemon builds one: it holds the credential. It resolves
// a workspace's project the way the kata CLI does and falls back to the
// configured default project.
type APIClient struct {
	endpoint       string
	tr             *transport
	workdir        string
	noWorkspace    bool
	defaultProject string
	teammate       string

	mu       sync.Mutex
	info     *ServerInfo
	projects map[string]projectRef
}

// projectRef is an active project as the server lists it.
type projectRef struct {
	Name string
	ID   int64
	UID  string
}

var _ Client = (*APIClient)(nil)

type apiIssueListPage struct {
	Complete   *bool          `json:"complete"`
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
	teammate := cfg.ResolvedTeammate()
	if err := config.ValidateKataTeammate(teammate); err != nil {
		return nil, err
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
		endpoint: normalizedEndpoint(endpoint), tr: tr, workdir: workdir, noWorkspace: settings.noWorkspace, teammate: teammate,
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
	err := ErrNoBinding
	if !c.noWorkspace {
		var b Binding
		if b, err = workspaceBinding(c.workdir); err == nil {
			return b, nil
		}
	}
	if errors.Is(err, ErrNoBinding) && c.defaultProject != "" {
		return Binding{Project: c.defaultProject}, nil
	}
	return Binding{}, err
}

// ResolveBinding is Binding with the project's full UID, which only the server
// knows. It contacts Kata, and fails with ErrProjectNotFound when the bound
// project is not an active project on the server.
func (c *APIClient) ResolveBinding(ctx context.Context) (Binding, error) {
	b, err := c.Binding(ctx)
	if err != nil {
		return Binding{}, err
	}
	if _, err := c.Info(ctx); err != nil {
		return Binding{}, err
	}
	project, err := c.project(ctx, b.Project)
	if err != nil {
		return Binding{}, err
	}
	b.ProjectUID = project.UID
	return b, nil
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

// project resolves an active project by name. It refreshes a cached project
// list when the requested project is missing.
func (c *APIClient) project(ctx context.Context, name string) (projectRef, error) {
	if name == "" {
		return projectRef{}, ErrNoBinding
	}
	c.mu.Lock()
	cached := c.projects
	c.mu.Unlock()
	if _, ok := cached[name]; cached == nil || !ok {
		var err error
		cached, err = c.refreshProjects(ctx)
		if err != nil {
			return projectRef{}, err
		}
	}
	project, ok := cached[name]
	if !ok {
		return projectRef{}, fmt.Errorf("%w: %q", ErrProjectNotFound, name)
	}
	return project, nil
}

// projectID is project for callers that need only the numeric ID.
func (c *APIClient) projectID(ctx context.Context, name string) (int64, error) {
	project, err := c.project(ctx, name)
	return project.ID, err
}

func (c *APIClient) refreshProjects(ctx context.Context) (map[string]projectRef, error) {
	var list gen.ListProjectsResponseBody
	if _, err := c.tr.do(ctx, http.MethodGet, "/api/v1/projects", nil, nil, nil, &list, http.StatusOK); err != nil {
		return nil, err
	}
	projects := make(map[string]projectRef, len(list.Projects))
	for _, project := range list.Projects {
		if project.Active && project.DeletedAt == nil {
			projects[project.Name] = projectRef{Name: project.Name, ID: project.ID, UID: project.UID}
		}
	}
	c.mu.Lock()
	c.projects = projects
	c.mu.Unlock()
	return projects, nil
}

// issuesPath checks compatibility, resolves project (the workspace binding or
// default when empty), and returns the issues collection path for it along
// with the resolved project.
func (c *APIClient) issuesPath(ctx context.Context, project string) (path string, resolved projectRef, err error) {
	if project == "" {
		b, err := c.Binding(ctx)
		if err != nil {
			return "", projectRef{}, err
		}
		project = b.Project
	}
	if _, err := c.Info(ctx); err != nil {
		return "", projectRef{}, err
	}
	resolved, err = c.project(ctx, project)
	if err != nil {
		return "", projectRef{}, err
	}
	return "/api/v1/projects/" + strconv.FormatInt(resolved.ID, 10) + "/issues", resolved, nil
}

// List returns issues filtered by status (default "open") for the bound
// project. Unlimited reads the complete set page by page.
func (c *APIClient) List(ctx context.Context, opts ListOpts) ([]Issue, error) {
	return c.ListProject(ctx, "", opts)
}

// ListProject is List for a named project; an empty name selects the bound or
// default project.
func (c *APIClient) ListProject(ctx context.Context, project string, opts ListOpts) ([]Issue, error) {
	path, _, err := c.issuesPath(ctx, project)
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
		if all.Truncated || all.Complete != nil && !*all.Complete || all.NextCursor != nil && *all.NextCursor != "" {
			return nil, fmt.Errorf("%w: legacy issue list is incomplete", ErrInvalidResponse)
		}
		return issuesFromAPI(issues), nil
	}
	// Cursors need a stable order.
	query.Set("sort", "oldest")
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
			if page.Complete == nil || !*page.Complete || page.Truncated {
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
			UID: issue.UID, ProjectUID: str(issue.ProjectUID), Metadata: issue.Metadata, Attribution: attributionOf(issue.Author, issue.Teammate, issue.SourceActor, issue.AccountableActor),
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

// Show returns one issue by short ID, UID, or project#short ID, with its
// comments.
func (c *APIClient) Show(ctx context.Context, ref string) (Issue, error) {
	return c.showIssue(ctx, ref, false)
}

func (c *APIClient) showIssue(ctx context.Context, ref string, complete bool) (Issue, error) {
	project, id, err := splitRef(ref)
	if err != nil {
		return Issue{}, err
	}
	shown, resolved, err := c.readShow(ctx, project, id, nil, complete)
	if err != nil {
		return Issue{}, err
	}
	iss := Issue{
		ShortID: shown.Issue.ShortID, QualifiedID: resolved.Name + "#" + shown.Issue.ShortID, Title: shown.Issue.Title,
		Body: shown.Issue.Body, Status: shown.Issue.Status, UID: shown.Issue.UID, Metadata: shown.Issue.Metadata, Attribution: attributionOf(shown.Issue.Author, shown.Issue.Teammate, shown.Issue.SourceActor, shown.Issue.AccountableActor),
		ProjectUID:        cmp.Or(str(shown.Issue.ProjectUID), resolved.UID),
		Comments:          commentsFromAPI(shown.Comments),
		CommentsTruncated: shown.CommentsTruncated != nil && *shown.CommentsTruncated,
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

// show reads one issue with query, and checks Kata answered with the issue
// that was asked for.
func (c *APIClient) show(ctx context.Context, project, id string, query url.Values) (gen.ShowIssueResponseBody, projectRef, error) {
	return c.readShow(ctx, project, id, query, false)
}

func (c *APIClient) readShow(ctx context.Context, project, id string, query url.Values, complete bool) (gen.ShowIssueResponseBody, projectRef, error) {
	path, resolved, err := c.issuesPath(ctx, project)
	if err != nil {
		return gen.ShowIssueResponseBody{}, projectRef{}, err
	}
	var response apiShowResponse
	if _, err := c.tr.do(ctx, http.MethodGet, path+"/"+id, query, nil, nil, &response, http.StatusOK); err != nil {
		return gen.ShowIssueResponseBody{}, projectRef{}, err
	}
	shown := response.shown
	if complete {
		for _, comment := range shown.Comments {
			if !boundedReceiptUID(comment.UID) {
				return gen.ShowIssueResponseBody{}, projectRef{}, fmt.Errorf("%w: comment has an invalid UID", ErrInvalidResponse)
			}
		}
	}
	if complete && (!response.commentsArray || shown.CommentsTruncated != nil && *shown.CommentsTruncated) {
		return gen.ShowIssueResponseBody{}, projectRef{}, fmt.Errorf("%w: issue comments are incomplete", ErrInvalidResponse)
	}
	if !strings.EqualFold(shown.Issue.ShortID, id) && shown.Issue.UID != id {
		return gen.ShowIssueResponseBody{}, projectRef{}, fmt.Errorf("%w: Kata returned a different issue than %q", ErrInvalidResponse, id)
	}
	return shown, resolved, nil
}

func str(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func commentFromAPI(c gen.CommentOut) Comment {
	return Comment{
		UID: c.UID, Handle: str(c.Handle), IssueUID: str(c.IssueUID), Body: c.Body,
		CreatedAt: c.CreatedAt, EditedAt: c.EditedAt,
		ReplyTo: str(c.ReplyToUID), Kind: CommentKind(str(c.ReplyKind)),
		Attribution: attributionOf(c.Author, c.Teammate, c.SourceActor, c.AccountableActor),
	}
}

func commentsFromAPI(comments []gen.CommentOut) []Comment {
	if len(comments) == 0 {
		return nil
	}
	out := make([]Comment, 0, len(comments))
	for _, c := range comments {
		out = append(out, commentFromAPI(c))
	}
	return out
}

func attributionOf(author string, teammate, source, accountable *string) Attribution {
	return Attribution{Author: author, Teammate: str(teammate), SourceActor: str(source), AccountableActor: str(accountable)}
}

// Create files an issue with the idempotency key and metadata in the request.
// The project is the request's, else the workspace binding, else the default.
// A look-alike soft block is surfaced as an error, as with the kata CLI.
func (c *APIClient) Create(ctx context.Context, req CreateReq) (CreateResult, error) {
	body, err := createRequest(req)
	if err != nil {
		return CreateResult{}, err
	}
	if err := ValidateRequestSize(body); err != nil {
		return CreateResult{}, err
	}
	path, project, err := c.issuesPath(ctx, req.Project)
	if err != nil {
		return CreateResult{}, err
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
		ShortID: created.Issue.ShortID, IssueUID: created.Issue.UID,
		ProjectUID:  cmp.Or(str(created.Issue.ProjectUID), project.UID),
		Reused:      created.Reused != nil && *created.Reused,
		Attribution: attributionOf(created.Issue.Author, created.Issue.Teammate, created.Issue.SourceActor, created.Issue.AccountableActor),
	}, nil
}

// validCommentUID reports whether s has the shape of a Kata UID: a 26-character
// ULID (internal/uid/uid.go EncodedLen at v0.19.0). The server checks the rest.
func validCommentUID(s string) bool {
	if len(s) != 26 {
		return false
	}
	for _, r := range s {
		isDigit, isLower, isUpper := '0' <= r && r <= '9', 'a' <= r && r <= 'z', 'A' <= r && r <= 'Z'
		if !isDigit && !isLower && !isUpper {
			return false
		}
	}
	return true
}

// Comment appends a comment or typed reply. Typed replies are refused before
// anything is sent when the server cannot store them or when a confirm or
// refute has too little evidence; a plain comment works on any compatible
// server. The idempotency key is required so a retry replays instead of
// duplicating, and a replay (changed=false) is a success. A rejection by Kata
// is a *CommentError when it is one of the distinct cases callers handle:
// ErrDuplicateReply, ErrIdempotencyMismatch, ErrIssueClosed, ErrCommentNotFound.
// Roborev never asks Kata to force a duplicate reply.
func (c *APIClient) Comment(ctx context.Context, req CommentReq) (CommentResult, error) {
	if err := validatePathSegment(req.Issue); err != nil {
		return CommentResult{}, err
	}
	if strings.TrimSpace(req.Body) == "" {
		return CommentResult{}, fmt.Errorf("%w: comment body is required", ErrRequestRejected)
	}
	if req.IdempotencyKey == "" {
		return CommentResult{}, fmt.Errorf("%w: an idempotency key is required", ErrRequestRejected)
	}
	if req.Kind != "" && !req.Kind.valid() {
		return CommentResult{}, fmt.Errorf("%w: unknown reply kind %q", ErrRequestRejected, req.Kind)
	}
	if (req.Kind == "") != (req.ReplyTo == "") {
		return CommentResult{}, fmt.Errorf("%w: a reply needs both its kind and the comment it answers", ErrRequestRejected)
	}
	if req.ReplyTo != "" && !validCommentUID(req.ReplyTo) {
		return CommentResult{}, fmt.Errorf("%w: the comment a reply answers is named by its UID", ErrRequestRejected)
	}
	teammate := cmp.Or(req.Teammate, c.teammate)
	if err := config.ValidateKataTeammate(teammate); err != nil {
		return CommentResult{}, fmt.Errorf("%w: %w", ErrRequestRejected, err)
	}
	if err := ValidateEvidence(req.Kind, req.Body); err != nil {
		return CommentResult{}, err
	}
	path, _, err := c.issuesPath(ctx, req.Project)
	if err != nil {
		return CommentResult{}, err
	}
	if req.Kind != "" {
		info, err := c.Info(ctx)
		if err != nil {
			return CommentResult{}, err
		}
		if !info.SupportsReplies() {
			return CommentResult{}, ErrRepliesUnsupported
		}
	}
	body := gen.CommentRequestBody{Actor: new(WriteActor), Body: req.Body, Teammate: &teammate}
	if req.Kind != "" {
		body.Kind = new(gen.CommentRequestBodyKind(req.Kind))
		body.ReplyTo = &req.ReplyTo
	}
	var posted gen.CommentResponseBody
	header := http.Header{"Idempotency-Key": {req.IdempotencyKey}}
	if _, err := c.tr.do(ctx, http.MethodPost, path+"/"+req.Issue+"/comments", nil, body, header, &posted, http.StatusOK, http.StatusCreated); err != nil {
		return CommentResult{}, classifyCommentError(err)
	}
	comment := commentFromAPI(commentOutOf(posted.Comment))
	if !boundedReceiptUID(comment.UID) {
		return CommentResult{}, fmt.Errorf("%w: comment has an invalid UID", ErrInvalidResponse)
	}
	// A server that ignored the reply fields would have stored a plain comment.
	if comment.Kind != req.Kind || !strings.EqualFold(comment.ReplyTo, req.ReplyTo) {
		return CommentResult{}, fmt.Errorf("%w: Kata stored a %q reply to %q instead of the %q reply to %q that was sent",
			ErrInvalidResponse, comment.Kind, comment.ReplyTo, req.Kind, req.ReplyTo)
	}
	return CommentResult{
		UID: comment.UID, Body: comment.Body, EditedAt: comment.EditedAt, Changed: posted.Changed, Kind: comment.Kind, ReplyTo: comment.ReplyTo, Attribution: comment.Attribution,
	}, nil
}

// commentOutOf views the comment a write returned as the shape a read returns.
func commentOutOf(c gen.Comment) gen.CommentOut {
	return gen.CommentOut{
		AccountableActor: c.AccountableActor, Author: c.Author, Body: c.Body, CreatedAt: c.CreatedAt, EditedAt: c.EditedAt,
		ReplyKind: c.ReplyKind, ReplyToUID: c.ReplyToUID, SourceActor: c.SourceActor, Teammate: c.Teammate, UID: c.UID,
	}
}

// Thread reads the comment named by req.Root and every reply beneath it,
// including replies posted on other issues of the project. Kata returns at most
// 50 comments, keeps the root, and sets Truncated when it left some out. A
// server that predates typed replies would ignore the selection and answer with
// the whole issue, so it is refused.
func (c *APIClient) Thread(ctx context.Context, req ThreadReq) (Thread, error) {
	if !validCommentUID(req.Root) {
		return Thread{}, fmt.Errorf("%w: a thread is read from its root comment's UID", ErrRequestRejected)
	}
	project, id, err := splitRef(req.Issue)
	if err != nil {
		return Thread{}, err
	}
	info, err := c.Info(ctx)
	if err != nil {
		return Thread{}, err
	}
	if !info.SupportsReplies() {
		return Thread{}, ErrRepliesUnsupported
	}
	shown, _, err := c.show(ctx, project, id, url.Values{"thread": {req.Root}})
	if err != nil {
		return Thread{}, classifyCommentError(err)
	}
	thread := Thread{
		Comments:  commentsFromAPI(shown.Comments),
		Truncated: shown.CommentsTruncated != nil && *shown.CommentsTruncated,
	}
	if !slices.ContainsFunc(thread.Comments, func(cm Comment) bool { return strings.EqualFold(cm.UID, req.Root) }) {
		return Thread{}, fmt.Errorf("%w: the thread does not contain its root comment", ErrInvalidResponse)
	}
	return thread, nil
}
