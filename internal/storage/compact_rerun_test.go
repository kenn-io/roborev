package storage

import (
	"database/sql"
	"encoding/json/jsontext"
	"testing"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAccountableCompactRerunGuardIsAtomic(t *testing.T) {
	t.Parallel()
	for _, closed := range []bool{false, true} {
		t.Run(map[bool]string{false: "source_open", true: "source_closed"}[closed], func(t *testing.T) {
			db := openTestDB(t)
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			repo, err := db.GetOrCreateRepo("/tmp/compact-rerun-fixture")
			require.NoError(t, err)
			source, err := db.EnqueueJob(EnqueueOpts{RepoID: repo.ID, GitRef: "source", Agent: "test"})
			require.NoError(t, err)
			_, err = db.ClaimJob("worker")
			require.NoError(t, err)
			raw := jsontext.Value(`{"schema_version":3,"summary":"Source","verdict":"pass","findings":[]}`)
			require.NoError(t, db.CompleteJobResult(source.ID, "test", ReviewCompletion{StructuredOutput: raw}))
			require.NoError(t, db.MarkReviewClosedByJobID(source.ID, closed))
			job, err := db.EnqueueJob(EnqueueOpts{RepoID: repo.ID, GitRef: "compact", Agent: "test", JobType: JobTypeCompact, Prompt: CompactPromptPrefix + "source snapshot"})
			require.NoError(t, err)
			_, err = db.ClaimJob("worker")
			require.NoError(t, err)
			raw = jsontext.Value(`{"schema_version":3,"summary":"<!-- roborev:compact-audit:v1 -->\nHistorical originals and decisions","verdict":"fail","findings":[{"severity":"low","problem":"Retained original","fix":"Preserve legitimate behavior"}]}`)
			require.NoError(t, db.CompleteJobResult(job.ID, "test", ReviewCompletion{StructuredOutput: raw}))
			before, err := db.GetReviewByJobID(job.ID)
			require.NoError(t, err)
			beforeJob, err := db.GetJobByID(job.ID)
			require.NoError(t, err)
			beforeSource, err := db.GetReviewByJobID(source.ID)
			require.NoError(t, err)
			requestID := uuid.New()
			_, _, err = db.ReenqueueJobWithRequest(job.ID, ReenqueueOpts{Agent: "test"}, requestID)
			require.ErrorIs(t, err, ErrAccountableCompactRerun)
			require.EqualError(t, err, "this completed compact review cannot be rerun; its findings and history are preserved")
			require.ErrorIs(t, db.ReenqueueJob(job.ID, ReenqueueOpts{}), ErrAccountableCompactRerun)
			after, err := db.GetReviewByJobID(job.ID)
			require.NoError(t, err)
			afterJob, err := db.GetJobByID(job.ID)
			require.NoError(t, err)
			afterSource, err := db.GetReviewByJobID(source.ID)
			require.NoError(t, err)
			a := assert.New(t)
			a.Equal(before, after)
			a.Equal(beforeJob, afterJob)
			a.Equal(beforeSource, afterSource)
			_, found, err := db.GetRerunRequest(requestID, job.ID)
			require.NoError(t, err)
			a.False(found)
		})
	}
}

func TestCompactRerunGuardKeepsOrdinaryAndLegacyBehavior(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{JobTypeReview, JobTypeCompact} {
		t.Run(kind, func(t *testing.T) {
			db := openTestDB(t)
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			repo, err := db.GetOrCreateRepo("/tmp/rerun-compatibility-fixture")
			require.NoError(t, err)
			text := CompactPromptPrefix + "ordinary prompt with similar text"
			if kind == JobTypeCompact {
				text = "legacy custom compact instructions"
			}
			job, err := db.EnqueueJob(EnqueueOpts{RepoID: repo.ID, GitRef: "subject", Agent: "test", JobType: kind, Prompt: text})
			require.NoError(t, err)
			_, err = db.ClaimJob("worker")
			require.NoError(t, err)
			require.NoError(t, db.CompleteJobResult(job.ID, "test", ReviewCompletion{StructuredOutput: jsontext.Value(`{"schema_version":3,"summary":"Historical custom content","verdict":"pass","findings":[]}`)}))
			require.NoError(t, db.ReenqueueJob(job.ID, ReenqueueOpts{}))
			updated, err := db.GetJobByID(job.ID)
			require.NoError(t, err)
			assert.Equal(t, JobStatusQueued, updated.Status)
			_, err = db.GetReviewByJobID(job.ID)
			require.ErrorIs(t, err, sql.ErrNoRows)
		})
	}
}
