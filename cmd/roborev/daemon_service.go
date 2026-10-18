package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/cenkalti/backoff/v7"

	"go.kenn.io/roborev/internal/daemon"
)

type daemonService struct {
	unit, executable string
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
	runDaemonService   = startDaemonService
	drainManagedDaemon = daemon.KillDaemon
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
	return os.SameFile(selected, cli), nil
}

func recoverDaemon(info *daemon.RuntimeInfo, probeErr error) error {
	ctx, cancel := context.WithTimeout(context.Background(), daemonStartTimeout)
	defer cancel()
	owner, err := findDaemonService(ctx, info.PID)
	if err != nil {
		return err
	}
	if owner == nil {
		return restartDaemonForEnsure()
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
		cliPath, _ := os.Readlink("/proc/self/exe")
		fmt.Fprintf(lifecycleOut, "Warning: systemd service %s uses %s; CLI image %s is a different installation. Using the responding daemon; its version differs or cannot be verified.\n", owner.unit, owner.executable, cliPath)
		return nil
	}
	release, err := daemon.RuntimeStore().AcquireStartLock(ctx)
	if err != nil {
		return fail(err)
	}
	defer release()
	current, err := getAnyRunningDaemonForStart(ctx)
	if errors.Is(err, os.ErrNotExist) {
		current, err = info, nil
	}
	if err != nil {
		return fail(fmt.Errorf("rediscover daemon: %w", err))
	}
	selected, err := findDaemonService(ctx, current.PID)
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
	if err := drainManagedDaemon(current); err != nil {
		return fail(err)
	}
	// Reviews may take longer than startup. Give the replacement a fresh budget.
	readyCtx, readyCancel := context.WithTimeout(context.Background(), daemonStartTimeout)
	defer readyCancel()
	if err := runDaemonService(readyCtx, owner.unit); err != nil {
		return fail(err)
	}
	_, err = backoff.Retry(readyCtx, func() (bool, error) {
		replacement, err := getAnyRunningDaemonForStart(readyCtx)
		if err != nil {
			return false, err
		}
		service, err := findDaemonService(readyCtx, replacement.PID)
		if err != nil {
			return false, err
		}
		if service == nil || service.unit != owner.unit {
			return false, fmt.Errorf("replacement is outside service %s", owner.unit)
		}
		return true, nil
	}, backoff.WithBackOff(backoff.NewConstantBackOff(50*time.Millisecond)), backoff.WithMaxElapsedTime(0))
	if err != nil {
		return fail(backoff.AsRetryError(err).LastErr)
	}
	return nil
}
