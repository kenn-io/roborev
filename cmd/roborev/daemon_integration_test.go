//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

func TestDaemonRunStartsAndShutdownsCleanly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping daemon integration test on Windows due to file locking differences")
	}

	// Mock setupSignalHandler to verify cleanup
	var cleanupCalled bool
	origSetupSignalHandler := setupSignalHandler
	setupSignalHandler = func() (chan os.Signal, func()) {
		// Return a dummy channel that will never fire signals
		sigCh := make(chan os.Signal, 1)
		return sigCh, func() {
			cleanupCalled = true
		}
	}
	defer func() { setupSignalHandler = origSetupSignalHandler }()

	dbPath, configPath := setupTestDaemon(t)

	// Create context for cancellation
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Create the daemon run command with custom flags
	// Use an ephemeral port (0) to avoid conflicts with production.
	cmd := daemonRunCmd()
	cmd.SetContext(ctx)
	cmd.SetArgs([]string{
		"--db", dbPath,
		"--config", configPath,
		"--addr", "127.0.0.1:0",
	})

	// Run daemon in goroutine
	errCh := make(chan error, 1)
	go func() {
		errCh <- cmd.Execute()
	}()

	// Wait for daemon to start (check if DB file is created)
	if !waitFor(t, 5*time.Second, func() bool {
		_, err := os.Stat(dbPath)
		return err == nil
	}) {
		require.Condition(t, func() bool {
			return false
		}, "timed out waiting for database creation")
	}

	// Verify DB was created (redundant with waitFor success, but keeps original intent)
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		require.Condition(t, func() bool {
			return false
		}, "expected database to be created")
	}

	// Check that daemon didn't exit early with an error
	select {
	case err := <-errCh:
		require.Condition(t, func() bool {
			return false
		}, "daemon exited unexpectedly: %v", err)
	case <-time.After(100 * time.Millisecond):
		// Daemon is still running - good
	}

	// Wait for daemon to be fully started and responsive.
	// Use longer timeout for race detector which adds significant overhead.
	myPID := os.Getpid()

	if !waitForDaemonReady(t, 30*time.Second, myPID) {
		// Provide more context for debugging CI failures
		runtimes, _ := daemon.ListAllRuntimes()
		require.Condition(t, func() bool {
			return false
		}, "daemon did not create runtime file or is not responding (myPID=%d, found %d runtimes)", myPID, len(runtimes))
	}

	// Trigger shutdown via context cancellation instead of sending OS signal
	cancel()

	// Wait for daemon to exit (longer timeout for race detector)
	select {
	case <-errCh:
		// Daemon exited - good
		if !cleanupCalled {
			assert.Condition(t, func() bool {
				return false
			}, "expected signal.Stop (cleanup) to be called")
		}
	case <-time.After(10 * time.Second):
		require.Condition(t, func() bool {
			return false
		}, "daemon did not exit within 10 second timeout")
	}
}

func setupTestDaemon(t *testing.T) (string, string) {
	t.Helper()

	// Use temp directories for isolation
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
	configPath := filepath.Join(tmpDir, "config.toml")

	// Isolate runtime dir to avoid writing to the real daemon runtime store.
	t.Setenv("ROBOREV_DATA_DIR", tmpDir)

	// Write minimal config
	if err := os.WriteFile(configPath, []byte(`max_workers = 1`), 0o644); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "write config: %v", err)
	}

	return dbPath, configPath
}

