package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func kataTestSeed() KataPublicationSeed {
	return KataPublicationSeed{Endpoint: "https://kata.example.test/base", Project: "example", AliasKind: "git", AliasIdentity: "example.test/repo", Policy: "comments"}
}

func TestKataPublicationSeedSupportsAliasWithoutProjectName(t *testing.T) {
	t.Parallel()
	seed := kataTestSeed()
	seed.Project = ""
	encoded, err := newKataPublication(seed, reviewFixtureJSON("FAIL: finding"))
	require.NoError(t, err)
	var marker KataPublication
	require.NoError(t, json.Unmarshal([]byte(encoded), &marker))
	assert.Equal(t, seed, marker.KataPublicationSeed)
	seed.AliasIdentity = ""
	_, err = newKataPublication(seed, reviewFixtureJSON("FAIL: finding"))
	require.Error(t, err)
}

func TestKataPublicationMigrationIsRepeatable(t *testing.T) {
	t.Parallel()
	db := openUnmigratedDB(t)
	require.NoError(t, db.runSchemaMigrations(t.Context(), schemaMigrations[:16]))
	for range 2 {
		require.NoError(t, db.runSchemaMigrations(t.Context(), schemaMigrations))
		var count int
		require.NoError(t, db.QueryRowContext(t.Context(), `SELECT count(*) FROM pragma_table_info('reviews') WHERE name = 'kata_publication' AND type = 'TEXT'`).Scan(&count))
		assert.Equal(t, 1, count)
		_, err := db.ExecContext(t.Context(), `DELETE FROM schema_migrations WHERE version = 17`)
		require.NoError(t, err)
	}
}

func TestKataPublicationCompletionAndRollback(t *testing.T) {
	t.Parallel()
	env := setupJobEnv(t, "/tmp/kata-completion", "kata-completion")
	claimJob(t, env.db, "worker")
	seed := kataTestSeed()
	raw := reviewFixtureJSON("FAIL: synthetic finding")
	require.NoError(t, env.db.CompleteJobResult(env.job.ID, "test", ReviewCompletion{StructuredOutput: raw, KataPublication: &seed}))
	record, err := env.db.GetKataPublication(t.Context(), env.job.ID)
	require.NoError(t, err)
	require.NotNil(t, record.Marker)
	assert.Equal(t, seed, record.Marker.KataPublicationSeed)
	assert.Equal(t, "pending", record.Marker.State)
	assert.Equal(t, record.DocumentHash, record.Marker.DocumentHash)
	assert.Empty(t, record.Marker.ProjectUID)
	assert.Empty(t, record.Marker.RequestFingerprint)
	pending, err := env.db.PendingKataPublications(t.Context())
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, record.ReviewUUID, pending[0].ReviewUUID)
	review, err := env.db.GetReviewByJobID(env.job.ID)
	require.NoError(t, err)
	encoded, err := json.Marshal(review)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), "kata_publication")
	assert.NotContains(t, string(encoded), seed.Endpoint)
	page, err := env.db.ExportReviews(ExportReviewsOptions{Profile: ExportProfileContent, Limit: 10})
	require.NoError(t, err)
	require.Len(t, page.Reviews, 1)
	encoded, err = json.Marshal(page)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), seed.Endpoint)
	machineID, err := env.db.GetMachineID()
	require.NoError(t, err)
	require.NoError(t, env.db.MarkJobSynced(env.job.ID))
	toSync, err := env.db.GetReviewsToSync(machineID, 10)
	require.NoError(t, err)
	require.Len(t, toSync, 1)
	encoded, err = json.Marshal(toSync)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), seed.Endpoint)

	passed := enqueueJob(t, env.db, env.repo.ID, env.commit.ID, "passed")
	setJobStatus(t, env.db, passed.ID, JobStatusRunning)
	require.NoError(t, env.db.CompleteJobResult(passed.ID, "test", ReviewCompletion{StructuredOutput: reviewFixtureJSON("No issues found."), KataPublication: &seed}))
	passedRecord, err := env.db.GetKataPublication(t.Context(), passed.ID)
	require.NoError(t, err)
	assert.Nil(t, passedRecord.Marker)
	require.ErrorIs(t, env.db.AuthorizeKataPublication(t.Context(), passedRecord, seed), ErrReviewChanged)

	invalid := enqueueJob(t, env.db, env.repo.ID, env.commit.ID, "invalid-authorization")
	setJobStatus(t, env.db, invalid.ID, JobStatusRunning)
	invalidSeed := seed
	invalidSeed.Policy = "unexpected"
	require.NoError(t, env.db.CompleteJobResult(invalid.ID, "test", ReviewCompletion{StructuredOutput: raw, KataPublication: &invalidSeed}))
	invalidRecord, err := env.db.GetKataPublication(t.Context(), invalid.ID)
	require.NoError(t, err)
	assert.Nil(t, invalidRecord.Marker)

	canceled := enqueueJob(t, env.db, env.repo.ID, env.commit.ID, "canceled")
	setJobStatus(t, env.db, canceled.ID, JobStatusCanceled)
	require.NoError(t, env.db.CompleteJobResult(canceled.ID, "test", ReviewCompletion{StructuredOutput: raw, KataPublication: &seed}))
	_, err = env.db.GetKataPublication(t.Context(), canceled.ID)
	require.ErrorIs(t, err, sql.ErrNoRows)

	failed := enqueueJob(t, env.db, env.repo.ID, env.commit.ID, "rollback")
	setJobStatus(t, env.db, failed.ID, JobStatusRunning)
	_, err = env.db.ExecContext(t.Context(), `CREATE TRIGGER fail_kata_insert BEFORE INSERT ON reviews BEGIN SELECT RAISE(ABORT, 'injected failure'); END`)
	require.NoError(t, err)
	require.ErrorContains(t, env.db.CompleteJobResult(failed.ID, "test", ReviewCompletion{StructuredOutput: raw, KataPublication: &seed}), "injected failure")
	job, err := env.db.GetJobByID(failed.ID)
	require.NoError(t, err)
	assert.Equal(t, JobStatusRunning, job.Status)
	_, err = env.db.GetKataPublication(t.Context(), failed.ID)
	require.ErrorIs(t, err, sql.ErrNoRows)
}

