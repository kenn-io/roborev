package telemetry

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json/v2"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/gofrs/flock"
	"go.kenn.io/kit/atomicfile"
	"go.kenn.io/kit/telemetry/posthog"

	"go.kenn.io/roborev/internal/config"
)

// ClientGate shares receipts across processes and keeps a memory fallback when storage fails.
// File and network work belongs at startup or in the caller's send goroutine.
type ClientGate struct {
	mu       sync.Mutex
	ioMuOnce sync.Once
	ioMu     chan struct{}
	state    clientState
	path     string
	// unsupported records a newer-version file, which this process must not read or replace.
	unsupported bool
	// unsaved means state holds changes a failed write could not save. While the file still
	// holds unsavedFrom, the bytes those changes were made from, state stays the newer copy.
	// ioMu guards both.
	unsaved     bool
	unsavedFrom []byte
	now         func() time.Time
}

const clientStateVersion = 1

// receiptPollInterval paces receipt lock retries and probe waits.
const receiptPollInterval = 10 * time.Millisecond

type clientState struct {
	Version   int                             `json:"version"`
	Day       string                          `json:"day"`
	Endpoints map[string]*clientEndpointState `json:"endpoints"`
}

type clientEndpointState struct {
	Disabled bool                     `json:"disabled"`
	Events   map[string]clientAttempt `json:"events"`
}

type clientAttempt struct {
	Sent     bool `json:"sent"`
	Attempts int  `json:"attempts"`
	// Rejected counts answers without a receipt. Six quiet the event until the next UTC day, so a
	// daemon too old to send receipts isn't retried on every command; unanswered requests only back off.
	Rejected   int       `json:"rejected,omitempty"`
	Next       time.Time `json:"next"`
	Pending    int       `json:"pending,omitempty"`
	InFlight   bool      `json:"in_flight,omitempty"`
	LeaseUntil time.Time `json:"lease_until,omitempty"`
	// CallID names the agent call being sent, so the daemon counts a retried call once.
	CallID string `json:"call_id,omitempty"`
}

var DefaultClientGate = &ClientGate{}

func (g *ClientGate) clock() time.Time {
	if g.now != nil {
		return g.now()
	}
	return time.Now()
}

func (g *ClientGate) lockIO(ctx context.Context) bool {
	g.ioMuOnce.Do(func() {
		g.ioMu = make(chan struct{}, 1)
		g.ioMu <- struct{}{}
	})
	if ctx.Err() != nil {
		return false
	}
	select {
	case <-ctx.Done():
		return false
	case <-g.ioMu:
		if ctx.Err() != nil {
			g.unlockIO()
			return false
		}
		return true
	}
}

func (g *ClientGate) unlockIO() {
	g.ioMu <- struct{}{}
}

// ReadyEvent checks memory only, so callers can gate work before scheduling a send.
func (g *ClientGate) ReadyEvent(scope, event, surface, screen string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.unsupported {
		return false
	}
	if g.path != filepath.Join(config.DataDir(), "telemetry-client.json") || g.state.Day != g.clock().UTC().Format(time.DateOnly) {
		return true
	}
	endpoint := g.state.Endpoints[scope]
	if endpoint == nil {
		return true
	}
	a := endpoint.Events[event+":"+surface+":"+screen]
	if event == EventAgentActive {
		// Agent calls queue during backoff; postAgentCalls paces their delivery.
		return !endpoint.Disabled && a.Rejected < 6
	}
	return !endpoint.Disabled && !a.Sent && a.Rejected < 6 && !g.clock().Before(a.Next)
}

// AwaitProbe reports whether endpoint activity may follow a successful receipt
// for the event that checks whether the daemon accepts telemetry. While a
// reservation for that event is unsettled, it rereads the shared receipt until
// its sender settles it, its lease ends, or ctx ends.
func (g *ClientGate) AwaitProbe(ctx context.Context, scope, event, surface string) bool {
	key := event + ":" + surface + ":"
	for first := true; ; first = false {
		ready, inFlight, leaseUntil := g.probeState(scope, key)
		if ready || !inFlight {
			return ready
		}
		if !first {
			if !g.clock().Before(leaseUntil) {
				return false
			}
			select {
			case <-ctx.Done():
				return false
			case <-time.After(receiptPollInterval):
			}
		}
		// Another sender may have settled the probe since this process last read the file.
		if !g.update(ctx, func(*clientState) {}) {
			return false
		}
	}
}

