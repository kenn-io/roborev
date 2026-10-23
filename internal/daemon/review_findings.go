package daemon

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"uuid"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/roborev/internal/storage"
)

// ReviewFindingsInput holds query parameters for GET /api/review/findings.
type ReviewFindingsInput struct {
	JobID int64 `query:"job_id" required:"true" minimum:"1" doc:"Job whose review's findings to list"`
}

// ReviewFinding is one finding of a structured review with the selector that
// names it.
type ReviewFinding struct {
	ID           string   `json:"id" doc:"Selector for this finding, such as f:... ; --finding-id takes it whole, as listed"`
	Index        int      `json:"index" doc:"Zero-based position of the finding in the review document"`
	Severity     string   `json:"severity"`
	Problem      string   `json:"problem"`
	Fix          string   `json:"fix"`
	Location     string   `json:"location,omitempty"`
	Sources      []int    `json:"sources,omitempty" doc:"1-based numbers of the member reviews a combined finding cites"`
	SourceLabels []string `json:"source_labels,omitempty" doc:"Labels of the member reviews a combined finding cites"`
}

// ReviewFindingsBody lists the findings of one job's current review. A review
// without structured findings, such as a clean or historical prose review,
// has an empty list.
type ReviewFindingsBody struct {
	JobID        int64           `json:"job_id"`
	ReviewUUID   *uuid.UUID      `json:"review_uuid,omitempty" format:"uuid" doc:"The review the selectors belong to"`
	DocumentHash string          `json:"document_hash,omitempty" doc:"SHA-256 of the canonical review document the selectors were derived from"`
	Findings     []ReviewFinding `json:"findings"`
}

// ReviewFindingsOutput is the response for GET /api/review/findings.
type ReviewFindingsOutput struct{ Body ReviewFindingsBody }

// humaReviewFindings lists a review's findings under stable selectors. The
// first call pins an immutable copy of each finding, so a selector keeps naming
// the finding it named even after a rerun replaces the review.
func (s *Server) humaReviewFindings(ctx context.Context, in *ReviewFindingsInput) (*ReviewFindingsOutput, error) {
	job, err := s.db.GetJobByID(in.JobID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, huma.Error404NotFound("review not found")
	}
	if err != nil {
		return nil, huma.Error500InternalServerError(fmt.Sprintf("get job: %v", err))
	}
	// The remote handler's allowlist does not include this route today, so a
	// remote caller never reaches here. The check stays so that adding the route
	// to that allowlist cannot expose another repository's findings.
	if !remoteAllowsJob(ctx, job) {
		return nil, huma.Error403Forbidden("review repository denied")
	}
	snapshots, err := s.db.AssignFindingSnapshots(ctx, in.JobID)
	switch {
	case errors.Is(err, storage.ErrNoReview):
		return nil, huma.Error404NotFound("review not found")
	case errors.Is(err, storage.ErrFindingChanged):
		return nil, huma.NewError(http.StatusConflict, err.Error())
	case err != nil:
		return nil, huma.Error500InternalServerError(fmt.Sprintf("assign finding selectors: %v", err))
	}
	out := &ReviewFindingsOutput{Body: ReviewFindingsBody{JobID: in.JobID, Findings: []ReviewFinding{}}}
	for _, snap := range snapshots {
		out.Body.ReviewUUID = &snap.ReviewUUID
		out.Body.DocumentHash = snap.DocumentHash
		out.Body.Findings = append(out.Body.Findings, reviewFindingFromSnapshot(snap))
	}
	return out, nil
}

func reviewFindingFromSnapshot(snap storage.FindingSnapshot) ReviewFinding {
	return ReviewFinding{
		ID: snap.Selector, Index: snap.Index, Severity: snap.Finding.Severity,
		Problem: snap.Finding.Problem, Fix: snap.Finding.Fix, Location: snap.Finding.Location,
		Sources: snap.Finding.Sources, SourceLabels: snap.Provenance.SourceLabels,
	}
}

// Excerpt renders the finding as plain text for a prompt or an annotation: the
// severity, the location when there is one, the problem, and the fix.
func (f ReviewFinding) Excerpt() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Severity: %s\n", f.Severity)
	if f.Location != "" {
		fmt.Fprintf(&b, "Location: %s\n", f.Location)
	}
	fmt.Fprintf(&b, "Problem: %s\nFix: %s", f.Problem, f.Fix)
	return b.String()
}
