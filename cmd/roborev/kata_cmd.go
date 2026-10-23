package main

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"strconv"

	"github.com/spf13/cobra"
	gitrepo "go.kenn.io/kit/git/repo"

	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/pkg/client/generated"
)

func kataCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "kata", Short: "Publish reviews to Kata and inspect their delivery"}
	cmd.AddCommand(kataCreateCmd(), kataShowCmd())
	return cmd
}

func kataCreateCmd() *cobra.Command { return kataPublicationCmd(true) }

func kataShowCmd() *cobra.Command { return kataPublicationCmd(false) }

func kataPublicationCmd(publish bool) *cobra.Command {
	var forceJobID bool
	var jsonOutput bool
	name, description := "show", "Look up a job's Kata issues and finding comments"
	if publish {
		name, description = "create", "Explicitly publish a completed job through its matching Kata hooks"
	}
	cmd := &cobra.Command{Use: name + " <job_id|sha>", Short: description, Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		jobID, err := resolveKataJob(cmd, args[0], forceJobID)
		if err != nil {
			return err
		}
		api := getDaemonEndpoint().APIClient(0)
		var body any
		if publish {
			response, err := api.KataPublish(cmd.Context(), &generated.KataPublishRequestOptions{Body: &generated.KataPublishRequest{JobID: jobID}})
			if err != nil {
				return daemonRequestError("publish review to Kata", err)
			}
			body = response
		} else {
			response, err := api.KataPublications(cmd.Context(), &generated.KataPublicationsRequestOptions{Query: &generated.KataPublicationsQuery{JobID: jobID}})
			if err != nil {
				return daemonRequestError("look up Kata publications", err)
			}
			body = response
		}
		if jsonOutput {
			return json.MarshalEncode(jsontext.NewEncoder(cmd.OutOrStdout(), jsontext.WithIndent("  ")), body)
		}
		// Decode the generated response into the shared public presentation type.
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		var result daemon.KataPublicationsBody
		if err = json.Unmarshal(data, &result); err != nil {
			return err
		}
		if len(result.Publications) == 0 {
			fmt.Fprintf(cmd.OutOrStdout(), "No saved Kata publications for job %d.\n", jobID)
		}
		for _, publication := range result.Publications {
			issue := publication.Issue
			if issue == "" {
				issue = "issue pending"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s  %s  %s (%s)\n", publication.OperationID, issue, publication.State, publication.Policy)
			for _, finding := range publication.Findings {
				comment := finding.Comment
				if comment == "" {
					comment = "comment pending"
				}
				fmt.Fprintf(cmd.OutOrStdout(), "  %s  %s  %s\n", finding.ID, comment, finding.State)
			}
			if publication.LastError != "" {
				fmt.Fprintln(cmd.OutOrStdout(), "  "+publication.LastError)
			}
		}
		return nil
	}}
	cmd.Flags().BoolVar(&forceJobID, "job", false, "force argument to be treated as a job ID (not SHA)")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "output delivery mappings as JSON")
	return cmd
}

// resolveKataJob follows the usual git-ref-first selection, with --job forcing
// a positive numeric ID. Ref lookup uses the daemon's existing job resolver.
func resolveKataJob(cmd *cobra.Command, ref string, forceJobID bool) (int64, error) {
	ctx := cmd.Context()
	if getDaemonEndpoint().IsRemote() && !forceJobID {
		return 0, usageErr(cmd, fmt.Errorf("remote Kata commands require --job with a numeric job ID"))
	}
	var sha string
	if !forceJobID {
		if root, err := gitrepo.Root(ctx, "."); err == nil {
			sha, _ = gitrepo.Resolve(ctx, root, ref)
		}
	}
	var jobID int64
	if sha == "" {
		id, err := strconv.ParseInt(ref, 10, 64)
		if err != nil || id <= 0 {
			return 0, usageErr(cmd, fmt.Errorf("argument %q is not a valid git ref or positive job ID", ref))
		}
		jobID = id
	}
	if err := ensureDaemon(); err != nil {
		return 0, fmt.Errorf("daemon not running: %w", err)
	}
	if jobID != 0 {
		return jobID, nil
	}
	root, err := gitrepo.MainRoot(ctx, ".")
	if err != nil {
		return 0, err
	}
	job, err := findJobForCommit(root, sha)
	if err != nil {
		return 0, err
	}
	if job == nil {
		return 0, fmt.Errorf("no review job found for %s", ref)
	}
	return job.ID, nil
}
