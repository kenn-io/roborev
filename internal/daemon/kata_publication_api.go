package daemon

import (
	"context"
	"database/sql"
	"errors"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/roborev/internal/kata"
	"go.kenn.io/roborev/internal/katacontext"
	"go.kenn.io/roborev/internal/katapublish"
	"go.kenn.io/roborev/internal/storage"
)

// Keep the existing daemon names while sharing their safe presentation with
// issue-scoped read-back. These types contain no frozen payload or credential.
type (
	KataFindingPublication = katacontext.KataFindingPublication
	KataPublication        = katacontext.KataPublication
)

type KataPublicationsBody struct {
	JobID        int64                        `json:"job_id"`
	Publications []KataPublication            `json:"publications"`
	Operations   []storage.KataResponseStatus `json:"operations,omitempty"`
}

type (
	KataPublicationsOutput struct{ Body KataPublicationsBody }
	KataPublicationsInput  struct {
		JobID      int64  `query:"job_id" required:"true" minimum:"1" doc:"Job whose saved Kata publications to look up"`
		ReviewUUID string `query:"review_uuid" doc:"Only return publications belonging to this immutable review"`
	}
)

type KataPublishRequest struct {
	JobID int64 `json:"job_id" minimum:"1" doc:"Completed or failed job to explicitly publish using its matching Kata hooks"`
}
type KataPublishInput struct{ Body KataPublishRequest }

func kataPublicationMapping(h storage.KataHandoff) KataPublication {
	diagnostic := ""
	if h.LastError != "" {
		diagnostic = "Kata publication pending; destination, ledger or receipt could not be validated"
	}
	out := KataPublication{ReviewUUID: h.ReviewUUID, OperationID: h.OperationID, State: h.Status, Policy: h.Request.Policy, Issue: h.State.IssueRef, IssueUID: h.State.IssueUID, LastError: diagnostic, Findings: []KataFindingPublication{}}
	for _, entry := range h.Request.Entries {
		out.Findings = append(out.Findings, KataFindingPublication{ID: entry.Selector, Issue: out.Issue, IssueUID: out.IssueUID, Comment: h.State.Receipts[entry.Selector].UID, State: out.State, Policy: out.Policy, OperationID: out.OperationID, LastError: out.LastError})
	}
	return out
}

func (s *Server) kataPublicationJob(ctx context.Context, id int64) (*storage.ReviewJob, error) {
	job, err := s.db.GetJobByID(id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, huma.Error404NotFound("job not found")
	}
	if err != nil {
		return nil, huma.Error500InternalServerError("could not load publication job")
	}
	if !remoteAllowsJob(ctx, job) {
		return nil, huma.Error403Forbidden("review repository denied")
	}
	return job, nil
}

func (s *Server) humaKataPublish(ctx context.Context, in *KataPublishInput) (*KataPublicationsOutput, error) {
	job, err := s.kataPublicationJob(ctx, in.Body.JobID)
	if err != nil {
		return nil, err
	}
	if !s.configWatcher.Config().Integrations.Kata.Enabled {
		return nil, huma.Error409Conflict("Kata publication requires the enabled [integrations.kata] connection")
	}
	if job.Status != storage.JobStatusDone && job.Status != storage.JobStatusFailed {
		return nil, huma.Error409Conflict("job is not complete")
	}
	var reviewUUID string
	if job.Status == storage.JobStatusDone {
		review, reviewErr := s.db.GetReviewByJobID(job.ID)
		if reviewErr != nil || review.UUID == nil {
			return nil, huma.Error409Conflict("could not load the current publication review")
		}
		reviewUUID = review.UUID.String()
	}
	if err = s.db.CaptureKataHandoffs(ctx, job.ID); err != nil {
		return nil, huma.Error409Conflict("could not capture Kata publication; review or publication policy changed")
	}
	records, err := s.db.KataHandoffs(ctx, job.ID)
	if err != nil {
		return nil, huma.Error500InternalServerError("could not load Kata publications")
	}
	out := &KataPublicationsOutput{Body: KataPublicationsBody{JobID: job.ID, Publications: []KataPublication{}}}
	for _, h := range records {
		if len(h.Request.Responses) > 0 {
			continue
		}
		if h.ReviewUUID != reviewUUID || reviewUUID == "" && h.Request.FailureRun != storage.KataFailureRun(job) {
			continue
		}
		out.Body.Publications = append(out.Body.Publications, kataPublicationMapping(h))
	}
	if len(out.Body.Publications) == 0 {
		return nil, huma.Error409Conflict("no matching Kata hook authorizes publication for this job")
	}
	return out, nil
}

func (s *Server) humaKataPublications(ctx context.Context, in *KataPublicationsInput) (*KataPublicationsOutput, error) {
	if _, err := s.kataPublicationJob(ctx, in.JobID); err != nil {
		return nil, err
	}
	return s.kataPublications(ctx, in.JobID, in.ReviewUUID)
}

func (s *Server) kataPublications(ctx context.Context, jobID int64, reviewUUID string) (*KataPublicationsOutput, error) {
	records, err := s.db.KataHandoffs(ctx, jobID)
	if err != nil {
		return nil, huma.Error500InternalServerError("could not load Kata publications")
	}
	out := &KataPublicationsOutput{Body: KataPublicationsBody{JobID: jobID, Publications: []KataPublication{}}}
	out.Body.Operations, err = s.db.KataResponseStatuses(ctx, jobID, "")
	if err != nil {
		return nil, huma.Error500InternalServerError("could not load response statuses")
	}
	for _, h := range records {
		if len(h.Request.Responses) > 0 {
			continue
		}
		if reviewUUID != "" && h.ReviewUUID != reviewUUID {
			continue
		}
		// Lookup is read-only. It can recover identifiers after a lost response,
		// but only the worker's durable checkpoint completes the local operation.
		if h.Status != "done" && s.configWatcher.Config().Integrations.Kata.Enabled {
			if h.Request.CredentialFingerprint != kataCredentialFingerprint(s.configWatcher.Config().Integrations.Kata) {
				h.LastError = "Kata publication remains pending"
			} else if connection, connectionErr := kataPublicationConnection(s.configWatcher.Config().Integrations.Kata, h.Request); connectionErr == nil {
				client, clientErr := kata.NewAPIClient("", connection, kata.WithoutWorkspace())
				if clientErr == nil {
					recovered, lookupErr := katapublish.Lookup(ctx, client, h.Request, h.State)
					h.State = recovered
					if lookupErr != nil {
						h.LastError = "Kata publication remains pending"
					}
				}
			} else {
				h.LastError = "Kata publication remains pending"
			}
		}
		out.Body.Publications = append(out.Body.Publications, kataPublicationMapping(h))
	}
	return out, nil
}
