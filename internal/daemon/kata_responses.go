package daemon

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/findingid"
	"go.kenn.io/roborev/internal/kata"
	"go.kenn.io/roborev/internal/katapublish"
	"go.kenn.io/roborev/internal/kataresponse"
	"go.kenn.io/roborev/internal/storage"
)

// KataResponseRequest has a distinct schema name alongside agent-hook requests.
type KataResponseRequest kataresponse.Request

type (
	KataResponseInput       struct{ Body KataResponseRequest }
	KataResponseOutput      struct{ Body storage.KataResponseStatus }
	KataResponseStatusInput struct {
		OperationID string `query:"operation_id" required:"true"`
	}
)

func (s *Server) humaKataResponseStatus(ctx context.Context, in *KataResponseStatusInput) (*KataResponseOutput, error) {
	if err := kataresponse.ValidateIdempotencyKey(in.OperationID); err != nil {
		return nil, huma.Error400BadRequest(err.Error())
	}
	status, err := s.db.KataResponseStatus(ctx, in.OperationID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, huma.Error404NotFound("response operation not found")
	}
	if err != nil {
		return nil, huma.Error500InternalServerError("read response operation failed")
	}
	job, err := s.db.GetJobByID(status.JobID)
	if err != nil {
		return nil, huma.Error404NotFound("review job unavailable")
	}
	if !remoteAllowsJob(ctx, job) {
		return nil, huma.Error403Forbidden("review repository denied")
	}
	return &KataResponseOutput{Body: status}, nil
}

func (s *Server) humaKataResponse(ctx context.Context, in *KataResponseInput) (*KataResponseOutput, error) {
	intent := kataresponse.Request(in.Body)
	if err := intent.Validate(); err != nil {
		return nil, huma.Error400BadRequest(err.Error())
	}
	resumed, err := s.db.ResumeKataResponse(ctx, intent)
	if errors.Is(err, storage.ErrResponseChanged) {
		return nil, huma.Error409Conflict(err.Error())
	}
	if err != nil {
		return nil, huma.Error500InternalServerError("read response operation failed")
	}
	if resumed != nil {
		job, err := s.db.GetJobByID(resumed.JobID)
		if err != nil {
			return nil, huma.Error404NotFound("review job unavailable")
		}
		if !remoteAllowsJob(ctx, job) {
			return nil, huma.Error403Forbidden("review repository denied")
		}
		return &KataResponseOutput{Body: *resumed}, nil
	}
	cfg := s.configWatcher.Config().Integrations.Kata
	if !cfg.Enabled {
		return nil, huma.Error409Conflict("typed finding responses require the daemon Kata API connection")
	}
	snapshot, capture, err := s.resolveKataResponseTarget(ctx, intent, cfg)
	if err != nil {
		return nil, err
	}
	job, err := s.kataPublicationJob(ctx, snapshot.JobID)
	if err != nil {
		return nil, err
	}
	if intent.Replacement != "" {
		replacement, err := s.db.FindingSnapshot(ctx, intent.Replacement)
		if err != nil {
			return nil, huma.Error400BadRequest("replacement finding unavailable")
		}
		replacementJob, err := s.db.GetJobByID(replacement.JobID)
		if err != nil || replacementJob.RepoID != job.RepoID || replacement.Selector == snapshot.Selector {
			return nil, huma.Error400BadRequest("replacement must name another finding in the same repository")
		}
	}
	fingerprint, err := intent.Fingerprint()
	if err != nil {
		return nil, huma.Error500InternalServerError("fingerprint response failed")
	}
	capture.OperationID = intent.OperationID
	response := &capture.Responses[0]
	response.OperationID = intent.OperationID
	response.Fingerprint = fingerprint
	response.Kind = intent.Kind
	response.Body = intent.Body
	if capture.Policy == "summary" {
		response.Body = fmt.Sprintf("%s response for finding %s. Evidence is available locally under operation %s.", intent.Kind, snapshot.Selector, intent.OperationID)
	}
	if intent.Replacement != "" {
		response.Body += "\n\nReplacement: " + intent.Replacement
	}
	if intent.TargetComment != "" {
		// Continuations are resolved once against the frozen issue, never through
		// current repository/project bindings. Delivery checks lineage again.
		issue, err := frozenResponseIssue(ctx, cfg, *capture)
		if err != nil {
			return nil, err
		}
		target, err := kata.ResolveCommentReference(issue, intent.TargetComment)
		if err == nil {
			target, err = katapublish.ContinuationTarget(issue, response.Root, target)
		}
		if err != nil {
			return nil, huma.Error400BadRequest("target-comment must descend from the finding root")
		}
		response.Target = target
	}
	status, err := s.db.EnqueueKataResponse(ctx, intent, *capture)
	if errors.Is(err, storage.ErrResponseChanged) {
		return nil, huma.Error409Conflict(err.Error())
	}
	if err != nil {
		return nil, huma.Error500InternalServerError("persist response operation failed")
	}
	return &KataResponseOutput{Body: status}, nil
}

