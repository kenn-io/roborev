package storage

import (
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
				require.NoError(t, db.ClearJobSession(isolated.ID, "worker"))
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

func TestClearJobSessionRollsBackWhenProvenanceFails(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	db := openTestDB(t)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	_, _, job := createJobChain(t, db, "/tmp/example-repo", "session-provenance")
	claimJob(t, db, "worker")
	source := uuid.New()
	_, err := db.Exec(`UPDATE review_jobs SET session_id='prior-session', session_resumed=1, resume_source_job_uuid=? WHERE id=?`, source, job.ID)
	require.NoError(t, err)
	_, err = db.Exec(`CREATE TRIGGER reject_provenance BEFORE INSERT ON daemon_state
		BEGIN SELECT RAISE(ABORT, 'synthetic provenance failure'); END`)
	require.NoError(t, err)
	require.ErrorContains(t, db.ClearJobSession(job.ID, "worker"), "synthetic provenance failure")
	got, err := db.GetJobByID(job.ID)
	require.NoError(t, err)
	assert.Equal("prior-session", got.SessionID)
	assert.Equal(&source, got.ResumeSourceJobUUID)
	var resumed int
	require.NoError(t, db.QueryRow("SELECT session_resumed FROM review_jobs WHERE id=?", job.ID).Scan(&resumed))
	assert.Equal(1, resumed)
}
