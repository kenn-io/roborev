package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/storage"
)

type restartDrainTarget struct {
	Endpoint daemon.DaemonEndpoint
	PID      int
}

var (
	restartDrainPollInterval              = 200 * time.Millisecond
	restartDrainProcessExists             = daemon.ProcessExists
	restartDrainStatus                    = fetchRestartDrainStatus
	restartDrainSetPaused                 = requestQueuePaused
	restartDrainCurrentEndpoint           = getDaemonEndpoint
	restartDrainStderr          io.Writer = os.Stderr
)

func fetchRestartDrainStatus(
	ctx context.Context, ep daemon.DaemonEndpoint,
) (storage.DaemonStatus, error) {
	req, err := http.NewRequestWithContext(
		ctx, http.MethodGet, ep.BaseURL()+"/api/status", nil,
	)
	if err != nil {
		return storage.DaemonStatus{}, fmt.Errorf("build daemon status request: %w", err)
	}
	resp, err := ep.HTTPClient(2 * time.Second).Do(req)
	if err != nil {
		return storage.DaemonStatus{}, fmt.Errorf("get daemon status: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return storage.DaemonStatus{}, fmt.Errorf("get daemon status: daemon returned %s", resp.Status)
	}
	var status storage.DaemonStatus
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return storage.DaemonStatus{}, fmt.Errorf("parse daemon status: %w", err)
	}
	return status, nil
}

func requestQueuePaused(
	ctx context.Context, ep daemon.DaemonEndpoint, paused bool,
) (bool, error) {
	path := "/api/queue/unpause"
	if paused {
		path = "/api/queue/pause"
	}
	req, err := http.NewRequestWithContext(
		ctx, http.MethodPost, ep.BaseURL()+path, nil,
	)
	if err != nil {
		return false, fmt.Errorf("build queue pause request: %w", err)
	}
	resp, err := ep.HTTPClient(2 * time.Second).Do(req)
	if err != nil {
		return false, fmt.Errorf("update queue pause state: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf(
			"update queue pause state: daemon returned %s", resp.Status,
		)
	}
	var result queuePauseResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return false, fmt.Errorf("parse queue pause response: %w", err)
	}
	return result.QueuePaused, nil
}

func drainAndRestart(
	ctx context.Context,
	target restartDrainTarget,
	replace func() error,
) (err error) {
	status, daemonExited, err := observeRestartDrainStatus(ctx, target)
	if err != nil {
		return err
	}
	if daemonExited {
		return replace()
	}

	introducedPause := !status.QueuePaused
	if introducedPause {
		paused, pauseErr := restartDrainSetPaused(ctx, target.Endpoint, true)
		if pauseErr != nil {
			return pauseErr
		}
		if !paused {
			return errors.New("daemon did not pause queue processing")
		}
		defer func() {
			restoreCtx, cancel := context.WithTimeout(
				context.Background(), 2*time.Second,
			)
			defer cancel()
			_, restoreErr := restartDrainSetPaused(
				restoreCtx, restartDrainCurrentEndpoint(), false,
			)
			if restoreErr != nil {
				err = errors.Join(err, fmt.Errorf(
					"restore queue pause state: %w", restoreErr,
				))
			}
		}()
	}

	lastRunning := -1
	waited := false
	mustRecheckAfterPause := introducedPause
	for {
		if status.RunningJobs != lastRunning && status.RunningJobs > 0 {
			if !waited {
				fmt.Fprintf(
					restartDrainStderr,
					"Waiting for %d running reviews to finish before restarting daemon; no new reviews will start...\n",
					status.RunningJobs,
				)
			} else {
				fmt.Fprintf(
					restartDrainStderr,
					"Still waiting for %d running reviews to finish...\n",
					status.RunningJobs,
				)
			}
			waited = true
			lastRunning = status.RunningJobs
		}
		if status.RunningJobs == 0 && !mustRecheckAfterPause {
			break
		}

		if err := waitForRestartDrainPoll(ctx); err != nil {
			return err
		}
		var daemonExited bool
		status, daemonExited, err = observeRestartDrainStatus(ctx, target)
		if err != nil {
			return err
		}
		if daemonExited {
			break
		}
		mustRecheckAfterPause = false
	}
	if waited {
		fmt.Fprintln(restartDrainStderr, "Running reviews finished; restarting daemon...")
	}
	return replace()
}

func observeRestartDrainStatus(
	ctx context.Context, target restartDrainTarget,
) (storage.DaemonStatus, bool, error) {
	reportedUnavailable := false
	for {
		status, err := restartDrainStatus(ctx, target.Endpoint)
		if err == nil {
			return status, false, nil
		}
		if ctx.Err() != nil {
			return storage.DaemonStatus{}, false, ctx.Err()
		}
		if target.PID > 0 && !restartDrainProcessExists(target.PID) {
			return storage.DaemonStatus{}, true, nil
		}
		if !reportedUnavailable {
			fmt.Fprintln(
				restartDrainStderr,
				"Waiting for daemon status before restarting safely...",
			)
			reportedUnavailable = true
		}
		if err := waitForRestartDrainPoll(ctx); err != nil {
			return storage.DaemonStatus{}, false, err
		}
	}
}

func waitForRestartDrainPoll(ctx context.Context) error {
	timer := time.NewTimer(restartDrainPollInterval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
