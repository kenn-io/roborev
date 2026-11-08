package daemon

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/roborev/internal/review"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/tokens"
)

type JobInvocationsInput struct {
	JobID int64 `query:"job_id" minimum:"1" required:"true" doc:"Local review job ID"`
}

type JobInvocationsOutput struct {
	Body struct {
		Invocations []storage.ReviewInvocation `json:"invocations"`
	}
}

func (s *Server) humaJobInvocations(ctx context.Context, input *JobInvocationsInput) (*JobInvocationsOutput, error) {
	if _, err := s.db.GetJobByIDContext(ctx, input.JobID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, huma.Error404NotFound("job not found")
		}
		return nil, huma.Error500InternalServerError("read job", err)
	}
	reports, err := s.db.ListReviewInvocations(ctx, input.JobID)
	if err != nil {
		return nil, huma.Error500InternalServerError("read review invocations", err)
	}
	output := &JobInvocationsOutput{}
	output.Body.Invocations = reports
	return output, nil
}

func (wp *WorkerPool) finishReviewInvocation(ctx context.Context, report storage.ReviewInvocation, result review.ReviewResult, runErr error, sessionID string) error {
	report.FinishedAt = new(time.Now().UTC())
	report.Status = "succeeded"
	if runErr != nil {
		report.Status = "failed"
		if errors.Is(runErr, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
			report.Status = "canceled"
		}
		report.Error = runErr.Error()
	}
	report.SessionID = sessionID
	report.RawOutput = result.RawOutput
	report.Output = result.Output
	report.StructuredOutput = result.StructuredOutput
	file, err := os.Open(JobLogPath(report.JobID))
	if err == nil {
		report.Usage, err = tokens.ParseSchemaUsage(report.Agent, file)
		err = errors.Join(err, file.Close())
		if err == nil && report.Usage == nil {
			report.Usage, err = tokens.ParseCodexUsageFile(JobLogPath(report.JobID))
		}
	}
	if err != nil {
		report.UsageError = fmt.Sprintf("read invocation usage: %v", err)
	}
	// Preserve evidence after cancellation; the record belongs to this invocation,
	// independently of whether its former job claim can still transition.
	return wp.db.FinishReviewInvocation(context.WithoutCancel(ctx), report)
}