func waitFor(t *testing.T, timeout time.Duration, check func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if check() {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

func waitForDaemonReady(t *testing.T, timeout time.Duration, pid int) bool {
	t.Helper()
	return waitFor(t, timeout, func() bool {
		runtimes, err := daemon.ListAllRuntimes()
		if err == nil {
			for _, rt := range runtimes {
				if rt.PID == pid && daemon.IsDaemonAlive(rt.Endpoint()) {
					return true
				}
			}
		}
		return false
	})
}

func TestDaemonShutdownBySignal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping signal test on Windows")
	}

	dbPath, configPath := setupTestDaemon(t)
	tmpDir := filepath.Dir(dbPath) // Extract isolated temp dir for binary build

	// 1. Build a test binary
	binPath := filepath.Join(tmpDir, "roborev-test")
	// Use "." since we are in cmd/roborev package. The built daemon runs in a
	// subprocess, so compile kit's test telemetry disable tag into that binary.
	buildCmd := exec.Command("go", "build", "-tags", "kit_posthog_disabled", "-o", binPath, ".")
	if out, err := buildCmd.CombinedOutput(); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "failed to build test binary: %v\n%s", err, out)
	}

	// 2. Start daemon in subprocess
	// Use an ephemeral port (0) to avoid conflicts
	cmd := exec.Command(binPath, "daemon", "run",
		"--db", dbPath,
		"--config", configPath,
		"--addr", "127.0.0.1:0",
	)
	// Important: Set ROBOREV_DATA_DIR so it writes runtime files under our tmpDir
	cmd.Env = append(os.Environ(), "ROBOREV_DATA_DIR="+tmpDir)

	// Capture output for debugging. Use syncBuffer: assertion message
	// arguments call String() while the exec copy goroutine still writes.
	outputBuffer := new(syncBuffer)
	cmd.Stdout = outputBuffer
	cmd.Stderr = outputBuffer

	if err := cmd.Start(); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "failed to start daemon: %v", err)
	}

	// Ensure cleanup in case of failure
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	t.Cleanup(func() {
		if cmd.ProcessState == nil || !cmd.ProcessState.Exited() {
			_ = cmd.Process.Kill()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
			}
		}
	})

	// 3. Wait for daemon to be ready
	// The daemon writes runtime/daemon.<pid>.json
	daemonJSON := filepath.Join(tmpDir, "runtime", fmt.Sprintf("daemon.%d.json", cmd.Process.Pid))
	// This package runs concurrently with the full repository under the race
	// detector. Daemon startup can spend several seconds opening and migrating
	// its isolated database while the runner is saturated.
	if !waitFor(t, 30*time.Second, func() bool {
		_, err := os.Stat(daemonJSON)
		return err == nil
	}) {
		require.
			// Cleanup handled by defer
			Condition(t, func() bool {
				return false
			}, "timed out waiting for daemon to start (%s not found). Output:\n%s", daemonJSON, outputBuffer.String())
	}

	// 4. Send SIGINT
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "failed to send SIGINT: %v", err)
	}

	// 5. Wait for exit

	select {
	case err := <-done:
		if err != nil {
			// Check if it's an exit status error. Ideally exit code 0.
			if exitErr, ok := err.(*exec.ExitError); ok {
				require.Condition(t, func() bool {
					return false
				}, "daemon exited with non-zero status: %v (code %d)", err, exitErr.ExitCode())
			}
			require.Condition(t, func() bool {
				return false
			}, "daemon wait returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		require.Condition(t, func() bool {
			return false
		}, "timed out waiting for daemon to exit after SIGINT")
	}
}

func TestDaemonSignalCleanup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping daemon signal test on Windows due to file locking differences")
	}

	// Verify that signal.Stop is called when shutdown
	// is triggered by a signal.
	var cleanupCalled bool
	origSetupSignalHandler := setupSignalHandler
	defer func() { setupSignalHandler = origSetupSignalHandler }()

	// Use a buffered channel so the mock can send sigCh
	// back to the test goroutine without racing.
	sigReady := make(chan chan os.Signal, 1)

	setupSignalHandler = func() (chan os.Signal, func()) {
		sigCh := make(chan os.Signal, 1)
		sigReady <- sigCh
		return sigCh, func() {
			cleanupCalled = true
		}
	}

	dbPath, configPath := setupTestDaemon(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cmd := daemonRunCmd()
	cmd.SetContext(ctx)
	cmd.SetArgs([]string{
		"--db", dbPath,
		"--config", configPath,
		"--addr", "127.0.0.1:0",
	})

	errCh := make(chan error, 1)
	go func() {
		errCh <- cmd.Execute()
	}()

	// Wait for the signal handler to be installed (race-free).
	// Allow for database initialization overhead under the race detector.
	var sigCh chan os.Signal
	select {
	case sigCh = <-sigReady:
	case <-time.After(10 * time.Second):
		require.Condition(t, func() bool {
			return false
		}, "timed out waiting for signal handler setup")
	}

	// Trigger shutdown via signal.
	sigCh <- os.Interrupt

	select {
	case err := <-errCh:
		if err != nil {
			require.Condition(t, func() bool {
				return false
			}, "daemon exited with error: %v", err)
		}
		if !cleanupCalled {
			assert.Condition(t, func() bool {
				return false
			}, "expected signal.Stop (cleanup) to be"+
				" called after signal shutdown")
		}
	case <-time.After(5 * time.Second):
		require.Condition(t, func() bool {
			return false
		}, "daemon did not exit within timeout")
	}
}

