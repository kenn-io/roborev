package storage

import (
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewAttemptRetiresLegacyReview(t *testing.T) {
	t.Parallel()
	for _, action := range []string{"rerun", "pull", "rejected rerun", "stale pull"} {
		t.Run(action, func(t *testing.T) {
			t.Parallel()
			assert := assert.New(t)
			env := setupJobEnv(t, t.TempDir(), "legacy-attempt")
			_, err := env.db.Exec(`UPDATE review_jobs SET status = 'done' WHERE id = ?`, env.job.ID)
			require.NoError(t, err)
			_, err = env.db.Exec(`INSERT INTO reviews (job_id, uuid, agent, output) VALUES (?, ?, 'test', 'Historical review to preserve.')`, env.job.ID, testUUID("archived-attempt"))
			require.NoError(t, err)
			require.NoError(t, env.db.migrateLegacyReviews())
			records, err := env.db.UnresolvedLegacyReviews()
			require.NoError(t, err)
			require.Len(t, records, 1)
			// Exercise both archive-only and restored legacy rows.
			if action == "pull" {
				require.NoError(t, env.db.restoreLegacyReviews())
			}
			accepted := action == "rerun" || action == "pull"
			if action == "rerun" || action == "rejected rerun" {
				if !accepted {
					_, err = env.db.Exec(`UPDATE review_jobs SET status = 'running' WHERE id = ?`, env.job.ID)
					require.NoError(t, err)
				}
				err = env.db.ReenqueueJob(env.job.ID, ReenqueueOpts{})
				if accepted {
					require.NoError(t, err)
				} else {
					require.ErrorIs(t, err, sql.ErrNoRows)
				}
			} else {
				attempt := env.job.EnqueuedAt.Add(time.Hour)
				if !accepted {
					attempt = env.job.EnqueuedAt.Add(-time.Hour)
				}
				changed, err := env.db.upsertPulledJob(PulledJob{
					UUID: *env.job.UUID, SourceMachineID: testUUID("remote"),
					GitRef: "legacy-attempt", Agent: "test", JobType: JobTypeReview,
					Status: "failed", EnqueuedAt: attempt, UpdatedAt: attempt,
				}, env.repo.ID, nil)
				require.NoError(t, err)
				assert.Equal(accepted, changed)
			}
			var original string
			require.NoError(t, env.db.QueryRow(`SELECT output FROM legacy_reviews WHERE archive_id = ?`, records[0].ID).Scan(&original))
			assert.Equal("Historical review to preserve.", original)
			err = env.db.ResolveLegacyReview(records[0].ID, reviewFixtureJSON("Converted historical review."))
			if accepted {
				require.ErrorIs(t, err, sql.ErrNoRows)
				require.NoError(t, env.db.restoreLegacyReviews())
				_, err = env.db.GetReviewByJobID(env.job.ID)
				assert.ErrorIs(err, sql.ErrNoRows)
			} else {
				require.NoError(t, err)
			}
		})
	}
}
