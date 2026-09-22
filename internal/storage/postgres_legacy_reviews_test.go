//go:build postgres

package storage

import (
	"encoding/json/jsontext"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/pkg/structuredreview"
)

func TestIntegrationLegacyReviewMigration(t *testing.T) {
	pool := openTestPgPool(t)
	ctx := t.Context()
	repoID := createTestRepo(t, pool.Pool(), TestRepoOpts{})
	commitID := createTestCommit(t, pool.Pool(), TestCommitOpts{RepoID: repoID})
	jobID := uuid.New()
	createTestJob(t, pool.Pool(), TestJobOpts{UUID: jobID, RepoID: repoID, CommitID: commitID})
	reviewID := uuid.New()
	_, err := pool.Pool().Exec(ctx, `INSERT INTO reviews (uuid, job_uuid, agent, prompt, output, updated_by_machine_id)
 VALUES ($1, $2, 'test', 'prompt', 'Legacy finding with missing details', $3)`, reviewID, jobID, defaultTestMachineID)
	require.NoError(t, err)
	pulled, _, err := pool.PullReviews(ctx, uuid.New(), []uuid.UUID{jobID}, "", 10)
	require.NoError(t, err)
	assert.Empty(t, pulled)
	require.NoError(t, pool.migrateLegacyReviews(ctx))
	var original, reason string
	require.NoError(t, pool.Pool().QueryRow(ctx, `SELECT record->>'output', migration_error FROM legacy_reviews WHERE uuid = $1`, reviewID).Scan(&original, &reason))
	assert.Equal(t, "Legacy finding with missing details", original)
	assert.Contains(t, reason, "AI conversion required")
	var active int
	require.NoError(t, pool.Pool().QueryRow(ctx, `SELECT count(*) FROM reviews WHERE uuid = $1`, reviewID).Scan(&active))
	assert.Zero(t, active)
	require.NoError(t, pool.migrateLegacyReviews(ctx))
	raw := jsontext.Value(`{"schema_version":2,"summary":"Converted review.","verdict":"pass","findings":[]}`)
	require.NoError(t, pool.UpsertReview(ctx, SyncableReview{
		UUID: reviewID, JobUUID: jobID,
		Agent: "test", Prompt: "prompt", StructuredOutput: raw, UpdatedByMachineID: defaultTestMachineID, CreatedAt: time.Now(),
	}))
	var resolved bool
	var output string
	require.NoError(t, pool.Pool().QueryRow(ctx, `SELECT resolved_at IS NOT NULL FROM legacy_reviews WHERE uuid = $1`, reviewID).Scan(&resolved))
	assert.True(t, resolved)
	require.NoError(t, pool.Pool().QueryRow(ctx, `SELECT output FROM reviews WHERE uuid = $1`, reviewID).Scan(&output))
	assert.Empty(t, output)
}