// syncBuffer is a goroutine-safe bytes.Buffer for capturing subprocess
// output: the exec stdout-copy goroutine writes while assertion message
// arguments read, and those arguments are evaluated even on success.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestDaemonLifecycleEndToEnd exercises the real daemon binary on every OS,
// including Windows: spawn, runtime publication, liveness probe, a
// DB-backed API endpoint, and HTTP shutdown (the production stop path).
// The DB-backed /api/status check is deliberate: a daemon can answer
// /api/ping from memory while every database-backed endpoint hangs, which is
// exactly the "zombie daemon" failure mode from issue #834.
func TestDaemonLifecycleEndToEnd(t *testing.T) {
	dbPath, configPath := setupTestDaemon(t)
	tmpDir := filepath.Dir(dbPath)

	binPath := filepath.Join(tmpDir, "roborev-test")
	if runtime.GOOS == "windows" {
		binPath += ".exe"
	}
	// The built daemon runs in a subprocess, so compile kit's test telemetry
	// disable tag into that binary.
	buildCmd := exec.Command("go", "build", "-tags", "kit_posthog_disabled", "-o", binPath, ".")
	out, err := buildCmd.CombinedOutput()
	require.NoError(t, err, "failed to build test binary: %s", out)

	cmd := exec.Command(binPath, "daemon", "run",
		"--db", dbPath,
		"--config", configPath,
		"--addr", "127.0.0.1:0",
	)
	cmd.Env = append(os.Environ(), "ROBOREV_DATA_DIR="+tmpDir)
	outputBuffer := new(syncBuffer)
	cmd.Stdout = outputBuffer
	cmd.Stderr = outputBuffer
	require.NoError(t, cmd.Start(), "failed to start daemon")

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		if cmd.ProcessState == nil || !cmd.ProcessState.Exited() {
			_ = cmd.Process.Kill()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
			}
		}
	})

	// Wait for the daemon to publish its runtime record and answer pings.
	pid := cmd.Process.Pid
	startupTimeout := 30 * time.Second
	if runtime.GOOS == "windows" {
		// The Windows CI runner executes all Go packages concurrently. Under that
		// I/O load, opening and migrating the isolated SQLite database can take
		// longer than the normal startup deadline.
		startupTimeout = 2 * time.Minute
	}
	var info *daemon.RuntimeInfo
	require.True(t, waitFor(t, startupTimeout, func() bool {
		read, err := daemon.ReadRuntimeForPID(pid)
		if err != nil {
			return false
		}
		info = read
		return true
	}), "daemon never published a runtime record. Output:\n%s", outputBuffer.String())

	ep := info.Endpoint()
	probe, err := daemon.ProbeDaemon(ep, 5*time.Second)
	require.NoError(t, err, "daemon must answer /api/ping. Output:\n%s", outputBuffer.String())
	assert.Equal(t, pid, probe.PID)

	// A live daemon must serve database-backed endpoints, not just ping.
	client := ep.HTTPClient(10 * time.Second)
	resp, err := client.Get(ep.BaseURL() + "/api/status")
	require.NoError(t, err, "daemon must serve /api/status. Output:\n%s", outputBuffer.String())
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	// Stop via HTTP shutdown, the path the CLI uses on every platform.
	resp, err = client.Post(ep.BaseURL()+"/api/shutdown", "application/json", nil)
	require.NoError(t, err, "shutdown request failed")
	resp.Body.Close()

	select {
	case <-done:
		// Daemon exited.
	case <-time.After(15 * time.Second):
		require.Fail(t, "daemon did not exit after /api/shutdown", "Output:\n%s", outputBuffer.String())
	}
	assert.False(t, daemon.ProcessExists(pid), "daemon process must be gone after shutdown")
}

type isolatedUpdateDaemon struct {
	cmd      *exec.Cmd
	endpoint daemon.DaemonEndpoint
	done     chan error
	output   *syncBuffer
}

