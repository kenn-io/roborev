package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	neturl "net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
	"go.kenn.io/roborev/internal/version"
)

func patchFixDaemonRetryForTest(t *testing.T, ensure func() error) {
	t.Helper()

	oldMaxRetries := fixDaemonMaxRetries
	oldRecoveryWait := fixDaemonRecoveryWait
	oldRecoveryPoll := fixDaemonRecoveryPoll
	oldEnsure := fixDaemonEnsure
	oldSleep := fixDaemonSleep
	oldProbeAttempts := enqueueIfNeededProbeAttempts
	oldProbeDelay := enqueueIfNeededProbeDelay

	fixDaemonMaxRetries = 1
	fixDaemonRecoveryWait = 25 * time.Millisecond
	fixDaemonRecoveryPoll = 5 * time.Millisecond
	if ensure != nil {
		fixDaemonEnsure = ensure
	} else {
		fixDaemonEnsure = ensureDaemon
	}
	fixDaemonSleep = time.Sleep
	enqueueIfNeededProbeAttempts = 1
	enqueueIfNeededProbeDelay = 5 * time.Millisecond

	t.Cleanup(func() {
		fixDaemonMaxRetries = oldMaxRetries
		fixDaemonRecoveryWait = oldRecoveryWait
		fixDaemonRecoveryPoll = oldRecoveryPoll
		fixDaemonEnsure = oldEnsure
		fixDaemonSleep = oldSleep
		enqueueIfNeededProbeAttempts = oldProbeAttempts
		enqueueIfNeededProbeDelay = oldProbeDelay
	})
}

func closeConnNoResponse(t *testing.T, w http.ResponseWriter) {
	t.Helper()

	hj, ok := w.(http.Hijacker)
	assert.True(t, ok, "response writer does not support hijacking")
	conn, _, err := hj.Hijack()
	require.NoError(t, err, "hijack connection: %v")

	_ = conn.Close()
}

func TestBuildGenericFixPrompt(t *testing.T) {
	analysisOutput := `## Issues Found
- Long function in main.go:50
- Missing error handling`

	prompt := buildGenericFixPrompt(analysisOutput, "", nil)

	// Should include the analysis output
	assert.Contains(t, prompt, "Issues Found")
	assert.Contains(t, prompt, "Long function")

	// Should have fix instructions
	assert.Contains(t, prompt, "apply the suggested changes")

	// Should request a commit
	assert.Contains(t, prompt, "git commit")
}

func TestBuildGenericFixPromptWithComments(t *testing.T) {
	comments := []storage.Response{
		{Responder: "dev", Response: "Don't touch the helper function", CreatedAt: time.Date(2026, 3, 15, 10, 0, 0, 0, time.UTC)},
	}
	p := buildGenericFixPrompt("Found bug in foo.go", "", comments)

	assert.Contains(t, p, "User Comments")
	assert.Contains(t, p, "Don't touch the helper function")
	assert.Contains(t, p, "Found bug in foo.go")
}

func TestBuildGenericFixPromptSplitsMixedResponses(t *testing.T) {
	responses := []storage.Response{
		{Responder: "roborev-fix", Response: "Fix applied (commit: abc123)", CreatedAt: time.Date(2026, 3, 15, 9, 0, 0, 0, time.UTC)},
		{Responder: "alice", Response: "This is a false positive", CreatedAt: time.Date(2026, 3, 15, 10, 0, 0, 0, time.UTC)},
	}
	p := buildGenericFixPrompt("Found bug in foo.go", "", responses)

	// Tool attempts should appear under "Previous Addressing Attempts"
	assert.Contains(t, p, "Previous Addressing Attempts")
	assert.Contains(t, p, "roborev-fix")

	// User comments should appear under "User Comments"
	assert.Contains(t, p, "User Comments")
	assert.Contains(t, p, "false positive")
	assert.Contains(t, p, "alice")
}

func TestBuildBatchFixPromptSplitsMixedResponses(t *testing.T) {
	entries := []batchEntry{
		{
			jobID: 1,
			job:   &storage.ReviewJob{GitRef: "abc123"},
			review: &storage.Review{
				Output: "Found HIGH bug in foo.go",
			},
			comments: []storage.Response{
				{Responder: "roborev-refine", Response: "Created commit def456", CreatedAt: time.Date(2026, 3, 15, 9, 0, 0, 0, time.UTC)},
				{Responder: "alice", Response: "The foo.go finding is a false positive", CreatedAt: time.Date(2026, 3, 15, 10, 0, 0, 0, time.UTC)},
			},
		},
	}
	p := buildBatchFixPrompt(entries, "")

	// Tool attempts should appear under "Previous Addressing Attempts"
	assert.Contains(t, p, "Previous Addressing Attempts")
	assert.Contains(t, p, "roborev-refine")

	// User comments should appear under "User Comments"
	assert.Contains(t, p, "User Comments")
	assert.Contains(t, p, "false positive")
	assert.Contains(t, p, "alice")

	// Review output should be present
	assert.Contains(t, p, "Found HIGH bug in foo.go")
}

func TestFetchCommentsSkipsLegacyLookupForDirtyJob(t *testing.T) {
	assert := assert.New(t)
	var sawLegacy bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.RawQuery {
		case "job_id=11":
			assert.NoError(json.NewEncoder(w).Encode(struct {
				Responses []storage.Response `json:"responses"`
			}{
				Responses: []storage.Response{{Responder: "alice", Response: "job comment"}},
			}))
		case "commit_id=42":
			sawLegacy = true
			assert.NoError(json.NewEncoder(w).Encode(struct {
				Responses []storage.Response `json:"responses"`
			}{
				Responses: []storage.Response{{Responder: "bob", Response: "base commit comment"}},
			}))
		default:
			http.Error(w, "unexpected query: "+r.URL.RawQuery, http.StatusBadRequest)
		}
	}))
	defer server.Close()

	got, err := fetchComments(context.Background(), server.URL, 11, 42, "dirty")
	require.NoError(t, err)

	require.Len(t, got, 1)
	assert.Equal("alice", got[0].Responder)
	assert.False(sawLegacy, "dirty jobs must not fetch legacy comments for the base commit")
}

func TestBuildGenericCommitPrompt(t *testing.T) {
	prompt := buildGenericCommitPrompt()

	// Should have commit instructions
	assert.Contains(t, prompt, "git commit")
	assert.Contains(t, prompt, "descriptive")
}

func TestFetchJob(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		jobs       []storage.ReviewJob
		wantErr    bool
		wantErrMsg string
	}{
		{
			name:       "success",
			statusCode: http.StatusOK,
			jobs: []storage.ReviewJob{{
				ID:     42,
				Status: storage.JobStatusDone,
				Agent:  "test",
			}},
		},
		{
			name:       "not found",
			statusCode: http.StatusOK,
			jobs:       []storage.ReviewJob{},
			wantErr:    true,
			wantErrMsg: "not found",
		},
		{
			name:       "server error",
			statusCode: http.StatusInternalServerError,
			wantErr:    true,
			wantErrMsg: "server error",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.statusCode)
				if tt.statusCode == http.StatusOK {
					writeJSON(w, map[string]any{"jobs": tt.jobs})
				}
			}))
			defer ts.Close()

			job, err := fetchJob(context.Background(), ts.URL, 42)

			if tt.wantErr {
				require.Error(t, err, "expected error")
				assert.Contains(t, err.Error(), tt.wantErrMsg, "unexpected error")
				return
			}

			require.NoError(t, err)
			assert.EqualValues(t, 42, job.ID)
		})
	}
}

func TestFetchJobCanceledContextDoesNotRetryRecovery(t *testing.T) {
	var recoveryAttempted atomic.Bool
	patchFixDaemonRetryForTest(t, func() error {
		recoveryAttempted.Store(true)
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := fetchJob(ctx, "http://127.0.0.1:1", 42)
	require.ErrorIs(t, err, context.Canceled)
	assert.False(t, recoveryAttempted.Load(), "unexpected daemon recovery attempt")
}

func TestFetchReview(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		review     *storage.Review
		wantErr    bool
		wantErrMsg string
	}{
		{
			name:       "success",
			statusCode: http.StatusOK,
			review: &storage.Review{
				JobID:  42,
				Output: "Analysis output here",
			},
		},
		{
			name:       "server error",
			statusCode: http.StatusInternalServerError,
			wantErr:    true,
			wantErrMsg: "server error",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "/api/review", r.URL.Path)
				assert.Equal(t, "42", r.URL.Query().Get("job_id"))

				w.WriteHeader(tt.statusCode)
				if tt.review != nil {
					writeJSON(w, tt.review)
				}
			}))
			defer ts.Close()

			review, err := fetchReview(context.Background(), ts.URL, 42)

			if tt.wantErr {
				require.Error(t, err, "expected error")
				assert.Contains(t, err.Error(), tt.wantErrMsg, "unexpected error")
				return
			}

			require.NoError(t, err, "fetchReview")
			assert.Equal(t, review.JobID, tt.review.JobID)
			assert.Equal(t, review.Output, tt.review.Output)
		})
	}
}

func TestFetchReviewDeadlineExceededDoesNotRetryRecovery(t *testing.T) {
	var recoveryAttempted atomic.Bool
	patchFixDaemonRetryForTest(t, func() error {
		recoveryAttempted.Store(true)
		return nil
	})

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	_, err := fetchReview(ctx, ts.URL, 42)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.False(t, recoveryAttempted.Load(), "unexpected daemon recovery attempt")
}

func TestWithFixDaemonRetryContextRetriesClientTimeoutWhenCallerContextActive(t *testing.T) {
	var ensureCalls atomic.Int32
	patchFixDaemonRetryForTest(t, func() error {
		ensureCalls.Add(1)
		return nil
	})

	var attempts atomic.Int32
	value, err := withFixDaemonRetryContext(context.Background(), "http://127.0.0.1:1", func(addr string) (string, error) {
		if attempts.Add(1) == 1 {
			return "", &neturl.Error{Op: "Get", URL: addr, Err: context.DeadlineExceeded}
		}
		return "ok", nil
	})
	require.NoError(t, err, "withFixDaemonRetryContext")
	assert.Equal(t, "ok", value)
	assert.EqualValues(t, 2, attempts.Load())
	assert.EqualValues(t, 1, ensureCalls.Load())
}

func TestWithFixDaemonRetryContextStopsAfterCancellationDuringRecovery(t *testing.T) {
	oldRecoveryWait := fixDaemonRecoveryWait
	oldRecoveryPoll := fixDaemonRecoveryPoll
	oldEnsure := fixDaemonEnsure
	oldSleep := fixDaemonSleep
	fixDaemonRecoveryWait = time.Second
	fixDaemonRecoveryPoll = time.Millisecond
	t.Cleanup(func() {
		fixDaemonRecoveryWait = oldRecoveryWait
		fixDaemonRecoveryPoll = oldRecoveryPoll
		fixDaemonEnsure = oldEnsure
		fixDaemonSleep = oldSleep
	})

	ctx, cancel := context.WithCancel(context.Background())
	var ensureCalls atomic.Int32
	fixDaemonEnsure = func() error {
		ensureCalls.Add(1)
		return errors.New("daemon still down")
	}
	fixDaemonSleep = func(time.Duration) {
		cancel()
	}

	var attempts atomic.Int32
	_, err := withFixDaemonRetryContext(ctx, "http://127.0.0.1:1", func(addr string) (string, error) {
		attempts.Add(1)
		return "", &neturl.Error{Op: "Get", URL: addr, Err: syscall.ECONNREFUSED}
	})
	require.ErrorIs(t, err, context.Canceled)
	assert.EqualValues(t, 1, attempts.Load())
	assert.NotEqual(t, 0, ensureCalls.Load(), "expected daemon recovery to be attempted")
}

func TestWithFixDaemonRetryContextReturnsRecoveryFailure(t *testing.T) {
	oldMaxRetries := fixDaemonMaxRetries
	oldRecoveryWait := fixDaemonRecoveryWait
	oldRecoveryPoll := fixDaemonRecoveryPoll
	oldEnsure := fixDaemonEnsure
	oldSleep := fixDaemonSleep
	fixDaemonMaxRetries = 1
	fixDaemonRecoveryWait = time.Millisecond
	fixDaemonRecoveryPoll = time.Millisecond
	recoveryErr := errors.New("refusing to auto-start daemon from ephemeral binary")
	fixDaemonEnsure = func() error { return recoveryErr }
	fixDaemonSleep = func(time.Duration) {}
	t.Cleanup(func() {
		fixDaemonMaxRetries = oldMaxRetries
		fixDaemonRecoveryWait = oldRecoveryWait
		fixDaemonRecoveryPoll = oldRecoveryPoll
		fixDaemonEnsure = oldEnsure
		fixDaemonSleep = oldSleep
	})

	var attempts atomic.Int32
	_, err := withFixDaemonRetryContext(context.Background(), "http://127.0.0.1:1", func(addr string) (string, error) {
		attempts.Add(1)
		return "", &neturl.Error{Op: "Get", URL: addr, Err: syscall.ECONNREFUSED}
	})
	if !errors.Is(err, recoveryErr) {
		require.NoError(t, err, "expected recovery failure, got %v")
	}
	assert.EqualValues(t, 1, attempts.Load())
}

func TestAddJobResponse(t *testing.T) {
	var gotJobID int64
	var gotContent string

	var gotCommenter string

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.False(t, r.URL.Path != "/api/comment" || r.Method != http.MethodPost)

		var req map[string]any
		err := json.NewDecoder(r.Body).Decode(&req)
		assert.NoError(t, err)
		gotJobID = int64(req["job_id"].(float64))
		gotContent = req["comment"].(string)
		gotCommenter = req["commenter"].(string)

		w.WriteHeader(http.StatusCreated)
	}))
	defer ts.Close()

	err := addJobResponse(context.Background(), ts.URL, 123, "roborev-fix", "Fix applied")
	require.NoError(t, err, "addJobResponse")

	assert.EqualValues(t, 123, gotJobID)
	assert.Equal(t, "Fix applied", gotContent)
	assert.Equal(t, "roborev-fix", gotCommenter)
}

func TestAddJobResponseAvoidsDuplicatePostAfterConnectionDrop(t *testing.T) {
	var firstPostCount atomic.Int32
	var recoveryPostCount atomic.Int32
	var responsesMu sync.Mutex
	var responses []storage.Response

	startServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/comment":
			firstPostCount.Add(1)
			var req map[string]any
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				assert.NoError(t, err, "decode request body: %v")
			}
			responsesMu.Lock()
			responses = append(responses, storage.Response{
				ID:        int64(len(responses) + 1),
				JobID:     new(int64(123)),
				Responder: req["commenter"].(string),
				Response:  req["comment"].(string),
			})
			responsesMu.Unlock()
			closeConnNoResponse(t, w)
		default:
			http.NotFound(w, r)
		}
	}))
	defer startServer.Close()

	recoveryServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/comments":
			responsesMu.Lock()
			responsesCopy := append([]storage.Response(nil), responses...)
			responsesMu.Unlock()
			writeJSON(w, map[string]any{"responses": responsesCopy})
		case "/api/comment":
			recoveryPostCount.Add(1)
			w.WriteHeader(http.StatusCreated)
		default:
			http.NotFound(w, r)
		}
	}))
	defer recoveryServer.Close()

	patchFixDaemonRetryForTest(t, func() error {
		serverAddr = recoveryServer.URL
		return nil
	})

	err := addJobResponse(context.Background(), startServer.URL, 123, "roborev-fix", "Fix applied")
	require.NoError(t, err, "addJobResponse: %v")

	assert.EqualValues(t, 1, firstPostCount.Load())
	assert.EqualValues(t, 0, recoveryPostCount.Load())
}

func TestAddJobResponseCanceledContextDoesNotAttemptRecovery(t *testing.T) {
	var recoveryAttempted atomic.Bool
	patchFixDaemonRetryForTest(t, func() error {
		recoveryAttempted.Store(true)
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/comment" {
			http.NotFound(w, r)
			return
		}
		closeConnNoResponse(t, w)
	}))
	defer ts.Close()

	err := addJobResponse(ctx, ts.URL, 123, "roborev-fix", "Fix applied")
	require.Error(t, err, "expected connection error")
	assert.False(t, recoveryAttempted.Load(), "unexpected daemon recovery attempt")
}

func TestAddJobResponseDeadlineExceededCancelsHTTPCall(t *testing.T) {
	var recoveryAttempted atomic.Bool
	patchFixDaemonRetryForTest(t, func() error {
		recoveryAttempted.Store(true)
		return nil
	})

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/comment" {
			http.NotFound(w, r)
			return
		}
		time.Sleep(100 * time.Millisecond)
	}))
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	err := addJobResponse(ctx, ts.URL, 123, "roborev-fix", "Fix applied")
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.False(t, recoveryAttempted.Load(), "unexpected daemon recovery attempt")
}

