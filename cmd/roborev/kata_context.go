package main

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"uuid"

	"github.com/spf13/cobra"

	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/katacontext"
	"go.kenn.io/roborev/internal/storage"
	roborevclient "go.kenn.io/roborev/pkg/client"
)

type kataShowView struct {
	JobID              int64                        `json:"job_id,omitempty"`
	IssueRef           string                       `json:"issue_ref,omitempty"`
	Publications       []daemon.KataPublication     `json:"publications"`
	Snapshots          []katacontext.Snapshot       `json:"snapshots"`
	Operations         []storage.KataResponseStatus `json:"operations,omitempty"`
	ContextUnavailable bool                         `json:"context_unavailable,omitempty"`
	Message            string                       `json:"message,omitempty"`
}

// runKataShow extends the parent's one kataShowCmd. Its existing job/SHA
// resolver remains authoritative; issue entry reads only frozen local mappings.
func runKataShow(cmd *cobra.Command, args []string, forceJobID, asJSON bool, issueRef string) error {
	view := kataShowView{IssueRef: issueRef, Publications: []daemon.KataPublication{}, Snapshots: []katacontext.Snapshot{}}
	if issueRef != "" {
		if len(args) != 0 || forceJobID {
			return usageErr(cmd, fmt.Errorf("--issue cannot be combined with a job or commit argument"))
		}
		if err := ensureDaemon(); err != nil {
			return err
		}
		body, err := fetchKataReplyContext(cmd.Context(), getDaemonEndpoint().APIClient(0), 0, "", issueRef, "", "")
		if err != nil {
			return err
		}
		view.Publications, view.Snapshots, view.Operations, view.Message = body.Publications, body.Snapshots, body.Operations, body.Message
	} else {
		if len(args) != 1 {
			return usageErr(cmd, fmt.Errorf("kata show requires a job, commit, or --issue reference"))
		}
		jobID, err := resolveKataJob(cmd, args[0], forceJobID)
		if err != nil {
			return err
		}
		view.JobID = jobID
		api := getDaemonEndpoint().APIClient(0)
		response, err := api.KataPublicationsRaw(cmd.Context(), nil, roborevclient.WithQuery(url.Values{"job_id": {strconv.FormatInt(jobID, 10)}}))
		if err != nil {
			return fmt.Errorf("read saved Kata publications: daemon unavailable")
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return fmt.Errorf("read saved Kata publications: daemon returned HTTP %d", response.StatusCode)
		}
		var saved daemon.KataPublicationsBody
		if err := json.UnmarshalRead(response.Body, &saved); err != nil {
			return fmt.Errorf("invalid saved Kata publications from daemon")
		}
		if saved.JobID != jobID {
			return fmt.Errorf("saved Kata publications belong to another job")
		}
		view.Publications, view.Operations = saved.Publications, saved.Operations
		seen := map[string]bool{}
		for _, publication := range saved.Publications {
			if publication.ReviewUUID == "" || seen[publication.ReviewUUID] {
				continue
			}
			seen[publication.ReviewUUID] = true
			body, err := fetchKataReplyContext(cmd.Context(), api, jobID, publication.ReviewUUID, "", "", "")
			if err != nil {
				view.ContextUnavailable = true
				continue
			}
			view.Snapshots = append(view.Snapshots, body.Snapshots...)
			view.Operations = append(view.Operations, body.Operations...)
			if body.Message != "" {
				view.Message = body.Message
			}
		}
	}
	seenOperations := map[string]bool{}
	operations := view.Operations[:0]
	for _, operation := range view.Operations {
		if !seenOperations[operation.OperationID] {
			seenOperations[operation.OperationID] = true
			operations = append(operations, operation)
		}
	}
	view.Operations = operations
	if asJSON {
		return json.MarshalEncode(jsontext.NewEncoder(cmd.OutOrStdout(), jsontext.WithIndent("  ")), view)
	}
	for _, publication := range view.Publications {
		issue := publication.Issue
		if issue == "" {
			issue = "issue pending"
		}
		cmd.Printf("%s  %s  %s (%s)\n", publication.OperationID, issue, publication.State, publication.Policy)
		for _, finding := range publication.Findings {
			cmd.Printf("  %s  %s  %s\n", finding.ID, finding.Comment, finding.State)
		}
		if publication.LastError != "" {
			cmd.Println("  " + publication.LastError)
		}
	}
	if view.Message != "" {
		cmd.Println(view.Message)
	}
	if view.ContextUnavailable {
		cmd.Println("Kata reply context unavailable; saved publication and delivery status remain visible.")
	}
	if len(view.Snapshots) > 0 {
		cmd.Println(katacontext.Format(view.Snapshots))
	}
	for _, operation := range view.Operations {
		cmd.Printf("%s  %s (%s)\n", operation.OperationID, operation.State, operation.Kind)
		if operation.Target != "" {
			cmd.Println("  Target: " + operation.Target)
		}
		if operation.Receipt != "" {
			cmd.Println("  Receipt: " + operation.Receipt)
		}
		if operation.ExistingComment != "" {
			cmd.Println("  Existing comment: " + operation.ExistingComment)
		}
		if operation.Message != "" {
			cmd.Println("  " + operation.Message)
		}
	}
	if len(view.Publications) == 0 && len(view.Snapshots) == 0 && len(view.Operations) == 0 {
		cmd.Println("No available saved Kata findings or delivery operations for this reference.")
	}
	return nil
}

