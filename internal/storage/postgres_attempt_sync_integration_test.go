//go:build postgres

package storage

import (
	"database/sql"
	"fmt"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func importedAttemptNodes(t *testing.T) (*integrationEnv, testNode, testNode, int64, int64) {
	t.Helper()
	env := newIntegrationEnv(t)
	a := env.setupNode("source", "attempt-repository", "1h")
	b := env.setupNode("rerun", "attempt-repository", "1h")
	job, _ := createCompletedReview(t, a.DB, a.Repo.ID, "abc123", "Author", "Subject", "original prompt", "Original review.")
	_, err := a.Worker.SyncNow()
	require.NoError(t, err)
	_, err = b.Worker.SyncNow()
	require.NoError(t, err)
	var importedID int64
	require.NoError(t, b.DB.QueryRowContext(t.Context(), `SELECT id FROM review_jobs WHERE uuid = ?`, job.UUID).Scan(&importedID))
	return env, a, b, job.ID, importedID
}

func startImportedAttempt(t *testing.T, node testNode, id int64, prompt string) {
	t.Helper()
	require.NoError(t, node.DB.ReenqueueJob(id, ReenqueueOpts{}))
	job, err := node.DB.ClaimJob("attempt-worker")
	require.NoError(t, err)
	require.NotNil(t, job)
	require.Equal(t, id, job.ID)
	require.NoError(t, node.DB.SaveJobPrompt(id, prompt))
}

func TestIntegrationImportedRerunWithoutReview(t *testing.T) { //nolint:paralleltest // shares the roborev schema in the PostgreSQL database at TEST_POSTGRES_URL
	for _, tc := range []struct {
		name   string
		status JobStatus
		prompt string
	}{
		{"failed", JobStatusFailed, "unsuccessful attempt prompt"},
		{"canceled", JobStatusCanceled, "unsuccessful attempt prompt"},
		{"failed before prompt", JobStatusFailed, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status := tc.status
			_, a, b, sourceID, importedID := importedAttemptNodes(t)
			startImportedAttempt(t, b, importedID, tc.prompt)
			if status == JobStatusFailed {
				changed, err := b.DB.FailJob(importedID, "attempt-worker", "Synthetic failure")
				require.NoError(t, err)
				require.True(t, changed)
			} else {
				require.NoError(t, b.DB.CancelJob(importedID))
				changed, err := b.DB.ReleaseCanceledJob(importedID, "attempt-worker")
				require.NoError(t, err)
				require.True(t, changed)
			}
			_, err := b.Worker.SyncNow()
			require.NoError(t, err)
			_, err = a.Worker.SyncNow()
			require.NoError(t, err)
			for i, node := range []testNode{a, b} {
				id := []int64{sourceID, importedID}[i]
				job, err := node.DB.GetJobByID(id)
				require.NoError(t, err)
				assert.Equal(t, status, job.Status)
				assert.Equal(t, tc.prompt, job.Prompt)
				_, err = node.DB.GetReviewByJobID(id)
				assert.ErrorIs(t, err, sql.ErrNoRows)
			}
		})
	}
}

func TestIntegrationOlderAttemptFinishesLast(t *testing.T) { //nolint:paralleltest // shares the roborev schema in the PostgreSQL database at TEST_POSTGRES_URL
	env, a, b, sourceID, importedID := importedAttemptNodes(t)
	startImportedAttempt(t, a, sourceID, "obsolete overlapping attempt")
	startImportedAttempt(t, b, importedID, "current attempt prompt")
	require.NoError(t, completeReviewFixture(b.DB, importedID, "test", "current attempt prompt", "Current review."))
	_, err := b.Worker.SyncNow()
	require.NoError(t, err)
	// The earlier executor finishes and uploads after the newer attempt.
	require.NoError(t, completeReviewFixture(a.DB, sourceID, "test", "obsolete overlapping attempt", "Obsolete review."))
	_, err = a.Worker.SyncNow()
	require.NoError(t, err)
	_, err = b.Worker.SyncNow()
	require.NoError(t, err)
	c := env.setupNode("observer", "attempt-repository", "1h")
	jobs, err := c.DB.ListJobs("", "", 10, 0)
	require.NoError(t, err)
	require.Len(t, jobs, 1)
	assert.Equal(t, "current attempt prompt", jobs[0].Prompt)
	review, err := c.DB.GetReviewByJobID(jobs[0].ID)
	require.NoError(t, err)
	assert.Equal(t, "Current review.", review.StructuredOutput["summary"])
}

