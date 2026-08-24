package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	gitrepo "go.kenn.io/kit/git/repo"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/git"
)

// hookHTTPClient returns an HTTP client for hook requests with the given
// timeout, which bounds how long the hook waits for the daemon so a stalled
// daemon never blocks a commit. The timeout is resolved from config (see
// config.ResolveHookTimeout). Tests override this variable to inject custom
// transports.
var hookHTTPClient = func(timeout time.Duration) *http.Client {
	return getDaemonHTTPClient(timeout)
}

// hookLogPath can be overridden in tests.
var hookLogPath = ""

// testHookAfterBatchPlan runs after batch planning and before the enqueue
// request is built. Tests use it to simulate concurrent git activity.
var testHookAfterBatchPlan = func() {}

func postCommitCmd() *cobra.Command {
	var (
		repoPath    string
		baseBranch  string
		flush       bool
		flushPush   bool
		flushBranch string
		flushHead   string
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
				// Include the underlying error: failures here are
				// not always "no repo" (e.g. git exits 128 on
				// dubious-ownership refusals) and the hook log is
				// the only place they surface.
				hookLog(repoPath, "skip", fmt.Sprintf(
					"not a git repo: %v", err,
				))
				return nil
			}
			if flushPush {
				flushPushedPostCommitBatches(ctx, root, os.Stdin)
				return nil
			}

			// The rebase guard suppresses per-commit enqueues while a rebase
			// replays commits. Pre-push flushes carry an explicit branch and
			// pushed SHA that a rebase in this worktree cannot corrupt, and
			// skipping them would push pending commits unreviewed.
			if flushBranch == "" && git.IsRebaseInProgress(root) {
				hookLog(root, "skip", "rebase in progress")
				return nil
			}

			// Migrate stale relative core.hooksPath to absolute
			// so linked worktrees resolve hooks correctly.
			_ = gitrepo.EnsureAbsoluteHooksPath(ctx, root)
			var batch postCommitBatchDecision
			batchSize := 0
			unlock, err := acquirePostCommitBatchLock(ctx, root)
			switch {
			case err != nil && (flush || flushBranch != ""):
				// A flush can wait: its pending range is recorded, so a
				// later commit or push retries it.
				hookLog(root, "fail", fmt.Sprintf(
					"acquire post-commit lock: %v", err,
				))
				return nil
			case err != nil:
				// Fail open like every other error: an immediate
				// single-commit review that reads and writes no batch state.
				// In immediate mode nothing would retry a dropped commit.
				batch = postCommitBatchDecision{
					Ready:  true,
					Reason: "acquire post-commit lock: " + err.Error(),
				}
			default:
				// Keep planning, enqueueing, and checkpoint advancement in
				// one transaction. Releasing this lock before the daemon
				// response would let a concurrent hook plan from stale batch
				// state.
				defer func() { _ = unlock() }()
				var err error
				batchSize, err = config.ResolvePostCommitBatchSizeWithError(root)
				if err != nil {
					hookLog(root, "fail", fmt.Sprintf(
						"load batch config: %v", err,
					))
					return nil
				}
				if flushBranch != "" {
					batch = planStoredPostCommitBatch(
						ctx, root, batchSize, flushBranch, flushHead,
					)
				} else {
					batch = planPostCommitBatch(ctx, root, batchSize)
				}
			}
			testHookAfterBatchPlan()
			if flush && !batch.Enabled {
				hookLog(root, "skip", "batch flush disabled")
				return nil
			}
			if batch.Enabled && !batch.Ready && (!flush || batch.Pending == 0) {
				hookLog(root, "skip", fmt.Sprintf(
					"batch pending branch=%s commits=%d threshold=%d",
					batch.Branch, batch.Pending, batchSize,
				))
				return nil
			}

			if err := ensureDaemon(); err != nil {
				hookLog(root, "fail", fmt.Sprintf(
					"daemon unavailable: %v", err,
				))
				return nil
			}

			gitRef := "HEAD"
			if batch.Enabled {
				gitRef = batch.AccumulatedRef()
			}
			// Enqueue the branch and head the plan captured under the batch
			// lock. Re-reading live git state here would let a concurrent
			// commit or checkout change what is reviewed while
			// advancePostCommitBatch still advances the planned branch.
			branchName := gitrepo.CurrentBranch(ctx, root)
			headRef := "HEAD"
			if flushBranch != "" {
				branchName = flushBranch
				headRef = "refs/heads/" + flushBranch
			}
			if batch.Branch != "" {
				branchName = batch.Branch
			}
			if batch.Head != "" {
				headRef = batch.Head
			}
			if ref, ok := tryBranchReviewForRef(
				ctx, root, baseBranch, headRef, branchName,
			); ok {
				gitRef = ref
			}

			reqBody, _ := json.Marshal(daemon.EnqueueRequest{
				RepoPath: root,
				GitRef:   gitRef,
				Branch:   branchName,
				Source:   "post_commit",
			})

			// Resolve the hook timeout from config (per-repo > global >
			// platform default). ResolveHookTimeout is strictly filesystem-only
			// (it reads .roborev.toml directly and never spawns git), so it adds
			// no git-subprocess latency to the hook. A failed global load falls
			// back to the platform default inside Resolve.
			globalCfg, _ := config.LoadGlobal()
			timeout := config.ResolveHookTimeout(root, globalCfg)

			ep := getDaemonEndpoint()
			resp, err := hookHTTPClient(timeout).Post(
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

			checkpointErr := advancePostCommitBatch(ctx, root, batch)
			message := fmt.Sprintf(
				"enqueued ref=%s branch=%s", gitRef, branchName,
			)
			if batch.Enabled {
				message += fmt.Sprintf(" batch_commits=%d", batch.Pending)
			}
			if batch.Reason != "" {
				message += " fallback=" + batch.Reason
			}
			if checkpointErr != nil {
				message += " checkpoint_error=" + checkpointErr.Error()
			}
			hookLog(root, "ok", message)
			return nil
		},
	}

	cmd.Flags().StringVar(
		&repoPath, "repo", "",
		"path to git repository (default: current directory)",
	)
	cmd.Flags().BoolVar(&flush, "flush", false, "flush pending batched commits")
	_ = cmd.Flags().MarkHidden("flush")
	cmd.Flags().BoolVar(&flushPush, "flush-push", false, "flush pushed branches")
	_ = cmd.Flags().MarkHidden("flush-push")
	cmd.Flags().StringVar(&flushBranch, "flush-branch", "", "branch to flush")
	_ = cmd.Flags().MarkHidden("flush-branch")
	cmd.Flags().StringVar(&flushHead, "flush-head", "", "pushed commit to flush")
	_ = cmd.Flags().MarkHidden("flush-head")
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

