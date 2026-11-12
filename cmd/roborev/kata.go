package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"uuid"

	"github.com/spf13/cobra"

	client "go.kenn.io/roborev/pkg/client"
	"go.kenn.io/roborev/pkg/client/generated"
)

func kataCmd() *cobra.Command {
	root := &cobra.Command{Use: "kata", Short: "Inspect or publish review findings in Kata"}
	var findings, project string
	var issueSelector bool
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
				if issueSelector {
					query.Issue = &args[0]
				} else if idErr == nil && id > 0 {
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
				if problem, ok := errors.AsType[generated.ErrorModel](err); ok && problem.Detail != nil {
					detail := sanitizeSearchSingleLine(*problem.Detail)
					problem.Detail = &detail
					err = problem
				}
				if !write {
					classified := client.ClassifyKataContextError(err)
					if errors.Is(classified, client.ErrKataAuthentication) || errors.Is(classified, client.ErrKataAccess) || errors.Is(classified, context.Canceled) || errors.Is(classified, context.DeadlineExceeded) {
						return fmt.Errorf("kata context: %w", classified)
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
			fmt.Fprintf(out, "Status: %s", sanitizeSearchSingleLine(response.State))
			if response.Expired {
				fmt.Fprint(out, " (expired)")
			}
			fmt.Fprintln(out)
			project := sanitizeSearchSingleLine(deref(response.Project))
			if response.IssueUID != nil {
				shortID := sanitizeSearchSingleLine(deref(response.IssueShortID))
				if project != "" && shortID != "" {
					shortID = project + "#" + shortID
				}
				if shortID == "" {
					fmt.Fprintf(out, "Issue: %s\n", sanitizeSearchSingleLine(*response.IssueUID))
				} else {
					fmt.Fprintf(out, "Issue: %s (%s)\n", shortID, sanitizeSearchSingleLine(*response.IssueUID))
				}
			}
			if project != "" {
				fmt.Fprintf(out, "Project: %s\n", project)
			}
			if response.DestinationID != nil {
				fmt.Fprintf(out, "Destination: %s\n", sanitizeSearchSingleLine(*response.DestinationID))
			}
			if response.SourceOutput != nil && *response.SourceOutput != "" {
				fmt.Fprintf(out, "\nOriginal review:\n%s\n", sanitizeSearchExcerpt(*response.SourceOutput))
			}
			if response.Issue != nil {
				fmt.Fprintf(out, "\nIssue: %s\n%s\n", sanitizeSearchSingleLine(response.Issue.Title), sanitizeSearchExcerpt(response.Issue.Body))
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
				label := "Comment"
				if comment.ReplyToUID != nil {
					label = "Reply"
				}
				kind := sanitizeSearchSingleLine(deref(comment.ReplyKind))
				if kind != "" {
					kind = "; " + kind
				}
				fmt.Fprintf(out, "\n%s %s (%s%s) by %s", label, sanitizeSearchSingleLine(deref(comment.Handle)), sanitizeSearchSingleLine(comment.UID), kind, sanitizeSearchSingleLine(comment.Author))
				if comment.ReplyToUID != nil {
					fmt.Fprintf(out, " to %s", sanitizeSearchSingleLine(*comment.ReplyToUID))
				}
				if comment.Teammate != nil {
					fmt.Fprintf(out, " (%s)", sanitizeSearchSingleLine(*comment.Teammate))
				}
				if comment.Lineage != nil {
					fmt.Fprintf(out, "; lineage: %s", sanitizeSearchSingleLine(*comment.Lineage))
				}
				if comment.EditedAt != nil {
					fmt.Fprint(out, " [edited]")
				}
				fmt.Fprint(out, "; edit history: unknown")
				fmt.Fprintf(out, ":\n%s\n", sanitizeSearchExcerpt(comment.Body))
				if comment.Reply != nil {
					showKataLink(out, "Reply target", comment.UID, *comment.Reply)
				}
				for _, link := range comment.Backlinks {
					showKataLink(out, "Backlink", comment.UID, link)
				}
				if comment.BacklinksTruncated != nil && *comment.BacklinksTruncated {
					fmt.Fprintln(out, "Backlinks truncated")
				}
			}
			if response.Message != nil && strings.TrimSpace(*response.Message) != "" {
				return fmt.Errorf("publication incomplete: %s", sanitizeSearchSingleLine(*response.Message))
			}
			return nil
		}
	}
	show.RunE = run(false)
	show.Flags().BoolVar(&issueSelector, "issue", false, "Interpret the selector as a Kata issue, including numeric short IDs")
	show.Flags().StringVar(&project, "project", "", "Kata project for an issue or unmarked review")
	create.RunE = run(true)
	create.Flags().StringVar(&findings, "findings", "", "disclosure for a previously unmarked review: comments or summary")
	root.AddCommand(show, create, kataResponseCmd())
	return root
}

func showKataLink(out io.Writer, label, enclosingUID string, link generated.CommentLink) {
	fmt.Fprintf(out, "%s for %s: %s (%s; %s)", label, sanitizeSearchSingleLine(enclosingUID), sanitizeSearchSingleLine(deref(link.Handle)), sanitizeSearchSingleLine(deref(link.UID)), sanitizeSearchSingleLine(link.Kind))
	for _, field := range []struct {
		name  string
		value *string
	}{
		{"author", link.Author}, {"teammate", link.Teammate}, {"issue", link.IssueShortID}, {"issue UID", link.IssueUID}, {"project UID", link.ProjectUID}, {"status", link.Status},
	} {
		if field.value != nil {
			fmt.Fprintf(out, "; %s: %s", field.name, sanitizeSearchSingleLine(*field.value))
		}
	}
	if link.ProjectID != nil {
		fmt.Fprintf(out, "; project ID: %d", *link.ProjectID)
	}
	if link.CreatedAt != nil {
		fmt.Fprintf(out, "; created: %s", link.CreatedAt.Format(time.RFC3339Nano))
	}
	if link.EditedAt != nil {
		fmt.Fprintf(out, "; edited: %s", link.EditedAt.Format(time.RFC3339Nano))
	}
	if link.TargetEdited != nil {
		fmt.Fprintf(out, "; target edited: %t", *link.TargetEdited)
	}
	if link.Body != nil {
		fmt.Fprintf(out, ":\n%s", sanitizeSearchExcerpt(*link.Body))
	}
	fmt.Fprintln(out)
}
