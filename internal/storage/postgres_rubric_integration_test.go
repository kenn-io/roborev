//go:build postgres

package storage

import (
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIntegrationRubricDigestStorageRoundTrip(t *testing.T) { //nolint:paralleltest // shares the PostgreSQL test schema
	pool := openTestPgPool(t)
	ctx := t.Context()
	source := openTestDB(t)
	t.Cleanup(func() { source.Close() })
	destination := openTestDB(t)
	t.Cleanup(func() { destination.Close() })
	sourceRepo := createRepo(t, source, t.TempDir())
	destinationRepo := createRepo(t, destination, t.TempDir())
	machine, err := source.GetMachineID()
	require.NoError(t, err)
	require.NoError(t, pool.RegisterMachine(ctx, machine, "rubric-source"))
	pgRepo, err := pool.GetOrCreateRepo(ctx, "rubric-roundtrip-"+uuid.New().String())
	require.NoError(t, err)

	first := "1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef"
	changed := "abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890"
	for _, mode := range []string{"single", "batch"} {
		t.Run(mode, func(t *testing.T) {
			job, err := source.EnqueueJob(EnqueueOpts{
				RepoID: sourceRepo.ID, GitRef: mode, Agent: "test",
				ReviewType: "custom", RubricSHA256: &first,
			})
			require.NoError(t, err)
			setStatus(t, source, job.ID, JobStatusDone)
			base := time.Now().UTC().Add(-time.Hour)
			for i, digest := range []*string{&first, &changed, nil} {
				attempt := preciseTimestampAt(base.Add(time.Duration(i) * time.Second))
				_, err := source.ExecContext(ctx, `UPDATE review_jobs SET rubric_sha256 = ?, enqueued_at = ?, updated_at = ? WHERE id = ?`, digest, attempt, attempt, job.ID)
				require.NoError(t, err)
				outgoing, err := source.GetJobsToSync(machine, 100)
				require.NoError(t, err)
				var selected *SyncableJob
				for i := range outgoing {
					if outgoing[i].ID == job.ID {
						selected = &outgoing[i]
						break
					}
				}
				require.NotNil(t, selected)
				assert.Equal(t, digest, selected.RubricSHA256)
				if mode == "single" {
					require.NoError(t, pool.UpsertJob(ctx, *selected, pgRepo, nil))
				} else {
					success, err := pool.BatchUpsertJobs(ctx, []JobWithPgIDs{{Job: *selected, PgRepoID: pgRepo}})
					require.NoError(t, err)
					assert.Equal(t, []bool{true}, success)
				}
				cursor := ""
				var match *PulledJob
				for match == nil {
					pulled, next, err := pool.PullJobs(ctx, uuid.New(), cursor, 500)
					require.NoError(t, err)
					require.NotEmpty(t, pulled, "job must be present in PostgreSQL pull")
					for i := range pulled {
						if pulled[i].UUID == selected.UUID {
							match = &pulled[i]
							break
						}
					}
					cursor = next
				}
				assert.Equal(t, digest, match.RubricSHA256)
				require.NoError(t, destination.UpsertPulledJob(*match, destinationRepo.ID, nil))
				var id int64
				require.NoError(t, destination.QueryRowContext(ctx, `SELECT id FROM review_jobs WHERE uuid = ?`, selected.UUID).Scan(&id))
				stored, err := destination.GetJobByID(id)
				require.NoError(t, err)
				assert.Equal(t, digest, stored.RubricSHA256)
				if digest == nil {
					assert.Empty(t, RubricID(stored.ReviewType, stored.RubricSHA256))
				} else {
					assert.Equal(t, "custom@"+(*digest)[:16], RubricID(stored.ReviewType, stored.RubricSHA256))
				}
			}
		})
	}
}
