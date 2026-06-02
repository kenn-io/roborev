package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
	gitrepo "go.kenn.io/kit/git/repo"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/git"
)

// hookHTTPClient returns an HTTP client for hook requests. Short timeout
// ensures hooks never block commits if the daemon stalls.
// Tests can override this variable to inject custom transports.
var hookHTTPClient = func() *http.Client {
	return getDaemonHTTPClient(3 * time.Second)
}

// hookLogPath can be overridden in tests.
var hookLogPath = ""

func postCommitCmd() *cobra.Command {
	var (
		repoPath   string
		baseBranch string
	)

	cmd := &cobra.Command{
		Use:   "post-commit",
		Short: "Hook entry point: enqueue a review after commit",
		Args:  cobra.NoArgs,
		// Hook entrypoint: any failure must be silent (logged to the
		// hook log file, never printed to git's stderr). Set both
		// silences explicitly so future changes that return non-nil
		// from RunE don't leak Cobra-formatted output to git.
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if repoPath == "" {
				repoPath = "."
			}

			root, err := gitrepo.Root(ctx, repoPath)
			if err != nil {
				hookLog(repoPath, "skip", "not a git repo")
				return nil
			}

			if git.IsRebaseInProgress(root) {
				hookLog(root, "skip", "rebase in progress")
				return nil
			}

			// Migrate stale relative core.hooksPath to absolute
			// so linked worktrees resolve hooks correctly.
			_ = gitrepo.EnsureAbsoluteHooksPath(ctx, root)

			if err := ensureDaemon(); err != nil {
				hookLog(root, "fail", fmt.Sprintf(
					"daemon unavailable: %v", err,
				))
				return nil
			}

			var gitRef string
			if ref, ok := tryBranchReview(ctx, root, baseBranch); ok {
				gitRef = ref
			} else {
				gitRef = "HEAD"
			}

			branchName := gitrepo.CurrentBranch(ctx, root)

			reqBody, _ := json.Marshal(daemon.EnqueueRequest{
				RepoPath: root,
				GitRef:   gitRef,
				Branch:   branchName,
				Source:   "post_commit",
			})

			ep := getDaemonEndpoint()
			resp, err := hookHTTPClient().Post(
				ep.BaseURL()+"/api/enqueue",
				"application/json",
				bytes.NewReader(reqBody),
			)
			if err != nil {
				hookLog(root, "fail", fmt.Sprintf(
					"enqueue request failed: %v", err,
				))
				return nil
			}
			defer resp.Body.Close()

			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode >= 400 {
				hookLog(root, "fail", fmt.Sprintf(
					"daemon returned %d: %s",
					resp.StatusCode,
					truncateBytes(body, 200),
				))
				return nil
			}

			hookLog(root, "ok", fmt.Sprintf(
				"enqueued ref=%s branch=%s", gitRef, branchName,
			))
			return nil
		},
	}

	cmd.Flags().StringVar(
		&repoPath, "repo", "",
		"path to git repository (default: current directory)",
	)
	cmd.Flags().StringVar(
		&baseBranch, "base", "",
		"base branch for branch review comparison",
	)

	// Accept --quiet without error for backward compat with
	// old hooks that called `roborev enqueue --quiet`.
	var quiet bool
	cmd.Flags().BoolVarP(
		&quiet, "quiet", "q", false,
		"accepted for backward compatibility (no-op)",
	)
	_ = cmd.Flags().MarkHidden("quiet")

	return cmd
}

// enqueueCmd returns a hidden backward-compatibility alias
// for postCommitCmd. Old hooks that call `roborev enqueue`
// continue to work.
func enqueueCmd() *cobra.Command {
	cmd := postCommitCmd()
	cmd.Use = "enqueue"
	cmd.Hidden = true
	return cmd
}

// hookLog appends a single JSONL entry to the post-commit log.
// Best-effort: errors are silently ignored so the hook never
// blocks a commit.
func hookLog(repo, outcome, message string) {
	logPath := hookLogPath
	if logPath == "" {
		logPath = filepath.Join(
			config.DataDir(), "post-commit.log",
		)
	}

	entry := struct {
		TS      string `json:"ts"`
		Repo    string `json:"repo"`
		Outcome string `json:"outcome"`
		Message string `json:"message"`
	}{
		TS:      time.Now().Format(time.RFC3339),
		Repo:    repo,
		Outcome: outcome,
		Message: message,
	}
	data, err := json.Marshal(entry)
	if err != nil {
		return
	}
	data = append(data, '\n')

	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return
	}
	f, err := os.OpenFile(
		logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600,
	)
	if err != nil {
		return
	}
	defer f.Close()
	_ = f.Chmod(0o600)
	_, _ = f.Write(data)
}

func truncateBytes(b []byte, max int) string {
	if len(b) <= max {
		return string(b)
	}
	return string(b[:max]) + "..."
}
