package main

import (
	"context"
	"encoding/json/v2"
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
	"strings"
	"syscall"
	"time"

	"github.com/cenkalti/backoff/v7"
	kitdaemon "go.kenn.io/kit/daemon"

	"go.kenn.io/roborev/internal/auth"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/version"
	roborevclient "go.kenn.io/roborev/pkg/client"
)

// lifecycleOut keeps daemon diagnostics separate from command and protocol output.
var lifecycleOut io.Writer = os.Stderr

var (
	// Polling intervals for waitForJob - exposed for testing
	pollStartInterval = 1 * time.Second
	pollMaxInterval   = 5 * time.Second

	// Update daemon restart controls - exposed for testing. The wait must
	// absorb slow Windows cold starts (antivirus rescans a freshly updated
	// binary) before reporting that manual intervention is needed.
	updateRestartWaitTimeout  = 10 * time.Second
	updateRestartPollInterval = 200 * time.Millisecond

	// Probe retry controls for ensureDaemon - exposed for testing. A single
	// failed probe must not trigger an unnecessary restart: the
	// daemon may be mid-startup or briefly too busy to answer.
	ensureProbeAttempts   = 3
	ensureProbeRetryDelay = 1 * time.Second
	probeDaemonForEnsure  = probeDaemonWithRetry

	// daemonStartTimeout bounds how long startDaemon waits for a spawned
	// daemon to become ready. Database initialization can exceed the former
	// 15-second budget (SQLite alone permits a 30-second busy wait). Match the
	// slow-start budget in TestDaemonLifecycleEndToEnd.
	daemonStartTimeout          = 2 * time.Minute
	getAnyRunningDaemon         = daemon.GetAnyRunningDaemon
	getAnyRunningDaemonForStart = daemon.GetAnyRunningDaemonContext
	listAllRuntimes             = daemon.ListAllRuntimes
	cleanupZombieDaemons        = daemon.CleanupZombieDaemons
	isPIDAliveForUpdate         = isPIDAliveForUpdateDefault
	restartDaemonForEnsure      = restartDaemon
	startDaemonForEnsure        = startDaemon
	startDaemonDetached         = startDetachedDaemon
	stopDaemonForRestart        = stopDaemon
	startDaemonAfterRestart     = startDaemon
	stopDaemonForUpdate         = stopDaemon
	startUpdatedDaemon          = func(binDir string) error {
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
	probe, err := backoff.Retry(context.Background(), func() (*daemon.PingInfo, error) {
		return daemon.ProbeDaemon(ep, timeout)
	}, backoff.WithBackOff(backoff.NewConstantBackOff(ensureProbeRetryDelay)),
		backoff.WithMaxTries(uint(ensureProbeAttempts)), backoff.WithMaxElapsedTime(0))
	if err != nil {
		return nil, backoff.AsRetryError(err).LastErr
	}
	return probe, nil
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
	} else if daemon.IsDaemonAccessError(err) {
		return fallbackDaemonEndpoint().WithAccessError(err)
	}
	// Nothing running: use default
	return fallbackDaemonEndpoint()
}

