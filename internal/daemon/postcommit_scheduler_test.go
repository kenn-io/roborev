package daemon

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPostCommitSchedulerDebouncesPerBranch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := make(chan string, 10)
		scheduler := newPostCommitScheduler(func(_ context.Context, root, branch, head string) error {
			calls <- root + ":" + branch + ":" + head
			return nil
		})
		defer scheduler.stop()
		require.NoError(t, scheduler.schedule("repo", "checkout", "feature", "first", time.Minute))
		synctest.Wait()
		time.Sleep(40 * time.Second)
		require.NoError(t, scheduler.schedule("repo", "other-checkout", "feature", "second", time.Minute))
		require.NoError(t, scheduler.schedule("repo", "checkout", "other", "third", 20*time.Second))
		synctest.Wait()
		time.Sleep(20 * time.Second)
		synctest.Wait()
		require.Len(t, calls, 1)
		assert.Equal(t, "checkout:other:third", <-calls)
		time.Sleep(39 * time.Second)
		synctest.Wait()
		assert.Empty(t, calls)
		time.Sleep(time.Second)
		synctest.Wait()
		require.Len(t, calls, 1)
		assert.Equal(t, "other-checkout:feature:second", <-calls)
	})
}

func TestPostCommitSchedulerStop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := make(chan string, 1)
		scheduler := newPostCommitScheduler(func(ctx context.Context, _, _, head string) error {
			calls <- head
			<-ctx.Done()
			return ctx.Err()
		})
		require.NoError(t, scheduler.schedule("repo", "checkout", "running", "first", time.Second))
		require.NoError(t, scheduler.schedule("repo", "checkout", "waiting", "second", time.Minute))
		synctest.Wait()
		time.Sleep(time.Second)
		synctest.Wait()
		require.Len(t, calls, 1)
		assert.Equal(t, "first", <-calls)
		scheduler.stop()
		time.Sleep(time.Minute)
		synctest.Wait()
		assert.Empty(t, calls)
		require.Error(t, scheduler.schedule("repo", "checkout", "waiting", "third", time.Minute))
	})
}

func TestPostCommitSchedulerRetriesFailedFlush(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := make(chan time.Duration, 10)
		start := time.Now()
		var attempts int
		scheduler := newPostCommitScheduler(func(context.Context, string, string, string) error {
			calls <- time.Since(start)
			attempts++
			if attempts < 3 {
				return errors.New("daemon busy")
			}
			return nil
		})
		defer scheduler.stop()
		require.NoError(t, scheduler.schedule("repo", "checkout", "feature", "head", time.Minute))
		time.Sleep(time.Hour)
		synctest.Wait()

		require.Len(t, calls, 3, "the flush stops once it queues the range")
		assert.Equal(t, time.Minute, <-calls)
		assert.Equal(t, 3*time.Minute, <-calls, "the first retry waits twice the quiet period")
		assert.Equal(t, 7*time.Minute, <-calls, "each retry doubles the wait")
	})
}

func TestPostCommitSchedulerNewCommitReplacesFailedFlushRetry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := make(chan string, 10)
		scheduler := newPostCommitScheduler(func(_ context.Context, _, _, head string) error {
			calls <- head
			if head == "first" {
				return errors.New("daemon busy")
			}
			return nil
		})
		defer scheduler.stop()
		require.NoError(t, scheduler.schedule("repo", "checkout", "feature", "first", time.Minute))
		time.Sleep(time.Minute)
		synctest.Wait()
		require.Len(t, calls, 1)
		assert.Equal(t, "first", <-calls)

		// A new commit schedules its own flush, which covers the old range.
		require.NoError(t, scheduler.schedule("repo", "checkout", "feature", "second", time.Minute))
		time.Sleep(time.Hour)
		synctest.Wait()
		require.Len(t, calls, 1, "the failed flush stops retrying once replaced")
		assert.Equal(t, "second", <-calls)
	})
}
