package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testenv"
	"go.kenn.io/roborev/internal/testutil"
)

// safeRecorder wraps httptest.ResponseRecorder with mutex protection for concurrent access
type safeRecorder struct {
	*httptest.ResponseRecorder
	mu sync.Mutex
}

func newSafeRecorder() *safeRecorder {
	return &safeRecorder{ResponseRecorder: httptest.NewRecorder()}
}

func (s *safeRecorder) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ResponseRecorder.Write(p)
}

func (s *safeRecorder) WriteHeader(code int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ResponseRecorder.WriteHeader(code)
}

func (s *safeRecorder) Header() http.Header {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ResponseRecorder.Header()
}

func (s *safeRecorder) bodyString() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Body.String()
}

// waitForSubscriberIncrease polls until subscriber count increases from initialCount
func waitForSubscriberIncrease(b Broadcaster, initialCount int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if b.SubscriberCount() > initialCount {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

// waitForEvents polls until the response body contains at least minEvents newline-delimited events
func waitForEvents(w *safeRecorder, minEvents int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		body := w.bodyString()
		count := strings.Count(body, "\n")
		if count >= minEvents {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

// newTestServer creates a Server with a test DB and default config.
// Returns the server, DB (for seeding/assertions), and temp directory.
// setJobStatus is a test helper to update a job's status.
func setJobStatus(t *testing.T, db *storage.DB, jobID int64, status storage.JobStatus) {
	t.Helper()
	var query string
	switch status {
	case storage.JobStatusRunning:
		query = `UPDATE review_jobs SET status = 'running', started_at = datetime('now') WHERE id = ?`
	case storage.JobStatusDone:
		query = `UPDATE review_jobs SET status = 'done', started_at = datetime('now'), finished_at = datetime('now') WHERE id = ?`
	case storage.JobStatusFailed:
		query = `UPDATE review_jobs SET status = 'failed', started_at = datetime('now'), finished_at = datetime('now'), error = 'test error' WHERE id = ?`
	case storage.JobStatusCanceled:
		query = `UPDATE review_jobs SET status = 'canceled', started_at = datetime('now'), finished_at = datetime('now') WHERE id = ?`
	default:
		require.Condition(t, func() bool {
			return false
		}, "unsupported status in helper: %v", status)
	}
	res, err := db.Exec(query, jobID)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "failed to set job status: %v", err)
	}
	if rows, err := res.RowsAffected(); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "failed to get rows affected: %v", err)
	} else if rows != 1 {
		require.Condition(t, func() bool {
			return false
		}, "expected 1 row affected when setting job status, got %d", rows)
	}
}

// createTestJob is a helper to reduce boilerplate for creating a test repo, commit, and job.
func createTestJob(t *testing.T, db *storage.DB, dir, gitRef, agent string) *storage.ReviewJob {
	t.Helper()
	repo, err := db.GetOrCreateRepo(dir)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "GetOrCreateRepo failed: %v", err)
	}
	commit, err := db.GetOrCreateCommit(repo.ID, gitRef, "Author", "Subject", time.Now())
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "GetOrCreateCommit failed: %v", err)
	}
	job, err := db.EnqueueJob(storage.EnqueueOpts{RepoID: repo.ID, CommitID: commit.ID, GitRef: gitRef, Agent: agent})
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "EnqueueJob failed: %v", err)
	}
	return job
}

func newTestServer(t *testing.T) (*Server, *storage.DB, string) {
	t.Helper()
	db, tmpDir := testutil.OpenTestDBWithDir(t)
	cfg := config.DefaultConfig()
	server := NewServer(db, cfg, "")
	// NewServer spawns a HookRunner goroutine and a worker pool. Without
	// teardown, each test that calls newTestServer leaks them; on slow
	// Windows runners the cumulative goroutine / channel work pushes the
	// daemon test package past its 10m timeout. Close() stops the
	// HookRunner, the (empty) worker pool, and any other internal
	// background work.
	t.Cleanup(func() { _ = server.Close() })
	return server, db, tmpDir
}

