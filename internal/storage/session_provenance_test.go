package storage

import (
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/provenance"
)

func provenanceFixture() []provenance.Commit {
	return []provenance.Commit{{CommitSHA: "0123456789abcdef0123456789abcdef01234567", Coverage: "trailers", Sessions: []provenance.Session{{Agent: "pi", SessionID: "pi:writer", Source: "trailer", Availability: "unknown", Resolved: true}}}}
}

func TestSessionProvenanceAtomicCapture(t *testing.T) {
	t.Parallel()
	env := setupJobEnv(t, t.TempDir(), "capture")
	job := claimJob(t, env.db, "worker")
	job.SessionProvenance = provenanceFixture()
	saved, err := env.db.SaveJobPromptForAttempt(job, "worker", "first prompt")
	require.NoError(t, err)
	require.True(t, saved)
	loaded, err := env.db.GetJobByID(job.ID)
	require.NoError(t, err)
	assert.Equal(t, job.SessionProvenance, loaded.SessionProvenance)
	jobs, err := env.db.ListJobs("", "", 10, 0, WithoutPrompt())
	require.NoError(t, err)
	require.Len(t, jobs, 1)
	assert.Empty(t, jobs[0].SessionProvenance)
	_, err = env.db.Exec(`CREATE TRIGGER reject_capture BEFORE UPDATE OF session_provenance ON job_content BEGIN SELECT RAISE(ABORT,'reject capture'); END`)
	require.NoError(t, err)
	job.SessionProvenance = nil
	_, err = env.db.SaveJobPromptForAttempt(job, "worker", "replacement prompt")
	require.Error(t, err)
	loaded, err = env.db.GetJobByID(job.ID)
	require.NoError(t, err)
	assert.Equal(t, "first prompt", loaded.Prompt)
	assert.Equal(t, provenanceFixture(), loaded.SessionProvenance)
	saved, err = env.db.SaveJobPromptForAttempt(job, "obsolete-worker", "stale prompt")
	require.NoError(t, err)
	assert.False(t, saved)
}

func TestSessionProvenanceReset(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"retry", "failover", "restart", "update", "rerun", "cancel"} {
		t.Run(kind, func(t *testing.T) {
			env := setupJobEnv(t, t.TempDir(), kind)
			job := claimJob(t, env.db, "worker")
			job.SessionProvenance = provenanceFixture()
			saved, err := env.db.SaveJobPromptForAttempt(job, "worker", "prompt")
			require.NoError(t, err)
			require.True(t, saved)
			switch kind {
			case "retry":
				_, err = env.db.RetryJob(job.ID, "worker", 3, 0)
			case "failover":
				_, err = env.db.FailoverJob(job.ID, "worker", "test", "backup")
			case "restart":
				err = env.db.ResetStaleJobs()
			case "update":
				_, err = env.db.RequeueUpdateInterruptedJob(job.ID, "worker")
			case "rerun":
				require.NoError(t, env.db.CancelJob(job.ID))
				_, releaseErr := env.db.ReleaseCanceledJob(job.ID, "worker")
				require.NoError(t, releaseErr)
				err = env.db.ReenqueueJob(job.ID, ReenqueueOpts{})
			case "cancel":
				err = env.db.CancelJob(job.ID)
			}
			require.NoError(t, err)
			loaded, err := env.db.GetJobByID(job.ID)
			require.NoError(t, err)
			if kind == "cancel" {
				assert.Equal(t, provenanceFixture(), loaded.SessionProvenance)
			} else {
				assert.Empty(t, loaded.SessionProvenance)
			}
		})
	}
}

func TestSessionProvenancePullPresenceAndAttempt(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	defer db.Close()
	repo := createRepo(t, db, t.TempDir())
	raw, err := json.Marshal(provenanceFixture())
	require.NoError(t, err)
	incoming := PulledJob{UUID: testUUID("provenance"), GitRef: "head", Agent: "test", JobType: JobTypeReview, Status: "done", SourceMachineID: testUUID("owner"), EnqueuedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), UpdatedAt: time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC), SessionProvenance: string(raw), Prompt: "capture"}
	require.NoError(t, db.UpsertPulledJob(incoming, repo.ID, nil))
	var id int64
	require.NoError(t, db.QueryRow(`SELECT id FROM review_jobs WHERE uuid=?`, incoming.UUID).Scan(&id))
	check := func(want []provenance.Commit) {
		got, err := db.GetJobByID(id)
		require.NoError(t, err)
		assert.Equal(t, want, got.SessionProvenance)
	}
	check(provenanceFixture())
	older := incoming
	incoming.SessionProvenance = ""
	require.NoError(t, db.UpsertPulledJob(incoming, repo.ID, nil))
	check(provenanceFixture())
	incoming.SessionProvenance = "[]"
	require.NoError(t, db.UpsertPulledJob(incoming, repo.ID, nil))
	check(nil)
	incoming.SessionProvenance = string(raw)
	require.NoError(t, db.UpsertPulledJob(incoming, repo.ID, nil))
	check(provenanceFixture())
	incoming.SessionProvenance = ""
	incoming.EnqueuedAt = incoming.EnqueuedAt.Add(time.Hour)
	require.NoError(t, db.UpsertPulledJob(incoming, repo.ID, nil))
	check(nil)
	require.NoError(t, db.UpsertPulledJob(older, repo.ID, nil))
	check(nil)
}

func TestSessionProvenanceSurvivesPromptPruning(t *testing.T) {
	t.Parallel()
	env := setupJobEnv(t, t.TempDir(), "retention")
	job := claimJob(t, env.db, "worker")
	job.SessionProvenance = provenanceFixture()
	saved, err := env.db.SaveJobPromptForAttempt(job, "worker", "full prompt")
	require.NoError(t, err)
	require.True(t, saved)
	require.NoError(t, env.db.CompleteJobResult(job.ID, "test", ReviewCompletion{StructuredOutput: reviewFixtureJSON("No issues found.")}))
	_, err = env.db.PruneJobPrompts(t.Context(), time.Now().Add(time.Hour), false)
	require.NoError(t, err)
	loaded, err := env.db.GetJobByID(job.ID)
	require.NoError(t, err)
	assert.Empty(t, loaded.Prompt)
	assert.Equal(t, provenanceFixture(), loaded.SessionProvenance)
	review, err := env.db.GetReviewByJobID(job.ID)
	require.NoError(t, err)
	assert.Equal(t, provenanceFixture(), review.Job.SessionProvenance)
}
