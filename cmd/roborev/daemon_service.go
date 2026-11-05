package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/cenkalti/backoff/v7"

	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/version"
)

type daemonService struct {
	unit, executable string
	mainPID          int
	// Recovery refuses these unit settings before draining.
	serviceType                string
	remainAfterExit, transient bool
}

type managedRecoveryError struct {
	unit  string
	cause error
}

func (e *managedRecoveryError) Error() string {
	return fmt.Sprintf("%s %s: %v", serviceDescription, e.unit, e.cause)
}
func (e *managedRecoveryError) Unwrap() error { return e.cause }

var (
	restartThroughService = false
	serviceDescription    = "systemd service"
	serviceRestartCommand = "systemctl --user restart"
	serviceStartCommand   = "systemctl --user start"
	cliExecutableInfo     = func() (os.FileInfo, error) { return os.Stat("/proc/self/exe") }

	errRecoveryLock = errors.New("wait for daemon recovery lock")

	findDaemonService  = lookupDaemonService
	daemonCgroupUnit   = serviceCgroup
	serviceMainPID     = userServiceMainPID
	startDaemonService = startUserService
	drainManagedDaemon = daemon.KillDaemon
)

// supported reports unit settings that a service start cannot replace safely.
func (s *daemonService) supported() error {
	switch {
	case s.transient:
		return fmt.Errorf("transient units require manual restart; run %s %s", serviceRestartCommand, s.unit)
	case s.remainAfterExit:
		return fmt.Errorf("RemainAfterExit=yes requires manual restart; run %s %s", serviceRestartCommand, s.unit)
	case !slices.Contains([]string{"simple", "exec", "notify"}, s.serviceType):
		return fmt.Errorf("unsupported service Type=%s; run %s %s", s.serviceType, serviceRestartCommand, s.unit)
	}
	return nil
}

// keepReason explains why the service manager cannot replace a responsive daemon in
// place, or returns "" when it can.
func (s *daemonService) keepReason(pid int) string {
	if s.mainPID != pid {
		// A wrapper child stays in use; the service manager cannot replace it directly.
		return fmt.Sprintf("%s %s runs the daemon as a child process", serviceDescription, s.unit)
	}
	same, err := s.sameInstallation()
	if err != nil {
		return fmt.Sprintf("%s %s: cannot confirm executable %s: %v", serviceDescription, s.unit, s.executable, err)
	}
	if !same {
		return fmt.Sprintf("%s %s uses another installation (%s)", serviceDescription, s.unit, s.executable)
	}
	if err := s.supported(); err != nil {
		return (&managedRecoveryError{s.unit, err}).Error()
	}
	return ""
}

func pauseServiceBeforeRecovery(ctx context.Context, info *daemon.RuntimeInfo, unit string) error {
	resp, err := info.Endpoint().APIClient(0).PauseQueueWithResponse(ctx)
	if resp != nil && resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("daemon lacks queue-pause support; recovery stopped before shutdown; wait for active reviews to finish, then run %s %s and retry roborev pause", serviceRestartCommand, unit)
	}
	return err
}

// sameInstallation follows the configured ExecStart path to the installed binary.
func (s *daemonService) sameInstallation() (bool, error) {
	selected, err := os.Stat(s.executable)
	if err != nil {
		return false, err
	}
	cli, err := cliExecutableInfo()
	if err != nil {
		return false, err
	}
	return os.SameFile(selected, cli), nil
}

// acquireRecoveryLock lets waiting commands share the active review drain.
// The holder drains without a deadline, so waiters wait as long as it does
// and then reuse its replacement instead of failing mid-drain.
func acquireRecoveryLock(ctx context.Context) (func(), error) {
	store := daemon.RuntimeStore()
	release, acquired, err := store.TryAcquireStartLock(ctx)
	if err != nil || acquired {
		return release, err
	}
	fmt.Fprintln(lifecycleOut, "Waiting for daemon recovery lock; another command may be waiting for running reviews to finish...")
	return store.AcquireStartLock(ctx)
}

// snapshotDaemonService treats a vanished PID as unmanaged when it ran
// outside every user service at discovery, or when no user manager could
// restart it, so recovery need not wait for a replacement.
func snapshotDaemonService(ctx context.Context, pid int, outsideService bool) (*daemonService, error) {
	owner, err := findDaemonService(ctx, pid)
	if errors.Is(err, os.ErrNotExist) && (outsideService || !userManagerAvailable(ctx)) {
		return nil, nil
	}
	return owner, err
}

