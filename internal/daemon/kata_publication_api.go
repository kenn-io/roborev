package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"uuid"

	"github.com/danielgtaylor/huma/v2"
	gen "go.kenn.io/kata/pkg/client/generated"

	"go.kenn.io/roborev/internal/kata"
	"go.kenn.io/roborev/internal/prompt"
	"go.kenn.io/roborev/internal/storage"
)

func (s *Server) readKataPromptContext(ctx context.Context, review *storage.Review) (any, error) {
	if !s.configWatcher.Config().Integrations.Kata.Enabled {
		return nil, nil
	}
	out, err := s.humaKataPublication(ctx, &KataPublicationInput{JobID: review.JobID, ReviewUUID: review.UUID.String()})
	if err != nil {
		var status huma.StatusError
		if errors.As(err, &status) && (status.GetStatus() == http.StatusUnauthorized || status.GetStatus() == http.StatusForbidden || status.GetStatus() == http.StatusRequestTimeout || status.GetStatus() == http.StatusGatewayTimeout) {
			return nil, fmt.Errorf("%w: %w", prompt.ErrKataContextFatal, err)
		}
		return nil, err
	}
	if out.Body.State != "unavailable" || out.Body.DocumentHash != "" {
		if err := prompt.ValidateKataContextSource(review, out.Body.SourceDocument, out.Body.DocumentHash); err != nil {
			return nil, err
		}
	}
	if out.Body.State == "missing" {
		return nil, nil
	}
	return out.Body, nil
}

type KataComment struct {
	UID         string     `json:"uid"`
	Handle      *string    `json:"handle,omitempty"`
	Body        string     `json:"body"`
	Author      string     `json:"author"`
	Teammate    *string    `json:"teammate,omitempty"`
	ReplyToUID  *string    `json:"reply_to_uid,omitempty"`
	ReplyKind   *string    `json:"reply_kind,omitempty"`
	EditedAt    *time.Time `json:"edited_at,omitempty"`
	RootUID     string     `json:"root_uid,omitempty"`
	Lineage     string     `json:"lineage,omitempty"`
	History     string     `json:"history,omitempty"`
}
type KataRoot struct {
	Position    int          `json:"position"`
	EndPosition *int         `json:"end_position,omitempty"`
	State       string       `json:"state"`
	Comment     *KataComment `json:"comment,omitempty"`
}
type KataPublicationBody struct {
	ReviewUUID     string                   `json:"review_uuid"`
	DocumentHash   string                   `json:"document_hash"`
	State          string                   `json:"state"`
	Expired        bool                     `json:"expired"`
	Policy         string                   `json:"policy,omitempty"`
	DestinationID  string                   `json:"destination_id,omitempty"`
	ProjectUID     string                   `json:"project_uid,omitempty"`
	Project        string                   `json:"project,omitempty"`
	IssueUID       string                   `json:"issue_uid,omitempty"`
	IssueShortID   string                   `json:"issue_short_id,omitempty"`
	IssueAuthor    string                   `json:"issue_author,omitempty"`
	IssueTeammate  *string                  `json:"issue_teammate,omitempty"`
	Roots          []KataRoot               `json:"roots"`
	Comments       []KataComment            `json:"comments"`
	Issue          *KataIssue               `json:"issue,omitempty"`
	SourceOutput   string                   `json:"source_output,omitempty"`
	SourceDocument storage.StructuredOutput `json:"source_document,omitempty"`
	Message        string                   `json:"message,omitempty"`
}
type KataPublicationInput struct {
	JobID      int64  `query:"job_id"`
	ReviewUUID string `query:"review_uuid"`
	Issue      string `query:"issue"`
	Project    string `query:"project"`
}
type KataPublicationRequest struct {
	JobID    int64  `json:"job_id" minimum:"1"`
	Findings string `json:"findings,omitempty"`
}
type (
	KataPublishInput      struct{ Body KataPublicationRequest }
	KataPublicationOutput struct{ Body KataPublicationBody }
)

func kataComment(c gen.CommentOut) KataComment {
	return KataComment{UID: c.UID, Handle: c.Handle, Body: c.Body, Author: c.Author, Teammate: c.Teammate, ReplyToUID: c.ReplyToUID, ReplyKind: c.ReplyKind, EditedAt: c.EditedAt}
}

