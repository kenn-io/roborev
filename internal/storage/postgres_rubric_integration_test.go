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
	machine := uuid.New()
	require.NoError(t, pool.RegisterMachine(ctx, machine, "rubric-source"))
	pgRepo, err := pool.GetOrCreateRepo(ctx, "rubric-roundtrip-"+uuid.New().String())
	require.NoError(t, err)

	first := "1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef"
	changed := "abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890"
	for _, mode := range []string{"single", "batch"} {
		t.Run(mode, func(t *testing.T) {
			job := SyncableJob{UUID: uuid.New(), GitRef: mode, Agent: "test", JobType: JobTypeReview,
				ReviewType: "custom", Status: "done", SourceMachineID: machine}
			base := time.Now().UTC().Add(-time.Hour)
			for i, digest := range []*string{&first, &changed, nil} {
				job.EnqueuedAt = base.Add(time.Duration(i) * time.Second)
				job.UpdatedAt = job.EnqueuedAt
				job.RubricSHA256 = digest
				if mode == "single" {
					require.NoError(t, pool.UpsertJob(ctx, job, pgRepo, nil))
				} else {
					success, err := pool.BatchUpsertJobs(ctx, []JobWithPgIDs{{Job: job, PgRepoID: pgRepo}})
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
						if pulled[i].UUID == job.UUID {
							match = &pulled[i]
							break
						}
					}
					cursor = next
				}
				assert.Equal(t, digest, match.RubricSHA256)
			}
		})
	}
}
