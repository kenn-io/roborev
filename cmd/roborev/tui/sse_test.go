package tui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/daemon"
)

func TestSSESubscription_ReceivesEvents(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/stream/events", r.URL.Path)

		w.Header().Set("Content-Type", "application/x-ndjson")
		json.NewEncoder(w).Encode(daemon.Event{Type: "review.closed", JobID: 42})
		w.(http.Flusher).Flush()

		<-r.Context().Done()
	}))
	defer ts.Close()

	ep := testEndpointFromURL(ts.URL)
	sseCh := make(chan struct{}, 1)
	stopCh := make(chan struct{})
	defer close(stopCh)

	go startSSESubscription(ep, sseCh, stopCh)

	select {
	case <-sseCh:
		// Got the signal
	case <-time.After(2 * time.Second):
		require.FailNow(t, "timed out waiting for SSE event signal")
	}
}

func TestSSESubscription_StopsOnStopChannel(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer ts.Close()

	ep := testEndpointFromURL(ts.URL)
	sseCh := make(chan struct{}, 1)
	stopCh := make(chan struct{})

	done := make(chan struct{})
	go func() {
		startSSESubscription(ep, sseCh, stopCh)
		close(done)
	}()

	close(stopCh)

	select {
	case <-done:
		// Goroutine exited
	case <-time.After(2 * time.Second):
		require.FailNow(t, "SSE goroutine did not exit after stop signal")
	}
}

func TestSSESubscription_ReconnectsOnError(t *testing.T) {
	var attempt atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempt.Add(1)
		if n == 1 {
			return
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		json.NewEncoder(w).Encode(daemon.Event{Type: "review.closed", JobID: 1})
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer ts.Close()

	ep := testEndpointFromURL(ts.URL)
	sseCh := make(chan struct{}, 1)
	stopCh := make(chan struct{})
	defer close(stopCh)

	go startSSESubscription(ep, sseCh, stopCh)

	select {
	case <-sseCh:
		require.GreaterOrEqual(t, int(attempt.Load()), 2, "should have reconnected")
	case <-time.After(5 * time.Second):
		require.FailNow(t, "timed out waiting for reconnected SSE event")
	}
}

func TestWaitForSSE_ReturnsOnSignal(t *testing.T) {
	ch := make(chan struct{}, 1)
	stopCh := make(chan struct{})
	ch <- struct{}{}

	cmd := waitForSSE(ch, stopCh)
	msg := cmd()

	_, ok := msg.(sseEventMsg)
	assert.True(t, ok, "expected sseEventMsg, got %T", msg)
}

func TestWaitForSSE_ReturnsNilOnStop(t *testing.T) {
	ch := make(chan struct{}, 1)
	stopCh := make(chan struct{})
	close(stopCh)

	cmd := waitForSSE(ch, stopCh)
	msg := cmd()

	assert.Nil(t, msg, "expected nil on stop")
}
