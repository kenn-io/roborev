package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gitrepo "go.kenn.io/kit/git/repo"

	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/githook"
)

func executePostCommitCmd(
	args ...string,
) (string, string, error) {
	var stdout, stderr bytes.Buffer
	cmd := postCommitCmd()
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return stdout.String(), stderr.String(), err
}

func executeEnqueueAliasCmd(
	args ...string,
) (string, string, error) {
	var stdout, stderr bytes.Buffer
	cmd := enqueueCmd()
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return stdout.String(), stderr.String(), err
}

func TestPostCommitSubmitsHEAD(t *testing.T) {
	repo, mux := setupTestEnvironment(t)
	reqCh := mockEnqueue(t, mux)

	repo.CommitFile("file.txt", "content", "initial commit")

	_, _, err := executePostCommitCmd("--repo", repo.Dir)
	require.NoError(t, err)

	req := <-reqCh
	assert.Equal(t, "HEAD", req.GitRef)
}

func TestPostCommitBranchReview(t *testing.T) {
	repo, mux := setupTestEnvironment(t)
	reqCh := mockEnqueue(t, mux)

	repo.Run("symbolic-ref", "HEAD", "refs/heads/main")
	repo.CommitFile("file.txt", "content", "initial")
	mainSHA := repo.Run("rev-parse", "HEAD")
	repo.Run("checkout", "-b", "feature")
	repo.CommitFile("feature.txt", "feature", "feature commit")
	writeRoborevConfig(t, repo, `post_commit_review = "branch"`)

	_, _, err := executePostCommitCmd("--repo", repo.Dir)
	require.NoError(t, err)

	req := <-reqCh
	want := mainSHA + "..HEAD"
	assert.Equal(t, want, req.GitRef)
}

func TestPostCommitFallsBackOnBaseBranch(t *testing.T) {
	repo, mux := setupTestEnvironment(t)
	reqCh := mockEnqueue(t, mux)

	repo.Run("symbolic-ref", "HEAD", "refs/heads/main")
	repo.CommitFile("file.txt", "content", "initial")
	writeRoborevConfig(t, repo, `post_commit_review = "branch"`)

	_, _, err := executePostCommitCmd("--repo", repo.Dir)
	require.NoError(t, err)

	req := <-reqCh
	assert.Equal(t, "HEAD", req.GitRef)
}

func TestPostCommitSilentExitNotARepo(t *testing.T) {
	dir := t.TempDir()
	stdout, stderr, err := executePostCommitCmd("--repo", dir)
	require.NoError(t, err)
	assert.Empty(t, stdout)
	assert.Empty(t, stderr)
}

func TestPostCommitAcceptsQuietFlag(t *testing.T) {
	repo, mux := setupTestEnvironment(t)
	mockEnqueue(t, mux)

	repo.CommitFile("file.txt", "content", "initial")

	_, _, err := executePostCommitCmd(
		"--repo", repo.Dir, "--quiet",
	)
	require.NoError(t, err)
}

func TestEnqueueAliasWorksIdentically(t *testing.T) {
	repo, mux := setupTestEnvironment(t)
	reqCh := mockEnqueue(t, mux)

	repo.CommitFile("file.txt", "content", "initial")

	_, _, err := executeEnqueueAliasCmd("--repo", repo.Dir)
	require.NoError(t, err)

	req := <-reqCh
	assert.Equal(t, "HEAD", req.GitRef)
}

func TestPostCommitRejectsPositionalArgs(t *testing.T) {
	_, _, err := executePostCommitCmd("abc123")
	require.Error(t, err)

	assert.Contains(t, err.Error(), "unknown command")
}

func TestEnqueueRejectsPositionalArgs(t *testing.T) {
	_, _, err := executeEnqueueAliasCmd("abc123")
	require.Error(t, err)
}

