package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
	"uuid"

	"github.com/spf13/cobra"
	katagen "go.kenn.io/kata/pkg/client/generated"
	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/kata"
	"go.kenn.io/roborev/pkg/client/generated"
)

func kataResponseCmd() *cobra.Command {
	var project, destination, key, body string
	kinds := map[string]*string{}
	cmd := &cobra.Command{Use: "comment ISSUE", Short: "Reply to a published Kata finding", Args: cobra.ExactArgs(1)}
	for _, kind := range []string{"reply", "confirm", "refute", "supersede"} {
		value := new(string)
		kinds[kind] = value
		cmd.Flags().StringVar(value, kind, "", "reply kind and target comment UID or c: handle")
	}
	cmd.Flags().StringVarP(&body, "message", "m", "", "reply text or evidence")
	cmd.Flags().StringVar(&project, "project", "", "Kata project")
	cmd.Flags().StringVar(&destination, "destination-id", "", "expected destination from a previous attempt")
	cmd.Flags().StringVar(&key, "idempotency-key", "", "opaque key for retrying this reply")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		kind, target := "", ""
		for name, value := range kinds {
			if cmd.Flags().Changed(name) {
				if kind != "" || *value == "" {
					return validationErr(cmd, errors.New("select exactly one nonempty reply target"))
				}
				kind, target = name, *value
			}
		}
		if kind == "" {
			return validationErr(cmd, errors.New("select exactly one reply kind"))
		}
		if !cmd.Flags().Changed("idempotency-key") {
			key = uuid.New().String()
		}
		if err := kata.ValidateReply(kind, body, key); err != nil {
			return validationErr(cmd, err)
		}
		ref := args[0]
		if name, _, qualified := strings.Cut(ref, "#"); qualified && project != "" && name != project {
			return validationErr(cmd, errors.New("conflicting project selectors"))
		}
		if err := ensureDaemon(); err != nil {
			return fmt.Errorf("connect to daemon: %w", err)
		}
		ep := getDaemonEndpoint()
		api := ep.APIClient(0)
		query := &generated.KataShowIssueQuery{Ref: ref}
		if project != "" {
			query.Project = &project
		} else if !strings.Contains(ref, "#") {
			dir, err := os.Getwd()
			if err != nil {
				return err
			}
			binding, err := localKataBinding(cmd.Context(), dir)
			if err == nil {
				query.Project, query.WorkspaceAliasIdentity, query.WorkspaceAliasKind = kataSelectionFields(binding)
			} else if !errors.Is(err, kata.ErrNoBinding) {
				return err
			}
		}
		issue, err := api.KataShowIssue(cmd.Context(), &generated.KataShowIssueRequestOptions{Query: query})
		if err != nil {
			return kataResponseError("read Kata issue", err)
		}
		if issue.UID == nil || issue.IssueID == nil || issue.ProjectID == nil || issue.Project == nil || issue.ProjectUID == nil || issue.DestinationID == nil || issue.CommentsComplete == nil || !*issue.CommentsComplete || issue.PublicationEnvelope == nil {
			return errors.New("Kata issue lacks complete publication identity or comments")
		}
		if destination != "" && destination != *issue.DestinationID {
			return errors.New("Kata destination changed")
		}
		env := issue.PublicationEnvelope
		metadata := map[string]any{"roborev.version": env.Version, "roborev.review_uuid": env.ReviewUUID.String(), "roborev.document_hash": env.DocumentHash, "roborev.policy": env.Policy, "roborev.completed_at": env.CompletedAt.Format(time.RFC3339Nano), "roborev.finding_count": env.FindingCount}
		full := kata.PublicationIssue{Issue: katagen.ShowIssueOut{UID: *issue.UID, ID: *issue.IssueID, ProjectID: *issue.ProjectID, Metadata: metadata}}
		for _, c := range issue.Comments {
			full.Comments = append(full.Comments, katagen.CommentOut{UID: c.UID, Handle: c.Handle, Body: c.Body, Author: c.Author, EditedAt: c.EditedAt, IssueID: c.IssueID, IssueUID: c.IssueUID, ProjectID: c.ProjectID, ProjectUID: c.ProjectUID, ProjectName: c.ProjectName, ReplyToUID: c.ReplyToUID, ReplyKind: c.ReplyKind})
		}
		selected, err := kata.ResolvePublicationTarget(kata.PublicationProject{ID: full.Issue.ProjectID, Name: *issue.Project, UID: *issue.ProjectUID}, full, target)
		if err != nil {
			return err
		}
		retry := fmt.Sprintf("roborev --server %s kata comment %s --project %s --destination-id %s --%s %s --idempotency-key %s -m %s", daemon.ShellEscape(ep.ConfigAddr()), daemon.ShellEscape(*issue.UID), daemon.ShellEscape(*issue.Project), daemon.ShellEscape(*issue.DestinationID), kind, daemon.ShellEscape(selected), daemon.ShellEscape(key), daemon.ShellEscape(body))
		instructions := fmt.Sprintf("Issue: %s\nTarget: %s\nKind: %s\nKey: %s\nDestination: %s\nProject: %s\nRetry: %s\n", *issue.UID, selected, kind, key, *issue.DestinationID, *issue.Project, retry)
		n, err := io.WriteString(cmd.OutOrStdout(), instructions)
		if err == nil && n != len(instructions) {
			err = io.ErrShortWrite
		}
		if err != nil {
			return fmt.Errorf("write retry instructions: %w", err)
		}
		request := generated.KataResponseRequest{Project: *issue.Project, DestinationID: *issue.DestinationID, Generation: issue.Generation, IssueUID: *issue.UID, TargetUID: selected, Kind: kind, Body: body, IdempotencyKey: key}
		receipt, err := api.KataResponse(cmd.Context(), &generated.KataResponseRequestOptions{Body: &request})
		if err != nil {
			return kataResponseError("send Kata reply", err)
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "Comment: %s\nAuthor: %s\nTeammate: %s\nChanged: %t\n", receipt.UID, receipt.Author, receipt.Teammate, receipt.Changed)
		return err
	}
	return cmd
}

func kataResponseError(action string, err error) error {
	if status, answered := daemonAnswerStatus(err); answered && status == http.StatusNotFound {
		if _, structured := errors.AsType[generated.ErrorModel](err); !structured {
			return errors.New("daemon does not support Kata replies; restart it")
		}
	}
	return kataDaemonError(action, err)
}
