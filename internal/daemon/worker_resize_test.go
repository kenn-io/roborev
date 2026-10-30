package daemon

import (
	"context"
	"io"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

func TestWorkerPoolResizesWhileRunning(t *testing.T) {
	const agentName = "resize-blocker"
	synctest.Test(t, func(t *testing.T) {
		assert := assert.New(t)
		db := testutil.OpenTestDB(t)
		cfg := config.DefaultConfig()
		cfg.MaxWorkers = 1
		server := newServerWithLogs(db, cfg, "", newTestErrorLog(), newTestActivityLog())
		t.Cleanup(func() { _ = server.Close() })
		repo := testutil.NewTestRepoWithCommit(t)
		storedRepo, err := db.GetOrCreateRepo(repo.Path())
		require.NoError(t, err)
		release := make(chan struct{})
		agent.RegisterForTest(t, &agent.FakeAgent{NameStr: agentName, ReviewFn: func(ctx context.Context, _, _, _ string, _ io.Writer) (string, error) {
			select {
			case <-release:
				return "No issues found.", nil
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}})
		subscriber, events := server.broadcaster.Subscribe("")
		defer server.broadcaster.Unsubscribe(subscriber)
		// Wait on job events rather than synctest.Wait alone: SQLite lock
		// retries between two workers can let synctest.Wait return early.
		type jobEvent struct {
			eventType string
			jobID     int64
		}
		seen := map[jobEvent]bool{}
		awaitEvent := func(eventType string, jobID int64) {
			t.Helper()
			deadline := time.After(time.Minute)
			for !seen[jobEvent{eventType, jobID}] {
				select {
				case event := <-events:
					seen[jobEvent{event.Type, event.JobID}] = true
				case <-deadline:
					require.FailNowf(t, "missing event", "no %s event for job %d", eventType, jobID)
				}
			}
		}
		pool := server.workerPool
		pool.Start()
		defer pool.Stop()
		enqueue := func() int64 {
			job, err := db.EnqueueJob(storage.EnqueueOpts{RepoID: storedRepo.ID, GitRef: "prompt", Agent: agentName, Prompt: "Review this codebase", JobType: storage.JobTypeTask})
			require.NoError(t, err)
			pool.wake()
			return job.ID
		}
		status := func(id int64) storage.JobStatus {
			job, err := db.GetJobByID(id)
			require.NoError(t, err)
			return job.Status
		}

		first, second := enqueue(), enqueue()
		awaitEvent("review.started", first)
		synctest.Wait()
		assert.Equal(storage.JobStatusQueued, status(second), "one worker runs one job")

		pool.SetMaxWorkers(2)
		assert.Equal(2, pool.MaxWorkers())
		awaitEvent("review.started", second) // growing the pool starts a worker for queued work

		pool.SetMaxWorkers(0)
		synctest.Wait()
		assert.Equal(storage.JobStatusRunning, status(first), "shrinking must not cancel a running job")
		assert.Equal(storage.JobStatusRunning, status(second), "shrinking must not cancel a running job")

		close(release)
		awaitEvent("review.completed", first)
		awaitEvent("review.completed", second)
		assert.Equal(storage.JobStatusDone, status(first))
		assert.Equal(storage.JobStatusDone, status(second))

		third := enqueue()
		time.Sleep(10 * time.Second) // several fallback polls
		synctest.Wait()
		assert.Equal(storage.JobStatusQueued, status(third), "retired workers must not claim new work")

		pool.SetMaxWorkers(1)
		awaitEvent("review.completed", third)
	})
}

func TestConfigReloadResizesWorkerPool(t *testing.T) {
	for _, test := range []struct {
		name   string
		pinned int
		want   int
	}{
		{"config file sets the size", 0, 6},
		{"--workers keeps its size", 3, 3},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			writeTestFile(t, path, "max_workers = 2\n")
			cfg, err := config.LoadGlobalFrom(path)
			require.NoError(t, err)
			var options []ServerOption
			if test.pinned > 0 {
				// daemon run applies the flag to the loaded config and pins it.
				cfg.MaxWorkers = test.pinned
				options = append(options, WithPinnedMaxWorkers(test.pinned))
			}
			server := newServerWithLogs(testutil.OpenTestDB(t), cfg, path, newTestErrorLog(), newTestActivityLog())
			for _, option := range options {
				option(server)
			}
			t.Cleanup(func() { _ = server.Close() })

			writeTestFile(t, path, "max_workers = 6\n")
			server.configWatcher.reloadConfig()

			assert.Equal(t, test.want, server.workerPool.MaxWorkers())
			assert.Equal(t, test.want, server.configWatcher.Config().MaxWorkers)
		})
	}
}
