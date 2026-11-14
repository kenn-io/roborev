package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/pkg/client/generated"
)

// errInterruptTargetGone reports that the discovered daemon exited, so there
// is nothing left to interrupt.
var errInterruptTargetGone = errors.New("daemon exited before interruption")

func interruptDaemonWithUpdateAPI(
	ctx context.Context, info *daemon.RuntimeInfo, endpoint daemon.DaemonEndpoint,
) error {
	err := interruptPausedDaemon(ctx, info, endpoint)
	if errors.Is(err, errInterruptTargetGone) {
		return nil
	}
	return err
}

func interruptPausedDaemon(
	ctx context.Context, info *daemon.RuntimeInfo, endpoint daemon.DaemonEndpoint,
) (retErr error) {
	if info == nil || info.PID <= 0 {
		return daemon.ErrInterruptShutdownUnsupported
	}
	// A v0.75.0 daemon clears a queued rebuttal's session if a worker claims it
	// before the update gate is set, so block claims before the running-job check.
	restore, err := pauseQueueForInterrupt(ctx, info, endpoint)
	if restore != nil {
		defer func() {
			retErr = errors.Join(retErr, restore(context.WithoutCancel(ctx)))
		}()
	}
	if err != nil {
		return err
	}

	session, err := prepareInterruptUpdateDaemon(ctx, endpoint, rand.Text(), info)
	if err != nil || session == nil {
		return err
	}
	defer func() {
		if retErr == nil {
			return
		}
		_, releaseErr := session.release(context.WithoutCancel(ctx))
		retErr = errors.Join(retErr, releaseErr)
	}()

	if err := waitForPreparedDrain(ctx, session, io.Discard); err != nil {
		return err
	}
	// The update lease still blocks claims, so the queue can resume first.
	if err := restore(ctx); err != nil {
		return err
	}
	return session.shutdown(ctx)
}

// pauseQueueForInterrupt pauses the queue unless it was already paused and
// returns an idempotent restore of the prior state. Restoration returns an
// error if the discovered daemon is gone or replaced, so no request reaches a
// replacement PID. Check after the status read and before pausing, so a
// replacement cannot receive the first mutating compatibility request.
func pauseQueueForInterrupt(
	ctx context.Context, info *daemon.RuntimeInfo, endpoint daemon.DaemonEndpoint,
) (func(context.Context) error, error) {
	noop := func(context.Context) error { return nil }
	if info.Stale() {
		return nil, errInterruptTargetGone
	}
	status, err := fetchDaemonStatus(ctx, endpoint)
	if err != nil {
		return nil, err
	}
	if status.QueuePaused {
		return noop, nil
	}
	api, err := newUpdateAPI(endpoint)
	if err != nil {
		return nil, err
	}
	done := false
	recoveryError := func(err error) error {
		if errors.Is(err, errInterruptTargetGone) {
			return errors.New("daemon exited; queue may still be paused; start the daemon and run roborev unpause")
		}
		return fmt.Errorf("queue may still be paused; start the daemon and run roborev unpause: %w", err)
	}
	restore := func(ctx context.Context) error {
		if done {
			return nil
		}
		alive, err := interruptTargetAtEndpoint(ctx, info, endpoint)
		if err != nil {
			// A replacement may own the endpoint now, so do not retry there.
			done = true
			return recoveryError(err)
		}
		if !alive {
			done = true
			return recoveryError(errInterruptTargetGone)
		}
		resp, err := api.UnpauseQueueWithResponse(ctx)
		if err != nil {
			return fmt.Errorf("restore queue pause state: %w", err)
		}
		if resp == nil || resp.JSON200 == nil || resp.JSON200.QueuePaused {
			return errors.New("restore queue pause state returned no unpaused status")
		}
		done = true
		return nil
	}
	alive, err := interruptTargetAtEndpoint(ctx, info, endpoint)
	if err != nil {
		return nil, err
	}
	if !alive {
		return nil, errInterruptTargetGone
	}
	resp, callErr := api.PauseQueueWithResponse(ctx)
	if resp != nil && resp.StatusCode == http.StatusNotFound {
		return nil, errors.New("running daemon does not support safe review interruption; use --running=wait or restart it first")
	}
	if callErr != nil {
		// The caller restores the pause because the request may have applied.
		return restore, fmt.Errorf("pause queue before interruption: %w", callErr)
	}
	if resp == nil || resp.JSON200 == nil || !resp.JSON200.QueuePaused {
		return restore, errors.New("pause queue before interruption returned no paused status")
	}
	alive, err = interruptTargetAtEndpoint(ctx, info, endpoint)
	if err != nil {
		done = true
		return nil, recoveryError(err)
	}
	if !alive {
		done = true
		return nil, recoveryError(errInterruptTargetGone)
	}
	return restore, nil
}

func prepareInterruptUpdateDaemon(
	ctx context.Context,
	endpoint daemon.DaemonEndpoint,
	ownerID string,
	info *daemon.RuntimeInfo,
) (*updateDaemonSession, error) {
	api, err := newUpdateAPI(endpoint)
	if err != nil {
		return nil, err
	}
	resp, err := api.ListJobsWithResponse(ctx, &generated.ListJobsRequestOptions{
		Query: &generated.ListJobsQuery{
			Status:     new("running"),
			OmitPrompt: new(generated.ListJobsQueryOmitPromptTrue),
			OmitStats:  new(generated.ListJobsQueryOmitStatsTrue),
			Limit:      new(int64(0)),
		},
	})
	if err != nil {
		return nil, fmt.Errorf("check running jobs before interruption: %w", err)
	}
	if resp == nil {
		return nil, errors.New("check running jobs before interruption returned no response")
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, errors.New("running daemon does not support safe review interruption; use --running=wait or restart it first")
	}
	if resp.StatusCode != http.StatusOK || resp.JSON200 == nil {
		return nil, fmt.Errorf("check running jobs before interruption returned HTTP %d", resp.StatusCode)
	}
	for _, job := range resp.JSON200.Jobs {
		if job.Source != nil &&
			*job.Source == storage.JobSourceRebuttal &&
			job.SessionID != nil && *job.SessionID != "" {
			return nil, errors.New("cannot interrupt a running rebuttal without losing its agent session; use --running=wait")
		}
	}
	alive, err := interruptTargetAtEndpoint(ctx, info, endpoint)
	if err != nil || !alive {
		return nil, err
	}
	session, err := prepareUpdateDaemon(ctx, endpoint, ownerID, policyInterrupt, io.Discard)
	if err != nil {
		return nil, err
	}
	session.expectedDaemon = info
	return session, nil
}

func interruptTargetAtEndpoint(
	ctx context.Context, info *daemon.RuntimeInfo, endpoint daemon.DaemonEndpoint,
) (bool, error) {
	if info == nil || info.PID <= 0 {
		return false, daemon.ErrInterruptShutdownUnsupported
	}
	if info.Stale() {
		return false, nil
	}
	ping, err := daemon.ProbeDaemonPing(endpoint, 2*time.Second)
	if info.Stale() {
		return false, nil
	}
	if err != nil && ctx.Err() != nil {
		return false, ctx.Err()
	}
	if err != nil || ping == nil || ping.PID != info.PID {
		return false, daemon.ErrInterruptShutdownUnsupported
	}
	return true, nil
}
