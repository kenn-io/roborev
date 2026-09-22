package daemon

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"errors"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/pkg/structuredreview"
)

// MigrateReviewInput supplies a complete structured replacement for a legacy review.
type MigrateReviewInput struct {
	Body struct {
		ReviewID int64          `json:"review_id" minimum:"1"`
		Document jsontext.Value `json:"document"`
	}
}

type MigrateReviewOutput struct {
	Body struct {
		Success bool `json:"success"`
	}
}

func (s *Server) humaMigrateReview(_ context.Context, input *MigrateReviewInput) (*MigrateReviewOutput, error) {
	doc, err := structuredreview.Decode(input.Body.Document)
	if err != nil {
		return nil, huma.Error400BadRequest(err.Error())
	}
	if doc.Legacy != nil {
		return nil, huma.Error400BadRequest("document must contain structured findings, not legacy Markdown")
	}
	if err := s.db.MigrateReview(input.Body.ReviewID, input.Body.Document); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, huma.Error404NotFound("review not found")
		}
		if errors.Is(err, storage.ErrReviewNotLegacy) {
			return nil, huma.Error409Conflict(err.Error())
		}
		if errors.Is(err, storage.ErrInvalidReviewDocument) {
			return nil, huma.Error400BadRequest(err.Error())
		}
		return nil, huma.Error500InternalServerError("could not migrate review", err)
	}
	response := &MigrateReviewOutput{}
	response.Body.Success = true
	return response, nil
}
