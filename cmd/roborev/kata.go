package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"uuid"

	"github.com/spf13/cobra"

	client "go.kenn.io/roborev/pkg/client"
	"go.kenn.io/roborev/pkg/client/generated"
)

func kataCmd() *cobra.Command {
	root := &cobra.Command{Use: "kata", Short: "Inspect or publish review findings in Kata"}
	var findings, project string
	show := &cobra.Command{Use: "show JOB|REVIEW-UUID|ISSUE", Short: "Show the Kata discussion for a review or issue", Args: cobra.ExactArgs(1)}
	create := &cobra.Command{Use: "create JOB", Short: "Create or resume the Kata publication for a review", Args: cobra.ExactArgs(1)}
	run := func(write bool) func(*cobra.Command, []string) error {
		return func(cmd *cobra.Command, args []string) error {
			id, idErr := strconv.ParseInt(args[0], 10, 64)
			if write && (idErr != nil || id <= 0) {
				return validationErr(cmd, fmt.Errorf("job must be a positive numeric ID"))
			}
			if findings != "" && findings != "comments" && findings != "summary" {
				return validationErr(cmd, fmt.Errorf("findings must be comments or summary"))
			}
			if err := ensureDaemon(); err != nil {
				return fmt.Errorf("connect to daemon: %w", err)
			}
			api := getDaemonEndpoint().APIClient(0)
			var response *generated.KataPublicationBody
			var err error
			if write {
				body := generated.KataPublicationRequest{JobID: id}
				if findings != "" {
					body.Findings = &findings
				}
				response, err = api.KataPublish(cmd.Context(), &generated.KataPublishRequestOptions{Body: &body})
			} else {
				query := &generated.KataPublicationQuery{}
				if idErr == nil && id > 0 {
					query.JobID = &id
				} else if _, parseErr := uuid.Parse(args[0]); parseErr == nil { //nolint:forbidigo // Parse the user-supplied selector.
					query.ReviewUUID = &args[0]
				} else {
					query.Issue = &args[0]
				}
				if project != "" {
					query.Project = &project
				}
				response, err = api.KataPublication(cmd.Context(), &generated.KataPublicationRequestOptions{Query: query})
			}
			if err != nil {
				if !write {
					classified := client.ClassifyKataContextError(err)
					if errors.Is(classified, client.ErrKataAuthentication) || errors.Is(classified, client.ErrKataAccess) || errors.Is(classified, context.Canceled) || errors.Is(classified, context.DeadlineExceeded) {
						return fmt.Errorf("Kata context: %w", classified)
					}
				}
				status, answered := daemonAnswerStatus(err)
				if answered && status == http.StatusNotFound {
					if _, structured := errors.AsType[generated.ErrorModel](err); !structured {
						return fmt.Errorf("daemon does not support Kata publication; restart it")
					}
				}
				return daemonRequestError("Kata publication", err)
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Status: %s", response.State)
			if response.Expired {
				fmt.Fprint(out, " (expired)")
			}
			fmt.Fprintln(out)
			if response.Project != nil {
				fmt.Fprintf(out, "Project: %s\n", *response.Project)
			}
			if response.IssueUID != nil {
				fmt.Fprintf(out, "Issue: %s (%s)\n", deref(response.IssueShortID), deref(response.IssueUID))
			}
			if response.Project != nil && *response.Project != "" {
				fmt.Fprintf(out, "Project: %s\n", *response.Project)
			}
			if response.DestinationID != nil {
				fmt.Fprintf(out, "Destination: %s\n", *response.DestinationID)
			}
			if response.SourceOutput != nil && *response.SourceOutput != "" {
				fmt.Fprintf(out, "\nOriginal review:\n%s\n", *response.SourceOutput)
			}
			if response.Issue != nil {
				fmt.Fprintf(out, "\nIssue: %s\n%s\n", response.Issue.Title, response.Issue.Body)
			}
			for _, root := range response.Roots {
				handle := ""
				if root.Comment != nil {
					handle = deref(root.Comment.Handle)
				}
				if root.EndPosition != nil {
					fmt.Fprintf(out, "Findings %d-%d: %s", root.Position+1, *root.EndPosition+1, root.State)
				} else {
					fmt.Fprintf(out, "Finding %d: %s", root.Position+1, root.State)
				}
				if handle != "" {
					fmt.Fprintf(out, " %s", handle)
				}
				if root.Comment != nil {
					fmt.Fprintf(out, " (author: %s; UID: %s)", root.Comment.Author, root.Comment.UID)
				}
				fmt.Fprintln(out)
			}
			for _, comment := range response.Comments {
				fmt.Fprintf(out, "\nComment %s (%s; %s) by %s", deref(comment.Handle), comment.UID, deref(comment.ReplyKind), comment.Author)
				if comment.ReplyToUID != nil {
					fmt.Fprintf(out, " to %s", *comment.ReplyToUID)
				}
				if comment.Teammate != nil {
					fmt.Fprintf(out, " (%s)", *comment.Teammate)
				}
				if comment.Lineage != nil {
					fmt.Fprintf(out, "; lineage: %s", *comment.Lineage)
				}
				if comment.EditedAt != nil {
					fmt.Fprint(out, " [edited]")
				}
				fmt.Fprint(out, "; edit history: unknown")
				fmt.Fprintf(out, ":\n%s\n", comment.Body)
			}
			if response.Message != nil && strings.TrimSpace(*response.Message) != "" {
				return fmt.Errorf("publication incomplete: %s", *response.Message)
			}
			return nil
		}
	}
	show.RunE = run(false)
	show.Flags().StringVar(&project, "project", "", "Kata project for an explicit issue or UUID")
	create.RunE = run(true)
	create.Flags().StringVar(&findings, "findings", "", "disclosure for a previously unmarked review: comments or summary")
	root.AddCommand(show, create, kataResponseCmd())
	return root
}
