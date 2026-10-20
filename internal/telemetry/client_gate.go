package telemetry

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/gofrs/flock"
	"go.kenn.io/kit/atomicfile"
	"go.kenn.io/kit/telemetry/posthog"

	"go.kenn.io/roborev/internal/config"
)

const (
	clientStateFile = "telemetry-client.json"
	// maxProbeRejections bounds the answers without a receipt that the app_opened check accepts per daemon and surface each UTC day.
	maxProbeRejections = 6
	// receiptPollInterval paces waits for the receipt lock.
	receiptPollInterval = 10 * time.Millisecond
)

// ClientGate keeps each daemon's opt-out and the day's app_opened check in one file shared by CLI, TUI, and MCP
// processes, so a day sends one check per daemon and a disabled daemon hears nothing more.
// It keeps no counts for other reports; a restart may send one twice and the daemon dedupes.
// File and network work belongs in the caller's send goroutine; Ready reads memory only.
type ClientGate struct {
	mu     sync.Mutex
	state  clientState
	path   string
	loaded bool
	now    func() time.Time
}

// DefaultClientGate is the process-wide gate.
var DefaultClientGate = &ClientGate{}

type clientState struct {
	Day       string                     `json:"day"`
	Endpoints map[string]*clientEndpoint `json:"endpoints"`
}

type clientEndpoint struct {
	Disabled bool                   `json:"disabled,omitempty"`
	Probes   map[string]clientProbe `json:"probes,omitempty"`
}

type clientProbe struct {
	Sent     bool `json:"sent,omitempty"`
	Attempts int  `json:"attempts,omitempty"`
	// Rejected counts 2xx answers without a receipt, such as from a daemon too old to send them. Six stop the
	// check for the day; failures and requests that got no answer only extend the backoff.
	Rejected int       `json:"rejected,omitempty"`
	Next     time.Time `json:"next"`
}

func (g *ClientGate) clock() time.Time {
	if g.now != nil {
		return g.now()
	}
	return time.Now()
}

// current returns today's receipts for the current data directory, dropping memory from another file or day.
// Callers hold g.mu.
func (g *ClientGate) current() *clientState {
	path := filepath.Join(config.DataDir(), clientStateFile)
	day := g.clock().UTC().Format(time.DateOnly)
	if g.path != path || g.state.Day != day {
		g.path, g.loaded = path, false
		g.state = clientState{Day: day, Endpoints: map[string]*clientEndpoint{}}
	}
	return &g.state
}

func (s *clientState) endpoint(scope string) *clientEndpoint {
	e := s.Endpoints[scope]
	if e == nil {
		e = &clientEndpoint{}
		s.Endpoints[scope] = e
	}
	if e.Probes == nil {
		e.Probes = map[string]clientProbe{}
	}
	return e
}

// probeOpen reports whether the surface's app_opened check could still send today.
func (e *clientEndpoint) probeOpen(surface string) bool {
	if e == nil {
		return true
	}
	p := e.Probes[surface]
	return !e.Disabled && !p.Sent && p.Rejected < maxProbeRejections
}

// probeDue also waits out backoff, which an attempt in flight counts as, so only the lock holder sends.
func (e *clientEndpoint) probeDue(surface string, now time.Time) bool {
	return e.probeOpen(surface) && (e == nil || !now.Before(e.Probes[surface].Next))
}

// Ready reports whether the daemon may still want reports today. It reads memory only, so input paths can call it.
func (g *ClientGate) Ready(scope string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	e := g.current().Endpoints[scope]
	return e == nil || !e.Disabled
}

// Load reads the shared receipts into memory.
func (g *ClientGate) Load(ctx context.Context) {
	if unlock, ok := g.lock(ctx); ok {
		defer unlock()
		g.merge()
	}
}

// Probe sends the day's app_opened check for surface unless a receipt, an opt-out, or backoff covers it.
// It holds the receipt lock through the request, so processes started together send one check and
// the rest read its result. If ctx ends first the report is dropped.
func (g *ClientGate) Probe(ctx context.Context, client *http.Client, scope, url, surface string) {
	g.mu.Lock()
	open := g.current().Endpoints[scope].probeOpen(surface)
	g.mu.Unlock()
	if !open {
		return
	}
	unlock, ok := g.lock(ctx)
	if !ok {
		return
	}
	defer unlock()
	g.merge()
	send := false
	g.commit(scope, func(e *clientEndpoint) bool {
		now := g.clock()
		if !e.probeDue(surface, now) {
			return false
		}
		p := e.Probes[surface]
		p.Attempts++
		p.Next = now.Add(backoffDelay(p.Attempts))
		e.Probes[surface] = p
		send = true
		return true
	})
	if !send {
		return
	}
	answers := &lastAnswer{base: client.Transport}
	answering := *client
	answering.Transport = answers
	status, err := posthog.PostEvent(ctx, &answering, url, EventAppOpened, map[string]any{PropertySurface: surface})
	if err != nil {
		if answers.status >= 200 && answers.status < 300 {
			g.commit(scope, func(e *clientEndpoint) bool {
				p := e.Probes[surface]
				p.Rejected++
				e.Probes[surface] = p
				return true
			})
		}
		return
	}
	g.commit(scope, func(e *clientEndpoint) bool {
		if status == posthog.StatusDisabled {
			e.Disabled = true
		} else {
			p := e.Probes[surface]
			p.Sent = true
			e.Probes[surface] = p
		}
		return true
	})
}