func TestIntegrationReviewWaitsForProducingAttempt(t *testing.T) { //nolint:paralleltest // shares the roborev schema in the PostgreSQL database at TEST_POSTGRES_URL
	_, a, b, sourceID, importedID := importedAttemptNodes(t)
	startImportedAttempt(t, b, importedID, "current prompt")
	require.NoError(t, completeReviewFixture(b.DB, importedID, "test", "current prompt", "Current review."))
	_, err := b.Worker.SyncNow()
	require.NoError(t, err)
	// Skip the job phase to model a newer attempt arriving between this
	// client's job and review queries. Its review cursor must not advance.
	jobCursor, err := a.DB.GetSyncState(SyncStateLastJobCursor)
	require.NoError(t, err)
	reviewCursor, err := a.DB.GetSyncState(SyncStateLastReviewCursor)
	require.NoError(t, err)
	require.NoError(t, a.DB.SetSyncState(SyncStateLastJobCursor, time.Now().Add(24*time.Hour).Format(time.RFC3339Nano)+" 0"))
	_, err = a.Worker.SyncNow()
	require.ErrorContains(t, err, "review attempt is newer than local job")
	after, err := a.DB.GetSyncState(SyncStateLastReviewCursor)
	require.NoError(t, err)
	assert.Equal(t, reviewCursor, after)
	require.NoError(t, a.DB.SetSyncState(SyncStateLastJobCursor, jobCursor))
	_, err = a.Worker.SyncNow()
	require.NoError(t, err)
	review, err := a.DB.GetReviewByJobID(sourceID)
	require.NoError(t, err)
	assert.Equal(t, "Current review.", review.StructuredOutput["summary"])
}

func TestIntegrationConcurrentRerunsWithTiedTimes(t *testing.T) { //nolint:paralleltest // shares the PostgreSQL schema
	for _, winnerFirst := range []bool{true, false} {
		t.Run(map[bool]string{true: "winner uploads first", false: "winner uploads last"}[winnerFirst], func(t *testing.T) {
			env := newIntegrationEnv(t)
			a := env.setupNode("source", "tied-attempt-repository", "1h")
			b := env.setupNode("rerun-one", "tied-attempt-repository", "1h")
			c := env.setupNode("rerun-two", "tied-attempt-repository", "1h")
			job, _ := createCompletedReview(t, a.DB, a.Repo.ID, "abc123", "Author", "Subject", "original prompt", "Original result.")
			_, err := a.DB.ExecContext(t.Context(), `UPDATE review_jobs SET enqueued_at = ? WHERE id = ?`, preciseTimestampAt(time.Now().Add(time.Hour)), job.ID)
			require.NoError(t, err)
			for _, node := range []testNode{a, b, c} {
				_, err := node.Worker.SyncNow()
				require.NoError(t, err)
			}
			ownerB, err := b.DB.GetMachineID()
			require.NoError(t, err)
			ownerC, err := c.DB.GetMachineID()
			require.NoError(t, err)
			winner, loser := b, c
			if ownerB.Compare(ownerC) < 0 {
				winner, loser = c, b
			}
			var attempts []time.Time
			for i, node := range []testNode{winner, loser} {
				var id int64
				require.NoError(t, node.DB.QueryRowContext(t.Context(), `SELECT id FROM review_jobs WHERE uuid = ?`, job.UUID).Scan(&id))
				label := []string{"Winning result.", "Losing result."}[i]
				startImportedAttempt(t, node, id, label)
				require.NoError(t, node.DB.CompleteJobResult(id, "test", ReviewCompletion{StructuredOutput: reviewFixtureJSON(label)}))
				current, err := node.DB.GetJobByID(id)
				require.NoError(t, err)
				attempts = append(attempts, current.EnqueuedAt)
			}
			require.Equal(t, attempts[0], attempts[1], "both lagging clocks allocate the same next microsecond")
			order := []testNode{loser, winner}
			if winnerFirst {
				order = []testNode{winner, loser}
			}
			for _, node := range order {
				_, err := node.Worker.SyncNow()
				require.NoError(t, err)
			}
			for range 2 {
				for _, node := range []testNode{a, b, c} {
					_, err := node.Worker.SyncNow()
					require.NoError(t, err)
				}
			}
			winnerID, err := winner.DB.GetMachineID()
			require.NoError(t, err)
			for _, node := range []testNode{a, b, c} {
				var id int64
				require.NoError(t, node.DB.QueryRowContext(t.Context(), `SELECT id FROM review_jobs WHERE uuid = ?`, job.UUID).Scan(&id))
				current, err := node.DB.GetJobByID(id)
				require.NoError(t, err)
				assert.Equal(t, winnerID, *current.SourceMachineID)
				assert.Equal(t, "Winning result.", current.Prompt)
				review, err := node.DB.GetReviewByJobID(id)
				require.NoError(t, err)
				assert.Equal(t, "Winning result.", review.StructuredOutput["summary"])
			}
		})
	}
}

