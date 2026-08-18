package daemon

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
)

type updateDrainTestStatus struct {
	LeaseToken          string    `json:"lease_token"`
	Policy              string    `json:"policy"`
	ExpiresAt           time.Time `json:"expires_at"`
	RunningJobs         int       `json:"running_jobs"`
	TargetedRunningJobs int       `json:"targeted_running_jobs"`
	ActiveWorkers       int       `json:"active_workers"`
	Recovering          bool      `json:"recovering"`
}

func postUpdateDrain(
	t *testing.T, server *Server, path string, body any,
) *httptest.ResponseRecorder {
	t.Helper()
	data, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(w, req)
	return w
}

func prepareUpdateDrain(
	t *testing.T, server *Server, owner, policy string,
) updateDrainTestStatus {
	t.Helper()
	w := postUpdateDrain(t, server, "/api/update/prepare", map[string]string{
		"owner_id": owner,
		"policy":   policy,
	})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var status updateDrainTestStatus
	require.NoError(t, json.NewDecoder(w.Result().Body).Decode(&status))
	return status
}

func releaseUpdateDrain(
	t *testing.T, server *Server, token string,
) *httptest.ResponseRecorder {
	t.Helper()
	return postUpdateDrain(t, server, "/api/update/release", map[string]string{
		"lease_token": token,
	})
}

func TestPrepareUpdateDrainBlocksClaimsAndReportsRunningJobs(t *testing.T) {
	server, db, dir := newTestServer(t)
	createTestJob(t, db, dir, "running-update", "test")
	claimed, err := db.ClaimJob("worker-update")
	require.NoError(t, err)
	require.NotNil(t, claimed)

	status := prepareUpdateDrain(t, server, "owner-1", "wait")

	assert.NotEmpty(t, status.LeaseToken)
	assert.Equal(t, "wait", status.Policy)
	assert.Equal(t, 1, status.RunningJobs)
	assert.Equal(t, 1, status.TargetedRunningJobs)
	draining, err := db.IsShutdownDraining()
	require.NoError(t, err)
	assert.True(t, draining)
	createTestJob(t, db, dir, "queued-during-update", "test")
	next, err := db.ClaimJob("worker-next")
	require.NoError(t, err)
	assert.Nil(t, next)
}

func TestPrepareUpdateDrainOwnerAndPolicyConflicts(t *testing.T) {
	server, _, _ := newTestServer(t)
	first := prepareUpdateDrain(t, server, "owner-1", "wait")
	second := prepareUpdateDrain(t, server, "owner-1", "wait")
	assert.Equal(t, first.LeaseToken, second.LeaseToken)

	changedPolicy := postUpdateDrain(t, server, "/api/update/prepare", map[string]string{
		"owner_id": "owner-1",
		"policy":   "interrupt",
	})
	assert.Equal(t, http.StatusConflict, changedPolicy.Code)
	assert.Contains(t, changedPolicy.Body.String(), "different policy")

	otherOwner := postUpdateDrain(t, server, "/api/update/prepare", map[string]string{
		"owner_id": "owner-2",
		"policy":   "wait",
	})
	assert.Equal(t, http.StatusConflict, otherOwner.Code)
	assert.Contains(t, otherOwner.Body.String(), "lease expires in")
}

func TestAbortUpdateDrainRejectsRunningReviewsAndReopensClaims(t *testing.T) {
	server, db, dir := newTestServer(t)
	createTestJob(t, db, dir, "abort-running", "test")
	claimed, err := db.ClaimJob("worker-update")
	require.NoError(t, err)
	require.NotNil(t, claimed)

	w := postUpdateDrain(t, server, "/api/update/prepare", map[string]string{
		"owner_id": "owner-abort",
		"policy":   "abort",
	})

	assert.Equal(t, http.StatusConflict, w.Code)
	draining, err := db.IsShutdownDraining()
	require.NoError(t, err)
	assert.False(t, draining)
}

func TestRenewAndReleaseUpdateDrain(t *testing.T) {
	server, db, _ := newTestServer(t)
	lease := prepareUpdateDrain(t, server, "owner-renew", "wait")

	renew := postUpdateDrain(t, server, "/api/update/renew", map[string]string{
		"lease_token": lease.LeaseToken,
	})
	require.Equal(t, http.StatusOK, renew.Code, renew.Body.String())
	var renewed updateDrainTestStatus
	require.NoError(t, json.NewDecoder(renew.Result().Body).Decode(&renewed))
	assert.Equal(t, lease.LeaseToken, renewed.LeaseToken)
	assert.False(t, renewed.ExpiresAt.Before(lease.ExpiresAt))

	release := releaseUpdateDrain(t, server, lease.LeaseToken)
	require.Equal(t, http.StatusOK, release.Code, release.Body.String())
	assert.Contains(t, release.Body.String(), `"released":true`)
	draining, err := db.IsShutdownDraining()
	require.NoError(t, err)
	assert.False(t, draining)
}

