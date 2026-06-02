package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	gitrepo "go.kenn.io/kit/git/repo"

	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/tokens"
)

// showPanelMember is one reviewer in the additive show --json panel block.
type showPanelMember struct {
	JobID      int64  `json:"job_id"`
	Name       string `json:"name"`
	Agent      string `json:"agent"`
	ReviewType string `json:"review_type"`
	Status     string `json:"status"`
	Verdict    string `json:"verdict,omitempty"`
}

// showPanelBlock is the additive "panel" object on show --json for a synthesis
// (parent) review: the run handle plus its member reviewers.
type showPanelBlock struct {
	RunUUID        string            `json:"run_uuid"`
	Name           string            `json:"name"`
	SynthesisJobID int64             `json:"synthesis_job_id"`
	Members        []showPanelMember `json:"members"`
}

// buildShowPanelBlock maps a run's member jobs to the panel block. Members are
// assumed already ordered by panel_member_index.
func buildShowPanelBlock(synthesisJobID int64, runUUID, name string, members []storage.ReviewJob) showPanelBlock {
	block := showPanelBlock{RunUUID: runUUID, Name: name, SynthesisJobID: synthesisJobID}
	for _, m := range members {
		member := showPanelMember{
			JobID:      m.ID,
			Name:       m.PanelMemberName,
			Agent:      m.Agent,
			ReviewType: m.ReviewType,
			Status:     string(m.Status),
		}
		if m.Verdict != nil {
			member.Verdict = *m.Verdict
		}
		block.Members = append(block.Members, member)
	}
	return block
}

// formatReviewersSummary renders a one-line panel header for human output,
// e.g. "3 reviewers: bug P, security F, design -" ('-' = no verdict yet).
func formatReviewersSummary(members []storage.ReviewJob) string {
	parts := make([]string, len(members))
	for i, m := range members {
		verdict := "-"
		if m.Verdict != nil && *m.Verdict != "" {
			verdict = *m.Verdict
		}
		name := m.PanelMemberName
		if name == "" {
			name = m.Agent
		}
		parts[i] = fmt.Sprintf("%s %s", name, verdict)
	}
	return fmt.Sprintf("%d reviewers: %s", len(members), strings.Join(parts, ", "))
}

func showCmd() *cobra.Command {
	var forceJobID bool
	var showPrompt bool
	var jsonOutput bool

	cmd := &cobra.Command{
		Use:   "show [job_id|sha]",
		Short: "Show review for a commit or job",
		Long: `Show review output for a commit or job.

The argument can be either a job ID (numeric) or a commit SHA.
Job IDs are displayed in review notifications and the TUI.

In a git repo, the argument is first tried as a git ref. If that fails
and it's numeric, it's treated as a job ID. Use --job to force job ID.

Examples:
  roborev show              # Show review for HEAD
  roborev show abc123       # Show review for commit
  roborev show 42           # Job ID (if "42" is not a valid git ref)
  roborev show --job 42     # Force as job ID even if "42" is a valid ref
  roborev show --prompt 42  # Show the prompt sent to the agent`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			// Ensure daemon is running (and restart if version mismatch)
			if err := ensureDaemon(); err != nil {
				return fmt.Errorf("daemon not running: %w", err)
			}

			ep := getDaemonEndpoint()
			addr := ep.BaseURL()
			client := ep.HTTPClient(5 * time.Second)

			var queryURL string
			var displayRef string

			if len(args) == 0 {
				if forceJobID {
					return usageErr(cmd, fmt.Errorf("--job requires a job ID argument"))
				}
				// Default to HEAD
				sha := "HEAD"
				root, rootErr := gitrepo.Root(ctx, ".")
				if rootErr != nil {
					return fmt.Errorf("not in a git repository; use a job ID instead (e.g., roborev show 42)")
				}
				if resolved, err := gitrepo.Resolve(ctx, root, sha); err == nil {
					sha = resolved
				}
				queryURL = addr + "/api/review?sha=" + sha
				displayRef = gitrepo.ShortSHA(sha)
			} else {
				arg := args[0]
				var isJobID bool
				var resolvedSHA string

				if forceJobID {
					isJobID = true
				} else {
					// Try to resolve as SHA first (handles numeric SHAs like "123456")
					if root, err := gitrepo.Root(ctx, "."); err == nil {
						if resolved, err := gitrepo.Resolve(ctx, root, arg); err == nil {
							resolvedSHA = resolved
						}
					}
					// If not resolvable as SHA and is numeric, treat as job ID
					if resolvedSHA == "" {
						if _, err := strconv.ParseInt(arg, 10, 64); err == nil {
							isJobID = true
						}
					}
				}

				if isJobID {
					queryURL = addr + "/api/review?job_id=" + arg
					displayRef = "job " + arg
				} else {
					sha := arg
					if resolvedSHA != "" {
						sha = resolvedSHA
					}
					queryURL = addr + "/api/review?sha=" + sha
					displayRef = gitrepo.ShortSHA(sha)
				}
			}

			resp, err := client.Get(queryURL)
			if err != nil {
				return fmt.Errorf("failed to connect to daemon (is it running?)")
			}
			defer resp.Body.Close()

			if resp.StatusCode == http.StatusNotFound {
				return fmt.Errorf("no review found for %s", displayRef)
			}

			var review storage.Review
			if err := json.NewDecoder(resp.Body).Decode(&review); err != nil {
				return fmt.Errorf("failed to parse response: %w", err)
			}

			if jsonOutput {
				// Include comments so tools/skills can see developer feedback.
				type reviewWithComments struct {
					storage.Review
					Comments []storage.Response `json:"comments,omitempty"`
					Panel    *showPanelBlock    `json:"panel,omitempty"`
				}
				out := reviewWithComments{Review: review}
				out.Comments = fetchShowComments(client, addr, review)
				if review.Job != nil && review.Job.IsSynthesisJob() && review.Job.PanelRunUUID != "" {
					if members, err := fetchPanelMembers(client, addr, review.Job.PanelRunUUID); err == nil && len(members) > 0 {
						block := buildShowPanelBlock(review.JobID, review.Job.PanelRunUUID, review.Job.PanelName, members)
						out.Panel = &block
					}
				}
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(&out)
			}

			// Avoid redundant "job X (job X, ...)" output
			if strings.HasPrefix(displayRef, "job ") {
				fmt.Printf("Review for %s (by %s)\n", displayRef, review.Agent)
			} else {
				fmt.Printf("Review for %s (job %d, by %s)\n", displayRef, review.JobID, review.Agent)
			}
			if review.Job != nil {
				if tu := tokens.ParseJSON(review.Job.TokenUsage); tu != nil {
					fmt.Printf("Tokens: %s\n", tu.FormatSummary())
				}
			}
			if review.Job != nil && review.Job.IsSynthesisJob() && review.Job.PanelRunUUID != "" {
				if members, err := fetchPanelMembers(client, addr, review.Job.PanelRunUUID); err == nil && len(members) > 0 {
					fmt.Println(formatReviewersSummary(members))
				}
			}
			fmt.Println(strings.Repeat("-", 60))
			if showPrompt {
				fmt.Println(review.Prompt)
			} else {
				fmt.Println(review.Output)
			}

			// Fetch and display comments (including legacy commit-based)
			if allComments := fetchShowComments(client, addr, review); len(allComments) > 0 {
				fmt.Println()
				fmt.Println("--- Comments ---")
				for _, r := range allComments {
					ts := r.CreatedAt.Format("Jan 02 15:04")
					fmt.Printf("\n[%s] %s:\n", ts, r.Responder)
					fmt.Println(r.Response)
				}
			}

			return nil
		},
	}

	cmd.Flags().BoolVar(&forceJobID, "job", false, "force argument to be treated as job ID")
	cmd.Flags().BoolVar(&showPrompt, "prompt", false, "show the prompt sent to the agent instead of the review output")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "output as JSON")
	return cmd
}