func TestPostCommitLogsSuccess(t *testing.T) {
	repo, mux := setupTestEnvironment(t)
	mockEnqueue(t, mux)

	logFile := filepath.Join(t.TempDir(), "post-commit.log")
	old := hookLogPath
	hookLogPath = logFile
	t.Cleanup(func() { hookLogPath = old })

	repo.CommitFile("file.txt", "content", "initial commit")

	_, _, err := executePostCommitCmd("--repo", repo.Dir)
	require.NoError(t, err)

	data, err := os.ReadFile(logFile)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"outcome":"ok"`)
	assert.Contains(t, string(data), `"repo"`)
}

func TestPostCommitLogsSkipNotARepo(t *testing.T) {
	logFile := filepath.Join(t.TempDir(), "post-commit.log")
	old := hookLogPath
	hookLogPath = logFile
	t.Cleanup(func() { hookLogPath = old })

	dir := t.TempDir()
	_, _, err := executePostCommitCmd("--repo", dir)
	require.NoError(t, err)

	data, err := os.ReadFile(logFile)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"outcome":"skip"`)
	assert.Contains(t, string(data), "not a git repo")
}

func TestPostCommitLogsSkipRebase(t *testing.T) {
	repo, _ := setupTestEnvironment(t)
	repo.CommitFile("file.txt", "content", "initial commit")

	logFile := filepath.Join(t.TempDir(), "post-commit.log")
	old := hookLogPath
	hookLogPath = logFile
	t.Cleanup(func() { hookLogPath = old })

	// Simulate rebase in progress.
	gitDir := strings.TrimSpace(repo.Run("rev-parse", "--git-dir"))
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(repo.Dir, gitDir)
	}
	require.NoError(t, os.MkdirAll(
		filepath.Join(gitDir, "rebase-merge"), 0o755))

	_, _, err := executePostCommitCmd("--repo", repo.Dir)
	require.NoError(t, err)

	data, err := os.ReadFile(logFile)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"outcome":"skip"`)
	assert.Contains(t, string(data), "rebase in progress")
}

func TestPostCommitLogsDaemonFailure(t *testing.T) {
	logFile := filepath.Join(t.TempDir(), "post-commit.log")
	old := hookLogPath
	hookLogPath = logFile
	t.Cleanup(func() { hookLogPath = old })

	repo := newTestGitRepo(t)
	repo.CommitFile("file.txt", "content", "initial")

	// Point serverAddr at nothing so ensureDaemon fails.
	patchServerAddr(t, "http://127.0.0.1:1")

	_, _, err := executePostCommitCmd("--repo", repo.Dir)
	require.NoError(t, err)

	data, err := os.ReadFile(logFile)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"outcome":"fail"`)
	assert.Contains(t, string(data), "daemon")
}

func TestPostCommitLogsCreatesParentDir(t *testing.T) {
	// Log path under a directory that doesn't exist yet,
	// simulating a fresh install where ~/.roborev is absent.
	logFile := filepath.Join(t.TempDir(), "subdir", "post-commit.log")
	old := hookLogPath
	hookLogPath = logFile
	t.Cleanup(func() { hookLogPath = old })

	dir := t.TempDir()
	_, _, err := executePostCommitCmd("--repo", dir)
	require.NoError(t, err)

	data, err := os.ReadFile(logFile)
	require.NoError(t, err, "log file should be created even when parent dir is absent")
	assert.Contains(t, string(data), `"outcome":"skip"`)
}

// stallingRoundTripper blocks until the request context is
// cancelled, then returns an error. This simulates a daemon
// that accepts connections but never responds, without needing
// a real httptest server or a long sleep.
type stallingRoundTripper struct {
	hit       chan struct{}
	cancelled chan time.Duration
}

func (s *stallingRoundTripper) RoundTrip(
	req *http.Request,
) (*http.Response, error) {
	start := time.Now()
	select {
	case s.hit <- struct{}{}:
	default:
	}
	select {
	case <-req.Context().Done():
		select {
		case s.cancelled <- time.Since(start):
		default:
		}
	case <-time.After(5 * time.Second):
		return nil, fmt.Errorf("stallingRoundTripper: context was never cancelled")
	}
	return nil, fmt.Errorf("request cancelled: %w", req.Context().Err())
}

