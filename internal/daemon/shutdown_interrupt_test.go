package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testenv"
)

func TestInterruptShutdownTargetsRunningAttempts(t *testing.T) {
	for _, alreadyDraining := range []bool{false, true} {
		t.Run(fmt.Sprint(alreadyDraining), func(t *testing.T) {
			server, db, dir := newTestServer(t)
			createTestJob(t, db, dir, "interrupt-shutdown", "test")
			job, err := db.ClaimJob("worker-interrupt")
			require.NoError(t, err)
			require.NotNil(t, job)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			server.workerPool.registerRunningJob(job.ID, cancel, time.Time{})
			if alreadyDraining {
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
			assert.True(t, server.workerPool.handleUpdateInterruption(ctx, "worker-interrupt", job))
			stored, err := db.GetJobByID(job.ID)
			require.NoError(t, err)
			assert.Equal(t, storage.JobStatusQueued, stored.Status)
			assert.Equal(t, 0, stored.RetryCount)
		})
	}
}

func TestInterruptShutdownStopRetriesFailedRequeue(t *testing.T) {
	server, db, dir := newTestServer(t)
	createTestJob(t, db, dir, "interrupt-requeue-retry", "test")
	job, err := db.ClaimJob("worker-interrupt")
	require.NoError(t, err)
	require.NotNil(t, job)
	response := postUpdateDrain(t, server, "/api/shutdown/interrupt", map[string]int{"expected_pid": os.Getpid()})
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	_, err = db.ExecContext(t.Context(), fmt.Sprintf(`
		CREATE TRIGGER fail_update_requeue
		BEFORE UPDATE ON review_jobs
		WHEN OLD.id = %d AND NEW.status = 'queued'
		BEGIN
			SELECT RAISE(FAIL, 'synthetic requeue failure');
		END
	`, job.ID))
	require.NoError(t, err)
	assert.True(t, server.workerPool.handleUpdateInterruption(t.Context(), "worker-interrupt", job))
	stored, err := db.GetJobByID(job.ID)
	require.NoError(t, err)
	assert.Equal(t, storage.JobStatusRunning, stored.Status)
	_, err = db.ExecContext(t.Context(), `DROP TRIGGER fail_update_requeue`)
	require.NoError(t, err)
	require.NoError(t, server.Stop())
	stored, err = db.GetJobByID(job.ID)
	require.NoError(t, err)
	assert.Equal(t, storage.JobStatusQueued, stored.Status)
	assert.Equal(t, 0, stored.RetryCount)
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

func TestInterruptShutdownDatabaseFailure(t *testing.T) {
	server, db, _ := newTestServer(t)
	require.NoError(t, db.Close())
	response := postUpdateDrain(t, server, "/api/shutdown/interrupt", map[string]int{"expected_pid": os.Getpid()})
	require.Equal(t, http.StatusInternalServerError, response.Code)
}

func TestInterruptShutdownRequest(t *testing.T) {
	const pid = 123
	for _, tc := range []struct {
		name   string
		status int
		detail string
		dead   bool
		want   string
	}{
		{name: "older daemon", status: http.StatusNotFound, want: "--running=wait"},
		{name: "PID changed", status: http.StatusPreconditionFailed, detail: "daemon PID changed", want: "daemon PID changed"},
		{name: "original exited", status: http.StatusPreconditionFailed, dead: true},
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
				w.Header().Set("Content-Type", "application/problem+json")
				w.WriteHeader(tc.status)
				assert.NoError(t, json.NewEncoder(w).Encode(map[string]string{"detail": tc.detail}))
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
	err := InterruptDaemon(&RuntimeInfo{Network: "tcp", Address: server.Listener.Addr().String()})
	require.ErrorContains(t, err, "--running=wait")
	assert.Zero(t, shutdowns.Load())
}
