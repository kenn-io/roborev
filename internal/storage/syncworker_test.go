package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
)

// newSyncHealthPool stalls connections and queries while reachable is false.
func newSyncHealthPool(t *testing.T, reachable *atomic.Bool) (*PgPool, *atomic.Int64, func()) {
	t.Helper()
	var lc net.ListenConfig
	listener, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	release := make(chan struct{})
	releaseStalls := sync.OnceFunc(func() { close(release) })
	var accepted atomic.Int64
	var server sync.WaitGroup
	server.Go(func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			server.Go(func() {
				defer conn.Close()
				if !reachable.Load() {
					<-release
					return
				}
				backend := pgproto3.NewBackend(conn, conn)
				if _, err := backend.ReceiveStartupMessage(); err != nil {
					return
				}
				backend.Send(&pgproto3.AuthenticationOk{})
				backend.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
				if backend.Flush() != nil {
					return
				}
				for {
					message, err := backend.Receive()
					if err != nil {
						return
					}
					switch message.(type) {
					case *pgproto3.Query:
						if !reachable.Load() {
							<-release
							return
						}
						backend.Send(&pgproto3.EmptyQueryResponse{})
						backend.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
						if backend.Flush() != nil {
							return
						}
					case *pgproto3.Terminate:
						return
					}
				}
			})
		}
	})
	t.Cleanup(func() {
		releaseStalls()
		_ = listener.Close()
		server.Wait()
	})
	pool, err := pgxpool.New(t.Context(), "postgres://test:test@"+listener.Addr().String()+"/test?sslmode=disable")
	require.NoError(t, err)
	t.Cleanup(func() {
		releaseStalls()
		pool.Close()
	})
	return &PgPool{pool: pool}, &accepted, releaseStalls
}