func TestUpdateDrainCutoverIntegration(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the synthetic review agent is a POSIX shell script")
	}
	buildDir := t.TempDir()
	oldBinary := buildVersionedUpdateDaemon(t, buildDir, "roborev-old", "v-old")
	newBinary := buildVersionedUpdateDaemon(t, buildDir, "roborev-new", "v-new")

	for _, policy := range []runningReviewPolicy{policyWait, policyInterrupt} {
		t.Run(string(policy), func(t *testing.T) {
			dataDir := t.TempDir()
			t.Setenv("ROBOREV_DATA_DIR", dataDir)
			repo := testutil.NewGitRepo(t)
			firstSHA := repo.CommitFile("first.txt", "first\n", "first")
			script := writeSlowUpdateAgent(t, dataDir)
			configPath := filepath.Join(dataDir, "config.toml")
			configBody := fmt.Sprintf("max_workers = 1\ncodex_cmd = %s\n", strconv.Quote(script))
			require.NoError(t, os.WriteFile(configPath, []byte(configBody), 0o600))
			dbPath := filepath.Join(dataDir, "reviews.db")

			old := startIsolatedUpdateDaemon(t, oldBinary, dataDir, dbPath, configPath)
			assertDaemonUpdateVersion(t, old.endpoint, "v-old")
			active := enqueueUpdateJob(t, old.endpoint, repo.Path(), firstSHA)
			waitForUpdateJobStatus(t, old.endpoint, active.ID, storage.JobStatusRunning, 10*time.Second)

			session, err := prepareUpdateDaemon(
				context.Background(), old.endpoint, "integration-owner", policy, io.Discard,
			)
			require.NoError(t, err)
			secondSHA := repo.CommitFile("second.txt", "second\n", "second")
			queued := enqueueUpdateJob(t, old.endpoint, repo.Path(), secondSHA)
			assert.Never(t, func() bool {
				job, err := readUpdateJob(old.endpoint, queued.ID)
				return err != nil || job.Status != storage.JobStatusQueued
			}, 250*time.Millisecond, 25*time.Millisecond)

			drainCtx, cancelDrain := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancelDrain()
			require.NoError(t, waitForPreparedDrain(drainCtx, session, io.Discard))
			require.NoError(t, session.shutdown(context.Background()))
			waitForIsolatedUpdateDaemonExit(t, old, 10*time.Second)

			replacement := startIsolatedUpdateDaemon(t, newBinary, dataDir, dbPath, configPath)
			assertDaemonUpdateVersion(t, replacement.endpoint, "v-new")
			waitForUpdateJobStatus(t, replacement.endpoint, active.ID, storage.JobStatusDone, 15*time.Second)
			waitForUpdateJobStatus(t, replacement.endpoint, queued.ID, storage.JobStatusDone, 15*time.Second)
			assert.Equal(t, 0, getUpdateJob(t, replacement.endpoint, active.ID).RetryCount)

			shutdownIsolatedUpdateDaemon(t, replacement)
		})
	}
}

func buildVersionedUpdateDaemon(t *testing.T, dir, name, version string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	cmd := exec.Command(
		"go", "build", "-tags", "kit_posthog_disabled",
		"-ldflags", "-X go.kenn.io/roborev/internal/version.Version="+version,
		"-o", path, ".",
	)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "build versioned daemon: %s", out)
	return path
}

func writeSlowUpdateAgent(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "slow-codex")
	body := `#!/bin/sh
case "$*" in
  *--help*) echo "usage: codex exec --json --sandbox --ignore-user-config"; exit 0 ;;
esac
sleep 2
printf '%s\n' '{"type":"thread.started","thread_id":"integration-session"}'
printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"No issues found."}}'
`
	require.NoError(t, os.WriteFile(path, []byte(body), 0o755))
	return path
}

