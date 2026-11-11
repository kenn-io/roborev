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
)

func TestInterruptShutdownTargetsRunningAttempts(t *testing.T) {
	for _, alreadyDraining := range []bool{false, true} {
		t.Run(fmt.Sprint(alreadyDraining), func(t *testing.T) {
			server, db, dir := newTestServer(t)
			createTestJob(t, db, dir, "interrupt-shutdown", "test")
			job, err := db.ClaimJob("worker-interrupt")
			require.NoError(t, err)
			require.NotNil(t, job)
			if alreadyDraining {
				require.NoError(t, server.beginShutdownDrain())
			}
			response := postUpdateDrain(t, server, "/api/shutdown/interrupt", map[string]int{"expected_pid": os.Getpid()})
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			// Registration after the snapshot must still observe the interrupt.
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			server.workerPool.registerRunningJob(job.ID, cancel, time.Time{})
			require.ErrorIs(t, ctx.Err(), context.Canceled)
			assert.True(t, server.workerPool.handleUpdateInterruption(t.Context(), "worker-interrupt", job))
			stored, err := db.GetJobByID(job.ID)
			require.NoError(t, err)
			assert.Equal(t, storage.JobStatusQueued, stored.Status)
			assert.Equal(t, job.RetryCount, stored.RetryCount)
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
}

func TestInterruptShutdownSnapshotFailureRollsBackGate(t *testing.T) {
	server, db, _ := newTestServer(t)
	_, err := db.ExecContext(t.Context(), "DROP TABLE review_jobs")
	require.NoError(t, err)
	response := postUpdateDrain(t, server, "/api/shutdown/interrupt", map[string]int{"expected_pid": os.Getpid()})
	require.Equal(t, http.StatusInternalServerError, response.Code)
	draining, err := db.IsShutdownDraining()
	require.NoError(t, err)
	assert.False(t, draining)
	assert.False(t, server.shutdownDraining)
}

func TestInterruptShutdownRequestOlderDaemon(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/shutdown/interrupt", r.URL.Path)
		http.NotFound(w, r)
	}))
	defer server.Close()
	ep, err := ParseEndpoint(server.Listener.Addr().String())
	require.NoError(t, err)
	err = requestDaemonShutdown(t.Context(), ep, func() bool { return false }, 123, true)
	require.ErrorContains(t, err, "--running=wait")
}

func TestInterruptShutdownRequestKeepsOriginalPID(t *testing.T) {
	const pid = 123
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/shutdown/interrupt", r.URL.Path)
		var body struct {
			ExpectedPID int `json:"expected_pid"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		assert.Equal(t, pid, body.ExpectedPID)
		w.WriteHeader(http.StatusPreconditionFailed)
	}))
	defer server.Close()
	ep, err := ParseEndpoint(server.Listener.Addr().String())
	require.NoError(t, err)
	err = requestDaemonShutdown(t.Context(), ep, func() bool { return false }, pid, true)
	require.ErrorContains(t, err, "412")
}

func TestInterruptShutdownRequestAcceptsOriginalExit(t *testing.T) {
	var dead atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dead.Store(true)
		w.WriteHeader(http.StatusPreconditionFailed)
	}))
	defer server.Close()
	ep, err := ParseEndpoint(server.Listener.Addr().String())
	require.NoError(t, err)
	require.NoError(t, requestDaemonShutdown(t.Context(), ep, dead.Load, 123, true))
}
