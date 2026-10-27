package daemon

import (
	"context"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
)

// sleepingClock models a machine that can sleep: synctest's fake clock stands
// in for the monotonic clock, which pauses during sleep, and sleep adds the
// slept time to the wall clock only.
type sleepingClock struct {
	slept atomic.Int64
}

func (c *sleepingClock) now() time.Time {
	return time.Now().Add(time.Duration(c.slept.Load()))
}

func (c *sleepingClock) sleep(d time.Duration) {
	c.slept.Add(int64(d))
}

func TestWallClockDeadlineCountsMachineSleep(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		assert := assert.New(t)
		clock := &sleepingClock{}
		start := time.Now()
		ctx, cancel := newWallClockDeadline(context.Background(), clock.now().Add(30*time.Minute), clock.now)
		defer cancel()
		child, cancelChild := context.WithCancel(ctx)
		defer cancelChild()

		time.Sleep(25 * time.Minute)
		clock.sleep(30 * time.Minute)
		time.Sleep(wallClockCheckInterval)
		synctest.Wait()

		assert.ErrorIs(ctx.Err(), context.DeadlineExceeded)
		assert.ErrorIs(context.Cause(ctx), context.DeadlineExceeded)
		assert.ErrorIs(child.Err(), context.DeadlineExceeded)
		assert.Less(time.Since(start), 30*time.Minute)
	})
}

func TestWallClockDeadlineWithoutSleep(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		ctx, cancel := withWallClockTimeout(context.Background(), 30*time.Minute)
		defer cancel()

		<-ctx.Done()

		assert.ErrorIs(t, ctx.Err(), context.DeadlineExceeded)
		assert.Equal(t, 30*time.Minute, time.Since(start))
	})
}

func TestWallClockDeadlineCancel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		parent, cancelParent := context.WithCancel(context.Background())
		fromParent, cancelFromParent := withWallClockTimeout(parent, 30*time.Minute)
		defer cancelFromParent()
		direct, cancelDirect := withWallClockTimeout(context.Background(), 30*time.Minute)

		cancelParent()
		cancelDirect()
		synctest.Wait()

		assert.ErrorIs(t, fromParent.Err(), context.Canceled)
		assert.ErrorIs(t, direct.Err(), context.Canceled)
	})
}
