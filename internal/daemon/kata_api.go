package daemon

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/roborev/internal/kata"
)

// The daemon owns the Kata connection and its credential. These endpoints let
// the CLI, including one that talks to a remote daemon, read and write Kata
// through it when [integrations.kata] is enabled. A CLI never receives the
// endpoint or the credential, and the daemon never runs the kata CLI for them.

// KataStatusInput holds query parameters for GET /api/kata/status.
type KataStatusInput struct {
	Repo    string `query:"repo" doc:"Repository root on the daemon host whose .kata.local.toml or .kata.toml binding selects the project"`
	Project string `query:"project" doc:"Project to check instead of the repository's binding or the default project"`
}

// KataStatusBody is the daemon's view of its Kata connection.
type KataStatusBody struct {
	Enabled          bool   `json:"enabled" doc:"Whether [integrations.kata] is enabled on the daemon"`
	State            string `json:"state" doc:"Connection state: disabled, unreachable, authentication_required, incompatible, project_not_found, or ready"`
	Project          string `json:"project,omitempty" doc:"Project in effect: the requested one, the repository's binding, or the default project"`
	DefaultProject   string `json:"default_project,omitempty" doc:"The configured default project"`
	Teammate         string `json:"teammate,omitempty" doc:"Teammate name for roborev's own Kata writes"`
	ServerVersion    string `json:"server_version,omitempty" doc:"Kata server version"`
	APISchemaVersion string `json:"api_schema_version,omitempty" doc:"Kata API schema version"`
	SupportsReplies  bool   `json:"supports_replies" doc:"Whether the server stores typed comment replies (API schema 0.28.0 or later)"`
	Message          string `json:"message" doc:"Plain description of the state"`
	SecurityNote     string `json:"security_note,omitempty" doc:"Reduced security guarantee on this platform, if any"`
}

// KataStatusOutput is the response for GET /api/kata/status.
type KataStatusOutput struct{ Body KataStatusBody }

// KataLinkPeer identifies a related Kata issue.
type KataLinkPeer struct {
	ShortID     string `json:"short_id"`
	Project     string `json:"project,omitempty"`
	QualifiedID string `json:"qualified_id,omitempty"`
	Status      string `json:"status,omitempty"`
}

// KataIssue is the subset of a Kata issue roborev reads.
type KataIssue struct {
	ShortID     string         `json:"short_id"`
	QualifiedID string         `json:"qualified_id"`
	Title       string         `json:"title"`
	Body        string         `json:"body"`
	Status      string         `json:"status"`
	Labels      []string       `json:"labels"`
	Parent      *KataLinkPeer  `json:"parent,omitempty"`
	Blocks      []KataLinkPeer `json:"blocks,omitempty"`
	BlockedBy   []KataLinkPeer `json:"blocked_by,omitempty"`
	Related     []KataLinkPeer `json:"related,omitempty"`
}

// KataIssuesInput holds query parameters for GET /api/kata/issues.
type KataIssuesInput struct {
	Project   string `query:"project" doc:"Project to read; empty selects the default project"`
	Status    string `query:"status" doc:"Issue status filter: open (default), closed, or all"`
	Unlimited bool   `query:"unlimited" doc:"Read the complete set instead of the first 200 issues"`
}

// KataIssuesOutput is the response for GET /api/kata/issues.
type KataIssuesOutput struct {
	Body struct {
		Issues []KataIssue `json:"issues"`
	}
}

// KataIssueInput holds query parameters for GET /api/kata/issue.
type KataIssueInput struct {
	Ref     string `query:"ref" required:"true" doc:"Issue short ID, UID, or project#short ID"`
	Project string `query:"project" doc:"Project for a bare ref; empty selects the default project"`
}

// KataIssueOutput is the response for GET /api/kata/issue.
type KataIssueOutput struct{ Body KataIssue }

// KataCreateRequest files one Kata issue as roborev.
type KataCreateRequest struct {
	Title          string   `json:"title" doc:"Issue title"`
	Body           string   `json:"body,omitempty" doc:"Issue body"`
	Project        string   `json:"project,omitempty" doc:"Project to file into; empty selects the default project"`
	Labels         []string `json:"labels,omitempty"`
	Priority       *int     `json:"priority,omitempty" doc:"Priority 0 to 4"`
	IdempotencyKey string   `json:"idempotency_key,omitempty" doc:"Makes a retry return the issue the first request created"`
}

// KataCreateInput is the request for POST /api/kata/issues.
type KataCreateInput struct{ Body KataCreateRequest }

// KataCreateResponse reports the filed issue and who Kata says wrote it.
type KataCreateResponse struct {
	ShortID string `json:"short_id"`
	Reused  bool   `json:"reused" doc:"Whether Kata answered with an issue an earlier request under the same key created"`
	Author  string `json:"author,omitempty" doc:"The actor Kata recorded; an authenticated credential overrides roborev's requested actor"`
}

// KataCreateOutput is the response for POST /api/kata/issues.
type KataCreateOutput struct{ Body KataCreateResponse }