func (g *ClientGate) probeState(scope, key string) (ready, inFlight bool, leaseUntil time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.unsupported {
		return false, false, time.Time{}
	}
	if g.path != filepath.Join(config.DataDir(), "telemetry-client.json") || g.state.Day != g.clock().UTC().Format(time.DateOnly) {
		return false, false, time.Time{}
	}
	endpoint := g.state.Endpoints[scope]
	if endpoint == nil || endpoint.Disabled {
		return false, false, time.Time{}
	}
	a := endpoint.Events[key]
	return a.Sent, a.InFlight, a.LeaseUntil
}

// Load primes memory before callers register activity callbacks. It reads only
// while it can take the receipt lock at once: a writer holding the lock may be
// replacing the file, and an open reader blocks that replacement on Windows.
// Without a read, the first update reads the file under the lock.
func (g *ClientGate) Load() {
	if !g.lockIO(context.Background()) {
		return
	}
	defer g.unlockIO()
	path := filepath.Join(config.DataDir(), "telemetry-client.json")
	var stored clientState
	unsupported := false
	lock := flock.New(path + ".lock")
	if locked, err := lock.TryLock(); locked && err == nil {
		data, err := os.ReadFile(path)
		_ = lock.Unlock()
		if err == nil {
			if json.Unmarshal(data, &stored) != nil {
				stored = clientState{}
			} else if stored.Version > clientStateVersion {
				unsupported = true
				stored = clientState{}
			} else if stored.Version < clientStateVersion {
				stored = clientState{}
			} else {
				stored.normalize()
			}
		}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.path = path
	g.state = stored
	g.unsaved, g.unsavedFrom = false, nil
	g.unsupported = unsupported
}

// update applies change under the receipt lock and saves it. Without usable storage, the change
// stays in memory until another process changes the file. The file then wins, because that
// process may have queued or settled calls meanwhile; an unsettled call is resent under its ID
// and the daemon counts it once.
func (g *ClientGate) update(ctx context.Context, change func(*clientState)) bool {
	applied, _ := g.updateSaved(ctx, change)
	return applied
}

// updateSaved also reports unsaved when a failed write left the change only in memory.
func (g *ClientGate) updateSaved(ctx context.Context, change func(*clientState)) (applied, unsaved bool) {
	if !g.lockIO(ctx) {
		return false, false
	}
	defer g.unlockIO()
	path := filepath.Join(config.DataDir(), "telemetry-client.json")
	var lock *flock.Flock
	if os.MkdirAll(filepath.Dir(path), 0o700) == nil {
		lock = flock.New(path + ".lock")
		lockCtx, cancel := context.WithTimeout(ctx, NotificationTimeout)
		locked, err := lock.TryLockContext(lockCtx, receiptPollInterval)
		cancel()
		// A wait that ran out or was canceled says nothing about storage, so memory stays as it is.
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) || (!locked && err == nil) {
			return false, false
		}
		if err != nil {
			lock = nil
		} else {
			held := lock
			defer func() { _ = held.Unlock() }()
		}
	}
	var stored clientState
	var raw []byte
	loaded := false
	unsupported := false
	if lock != nil {
		data, err := os.ReadFile(path)
		if err == nil {
			raw = data
			if json.Unmarshal(data, &stored) == nil {
				if stored.Version > clientStateVersion {
					unsupported = true
				} else {
					if stored.Version < clientStateVersion {
						stored = clientState{}
					}
					stored.normalize()
					loaded = true
				}
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			lock = nil
		}
	}
	if unsupported {
		g.mu.Lock()
		g.unsupported = true
		g.mu.Unlock()
		return false, false
	}
	g.mu.Lock()
	if g.path != path {
		g.state = clientState{}
		g.path = path
		g.unsaved, g.unsavedFrom = false, nil
		g.unsupported = false
	}
	if loaded && (!g.unsaved || !bytes.Equal(raw, g.unsavedFrom)) {
		g.state = stored
		g.unsaved, g.unsavedFrom = false, nil
	}
	day := g.clock().UTC().Format(time.DateOnly)
	if g.state.Day != day || g.state.Endpoints == nil {
		g.state = clientState{Version: clientStateVersion, Day: day, Endpoints: map[string]*clientEndpointState{}}
	}
	before, _ := json.Marshal(g.state, json.Deterministic(true))
	g.unsupported = false
	change(&g.state)
	data, err := json.Marshal(g.state, json.Deterministic(true))
	unchanged := err == nil && bytes.Equal(before, data)
	g.mu.Unlock()
	if unchanged || lock == nil || err != nil {
		return true, false
	}
	if !writeReceipts(path, data) {
		g.unsaved, g.unsavedFrom = true, raw
		return true, true
	}
	g.unsaved, g.unsavedFrom = false, nil
	return true, false
}

func writeReceipts(path string, data []byte) bool {
	f, err := os.CreateTemp(filepath.Dir(path), ".telemetry-client-*")
	if err != nil {
		return false
	}
	defer func() { _ = os.Remove(f.Name()) }()
	_, err = f.Write(data)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = atomicfile.Replace(f.Name(), path)
	}
	return err == nil
}

