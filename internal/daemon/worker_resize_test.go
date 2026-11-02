package daemon

import (
	"context"
	"io"
	"path/filepath"
	"sync"
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

func TestConfigReloadAppliesWorkerCountsInOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	writeTestFile(t, path, "max_workers = 1\n")
	cfg, err := config.LoadGlobalFrom(path)
	require.NoError(t, err)
	watcher := NewConfigWatcher(path, cfg, NewBroadcaster(), nil)
	pool := NewWorkerPool(nil, watcher, 1, NewBroadcaster(), nil, nil)
	entered, release := make(chan struct{}), make(chan struct{})
	watcher.OnReload(func(cfg *config.Config) {
		if cfg.MaxWorkers == 0 {
			close(entered)
			<-release
		}
		pool.SetMaxWorkers(cfg.MaxWorkers)
	})

	writeTestFile(t, path, "max_workers = 0\n")
	var reloads sync.WaitGroup
	reloads.Go(watcher.reloadConfig)
	<-entered
	writeTestFile(t, path, "max_workers = 3\n")
	reloads.Go(watcher.reloadConfig)
	// Reloads perform filesystem I/O and contend on a mutex; synctest
	// cannot treat either operation as durably blocked.
	assert.Never(t, func() bool { return watcher.ReloadCounter() > 1 }, 100*time.Millisecond, time.Millisecond)
	close(release)
	reloads.Wait()

	assert.Equal(t, 3, watcher.Config().MaxWorkers)
	assert.Equal(t, 3, pool.MaxWorkers(), "an older callback must not overwrite the latest worker count")
}

func TestWorkerPoolShrinkDuringClaimBackoff(t *testing.T) {
	for _, test := range []struct {
		name   string
		regrow bool
		want   storage.JobStatus
	}{
		{"surplus worker retires", false, storage.JobStatusQueued},
		{"immediate regrow resumes claiming", true, storage.JobStatusDone},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				assert := assert.New(t)
				db := testutil.OpenTestDB(t)
				repo, err := db.GetOrCreateRepo(t.TempDir())
				require.NoError(t, err)
				job, err := db.EnqueueJob(storage.EnqueueOpts{
					RepoID: repo.ID, GitRef: "prompt", Agent: "test",
					Prompt: "Review this codebase", JobType: storage.JobTypeTask,
				})
				require.NoError(t, err)
				// A hydration error rolls the claim back and enters the worker's
				// five-second retry backoff without a pending SQLite operation.
				_, err = db.Exec("UPDATE review_jobs SET panel_member_index = 'invalid' WHERE id = ?", job.ID)
				require.NoError(t, err)
				errorLog := newTestErrorLog()
				pool := NewWorkerPool(db, NewStaticConfig(config.DefaultConfig()), 1, NewBroadcaster(), errorLog, nil)
				pool.Start()
				defer pool.Stop()
				synctest.Wait()
				require.NotEmpty(t, errorLog.Recent())
				assert.Contains(errorLog.Recent()[0].Message, "panel_member_index")
				_, err = db.Exec("UPDATE review_jobs SET panel_member_index = NULL WHERE id = ?", job.ID)
				require.NoError(t, err)

				pool.SetMaxWorkers(0)
				if test.regrow {
					pool.SetMaxWorkers(1)
				}
				time.Sleep(10 * time.Second) // pass two claim retry intervals in virtual time
				synctest.Wait()
				stored, err := db.GetJobByID(job.ID)
				require.NoError(t, err)
				assert.Equal(test.want, stored.Status)
			})
		})
	}
}

func TestWorkerPoolShrinkDuringBlockedClaim(t *testing.T) {
	assert := assert.New(t)
	db := testutil.OpenTestDB(t)
	repo, err := db.GetOrCreateRepo(t.TempDir())
	require.NoError(t, err)
	job, err := db.EnqueueJob(storage.EnqueueOpts{
		RepoID: repo.ID, GitRef: "prompt", Agent: "test",
		Prompt: "Review this codebase", JobType: storage.JobTypeTask,
	})
	require.NoError(t, err)
	// A separate SQLite write transaction blocks BEGIN IMMEDIATE in the
	// worker while still allowing its queue-pause read in WAL mode.
	lockConn, err := db.Conn(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, lockConn.Close()) })
	_, err = lockConn.ExecContext(t.Context(), "BEGIN IMMEDIATE")
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = lockConn.ExecContext(context.Background(), "ROLLBACK") })
	pool := NewWorkerPool(db, NewStaticConfig(config.DefaultConfig()), 1, NewBroadcaster(), nil, nil)
	pool.Start()
	defer pool.Stop()
	// SQLite lock waits run outside synctest's virtual clock.
	require.Eventually(t, func() bool {
		pool.runningJobsMu.Lock()
		defer pool.runningJobsMu.Unlock()
		return pool.workerJobs["worker-0"] == workerClaimingJobID
	}, 10*time.Second, time.Millisecond)

	pool.SetMaxWorkers(0)
	_, err = lockConn.ExecContext(t.Context(), "COMMIT")
	require.NoError(t, err)
	// Let the blocked SQLite claim unwind and the surplus worker retire.
	require.Eventually(t, func() bool {
		pool.workersMu.Lock()
		defer pool.workersMu.Unlock()
		return len(pool.liveWorkers) == 0
	}, 10*time.Second, time.Millisecond)
	stored, err := db.GetJobByID(job.ID)
	require.NoError(t, err)
	assert.Equal(storage.JobStatusQueued, stored.Status, "a pending claim must roll back when its worker becomes surplus")
	assert.Empty(stored.WorkerID)
}
