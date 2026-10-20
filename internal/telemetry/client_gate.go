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
	// clientStateVersion is the file format this build reads and writes; a newer file is left alone.
	clientStateVersion = 1
	// maxProbeRejections bounds the answers without a receipt that the app_opened check accepts per daemon and surface each UTC day.
	maxProbeRejections = 6
	// receiptPollInterval paces waits for the receipt lock.
	receiptPollInterval = 10 * time.Millisecond
	// surfaceAgent keys the agent call's daily check, which shares the app_opened check's receipts and backoff.
	surfaceAgent = "agent"
)

// ClientGate keeps each daemon's opt-out and the day's app_opened check in one file shared by CLI, TUI, and MCP
// processes, so a day sends one check per daemon and a disabled daemon hears nothing more.
// It keeps no counts for other reports; a restart may send one twice and the daemon dedupes.
// File and network work belongs in the caller's send goroutine; Ready reads memory only.
type ClientGate struct {
	probeMu sync.Mutex
	mu      sync.Mutex
	state   clientState
	path    string
	loaded  bool
	// frozen means the file has a newer format: no reports go out and the file stays as found.
	frozen bool
	// down holds the backoff after reports that got no answer or a 5xx, per daemon and kept in memory only.
	down map[string]reportBackoff
	now  func() time.Time
}

// DefaultClientGate is the process-wide gate.
var DefaultClientGate = &ClientGate{}

type reportBackoff struct {
	failures int
	until    time.Time
}

type clientState struct {
	Version   int                        `json:"version"`
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
	// Rejected counts answers that are neither a receipt nor a 5xx, such as an empty 2xx from a daemon too old
	// to send receipts, or a 4xx. Six stop the check for the day; 5xx answers and requests that got no answer
	// only extend the backoff.
	Rejected int       `json:"rejected,omitempty"`
	Next     time.Time `json:"next"`
}

func (g *ClientGate) clock() time.Time {
	if g.now != nil {
		return g.now()
	}
	return time.Now()
}

func (g *ClientGate) today() string { return g.clock().UTC().Format(time.DateOnly) }