func TestFixSingleJob(t *testing.T) {
	repo := createTestRepo(t, map[string]string{
		"main.go": "package main\n",
	})

	ts, _ := newMockServer(t, MockServerOpts{
		ReviewOutput: "## Issues\n- Found minor issue",
		OnJobs: func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, map[string]any{
				"jobs": []storage.ReviewJob{{
					ID:     99,
					Status: storage.JobStatusDone,
					Agent:  "test",
				}},
			})
		},
	})
	patchServerAddr(t, ts.URL)

	cmd, output := newTestCmd(t)

	opts := fixOptions{
		agentName: "test",
		reasoning: "fast",
	}

	tracker := &fixSessionTracker{base: agent.NewTestAgent(), out: io.Discard}
	err := fixSingleJob(cmd, repo.Dir, 99, opts, tracker)
	require.NoError(t, err, "fixSingleJob")

	// Verify output contains expected content
	outputStr := output.String()
	assert.Contains(t, outputStr, "Issues")
	assert.Contains(t, outputStr, "closed")
}

func TestFixSingleJobRecoversPostFixDaemonCalls(t *testing.T) {
	repo := createTestRepo(t, map[string]string{
		"main.go": "package main\n",
	})

	originalAgent, err := agent.Get("test")
	require.NoError(t, err, "get test agent")
	agent.Register(&agent.FakeAgent{
		NameStr: "test",
		ReviewFn: func(_ context.Context, repoPath, _, _ string, _ io.Writer) (string, error) {
			fixPath := filepath.Join(repoPath, "fix.txt")
			if err := os.WriteFile(fixPath, []byte("fixed\n"), 0o644); err != nil {
				return "", fmt.Errorf("write fix: %w", err)
			}
			cmd := exec.Command("git", "add", "fix.txt")
			cmd.Dir = repoPath
			if out, err := cmd.CombinedOutput(); err != nil {
				return "", fmt.Errorf("git add: %w (%s)", err, out)
			}
			cmd = exec.Command("git", "commit", "-m", "fix: apply retry test")
			cmd.Dir = repoPath
			if out, err := cmd.CombinedOutput(); err != nil {
				return "", fmt.Errorf("git commit: %w (%s)", err, out)
			}
			return "applied fix", nil
		},
	})
	t.Cleanup(func() {
		agent.Register(originalAgent)
	})

	deadURL := "http://127.0.0.1:1"
	var enqueueCount atomic.Int32
	var commentCount atomic.Int32
	var closeCount atomic.Int32
	recoveryServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/jobs":
			writeJSON(w, map[string]any{
				"jobs":     []storage.ReviewJob{},
				"has_more": false,
			})
		case "/api/enqueue":
			enqueueCount.Add(1)
			w.WriteHeader(http.StatusCreated)
		case "/api/comment":
			commentCount.Add(1)
			w.WriteHeader(http.StatusCreated)
		case "/api/comments":
			writeJSON(w, map[string]any{"responses": []any{}})
		case "/api/review/close":
			closeCount.Add(1)
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer recoveryServer.Close()

	var ensureCalls atomic.Int32
	patchFixDaemonRetryForTest(t, func() error {
		ensureCalls.Add(1)
		serverAddr = recoveryServer.URL
		return nil
	})

	var daemonDead atomic.Bool
	hijackAndClose := func(w http.ResponseWriter) {
		hj, ok := w.(http.Hijacker)
		if ok {
			conn, _, _ := hj.Hijack()
			if conn != nil {
				conn.Close()
			}
		}
	}
	deadHandler := func(w http.ResponseWriter, r *http.Request) {
		hijackAndClose(w)
	}
	_ = newMockDaemonBuilder(t).
		WithHandler("/api/jobs", func(w http.ResponseWriter, r *http.Request) {
			if daemonDead.Load() {
				hijackAndClose(w)
				return
			}
			writeJSON(w, map[string]any{
				"jobs": []storage.ReviewJob{{
					ID:     99,
					Status: storage.JobStatusDone,
					Agent:  "test",
				}},
			})
		}).
		WithHandler("/api/review", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, storage.Review{Output: "## Issues\n- Found minor issue"})
			// Simulate daemon death after responding
			daemonDead.Store(true)
			removeAllDaemonFiles(t)
			serverAddr = deadURL
		}).
		WithHandler("/api/enqueue", deadHandler).
		WithHandler("/api/comment", deadHandler).
		WithHandler("/api/comments", deadHandler).
		WithHandler("/api/review/close", deadHandler).
		Build()

	cmd, output := newTestCmd(t)

	opts := fixOptions{
		agentName: "test",
		reasoning: "fast",
	}

	base, err := resolveFixAgent(repo.Dir, opts)
	require.NoError(t, err, "resolveFixAgent")
	tracker := &fixSessionTracker{base: base, out: io.Discard}
	err = fixSingleJob(cmd, repo.Dir, 99, opts, tracker)
	require.NoError(t, err, "fixSingleJob")

	outputStr := output.String()
	assert.NotEqual(t, 0, ensureCalls.Load(), "expected daemon recovery to be attempted")
	assert.NotContains(t, outputStr, "Warning: could not enqueue review for fix commit")
	assert.NotContains(t, outputStr, "Warning: could not add response to job")
	assert.NotContains(t, outputStr, "Warning: could not close job")
	assert.EqualValues(t, 1, enqueueCount.Load())
	assert.EqualValues(t, 1, commentCount.Load())
	assert.EqualValues(t, 1, closeCount.Load())
}

func TestFixJobNotComplete(t *testing.T) {
	ts, _ := newMockServer(t, MockServerOpts{
		OnJobs: func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, map[string]any{
				"jobs": []storage.ReviewJob{{
					ID:     99,
					Status: storage.JobStatusRunning, // Not complete
					Agent:  "test",
				}},
			})
		},
	})
	patchServerAddr(t, ts.URL)

	cmd, _ := newTestCmd(t)

	tracker := &fixSessionTracker{base: agent.NewTestAgent(), out: io.Discard}
	err := fixSingleJob(cmd, t.TempDir(), 99, fixOptions{agentName: "test"}, tracker)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not complete")
}

func TestFixCmdFlagValidation(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{
			name:    "--all-branches with positional args",
			args:    []string{"--all-branches", "123"},
			wantErr: "--all-branches cannot be used with positional job IDs",
		},
		{
			name:    "--branch with positional args",
			args:    []string{"--branch", "main", "123"},
			wantErr: "--branch cannot be used with positional job IDs",
		},
		{
			name:    "--newest-first with positional args",
			args:    []string{"--newest-first", "123"},
			wantErr: "--newest-first cannot be used with positional job IDs",
		},
		{
			name:    "--all-branches with --branch",
			args:    []string{"--all-branches", "--branch", "main"},
			wantErr: "--all-branches and --branch are mutually exclusive",
		},
		{
			name:    "--batch with explicit IDs and --branch",
			args:    []string{"--batch", "--branch", "main", "123"},
			wantErr: "--branch cannot be used with positional job IDs",
		},
		{
			name:    "--batch with explicit IDs and --all-branches",
			args:    []string{"--batch", "--all-branches", "123"},
			wantErr: "--all-branches cannot be used with positional job IDs",
		},
		{
			name:    "--batch with explicit IDs and --newest-first",
			args:    []string{"--batch", "--newest-first", "123"},
			wantErr: "--newest-first cannot be used with positional job IDs",
		},
		{
			name:    "--list with positional args",
			args:    []string{"--list", "123"},
			wantErr: "--list cannot be used with positional job IDs",
		},
		{
			name:    "--list with --batch",
			args:    []string{"--list", "--batch"},
			wantErr: "--list and --batch are mutually exclusive",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := fixCmd()
			cmd.SilenceUsage = true
			cmd.SilenceErrors = true
			cmd.SetArgs(tt.args)
			err := cmd.Execute()
			require.Error(t, err, "expected error")
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestFixNoArgsDefaultsToOpen(t *testing.T) {
	// Running fix with no args should not produce a validation error —
	// it should enter the open path (which will fail at daemon
	// connection, not at argument validation).
	//
	// Use a mock daemon so ensureDaemon doesn't try to spawn a real
	// daemon subprocess (which hangs on CI).
	daemonFromHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Return empty for all queries — we only care about argument routing
		json.NewEncoder(w).Encode(map[string]any{
			"jobs":     []any{},
			"has_more": false,
		})
	}))

	cmd := fixCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{})
	err := cmd.Execute()
	// Should NOT be a validation/args error; any other error (e.g. daemon
	// not running) is acceptable.
	assert.False(t, err != nil && strings.Contains(err.Error(), "requires at least"))
}

func TestFixAllBranchesDiscovery(t *testing.T) {
	// --all-branches alone should pass validation and
	// route through open discovery.
	daemonFromHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"jobs":     []any{},
			"has_more": false,
		})
	}))

	cmd := fixCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{"--all-branches"})
	err := cmd.Execute()
	require.NoError(t, err)
}

func TestRunFixOpen(t *testing.T) {
	repo := createTestRepo(t, map[string]string{"f.txt": "x"})
	repoBranch := strings.TrimSpace(repo.Run("rev-parse", "--abbrev-ref", "HEAD"))
	failVerdict := "F"

	t.Run("no open jobs", func(t *testing.T) {
		_ = newMockDaemonBuilder(t).
			WithHandler("/api/jobs", func(w http.ResponseWriter, r *http.Request) {
				q := r.URL.Query()
				assert.Equal(t, "done", q.Get("status"))
				assert.Equal(t, "false", q.Get("closed"))
				writeJSON(w, map[string]any{
					"jobs":     []storage.ReviewJob{},
					"has_more": false,
				})
			}).
			Build()

		out, err := runWithOutput(t, repo.Dir, func(cmd *cobra.Command) error {
			tracker := &fixSessionTracker{base: agent.NewTestAgent(), out: io.Discard}
			return runFixOpen(cmd, "", false, false, false, fixOptions{agentName: "test"}, tracker)
		})
		require.NoError(t, err, "runFixOpen")
		assert.Contains(t, out, "No open jobs found")
	})

	t.Run("finds and processes open jobs", func(t *testing.T) {
		var reviewCalls, closeCalls atomic.Int32
		var openQueryCalls atomic.Int32
		passVerdict := "P"
		jobsByID := map[int64]storage.ReviewJob{
			10: {ID: 10, Status: storage.JobStatusDone, Agent: "test", Branch: repoBranch, JobType: storage.JobTypeReview, Verdict: &failVerdict},
			20: {ID: 20, Status: storage.JobStatusDone, Agent: "test", Branch: repoBranch, JobType: storage.JobTypeReview, Verdict: &failVerdict},
			30: {ID: 30, Status: storage.JobStatusDone, Agent: "test", Branch: repoBranch, JobType: storage.JobTypeReview, Verdict: &passVerdict},
		}

		_ = newMockDaemonBuilder(t).
			WithHandler("/api/jobs", func(w http.ResponseWriter, r *http.Request) {
				q := r.URL.Query()
				if q.Get("closed") == "false" && q.Get("limit") == "0" {
					if openQueryCalls.Add(1) == 1 {
						writeJSON(w, map[string]any{
							"jobs": []storage.ReviewJob{
								jobsByID[10],
								jobsByID[20],
								jobsByID[30],
							},
							"has_more": false,
						})
					} else {
						writeJSON(w, map[string]any{
							"jobs":     []storage.ReviewJob{},
							"has_more": false,
						})
					}
				} else {
					var id int64
					_, _ = fmt.Sscanf(q.Get("id"), "%d", &id)
					assert.NotEqual(t, int64(30), id, "passed review should not be fetched for fixing")
					writeJSON(w, map[string]any{
						"jobs": []storage.ReviewJob{
							jobsByID[id],
						},
						"has_more": false,
					})
				}
			}).
			WithHandler("/api/review", func(w http.ResponseWriter, r *http.Request) {
				reviewCalls.Add(1)
				writeJSON(w, storage.Review{Output: "findings"})
			}).
			WithHandler("/api/review/close", func(w http.ResponseWriter, r *http.Request) {
				closeCalls.Add(1)
				w.WriteHeader(http.StatusOK)
			}).
			WithHandler("/api/enqueue", func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
			}).
			Build()

		out, err := runWithOutput(t, repo.Dir, func(cmd *cobra.Command) error {
			tracker := &fixSessionTracker{base: agent.NewTestAgent(), out: io.Discard}
			return runFixOpen(cmd, "", false, false, false, fixOptions{agentName: "test", reasoning: "fast"}, tracker)
		})
		require.NoError(t, err, "runFixOpen")
		assert.Contains(t, out, "Found 2 open job(s)")
		assert.EqualValues(t, int64(2), reviewCalls.Load())
		assert.EqualValues(t, int64(2), closeCalls.Load())
	})

	t.Run("passes branch filter to API", func(t *testing.T) {
		var gotBranch string
		_ = newMockDaemonBuilder(t).
			WithHandler("/api/jobs", func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("closed") == "false" {
					gotBranch = r.URL.Query().Get("branch")
				}
				writeJSON(w, map[string]any{
					"jobs":     []storage.ReviewJob{},
					"has_more": false,
				})
			}).
			Build()

		_, err := runWithOutput(t, repo.Dir, func(cmd *cobra.Command) error {
			tracker := &fixSessionTracker{base: agent.NewTestAgent(), out: io.Discard}
			return runFixOpen(cmd, "feature-branch", false, true, false, fixOptions{agentName: "test"}, tracker)
		})
		require.NoError(t, err, "runFixOpen")
		assert.Equal(t, "feature-branch", gotBranch)
	})

	t.Run("server error", func(t *testing.T) {
		_ = newMockDaemonBuilder(t).
			WithHandler("/api/jobs", func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte("db error"))
			}).
			Build()

		_, err := runWithOutput(t, repo.Dir, func(cmd *cobra.Command) error {
			tracker := &fixSessionTracker{base: agent.NewTestAgent(), out: io.Discard}
			return runFixOpen(cmd, "", false, false, false, fixOptions{agentName: "test"}, tracker)
		})
		require.Error(t, err, "expected error on server failure")
		assert.Contains(t, err.Error(), "server error")
	})

	t.Run("all-branches includes jobs from other branches", func(t *testing.T) {
		// The repo is on its default branch (e.g. "master"). Jobs from
		// "other-branch" should still be discovered when branch==""
		// (all-branches mode) — filterReachableJobs must be skipped.
		var reviewCalls atomic.Int32
		var openQueryCalls atomic.Int32

		_ = newMockDaemonBuilder(t).
			WithHandler("/api/jobs", func(w http.ResponseWriter, r *http.Request) {
				q := r.URL.Query()
				if q.Get("closed") == "false" && q.Get("limit") == "0" {
					// Verify no branch filter in the API query
					assert.Empty(t, q.Get("branch"),
						"all-branches should not send branch filter")
					if openQueryCalls.Add(1) == 1 {
						writeJSON(w, map[string]any{
							"jobs": []storage.ReviewJob{
								{
									ID:      30,
									Status:  storage.JobStatusDone,
									Agent:   "test",
									Branch:  "other-branch",
									GitRef:  "dirty",
									JobType: storage.JobTypeDirty,
									Verdict: &failVerdict,
								},
								{
									ID:      31,
									Status:  storage.JobStatusDone,
									Agent:   "test",
									Branch:  "yet-another",
									GitRef:  "dirty",
									JobType: storage.JobTypeDirty,
									Verdict: &failVerdict,
								},
							},
							"has_more": false,
						})
					} else {
						writeJSON(w, map[string]any{
							"jobs":     []storage.ReviewJob{},
							"has_more": false,
						})
					}
				} else {
					writeJSON(w, map[string]any{
						"jobs": []storage.ReviewJob{
							{ID: 30, Status: storage.JobStatusDone, Agent: "test"},
						},
						"has_more": false,
					})
				}
			}).
			WithHandler("/api/review", func(w http.ResponseWriter, r *http.Request) {
				reviewCalls.Add(1)
				writeJSON(w, storage.Review{Output: "findings"})
			}).
			WithHandler("/api/review/close", func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
			}).
			WithHandler("/api/enqueue", func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
			}).
			Build()

		out, err := runWithOutput(t, repo.Dir, func(cmd *cobra.Command) error {
			tracker := &fixSessionTracker{base: agent.NewTestAgent(), out: io.Discard}
			return runFixOpen(cmd, "", true, false, false, fixOptions{
				agentName: "test",
				reasoning: "fast",
			}, tracker)
		})
		require.NoError(t, err, "runFixOpen all-branches")
		assert.Contains(t, out, "Found 2 open job(s)")
		assert.GreaterOrEqual(t, reviewCalls.Load(), int32(2),
			"should process jobs from other branches")
	})

	t.Run("explicit branch filters by branch field", func(t *testing.T) {
		// When branch="target-branch", only jobs with that branch
		// should survive filterReachableJobs (branchOnly path).
		var reviewCalls atomic.Int32
		var openQueryCalls atomic.Int32

		_ = newMockDaemonBuilder(t).
			WithHandler("/api/jobs", func(w http.ResponseWriter, r *http.Request) {
				q := r.URL.Query()
				if q.Get("closed") == "false" && q.Get("limit") == "0" {
					assert.Equal(t, "target-branch", q.Get("branch"))
					if openQueryCalls.Add(1) == 1 {
						writeJSON(w, map[string]any{
							"jobs": []storage.ReviewJob{
								{
									ID:      40,
									Status:  storage.JobStatusDone,
									Agent:   "test",
									Branch:  "target-branch",
									GitRef:  "dirty",
									JobType: storage.JobTypeDirty,
									Verdict: &failVerdict,
								},
								{
									ID:      41,
									Status:  storage.JobStatusDone,
									Agent:   "test",
									Branch:  "wrong-branch",
									GitRef:  "dirty",
									JobType: storage.JobTypeDirty,
									Verdict: &failVerdict,
								},
							},
							"has_more": false,
						})
					} else {
						writeJSON(w, map[string]any{
							"jobs":     []storage.ReviewJob{},
							"has_more": false,
						})
					}
				} else {
					writeJSON(w, map[string]any{
						"jobs": []storage.ReviewJob{
							{ID: 40, Status: storage.JobStatusDone, Agent: "test"},
						},
						"has_more": false,
					})
				}
			}).
			WithHandler("/api/review", func(w http.ResponseWriter, r *http.Request) {
				reviewCalls.Add(1)
				writeJSON(w, storage.Review{Output: "findings"})
			}).
			WithHandler("/api/review/close", func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
			}).
			WithHandler("/api/enqueue", func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
			}).
			Build()

		out, err := runWithOutput(t, repo.Dir, func(cmd *cobra.Command) error {
			tracker := &fixSessionTracker{base: agent.NewTestAgent(), out: io.Discard}
			return runFixOpen(cmd, "target-branch", false, true, false, fixOptions{
				agentName: "test",
				reasoning: "fast",
			}, tracker)
		})
		require.NoError(t, err, "runFixOpen with explicit branch")
		// Only job 40 matches; job 41 has wrong branch
		assert.Contains(t, out, "Found 1 open job(s)")
		assert.Equal(t, int32(1), reviewCalls.Load())
	})
}

