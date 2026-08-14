package daemon

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/roborev/internal/storage"
)

const ReviewProjectionSchemaVersion = 1

type ReviewProjectionInput struct {
	JobID  int64  `query:"job_id" default:"-1" doc:"Daemon-local review job ID"`
	Repo   string `query:"repo" doc:"Exact repository path for contextual lookup"`
	Branch string `query:"branch" doc:"Optional exact branch for contextual lookup"`
}

type ReviewProjectionJob struct {
	ID              int64                 `json:"id"`
	UUID            string                `json:"uuid,omitempty"`
	Project         string                `json:"project"`
	GitRef          string                `json:"git_ref"`
	Branch          string                `json:"branch,omitempty"`
	CommitSubject   string                `json:"commit_subject,omitempty"`
	Agent           string                `json:"agent"`
	Model           string                `json:"model,omitempty"`
	Status          storage.JobStatus     `json:"status"`
	Verdict         string                `json:"verdict,omitempty"`
	ReviewType      string                `json:"review_type,omitempty"`
	Source          string                `json:"source,omitempty"`
	EnqueuedAt      time.Time             `json:"enqueued_at"`
	StartedAt       *time.Time            `json:"started_at,omitempty"`
	FinishedAt      *time.Time            `json:"finished_at,omitempty"`
	PanelRole       string                `json:"panel_role,omitempty"`
	PanelName       string                `json:"panel_name,omitempty"`
	PanelMemberName string                `json:"panel_member_name,omitempty"`
	PanelSummary    *storage.PanelSummary `json:"panel_summary,omitempty"`
}

type ReviewProjectionReview struct {
	ID        int64     `json:"id"`
	Output    string    `json:"output"`
	CreatedAt time.Time `json:"created_at"`
	Closed    bool      `json:"closed"`
}

type ReviewProjectionResponse struct {
	ID        int64     `json:"id"`
	Responder string    `json:"responder"`
	Response  string    `json:"response"`
	CreatedAt time.Time `json:"created_at"`
}

type ReviewProjection struct {
	SchemaVersion int                        `json:"schema_version"`
	Job           ReviewProjectionJob        `json:"job"`
	Review        *ReviewProjectionReview    `json:"review,omitempty"`
	Responses     []ReviewProjectionResponse `json:"responses"`
	PanelMembers  []ReviewProjectionJob      `json:"panel_members"`
}

type ReviewProjectionOutput struct {
	Body ReviewProjection
}

func (s *Server) humaGetReviewProjection(
	ctx context.Context, input *ReviewProjectionInput,
) (*ReviewProjectionOutput, error) {
	hasJob := input.JobID >= 0
	hasRepo := input.Repo != ""
	if hasJob == hasRepo || (!hasRepo && input.Branch != "") {
		return nil, huma.Error400BadRequest("select exactly one of job_id or repo")
	}

	var job *storage.ReviewJob
	var err error
	if hasJob {
		job, err = s.db.GetJobByID(input.JobID)
	} else {
		job, err = s.db.GetLatestLogicalReviewJob(input.Repo, input.Branch)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return nil, huma.Error404NotFound("review not found")
	}
	if err != nil {
		return nil, huma.Error500InternalServerError(fmt.Sprintf("get review job: %v", err))
	}

	projection := ReviewProjection{
		SchemaVersion: ReviewProjectionSchemaVersion,
		Job:           projectReviewJob(*job),
		Responses:     []ReviewProjectionResponse{},
		PanelMembers:  []ReviewProjectionJob{},
	}
	review, err := s.db.GetReviewByJobID(job.ID)
	if err == nil {
		if review.Job != nil {
			projection.Job = projectReviewJob(*review.Job)
		}
		projection.Review = &ReviewProjectionReview{
			ID: review.ID, Output: review.Output,
			CreatedAt: review.CreatedAt, Closed: review.Closed,
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, huma.Error500InternalServerError(fmt.Sprintf("get review: %v", err))
	}
	commitID, fallbackSHA := job.LegacyCommentLookupTarget()
	responses, err := s.db.GetAllCommentsForJob(job.ID, commitID, fallbackSHA)
	if err != nil {
		return nil, huma.Error500InternalServerError(fmt.Sprintf("get responses: %v", err))
	}
	for _, response := range responses {
		projection.Responses = append(projection.Responses, ReviewProjectionResponse{
			ID: response.ID, Responder: response.Responder,
			Response: response.Response, CreatedAt: response.CreatedAt,
		})
	}
	if job.PanelRunUUID != "" {
		members, membersErr := s.db.GetPanelMembers(job.PanelRunUUID)
		if membersErr != nil {
			return nil, huma.Error500InternalServerError(fmt.Sprintf("get panel members: %v", membersErr))
		}
		for _, member := range members {
			projection.PanelMembers = append(projection.PanelMembers, projectReviewJob(member))
		}
	}
	return &ReviewProjectionOutput{Body: projection}, nil
}

func projectReviewJob(job storage.ReviewJob) ReviewProjectionJob {
	verdict := ""
	if job.Verdict != nil {
		verdict = *job.Verdict
	}
	return ReviewProjectionJob{
		ID: job.ID, UUID: job.UUID, Project: job.RepoName,
		GitRef: job.GitRef, Branch: job.Branch, CommitSubject: job.CommitSubject,
		Agent: job.Agent, Model: job.Model, Status: job.Status, Verdict: verdict,
		ReviewType: job.ReviewType, Source: job.Source, EnqueuedAt: job.EnqueuedAt,
		StartedAt: job.StartedAt, FinishedAt: job.FinishedAt,
		PanelRole: job.PanelRole, PanelName: job.PanelName,
		PanelMemberName: job.PanelMemberName, PanelSummary: job.PanelSummary,
	}
}