func TestIntegrationLegacyReviewExplicitConversion(t *testing.T) {
	pool := openTestPgPool(t)
	ctx := t.Context()
	repoID := createTestRepo(t, pool.Pool(), TestRepoOpts{})
	commitID := createTestCommit(t, pool.Pool(), TestCommitOpts{RepoID: repoID})
	jobID, reviewID := uuid.New(), uuid.New()
	createTestJob(t, pool.Pool(), TestJobOpts{UUID: jobID, RepoID: repoID, CommitID: commitID})
	runID := uuid.New()
	_, err := pool.Pool().Exec(ctx, `UPDATE review_jobs SET job_type = 'synthesis', panel_run_uuid = $1, panel_role = 'synthesis' WHERE uuid = $2`, runID, jobID)
	require.NoError(t, err)
	for i, status := range []string{"failed", "done"} {
		memberID := uuid.New()
		createTestJob(t, pool.Pool(), TestJobOpts{UUID: memberID, RepoID: repoID, CommitID: commitID, Status: status})
		_, err := pool.Pool().Exec(ctx, `UPDATE review_jobs SET panel_run_uuid = $1, panel_role = 'member', panel_member_index = $2, review_type = 'security' WHERE uuid = $3`, runID, i, memberID)
		require.NoError(t, err)
		if status == "done" {
			memberReviewID := uuid.New()
			_, err := pool.Pool().Exec(ctx, `INSERT INTO reviews (uuid, job_uuid, agent, prompt, output, updated_by_machine_id) VALUES ($1, $2, 'test', 'prompt', 'The member wrote prose that roborev cannot read back.', $3)`, memberReviewID, memberID, defaultTestMachineID)
			require.NoError(t, err)
			require.NoError(t, pool.migrateLegacyReviews(ctx))
			clean := jsontext.Value(`{"schema_version":2,"summary":"Current review.","verdict":"pass","findings":[]}`)
			require.NoError(t, pool.ResolveLegacyReview(ctx, memberReviewID, clean))
			_, err = pool.Pool().Exec(ctx, `INSERT INTO legacy_reviews (uuid, record, migration_error, resolved_at) SELECT $1, record, migration_error, resolved_at FROM legacy_reviews WHERE uuid = $2`, uuid.New(), memberReviewID)
			require.NoError(t, err)
		}
	}
	incoming := SyncableReview{UUID: reviewID, JobUUID: jobID, Agent: "test", Prompt: "original prompt", Output: "Legacy finding", UpdatedByMachineID: defaultTestMachineID, CreatedAt: time.Now()}
	_, err = pool.Pool().Exec(ctx, `INSERT INTO reviews (uuid, job_uuid, agent, prompt, output, updated_by_machine_id) VALUES ($1, $2, 'test', 'original prompt', 'Legacy finding', $3)`, reviewID, jobID, defaultTestMachineID)
	require.NoError(t, err)
	require.NoError(t, pool.migrateLegacyReviews(ctx))
	records, err := pool.UnresolvedLegacyReviews(ctx)
	require.NoError(t, err)
	var record PostgresLegacyReview
	for _, candidate := range records {
		if candidate.ID == reviewID {
			record = candidate
		}
	}
	require.Len(t, record.Sources, 1)
	assert.Equal(t, 1, record.Sources[0].Number)
	assert.Equal(t, "test (security)", record.Sources[0].Agent)
	assert.Equal(t, reviewID, record.ID)
	assert.Equal(t, "Legacy finding", record.Output)
	raw := jsontext.Value(`{"schema_version":2,"summary":"Converted review.","verdict":"pass","findings":[]}`)
	require.Error(t, pool.ResolveLegacyReview(ctx, reviewID, jsontext.Value(`invalid`)))
	require.NoError(t, pool.ResolveLegacyReview(ctx, reviewID, raw))
	pulled, _, err := pool.PullReviews(ctx, defaultTestMachineID, []uuid.UUID{jobID}, "", 10)
	require.NoError(t, err)
	require.Len(t, pulled, 1)
	assert.Equal(t, reviewID, pulled[0].UUID)
	var output, prompt, stored, original string
	require.NoError(t, pool.Pool().QueryRow(ctx, `SELECT output, prompt, structured_output::text FROM reviews WHERE uuid = $1`, reviewID).Scan(&output, &prompt, &stored))
	assert.Empty(t, output)
	assert.Equal(t, "original prompt", prompt)
	assert.Contains(t, stored, `"source_labels": ["test (security)"]`)
	require.NoError(t, pool.UpsertReview(ctx, incoming))
	records, err = pool.UnresolvedLegacyReviews(ctx)
	require.NoError(t, err)
	for _, record := range records {
		assert.NotEqual(t, reviewID, record.ID)
	}
	require.NoError(t, pool.Pool().QueryRow(ctx, `SELECT record->>'output' FROM legacy_reviews WHERE uuid = $1`, reviewID).Scan(&original))
	assert.Equal(t, "Legacy finding", original)
}

