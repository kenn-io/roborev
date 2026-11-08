package daemon

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
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

// invocationOutput keeps this call's evidence independent of the best-effort
// job log, which may still contain an earlier attempt after an open failure.
// Some adapters write stdout and stderr concurrently.
type invocationOutput struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (o *invocationOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.buf.Write(p)
}

func (o *invocationOutput) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.buf.String()
}

func (wp *WorkerPool) finishReviewInvocation(ctx context.Context, report storage.ReviewInvocation, result review.ReviewResult, runErr error, sessionID, stream string) error {
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
	if report.RawOutput == "" {
		// Native adapters can reject a partial response before returning it.
		// Keep the bytes they emitted even when no final response is available.
		report.RawOutput = stream
	}
	report.Output = result.Output
	report.StructuredOutput = result.StructuredOutput
	var err error
	report.Usage, err = tokens.ParseSchemaUsage(report.Agent, strings.NewReader(stream))
	if err == nil && report.Usage == nil {
		report.Usage, err = tokens.ParseCodexUsageJSONL(strings.NewReader(stream))
	}
	if err != nil {
		report.UsageError = fmt.Sprintf("read invocation usage: %v", err)
	}
	// Preserve evidence after cancellation; the record belongs to this invocation,
	// independently of whether its former job claim can still transition.
	return wp.db.FinishReviewInvocation(context.WithoutCancel(ctx), report)
}
