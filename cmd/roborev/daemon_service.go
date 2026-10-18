package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cenkalti/backoff/v7"

	"go.kenn.io/roborev/internal/daemon"
)

type daemonService struct {
	unit, executable string
	mainPID          int
	simple           bool
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
	cliPath, err := os.Readlink("/proc/self/exe")
	return os.SameFile(selected, cli) || err == nil && filepath.Clean(s.executable) == filepath.Clean(strings.TrimSuffix(cliPath, " (deleted)")), nil
}

func recoverDaemon(info *daemon.RuntimeInfo, probeErr error) error {
	ctx, cancel := context.WithTimeout(context.Background(), daemonStartTimeout)
	defer cancel()
	owner, err := findDaemonService(ctx, info.PID)
	if probeErr == nil && errors.Is(err, errUserBusUnavailable) {
		return nil
	}
	if err != nil {
		return err
	}
	if owner == nil {
		return restartDaemonForEnsure()
	}
	defer owner.manager.close()
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
	// Wait for any active recovery, including its review drain, then give
	// rediscovery a fresh budget.
	release, err := daemon.RuntimeStore().AcquireStartLock(context.Background())
	if err != nil {
		return fail(err)
	}
	defer release()
	ctx, cancel = context.WithTimeout(context.Background(), daemonStartTimeout)
	defer cancel()
	current, err := findDaemonForStart(ctx)
	if err != nil {
		return fail(fmt.Errorf("rediscover daemon: %w", err))
	}
	if current == nil {
		current = info
	}
	selected, err := owner.manager.lookup(ctx, current.PID)
	if err != nil {
		return err
	}
	if selected == nil || selected.unit != owner.unit {
		return fail(fmt.Errorf("daemon owner changed before recovery"))
	}
	if current.PID != info.PID {
		return nil
	}
	same, err = selected.sameInstallation()
	if err != nil {
		return fail(err)
	}
	if !same {
		return fail(fmt.Errorf("service executable changed before recovery"))
	}
	fmt.Fprintln(lifecycleOut, "Waiting for daemon shutdown; no new reviews will start, and any running reviews will finish first...")
	if err := drainManagedDaemon(current); err != nil {
		return fail(err)
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

// awaitReplacement confirms a responsive daemon is the unit's main process.
// Type=simple units report started before the daemon responds, so poll them.
func (s *daemonService) awaitReplacement(ctx context.Context) error {
	tries := uint(1)
	if s.simple {
		tries = 0
	}
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
		backoff.WithMaxTries(tries), backoff.WithMaxElapsedTime(0))
	if err != nil {
		return backoff.AsRetryError(err).LastErr
	}
	return nil
}
