package daemon

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
)

type cancelDeadlineConnector struct {
	driver    driver.Driver
	dsn       string
	targetID  int64
	committed chan struct{}
	release   <-chan struct{}
}

func (c *cancelDeadlineConnector) Driver() driver.Driver { return c.driver }

func (c *cancelDeadlineConnector) Connect(context.Context) (driver.Conn, error) {
	conn, err := c.driver.Open(c.dsn)
	if err != nil {
		return nil, err
	}
	return &cancelDeadlineConn{Conn: conn, connector: c}, nil
}

type cancelDeadlineConn struct {
	driver.Conn
	connector *cancelDeadlineConnector
}

func (c *cancelDeadlineConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
}

func (c *cancelDeadlineConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	result, err := c.Conn.(driver.ExecerContext).ExecContext(ctx, query, args)
	if err == nil && strings.Contains(query, "SET status = 'canceled'") && len(args) >= 3 && args[2].Value == c.connector.targetID {
		rows, rowsErr := result.RowsAffected()
		if rowsErr == nil && rows > 0 {
			close(c.connector.committed)
			<-c.connector.release
		}
	}
	return result, err
}

func TestCancelOwnershipBeforeIndependentDeadline(t *testing.T) {
	for _, panel := range []bool{false, true} {
		name := "direct"
		if panel {
			name = "panel member"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				server, db, dir := newTestServer(t)
				var targetID, requestID int64
				if panel {
					_, members, synth := enqueueServerPanelRun(t, db, 1)
					targetID, requestID = members[0].ID, synth.ID
				} else {
					repo, err := db.GetOrCreateRepo(dir)
					require.NoError(t, err)
					job, err := db.EnqueueJob(storage.EnqueueOpts{RepoID: repo.ID, GitRef: "abc123", Agent: "test"})
					require.NoError(t, err)
					targetID, requestID = job.ID, job.ID
				}
				job, err := db.ClaimJob(testWorkerID)
				require.NoError(t, err)
				require.NotNil(t, job)
				require.Equal(t, targetID, job.ID)
				_, events := server.broadcaster.Subscribe("")
				ctx, cancel := withWallClockTimeout(t.Context(), time.Minute)
				defer cancel()
				server.workerPool.registerRunningJob(job.ID, cancel, time.Time{})

				var path string
				require.NoError(t, db.QueryRowContext(t.Context(), `SELECT file FROM pragma_database_list WHERE name = 'main'`).Scan(&path))
				committed, release := make(chan struct{}), make(chan struct{})
				connector := &cancelDeadlineConnector{driver: db.Driver(), dsn: path, targetID: targetID, committed: committed, release: release}
				pool := sql.OpenDB(connector)
				pool.SetMaxOpenConns(1)
				server.db = &storage.DB{DB: pool}
				var releaseOnce sync.Once
				releaseHandler := func() { releaseOnce.Do(func() { close(release) }) }
				var handlerDone, workerDone chan struct{}
				defer func() {
					releaseHandler()
					cancel()
					if handlerDone != nil {
						<-handlerDone
					}
					if workerDone != nil {
						<-workerDone
					}
					server.db = db
					assert.NoError(t, pool.Close())
				}()

				handlerDone = make(chan struct{})
				var response *CancelJobOutput
				var handlerErr error
				go func() {
					defer close(handlerDone)
					response, handlerErr = server.humaCancelJob(t.Context(), &CancelJobInput{Body: CancelJobRequest{JobID: requestID}})
				}()
				state := "committed"
				select {
				case <-committed:
				case <-handlerDone:
					state = "handler returned"
				}
				require.Equal(t, "committed", state, "cancel handler returned before the target update")
				current, err := db.GetJobByID(targetID)
				require.NoError(t, err)
				require.Equal(t, storage.JobStatusCanceled, current.Status)
				workerDone = make(chan struct{})
				go func() {
					defer close(workerDone)
					server.workerPool.savePromptOrFinish(ctx, testWorkerID, job, "prompt", nil, "")
				}()
				synctest.Wait()
				time.Sleep(time.Minute)
				<-ctx.Done()
				require.ErrorIs(t, ctx.Err(), context.DeadlineExceeded)
				// The handler is paused after SQLite committed and before ownership
				// publication. Check its writer contract directly: waiting for the
				// worker's RWMutex acquisition is not durable blocking under synctest.
				acquired := server.workerPool.attemptTransitionsMu.TryRLock()
				if acquired {
					server.workerPool.attemptTransitionsMu.RUnlock()
				}
				assert.False(t, acquired, "HTTP must hold the transition writer gate across ownership publication")
				assert.Empty(t, events, "no event observed while HTTP ownership is unpublished")
				releaseHandler()
				<-handlerDone
				<-workerDone
				require.NoError(t, handlerErr)
				require.NotNil(t, response)
				wantCount := 1
				if panel {
					wantCount = 2
				}
				require.Len(t, events, wantCount)
				seen := make(map[int64]int)
				for range wantCount {
					event := <-events
					assert.Equal(t, "review.canceled", event.Type)
					seen[event.JobID]++
				}
				assert.Equal(t, 1, seen[targetID])
				if panel {
					assert.Equal(t, 1, seen[requestID])
				}
			})
		})
	}
}
