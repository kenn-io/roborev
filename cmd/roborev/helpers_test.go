package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/storage"
)

// TestGitRepo wraps a temporary git repository for test use.
type TestGitRepo struct {
	Dir string
	t   *testing.T
}

// newTestGitRepo creates and initializes a temporary git repository.
func newTestGitRepo(t *testing.T) *TestGitRepo {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	resolved, err := filepath.EvalSymlinks(dir)
	require.NoError(t, err, "Failed to resolve symlinks: %v")

	r := &TestGitRepo{Dir: resolved, t: t}
	r.Run("init")
	r.Run("config", "user.email", "test@test.com")
	r.Run("config", "user.name", "Test")
	return r
}

// newBareTestGitRepo creates a bare git repository for use as a remote.
func newBareTestGitRepo(t *testing.T) *TestGitRepo {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	resolved, err := filepath.EvalSymlinks(dir)
	require.NoError(t, err, "Failed to resolve symlinks: %v")

	r := &TestGitRepo{Dir: resolved, t: t}
	r.Run("init", "--bare")
	return r
}

// chdir changes to dir and registers a t.Cleanup to restore the original directory.
func chdir(t *testing.T, dir string) {
	t.Helper()
	orig, err := os.Getwd()
	require.NoError(t, err, "Failed to getwd: %v")

	err = os.Chdir(dir)
	require.NoError(t, err, "Failed to chdir: %v")
	t.Cleanup(func() { os.Chdir(orig) })
}

// Run executes a git command in the repo directory and returns trimmed output.
// It isolates git from the user's global config to prevent flaky tests.
func (r *TestGitRepo) Run(args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = r.Dir
	// Build a clean environment with only the variables git needs,
	// avoiding conflicts from inherited duplicates.
	gitEnv := []string{
		"HOME=" + r.Dir,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=Test",
		"GIT_AUTHOR_EMAIL=test@test.com",
		"GIT_COMMITTER_NAME=Test",
		"GIT_COMMITTER_EMAIL=test@test.com",
	}
	overridden := map[string]bool{
		"HOME":                true,
		"GIT_CONFIG_NOSYSTEM": true,
		"GIT_AUTHOR_NAME":     true,
		"GIT_AUTHOR_EMAIL":    true,
		"GIT_COMMITTER_NAME":  true,
		"GIT_COMMITTER_EMAIL": true,
	}
	for _, env := range os.Environ() {
		if key, _, ok := strings.Cut(env, "="); ok && !overridden[key] {
			gitEnv = append(gitEnv, env)
		}
	}
	cmd.Env = gitEnv
	out, err := cmd.CombinedOutput()
	require.NoError(r.t, err, "git %v failed:\n%s", args, out)
	return strings.TrimSpace(string(out))
}

// CommitFile creates or overwrites a file, stages it, and commits with the
// given message. Returns the full commit SHA.
func (r *TestGitRepo) CommitFile(name, content, msg string) string {
	r.t.Helper()
	fullPath := filepath.Join(r.Dir, name)
	err := os.MkdirAll(filepath.Dir(fullPath), 0o755)
	require.NoError(r.t, err)
	err = os.WriteFile(fullPath, []byte(content), 0o644)
	require.NoError(r.t, err)
	r.Run("add", name)
	r.Run("commit", "-m", msg)
	return r.Run("rev-parse", "HEAD")
}

// WriteFiles writes the given files to the repository directory.
func (r *TestGitRepo) WriteFiles(files map[string]string) {
	r.t.Helper()
	writeFiles(r.t, r.Dir, files)
}

// patchServerAddr safely swaps the global serverAddr variable and restores it
// when the test completes.
func patchServerAddr(t *testing.T, newURL string) {
	t.Helper()
	old := serverAddr
	serverAddr = newURL
	t.Cleanup(func() { serverAddr = old })
}