func TestServerStartRejectsNonLoopbackBindAddr(t *testing.T) {
	tests := []struct {
		name string
		addr string
	}{
		{name: "all interfaces", addr: "0.0.0.0:0"},
		{name: "unspecified host", addr: ":7373"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db, _ := testutil.OpenTestDBWithDir(t)
			cfg := config.DefaultConfig()
			cfg.ServerAddr = tt.addr

			server := NewServer(db, cfg, "")
			err := server.Start(context.Background())
			if err == nil {
				require.Condition(t, func() bool {
					return false
				}, "expected start error for %q", tt.addr)
			}
			if !strings.Contains(err.Error(), "loopback host") {
				require.Condition(t, func() bool {
					return false
				}, "unexpected error for %q: %v", tt.addr, err)
			}
		})
	}
}

func TestWaitForServerReadySurfacesServeError(t *testing.T) {
	serveErrCh := make(chan error, 1)
	wantErr := errors.New("serve failed")
	serveErrCh <- wantErr

	ready, serveExited, err := waitForServerReady(context.Background(), DaemonEndpoint{Network: "tcp", Address: "127.0.0.1:1"}, 50*time.Millisecond, serveErrCh)
	if ready {
		require.Condition(t, func() bool {
			return false
		}, "expected ready=false when serve exits early")
	}
	if !serveExited {
		require.Condition(t, func() bool {
			return false
		}, "expected serveExited=true when serveErrCh was consumed")
	}
	if !errors.Is(err, wantErr) {
		require.Condition(t, func() bool {
			return false
		}, "expected error %v, got %v", wantErr, err)
	}
}

func TestWaitForServerReadyLeavesServeExitUnreadWhenContextAlreadyCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	serveErrCh := make(chan error, 1)
	serveErrCh <- http.ErrServerClosed

	ready, serveExited, err := waitForServerReady(ctx, DaemonEndpoint{Network: "tcp", Address: "127.0.0.1:1"}, 50*time.Millisecond, serveErrCh)
	if ready {
		require.Condition(t, func() bool {
			return false
		}, "expected ready=false when startup is canceled")
	}
	if serveExited {
		require.Condition(t, func() bool {
			return false
		}, "expected serveExited=false when waitForServerReady exits before reading serveErrCh")
	}
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "expected nil error, got %v", err)
	}
}

func TestAwaitServeExitOnUnreadyStartupReturnsImmediatelyWhenServeAlreadyExited(t *testing.T) {
	serveErrCh := make(chan error)
	done := make(chan error, 1)
	go func() {
		done <- awaitServeExitOnUnreadyStartup(true, serveErrCh)
	}()

	select {
	case err := <-done:
		if err != nil {
			require.Condition(t, func() bool {
				return false
			}, "expected nil error, got %v", err)
		}
	case <-time.After(time.Second):
		require.Condition(t, func() bool {
			return false
		}, "awaitServeExitOnUnreadyStartup blocked even though serve had already exited")
	}
}