func TestWaitUpdateDrainExpiresAndReopensClaims(t *testing.T) {
	server, db, _ := newTestServer(t)
	prepareUpdateDrain(t, server, "owner-expiry", "wait")

	server.shutdownDrainMu.Lock()
	lease := server.updateDrain
	lease.expiresAt = time.Now().Add(-time.Second)
	server.updateCoordinator.armExpiryLocked(lease, time.Millisecond)
	server.shutdownDrainMu.Unlock()

	require.Eventually(t, func() bool {
		draining, err := db.IsShutdownDraining()
		return err == nil && !draining
	}, time.Second, 10*time.Millisecond)
}

func TestFailedAbortRollbackRemainsVisibleUntilGateRecovers(t *testing.T) {
	server, db, dir := newTestServer(t)
	createTestJob(t, db, dir, "rollback-running", "test")
	claimed, err := db.ClaimJob("worker-update")
	require.NoError(t, err)
	require.NotNil(t, claimed)
	_, err = db.Exec(`
		CREATE TRIGGER fail_update_drain_release
		BEFORE UPDATE ON daemon_state
		WHEN NEW.key = 'shutdown_draining' AND NEW.value = 'false'
		BEGIN
			SELECT RAISE(FAIL, 'synthetic release failure');
		END
	`)
	require.NoError(t, err)

	w := postUpdateDrain(t, server, "/api/update/prepare", map[string]string{
		"owner_id": "owner-rollback",
		"policy":   "abort",
	})

	assert.Equal(t, http.StatusConflict, w.Code)
	active, policy, expiresAt := server.updateDrainStatus()
	assert.True(t, active)
	assert.Equal(t, "abort", policy)
	assert.False(t, expiresAt.After(time.Now()))
	draining, err := db.IsShutdownDraining()
	require.NoError(t, err)
	assert.True(t, draining)

	_, err = db.Exec(`DROP TRIGGER fail_update_drain_release`)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		draining, drainErr := db.IsShutdownDraining()
		return drainErr == nil && !draining
	}, time.Second, 10*time.Millisecond)
}

func TestReleaseUpdateDrainCannotClearShutdownOwnership(t *testing.T) {
	server, db, _ := newTestServer(t)
	lease := prepareUpdateDrain(t, server, "owner-shutdown", "wait")
	require.NoError(t, server.beginShutdownDrain())

	w := releaseUpdateDrain(t, server, lease.LeaseToken)

	assert.Equal(t, http.StatusConflict, w.Code)
	draining, err := db.IsShutdownDraining()
	require.NoError(t, err)
	assert.True(t, draining)
}

func TestPrepareUpdateDrainRejectsShutdownOwnership(t *testing.T) {
	server, _, _ := newTestServer(t)
	require.NoError(t, server.beginShutdownDrain())

	w := postUpdateDrain(t, server, "/api/update/prepare", map[string]string{
		"owner_id": "owner-late",
		"policy":   "wait",
	})

	assert.Equal(t, http.StatusConflict, w.Code)
	assert.Contains(t, w.Body.String(), "shutdown")
}

func TestInterruptDrainWaitsUntilEveryTargetUnwinds(t *testing.T) {
	for _, terminal := range []storage.JobStatus{
		storage.JobStatusDone,
		storage.JobStatusCanceled,
		storage.JobStatusFailed,
		storage.JobStatusQueued,
	} {
		t.Run(string(terminal), func(t *testing.T) {
			server, db, dir := newTestServer(t)
			job := createTestJob(t, db, dir, "interrupt-"+string(terminal), "test")
			claimed, err := db.ClaimJob("worker-update")
			require.NoError(t, err)
			require.Equal(t, job.ID, claimed.ID)
			server.workerPool.activeWorkers.Store(1)
			lease := prepareUpdateDrain(t, server, "owner-interrupt", "interrupt")

			firstRelease := releaseUpdateDrain(t, server, lease.LeaseToken)
			require.Equal(t, http.StatusOK, firstRelease.Code, firstRelease.Body.String())
			assert.Contains(t, firstRelease.Body.String(), `"released":false`)
			active, policy, expiresAt := server.updateDrainStatus()
			assert.True(t, active)
			assert.Equal(t, "interrupt", policy)
			assert.False(t, expiresAt.After(time.Now()))

			if terminal == storage.JobStatusQueued {
				_, err = db.Exec(`
					UPDATE review_jobs
					SET status = 'queued', worker_id = NULL, started_at = NULL
					WHERE id = ?
				`, job.ID)
				require.NoError(t, err)
			} else {
				setJobStatus(t, db, job.ID, terminal)
			}
			server.workerPool.activeWorkers.Store(0)
			require.Eventually(t, func() bool {
				draining, drainErr := db.IsShutdownDraining()
				return drainErr == nil && !draining
			}, time.Second, 10*time.Millisecond)
		})
	}
}

func TestStatusReportsUpdateDrain(t *testing.T) {
	server, _, _ := newTestServer(t)
	lease := prepareUpdateDrain(t, server, "owner-status", "wait")
	w := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var status storage.DaemonStatus
	require.NoError(t, json.NewDecoder(w.Result().Body).Decode(&status))
	assert.True(t, status.UpdateDraining)
	assert.Equal(t, "wait", status.UpdateDrainPolicy)
	assert.Equal(t, lease.ExpiresAt.Format(time.RFC3339), status.UpdateDrainExpiresAt)
}
