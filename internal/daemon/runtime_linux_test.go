//go:build linux

package daemon

import (
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/testutil"
)

func TestKillDaemonFinishesForUnreapedChild(t *testing.T) {
	done := make(chan struct{})
	finished := func() bool {
		select {
		case <-done:
			return true
		default:
			return false
		}
	}
	// The child is reaped first during cleanup, releasing the wait even on failure.
	t.Cleanup(func() { require.Eventually(t, finished, 5*time.Second, 10*time.Millisecond) })
	pid, exit := testutil.StartLivenessChild(t)
	address, mux := startMockDaemon(t)
	mux.HandleFunc("/api/ping", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(PingInfo{OK: true, Service: daemonServiceName, PID: pid})
	})
	var shutdown atomic.Bool
	mux.HandleFunc("/api/shutdown", func(w http.ResponseWriter, r *http.Request) {
		shutdown.Store(true)
		w.WriteHeader(http.StatusOK)
	})
	path := createRuntimeFile(t, t.TempDir(), pid, nil)
	var result error
	go func() {
		result = KillDaemon(&RuntimeInfo{PID: pid, Network: "tcp", Address: address, SourcePath: path})
		close(done)
	}()

	require.Eventually(t, shutdown.Load, 5*time.Second, 10*time.Millisecond)
	assert.False(t, finished(), "shutdown must wait for the live daemon")
	exit()
	require.Eventually(t, finished, 5*time.Second, 10*time.Millisecond)
	require.NoError(t, result)
	assert.NoFileExists(t, path)
}
