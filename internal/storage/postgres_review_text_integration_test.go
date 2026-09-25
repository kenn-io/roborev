//go:build postgres

package storage

import (
	"encoding/json/jsontext"
	"path/filepath"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIntegrationReviewTextSanitizedForPostgres(t *testing.T) { //nolint:paralleltest // shares the roborev schema in the PostgreSQL database at TEST_POSTGRES_URL
	pool := openTestPgPool(t)
	ctx := t.Context()
	repoID := createTestRepo(t, pool.Pool(), TestRepoOpts{})
	commitID := createTestCommit(t, pool.Pool(), TestCommitOpts{RepoID: repoID})
	reviewJobID, taskJobID := uuid.New(), uuid.New()
	for _, jobID := range []uuid.UUID{reviewJobID, taskJobID} {
		createTestJob(t, pool.Pool(), TestJobOpts{UUID: jobID, RepoID: repoID, CommitID: commitID})
	}
	_, err := pool.Pool().Exec(ctx, `UPDATE review_jobs SET job_type = 'task' WHERE uuid = $1`, taskJobID)
	require.NoError(t, err)

	structuredBytes := append([]byte(`{"schema_version":2,"summary":"Summary\u0000 `), 0xff)
	structuredBytes = append(structuredBytes, []byte(`","verdict":"fail","findings":[{"severity":"high","problem":"Problem\u0000","fix":"Keep the record.","location":null}]}`)...)
	structured := jsontext.Value(structuredBytes)
	batch := []SyncableReview{
		{
			UUID: uuid.New(), JobUUID: reviewJobID,
			Agent: "ag\xffent", Prompt: "café\x00 + \xe9", StructuredOutput: structured,
			UpdatedByMachineID: defaultTestMachineID, CreatedAt: time.Now(),
		},
		{
			UUID: uuid.New(), JobUUID: taskJobID,
			Agent: "test", Prompt: "valid", Output: "résumé\x00 \xe9",
			UpdatedByMachineID: defaultTestMachineID, CreatedAt: time.Now(),
		},
		{
			UUID: uuid.New(), JobUUID: taskJobID,
			Agent: "test", Prompt: "Ελληνικά", Output: "naïve",
			UpdatedByMachineID: defaultTestMachineID, CreatedAt: time.Now(),
		},
	}

	t.Run("batch", func(t *testing.T) {
		assert := assert.New(t)
		success, err := pool.BatchUpsertReviews(ctx, batch)
		require.NoError(t, err)
		assert.Equal([]bool{true, true, true}, success)
		assert.Equal("ag\xffent", batch[0].Agent)
		assert.Equal("café\x00 + \xe9", batch[0].Prompt)
		assert.Equal("résumé\x00 \xe9", batch[1].Output)

		for i, want := range []struct{ agent, prompt, output string }{
			{"ag\uFFFDent", "café\uFFFD + \uFFFD", ""},
			{"test", "valid", "résumé\uFFFD \uFFFD"},
			{"test", "Ελληνικά", "naïve"},
		} {
			var agent, prompt, output string
			require.NoError(t, pool.Pool().QueryRow(ctx,
				`SELECT agent, prompt, output FROM reviews WHERE uuid = $1`, batch[i].UUID,
			).Scan(&agent, &prompt, &output))
			assert.Equal(want.agent, agent)
			assert.Equal(want.prompt, prompt)
			assert.Equal(want.output, output)
		}
		var structuredOutput string
		require.NoError(t, pool.Pool().QueryRow(ctx,
			`SELECT structured_output::text FROM reviews WHERE uuid = $1`, batch[0].UUID,
		).Scan(&structuredOutput))
		assert.JSONEq(`{"schema_version":2,"summary":"Summary\uFFFD \uFFFD","verdict":"fail","findings":[{"severity":"high","problem":"Problem\uFFFD","fix":"Keep the record.","location":null}]}`, structuredOutput)
	})

	t.Run("single", func(t *testing.T) {
		assert := assert.New(t)
		review := SyncableReview{
			UUID: uuid.New(), JobUUID: taskJobID,
			Agent: "é\xff", Prompt: "\x00界", Output: "\xe9 café",
			UpdatedByMachineID: defaultTestMachineID, CreatedAt: time.Now(),
		}
		require.NoError(t, pool.UpsertReview(ctx, review))
		var agent, prompt, output string
		require.NoError(t, pool.Pool().QueryRow(ctx,
			`SELECT agent, prompt, output FROM reviews WHERE uuid = $1`, review.UUID,
		).Scan(&agent, &prompt, &output))
		assert.Equal("é\uFFFD", agent)
		assert.Equal("\uFFFD界", prompt)
		assert.Equal("\uFFFD café", output)
		assert.Equal("é\xff", review.Agent)
		assert.Equal("\x00界", review.Prompt)
		assert.Equal("\xe9 café", review.Output)

		structuredReview := SyncableReview{
			UUID: uuid.New(), JobUUID: taskJobID,
			StructuredOutput:   jsontext.Value(`{"schema_version":2,"summary":"Single\u0000","verdict":"pass","findings":[]}`),
			UpdatedByMachineID: defaultTestMachineID, CreatedAt: time.Now(),
		}
		require.NoError(t, pool.UpsertReview(ctx, structuredReview))
		var structuredOutput string
		require.NoError(t, pool.Pool().QueryRow(ctx,
			`SELECT structured_output::text FROM reviews WHERE uuid = $1`, structuredReview.UUID,
		).Scan(&structuredOutput))
		assert.JSONEq(`{"schema_version":2,"summary":"Single\uFFFD","verdict":"pass","findings":[]}`, structuredOutput)
	})
}

func TestIntegrationReviewSyncPreservesSQLiteText(t *testing.T) { //nolint:paralleltest // shares the roborev schema in the PostgreSQL database at TEST_POSTGRES_URL
	assert := assert.New(t)
	env := newIntegrationEnv(t)
	db := env.openDB("source.db")
	repo, err := db.GetOrCreateRepo(filepath.Join(env.TmpDir, "repo"), "https://example.test/repo.git")
	require.NoError(t, err)
	commit, err := db.GetOrCreateCommit(repo.ID, "abcdef123456", "Author", "Subject", time.Now())
	require.NoError(t, err)
	job, err := db.EnqueueJob(EnqueueOpts{
		RepoID: repo.ID, CommitID: commit.ID, GitRef: "HEAD", Agent: "test", JobType: JobTypeTask,
	})
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE review_jobs SET status = 'running', started_at = datetime('now') WHERE id = ?`, job.ID)
	require.NoError(t, err)
	prompt, output := "café\xe9\x00", "résumé\xff"
	require.NoError(t, db.CompleteJob(job.ID, "test", prompt, output))
	review, err := db.GetReviewByJobID(job.ID)
	require.NoError(t, err)
	require.NotNil(t, review.UUID)

	worker := startSyncWorkerNoSync(t, db, env.pgURL, "review-text-source", "1h")
	stats, err := worker.SyncNow()
	require.NoError(t, err)
	assert.Equal(1, stats.PushedReviews)
	var pgPrompt, pgOutput string
	require.NoError(t, env.Pool.Pool().QueryRow(env.Ctx,
		`SELECT prompt, output FROM reviews WHERE uuid = $1`, *review.UUID,
	).Scan(&pgPrompt, &pgOutput))
	assert.Equal("café\uFFFD\uFFFD", pgPrompt)
	assert.Equal("résumé\uFFFD", pgOutput)

	machineID, err := db.GetMachineID()
	require.NoError(t, err)
	pending, err := db.GetReviewsToSync(machineID, 10)
	require.NoError(t, err)
	assert.Empty(pending)
	var sqlitePrompt, sqliteOutput string
	require.NoError(t, db.QueryRow(`SELECT prompt, output FROM reviews WHERE id = ?`, review.ID).
		Scan(&sqlitePrompt, &sqliteOutput))
	assert.Equal([]byte(prompt), []byte(sqlitePrompt))
	assert.Equal([]byte(output), []byte(sqliteOutput))
}
