package storage

import (
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUserWritesQueueBehindPullTransaction(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"enqueue", "post-commit", "comment", "close"} {
		t.Run(kind, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newSyncTestHelper(t)
				job := h.createCompletedJob("existing")
				_, err := h.db.ExecContext(t.Context(), `
					CREATE TABLE write_order (id INTEGER PRIMARY KEY, writer TEXT);
					CREATE TRIGGER job_write_order AFTER INSERT ON review_jobs BEGIN
						INSERT INTO write_order (writer) VALUES (NEW.git_ref);
					END;
					CREATE TRIGGER comment_write_order AFTER INSERT ON responses BEGIN
						INSERT INTO write_order (writer) VALUES ('user');
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
					case "comment":
						_, err = h.db.AddCommentToJob(job.ID, "user", "comment during pull")
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

func TestPulledWritesWaitForWriter(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"job", "review"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			h := newSyncTestHelper(t)
			job := h.createPendingJob("pull-contention")
			setStatus(t, h.db, job.ID, JobStatusDone)
			now := time.Now().UTC()
			enqueuedAt := storedJobEnqueuedAt(t, h.db, job.ID)
			owner := storedJobSourceMachineID(t, h.db, job.ID)
			pull := func() error {
				return h.db.UpsertPulledJob(PulledJob{
					UUID: *job.UUID, GitRef: "updated-ref", Agent: "test",
					JobType: JobTypeReview, Status: string(JobStatusDone),
					EnqueuedAt: enqueuedAt, SourceMachineID: owner, UpdatedAt: now,
				}, h.repo.ID, nil)
			}
			if kind == "review" {
				pull = func() error {
					return h.db.UpsertPulledReview(PulledReview{
						UUID: testUUID("contended-review"), JobUUID: *job.UUID, Agent: "test",
						JobEnqueuedAt: enqueuedAt, JobSourceMachineID: owner,
						StructuredOutput:   reviewFixtureJSON("pulled finding"),
						UpdatedByMachineID: testUUID("peer"), CreatedAt: now, UpdatedAt: now,
					})
				}
			}

			writer, err := h.db.BeginTx(t.Context(), nil)
			require.NoError(t, err)
			defer func() { _ = writer.Rollback() }()
			_, err = writer.ExecContext(t.Context(), `INSERT INTO sync_state (key, value) VALUES ('writer-lock', 'held')`)
			require.NoError(t, err)

			done := make(chan error, 1)
			go func() { done <- pull() }()
			// Wall-clock wait: hold a real SQLite writer while the pull starts.
			var pullErr error
			finished := false
			select {
			case pullErr = <-done:
				finished = true
			case <-time.After(100 * time.Millisecond): //nolint:kennlint // deadlinetest: holds a real SQLite writer while the pull waits for its lock
			}
			require.NoError(t, writer.Commit())
			if !finished {
				pullErr = <-done
			}
			require.NoError(t, pullErr)
			if kind == "job" {
				stored, err := h.db.GetJobByID(job.ID)
				require.NoError(t, err)
				assert.Equal(t, "updated-ref", stored.GitRef)
			} else {
				stored, err := h.db.GetReviewByJobID(job.ID)
				require.NoError(t, err)
				assert.Equal(t, testUUID("contended-review"), *stored.UUID)
			}
		})
	}
}