func TestAwaitServeExitOnUnreadyStartupWaitsForServeExit(t *testing.T) {
	t.Parallel()
	serveErrCh := make(chan error)
	done := make(chan error, 1)
	go func() {
		done <- awaitServeExitOnUnreadyStartup(false, serveErrCh)
	}()

	select {
	case err := <-done:
		require.Condition(t, func() bool {
			return false
		}, "expected helper to block before serve exit, got %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	serveErrCh <- http.ErrServerClosed

	select {
	case err := <-done:
		if err != nil {
			require.Condition(t, func() bool {
				return false
			}, "expected nil error, got %v", err)
		}
	case <-time.After(time.Second):
		require.Condition(t, func() bool {
			return false
		}, "awaitServeExitOnUnreadyStartup did not return after serve exited")
	}
}

func TestServerStartReadinessFailureDoesNotLeavePanelSweep(t *testing.T) {
	testenv.SetDataDir(t)

	db, _ := testutil.OpenTestDBWithDir(t)
	cfg := config.DefaultConfig()
	cfg.ServerAddr = "127.0.0.1:0"
	server := NewServer(db, cfg, "")
	t.Cleanup(func() { _ = server.Close() })

	server.httpServer.Handler = http.NotFoundHandler()

	err := server.Start(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to become ready")

	server.sweepMu.Lock()
	sweepCancel := server.sweepCancel
	server.sweepMu.Unlock()
	assert.Nil(t, sweepCancel, "panel sweep must not remain active after startup readiness failure")
}

func TestServerStartSupportsIPv6LoopbackBindAddr(t *testing.T) {
	ln, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Skipf("IPv6 loopback not available: %v", err)
	}
	ln.Close()

	testenv.SetDataDir(t)

	db, _ := testutil.OpenTestDBWithDir(t)
	cfg := config.DefaultConfig()
	cfg.ServerAddr = "[::1]:0"
	server := NewServer(db, cfg, "")

	errCh := make(chan error, 1)
	go func() {
		errCh <- server.Start(context.Background())
	}()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-errCh:
			require.Condition(t, func() bool {
				return false
			}, "server exited before becoming ready: %v", err)
		default:
		}

		info, err := ReadRuntime()
		if err == nil {
			host, _, splitErr := net.SplitHostPort(info.Address)
			if splitErr != nil {
				require.Condition(t, func() bool {
					return false
				}, "runtime addr %q is invalid: %v", info.Address, splitErr)
			}
			if host != "::1" {
				require.Condition(t, func() bool {
					return false
				}, "expected IPv6 loopback host, got %q", host)
			}
			if stopErr := server.Stop(); stopErr != nil {
				require.Condition(t, func() bool {
					return false
				}, "server.Stop() error: %v", stopErr)
			}
			select {
			case err := <-errCh:
				if err != nil {
					require.Condition(t, func() bool {
						return false
					}, "server.Start() returned error after stop: %v", err)
				}
			case <-time.After(5 * time.Second):
				require.Condition(t, func() bool {
					return false
				}, "timed out waiting for server to stop")
			}
			return
		}

		time.Sleep(10 * time.Millisecond)
	}
	require.Condition(t, func() bool {
		return false
	}, "timed out waiting for IPv6 daemon runtime")
}

func TestNewServerAllowUnsafeAgents(t *testing.T) {
	boolTrue := true
	boolFalse := false

	t.Run("config nil keeps agent state false", func(t *testing.T) {
		// Save and restore global state
		prev := agent.AllowUnsafeAgents()
		t.Cleanup(func() { agent.SetAllowUnsafeAgents(prev) })

		// Pre-set to true to verify it gets reset
		agent.SetAllowUnsafeAgents(true)

		db, _ := testutil.OpenTestDBWithDir(t)
		cfg := config.DefaultConfig()
		cfg.AllowUnsafeAgents = nil // not set

		_ = NewServer(db, cfg, "")

		if agent.AllowUnsafeAgents() {
			assert.Condition(t, func() bool {
				return false
			}, "expected AllowUnsafeAgents to be false when config is nil")
		}
	})

	t.Run("config false sets agent state false", func(t *testing.T) {
		prev := agent.AllowUnsafeAgents()
		t.Cleanup(func() { agent.SetAllowUnsafeAgents(prev) })

		// Pre-set to true to verify it gets reset
		agent.SetAllowUnsafeAgents(true)

		db, _ := testutil.OpenTestDBWithDir(t)
		cfg := config.DefaultConfig()
		cfg.AllowUnsafeAgents = &boolFalse

		_ = NewServer(db, cfg, "")

		if agent.AllowUnsafeAgents() {
			assert.Condition(t, func() bool {
				return false
			}, "expected AllowUnsafeAgents to be false when config is false")
		}
	})

	t.Run("config true sets agent state true", func(t *testing.T) {
		prev := agent.AllowUnsafeAgents()
		t.Cleanup(func() { agent.SetAllowUnsafeAgents(prev) })

		// Pre-set to false to verify it gets set
		agent.SetAllowUnsafeAgents(false)

		db, _ := testutil.OpenTestDBWithDir(t)
		cfg := config.DefaultConfig()
		cfg.AllowUnsafeAgents = &boolTrue

		_ = NewServer(db, cfg, "")

		if !agent.AllowUnsafeAgents() {
			assert.Condition(t, func() bool {
				return false
			}, "expected AllowUnsafeAgents to be true when config is true")
		}
	})
}

