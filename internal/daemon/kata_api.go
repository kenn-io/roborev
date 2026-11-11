package daemon

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/kata"
)

// The daemon owns the Kata connection and its credential. These endpoints let
// the CLI, including one that talks to a remote daemon, read Kata through it
// when [integrations.kata] is enabled. A CLI never receives the
// endpoint or the credential, and the daemon never runs the kata CLI for them.

// KataStatusInput holds query parameters for GET /api/kata/status.
type KataStatusInput struct {
	Project                string `query:"project" doc:"Project to check instead of the default project"`
	WorkspaceAliasIdentity string `query:"workspace_alias_identity" doc:"Registered workspace alias identity; use with workspace_alias_kind instead of project"`
	WorkspaceAliasKind     string `query:"workspace_alias_kind" doc:"Registered workspace alias kind: git or local"`
}

// KataStatusBody is the daemon's view of its Kata connection.
type KataStatusBody struct {
	Enabled          bool   `json:"enabled" doc:"Whether [integrations.kata] is enabled on the daemon"`
	State            string `json:"state" doc:"Connection state: disabled, unreachable, authentication_required, incompatible, project_not_found, or ready"`
	Project          string `json:"project,omitempty" doc:"Project in effect: the requested one or the default project"`
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
	UID              string                    `json:"uid,omitempty"`
	Author           string                    `json:"author,omitempty"`
	Teammate         *string                   `json:"teammate,omitempty"`
	Project          string                    `json:"project,omitempty"`
	Generation       uint64                    `json:"generation"`
	ReplyTeammate    string                    `json:"reply_teammate,omitempty"`
	ProjectID        int64                     `json:"project_id,omitempty"`
	IssueID          int64                     `json:"issue_id,omitempty"`
	ProjectUID       string                    `json:"project_uid,omitempty"`
	DestinationID    string                    `json:"destination_id,omitempty"`
	Metadata         map[string]any            `json:"metadata,omitempty"`
	Comments         []KataComment             `json:"comments,omitempty"`
	CommentsComplete *bool                     `json:"comments_complete,omitempty"`
	Envelope         *kata.PublicationEnvelope `json:"publication_envelope,omitempty"`
	Roots            []KataRoot                `json:"roots,omitempty"`
	ShortID          string                    `json:"short_id"`
	QualifiedID      string                    `json:"qualified_id"`
	Title            string                    `json:"title"`
	Body             string                    `json:"body"`
	Status           string                    `json:"status"`
	Labels           []string                  `json:"labels"`
	Parent           *KataLinkPeer             `json:"parent,omitempty"`
	Blocks           []KataLinkPeer            `json:"blocks,omitempty"`
	BlockedBy        []KataLinkPeer            `json:"blocked_by,omitempty"`
	Related          []KataLinkPeer            `json:"related,omitempty"`
}

// KataIssuesInput holds query parameters for GET /api/kata/issues.
type KataIssuesInput struct {
	Project                string `query:"project" doc:"Project to read; empty selects the default project"`
	WorkspaceAliasIdentity string `query:"workspace_alias_identity" doc:"Registered workspace alias identity; use with workspace_alias_kind instead of project"`
	WorkspaceAliasKind     string `query:"workspace_alias_kind" doc:"Registered workspace alias kind: git or local"`
	Status                 string `query:"status" doc:"Issue status filter: open (default), closed, or all"`
	Unlimited              bool   `query:"unlimited" doc:"Read the complete set instead of the first 200 issues"`
}

// KataIssuesOutput is the response for GET /api/kata/issues.
type KataIssuesOutput struct {
	Body struct {
		Issues []KataIssue `json:"issues"`
	}
}

// KataIssueInput holds query parameters for GET /api/kata/issue.
type KataIssueInput struct {
	Ref                    string `query:"ref" required:"true" doc:"Issue short ID, UID, or project#short ID"`
	Project                string `query:"project" doc:"Project for a bare ref; empty selects the default project"`
	WorkspaceAliasIdentity string `query:"workspace_alias_identity" doc:"Registered workspace alias identity; use with workspace_alias_kind instead of project"`
	WorkspaceAliasKind     string `query:"workspace_alias_kind" doc:"Registered workspace alias kind: git or local"`
}

// KataIssueOutput is the response for GET /api/kata/issue.
type KataIssueOutput struct{ Body KataIssue }

// kataStatus evaluates the connection for an explicit project or alias.
// It never fails: a broken connection is a state.
func (s *Server) kataStatus(ctx context.Context, project string, alias *kata.ProjectAlias) KataStatusBody {
	cfg := s.configWatcher.Config().Integrations.Kata
	body := KataStatusBody{Enabled: cfg.Enabled, DefaultProject: cfg.DefaultProject, Teammate: cfg.ResolvedTeammate()}
	opts := []kata.APIOption{kata.WithoutWorkspace()}
	if alias != nil {
		opts = append(opts, kata.WithWorkspaceAlias(*alias))
	}
	status := kata.EvaluateWith(ctx, "", cfg, project, opts...)
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
	project := strings.TrimSpace(in.Project)
	alias, err := kataWorkspaceAlias(in.WorkspaceAliasIdentity, in.WorkspaceAliasKind, project)
	if err != nil {
		return nil, err
	}
	return &KataStatusOutput{Body: s.kataStatus(ctx, project, alias)}, nil
}