// mustParseEndpoint parses a server URL into a DaemonEndpoint, failing the
// test if parsing fails. Useful for converting httptest.Server.URL to an endpoint.
func mustParseEndpoint(t *testing.T, serverURL string) daemon.DaemonEndpoint {
	t.Helper()
	ep, err := daemon.ParseEndpoint(serverURL)
	require.NoError(t, err, "parse endpoint %q", serverURL)
	return ep
}

// createTestRepo creates a temporary git repository with the given files
// committed. It returns the TestGitRepo.
func createTestRepo(t *testing.T, files map[string]string) *TestGitRepo {
	t.Helper()

	r := newTestGitRepo(t)
	r.WriteFiles(files)
	r.Run("add", ".")
	r.Run("commit", "-m", "initial")
	return r
}

// writeTestFiles creates files in a directory without git. Returns the directory.
func writeTestFiles(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	writeFiles(t, dir, files)
	return dir
}

// writeFiles is a helper to write files to a directory.
func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for path, content := range files {
		fullPath := filepath.Join(dir, path)
		err := os.MkdirAll(filepath.Dir(fullPath), 0o755)
		require.NoError(t, err, "mkdir: %v")
		err = os.WriteFile(fullPath, []byte(content), 0o644)
		require.NoError(t, err, "write %s: %v", path, err)
	}
}

// mockReviewDaemon sets up a mock daemon that returns the given review on
// GET /api/review. It returns a function to retrieve the last received query
// string.
func mockReviewDaemon(t *testing.T, review storage.Review) func() string {
	t.Helper()
	var mu sync.Mutex
	var receivedQuery string
	daemonFromHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/review" && r.Method == "GET" {
			mu.Lock()
			receivedQuery = r.URL.RawQuery
			mu.Unlock()
			json.NewEncoder(w).Encode(review)
			return
		}
	}))
	return func() string {
		mu.Lock()
		defer mu.Unlock()
		return receivedQuery
	}
}

// runShowCmd executes showCmd() with the given args and returns captured stdout.
func runShowCmd(t *testing.T, args ...string) string {
	t.Helper()
	cmd := showCmd()
	cmd.SetArgs(args)
	return captureStdout(t, func() {
		err := cmd.Execute()
		require.NoError(t, err)
	})
}

// newTestCmd creates a cobra.Command with output captured to the returned buffer.
func newTestCmd(t *testing.T) (*cobra.Command, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&buf)
	return cmd, &buf
}

// MockServerState tracks counters for API calls made to a mock server.
type MockServerState struct {
	EnqueueCount int32
	JobsCount    int32
	ReviewCount  int32
	CloseCount   int32
	CommentCount int32
}

func (s *MockServerState) Enqueues() int32 { return atomic.LoadInt32(&s.EnqueueCount) }
func (s *MockServerState) Jobs() int32     { return atomic.LoadInt32(&s.JobsCount) }
func (s *MockServerState) Reviews() int32  { return atomic.LoadInt32(&s.ReviewCount) }
func (s *MockServerState) Closes() int32   { return atomic.LoadInt32(&s.CloseCount) }
func (s *MockServerState) Comments() int32 { return atomic.LoadInt32(&s.CommentCount) }

// MockServerOpts configures the behavior of a mock roborev server.
type MockServerOpts struct {
	// JobIDStart is the starting job ID for enqueue responses (0 defaults to 1).
	JobIDStart int64
	// Agent is the agent name in responses (default "test").
	Agent string
	// DoneAfterPolls is the number of /api/jobs polls before reporting done (default 2).
	DoneAfterPolls int32
	// ReviewOutput is the review text returned by /api/review.
	ReviewOutput string
	// JobStatusSequence is an optional sequence of job statuses to return on successive /api/jobs polls.
	JobStatusSequence []storage.JobStatus
	// JobError is an optional error string to return in the job response.
	JobError string
	// JobNotFound, if true, simulates a missing job (returns empty jobs array).
	JobNotFound bool
	// OnEnqueue is an optional callback for /api/enqueue requests.
	OnEnqueue func(w http.ResponseWriter, r *http.Request)
	// OnJobs is an optional callback for /api/jobs requests. If set, overrides default behavior.
	OnJobs func(w http.ResponseWriter, r *http.Request)
	// OnClose is an optional callback for /api/review/close requests.
	OnClose func(w http.ResponseWriter, r *http.Request)
}