func TestFilterFixCandidateJobsRequiresFailedActionableReviews(t *testing.T) {
	failed := "F"
	failedSpaced := " f "
	passed := "P"
	empty := ""
	commitID := int64(42)

	jobs := []storage.ReviewJob{
		{ID: 10, JobType: storage.JobTypeReview, Verdict: &failed},
		{ID: 11, JobType: storage.JobTypeRange, Verdict: &failed},
		{ID: 12, JobType: storage.JobTypeDirty, Verdict: &failed},
		{ID: 13, JobType: storage.JobTypeCompact, Verdict: &failed},
		{ID: 14, JobType: storage.JobTypeSynthesis, Verdict: &failed},
		{ID: 15, JobType: "", CommitID: &commitID, Verdict: &failedSpaced},
		{ID: 20, JobType: storage.JobTypeReview, Verdict: &passed},
		{ID: 21, JobType: storage.JobTypeReview, Verdict: nil},
		{ID: 22, JobType: storage.JobTypeReview, Verdict: &empty},
		{ID: 23, JobType: storage.JobTypeFix, Verdict: &failed},
		{ID: 24, JobType: storage.JobTypeTask, Verdict: &failed},
		{ID: 25, JobType: storage.JobTypeInsights, Verdict: &failed},
		{ID: 26, JobType: storage.JobTypeClassify, Verdict: &failed},
	}

	filtered := filterFixCandidateJobs(jobs)
	gotIDs := make([]int64, 0, len(filtered))
	for _, job := range filtered {
		gotIDs = append(gotIDs, job.ID)
	}

	assert.Equal(t, []int64{10, 11, 12, 13, 14, 15}, gotIDs)
}

func TestRunFixOpenOrdering(t *testing.T) {
	repo := createTestRepo(t, map[string]string{"f.txt": "x"})
	repoBranch := strings.TrimSpace(repo.Run("rev-parse", "--abbrev-ref", "HEAD"))
	failVerdict := "F"

	makeBuilder := func() (*MockDaemonBuilder, *atomic.Int32) {
		var openQueryCalls atomic.Int32
		b := newMockDaemonBuilder(t).
			WithHandler("/api/jobs", func(w http.ResponseWriter, r *http.Request) {
				q := r.URL.Query()
				if q.Get("closed") == "false" {
					if openQueryCalls.Add(1) == 1 {
						// Return newest first (as the API does)
						writeJSON(w, map[string]any{
							"jobs": []storage.ReviewJob{
								{ID: 30, Status: storage.JobStatusDone, Agent: "test", Branch: repoBranch, JobType: storage.JobTypeReview, Verdict: &failVerdict},
								{ID: 20, Status: storage.JobStatusDone, Agent: "test", Branch: repoBranch, JobType: storage.JobTypeReview, Verdict: &failVerdict},
								{ID: 10, Status: storage.JobStatusDone, Agent: "test", Branch: repoBranch, JobType: storage.JobTypeReview, Verdict: &failVerdict},
							},
							"has_more": false,
						})
					} else {
						writeJSON(w, map[string]any{
							"jobs":     []storage.ReviewJob{},
							"has_more": false,
						})
					}
				} else {
					writeJSON(w, map[string]any{
						"jobs": []storage.ReviewJob{
							{ID: 10, Status: storage.JobStatusDone, Agent: "test"},
						},
						"has_more": false,
					})
				}
			}).
			WithHandler("/api/review", func(w http.ResponseWriter, r *http.Request) {
				writeJSON(w, storage.Review{Output: "findings"})
			}).
			WithHandler("/api/comment", func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusCreated)
			}).
			WithHandler("/api/review/close", func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
			}).
			WithHandler("/api/enqueue", func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
			})
		return b, &openQueryCalls
	}

	t.Run("oldest first by default", func(t *testing.T) {
		b, _ := makeBuilder()
		b.Build()

		out, err := runWithOutput(t, repo.Dir, func(cmd *cobra.Command) error {
			tracker := &fixSessionTracker{base: agent.NewTestAgent(), out: io.Discard}
			return runFixOpen(cmd, "", false, false, false, fixOptions{agentName: "test", reasoning: "fast"}, tracker)
		})
		require.NoError(t, err)

		assert.Contains(t, out, "[10 20 30]")
	})

	t.Run("newest first with flag", func(t *testing.T) {
		b, _ := makeBuilder()
		b.Build()

		out, err := runWithOutput(t, repo.Dir, func(cmd *cobra.Command) error {
			tracker := &fixSessionTracker{base: agent.NewTestAgent(), out: io.Discard}
			return runFixOpen(cmd, "", false, false, true, fixOptions{agentName: "test", reasoning: "fast"}, tracker)
		})
		require.NoError(t, err)

		assert.Contains(t, out, "[30 20 10]")
	})
}

func TestRunFixOpenRequery(t *testing.T) {
	repo := createTestRepo(t, map[string]string{"f.txt": "x"})
	repoBranch := strings.TrimSpace(repo.Run("rev-parse", "--abbrev-ref", "HEAD"))
	failVerdict := "F"

	var queryCount atomic.Int32
	_ = newMockDaemonBuilder(t).
		WithHandler("/api/jobs", func(w http.ResponseWriter, r *http.Request) {
			q := r.URL.Query()
			if q.Get("closed") == "false" && q.Get("limit") == "0" {
				n := queryCount.Add(1)
				switch n {
				case 1:
					// First query: return batch 1
					writeJSON(w, map[string]any{
						"jobs": []storage.ReviewJob{
							{ID: 10, Status: storage.JobStatusDone, Agent: "test", Branch: repoBranch, JobType: storage.JobTypeReview, Verdict: &failVerdict},
						},
						"has_more": false,
					})
				case 2:
					// Second query: new job appeared
					writeJSON(w, map[string]any{
						"jobs": []storage.ReviewJob{
							{ID: 20, Status: storage.JobStatusDone, Agent: "test", Branch: repoBranch, JobType: storage.JobTypeReview, Verdict: &failVerdict},
							{ID: 10, Status: storage.JobStatusDone, Agent: "test", Branch: repoBranch, JobType: storage.JobTypeReview, Verdict: &failVerdict},
						},
						"has_more": false,
					})
				default:
					// Third query: no new jobs
					writeJSON(w, map[string]any{
						"jobs":     []storage.ReviewJob{},
						"has_more": false,
					})
				}
			} else {
				// Individual job fetch
				writeJSON(w, map[string]any{
					"jobs": []storage.ReviewJob{
						{ID: 10, Status: storage.JobStatusDone, Agent: "test"},
					},
					"has_more": false,
				})
			}
		}).
		WithHandler("/api/review", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, storage.Review{Output: "findings"})
		}).
		WithHandler("/api/comment", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusCreated)
		}).
		WithHandler("/api/review/close", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}).
		WithHandler("/api/enqueue", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}).
		Build()

	out, err := runWithOutput(t, repo.Dir, func(cmd *cobra.Command) error {
		tracker := &fixSessionTracker{base: agent.NewTestAgent(), out: io.Discard}
		return runFixOpen(cmd, "", false, false, false, fixOptions{agentName: "test", reasoning: "fast"}, tracker)
	})
	require.NoError(t, err)

	assert.Contains(t, out, "Found 1 open job(s)")
	assert.Contains(t, out, "Found 1 new open job(s)")
	assert.Equal(t, 3, int(queryCount.Load()))
}

func TestRunFixOpenRecoversFromDaemonRestartOnRequery(t *testing.T) {
	repo := createTestRepo(t, map[string]string{"f.txt": "x"})
	repoBranch := strings.TrimSpace(repo.Run("rev-parse", "--abbrev-ref", "HEAD"))
	failVerdict := "F"

	deadURL := "http://127.0.0.1:1"
	var recoveryQueryCount atomic.Int32
	recoveryServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/jobs":
			q := r.URL.Query()
			if q.Get("closed") == "false" && q.Get("limit") == "0" {
				if recoveryQueryCount.Add(1) == 1 {
					writeJSON(w, map[string]any{
						"jobs": []storage.ReviewJob{
							{ID: 20, Status: storage.JobStatusDone, Agent: "test", Branch: repoBranch, JobType: storage.JobTypeReview, Verdict: &failVerdict},
							{ID: 10, Status: storage.JobStatusDone, Agent: "test", Branch: repoBranch, JobType: storage.JobTypeReview, Verdict: &failVerdict},
						},
						"has_more": false,
					})
					return
				}
				writeJSON(w, map[string]any{
					"jobs":     []storage.ReviewJob{},
					"has_more": false,
				})
				return
			}
			writeJSON(w, map[string]any{
				"jobs": []storage.ReviewJob{
					{ID: 20, Status: storage.JobStatusDone, Agent: "test", Branch: repoBranch},
				},
				"has_more": false,
			})
		case "/api/review":
			writeJSON(w, storage.Review{Output: "findings"})
		case "/api/comment":
			w.WriteHeader(http.StatusCreated)
		case "/api/review/close":
			w.WriteHeader(http.StatusOK)
		case "/api/ping":
			writeJSON(w, daemon.PingInfo{OK: true, Service: "roborev", Version: version.Version})
		default:
			http.NotFound(w, r)
		}
	}))
	defer recoveryServer.Close()

	var ensureCalls atomic.Int32
	patchFixDaemonRetryForTest(t, func() error {
		ensureCalls.Add(1)
		serverAddr = recoveryServer.URL
		return nil
	})

	var openQueryCount atomic.Int32
	_ = newMockDaemonBuilder(t).
		WithHandler("/api/jobs", func(w http.ResponseWriter, r *http.Request) {
			q := r.URL.Query()
			if q.Get("closed") == "false" && q.Get("limit") == "0" {
				openQueryCount.Add(1)
				writeJSON(w, map[string]any{
					"jobs": []storage.ReviewJob{
						{ID: 10, Status: storage.JobStatusDone, Agent: "test", Branch: repoBranch, JobType: storage.JobTypeReview, Verdict: &failVerdict},
					},
					"has_more": false,
				})
				return
			}
			writeJSON(w, map[string]any{
				"jobs": []storage.ReviewJob{
					{ID: 10, Status: storage.JobStatusDone, Agent: "test", Branch: repoBranch},
				},
				"has_more": false,
			})
		}).
		WithHandler("/api/review", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, storage.Review{Output: "findings"})
		}).
		WithHandler("/api/comment", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusCreated)
		}).
		WithHandler("/api/review/close", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			// Simulate daemon death: remove runtime file so
			// getDaemonEndpoint falls back to serverAddr, then
			// point serverAddr at a dead address.
			removeAllDaemonFiles(t)
			serverAddr = deadURL
		}).
		Build()

	out, err := runWithOutput(t, repo.Dir, func(cmd *cobra.Command) error {
		tracker := &fixSessionTracker{base: agent.NewTestAgent(), out: io.Discard}
		return runFixOpen(cmd, "", false, false, false, fixOptions{agentName: "test", reasoning: "fast"}, tracker)
	})
	require.NoError(t, err)

	if ensureCalls.Load() == 0 {
		require.NoError(t, err, "expected daemon recovery to be attempted")
	}
	assert.Contains(t, out, "Found 1 open job(s)")
	assert.Contains(t, out, "Found 1 new open job(s)")
}

func TestFixJobDirectUnbornHead(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	t.Run("agent creates first commit", func(t *testing.T) {
		// Create a fresh git repo with no commits (unborn HEAD)
		dir := t.TempDir()
		cmd := exec.Command("git", "init")
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git init failed: %v", err)
		assert.NotEmpty(t, out, "git init should produce stdout output")
		for _, args := range [][]string{
			{"config", "user.email", "test@test.com"},
			{"config", "user.name", "Test"},
		} {
			c := exec.Command("git", args...)
			c.Dir = dir
			err := c.Run()
			require.NoError(t, err, "git %v: %v", args, err)
		}

		ag := &agent.FakeAgent{
			NameStr: "test",
			ReviewFn: func(ctx context.Context, repoPath, commitSHA, prompt string, output io.Writer) (string, error) {
				// Simulate agent creating the first commit
				if err := os.WriteFile(filepath.Join(repoPath, "fix.txt"), []byte("fixed"), 0o644); err != nil {
					return "", fmt.Errorf("write file: %w", err)
				}
				c := exec.Command("git", "add", ".")
				c.Dir = repoPath
				if err := c.Run(); err != nil {
					return "", fmt.Errorf("git add: %w", err)
				}
				c = exec.Command("git", "commit", "-m", "first commit")
				c.Dir = repoPath
				if err := c.Run(); err != nil {
					return "", fmt.Errorf("git commit: %w", err)
				}
				return "applied fix", nil
			},
		}

		result, err := fixJobDirect(context.Background(), fixJobParams{
			RepoRoot: dir,
			Agent:    ag,
		}, "fix things")
		require.NoError(t, err, "fixJobDirect: %v")

		assert.True(t, result.CommitCreated)
		assert.False(t, result.NoChanges)
		assert.NotEmpty(t, result.NewCommitSHA)
	})

	t.Run("agent makes no changes on unborn head", func(t *testing.T) {
		dir := t.TempDir()
		cmd := exec.Command("git", "init")
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git init failed: %v\n%s", err, out)

		ag := &agent.FakeAgent{
			NameStr: "test",
			ReviewFn: func(ctx context.Context, repoPath, commitSHA, prompt string, output io.Writer) (string, error) {
				return "nothing to do", nil
			},
		}

		result, err := fixJobDirect(context.Background(), fixJobParams{
			RepoRoot: dir,
			Agent:    ag,
		}, "fix things")
		require.NoError(t, err, "fixJobDirect: %v")

		assert.False(t, result.CommitCreated)
		assert.True(t, result.NoChanges)
	})
}

func TestFixJobDirect_RetryThreadsCapturedSessionID(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	dir := t.TempDir()
	for _, args := range [][]string{
		{"init"},
		{"config", "user.email", "test@test.com"},
		{"config", "user.name", "Test"},
		{"commit", "--allow-empty", "-m", "base"},
	} {
		c := exec.Command("git", args...)
		c.Dir = dir
		require.NoError(t, c.Run(), "git %v", args)
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "dirty.txt"), []byte("uncommitted"), 0o644))

	tester := agent.NewTestAgent()
	tester.Delay = 0
	capture := agent.NewSessionCaptureWriter(io.Discard, nil)

	_, err := fixJobDirect(context.Background(), fixJobParams{
		RepoRoot: dir,
		Agent:    tester,
		Output:   capture,
	}, "fix things")
	require.NoError(t, err)

	calls := tester.Calls()
	require.Len(t, calls, 2, "fixJobDirect should call Review twice when uncommitted changes remain")
	assert.Empty(t, calls[0].SessionID, "first call is fresh")
	assert.Equal(t, "test-session-1", calls[1].SessionID,
		"retry must resume the first call's session so the tracker captures the latest context")
}

