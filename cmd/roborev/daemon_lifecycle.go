package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	kitdaemon "go.kenn.io/kit/daemon"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/version"
)

var (
	// Polling intervals for waitForJob - exposed for testing
	pollStartInterval = 1 * time.Second
	pollMaxInterval   = 5 * time.Second

	// Update daemon restart controls - exposed for testing. The wait must
	// absorb slow Windows cold starts (antivirus rescans a freshly updated
	// binary) without falling into the force-kill path.
	updateRestartWaitTimeout  = 10 * time.Second
	updateRestartPollInterval = 200 * time.Millisecond

	// Probe retry controls for ensureDaemon - exposed for testing. A single
	// failed probe must not trigger a destructive kill-and-restart: the
	// daemon may be mid-startup or briefly too busy to answer.
	ensureProbeAttempts   = 3
	ensureProbeRetryDelay = 1 * time.Second

	// daemonStartTimeout bounds how long startDaemon waits for a spawned
	// daemon to become ready.
	daemonStartTimeout      = 15 * time.Second
	getAnyRunningDaemon     = daemon.GetAnyRunningDaemon
	listAllRuntimes         = daemon.ListAllRuntimes
	cleanupZombieDaemons    = daemon.CleanupZombieDaemons
	isPIDAliveForUpdate     = isPIDAliveForUpdateDefault
	restartDaemonForEnsure  = restartDaemon
	startDaemonForEnsure    = startDaemon
	stopDaemonForUpdate     = stopDaemon
	killAllDaemonsForUpdate = killAllDaemons
	startUpdatedDaemon      = func(binDir string) error {
		newBinary := filepath.Join(binDir, "roborev")
		if runtime.GOOS == "windows" {
			newBinary += ".exe"
		}
		return kitdaemon.StartDetached(context.Background(), kitdaemon.StartDetachedOptions{
			Executable: newBinary,
			Args:       []string{"daemon", "run"},
			Env:        filterGitEnv(os.Environ()),
		})
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

type detachedDaemonOptions struct {
	Executable      string
	Args            []string
	Env             []string
	Stdout          io.Writer
	Stderr          io.Writer
	RefuseEphemeral bool
}

// probeDaemonWithRetry probes ep several times before reporting failure, so
// transient unresponsiveness does not escalate into a daemon restart.
func probeDaemonWithRetry(ep daemon.DaemonEndpoint, timeout time.Duration) (*daemon.PingInfo, error) {
	var lastErr error
	for attempt := range ensureProbeAttempts {
		if attempt > 0 {
			time.Sleep(ensureProbeRetryDelay)
		}
		probe, err := daemon.ProbeDaemon(ep, timeout)
		if err == nil {
			return probe, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

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
			probe, err := probeDaemonWithRetry(info.Endpoint(), 2*time.Second)
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

	// Try the configured default address for manual daemon runs that do not
	// have a runtime file yet.
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

	// Legacy pre-kit daemons are invisible to kit discovery because they do
	// not serve /api/ping, but they can still hold the default port and DB.
	cleanupZombieDaemons(ep)

	// Start daemon in background
	return startDaemonForEnsure()
}

func startDaemon() error {
	if verbose {
		fmt.Println("Starting daemon...")
	}

	// Persist any pending queue-pause state before the daemon's workers start.
	// startDaemon runs only after any previous daemon has been stopped (restart)
	// or when none was running (cold start), so this never migrates a live DB.
	if pendingStartPause != nil {
		if err := writeLocalQueuePaused(*pendingStartPause); err != nil {
			return fmt.Errorf("persist initial queue pause state: %w", err)
		}
	}

	manager := kitdaemon.Manager{
		Store:    daemon.RuntimeStore(),
		Discover: daemon.DiscoverOptions(1 * time.Second),
		Start: func(ctx context.Context) error {
			exe, err := os.Executable()
			if err != nil {
				return fmt.Errorf("failed to find executable: %w", err)
			}
			stdout, stderr, closeLogs, err := openDetachedDaemonLogs()
			if err != nil {
				return err
			}
			defer closeLogs()
			return startDetachedDaemon(ctx, detachedDaemonOptions{
				Executable:      exe,
				Args:            []string{"daemon", "run"},
				Env:             filterGitEnv(os.Environ()),
				Stdout:          stdout,
				Stderr:          stderr,
				RefuseEphemeral: os.Getenv("ROBOREV_TEST_ALLOW_AUTOSTART") != "1",
			})
		},
	}
	if _, _, err := manager.Ensure(context.Background(), daemonStartTimeout); err != nil {
		return fmt.Errorf("failed to start daemon: %w", err)
	}
	return nil
}

func openDetachedDaemonLogs() (*os.File, *os.File, func(), error) {
	logDir := filepath.Join(config.DataDir(), "logs")
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		return nil, nil, nil, fmt.Errorf("create daemon log directory: %w", err)
	}
	stdout, err := os.OpenFile(filepath.Join(logDir, "daemon.stdout.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("open daemon stdout log: %w", err)
	}
	stderr, err := os.OpenFile(filepath.Join(logDir, "daemon.stderr.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		_ = stdout.Close()
		return nil, nil, nil, fmt.Errorf("open daemon stderr log: %w", err)
	}
	closeLogs := func() {
		_ = stdout.Close()
		_ = stderr.Close()
	}
	return stdout, stderr, closeLogs, nil
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
	killAllDaemonsPlatform()
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