func TestSyncHealthCheckReturnsCachedStateDuringStall(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	var reachable atomic.Bool
	pool, accepted, releaseStalls := newSyncHealthPool(t, &reachable)
	worker := NewSyncWorker(nil, config.SyncConfig{})
	worker.running, worker.pgPool, worker.pgReachable = true, pool, true
	worker.healthCheckedAt = time.Now().Add(-healthInterval)
	worker.healthCtx, worker.healthCancel = context.WithCancel(t.Context())
	t.Cleanup(func() {
		releaseStalls()
		worker.mu.Lock()
		pool := worker.closePool()
		worker.mu.Unlock()
		pool.Close()
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		var calls sync.WaitGroup
		for range 20 {
			calls.Go(func() {
				healthy, message := worker.HealthCheck()
				assert.True(healthy)
				assert.Equal("connected", message)
			})
		}
		calls.Wait()
	}()
	t.Cleanup(func() { <-done })
	// The stalled TCP connection is external work that synctest cannot observe.
	completed := false
	select {
	case <-done:
		completed = true
	case <-time.After(time.Second):
	}
	assert.True(completed, "HealthCheck waited for PostgreSQL instead of returning cached health")
	// Wait for the actual stalled socket connection, outside synctest.
	require.Eventually(t, func() bool { return accepted.Load() == 1 }, time.Second, time.Millisecond)
	releaseStalls()
	// Wait for the socket failure to reach the probe goroutine, outside synctest.
	require.Eventually(t, func() bool {
		worker.mu.Lock()
		defer worker.mu.Unlock()
		return !worker.healthPinging
	}, 5*time.Second, time.Millisecond)
	// pgx opens a separate cancellation socket when the stalled connection fails.
	assert.Equal(int64(1), pool.Pool().Stat().NewConnsCount(), "concurrent reads must share one ping")
	healthy, message := worker.HealthCheck()
	assert.False(healthy)
	assert.Equal("connection lost", message)

	reachable.Store(true)
	worker.mu.Lock()
	worker.healthCheckedAt = time.Now().Add(-healthInterval)
	worker.mu.Unlock()
	healthy, message = worker.HealthCheck()
	assert.False(healthy)
	assert.Equal("connection lost", message)
	// The recovery ping uses the real TCP socket, outside synctest.
	require.Eventually(t, func() bool {
		healthy, _ := worker.HealthCheck()
		return healthy
	}, 5*time.Second, time.Millisecond)
	healthy, message = worker.HealthCheck()
	assert.True(healthy)
	assert.Equal("connected", message)
	assert.Equal(int64(2), pool.Pool().Stat().NewConnsCount())
}

func TestSyncHealthRecordsSyncLoopPingAfterFailedProbe(t *testing.T) {
	t.Parallel()
	var reachable atomic.Bool
	reachable.Store(true)
	pool, _, _ := newSyncHealthPool(t, &reachable)
	db := openTestDB(t)
	worker := NewSyncWorker(db, config.SyncConfig{})
	worker.running, worker.pgPool = true, pool
	worker.pgReachable = false
	worker.healthCheckedAt = time.Now()

	// A local database failure sends syncLoop through its post-failure ping.
	require.NoError(t, db.Close())
	done := make(chan struct{})
	go func() {
		defer close(done)
		worker.syncLoop(worker.stopCh, time.Millisecond, false)
	}()
	t.Cleanup(func() {
		close(worker.stopCh)
		<-done
	})
	// The sync loop's recovery ping uses a real TCP socket, outside synctest.
	require.Eventually(t, func() bool {
		healthy, _ := worker.HealthCheck()
		return healthy
	}, time.Second, time.Millisecond)
	healthy, message := worker.HealthCheck()
	assert.True(t, healthy)
	assert.Equal(t, "connected", message)
}

func TestSyncHealthClosePoolCancelsPingAndIgnoresLateResult(t *testing.T) {
	t.Parallel()
	for _, established := range []bool{false, true} {
		name := "connecting"
		if established {
			name = "established"
		}
		t.Run(name, func(t *testing.T) {
			assert := assert.New(t)
			var reachable atomic.Bool
			reachable.Store(established)
			pool, accepted, releaseStalls := newSyncHealthPool(t, &reachable)
			if established {
				require.NoError(t, pool.Ping(t.Context()))
				reachable.Store(false)
			}
			worker := NewSyncWorker(nil, config.SyncConfig{})
			worker.running, worker.pgPool, worker.pgReachable = true, pool, true
			worker.healthCheckedAt = time.Now().Add(-healthInterval)
			worker.healthCtx, worker.healthCancel = context.WithCancel(t.Context())
			t.Cleanup(worker.healthCancel)
			healthy, message := worker.HealthCheck()
			if !established {
				assert.True(healthy)
				assert.Equal("connected", message)
			}
			// Wait until the probe reaches the real TCP connection, outside synctest.
			require.Eventually(t, func() bool {
				if established {
					return pool.Pool().Stat().AcquiredConns() == 1
				}
				return accepted.Load() == 1
			}, time.Second, time.Millisecond)
			worker.mu.Lock()
			checkedAt := worker.healthCheckedAt
			worker.mu.Unlock()
			closed := make(chan struct{})
			if established {
				// No sync loop is running in this fixture for Stop to wait on.
				close(worker.doneCh)
			}
			go func() {
				defer close(closed)
				if established {
					worker.Stop()
					return
				}
				worker.mu.Lock()
				pool := worker.closePool()
				worker.mu.Unlock()
				pool.Close()
			}()
			t.Cleanup(func() {
				releaseStalls()
				<-closed
			})
			if established {
				// pgx cancellation opens a second socket and waits for the stalled server.
				require.Eventually(t, func() bool { return accepted.Load() == 2 }, time.Second, time.Millisecond)
				closeFinished := false
				select {
				case <-closed:
					closeFinished = true
				default:
				}
				assert.False(closeFinished, "pool close finished before the health check")

				done := make(chan struct{})
				go func() {
					defer close(done)
					healthy, message := worker.HealthCheck()
					assert.False(healthy)
					assert.Equal("not running", message)
				}()
				// Release sockets before waiting, even if the health check is blocked.
				t.Cleanup(func() {
					releaseStalls()
					<-done
				})
				// Pool closure waits on real sockets, which synctest cannot observe.
				completed := false
				select {
				case <-done:
					completed = true
				case <-time.After(time.Second):
				}
				assert.True(completed, "HealthCheck waited for pool closure")
				releaseStalls()
			}
			// Pool closure waits on real socket cancellation, outside synctest.
			closeFinished := false
			select {
			case <-closed:
				closeFinished = true
			case <-time.After(time.Second):
			}
			assert.True(closeFinished, "closePool waited for the ping timeout")
			// Wait for the probe goroutine after its socket is canceled.
			require.Eventually(t, func() bool {
				worker.mu.Lock()
				defer worker.mu.Unlock()
				return !worker.healthPinging
			}, time.Second, time.Millisecond)
			worker.mu.Lock()
			assert.True(worker.pgReachable, "the canceled pool's result must be ignored")
			assert.Equal(checkedAt, worker.healthCheckedAt)
			worker.mu.Unlock()
			if !established {
				healthy, message = worker.HealthCheck()
				assert.False(healthy)
				assert.Equal("not connected", message)
			}
		})
	}
}

func TestSyncPullWritesNotifyAfterEachCommittedMutation(t *testing.T) {
	t.Parallel()
	h := newSyncTestHelper(t)
	worker := NewSyncWorker(h.db, testSyncConfig())
	var notifications atomic.Int64
	worker.SetAfterPullWrite(func() { notifications.Add(1) })

	now := time.Now().UTC().Truncate(time.Second)
	finished := now
	job := PulledJob{
		UUID: testUUID("search-wake-job"), RepoIdentity: "github.com/example/search-wake",
		CommitSHA: "abc123", CommitAuthor: "Test", CommitSubject: "search wake",
		CommitTimestamp: now, GitRef: "abc123", Agent: "test", Reasoning: "thorough",
		JobType: JobTypeReview, ReviewType: "code", Status: string(JobStatusDone),
		EnqueuedAt: now, FinishedAt: &finished, SourceMachineID: testUUID("search-wake-machine"),
		UpdatedAt: now,
	}
	require.NoError(t, worker.pullJob(job))
	assert.Equal(t, int64(3), notifications.Load(), "repo, commit, and job each notify")

	review := PulledReview{
		JobEnqueuedAt: now, JobSourceMachineID: job.SourceMachineID,
		UUID: testUUID("search-wake-review"), JobUUID: job.UUID, Agent: "test",
		StructuredOutput:   reviewFixtureJSON("searchable finding"),
		UpdatedByMachineID: testUUID("search-wake-review-machine"), CreatedAt: now, UpdatedAt: now,
	}
	require.NoError(t, worker.pullReview(review))
	assert.Equal(t, int64(4), notifications.Load())

	response := PulledResponse{
		UUID: testUUID("search-wake-response"), JobUUID: job.UUID,
		Responder: "human", Response: "fixed", SourceMachineID: testUUID("search-wake-response-machine"),
		CreatedAt: now,
	}
	require.NoError(t, worker.pullResponse(response))
	assert.Equal(t, int64(5), notifications.Load())

	// Replaying the cursor lookback window must not schedule redundant work.
	require.NoError(t, worker.pullJob(job))
	require.NoError(t, worker.pullReview(review))
	require.NoError(t, worker.pullResponse(response))
	assert.Equal(t, int64(5), notifications.Load())
}

func TestSyncPullWritesDoNotNotifyRejectedRolledBackOrOrphanRows(t *testing.T) {
	t.Parallel()
	h := newSyncTestHelper(t)
	worker := NewSyncWorker(h.db, testSyncConfig())
	var notifications atomic.Int64
	worker.SetAfterPullWrite(func() { notifications.Add(1) })

	now := time.Now().UTC()
	missingJob := testUUID("missing-search-wake-job")
	require.NoError(t, worker.pullReview(PulledReview{
		JobEnqueuedAt: now,
		UUID:          testUUID("orphan-review"), JobUUID: missingJob, Agent: "test",
		UpdatedByMachineID: testUUID("orphan-review-machine"), CreatedAt: now, UpdatedAt: now,
	}))
	require.NoError(t, worker.pullResponse(PulledResponse{
		UUID: testUUID("orphan-response"), JobUUID: missingJob,
		Responder: "human", Response: "orphan", SourceMachineID: testUUID("orphan-response-machine"),
		CreatedAt: now,
	}))
	assert.Zero(t, notifications.Load())

	job := PulledJob{
		UUID: testUUID("invalid-review-parent"), RepoIdentity: "github.com/example/no-notify",
		GitRef: "main", Agent: "test", Reasoning: "thorough", JobType: JobTypeReview,
		ReviewType: "code", Status: string(JobStatusDone), EnqueuedAt: now,
		SourceMachineID: testUUID("invalid-review-parent-machine"), UpdatedAt: now,
	}
	require.NoError(t, worker.pullJob(job))
	before := notifications.Load()
	err := worker.pullReview(PulledReview{
		JobEnqueuedAt: now, JobSourceMachineID: job.SourceMachineID,
		UUID: testUUID("invalid-review"), JobUUID: job.UUID, Agent: "test",
		StructuredOutput:   []byte(`{"schema_version":`),
		UpdatedByMachineID: testUUID("invalid-review-machine"), CreatedAt: now, UpdatedAt: now,
	})
	require.Error(t, err)
	assert.Equal(t, before, notifications.Load(), "failed review transaction must not notify")

	_, err = h.db.ExecContext(t.Context(), `UPDATE review_jobs SET status = 'applied' WHERE uuid = ?`, job.UUID)
	require.NoError(t, err)
	stale := PulledJob{
		UUID: job.UUID, RepoIdentity: job.RepoIdentity, GitRef: "stale", Agent: "test",
		Reasoning: "thorough", JobType: JobTypeReview, ReviewType: "code",
		Status: string(JobStatusDone), EnqueuedAt: now.Add(-time.Hour),
		SourceMachineID: job.SourceMachineID, UpdatedAt: now.Add(-time.Hour),
	}
	require.NoError(t, worker.pullJob(stale))
	assert.Equal(t, before, notifications.Load(), "rejected stale job must not notify")
}

func TestSyncPullReviewRollsBackIntermediateWriteWithoutNotification(t *testing.T) {
	t.Parallel()
	h := newSyncTestHelper(t)
	worker := NewSyncWorker(h.db, testSyncConfig())
	var notifications atomic.Int64
	worker.SetAfterPullWrite(func() { notifications.Add(1) })

	now := time.Now().UTC().Truncate(time.Second)
	job := PulledJob{
		UUID: testUUID("rollback-review-parent"), RepoIdentity: "github.com/example/rollback-review",
		GitRef: "main", Agent: "test", Reasoning: "thorough", JobType: JobTypeReview,
		ReviewType: "code", Status: string(JobStatusDone), EnqueuedAt: now,
		SourceMachineID: testUUID("rollback-review-parent-machine"), UpdatedAt: now,
	}
	require.NoError(t, worker.pullJob(job))
	before := notifications.Load()

	var jobID int64
	require.NoError(t, h.db.QueryRowContext(t.Context(), `SELECT id FROM review_jobs WHERE uuid = ?`, job.UUID).Scan(&jobID))
	reviewUUID := testUUID("rollback-review")
	_, err := h.db.ExecContext(t.Context(), `
		INSERT INTO legacy_reviews (
			job_id, agent, output, created_at, closed, uuid, migration_error
		) VALUES (?, 'test', 'legacy', ?, 0, ?, 'pending conversion')
	`, jobID, now.Format(time.RFC3339), reviewUUID)
	require.NoError(t, err)
	_, err = h.db.ExecContext(t.Context(), `
		CREATE TRIGGER fail_pulled_review_legacy_resolution
		BEFORE UPDATE OF resolved_at ON legacy_reviews
		BEGIN
			SELECT RAISE(ABORT, 'injected late review transaction failure');
		END
	`)
	require.NoError(t, err)

	err = worker.pullReview(PulledReview{
		JobEnqueuedAt: now, JobSourceMachineID: job.SourceMachineID,
		UUID: reviewUUID, JobUUID: job.UUID, Agent: "test",
		StructuredOutput:   reviewFixtureJSON("transaction should roll back"),
		UpdatedByMachineID: testUUID("rollback-review-machine"), CreatedAt: now, UpdatedAt: now,
	})
	require.ErrorContains(t, err, "injected late review transaction failure")
	assert.Equal(t, before, notifications.Load(), "rolled-back review must not notify")

	var reviewCount int
	require.NoError(t, h.db.QueryRowContext(t.Context(), `SELECT count(*) FROM reviews WHERE uuid = ?`, reviewUUID).Scan(&reviewCount))
	assert.Zero(t, reviewCount, "review insert before the injected failure must roll back")
	var resolvedAt *string
	require.NoError(t, h.db.QueryRowContext(t.Context(), `SELECT resolved_at FROM legacy_reviews WHERE uuid = ?`, reviewUUID).Scan(&resolvedAt))
	assert.Nil(t, resolvedAt)
}

func TestSyncPullWriteCallbackIsRaceSafeNilSafeAndInvokedOutsideLock(t *testing.T) {
	t.Parallel()
	h := newSyncTestHelper(t)
	worker := NewSyncWorker(h.db, testSyncConfig())
	worker.SetAfterPullWrite(nil)
	worker.notifyAfterPullWrite()

	done := make(chan struct{})
	worker.SetAfterPullWrite(func() {
		worker.SetAfterPullWrite(nil)
		close(done)
	})
	go worker.notifyAfterPullWrite()
	callbackCompleted := false
	select {
	case <-done:
		callbackCompleted = true
	case <-time.After(time.Second):
	}
	require.True(t, callbackCompleted, "callback ran while the worker mutex was held")

	var wg sync.WaitGroup
	for range 100 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			worker.SetAfterPullWrite(func() {})
		}()
		go func() {
			defer wg.Done()
			assert.NoError(t, worker.syncMu.Acquire(t.Context(), 1))
			worker.notifyAfterPullWrite()
			worker.syncMu.Release(1)
		}()
	}
	wg.Wait()
}