// discoveredOutsideService reports whether a just-discovered daemon PID runs
// outside every user service. Callers read it before slow probes, while the
// PID is known to be alive.
func discoveredOutsideService(pid int) bool {
	unit, err := daemonCgroupUnit(pid)
	return err == nil && unit == ""
}

func recoverDaemon(info *daemon.RuntimeInfo, probeErr error, outsideService bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), daemonStartTimeout)
	snapshot, snapshotErr := snapshotDaemonService(ctx, info.PID, outsideService)
	cancel()
	if probeErr == nil && snapshotErr == nil && snapshot != nil {
		// Keeping a responsive daemon changes nothing, so it needs no lock.
		if reason := snapshot.keepReason(info.PID); reason != "" {
			fmt.Fprintf(lifecycleOut, "Daemon recovery skipped; %s\n", reason)
			return nil
		}
	}
	release, err := acquireRecoveryLock(context.Background())
	if err != nil {
		return fmt.Errorf("%w: %w", errRecoveryLock, err)
	}
	defer func() {
		if release != nil {
			release()
		}
	}()
	ctx, cancel = context.WithTimeout(context.Background(), daemonStartTimeout)
	defer cancel()
	current, err := findDaemonForStart(ctx)
	if err != nil {
		return fmt.Errorf("rediscover daemon: %w", err)
	}
	if current == nil && errors.Is(snapshotErr, os.ErrNotExist) {
		// The old PID disappeared before its owner could be captured.
		// A user service may still be waiting for RestartSec to elapse.
		current, err = backoff.Retry(ctx, func() (*daemon.RuntimeInfo, error) {
			candidate, err := findDaemonForStart(ctx)
			if err != nil {
				return nil, backoff.Permanent(err)
			}
			if candidate == nil {
				return nil, os.ErrNotExist
			}
			return candidate, nil
		}, backoff.WithBackOff(backoff.NewConstantBackOff(50*time.Millisecond)), backoff.WithMaxElapsedTime(0))
		if err != nil {
			return &managedRecoveryError{"unknown", fmt.Errorf("daemon ownership uncertain after PID %d disappeared: %w", info.PID, err)}
		}
	}
	responsive := current != nil
	if responsive && current.PID == info.PID && probeErr != nil {
		// Discovery retains the runtime record's version. Confirm the live
		// version before reusing a daemon whose earlier probe failed.
		probe, err := probeDaemonForEnsure(current.Endpoint(), 2*time.Second)
		if daemon.IsDaemonAccessError(err) {
			return err
		}
		if err == nil && probe.Version == version.Version {
			return nil
		}
	}
	if responsive && current.PID != info.PID && current.Version == version.Version {
		return nil
	}
	if responsive {
		probeErr = nil
	} else {
		current = info
		if probeErr == nil {
			probeErr = os.ErrNotExist
		}
	}
	owner, err := findDaemonService(ctx, current.PID)
	vanished := !responsive && errors.Is(err, os.ErrNotExist)
	if vanished {
		// The snapshot identifies the unit only when no daemon replaced this one.
		owner, err = snapshot, snapshotErr
	}
	if err != nil {
		if responsive {
			fmt.Fprintf(lifecycleOut, "Daemon recovery skipped; %v\n", err)
			return nil
		}
		return err
	}
	if owner == nil {
		if err := stopDaemonRuntimes(listAllRuntimes, drainManagedDaemon, lifecycleOut); err != nil && !errors.Is(err, ErrDaemonNotRunning) {
			return err
		}
		release()
		release = nil
		return startDaemonForEnsure()
	}
	fail := func(err error) error { return &managedRecoveryError{owner.unit, err} }
	if responsive {
		if reason := owner.keepReason(current.PID); reason != "" {
			fmt.Fprintf(lifecycleOut, "Daemon recovery skipped; %s\n", reason)
			return nil
		}
	} else {
		if owner.mainPID != current.PID {
			return fail(fmt.Errorf("service child requires attention: %w; run %s %s", probeErr, serviceRestartCommand, owner.unit))
		}
		same, err := owner.sameInstallation()
		if err != nil {
			return fail(fmt.Errorf("cannot confirm executable %s: %w", owner.executable, err))
		}
		if !same {
			return fail(fmt.Errorf("installation %s requires attention: %w; run %s %s", owner.executable, probeErr, serviceRestartCommand, owner.unit))
		}
		if err := owner.supported(); err != nil {
			return fail(err)
		}
	}
	// Discovery can miss a daemon that later answers shutdown retries.
	// Persist pause before any drain or service start, or leave it alone.
	if pendingStartPause != nil && *pendingStartPause {
		if vanished {
			return fail(fmt.Errorf("pause queue before shutdown: daemon PID %d has exited; run %s %s, then retry roborev pause", current.PID, serviceStartCommand, owner.unit))
		}
		if err := pauseServiceBeforeRecovery(ctx, current, owner.unit); err != nil {
			return fail(fmt.Errorf("pause queue before shutdown: %w", err))
		}
	}
	if !vanished {
		fmt.Fprintln(lifecycleOut, "Waiting for daemon shutdown; no new reviews will start, and any running reviews will finish first...")
		if err := drainManagedDaemon(current); err != nil {
			return fail(err)
		}
	}
	_, err = startServiceReplacement(context.Background(), owner)
	return err
}

