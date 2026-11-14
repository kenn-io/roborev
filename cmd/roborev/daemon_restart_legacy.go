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

func interruptDaemonWithUpdateAPI(
	ctx context.Context, info *daemon.RuntimeInfo, endpoint daemon.DaemonEndpoint,
) (retErr error) {
	session, err := prepareInterruptUpdateDaemon(ctx, endpoint, rand.Text(), info)
	if err != nil {
		return err
	}
	if session == nil {
		return nil
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
	return session.shutdown(ctx)
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
		if job.Status == "running" && job.Source != nil &&
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
	ping, err := daemon.ProbeDaemonPingContext(ctx, endpoint, 2*time.Second)
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
