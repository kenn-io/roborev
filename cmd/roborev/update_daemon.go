package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"go.kenn.io/roborev/internal/daemon"
	roborevclient "go.kenn.io/roborev/pkg/client"
	"go.kenn.io/roborev/pkg/client/generated"
)

type runningReviewPolicy string

const (
	policyWait      runningReviewPolicy = "wait"
	policyInterrupt runningReviewPolicy = "interrupt"
	policyAbort     runningReviewPolicy = "abort"
)

var (
	updateLeaseRenewInterval     = 20 * time.Second
	updateDrainPollInterval      = 250 * time.Millisecond
	updateInterruptUnwindTimeout = 30 * time.Second
)

type updateDaemonSession struct {
	Endpoint        daemon.DaemonEndpoint
	OwnerID         string
	Token           string
	Policy          runningReviewPolicy
	Legacy          bool
	Prepared        bool
	Installed       bool
	ShutdownOwned   bool
	status          generated.UpdateDrainStatus
	cancelHeartbeat context.CancelFunc
}

func prepareUpdateDaemon(
	ctx context.Context,
	endpoint daemon.DaemonEndpoint,
	ownerID string,
	policy runningReviewPolicy,
	out io.Writer,
) (*updateDaemonSession, error) {
	api, err := newUpdateAPI(endpoint)
	if err != nil {
		return nil, err
	}
	resp, callErr := api.PrepareUpdateWithResponse(ctx, &generated.PrepareUpdateRequestOptions{
		Body: &generated.PrepareUpdateBody{
			OwnerID: ownerID,
			Policy:  generated.UpdateDrainRequestBodyPolicy(policy),
		},
	})
	if resp != nil && resp.StatusCode == http.StatusNotFound {
		return prepareLegacyUpdateDaemon(ctx, endpoint, ownerID, policy, out)
	}
	if resp != nil && resp.StatusCode == http.StatusConflict &&
		resp.ApplicationProblemPlusJSON409 != nil &&
		resp.ApplicationProblemPlusJSON409.Detail != nil &&
		*resp.ApplicationProblemPlusJSON409.Detail == errUpdateReviewsRunning.Error() {
		return nil, errUpdateReviewsRunning
	}
	if callErr != nil {
		return nil, updateProtocolError("prepare", resp, callErr)
	}
	if resp == nil || resp.JSON200 == nil {
		return nil, fmt.Errorf("prepare daemon update returned no status")
	}
	if resp.JSON200.LeaseToken == nil || *resp.JSON200.LeaseToken == "" {
		return nil, errors.New("prepare daemon update returned an empty lease token")
	}
	return &updateDaemonSession{
		Endpoint: endpoint,
		OwnerID:  ownerID,
		Token:    *resp.JSON200.LeaseToken,
		Policy:   policy,
		Prepared: true,
		status:   *resp.JSON200,
	}, nil
}

func prepareLegacyUpdateDaemon(
	ctx context.Context,
	endpoint daemon.DaemonEndpoint,
	ownerID string,
	policy runningReviewPolicy,
	out io.Writer,
) (*updateDaemonSession, error) {
	session := &updateDaemonSession{
		Endpoint: endpoint,
		OwnerID:  ownerID,
		Policy:   policy,
		Legacy:   true,
	}
	switch policy {
	case policyWait:
		fmt.Fprintln(out, "Daemon       compatibility mode; using graceful shutdown")
	case policyInterrupt:
		return nil, errors.New("running daemon does not support safe review interruption; use --running=wait or restart it first")
	case policyAbort:
		status, err := fetchDaemonStatus(ctx, endpoint)
		if err != nil {
			return nil, err
		}
		if status.RunningJobs != 0 {
			return nil, errUpdateReviewsRunning
		}
		fmt.Fprintln(out, "Daemon       compatibility mode; a racing review will be preserved by graceful shutdown")
	default:
		return nil, fmt.Errorf("unsupported running-review policy %q", policy)
	}
	return session, nil
}

func newUpdateAPI(endpoint daemon.DaemonEndpoint) (*roborevclient.Client, error) {
	return roborevclient.NewWithHTTPClient(
		endpoint.BaseURL(), endpoint.HTTPClient(5*time.Second),
	)
}