func kataPublicationBody(status *KataPublicationStatus) KataPublicationBody {
	body := KataPublicationBody{ReviewUUID: status.ReviewUUID.String(), DocumentHash: status.DocumentHash, State: status.State, DestinationID: status.DestinationID, ProjectUID: status.Project.UID, Project: status.Project.Name, Roots: []KataRoot{}, Comments: []KataComment{}}
	if status.Issue != nil {
		issue := status.Issue.Issue
		body.IssueUID, body.IssueShortID, body.IssueAuthor, body.IssueTeammate = issue.UID, issue.ShortID, issue.Author, issue.Teammate
		if envelope, err := kata.ReadPublicationEnvelope(issue.Metadata); err == nil {
			body.Policy = envelope.Policy
		}
		for _, comment := range status.Issue.Comments {
			body.Comments = append(body.Comments, kataComment(comment))
		}
	}
	for _, root := range status.Roots {
		r := KataRoot{Position: root.Position, EndPosition: root.EndPosition, State: root.State}
		if root.Comment != nil {
			c := kataComment(*root.Comment)
			r.Comment = &c
		}
		body.Roots = append(body.Roots, r)
	}
	return body
}

func (s *Server) publicationResult(ctx context.Context, id int64, status *KataPublicationStatus, err error) (*KataPublicationOutput, error) {
	if status == nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, huma.Error404NotFound("review not found")
		}
		if err != nil {
			return nil, huma.Error409Conflict(err.Error())
		}
		return nil, huma.Error500InternalServerError("publication unavailable")
	}
	body := kataPublicationBody(status)
	if status.Issue != nil {
		issue := kataIssueFromPublication(status.Project, status.DestinationID, status.Generation, *status.Issue, &status.Roots)
		issue.ReplyTeammate = status.ReplyTeammate
		issue.Roots = body.Roots
		body.Comments = issue.Comments
		body.Issue = &issue
	}
	if review := status.Source; review != nil {
		body.Expired = !time.Now().Add(kata.DefaultRequestTimeout).Before(review.CreatedAt.Add(7 * 24 * time.Hour))
		body.SourceOutput, body.SourceDocument = review.Output, review.StructuredOutput
	}
	record, recordErr := s.db.GetKataPublication(ctx, id)
	if recordErr == nil && record.Marker != nil && body.Policy == "" {
		body.Policy = record.Marker.Policy
	}
	if err != nil {
		body.Message = err.Error()
	}
	return &KataPublicationOutput{Body: body}, nil
}