func TestIntegrationPendingReviewEditSurvivesAttemptRepair(t *testing.T) { //nolint:paralleltest // shares the PostgreSQL schema
	for _, conversion := range []bool{false, true} {
		t.Run(map[bool]string{false: "closure", true: "conversion"}[conversion], func(t *testing.T) {
			env := newIntegrationEnv(t)
			a := env.setupNode("producer", "repair-repository", "1h")
			b := env.setupNode("editor", "repair-repository", "1h")
			job, originalReview := createCompletedReview(t, a.DB, a.Repo.ID, "abc123", "Author", "Subject", "complete prompt", "Original review.")
			if conversion {
				_, err := a.DB.ExecContext(t.Context(), `UPDATE reviews SET structured_output = '{"schema_version":0,"legacy":{"markdown":"Historical finding.","recorded_verdict":null}}' WHERE id = ?`, originalReview.ID)
				require.NoError(t, err)
			}
			_, err := a.Worker.SyncNow()
			require.NoError(t, err)
			_, err = b.Worker.SyncNow()
			require.NoError(t, err)
			var importedID int64
			require.NoError(t, b.DB.QueryRowContext(t.Context(), `SELECT id FROM review_jobs WHERE uuid = ?`, job.UUID).Scan(&importedID))
			if conversion {
				var archiveID int64
				require.NoError(t, b.DB.QueryRowContext(t.Context(), `SELECT archive_id FROM legacy_reviews WHERE uuid = ?`, originalReview.UUID).Scan(&archiveID))
				require.NoError(t, b.DB.ResolveLegacyReview(archiveID, reviewFixtureJSON("Converted review.")))
			} else {
				require.NoError(t, b.DB.MarkReviewClosedByJobID(importedID, true))
			}
			editor, err := b.DB.GetMachineID()
			require.NoError(t, err)
			pendingEdit, err := b.DB.GetReviewsToSync(editor, 10)
			require.NoError(t, err)
			require.Len(t, pendingEdit, 1)
			// Older clients could upload a rerun's review while leaving its
			// parent's original enqueue time in PostgreSQL. The producer's
			// upgrade republishes the real attempt without changing review UUID.
			repaired := job.EnqueuedAt.Add(time.Hour).Truncate(time.Microsecond)
			_, err = a.DB.ExecContext(t.Context(), `UPDATE review_jobs SET enqueued_at = ?, synced_at = NULL WHERE id = ?`, preciseTimestampAt(repaired), job.ID)
			require.NoError(t, err)
			producer, err := a.DB.GetMachineID()
			require.NoError(t, err)
			pendingJobs, err := a.DB.GetJobsToSync(producer, 10)
			require.NoError(t, err)
			require.Len(t, pendingJobs, 1)
			var pgRepoID int64
			var pgCommitID *int64
			require.NoError(t, env.Pool.pool.QueryRow(t.Context(), `SELECT repo_id, commit_id FROM review_jobs WHERE uuid = $1`, job.UUID).Scan(&pgRepoID, &pgCommitID))
			require.NoError(t, env.Pool.UpsertJob(t.Context(), pendingJobs[0], pgRepoID, pgCommitID))
			// Pause the producer between its job and review uploads. The editor
			// must wait for the review association before pulling this parent.
			_, err = b.Worker.SyncNow()
			require.ErrorContains(t, err, "review attempt association is pending")
			pendingEdit, err = b.DB.GetReviewsToSync(editor, 10)
			require.NoError(t, err)
			require.Len(t, pendingEdit, 1)
			_, err = a.Worker.SyncNow()
			require.NoError(t, err)
			_, err = env.Pool.pool.Exec(t.Context(), `CREATE FUNCTION reject_pending_edit() RETURNS trigger LANGUAGE plpgsql AS $$
 BEGIN RAISE EXCEPTION 'synthetic edit failure'; END $$;
 CREATE TRIGGER reject_pending_edit BEFORE INSERT OR UPDATE ON reviews FOR EACH ROW EXECUTE FUNCTION reject_pending_edit()`)
			require.NoError(t, err)
			_, err = b.Worker.SyncNow()
			require.ErrorContains(t, err, "synthetic edit failure")
			pendingEdit, err = b.DB.GetReviewsToSync(editor, 10)
			require.NoError(t, err)
			require.Len(t, pendingEdit, 1, "a failed push must not pull away the pending edit")
			_, err = env.Pool.pool.Exec(t.Context(), `DROP TRIGGER reject_pending_edit ON reviews; DROP FUNCTION reject_pending_edit()`)
			require.NoError(t, err)
			_, err = b.Worker.SyncNow()
			require.NoError(t, err)
			_, err = a.Worker.SyncNow()
			require.NoError(t, err)
			for i, node := range []testNode{a, b} {
				id := []int64{job.ID, importedID}[i]
				currentJob, err := node.DB.GetJobByID(id)
				require.NoError(t, err)
				assert.Equal(t, repaired.UTC(), currentJob.EnqueuedAt.UTC())
				assert.Equal(t, job.SourceMachineID, currentJob.SourceMachineID)
				review, err := node.DB.GetReviewByJobID(id)
				require.NoError(t, err)
				assert.Equal(t, originalReview.UUID, review.UUID)
				if conversion {
					assert.Equal(t, "Converted review.", review.StructuredOutput["summary"])
					var unresolved int
					require.NoError(t, node.DB.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM legacy_reviews WHERE uuid = ? AND resolved_at IS NULL`, review.UUID).Scan(&unresolved))
					assert.Zero(t, unresolved)
				} else {
					assert.True(t, review.Closed)
				}
			}
			machine, err := b.DB.GetMachineID()
			require.NoError(t, err)
			pending, err := b.DB.GetReviewsToSync(machine, 10)
			require.NoError(t, err)
			assert.Empty(t, pending)
		})
	}
}

func TestIntegrationRejectedReviewUploadIsNotAcknowledged(t *testing.T) { //nolint:paralleltest // shares the PostgreSQL schema
	env, a, b, sourceID, importedID := importedAttemptNodes(t)
	machine, err := a.DB.GetMachineID()
	require.NoError(t, err)
	require.NoError(t, a.DB.MarkReviewClosedByJobID(sourceID, true))
	pending, err := a.DB.GetReviewsToSync(machine, 10)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	startImportedAttempt(t, b, importedID, "new attempt")
	require.NoError(t, completeReviewFixture(b.DB, importedID, "test", "new attempt", "New review."))
	producer, err := b.DB.GetMachineID()
	require.NoError(t, err)
	jobs, err := b.DB.GetJobsToSync(producer, 10)
	require.NoError(t, err)
	require.Len(t, jobs, 1)
	var pgRepoID int64
	var pgCommitID *int64
	require.NoError(t, env.Pool.pool.QueryRow(t.Context(), `SELECT repo_id, commit_id FROM review_jobs WHERE uuid = $1`, jobs[0].UUID).Scan(&pgRepoID, &pgCommitID))
	require.NoError(t, env.Pool.UpsertJob(t.Context(), jobs[0], pgRepoID, pgCommitID))
	// Until the new review arrives, this is indistinguishable from a repair.
	// Once it arrives, the old UUID must be ignored instead of blocking sync.
	require.ErrorIs(t, env.Pool.UpsertReview(t.Context(), pending[0]), errReviewAttemptPending)
	_, err = b.Worker.SyncNow()
	require.NoError(t, err)
	success, err := env.Pool.BatchUpsertReviews(t.Context(), pending)
	require.NoError(t, err)
	assert.Equal(t, []bool{false}, success)
}

func TestIntegrationParentUploadPreservesPeerEditWithClockSkew(t *testing.T) { //nolint:paralleltest // shares the PostgreSQL schema
	env, a, b, sourceID, importedID := importedAttemptNodes(t)
	// PostgreSQL can be ahead of both clients. Keep its timestamps advancing
	// normally while preserving that offset through every review write.
	_, err := env.Pool.pool.Exec(t.Context(), `CREATE FUNCTION future_review_timestamp() RETURNS trigger LANGUAGE plpgsql AS $$
 BEGIN NEW.updated_at = clock_timestamp() + interval '1 hour'; RETURN NEW; END $$;
 CREATE TRIGGER future_review_timestamp BEFORE INSERT OR UPDATE ON reviews FOR EACH ROW EXECUTE FUNCTION future_review_timestamp()`)
	require.NoError(t, err)
	require.NoError(t, b.DB.MarkReviewClosedByJobID(importedID, true))
	_, err = b.Worker.SyncNow()
	require.NoError(t, err)
	_, err = a.Worker.SyncNow()
	require.NoError(t, err)
	cached, err := a.DB.GetReviewByJobID(sourceID)
	require.NoError(t, err)
	require.True(t, cached.Closed)
	require.NoError(t, b.DB.MarkReviewClosedByJobID(importedID, false))
	_, err = b.Worker.SyncNow()
	require.NoError(t, err)
	// A parent upload must repair only the review association, even though
	// the cached review's server timestamp is later than its local sync mark.
	_, err = a.DB.ExecContext(t.Context(), `UPDATE review_jobs SET synced_at = NULL WHERE id = ?`, sourceID)
	require.NoError(t, err)
	_, err = a.Worker.SyncNow()
	require.NoError(t, err)
	var remoteClosed bool
	require.NoError(t, env.Pool.pool.QueryRow(t.Context(), `SELECT closed FROM reviews WHERE uuid = $1`, cached.UUID).Scan(&remoteClosed))
	assert.False(t, remoteClosed, "the parent's cached closure must not overwrite the peer's reopen")
	current, err := a.DB.GetReviewByJobID(sourceID)
	require.NoError(t, err)
	assert.False(t, current.Closed)
}

func TestIntegrationReviewEditAfterPushSurvivesRepairPull(t *testing.T) { //nolint:paralleltest // shares the PostgreSQL schema
	for _, conversion := range []bool{false, true} {
		t.Run(map[bool]string{false: "closure", true: "conversion"}[conversion], func(t *testing.T) {
			env := newIntegrationEnv(t)
			a := env.setupNode("producer", "late-edit-repository", "1h")
			b := env.setupNode("editor", "late-edit-repository", "1h")
			job, review := createCompletedReview(t, a.DB, a.Repo.ID, "abc123", "Author", "Subject", "prompt", "Original review.")
			if conversion {
				_, err := a.DB.ExecContext(t.Context(), `UPDATE reviews SET structured_output = '{"schema_version":0,"legacy":{"markdown":"Historical finding.","recorded_verdict":null}}' WHERE id = ?`, review.ID)
				require.NoError(t, err)
			}
			_, err := a.Worker.SyncNow()
			require.NoError(t, err)
			_, err = b.Worker.SyncNow()
			require.NoError(t, err)
			var importedID int64
			require.NoError(t, b.DB.QueryRowContext(t.Context(), `SELECT id FROM review_jobs WHERE uuid = ?`, job.UUID).Scan(&importedID))
			_, err = a.DB.ExecContext(t.Context(), `UPDATE review_jobs SET enqueued_at = ?, synced_at = NULL WHERE id = ?`, preciseTimestampAt(job.EnqueuedAt.Add(time.Hour)), job.ID)
			require.NoError(t, err)
			_, err = a.Worker.SyncNow()
			require.NoError(t, err)
			pushed, err := b.Worker.pushChangesWithStats(t.Context(), env.Pool)
			require.NoError(t, err)
			require.Zero(t, pushed.Reviews)
			// The user edits after the final upload query, before the parent pull.
			if conversion {
				var archiveID int64
				require.NoError(t, b.DB.QueryRowContext(t.Context(), `SELECT archive_id FROM legacy_reviews WHERE uuid = ?`, review.UUID).Scan(&archiveID))
				require.NoError(t, b.DB.ResolveLegacyReview(archiveID, reviewFixtureJSON("Converted review.")))
			} else {
				require.NoError(t, b.DB.MarkReviewClosedByJobID(importedID, true))
			}
			_, err = b.Worker.pullChangesWithStats(t.Context(), env.Pool)
			require.NoError(t, err)
			machine, err := b.DB.GetMachineID()
			require.NoError(t, err)
			pending, err := b.DB.GetReviewsToSync(machine, 10)
			require.NoError(t, err)
			require.Len(t, pending, 1, "the parent and review pull must retain the pending edit")
			assert.False(t, pending[0].MetadataOnly)
			_, err = b.Worker.SyncNow()
			require.NoError(t, err)
			_, err = a.Worker.SyncNow()
			require.NoError(t, err)
			current, err := a.DB.GetReviewByJobID(job.ID)
			require.NoError(t, err)
			if conversion {
				assert.Equal(t, "Converted review.", current.StructuredOutput["summary"])
			} else {
				assert.True(t, current.Closed)
			}
		})
	}
}

func TestIntegrationReviewEditBeyondPushCapSurvivesRepairPull(t *testing.T) { //nolint:paralleltest // shares the PostgreSQL schema
	env := newIntegrationEnv(t)
	a := env.setupNode("producer", "push-cap-repository", "1h")
	b := env.setupNode("editor", "push-cap-repository", "1h")
	// SyncNow caps uploads at 20 batches of 25. Leave one pending review
	// beyond that cap when the editor starts pulling repaired parents.
	for i := range 501 {
		createCompletedReview(t, a.DB, a.Repo.ID, fmt.Sprintf("review-%d", i), "Author", "Subject", "prompt", "Original review.")
	}
	for range 2 {
		_, err := a.Worker.SyncNow()
		require.NoError(t, err)
	}
	_, err := b.Worker.SyncNow()
	require.NoError(t, err)
	_, err = a.DB.ExecContext(t.Context(), `UPDATE review_jobs SET enqueued_at = datetime(enqueued_at, '+1 hour'), synced_at = NULL`)
	require.NoError(t, err)
	for range 2 {
		_, err := a.Worker.SyncNow()
		require.NoError(t, err)
	}
	jobs, err := b.DB.ListJobs("", "", 1000, 0)
	require.NoError(t, err)
	require.Len(t, jobs, 501)
	for _, job := range jobs {
		require.NoError(t, b.DB.MarkReviewClosedByJobID(job.ID, true))
	}
	stats, err := b.Worker.SyncNow()
	require.NoError(t, err)
	assert.Equal(t, 500, stats.PushedReviews)
	var closed int
	require.NoError(t, b.DB.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM reviews WHERE closed = 1`).Scan(&closed))
	assert.Equal(t, 501, closed)
	machine, err := b.DB.GetMachineID()
	require.NoError(t, err)
	pending, err := b.DB.GetReviewsToSync(machine, 10)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	_, err = b.Worker.SyncNow()
	require.NoError(t, err)
	require.NoError(t, env.Pool.pool.QueryRow(t.Context(), `SELECT COUNT(*) FROM reviews WHERE closed`).Scan(&closed))
	assert.Equal(t, 501, closed)
}

func TestIntegrationLegacyClosureMergesWithPeerConversion(t *testing.T) { //nolint:paralleltest // shares the PostgreSQL schema
	for _, ownerCloses := range []bool{false, true} {
		t.Run(map[bool]string{false: "peer closes", true: "owner closes"}[ownerCloses], func(t *testing.T) {
			env := newIntegrationEnv(t)
			a := env.setupNode("producer", "conversion-repository", "1h")
			b := env.setupNode("peer", "conversion-repository", "1h")
			job, review := createCompletedReview(t, a.DB, a.Repo.ID, "abc123", "Author", "Subject", "prompt", "Original review.")
			_, err := a.DB.ExecContext(t.Context(), `UPDATE reviews SET structured_output = '{"schema_version":0,"legacy":{"markdown":"Historical finding.","recorded_verdict":null}}' WHERE id = ?`, review.ID)
			require.NoError(t, err)
			_, err = a.Worker.SyncNow()
			require.NoError(t, err)
			_, err = b.Worker.SyncNow()
			require.NoError(t, err)
			converter, closer := a, b
			if ownerCloses {
				converter, closer = b, a
			}
			var convertedID, closedJobID int64
			require.NoError(t, converter.DB.QueryRowContext(t.Context(), `SELECT id FROM reviews WHERE uuid = ?`, review.UUID).Scan(&convertedID))
			require.NoError(t, closer.DB.QueryRowContext(t.Context(), `SELECT id FROM review_jobs WHERE uuid = ?`, job.UUID).Scan(&closedJobID))
			require.NoError(t, converter.DB.MigrateReview(convertedID, reviewFixtureJSON(`{"schema_version":2,"summary":"Converted review.","verdict":"pass","findings":[]}`)))
			_, err = converter.DB.ExecContext(t.Context(), `UPDATE reviews SET reviewed_file_count = 3, excluded_file_count = 2 WHERE id = ?`, convertedID)
			require.NoError(t, err)
			_, err = converter.Worker.SyncNow()
			require.NoError(t, err)
			// The closer still has the legacy envelope, and edits it without
			// first pulling the conversion. Its closure must reach both nodes.
			require.NoError(t, closer.DB.MarkReviewClosedByJobID(closedJobID, true))
			_, err = closer.Worker.SyncNow()
			require.NoError(t, err)
			_, err = converter.Worker.SyncNow()
			require.NoError(t, err)
			for _, node := range []testNode{a, b} {
				var id int64
				require.NoError(t, node.DB.QueryRowContext(t.Context(), `SELECT id FROM review_jobs WHERE uuid = ?`, job.UUID).Scan(&id))
				current, err := node.DB.GetReviewByJobID(id)
				require.NoError(t, err)
				assert.True(t, current.Closed)
				assert.Equal(t, "Converted review.", current.StructuredOutput["summary"])
				assert.Equal(t, VerdictPass, current.Verdict())
				var reviewed, excluded int
				require.NoError(t, node.DB.QueryRowContext(t.Context(), `SELECT reviewed_file_count, excluded_file_count FROM reviews WHERE id = ?`, current.ID).Scan(&reviewed, &excluded))
				assert.Equal(t, 3, reviewed)
				assert.Equal(t, 2, excluded)
				machine, err := node.DB.GetMachineID()
				require.NoError(t, err)
				pending, err := node.DB.GetReviewsToSync(machine, 10)
				require.NoError(t, err)
				assert.Empty(t, pending)
			}
		})
	}
}

func TestIntegrationMirrorResetRestoresRepairedReview(t *testing.T) { //nolint:paralleltest // shares the PostgreSQL schema
	env, a, b, sourceID, importedID := importedAttemptNodes(t)
	require.NoError(t, b.DB.MarkReviewClosedByJobID(importedID, true))
	_, err := b.Worker.SyncNow()
	require.NoError(t, err)
	_, err = a.Worker.SyncNow()
	require.NoError(t, err)
	// A parent repush produces a mirror-authored review association repair.
	_, err = a.DB.ExecContext(t.Context(), `UPDATE review_jobs SET synced_at = NULL WHERE id = ?`, sourceID)
	require.NoError(t, err)
	_, err = a.Worker.SyncNow()
	require.NoError(t, err)
	before, err := a.DB.GetReviewByJobID(sourceID)
	require.NoError(t, err)
	owner, err := a.DB.GetMachineID()
	require.NoError(t, err)
	peer, err := b.DB.GetMachineID()
	require.NoError(t, err)
	var editor uuid.UUID
	require.NoError(t, a.DB.QueryRowContext(t.Context(), `SELECT updated_by_machine_id FROM reviews WHERE id = ?`, before.ID).Scan(&editor))
	require.NotEqual(t, owner, editor)
	require.NotEqual(t, peer, editor)
	a.Worker.Stop()
	b.Worker.Stop()
	// Reconnecting to an empty mirror clears the acknowledgments and
	// cursors. The owner must restore this review despite its last editor.
	_, err = env.Pool.pool.Exec(t.Context(), `DROP SCHEMA roborev CASCADE`)
	require.NoError(t, err)
	require.NoError(t, env.Pool.EnsureSchema(t.Context()))
	worker := startSyncWorker(t, a.DB, env.pgURL, "restored-owner", "1h")
	_, err = worker.SyncNow()
	require.NoError(t, err)
	var closed bool
	var document string
	require.NoError(t, env.Pool.pool.QueryRow(t.Context(), `SELECT closed, structured_output->>'summary' FROM reviews WHERE uuid = $1`, before.UUID).Scan(&closed, &document))
	assert.True(t, closed)
	assert.Equal(t, "Original review.", document)
	pending, err := a.DB.GetReviewsToSync(owner, 10)
	require.NoError(t, err)
	assert.Empty(t, pending)
}
