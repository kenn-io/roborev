package main

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	gitrepo "go.kenn.io/kit/git/repo"

	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/pkg/client/generated"
)

func listCmd() *cobra.Command {
	var (
		branch        string
		allBranches   bool
		repoPath      string
		limit         int
		status        string
		analysisType  string
		analysisFiles []string
		jsonOutput    bool
		closed        bool
		open          bool
	)

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List review jobs",
		Long: `List review jobs with optional filtering.

By default, lists jobs for the current repo and branch.

Examples:
  roborev list                        # Jobs for current repo/branch
  roborev list --json                 # Output as JSON
  roborev list --branch main          # Jobs for main branch
  roborev list --all-branches         # Jobs across all branches
  roborev list --status done          # Only completed jobs
  roborev list --open                 # Only open (unresolved) reviews
  roborev list --closed               # Only closed reviews
  roborev list --limit 5              # Show at most 5 jobs`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if branch != "" && allBranches {
				return usageErr(cmd, fmt.Errorf("--branch and --all-branches are mutually exclusive"))
			}

			ctx := cmd.Context()
			if err := ensureDaemon(); err != nil {
				return fmt.Errorf("daemon not running: %w", err)
			}

			ep := getDaemonEndpoint()

			// Auto-resolve repo from cwd when not specified.
			// Use worktree root for branch detection, main repo root for API queries
			// (daemon stores jobs under the main repo path).
			localRepoPath := repoPath
			if localRepoPath == "" {
				if root, err := gitrepo.Root(ctx, "."); err == nil {
					localRepoPath = root
				}
			}
			if repoPath == "" {
				if root, err := gitrepo.MainRoot(ctx, "."); err == nil {
					repoPath = root
				}
			} else {
				// Normalize explicit --repo to main repo root so worktree
				// paths match the daemon's stored repo path.
				if root, err := gitrepo.MainRoot(ctx, repoPath); err == nil {
					repoPath = root
				}
			}
			// Auto-resolve branch from the target repo when not specified.
			if branch == "" && !allBranches && localRepoPath != "" {
				branch = gitrepo.CurrentBranch(ctx, localRepoPath)
			}

			// Workspace mode: not in a git repo and no --repo specified
			var repoPrefix string
			if repoPath == "" && localRepoPath == "" {
				if abs, err := filepath.Abs("."); err == nil {
					repoPrefix = filepath.ToSlash(abs)
				}
			}

			// Build query URL
			params := generated.ListJobsQuery{}
			if repoPrefix != "" {
				params.RepoPrefix = new(repoPrefix)
			} else if repoPath != "" {
				params.Repo = []string{repoPath}
			}
			if branch != "" && (repoPrefix == "" || cmd.Flags().Changed("branch")) {
				params.Branch = new(branch)
				params.BranchIncludeEmpty = new(generated.ListJobsQueryBranchIncludeEmpty("true"))
			}
			if status != "" {
				params.Status = new(status)
			}
			if analysisType != "" {
				params.AnalysisType = new(analysisType)
			}
			if len(analysisFiles) > 0 {
				basePath := localRepoPath
				if basePath == "" {
					basePath = repoPath
				}
				params.AnalysisFile = make([]string, 0, len(analysisFiles))
				for _, file := range analysisFiles {
					params.AnalysisFile = append(params.AnalysisFile, normalizeListFile(basePath, file))
				}
			}
			if closed {
				params.Closed = new(generated.ListJobsQueryClosed("true"))
			} else if open {
				params.Closed = new(generated.ListJobsQueryClosed("false"))
			}
			params.Limit = new(int64(limit))

			client := ep.APIClient(5 * time.Second)
			resp, err := client.ListJobsRaw(cmd.Context(), &generated.ListJobsRequestOptions{Query: &params})
			if err != nil {
				return fmt.Errorf("failed to connect to daemon (is it running?)")
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(resp.Body)
				return fmt.Errorf("daemon returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
			}

			var jobsResp struct {
				Jobs    []storage.ReviewJob `json:"jobs"`
				HasMore bool                `json:"has_more"`
			}
			if err := json.UnmarshalRead(resp.Body, &jobsResp); err != nil {
				return fmt.Errorf("failed to parse response: %w", err)
			}

			if jsonOutput {
				enc := jsontext.NewEncoder(os.Stdout, jsontext.WithIndent("  "))
				return json.MarshalEncode(enc, jobsResp.Jobs)
			}

			if len(jobsResp.Jobs) == 0 {
				fmt.Println("No jobs found.")
				return nil
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			showFiles := false
			for _, j := range jobsResp.Jobs {
				if len(j.AnalysisFiles) > 0 {
					showFiles = true
					break
				}
			}
			if showFiles {
				fmt.Fprintf(w, "ID\tSHA\tRepo\tAgent\tStatus\tTime\tFiles\n")
			} else {
				fmt.Fprintf(w, "ID\tSHA\tRepo\tAgent\tStatus\tTime\n")
			}
			for _, j := range jobsResp.Jobs {
				elapsed := ""
				if j.StartedAt != nil {
					if j.FinishedAt != nil {
						elapsed = j.FinishedAt.Sub(*j.StartedAt).Round(time.Second).String()
					} else {
						elapsed = time.Since(*j.StartedAt).Round(time.Second).String() + "..."
					}
				}
				if showFiles {
					fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\t%s\n",
						j.ID, shortRef(j.GitRef), j.RepoName, j.Agent, j.Status, elapsed, strings.Join(j.AnalysisFiles, ", "))
				} else {
					fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\n",
						j.ID, shortRef(j.GitRef), j.RepoName, j.Agent, j.Status, elapsed)
				}
			}
			w.Flush()

			if jobsResp.HasMore {
				fmt.Println("(more results available, use --limit to increase)")
			}

			return nil
		},
	}

	cmd.Flags().StringVar(&branch, "branch", "", "filter by branch (default: current branch)")
	cmd.Flags().BoolVar(&allBranches, "all-branches", false, "include jobs from all branches")
	cmd.Flags().StringVar(&repoPath, "repo", "", "filter by repo path (default: current repo)")
	cmd.Flags().IntVar(&limit, "limit", 50, "max number of jobs to return")
	cmd.Flags().StringVar(&status, "status", "", "filter by status (queued, running, done, failed)")
	cmd.Flags().StringVar(&analysisType, "analysis-type", "", "filter by recorded analysis type")
	cmd.Flags().StringArrayVar(&analysisFiles, "file", nil, "filter by recorded repository-relative analysis file")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "output as JSON")
	cmd.Flags().BoolVar(&closed, "closed", false, "show only closed reviews")
	cmd.Flags().BoolVar(&open, "open", false, "show only open reviews")
	cmd.Flags().BoolVar(&open, "unaddressed", false, "show only open reviews")
	_ = cmd.Flags().MarkHidden("unaddressed")
	cmd.MarkFlagsMutuallyExclusive("closed", "open")
	cmd.MarkFlagsMutuallyExclusive("closed", "unaddressed")
	return cmd
}

func normalizeListFile(repoRoot, file string) string {
	clean := filepath.Clean(file)
	if repoRoot != "" {
		path := file
		if !filepath.IsAbs(path) {
			path = filepath.Join(repoRoot, path)
		}
		if abs, err := filepath.Abs(path); err == nil {
			if rel, err := filepath.Rel(repoRoot, abs); err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				clean = rel
			}
		}
	}
	return filepath.ToSlash(clean)
}
