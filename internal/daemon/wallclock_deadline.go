package daemon

import (
	"context"
	"sync"
	"time"
)

// wallClockCheckInterval bounds how long a job may outlive its wall-clock
// deadline after the machine wakes from sleep. Go timers run on the monotonic
// clock, which stops while the machine sleeps, so the deadline is rechecked
// against the wall clock at this interval rather than trusted to one timer.
// Job timeouts are configured in whole minutes, so ten seconds is small
// against the smallest configurable value.
const wallClockCheckInterval = 10 * time.Second

// withWallClockTimeout returns a context that ends with
// context.DeadlineExceeded once the wall clock passes now+timeout, including
// any time the machine spent asleep. context.WithTimeout would not count that
// time: a review that spanned a 30-minute sleep finished long after its
// job_timeout_minutes limit had passed in real time.
func withWallClockTimeout(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	return newWallClockDeadline(parent, time.Now().Add(timeout), time.Now)
}

// wallClockDeadlineCtx is a context whose deadline is measured on the wall
// clock. It wraps a cancel-cause context so Value and context.Cause resolve
// through it, but exposes its own Done channel: the context package then
// propagates this context's Err, not the wrapped context's Canceled, to
// derived contexts.
type wallClockDeadlineCtx struct {
	context.Context
	cancelInner context.CancelCauseFunc
	deadline    time.Time
	now         func() time.Time
	done        chan struct{}
	once        sync.Once
	mu          sync.Mutex
	err         error
}

func newWallClockDeadline(parent context.Context, deadline time.Time, now func() time.Time) (context.Context, context.CancelFunc) {
	// Round(0) drops the monotonic reading so comparisons, including the
	// stalled-job health check that reads Deadline, use the wall clock.
	deadline = deadline.Round(0)
	if parentDeadline, ok := parent.Deadline(); ok && parentDeadline.Before(deadline) {
		deadline = parentDeadline.Round(0)
	}
	inner, cancelInner := context.WithCancelCause(parent)
	c := &wallClockDeadlineCtx{
		Context:     inner,
		cancelInner: cancelInner,
		deadline:    deadline,
		now:         now,
		done:        make(chan struct{}),
	}
	go c.watch(inner)
	return c, func() { c.finish(context.Canceled) }
}

func (c *wallClockDeadlineCtx) watch(inner context.Context) {
	for {
		remaining := c.deadline.Sub(c.now())
		if remaining <= 0 {
			c.finish(context.DeadlineExceeded)
			return
		}
		timer := time.NewTimer(min(remaining, wallClockCheckInterval))
		select {
		case <-inner.Done():
			timer.Stop()
			c.finish(inner.Err())
			return
		case <-timer.C:
		}
	}
}

func (c *wallClockDeadlineCtx) finish(err error) {
	c.once.Do(func() {
		c.mu.Lock()
		c.err = err
		c.mu.Unlock()
		c.cancelInner(err)
		close(c.done)
	})
}

func (c *wallClockDeadlineCtx) Deadline() (time.Time, bool) {
	return c.deadline, true
}

func (c *wallClockDeadlineCtx) Done() <-chan struct{} {
	return c.done
}

func (c *wallClockDeadlineCtx) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}