// fetchPanelMembers loads a panel run's member rows (ordered by member index)
// via GET /api/jobs?panel_run=<uuid>. The endpoint returns members plus the
// synthesis row; this keeps only members. limit=0 requests the full run so a
// panel with >=50 rows is not truncated (the synthesis row also counts toward
// the default cap).
func fetchPanelMembers(client *http.Client, addr, runUUID string) ([]storage.ReviewJob, error) {
	u := addr + "/api/jobs?panel_run=" + url.QueryEscape(runUUID) + "&limit=0"
	resp, err := client.Get(u)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list panel members: server returned %s", resp.Status)
	}

	var result struct {
		Jobs []storage.ReviewJob `json:"jobs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	var members []storage.ReviewJob
	for _, j := range result.Jobs {
		if j.PanelRole == storage.PanelRoleMember {
			members = append(members, j)
		}
	}
	sort.Slice(members, func(i, j int) bool {
		return members[i].PanelMemberIndex < members[j].PanelMemberIndex
	})
	return members, nil
}

// fetchShowComments retrieves comments for a review, merging legacy
// SHA-based comments via storage.MergeResponses.
func fetchShowComments(client *http.Client, addr string, review storage.Review) []storage.Response {
	var responses []storage.Response

	// Fetch by job ID
	commentsURL := addr + fmt.Sprintf("/api/comments?job_id=%d", review.JobID)
	if resp, err := client.Get(commentsURL); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not fetch comments for job %d: %v\n", review.JobID, err)
	} else if resp != nil {
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			var result struct {
				Responses []storage.Response `json:"responses"`
			}
			if json.NewDecoder(resp.Body).Decode(&result) == nil {
				responses = result.Responses
			}
		}
	}

	// Also fetch legacy commit-based comments and merge.
	// Prefer commit_id (unambiguous), fall back to SHA for legacy jobs.
	var legacyURL string
	if review.Job != nil {
		commitID, fallbackSHA := review.Job.LegacyCommentLookupTarget()
		if commitID > 0 {
			legacyURL = addr + fmt.Sprintf("/api/comments?commit_id=%d", commitID)
		} else if fallbackSHA != "" {
			legacyURL = addr + fmt.Sprintf("/api/comments?sha=%s", fallbackSHA)
		}
	}
	if legacyURL != "" {
		if resp, err := client.Get(legacyURL); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: could not fetch legacy comments: %v\n", err)
		} else if resp != nil {
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				var result struct {
					Responses []storage.Response `json:"responses"`
				}
				if json.NewDecoder(resp.Body).Decode(&result) == nil {
					responses = storage.MergeResponses(responses, result.Responses)
				}
			}
		}
	}

	return responses
}