func fetchDaemonStatus(
	ctx context.Context, endpoint daemon.DaemonEndpoint,
) (*generated.DaemonStatus, error) {
	api, err := newUpdateAPI(endpoint)
	if err != nil {
		return nil, err
	}
	resp, err := api.GetStatusWithResponse(ctx)
	if err != nil {
		return nil, fmt.Errorf("read daemon status: %w", err)
	}
	if resp == nil || resp.JSON200 == nil {
		return nil, errors.New("read daemon status: empty response")
	}
	return resp.JSON200, nil
}

func (s *updateDaemonSession) renew(ctx context.Context) (generated.UpdateDrainStatus, error) {
	if s.Legacy || !s.Prepared {
		return s.status, nil
	}
	api, err := newUpdateAPI(s.Endpoint)
	if err != nil {
		return generated.UpdateDrainStatus{}, err
	}
	resp, callErr := api.RenewUpdateWithResponse(ctx, &generated.RenewUpdateRequestOptions{
		Body: &generated.RenewUpdateBody{LeaseToken: s.Token},
	})
	if callErr != nil {
		return generated.UpdateDrainStatus{}, updateProtocolError("renew", resp, callErr)
	}
	if resp == nil || resp.JSON200 == nil {
		return generated.UpdateDrainStatus{}, errors.New("renew update lease returned no status")
	}
	return *resp.JSON200, nil
}

func (s *updateDaemonSession) release(ctx context.Context) (bool, error) {
	if s.Legacy || !s.Prepared {
		return true, nil
	}
	api, err := newUpdateAPI(s.Endpoint)
	if err != nil {
		return false, err
	}
	resp, callErr := api.ReleaseUpdateWithResponse(ctx, &generated.ReleaseUpdateRequestOptions{
		Body: &generated.ReleaseUpdateBody{LeaseToken: s.Token},
	})
	if callErr != nil {
		return false, updateProtocolError("release", resp, callErr)
	}
	if resp == nil || resp.JSON200 == nil {
		return false, errors.New("release update lease returned no status")
	}
	return resp.JSON200.Released, nil
}

func (s *updateDaemonSession) shutdown(ctx context.Context) error {
	api, err := newUpdateAPI(s.Endpoint)
	if err != nil {
		return err
	}
	resp, callErr := api.ShutdownWithResponse(ctx)
	if callErr != nil {
		return updateProtocolError("shutdown", resp, callErr)
	}
	if resp == nil || resp.JSON200 == nil {
		return errors.New("daemon shutdown returned no status")
	}
	return nil
}

func (s *updateDaemonSession) startHeartbeat(ctx context.Context) <-chan error {
	errCh := make(chan error, 1)
	if s.Legacy || !s.Prepared {
		close(errCh)
		return errCh
	}
	heartbeatCtx, cancel := context.WithCancel(ctx)
	s.cancelHeartbeat = cancel
	go func() {
		defer close(errCh)
		ticker := time.NewTicker(updateLeaseRenewInterval)
		defer ticker.Stop()
		for {
			select {
			case <-heartbeatCtx.Done():
				return
			case <-ticker.C:
				if _, err := s.renew(heartbeatCtx); err != nil {
					if heartbeatCtx.Err() != nil {
						return
					}
					errCh <- err
					return
				}
			}
		}
	}()
	return errCh
}

func (s *updateDaemonSession) stopHeartbeat() {
	if s.cancelHeartbeat != nil {
		s.cancelHeartbeat()
	}
}

