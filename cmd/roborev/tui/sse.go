package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	tea "charm.land/bubbletea/v2"

	"go.kenn.io/roborev/internal/daemon"
)

// sseEventMsg signals that the daemon broadcast an event.
// The TUI uses this to trigger an immediate data refresh.
type sseEventMsg struct{}

// startSSESubscription maintains a persistent NDJSON connection to the
// daemon's /api/stream/events endpoint. On each received event it sends
// a non-blocking signal to sseCh. The goroutine reconnects with
// exponential backoff on errors and exits when stopCh is closed.
func startSSESubscription(
	endpoint daemon.DaemonEndpoint,
	sseCh chan<- struct{},
	stopCh <-chan struct{},
) {
	const maxBackoff = 30 * time.Second
	backoff := time.Second

	for {
		connected, err := sseReadLoop(endpoint, sseCh, stopCh)
		if err == nil {
			return
		}

		// Reset backoff after a connection that successfully read events,
		// since the next failure is likely a fresh problem (daemon restart).
		if connected {
			backoff = time.Second
		}

		select {
		case <-stopCh:
			return
		case <-time.After(backoff):
		}

		backoff = min(backoff*2, maxBackoff)
	}
}

// sseReadLoop connects to the event stream and reads NDJSON lines until
// the connection drops or stopCh fires. Returns (false, nil) when stopCh
// is closed, (connected, err) on connection/decode failure. connected is
// true if at least one event was successfully read.
func sseReadLoop(
	endpoint daemon.DaemonEndpoint,
	sseCh chan<- struct{},
	stopCh <-chan struct{},
) (connected bool, err error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		select {
		case <-stopCh:
			cancel()
		case <-ctx.Done():
		}
	}()

	client := endpoint.HTTPClient(0)
	req, err := http.NewRequestWithContext(
		ctx, http.MethodGet,
		endpoint.BaseURL()+"/api/stream/events", nil,
	)
	if err != nil {
		return false, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("stream events: %s", resp.Status)
	}

	decoder := json.NewDecoder(resp.Body)
	for {
		var event daemon.Event
		if err := decoder.Decode(&event); err != nil {
			select {
			case <-stopCh:
				return connected, nil
			default:
				return connected, err
			}
		}
		connected = true

		select {
		case sseCh <- struct{}{}:
		default:
		}
	}
}

// waitForSSE returns a tea.Cmd that blocks until a signal arrives on
// sseCh or stopCh is closed, then delivers an sseEventMsg (or nil on
// stop) to the Bubbletea event loop. Accepting stopCh avoids the need
// to close sseCh on reconnect, which would race with the producer goroutine.
func waitForSSE(sseCh <-chan struct{}, stopCh <-chan struct{}) tea.Cmd {
	return func() tea.Msg {
		select {
		case <-sseCh:
			return sseEventMsg{}
		case <-stopCh:
			return nil
		}
	}
}
