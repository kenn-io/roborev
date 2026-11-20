// Package proctrack gives a job attempt ownership of the processes it starts.
// Each tracked root runs in its own process group. When a root exits, its
// group is stopped; when the attempt ends, everything it still owns is stopped.
// With a memory limit, a shared sampler sums the attempt's resident memory and
// stops and cancels the attempt when the sum exceeds the limit.
//
// Without a scope in the context, and on platforms other than Linux and
// macOS, Start and Process.Wait behave like exec.Cmd's Start and Wait.
package proctrack

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// ErrClosed is returned by Start after the scope's cleanup has begun. The
// command is not started.
var ErrClosed = errors.New("job processes are being stopped")

var errNoPidfd = errors.New("no pidfd support")

// handoffWait bounds how long cleanup waits for stopped roots to exit.
var handoffWait = 15 * time.Second

// startWatch is a variable so tests can delay the watcher past the root's exit.
var startWatch = func(r *root) { go r.watch() }

type ctxKey struct{}

// tracker is the state one attempt and its nested scopes share.
type tracker struct {
	mu      sync.Mutex
	cond    *sync.Cond // signaled when pending drops
	pending int        // launches admitted but not yet registered
	limit   uint64
	attempt *Scope
	warned  map[int]bool
}

// Scope owns the processes started with its context: a job attempt, or a phase
// of one opened with OpenScope. A nil *Scope is valid and owns nothing.
type Scope struct {
	t        *tracker
	parent   *Scope
	children []*Scope
	cancel   context.CancelCauseFunc
	closed   bool
	roots    map[int]*root // owned process groups, by group ID
	// remembered holds every process seen in this scope on the last sample,
	// by identity, so a process that leaves its group is still found.
	remembered map[ident]Proc
	breach     *Breach
}

type root struct {
	scope      *Scope
	pid        int
	stop       func()
	registered time.Time // when Start registered the root's group
	handedOff  chan struct{}
}

// New starts tracking processes for one job attempt. limitBytes is the summed
// RSS limit; 0 disables sampling but keeps process ownership. Call Close when
// the attempt ends. On unsupported platforms New returns ctx and a nil scope.
func New(ctx context.Context, limitBytes uint64) (context.Context, *Scope) {
	if !supported {
		return ctx, nil
	}
	t := &tracker{limit: limitBytes, warned: map[int]bool{}}
	t.cond = sync.NewCond(&t.mu)
	ctx, s := newScope(ctx, t, nil)
	t.attempt = s
	if limitBytes > 0 {
		startSampling(t)
	}
	return ctx, s
}

// OpenScope opens a phase nested in the context's scope, with a child context
// that a breach in this phase cancels. It returns ctx and a nil scope when ctx
// has none. Call Close when the phase ends.
func OpenScope(ctx context.Context) (context.Context, *Scope) {
	parent := FromContext(ctx)
	if parent == nil {
		return ctx, nil
	}
	ctx, s := newScope(ctx, parent.t, parent)
	parent.t.mu.Lock()
	parent.children = append(parent.children, s)
	parent.t.mu.Unlock()
	return ctx, s
}

func newScope(ctx context.Context, t *tracker, parent *Scope) (context.Context, *Scope) {
	ctx, cancel := context.WithCancelCause(ctx)
	s := &Scope{t: t, parent: parent, cancel: cancel, roots: map[int]*root{}, remembered: map[ident]Proc{}}
	return context.WithValue(ctx, ctxKey{}, s), s
}

// ContextWith returns ctx carrying s, for a launch whose context does not
// derive from the scope's own, such as one that must outlive a request.
func ContextWith(ctx context.Context, s *Scope) context.Context {
	if s == nil {
		return ctx
	}
	return context.WithValue(ctx, ctxKey{}, s)
}

// FromContext returns the innermost scope in ctx, or nil.
func FromContext(ctx context.Context) *Scope {
	s, _ := ctx.Value(ctxKey{}).(*Scope)
	return s
}

// Breach returns the memory breach recorded for this scope, or nil.
func (s *Scope) Breach() *Breach {
	if s == nil {
		return nil
	}
	s.t.mu.Lock()
	defer s.t.mu.Unlock()
	return s.breach
}

// Close stops every process the scope and its open phases own, then releases
// the scope's context. The error names processes that could not be stopped.
func (s *Scope) Close() error {
	if s == nil {
		return nil
	}
	err := s.cleanup()
	s.t.mu.Lock()
	if p := s.parent; p != nil {
		for i, c := range p.children {
			if c == s {
				p.children = append(p.children[:i], p.children[i+1:]...)
				break
			}
		}
	}
	s.t.mu.Unlock()
	if s.parent == nil {
		stopSampling(s.t)
	}
	s.cancel(nil)
	return err
}

func (s *Scope) closedLocked() bool {
	for c := s; c != nil; c = c.parent {
		if c.closed {
			return true
		}
	}
	return false
}

// treeLocked returns s and its open phases, outermost first.
func (s *Scope) treeLocked() []*Scope {
	out := []*Scope{s}
	for _, c := range s.children {
		out = append(out, c.treeLocked()...)
	}
	return out
}

