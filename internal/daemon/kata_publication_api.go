package daemon

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/danielgtaylor/huma/v2"
	gen "go.kenn.io/kata/pkg/client/generated"
	"go.kenn.io/roborev/internal/kata"
)

type KataComment struct {
	UID        string     `json:"uid"`
	Handle     *string    `json:"handle,omitempty"`
	Body       string     `json:"body"`
	Author     string     `json:"author"`
	Teammate   *string    `json:"teammate,omitempty"`
	ReplyToUID *string    `json:"reply_to_uid,omitempty"`
	ReplyKind  *string    `json:"reply_kind,omitempty"`
	EditedAt   *time.Time `json:"edited_at,omitempty"`
}
type KataRoot struct {
	Position int          `json:"position"`
	State    string       `json:"state"`
	Comment  *KataComment `json:"comment,omitempty"`
}
type KataPublicationBody struct {
	ReviewUUID    string        `json:"review_uuid"`
	DocumentHash  string        `json:"document_hash"`
	State         string        `json:"state"`
	Expired       bool          `json:"expired"`
	Policy        string        `json:"policy,omitempty"`
	DestinationID string        `json:"destination_id,omitempty"`
	ProjectUID    string        `json:"project_uid,omitempty"`
	Project       string        `json:"project,omitempty"`
	IssueUID      string        `json:"issue_uid,omitempty"`
	IssueShortID  string        `json:"issue_short_id,omitempty"`
	IssueAuthor   string        `json:"issue_author,omitempty"`
	IssueTeammate *string       `json:"issue_teammate,omitempty"`
	Roots         []KataRoot    `json:"roots"`
	Comments      []KataComment `json:"comments"`
	Message       string        `json:"message,omitempty"`
}
type KataPublicationInput struct {
	JobID int64 `query:"job_id" required:"true" minimum:"1"`
}
type KataPublicationRequest struct {
	JobID    int64  `json:"job_id" minimum:"1"`
	Findings string `json:"findings,omitempty"`
}
type KataPublishInput struct{ Body KataPublicationRequest }
type KataPublicationOutput struct{ Body KataPublicationBody }

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
		r := KataRoot{Position: root.Position, State: root.State}
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
	review, reviewErr := s.db.GetReviewByJobIDContext(ctx, id)
	if reviewErr == nil {
		body.Expired = !time.Now().Add(kata.DefaultRequestTimeout).Before(review.CreatedAt.Add(7 * 24 * time.Hour))
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
	if in.JobID <= 0 {
		return nil, huma.Error400BadRequest("job_id must be positive")
	}
	status, err := s.hookRunner.publisher.Inspect(ctx, in.JobID)
	return s.publicationResult(ctx, in.JobID, status, err)
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
