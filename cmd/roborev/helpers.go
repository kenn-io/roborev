package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/roborev-dev/roborev/internal/git"
	"github.com/roborev-dev/roborev/internal/githook"
	"github.com/roborev-dev/roborev/internal/storage"
	"github.com/spf13/cobra"
)

// exitError signals a specific exit code with no further error output.
// Always construct via silentExit so cobra's "Error: ..." line is suppressed
// for this command — the caller has already printed any user-facing message.
// If cause is set, Error()/Unwrap() expose it so programmatic callers can
// still inspect the underlying failure; cobra's printing is still silenced
// via the SilenceErrors flag that silentExit sets.
type exitError struct {
	code  int
	cause error
}

func (e *exitError) Error() string {
	if e.cause != nil {
		return e.cause.Error()
	}
	return fmt.Sprintf("exit code %d", e.code)
}

func (e *exitError) Unwrap() error { return e.cause }

// silentExit returns an exitError and silences cobra's error output for
// the given command. Use when the caller has already printed any user-
// facing message and just needs the process to exit with a specific code.
// Must only be called from a single-threaded context (RunE, post-join code),
// not from goroutines that share cmd.
func silentExit(cmd *cobra.Command, code int) error {
	cmd.SilenceErrors = true
	return &exitError{code: code}
}

// silenceIfExit silences cobra's error output for cmd when err is an
// *exitError, then returns err unchanged. Use at top-level RunE return
// points whose error may have originated in concurrent code that could
// not safely mutate cmd itself.
func silenceIfExit(cmd *cobra.Command, err error) error {
	var exitErr *exitError
	if errors.As(err, &exitErr) {
		cmd.SilenceErrors = true
	}
	return err
}

// usageErr re-enables the usage block for cmd and returns err. Use inside
// RunE for validation that's semantically part of the invocation contract
// (mutually exclusive flags, missing required values, bad enum values).
// PersistentPreRunE on the root silences usage for runtime errors; this
// flips it back for the specific case of "the user invoked me wrong."
func usageErr(cmd *cobra.Command, err error) error {
	cmd.SilenceUsage = false
	return err
}

// quietExit prepares a --quiet command's error for return. It silences
// the usage block (so an upstream usageErr doesn't print a help wall
// in quiet mode) and wraps plain errors in *exitError{code: 1} so the
// process exits non-zero, but it does NOT silence cobra's "Error: ..."
// line — runtime failures (daemon down, bad git ref, polling errors)
// should still tell the user what went wrong. Verdict-based exits use
// the bare *exitError sentinel; their callers silence cobra separately
// via silenceIfExit because that path has already shown the review
// output (or chose not to in quiet mode).
func quietExit(cmd *cobra.Command, err error) error {
	if err == nil {
		return nil
	}
	cmd.SilenceUsage = true
	var exitErr *exitError
	if errors.As(err, &exitErr) {
		return err
	}
	return &exitError{code: 1, cause: err}
}

func shortRef(ref string) string {
	// For ranges like "abc123..def456", show as "abc123..def456" (up to 17 chars)
	// For single SHAs, truncate to 7 chars
	if strings.Contains(ref, "..") {
		if len(ref) > 17 {
			return ref[:17]
		}
		return ref
	}
	return git.ShortSHA(ref)
}

// shortJobRef returns a display-friendly ref for a job, handling special job types.
// Task jobs (no CommitID, no DiffContent) display their GitRef directly (run, analyze, or custom label).
// Regular review jobs display their GitRef shortened.
func shortJobRef(job storage.ReviewJob) string {
	// Task jobs are identified by: no CommitID, no DiffContent
	// (Note: Prompt field is set for ALL jobs after worker starts, so can't use that)
	if job.CommitID == nil && job.DiffContent == nil {
		// Map legacy "prompt" to "run" for display consistency
		if job.GitRef == "prompt" {
			return "run"
		}
		// Return GitRef directly as the display label (run, analyze, or custom)
		return job.GitRef
	}
	return shortRef(job.GitRef)
}

// resolveReasoningWithFast returns the effective reasoning value, applying
// the --fast shorthand only when --reasoning wasn't explicitly set.
func resolveReasoningWithFast(reasoning string, fast bool, reasoningExplicitlySet bool) string {
	if fast && !reasoningExplicitlySet {
		return "fast"
	}
	return reasoning
}

// autoInstallHooks upgrades outdated hooks and installs
// companion hooks (e.g. post-rewrite when post-commit
// exists). It does NOT install hooks from scratch so that
// explicit uninstall-hook is respected.
func autoInstallHooks(repoPath string) {
	hooksDir, err := git.GetHooksPath(repoPath)
	if err != nil {
		return
	}
	for _, name := range []string{"post-commit", "post-rewrite"} {
		marker := githook.VersionMarker(name)
		if githook.NeedsUpgrade(repoPath, name, marker) ||
			githook.Missing(repoPath, name) {
			if err := githook.Install(hooksDir, name, false); err != nil {
				// Non-shell hooks are a persistent condition;
				// don't warn on every invocation.
				if !errors.Is(err, githook.ErrNonShellHook) {
					fmt.Fprintf(os.Stderr,
						"Warning: auto-install %s hook: %v\n",
						name, err)
				}
			}
		}
	}
}