func fetchKataReplyContext(ctx context.Context, api *roborevclient.Client, jobID int64, reviewUUID, ref, runID, purpose string) (katacontext.KataReplyContext, error) {
	var response *http.Response
	var err error
	if runID == "" {
		query := url.Values{"review_uuid": {reviewUUID}, "ref": {ref}}
		if jobID > 0 {
			query.Set("job_id", strconv.FormatInt(jobID, 10))
		}
		response, err = api.KataContextRaw(ctx, nil, roborevclient.WithQuery(query))
	} else {
		raw, encodeErr := json.Marshal(daemon.KataConsumeContextRequest{JobID: jobID, ReviewUUID: reviewUUID, Ref: ref, RunID: runID, Purpose: purpose})
		if encodeErr != nil {
			return katacontext.KataReplyContext{}, encodeErr
		}
		response, err = api.KataConsumeContextRaw(ctx, nil, roborevclient.WithBody(raw))
	}
	if err != nil {
		return katacontext.KataReplyContext{}, fmt.Errorf("fetch Kata finding replies: daemon unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return katacontext.KataReplyContext{}, fmt.Errorf("fetch Kata finding replies: daemon returned HTTP %d", response.StatusCode)
	}
	var out katacontext.KataReplyContext
	if err := json.UnmarshalRead(response.Body, &out); err != nil {
		return out, fmt.Errorf("invalid Kata reply context from daemon")
	}
	if jobID > 0 && out.JobID != jobID {
		return katacontext.KataReplyContext{}, fmt.Errorf("kata reply context belongs to another job")
	}
	if runID != "" && out.RunID != runID {
		return katacontext.KataReplyContext{}, fmt.Errorf("kata reply context belongs to another run")
	}
	if ref != "" && out.IssueRef != ref {
		return katacontext.KataReplyContext{}, fmt.Errorf("kata reply context belongs to another issue")
	}
	for _, snap := range out.Snapshots {
		if reviewUUID != "" && snap.ReviewUUID != reviewUUID {
			return katacontext.KataReplyContext{}, fmt.Errorf("kata reply context belongs to another review")
		}
	}
	return out, nil
}

// Each invocation owns one consumption ID, shared by its planning and fix
// phases. A subsequent invocation gets a fresh reply snapshot.
func kataReplyPromptReader(api *roborevclient.Client, purpose string) func(context.Context, *storage.Review) string {
	runID := uuid.New().String() //nolint:forbidigo // Consumption UUID in the daemon API request.
	type source struct {
		jobID      int64
		reviewUUID string
	}
	var mu sync.Mutex
	captured := map[source]string{}
	return func(ctx context.Context, review *storage.Review) string {
		if review.UUID == nil || (review.Job != nil && review.Job.IsCIReview()) {
			return ""
		}
		key := source{review.JobID, review.UUID.String()} //nolint:forbidigo // Immutable review UUID in the daemon API query.
		mu.Lock()
		defer mu.Unlock()
		if text, exists := captured[key]; exists {
			return text
		}
		body, err := fetchKataReplyContext(ctx, api, review.JobID, key.reviewUUID, "", runID, purpose)
		text := ""
		if err != nil {
			text = "\n\nKata finding reply context unavailable; evaluate the original findings independently."
		} else if len(body.Snapshots) > 0 {
			text = "\n\nKata reply snapshot run: " + body.RunID + katacontext.Format(body.Snapshots)
		}
		captured[key] = text
		return text
	}
}
