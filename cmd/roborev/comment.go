package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	gitrepo "go.kenn.io/kit/git/repo"
)

func commentCmd() *cobra.Command {
	var (
		commenter  string
		message    string
		forceJobID bool
	)

	cmd := &cobra.Command{
		Use:   "comment <job_id|sha> [message]",
		Short: "Add a comment to a review",
		Long: `Add a comment or note to a review.

The first argument can be either a job ID (numeric) or a commit SHA.
Using job IDs is recommended since they are displayed in the TUI.

Examples:
  roborev comment 42 "Fixed the null pointer issue"
  roborev comment 42 -m "Added missing error handling"
  roborev comment abc123 "Addressed by refactoring"
  roborev comment 42     # Opens editor for message
  roborev comment --job 1234567 "msg"  # Force numeric arg as job ID`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			ref := args[0]

			// Check if ref is a job ID (numeric) or SHA
			var jobID int64
			var sha string

			if forceJobID {
				// --job flag: treat ref as job ID
				id, err := strconv.ParseInt(ref, 10, 64)
				if err != nil {
					return usageErr(cmd, fmt.Errorf("--job requires numeric job ID, got %q", ref))
				}
				jobID = id
			} else {
				// Auto-detect: try git object first, then job ID
				// A numeric string could be either - check if it resolves as a git object first
				if root, err := gitrepo.Root(ctx, "."); err == nil {
					if resolved, err := gitrepo.Resolve(ctx, root, ref); err == nil {
						sha = resolved
					}
				}

				// If not a valid git object, try parsing as job ID
				if sha == "" {
					if id, err := strconv.ParseInt(ref, 10, 64); err == nil {
						jobID = id
					} else {
						// Not a valid git object or job ID - use ref as-is
						sha = ref
					}
				}
			}

			// Ensure daemon is running
			if err := ensureDaemon(); err != nil {
				return fmt.Errorf("daemon not running: %w", err)
			}

			// Message can be positional argument or flag
			if len(args) > 1 {
				message = args[1]
			}

			// If no message provided, open editor
			if message == "" {
				editor := os.Getenv("EDITOR")
				if editor == "" {
					editor = "vim"
				}

				tmpfile, err := os.CreateTemp("", "roborev-comment-*.md")
				if err != nil {
					return fmt.Errorf("create temp file: %w", err)
				}
				tmpfile.Close()
				defer os.Remove(tmpfile.Name())

				editorCmd := exec.Command(editor, tmpfile.Name())
				editorCmd.Stdin = os.Stdin
				editorCmd.Stdout = os.Stdout
				editorCmd.Stderr = os.Stderr
				if err := editorCmd.Run(); err != nil {
					return fmt.Errorf("editor failed: %w", err)
				}

				content, err := os.ReadFile(tmpfile.Name())
				if err != nil {
					return fmt.Errorf("read comment: %w", err)
				}
				message = strings.TrimSpace(string(content))
			}

			if message == "" {
				return fmt.Errorf("empty comment, aborting")
			}

			if commenter == "" {
				commenter = os.Getenv("USER")
				if commenter == "" {
					commenter = "anonymous"
				}
			}

			// Build request with either job_id or sha
			reqData := map[string]any{
				"commenter": commenter,
				"comment":   message,
			}
			if jobID != 0 {
				reqData["job_id"] = jobID
			} else {
				reqData["sha"] = sha
			}

			reqBody, _ := json.Marshal(reqData)

			ep := getDaemonEndpoint()
			addr := ep.BaseURL()
			resp, err := ep.HTTPClient(5*time.Second).Post(addr+"/api/comment", "application/json", bytes.NewReader(reqBody))
			if err != nil {
				return fmt.Errorf("failed to connect to daemon: %w", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusCreated {
				body, _ := io.ReadAll(resp.Body)
				return fmt.Errorf("failed to add comment: %s", body)
			}

			fmt.Println("Comment added successfully")
			return nil
		},
	}

	cmd.Flags().StringVar(&commenter, "commenter", "", "commenter name (default: $USER)")
	cmd.Flags().StringVarP(&message, "message", "m", "", "comment message (opens editor if not provided)")
	cmd.Flags().BoolVar(&forceJobID, "job", false, "force argument to be treated as job ID (not SHA)")

	return cmd
}

// respondCmd returns an alias for commentCmd
func respondCmd() *cobra.Command {
	cmd := commentCmd()
	cmd.Use = "respond <job_id|sha> [message]"
	cmd.Short = "Alias for 'comment' - add a comment to a review"
	return cmd
}

func closeCmd() *cobra.Command {
	var reopen bool

	cmd := &cobra.Command{
		Use:     "close <job_id>",
		Short:   "Close a review (mark as resolved)",
		Aliases: []string{"address"},
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Ensure daemon is running
			if err := ensureDaemon(); err != nil {
				return fmt.Errorf("daemon not running: %w", err)
			}

			jobID, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil || jobID <= 0 {
				return fmt.Errorf("invalid job_id: %s", args[0])
			}

			closed := !reopen
			reqBody, _ := json.Marshal(map[string]any{
				"job_id": jobID,
				"closed": closed,
			})

			ep := getDaemonEndpoint()
			addr := ep.BaseURL()
			resp, err := ep.HTTPClient(5*time.Second).Post(addr+"/api/review/close", "application/json", bytes.NewReader(reqBody))
			if err != nil {
				return fmt.Errorf("failed to connect to daemon: %w", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(resp.Body)
				return fmt.Errorf("failed to mark review: %s", body)
			}

			if closed {
				fmt.Printf("Job %d closed\n", jobID)
			} else {
				fmt.Printf("Job %d reopened\n", jobID)
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&reopen, "reopen", false, "reopen a closed review")
	// Deprecated alias for --reopen
	cmd.Flags().BoolVar(&reopen, "unaddress", false, "deprecated: use --reopen")
	_ = cmd.Flags().MarkHidden("unaddress")

	return cmd
}