func testSyncConfig() config.SyncConfig {
	return config.SyncConfig{Enabled: true, Interval: "1h"}
}

func TestSyncFailureOutages(t *testing.T) {
	t.Parallel()
	worker := NewSyncWorker(nil, config.SyncConfig{})
	var reasons []string
	worker.SetOnSyncFailed(func(reason string) { reasons = append(reasons, reason) })
	remote := &syncSourceError{context.DeadlineExceeded}
	worker.observeFailure(&syncSourceError{&pgconn.PgError{Code: "23505"}})
	worker.observeFailure(remote) // No successful full pass yet.
	assert.Empty(t, reasons)
	worker.observePass()
	worker.observeFailure(fmt.Errorf("push experiment definition example: %w", &syncSourceError{&pgconn.PgError{Code: "22001"}}))
	worker.observeFailure(fmt.Errorf("push experiment assignment job/example: %w", &syncSourceError{&pgconn.PgError{Code: "23505"}}))
	assert.Empty(t, reasons)
	worker.observeFailure(errors.New("local SQLite failure"))
	worker.observeFailure(remote)
	worker.observeFailure(remote)
	assert.Equal(t, []string{"timeout"}, reasons)
	worker.observeFailure(&syncSourceError{&pgconn.PgError{Code: "22001"}})
	worker.observeFailure(errors.New("local SQLite failure"))
	worker.observeFailure(remote)
	assert.Equal(t, []string{"timeout"}, reasons)
	worker.observePass()
	worker.observeFailure(&syncSourceError{&pgconn.PgError{Code: "28P01"}})
	assert.Equal(t, []string{"timeout", "auth"}, reasons)
	worker.observePass()
	worker.observeFailure(&syncSourceError{&pgconn.PgError{Code: "25006"}})
	assert.Equal(t, []string{"timeout", "auth", "other"}, reasons)
}