func TestKataPublicationAuthorizationAndCompareAndSwap(t *testing.T) {
	t.Parallel()
	env := setupJobEnv(t, "/tmp/kata-cas", "kata-cas")
	claimJob(t, env.db, "worker")
	raw := reviewFixtureJSON("FAIL: finding")
	require.NoError(t, env.db.CompleteJobResult(env.job.ID, "test", ReviewCompletion{StructuredOutput: raw}))
	record, err := env.db.GetKataPublication(t.Context(), env.job.ID)
	require.NoError(t, err)
	assert.Nil(t, record.Marker)
	require.NoError(t, env.db.AuthorizeKataPublication(t.Context(), record, kataTestSeed()))
	require.ErrorIs(t, env.db.AuthorizeKataPublication(t.Context(), record, kataTestSeed()), ErrReviewChanged)
	record, err = env.db.GetKataPublicationByUUID(t.Context(), record.ReviewUUID)
	require.NoError(t, err)
	next := *record.Marker
	next.ProjectUID, next.RequestFingerprint = "project-uid", "request-hash"
	require.NoError(t, env.db.UpdateKataPublication(t.Context(), record, next))
	require.ErrorIs(t, env.db.UpdateKataPublication(t.Context(), record, next), ErrReviewChanged)
	record, err = env.db.GetKataPublication(t.Context(), env.job.ID)
	require.NoError(t, err)
	changed := *record.Marker
	changed.ProjectUID = "different"
	require.ErrorContains(t, env.db.UpdateKataPublication(t.Context(), record, changed), "invalid")
	changed = *record.Marker
	changed.State = "complete"
	require.NoError(t, env.db.UpdateKataPublication(t.Context(), record, changed))
	pending, err := env.db.PendingKataPublications(t.Context())
	require.NoError(t, err)
	assert.Empty(t, pending)

	// A same-UUID document edit invalidates both a stale update and a POST.
	_, err = env.db.ExecContext(t.Context(), `UPDATE reviews SET structured_output = ? WHERE job_id = ?`, string(reviewFixtureJSON("FAIL: edited")), env.job.ID)
	require.NoError(t, err)
	require.ErrorIs(t, env.db.UpdateKataPublication(t.Context(), record, changed), ErrReviewChanged)
	called := false
	err = env.db.RunKataPublication(t.Context(), record.ReviewUUID, record.DocumentHash, func(context.Context) error { called = true; return nil })
	require.ErrorIs(t, err, ErrReviewChanged)
	assert.False(t, called)
	require.NoError(t, env.db.ReenqueueJob(env.job.ID, ReenqueueOpts{}))
	_, err = env.db.GetKataPublicationByUUID(t.Context(), record.ReviewUUID)
	require.ErrorIs(t, err, sql.ErrNoRows)
}

