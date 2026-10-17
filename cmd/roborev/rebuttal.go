package main

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	"github.com/spf13/cobra"

	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/rebuttal"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/telemetry"
	roborevclient "go.kenn.io/roborev/pkg/client"
)

type rebuttalOptions struct {
	enabled, close bool
	finding        string
}

func (o rebuttalOptions) validate() error {
	if !o.enabled && (o.close || o.finding != "") {
		return fmt.Errorf("--finding and --close require --rebuttal")
	}
	if o.enabled && strings.TrimSpace(o.finding) == "" {
		return fmt.Errorf("--rebuttal requires --finding with an excerpt of the original review")
	}
	return nil
}

// rawDaemonCall is the shape of the generated raw client's POST methods.
type rawDaemonCall[O any] func(context.Context, *O, ...runtime.RequestEditorFn) (*http.Response, error)

// postRebuttalAPI sends body to a daemon endpoint and decodes a successful
// response into result when result is non-nil.
func postRebuttalAPI[O any](ctx context.Context, call rawDaemonCall[O], body, result any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("encode request: %w", err)
	}
	resp, err := call(ctx, nil, roborevclient.WithBody(data))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		msg, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			return fmt.Errorf("%s (read response body: %w)", resp.Status, readErr)
		}
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	if result == nil {
		return nil
	}
	return json.UnmarshalRead(resp.Body, result)
}

func runCommentRebuttal(cmd *cobra.Command, jobID int64, commenter, message string, o rebuttalOptions) error {
	ctx := cmd.Context()
	ep := getDaemonEndpoint()
	api := ep.APIClient(30 * time.Second)
	var job storage.ReviewJob
	if err := postRebuttalAPI(ctx, api.RebuttalRaw, daemon.RebuttalRequest{
		JobID: jobID, Finding: o.finding, Comment: message, Commenter: commenter,
	}, &job); err != nil {
		return fmt.Errorf("queue rebuttal: %w", err)
	}
	cmd.Printf("Rebuttal job %d queued for review %d\n", job.ID, jobID)
	review, err := waitForJobCompletion(ctx, ep.BaseURL(), job.ID, nil)
	if err != nil {
		return fmt.Errorf("rebuttal job %d: %w; original review left open", job.ID, err)
	}
	assessment, err := rebuttal.Parse(review.Output)
	if err != nil {
		return fmt.Errorf("rebuttal job %d: %w; original review left open", job.ID, err)
	}
	if job.OriginalReviewUUID == nil {
		return fmt.Errorf("rebuttal job %d has no UUID for a safe rebuttal assessment; original review left open", job.ID)
	}
	body := fmt.Sprintf("Rebuttal job %d (%s): %s\n\nFinding:\n%s\n\n%s", job.ID, job.Agent, assessment.Outcome, o.finding, assessment.Explanation)
	if err := postRebuttalAPI(ctx, api.AddCommentRaw, daemon.AddCommentRequest{
		JobID: jobID, Commenter: job.Agent, Comment: body,
		ExpectedReviewUUID: job.OriginalReviewUUID,
	}, nil); err != nil {
		return fmt.Errorf("record rebuttal job %d: %w; original review left open", job.ID, err)
	}
	if o.close {
		if err := postRebuttalAPI(ctx, api.CloseReviewRaw, daemon.CloseReviewRequest{
			JobID: jobID, Closed: true, ExpectedReviewUUID: job.OriginalReviewUUID,
		}, nil); err != nil {
			return fmt.Errorf("rebuttal job %d recorded but close failed: %w", job.ID, err)
		}
		reportCLIActionOutput(ctx, cmd.OutOrStderr(), telemetry.EventReviewClosed)
	}
	cmd.Printf("%s: %s\n", assessment.Outcome, assessment.Explanation)
	return nil
}