func (s *Server) humaKataPublication(ctx context.Context, in *KataPublicationInput) (*KataPublicationOutput, error) {
	count := 0
	if in.JobID > 0 {
		count++
	}
	if in.ReviewUUID != "" {
		count++
	}
	if in.Issue != "" {
		count++
	}
	if (count != 1 && (count != 2 || in.JobID <= 0 || in.ReviewUUID == "")) || in.JobID < 0 {
		return nil, huma.Error400BadRequest("select a job_id, review_uuid, or issue; job_id and review_uuid may be combined")
	}
	jobID := in.JobID
	var id uuid.UUID
	if in.ReviewUUID != "" {
		var err error
		id, err = uuid.Parse(in.ReviewUUID) //nolint:forbidigo // Parse an external selector.
		if err != nil || id == uuid.Nil() {
			return nil, huma.Error400BadRequest("invalid review UUID")
		}
		if jobID == 0 {
			record, err := s.db.GetKataPublicationByUUID(ctx, id)
			if err != nil && record == nil && !errors.Is(err, sql.ErrNoRows) {
				return nil, kataReadError(err)
			}
			if record != nil {
				jobID = record.JobID
			}
		}
	}
	if jobID > 0 {
		status, err := s.hookRunner.publisher.inspectSelected(ctx, jobID, in.Project, id)
		if errors.Is(err, errKataPinnedProject) {
			return nil, huma.Error400BadRequest(err.Error())
		}
		if ctx.Err() != nil {
			return nil, kataReadError(ctx.Err())
		}
		if fatalKataRead(ctx, err) {
			return nil, kataReadError(err)
		}
		if id != uuid.Nil() && (errors.Is(err, storage.ErrReviewChanged) || errors.Is(err, sql.ErrNoRows)) {
			return unavailableKataContext(err), nil
		}
		if err != nil && status == nil && !errors.Is(err, sql.ErrNoRows) {
			out := unavailableKataContext(err)
			if review, readErr := s.db.GetReviewByJobIDContext(ctx, jobID); readErr == nil && (id == uuid.Nil() || (review.UUID != nil && *review.UUID == id)) {
				out.Body.SourceOutput, out.Body.SourceDocument = review.Output, review.StructuredOutput
			} else if record, _ := s.db.GetKataPublication(ctx, jobID); record != nil && record.DocumentHash == "" && (id == uuid.Nil() || record.ReviewUUID == id) {
				// Invalid legacy documents cannot hydrate, but their raw source is still evidence.
				out.Body.SourceOutput = record.SourceOutput
			}
			if ctx.Err() != nil {
				return nil, kataReadError(ctx.Err())
			}
			return out, nil
		}
		if err != nil && status != nil && status.State == "missing" {
			if status.Issue == nil {
				status.State = "unavailable"
			} else {
				status.State = "partial"
			}
		}
		return s.publicationResult(ctx, jobID, status, err)
	}
	projectName, issueRef := strings.TrimSpace(in.Project), ""
	if in.Issue != "" {
		var err error
		projectName, issueRef, err = kataIssueSelection(in.Issue, projectName)
		if err != nil {
			return nil, err
		}
	}
	cfg, generation := s.configWatcher.kataSnapshot()
	if !cfg.Enabled {
		return nil, huma.Error409Conflict("Kata integration is disabled")
	}
	client, err := kata.NewAPIClient("", cfg, kata.WithoutWorkspace())
	if err != nil {
		return nil, kataReadError(err)
	}
	defer client.CloseIdleConnections()
	project, err := client.ResolvePublicationProject(ctx, projectName)
	if ctx.Err() != nil {
		return nil, kataReadError(ctx.Err())
	}
	if err != nil {
		if !fatalKataRead(ctx, err) && errors.Is(err, kata.ErrUnreachable) {
			return unavailableKataContext(err), nil
		}
		return nil, kataReadError(err)
	}
	issue, metadata, err := client.ReadPublicationIssue(ctx, project, id, issueRef)
	if ctx.Err() != nil {
		return nil, kataReadError(ctx.Err())
	}
	if err != nil {
		if !fatalKataRead(ctx, err) && errors.Is(err, kata.ErrUnreachable) {
			return unavailableKataContext(err), nil
		}
		if errors.Is(err, kata.ErrConflict) {
			return &KataPublicationOutput{Body: KataPublicationBody{ReviewUUID: id.String(), State: "ambiguous", Message: err.Error(), Project: project.Name, ProjectUID: project.UID, DestinationID: kataDestinationID(client.PublicationEndpoint(), project.UID), Roots: []KataRoot{}, Comments: []KataComment{}}}, nil
		}
		return nil, kataReadError(err)
	}
	body := KataPublicationBody{State: "missing", Roots: []KataRoot{}, Comments: []KataComment{}, Project: project.Name, ProjectUID: project.UID, DestinationID: kataDestinationID(client.PublicationEndpoint(), project.UID)}
	if id != uuid.Nil() {
		body.ReviewUUID = id.String()
	}
	if issue == nil {
		return &KataPublicationOutput{Body: body}, nil
	}
	body.State = "unverified"
	full := kataIssueFromPublication(project, body.DestinationID, generation, *issue, nil)
	full.ReplyTeammate = cfg.ResolvedTeammate()
	body.Issue = &full
	body.IssueUID, body.IssueShortID, body.IssueAuthor, body.IssueTeammate = full.UID, full.ShortID, full.Author, full.Teammate
	body.Comments, body.Roots = full.Comments, full.Roots
	if full.Envelope != nil {
		body.Policy = full.Envelope.Policy
		body.ReviewUUID = full.Envelope.ReviewUUID.String()
		body.DocumentHash = full.Envelope.DocumentHash
		// An explicit issue can carry local source evidence without asserting
		// that its destination or envelope has been verified.
		if in.Issue != "" {
			record, lookupErr := s.db.GetKataPublicationByUUID(ctx, full.Envelope.ReviewUUID)
			if lookupErr == nil && record.DocumentHash == full.Envelope.DocumentHash {
				review, readErr := s.db.GetReviewByJobIDContext(ctx, record.JobID)
				if readErr == nil && review.UUID != nil && *review.UUID == record.ReviewUUID {
					raw, marshalErr := json.Marshal(review.StructuredOutput)
					hash, hashErr := storage.KataDocumentHash(raw)
					if marshalErr == nil && hashErr == nil && hash == record.DocumentHash {
						body.SourceOutput, body.SourceDocument = review.Output, review.StructuredOutput
					} else {
						body.Message = "local source changed during inspection"
					}
				} else if readErr != nil {
					body.Message = fmt.Sprintf("local source unavailable: %v", readErr)
				}
			} else if lookupErr != nil && !errors.Is(lookupErr, sql.ErrNoRows) {
				body.Message = fmt.Sprintf("local source unavailable: %v", lookupErr)
			}
			if ctx.Err() != nil {
				return nil, kataReadError(ctx.Err())
			}
		}
		if id != uuid.Nil() && (full.Envelope.ReviewUUID != id || !samePublicationEnvelope(metadata, *full.Envelope)) {
			body.State = "ambiguous"
		}
	} else if id != uuid.Nil() {
		body.State = "ambiguous"
	}
	if issue.Truncated && body.State != "ambiguous" {
		body.State = "truncated"
	}
	return &KataPublicationOutput{Body: body}, nil
}