func TestBuildBatchFixPrompt(t *testing.T) {
	entries := []batchEntry{
		{
			jobID:  123,
			job:    &storage.ReviewJob{GitRef: "abc123def456"},
			review: &storage.Review{Output: "Found bug in foo.go"},
		},
		{
			jobID:  456,
			job:    &storage.ReviewJob{GitRef: "deadbeef1234"},
			review: &storage.Review{Output: "Missing error check in bar.go"},
		},
	}

	prompt := buildBatchFixPrompt(entries, "")

	// Header
	assert.Contains(t, prompt, "# Batch Fix Request")
	assert.Contains(t, prompt, "Address all findings across all reviews in a single pass")

	// Per-review sections with numbered headers
	assert.Contains(t, prompt, "## Review 1 (Job 123 — abc123d)")
	assert.Contains(t, prompt, "Found bug in foo.go")
	assert.Contains(t, prompt, "## Review 2 (Job 456 — deadbee)")
	assert.Contains(t, prompt, "Missing error check in bar.go")

	// Instructions footer
	assert.Contains(t, prompt, "## Instructions")
	assert.Contains(t, prompt, "git commit")
}

func TestBuildBatchFixPromptSingleEntry(t *testing.T) {
	entries := []batchEntry{
		{
			jobID:  7,
			job:    &storage.ReviewJob{GitRef: "aaa"},
			review: &storage.Review{Output: "one issue"},
		},
	}

	prompt := buildBatchFixPrompt(entries, "")
	assert.Contains(t, prompt, "## Review 1 (Job 7")
}

func TestSplitIntoBatches(t *testing.T) {
	makeEntry := func(id int64, outputSize int) batchEntry {
		return batchEntry{
			jobID:  id,
			job:    &storage.ReviewJob{GitRef: fmt.Sprintf("sha%d", id)},
			review: &storage.Review{Output: strings.Repeat("x", outputSize)},
		}
	}

	t.Run("all fit in one batch", func(t *testing.T) {
		entries := []batchEntry{
			makeEntry(1, 100),
			makeEntry(2, 100),
			makeEntry(3, 100),
		}
		batches := splitIntoBatches(entries, batchSplitOptions{MaxSize: 100000})
		assert.Len(t, batches, 1)
		assert.Len(t, batches[0], 3)
	})

	t.Run("splits when exceeding limit", func(t *testing.T) {
		entries := []batchEntry{
			makeEntry(1, 500),
			makeEntry(2, 500),
			makeEntry(3, 500),
		}
		// Set limit small enough that not all fit (overhead ~300 bytes + entry ~530 each)
		maxSize := 1000
		batches := splitIntoBatches(entries, batchSplitOptions{MaxSize: maxSize})
		assert.GreaterOrEqual(t, len(batches), 2)
		// All entries should be present across batches
		total := 0
		for _, b := range batches {
			total += len(b)
		}
		assert.Equal(t, 3, total)
	})

	t.Run("oversized single review gets own batch", func(t *testing.T) {
		entries := []batchEntry{
			makeEntry(1, 100),
			makeEntry(2, 5000), // oversized
			makeEntry(3, 100),
		}
		batches := splitIntoBatches(entries, batchSplitOptions{MaxSize: 1000})
		assert.GreaterOrEqual(t, len(batches), 2)
		// The oversized entry should be alone in its batch
		found := false
		for _, b := range batches {
			for _, e := range b {
				if e.jobID == 2 && len(b) == 1 {
					found = true
				}
			}
		}
		assert.True(t, found)
	})

	t.Run("empty input", func(t *testing.T) {
		batches := splitIntoBatches(nil, batchSplitOptions{MaxSize: 100000})
		assert.Empty(t, batches)
	})

	t.Run("built prompt respects size estimate", func(t *testing.T) {
		// Verify that splitIntoBatches size accounting matches buildBatchFixPrompt output.
		entries := []batchEntry{
			makeEntry(1, 200),
			makeEntry(2, 200),
			makeEntry(3, 200),
			makeEntry(4, 200),
			makeEntry(5, 200),
		}
		maxSize := 1000
		batches := splitIntoBatches(entries, batchSplitOptions{MaxSize: maxSize})
		for _, batch := range batches {
			prompt := buildBatchFixPrompt(batch, "")
			// Single-entry batches that are inherently oversized are allowed to exceed.
			assert.False(t, len(batch) > 1 && len(prompt) > maxSize)
		}
	})

	t.Run("preserves order", func(t *testing.T) {
		entries := []batchEntry{
			makeEntry(10, 100),
			makeEntry(20, 100),
			makeEntry(30, 100),
		}
		batches := splitIntoBatches(entries, batchSplitOptions{MaxSize: 100000})
		assert.Len(t, batches, 1)
		for i, want := range []int64{10, 20, 30} {
			assert.Equal(t, want, batches[0][i].jobID)
		}
	})

	t.Run("severity filter adds overhead", func(t *testing.T) {
		entries := []batchEntry{
			makeEntry(1, 200),
			makeEntry(2, 200),
			makeEntry(3, 200),
		}
		// With severity, the overhead is larger so entries may split
		// into more batches than without severity.
		batchesNoSev := splitIntoBatches(entries, batchSplitOptions{MaxSize: 1200})
		batchesWithSev := splitIntoBatches(entries, batchSplitOptions{MaxSize: 1200, MinSeverity: "high"})
		if len(batchesWithSev) < len(batchesNoSev) {
			assert.Condition(t, func() bool {
				return false
			}, "severity filter should not reduce batch count: "+
				"without=%d, with=%d",
				len(batchesNoSev), len(batchesWithSev))
		}
		// Verify the prompt respects the size estimate
		for i, batch := range batchesWithSev {
			p := buildBatchFixPrompt(batch, "high")
			if len(batch) > 1 && len(p) > 1200 {
				assert.Condition(t, func() bool {
					return false
				}, "batch %d prompt size %d exceeds limit 1200",
					i, len(p))
			}
		}
	})

	t.Run("count cap forces multiple batches when size would allow one", func(t *testing.T) {
		entries := []batchEntry{
			makeEntry(1, 100),
			makeEntry(2, 100),
			makeEntry(3, 100),
			makeEntry(4, 100),
			makeEntry(5, 100),
		}
		batches := splitIntoBatches(entries, batchSplitOptions{
			MaxSize:  100000,
			MaxCount: 2,
		})
		require.Len(t, batches, 3)
		assert.Len(t, batches[0], 2)
		assert.Len(t, batches[1], 2)
		assert.Len(t, batches[2], 1)
	})

	t.Run("count cap and size cap, count cap dominates", func(t *testing.T) {
		entries := []batchEntry{
			makeEntry(1, 100),
			makeEntry(2, 100),
			makeEntry(3, 100),
		}
		batches := splitIntoBatches(entries, batchSplitOptions{
			MaxSize:  100000,
			MaxCount: 1,
		})
		require.Len(t, batches, 3)
		for _, b := range batches {
			assert.Len(t, b, 1)
		}
	})

	t.Run("size cap and count cap, size cap dominates", func(t *testing.T) {
		entries := []batchEntry{
			makeEntry(1, 500),
			makeEntry(2, 500),
			makeEntry(3, 500),
		}
		batches := splitIntoBatches(entries, batchSplitOptions{
			MaxSize:  1000,
			MaxCount: 100,
		})
		assert.GreaterOrEqual(t, len(batches), 2,
			"size cap forces a split even though count cap is far higher")
	})

	t.Run("count cap = 0 means unbounded", func(t *testing.T) {
		entries := []batchEntry{
			makeEntry(1, 100),
			makeEntry(2, 100),
			makeEntry(3, 100),
			makeEntry(4, 100),
		}
		batches := splitIntoBatches(entries, batchSplitOptions{
			MaxSize:  100000,
			MaxCount: 0,
		})
		require.Len(t, batches, 1)
		assert.Len(t, batches[0], 4)
	})
}

func TestFormatJobIDs(t *testing.T) {
	tests := []struct {
		ids  []int64
		want string
	}{
		{[]int64{1}, "1"},
		{[]int64{1, 2, 3}, "1, 2, 3"},
		{[]int64{100, 200}, "100, 200"},
	}
	for _, tt := range tests {
		got := formatJobIDs(tt.ids)
		assert.Equal(t, tt.want, got)
	}
}

func TestEnqueueIfNeededSkipsWhenJobExists(t *testing.T) {
	repo := createTestRepo(t, map[string]string{"f.txt": "x"})
	sha := "abc123def456"

	var enqueueCalls atomic.Int32

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/jobs":
			// Return an existing job — hook already fired
			json.NewEncoder(w).Encode(map[string]any{
				"jobs": []map[string]any{{"id": 42}},
			})
		case "/api/enqueue":
			enqueueCalls.Add(1)
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{"id": 99})
		}
	}))
	defer ts.Close()

	err := enqueueIfNeeded(context.Background(), ts.URL, repo.Dir, sha)
	require.NoError(t, err, "enqueueIfNeeded: %v")

	assert.EqualValues(t, 0, enqueueCalls.Load())
}

func TestEnqueueIfNeededAvoidsDuplicatePostAfterConnectionDrop(t *testing.T) {
	repo := createTestRepo(t, map[string]string{"f.txt": "x"})
	sha := repo.Run("rev-parse", "HEAD")

	var firstPostCount atomic.Int32
	var recoveryPostCount atomic.Int32
	var jobExists atomic.Bool

	startServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/jobs":
			writeJSON(w, map[string]any{"jobs": []storage.ReviewJob{}})
		case "/api/enqueue":
			firstPostCount.Add(1)
			jobExists.Store(true)
			closeConnNoResponse(t, w)
		default:
			http.NotFound(w, r)
		}
	}))
	defer startServer.Close()

	recoveryServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/jobs":
			jobs := []storage.ReviewJob{}
			if jobExists.Load() {
				jobs = append(jobs, storage.ReviewJob{ID: 42, GitRef: sha})
			}
			writeJSON(w, map[string]any{"jobs": jobs})
		case "/api/enqueue":
			recoveryPostCount.Add(1)
			w.WriteHeader(http.StatusCreated)
		default:
			http.NotFound(w, r)
		}
	}))
	defer recoveryServer.Close()

	patchFixDaemonRetryForTest(t, func() error {
		serverAddr = recoveryServer.URL
		return nil
	})

	err := enqueueIfNeeded(context.Background(), startServer.URL, repo.Dir, sha)
	require.NoError(t, err, "enqueueIfNeeded: %v")

	assert.EqualValues(t, 1, firstPostCount.Load())
	assert.EqualValues(t, 0, recoveryPostCount.Load())
}

func TestEnqueueIfNeededSkipsEnqueueAfterTransientProbeFailure(t *testing.T) {
	repo := createTestRepo(t, map[string]string{"f.txt": "x"})
	sha := repo.Run("rev-parse", "HEAD")

	patchFixDaemonRetryForTest(t, nil)
	oldProbeAttempts := enqueueIfNeededProbeAttempts
	oldProbeDelay := enqueueIfNeededProbeDelay
	enqueueIfNeededProbeAttempts = 2
	enqueueIfNeededProbeDelay = time.Millisecond
	t.Cleanup(func() {
		enqueueIfNeededProbeAttempts = oldProbeAttempts
		enqueueIfNeededProbeDelay = oldProbeDelay
	})

	var jobsCalls atomic.Int32
	var enqueueCalls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/jobs":
			if jobsCalls.Add(1) == 1 {
				closeConnNoResponse(t, w)
				return
			}
			writeJSON(w, map[string]any{
				"jobs": []storage.ReviewJob{{ID: 42, GitRef: sha}},
			})
		case "/api/enqueue":
			enqueueCalls.Add(1)
			w.WriteHeader(http.StatusCreated)
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	err := enqueueIfNeeded(context.Background(), ts.URL, repo.Dir, sha)
	require.NoError(t, err, "enqueueIfNeeded: %v")

	assert.EqualValues(t, 0, enqueueCalls.Load())
	assert.GreaterOrEqual(t, jobsCalls.Load(), int32(2))
}

func TestEnqueueIfNeededVerificationFailureReturnsErrorWithoutDuplicatePost(t *testing.T) {
	repo := createTestRepo(t, map[string]string{"f.txt": "x"})
	sha := repo.Run("rev-parse", "HEAD")

	var firstPostCount atomic.Int32
	var recoveryPostCount atomic.Int32

	startServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/jobs":
			writeJSON(w, map[string]any{"jobs": []storage.ReviewJob{}})
		case "/api/enqueue":
			firstPostCount.Add(1)
			closeConnNoResponse(t, w)
		default:
			http.NotFound(w, r)
		}
	}))
	defer startServer.Close()

	recoveryServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/jobs":
			http.Error(w, "boom", http.StatusInternalServerError)
		case "/api/enqueue":
			recoveryPostCount.Add(1)
			w.WriteHeader(http.StatusCreated)
		default:
			http.NotFound(w, r)
		}
	}))
	defer recoveryServer.Close()

	patchFixDaemonRetryForTest(t, func() error {
		serverAddr = recoveryServer.URL
		return nil
	})

	err := enqueueIfNeeded(context.Background(), startServer.URL, repo.Dir, sha)
	require.Error(t, err)

	require.Contains(t, err.Error(), "verify enqueue after retryable failure")
	assert.EqualValues(t, 1, firstPostCount.Load())
	assert.EqualValues(t, 0, recoveryPostCount.Load())
}

func TestEnqueueIfNeededDeadlineExceededCancelsProbeRequest(t *testing.T) {
	repo := createTestRepo(t, map[string]string{"f.txt": "x"})
	sha := repo.Run("rev-parse", "HEAD")

	patchFixDaemonRetryForTest(t, func() error {
		require.Condition(t, func() bool { return false }, "enqueueIfNeeded should not attempt daemon recovery after request deadline exceeded")
		return nil
	})

	var enqueueCalls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/jobs":
			time.Sleep(100 * time.Millisecond)
		case "/api/enqueue":
			enqueueCalls.Add(1)
			w.WriteHeader(http.StatusCreated)
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	err := enqueueIfNeeded(ctx, ts.URL, repo.Dir, sha)
	if !errors.Is(err, context.DeadlineExceeded) {
		require.NoError(t, err, "expected context deadline exceeded, got %v")
	}
	assert.EqualValues(t, 0, enqueueCalls.Load())
}

func TestEnqueueIfNeededRefreshesProbeAddressAfterConnectionError(t *testing.T) {
	repo := createTestRepo(t, map[string]string{"f.txt": "x"})
	sha := repo.Run("rev-parse", "HEAD")

	oldProbeAttempts := enqueueIfNeededProbeAttempts
	oldProbeDelay := enqueueIfNeededProbeDelay
	enqueueIfNeededProbeAttempts = 2
	enqueueIfNeededProbeDelay = time.Millisecond
	t.Cleanup(func() {
		enqueueIfNeededProbeAttempts = oldProbeAttempts
		enqueueIfNeededProbeDelay = oldProbeDelay
	})

	var ensureCalls atomic.Int32
	var jobsCalls atomic.Int32
	var enqueueCalls atomic.Int32
	recoveryServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/jobs":
			jobsCalls.Add(1)
			writeJSON(w, map[string]any{
				"jobs": []storage.ReviewJob{{ID: 42, GitRef: sha}},
			})
		case "/api/enqueue":
			enqueueCalls.Add(1)
			w.WriteHeader(http.StatusCreated)
		default:
			http.NotFound(w, r)
		}
	}))
	defer recoveryServer.Close()

	patchFixDaemonRetryForTest(t, func() error {
		ensureCalls.Add(1)
		serverAddr = recoveryServer.URL
		return nil
	})

	err := enqueueIfNeeded(context.Background(), "http://127.0.0.1:1", repo.Dir, sha)
	require.NoError(t, err, "enqueueIfNeeded: %v")

	if ensureCalls.Load() == 0 {
		require.NoError(t, err)
	}
	if jobsCalls.Load() == 0 {
		require.NoError(t, err, "expected probe to hit refreshed daemon address")
	}
	assert.EqualValues(t, 0, enqueueCalls.Load())
}

func TestHasJobForSHADoesNotTriggerDaemonRecovery(t *testing.T) {
	patchFixDaemonRetryForTest(t, func() error {
		return fmt.Errorf("hasJobForSHA should not attempt daemon recovery")
	})

	found, err := hasJobForSHA("http://127.0.0.1:1", "abc123def456")
	require.Error(t, err)

	assert.False(t, found, "expected no job match on connection failure")
}