func waitForPreparedDrain(
	ctx context.Context, session *updateDaemonSession, out io.Writer,
) error {
	wroteProgress := false
	defer func() {
		if wroteProgress && out != nil {
			fmt.Fprintln(out)
		}
	}()
	if session.Legacy {
		if session.Policy != policyAbort {
			return nil
		}
		status, err := fetchDaemonStatus(ctx, session.Endpoint)
		if err != nil {
			return err
		}
		if status.RunningJobs != 0 {
			return errUpdateReviewsRunning
		}
		return nil
	}

	started := time.Now()
	for {
		status, err := session.renew(ctx)
		if err != nil {
			return err
		}
		done := status.RunningJobs == 0
		if session.Policy == policyInterrupt {
			done = status.TargetedRunningJobs == 0 && status.ActiveWorkers == 0
		}
		if done {
			return nil
		}
		if session.Policy == policyInterrupt && time.Since(started) >= updateInterruptUnwindTimeout {
			return fmt.Errorf("timed out after %s waiting for interrupted reviews to unwind", updateInterruptUnwindTimeout)
		}
		if out != nil {
			message := fmt.Sprintf(
				"waiting for %d running %s",
				status.RunningJobs, pluralUpdateCount(status.RunningJobs, "review", "reviews"),
			)
			if session.Policy == policyInterrupt {
				if status.TargetedRunningJobs != 0 {
					message = fmt.Sprintf(
						"waiting for %d interrupted %s",
						status.TargetedRunningJobs,
						pluralUpdateCount(status.TargetedRunningJobs, "review", "reviews"),
					)
				} else {
					message = fmt.Sprintf(
						"waiting for %d %s to unwind",
						status.ActiveWorkers,
						pluralUpdateCount(status.ActiveWorkers, "worker", "workers"),
					)
				}
			}
			fmt.Fprintf(out, "\r%-13s%s", "Daemon", message)
			wroteProgress = true
		}
		timer := time.NewTimer(updateDrainPollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func pluralUpdateCount(count int64, singular, plural string) string {
	if count == 1 {
		return singular
	}
	return plural
}

func updateProtocolError(op string, response any, callErr error) error {
	detail := ""
	switch resp := response.(type) {
	case *generated.PrepareUpdateResp:
		if resp != nil && resp.ApplicationProblemPlusJSON409 != nil && resp.ApplicationProblemPlusJSON409.Detail != nil {
			detail = *resp.ApplicationProblemPlusJSON409.Detail
		}
	case *generated.RenewUpdateResp:
		if resp != nil && resp.ApplicationProblemPlusJSON409 != nil && resp.ApplicationProblemPlusJSON409.Detail != nil {
			detail = *resp.ApplicationProblemPlusJSON409.Detail
		}
	case *generated.ReleaseUpdateResp:
		if resp != nil && resp.ApplicationProblemPlusJSON409 != nil && resp.ApplicationProblemPlusJSON409.Detail != nil {
			detail = *resp.ApplicationProblemPlusJSON409.Detail
		}
	}
	if detail != "" {
		return fmt.Errorf("%s daemon update: %s", op, detail)
	}
	return fmt.Errorf("%s daemon update: %w", op, callErr)
}

func normalizeUpdateVersion(value string) string {
	return strings.TrimPrefix(value, "v")
}

func requireUpdatedDaemonVersion(observed, expected string) error {
	if normalizeUpdateVersion(observed) != normalizeUpdateVersion(expected) {
		return fmt.Errorf("daemon version mismatch: expected %s, observed %s", expected, observed)
	}
	return nil
}

func waitForLegacyDaemonExit(ctx context.Context, previousPID int) error {
	for !previousPIDExited(previousPID) {
		timer := time.NewTimer(updateRestartPollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return nil
}

func restartAndVerifyUpdatedDaemon(
	ctx context.Context,
	binDir string,
	expectedVersion string,
	previous *daemon.RuntimeInfo,
) error {
	if previous == nil {
		return nil
	}
	replacementPID, err := waitForDaemonExitContext(ctx, previous.PID)
	if err != nil {
		return err
	}
	if replacementPID == 0 {
		if err := startUpdatedDaemon(binDir); err != nil {
			return fmt.Errorf("start updated daemon: %w", err)
		}
	}
	info, err := waitForResponsiveUpdatedDaemon(ctx, updateRestartWaitTimeout)
	if err != nil {
		return err
	}
	return requireUpdatedDaemonVersion(info.Version, expectedVersion)
}

func waitForDaemonExitContext(
	ctx context.Context, previousPID int,
) (replacementPID int, err error) {
	for {
		info, discoverErr := getAnyRunningDaemon()
		if discoverErr != nil {
			if previousPIDExited(previousPID) {
				return replacementRuntimePID(previousPID), nil
			}
		} else if info.PID != previousPID && previousPIDExited(previousPID) {
			return info.PID, nil
		}
		timer := time.NewTimer(updateRestartPollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return 0, ctx.Err()
		case <-timer.C:
		}
	}
}

func waitForResponsiveUpdatedDaemon(
	ctx context.Context, timeout time.Duration,
) (*daemon.RuntimeInfo, error) {
	deadline := time.Now().Add(timeout)
	for {
		if info, err := getAnyRunningDaemon(); err == nil {
			return info, nil
		}
		if time.Now().After(deadline) {
			return nil, errors.New("updated daemon did not become ready")
		}
		timer := time.NewTimer(updateRestartPollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

var errUpdateReviewsRunning = errors.New("reviews are running")
