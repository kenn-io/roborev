//go:build integration

package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/daemon"
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

	if !waitForDaemonReady(t, 10*time.Second, myPID) {
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

	// Capture output for debugging
	outputBuffer := new(bytes.Buffer)
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
	if !waitFor(t, 5*time.Second, func() bool {
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
	var sigCh chan os.Signal
	select {
	case sigCh = <-sigReady:
	case <-time.After(5 * time.Second):
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
