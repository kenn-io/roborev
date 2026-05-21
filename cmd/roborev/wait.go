package main

import (
	"errors"
	"fmt"
	"strconv"
	"sync"

	"github.com/roborev-dev/roborev/internal/git"
	"github.com/spf13/cobra"
)

func waitCmd() *cobra.Command {
	var (
		shaFlag    string
		forceJobID bool
		quiet      bool
	)

	cmd := &cobra.Command{
		Use:   "wait [job_id|sha ...]",
		Short: "Wait for an existing review job to complete",
		Long: `Wait for an already-running review job to complete, without enqueuing a new one.

When using an external coding agent to perform a review-fix refinement loop,
wait provides a token-efficient method for letting the agent wait for a roborev
review to complete. The post-commit hook triggers the review, and the agent
calls wait to block until the result is ready.

The argument can be a job ID (numeric) or a git ref (commit SHA, branch, HEAD).
If no argument is given, defaults to HEAD.

Multiple arguments can be given to wait for several jobs concurrently.
With --job, all arguments are treated as job IDs.

Exit codes:
  0  All reviews completed with verdict PASS
  1  Any failure (FAIL verdict, no job found, job error)

Examples:
  roborev wait                   # Wait for most recent job for HEAD
  roborev wait abc123            # Wait for most recent job for commit
  roborev wait 42                # Job ID (if "42" is not a valid git ref)
  roborev wait --job 42          # Force as job ID
  roborev wait --job 10 20 30    # Wait for multiple job IDs
  roborev wait --sha HEAD~1      # Wait for job matching HEAD~1`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			// In quiet mode (post-commit hook), collapse any error into a
			// silent non-zero exit so the hook doesn't write to stderr.
			if quiet {
				defer func() { err = quietExit(cmd, err) }()
			}

			// Validate flag/arg combinations
			if len(args) > 0 && shaFlag != "" {
				return usageErr(cmd, fmt.Errorf("cannot use both a positional argument and --sha"))
			}
			if forceJobID && len(args) == 0 {
				return usageErr(cmd, fmt.Errorf("--job requires a job ID argument"))
			}

			// Multiple args: wait for all concurrently
			if len(args) > 1 {
				return waitMultiple(cmd, args, forceJobID, quiet)
			}

			// Resolve the target to a job ID (local validation first,
			// daemon contact deferred until actually needed)
			var jobID int64
			var ref string // git ref to resolve via findJobForCommit

			if shaFlag != "" {
				ref = shaFlag
			} else if len(args) > 0 {
				arg := args[0]
				if forceJobID {
					// --job flag: treat as job ID directly
					id, err := strconv.ParseInt(arg, 10, 64)
					if err != nil || id <= 0 {
						return fmt.Errorf("invalid job ID: %s", arg)
					}
					jobID = id
				} else {
					// Try to resolve as git ref first (handles numeric SHAs like "123456")
					if repoRoot, err := git.GetRepoRoot("."); err == nil {
						if _, err := git.ResolveSHA(repoRoot, arg); err == nil {
							ref = arg
						}
					}
					if ref == "" {
						// Not a valid git ref — try as numeric job ID
						if id, err := strconv.ParseInt(arg, 10, 64); err == nil && id > 0 {
							jobID = id
						} else {
							return fmt.Errorf("argument %q is not a valid git ref or job ID", arg)
						}
					}
				}
			} else {
				ref = "HEAD"
			}

			// Validate git ref before contacting daemon
			var sha string
			if ref != "" {
				repoRoot, _ := git.GetRepoRoot(".")
				resolved, err := git.ResolveSHA(repoRoot, ref)
				if err != nil {
					return fmt.Errorf("invalid git ref: %s", ref)
				}
				sha = resolved
			}

			// All local validation passed — now ensure daemon is running
			if err := ensureDaemon(); err != nil {
				return fmt.Errorf("daemon not running: %w", err)
			}

			// If we have a ref to resolve, use findJobForCommit
			if sha != "" && jobID == 0 {
				mainRoot, _ := git.GetMainRepoRoot(".")
				if mainRoot == "" {
					mainRoot, _ = git.GetRepoRoot(".")
				}
				job, err := findJobForCommit(mainRoot, sha)
				if err != nil {
					return err
				}
				if job == nil {
					if !quiet {
						cmd.Printf("No job found for %s\n", ref)
					}
					return silentExit(cmd, 1)
				}
				jobID = job.ID
			}

			ep := getDaemonEndpoint()
			waitErr := waitForJob(cmd, ep, jobID, quiet)
			if waitErr != nil {
				// Map ErrJobNotFound to exit 1 with a user-facing message
				// (waitForJob returns a plain error to stay compatible with reviewCmd)
				if errors.Is(waitErr, ErrJobNotFound) {
					if !quiet {
						cmd.Printf("No job found for job %d\n", jobID)
					}
					return silentExit(cmd, 1)
				}
			}
			// Verdict-fail comes back as a bare *exitError from showReview;
			// silence cobra's printing here (showReview can't, since it may
			// be called from waitMultiple goroutines that share cmd).
			return silenceIfExit(cmd, waitErr)
		},
	}

	cmd.Flags().StringVar(&shaFlag, "sha", "", "git ref to find the most recent job for")
	cmd.Flags().BoolVar(&forceJobID, "job", false, "force arguments to be treated as job IDs")
	cmd.Flags().BoolVarP(&quiet, "quiet", "q", false, "suppress informational output and the usage block; runtime errors are still printed")

	return cmd
}