// seedRepoWithJobs creates a repo and enqueues a number of jobs with predictable SHAs.
// shaPrefix is used to generate SHAs like "{prefix}sha{a,b,c...}".
func seedRepoWithJobs(t *testing.T, db *storage.DB, repoPath string, jobCount int, shaPrefix string) (*storage.Repo, []*storage.ReviewJob) {
	t.Helper()
	repo, err := db.GetOrCreateRepo(repoPath)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "GetOrCreateRepo failed: %v", err)
	}

	var jobs []*storage.ReviewJob
	for i := range jobCount {
		sha := fmt.Sprintf("%ssha%c", shaPrefix, 'a'+i)
		commit, err := db.GetOrCreateCommit(repo.ID, sha, "Author", "Subject", time.Now())
		if err != nil {
			require.Condition(t, func() bool {
				return false
			}, "GetOrCreateCommit failed: %v", err)
		}
		job, err := db.EnqueueJob(storage.EnqueueOpts{RepoID: repo.ID, CommitID: commit.ID, GitRef: sha, Agent: "test"})
		if err != nil {
			require.Condition(t, func() bool {
				return false
			}, "EnqueueJob failed: %v", err)
		}
		jobs = append(jobs, job)
	}
	return repo, jobs
}

// setJobBranch directly updates a job's branch in the DB (for test setup).
func setJobBranch(t *testing.T, db *storage.DB, jobID int64, branch string) {
	t.Helper()
	if _, err := db.Exec("UPDATE review_jobs SET branch = ? WHERE id = ?", branch, jobID); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "setJobBranch failed: %v", err)
	}
}

func TestGetMachineID_CachingBehavior(t *testing.T) {
	t.Run("caches valid machine ID", func(t *testing.T) {
		server, _, _ := newTestServer(t)

		// First call should fetch from DB and cache
		id1 := server.getMachineID()
		if id1 == "" {
			require.Condition(t, func() bool {
				return false
			}, "Expected non-empty machine ID on first call")
		}

		// Second call should return cached value
		id2 := server.getMachineID()
		if id2 != id1 {
			assert.Condition(t, func() bool {
				return false
			}, "Expected cached value %q, got %q", id1, id2)
		}

		// Verify internal state is cached
		server.machineIDMu.Lock()
		cachedID := server.machineID
		server.machineIDMu.Unlock()
		if cachedID != id1 {
			assert.Condition(t, func() bool {
				return false
			}, "Expected internal machineID to be %q, got %q", id1, cachedID)
		}
	})

	t.Run("error then success caches on success", func(t *testing.T) {
		// This tests the error→success retry path:
		// 1. First call fails (DB closed) → returns empty, not cached
		// 2. DB replaced with working one
		// 3. Second call succeeds → returns valid ID and caches it

		server, db, tmpDir := newTestServer(t)

		// Close the DB to simulate error condition
		db.Close()

		// First call should return empty since DB is closed
		id1 := server.getMachineID()
		if id1 != "" {
			require.Condition(t, func() bool {
				return false
			}, "Expected empty machine ID on error, got %q", id1)
		}

		// Verify nothing was cached
		server.machineIDMu.Lock()
		if server.machineID != "" {
			server.machineIDMu.Unlock()
			require.Condition(t, func() bool {
				return false
			}, "Should not cache on error")
		}
		server.machineIDMu.Unlock()

		// "Fix" the error by opening a new DB and replacing it
		newDB, err := storage.Open(filepath.Join(tmpDir, "reviews.db"))
		if err != nil {
			require.Condition(t, func() bool {
				return false
			}, "Failed to reopen DB: %v", err)
		}
		t.Cleanup(func() { newDB.Close() })
		server.db = newDB

		// Second call should succeed and cache
		id2 := server.getMachineID()
		if id2 == "" {
			require.Condition(t, func() bool {
				return false
			}, "Expected non-empty machine ID after DB recovery")
		}

		// Verify it's now cached
		server.machineIDMu.Lock()
		cachedID := server.machineID
		server.machineIDMu.Unlock()
		if cachedID != id2 {
			assert.Condition(t, func() bool {
				return false
			}, "Expected cached ID %q, got %q", id2, cachedID)
		}

		// Third call should return cached value
		id3 := server.getMachineID()
		if id3 != id2 {
			assert.Condition(t, func() bool {
				return false
			}, "Expected cached ID %q on third call, got %q", id2, id3)
		}
	})
}

