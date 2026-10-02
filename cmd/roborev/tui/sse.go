package tui

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/cenkalti/backoff/v7"

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
	ctx, cancel := context.WithCancel(context.Background())
	cancelDone := make(chan struct{})
	go func() {
		defer close(cancelDone)
		select {
		case <-stopCh:
			cancel()
		case <-ctx.Done():
		}
	}()
	defer func() {
		cancel()
		<-cancelDone
	}()
	policy := backoff.NewExponentialBackOff()
	policy.InitialInterval = time.Second
	policy.MaxInterval = 30 * time.Second
	policy.Multiplier = 2
	_, _ = backoff.Retry(ctx, func() (struct{}, error) {
		connected, err := sseReadLoop(ctx, endpoint, sseCh)
		// A stream that read events starts a fresh failure sequence.
		if connected {
			policy.Reset()
		}
		return struct{}{}, permanentSSEError(err)
	}, backoff.WithBackOff(policy), backoff.WithMaxTries(0), backoff.WithMaxElapsedTime(0))
}

func permanentSSEError(err error) error {
	if daemon.IsDaemonAccessError(err) {
		return backoff.Permanent(err)
	}
	return err
}

// sseReadLoop connects to the event stream and reads NDJSON lines until
// the connection drops or ctx ends. Returns (connected, nil) when ctx
// ends, (connected, err) on connection/decode failure. connected is
// true if at least one event was successfully read.
func sseReadLoop(
	ctx context.Context,
	endpoint daemon.DaemonEndpoint,
	sseCh chan<- struct{},
) (connected bool, err error) {
	resp, err := endpoint.APIClient(0).StreamEventsRaw(ctx, nil)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		return false, fmt.Errorf("%w: server error (%d)", daemon.ErrDaemonAccessDenied, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("stream events: %s", resp.Status)
	}

	decoder := jsontext.NewDecoder(resp.Body)
	for {
		var event daemon.Event
		if err := json.UnmarshalDecode(decoder, &event); err != nil {
			select {
			case <-ctx.Done():
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
