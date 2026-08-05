package agent

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.kenn.io/roborev/internal/procutil"
)

var subprocessWaitDelay = 5 * time.Second

type subprocessTracker struct {
	canceledByContext atomic.Bool
}

// subprocessConfig holds the options configureSubprocess accepts.
type subprocessConfig struct {
	keepGitHubCredentials bool
}

type subprocessOption func(*subprocessConfig)

// withGitHubCredentials keeps GH_TOKEN/GITHUB_TOKEN in the child
// environment. Only for agent CLIs that authenticate with a GitHub token
// and cannot start without it (copilot, kiro-cli); see forge_env.go.
func withGitHubCredentials() subprocessOption {
	return func(cfg *subprocessConfig) {
		cfg.keepGitHubCredentials = true
	}
}

func configureSubprocess(cmd *exec.Cmd, opts ...subprocessOption) *subprocessTracker {
	var cfg subprocessConfig
	for _, opt := range opts {
		opt(&cfg)
	}

	procutil.HideConsole(cmd)
	cmd.WaitDelay = subprocessWaitDelay

	// Prevent agents from taking .git/index.lock in the user's repo.
	// Git 2.15+ honours GIT_OPTIONAL_LOCKS=0: read-only commands like
	// "git status" and "git diff" skip the optional index lock they
	// normally take to refresh cached stat data. Without this, agent
	// processes running in the background contend with the user's own
	// git operations (staging, committing, etc.).
	if cmd.Env == nil {
		cmd.Env = cmd.Environ()
	}
	// Single choke point for forge credential removal: agents process
	// untrusted PR/MR content and must not be able to read the tokens
	// roborev posts comments with. See forge_env.go.
	//
	// Logged for the same reason the ACP path logs it: an agentic fix job whose
	// plan shells out to gh fails with "authentication required" and nothing in
	// that error points at roborev having removed the token.
	cmd.Env = logRemovedUntrustedEnv(
		cmd.Env,
		stripUntrustedEnv(cmd.Env, cfg.keepGitHubCredentials),
		"agent "+filepath.Base(cmd.Path))
	cmd.Env = append(cmd.Env, "GIT_OPTIONAL_LOCKS=0")

	tracker := &subprocessTracker{}
	// Ensure Cancel is always set. Go's exec.CommandContext only provides a
	// default Kill cancel when both Cancel==nil and WaitDelay==0. Since we
	// set WaitDelay above, the default is suppressed and context cancellation
	// would never signal the process without this.
	if cmd.Cancel == nil {
		cmd.Cancel = func() error {
			return cmd.Process.Kill()
		}
	}
	cancel := cmd.Cancel
	cmd.Cancel = func() error {
		err := cancel()
		if err == nil {
			tracker.canceledByContext.Store(true)
		}
		return err
	}
	return tracker
}

func configureCapabilityProbe(cmd *exec.Cmd) {
	procutil.HideConsole(cmd)
	// A probe runs the agent binary (`claude --help` and friends) before any
	// review starts, so it needs the same environment scrub as the review
	// subprocess. Skipping it left the forge tokens readable to anything a
	// preload hook injected into that binary, ahead of the sanitized run.
	if cmd.Env == nil {
		cmd.Env = cmd.Environ()
	}
	cmd.Env = logRemovedUntrustedEnv(
		cmd.Env,
		stripUntrustedEnv(cmd.Env, false),
		"agent probe "+filepath.Base(cmd.Path))
	if cmd.Path != "" &&
		!filepath.IsAbs(cmd.Path) &&
		strings.ContainsAny(cmd.Path, `/\`) {
		if absPath, err := filepath.Abs(cmd.Path); err == nil {
			cmd.Path = absPath
		}
	}
	cmd.Dir = os.TempDir()
}

func closeOnContextDone(ctx context.Context, c io.Closer) func() {
	if c == nil || ctx.Done() == nil {
		return func() {}
	}
	done := make(chan struct{})
	var once sync.Once
	var stopped atomic.Bool
	go func() {
		select {
		case <-ctx.Done():
			if stopped.Load() {
				return
			}
			_ = c.Close()
		case <-done:
		}
	}()
	return func() {
		stopped.Store(true)
		once.Do(func() {
			close(done)
		})
	}
}

func contextProcessError(
	ctx context.Context, tracker *subprocessTracker, runErr, parseErr error,
) error {
	ctxErr := ctx.Err()
	if ctxErr == nil {
		return nil
	}
	if runErr != nil {
		if errors.Is(runErr, ctxErr) ||
			errors.Is(runErr, exec.ErrWaitDelay) ||
			(tracker != nil &&
				tracker.canceledByContext.Load() &&
				processErrIndicatesContextTermination(runErr)) {
			return ctxErr
		}
		return nil
	}
	if parseErr != nil &&
		parseErrIndicatesClosedPipe(parseErr) &&
		tracker != nil &&
		tracker.canceledByContext.Load() {
		return ctxErr
	}
	return nil
}

func parseErrIndicatesClosedPipe(err error) bool {
	return errors.Is(err, fs.ErrClosed) ||
		strings.Contains(err.Error(), "file already closed")
}

func processErrIndicatesContextTermination(err error) bool {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "signal: killed") ||
		strings.Contains(msg, "signal: terminated")
}
