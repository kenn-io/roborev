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
				if problem, ok := errors.AsType[generated.ErrorModel](err); ok && problem.Detail != nil {
					detail := sanitizeSearchSingleLine(*problem.Detail)
					problem.Detail = &detail
					err = problem
				}
				return daemonRequestError("Kata publication", err)
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Status: %s", sanitizeSearchSingleLine(response.State))
			if response.Expired {
				fmt.Fprint(out, " (expired)")
			}
			fmt.Fprintln(out)
			if response.IssueUID != nil {
				fmt.Fprintf(out, "Issue: %s (%s)\n", sanitizeSearchSingleLine(deref(response.IssueShortID)), sanitizeSearchSingleLine(deref(response.IssueUID)))
			}
			if response.DestinationID != nil {
				fmt.Fprintf(out, "Destination: %s\n", sanitizeSearchSingleLine(*response.DestinationID))
			}
			for _, root := range response.Roots {
				handle := ""
				if root.Comment != nil {
					handle = sanitizeSearchSingleLine(deref(root.Comment.Handle))
				}
				if root.EndPosition != nil {
					fmt.Fprintf(out, "Findings %d-%d: %s", root.Position+1, *root.EndPosition+1, sanitizeSearchSingleLine(root.State))
				} else {
					fmt.Fprintf(out, "Finding %d: %s", root.Position+1, sanitizeSearchSingleLine(root.State))
				}
				if handle != "" {
					fmt.Fprintf(out, " %s", handle)
				}
				if root.Comment != nil {
					fmt.Fprintf(out, " (author: %s; UID: %s)", sanitizeSearchSingleLine(root.Comment.Author), sanitizeSearchSingleLine(root.Comment.UID))
				}
				fmt.Fprintln(out)
			}
			for _, comment := range response.Comments {
				if comment.ReplyToUID == nil {
					continue
				}
				fmt.Fprintf(out, "Reply %s (%s; %s) by %s to %s", sanitizeSearchSingleLine(deref(comment.Handle)), sanitizeSearchSingleLine(comment.UID), sanitizeSearchSingleLine(deref(comment.ReplyKind)), sanitizeSearchSingleLine(comment.Author), sanitizeSearchSingleLine(*comment.ReplyToUID))
				if comment.Teammate != nil {
					fmt.Fprintf(out, " (%s)", sanitizeSearchSingleLine(*comment.Teammate))
				}
				if comment.EditedAt != nil {
					fmt.Fprint(out, " [edited]")
				}
				fmt.Fprintf(out, ":\n%s\n", sanitizeSearchExcerpt(comment.Body))
			}
			if response.Message != nil && strings.TrimSpace(*response.Message) != "" {
				return fmt.Errorf("publication incomplete: %s", sanitizeSearchSingleLine(*response.Message))
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
