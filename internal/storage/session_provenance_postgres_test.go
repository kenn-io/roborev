//go:build postgres

package storage

import (
	_ "embed"
	"encoding/json/v2"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIntegrationSessionProvenancePostgres(t *testing.T) { //nolint:paralleltest // owns an isolated fixture database schema
	pool := openTestPgPool(t)
	ctx := t.Context()
	machine := uuid.New()
	require.NoError(t, pool.RegisterMachine(ctx, machine, "fixture"))
	repoID, err := pool.GetOrCreateRepo(ctx, "provenance-fixture-"+uuid.New().String())
	require.NoError(t, err)
	raw, err := json.Marshal(provenanceFixture())
	require.NoError(t, err)
	job := SyncableJob{UUID: uuid.New(), GitRef: "head", Agent: "test", Status: "done", EnqueuedAt: time.Now().UTC().Truncate(time.Microsecond), SourceMachineID: machine, SessionProvenance: string(raw)}
	read := func() string {
		var value string
		require.NoError(t, pool.pool.QueryRow(ctx, `SELECT COALESCE(session_provenance,'') FROM review_jobs WHERE uuid=$1`, job.UUID).Scan(&value))
		return value
	}
	require.NoError(t, pool.UpsertJob(ctx, job, repoID, nil))
	assert.Equal(t, string(raw), read())
	older := job
	job.SessionProvenance = ""
	_, err = pool.BatchUpsertJobs(ctx, []JobWithPgIDs{{Job: job, PgRepoID: repoID}})
	require.NoError(t, err)
	assert.Equal(t, string(raw), read())
	job.SessionProvenance = "[]"
	require.NoError(t, pool.UpsertJob(ctx, job, repoID, nil))
	assert.Equal(t, "[]", read())
	job.SessionProvenance = string(raw)
	_, err = pool.BatchUpsertJobs(ctx, []JobWithPgIDs{{Job: job, PgRepoID: repoID}})
	require.NoError(t, err)
	assert.Equal(t, string(raw), read())
	pulled, _, err := pool.PullJobs(ctx, uuid.New(), "", 100)
	require.NoError(t, err)
	found := false
	for _, row := range pulled {
		if row.UUID == job.UUID {
			assert.Equal(t, string(raw), row.SessionProvenance)
			found = true
		}
	}
	assert.True(t, found)
	job.EnqueuedAt = job.EnqueuedAt.Add(time.Second)
	job.SessionProvenance = ""
	require.NoError(t, pool.UpsertJob(ctx, job, repoID, nil))
	assert.Empty(t, read())
	_, err = pool.BatchUpsertJobs(ctx, []JobWithPgIDs{{Job: older, PgRepoID: repoID}})
	require.NoError(t, err)
	assert.Empty(t, read())
}

//go:embed schemas/postgres_v24.sql
var provenancePostgresV24Schema string

func TestPostgresMigrationSessionProvenance(t *testing.T) { //nolint:paralleltest // owns the fixture schema during migration
	pool := openTestPgPoolRawAtVersion(t, 24)
	require.NoError(t, pool.EnsureSchema(t.Context()))
	require.NoError(t, pool.EnsureSchema(t.Context()), "migration is safe to reopen")
	var version int
	require.NoError(t, pool.pool.QueryRow(t.Context(), `SELECT MAX(version) FROM schema_version`).Scan(&version))
	assert.Equal(t, pgSchemaVersion, version)
	rows, err := pool.pool.Query(t.Context(), `SELECT session_provenance FROM review_jobs LIMIT 0`)
	require.NoError(t, err)
	rows.Close()
}
