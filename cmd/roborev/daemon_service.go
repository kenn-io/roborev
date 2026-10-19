package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
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
	return fmt.Sprintf("systemd service %s: %v", e.unit, e.cause)
}
func (e *managedRecoveryError) Unwrap() error { return e.cause }

var (
	findDaemonService  = lookupDaemonService
	serviceMainPID     = userServiceMainPID
	startDaemonService = startUserService
	drainManagedDaemon = daemon.KillDaemon
	// errUserManagerUnavailable leaves a responsive daemon in use without recovery.
	errUserManagerUnavailable = errors.New("user service manager unavailable")
)

// supported reports unit settings that systemctl start cannot replace safely.
func (s *daemonService) supported() error {
	switch {
	case s.transient:
		return errors.New("transient units require manual restart")
	case s.remainAfterExit:
		return errors.New("RemainAfterExit=yes requires manual restart")
	case !slices.Contains([]string{"simple", "exec", "notify"}, s.serviceType):
		return fmt.Errorf("unsupported service Type=%s", s.serviceType)
	}
	return nil
}

func (s *daemonService) sameInstallation() (bool, error) {
	selected, err := os.Stat(s.executable)
	if err != nil {
		return false, err
	}
	cli, err := os.Stat("/proc/self/exe")
	if err != nil {
		return false, err
	}
	cliPath, _ := os.Executable()
	return os.SameFile(selected, cli) || filepath.Clean(s.executable) == filepath.Clean(cliPath), nil
}

// acquireRecoveryLock lets waiting commands share the active review drain.
func acquireRecoveryLock() (func(), error) {
	store := daemon.RuntimeStore()
	release, acquired, err := store.TryAcquireStartLock(context.Background())
	if err != nil || acquired {
		return release, err
	}
	fmt.Fprintln(lifecycleOut, "Waiting for daemon recovery lock; another command may be waiting for running reviews to finish...")
	return store.AcquireStartLock(context.Background())
}

func recoverDaemon(info *daemon.RuntimeInfo, probeErr error) error {
	// Only a daemon that answered the version probe receives a pre-drain pause.
	versionAnswered := probeErr == nil
	ctx, cancel := context.WithTimeout(context.Background(), daemonStartTimeout)
	snapshot, snapshotErr := findDaemonService(ctx, info.PID)
	cancel()
	release, err := acquireRecoveryLock()
	if err != nil {
		return err
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
	responsive := current != nil
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
		if errors.Is(err, os.ErrNotExist) {
			err = nil
		}
	}
	if responsive && errors.Is(err, errUserManagerUnavailable) {
		if verbose {
			fmt.Fprintf(lifecycleOut, "Daemon recovery skipped; %v\n", err)
		}
		return nil
	}
	if err != nil {
		return err
	}
	if responsive && current.PID != info.PID && current.Version == version.Version {
		return nil
	}
	if owner == nil {
		if !vanished {
			if err := drainManagedDaemon(current); err != nil {
				return err
			}
		}
		release()
		release = nil
		return startDaemonForEnsure()
	}
	fail := func(err error) error { return &managedRecoveryError{owner.unit, err} }
	if owner.mainPID != current.PID {
		// A wrapper child stays in use; systemd cannot replace it directly.
		if responsive {
			return nil
		}
		return fail(fmt.Errorf("service child requires attention: %w", probeErr))
	}
	same, err := owner.sameInstallation()
	if err != nil {
		return fail(fmt.Errorf("cannot confirm executable %s: %w", owner.executable, err))
	}
	if !same {
		if probeErr != nil {
			return fail(fmt.Errorf("installation %s requires attention: %w", owner.executable, probeErr))
		}
		return nil
	}
	if err := owner.supported(); err != nil {
		return fail(err)
	}
	if !vanished {
		// Replacement workers can start before a later API request reaches SQLite.
		if pendingStartPause != nil && *pendingStartPause && versionAnswered && responsive {
			if _, err := current.Endpoint().APIClient(0).PauseQueue(ctx); err != nil {
				return fail(fmt.Errorf("pause queue before shutdown: %w", err))
			}
		}
		fmt.Fprintln(lifecycleOut, "Waiting for daemon shutdown; no new reviews will start, and any running reviews will finish first...")
		if err := drainManagedDaemon(current); err != nil {
			return fail(err)
		}
	}
	// Reviews may take longer than startup. Give the replacement a fresh budget.
	readyCtx, readyCancel := context.WithTimeout(context.Background(), daemonStartTimeout)
	defer readyCancel()
	// Start leaves a socket-activated replacement running.
	if err := startDaemonService(readyCtx, owner.unit); err != nil {
		return fail(err)
	}
	if err := awaitServiceReplacement(readyCtx, owner.unit); err != nil {
		return fail(err)
	}
	return nil
}

// awaitServiceReplacement polls until a responsive daemon is the unit's main
// process. Unit startup can precede runtime publication.
func awaitServiceReplacement(ctx context.Context, unit string) error {
	_, err := backoff.Retry(ctx, func() (struct{}, error) {
		replacement, err := getAnyRunningDaemonForStart(ctx)
		if err != nil {
			err = fmt.Errorf("replacement readiness: %w", err)
			if !errors.Is(err, os.ErrNotExist) {
				err = backoff.Permanent(err)
			}
			return struct{}{}, err
		}
		mainPID, err := serviceMainPID(ctx, unit)
		if err != nil {
			return struct{}{}, backoff.Permanent(err)
		}
		if mainPID != replacement.PID {
			return struct{}{}, fmt.Errorf("replacement is outside service %s", unit)
		}
		return struct{}{}, nil
	}, backoff.WithBackOff(backoff.NewConstantBackOff(50*time.Millisecond)),
		backoff.WithMaxElapsedTime(0))
	if err != nil {
		return backoff.AsRetryError(err).LastErr
	}
	return nil
}