func startServiceReplacement(ctx context.Context, owner *daemonService) (*daemon.RuntimeInfo, error) {
	fail := func(err error) error { return &managedRecoveryError{owner.unit, err} }
	// Reviews may take longer than startup. Give the replacement a fresh budget.
	readyCtx, readyCancel := context.WithTimeout(ctx, daemonStartTimeout)
	defer readyCancel()
	// Start leaves a socket-activated replacement running.
	if err := startDaemonService(readyCtx, owner.unit); err != nil {
		return nil, fail(err)
	}
	info, err := awaitServiceReplacement(readyCtx, owner.unit)
	if err != nil {
		return nil, fail(err)
	}
	return info, nil
}

// awaitServiceReplacement polls until a responsive daemon is the unit's main
// process. Unit startup can precede runtime publication.
func awaitServiceReplacement(ctx context.Context, unit string) (*daemon.RuntimeInfo, error) {
	return backoff.Retry(ctx, func() (*daemon.RuntimeInfo, error) {
		replacement, err := getAnyRunningDaemonForStart(ctx)
		if err != nil {
			err = fmt.Errorf("replacement readiness: %w", err)
			if !errors.Is(err, os.ErrNotExist) {
				err = backoff.Permanent(err)
			}
			return nil, err
		}
		mainPID, err := serviceMainPID(ctx, unit)
		if err != nil {
			return nil, backoff.Permanent(err)
		}
		if mainPID != replacement.PID {
			return nil, fmt.Errorf("replacement is outside service %s", unit)
		}
		return replacement, nil
	}, backoff.WithBackOff(backoff.NewConstantBackOff(50*time.Millisecond)),
		backoff.WithMaxElapsedTime(0))
}

// restartService checks ownership before an explicit restart shuts down a daemon.
func restartService(ctx context.Context, discover func() ([]*daemon.RuntimeInfo, error)) (owners []*daemonService, runtimes []*daemon.RuntimeInfo, release func(), err error) {
	release, err = acquireRecoveryLock(ctx)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("%w: %w", errRecoveryLock, err)
	}
	defer func() {
		if err != nil || len(owners) == 0 {
			release()
			release = nil
		}
	}()
	runtimes, err = discover()
	if err != nil {
		return nil, nil, release, err
	}
	seen := make(map[string]bool)
	for _, info := range runtimes {
		if info.PID <= 0 || info.Stale() {
			continue
		}
		candidate, err := findDaemonService(ctx, info.PID)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, nil, release, err
		}
		if candidate == nil {
			continue
		}
		if reason := candidate.keepReason(info.PID); reason != "" {
			hint := fmt.Sprintf("run %s %s", serviceRestartCommand, candidate.unit)
			if strings.Contains(reason, hint) {
				return nil, nil, release, errors.New(reason)
			}
			return nil, nil, release, fmt.Errorf("%s; %s", reason, hint)
		}
		if !seen[candidate.unit] {
			owners = append(owners, candidate)
			seen[candidate.unit] = true
		}
	}
	return owners, runtimes, release, nil
}