// Post sends one report unless the daemon is disabled; callers run Probe first, so a report waits only
// for an attempt in flight. A disabled reply from any route is saved.
func (g *ClientGate) Post(ctx context.Context, client *http.Client, scope, url, event string, props map[string]any) (posthog.Status, error) {
	if !g.isLoaded() {
		g.Load(ctx)
	}
	if !g.Ready(scope) {
		return posthog.StatusSkipped, nil
	}
	status, err := posthog.PostEvent(ctx, client, url, event, props)
	if status == posthog.StatusDisabled {
		g.disable(ctx, scope)
	}
	return status, err
}

func (g *ClientGate) isLoaded() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.current()
	return g.loaded
}

// disable keeps the opt-out in memory first, so an unwritable file loses nothing.
func (g *ClientGate) disable(ctx context.Context, scope string) {
	g.mu.Lock()
	g.current().endpoint(scope).Disabled = true
	g.mu.Unlock()
	// The request may have used up ctx; saving gets its own bound.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), NotificationTimeout)
	defer cancel()
	if unlock, ok := g.lock(ctx); ok {
		defer unlock()
		g.merge()
		g.commit(scope, func(e *clientEndpoint) bool {
			e.Disabled = true
			return true
		})
	}
}

// lock takes the cross-process receipt lock; false means ctx ended first. When storage is unusable
// there is nothing to wait for, and memory alone carries the receipts.
func (g *ClientGate) lock(ctx context.Context) (func(), bool) {
	g.mu.Lock()
	g.current()
	path := g.path
	g.mu.Unlock()
	_ = os.MkdirAll(filepath.Dir(path), 0o700)
	l := flock.New(path + ".lock")
	if locked, _ := l.TryLockContext(ctx, receiptPollInterval); !locked {
		if ctx.Err() != nil {
			return nil, false
		}
		return func() {}, true
	}
	return func() { _ = l.Unlock() }, true
}

// merge folds the saved receipts into memory, keeping whichever side has more: opt-outs and sent checks
// stick, attempts only grow, and backoff only lengthens. Unreadable or other-day content reads as empty.
// Callers hold the receipt lock.
func (g *ClientGate) merge() {
	g.mu.Lock()
	defer g.mu.Unlock()
	state := g.current()
	g.loaded = true
	data, err := os.ReadFile(g.path)
	if err != nil {
		return
	}
	var saved clientState
	if json.Unmarshal(data, &saved) != nil || saved.Day != state.Day {
		return
	}
	for scope, se := range saved.Endpoints {
		if se == nil {
			continue
		}
		e := state.endpoint(scope)
		e.Disabled = e.Disabled || se.Disabled
		for surface, sp := range se.Probes {
			p := e.Probes[surface]
			p.Sent = p.Sent || sp.Sent
			p.Attempts = max(p.Attempts, sp.Attempts)
			p.Rejected = max(p.Rejected, sp.Rejected)
			if sp.Next.After(p.Next) {
				p.Next = sp.Next
			}
			e.Probes[surface] = p
		}
	}
}

// commit applies change to memory and, when it reports a change, saves the receipts. Callers hold the receipt lock.
func (g *ClientGate) commit(scope string, change func(*clientEndpoint) bool) {
	g.mu.Lock()
	state := g.current()
	if !change(state.endpoint(scope)) {
		g.mu.Unlock()
		return
	}
	path := g.path
	data, err := json.Marshal(state)
	g.mu.Unlock()
	if err == nil {
		_ = atomicfile.WriteFile(path, data, atomicfile.WithPrivate(), atomicfile.WithoutSync())
	}
}

// lastAnswer records the status of the last response. A 2xx answer that still fails is a daemon without
// receipts, such as an older one; 5xx answers and requests that got no answer only back off.
type lastAnswer struct {
	base   http.RoundTripper
	status int
}

func (l *lastAnswer) RoundTrip(req *http.Request) (*http.Response, error) {
	base := l.base
	if base == nil {
		base = http.DefaultTransport
	}
	resp, err := base.RoundTrip(req)
	if resp != nil {
		l.status = resp.StatusCode
	}
	return resp, err
}

func backoffDelay(attempts int) time.Duration {
	return time.Second * time.Duration(1<<max(0, min(attempts-1, 4)))
}
