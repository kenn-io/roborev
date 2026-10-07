package storage

import (
	"database/sql"
	"path/filepath"
	"testing"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsolatedSessionsExcludedFromReuse(t *testing.T) {
	t.Parallel()
	for _, panel := range []bool{false, true} {
		for _, backfill := range []bool{false, true} {
			name := "standalone"
			if panel {
				name = "panel"
			}
			if backfill {
				name += "/backfill"
			} else {
				name += "/capture"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				assert := assert.New(t)
				dbPath := filepath.Join(t.TempDir(), "reviews.db")
				db, err := Open(dbPath)
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, db.Close()) })
				repo, err := db.GetOrCreateRepo(filepath.Join(t.TempDir(), "repo"))
				require.NoError(t, err)
				machine, err := db.GetMachineID()
				require.NoError(t, err)
				opts := EnqueueOpts{RepoID: repo.ID, GitRef: "abc123", Branch: "feature/session", Agent: "test"}
				query := ReusableSessionQuery{RepoID: repo.ID, Branch: opts.Branch, Agent: "test", Reasoning: "thorough", SourceMachineID: machine}
				if panel {
					run := uuid.New()
					opts.PanelRunUUID = &run
					opts.PanelRole = PanelRoleMember
					opts.PanelName = "panel"
					opts.PanelMemberName = "reviewer"
					query.PanelName = opts.PanelName
					query.PanelMemberName = opts.PanelMemberName
				}
				normal, err := db.EnqueueJob(opts)
				require.NoError(t, err)
				_, err = db.ClaimJob("worker")
				require.NoError(t, err)
				require.NoError(t, db.SaveJobSessionID(normal.ID, "worker", "normal-session"))
				require.NoError(t, completeReviewFixture(db, normal.ID, "test", "prompt", "No issues found."))

				isolated, err := db.EnqueueJob(opts)
				require.NoError(t, err)
				claimed, err := db.ClaimJob("worker")
				require.NoError(t, err)
				require.Equal(t, isolated.ID, claimed.ID)
				require.NoError(t, db.IsolateJobSession(isolated.ID, "worker"))
				if !backfill {
					require.NoError(t, db.SaveJobSessionID(isolated.ID, "worker", "isolated-session"))
				}
				require.NoError(t, completeReviewFixture(db, isolated.ID, "test", "prompt", "No issues found."))
				if backfill {
					written, err := db.BackfillJobTokenUsageIfCurrent(TokenUsageWrite{
						JobID: isolated.ID, SessionID: "isolated-session",
						TokenUsageJSON: `{"input":1}`, ExpectedStartedAt: claimed.StartedAtRaw,
					})
					require.NoError(t, err)
					require.True(t, written)
				}
				require.NoError(t, db.Close())
				db, err = Open(dbPath)
				require.NoError(t, err)
				stored, err := db.GetJobByID(isolated.ID)
				require.NoError(t, err)
				assert.Equal("isolated-session", stored.SessionID, "retain identity for usage accounting")
				candidates, err := db.FindCompatibleReusableSessionCandidates(query)
				require.NoError(t, err)
				require.Len(t, candidates, 1, "exclude the disposable source after reopening the database")
				assert.Equal(normal.ID, candidates[0].ID)
				if !panel {
					legacy, err := db.FindReusableSessionCandidates(repo.ID, opts.Branch, "test", "", "", 1)
					require.NoError(t, err)
					require.Len(t, legacy, 1)
					assert.Equal(normal.ID, legacy[0].ID)
				}

				require.NoError(t, db.ReenqueueJob(isolated.ID, ReenqueueOpts{}))
				retried, err := db.ClaimJob("worker")
				require.NoError(t, err)
				assert.NotEqual(claimed.StartedAtRaw, retried.StartedAtRaw)
				require.NoError(t, db.SaveJobSessionID(isolated.ID, "worker", "later-normal-session"))
				require.NoError(t, completeReviewFixture(db, isolated.ID, "test", "prompt", "No issues found."))
				candidates, err = db.FindCompatibleReusableSessionCandidates(query)
				require.NoError(t, err)
				require.Len(t, candidates, 2, "a later non-isolated attempt remains reusable")
				assert.Equal(isolated.ID, candidates[0].ID)
				assert.Equal("later-normal-session", candidates[0].SessionID)
			})
		}
	}
}