func startIsolatedUpdateDaemon(
	t *testing.T, binary, dataDir, dbPath, configPath string,
) *isolatedUpdateDaemon {
	t.Helper()
	cmd := exec.Command(binary, "daemon", "run",
		"--db", dbPath,
		"--config", configPath,
		"--addr", "127.0.0.1:0",
	)
	cmd.Env = append(os.Environ(), "ROBOREV_DATA_DIR="+dataDir)
	output := new(syncBuffer)
	cmd.Stdout = output
	cmd.Stderr = output
	require.NoError(t, cmd.Start())
	instance := &isolatedUpdateDaemon{
		cmd: cmd, done: make(chan error, 1), output: output,
	}
	go func() { instance.done <- cmd.Wait() }()
	t.Cleanup(func() {
		if cmd.ProcessState == nil || !cmd.ProcessState.Exited() {
			_ = cmd.Process.Kill()
			select {
			case <-instance.done:
			case <-time.After(2 * time.Second):
			}
		}
	})
	require.Eventually(t, func() bool {
		info, err := daemon.ReadRuntimeForPID(cmd.Process.Pid)
		if err != nil || !daemon.IsDaemonAlive(info.Endpoint()) {
			return false
		}
		instance.endpoint = info.Endpoint()
		return true
	}, 30*time.Second, 50*time.Millisecond, "daemon startup output:\n%s", output.String())
	return instance
}

func waitForIsolatedUpdateDaemonExit(
	t *testing.T, instance *isolatedUpdateDaemon, timeout time.Duration,
) {
	t.Helper()
	exited := false
	var exitErr error
	select {
	case exitErr = <-instance.done:
		exited = true
	case <-time.After(timeout):
	}
	require.True(t, exited, "daemon exit timed out; output:\n%s", instance.output.String())
	require.NoError(t, exitErr, "daemon output:\n%s", instance.output.String())
}

func shutdownIsolatedUpdateDaemon(t *testing.T, instance *isolatedUpdateDaemon) {
	t.Helper()
	client := instance.endpoint.HTTPClient(5 * time.Second)
	resp, err := client.Post(instance.endpoint.BaseURL()+"/api/shutdown", "application/json", nil)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	waitForIsolatedUpdateDaemonExit(t, instance, 10*time.Second)
}

func enqueueUpdateJob(
	t *testing.T, endpoint daemon.DaemonEndpoint, repoPath, gitRef string,
) storage.ReviewJob {
	t.Helper()
	body, err := json.Marshal(daemon.EnqueueRequest{
		RepoPath: repoPath,
		GitRef:   gitRef,
		Agent:    "codex",
	})
	require.NoError(t, err)
	client := endpoint.HTTPClient(5 * time.Second)
	resp, err := client.Post(endpoint.BaseURL()+"/api/enqueue", "application/json", bytes.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	var job storage.ReviewJob
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&job))
	return job
}

func getUpdateJob(
	t *testing.T, endpoint daemon.DaemonEndpoint, jobID int64,
) storage.ReviewJob {
	t.Helper()
	job, err := readUpdateJob(endpoint, jobID)
	require.NoError(t, err)
	return job
}

func readUpdateJob(
	endpoint daemon.DaemonEndpoint, jobID int64,
) (storage.ReviewJob, error) {
	client := endpoint.HTTPClient(5 * time.Second)
	resp, err := client.Get(fmt.Sprintf("%s/api/jobs?id=%d", endpoint.BaseURL(), jobID))
	if err != nil {
		return storage.ReviewJob{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return storage.ReviewJob{}, fmt.Errorf("get job: HTTP %d", resp.StatusCode)
	}
	var result struct {
		Jobs []storage.ReviewJob `json:"jobs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return storage.ReviewJob{}, err
	}
	if len(result.Jobs) != 1 {
		return storage.ReviewJob{}, fmt.Errorf("get job: got %d jobs", len(result.Jobs))
	}
	return result.Jobs[0], nil
}

func waitForUpdateJobStatus(
	t *testing.T,
	endpoint daemon.DaemonEndpoint,
	jobID int64,
	want storage.JobStatus,
	timeout time.Duration,
) {
	t.Helper()
	require.Eventually(t, func() bool {
		job, err := readUpdateJob(endpoint, jobID)
		return err == nil && job.Status == want
	}, timeout, 50*time.Millisecond)
}

func assertDaemonUpdateVersion(
	t *testing.T, endpoint daemon.DaemonEndpoint, want string,
) {
	t.Helper()
	info, err := daemon.ProbeDaemon(endpoint, 5*time.Second)
	require.NoError(t, err)
	assert.Equal(t, want, info.Version)
}