func TestPostCommitTimesOutOnSlowDaemon(t *testing.T) {
	repo, mux := setupTestEnvironment(t)
	// Register a handler so ensureDaemon succeeds, but the
	// actual POST will go through the stalling RoundTripper.
	realHandlerCalled := false
	mux.HandleFunc("/api/enqueue", func(
		w http.ResponseWriter, r *http.Request,
	) {
		realHandlerCalled = true
	})

	repo.CommitFile("file.txt", "content", "initial")

	rt := &stallingRoundTripper{
		hit:       make(chan struct{}, 1),
		cancelled: make(chan time.Duration, 1),
	}
	orig := hookHTTPClient
	hookHTTPClient = func() *http.Client {
		return &http.Client{
			Timeout:   50 * time.Millisecond,
			Transport: rt,
		}
	}
	t.Cleanup(func() { hookHTTPClient = orig })

	_, _, err := executePostCommitCmd("--repo", repo.Dir)

	require.NoError(t, err)

	select {
	case <-rt.hit:
		// RoundTrip was called — timeout path was exercised
	default:
		require.NoError(t, err, "RoundTrip was never called; timeout not exercised")
	}
	assert.False(t, realHandlerCalled, "real handler should not be reached")

	select {
	case elapsed := <-rt.cancelled:
		assert.LessOrEqual(t, elapsed, time.Second,
			"request took %v; should return promptly via timeout",
			elapsed)
	default:
		require.FailNow(t, "request context was not cancelled by timeout")
	}
}

func TestEnqueueAliasIsHidden(t *testing.T) {
	cmd := enqueueCmd()
	assert.True(t, cmd.Hidden)
	assert.Contains(t, cmd.Use, "enqueue")
}

// repoUnderTest holds a repo for post-commit hook tests.
type repoUnderTest struct {
	// repo is the directory post-commit runs from (may be a worktree).
	repo *TestGitRepo
}

// setupPlainRepo returns a repoUnderTest backed by a plain (non-worktree) repo.
func setupPlainRepo(t *testing.T) repoUnderTest {
	t.Helper()
	repo := newTestGitRepo(t)
	repo.CommitFile("file.txt", "content", "initial commit")
	return repoUnderTest{repo: repo}
}

// setupWorktreeRepo returns a repoUnderTest backed by a linked worktree.
func setupWorktreeRepo(t *testing.T) repoUnderTest {
	t.Helper()
	mainRepo := newTestGitRepo(t)
	mainRepo.CommitFile("file.txt", "content", "initial commit")

	wtDir := t.TempDir()
	resolved, err := filepath.EvalSymlinks(wtDir)
	require.NoError(t, err)
	mainRepo.Run("worktree", "add", resolved, "-b", "worktree-branch")

	return repoUnderTest{repo: &TestGitRepo{Dir: resolved, t: t}}
}

// mockEnqueueCapture registers a handler on mux that captures full enqueue
// requests. The returned channel receives at most one request.
func mockEnqueueCapture(t *testing.T, mux *http.ServeMux) <-chan daemon.EnqueueRequest {
	t.Helper()
	ch := make(chan daemon.EnqueueRequest, 1)
	mux.HandleFunc("/api/enqueue", func(w http.ResponseWriter, r *http.Request) {
		var req daemon.EnqueueRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		select {
		case ch <- req:
		default:
			assert.Fail(t, "mockEnqueueCapture: unexpected extra request")
			http.Error(w, "duplicate request", http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{"id": 1})
	})
	return ch
}

// TestPostCommitSendsLocalRepoPath checks that the RepoPath in the enqueue
// request is the local (worktree) path in both plain repos and linked
// worktrees. The daemon canonicalizes to the main repo root itself.
func TestPostCommitSendsLocalRepoPath(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T) repoUnderTest
	}{
		{"plain repo", setupPlainRepo},
		{"worktree", setupWorktreeRepo},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := tt.setup(t)
			mux := http.NewServeMux()
			daemonFromHandler(t, mux)
			reqCh := mockEnqueueCapture(t, mux)

			r.repo.CommitFile("change.txt", "content", "a commit")

			_, _, err := executePostCommitCmd("--repo", r.repo.Dir)
			require.NoError(t, err)

			req := <-reqCh
			assert.Equal(t, r.repo.Dir, req.RepoPath)
		})
	}
}