func frozenResponseIssue(ctx context.Context, cfg config.KataIntegrationConfig, request katapublish.Request) (kata.Issue, error) {
	connection, err := kataPublicationConnection(cfg, request)
	if err != nil {
		return kata.Issue{}, huma.Error409Conflict("captured Kata connection unavailable")
	}
	api, err := kata.NewAPIClient("", connection, kata.WithoutWorkspace())
	if err != nil {
		return kata.Issue{}, huma.Error409Conflict("Kata connection unavailable")
	}
	defer api.CloseIdleConnections()
	if api.Endpoint() != request.Endpoint {
		return kata.Issue{}, huma.Error409Conflict("captured Kata endpoint changed")
	}
	issue, err := api.ShowComplete(ctx, request.Project+"#"+request.IssueUID)
	if err != nil || issue.UID != request.IssueUID || issue.ProjectUID != request.ProjectUID {
		return kata.Issue{}, huma.Error409Conflict("complete frozen finding thread unavailable")
	}
	return issue, nil
}

func (s *Server) resolveKataResponseTarget(ctx context.Context, intent kataresponse.Request, cfg config.KataIntegrationConfig) (storage.FindingSnapshot, *katapublish.Request, error) {
	if id, err := findingid.Parse(intent.FindingID); err == nil {
		selector := id.String()
		snapshot, err := s.db.FindingSnapshot(ctx, selector)
		if err != nil {
			return snapshot, nil, huma.Error404NotFound("finding snapshot unavailable")
		}
		if intent.JobID != 0 && intent.JobID != snapshot.JobID {
			return snapshot, nil, huma.Error400BadRequest("finding belongs to another job")
		}
		if _, err := s.kataPublicationJob(ctx, snapshot.JobID); err != nil {
			return snapshot, nil, err
		}
		capture, err := s.frozenFindingResponse(ctx, snapshot.JobID, selector, cfg.ResolvedTeammate())
		return snapshot, capture, err
	}
	roots, err := s.db.KataResponseRoots(ctx)
	if err != nil {
		return storage.FindingSnapshot{}, nil, huma.Error500InternalServerError("read frozen finding roots failed")
	}
	var selected storage.FindingSnapshot
	var capture *katapublish.Request
	qualification, _ := kata.ParseCommentReference(intent.FindingID) // Intent validation already checked its syntax.
	issues := map[string]kata.Issue{}
	for _, root := range roots {
		if intent.JobID != 0 && root.JobID != intent.JobID || root.State.IssueUID == "" || root.State.ProjectUID == "" {
			continue
		}
		if qualification.Project != "" && qualification.Project != root.Request.Project {
			continue
		}
		frozenIssue := root.State.IssueRef
		if hash := strings.LastIndexByte(frozenIssue, '#'); hash >= 0 {
			frozenIssue = frozenIssue[hash+1:]
		}
		// A full UID alone does not reveal the captured issue's display ref.
		// Exclude only destinations the explicit qualification rules out.
		if qualification.Issue != "" && frozenIssue != "" && !strings.EqualFold(frozenIssue, root.State.IssueUID) && !strings.EqualFold(qualification.Issue, frozenIssue) {
			continue
		}
		job, err := s.db.GetJobByID(root.JobID)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return selected, nil, huma.Error500InternalServerError("read mapped review job failed")
		}
		if !remoteAllowsJob(ctx, job) {
			continue
		}
		destination := root.Request
		destination.IssueUID, destination.ProjectUID = root.State.IssueUID, root.State.ProjectUID
		key := destination.Endpoint + "\x00" + destination.ProjectUID + "\x00" + destination.IssueUID + "\x00" + destination.CredentialFingerprint
		issue, ok := issues[key]
		if !ok {
			issue, err = frozenResponseIssue(ctx, cfg, destination)
			if err != nil {
				return selected, nil, err
			}
			issues[key] = issue
		}
		target, err := kata.ResolveCommentReference(issue, intent.FindingID)
		if errors.Is(err, kata.ErrCommentNotFound) {
			continue
		}
		if err != nil {
			return selected, nil, huma.Error409Conflict("comment reference is unavailable or ambiguous")
		}
		for _, entry := range root.Request.Entries {
			if _, err := katapublish.ContinuationTarget(issue, root.State.Receipts[entry.Selector].UID, target); err != nil {
				continue
			}
			if capture != nil {
				return selected, nil, huma.Error409Conflict("comment reference has multiple finding mappings")
			}
			selected, err = s.db.FindingSnapshot(ctx, entry.Selector)
			if err != nil || selected.JobID != root.JobID {
				return selected, nil, huma.Error409Conflict("mapped finding snapshot unavailable")
			}
			capture, err = frozenFindingResponseFromHandoff(root, entry, cfg.ResolvedTeammate())
			if err != nil {
				return selected, nil, err
			}
			capture.Responses[0].Target = target
		}
	}
	if capture == nil {
		return selected, nil, huma.Error409Conflict("comment reference has no unique completed finding mapping")
	}
	return selected, capture, nil
}