func TestSyncPassWithRejectedRowsArmsOutages(t *testing.T) {
	t.Parallel()
	worker := NewSyncWorker(nil, config.SyncConfig{})
	var reasons []string
	worker.SetOnSyncFailed(func(reason string) { reasons = append(reasons, reason) })
	worker.passError = &syncSourceError{&pgconn.PgError{Code: "23505"}}
	worker.observePass()
	worker.observeFailure(&syncSourceError{net.ErrClosed})
	assert.Equal(t, []string{"network"}, reasons)
	worker.passError = &syncSourceError{&pgconn.PgError{Code: "42501"}}
	worker.observePass()
	worker.observeFailure(&syncSourceError{context.DeadlineExceeded})
	assert.Equal(t, []string{"network"}, reasons, "a swallowed source failure keeps the outage open")
}

func TestSyncStoppedPassReportsSavedSourceFailure(t *testing.T) {
	t.Parallel()
	worker := NewSyncWorker(nil, config.SyncConfig{})
	var reasons []string
	worker.SetOnSyncFailed(func(reason string) { reasons = append(reasons, reason) })
	worker.observePass()
	conflict := fmt.Errorf("%w: experiment definition conflict", errSyncRowRejected)
	worker.observeFailure(fmt.Errorf("push experiment definition example: %w", &syncSourceError{conflict}))
	assert.Empty(t, reasons, "an experiment conflict is not an outage")
	worker.passError = &syncSourceError{net.ErrClosed}
	worker.observeFailure(fmt.Errorf("batch upsert reviews: %w", &syncSourceError{errReviewAttemptPending}))
	assert.Equal(t, []string{"network"}, reasons)
}

func TestSyncFailureReason(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		err  error
		want string
	}{
		{&pgconn.PgError{Code: "28P01"}, "auth"},
		{&pgconn.PgError{Code: "42501"}, "auth"},
		{&pgconn.PgError{Code: "08006"}, "network"},
		{&pgconn.PgError{Code: "57P01"}, "network"},
		{&pgconn.PgError{Code: "57P03"}, "network"},
		{&pgconn.PgError{Code: "53300"}, "network"},
		{io.EOF, "network"},
		{io.ErrUnexpectedEOF, "network"},
		{net.ErrClosed, "network"},
		{pgconn.ErrConnClosed, "network"},
		{&net.OpError{Op: "dial", Err: errors.New("connection refused")}, "network"},
		{context.DeadlineExceeded, "timeout"},
		{&net.DNSError{IsTimeout: true}, "timeout"},
		{errors.New("failed"), "other"},
	} {
		assert.Equal(t, tc.want, syncFailureReason(fmt.Errorf("sync: %w", tc.err)))
	}
}