// resolvedArg holds locally-validated arg data before daemon contact.
type resolvedArg struct {
	jobID int64  // set if arg is a numeric job ID
	sha   string // set if arg is a git ref (resolved SHA)
	ref   string // original ref string (for error messages)
}

// waitMultiple resolves multiple args to job IDs and waits for all
// concurrently. Returns exit code 1 if any job fails or has a FAIL
// verdict.
func waitMultiple(
	cmd *cobra.Command,
	args []string,
	forceJobID, quiet bool,
) error {
	// Phase 1: Local validation (no daemon contact).
	repoRoot, _ := git.GetRepoRoot(".")
	resolved := make([]resolvedArg, 0, len(args))
	for _, arg := range args {
		if forceJobID {
			id, err := strconv.ParseInt(arg, 10, 64)
			if err != nil || id <= 0 {
				return fmt.Errorf("invalid job ID: %s", arg)
			}
			resolved = append(resolved, resolvedArg{jobID: id})
		} else {
			// Try git ref first
			var sha string
			if repoRoot != "" {
				if s, err := git.ResolveSHA(repoRoot, arg); err == nil {
					sha = s
				}
			}
			if sha != "" {
				resolved = append(resolved, resolvedArg{sha: sha, ref: arg})
			} else {
				id, err := strconv.ParseInt(arg, 10, 64)
				if err != nil || id <= 0 {
					return fmt.Errorf(
						"argument %q is not a valid git ref or job ID",
						arg,
					)
				}
				resolved = append(resolved, resolvedArg{jobID: id})
			}
		}
	}

	// Phase 2: Ensure daemon is running.
	if err := ensureDaemon(); err != nil {
		return fmt.Errorf("daemon not running: %w", err)
	}

	// Phase 3: Resolve git refs to job IDs via daemon.
	jobIDs := make([]int64, 0, len(resolved))
	for _, r := range resolved {
		if r.jobID != 0 {
			jobIDs = append(jobIDs, r.jobID)
			continue
		}
		mainRoot, _ := git.GetMainRepoRoot(".")
		if mainRoot == "" {
			mainRoot = repoRoot
		}
		job, err := findJobForCommit(mainRoot, r.sha)
		if err != nil {
			return err
		}
		if job == nil {
			if !quiet {
				cmd.Printf("No job found for %s\n", r.ref)
			}
			return silentExit(cmd, 1)
		}
		jobIDs = append(jobIDs, job.ID)
	}

	ep := getDaemonEndpoint()

	// Wait for all jobs concurrently.
	// Always poll in quiet mode to avoid interleaved output from
	// goroutines; we display results serially after all complete.
	type result struct {
		jobID int64
		err   error
	}
	results := make([]result, len(jobIDs))
	var wg sync.WaitGroup
	for i, id := range jobIDs {
		wg.Add(1)
		go func(idx int, jobID int64) {
			defer wg.Done()
			err := waitForJob(cmd, ep, jobID, true)
			results[idx] = result{jobID: jobID, err: err}
		}(i, id)
	}
	wg.Wait()

	// Report results serially.
	var hasErr bool
	for _, r := range results {
		if r.err == nil {
			continue
		}
		hasErr = true
		if !quiet {
			switch {
			case errors.Is(r.err, ErrJobNotFound):
				cmd.Printf("Job %d: no job found\n", r.jobID)
			case isExitCode(r.err, 1):
				cmd.Printf("Job %d: review has issues\n", r.jobID)
			default:
				cmd.Printf("Job %d: %v\n", r.jobID, r.err)
			}
		}
	}

	if hasErr {
		// In quiet mode the per-job loop above printed nothing, so a
		// silent exit would leave the caller with no clue what failed.
		// Surface the first runtime error (plain error, not *exitError)
		// so the quiet defer's quietExit can wrap it and cobra can show
		// the cause. If every error is an *exitError (verdict-fail or
		// job-not-found), nothing more useful can be said.
		if quiet {
			for _, r := range results {
				if r.err == nil {
					continue
				}
				var exitErr *exitError
				if !errors.As(r.err, &exitErr) {
					return r.err
				}
			}
		}
		return silentExit(cmd, 1)
	}

	return nil
}

// isExitCode reports whether err is an *exitError with the given code.
func isExitCode(err error, code int) bool {
	var e *exitError
	if errors.As(err, &e) {
		return e.code == code
	}
	return false
}
