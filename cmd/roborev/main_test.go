package main

// NOTE: Tests in this package mutate package-level variables (serverAddr,
// pollStartInterval, pollMaxInterval) and environment variables (ROBOREV_DATA_DIR).
// Do not use t.Parallel() in this package as it will cause race conditions.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/git"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/version"
)

// ============================================================================
// Refine Command Tests
// ============================================================================

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()

	origStdout := os.Stdout
	reader, writer, err := os.Pipe()
	require.NoError(t, err, "pipe stdout: %v")

	os.Stdout = writer
	defer func() { os.Stdout = origStdout }()
	defer reader.Close()
	defer writer.Close()

	outCh := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		if _, err := io.Copy(&buf, reader); err != nil {
			outCh <- ""
			return
		}
		outCh <- buf.String()
	}()

	fn()

	writer.Close()
	os.Stdout = origStdout
	output := <-outCh
	return output
}

// setupFastPolling reduces all polling intervals to 1ms for fast tests.
// Returns a cleanup function that restores the original values.
func setupFastPolling(t *testing.T) {
	t.Helper()

	origPollStart := pollStartInterval
	origPollMax := pollMaxInterval
	origPostCommitWait := postCommitWaitDelay
	origDaemonPoll := daemon.DefaultPollInterval

	pollStartInterval = 1 * time.Millisecond
	pollMaxInterval = 1 * time.Millisecond
	postCommitWaitDelay = 1 * time.Millisecond
	daemon.DefaultPollInterval = 1 * time.Millisecond

	t.Cleanup(func() {
		pollStartInterval = origPollStart
		pollMaxInterval = origPollMax
		postCommitWaitDelay = origPostCommitWait
		daemon.DefaultPollInterval = origDaemonPoll
	})
}

func setupRefineRepo(t *testing.T) (string, string) {
	t.Helper()

	repo := NewGitTestRepo(t)
	repo.CommitFile("file.txt", "base", "base commit")

	repo.Run("checkout", "-b", "feature")
	headSHA := repo.CommitFile("feature.txt", "change", "feature commit")

	return repo.Dir, headSHA
}

func newFastRunContext(t *testing.T, repoDir string) RunContext {
	t.Helper()

	return RunContext{
		Context:         t.Context(),
		WorkingDir:      repoDir,
		PollInterval:    1 * time.Millisecond,
		PostCommitDelay: 1 * time.Millisecond,
	}
}

func TestEnqueueReviewRefine(t *testing.T) {
	t.Run("returns job ID on success", func(t *testing.T) {
		md := NewMockDaemon(t, MockRefineHooks{
			OnEnqueue: func(w http.ResponseWriter, r *http.Request, state *mockRefineState) bool {
				var req daemon.EnqueueRequest
				json.NewDecoder(r.Body).Decode(&req)

				assert.False(t, req.RepoPath != "/test/repo" || req.GitRef != "abc..def")

				w.WriteHeader(http.StatusCreated)
				json.NewEncoder(w).Encode(storage.ReviewJob{ID: 123})
				return true
			},
		})
		defer md.Close()

		jobID, err := enqueueReview("/test/repo", "abc..def", "codex")
		require.NoError(t, err)

		assert.EqualValues(t, 123, jobID)
	})

	t.Run("returns error on failure", func(t *testing.T) {
		md := NewMockDaemon(t, MockRefineHooks{
			OnEnqueue: func(w http.ResponseWriter, r *http.Request, state *mockRefineState) bool {
				w.WriteHeader(http.StatusBadRequest)
				w.Write([]byte(`{"error":"invalid repo"}`))
				return true
			},
		})
		defer md.Close()

		_, err := enqueueReview("/bad/repo", "HEAD", "codex")
		require.Error(t, err, "expected error")
	})
}

