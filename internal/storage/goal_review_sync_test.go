package storage

import (
	"encoding/json/jsontext"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const goalReviewDocument = `{"schema_version":2,"summary":"Goal review complete.","verdict":"fail","findings":[{"severity":"medium","problem":"The plan omits a required outcome.","fix":"Add the missing outcome.","location":"plan.md:4"}]}`

const goalReviewMarkdown = "## Summary\n\nGoal review complete.\n\n**Agent assessment:** Fail\n\n## Findings\n\n### 1. Medium\n\n**Location:** plan.md:4\n\n**Problem:** The plan omits a required outcome.\n\n**Fix:** Add the missing outcome.\n"

func TestGoalReviewDocumentStorage(t *testing.T) {
	t.Parallel()
	for _, operation := range []string{"complete", "pull"} {
		t.Run(operation, func(t *testing.T) {
			t.Parallel()
			assert := assert.New(t)
			db := openTestDB(t)
			t.Cleanup(func() { db.Close() })
			repo, err := db.GetOrCreateRepo(t.TempDir())
			require.NoError(t, err)
			job, err := db.EnqueueJob(EnqueueOpts{
				RepoID: repo.ID, Agent: "test", GitRef: "snapshot-digest", JobType: JobTypeGoalReview,
			})
			require.NoError(t, err)
			switch operation {
			case "complete":
				_, err := db.ClaimJob("worker")
				require.NoError(t, err)
				require.NoError(t, db.CompleteJobResult(job.ID, "test", ReviewCompletion{
					Output: "stale prose", StructuredOutput: jsontext.Value(goalReviewDocument),
				}))
			case "pull":
				require.NoError(t, db.UpsertPulledReview(PulledReview{
					UUID: uuid.New(), JobUUID: *job.UUID, Agent: "test",
					StructuredOutput: jsontext.Value(goalReviewDocument), VerdictBool: new(false),
					UpdatedByMachineID: uuid.New(), CreatedAt: time.Now(), UpdatedAt: time.Now(),
				}))
			}
			review, err := db.GetReviewByJobID(job.ID)
			require.NoError(t, err)
			assert.Equal(goalReviewMarkdown, review.Output)
			assert.Equal(VerdictFail, review.Verdict())
			assert.Equal([]any{map[string]any{
				"severity": "medium", "problem": "The plan omits a required outcome.",
				"fix": "Add the missing outcome.", "location": "plan.md:4",
			}}, review.StructuredOutput["findings"])
			var storedOutput string
			require.NoError(t, db.QueryRow(`SELECT output FROM reviews WHERE job_id = ?`, job.ID).Scan(&storedOutput))
			assert.Empty(storedOutput)
		})
	}
}
