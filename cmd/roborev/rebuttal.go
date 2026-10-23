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
	"go.kenn.io/roborev/internal/findingid"
	"go.kenn.io/roborev/internal/rebuttal"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/telemetry"
	roborevclient "go.kenn.io/roborev/pkg/client"
	"go.kenn.io/roborev/pkg/client/generated"
)

type rebuttalOptions struct {
	enabled, close bool
	finding        string
	findingID      string
	replacement    string
}

func (o rebuttalOptions) validate() error {
	hasFinding, hasFindingID := strings.TrimSpace(o.finding) != "", strings.TrimSpace(o.findingID) != ""
	if !o.enabled && (o.close || o.finding != "" || o.findingID != "") {
		return fmt.Errorf("--finding, --finding-id and --close require --rebuttal")
	}
	if hasFinding && hasFindingID {
		return fmt.Errorf("--finding and --finding-id select the finding two ways; use one")
	}
	if o.enabled && !hasFinding && !hasFindingID {
		return fmt.Errorf("--rebuttal requires --finding with an excerpt of the original review, or --finding-id with a finding selector")
	}
	if hasFindingID {
		if _, err := findingid.Parse(strings.TrimSpace(o.findingID)); err != nil {
			return fmt.Errorf("--finding-id takes a complete selector copied from 'roborev show', not part of one: %w", err)
		}
	}
	if o.replacement != "" {
		if !o.enabled {
			return fmt.Errorf("--replacement requires --rebuttal or --kata supersede")
		}
		if _, err := findingid.Parse(o.replacement); err != nil {
			return fmt.Errorf("replacement: %w", err)
		}
	}
	return nil
}

// listReviewFindings asks the daemon for the current findings of a job's
// review, each with its selector and text.
func listReviewFindings(ctx context.Context, api *roborevclient.Client, jobID int64) ([]daemon.ReviewFinding, error) {
	resp, err := api.ListReviewFindingsRaw(ctx, &generated.ListReviewFindingsRequestOptions{Query: &generated.ListReviewFindingsQuery{JobID: jobID}})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	var body daemon.ReviewFindingsBody
	if err := json.UnmarshalRead(resp.Body, &body); err != nil {
		return nil, fmt.Errorf("read findings: %w", err)
	}
	return body.Findings, nil
}

// selectFinding finds the finding with this complete selector among a review's
// findings.
func selectFinding(findings []daemon.ReviewFinding, selector string) (daemon.ReviewFinding, error) {
	id, err := findingid.Parse(selector)
	if err != nil {
		return daemon.ReviewFinding{}, err
	}
	for _, f := range findings {
		if f.ID == id.String() {
			return f, nil
		}
	}
	return daemon.ReviewFinding{}, fmt.Errorf("%w: %s is not a finding of this review", findingid.ErrNotFound, selector)
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
	request := daemon.RebuttalRequest{JobID: jobID, Finding: o.finding, Comment: message, Commenter: commenter, Replacement: o.replacement}
	if strings.TrimSpace(o.findingID) != "" {
		findings, err := listReviewFindings(ctx, api, jobID)
		if err != nil {
			return fmt.Errorf("list findings of review %d: %w", jobID, err)
		}
		selected, err := selectFinding(findings, strings.TrimSpace(o.findingID))
		if err != nil {
			return fmt.Errorf("select finding %q of review %d: %w", o.findingID, jobID, err)
		}
		// The daemon checks the selector against the review again when it queues
		// the rebuttal. The excerpt is only what the recorded assessment quotes.
		request.Finding, request.FindingID = "", selected.ID
	}
	var job storage.ReviewJob
	if err := postRebuttalAPI(ctx, api.RebuttalRaw, request, &job); err != nil {
		return fmt.Errorf("queue rebuttal: %w", err)
	}
	if !job.ReassessmentDurable {
		return fmt.Errorf("daemon does not persist reassessment results; update and restart it before retrying; queued job %d and original review left open", job.ID)
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
	if o.close {
		if err := postRebuttalAPI(ctx, api.CloseReviewRaw, daemon.CloseReviewRequest{
			JobID: jobID, Closed: true, ExpectedReviewUUID: job.OriginalReviewUUID,
		}, nil); err != nil {
			return fmt.Errorf("rebuttal job %d recorded but close failed: %w", job.ID, err)
		}
		reportCLIActionOutput(cmd.OutOrStderr(), telemetry.EventReviewClosed)
	}
	cmd.Printf("%s: %s\n", assessment.Outcome, assessment.Explanation)
	return nil
}
