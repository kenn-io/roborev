package daemon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
)

func TestInterruptShutdownRequeuesRunningJobs(t *testing.T) {
	for _, tc := range []struct {
		name            string
		alreadyDraining bool
	}{{"fresh", false}, {"already draining", true}} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			server, db, dir := newTestServer(t)
			createTestJob(t, db, dir, "interrupt-shutdown", "test")
			job, err := db.ClaimJob("worker-interrupt")
			require.NoError(t, err)
			require.NotNil(t, job)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			server.workerPool.registerRunningJob(job.ID, cancel, time.Time{}, nil)
			if tc.alreadyDraining {
				require.NoError(t, server.beginShutdownDrain())
			}

			response := postUpdateDrain(t, server, "/api/shutdown/interrupt", struct{}{})
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())

			require.ErrorIs(t, ctx.Err(), context.Canceled)
			assert.True(server.workerPool.stopped)
			assert.True(server.shutdownDraining)
			requested := false
			select {
			case <-server.ShutdownRequested():
				requested = true
			default:
			}
			assert.True(requested)
			// The canceled worker unwinds through the update-interruption path.
			assert.True(server.workerPool.handleUpdateInterruption(ctx, "worker-interrupt", job))
			stored, err := db.GetJobByID(job.ID)
			require.NoError(t, err)
			assert.Equal(storage.JobStatusQueued, stored.Status)
			assert.Equal(job.RetryCount, stored.RetryCount)
		})
	}
}

func TestInterruptShutdownRejectsActiveUpdateDrain(t *testing.T) {
	server, _, _ := newTestServer(t)
	prepareUpdateDrain(t, server, "owner", "wait")
	response := postUpdateDrain(t, server, "/api/shutdown/interrupt", struct{}{})
	assert.Equal(t, http.StatusConflict, response.Code)
	assert.False(t, server.shutdownDraining)
}

func TestInterruptShutdownGateFailureKeepsDaemonRunning(t *testing.T) {
	server, db, _ := newTestServer(t)
	_, err := db.ExecContext(t.Context(), `
		CREATE TRIGGER fail_gate BEFORE INSERT ON daemon_state
		WHEN NEW.key = 'shutdown_draining'
		BEGIN SELECT RAISE(FAIL, 'synthetic'); END
	`)
	require.NoError(t, err)
	response := postUpdateDrain(t, server, "/api/shutdown/interrupt", struct{}{})
	require.Equal(t, http.StatusInternalServerError, response.Code)
	assert.False(t, server.shutdownDraining)
	assert.False(t, server.workerPool.stopped)
}

func TestRequestInterruptShutdownRejectsOlderDaemon(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/shutdown/interrupt", r.URL.Path)
		http.NotFound(w, r)
	}))
	defer server.Close()
	ep, err := ParseEndpoint(server.Listener.Addr().String())
	require.NoError(t, err)
	err = requestDaemonShutdown(t.Context(), ep, func() bool { return false }, true)
	require.ErrorIs(t, err, ErrInterruptUnsupported)
}

func TestRequestDaemonShutdownReportsDaemonError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"detail":"list running jobs: synthetic"}`, http.StatusConflict)
	}))
	defer server.Close()
	ep, err := ParseEndpoint(server.Listener.Addr().String())
	require.NoError(t, err)
	err = requestDaemonShutdown(t.Context(), ep, func() bool { return false }, true)
	require.ErrorContains(t, err, "list running jobs: synthetic")
}
