//go:build integration

package daemon

import (
	"math"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestKillDaemonMakesHTTPForLoopback(t *testing.T) {
	// Create a test server that tracks if shutdown was called
	var shutdownCalled atomic.Bool
	var requestCount atomic.Int32

	addr, mux := startMockDaemon(t)
	mux.HandleFunc("/api/shutdown", func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		shutdownCalled.Store(true)
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		// Return 500 for status checks so KillDaemon exits quickly
		w.WriteHeader(http.StatusInternalServerError)
	})

	info := &RuntimeInfo{
		PID:     math.MaxInt32,
		Address: addr, // Loopback address from test server
	}

	// This should make HTTP request since address is loopback
	KillDaemon(info)

	if !shutdownCalled.Load() {
		assert.Condition(t, func() bool {
			return false
		}, "KillDaemon should make HTTP shutdown request to loopback addresses")
	}
	if requestCount.Load() == 0 {
		assert.Condition(t, func() bool {
			return false
		}, "KillDaemon should have made at least one HTTP request")
	}
}
