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
	done := make(chan error, 1)
	go func() {
		done <- KillDaemon(&RuntimeInfo{PID: pid, Network: "tcp", Address: address, SourcePath: path})
	}()

	require.Eventually(t, shutdown.Load, 5*time.Second, 10*time.Millisecond)
	exit()
	var result error
	require.Eventually(t, func() bool {
		select {
		case result = <-done:
			return true
		default:
			return false
		}
	}, 5*time.Second, 10*time.Millisecond)
	require.NoError(t, result)
	assert.NoFileExists(t, path)
}