// current returns today's receipts for the current data directory, dropping memory from another file or day.
// Callers hold g.mu.
func (g *ClientGate) current() *clientState {
	path := filepath.Join(config.DataDir(), clientStateFile)
	day := g.today()
	if g.path != path || g.state.Day != day {
		g.path, g.loaded, g.frozen, g.down = path, false, false, map[string]reportBackoff{}
		g.state = clientState{Version: clientStateVersion, Day: day, Endpoints: map[string]*clientEndpoint{}}
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
	return !g.frozen && (e == nil || !e.Disabled)
}

// Load reads the shared receipts into memory once per day; Probe and Post call it themselves.
func (g *ClientGate) Load(ctx context.Context) {
	g.mu.Lock()
	g.current()
	loaded := g.loaded
	g.mu.Unlock()
	if loaded {
		return
	}
	if unlock, ok := g.lock(ctx); ok {
		defer unlock()
		g.merge()
	}
}

// Probe sends the day's app_opened check for surface unless a receipt, an opt-out, or backoff covers it.
func (g *ClientGate) Probe(ctx context.Context, client *http.Client, scope, url, surface string) {
	g.probe(ctx, client, scope, url, surface, EventAppOpened, map[string]any{PropertySurface: surface})
}

// probe sends event as surface's check for the day unless a receipt, an opt-out, or backoff covers it, and
// returns whether it sent. It holds the receipt lock through the request, so processes started together send
// one check and the rest read its result. If ctx ends first the report is dropped.
func (g *ClientGate) probe(ctx context.Context, client *http.Client, scope, url, surface, event string, props map[string]any) (send bool) {
	for !g.probeMu.TryLock() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(receiptPollInterval):
		}
	}
	defer g.probeMu.Unlock()
	g.mu.Lock()
	e := g.current().Endpoints[scope]
	open := !g.frozen && e.probeDue(surface, g.clock())
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
	day := g.today()
	g.commit("", scope, func(e *clientEndpoint) bool {
		now := g.clock()
		if g.frozen || !e.probeDue(surface, now) {
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
	answering, answers := trackAnswer(client)
	status, err := posthog.PostEvent(ctx, answering, url, event, props)
	if err == nil {
		g.commit(day, scope, func(e *clientEndpoint) bool {
			if status == posthog.StatusDisabled {
				e.Disabled = true
			} else {
				p := e.Probes[surface]
				p.Sent = true
				e.Probes[surface] = p
			}
			return true
		})
	} else if answers.status > 0 && answers.status < 500 {
		g.commit(day, scope, func(e *clientEndpoint) bool {
			p := e.Probes[surface]
			p.Rejected++
			e.Probes[surface] = p
			return true
		})
	}
	return
}

// Post sends one report unless the daemon is disabled or backing off; callers run Probe first. A 5xx or no
// answer starts a memory-only 1 to 16 second backoff that skips reports meanwhile, and the surface's saved
// app_opened backoff covers them too. A disabled reply from any route is saved. It returns whether the report
// settled: the daemon answered below 500, it wants no reports, or the report went out as the day's agent check.
// A caller may resend an unsettled report later.
func (g *ClientGate) Post(ctx context.Context, client *http.Client, scope, url, event string, props map[string]any) bool {
	surface, sent := g.prepare(ctx, client, scope, url, event, props)
	ready := g.Ready(scope)
	if sent || !ready || g.backingOff(scope, surface) {
		return sent || !ready
	}
	answering, answers := trackAnswer(client)
	day := g.today()
	status, err := posthog.PostEvent(ctx, answering, url, event, props)
	if status == posthog.StatusDisabled {
		g.disable(ctx, day, scope)
	}
	return g.noteReport(scope, err == nil || (answers.status > 0 && answers.status < 500))
}

// prepare loads the receipts and returns the surface whose saved backoff covers the report. Until the day's
// receipt is known, an agent call goes out as the agent surface's check, so concurrent agent processes send
// one call and the rest read its result; sent reports whether this call went out that way.
func (g *ClientGate) prepare(ctx context.Context, client *http.Client, scope, url, event string, props map[string]any) (surface string, sent bool) {
	if event != EventAgentActive {
		g.Load(ctx)
		surface, _ = props[PropertySurface].(string)
		return surface, false
	}
	return surfaceAgent, g.probe(ctx, client, scope, url, surfaceAgent, event, props)
}

// backingOff covers a daemon whose reports just failed and the surface's saved app_opened backoff.
func (g *ClientGate) backingOff(scope, surface string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	e := g.current().Endpoints[scope]
	return g.clock().Before(g.down[scope].until) || e.probeOpen(surface) && !e.probeDue(surface, g.clock())
}

// noteReport extends the scope's backoff after a failed report, clears it after an answered one, and returns answered.
func (g *ClientGate) noteReport(scope string, answered bool) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.current()
	if answered {
		delete(g.down, scope)
		return true
	}
	b := g.down[scope]
	b.failures++
	b.until = g.clock().Add(backoffDelay(b.failures))
	g.down[scope] = b
	return false
}

// disable keeps the opt-out in memory first, so an unwritable file loses nothing.
func (g *ClientGate) disable(ctx context.Context, day, scope string) {
	g.mu.Lock()
	if state := g.current(); state.Day == day {
		state.endpoint(scope).Disabled = true
	}
	g.mu.Unlock()
	// The request may have used up ctx; saving gets its own bound.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), NotificationTimeout)
	defer cancel()
	if unlock, ok := g.lock(ctx); ok {
		defer unlock()
		g.merge()
		g.commit(day, scope, func(e *clientEndpoint) bool {
			e.Disabled = true
			return true
		})
	}
}

// lock takes the cross-process receipt lock; false means ctx ended first. With unusable storage,
// probeMu still serializes probes and memory alone carries the receipts.
func (g *ClientGate) lock(ctx context.Context) (func(), bool) {
	g.mu.Lock()
	g.current()
	path := g.path
	g.mu.Unlock()
	_ = os.MkdirAll(filepath.Dir(path), 0o700)
	l := flock.New(path + ".lock")
	if locked, _ := l.TryLockContext(ctx, receiptPollInterval); !locked && ctx.Err() != nil {
		return nil, false
	}
	return func() { _ = l.Unlock() }, true
}

// merge folds the saved receipts into memory, keeping whichever side has more: opt-outs and sent checks
// stick, attempts only grow, and backoff only lengthens. Unreadable or other-day content reads as empty.
// Callers hold the receipt lock.
func (g *ClientGate) merge() {
	path := filepath.Join(config.DataDir(), clientStateFile)
	data, err := os.ReadFile(path)
	var header struct {
		Version int `json:"version"`
	}
	newer := json.Unmarshal(data, &header) == nil && header.Version > clientStateVersion
	var saved clientState
	decodeErr := json.Unmarshal(data, &saved)
	g.mu.Lock()
	defer g.mu.Unlock()
	state := g.current()
	g.loaded = true
	if err != nil || g.path != path {
		return
	}
	if newer {
		g.frozen = true
		return
	}
	if decodeErr != nil || saved.Day != state.Day {
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

// commit applies change to memory and, when it reports a change, saves the receipts, unless the file is
// frozen. A day other than "" limits the change to that UTC day. Callers hold the receipt lock.
func (g *ClientGate) commit(day, scope string, change func(*clientEndpoint) bool) {
	g.mu.Lock()
	state := g.current()
	if day != "" && state.Day != day || !change(state.endpoint(scope)) {
		g.mu.Unlock()
		return
	}
	path, frozen := g.path, g.frozen
	data, err := json.Marshal(state)
	g.mu.Unlock()
	if err == nil && !frozen {
		_ = atomicfile.WriteFile(path, data, atomicfile.WithPrivate(), atomicfile.WithoutSync())
	}
}

// lastAnswer records the status of the last response, or zero when none arrived.
type lastAnswer struct {
	base   http.RoundTripper
	status int
}

// trackAnswer copies client so its transport records the status of the last response.
func trackAnswer(client *http.Client) (*http.Client, *lastAnswer) {
	answers := &lastAnswer{base: client.Transport}
	tracked := *client
	tracked.Transport = answers
	return &tracked, answers
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