func frozenFindingResponseFromHandoff(handoff storage.KataHandoff, entry katapublish.Entry, teammate string) (*katapublish.Request, error) {
	root := handoff.State.Receipts[entry.Selector].UID
	if root == "" || handoff.State.IssueUID == "" || handoff.State.ProjectUID == "" || !handoff.State.Done {
		return nil, huma.Error409Conflict("completed publication root manifest unavailable")
	}
	entries, err := katapublish.PublicationManifest(handoff.Request, handoff.State)
	if err != nil {
		return nil, huma.Error409Conflict("completed publication root manifest unavailable")
	}
	request := handoff.Request
	request.PublicationEntries = entries
	request.PublicationOperationID = request.OperationID
	request.PublicationFingerprint = handoff.Fingerprint
	request.PublicationProjectUID = request.ProjectUID
	request.RootAttributionHash = katapublish.AttributionFingerprint(handoff.State.Receipts[entry.Selector].Attribution)
	request.ProjectUID = handoff.State.ProjectUID
	request.IssueUID = handoff.State.IssueUID
	request.Create = kata.CreateReq{}
	request.Entries = []katapublish.Entry{entry}
	request.Responses = []katapublish.Response{{Root: root, Target: root, Teammate: teammate}}
	request.Teammate = teammate
	return &request, nil
}

func (s *Server) frozenFindingResponse(ctx context.Context, jobID int64, selector, teammate string) (*katapublish.Request, error) {
	handoffs, err := s.db.KataHandoffs(ctx, jobID)
	if err != nil {
		return nil, huma.Error500InternalServerError("read finding mapping failed")
	}
	var result *katapublish.Request
	for _, handoff := range handoffs {
		if len(handoff.Request.Responses) > 0 || !handoff.State.Done {
			continue
		}
		for _, entry := range handoff.Request.Entries {
			if entry.Selector != selector {
				continue
			}
			if result != nil {
				return nil, huma.Error409Conflict("finding has multiple publication destinations; response mapping is ambiguous")
			}
			result, err = frozenFindingResponseFromHandoff(handoff, entry, teammate)
			if err != nil {
				return nil, err
			}
		}
	}
	if result == nil {
		return nil, huma.Error409Conflict("finding has no completed Kata comment mapping; retry after publication completes")
	}
	return result, nil
}

func (s *Server) captureReassessmentDelivery(ctx context.Context, parent *storage.ReviewJob, selector string, cfg config.KataIntegrationConfig) (*katapublish.Request, error) {
	teammate := cfg.ReassessmentTeammate(parent.Agent)
	if err := config.ValidateKataTeammate(teammate); err != nil {
		return nil, huma.Error400BadRequest("combined reassessment teammate-agent is invalid")
	}
	return s.frozenFindingResponse(ctx, parent.ID, strings.TrimSpace(selector), teammate)
}
