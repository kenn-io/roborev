package storage

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWriteGateCanceledWaiterDoesNotBlockNextWrite(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		db := openTestDB(t)
		defer db.Close()
		tx, err := db.BeginTx(t.Context(), nil)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()
		_, err = tx.ExecContext(t.Context(), `INSERT INTO sync_state (key, value) VALUES ('holder', 'rolled back')`)
		require.NoError(t, err)

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		canceled := make(chan error, 1)
		go func() {
			_, err := db.ExecContext(ctx, `INSERT INTO sync_state (key, value) VALUES ('canceled', 'absent')`)
			canceled <- err
		}()
		synctest.Wait()
		next := make(chan error, 1)
		go func() {
			_, err := db.ExecContext(t.Context(), `INSERT INTO sync_state (key, value) VALUES ('next', 'committed')`)
			next <- err
		}()
		synctest.Wait()
		cancel()
		require.ErrorIs(t, <-canceled, context.Canceled)
		require.NoError(t, tx.Rollback())
		require.NoError(t, <-next)
		var count int
		require.NoError(t, db.QueryRowContext(t.Context(), `SELECT count(*) FROM sync_state WHERE key IN ('holder', 'canceled')`).Scan(&count))
		assert.Equal(t, 0, count)
	})
}

func TestWriteGateReleasesAfterFailedBeginOrRollback(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		db := openTestDB(t)
		defer db.Close()
		conn, err := db.Conn(t.Context())
		require.NoError(t, err)
		defer conn.Close()
		require.NoError(t, beginImmediate(t.Context(), conn))
		_, err = conn.ExecContext(t.Context(), `INSERT INTO sync_state (key, value) VALUES ('rollback', 'absent')`)
		require.NoError(t, err)
		require.NoError(t, rollbackConn(conn))
		// Invalid SQL fails after acquiring the gate. Keep the connection
		// open to prove the error itself releases it.
		_, err = conn.ExecContext(t.Context(), "BEGIN IMMEDIATE invalid")
		require.Error(t, err)

		// A leaked gate makes this wait out the fake-clock write timeout.
		_, err = db.ExecContext(t.Context(), `INSERT INTO sync_state (key, value) VALUES ('next', 'committed')`)
		require.NoError(t, err)
	})
}

func TestUserWritesQueueBehindPullTransaction(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"enqueue", "post-commit", "close"} {
		t.Run(kind, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newSyncTestHelper(t)
				job := h.createCompletedJob("existing")
				_, err := h.db.ExecContext(t.Context(), `
					CREATE TABLE write_order (id INTEGER PRIMARY KEY, writer TEXT);
					CREATE TRIGGER job_write_order AFTER INSERT ON review_jobs BEGIN
						INSERT INTO write_order (writer) VALUES (NEW.git_ref);
					END;
					CREATE TRIGGER close_write_order AFTER UPDATE ON reviews BEGIN
						INSERT INTO write_order (writer) VALUES ('user');
					END;
				`)
				require.NoError(t, err)

				conn, err := h.db.Conn(t.Context())
				require.NoError(t, err)
				defer conn.Close()
				var gate *writeGate
				require.NoError(t, conn.Raw(func(raw any) error {
					gate = raw.(*writeGateConn).gate
					return nil
				}))
				require.NoError(t, conn.Close())
				require.NoError(t, gate.acquire(t.Context()))

				userDone := make(chan error, 1)
				go func() {
					var err error
					opts := EnqueueOpts{RepoID: h.repo.ID, GitRef: "user", Agent: "test"}
					switch kind {
					case "enqueue":
						_, err = h.db.EnqueueJob(opts)
					case "post-commit":
						_, _, err = h.db.EnqueuePostCommitJob(opts)
					case "close":
						err = h.db.MarkReviewClosedByJobID(job.ID, true)
					}
					userDone <- err
				}()
				synctest.Wait()
				var committed int
				require.NoError(t, h.db.QueryRowContext(t.Context(), `SELECT count(*) FROM write_order`).Scan(&committed))
				assert.Equal(t, 0, committed, "user write must wait for admission")
				gate.queue.Release(1)

				// Let the user's GetMachineID write finish, then hold admission
				// until its actual write has joined the queue ahead of the pull.
				require.NoError(t, gate.acquire(t.Context()))
				synctest.Wait()
				require.NoError(t, h.db.QueryRowContext(t.Context(), `SELECT count(*) FROM write_order`).Scan(&committed))
				assert.Equal(t, 0, committed, "user write must wait for admission after GetMachineID finishes")
				pullDone := make(chan error, 1)
				go func() {
					pullDone <- h.db.UpsertPulledJob(PulledJob{
						UUID: testUUID("queued-pull"), GitRef: "pull", Agent: "test",
						JobType: JobTypeReview, Status: string(JobStatusDone),
						EnqueuedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
						SourceMachineID: testUUID("peer"),
					}, h.repo.ID, nil)
				}()
				synctest.Wait()
				gate.queue.Release(1)
				require.NoError(t, <-userDone)
				require.NoError(t, <-pullDone)

				var order string
				require.NoError(t, h.db.QueryRowContext(t.Context(), `SELECT group_concat(writer, ',') FROM (SELECT writer FROM write_order ORDER BY id)`).Scan(&order))
				assert.Equal(t, "user,pull", order)
			})
		})
	}
}

func TestReadTransactionDoesNotHoldWriters(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		h := newSyncTestHelper(t)
		job := h.createCompletedJob("read-transaction")
		tx, err := h.db.BeginTx(t.Context(), nil)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()
		var count int
		require.NoError(t, tx.QueryRowContext(t.Context(), `SELECT count(*) FROM reviews`).Scan(&count))
		// A gated reader would leave this write to time out on the fake clock.
		_, err = h.db.AddCommentToJob(job.ID, "user", "comment beside reader")
		require.NoError(t, err)
	})
}
