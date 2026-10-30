package daemon

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPostCommitSchedulerDebouncesPerBranch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := make(chan string, 10)
		scheduler := newPostCommitScheduler(func(_ context.Context, root, branch, head string) {
			calls <- root + ":" + branch + ":" + head
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
		scheduler := newPostCommitScheduler(func(ctx context.Context, _, _, head string) {
			calls <- head
			<-ctx.Done()
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