func TestIntegrationLegacyReviewAutomaticConversion(t *testing.T) {
	pool := openTestPgPool(t)
	ctx := t.Context()
	repoID := createTestRepo(t, pool.Pool(), TestRepoOpts{})
	commitID := createTestCommit(t, pool.Pool(), TestCommitOpts{RepoID: repoID})
	insert := func(markdown string, verdict, closed bool) uuid.UUID {
		jobID, reviewID := uuid.New(), uuid.New()
		createTestJob(t, pool.Pool(), TestJobOpts{UUID: jobID, RepoID: repoID, CommitID: commitID})
		_, err := pool.Pool().Exec(ctx, `INSERT INTO reviews (uuid, job_uuid, agent, prompt, output, verdict_bool, closed, updated_by_machine_id)
 VALUES ($1, $2, 'test', 'original prompt', $3, $4, $5, $6)`, reviewID, jobID, markdown, verdict, closed, defaultTestMachineID)
		require.NoError(t, err)
		return reviewID
	}
	type stored struct {
		output, prompt, document string
		verdict, closed          bool
	}
	read := func(id uuid.UUID) stored {
		var got stored
		require.NoError(t, pool.Pool().QueryRow(ctx, `SELECT output, prompt, structured_output::text, verdict_bool, closed FROM reviews WHERE uuid = $1`, id).
			Scan(&got.output, &got.prompt, &got.document, &got.verdict, &got.closed))
		return got
	}
	active := func(id uuid.UUID) int {
		var count int
		require.NoError(t, pool.Pool().QueryRow(ctx, `SELECT count(*) FROM reviews WHERE uuid = $1`, id).Scan(&count))
		return count
	}
	wantDocument := `{"schema_version":1,"summary":"The change adds a save routine.","findings":[
		{"severity":"high","problem":"The write is not atomic.","fix":"Write to a temporary file and rename it.","location":"store/save.go:88"},
		{"severity":"low","problem":"The comment is stale.","fix":"Update the comment.","location":"store/save.go:12"}]}`

	// The mirror upgrade converts what roborev can read back and archives the rest.
	upgraded := insert(legacyFindingsMarkdown, false, true)
	upgradedProse := insert(legacyProseMarkdown, false, false)
	require.NoError(t, pool.migrateLegacyReviews(ctx))
	got := read(upgraded)
	assert.Empty(t, got.output)
	assert.JSONEq(t, wantDocument, got.document)
	assert.False(t, got.verdict, "verdict is unchanged")
	assert.True(t, got.closed, "closed state is unchanged")
	assert.Equal(t, "original prompt", got.prompt)
	assert.Zero(t, active(upgradedProse))
	var reason string
	require.NoError(t, pool.Pool().QueryRow(ctx, `SELECT migration_error FROM legacy_reviews WHERE uuid = $1 AND resolved_at IS NULL`, upgradedProse).Scan(&reason))
	assert.Contains(t, reason, "automatic conversion refused (unrecognized_format)")

	// A mirror upgraded by an older release archived everything. convert
	// restores the same reviews from the archive.
	archived := insert(legacyFindingsMarkdown, false, true)
	archivedMismatch := insert(legacyFindingsMarkdown, true, false)
	for _, id := range []uuid.UUID{archived, archivedMismatch} {
		_, err := pool.Pool().Exec(ctx, `INSERT INTO legacy_reviews (uuid, record, migration_error)
 SELECT r.uuid, to_jsonb(r), 'No valid review JSON document; AI conversion required' FROM reviews r WHERE r.uuid = $1`, id)
		require.NoError(t, err)
		_, err = pool.Pool().Exec(ctx, `DELETE FROM reviews WHERE uuid = $1`, id)
		require.NoError(t, err)
	}

	dryRun, err := pool.ConvertLegacyReviews(ctx, true)
	require.NoError(t, err)
	assert.Zero(t, active(archived), "a dry run restores nothing")

	report, err := pool.ConvertLegacyReviews(ctx, false)
	require.NoError(t, err)
	dryRun.DryRun = false
	assert.Equal(t, dryRun, report, "the dry run reports what the real run does")
	assert.GreaterOrEqual(t, report.Converted, 1)
	assert.GreaterOrEqual(t, report.Refused[LegacyRefusalVerdictMismatch], 1)
	assert.GreaterOrEqual(t, report.Refused["unrecognized_format"], 1)
	got = read(archived)
	assert.JSONEq(t, wantDocument, got.document)
	assert.False(t, got.verdict)
	assert.True(t, got.closed)
	assert.Zero(t, active(archivedMismatch), "a conversion that would change the verdict stays archived")

	again, err := pool.ConvertLegacyReviews(ctx, false)
	require.NoError(t, err)
	assert.Zero(t, again.Converted, "a second run has nothing new to convert")
	assert.Equal(t, 1, active(archived))
}