func TestKataPublicationGuardBlocksMutationsAndAllowsMarkerUpdate(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		env := setupJobEnv(t, "/tmp/kata-guard", "kata-guard")
		claimJob(t, env.db, "worker")
		seed := kataTestSeed()
		require.NoError(t, env.db.CompleteJobResult(env.job.ID, "test", ReviewCompletion{StructuredOutput: reviewFixtureJSON("FAIL: finding"), KataPublication: &seed}))
		record, err := env.db.GetKataPublication(t.Context(), env.job.ID)
		require.NoError(t, err)
		entered, release := make(chan struct{}), make(chan struct{})
		guardDone := make(chan error, 1)
		go func() {
			guardDone <- env.db.RunKataPublication(t.Context(), record.ReviewUUID, record.DocumentHash, func(ctx context.Context) error {
				close(entered)
				<-release
				// No nested attempt reader: a waiting writer must not deadlock this update.
				next := *record.Marker
				next.ProjectUID, next.RequestFingerprint = "uid", "hash"
				return env.db.UpdateKataPublication(ctx, record, next)
			})
		}()
		<-entered
		deleteDone := make(chan error, 1)
		go func() { deleteDone <- env.db.DeleteRepoContext(t.Context(), env.repo.ID, true) }()
		synctest.Wait()
		close(release)
		require.NoError(t, <-guardDone)
		require.NoError(t, <-deleteDone)
		err = env.db.RunKataPublication(t.Context(), record.ReviewUUID, record.DocumentHash, func(context.Context) error { return errors.New("unexpected callback") })
		require.ErrorIs(t, err, ErrReviewChanged)
	})
}

func TestKataReviewPullAcquiresAttemptLockBeforeTransaction(t *testing.T) {
	t.Parallel()
	h := newSyncTestHelper(t)
	s, err := h.db.newPullSession(t.Context())
	require.NoError(t, err)
	defer s.close()
	h.db.attempts.mu.RLock()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, NewSyncWorker(h.db, testSyncConfig()).pullReview(ctx, s, PulledReview{}), context.Canceled)
	assert.True(t, s.started.IsZero())
	h.db.attempts.mu.RUnlock()
	// Unlocked batches must finish before acquiring the attempt lock.
	require.NoError(t, s.begin(t.Context(), false))
	s.budget = time.Hour
	require.NoError(t, s.begin(t.Context(), true))
	assert.True(t, s.jobs)
	require.NoError(t, s.commit(t.Context()))
}

func TestKataPublicationPullKeepsAuthorizationLocalAndGuardsEdits(t *testing.T) {
	t.Parallel()
	for _, batched := range []bool{false, true} {
		t.Run(map[bool]string{false: "single", true: "batched"}[batched], func(t *testing.T) {
			h := newSyncTestHelper(t)
			job := h.createPendingJob("publication-pull")
			setJobStatus(t, h.db, job.ID, JobStatusRunning)
			seed := kataTestSeed()
			require.NoError(t, h.db.CompleteJobResult(job.ID, "test", ReviewCompletion{StructuredOutput: reviewFixtureJSON("FAIL: first"), KataPublication: &seed}))
			record, err := h.db.GetKataPublication(t.Context(), job.ID)
			require.NoError(t, err)
			peer := testUUID("publication-peer")
			_, err = h.db.ExecContext(t.Context(), `UPDATE review_jobs SET source_machine_id = ? WHERE id = ?`, peer, job.ID)
			require.NoError(t, err)
			_, err = h.db.ExecContext(t.Context(), `UPDATE reviews SET synced_at = '2020-01-01T00:00:00Z', updated_at = '2020-01-01T00:00:00Z' WHERE job_id = ?`, job.ID)
			require.NoError(t, err)
			now := time.Now().UTC().Add(time.Hour)
			pulled := PulledReview{UUID: record.ReviewUUID, JobUUID: *job.UUID, Agent: "test", JobEnqueuedAt: storedJobEnqueuedAt(t, h.db, job.ID), JobSourceMachineID: peer, StructuredOutput: reviewFixtureJSON("FAIL: changed"), UpdatedByMachineID: peer, CreatedAt: now, UpdatedAt: now}
			pull := func(r PulledReview) {
				t.Helper()
				if !batched {
					require.NoError(t, h.db.UpsertPulledReview(r))
					return
				}
				s, err := h.db.newPullSession(t.Context())
				require.NoError(t, err)
				defer s.close()
				require.NoError(t, NewSyncWorker(h.db, testSyncConfig()).pullReview(t.Context(), s, r))
				require.NoError(t, s.commit(t.Context()))
			}
			pull(pulled)
			edited, err := h.db.GetKataPublication(t.Context(), job.ID)
			require.NoError(t, err)
			require.NotNil(t, edited.Marker)
			assert.NotEqual(t, edited.DocumentHash, edited.Marker.DocumentHash)
			require.ErrorIs(t, h.db.RunKataPublication(t.Context(), record.ReviewUUID, record.DocumentHash, func(context.Context) error { return nil }), ErrReviewChanged)
			// A replacement review never inherits the old local authorization.
			pulled.UUID = testUUID("replacement-review")
			pulled.CreatedAt = now.Add(time.Hour)
			pulled.UpdatedAt = pulled.CreatedAt
			pull(pulled)
			replacement, err := h.db.GetKataPublication(t.Context(), job.ID)
			require.NoError(t, err)
			assert.Equal(t, pulled.UUID, replacement.ReviewUUID)
			assert.Nil(t, replacement.Marker)
		})
	}
}