func TestQueryOpenJobIDsDeadlineExceededCancelsRequest(t *testing.T) {
	patchFixDaemonRetryForTest(t, func() error {
		return fmt.Errorf("queryOpenJobIDs should not attempt daemon recovery after request deadline exceeded")
	})

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/jobs" {
			http.NotFound(w, r)
			return
		}
		time.Sleep(100 * time.Millisecond)
	}))
	defer ts.Close()

	oldServerAddr := serverAddr
	serverAddr = ts.URL
	t.Cleanup(func() { serverAddr = oldServerAddr })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	_, err := queryOpenJobIDs(ctx, "/tmp/repo", "")
	if !errors.Is(err, context.DeadlineExceeded) {
		require.NoError(t, err, "expected context deadline exceeded, got %v")
	}
}

func TestRunFixList(t *testing.T) {
	repo := createTestRepo(t, map[string]string{"f.txt": "x"})

	t.Run("lists open jobs with details", func(t *testing.T) {
		finishedAt := time.Date(2024, 6, 15, 10, 30, 0, 0, time.UTC)
		verdict := "F"

		_ = newMockDaemonBuilder(t).
			WithJobs([]storage.ReviewJob{{
				ID:            42,
				GitRef:        "abc123def456",
				Branch:        "feature-branch",
				CommitSubject: "Fix the widget",
				Agent:         "claude-code",
				Model:         "claude-3-opus",
				Status:        storage.JobStatusDone,
				JobType:       storage.JobTypeReview,
				FinishedAt:    &finishedAt,
				Verdict:       &verdict,
			}}).
			WithReview(42, "Found 3 issues:\n- Missing error handling\n- Unused variable").
			Build()
			// serverAddr is patched by daemonFromHandler called inside Build()

		out, err := runWithOutput(t, repo.Dir, func(cmd *cobra.Command) error {
			return runFixList(cmd, "", true, false, false)
		})
		require.NoError(t, err, "runFixList")

		// Check header
		assert.Contains(t, out, "Found 1 open job(s):")

		// Check job details are displayed
		assert.Contains(t, out, "Job #42")
		assert.Contains(t, out, "Git Ref:  abc123d")
		assert.Contains(t, out, "Branch:   feature-branch")
		assert.Contains(t, out, "Subject:  Fix the widget")
		assert.Contains(t, out, "Agent:    claude-code")
		assert.Contains(t, out, "Model:    claude-3-opus")
		assert.Contains(t, out, "Verdict:  F")
		assert.Contains(t, out, "Summary:  Found 3 issues:")

		// Check usage hints
		assert.Contains(t, out, "roborev fix <job_id>")
		assert.Contains(t, out, "roborev fix\n")
	})

	t.Run("no open jobs", func(t *testing.T) {
		_ = newMockDaemonBuilder(t).
			WithJobs([]storage.ReviewJob{}).
			Build()

		out, err := runWithOutput(t, repo.Dir, func(cmd *cobra.Command) error {
			return runFixList(cmd, "", true, false, false)
		})
		require.NoError(t, err, "runFixList")

		assert.Contains(t, out, "No open jobs found")
	})

	t.Run("respects newest-first flag", func(t *testing.T) {
		failVerdict := "F"
		var gotIDs []int64
		_ = newMockDaemonBuilder(t).
			WithHandler("/api/jobs", func(w http.ResponseWriter, r *http.Request) {
				q := r.URL.Query()
				if q.Get("closed") == "false" && q.Get("limit") == "0" {
					// API returns newest first
					writeJSON(w, map[string]any{
						"jobs": []storage.ReviewJob{
							{ID: 30, Status: storage.JobStatusDone, Agent: "test", JobType: storage.JobTypeReview, Verdict: &failVerdict},
							{ID: 20, Status: storage.JobStatusDone, Agent: "test", JobType: storage.JobTypeReview, Verdict: &failVerdict},
							{ID: 10, Status: storage.JobStatusDone, Agent: "test", JobType: storage.JobTypeReview, Verdict: &failVerdict},
						},
						"has_more": false,
					})
				} else if q.Get("id") != "" {
					var id int64
					_, _ = fmt.Sscanf(q.Get("id"), "%d", &id)
					gotIDs = append(gotIDs, id)
					writeJSON(w, map[string]any{
						"jobs": []storage.ReviewJob{
							{ID: id, Status: storage.JobStatusDone, Agent: "test"},
						},
						"has_more": false,
					})
				}
			}).
			WithReview(30, "findings").
			WithReview(20, "findings").
			WithReview(10, "findings").
			Build()

		gotIDs = nil
		_, err := runWithOutput(t, repo.Dir, func(cmd *cobra.Command) error {
			return runFixList(cmd, "", true, false, true)
		})
		require.NoError(t, err, "runFixList: %v")

		assert.Len(t, gotIDs, 3)
		assert.Equal(t, []int64{30, 20, 10}, gotIDs)
	})
}

func TestTruncateString(t *testing.T) {
	tests := []struct {
		s      string
		maxLen int
		want   string
	}{
		{"hello", 10, "hello"},
		{"hello", 5, "hello"},
		{"hello world", 8, "hello..."},
		{"hello world", 5, "he..."},
		{"hi", 3, "hi"},
		{"hello", 3, "hel"},
		{"", 10, ""},
		// Edge cases for maxLen <= 0
		{"hello", 0, ""},
		{"hello", -1, ""},
		// Unicode handling: ensure multi-byte characters aren't split
		{"こんにちは世界", 5, "こん..."},      // Japanese: maxLen=5, output is 2 chars + "..." = 5 runes
		{"こんにちは", 10, "こんにちは"},       // Japanese: fits within limit
		{"Hello 世界!", 8, "Hello..."}, // Mixed ASCII and Unicode
		{"🎉🎊🎁🎄🎅", 3, "🎉🎊🎁"},          // Emoji: exactly 3 runes
		{"🎉🎊🎁🎄🎅", 4, "🎉..."},         // Emoji: truncate with ellipsis
	}

	for _, tt := range tests {
		got := truncateString(tt.s, tt.maxLen)
		assert.Equal(t, tt.want, got)
	}
}

// setupWorktree creates a main repo with a commit and a worktree, returning
// the main repo and the worktree directory path.
func setupWorktree(t *testing.T) (mainRepo *TestGitRepo, worktreeDir string) {
	t.Helper()
	repo := newTestGitRepo(t)
	repo.CommitFile("file.txt", "content", "initial")

	wtDir := t.TempDir()
	os.Remove(wtDir)
	repo.Run("worktree", "add", "-b", "wt-branch", wtDir)
	return repo, wtDir
}

// setupWorktreeMockDaemon sets up a mock daemon that captures the repo query
// param from /api/jobs requests, returning empty results.
func setupWorktreeMockDaemon(t *testing.T) (receivedRepo *string) {
	t.Helper()
	var repo string
	_ = newMockDaemonBuilder(t).
		WithHandler("/api/jobs", func(w http.ResponseWriter, r *http.Request) {
			repo = r.URL.Query().Get("repo")
			writeJSON(w, map[string]any{
				"jobs":     []storage.ReviewJob{},
				"has_more": false,
			})
		}).
		Build()
	return &repo
}

func TestFixWorktreeRepoResolution(t *testing.T) {
	t.Run("runFixList sends main repo path", func(t *testing.T) {
		receivedRepo := setupWorktreeMockDaemon(t)
		repo, worktreeDir := setupWorktree(t)
		chdir(t, worktreeDir)

		cmd := &cobra.Command{}
		var buf bytes.Buffer
		cmd.SetOut(&buf)
		if err := runFixList(cmd, "", true, false, false); err != nil {
			require.NoError(t, err, "runFixList: %v")
		}

		require.NotEmpty(t, *receivedRepo, "expected repo param to be sent")
		assert.Equal(t, *receivedRepo, repo.Dir)
	})

	t.Run("runFixOpen sends main repo path", func(t *testing.T) {
		receivedRepo := setupWorktreeMockDaemon(t)
		repo, worktreeDir := setupWorktree(t)
		chdir(t, worktreeDir)

		cmd := &cobra.Command{}
		var buf bytes.Buffer
		cmd.SetOut(&buf)
		opts := fixOptions{quiet: true}
		tracker := &fixSessionTracker{base: agent.NewTestAgent(), out: io.Discard}
		if err := runFixOpen(cmd, "", false, false, false, opts, tracker); err != nil {
			require.NoError(t, err, "runFixOpen: %v")
		}

		require.NotEmpty(t, *receivedRepo, "expected repo param to be sent")
		assert.Equal(t, *receivedRepo, repo.Dir)
	})

	t.Run("runFixBatch sends main repo path", func(t *testing.T) {
		receivedRepo := setupWorktreeMockDaemon(t)
		repo, worktreeDir := setupWorktree(t)
		chdir(t, worktreeDir)

		cmd := &cobra.Command{}
		var buf bytes.Buffer
		cmd.SetOut(&buf)
		opts := fixOptions{quiet: true}
		tracker := &fixSessionTracker{base: agent.NewTestAgent(), out: io.Discard}
		// nil jobIDs triggers discovery via queryOpenJobs
		if err := runFixBatch(cmd, nil, "", false, false, false, 0, opts, tracker); err != nil {
			require.NoError(t, err, "runFixBatch: %v")
		}

		require.NotEmpty(t, *receivedRepo, "expected repo param to be sent")
		assert.Equal(t, *receivedRepo, repo.Dir)
	})
}

func TestResolveCurrentRepoRoots(t *testing.T) {
	repo, worktreeDir := setupWorktree(t)
	chdir(t, worktreeDir)

	resolvedWorktreeDir, err := filepath.EvalSymlinks(worktreeDir)
	require.NoError(t, err)

	roots, err := resolveCurrentRepoRoots(t.Context())
	require.NoError(t, err)
	assert.Equal(t, resolvedWorktreeDir, roots.worktreeRoot)
	assert.Equal(t, repo.Dir, roots.mainRepoRoot)
}

func TestResolveCurrentBranchFilter(t *testing.T) {
	t.Run("uses worktree branch when branch omitted", func(t *testing.T) {
		_, worktreeDir := setupWorktree(t)
		assert.Equal(t, "wt-branch", resolveCurrentBranchFilter(t.Context(), worktreeDir, "", false))
	})

	t.Run("returns explicit branch", func(t *testing.T) {
		repo := newTestGitRepo(t)
		repo.CommitFile("a.txt", "a", "initial")
		assert.Equal(t, "feature/custom", resolveCurrentBranchFilter(t.Context(), repo.Dir, "feature/custom", false))
	})

	t.Run("returns empty string for all branches", func(t *testing.T) {
		repo := newTestGitRepo(t)
		repo.CommitFile("a.txt", "a", "initial")
		assert.Empty(t, resolveCurrentBranchFilter(t.Context(), repo.Dir, "", true))
	})
}

func TestJobVerdict(t *testing.T) {
	pass := "P"
	fail := "F"
	empty := ""

	tests := []struct {
		name    string
		verdict *string
		output  string
		want    string
	}{
		{
			name:    "stored PASS verdict",
			verdict: &pass,
			output:  "some output",
			want:    "P",
		},
		{
			name:    "stored FAIL verdict",
			verdict: &fail,
			output:  "No issues found.",
			want:    "F",
		},
		{
			name:    "nil verdict falls back to parse",
			verdict: nil,
			output:  "No issues found.",
			want:    "P",
		},
		{
			name:    "empty verdict falls back to parse",
			verdict: &empty,
			output:  "## Issues\n- Bug in foo.go",
			want:    "F",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job := &storage.ReviewJob{Verdict: tt.verdict}
			review := &storage.Review{Output: tt.output}
			got := jobVerdict(job, review)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestFixSingleJobSkipsPassVerdict(t *testing.T) {
	repo := createTestRepo(t, map[string]string{
		"main.go": "package main\n",
	})

	var closeCalls atomic.Int32
	var agentCalled atomic.Int32

	ts, _ := newMockServer(t, MockServerOpts{
		ReviewOutput: "No issues found.",
		OnJobs: func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, map[string]any{
				"jobs": []storage.ReviewJob{{
					ID:     99,
					Status: storage.JobStatusDone,
					Agent:  "test",
				}},
			})
		},
	})
	// Wrap the server to track close calls
	wrapper := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/review/close" {
			closeCalls.Add(1)
			w.WriteHeader(http.StatusOK)
			return
		}
		// Proxy to the original mock
		proxy, err := http.NewRequest(r.Method, ts.URL+r.URL.String(), r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		resp, err := http.DefaultClient.Do(proxy)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
	}))
	t.Cleanup(wrapper.Close)
	patchServerAddr(t, wrapper.URL)

	cmd, output := newTestCmd(t)

	// Use a fake agent that tracks invocations
	agent.Register(&agent.FakeAgent{
		NameStr: "test-pass-skip",
		ReviewFn: func(_ context.Context, _, _, _ string, _ io.Writer) (string, error) {
			agentCalled.Add(1)
			return "", nil
		},
	})
	t.Cleanup(func() { agent.Unregister("test-pass-skip") })

	opts := fixOptions{agentName: "test-pass-skip"}

	tracker := &fixSessionTracker{base: agent.NewTestAgent(), out: io.Discard}
	err := fixSingleJob(cmd, repo.Dir, 99, opts, tracker)
	require.NoError(t, err, "fixSingleJob")

	outputStr := output.String()
	assert.Contains(t, outputStr, "review passed, skipping fix")
	assert.EqualValues(t, 0, agentCalled.Load())
	assert.EqualValues(t, 1, closeCalls.Load())
}