func TestIntegrationRestoreLegacyAndImport(t *testing.T) {
	pool := openTestPgPool(t)
	ctx := t.Context()
	repoID := createTestRepo(t, pool.Pool(), TestRepoOpts{})
	commitID := createTestCommit(t, pool.Pool(), TestCommitOpts{RepoID: repoID})
	jobID, reviewID := uuid.New(), uuid.New()
	createTestJob(t, pool.Pool(), TestJobOpts{UUID: jobID, RepoID: repoID, CommitID: commitID})
	_, err := pool.Pool().Exec(ctx, `INSERT INTO reviews (uuid, job_uuid, agent, prompt, output, closed, verdict_bool, updated_by_machine_id)
 VALUES ($1, $2, 'test', 'prompt', 'The write loses data. Keep the old file until rename.', true, NULL, $3)`, reviewID, jobID, defaultTestMachineID)
	require.NoError(t, err)
	require.NoError(t, pool.migrateLegacyReviews(ctx))
	require.NoError(t, pool.restoreLegacyReviews(ctx))
	require.NoError(t, pool.restoreLegacyReviews(ctx))
	var raw jsontext.Value
	var closed bool
	var verdict *bool
	require.NoError(t, pool.Pool().QueryRow(ctx, `SELECT structured_output, closed, verdict_bool FROM reviews WHERE uuid = $1`, reviewID).Scan(&raw, &closed, &verdict))
	doc, err := structuredreview.Decode(raw)
	require.NoError(t, err)
	require.NotNil(t, doc.Legacy)
	assert.Equal(t, "The write loses data. Keep the old file until rename.", doc.Legacy.Markdown)
	assert.True(t, closed)
	assert.Nil(t, verdict)
	converted := jsontext.Value(`{"schema_version":1,"summary":"Converted.","findings":[]}`)
	require.NoError(t, pool.ResolveLegacyReview(ctx, reviewID, converted))
	require.ErrorIs(t, pool.ResolveLegacyReview(ctx, reviewID, converted), ErrReviewNotLegacy)
	require.NoError(t, pool.UpsertReview(ctx, SyncableReview{UUID: reviewID, JobUUID: jobID, Agent: "test", StructuredOutput: raw, CreatedAt: time.Now(), UpdatedByMachineID: defaultTestMachineID}))
	require.NoError(t, pool.Pool().QueryRow(ctx, `SELECT structured_output, closed FROM reviews WHERE uuid = $1`, reviewID).Scan(&raw, &closed))
	doc, err = structuredreview.Decode(raw)
	require.NoError(t, err)
	assert.Nil(t, doc.Legacy)
	assert.Equal(t, "Converted.", doc.Summary)
	assert.True(t, closed)
}

func TestIntegrationLegacyMigrationBatches(t *testing.T) {
	env := newIntegrationEnv(t)
	pool, ctx := env.Pool, t.Context()
	repoID := createTestRepo(t, pool.Pool(), TestRepoOpts{})
	commitID := createTestCommit(t, pool.Pool(), TestCommitOpts{RepoID: repoID})
	for range 205 {
		jobID := uuid.New()
		createTestJob(t, pool.Pool(), TestJobOpts{UUID: jobID, RepoID: repoID, CommitID: commitID})
		_, err := pool.Pool().Exec(ctx, `INSERT INTO reviews (uuid, job_uuid, agent, prompt, output, closed, updated_by_machine_id)
 VALUES ($1, $2, 'test', 'prompt', 'Preserve this historical text.', true, $3)`, uuid.New(), jobID, defaultTestMachineID)
		require.NoError(t, err)
	}
	require.NoError(t, pool.migrateLegacyReviews(ctx))
	first, err := pool.unresolvedLegacyReviews(ctx, nil, 100, true)
	require.NoError(t, err)
	require.Len(t, first, 100)
	second, err := pool.unresolvedLegacyReviews(ctx, &first[99].ID, 100, true)
	require.NoError(t, err)
	require.Len(t, second, 100)
	last, err := pool.unresolvedLegacyReviews(ctx, &second[99].ID, 100, true)
	require.NoError(t, err)
	require.Len(t, last, 5)
	require.NoError(t, pool.restoreLegacyReviews(ctx))
	require.NoError(t, pool.restoreLegacyReviews(ctx))
	var preserved int
	require.NoError(t, pool.Pool().QueryRow(ctx, `SELECT count(*) FROM reviews r JOIN legacy_reviews l ON r.uuid = l.uuid
 WHERE r.job_uuid = (l.record->>'job_uuid')::uuid AND r.closed
 AND r.structured_output->'legacy'->>'markdown' = l.record->>'output'`).Scan(&preserved))
	assert.Equal(t, 205, preserved)
	missing, err := pool.unresolvedLegacyReviews(ctx, nil, 100, true)
	require.NoError(t, err)
	assert.Empty(t, missing)
	exported, err := pool.UnresolvedLegacyReviews(ctx)
	require.NoError(t, err)
	assert.Len(t, exported, 205)
}
