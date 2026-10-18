package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/cenkalti/backoff/v7"

	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/version"
)

type daemonService struct {
	unit, executable string
	mainPID          int
	manager          *daemonServiceManager
}

type daemonServiceManager struct {
	lookup func(context.Context, int) (*daemonService, error)
	start  func(context.Context, string) error
	close  func()
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
	drainManagedDaemon = daemon.KillDaemon
	// errUserBusUnavailable leaves a responsive daemon in use without recovery.
	errUserBusUnavailable = errors.New("user bus unavailable")
)

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

// acquireRecoveryLock bounds callers waiting behind an unlimited review drain.
func acquireRecoveryLock() (func(), error) {
	store := daemon.RuntimeStore()
	release, acquired, err := store.TryAcquireStartLock(context.Background())
	if err != nil || acquired {
		return release, err
	}
	fmt.Fprintln(lifecycleOut, "Waiting for daemon recovery lock; another command may be waiting for running reviews to finish...")
	ctx, cancel := context.WithTimeout(context.Background(), daemonStartTimeout)
	defer cancel()
	return store.AcquireStartLock(ctx)
}

func recoverDaemon(info *daemon.RuntimeInfo, probeErr error) error {
	ctx, cancel := context.WithTimeout(context.Background(), daemonStartTimeout)
	snapshot, snapshotErr := findDaemonService(ctx, info.PID)
	cancel()
	if snapshot != nil {
		defer snapshot.manager.close()
	}
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
		owner, err = snapshot, snapshotErr
		if errors.Is(err, os.ErrNotExist) {
			err = nil
		}
	}
	if responsive && errors.Is(err, errUserBusUnavailable) {
		if verbose {
			fmt.Fprintf(lifecycleOut, "Daemon recovery skipped; user bus unavailable: %v\n", err)
		}
		return nil
	}
	if err != nil {
		return err
	}
	if owner != nil && owner != snapshot {
		defer owner.manager.close()
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
	if !vanished {
		fmt.Fprintln(lifecycleOut, "Waiting for daemon shutdown; no new reviews will start, and any running reviews will finish first...")
		if err := drainManagedDaemon(current); err != nil {
			return fail(err)
		}
	}
	// Reviews may take longer than startup. Give the replacement a fresh budget.
	readyCtx, readyCancel := context.WithTimeout(context.Background(), daemonStartTimeout)
	defer readyCancel()
	if err := owner.manager.start(readyCtx, owner.unit); err != nil {
		return fail(err)
	}
	if err := owner.awaitReplacement(readyCtx); err != nil {
		return fail(err)
	}
	return nil
}

// awaitReplacement polls until a responsive daemon is the unit's main process.
// Unit startup can precede runtime publication.
func (s *daemonService) awaitReplacement(ctx context.Context) error {
	_, err := backoff.Retry(ctx, func() (struct{}, error) {
		replacement, err := getAnyRunningDaemonForStart(ctx)
		if err != nil {
			err = fmt.Errorf("replacement readiness: %w", err)
			if !errors.Is(err, os.ErrNotExist) {
				err = backoff.Permanent(err)
			}
			return struct{}{}, err
		}
		service, err := s.manager.lookup(ctx, replacement.PID)
		if err != nil {
			return struct{}{}, backoff.Permanent(err)
		}
		if service == nil || service.unit != s.unit {
			return struct{}{}, backoff.Permanent(fmt.Errorf("replacement is outside service %s", s.unit))
		}
		if service.mainPID != replacement.PID {
			return struct{}{}, fmt.Errorf("replacement is outside service %s", s.unit)
		}
		return struct{}{}, nil
	}, backoff.WithBackOff(backoff.NewConstantBackOff(50*time.Millisecond)),
		backoff.WithMaxElapsedTime(0))
	if err != nil {
		return backoff.AsRetryError(err).LastErr
	}
	return nil
}