// TestPostCommitSkipsEnqueueDuringRebase exercises the real Go code path
// (postCommitCmd → git.IsRebaseInProgress) by simulating a rebase state
// with a synthetic rebase-merge directory. This is the unit-level
// complement to TestPostCommitDoesNotEnqueueDuringRebase which tests the
// end-to-end shell hook flow.
func TestPostCommitSkipsEnqueueDuringRebase(t *testing.T) {
	tests := []struct {
		name     string
		setup    func(t *testing.T) repoUnderTest
		sentinel string // directory to create inside git dir
	}{
		{"plain repo/rebase-merge", setupPlainRepo, "rebase-merge"},
		{"plain repo/rebase-apply", setupPlainRepo, "rebase-apply"},
		{"worktree/rebase-merge", setupWorktreeRepo, "rebase-merge"},
		{"worktree/rebase-apply", setupWorktreeRepo, "rebase-apply"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := tt.setup(t)
			mux := http.NewServeMux()
			daemonFromHandler(t, mux)

			mux.HandleFunc("/api/enqueue", func(
				w http.ResponseWriter, r *http.Request,
			) {
				t.Error("enqueue should not be called during rebase")
				http.Error(w, "unexpected", http.StatusConflict)
			})

			// Resolve the actual git dir (may differ from .git/ in
			// linked worktrees where .git is a file).
			gitDir := strings.TrimSpace(r.repo.Run(
				"rev-parse", "--git-dir"))
			if !filepath.IsAbs(gitDir) {
				gitDir = filepath.Join(r.repo.Dir, gitDir)
			}
			require.NoError(t, os.MkdirAll(
				filepath.Join(gitDir, tt.sentinel), 0o755))

			_, _, err := executePostCommitCmd("--repo", r.repo.Dir)
			require.NoError(t, err)
		})
	}
}

// mockRoborevBinary creates a mock "roborev" shell script in a temp directory
// and returns the directory (to prepend to PATH). The mock script handles
// "post-commit" by using roborev's same rebase detection logic: it checks
// for rebase-merge/rebase-apply in git-dir and writes to the marker file
// only when NOT rebasing.
func mockRoborevBinary(t *testing.T, marker string) string {
	t.Helper()
	binDir := t.TempDir()
	script := fmt.Sprintf(`#!/bin/sh
# Mock roborev binary for testing post-commit hook behavior.
# Only handles the "post-commit" subcommand.
case "$1" in
  post-commit)
    git_dir=$(git rev-parse --git-dir 2>/dev/null) || exit 0
    [ -d "$git_dir/rebase-merge" ] && exit 0
    [ -d "$git_dir/rebase-apply" ] && exit 0
    echo enqueued >> %q
    ;;
esac
`, marker)
	require.NoError(t, os.WriteFile(
		filepath.Join(binDir, "roborev"),
		[]byte(script), 0o755))
	return binDir
}

// installMockHook installs the real githook-generated post-commit hook with
// the ROBOREV= line patched to point at a mock binary.
func installMockHook(t *testing.T, repoDir, mockBinDir string) {
	t.Helper()
	hooksDir, err := gitrepo.HooksPath(context.Background(), repoDir)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(hooksDir, 0o755))

	hookContent := githook.GeneratePostCommit()
	mockBin := filepath.Join(mockBinDir, "roborev")
	lines := strings.Split(hookContent, "\n")
	for i, line := range lines {
		if strings.HasPrefix(line, "ROBOREV=") {
			lines[i] = fmt.Sprintf("ROBOREV=%q", mockBin)
			break
		}
	}
	require.NoError(t, os.WriteFile(
		filepath.Join(hooksDir, "post-commit"),
		[]byte(strings.Join(lines, "\n")), 0o755))
}