func unavailableKataContext(err error) *KataPublicationOutput {
	return &KataPublicationOutput{Body: KataPublicationBody{State: "unavailable", Message: err.Error(), Roots: []KataRoot{}, Comments: []KataComment{}}}
}

func samePublicationEnvelope(metadata map[string]any, expected kata.PublicationEnvelope) bool {
	actual, err := kata.ReadPublicationEnvelope(metadata)
	return err == nil && actual.Version == expected.Version && actual.ReviewUUID == expected.ReviewUUID && actual.DocumentHash == expected.DocumentHash && actual.Policy == expected.Policy && actual.CompletedAt.Equal(expected.CompletedAt) && actual.FindingCount == expected.FindingCount
}

func fatalKataRead(ctx context.Context, err error) bool {
	return err != nil && (ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, kata.ErrAuthenticationRequired) || IsDaemonAccessError(err) || errors.Is(err, kata.ErrInsecureEndpoint) || errors.Is(err, kata.ErrPlatformSecurityLimit))
}

func kataReadError(err error) error {
	if apiErr, ok := kata.AsAPIError(err); ok && apiErr.Status == http.StatusForbidden {
		return huma.Error403Forbidden("Kata access denied")
	}
	switch {
	case errors.Is(err, context.Canceled):
		return huma.NewError(http.StatusRequestTimeout, "Kata read canceled")
	case errors.Is(err, context.DeadlineExceeded):
		return huma.NewError(http.StatusGatewayTimeout, "Kata read deadline exceeded")
	case errors.Is(err, kata.ErrAuthenticationRequired):
		return huma.Error401Unauthorized("Kata authentication required")
	case IsDaemonAccessError(err), errors.Is(err, kata.ErrInsecureEndpoint), errors.Is(err, kata.ErrPlatformSecurityLimit):
		return huma.Error403Forbidden("Kata access denied")
	default:
		return kataProxyError(fmt.Errorf("read Kata context: %w", err))
	}
}

func (s *Server) humaKataPublish(ctx context.Context, in *KataPublishInput) (*KataPublicationOutput, error) {
	req := in.Body
	if req.JobID <= 0 {
		return nil, huma.Error400BadRequest("job_id must be positive")
	}
	if req.Findings != "" && req.Findings != "comments" && req.Findings != "summary" {
		return nil, huma.Error400BadRequest("findings must be comments or summary")
	}
	record, err := s.db.GetKataPublication(ctx, req.JobID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, huma.Error404NotFound("review not found")
	}
	if err != nil {
		return nil, huma.Error409Conflict(err.Error())
	}
	review, err := s.db.GetReviewByJobIDContext(ctx, req.JobID)
	if err != nil {
		return nil, huma.Error404NotFound("review not found")
	}
	if !time.Now().Add(kata.DefaultRequestTimeout).Before(review.CreatedAt.Add(7 * 24 * time.Hour)) {
		status, inspectErr := s.hookRunner.publisher.Inspect(ctx, req.JobID)
		if inspectErr == nil {
			inspectErr = errors.New("kata publication window expired")
		}
		return s.publicationResult(ctx, req.JobID, status, inspectErr)
	}
	policy := ""
	if record.Marker == nil {
		if req.Findings == "" {
			return nil, huma.Error400BadRequest("findings is required for an unmarked review")
		}
		policy = req.Findings
	} else if req.Findings != "" && req.Findings != record.Marker.Policy {
		return nil, huma.Error409Conflict("saved publication policy cannot be changed")
	}
	status, err := s.hookRunner.publishKataManual(ctx, req.JobID, policy)
	return s.publicationResult(ctx, req.JobID, status, err)
}