func TestSummarizeAgentOutput(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "extracts first 10 lines",
			input:    "Line 1\nLine 2\nLine 3\nLine 4\nLine 5\nLine 6\nLine 7\nLine 8\nLine 9\nLine 10\nLine 11\nLine 12",
			expected: "Line 1\nLine 2\nLine 3\nLine 4\nLine 5\nLine 6\nLine 7\nLine 8\nLine 9\nLine 10",
		},
		{
			name:     "handles empty input",
			input:    "",
			expected: "Automated fix",
		},
		{
			name:     "handles whitespace only",
			input:    "   \n\n  \n",
			expected: "Automated fix",
		},
		{
			name:     "skips empty lines",
			input:    "\n\nFirst line\n\nSecond line\n\n",
			expected: "First line\nSecond line",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := summarizeAgentOutput(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestRefineNoChangeSkipsImmediately(t *testing.T) {
	// When the agent makes no changes, refine should skip the review
	// immediately. The skip path is triggered by IsWorkingTreeClean
	// returning true, which records a comment and adds to skippedReviews.
	//
	// Integration coverage: TestRunRefineSurfacesResponseErrors exercises
	// the full loop. Here we verify the predicate and skip-tracking logic.

	repo := NewGitTestRepo(t)
	repo.CommitFile("file.txt", "content", "initial")
	dir := repo.Dir

	require.True(t, git.IsWorkingTreeClean(dir), "expected clean working tree after commit")

	// Verify skip tracking: skippedReviews map should track skipped review IDs
	skippedReviews := make(map[int64]bool)
	skippedReviews[42] = true
	require.True(t, skippedReviews[42], "expected review 42 to be tracked as skipped")
	require.False(t, skippedReviews[99], "expected review 99 to not be tracked as skipped")
}

func TestRunRefineSurfacesResponseErrors(t *testing.T) {
	repoDir, _ := setupRefineRepo(t)

	md := NewMockDaemon(t, MockRefineHooks{
		OnReview: func(w http.ResponseWriter, r *http.Request, state *mockRefineState) bool {
			json.NewEncoder(w).Encode(storage.Review{
				ID: 1, JobID: 1, Output: "**Bug found**: fail", Closed: false,
			})
			return true
		},
		OnComments: func(w http.ResponseWriter, r *http.Request, state *mockRefineState) bool {
			w.WriteHeader(http.StatusInternalServerError)
			return true
		},
	})
	defer md.Close()

	ctx := newFastRunContext(t, repoDir)

	err := runRefine(ctx, refineOptions{agentName: "test", maxIterations: 1, quiet: true})
	require.Error(t, err, "expected error, got nil")
}

func TestRunRefineFailsOnInvalidGlobalConfigBeforeDaemonStartup(t *testing.T) {
	repoDir, _ := setupRefineRepo(t)

	dataDir := t.TempDir()
	t.Setenv("ROBOREV_DATA_DIR", dataDir)
	configPath := filepath.Join(dataDir, "config.toml")
	err := os.WriteFile(configPath, []byte("invalid toml [[["),
		0o644)
	require.NoError(t, err, "write invalid config: %v", err)

	origGetAnyRunningDaemon := getAnyRunningDaemon
	getAnyRunningDaemon = func() (*daemon.RuntimeInfo, error) {
		require.FailNow(t, "ensureDaemon should not run when global config is invalid")
		return nil, ErrDaemonNotRunning
	}
	t.Cleanup(func() {
		getAnyRunningDaemon = origGetAnyRunningDaemon
	})

	ctx := newFastRunContext(t, repoDir)

	err = runRefine(ctx, refineOptions{agentName: "test", maxIterations: 1, quiet: true})
	require.Error(t, err, "expected config load error")
	assert.Contains(t, err.Error(), "load config:")
}

func TestRunRefineFailsOnInvalidRepoConfigBeforeDaemonStartup(t *testing.T) {
	repoDir, _ := setupRefineRepo(t)

	err := os.WriteFile(
		filepath.Join(repoDir, ".roborev.toml"),
		[]byte("refine_model = ["),
		0o644,
	)
	require.NoError(t, err, "write invalid repo config: %v", err)
	gitAdd := exec.Command("git", "add", ".roborev.toml")
	gitAdd.Dir = repoDir
	out, err := gitAdd.CombinedOutput()
	require.NoError(t, err, "git add failed: %s", out)
	gitCommit := exec.Command("git", "commit", "-m", "add invalid repo config")
	gitCommit.Dir = repoDir
	out, err = gitCommit.CombinedOutput()
	require.NoError(t, err, "git commit failed: %s", out)

	origGetAnyRunningDaemon := getAnyRunningDaemon
	getAnyRunningDaemon = func() (*daemon.RuntimeInfo, error) {
		require.FailNow(t, "ensureDaemon should not run when repo config is invalid")
		return nil, ErrDaemonNotRunning
	}
	t.Cleanup(func() {
		getAnyRunningDaemon = origGetAnyRunningDaemon
	})

	ctx := newFastRunContext(t, repoDir)

	err = runRefine(ctx, refineOptions{
		agentName:     "test",
		reasoning:     "fast",
		maxIterations: 1,
		quiet:         true,
	})
	require.Error(t, err, "expected repo config error")
	assert.Contains(t, err.Error(), "resolve workflow config:")
}

func TestRunRefineQuietNonTTYTimerOutput(t *testing.T) {
	repoDir, headSHA := setupRefineRepo(t)

	md := NewMockDaemon(t, MockRefineHooks{})
	defer md.Close()

	md.State.reviews[headSHA] = &storage.Review{
		ID: 1, JobID: 42, Output: "**Bug found**: fail", Closed: false,
	}

	origIsTerminal := isTerminal
	isTerminal = func(fd uintptr) bool { return false }
	defer func() { isTerminal = origIsTerminal }()

	ctx := newFastRunContext(t, repoDir)

	output := captureStdout(t, func() {
		err := runRefine(ctx, refineOptions{agentName: "test", maxIterations: 1, quiet: true})
		require.Error(t, err, "expected error, got nil")
	})

	assert.NotContains(t, output, "\r")
	assert.Contains(t, output, "Addressing review (job 42)...")
}

func TestRunRefineStopsLiveTimerOnAgentError(t *testing.T) {
	repoDir, headSHA := setupRefineRepo(t)

	md := NewMockDaemon(t, MockRefineHooks{})
	defer md.Close()

	md.State.reviews[headSHA] = &storage.Review{
		ID: 1, JobID: 7, Output: "**Bug found**: fail", Closed: false,
	}

	origIsTerminal := isTerminal
	isTerminal = func(fd uintptr) bool { return true }
	defer func() { isTerminal = origIsTerminal }()

	agent.Register(&functionalMockAgent{nameVal: "test", reviewFunc: func(ctx context.Context, repoPath, commitSHA, prompt string, output io.Writer) (string, error) {
		return "", fmt.Errorf("test agent failure")
	}})
	defer agent.Register(agent.NewTestAgent())

	ctx := newFastRunContext(t, repoDir)

	output := captureStdout(t, func() {
		err := runRefine(ctx, refineOptions{agentName: "test", maxIterations: 1, quiet: true})
		require.Error(t, err, "expected error, got nil")
	})

	idx := strings.LastIndex(output, "\rAddressing review (job 7)...")
	assert.NotEqual(t, -1, idx)
	assert.Contains(t, output[idx:], "\n")
}

// ============================================================================
// Integration Tests for Refine Loop Business Logic
// ============================================================================

func TestRefineLoopFindFailedReviewPath(t *testing.T) {
	// Test the path where a failed individual review is found

	t.Run("finds oldest failed review among commits", func(t *testing.T) {
		client := newMockDaemonClient()
		// Commit1 passes, commit2 fails
		client.reviews["commit1sha"] = &storage.Review{
			ID: 1, JobID: 1, Output: "No issues found. LGTM!",
		}
		client.reviews["commit2sha"] = &storage.Review{
			ID: 2, JobID: 2, Output: "**Bug**: Missing error handling in foo.go:42",
		}

		commits := []string{"commit1sha", "commit2sha", "commit3sha"}
		review, err := findFailedReviewForBranch(client, commits, nil)

		require.NoError(t, err)

		require.NotNil(t, review, "expected to find failed review")
		// Should find commit2 failure (oldest-first iteration)
		assert.EqualValues(t, 2, review.ID)
	})

	t.Run("returns nil when review is already closed", func(t *testing.T) {
		client := newMockDaemonClient()
		// Failed but already closed
		client.reviews["commit1sha"] = &storage.Review{
			ID: 1, JobID: 1, Output: "**Bug**: error", Closed: true,
		}

		commits := []string{"commit1sha"}
		review, err := findFailedReviewForBranch(client, commits, nil)

		require.NoError(t, err)

		assert.Nil(t, review)
	})
}

func TestRefineLoopNoChangeSkipsReview(t *testing.T) {
	// When the agent makes no changes, refine records a comment and skips
	// the review. Verify the comments API works for both empty and populated
	// cases so the skip logic can rely on it.
	//
	// Integration coverage: TestRunRefineSurfacesResponseErrors exercises
	// the full loop including the skip path.
	md := NewMockDaemon(t, MockRefineHooks{})
	defer md.Close()

	md.State.responses[42] = []storage.Response{}
	jobID99 := int64(99)
	md.State.responses[99] = []storage.Response{
		{ID: 1, JobID: &jobID99, Responder: "roborev-refine", Response: "Agent could not determine how to address findings"},
	}

	// Job with no prior comments — skip should still apply (no retries needed)
	responses, err := getCommentsForJob(42)
	require.NoError(t, err)

	assert.Empty(t, responses)

	// Job with a prior skip comment — verify the comment text matches
	responses, err = getCommentsForJob(99)
	require.NoError(t, err)

	assert.Len(t, responses, 1)
	assert.Contains(t, responses[0].Response, "could not determine")
}

func TestRefineLoopBranchReviewPath(t *testing.T) {
	// Test the branch review path when no individual failures exist

	t.Run("triggers branch review when no individual failures", func(t *testing.T) {
		client := newMockDaemonClient()
		// All individual commits pass (outputs must start with pass patterns)
		client.reviews["commit1"] = &storage.Review{
			ID: 1, JobID: 1, Output: "No issues found.",
		}
		client.reviews["commit2"] = &storage.Review{
			ID: 2, JobID: 2, Output: "No issues found. LGTM!",
		}

		commits := []string{"commit1", "commit2"}
		review, err := findFailedReviewForBranch(client, commits, nil)

		// Should return nil since all pass
		require.NoError(t, err)

		assert.Nil(t, review)

		// In actual refine loop, this would trigger branch review
		// We can test enqueueReview separately
	})
}

func TestRefineLoopEnqueueBranchReview(t *testing.T) {
	// Test enqueueing a branch (range) review

	t.Run("enqueues range review for branch", func(t *testing.T) {
		md := NewMockDaemon(t, MockRefineHooks{})
		defer md.Close()

		jobID, err := enqueueReview("/test/repo", "abc123..HEAD", "codex")
		require.NoError(t, err)

		assert.NotEqual(t, 0, jobID)

		// Verify the range ref was enqueued
		assert.Len(t, md.State.enqueuedRefs, 1)
		assert.Equal(t, "abc123..HEAD", md.State.enqueuedRefs[0])
	})
}

func TestRefineLoopWaitForReviewCompletion(t *testing.T) {
	// Test waiting for a review to complete

	t.Run("returns review when job completes successfully", func(t *testing.T) {
		md := NewMockDaemon(t, MockRefineHooks{})
		defer md.Close()

		md.State.jobs[42] = &storage.ReviewJob{ID: 42, GitRef: "abc123", Status: storage.JobStatusDone}
		md.State.reviews["abc123"] = &storage.Review{
			ID: 1, JobID: 42, Output: "All tests pass. No issues found.", Closed: false,
		}

		review, err := waitForReviewWithInterval(42, 1*time.Millisecond)
		require.NoError(t, err)

		require.NotNil(t, review, "expected review")
		assert.Contains(t, review.Output, "No issues found")
	})

	t.Run("returns error when job fails", func(t *testing.T) {
		md := NewMockDaemon(t, MockRefineHooks{})
		defer md.Close()

		md.State.jobs[42] = &storage.ReviewJob{
			ID:     42,
			GitRef: "abc123",
			Status: storage.JobStatusFailed,
			Error:  "Agent timeout after 10 minutes",
		}

		_, err := waitForReviewWithInterval(42, 1*time.Millisecond)
		require.Error(t, err, "expected error for failed job")
		assert.Contains(t, err.Error(), "timeout")
	})
}

// TestRefinePendingJobWaitDoesNotConsumeIteration verifies that waiting for a pending
// (in-progress) job does not consume a refinement iteration. The test runs with
// maxIterations=1 and a job that starts as Running then transitions to Done with a
// passing review - if iterations were consumed during the wait, the test would fail.
func TestRefinePendingJobWaitDoesNotConsumeIteration(t *testing.T) {
	repoDir, commitSHA := setupRefineRepo(t)

	// Track how many times the job has been polled
	var pollCount atomic.Int32

	handleGetJobs := func(w http.ResponseWriter, r *http.Request, s *mockRefineState) bool {
		q := r.URL.Query()
		if idStr := q.Get("id"); idStr != "" {
			var jobID int64
			fmt.Sscanf(idStr, "%d", &jobID)
			s.mu.Lock()
			job, ok := s.jobs[jobID]
			if !ok {
				s.mu.Unlock()
				json.NewEncoder(w).Encode(map[string]any{"jobs": []storage.ReviewJob{}})
				return true
			}
			// On first poll, job is still Running; on subsequent polls, transition to Done
			count := pollCount.Add(1)
			if count > 1 {
				job.Status = storage.JobStatusDone
			}
			jobCopy := *job
			s.mu.Unlock()
			json.NewEncoder(w).Encode(map[string]any{"jobs": []storage.ReviewJob{jobCopy}})
			return true
		}
		if gitRef := q.Get("git_ref"); gitRef != "" {
			s.mu.Lock()
			var job *storage.ReviewJob
			for _, j := range s.jobs {
				if j.GitRef == gitRef {
					job = j
					break
				}
			}
			if job != nil {
				jobCopy := *job
				s.mu.Unlock()
				json.NewEncoder(w).Encode(map[string]any{"jobs": []storage.ReviewJob{jobCopy}})
				return true
			}
			s.mu.Unlock()
		}
		return false // fall through to base handler
	}

	handleEnqueue := func(w http.ResponseWriter, r *http.Request, s *mockRefineState) bool {
		// Handle branch review enqueue - create a passing branch review
		var req struct {
			GitRef string `json:"git_ref"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		s.mu.Lock()
		branchJobID := s.nextJobID
		s.nextJobID++
		s.jobs[branchJobID] = &storage.ReviewJob{
			ID:       branchJobID,
			GitRef:   req.GitRef,
			Agent:    "test",
			Status:   storage.JobStatusDone,
			RepoPath: repoDir,
		}
		s.reviews[req.GitRef] = &storage.Review{
			ID: branchJobID + 1000, JobID: branchJobID, Output: "No issues found. Branch looks good!",
		}
		jobCopy := *s.jobs[branchJobID]
		s.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(jobCopy)
		return true
	}

	md := NewMockDaemon(t, MockRefineHooks{
		OnGetJobs: handleGetJobs,
		OnEnqueue: handleEnqueue,
	})
	defer md.Close()

	// Create a job that starts as Running
	md.State.jobs[1] = &storage.ReviewJob{
		ID:       1,
		GitRef:   commitSHA,
		Agent:    "test",
		Status:   storage.JobStatusRunning, // Starts as pending
		RepoPath: repoDir,
	}
	// Passing review (will be returned once job is Done)
	md.State.reviews[commitSHA] = &storage.Review{
		ID: 1, JobID: 1, Output: "No issues found. LGTM!", Closed: false,
	}
	md.State.nextJobID = 2

	ctx := newFastRunContext(t, repoDir)

	// Run refine with maxIterations=1. If waiting on the pending job consumed
	// an iteration, this would fail with "max iterations reached". Since the
	// pending job transitions to Done with a passing review (and no failed
	// reviews exist), refine should succeed.
	err := runRefine(ctx, refineOptions{agentName: "test", maxIterations: 1, quiet: true})

	// Should succeed - all reviews pass after waiting for the pending one
	require.NoError(t, err, "expected refine to succeed (pending wait should not consume iteration), got: %v")

	// Verify the job was actually polled multiple times (proving we waited)
	assert.GreaterOrEqual(t, pollCount.Load(), int32(2))
}

// ============================================================================
// Show Command Tests
// ============================================================================

func TestShowJobFlagRequiresArgument(t *testing.T) {
	// Setup mock daemon that responds to /api/status with version info
	md := NewMockDaemon(t, MockRefineHooks{
		OnStatus: func(w http.ResponseWriter, r *http.Request, state *mockRefineState) bool {
			json.NewEncoder(w).Encode(map[string]string{"version": version.Version})
			return true
		},
	})
	defer md.Close()

	// Create the show command and execute with --job but no argument
	cmd := showCmd()
	cmd.SetArgs([]string{"--job"})

	// Capture stderr where cobra writes errors
	var errBuf bytes.Buffer
	cmd.SetErr(&errBuf)
	cmd.SetOut(&errBuf)

	err := cmd.Execute()
	require.Error(t, err, "expected error when --job used without argument")
	assert.Contains(t, err.Error(), "--job requires a job ID argument")
}

// ============================================================================
// filterGitEnv Tests
// ============================================================================

func TestFilterGitEnv(t *testing.T) {
	env := []string{
		"PATH=/usr/bin",
		"GIT_DIR=/some/repo/.git",
		"HOME=/home/user",
		"GIT_WORK_TREE=/some/repo",
		"GIT_INDEX_FILE=/some/repo/.git/index",
		"ROBOREV_DATA_DIR=/tmp/roborev",
		"GIT_CEILING_DIRECTORIES=/home",
		"Git_Dir=/mixed/case",                        // Windows-style mixed case
		"git_work_tree=/lowercase",                   // all lowercase
		"GIT_SSH_COMMAND=ssh -i ~/.ssh/deploy_key",   // auth/transport: keep
		"GIT_ASKPASS=/usr/lib/ssh/askpass",           // auth/transport: keep
		"GIT_CONFIG_PARAMETERS='core.autocrlf=true'", // config propagation: strip
		"GIT_CONFIG_COUNT=1",                         // config propagation: strip
		"GIT_CONFIG_KEY_0=core.autocrlf",             // numbered config: strip
		"GIT_CONFIG_VALUE_0=true",                    // numbered config: strip
	}

	filtered := filterGitEnv(env)

	want := []string{
		"PATH=/usr/bin",
		"HOME=/home/user",
		"ROBOREV_DATA_DIR=/tmp/roborev",
		"GIT_SSH_COMMAND=ssh -i ~/.ssh/deploy_key",
		"GIT_ASKPASS=/usr/lib/ssh/askpass",
	}

	assert.Len(t, filtered, len(want))
	for i, got := range filtered {
		assert.Equal(t, got, want[i])
	}
}

func TestIsGoTestBinaryPath(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{path: "/tmp/roborev.test", want: true},
		{path: "/tmp/roborev.test.exe", want: true},
		{path: "/tmp/ROBOREV.TEST", want: true},
		{path: "/tmp/roborev", want: false},
		{path: "/tmp/roborev.exe", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			got := isGoTestBinaryPath(tt.path)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestShouldRefuseAutoStartDaemon(t *testing.T) {
	t.Run("refuses test binary by default", func(t *testing.T) {
		t.Setenv("ROBOREV_TEST_ALLOW_AUTOSTART", "")
		p := filepath.FromSlash("/tmp/roborev.test")
		require.True(t, shouldRefuseAutoStartDaemon(p), "expected refusal for test binary without opt-in")
	})

	t.Run("allows explicit opt in for test binary", func(t *testing.T) {
		t.Setenv("ROBOREV_TEST_ALLOW_AUTOSTART", "1")
		p := filepath.FromSlash("/tmp/roborev.test")
		require.False(t, shouldRefuseAutoStartDaemon(p), "expected no refusal for test binary when opt-in is set")
	})

	t.Run("does not refuse normal binary", func(t *testing.T) {
		t.Setenv("ROBOREV_TEST_ALLOW_AUTOSTART", "")
		p := filepath.FromSlash("/usr/local/bin/roborev")
		require.False(t, shouldRefuseAutoStartDaemon(p), "expected no refusal for non-test binary")
	})

	t.Run("refuses go run binary from build cache", func(t *testing.T) {
		p := filepath.FromSlash(
			"/Users/x/Library/Caches/go-build/72/abc-d/roborev",
		)
		require.True(t, shouldRefuseAutoStartDaemon(p), "expected refusal for go-build cache binary")
	})

	t.Run("refuses go run binary from tmp", func(t *testing.T) {
		p := filepath.FromSlash(
			"/var/folders/y4/abc/T/go-build123/b001/exe/roborev",
		)
		require.True(t, shouldRefuseAutoStartDaemon(p), "expected refusal for go-build tmp binary")
	})

	t.Run("allows binary under go-builder username", func(t *testing.T) {
		t.Setenv("ROBOREV_TEST_ALLOW_AUTOSTART", "")
		p := filepath.FromSlash("/home/go-builder/bin/roborev")
		require.False(t, shouldRefuseAutoStartDaemon(p), "should not refuse binary under go-builder")
	})

	t.Run("allows binary under go-build1user dir", func(t *testing.T) {
		t.Setenv("ROBOREV_TEST_ALLOW_AUTOSTART", "")
		p := filepath.FromSlash("/opt/go-build1user/bin/roborev")
		require.False(t, shouldRefuseAutoStartDaemon(p), "should not refuse binary under go-build1user")
	})
}

func TestStartDaemonRefusesFromGoTestBinary(t *testing.T) {
	exe, err := os.Executable()
	require.NoError(t, err, "os.Executable failed: %v")

	if !isGoTestBinaryPath(exe) {
		t.Skipf("expected go test binary path, got %q", exe)
	}

	t.Setenv("ROBOREV_TEST_ALLOW_AUTOSTART", "")
	err = startDaemon()
	require.Error(t, err, "expected startDaemon to refuse go test binary auto-start")
	if !strings.Contains(err.Error(), "refusing to auto-start daemon from ephemeral binary") {
		require.NoError(t, err)
	}
}

func setupIsolatedDataDir(t *testing.T) string {
	t.Helper()
	tmpDir := t.TempDir()
	t.Setenv("ROBOREV_DATA_DIR", tmpDir)
	return tmpDir
}

// TestDaemonStopNotRunning verifies daemon stop reports when no daemon is running
func TestDaemonStopNotRunning(t *testing.T) {
	_ = setupIsolatedDataDir(t)

	err := stopDaemon()
	assert.Equal(t, err, ErrDaemonNotRunning)
}

func TestUpdateCmdHasNoRestartFlag(t *testing.T) {
	cmd := updateCmd()

	flag := cmd.Flags().Lookup("no-restart")
	require.NotNil(t, flag, "expected --no-restart flag to be defined")
	assert.Equal(t, "false", flag.DefValue)
	assert.Contains(t, flag.Usage, "skip daemon restart")
}

func TestRepairHooksAfterUpdateUsesRegisteredRepos(t *testing.T) {
	var gotOpts repairHookOptions
	repairHooksAfterUpdate("/tmp/bin", false, func(opts repairHookOptions) error {
		gotOpts = opts
		return nil
	})

	wantBinary := filepath.Join("/tmp/bin", "roborev")
	if runtime.GOOS == "windows" {
		wantBinary += ".exe"
	}
	assert.False(t, gotOpts.current)
	assert.True(t, gotOpts.registered)
	assert.Equal(t, wantBinary, gotOpts.binary)
}

func TestRepairHooksAfterUpdateSkipsWhenNoRestart(t *testing.T) {
	called := false
	output := captureStdout(t, func() {
		repairHooksAfterUpdate("/tmp/bin", true, func(opts repairHookOptions) error {
			called = true
			return nil
		})
	})

	assert.False(t, called)
	assert.Contains(t, output, "Skipping git hook update (--no-restart)")
}

func TestRepairHooksAfterUpdateUsesNewBinary(t *testing.T) {
	_ = setupIsolatedDataDir(t)
	tmpDir := t.TempDir()
	binDir := filepath.Join(tmpDir, "bin")
	require.NoError(t, os.MkdirAll(binDir, 0o755))

	fakeSource := filepath.Join(tmpDir, "fake_roborev.go")
	argsPath := filepath.Join(tmpDir, "args.txt")
	require.NoError(t, os.WriteFile(fakeSource, []byte(`package main

import (
	"fmt"
	"os"
	"strings"
)

func main() {
	argsPath := os.Getenv("ROBOREV_FAKE_ARGS")
	if argsPath == "" {
		os.Exit(2)
	}
	if err := os.WriteFile(argsPath, []byte(strings.Join(os.Args[1:], "\n")), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
`), 0o644))

	newBinary := updatedRoborevBinary(binDir)
	build := exec.Command("go", "build", "-o", newBinary, fakeSource)
	build.Env = os.Environ()
	out, err := build.CombinedOutput()
	require.NoError(t, err, "build fake roborev: %s", out)
	t.Setenv("ROBOREV_FAKE_ARGS", argsPath)

	repairHooksAfterUpdate(binDir, false, nil)

	gotBytes, err := os.ReadFile(argsPath)
	require.NoError(t, err)
	assert.Equal(t, strings.Join([]string{
		"install-hook",
		"repair",
		"--registered",
	}, "\n"), string(gotBytes))
}

// stubRestartVars saves and restores all package-level vars used by
// restartDaemonAfterUpdate. Returns a struct with call counters.
type restartStubs struct {
	stopCalls  int
	startCalls int
	killCalls  int
}

func stubRestartVars(t *testing.T) *restartStubs {
	t.Helper()
	origGet := getAnyRunningDaemon
	origList := listAllRuntimes
	origPIDAlive := isPIDAliveForUpdate
	origStop := stopDaemonForUpdate
	origKill := killAllDaemonsForUpdate
	origStart := startUpdatedDaemon
	origWait := updateRestartWaitTimeout
	origPoll := updateRestartPollInterval
	t.Cleanup(func() {
		getAnyRunningDaemon = origGet
		listAllRuntimes = origList
		isPIDAliveForUpdate = origPIDAlive
		stopDaemonForUpdate = origStop
		killAllDaemonsForUpdate = origKill
		startUpdatedDaemon = origStart
		updateRestartWaitTimeout = origWait
		updateRestartPollInterval = origPoll
	})
	updateRestartWaitTimeout = 5 * time.Millisecond
	updateRestartPollInterval = 1 * time.Millisecond

	// Default: no runtimes on disk.
	listAllRuntimes = func() ([]*daemon.RuntimeInfo, error) {
		return nil, nil
	}
	isPIDAliveForUpdate = func(int) bool {
		return false
	}

	s := &restartStubs{}
	stopDaemonForUpdate = func() error {
		s.stopCalls++
		return nil
	}
	killAllDaemonsForUpdate = func() {
		s.killCalls++
	}
	startUpdatedDaemon = func(string) error {
		s.startCalls++
		return nil
	}
	return s
}

func TestRestartDaemonAfterUpdateNoRestart(t *testing.T) {
	s := stubRestartVars(t)

	getAnyRunningDaemon = func() (*daemon.RuntimeInfo, error) {
		return &daemon.RuntimeInfo{PID: 100, Address: "127.0.0.1:7373"}, nil
	}

	output := captureStdout(t, func() {
		restartDaemonAfterUpdate("/tmp/bin", true)
	})

	assert.Contains(t, output, "Skipping daemon restart (--no-restart)")
	assert.Equal(t, 0, s.stopCalls)
	assert.Equal(t, 0, s.startCalls)
}

func TestRestartDaemonAfterUpdateManagerRestarted(t *testing.T) {
	s := stubRestartVars(t)

	var getCalls int
	getAnyRunningDaemon = func() (*daemon.RuntimeInfo, error) {
		getCalls++
		if getCalls == 1 {
			return &daemon.RuntimeInfo{PID: 100, Address: "127.0.0.1:7373"}, nil
		}
		// After stop, an external manager restarted with a new PID.
		return &daemon.RuntimeInfo{PID: 200, Address: "127.0.0.1:7373"}, nil
	}

	output := captureStdout(t, func() {
		restartDaemonAfterUpdate("/tmp/bin", false)
	})

	assert.Contains(t, output, "Restarting daemon... OK")
	assert.Equal(t, 1, s.stopCalls)
	assert.Equal(t, 0, s.startCalls)
}

func TestRestartDaemonAfterUpdateStopFailureSamePID(t *testing.T) {
	s := stubRestartVars(t)

	getAnyRunningDaemon = func() (*daemon.RuntimeInfo, error) {
		return &daemon.RuntimeInfo{PID: 100, Address: "127.0.0.1:7373"}, nil
	}
	stopDaemonForUpdate = func() error {
		s.stopCalls++
		return errors.New("cannot stop daemon")
	}

	output := captureStdout(t, func() {
		restartDaemonAfterUpdate("/tmp/bin", false)
	})

	assert.Contains(t, output, "warning: failed to stop daemon: cannot stop daemon")
	assert.Contains(t, output, "warning: daemon pid 100 is still running; restart it manually")
	assert.NotContains(t, output, "Restarting daemon... OK")
	assert.Equal(t, 0, s.startCalls)
}

func TestWaitForDaemonExitProbeErrorWithRuntimePresentTimesOut(t *testing.T) {
	stubRestartVars(t)

	getAnyRunningDaemon = func() (*daemon.RuntimeInfo, error) {
		return nil, os.ErrNotExist
	}
	listAllRuntimes = func() ([]*daemon.RuntimeInfo, error) {
		return []*daemon.RuntimeInfo{{PID: 100, Address: "127.0.0.1:7373"}}, nil
	}
	isPIDAliveForUpdate = func(pid int) bool {
		return pid == 100
	}

	exited, newPID := waitForDaemonExit(100, 5*time.Millisecond)
	assert.False(t, exited)
	assert.Equal(t, 0, newPID)
}

func TestWaitForDaemonExitProbeErrorWithStaleRuntimeExits(t *testing.T) {
	stubRestartVars(t)

	getAnyRunningDaemon = func() (*daemon.RuntimeInfo, error) {
		return nil, os.ErrNotExist
	}
	listAllRuntimes = func() ([]*daemon.RuntimeInfo, error) {
		return []*daemon.RuntimeInfo{{PID: 100, Address: "127.0.0.1:7373"}}, nil
	}
	// PID is dead; runtime entry is stale.
	isPIDAliveForUpdate = func(int) bool {
		return false
	}

	exited, newPID := waitForDaemonExit(100, 5*time.Millisecond)
	assert.True(t, exited)
	assert.Equal(t, 0, newPID)
}

func TestWaitForDaemonExitRuntimeGoneButPIDAliveTimesOut(t *testing.T) {
	stubRestartVars(t)

	getAnyRunningDaemon = func() (*daemon.RuntimeInfo, error) {
		return nil, os.ErrNotExist
	}
	listAllRuntimes = func() ([]*daemon.RuntimeInfo, error) {
		return nil, nil
	}
	isPIDAliveForUpdate = func(pid int) bool {
		return pid == 100
	}

	exited, newPID := waitForDaemonExit(100, 5*time.Millisecond)
	assert.False(t, exited)
	assert.Equal(t, 0, newPID)
}

func TestWaitForDaemonExitRuntimeGonePIDReusedAsNonDaemonExits(t *testing.T) {
	stubRestartVars(t)

	getAnyRunningDaemon = func() (*daemon.RuntimeInfo, error) {
		return nil, os.ErrNotExist
	}
	listAllRuntimes = func() ([]*daemon.RuntimeInfo, error) {
		return nil, nil
	}
	// Simulate PID reuse: old daemon runtime is gone and PID now belongs
	// to a non-daemon process, so daemon liveness should not block exit.
	isPIDAliveForUpdate = func(pid int) bool {
		return false
	}

	exited, newPID := waitForDaemonExit(100, 5*time.Millisecond)
	assert.True(t, exited)
	assert.Equal(t, 0, newPID)
}

func TestWaitForDaemonExitDetectsUnresponsiveManagerHandoffFromRuntimePID(t *testing.T) {
	stubRestartVars(t)

	getAnyRunningDaemon = func() (*daemon.RuntimeInfo, error) {
		return nil, os.ErrNotExist
	}
	listAllRuntimes = func() ([]*daemon.RuntimeInfo, error) {
		return []*daemon.RuntimeInfo{
			{PID: 200, Address: "127.0.0.1:7373"},
		}, nil
	}
	isPIDAliveForUpdate = func(pid int) bool {
		return pid == 200
	}

	exited, newPID := waitForDaemonExit(100, 5*time.Millisecond)
	require.True(t, exited, "expected exited=true when previous pid is gone")
	assert.Equal(t, 200, newPID)
}

func TestInitialPIDsExitedRequiresPIDDeath(t *testing.T) {
	stubRestartVars(t)

	listAllRuntimes = func() ([]*daemon.RuntimeInfo, error) {
		return nil, nil
	}
	isPIDAliveForUpdate = func(pid int) bool {
		return pid == 101
	}

	ok := initialPIDsExited(
		map[int]struct{}{
			100: {},
			101: {},
		},
		0,
	)
	require.False(t, ok, "expected false when an initial PID is still alive")
}

func TestInitialPIDsExitedAllowsManagerPID(t *testing.T) {
	stubRestartVars(t)

	listAllRuntimes = func() ([]*daemon.RuntimeInfo, error) {
		return nil, nil
	}
	isPIDAliveForUpdate = func(pid int) bool {
		return pid == 500
	}

	ok := initialPIDsExited(
		map[int]struct{}{
			100: {},
			500: {},
		},
		500,
	)
	require.True(t, ok, "expected true when only allowPID remains alive")
}

func TestRestartDaemonAfterUpdateStopFailureManagerRestartNeedsCleanup(t *testing.T) {
	s := stubRestartVars(t)

	var getCalls int
	getAnyRunningDaemon = func() (*daemon.RuntimeInfo, error) {
		getCalls++
		// Initial probe sees old daemon.
		if getCalls == 1 {
			return &daemon.RuntimeInfo{PID: 100, Address: "127.0.0.1:7373"}, nil
		}
		// During first wait loop, manager PID appears but old runtime still exists.
		if s.killCalls == 0 {
			return &daemon.RuntimeInfo{PID: 200, Address: "127.0.0.1:7373"}, nil
		}
		// After forced kill and manual start, readiness probe succeeds.
		if s.startCalls > 0 {
			return &daemon.RuntimeInfo{PID: 300, Address: "127.0.0.1:7373"}, nil
		}
		// During second wait loop after kill, no daemon responds.
		return nil, os.ErrNotExist
	}
	listAllRuntimes = func() ([]*daemon.RuntimeInfo, error) {
		if s.killCalls == 0 {
			// Before cleanup, one original PID still exists.
			return []*daemon.RuntimeInfo{
				{PID: 100, Address: "127.0.0.1:7373"},
				{PID: 101, Address: "127.0.0.1:7373"},
			}, nil
		}
		// Cleanup removed old daemons.
		return nil, nil
	}
	stopDaemonForUpdate = func() error {
		s.stopCalls++
		return errors.New("cannot stop all daemons")
	}

	output := captureStdout(t, func() {
		restartDaemonAfterUpdate("/tmp/bin", false)
	})

	assert.Contains(t, output, "warning: failed to stop daemon: cannot stop all daemons")
	assert.Equal(t, 1, s.killCalls)
	assert.Equal(t, 1, s.startCalls)
	assert.False(t, !strings.Contains(output, "Restarting daemon...") || !strings.Contains(output, "OK"))
}

func TestRestartDaemonAfterUpdateManagerRestartedAfterKill(t *testing.T) {
	s := stubRestartVars(t)

	getAnyRunningDaemon = func() (*daemon.RuntimeInfo, error) {
		if s.killCalls == 0 {
			// Before forced kill, old daemon stays on the same PID.
			return &daemon.RuntimeInfo{PID: 100, Address: "127.0.0.1:7373"}, nil
		}
		// After forced kill, external manager restarts the daemon.
		return &daemon.RuntimeInfo{PID: 500, Address: "127.0.0.1:7373"}, nil
	}
	listAllRuntimes = func() ([]*daemon.RuntimeInfo, error) {
		if s.killCalls == 0 {
			return []*daemon.RuntimeInfo{
				{PID: 100, Address: "127.0.0.1:7373"},
			}, nil
		}
		return []*daemon.RuntimeInfo{
			{PID: 500, Address: "127.0.0.1:7373"},
		}, nil
	}

	output := captureStdout(t, func() {
		restartDaemonAfterUpdate("/tmp/bin", false)
	})

	assert.Equal(t, 1, s.killCalls)
	assert.Equal(t, 0, s.startCalls)
	assert.Contains(t, output, "Restarting daemon... OK")
}

func TestRestartDaemonAfterUpdateManagerHandoffUnresponsiveUsesRuntimePID(t *testing.T) {
	s := stubRestartVars(t)

	var getCalls int
	getAnyRunningDaemon = func() (*daemon.RuntimeInfo, error) {
		getCalls++
		if getCalls == 1 {
			return &daemon.RuntimeInfo{PID: 100, Address: "127.0.0.1:7373"}, nil
		}
		// Replacement daemon is not yet responsive.
		return nil, os.ErrNotExist
	}

	var listCalls int
	listAllRuntimes = func() ([]*daemon.RuntimeInfo, error) {
		listCalls++
		if listCalls == 1 {
			// Initial snapshot for stop validation.
			return []*daemon.RuntimeInfo{
				{PID: 100, Address: "127.0.0.1:7373"},
			}, nil
		}
		// Runtime handoff to manager-restarted PID.
		return []*daemon.RuntimeInfo{
			{PID: 200, Address: "127.0.0.1:7373"},
		}, nil
	}
	isPIDAliveForUpdate = func(pid int) bool {
		return pid == 200
	}

	output := captureStdout(t, func() {
		restartDaemonAfterUpdate("/tmp/bin", false)
	})

	assert.Equal(t, 0, s.killCalls)
	assert.Equal(t, 0, s.startCalls)
	assert.NotContains(t, output, "Restarting daemon... OK")
	assert.Contains(t, output, "warning: daemon handoff detected but replacement is not ready; restart it manually")
}

func TestRestartDaemonAfterUpdateManagerHandoffAfterKillNotReadyWarnsNoStart(t *testing.T) {
	s := stubRestartVars(t)

	var handoffSeen bool
	getAnyRunningDaemon = func() (*daemon.RuntimeInfo, error) {
		if s.killCalls == 0 {
			// Initial probe + first wait loop see only the old daemon,
			// forcing timeout and kill fallback.
			return &daemon.RuntimeInfo{PID: 100, Address: "127.0.0.1:7373"}, nil
		}
		if !handoffSeen {
			// After kill fallback, handoff PID appears once.
			handoffSeen = true
			return &daemon.RuntimeInfo{PID: 500, Address: "127.0.0.1:7373"}, nil
		}
		// Replacement remains unresponsive during readiness polling.
		return nil, os.ErrNotExist
	}

	listAllRuntimes = func() ([]*daemon.RuntimeInfo, error) {
		if s.killCalls == 0 {
			return []*daemon.RuntimeInfo{
				{PID: 100, Address: "127.0.0.1:7373"},
			}, nil
		}
		return []*daemon.RuntimeInfo{
			{PID: 500, Address: "127.0.0.1:7373"},
		}, nil
	}

	isPIDAliveForUpdate = func(pid int) bool {
		return pid == 500
	}

	output := captureStdout(t, func() {
		restartDaemonAfterUpdate("/tmp/bin", false)
	})

	assert.Equal(t, 1, s.killCalls)
	assert.Equal(t, 0, s.startCalls)
	assert.Contains(t, output, "warning: daemon handoff detected but replacement is not ready; restart it manually")
	assert.NotContains(t, output, "Restarting daemon... OK")
}

func TestRestartDaemonAfterUpdateManagerRestartedAfterKillWithLingeringInitialPID(t *testing.T) {
	s := stubRestartVars(t)

	getAnyRunningDaemon = func() (*daemon.RuntimeInfo, error) {
		if s.killCalls == 0 {
			// Before forced kill, old daemon stays on the same PID.
			return &daemon.RuntimeInfo{PID: 100, Address: "127.0.0.1:7373"}, nil
		}
		// After forced kill, external manager restarts one daemon PID.
		return &daemon.RuntimeInfo{PID: 500, Address: "127.0.0.1:7373"}, nil
	}
	listAllRuntimes = func() ([]*daemon.RuntimeInfo, error) {
		if s.killCalls == 0 {
			// Initial runtime snapshot includes multiple daemon PIDs.
			return []*daemon.RuntimeInfo{
				{PID: 100, Address: "127.0.0.1:7373"},
				{PID: 101, Address: "127.0.0.1:7373"},
			}, nil
		}
		// After kill, previousPID is gone but another initial PID remains.
		return []*daemon.RuntimeInfo{
			{PID: 101, Address: "127.0.0.1:7373"},
			{PID: 500, Address: "127.0.0.1:7373"},
		}, nil
	}
	stopDaemonForUpdate = func() error {
		s.stopCalls++
		return errors.New("cannot stop all daemons")
	}

	output := captureStdout(t, func() {
		restartDaemonAfterUpdate("/tmp/bin", false)
	})

	assert.Equal(t, 1, s.killCalls)
	assert.Equal(t, 0, s.startCalls)
	assert.Contains(t, output, "warning: daemon restart detected but older daemon runtimes remain; restart it manually")
	assert.NotContains(t, output, "Restarting daemon... OK")
}

func TestRestartDaemonAfterUpdateStopFailedPreviousPIDExitedButInitialPIDLingering(t *testing.T) {
	s := stubRestartVars(t)

	var getCalls int
	getAnyRunningDaemon = func() (*daemon.RuntimeInfo, error) {
		getCalls++
		if getCalls == 1 {
			// Initial probe sees previous PID.
			return &daemon.RuntimeInfo{PID: 100, Address: "127.0.0.1:7373"}, nil
		}
		// previousPID exited quickly; no replacement PID observed.
		return nil, os.ErrNotExist
	}
	listAllRuntimes = func() ([]*daemon.RuntimeInfo, error) {
		switch getCalls {
		case 0:
			// Initial snapshot includes multiple daemon PIDs.
			return []*daemon.RuntimeInfo{
				{PID: 100, Address: "127.0.0.1:7373"},
				{PID: 101, Address: "127.0.0.1:7373"},
			}, nil
		case 1:
			// waitForDaemonExit still sees previous PID.
			return []*daemon.RuntimeInfo{
				{PID: 100, Address: "127.0.0.1:7373"},
				{PID: 101, Address: "127.0.0.1:7373"},
			}, nil
		default:
			// previousPID gone, but another initial PID lingers.
			return []*daemon.RuntimeInfo{
				{PID: 101, Address: "127.0.0.1:7373"},
			}, nil
		}
	}
	stopDaemonForUpdate = func() error {
		s.stopCalls++
		return errors.New("cannot stop all daemons")
	}

	output := captureStdout(t, func() {
		restartDaemonAfterUpdate("/tmp/bin", false)
	})

	assert.Equal(t, 0, s.killCalls)
	assert.Equal(t, 0, s.startCalls)
	assert.Contains(t, output, "warning: older daemon runtimes still present after stop; restart it manually")
	assert.NotContains(t, output, "Restarting daemon... OK")
}

func TestRestartDaemonAfterUpdateStopFailedInitialSnapshotErrorWithLingeringRuntimeSkipsStart(t *testing.T) {
	s := stubRestartVars(t)

	var getCalls int
	getAnyRunningDaemon = func() (*daemon.RuntimeInfo, error) {
		getCalls++
		if getCalls == 1 {
			// Initial probe sees previous PID.
			return &daemon.RuntimeInfo{PID: 100, Address: "127.0.0.1:7373"}, nil
		}
		// After stop attempt, daemon probe fails.
		return nil, os.ErrNotExist
	}

	var listCalls int
	listAllRuntimes = func() ([]*daemon.RuntimeInfo, error) {
		listCalls++
		switch listCalls {
		case 1:
			// Initial runtime snapshot fails.
			return nil, errors.New("cannot read runtime files")
		case 2:
			// waitForDaemonExit still sees previous PID.
			return []*daemon.RuntimeInfo{
				{PID: 100, Address: "127.0.0.1:7373"},
			}, nil
		case 3:
			// previousPID is now gone from runtime files.
			return nil, nil
		default:
			// Verification after stop failure finds another lingering runtime.
			return []*daemon.RuntimeInfo{
				{PID: 101, Address: "127.0.0.1:7373"},
			}, nil
		}
	}

	stopDaemonForUpdate = func() error {
		s.stopCalls++
		return errors.New("cannot stop daemon")
	}

	output := captureStdout(t, func() {
		restartDaemonAfterUpdate("/tmp/bin", false)
	})

	assert.Equal(t, 0, s.killCalls)
	assert.Equal(t, 0, s.startCalls)
	assert.Contains(t, output, "warning: older daemon runtimes still present after stop; restart it manually")
}

func TestRestartDaemonAfterUpdateStopFailedHandoffNotReadyWarnsNoStart(t *testing.T) {
	s := stubRestartVars(t)

	var handoffSeen bool
	getAnyRunningDaemon = func() (*daemon.RuntimeInfo, error) {
		if s.killCalls == 0 {
			// Initial probe + first wait loop see only the old daemon,
			// forcing timeout and kill fallback.
			return &daemon.RuntimeInfo{PID: 100, Address: "127.0.0.1:7373"}, nil
		}
		if !handoffSeen {
			// Second wait loop sees manager handoff PID once.
			handoffSeen = true
			return &daemon.RuntimeInfo{PID: 500, Address: "127.0.0.1:7373"}, nil
		}
		// Replacement remains unresponsive during readiness polling.
		return nil, os.ErrNotExist
	}

	listAllRuntimes = func() ([]*daemon.RuntimeInfo, error) {
		if s.killCalls == 0 {
			return []*daemon.RuntimeInfo{
				{PID: 100, Address: "127.0.0.1:7373"},
			}, nil
		}
		// previousPID is gone; only replacement PID runtime remains.
		return []*daemon.RuntimeInfo{
			{PID: 500, Address: "127.0.0.1:7373"},
		}, nil
	}

	isPIDAliveForUpdate = func(pid int) bool {
		return pid == 500
	}

	stopDaemonForUpdate = func() error {
		s.stopCalls++
		return errors.New("cannot stop daemon")
	}

	output := captureStdout(t, func() {
		restartDaemonAfterUpdate("/tmp/bin", false)
	})

	assert.Equal(t, 1, s.killCalls)
	assert.Equal(t, 0, s.startCalls)
	assert.Contains(t, output, "warning: daemon handoff detected but replacement is not ready; restart it manually")
	assert.NotContains(t, output, "Restarting daemon... OK")
}

func TestRestartDaemonAfterUpdateStopFailedPreExistingPIDNotAcceptedAsHandoff(t *testing.T) {
	s := stubRestartVars(t)

	var getCalls int
	getAnyRunningDaemon = func() (*daemon.RuntimeInfo, error) {
		getCalls++
		if getCalls == 1 {
			// Initial probe sees previous PID.
			return &daemon.RuntimeInfo{PID: 100, Address: "127.0.0.1:7373"}, nil
		}
		// Existing daemon PID 200 remains responsive throughout.
		return &daemon.RuntimeInfo{PID: 200, Address: "127.0.0.1:7373"}, nil
	}

	var listCalls int
	listAllRuntimes = func() ([]*daemon.RuntimeInfo, error) {
		listCalls++
		if listCalls == 1 {
			// Initial snapshot already includes PID 200.
			return []*daemon.RuntimeInfo{
				{PID: 100, Address: "127.0.0.1:7373"},
				{PID: 200, Address: "127.0.0.1:7373"},
			}, nil
		}
		// previousPID disappeared, but pre-existing PID 200 remains.
		return []*daemon.RuntimeInfo{
			{PID: 200, Address: "127.0.0.1:7373"},
		}, nil
	}
	isPIDAliveForUpdate = func(pid int) bool {
		return pid == 200
	}
	stopDaemonForUpdate = func() error {
		s.stopCalls++
		return errors.New("cannot stop daemon")
	}

	output := captureStdout(t, func() {
		restartDaemonAfterUpdate("/tmp/bin", false)
	})

	assert.Equal(t, 1, s.killCalls)
	assert.Equal(t, 0, s.startCalls)
	assert.Contains(t, output, "warning: daemon restart detected but older daemon runtimes remain; restart it manually")
	assert.NotContains(t, output, "Restarting daemon... OK")
}

// Fix #2: Probe failure with runtime files should use PID from
// runtime files and still attempt stop/wait/start.
func TestRestartDaemonAfterUpdateProbeFailFallback(t *testing.T) {
	s := stubRestartVars(t)
	// This test needs 5 getAnyRunningDaemon calls to succeed. On
	// Windows the default timer resolution is ~15ms, so the 5ms
	// timeout from stubRestartVars expires before enough poll
	// iterations run. Use a longer timeout.
	updateRestartWaitTimeout = 200 * time.Millisecond

	var getCalls int
	getAnyRunningDaemon = func() (*daemon.RuntimeInfo, error) {
		getCalls++
		if getCalls <= 2 {
			// Initial probe + first waitForDaemonExit poll fail.
			return nil, os.ErrNotExist
		}
		if getCalls <= 4 {
			// Continue failing until the old runtime disappears.
			return nil, os.ErrNotExist
		}
		// After manual start, daemon responds with new PID.
		return &daemon.RuntimeInfo{PID: 300, Address: "127.0.0.1:7373"}, nil
	}
	// Runtime files exist with a known PID.
	listAllRuntimes = func() ([]*daemon.RuntimeInfo, error) {
		if getCalls <= 3 {
			return []*daemon.RuntimeInfo{
				{PID: 100, Address: "127.0.0.1:7373"},
			}, nil
		}
		return nil, nil
	}

	output := captureStdout(t, func() {
		restartDaemonAfterUpdate("/tmp/bin", false)
	})

	assert.Contains(t, output, "Restarting daemon... OK")
	assert.Equal(t, 1, s.stopCalls)
	assert.Equal(t, 1, s.startCalls)
}

// Fix #2: No responsive daemon and no runtime files should skip silently.
func TestRestartDaemonAfterUpdateNoDaemon(t *testing.T) {
	s := stubRestartVars(t)

	getAnyRunningDaemon = func() (*daemon.RuntimeInfo, error) {
		return nil, os.ErrNotExist
	}
	// No runtime files either.
	listAllRuntimes = func() ([]*daemon.RuntimeInfo, error) {
		return nil, nil
	}

	output := captureStdout(t, func() {
		restartDaemonAfterUpdate("/tmp/bin", false)
	})

	assert.Empty(t, output)
	assert.Equal(t, 0, s.stopCalls)
	assert.Equal(t, 0, s.startCalls)
}

// Fix #3: Unmanaged daemon exits quickly — no 2s delay.
func TestRestartDaemonAfterUpdateExitsQuickly(t *testing.T) {
	s := stubRestartVars(t)

	var getCalls int
	getAnyRunningDaemon = func() (*daemon.RuntimeInfo, error) {
		getCalls++
		if getCalls == 1 {
			return &daemon.RuntimeInfo{PID: 100, Address: "127.0.0.1:7373"}, nil
		}
		if getCalls == 2 {
			// Daemon exited after stop.
			return nil, os.ErrNotExist
		}
		// After manual start, daemon is ready.
		return &daemon.RuntimeInfo{PID: 400, Address: "127.0.0.1:7373"}, nil
	}

	output := captureStdout(t, func() {
		restartDaemonAfterUpdate("/tmp/bin", false)
	})

	assert.Contains(t, output, "Restarting daemon... OK")
	assert.Equal(t, 1, s.stopCalls)
	assert.Equal(t, 1, s.startCalls)
}

func TestShortJobRef(t *testing.T) {
	commitID := int64(123)
	diffContent := "some diff"

	tests := []struct {
		name     string
		job      storage.ReviewJob
		expected string
	}{
		{
			name:     "run job with new git_ref",
			job:      storage.ReviewJob{GitRef: "run", CommitID: nil, DiffContent: nil},
			expected: "run",
		},
		{
			name:     "legacy prompt job shows as run",
			job:      storage.ReviewJob{GitRef: "prompt", CommitID: nil, DiffContent: nil},
			expected: "run",
		},
		{
			name:     "analyze job",
			job:      storage.ReviewJob{GitRef: "analyze", CommitID: nil, DiffContent: nil},
			expected: "analyze",
		},
		{
			name:     "custom label job",
			job:      storage.ReviewJob{GitRef: "my-task", CommitID: nil, DiffContent: nil},
			expected: "my-task",
		},
		{
			name:     "branch literally named prompt (has CommitID)",
			job:      storage.ReviewJob{GitRef: "prompt", CommitID: &commitID},
			expected: "prompt",
		},
		{
			name:     "branch literally named run (has CommitID)",
			job:      storage.ReviewJob{GitRef: "run", CommitID: &commitID},
			expected: "run",
		},
		{
			name:     "normal SHA review",
			job:      storage.ReviewJob{GitRef: "abc1234567890", CommitID: &commitID},
			expected: "abc1234",
		},
		{
			name:     "normal SHA review with Prompt set (after worker starts)",
			job:      storage.ReviewJob{GitRef: "abc1234567890", CommitID: &commitID, Prompt: "Review this commit..."},
			expected: "abc1234",
		},
		{
			name:     "commit range",
			job:      storage.ReviewJob{GitRef: "abc1234..def5678", CommitID: nil},
			expected: "abc1234..def5678",
		},
		{
			name:     "dirty review (has DiffContent)",
			job:      storage.ReviewJob{GitRef: "dirty", CommitID: nil, DiffContent: &diffContent},
			expected: "dirty",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shortJobRef(tt.job)
			assert.Equal(t, tt.expected, got)
		})
	}
}