func flushPushedPostCommitBatches(
	ctx context.Context, root string, input io.Reader,
) {
	branches := make(map[string]string)
	pushedHeads := make(map[string]struct{})
	scanner := bufio.NewScanner(input)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		if strings.Trim(fields[1], "0") == "" {
			continue
		}
		head, err := git.ResolveSHACtx(ctx, root, fields[1]+"^{commit}")
		if err != nil {
			continue
		}
		pushedHeads[head] = struct{}{}
		branch := strings.TrimPrefix(fields[0], "refs/heads/")
		if branch == fields[0] {
			if fields[0] != "HEAD" {
				continue
			}
			branch = git.GetCurrentBranch(root)
			if branch == "" {
				continue
			}
		}
		branches[branch] = head
	}
	// A push also carries any unpushed ancestor commits, so stored batch
	// branches whose ranges sit on a pushed head's chain flush too —
	// otherwise a child branch pushes its parent's pending commits without a
	// review. Ancestor flushes run as separate invocations so a branch pushed
	// by name can additionally flush a recorded range on abandoned history.
	ancestors := pushedAncestorFlushBranches(ctx, root, branches, pushedHeads)
	for branch, head := range branches {
		flushPushedBranch(ctx, root, branch, head)
	}
	for branch, head := range ancestors {
		flushPushedBranch(ctx, root, branch, head)
	}
}

func flushPushedBranch(ctx context.Context, root, branch, head string) {
	branchRoot, checkedOut, err := git.WorktreePathForBranch(root, branch)
	if err != nil || !checkedOut {
		// No checkout holds this branch. Flush from the pushing worktree
		// anyway: its config may differ from the branch's own, but pushed
		// commits must never leave the machine unreviewed.
		hookLog(root, "ok", fmt.Sprintf(
			"batch flush using pushing worktree for branch=%s", branch,
		))
		branchRoot = root
	}
	cmd := postCommitCmd()
	cmd.SetContext(ctx)
	cmd.SetArgs([]string{
		"--repo", branchRoot, "--flush", "--flush-branch", branch,
		"--flush-head", head,
	})
	_ = cmd.Execute()
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
