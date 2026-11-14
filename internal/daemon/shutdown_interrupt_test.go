package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/testenv"
	"go.kenn.io/roborev/pkg/client/generated"
)

func TestInterruptShutdownTargetsRunningAttempts(t *testing.T) {
	for _, tc := range []struct {
		name            string
		alreadyDraining bool
	}{{"fresh shutdown", false}, {"already draining", true}} {
		t.Run(tc.name, func(t *testing.T) {
			server, db, dir := newTestServer(t)
			createTestJob(t, db, dir, "interrupt-shutdown", "test")
			job, err := db.ClaimJob("worker-interrupt")
			require.NoError(t, err)
			require.NotNil(t, job)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			server.workerPool.registerRunningJob(job.ID, cancel, time.Time{})
			if tc.alreadyDraining {
				require.NoError(t, server.beginShutdownDrain())
			}
			response := postUpdateDrain(t, server, "/api/shutdown/interrupt", map[string]int{"expected_pid": os.Getpid()})
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			draining, err := db.IsShutdownDraining()
			require.NoError(t, err)
			assert.True(t, draining)
			assert.True(t, server.shutdownDraining)
			assert.Contains(t, server.workerPool.updateInterruptTargets, job.ID)
			require.ErrorIs(t, ctx.Err(), context.Canceled)
			assert.True(t, server.workerPool.stopped)
			requested := false
			select {
			case <-server.ShutdownRequested():
				requested = true
			default:
			}
			assert.True(t, requested)
		})
	}
}

func TestInterruptShutdownRejectsMismatchAndUpdateLease(t *testing.T) {
	server, db, _ := newTestServer(t)
	response := postUpdateDrain(t, server, "/api/shutdown/interrupt", map[string]int{"expected_pid": os.Getpid() + 1})
	require.Equal(t, http.StatusPreconditionFailed, response.Code)
	draining, err := db.IsShutdownDraining()
	require.NoError(t, err)
	assert.False(t, draining)
	prepareUpdateDrain(t, server, "owner", "wait")
	response = postUpdateDrain(t, server, "/api/shutdown/interrupt", map[string]int{"expected_pid": os.Getpid()})
	assert.Equal(t, http.StatusConflict, response.Code)
	assert.False(t, server.shutdownDraining)
	draining, err = db.IsShutdownDraining()
	require.NoError(t, err)
	assert.True(t, draining) // The update lease owns the gate.
}

func TestInterruptShutdownDatabaseErrorLeavesShutdownUntouched(t *testing.T) {
	server, db, _ := newTestServer(t)
	_, err := db.ExecContext(t.Context(), `CREATE TRIGGER fail_gate BEFORE INSERT ON daemon_state WHEN NEW.key = 'shutdown_draining' BEGIN SELECT RAISE(FAIL, 'synthetic'); END`)
	require.NoError(t, err)
	response := postUpdateDrain(t, server, "/api/shutdown/interrupt", map[string]int{"expected_pid": os.Getpid()})
	require.Equal(t, http.StatusInternalServerError, response.Code)
	assert.False(t, server.shutdownDraining)
	assert.False(t, server.workerPool.stopped)
	requested := false
	select {
	case <-server.ShutdownRequested():
		requested = true
	default:
	}
	assert.False(t, requested)
}

func TestInterruptShutdownRequest(t *testing.T) {
	const pid = 123
	for _, tc := range []struct {
		name   string
		status int
		dead   bool
		want   string
	}{
		{name: "older daemon", status: http.StatusNotFound, want: "--running=wait"},
		{name: "PID changed", status: http.StatusPreconditionFailed, want: "412 Precondition Failed"},
		{name: "original exited", status: http.StatusNotFound, dead: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var dead atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "/api/shutdown/interrupt", r.URL.Path)
				var body struct {
					ExpectedPID int `json:"expected_pid"`
				}
				assert.NoError(t, json.NewDecoder(r.Body).Decode(&body))
				assert.Equal(t, pid, body.ExpectedPID)
				dead.Store(tc.dead)
				w.WriteHeader(tc.status)
			}))
			defer server.Close()
			ep, err := ParseEndpoint(server.Listener.Addr().String())
			require.NoError(t, err)
			err = requestDaemonShutdown(t.Context(), ep, dead.Load, pid, true)
			if tc.want == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tc.want)
			}
		})
	}
}

