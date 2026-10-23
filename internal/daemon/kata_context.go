package daemon

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/roborev/internal/katacontext"
	"go.kenn.io/roborev/internal/kataresponse"
)

type KataContextInput struct {
	JobID      int64  `query:"job_id"`
	ReviewUUID string `query:"review_uuid"`
	Ref        string `query:"ref" doc:"Frozen qualified issue reference or full issue UID"`
}

type KataConsumeContextRequest struct {
	JobID      int64  `json:"job_id,omitempty"`
	ReviewUUID string `json:"review_uuid,omitempty"`
	Ref        string `json:"ref,omitempty"`
	RunID      string `json:"run_id"`
	Purpose    string `json:"purpose"`
}

type (
	KataConsumeContextInput struct{ Body KataConsumeContextRequest }
	KataContextOutput       struct{ Body katacontext.KataReplyContext }
)

func (s *Server) humaKataContext(ctx context.Context, in *KataContextInput) (*KataContextOutput, error) {
	return s.kataContext(ctx, in.JobID, in.ReviewUUID, in.Ref, katacontext.Run{})
}

func (s *Server) humaKataConsumeContext(ctx context.Context, in *KataConsumeContextInput) (*KataContextOutput, error) {
	if kataresponse.ValidateOperationID(in.Body.RunID) != nil || strings.TrimSpace(in.Body.Purpose) == "" {
		return nil, huma.Error400BadRequest("run_id UUID and purpose are required")
	}
	return s.kataContext(ctx, in.Body.JobID, in.Body.ReviewUUID, in.Body.Ref, katacontext.Run{ID: in.Body.RunID, Purpose: in.Body.Purpose})
}

func (s *Server) kataContext(ctx context.Context, jobID int64, reviewUUID, ref string, run katacontext.Run) (*KataContextOutput, error) {
	ref = strings.TrimSpace(ref)
	if (jobID > 0) == (ref != "") || jobID < 0 {
		return nil, huma.Error400BadRequest("select one positive job_id or frozen issue ref")
	}
	out := &KataContextOutput{Body: katacontext.KataReplyContext{RunID: run.ID, JobID: jobID, ReviewUUID: reviewUUID, IssueRef: ref, Snapshots: []katacontext.Snapshot{}}}
	seenOperations := map[string]bool{}
	appendOperations := func(sourceJobID int64, issueUID string) error {
		statuses, err := s.db.KataResponseStatuses(ctx, sourceJobID, issueUID)
		if err != nil {
			return huma.Error503ServiceUnavailable("saved Kata response status unavailable")
		}
		for _, status := range statuses {
			if !seenOperations[status.OperationID] {
				seenOperations[status.OperationID] = true
				out.Body.Operations = append(out.Body.Operations, status)
			}
		}
		return nil
	}
	if jobID > 0 {
		job, err := s.kataPublicationJob(ctx, jobID)
		if err != nil {
			return nil, err
		}
		if job.IsCIReview() {
			return out, nil
		}
		if run.ID != "" && reviewUUID == "" {
			review, err := s.db.GetReviewByJobID(jobID)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return nil, huma.Error503ServiceUnavailable("source review unavailable")
			}
			if err == nil && review.UUID != nil {
				reviewUUID = review.UUID.String() //nolint:forbidigo // Pin the source review UUID for the consumed snapshot.
				out.Body.ReviewUUID = reviewUUID
			}
		}
		publications, err := s.kataPublications(ctx, jobID, reviewUUID)
		if err != nil {
			return nil, err
		}
		out.Body.Publications = publications.Body.Publications
		hasFindings := false
		for _, publication := range out.Body.Publications {
			if len(publication.Findings) > 0 {
				hasFindings = true
				break
			}
		}
		if hasFindings || run.ID != "" && reviewUUID != "" {
			views, err := katacontext.ReadJob(ctx, s.db, s.configWatcher.Config().Integrations.Kata, jobID, reviewUUID, run)
			if err != nil {
				return nil, huma.Error503ServiceUnavailable("Kata finding reply context unavailable")
			}
			out.Body.Snapshots = views
		}
		if err := appendOperations(jobID, ""); err != nil {
			return nil, err
		}
	} else {
		handoffs, err := s.db.KataHandoffsForIssue(ctx, ref)
		if err != nil {
			return nil, huma.Error500InternalServerError("read saved Kata issue mappings failed")
		}
		seen := map[string]bool{}
		for _, h := range handoffs {
			if len(h.Request.Responses) > 0 || (reviewUUID != "" && reviewUUID != h.ReviewUUID) {
				continue
			}
			job, err := s.kataPublicationJob(ctx, h.JobID)
			if err != nil {
				return nil, err
			}
			if job.IsCIReview() {
				continue
			}
			key := h.ReviewUUID + ":" + h.State.IssueUID
			if seen[key] {
				continue
			}
			seen[key] = true
			out.Body.Publications = append(out.Body.Publications, kataPublicationMapping(h))
			if err := appendOperations(h.JobID, h.State.IssueUID); err != nil {
				return nil, err
			}
			if len(h.Request.Entries) > 0 {
				views, err := katacontext.ReadIssueJob(ctx, s.db, s.configWatcher.Config().Integrations.Kata, h.JobID, h.ReviewUUID, h.State.IssueUID, run)
				if err != nil {
					return nil, huma.Error503ServiceUnavailable("Kata finding reply context unavailable")
				}
				for _, view := range views {
					if view.IssueUID == h.State.IssueUID {
						out.Body.Snapshots = append(out.Body.Snapshots, view)
					}
				}
			}
		}
	}
	if !s.configWatcher.Config().Integrations.Kata.Enabled {
		out.Body.Message = "Kata finding reply context is disabled"
	}
	return out, nil
}
