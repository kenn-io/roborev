package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/version"
)

var (
	// Polling intervals for waitForJob - exposed for testing
	pollStartInterval = 1 * time.Second
	pollMaxInterval   = 5 * time.Second

	// Update daemon restart controls - exposed for testing.
	updateRestartWaitTimeout  = 2 * time.Second
	updateRestartPollInterval = 200 * time.Millisecond
	getAnyRunningDaemon       = daemon.GetAnyRunningDaemon
	listAllRuntimes           = daemon.ListAllRuntimes
	isPIDAliveForUpdate       = isPIDAliveForUpdateDefault
	restartDaemonForEnsure    = restartDaemon
	stopDaemonForUpdate       = stopDaemon
	killAllDaemonsForUpdate   = killAllDaemons
	startUpdatedDaemon        = func(binDir string) error {
		newBinary := filepath.Join(binDir, "roborev")
		if runtime.GOOS == "windows" {
			newBinary += ".exe"
		}
		startCmd := exec.Command(newBinary, "daemon", "run")
		startCmd.Env = filterGitEnv(os.Environ())
		return startCmd.Start()
	}

	// setupSignalHandler allows tests to mock signal handling
	setupSignalHandler = func() (chan os.Signal, func()) {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, os.Interrupt)
		if runtime.GOOS != "windows" {
			// SIGTERM is not available on Windows
			signal.Notify(sigCh, os.Signal(syscall.Signal(15))) // SIGTERM
		}
		return sigCh, func() { signal.Stop(sigCh) }
	}
)

// ErrDaemonNotRunning indicates no daemon runtime file was found
var ErrDaemonNotRunning = fmt.Errorf("daemon not running (no runtime file found)")

// ErrJobNotFound indicates a job ID was not found during polling
var ErrJobNotFound = fmt.Errorf("job not found")

// parsedServerEndpoint caches the validated endpoint from the --server flag.
// Set once by validateServerFlag, read by getDaemonEndpoint.
var parsedServerEndpoint *daemon.DaemonEndpoint

func defaultDaemonEndpoint() daemon.DaemonEndpoint {
	return daemon.DaemonEndpoint{Network: "tcp", Address: "127.0.0.1:7373"}
}

func fallbackDaemonEndpoint() daemon.DaemonEndpoint {
	exe, err := os.Executable()
	if err == nil && shouldRefuseAutoStartDaemon(exe) {
		return daemon.DaemonEndpoint{Network: "tcp", Address: "127.0.0.1:1"}
	}
	return defaultDaemonEndpoint()
}

// validateServerFlag parses and validates the --server flag value.
// Called from PersistentPreRunE so invalid values fail fast.
func validateServerFlag() error {
	parsedServerEndpoint = nil
	if serverAddr == "" {
		return nil
	}
	ep, err := daemon.ParseEndpoint(serverAddr)
	if err != nil {
		return fmt.Errorf("invalid --server address %q: %w", serverAddr, err)
	}
	parsedServerEndpoint = &ep
	return nil
}

// getDaemonEndpoint returns the daemon endpoint from runtime file or config.
// An explicit --server flag takes precedence over auto-discovered daemons.
func getDaemonEndpoint() daemon.DaemonEndpoint {
	// Explicit --server flag takes precedence over auto-discovery
	if serverAddr != "" {
		if parsedServerEndpoint != nil {
			return *parsedServerEndpoint
		}
		ep, err := daemon.ParseEndpoint(serverAddr)
		if err != nil {
			return fallbackDaemonEndpoint()
		}
		return ep
	}
	// No explicit flag: discover running daemon
	if info, err := getAnyRunningDaemon(); err == nil {
		return info.Endpoint()
	}
	// Nothing running: use default
	return fallbackDaemonEndpoint()
}

// getDaemonHTTPClient returns an HTTP client configured for the daemon endpoint.
func getDaemonHTTPClient(timeout time.Duration) *http.Client {
	return getDaemonEndpoint().HTTPClient(timeout)
}

// registerRepoError is a server-side error from the register endpoint
// (daemon reachable but returned non-200). Distinguished from connection
// errors so callers can report appropriately.
type registerRepoError struct {
	StatusCode int
	Body       string
}

func (e *registerRepoError) Error() string {
	return fmt.Sprintf("server returned %d: %s", e.StatusCode, e.Body)
}

// isTransportError returns true if err indicates a transport-level failure
// (connection refused, timeout, DNS resolution, etc.) where the daemon is
// likely not reachable. Returns false for malformed URLs, TLS config errors,
// and other non-transport url.Error cases that deserve explicit reporting.
func isTransportError(err error) bool {
	var urlErr *url.Error
	if !errors.As(err, &urlErr) {
		return false
	}
	// Check if the underlying error is a net-level transport failure
	var opErr *net.OpError
	if errors.As(urlErr.Err, &opErr) {
		return true
	}
	// Also catch net.Error (timeout interface) that isn't wrapped in OpError
	var netErr net.Error
	return errors.As(urlErr.Err, &netErr)
}

// registerRepo tells the daemon to persist a repo to the DB so that the
// CI poller (and other components) can find it after a daemon restart.
func registerRepo(repoPath string) error {
	body, err := json.Marshal(map[string]string{"repo_path": repoPath})
	if err != nil {
		return err
	}
	ep := getDaemonEndpoint()
	resp, err := ep.HTTPClient(5*time.Second).Post(ep.BaseURL()+"/api/repos/register", "application/json", bytes.NewReader(body))
	if err != nil {
		return err // connection error (*url.Error wrapping net.Error)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(resp.Body)
		return &registerRepoError{StatusCode: resp.StatusCode, Body: string(msg)}
	}
	return nil
}

