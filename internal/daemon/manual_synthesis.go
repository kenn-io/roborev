package daemon

import (
	"context"
	"database/sql"
	"errors"
	"uuid"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/roborev/internal/storage"
)

type StartPanelSynthesisInput struct {
	Body struct {
		PanelRunUUID uuid.UUID `json:"panel_run_uuid" doc:"The held panel to synthesize after its voting members finish"`
	}
}

type StartPanelSynthesisOutput struct {
	Body *storage.ReviewJob
}

func (s *Server) humaStartPanelSynthesis(ctx context.Context, input *StartPanelSynthesisInput) (*StartPanelSynthesisOutput, error) {
	if input.Body.PanelRunUUID == uuid.Nil() {
		return nil, huma.Error400BadRequest("panel_run_uuid is required")
	}
	id, err := s.db.StartPanelSynthesis(ctx, input.Body.PanelRunUUID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, huma.Error404NotFound("panel not found")
	}
	if errors.Is(err, storage.ErrPanelSynthesisNotReady) {
		return nil, huma.Error409Conflict(err.Error())
	}
	if err != nil {
		return nil, huma.Error500InternalServerError("start panel synthesis", err)
	}
	s.workerPool.wake()
	job, err := s.db.GetJobByIDContext(ctx, id)
	if err != nil {
		return nil, huma.Error500InternalServerError("read synthesis job", err)
	}
	return &StartPanelSynthesisOutput{Body: job}, nil
}