func (g *ClientGate) updateMemory(ctx context.Context, change func(*clientState)) bool {
	if !g.lockIO(ctx) {
		return false
	}
	defer g.unlockIO()
	path := filepath.Join(config.DataDir(), "telemetry-client.json")
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.unsupported {
		return false
	}
	if g.path != path {
		g.state = clientState{}
		g.path = path
		g.unsaved, g.unsavedFrom = false, nil
		g.unsupported = false
	}
	day := g.clock().UTC().Format(time.DateOnly)
	if g.state.Day != day || g.state.Endpoints == nil {
		g.state = clientState{Version: clientStateVersion, Day: day, Endpoints: map[string]*clientEndpointState{}}
	}
	change(&g.state)
	return true
}

// Post reserves daily events and serializes agent notifications before sending.
func (g *ClientGate) Post(ctx context.Context, client *http.Client, scope, url, event string, props map[string]any, daily bool) (posthog.Status, error) {
	if !daily && event == EventAgentActive {
		return g.postAgentCalls(ctx, client, scope, url, event, props)
	}
	surface, _ := props[PropertySurface].(string)
	screen, _ := props[PropertyScreen].(string)
	key := event + ":" + surface + ":" + screen
	claimed := false
	var sendingDay string
	g.update(ctx, func(state *clientState) {
		endpoint := state.endpoint(scope)
		attempt := endpoint.Events[key]
		now := g.clock()
		if endpoint.Disabled || attempt.Sent || attempt.Rejected >= 6 || now.Before(attempt.Next) {
			return
		}
		sendingDay = state.Day
		claimed = true
		if !daily {
			return
		}
		attempt.Attempts++
		attempt.Next = now.Add(backoffDelay(attempt.Attempts))
		attempt.InFlight = true
		attempt.LeaseUntil = now.Add(NotificationTimeout)
		endpoint.Events[key] = attempt
	})
	if !claimed {
		return posthog.StatusSkipped, nil
	}
	status, err := posthog.PostEvent(ctx, client, url, event, props)
	updateCtx := ctx
	if err != nil {
		var cancel context.CancelFunc
		updateCtx, cancel = context.WithTimeout(context.Background(), NotificationTimeout)
		defer cancel()
	}
	change := func(state *clientState) {
		if state.Day != sendingDay {
			return
		}
		endpoint := state.endpoint(scope)
		if status == posthog.StatusDisabled {
			endpoint.Disabled = true
		}
		if err != nil {
			a := endpoint.Events[key]
			if a.Sent {
				return
			}
			if !daily {
				a.Attempts++
			}
			if rejected(err) {
				a.Rejected++
			}
			a.Next = g.clock().Add(backoffDelay(a.Attempts))
			a.InFlight = false
			a.LeaseUntil = time.Time{}
			endpoint.Events[key] = a
		}
		if err == nil {
			if daily {
				endpoint.Events[key] = clientAttempt{Sent: true}
			} else {
				delete(endpoint.Events, key)
			}
		}
	}
	updated := g.update(updateCtx, change)
	if !updated && (daily || err != nil || status == posthog.StatusDisabled) {
		fallbackCtx, cancel := context.WithTimeout(context.Background(), NotificationTimeout)
		defer cancel()
		g.updateMemory(fallbackCtx, change)
	}
	return status, err
}

// postAgentCalls queues each call before network work so process exit leaves it retryable.
func (g *ClientGate) postAgentCalls(ctx context.Context, client *http.Client, scope, url, event string, props map[string]any) (posthog.Status, error) {
	surface, _ := props[PropertySurface].(string)
	screen, _ := props[PropertyScreen].(string)
	key := event + ":" + surface + ":" + screen
	claimed := false
	var sendingDay, callID string
	updated := g.update(ctx, func(state *clientState) {
		endpoint := state.endpoint(scope)
		attempt := endpoint.Events[key]
		now := g.clock()
		if endpoint.Disabled {
			return
		}
		// An expired lease means its sender stopped waiting; the call keeps its ID, so a replay counts once.
		if attempt.InFlight && !now.Before(attempt.LeaseUntil) {
			leaseUntil := attempt.LeaseUntil
			if leaseUntil.IsZero() {
				leaseUntil = now
			}
			attempt.InFlight = false
			attempt.LeaseUntil = time.Time{}
			attempt.Attempts++
			attempt.Next = leaseUntil.Add(backoffDelay(attempt.Attempts))
		}
		if attempt.Rejected >= 6 {
			endpoint.Events[key] = attempt
			return
		}
		attempt.Pending++
		if attempt.InFlight || now.Before(attempt.Next) {
			endpoint.Events[key] = attempt
			return
		}
		attempt.InFlight = true
		attempt.LeaseUntil = now.Add(NotificationTimeout)
		if attempt.CallID == "" {
			attempt.CallID = rand.Text()
		}
		endpoint.Events[key] = attempt
		sendingDay, callID = state.Day, attempt.CallID
		claimed = true
	})
	if !updated || !claimed {
		return posthog.StatusSkipped, nil
	}
	return g.sendPendingAgentCalls(ctx, client, scope, url, event, props, key, sendingDay, callID)
}

