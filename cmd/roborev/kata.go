package main

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/roborev/pkg/client/generated"
)

func kataCmd() *cobra.Command {
	root := &cobra.Command{Use: "kata", Short: "Inspect or publish review findings in Kata"}
	var findings string
	show := &cobra.Command{Use: "show JOB", Short: "Show the Kata publication for a review", Args: cobra.ExactArgs(1)}
	create := &cobra.Command{Use: "create JOB", Short: "Create or resume the Kata publication for a review", Args: cobra.ExactArgs(1)}
	run := func(write bool) func(*cobra.Command, []string) error {
		return func(cmd *cobra.Command, args []string) error {
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil || id <= 0 {
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
			if write {
				body := generated.KataPublicationRequest{JobID: id}
				if findings != "" {
					body.Findings = &findings
				}
				response, err = api.KataPublish(cmd.Context(), &generated.KataPublishRequestOptions{Body: &body})
			} else {
				response, err = api.KataPublication(cmd.Context(), &generated.KataPublicationRequestOptions{Query: &generated.KataPublicationQuery{JobID: id}})
			}
			if err != nil {
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
			if response.IssueUID != nil {
				fmt.Fprintf(out, "Issue: %s (%s)\n", deref(response.IssueShortID), deref(response.IssueUID))
			}
			if response.DestinationID != nil {
				fmt.Fprintf(out, "Destination: %s\n", *response.DestinationID)
			}
			for _, root := range response.Roots {
				handle := ""
				if root.Comment != nil {
					handle = deref(root.Comment.Handle)
				}
				fmt.Fprintf(out, "Finding %d: %s", root.Position+1, root.State)
				if handle != "" {
					fmt.Fprintf(out, " %s", handle)
				}
				if root.Comment != nil {
					fmt.Fprintf(out, " (author: %s; UID: %s)", root.Comment.Author, root.Comment.UID)
				}
				fmt.Fprintln(out)
			}
			for _, comment := range response.Comments {
				if comment.ReplyToUID == nil {
					continue
				}
				fmt.Fprintf(out, "Reply %s by %s to %s", deref(comment.Handle), comment.Author, *comment.ReplyToUID)
				if comment.Teammate != nil {
					fmt.Fprintf(out, " (%s)", *comment.Teammate)
				}
				fmt.Fprintf(out, ":\n%s\n", comment.Body)
			}
			if response.Message != nil && strings.TrimSpace(*response.Message) != "" {
				return fmt.Errorf("publication incomplete: %s", *response.Message)
			}
			return nil
		}
	}
	show.RunE = run(false)
	create.RunE = run(true)
	create.Flags().StringVar(&findings, "findings", "", "disclosure for a previously unmarked review: comments or summary")
	root.AddCommand(show, create)
	return root
}