func TestKataBothReviewPullPathsWaitForPublication(t *testing.T) {
	t.Parallel()
	for _, batched := range []bool{false, true} {
		t.Run(map[bool]string{false: "single", true: "batched"}[batched], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newSyncTestHelper(t)
				s, err := h.db.newPullSession(t.Context())
				require.NoError(t, err)
				defer s.close()
				h.db.attempts.mu.RLock()
				done := make(chan error, 1)
				go func() {
					if batched {
						done <- NewSyncWorker(h.db, testSyncConfig()).pullReview(t.Context(), s, PulledReview{})
						return
					}
					done <- h.db.UpsertPulledReview(PulledReview{})
				}()
				synctest.Wait()
				assert.Empty(t, done, "review pull must wait for the publication guard")
				h.db.attempts.mu.RUnlock()
				require.NoError(t, <-done)
				require.NoError(t, s.commit(t.Context()))
			})
		})
	}
}

func TestKataDocumentHashPreservesStoredSchema(t *testing.T) {
	first := []byte(`{"schema_version":1,"summary":"s","findings":[{"severity":"low","problem":"p","fix":"f","location":"a.go:3"}]}`)
	reordered := []byte(`{ "findings": [{"location":"a.go:3", "fix":"f", "problem":"p", "severity":"low"}], "summary":"s", "schema_version":1 }`)
	a, err := KataDocumentHash(first)
	require.NoError(t, err)
	b, err := KataDocumentHash(reordered)
	require.NoError(t, err)
	assert.Equal(t, a, b)
	other, err := KataDocumentHash([]byte(`{"schema_version":2,"summary":"s","verdict":"fail","findings":[{"severity":"low","problem":"p","fix":"f","location":null}]}`))
	require.NoError(t, err)
	assert.NotEqual(t, a, other)
	_, err = KataDocumentHash([]byte(`{"schema_version":1`))
	require.Error(t, err)
}

func TestPendingKataPublicationsSkipsExpired(t *testing.T) {
	env := setupJobEnv(t, "/tmp/kata-expiry", "kata-expiry")
	claimJob(t, env.db, "worker")
	seed := kataTestSeed()
	require.NoError(t, env.db.CompleteJobResult(env.job.ID, "test", ReviewCompletion{StructuredOutput: reviewFixtureJSON("FAIL: finding"), KataPublication: &seed}))
	_, err := env.db.ExecContext(t.Context(), `UPDATE reviews SET created_at = datetime('now', '-8 days') WHERE job_id = ?`, env.job.ID)
	require.NoError(t, err)
	pending, err := env.db.PendingKataPublications(t.Context())
	require.NoError(t, err)
	assert.Empty(t, pending)
	_, err = env.db.ExecContext(t.Context(), `UPDATE reviews SET created_at = datetime('now', '-6 days') WHERE job_id = ?`, env.job.ID)
	require.NoError(t, err)
	pending, err = env.db.PendingKataPublications(t.Context())
	require.NoError(t, err)
	require.Len(t, pending, 1)
}