// kataStatus evaluates the connection for a repository on the daemon host, or
// for an explicit project. It never fails: a broken connection is a state.
func (s *Server) kataStatus(ctx context.Context, repo, project string) KataStatusBody {
	cfg := s.configWatcher.Config().Integrations.Kata
	body := KataStatusBody{Enabled: cfg.Enabled, DefaultProject: cfg.DefaultProject, Teammate: cfg.ResolvedTeammate()}
	var opts []kata.APIOption
	if repo == "" {
		opts = append(opts, kata.WithoutWorkspace())
	}
	status := kata.EvaluateWith(ctx, repo, cfg, project, opts...)
	body.State = string(status.State)
	body.Project = status.Project
	body.ServerVersion = status.ServerVersion
	body.APISchemaVersion = status.APISchemaVersion
	body.SupportsReplies = status.SupportsReplies()
	body.Message = status.Message
	body.SecurityNote = status.SecurityNote
	return body
}

func (s *Server) humaKataStatus(ctx context.Context, in *KataStatusInput) (*KataStatusOutput, error) {
	return &KataStatusOutput{Body: s.kataStatus(ctx, strings.TrimSpace(in.Repo), strings.TrimSpace(in.Project))}, nil
}

// kataProxyClient builds the API client for a proxied call. It ignores the
// daemon's own directory: the caller names the project, else the default one.
func (s *Server) kataProxyClient() (*kata.APIClient, error) {
	cfg := s.configWatcher.Config().Integrations.Kata
	if !cfg.Enabled {
		return nil, huma.Error409Conflict("the Kata API connection is not enabled; set [integrations.kata] in the daemon's config")
	}
	client, err := kata.NewAPIClient("", cfg, kata.WithoutWorkspace())
	if err != nil {
		return nil, huma.NewError(http.StatusBadGateway, "the Kata connection is misconfigured: "+err.Error())
	}
	return client, nil
}

// kataProxyError turns a Kata failure into the daemon's response. Kata's own
// error code stays in the message so a caller can tell a replayed key from a
// rejected one.
func kataProxyError(err error) error {
	switch {
	case errors.Is(err, kata.ErrNoBinding):
		return huma.Error404NotFound("no Kata project is selected: name one, or set integrations.kata.default_project")
	case errors.Is(err, kata.ErrProjectNotFound), errors.Is(err, kata.ErrNotFound):
		return huma.Error404NotFound(err.Error())
	case errors.Is(err, kata.ErrRequestRejected):
		return huma.Error400BadRequest(err.Error())
	case errors.Is(err, kata.ErrConflict):
		return huma.Error409Conflict(err.Error())
	}
	return huma.NewError(http.StatusBadGateway, "the daemon could not complete the Kata request: "+err.Error())
}

func kataIssueFromClient(issue kata.Issue) KataIssue {
	return KataIssue{
		ShortID: issue.ShortID, QualifiedID: issue.QualifiedID, Title: issue.Title, Body: issue.Body,
		Status: issue.Status, Labels: issue.Labels, Parent: kataPeer(issue.Parent),
		Blocks: kataPeers(issue.Blocks), BlockedBy: kataPeers(issue.BlockedBy), Related: kataPeers(issue.Related),
	}
}

func kataPeer(peer *kata.LinkPeer) *KataLinkPeer {
	if peer == nil {
		return nil
	}
	return &KataLinkPeer{ShortID: peer.ShortID, Project: peer.Project, QualifiedID: peer.QualifiedID, Status: peer.Status}
}

func kataPeers(peers []kata.LinkPeer) []KataLinkPeer {
	var out []KataLinkPeer
	for i := range peers {
		out = append(out, *kataPeer(&peers[i]))
	}
	return out
}

func (s *Server) humaKataIssues(ctx context.Context, in *KataIssuesInput) (*KataIssuesOutput, error) {
	switch in.Status {
	case "", "open", "closed", "all":
	default:
		return nil, huma.Error400BadRequest("status must be open, closed, or all")
	}
	client, err := s.kataProxyClient()
	if err != nil {
		return nil, err
	}
	issues, err := client.ListProject(ctx, strings.TrimSpace(in.Project), kata.ListOpts{Status: in.Status, Unlimited: in.Unlimited})
	if err != nil {
		return nil, kataProxyError(err)
	}
	out := &KataIssuesOutput{}
	out.Body.Issues = make([]KataIssue, 0, len(issues))
	for _, issue := range issues {
		out.Body.Issues = append(out.Body.Issues, kataIssueFromClient(issue))
	}
	return out, nil
}

func (s *Server) humaKataIssue(ctx context.Context, in *KataIssueInput) (*KataIssueOutput, error) {
	ref := strings.TrimSpace(in.Ref)
	if project := strings.TrimSpace(in.Project); project != "" && !strings.Contains(ref, "#") {
		ref = project + "#" + ref
	}
	client, err := s.kataProxyClient()
	if err != nil {
		return nil, err
	}
	issue, err := client.Show(ctx, ref)
	if err != nil {
		return nil, kataProxyError(err)
	}
	return &KataIssueOutput{Body: kataIssueFromClient(issue)}, nil
}

func (s *Server) humaKataCreate(ctx context.Context, in *KataCreateInput) (*KataCreateOutput, error) {
	req := in.Body
	client, err := s.kataProxyClient()
	if err != nil {
		return nil, err
	}
	result, err := client.Create(ctx, kata.CreateReq{
		Title: req.Title, Body: req.Body, Project: strings.TrimSpace(req.Project), Labels: req.Labels,
		Priority: req.Priority, IdempotencyKey: req.IdempotencyKey,
	})
	if err != nil {
		return nil, kataProxyError(err)
	}
	return &KataCreateOutput{Body: KataCreateResponse{ShortID: result.ShortID, Reused: result.Reused, Author: result.Attribution.Author}}, nil
}
