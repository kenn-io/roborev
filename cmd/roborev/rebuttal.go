package main

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/kata"
	"go.kenn.io/roborev/internal/rebuttal"
	"go.kenn.io/roborev/internal/storage"
	roborevclient "go.kenn.io/roborev/pkg/client"
)

type rebuttalOptions struct {
	enabled, close              bool
	finding, kataIssue, replyTo string
}

func (o rebuttalOptions) validate() error {
	if !o.enabled && (o.close || o.finding != "" || o.kataIssue != "" || o.replyTo != "") {
		return fmt.Errorf("--finding, --close, --kata and --reply-to require --rebuttal")
	}
	if o.enabled && strings.TrimSpace(o.finding) == "" {
		return fmt.Errorf("--rebuttal requires --finding with an excerpt of the original review")
	}
	if (o.kataIssue == "") != (o.replyTo == "") {
		return fmt.Errorf("--kata and --reply-to must be supplied together")
	}
	return nil
}

func rebuttalPost(ctx context.Context, path string, body any, result any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	ep := getDaemonEndpoint()
	client := ep.APIClient(30 * time.Second)
	var resp *http.Response
	switch path {
	case "/api/rebuttal":
		resp, err = client.RebuttalRaw(ctx, nil, roborevclient.WithBody(data))
	case "/api/comment":
		resp, err = client.AddCommentRaw(ctx, nil, roborevclient.WithBody(data))
	case "/api/review/close":
		resp, err = client.CloseReviewRaw(ctx, nil, roborevclient.WithBody(data))
	default:
		return fmt.Errorf("unsupported rebuttal API path %q", path)
	}
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s: %s: %s", path, resp.Status, string(b))
	}
	if result != nil {
		return json.UnmarshalRead(resp.Body, result)
	}
	return nil
}

func runCommentRebuttal(cmd *cobra.Command, jobID int64, commenter, message string, o rebuttalOptions) error {
	ctx := cmd.Context()
	var job storage.ReviewJob
	if err := rebuttalPost(ctx, "/api/rebuttal", daemon.RebuttalRequest{JobID: jobID, Finding: o.finding, Comment: message, Commenter: commenter}, &job); err != nil {
		return fmt.Errorf("queue rebuttal: %w", err)
	}
	cmd.Printf("Rebuttal job %d queued for review %d\n", job.ID, jobID)
	review, err := waitForJobCompletion(ctx, getDaemonEndpoint().BaseURL(), job.ID, nil)
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
	if err := rebuttalPost(ctx, "/api/comment", daemon.AddCommentRequest{
		JobID: jobID, Commenter: job.Agent, Comment: body,
		ExpectedReviewUUID: job.OriginalReviewUUID,
	}, nil); err != nil {
		return fmt.Errorf("record rebuttal job %d: %w; original review left open", job.ID, err)
	}
	if o.kataIssue != "" {
		if job.UUID == nil {
			return fmt.Errorf("rebuttal job %d has no UUID for a safe Kata reply", job.ID)
		}
		key := "roborev:rebuttal:" + job.UUID.String() //nolint:forbidigo // Kata idempotency key text boundary.
		workdir := job.RepoPath
		if job.WorktreePath != "" {
			workdir = job.WorktreePath
		}
		if err := kata.NewCLIClient(workdir).Reply(ctx, kata.ReplyReq{Issue: o.kataIssue, Target: o.replyTo, Kind: assessment.Outcome, Body: body, IdempotencyKey: key}); err != nil {
			return fmt.Errorf("rebuttal job %d recorded locally, Kata reply failed: %w; original review left open", job.ID, err)
		}
	}
	if o.close {
		if err := rebuttalPost(ctx, "/api/review/close", daemon.CloseReviewRequest{
			JobID: jobID, Closed: true, ExpectedReviewUUID: job.OriginalReviewUUID,
		}, nil); err != nil {
			return fmt.Errorf("rebuttal job %d recorded but close failed: %w", job.ID, err)
		}
	}
	cmd.Printf("%s: %s\n", assessment.Outcome, assessment.Explanation)
	return nil
}