// ensureDaemon checks if daemon is running, starts it if not.
// If daemon is running but has different version, restart it.
// Set ROBOREV_SKIP_VERSION_CHECK=1 to accept any daemon version without
// restarting (useful for development with go run).
func ensureDaemon() error {
	skipVersionCheck := os.Getenv("ROBOREV_SKIP_VERSION_CHECK") == "1"

	// First check runtime files for any running daemon
	if info, err := getAnyRunningDaemon(); err == nil {
		if !skipVersionCheck {
			probe, err := daemon.ProbeDaemon(info.Endpoint(), 2*time.Second)
			if err != nil {
				if verbose {
					fmt.Printf("Daemon probe failed, restarting...\n")
				}
				return restartDaemonForEnsure()
			}
			daemonVersion := probe.Version
			if daemonVersion == "" {
				if verbose {
					fmt.Printf("Daemon version unknown, restarting...\n")
				}
				return restartDaemonForEnsure()
			}
			if daemonVersion != version.Version {
				if verbose {
					fmt.Printf("Daemon version mismatch (daemon: %s, cli: %s), restarting...\n", daemonVersion, version.Version)
				}
				return restartDaemonForEnsure()
			}
		}

		return nil
	}

	// Try the configured default address for manual/legacy daemon runs that do
	// not have a runtime file yet.
	ep := getDaemonEndpoint()
	if probe, err := daemon.ProbeDaemon(ep, 2*time.Second); err == nil {
		if !skipVersionCheck {
			if probe.Version == "" {
				if verbose {
					fmt.Printf("Daemon version unknown, restarting...\n")
				}
				return restartDaemonForEnsure()
			}
			if probe.Version != version.Version {
				if verbose {
					fmt.Printf("Daemon version mismatch (daemon: %s, cli: %s), restarting...\n", probe.Version, version.Version)
				}
				return restartDaemonForEnsure()
			}
		}
		return nil
	}

	// Start daemon in background
	return startDaemon()
}

func startDaemon() error {
	if verbose {
		fmt.Println("Starting daemon...")
	}

	// Use the current executable with "daemon run" subcommand
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to find executable: %w", err)
	}
	if shouldRefuseAutoStartDaemon(exe) {
		return fmt.Errorf(
			"refusing to auto-start daemon from ephemeral binary (%s); "+
				"use the installed roborev binary instead",
			filepath.Base(exe),
		)
	}

	cmd := exec.Command(exe, "daemon", "run")
	cmd.Env = filterGitEnv(os.Environ())
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start daemon: %w", err)
	}

	// Wait for daemon to publish a responsive runtime entry.
	for range 30 {
		time.Sleep(100 * time.Millisecond)
		if _, err := daemon.GetAnyRunningDaemon(); err == nil {
			return nil
		}
	}

	return fmt.Errorf("daemon failed to start")
}

// stopDaemon stops any running daemons.
// Returns ErrDaemonNotRunning if no daemon runtime files are found.
func stopDaemon() error {
	runtimes, err := daemon.ListAllRuntimes()
	if err != nil {
		// Check if it's just a "not exist" type error
		if os.IsNotExist(err) {
			return ErrDaemonNotRunning
		}
		// Propagate other errors (permission, IO, etc.)
		return fmt.Errorf("failed to list daemon runtimes: %w", err)
	}
	if len(runtimes) == 0 {
		return ErrDaemonNotRunning
	}

	// Kill all found daemons, track failures
	var lastErr error
	for _, info := range runtimes {
		if !daemon.KillDaemon(info) {
			lastErr = fmt.Errorf("failed to kill daemon (pid %d)", info.PID)
		}
	}

	return lastErr
}

// killAllDaemons kills any roborev daemon processes that might be running
// This handles orphaned processes from old binaries or crashed restarts
func killAllDaemons() {
	if runtime.GOOS == "windows" {
		// On Windows, use wmic to find daemon processes by command line
		// and kill only those running "daemon run"
		_ = exec.Command("wmic", "process", "where",
			"commandline like '%roborev%daemon%run%'",
			"call", "terminate").Run()
	} else {
		// On Unix, use pkill to kill all roborev daemon processes
		// Use -f to match against full command line
		_ = exec.Command("pkill", "-f", "roborev daemon run").Run()
		time.Sleep(100 * time.Millisecond)
		// Force kill any remaining
		_ = exec.Command("pkill", "-9", "-f", "roborev daemon run").Run()
	}
	time.Sleep(200 * time.Millisecond)
}

// restartDaemon stops the running daemon and starts a new one
func restartDaemon() error {
	_ = stopDaemon() // Ignore error - killAllDaemons is the fallback
	// Also kill any orphaned daemon processes from old binaries
	killAllDaemons()

	// Checkpoint WAL to ensure clean state for new daemon
	// Retry a few times in case daemon hasn't fully released the DB
	if dbPath := storage.DefaultDBPath(); dbPath != "" {
		var lastErr error
		for range 3 {
			db, err := storage.Open(dbPath)
			if err != nil {
				lastErr = err
				time.Sleep(200 * time.Millisecond)
				continue
			}
			if _, err := db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
				lastErr = err
				db.Close()
				time.Sleep(200 * time.Millisecond)
				continue
			}
			db.Close()
			lastErr = nil
			break
		}
		if lastErr != nil && verbose {
			fmt.Printf("Warning: WAL checkpoint failed: %v\n", lastErr)
		}
	}

	return startDaemon()
}
