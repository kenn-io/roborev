package daemon

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/roborev/internal/storage"
)

// GetExperimentReportInput selects assignment cohorts, not historical results.
// Parse the strings ourselves so invalid options consistently return HTTP 400.
type GetExperimentReportInput struct {
	ID          string `path:"id" doc:"Immutable enrolled experiment ID"`
	Since       string `query:"since" doc:"Inclusive assignment-time lower bound (RFC3339); default all time"`
	Until       string `query:"until" doc:"Exclusive assignment-time upper bound (RFC3339); default all time"`
	MinSubjects string `query:"min_subjects" doc:"Positive minimum contributing subjects per arm; default 4"`
	options     storage.ExperimentReportOptions
}

// Resolve also distinguishes omitted parameters from explicitly empty values.
func (input *GetExperimentReportInput) Resolve(ctx huma.Context) []error {
	u := ctx.URL()
	query := u.Query()
	for _, bound := range []struct {
		name     string
		dest     *time.Time
		provided *bool
	}{{"since", &input.options.Since, &input.options.SinceProvided}, {"until", &input.options.Until, &input.options.UntilProvided}} {
		if query.Has(bound.name) {
			parsed, err := time.Parse(time.RFC3339, query.Get(bound.name))
			if err != nil {
				return []error{huma.Error400BadRequest(bound.name + " must be an RFC3339 timestamp")}
			}
			*bound.dest = parsed.UTC()
			*bound.provided = true
		}
	}
	if query.Has("min_subjects") {
		minimum, err := strconv.Atoi(query.Get("min_subjects"))
		if err != nil || minimum <= 0 {
			return []error{huma.Error400BadRequest("min_subjects must be a positive integer")}
		}
		input.options.MinSubjects = minimum
	}
	return nil
}

type GetExperimentReportOutput struct {
	Body *storage.ExperimentReport
}

func (s *Server) humaGetExperimentReport(_ context.Context, input *GetExperimentReportInput) (*GetExperimentReportOutput, error) {
	report, err := s.db.GetExperimentReport(input.ID, input.options)
	switch {
	case errors.Is(err, storage.ErrInvalidExperimentReportOptions):
		return nil, huma.Error400BadRequest("since must not be after until")
	case errors.Is(err, sql.ErrNoRows):
		return nil, huma.Error404NotFound("experiment not found")
	case err != nil:
		return nil, huma.Error500InternalServerError("get experiment report")
	default:
		return &GetExperimentReportOutput{Body: report}, nil
	}
}