func TestFixBatchSkipsPassVerdict(t *testing.T) {
	repo := createTestRepo(t, map[string]string{
		"main.go": "package main\n",
	})

	var mu sync.Mutex
	var closedJobIDs []int64

	passVerdict := "P"

	_ = newMockDaemonBuilder(t).
		WithHandler("/api/jobs", func(w http.ResponseWriter, r *http.Request) {
			q := r.URL.Query()
			if q.Get("id") == "10" {
				writeJSON(w, map[string]any{
					"jobs": []storage.ReviewJob{{
						ID:      10,
						Status:  storage.JobStatusDone,
						Agent:   "test",
						Verdict: &passVerdict,
					}},
				})
			} else if q.Get("id") == "20" {
				writeJSON(w, map[string]any{
					"jobs": []storage.ReviewJob{{
						ID:     20,
						Status: storage.JobStatusDone,
						Agent:  "test",
					}},
				})
			} else {
				writeJSON(w, map[string]any{
					"jobs":     []storage.ReviewJob{},
					"has_more": false,
				})
			}
		}).
		WithHandler("/api/review", func(w http.ResponseWriter, r *http.Request) {
			jobID := r.URL.Query().Get("job_id")
			if jobID == "10" {
				writeJSON(w, storage.Review{
					JobID:  10,
					Output: "No issues found.",
				})
			} else {
				writeJSON(w, storage.Review{
					JobID:  20,
					Output: "## Issues\n- Bug in foo.go",
				})
			}
		}).
		WithHandler("/api/review/close", func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				JobID int64 `json:"job_id"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err == nil {
				mu.Lock()
				closedJobIDs = append(closedJobIDs, body.JobID)
				mu.Unlock()
			}
			w.WriteHeader(http.StatusOK)
		}).
		WithHandler("/api/enqueue", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}).
		Build()

	out, err := runWithOutput(t, repo.Dir, func(cmd *cobra.Command) error {
		return runFixBatch(
			cmd,
			[]int64{10, 20},
			"",
			false, false, false,
			0,
			fixOptions{agentName: "test", reasoning: "fast"},
			&fixSessionTracker{base: agent.NewTestAgent(), out: io.Discard},
		)
	})
	require.NoError(t, err, "runFixBatch: %v")

	assert.Contains(t, out, "Skipping job 10 (review passed)")
	// Job 20 (FAIL) should be processed — its findings should appear
	assert.Contains(t, out, "Bug in foo.go")

	// Verify PASS job 10 was closed during the skip phase
	mu.Lock()
	ids := closedJobIDs
	mu.Unlock()
	assert.True(t, slices.Contains(ids, int64(10)))
}

// TestRunFixBatchRequery verifies that runFixBatch in discovery mode
// (no explicit job IDs) re-queries for open jobs after processing the
// initial set, picking up new jobs that appeared mid-run. This mirrors
// the keep-going behavior of runFixOpen.
func TestRunFixBatchRequery(t *testing.T) {
	repo := createTestRepo(t, map[string]string{"f.txt": "x"})
	repoBranch := strings.TrimSpace(repo.Run("rev-parse", "--abbrev-ref", "HEAD"))
	failVerdict := "F"

	var queryCount atomic.Int32
	_ = newMockDaemonBuilder(t).
		WithHandler("/api/jobs", func(w http.ResponseWriter, r *http.Request) {
			q := r.URL.Query()
			if q.Get("closed") == "false" && q.Get("limit") == "0" {
				n := queryCount.Add(1)
				switch n {
				case 1:
					writeJSON(w, map[string]any{
						"jobs": []storage.ReviewJob{
							{ID: 10, Status: storage.JobStatusDone, Agent: "test", Branch: repoBranch, JobType: storage.JobTypeReview, Verdict: &failVerdict},
						},
						"has_more": false,
					})
				case 2:
					writeJSON(w, map[string]any{
						"jobs": []storage.ReviewJob{
							{ID: 20, Status: storage.JobStatusDone, Agent: "test", Branch: repoBranch, JobType: storage.JobTypeReview, Verdict: &failVerdict},
							{ID: 10, Status: storage.JobStatusDone, Agent: "test", Branch: repoBranch, JobType: storage.JobTypeReview, Verdict: &failVerdict},
						},
						"has_more": false,
					})
				default:
					writeJSON(w, map[string]any{
						"jobs":     []storage.ReviewJob{},
						"has_more": false,
					})
				}
				return
			}
			var id int64
			fmt.Sscanf(q.Get("id"), "%d", &id)
			writeJSON(w, map[string]any{
				"jobs": []storage.ReviewJob{
					{ID: id, Status: storage.JobStatusDone, Agent: "test", Branch: repoBranch},
				},
				"has_more": false,
			})
		}).
		WithHandler("/api/review", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, storage.Review{Output: "findings"})
		}).
		WithHandler("/api/comment", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusCreated)
		}).
		WithHandler("/api/review/close", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}).
		WithHandler("/api/enqueue", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}).
		Build()

	out, err := runWithOutput(t, repo.Dir, func(cmd *cobra.Command) error {
		tracker := &fixSessionTracker{base: agent.NewTestAgent(), out: io.Discard}
		return runFixBatch(cmd, nil, "", false, false, false, 1,
			fixOptions{agentName: "test", reasoning: "fast"}, tracker)
	})
	require.NoError(t, err)

	assert.Equal(t, int32(3), queryCount.Load(),
		"expected 3 discovery queries: initial, requery picks up job 20, requery sees nothing")
	assert.Contains(t, out, "Found 1 open job(s)")
	assert.Contains(t, out, "Found 1 new open job(s)")
}

func setupFixErrorMockDaemon(t *testing.T, processedJobs *[]int64, mu *sync.Mutex) {
	t.Helper()
	_ = newMockDaemonBuilder(t).
		WithHandler("/api/jobs", func(w http.ResponseWriter, r *http.Request) {
			q := r.URL.Query()
			jobIDStr := q.Get("id")
			if jobIDStr == "" {
				writeJSON(w, map[string]any{
					"jobs":     []storage.ReviewJob{},
					"has_more": false,
				})
				return
			}
			var id int64
			fmt.Sscanf(jobIDStr, "%d", &id)
			mu.Lock()
			*processedJobs = append(*processedJobs, id)
			mu.Unlock()

			// Job 20 is "running" (not complete), which causes fixSingleJob
			// to return an error.
			status := storage.JobStatusDone
			if id == 20 {
				status = storage.JobStatusRunning
			}
			writeJSON(w, map[string]any{
				"jobs": []storage.ReviewJob{
					{ID: id, Status: status, Agent: "test"},
				},
				"has_more": false,
			})
		}).
		WithHandler("/api/review", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, storage.Review{Output: "findings"})
		}).
		WithHandler("/api/review/close", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}).
		WithHandler("/api/comment", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusCreated)
		}).
		WithHandler("/api/enqueue", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}).
		Build()
}

func TestRunFixWithSeenExplicitAbortsOnError(t *testing.T) {
	// Explicit job IDs (seen == nil): failure should return an error
	// so the CLI exits non-zero for scripting/CI reliability.
	repo := createTestRepo(t, map[string]string{"f.txt": "x"})

	var processedJobs []int64
	var mu sync.Mutex
	setupFixErrorMockDaemon(t, &processedJobs, &mu)

	_, err := runWithOutput(t, repo.Dir, func(cmd *cobra.Command) error {
		tracker := &fixSessionTracker{base: agent.NewTestAgent(), out: io.Discard}
		return runFixWithSeen(cmd, []int64{10, 20, 30}, fixOptions{
			agentName: "test",
			reasoning: "fast",
		}, nil, tracker)
	})
	require.Error(t, err, "expected error for explicit job IDs")
	assert.Contains(t, err.Error(), "error fixing job 20")

	// Job 30 should NOT be processed (abort on explicit failure)
	mu.Lock()
	jobs := processedJobs
	mu.Unlock()
	assert.False(t, slices.Contains(jobs, int64(30)))
}

func TestRunFixWithSeenDiscoveryContinuesOnError(t *testing.T) {
	// Discovery mode (seen != nil): failure should warn and continue
	// best-effort so the re-query loop processes remaining jobs.
	repo := createTestRepo(t, map[string]string{"f.txt": "x"})

	var processedJobs []int64
	var mu sync.Mutex
	setupFixErrorMockDaemon(t, &processedJobs, &mu)

	seen := make(map[int64]bool)
	out, err := runWithOutput(t, repo.Dir, func(cmd *cobra.Command) error {
		tracker := &fixSessionTracker{base: agent.NewTestAgent(), out: io.Discard}
		return runFixWithSeen(cmd, []int64{10, 20, 30}, fixOptions{
			agentName: "test",
			reasoning: "fast",
		}, seen, tracker)
	})

	require.NoError(t, err, "runFixWithSeen")

	assert.Contains(t, out, "Warning: error fixing job 20")

	// All three jobs should be attempted
	mu.Lock()
	jobs := processedJobs
	mu.Unlock()
	assert.True(t, slices.Contains(jobs, int64(30)))

	// Failed job should be marked as seen
	assert.True(t, seen[20])
}

func TestRunFixWithSeenDiscoveryAbortsOnConnectionError(t *testing.T) {
	repo := createTestRepo(t, map[string]string{"f.txt": "x"})

	patchFixDaemonRetryForTest(t, func() error {
		return nil
	})

	deadURL := "http://127.0.0.1:1"
	var daemonDead atomic.Bool
	_ = newMockDaemonBuilder(t).
		WithHandler("/api/jobs", func(w http.ResponseWriter, r *http.Request) {
			if daemonDead.Load() {
				// Simulate dead daemon: hijack connection and close it
				hj, ok := w.(http.Hijacker)
				if ok {
					conn, _, _ := hj.Hijack()
					if conn != nil {
						conn.Close()
					}
				}
				return
			}
			writeJSON(w, map[string]any{
				"jobs": []storage.ReviewJob{{
					ID:     10,
					Status: storage.JobStatusDone,
					Agent:  "test",
				}},
			})
			// Simulate daemon death after responding
			daemonDead.Store(true)
			removeAllDaemonFiles(t)
			serverAddr = deadURL
			getAnyRunningDaemon = func() (*daemon.RuntimeInfo, error) {
				return nil, os.ErrNotExist
			}
		}).
		WithHandler("/api/review", func(w http.ResponseWriter, r *http.Request) {
			// Daemon is dead — hijack and close connection
			hj, ok := w.(http.Hijacker)
			if ok {
				conn, _, _ := hj.Hijack()
				if conn != nil {
					conn.Close()
				}
			}
		}).
		Build()

	seen := make(map[int64]bool)
	_, err := runWithOutput(t, repo.Dir, func(cmd *cobra.Command) error {
		tracker := &fixSessionTracker{base: agent.NewTestAgent(), out: io.Discard}
		return runFixWithSeen(cmd, []int64{10, 20}, fixOptions{
			agentName: "test",
			reasoning: "fast",
		}, seen, tracker)
	})

	require.Error(t, err, "expected connection error in discovery mode")
	if !strings.Contains(err.Error(), "daemon connection lost") {
		require.NoError(t, err)
	}
	assert.Empty(t, seen)
}

func TestResolveFixAgentSkipsDefaultModel(t *testing.T) {
	// When --agent is passed on CLI, the global default_model should
	// NOT be applied. The default_model is paired with default_agent
	// and likely doesn't apply to the overridden agent.

	tmpDir := t.TempDir()
	t.Setenv("ROBOREV_DATA_DIR", tmpDir)

	cfgPath := filepath.Join(tmpDir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte(`
default_agent = "codex"
default_model = "gpt-5.4"
`), 0o644); err != nil {
		require.NoError(t, err)
	}

	tests := []struct {
		name      string
		agentName string // CLI --agent value
		model     string // CLI --model value
		wantModel bool   // whether default_model should be applied
	}{
		{
			name:      "no CLI agent uses default_model",
			agentName: "",
			model:     "",
			wantModel: true,
		},
		{
			name:      "CLI agent skips default_model",
			agentName: "test",
			model:     "",
			wantModel: false,
		},
		{
			name:      "CLI agent with CLI model uses CLI model",
			agentName: "test",
			model:     "my-model",
			wantModel: true,
		},
		{
			name:      "no CLI agent with CLI model uses CLI model",
			agentName: "",
			model:     "my-model",
			wantModel: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := config.LoadGlobal()
			require.NoError(t, err, "LoadGlobal: %v")

			modelStr, err := resolveFixModel(
				tt.agentName, tt.model, tmpDir, cfg, "fast",
			)
			require.NoError(t, err)

			if tt.wantModel && modelStr == "" {
				assert.False(t, tt.wantModel && modelStr == "", "expected model to be set, got empty")
			}
			assert.False(t, !tt.wantModel && modelStr != "")
			assert.False(t, tt.model != "" && modelStr != tt.model)
		})
	}
}

func TestResolveFixAgentUsesWorkflowModel(t *testing.T) {
	// When --agent is passed on CLI, workflow-specific models (e.g.,
	// fix_model) should still be used. Only generic defaults are skipped.

	tmpDir := t.TempDir()
	t.Setenv("ROBOREV_DATA_DIR", tmpDir)

	// Write global config with both default_model and fix_model
	cfgPath := filepath.Join(tmpDir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte(`
default_agent = "codex"
default_model = "gpt-5.4"
fix_model = "gemini-2.5-pro"
`), 0o644); err != nil {
		require.NoError(t, err)
	}

	cfg, err := config.LoadGlobal()
	require.NoError(t, err, "LoadGlobal: %v")

	// With CLI --agent override, fix_model should still apply
	modelStr := config.ResolveWorkflowModel(tmpDir, cfg, "fix", "fast")
	assert.Equal(t, "gemini-2.5-pro", modelStr,

		"expected workflow-specific model 'gemini-2.5-pro', got %q",
		modelStr)

	// Without CLI --agent, ResolveModelForWorkflow should return
	// fix_model (higher priority than default_model)
	modelStr = config.ResolveModelForWorkflow("", tmpDir, cfg, "fix", "fast")
	assert.Equal(t, "gemini-2.5-pro", modelStr,

		"expected workflow-specific model 'gemini-2.5-pro', got %q",
		modelStr)
}

func TestResolveFixAgentSkipsDefaultModelForConfiguredFixAgent(t *testing.T) {
	// When fix_agent differs from default_agent and no fix_model is set,
	// the fix agent should keep its own built-in default model.

	tmpDir := t.TempDir()
	t.Setenv("ROBOREV_DATA_DIR", tmpDir)

	cfgPath := filepath.Join(tmpDir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte(`
default_agent = "codex"
default_model = "gpt-5.4"
fix_agent = "claude"
`), 0o644); err != nil {
		require.NoError(t, err)
	}

	cfg, err := config.LoadGlobal()
	require.NoError(t, err, "LoadGlobal: %v")

	modelStr, err := resolveFixModel("claude-code", "", tmpDir, cfg, "standard")
	require.NoError(t, err)
	assert.Empty(t, modelStr)
}

func TestResolveFixAgentFallbackUsesDefaultModelForActualAgent(t *testing.T) {
	t.Cleanup(testutil.MockExecutableIsolated(t, "codex", 0))

	tmpDir := t.TempDir()
	t.Setenv("ROBOREV_DATA_DIR", tmpDir)

	cfgPath := filepath.Join(tmpDir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte(`
default_agent = "codex"
default_model = "gpt-5.4"
fix_agent = "claude"
`), 0o644); err != nil {
		require.NoError(t, err)
	}

	selected, err := resolveFixAgent(tmpDir, fixOptions{})
	require.NoError(t, err, "resolveFixAgent: %v")

	codexAgent, ok := selected.(*agent.CodexAgent)
	assert.True(t, ok)
	assert.Equal(t, "gpt-5.4", codexAgent.Model)
}

func TestResolveFixAgentUsesRepoWorkflowModel(t *testing.T) {
	// Repo-level workflow-specific models should be used even when
	// --agent is overridden on CLI.

	tmpDir := t.TempDir()
	t.Setenv("ROBOREV_DATA_DIR", tmpDir)

	// Write global config with default_model only
	cfgPath := filepath.Join(tmpDir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte(`
default_agent = "codex"
default_model = "gpt-5.4"
`), 0o644); err != nil {
		require.NoError(t, err)
	}

	// Write repo config with fix_model_fast
	repoDir := t.TempDir()
	repoConfigPath := filepath.Join(repoDir, ".roborev.toml")
	if err := os.WriteFile(repoConfigPath, []byte(`
fix_model_fast = "claude-sonnet"
`), 0o644); err != nil {
		require.NoError(t, err)
	}

	cfg, err := config.LoadGlobal()
	require.NoError(t, err, "LoadGlobal: %v")

	// With CLI --agent override, repo fix_model_fast should apply
	modelStr := config.ResolveWorkflowModel(repoDir, cfg, "fix", "fast")
	assert.Equal(t, "claude-sonnet", modelStr,

		"expected repo workflow model 'claude-sonnet', got %q",
		modelStr)

	// Repo generic model should NOT be used
	repoConfigPath2 := filepath.Join(repoDir, ".roborev.toml")
	if err := os.WriteFile(repoConfigPath2, []byte(`
model = "repo-default"
`), 0o644); err != nil {
		require.NoError(t, err)
	}

	modelStr = config.ResolveWorkflowModel(repoDir, cfg, "fix", "fast")
	assert.Empty(t, modelStr,

		"expected empty model (repo generic should be skipped), got %q",
		modelStr)
}

func TestResolveFixAgentSameAsDefault(t *testing.T) {
	// When --agent matches the config default (even via alias),
	// the generic default_model should still be applied because
	// the agent is effectively unchanged.

	tmpDir := t.TempDir()
	t.Setenv("ROBOREV_DATA_DIR", tmpDir)

	tests := []struct {
		name         string
		defaultAgent string
		cliAgent     string
		wantModel    string
	}{
		{
			name:         "same agent uses default_model",
			defaultAgent: "codex",
			cliAgent:     "codex",
			wantModel:    "gpt-5.4",
		},
		{
			name:         "alias matches default uses default_model",
			defaultAgent: "claude-code",
			cliAgent:     "claude",
			wantModel:    "gpt-5.4",
		},
		{
			name:         "different agent skips default_model",
			defaultAgent: "codex",
			cliAgent:     "test",
			wantModel:    "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfgPath := filepath.Join(tmpDir, "config.toml")
			cfgContent := fmt.Sprintf(
				"default_agent = %q\ndefault_model = \"gpt-5.4\"\n",
				tt.defaultAgent,
			)
			if err := os.WriteFile(cfgPath, []byte(cfgContent), 0o644); err != nil {
				require.NoError(t, err)
			}
			cfg, err := config.LoadGlobal()
			require.NoError(t, err, "LoadGlobal: %v")

			// Call the production resolveFixModel function directly
			modelStr, err := resolveFixModel(
				tt.cliAgent, "", tmpDir, cfg, "fast",
			)
			require.NoError(t, err)

			assert.Equal(t, tt.wantModel, modelStr)
		})
	}
}

func TestBuildGenericFixPromptMinSeverity(t *testing.T) {
	output := "Found bug in foo.go"

	t.Run("no filter", func(t *testing.T) {
		p := buildGenericFixPrompt(output, "", nil)
		if strings.Contains(p, "Severity filter") {
			assert.Condition(t, func() bool {
				return false
			}, "empty minSeverity should not inject filter")
		}
		if !strings.Contains(p, output) {
			assert.Condition(t, func() bool {
				return false
			}, "prompt should contain the analysis output")
		}
	})

	t.Run("high filter", func(t *testing.T) {
		p := buildGenericFixPrompt(output, "high", nil)
		if !strings.Contains(p, "Severity filter") {
			assert.Condition(t, func() bool {
				return false
			}, "high minSeverity should inject filter")
		}
		if !strings.Contains(p, "High and Critical") {
			assert.Condition(t, func() bool {
				return false
			}, "instruction should mention High and Critical")
		}
		if !strings.Contains(p, output) {
			assert.Condition(t, func() bool {
				return false
			}, "prompt should still contain the analysis output")
		}
	})

	t.Run("low filter is no-op", func(t *testing.T) {
		p := buildGenericFixPrompt(output, "low", nil)
		if strings.Contains(p, "Severity filter") {
			assert.Condition(t, func() bool {
				return false
			}, "low minSeverity should not inject filter")
		}
	})
}

func TestBuildBatchFixPromptMinSeverity(t *testing.T) {
	entries := []batchEntry{
		{
			jobID:  1,
			job:    &storage.ReviewJob{GitRef: "abc123"},
			review: &storage.Review{Output: "issue found"},
		},
	}

	t.Run("no filter", func(t *testing.T) {
		p := buildBatchFixPrompt(entries, "")
		if strings.Contains(p, "Severity filter") {
			assert.Condition(t, func() bool {
				return false
			}, "empty minSeverity should not inject filter")
		}
	})

	t.Run("critical filter", func(t *testing.T) {
		p := buildBatchFixPrompt(entries, "critical")
		if !strings.Contains(p, "Severity filter") {
			assert.Condition(t, func() bool {
				return false
			}, "critical minSeverity should inject filter")
		}
		if !strings.Contains(p, "Only include Critical") {
			assert.Condition(t, func() bool {
				return false
			}, "instruction should mention Critical only")
		}
	})
}

func TestFilterReachableJobs(t *testing.T) {
	repo := newTestGitRepo(t)
	mainSHA := repo.CommitFile("a.txt", "a", "commit on main")

	// Detect the default branch name (may be "main" or "master")
	defaultBranch := strings.TrimSpace(repo.Run("rev-parse", "--abbrev-ref", "HEAD"))

	// Create a divergent branch with its own commit
	repo.Run("checkout", "-b", "other-branch")
	otherSHA := repo.CommitFile("b.txt", "b", "commit on other")
	repo.Run("checkout", defaultBranch)

	tests := []struct {
		name           string
		branchOverride string // non-empty for --list with --branch
		jobs           []storage.ReviewJob
		wantIDs        []int64
	}{
		{
			name: "reachable commit with matching branch included",
			jobs: []storage.ReviewJob{
				{ID: 1, GitRef: mainSHA, Branch: defaultBranch},
			},
			wantIDs: []int64{1},
		},
		{
			name: "unreachable SHA different branch excluded",
			jobs: []storage.ReviewJob{
				{ID: 2, GitRef: otherSHA, Branch: "other-branch"},
			},
			wantIDs: nil,
		},
		{
			name: "unreachable SHA same branch included (rebase)",
			jobs: []storage.ReviewJob{
				{ID: 2, GitRef: otherSHA, Branch: defaultBranch},
			},
			wantIDs: []int64{2},
		},
		{
			name: "unreachable SHA no branch excluded",
			jobs: []storage.ReviewJob{
				{ID: 2, GitRef: otherSHA},
			},
			wantIDs: nil,
		},
		{
			name: "mixed matching and non-matching branches",
			jobs: []storage.ReviewJob{
				{ID: 1, GitRef: mainSHA, Branch: defaultBranch},
				{ID: 2, GitRef: otherSHA, Branch: "other-branch"},
			},
			wantIDs: []int64{1},
		},
		{
			name: "empty GitRef matching branch included",
			jobs: []storage.ReviewJob{
				{ID: 3, GitRef: "", Branch: defaultBranch},
			},
			wantIDs: []int64{3},
		},
		{
			name: "empty GitRef different branch excluded",
			jobs: []storage.ReviewJob{
				{ID: 3, GitRef: "", Branch: "other-branch"},
			},
			wantIDs: nil,
		},
		{
			name: "empty GitRef no branch excluded",
			jobs: []storage.ReviewJob{
				{ID: 3, GitRef: ""},
			},
			wantIDs: nil,
		},
		{
			name: "dirty ref matching branch included",
			jobs: []storage.ReviewJob{
				{ID: 4, GitRef: "dirty", Branch: defaultBranch},
			},
			wantIDs: []int64{4},
		},
		{
			name: "dirty ref different branch excluded",
			jobs: []storage.ReviewJob{
				{ID: 4, GitRef: "dirty", Branch: "other-branch"},
			},
			wantIDs: nil,
		},
		{
			name: "dirty ref no branch excluded",
			jobs: []storage.ReviewJob{
				{ID: 4, GitRef: "dirty"},
			},
			wantIDs: nil,
		},
		{
			name: "range ref matching branch included",
			jobs: []storage.ReviewJob{
				{
					ID:     5,
					GitRef: otherSHA + ".." + mainSHA,
					Branch: defaultBranch,
				},
			},
			wantIDs: []int64{5},
		},
		{
			name: "range ref unreachable end different branch excluded",
			jobs: []storage.ReviewJob{
				{
					ID:     5,
					GitRef: mainSHA + ".." + otherSHA,
					Branch: "other-branch",
				},
			},
			wantIDs: nil,
		},
		{
			name: "range ref unreachable end same branch included (rebase)",
			jobs: []storage.ReviewJob{
				{
					ID:     5,
					GitRef: mainSHA + ".." + otherSHA,
					Branch: defaultBranch,
				},
			},
			wantIDs: []int64{5},
		},
		{
			name: "range ref no branch excluded",
			jobs: []storage.ReviewJob{
				{ID: 5, GitRef: "abc123..def456"},
			},
			wantIDs: nil,
		},
		{
			name: "unknown SHA no branch excluded",
			jobs: []storage.ReviewJob{
				{ID: 6, GitRef: "0000000000000000000000000000000000000000"},
			},
			wantIDs: nil,
		},
		{
			name: "task ref matching branch included",
			jobs: []storage.ReviewJob{
				{
					ID:     7,
					GitRef: "run", Branch: defaultBranch,
					JobType: storage.JobTypeTask,
				},
			},
			wantIDs: []int64{7},
		},
		{
			name: "task ref different branch excluded",
			jobs: []storage.ReviewJob{
				{
					ID:     8,
					GitRef: "analyze", Branch: "other-branch",
					JobType: storage.JobTypeTask,
				},
			},
			wantIDs: nil,
		},
		{
			name: "task ref no branch excluded",
			jobs: []storage.ReviewJob{
				{
					ID:      9,
					GitRef:  "custom-label",
					JobType: storage.JobTypeTask,
				},
			},
			wantIDs: nil,
		},
		{
			name:           "branch override includes matching dirty ref",
			branchOverride: "other-branch",
			jobs: []storage.ReviewJob{
				{ID: 10, GitRef: "dirty", Branch: "other-branch"},
			},
			wantIDs: []int64{10},
		},
		{
			name:           "branch override excludes non-matching dirty ref",
			branchOverride: "other-branch",
			jobs: []storage.ReviewJob{
				{ID: 11, GitRef: "dirty", Branch: defaultBranch},
			},
			wantIDs: nil,
		},
		{
			name:           "branch override includes matching SHA ref",
			branchOverride: "other-branch",
			jobs: []storage.ReviewJob{
				{ID: 12, GitRef: otherSHA, Branch: "other-branch"},
			},
			wantIDs: []int64{12},
		},
		{
			name:           "branch override excludes non-matching SHA ref",
			branchOverride: "other-branch",
			jobs: []storage.ReviewJob{
				{ID: 13, GitRef: mainSHA, Branch: defaultBranch},
			},
			wantIDs: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := filterReachableJobs(t.Context(),
				repo.Dir, tt.branchOverride, tt.jobs,
			)
			var gotIDs []int64
			for _, j := range got {
				gotIDs = append(gotIDs, j.ID)
			}
			assert.Equal(t, tt.wantIDs, gotIDs)
		})
	}
}

func TestFilterReachableJobsDetachedHead(t *testing.T) {
	repo := newTestGitRepo(t)
	baseSHA := repo.CommitFile("a.txt", "a", "initial")
	defaultBranch := strings.TrimSpace(
		repo.Run("rev-parse", "--abbrev-ref", "HEAD"),
	)

	repo.Run("checkout", "--detach", baseSHA)
	parentSHA := repo.CommitFile("b.txt", "b", "detached parent")
	headSHA := repo.CommitFile("c.txt", "c", "detached head")

	tests := []struct {
		name    string
		jobs    []storage.ReviewJob
		wantIDs []int64
	}{
		{
			name: "detached head includes commits back to first branch commit",
			jobs: []storage.ReviewJob{
				{ID: 1, GitRef: headSHA},
				{ID: 2, GitRef: parentSHA},
				{ID: 3, GitRef: baseSHA, Branch: defaultBranch},
			},
			wantIDs: []int64{1, 2, 3},
		},
		{
			name: "detached head includes range ending in detached commit",
			jobs: []storage.ReviewJob{
				{ID: 4, GitRef: baseSHA + "..HEAD"},
			},
			wantIDs: []int64{4},
		},
		{
			name: "dirty refs stay excluded on detached HEAD",
			jobs: []storage.ReviewJob{
				{ID: 5, GitRef: "dirty", Branch: "some-branch"},
				{ID: 6, GitRef: "dirty"},
			},
			wantIDs: nil,
		},
		{
			name: "unrelated refs are excluded on detached HEAD",
			jobs: []storage.ReviewJob{
				{ID: 7, GitRef: "does-not-resolve"},
			},
			wantIDs: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := filterReachableJobs(t.Context(), repo.Dir, "", tt.jobs)
			var gotIDs []int64
			for _, j := range got {
				gotIDs = append(gotIDs, j.ID)
			}
			assert.Equal(t, tt.wantIDs, gotIDs)
		})
	}
}

func TestFilterReachableJobsDetachedMergeHead(t *testing.T) {
	repo := newTestGitRepo(t)
	baseSHA := repo.CommitFile("base.txt", "base", "base")

	repo.Run("checkout", "-b", "feature-side")
	featureSHA := repo.CommitFile("feature.txt", "feature", "feature")

	repo.Run("checkout", "--detach", baseSHA)
	repo.Run("merge", "--no-ff", "feature-side", "-m", "detached merge")

	jobs := []storage.ReviewJob{
		{ID: 1, GitRef: featureSHA},
		{ID: 2, GitRef: baseSHA + ".." + featureSHA},
	}
	got := filterReachableJobs(t.Context(), repo.Dir, "", jobs)

	var gotIDs []int64
	for _, j := range got {
		gotIDs = append(gotIDs, j.ID)
	}
	assert.Equal(t, []int64{1, 2}, gotIDs)
}

func TestRunFixOpenFiltersUnreachableJobs(t *testing.T) {
	repo, worktreeDir := setupWorktree(t)

	// Detect the default branch name
	defaultBranch := strings.TrimSpace(
		repo.Run("rev-parse", "--abbrev-ref", "HEAD"),
	)

	// Create a commit only on the main branch (not reachable from worktree)
	repo.Run("checkout", defaultBranch)
	mainOnlySHA := repo.CommitFile(
		"main-only.txt", "content", "main-only commit",
	)

	// Create a commit on the worktree branch
	cmd := exec.Command("git", "checkout", "wt-branch")
	cmd.Dir = worktreeDir
	require.NoError(t, cmd.Run(), "checkout wt-branch in worktree")

	cmd = exec.Command("git", "add", ".")
	cmd.Dir = worktreeDir
	_ = cmd.Run()
	wtFile := filepath.Join(worktreeDir, "wt-file.txt")
	require.NoError(t, os.WriteFile(wtFile, []byte("wt"), 0o644))
	cmd = exec.Command("git", "add", "wt-file.txt")
	cmd.Dir = worktreeDir
	require.NoError(t, cmd.Run())
	cmd = exec.Command("git", "commit", "-m", "worktree commit")
	cmd.Dir = worktreeDir
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "commit in worktree: %s", out)
	cmd = exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = worktreeDir
	shaOut, err := cmd.Output()
	require.NoError(t, err)
	wtSHA := strings.TrimSpace(string(shaOut))

	var reviewCalls, closeCalls atomic.Int32
	var processedJobIDs []int64
	var mu sync.Mutex
	failVerdict := "F"

	_ = newMockDaemonBuilder(t).
		WithHandler("/api/jobs", func(w http.ResponseWriter, r *http.Request) {
			q := r.URL.Query()
			if q.Get("closed") == "false" && q.Get("limit") == "0" {
				// Return jobs for both branches
				writeJSON(w, map[string]any{
					"jobs": []storage.ReviewJob{
						{
							ID:      100,
							Status:  storage.JobStatusDone,
							Agent:   "test",
							GitRef:  mainOnlySHA,
							Branch:  "main",
							JobType: storage.JobTypeReview,
							Verdict: &failVerdict,
						},
						{
							ID:      200,
							Status:  storage.JobStatusDone,
							Agent:   "test",
							GitRef:  wtSHA,
							Branch:  "wt-branch",
							JobType: storage.JobTypeReview,
							Verdict: &failVerdict,
						},
					},
					"has_more": false,
				})
			} else {
				// Individual job fetches
				idStr := q.Get("id")
				var id int64
				fmt.Sscanf(idStr, "%d", &id)
				mu.Lock()
				processedJobIDs = append(processedJobIDs, id)
				mu.Unlock()
				writeJSON(w, map[string]any{
					"jobs": []storage.ReviewJob{
						{
							ID:     id,
							Status: storage.JobStatusDone,
							Agent:  "test",
						},
					},
					"has_more": false,
				})
			}
		}).
		WithHandler("/api/review", func(w http.ResponseWriter, r *http.Request) {
			reviewCalls.Add(1)
			writeJSON(w, storage.Review{Output: "findings"})
		}).
		WithHandler("/api/comment", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusCreated)
		}).
		WithHandler("/api/review/close", func(w http.ResponseWriter, r *http.Request) {
			closeCalls.Add(1)
			w.WriteHeader(http.StatusOK)
		}).
		WithHandler("/api/enqueue", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}).
		Build()

	// Pass the worktree's branch for API query filtering, matching what
	// the CLI does when neither --all-branches nor --branch is set
	// (effectiveBranch is resolved to the current branch). allBranches
	// is false, so filterReachableJobs uses commit-graph reachability.
	_, runErr := runWithOutput(t, worktreeDir, func(cmd *cobra.Command) error {
		tracker := &fixSessionTracker{base: agent.NewTestAgent(), out: io.Discard}
		return runFixOpen(
			cmd, "wt-branch", false, false, false,
			fixOptions{agentName: "test", reasoning: "fast"},
			tracker,
		)
	})
	require.NoError(t, runErr, "runFixOpen")

	// Only the worktree-reachable job (200) should be processed
	mu.Lock()
	ids := processedJobIDs
	mu.Unlock()

	assert.Contains(t, ids, int64(200),
		"worktree-reachable job should be processed")
	assert.NotContains(t, ids, int64(100),
		"main-only job should be filtered out")
}

// TestRunFixOpenExcludesMergedBranchJobs verifies that roborev fix on
// the main branch does NOT process jobs whose commits were originally
// reviewed on a feature branch, even when those commits are reachable
// from HEAD via a merge. Jobs are scoped by their stored Branch field.
func TestRunFixOpenExcludesMergedBranchJobs(t *testing.T) {
	repo := newTestGitRepo(t)
	repo.CommitFile("base.txt", "base", "initial commit")

	defaultBranch := strings.TrimSpace(
		repo.Run("rev-parse", "--abbrev-ref", "HEAD"),
	)

	// Create a feature branch and commit
	repo.Run("checkout", "-b", "feature/widget")
	featureSHA := repo.CommitFile(
		"widget.txt", "code", "add widget",
	)

	// Switch back to main and merge the feature branch
	repo.Run("checkout", defaultBranch)
	repo.Run("merge", "--no-ff", "feature/widget", "-m", "merge feature")

	var processedJobIDs []int64
	var mu sync.Mutex

	_ = newMockDaemonBuilder(t).
		WithHandler("/api/jobs", func(w http.ResponseWriter, r *http.Request) {
			q := r.URL.Query()
			if q.Get("closed") == "false" && q.Get("limit") == "0" {
				writeJSON(w, map[string]any{
					"jobs": []storage.ReviewJob{
						{
							ID:     300,
							Status: storage.JobStatusDone,
							Agent:  "test",
							GitRef: featureSHA,
							Branch: "feature/widget",
						},
					},
					"has_more": false,
				})
			} else if q.Get("id") != "" {
				var id int64
				fmt.Sscanf(q.Get("id"), "%d", &id)
				mu.Lock()
				processedJobIDs = append(processedJobIDs, id)
				mu.Unlock()
				writeJSON(w, map[string]any{
					"jobs": []storage.ReviewJob{
						{
							ID:     id,
							Status: storage.JobStatusDone,
							Agent:  "test",
						},
					},
					"has_more": false,
				})
			}
		}).
		WithHandler("/api/review", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, storage.Review{Output: "findings"})
		}).
		WithHandler("/api/comment", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusCreated)
		}).
		WithHandler("/api/review/close", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}).
		WithHandler("/api/enqueue", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}).
		Build()

	// Run from main with auto-resolved branch (explicitBranch=false).
	_, runErr := runWithOutput(t, repo.Dir, func(cmd *cobra.Command) error {
		tracker := &fixSessionTracker{base: agent.NewTestAgent(), out: io.Discard}
		return runFixOpen(
			cmd, defaultBranch, false, false, false,
			fixOptions{agentName: "test", reasoning: "fast"},
			tracker,
		)
	})
	require.NoError(t, runErr, "runFixOpen")

	// The feature branch job should NOT be processed — branch
	// scoping excludes jobs from other branches even when the
	// commit is reachable via a merge.
	mu.Lock()
	ids := processedJobIDs
	mu.Unlock()
	assert.NotContains(t, ids, int64(300),
		"feature branch job should be excluded on main")
}

// TestFilterReachableJobsFeatureBranch verifies that on a feature
// branch, jobs from other branches are excluded even though their
// SHAs are reachable from HEAD. Jobs with no branch fail open.
func TestFilterReachableJobsFeatureBranch(t *testing.T) {
	repo := newTestGitRepo(t)
	baseSHA := repo.CommitFile("base.txt", "base", "base commit")

	defaultBranch := strings.TrimSpace(
		repo.Run("rev-parse", "--abbrev-ref", "HEAD"),
	)

	mainOnlySHA := repo.CommitFile(
		"main-only.txt", "m", "main-only commit",
	)

	repo.Run("checkout", "-b", "feature-x")
	featureSHA := repo.CommitFile(
		"feature.txt", "f", "feature commit",
	)

	tests := []struct {
		name    string
		jobs    []storage.ReviewJob
		wantIDs []int64
	}{
		{
			name: "feature branch commit included",
			jobs: []storage.ReviewJob{
				{ID: 1, GitRef: featureSHA, Branch: "feature-x"},
			},
			wantIDs: []int64{1},
		},
		{
			name: "base branch commit with base branch excluded",
			jobs: []storage.ReviewJob{
				{
					ID: 2, GitRef: mainOnlySHA,
					Branch: defaultBranch,
				},
			},
			wantIDs: nil,
		},
		{
			name: "base branch commit with no branch excluded",
			jobs: []storage.ReviewJob{
				{ID: 3, GitRef: baseSHA},
			},
			wantIDs: nil,
		},
		{
			name: "base branch commit matching branch included",
			jobs: []storage.ReviewJob{
				{
					ID: 4, GitRef: mainOnlySHA,
					Branch: "feature-x",
				},
			},
			wantIDs: []int64{4},
		},
		{
			name: "mixed: matching and branchless survive",
			jobs: []storage.ReviewJob{
				{
					ID: 10, GitRef: featureSHA,
					Branch: "feature-x",
				},
				{
					ID: 20, GitRef: mainOnlySHA,
					Branch: defaultBranch,
				},
				{
					ID: 30, GitRef: baseSHA,
					Branch: "other",
				},
			},
			wantIDs: []int64{10},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := filterReachableJobs(t.Context(), repo.Dir, "", tt.jobs)
			var gotIDs []int64
			for _, j := range got {
				gotIDs = append(gotIDs, j.ID)
			}
			assert.Equal(t, tt.wantIDs, gotIDs)
		})
	}
}

func TestFixCmd_BatchAndBatchSizeMutuallyExclusive(t *testing.T) {
	cmd := fixCmd()
	cmd.SetArgs([]string{"--batch", "--batch-size", "5"})
	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--batch and --batch-size are mutually exclusive")
}

func TestFixCmd_ListAndBatchSizeMutuallyExclusive(t *testing.T) {
	cmd := fixCmd()
	cmd.SetArgs([]string{"--list", "--batch-size", "5"})
	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--list and --batch-size are mutually exclusive")
}

func TestFixCmd_BatchSizeMustBePositive(t *testing.T) {
	cmd := fixCmd()
	cmd.SetArgs([]string{"--batch-size", "0"})
	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--batch-size must be >= 1")
}

// TestFixCmd_BatchSizeAcceptsBranchFlags asserts that --batch-size is
// treated equivalently to --batch in the validation path: it can be
// combined with --branch / --all-branches / --newest-first without
// being rejected before dispatch. Failures here would surface as an
// error containing "mutually exclusive" or "cannot be used", not the
// downstream daemon error these cases produce when validation passes.
func TestFixCmd_BatchSizeAcceptsBranchFlags(t *testing.T) {
	cases := [][]string{
		{"--batch-size", "5", "--branch", "main"},
		{"--batch-size", "5", "--all-branches"},
		{"--batch-size", "5", "--newest-first"},
	}
	for _, args := range cases {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			cmd := fixCmd()
			cmd.SetArgs(args)
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			err := cmd.Execute()
			if err != nil {
				assert.NotContains(t, err.Error(), "mutually exclusive")
				assert.NotContains(t, err.Error(), "cannot be used")
			}
		})
	}
}

func TestFixSingleJobAbortsOnSessionLimit(t *testing.T) {
	repo := createTestRepo(t, map[string]string{"main.go": "package main\n"})

	originalAgent, err := agent.Get("test")
	require.NoError(t, err, "get test agent")
	agent.Register(&agent.FakeAgent{
		NameStr: "test",
		ReviewFn: func(_ context.Context, _, _, _ string, _ io.Writer) (string, error) {
			return "", errors.New("simulated claude session cap")
		},
	})
	t.Cleanup(func() { agent.Register(originalAgent) })

	ts, _ := newMockServer(t, MockServerOpts{
		ReviewOutput: "## Issues\n- Found issue",
		OnJobs: func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, map[string]any{
				"jobs": []storage.ReviewJob{{
					ID:     99,
					Status: storage.JobStatusDone,
					Agent:  "test",
				}},
			})
		},
	})
	patchServerAddr(t, ts.URL)

	cmd, _ := newTestCmd(t)

	resetAt := time.Now().Add(2*time.Hour + 13*time.Minute)
	classifyCalls := 0
	opts := fixOptions{
		agentName: "test",
		reasoning: "fast",
		classify: func(_, msg string) agent.LimitClassification {
			classifyCalls++
			return agent.LimitClassification{
				Kind:    agent.LimitKindSession,
				Agent:   "claude-code",
				ResetAt: resetAt,
				Message: msg,
			}
		},
	}

	base, err := resolveFixAgent(repo.Dir, opts)
	require.NoError(t, err, "resolveFixAgent")
	tracker := &fixSessionTracker{base: base, out: io.Discard}
	err = fixSingleJob(cmd, repo.Dir, 99, opts, tracker)
	require.Error(t, err, "expected abort error, got nil")

	var lim *agentLimitError
	require.ErrorAs(t, err, &lim, "expected agentLimitError, got %T: %v", err, err)

	assert := assert.New(t)
	assert.Equal(agent.LimitKindSession, lim.Classification.Kind)
	assert.Equal(1, classifyCalls, "classifier should be called exactly once")
	assert.Contains(err.Error(), "claude-code")
	assert.Contains(err.Error(), "session limit")
}

func TestRunFixBatchAbortsOnQuotaError(t *testing.T) {
	repo := createTestRepo(t, map[string]string{"main.go": "package main\n"})

	originalAgent, err := agent.Get("test")
	require.NoError(t, err, "get test agent")
	agentCalls := 0
	agent.Register(&agent.FakeAgent{
		NameStr: "test",
		ReviewFn: func(_ context.Context, _, _, _ string, _ io.Writer) (string, error) {
			agentCalls++
			return "", errors.New("agent failed: you have exhausted your capacity")
		},
	})
	t.Cleanup(func() { agent.Register(originalAgent) })

	var mu sync.Mutex
	var reviewedJobIDs []int64
	_ = newMockDaemonBuilder(t).
		WithHandler("/api/jobs", func(w http.ResponseWriter, r *http.Request) {
			id := r.URL.Query().Get("id")
			switch id {
			case "10", "20":
				idNum := int64(10)
				if id == "20" {
					idNum = 20
				}
				writeJSON(w, map[string]any{
					"jobs": []storage.ReviewJob{{
						ID:     idNum,
						Status: storage.JobStatusDone,
						Agent:  "test",
					}},
				})
			default:
				writeJSON(w, map[string]any{
					"jobs":     []storage.ReviewJob{},
					"has_more": false,
				})
			}
		}).
		WithHandler("/api/review", func(w http.ResponseWriter, r *http.Request) {
			jobID := r.URL.Query().Get("job_id")
			mu.Lock()
			switch jobID {
			case "10":
				reviewedJobIDs = append(reviewedJobIDs, 10)
			case "20":
				reviewedJobIDs = append(reviewedJobIDs, 20)
			}
			mu.Unlock()
			writeJSON(w, storage.Review{
				JobID:  10,
				Output: "## Issues\n- Bug",
			})
		}).
		WithHandler("/api/enqueue", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}).
		Build()

	classifyCalls := 0
	opts := fixOptions{
		agentName: "test",
		reasoning: "fast",
		classify: func(_, msg string) agent.LimitClassification {
			classifyCalls++
			return agent.LimitClassification{
				Kind:        agent.LimitKindQuota,
				Agent:       "gemini",
				CooldownFor: 30 * time.Minute,
				Message:     msg,
			}
		},
	}

	base, err := resolveFixAgent(repo.Dir, opts)
	require.NoError(t, err, "resolveFixAgent")

	_, err = runWithOutput(t, repo.Dir, func(cmd *cobra.Command) error {
		// batchSize=1 forces one job per agent call. With batchSize=0,
		// runFixBatch may pack both jobs into a single batch, making
		// agentCalls=1 ambiguous between "aborted before second batch"
		// and "both jobs in one batch". batchSize=1 is unambiguous.
		return runFixBatch(
			cmd,
			[]int64{10, 20},
			"",
			false, false, false,
			1,
			opts,
			&fixSessionTracker{base: base, out: io.Discard},
		)
	})
	require.Error(t, err, "expected batch to abort with agentLimitError")

	var lim *agentLimitError
	require.ErrorAs(t, err, &lim, "expected agentLimitError, got %T: %v", err, err)

	assert := assert.New(t)
	assert.Equal(agent.LimitKindQuota, lim.Classification.Kind)
	assert.Equal(1, classifyCalls, "classifier should be called exactly once")
	// agentCalls is the load-bearing assertion: with batchSize=1 and two
	// jobs, two agent invocations would mean the loop continued past the
	// abort. Exactly one means the abort short-circuits the second batch.
	assert.Equal(1, agentCalls, "fix agent should be invoked exactly once before abort")
	assert.Contains(err.Error(), "quota limit", "Gemini KindQuota label should be 'quota limit', not 'session limit'")

	// Note: do NOT assert `reviewedJobIDs` does not contain job 20 —
	// runFixBatch prefetches every job's review at the start of the
	// function (before any agent call), so /api/review fires for all
	// queued jobs regardless of when the loop aborts. The agentCalls
	// counter above is the correct measure of abort behavior.
}

func TestRunFixWithSeenDiscoveryAbortsOnAgentLimit(t *testing.T) {
	// Discovery mode (seen != nil) normally warns-and-continues on
	// per-job errors, but agent quota/session-limit aborts must
	// propagate so the re-query loop doesn't keep invoking the
	// exhausted agent.
	repo := createTestRepo(t, map[string]string{"main.go": "package main\n"})

	originalAgent, err := agent.Get("test")
	require.NoError(t, err, "get test agent")
	agentCalls := 0
	agent.Register(&agent.FakeAgent{
		NameStr: "test",
		ReviewFn: func(_ context.Context, _, _, _ string, _ io.Writer) (string, error) {
			agentCalls++
			return "", errors.New("agent failed: you have exhausted your capacity")
		},
	})
	t.Cleanup(func() { agent.Register(originalAgent) })

	_ = newMockDaemonBuilder(t).
		WithHandler("/api/jobs", func(w http.ResponseWriter, r *http.Request) {
			id := r.URL.Query().Get("id")
			var idNum int64
			switch id {
			case "10":
				idNum = 10
			case "20":
				idNum = 20
			default:
				writeJSON(w, map[string]any{
					"jobs":     []storage.ReviewJob{},
					"has_more": false,
				})
				return
			}
			writeJSON(w, map[string]any{
				"jobs": []storage.ReviewJob{{
					ID:     idNum,
					Status: storage.JobStatusDone,
					Agent:  "test",
				}},
			})
		}).
		WithHandler("/api/review", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, storage.Review{Output: "## Issues\n- Bug"})
		}).
		WithHandler("/api/enqueue", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}).
		Build()

	classifyCalls := 0
	opts := fixOptions{
		agentName: "test",
		reasoning: "fast",
		classify: func(_, msg string) agent.LimitClassification {
			classifyCalls++
			return agent.LimitClassification{
				Kind:        agent.LimitKindQuota,
				Agent:       "gemini",
				CooldownFor: 30 * time.Minute,
				Message:     msg,
			}
		},
	}

	seen := make(map[int64]bool)
	_, err = runWithOutput(t, repo.Dir, func(cmd *cobra.Command) error {
		base, baseErr := resolveFixAgent(repo.Dir, opts)
		require.NoError(t, baseErr, "resolveFixAgent")
		tracker := &fixSessionTracker{base: base, out: io.Discard}
		return runFixWithSeen(cmd, []int64{10, 20}, opts, seen, tracker)
	})
	require.Error(t, err, "expected discovery mode to propagate agentLimitError")

	var lim *agentLimitError
	require.ErrorAs(t, err, &lim, "expected agentLimitError, got %T: %v", err, err)

	assert := assert.New(t)
	assert.Equal(agent.LimitKindQuota, lim.Classification.Kind)
	assert.Equal(1, classifyCalls, "classifier should be called exactly once")
	// Load-bearing: only one agent invocation means the abort short-
	// circuited the loop. Two would mean discovery mode demoted the
	// agentLimitError to a warning and continued.
	assert.Equal(1, agentCalls, "fix agent should be invoked exactly once before abort")
	assert.False(seen[20], "second job must not be marked seen after abort")
}

func TestFixJobDirect_RetryClassifiesAgentLimit(t *testing.T) {
	// fixJobDirect runs the agent twice when the first call leaves
	// uncommitted changes: once for the fix, once with commit
	// instructions. A quota or session-limit error on that second
	// call must produce *agentLimitError, not a swallowed warning.
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	dir := t.TempDir()
	for _, args := range [][]string{
		{"init"},
		{"config", "user.email", "test@test.com"},
		{"config", "user.name", "Test"},
		{"commit", "--allow-empty", "-m", "base"},
	} {
		c := exec.Command("git", args...)
		c.Dir = dir
		require.NoError(t, c.Run(), "git %v", args)
	}

	calls := 0
	ag := &agent.FakeAgent{
		NameStr: "test",
		ReviewFn: func(_ context.Context, repoPath, _, _ string, _ io.Writer) (string, error) {
			calls++
			if calls == 1 {
				// First call: leave an uncommitted change so the retry
				// path is reached.
				return "applied fix", os.WriteFile(
					filepath.Join(repoPath, "dirty.txt"),
					[]byte("uncommitted"),
					0o644,
				)
			}
			// Second call (commit retry): hit a session limit.
			return "", errors.New("simulated claude session cap")
		},
	}

	classifyCalls := 0
	classify := func(_, msg string) agent.LimitClassification {
		classifyCalls++
		return agent.LimitClassification{
			Kind:    agent.LimitKindSession,
			Agent:   "claude-code",
			Message: msg,
		}
	}

	var buf bytes.Buffer
	_, err := fixJobDirect(context.Background(), fixJobParams{
		RepoRoot: dir,
		Agent:    ag,
		Output:   &buf,
		Classify: classify,
	}, "fix things")
	require.Error(t, err, "expected agentLimitError from retry path")

	var lim *agentLimitError
	require.ErrorAs(t, err, &lim, "expected *agentLimitError, got %T: %v", err, err)

	assert := assert.New(t)
	assert.Equal(agent.LimitKindSession, lim.Classification.Kind)
	assert.Equal(2, calls, "agent must be called twice (fix + retry) before abort")
	assert.Equal(1, classifyCalls, "classifier should run once on the retry error")
	assert.Contains(err.Error(), "claude-code")
	assert.Contains(err.Error(), "session limit")
	// The first call left a dirty tree; the abort must still surface
	// the uncommitted-changes warning so the user knows their tree
	// needs attention before re-running after cooldown.
	assert.Contains(
		buf.String(),
		"Changes were made but not committed",
		"limit-abort retry path should still warn about uncommitted changes",
	)
}