func TestSessionIsolationMigration(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "reviews.db")
	db, err := Open(dbPath)
	require.NoError(t, err)
	_, _, job := createJobChain(t, db, "/tmp/example-repo", "session-migration")
	claimJob(t, db, "worker")
	require.NoError(t, db.SaveJobSessionID(job.ID, "worker", "existing-session"))
	require.NoError(t, db.Close())

	raw, err := openRawDB(dbPath)
	require.NoError(t, err)
	_, err = raw.Exec("ALTER TABLE review_jobs DROP COLUMN session_isolated")
	require.NoError(t, err)
	require.NoError(t, raw.Close())

	db, err = Open(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	var isolated bool
	require.NoError(t, db.QueryRow("SELECT session_isolated FROM review_jobs WHERE id=?", job.ID).Scan(&isolated))
	assert.False(t, isolated, "migration preserves eligibility of existing sessions")
	stored, err := db.GetJobByID(job.ID)
	require.NoError(t, err)
	assert.Equal(t, "existing-session", stored.SessionID)
	require.NoError(t, db.IsolateJobSession(job.ID, "worker"))
	require.NoError(t, db.Close())

	db, err = Open(dbPath)
	require.NoError(t, err)
	require.NoError(t, db.QueryRow("SELECT session_isolated FROM review_jobs WHERE id=?", job.ID).Scan(&isolated))
	assert.True(t, isolated, "reopening preserves the attempt's isolation marker")
}

func TestClaimJobStartsFreshSessionWhileAnotherJobResumesIt(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	db := openTestDB(t)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	repo, err := db.GetOrCreateRepo(filepath.Join(t.TempDir(), "repo"))
	require.NoError(t, err)
	source := uuid.New()
	enqueue := func(gitRef string) *ReviewJob {
		job, err := db.EnqueueJob(EnqueueOpts{
			RepoID: repo.ID, GitRef: gitRef, Branch: "feature/session", Agent: "test",
			SessionID: "shared-thread", ResumeSourceJobUUID: &source,
		})
		require.NoError(t, err)
		return job
	}
	first := enqueue("aaa111")
	second := enqueue("bbb222")

	claimedFirst, err := db.ClaimJob("worker-1")
	require.NoError(t, err)
	require.Equal(t, first.ID, claimedFirst.ID)
	assert.Equal("shared-thread", claimedFirst.SessionID, "a queued job does not contest the session")
	assert.Equal(&source, claimedFirst.ResumeSourceJobUUID)

	claimedSecond, err := db.ClaimJob("worker-2")
	require.NoError(t, err)
	require.Equal(t, second.ID, claimedSecond.ID)
	assert.Empty(claimedSecond.SessionID, "a running job already resumes the session")
	assert.Nil(claimedSecond.ResumeSourceJobUUID)

	var sessionID sql.NullString
	var resumed, isolated bool
	var resumeSource sql.NullString
	require.NoError(t, db.QueryRow(`
		SELECT session_id, session_resumed, session_isolated, resume_source_job_uuid
		FROM review_jobs WHERE id = ?`, second.ID,
	).Scan(&sessionID, &resumed, &isolated, &resumeSource))
	assert.False(sessionID.Valid)
	assert.False(resumed)
	assert.False(isolated, "the fresh session stays eligible for later reuse")
	assert.False(resumeSource.Valid)

	require.NoError(t, db.SaveJobSessionID(second.ID, "worker-2", "fresh-thread"))
	stored, err := db.GetJobByID(second.ID)
	require.NoError(t, err)
	assert.Equal("fresh-thread", stored.SessionID)

	require.NoError(t, completeReviewFixture(db, first.ID, "test", "prompt", "No issues found."))
	third := enqueue("ccc333")
	claimedThird, err := db.ClaimJob("worker-1")
	require.NoError(t, err)
	require.Equal(t, third.ID, claimedThird.ID)
	assert.Equal("shared-thread", claimedThird.SessionID, "the session is free once its writer finishes")
}

func TestClaimJobTreatsCanceledJobAsWriterUntilWorkerReleasesIt(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	db := openTestDB(t)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	repo, err := db.GetOrCreateRepo(filepath.Join(t.TempDir(), "repo"))
	require.NoError(t, err)
	enqueue := func(gitRef string) *ReviewJob {
		job, err := db.EnqueueJob(EnqueueOpts{
			RepoID: repo.ID, GitRef: gitRef, Branch: "feature/session", Agent: "test",
			SessionID: "shared-thread",
		})
		require.NoError(t, err)
		return job
	}
	writer := enqueue("aaa111")
	claimedWriter, err := db.ClaimJob("worker-1")
	require.NoError(t, err)
	require.Equal(t, writer.ID, claimedWriter.ID)
	require.NoError(t, db.CancelJob(writer.ID))

	beforeRelease := enqueue("bbb222")
	claimed, err := db.ClaimJob("worker-2")
	require.NoError(t, err)
	require.Equal(t, beforeRelease.ID, claimed.ID)
	assert.Empty(claimed.SessionID, "the canceled agent may still be writing the session")

	released, err := db.ReleaseCanceledJob(writer.ID, "worker-1")
	require.NoError(t, err)
	require.True(t, released)
	require.NoError(t, db.CancelJob(beforeRelease.ID))
	_, err = db.ReleaseCanceledJob(beforeRelease.ID, "worker-2")
	require.NoError(t, err)

	afterRelease := enqueue("ccc333")
	claimed, err = db.ClaimJob("worker-3")
	require.NoError(t, err)
	require.Equal(t, afterRelease.ID, claimed.ID)
	assert.Equal("shared-thread", claimed.SessionID, "the worker released the canceled job")
}
