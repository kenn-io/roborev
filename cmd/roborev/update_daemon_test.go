package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/daemon"
)

func updateTestEndpoint(t *testing.T, handler http.Handler) daemon.DaemonEndpoint {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	u, err := url.Parse(server.URL)
	require.NoError(t, err)
	return daemon.DaemonEndpoint{Network: "tcp", Address: u.Host}
}

func TestNormalizeUpdateVersion(t *testing.T) {
	for _, tc := range []struct {
		got  string
		want string
	}{
		{got: "v0.65.0", want: "0.65.0"},
		{got: "0.65.0", want: "0.65.0"},
		{got: "vv0.65.0", want: "v0.65.0"},
	} {
		assert.Equal(t, tc.want, normalizeUpdateVersion(tc.got))
	}
}

func TestPrepareUpdateDaemonLegacyPolicies(t *testing.T) {
	for _, tc := range []struct {
		policy       runningReviewPolicy
		wantFallback bool
		wantErr      bool
	}{
		{policy: policyWait, wantFallback: true},
		{policy: policyInterrupt, wantErr: true},
		{policy: policyAbort, wantFallback: true},
	} {
		t.Run(string(tc.policy), func(t *testing.T) {
			endpoint := updateTestEndpoint(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/status" {
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"running_jobs":0}`)
					return
				}
				http.NotFound(w, r)
			}))
			session, err := prepareUpdateDaemon(
				context.Background(), endpoint, "owner", tc.policy, io.Discard,
			)
			if tc.wantErr {
				require.Error(t, err)
				assert.Nil(t, session)
				return
			}
			require.NoError(t, err)
			require.NotNil(t, session)
			assert.Equal(t, tc.wantFallback, session.Legacy)
		})
	}
}

func TestUpdateDaemonLeaseRoundTrip(t *testing.T) {
	var renewCalls atomic.Int32
	var releaseCalls atomic.Int32
	endpoint := updateTestEndpoint(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/update/prepare":
			_, _ = io.WriteString(w, `{"lease_token":"lease-1","policy":"wait","expires_at":"2030-01-01T00:00:00Z","running_jobs":1,"targeted_running_jobs":1,"active_workers":1,"recovering":false}`)
		case "/api/update/renew":
			renewCalls.Add(1)
			_, _ = io.WriteString(w, `{"lease_token":"lease-1","policy":"wait","expires_at":"2030-01-01T00:00:00Z","running_jobs":0,"targeted_running_jobs":0,"active_workers":0,"recovering":false}`)
		case "/api/update/release":
			releaseCalls.Add(1)
			_, _ = io.WriteString(w, `{"released":true}`)
		default:
			http.NotFound(w, r)
		}
	}))
	session, err := prepareUpdateDaemon(
		context.Background(), endpoint, "owner", policyWait, io.Discard,
	)
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.True(t, session.Prepared)
	assert.Equal(t, "lease-1", session.Token)

	require.NoError(t, waitForPreparedDrain(context.Background(), session, io.Discard))
	released, err := session.release(context.Background())
	require.NoError(t, err)
	assert.True(t, released)
	assert.Equal(t, int32(1), renewCalls.Load())
	assert.Equal(t, int32(1), releaseCalls.Load())
}

func TestUpdateDaemonRenewDoesNotMutateSharedStatus(t *testing.T) {
	endpoint := updateTestEndpoint(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/update/prepare":
			_, _ = io.WriteString(w, `{"lease_token":"lease-1","policy":"wait","expires_at":"2030-01-01T00:00:00Z","running_jobs":1,"targeted_running_jobs":1,"active_workers":1,"recovering":false}`)
		case "/api/update/renew":
			_, _ = io.WriteString(w, `{"lease_token":"lease-1","policy":"wait","expires_at":"2030-01-01T00:00:00Z","running_jobs":0,"targeted_running_jobs":0,"active_workers":0,"recovering":false}`)
		default:
			http.NotFound(w, r)
		}
	}))
	session, err := prepareUpdateDaemon(
		context.Background(), endpoint, "owner", policyWait, io.Discard,
	)
	require.NoError(t, err)

	status, err := session.renew(context.Background())

	require.NoError(t, err)
	assert.Equal(t, int64(0), status.RunningJobs)
	assert.Equal(t, int64(1), session.status.RunningJobs)
}

func TestUpdateDaemonHeartbeatReportsLeaseLoss(t *testing.T) {
	var renewCalls atomic.Int32
	endpoint := updateTestEndpoint(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/update/prepare" {
			_, _ = io.WriteString(w, `{"lease_token":"lease-1","policy":"wait","expires_at":"2030-01-01T00:00:00Z","running_jobs":0,"targeted_running_jobs":0,"active_workers":0,"recovering":false}`)
			return
		}
		if r.URL.Path == "/api/update/renew" {
			renewCalls.Add(1)
			http.Error(w, `{"title":"Conflict","status":409}`, http.StatusConflict)
			return
		}
		http.NotFound(w, r)
	}))
	session, err := prepareUpdateDaemon(
		context.Background(), endpoint, "owner", policyWait, io.Discard,
	)
	require.NoError(t, err)
	oldInterval := updateLeaseRenewInterval
	updateLeaseRenewInterval = time.Millisecond
	t.Cleanup(func() { updateLeaseRenewInterval = oldInterval })

	errCh := session.startHeartbeat(context.Background())
	heartbeatErr := <-errCh

	require.Error(t, heartbeatErr)
	assert.Contains(t, heartbeatErr.Error(), "renew")
	assert.Equal(t, int32(1), renewCalls.Load())
}

func TestUpdateDaemonHeartbeatCancellationIsNotLeaseFailure(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	endpoint := updateTestEndpoint(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/update/prepare":
			_, _ = io.WriteString(w, `{"lease_token":"lease-1","policy":"wait","expires_at":"2030-01-01T00:00:00Z","running_jobs":0,"targeted_running_jobs":0,"active_workers":0,"recovering":false}`)
		case "/api/update/renew":
			close(started)
			<-release
			_, _ = io.WriteString(w, `{"lease_token":"lease-1","policy":"wait","expires_at":"2030-01-01T00:00:00Z","running_jobs":0,"targeted_running_jobs":0,"active_workers":0,"recovering":false}`)
		default:
			http.NotFound(w, r)
		}
	}))
	session, err := prepareUpdateDaemon(
		context.Background(), endpoint, "owner", policyWait, io.Discard,
	)
	require.NoError(t, err)
	oldInterval := updateLeaseRenewInterval
	updateLeaseRenewInterval = time.Millisecond
	t.Cleanup(func() { updateLeaseRenewInterval = oldInterval })

	errCh := session.startHeartbeat(context.Background())
	require.True(t, waitForUpdateTestSignal(started, time.Second))
	session.stopHeartbeat()
	close(release)
	heartbeatErr, ok := <-errCh

	assert.False(t, ok)
	assert.NoError(t, heartbeatErr)
}

func TestWaitForPreparedInterruptReportsWorkerUnwindAndTerminatesLine(t *testing.T) {
	var renewCalls atomic.Int32
	endpoint := updateTestEndpoint(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		running := 1
		if renewCalls.Add(1) > 1 {
			running = 0
		}
		_, _ = fmt.Fprintf(w, `{"lease_token":"lease-1","policy":"interrupt","expires_at":"2030-01-01T00:00:00Z","running_jobs":0,"targeted_running_jobs":0,"active_workers":%d,"recovering":false}`, running)
	}))
	session := &updateDaemonSession{
		Endpoint: endpoint, Token: "lease-1", Policy: policyInterrupt, Prepared: true,
	}
	oldPoll := updateDrainPollInterval
	updateDrainPollInterval = time.Millisecond
	t.Cleanup(func() { updateDrainPollInterval = oldPoll })
	var out bytes.Buffer

	require.NoError(t, waitForPreparedDrain(context.Background(), session, &out))
	assert.Contains(t, out.String(), "waiting for 1 worker to unwind")
	assert.True(t, strings.HasSuffix(out.String(), "\n"))
}

func TestRequireUpdatedDaemonVersion(t *testing.T) {
	require.NoError(t, requireUpdatedDaemonVersion("v0.65.0", "0.65.0"))
	err := requireUpdatedDaemonVersion("v0.64.0", "v0.65.0")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "expected v0.65.0")
}

func TestWaitForLegacyDaemonExitRespectsCancellation(t *testing.T) {
	stubRestartVars(t)
	isPIDAliveForUpdate = func(int) bool { return true }
	listAllRuntimes = func() ([]*daemon.RuntimeInfo, error) {
		return []*daemon.RuntimeInfo{{PID: 42}}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := waitForLegacyDaemonExit(ctx, 42)

	require.ErrorIs(t, err, context.Canceled)
}

func TestRestartAndVerifyUpdatedDaemonStartsAndChecksVersion(t *testing.T) {
	stubs := stubRestartVars(t)
	started := false
	startUpdatedDaemon = func(string) error {
		stubs.startCalls++
		started = true
		return nil
	}
	getAnyRunningDaemon = func() (*daemon.RuntimeInfo, error) {
		if !started {
			return nil, errors.New("old daemon exited")
		}
		return &daemon.RuntimeInfo{PID: 200, Version: "v0.65.0"}, nil
	}

	err := restartAndVerifyUpdatedDaemon(
		context.Background(), "/tmp/bin", "0.65.0",
		&daemon.RuntimeInfo{PID: 100},
	)

	require.NoError(t, err)
	assert.Equal(t, 1, stubs.startCalls)
}

func TestRestartAndVerifyUpdatedDaemonWaitsBeyondReadinessTimeoutForShutdown(
	t *testing.T,
) {
	stubs := stubRestartVars(t)
	updateRestartWaitTimeout = 5 * time.Millisecond
	updateRestartPollInterval = time.Millisecond
	var probes atomic.Int32
	var started atomic.Bool
	getAnyRunningDaemon = func() (*daemon.RuntimeInfo, error) {
		if started.Load() {
			return &daemon.RuntimeInfo{PID: 200, Version: "v0.65.0"}, nil
		}
		if probes.Add(1) < 15 {
			return &daemon.RuntimeInfo{PID: 100, Version: "v0.64.0"}, nil
		}
		return nil, errors.New("old daemon exited")
	}
	listAllRuntimes = func() ([]*daemon.RuntimeInfo, error) {
		if probes.Load() < 15 {
			return []*daemon.RuntimeInfo{{PID: 100}}, nil
		}
		return nil, nil
	}
	isPIDAliveForUpdate = func(pid int) bool {
		return pid == 100 && probes.Load() < 15
	}
	startUpdatedDaemon = func(string) error {
		stubs.startCalls++
		started.Store(true)
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	err := restartAndVerifyUpdatedDaemon(
		ctx, "/tmp/bin", "0.65.0", &daemon.RuntimeInfo{PID: 100},
	)

	require.NoError(t, err)
	assert.Equal(t, 1, stubs.startCalls)
}

func TestRestartAndVerifyUpdatedDaemonRejectsWrongVersion(t *testing.T) {
	stubRestartVars(t)
	getAnyRunningDaemon = func() (*daemon.RuntimeInfo, error) {
		return &daemon.RuntimeInfo{PID: 200, Version: "v0.64.0"}, nil
	}

	err := restartAndVerifyUpdatedDaemon(
		context.Background(), "/tmp/bin", "v0.65.0",
		&daemon.RuntimeInfo{PID: 100},
	)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "daemon version mismatch")
}

func TestPrepareUpdateDaemonConflictIncludesDetail(t *testing.T) {
	endpoint := updateTestEndpoint(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"title":  "Conflict",
			"status": 409,
			"detail": "another update is in progress; lease expires in 42s",
		})
	}))

	_, err := prepareUpdateDaemon(
		context.Background(), endpoint, "owner", policyWait, io.Discard,
	)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "lease expires in 42s")
}

func waitForUpdateTestSignal(ch <-chan struct{}, timeout time.Duration) bool {
	select {
	case <-ch:
		return true
	case <-time.After(timeout):
		return false
	}
}