// getDaemonHTTPClientForURL pairs address-based helpers with the URL they use.
// The selected Unix transport and terminal discovery errors stay attached when
// the URL identifies the selected endpoint. Other loopback TCP URLs get their
// own scoped client, including URLs chosen after daemon recovery.
func getDaemonHTTPClientForURL(baseURL string, timeout time.Duration) *http.Client {
	ep := getDaemonEndpoint()
	if baseURL == ep.BaseURL() {
		return ep.HTTPClient(timeout)
	}
	origin, err := url.Parse(baseURL)
	if err == nil && origin.Scheme == "http" && origin.Host != "" && origin.User == nil {
		if target, err := daemon.ParseEndpoint(origin.Host); err == nil {
			return target.HTTPClient(timeout)
		}
	}
	return auth.HTTPClient(baseURL, &http.Client{Timeout: timeout}, func() (string, error) {
		return "", fmt.Errorf("%w: invalid daemon API URL", daemon.ErrDaemonAccessDenied)
	})
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
	if _, ok := errors.AsType[*net.OpError](urlErr.Err); ok {
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
	resp, err := ep.APIClient(5*time.Second).RegisterRepoRaw(context.Background(), nil, roborevclient.WithBody(body))
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
	// An explicit endpoint is caller-managed. The command reports its request
	// error without discovering, starting, or restarting a local daemon.
	if serverAddr != "" {
		ep, err := daemon.ParseEndpoint(serverAddr)
		if err != nil {
			return fmt.Errorf("invalid --server address %q: %w", serverAddr, err)
		}
		_, err = daemon.ProbeDaemon(ep, 2*time.Second)
		return err
	}
	skipVersionCheck := os.Getenv("ROBOREV_SKIP_VERSION_CHECK") == "1"

	// First check runtime files for any running daemon
	info, discoveryErr := getAnyRunningDaemon()
	if daemon.IsDaemonAccessError(discoveryErr) {
		return discoveryErr
	}
	if discoveryErr == nil {
		if !skipVersionCheck {
			probe, err := probeDaemonForEnsure(info.Endpoint(), 2*time.Second)
			if err != nil {
				if daemon.IsDaemonAccessError(err) {
					return fmt.Errorf("probe daemon: %w", err)
				}
				if verbose {
					fmt.Fprintf(lifecycleOut, "Daemon probe failed, restarting...\n")
				}
				return restartDaemonForEnsure()
			}
			daemonVersion := probe.Version
			if daemonVersion == "" {
				if verbose {
					fmt.Fprintf(lifecycleOut, "Daemon version unknown, restarting...\n")
				}
				return restartDaemonForEnsure()
			}
			if daemonVersion != version.Version {
				if verbose {
					fmt.Fprintf(lifecycleOut, "Daemon version mismatch (daemon: %s, cli: %s), restarting...\n", daemonVersion, version.Version)
				}
				return restartDaemonForEnsure()
			}
		}

		return nil
	}

	// Try the configured default address for manual daemon runs that do not
	// have a runtime file yet.
	ep := getDaemonEndpoint()
	probe, probeErr := probeDaemonForEnsure(ep, 2*time.Second)
	if probeErr == nil {
		if !skipVersionCheck {
			if probe.Version == "" {
				if verbose {
					fmt.Fprintf(lifecycleOut, "Daemon version unknown, restarting...\n")
				}
				return restartDaemonForEnsure()
			}
			if probe.Version != version.Version {
				if verbose {
					fmt.Fprintf(lifecycleOut, "Daemon version mismatch (daemon: %s, cli: %s), restarting...\n", probe.Version, version.Version)
				}
				return restartDaemonForEnsure()
			}
		}
		return nil
	}
	if daemon.IsDaemonAccessError(probeErr) {
		return fmt.Errorf("probe daemon: %w", probeErr)
	}

	// Legacy pre-kit daemons are invisible to kit discovery because they do
	// not serve /api/ping, but they can still hold the default port and DB.
	cleanupZombieDaemons(ep)

	// Start daemon in background
	return startDaemonForEnsure()
}

func startDaemon() error {
	if verbose {
		fmt.Fprintln(lifecycleOut, "Starting daemon...")
	}

	var startupLogPath string
	var startupLogOffset int64
	manager := kitdaemon.Manager{
		Store: daemon.RuntimeStore(),
		FindFunc: func(ctx context.Context) (kitdaemon.RuntimeRecord, kitdaemon.PingInfo, bool, error) {
			info, err := getAnyRunningDaemonForStart(ctx)
			if errors.Is(err, os.ErrNotExist) {
				return kitdaemon.RuntimeRecord{}, kitdaemon.PingInfo{}, false, nil
			}
			if err != nil {
				return kitdaemon.RuntimeRecord{}, kitdaemon.PingInfo{}, false, err
			}
			return kitdaemon.RuntimeRecord{
				PID: info.PID, Network: info.Network, Address: info.Address,
				Service: info.Service, Version: info.Version,
			}, kitdaemon.PingInfo{OK: true, PID: info.PID, Service: info.Service, Version: info.Version}, true, nil
		},
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
			startupLogOffset, err = stderr.Seek(0, io.SeekEnd)
			if err != nil {
				return fmt.Errorf("locate daemon startup log: %w", err)
			}
			args := []string{"daemon", "run"}
			if pendingStartPause != nil {
				args = append(args, fmt.Sprintf("--queue-paused=%t", *pendingStartPause))
			}
			if err := startDaemonDetached(ctx, detachedDaemonOptions{
				Executable:      exe,
				Args:            args,
				Env:             filterGitEnv(os.Environ()),
				Stdout:          stdout,
				Stderr:          stderr,
				RefuseEphemeral: os.Getenv("ROBOREV_TEST_ALLOW_AUTOSTART") != "1",
			}); err != nil {
				return err
			}
			startupLogPath = stderr.Name()
			started := time.Now()
			progress := time.NewTicker(15 * time.Second)
			defer progress.Stop()

			for {
				ready, err := discoverDaemonForStart(ctx)
				if err != nil {
					return err
				}
				if ready {
					return nil
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-progress.C:
					fmt.Fprintf(lifecycleOut, "Still waiting for daemon startup (%s elapsed; timeout %s).%s\n",
						time.Since(started).Round(time.Second), daemonStartTimeout,
						daemonStartupLogSummary(startupLogPath, startupLogOffset))
				case <-time.After(50 * time.Millisecond):
				}
			}
		},
	}
	if _, _, err := manager.Ensure(context.Background(), daemonStartTimeout); err != nil {
		if errors.Is(err, context.DeadlineExceeded) && startupLogPath != "" {
			return fmt.Errorf("failed to start daemon: %w\nDaemon did not become ready within %s; it may still be initializing.%s",
				err, daemonStartTimeout, daemonStartupLogSummary(startupLogPath, startupLogOffset))
		}
		return fmt.Errorf("failed to start daemon: %w", err)
	}
	return nil
}

// daemonStartupLogSummary excludes log entries from earlier startup attempts.
func daemonStartupLogSummary(path string, offset int64) string {
	summary := "\nStartup log: " + path
	file, err := os.Open(path)
	if err != nil {
		return summary
	}
	defer file.Close()
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return summary
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return summary
	}
	if last := strings.TrimSpace(string(data)); last != "" {
		summary += "\nLatest startup message: " + last[strings.LastIndexByte(last, '\n')+1:]
	}
	return summary
}

func discoverDaemonForStart(ctx context.Context) (bool, error) {
	_, err := getAnyRunningDaemonForStart(ctx)
	if err == nil {
		return true, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	return false, nil
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
		fmt.Fprintln(
			os.Stderr,
			"Waiting for daemon shutdown; no new reviews will start, and any running reviews will finish first...",
		)
		if err := daemon.KillDaemon(info); err != nil {
			lastErr = fmt.Errorf("failed to stop daemon (pid %d): %w", info.PID, err)
			if errors.Is(err, daemon.ErrDaemonAccessDenied) {
				lastErr = fmt.Errorf("%w; restore the daemon's startup auth_key or stop PID %d manually", lastErr, info.PID)
			}
		}
	}

	return lastErr
}

// restartDaemon stops the running daemon and starts a new one
func restartDaemon() error {
	if err := stopDaemonForRestart(); err != nil &&
		!errors.Is(err, ErrDaemonNotRunning) {
		return err
	}

	return startDaemonAfterRestart()
}