func TestInterruptDaemonRejectsUnusableRuntime(t *testing.T) {
	testenv.SetDataDir(t)
	var shutdowns atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/shutdown/interrupt" || r.URL.Path == "/api/shutdown" {
			shutdowns.Add(1)
		}
		fmt.Fprint(w, `{"ok":true,"service":"roborev","pid":1}`)
	}))
	defer server.Close()
	err := InterruptDaemonWithFallback(&RuntimeInfo{Network: "tcp", Address: server.Listener.Addr().String()}, nil)
	require.ErrorContains(t, err, "--running=wait")
	assert.Zero(t, shutdowns.Load())
}

func TestInterruptDaemonFallsBackForOlderDaemon(t *testing.T) {
	testenv.SetDataDir(t)
	mockIdentifyProcess(t, func(int) processIdentity { return processIsRoborev })
	var pingCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/ping" {
			pingCalls.Add(1)
			fmt.Fprintf(w, `{"ok":true,"service":"roborev","pid":%d}`, os.Getpid())
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	endpoint := DaemonEndpoint{Network: "tcp", Address: server.Listener.Addr().String()}
	wantErr := errors.New("fallback called")
	info := &RuntimeInfo{PID: os.Getpid(), Network: endpoint.Network, Address: endpoint.Address}
	calls := 0
	err := InterruptDaemonWithFallback(info, func(_ context.Context, gotInfo *RuntimeInfo, got DaemonEndpoint) error {
		calls++
		assert.Same(t, info, gotInfo)
		assert.Equal(t, endpoint, got)
		return wantErr
	})
	require.ErrorIs(t, err, wantErr)
	assert.Equal(t, 1, calls)
	assert.Equal(t, int32(2), pingCalls.Load())
}

func TestInterruptDaemonRejectsFallbackWhenEndpointPIDChanges(t *testing.T) {
	testenv.SetDataDir(t)
	mockIdentifyProcess(t, func(int) processIdentity { return processIsRoborev })
	var pingCalls atomic.Int32
	var fallbackCalls atomic.Int32
	var compatibilityMutation atomic.Bool
	var replacement atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/ping":
			pid := os.Getpid()
			if pingCalls.Add(1) > 1 && replacement.Load() {
				pid++
			}
			fmt.Fprintf(w, `{"ok":true,"service":"roborev","pid":%d}`, pid)
		case "/api/shutdown/interrupt":
			replacement.Store(true)
			http.NotFound(w, r)
		case "/api/update/prepare":
			compatibilityMutation.Store(true)
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"lease_token":"lease-1","policy":"interrupt","expires_at":"2030-01-01T00:00:00Z","running_jobs":0,"targeted_running_jobs":0,"active_workers":0,"recovering":false}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	endpoint := DaemonEndpoint{Network: "tcp", Address: server.Listener.Addr().String()}
	fallbackErr := errors.New("fallback should not run for a replacement")
	info := &RuntimeInfo{PID: os.Getpid(), Network: endpoint.Network, Address: endpoint.Address}

	err := InterruptDaemonWithFallback(
		info,
		func(ctx context.Context, gotInfo *RuntimeInfo, got DaemonEndpoint) error {
			fallbackCalls.Add(1)
			assert.Same(t, info, gotInfo)
			assert.Equal(t, endpoint, got)
			_, err := got.APIClient(time.Second).PrepareUpdateWithResponse(
				ctx, &generated.PrepareUpdateRequestOptions{
					Body: &generated.PrepareUpdateBody{
						OwnerID: "legacy-fallback",
						Policy:  generated.Interrupt,
					},
				},
			)
			if err != nil {
				return err
			}
			return fallbackErr
		},
	)

	require.ErrorIs(t, err, ErrInterruptShutdownUnsupported)
	assert.Equal(t, int32(2), pingCalls.Load())
	assert.Zero(t, fallbackCalls.Load())
	assert.False(t, compatibilityMutation.Load())
}