// cleanup closes admission and stops everything the scope can prove it owns.
func (s *Scope) cleanup() error {
	t := s.t
	t.mu.Lock()
	scopes := s.treeLocked()
	for _, sc := range scopes {
		sc.closed = true
	}
	for t.pending > 0 {
		t.cond.Wait()
	}
	var errs []error
	var stops []func()
	var waits []*root
	owned := map[int]struct{}{}
	for _, sc := range scopes {
		for pgid, r := range sc.roots {
			owned[pgid] = struct{}{}
			waits = append(waits, r)
			if r.stop != nil {
				stops = append(stops, r.stop)
			} else if err := killGroup(pgid); err != nil {
				errs = append(errs, fmt.Errorf("process group %d: %w", pgid, err))
			}
		}
	}
	var remembered []Proc
	for _, sc := range scopes {
		for _, p := range sc.remembered {
			remembered = append(remembered, p)
		}
	}
	t.mu.Unlock()

	// A stop operation targets a sandbox scope, not a process number, so it
	// may run outside the lock and after its root's group is handed off.
	for _, stop := range stops {
		stop()
	}
	var left []string
	for _, p := range remembered {
		// Decide from the process as it is now, not as the last sample saw it:
		// it may have exited, or left its group after that sample.
		cur, err := readProc(p.PID)
		if err != nil || cur.Start != p.Start {
			continue
		}
		if _, ok := owned[cur.PGID]; ok {
			continue // the group SIGKILL above reached it
		}
		switch err := killIdentity(ident{p.PID, p.Start}); {
		case errors.Is(err, errNoPidfd):
			left = append(left, describe(p))
		case err != nil:
			errs = append(errs, fmt.Errorf("%s: %w", describe(p), err))
		}
	}
	deadline := time.NewTimer(handoffWait)
	defer deadline.Stop()
	for _, r := range waits {
		select {
		case <-r.handedOff:
		case <-deadline.C:
			left = append(left, fmt.Sprintf("pid %d", r.pid))
			deadline.Reset(0)
		}
	}
	if len(left) > 0 {
		errs = append(errs, fmt.Errorf("left running: %s", strings.Join(left, ", ")))
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("stop job processes: %w", err)
	}
	return nil
}

func describe(p Proc) string {
	return fmt.Sprintf("%s (pid %d)", p.Name, p.PID)
}

// StartOption configures a tracked launch.
type StartOption func(*root)

// WithStop makes stop, instead of a process-group SIGKILL, the way to stop the
// root's processes. Sandboxed roots use the sandbox's own cleanup.
func WithStop(stop func()) StartOption {
	return func(r *root) { r.stop = stop }
}

// Process is a started command. Its Wait must be used instead of cmd.Wait.
type Process struct {
	cmd *exec.Cmd
	r   *root
}

// Start starts cmd as a root of the context's scope, in its own process group.
// Without a scope it is cmd.Start.
func Start(ctx context.Context, cmd *exec.Cmd, opts ...StartOption) (*Process, error) {
	s := FromContext(ctx)
	if s == nil {
		if err := cmd.Start(); err != nil {
			return nil, err
		}
		return &Process{cmd: cmd}, nil
	}
	r := &root{scope: s, handedOff: make(chan struct{})}
	for _, opt := range opts {
		opt(r)
	}
	setProcessGroup(cmd)
	t := s.t
	t.mu.Lock()
	if s.closedLocked() {
		t.mu.Unlock()
		return nil, ErrClosed
	}
	t.pending++
	t.mu.Unlock()

	err := cmd.Start()

	t.mu.Lock()
	t.pending--
	if err == nil {
		r.pid = cmd.Process.Pid
		r.registered = time.Now()
		s.roots[r.pid] = r
	}
	t.cond.Broadcast()
	t.mu.Unlock()
	if err != nil {
		return nil, err
	}
	startWatch(r)
	return &Process{cmd: cmd, r: r}, nil
}

// Run starts cmd and waits for it.
func Run(ctx context.Context, cmd *exec.Cmd, opts ...StartOption) error {
	p, err := Start(ctx, cmd, opts...)
	if err != nil {
		return err
	}
	return p.Wait()
}

// Output runs cmd and returns its standard output, like cmd.Output.
func Output(ctx context.Context, cmd *exec.Cmd, opts ...StartOption) ([]byte, error) {
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	err := Run(ctx, cmd, opts...)
	return stdout.Bytes(), err
}

// CombinedOutput runs cmd and returns its standard output and standard error
// together, like cmd.CombinedOutput.
func CombinedOutput(ctx context.Context, cmd *exec.Cmd, opts ...StartOption) ([]byte, error) {
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := Run(ctx, cmd, opts...)
	return out.Bytes(), err
}

// Wait waits until the root's group has been stopped, then reaps the root with
// cmd.Wait and returns its exit status.
func (p *Process) Wait() error {
	if p.r != nil {
		<-p.r.handedOff
	}
	return p.cmd.Wait()
}

// watch observes the root's exit without reaping it, stops its group while the
// zombie root still reserves the group ID, then hands the root to Wait.
func (r *root) watch() {
	waitErr := waitExit(r.pid)
	if waitErr != nil {
		// Without proof that the root exited, its group is not signaled.
		log.Printf("agent process %d: %v", r.pid, waitErr)
	}
	t := r.scope.t
	if waitErr == nil && r.stop != nil {
		r.stop()
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if waitErr == nil && r.stop == nil {
		if err := killGroup(r.pid); err != nil {
			log.Printf("stop process group %d: %v", r.pid, err)
		}
	}
	delete(r.scope.roots, r.pid)
	close(r.handedOff)
}