// kataProxyClient builds the API client for a proxied call. It ignores the
// daemon's own directory and uses the caller's project name, workspace alias,
// or the configured default.
func (s *Server) kataProxyClient(alias *kata.ProjectAlias) (*kata.APIClient, error) {
	return kataProxyClientFor(s.configWatcher.Config().Integrations.Kata, alias)
}

func kataProxyClientFor(cfg config.KataIntegrationConfig, alias *kata.ProjectAlias) (*kata.APIClient, error) {
	if !cfg.Enabled {
		return nil, huma.Error409Conflict("the Kata API connection is not enabled; set [integrations.kata] in the daemon's config")
	}
	opts := []kata.APIOption{kata.WithoutWorkspace()}
	if alias != nil {
		opts = append(opts, kata.WithWorkspaceAlias(*alias))
	}
	client, err := kata.NewAPIClient("", cfg, opts...)
	if err != nil {
		return nil, huma.NewError(http.StatusBadGateway, "the Kata connection is misconfigured: "+err.Error())
	}
	return client, nil
}

func kataWorkspaceAlias(identity, kind, project string) (*kata.ProjectAlias, error) {
	if identity == "" && kind == "" {
		return nil, nil
	}
	if strings.TrimSpace(identity) == "" || strings.TrimSpace(kind) == "" {
		return nil, huma.Error400BadRequest("workspace alias identity and kind must be provided together")
	}
	if strings.TrimSpace(project) != "" {
		return nil, huma.Error400BadRequest("workspace alias and project cannot be provided together")
	}
	if kind != "git" && kind != "local" {
		return nil, huma.Error400BadRequest("workspace alias kind must be git or local")
	}
	return &kata.ProjectAlias{Identity: identity, Kind: kind}, nil
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
	project := strings.TrimSpace(in.Project)
	alias, err := kataWorkspaceAlias(in.WorkspaceAliasIdentity, in.WorkspaceAliasKind, project)
	if err != nil {
		return nil, err
	}
	client, err := s.kataProxyClient(alias)
	if err != nil {
		return nil, err
	}
	issues, err := client.ListProject(ctx, project, kata.ListOpts{Status: in.Status, Unlimited: in.Unlimited})
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
	project := strings.TrimSpace(in.Project)
	alias, err := kataWorkspaceAlias(in.WorkspaceAliasIdentity, in.WorkspaceAliasKind, project)
	if err != nil {
		return nil, err
	}
	if alias != nil && strings.Contains(ref, "#") {
		return nil, huma.Error400BadRequest("a qualified issue ref and workspace alias cannot be provided together")
	}
	if project != "" && !strings.Contains(ref, "#") {
		ref = project + "#" + ref
	}
	cfg, generation := s.configWatcher.kataSnapshot()
	client, err := kataProxyClientFor(cfg, alias)
	if err != nil {
		return nil, err
	}
	projectName := project
	issueRef := ref
	if named, rest, qualified := strings.Cut(ref, "#"); qualified {
		if named == "" {
			return nil, huma.Error400BadRequest("empty project in issue reference")
		}
		if project != "" && project != named {
			return nil, huma.Error400BadRequest("conflicting project selectors")
		}
		projectName, issueRef = named, rest
	}
	destination, err := client.ResolvePublicationProject(ctx, projectName)
	if errors.Is(err, kata.ErrIncompatible) || errors.Is(err, kata.ErrProjectNotFound) {
		issue, showErr := client.Show(ctx, ref)
		if showErr != nil {
			return nil, kataProxyError(showErr)
		}
		return &KataIssueOutput{Body: kataIssueFromClient(issue)}, nil
	}
	if err != nil {
		return nil, kataProxyError(err)
	}
	full, err := client.ShowPublicationIssue(ctx, destination, issueRef)
	if err != nil {
		return nil, kataProxyError(err)
	}
	result := kataIssueFromClient(full.Legacy)
	result.UID = full.Issue.UID
	result.IssueID = full.Issue.ID
	result.ProjectID = destination.ID
	result.Project = destination.Name
	result.Generation = generation
	result.ReplyTeammate = cfg.ResolvedTeammate()
	result.Author, result.Teammate = full.Issue.Author, full.Issue.Teammate
	result.ProjectUID = destination.UID
	result.DestinationID = kataDestinationID(client.PublicationEndpoint(), destination.UID)
	result.Metadata = full.Issue.Metadata
	result.CommentsComplete = new(!full.Truncated)
	for _, comment := range full.Comments {
		result.Comments = append(result.Comments, kataComment(comment))
	}
	if envelope, err := kata.ReadPublicationEnvelope(full.Issue.Metadata); err == nil {
		result.Envelope = &envelope
		for _, root := range kata.ReadPublicationRoots(envelope, full) {
			item := KataRoot{Position: root.Position, EndPosition: root.EndPosition, State: root.State}
			if root.Comment != nil {
				comment := kataComment(*root.Comment)
				item.Comment = &comment
			}
			result.Roots = append(result.Roots, item)
		}
	}
	return &KataIssueOutput{Body: result}, nil
}