type mockServerHandler struct {
	opts  MockServerOpts
	state *MockServerState
	jobID int64
}

func (h *mockServerHandler) handleEnqueue(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if h.opts.OnEnqueue != nil {
		atomic.AddInt32(&h.state.EnqueueCount, 1)
		h.opts.OnEnqueue(w, r)
		return
	}
	id := atomic.AddInt64(&h.jobID, 1)
	atomic.AddInt32(&h.state.EnqueueCount, 1)
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(storage.ReviewJob{
		ID:     id,
		Agent:  h.opts.Agent,
		Status: storage.JobStatusQueued,
	})
}

func (h *mockServerHandler) handleJobs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if h.opts.OnJobs != nil {
		atomic.AddInt32(&h.state.JobsCount, 1)
		h.opts.OnJobs(w, r)
		return
	}
	count := atomic.AddInt32(&h.state.JobsCount, 1)

	if h.opts.JobNotFound {
		json.NewEncoder(w).Encode(map[string]any{
			"jobs": []storage.ReviewJob{},
		})
		return
	}

	var status storage.JobStatus
	if len(h.opts.JobStatusSequence) > 0 {
		idx := int(count) - 1
		if idx >= len(h.opts.JobStatusSequence) {
			idx = len(h.opts.JobStatusSequence) - 1
		}
		status = h.opts.JobStatusSequence[idx]
	} else {
		status = storage.JobStatusQueued
		if count >= h.opts.DoneAfterPolls {
			status = storage.JobStatusDone
		}
	}

	json.NewEncoder(w).Encode(map[string]any{
		"jobs": []storage.ReviewJob{{
			ID:     atomic.LoadInt64(&h.jobID),
			Status: status,
			Error:  h.opts.JobError,
		}},
	})
}

func (h *mockServerHandler) handleReview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	atomic.AddInt32(&h.state.ReviewCount, 1)
	output := h.opts.ReviewOutput
	if output == "" {
		output = "review output"
	}
	json.NewEncoder(w).Encode(storage.Review{
		JobID:  atomic.LoadInt64(&h.jobID),
		Agent:  h.opts.Agent,
		Output: output,
	})
}

func (h *mockServerHandler) handleComment(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	atomic.AddInt32(&h.state.CommentCount, 1)
	w.WriteHeader(http.StatusCreated)
}

func (h *mockServerHandler) handleClose(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	atomic.AddInt32(&h.state.CloseCount, 1)
	if h.opts.OnClose != nil {
		h.opts.OnClose(w, r)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// newMockServer creates an httptest.Server that mimics the roborev daemon API.
// It handles /api/enqueue, /api/jobs, /api/review, and /api/review/close.
func newMockServer(t *testing.T, opts MockServerOpts) (*httptest.Server, *MockServerState) {
	t.Helper()
	state := &MockServerState{}

	if opts.Agent == "" {
		opts.Agent = "test"
	}
	if opts.DoneAfterPolls == 0 {
		opts.DoneAfterPolls = 2
	}
	jobIDStart := opts.JobIDStart
	if jobIDStart <= 0 {
		jobIDStart = 1
	}

	h := &mockServerHandler{
		opts:  opts,
		state: state,
		jobID: jobIDStart - 1,
	}

	mux := http.NewServeMux()

	mux.HandleFunc("/api/enqueue", h.handleEnqueue)
	mux.HandleFunc("/api/jobs", h.handleJobs)
	mux.HandleFunc("/api/review", h.handleReview)
	mux.HandleFunc("/api/comment", h.handleComment)
	mux.HandleFunc("/api/review/close", h.handleClose)

	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts, state
}