func (g *ClientGate) sendPendingAgentCalls(ctx context.Context, client *http.Client, scope, url, event string, props map[string]any, key, sendingDay, callID string) (posthog.Status, error) {
	for {
		status, err := posthog.PostEvent(ctx, client, withCallID(url, callID), event, props)
		updateCtx := ctx
		if err != nil {
			var cancel context.CancelFunc
			updateCtx, cancel = context.WithTimeout(context.Background(), NotificationTimeout)
			defer cancel()
		}
		sendNext := false
		finish := func(state *clientState) {
			if state.Day != sendingDay {
				return
			}
			endpoint := state.Endpoints[scope]
			if endpoint == nil {
				return
			}
			if status == posthog.StatusDisabled {
				endpoint.Disabled = true
				delete(endpoint.Events, key)
				return
			}
			attempt, ok := endpoint.Events[key]
			// Another sender already settled this call after its lease expired.
			if !ok || attempt.CallID != callID {
				return
			}
			if err != nil {
				attempt.Attempts++
				if rejected(err) {
					attempt.Rejected++
				}
				attempt.Next = g.clock().Add(backoffDelay(attempt.Attempts))
				attempt.InFlight = false
				attempt.LeaseUntil = time.Time{}
				endpoint.Events[key] = attempt
				return
			}
			if attempt.Pending > 0 {
				attempt.Pending--
			}
			attempt.Attempts, attempt.Rejected = 0, 0
			attempt.Next = time.Time{}
			attempt.InFlight = false
			attempt.LeaseUntil = time.Time{}
			attempt.CallID = ""
			if attempt.Pending > 0 {
				attempt.InFlight = true
				attempt.LeaseUntil = g.clock().Add(NotificationTimeout)
				attempt.CallID = rand.Text()
				callID = attempt.CallID
				endpoint.Events[key] = attempt
				sendNext = true
			} else {
				delete(endpoint.Events, key)
			}
		}
		applied, unsaved := g.updateSaved(updateCtx, finish)
		if !applied {
			sendNext = false
			fallbackCtx, cancel := context.WithTimeout(context.Background(), NotificationTimeout)
			g.updateMemory(fallbackCtx, finish)
			cancel()
		}
		// The file still names the settled call, so a later replay would assign the next call a
		// second ID. Leave it queued until its own ID is saved.
		if unsaved {
			sendNext = false
		}
		if err != nil || !sendNext {
			return status, err
		}
	}
}

// withCallID adds the agent call's ID to the daemon route.
func withCallID(raw, callID string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	q := u.Query()
	q.Set("call", callID)
	u.RawQuery = q.Encode()
	return u.String()
}

func (s *clientState) endpoint(scope string) *clientEndpointState {
	if s.Endpoints[scope] == nil {
		s.Endpoints[scope] = &clientEndpointState{Events: map[string]clientAttempt{}}
	} else if s.Endpoints[scope].Events == nil {
		s.Endpoints[scope].Events = map[string]clientAttempt{}
	}
	return s.Endpoints[scope]
}

func (s *clientState) normalize() {
	if s.Endpoints == nil {
		s.Endpoints = map[string]*clientEndpointState{}
		return
	}
	for _, endpoint := range s.Endpoints {
		if endpoint != nil && endpoint.Events == nil {
			endpoint.Events = map[string]clientAttempt{}
		}
	}
}

// rejected reports a failure the daemon answered, such as a reply without a receipt. A request
// that got no answer, such as a refused connection or a timeout, fails with a *url.Error.
func rejected(err error) bool {
	_, unanswered := errors.AsType[*url.Error](err)
	return err != nil && !unanswered
}

func backoffDelay(attempts int) time.Duration {
	return time.Second * time.Duration(1<<max(0, min(attempts-1, 4)))
}