func TestParseDuration(t *testing.T) {
	tests := []struct {
		input string
		want  time.Duration
		err   bool
	}{
		{"7d", 7 * 24 * time.Hour, false},
		{"24h", 24 * time.Hour, false},
		{"2w", 14 * 24 * time.Hour, false},
		{"1d", 24 * time.Hour, false},
		// Error cases
		{"", 0, true},
		{"d", 0, true},
		{"-7d", 0, true},
		{"0d", 0, true},
		{"7m", 0, true},
		{"abc", 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := parseDuration(tt.input)
			if tt.err {
				if err == nil {
					t.Errorf("parseDuration(%q) expected error, got %v", tt.input, got)
				}
				return
			}
			if err != nil {
				t.Errorf("parseDuration(%q) unexpected error: %v", tt.input, err)
				return
			}
			if got != tt.want {
				t.Errorf("parseDuration(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestServerStop_StopsCIPoller(t *testing.T) {
	server, db, _ := newTestServer(t)

	cfg := config.DefaultConfig()
	cfg.CI.Enabled = true
	cfg.CI.PollInterval = "1h"

	poller := NewCIPoller(db, NewStaticConfig(cfg), server.Broadcaster())
	if err := poller.Start(); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "Start poller: %v", err)
	}

	healthy, _ := poller.HealthCheck()
	if !healthy {
		require.Condition(t, func() bool {
			return false
		}, "expected poller running after Start")
	}

	server.SetCIPoller(poller)
	if err := server.Stop(); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "Server.Stop: %v", err)
	}

	healthy, msg := poller.HealthCheck()
	if healthy {
		require.Condition(t, func() bool {
			return false
		}, "expected poller stopped after Server.Stop, got (%v, %q)", healthy, msg)
	}
}

func TestCostOptionsFromInput(t *testing.T) {
	assert := assert.New(t)

	opts, err := costOptionsFromInput(&GetCostInput{
		Repo:        []string{"/a", "/b"},
		BranchEmpty: "true",
	})
	require.NoError(t, err)
	assert.Equal([]string{"/a", "/b"}, opts.RepoPaths)
	assert.True(opts.BranchEmpty)
	assert.True(opts.Since.IsZero(), "empty since means all-time")

	opts, err = costOptionsFromInput(&GetCostInput{Branch: "main", Since: "7d"})
	require.NoError(t, err)
	assert.Equal("main", opts.Branch)
	assert.False(opts.BranchEmpty)
	assert.False(opts.Since.IsZero())

	_, err = costOptionsFromInput(&GetCostInput{Since: "bogus"})
	assert.Error(err)
}