// TestPostCommitDoesNotEnqueueDuringRebase runs a real clean git rebase with
// hooks installed via githook.GeneratePostCommit and a mock roborev binary in
// PATH. It asserts that roborev's rebase detection prevents any enqueue during
// replayed commits.
//
// The mock binary reimplements the rebase guard in shell using the same
// git rev-parse --git-dir + rebase-merge/rebase-apply check that
// git.IsRebaseInProgress uses. TestPostCommitSkipsEnqueueDuringRebase (above)
// tests the real Go code path (postCommitCmd) via simulated rebase state; this
// test validates the end-to-end hook installation and invocation flow during
// an actual git rebase.
//
// The "hook before commits" variant installs the hook before the branch
// topology commits, so the hook fires for every setup commit as well. The
// "hook after commits" variant installs just before the rebase.
func TestPostCommitDoesNotEnqueueDuringRebase(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses shell hooks and Unix PATH semantics")
	}
	tests := []struct {
		name            string
		setup           func(t *testing.T) repoUnderTest
		hookBeforeSetup bool
	}{
		{"plain repo/hook before commits", setupPlainRepo, true},
		{"plain repo/hook after commits", setupPlainRepo, false},
		{"worktree/hook before commits", setupWorktreeRepo, true},
		{"worktree/hook after commits", setupWorktreeRepo, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := tt.setup(t)

			marker := filepath.Join(r.repo.Dir, "hook-enqueues.log")
			mockBinDir := mockRoborevBinary(t, marker)

			// Build env with mock roborev first in PATH so the hook finds it.
			gitEnv := append(os.Environ(),
				"PATH="+mockBinDir+string(os.PathListSeparator)+os.Getenv("PATH"),
				"HOME="+r.repo.Dir,
				"GIT_CONFIG_NOSYSTEM=1",
				"GIT_AUTHOR_NAME=Test",
				"GIT_AUTHOR_EMAIL=test@test.com",
				"GIT_COMMITTER_NAME=Test",
				"GIT_COMMITTER_EMAIL=test@test.com",
			)

			if tt.hookBeforeSetup {
				installMockHook(t, r.repo.Dir, mockBinDir)
			}

			// Create a branch topology for a clean rebase:
			//   base: A -- B (base.txt, no conflict)
			//          \
			//   current: C -- D -- E (branch files)
			gitCmd := func(args ...string) {
				t.Helper()
				cmd := exec.Command("git", args...)
				cmd.Dir = r.repo.Dir
				cmd.Env = gitEnv
				out, err := cmd.CombinedOutput()
				require.NoError(t, err, "git %v failed: %s", args, out)
			}
			gitCmd("checkout", "-b", "rebase-base")
			gitCmd("commit", "--allow-empty", "-m", "base commit")
			gitCmd("checkout", "-")
			// Create 3 feature commits with actual file changes.
			for i := 1; i <= 3; i++ {
				f := filepath.Join(r.repo.Dir, fmt.Sprintf("branch%d.txt", i))
				require.NoError(t, os.WriteFile(f, fmt.Appendf(nil, "content %d", i), 0o644))
				gitCmd("add", f)
				gitCmd("commit", "-m", fmt.Sprintf("feature commit %d", i))
			}

			if !tt.hookBeforeSetup {
				installMockHook(t, r.repo.Dir, mockBinDir)
			}

			// Positive control: make a normal commit to prove the hook
			// fires outside of a rebase. Without this, a broken hook
			// install would silently pass (0 == 0).
			gitCmd("commit", "--allow-empty", "-m", "positive control commit")
			data, err := os.ReadFile(marker)
			require.NoError(t, err, "hook should have fired on normal commit")
			preRebaseCount := strings.Count(string(data), "enqueued")
			require.GreaterOrEqual(t, preRebaseCount, 1,
				"hook must fire at least once before rebase to prove it works")

			// Run a full clean rebase — all 3 feature commits replay.
			cmd := exec.Command("git", "rebase", "rebase-base")
			cmd.Dir = r.repo.Dir
			cmd.Env = gitEnv
			out, err := cmd.CombinedOutput()
			require.NoError(t, err, "rebase should succeed cleanly: %s", out)

			// After the rebase, the marker count should be unchanged.
			// If the hook enqueued during the rebase, there would be more.
			data, err = os.ReadFile(marker)
			require.NoError(t, err)
			postRebaseCount := strings.Count(string(data), "enqueued")
			assert.Equal(t, preRebaseCount, postRebaseCount,
				"hook should not have enqueued during rebase (got %d, want %d)",
				postRebaseCount, preRebaseCount)
		})
	}
}
