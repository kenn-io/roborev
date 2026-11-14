//go:build postgres

package storage

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"log"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/cenkalti/backoff/v7"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/pkg/structuredreview"
)

func insertLegacyScanReview(t *testing.T, pool *PgPool, repoID, commitID int64, id uuid.UUID, age string, closed bool) uuid.UUID {
	t.Helper()
	jobID := uuid.New()
	createTestJob(t, pool.Pool(), TestJobOpts{UUID: jobID, RepoID: repoID, CommitID: commitID})
	_, err := pool.Pool().Exec(t.Context(), `INSERT INTO reviews (uuid, job_uuid, job_enqueued_at, job_source_machine_id, agent, prompt, output, updated_at, closed, updated_by_machine_id)
 VALUES ($1, $2, (SELECT enqueued_at FROM review_jobs WHERE uuid = $2), (SELECT source_machine_id FROM review_jobs WHERE uuid = $2), 'test', 'prompt', 'Preserve this historical text.', clock_timestamp() - $3::interval, $4, $5)`, id, jobID, age, closed, defaultTestMachineID)
	require.NoError(t, err)
	return id
}

func TestIntegrationLegacyReviewScanOnlyRevisitsNewWrites(t *testing.T) { //nolint:paralleltest // shares the roborev schema in the PostgreSQL database at TEST_POSTGRES_URL
	env := newIntegrationEnv(t)
	pool, ctx := env.Pool, t.Context()
	repoID := createTestRepo(t, pool.Pool(), TestRepoOpts{})
	commitID := createTestCommit(t, pool.Pool(), TestCommitOpts{RepoID: repoID})
	old := insertLegacyScanReview(t, pool, repoID, commitID, uuid.New(), "1 day", false)
	fresh := insertLegacyScanReview(t, pool, repoID, commitID, uuid.New(), "0 seconds", false)
	require.NoError(t, pool.migrateLegacyReviews(ctx, false))
	var output string
	require.NoError(t, pool.Pool().QueryRow(ctx, `SELECT record->>'output' FROM legacy_reviews WHERE uuid = $1`, fresh).Scan(&output))
	assert.Equal(t, "Preserve this historical text.", output)
	require.NoError(t, pool.Pool().QueryRow(ctx, `SELECT output FROM reviews WHERE uuid = $1`, old).Scan(&output))
	assert.Equal(t, "Preserve this historical text.", output)
}

func TestIntegrationLegacyReviewScanResumesAfterTimeout(t *testing.T) { //nolint:paralleltest // shares the roborev schema in the PostgreSQL database at TEST_POSTGRES_URL
	env := newIntegrationEnv(t)
	pool, ctx := env.Pool, t.Context()
	repoID := createTestRepo(t, pool.Pool(), TestRepoOpts{})
	commitID := createTestCommit(t, pool.Pool(), TestCommitOpts{RepoID: repoID})
	for range 205 {
		insertLegacyScanReview(t, pool, repoID, commitID, uuid.New(), "0 seconds", false)
	}
	// Exercise the first full scan, including resuming with no watermark.
	_, err := pool.Pool().Exec(ctx, `DELETE FROM sync_metadata WHERE key = $1`, legacyReviewScanKey)
	require.NoError(t, err)
	var locked uuid.UUID
	require.NoError(t, pool.Pool().QueryRow(ctx, `SELECT uuid FROM reviews ORDER BY uuid OFFSET 100 LIMIT 1`).Scan(&locked))
	tx, err := pool.Pool().Begin(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback(context.WithoutCancel(ctx)) })
	_, err = tx.Exec(ctx, `SELECT uuid FROM reviews WHERE uuid = $1 FOR UPDATE`, locked)
	require.NoError(t, err)
	scanCtx, cancel := context.WithCancel(ctx)
	result := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		result <- pool.migrateLegacyReviews(scanCtx, false)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	// PostgreSQL persists the checkpoint outside synctest's control.
	waitCondition(t, 10*time.Second, "saved legacy scan run", func() (bool, error) {
		var stored string
		if err := pool.Pool().QueryRow(ctx, `SELECT value FROM sync_metadata WHERE key = $1`, legacyReviewScanKey).Scan(&stored); err != nil {
			return false, err
		}
		var state legacyReviewScanState
		if err := json.Unmarshal([]byte(stored), &state); err != nil {
			return false, err
		}
		return state.Run != nil && state.Run.After != nil, nil
	})
	// The inline upgrade waits for the row lock; PostgreSQL is external to synctest.
	waitCondition(t, 10*time.Second, "inline scan waiting for locked row", func() (bool, error) {
		var blocked bool
		err := pool.Pool().QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND query LIKE '%FOR UPDATE OF r%')`).Scan(&blocked)
		return blocked, err
	})
	cancel()
	require.ErrorIs(t, <-result, context.Canceled)
	require.NoError(t, tx.Rollback(ctx))
	low := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	insertLegacyScanReview(t, pool, repoID, commitID, low, "1 day", false)
	require.NoError(t, pool.migrateLegacyReviews(ctx, false))
	var archived int
	require.NoError(t, pool.Pool().QueryRow(ctx, `SELECT count(*) FROM legacy_reviews`).Scan(&archived))
	assert.Equal(t, 205, archived)
	var output string
	require.NoError(t, pool.Pool().QueryRow(ctx, `SELECT output FROM reviews WHERE uuid = $1`, low).Scan(&output))
	assert.Equal(t, "Preserve this historical text.", output)
}

func TestIntegrationLegacyReviewScanUnreadableState(t *testing.T) { //nolint:paralleltest // shares the roborev schema and captures the migration logger
	env := newIntegrationEnv(t)
	pool, ctx := env.Pool, t.Context()
	repoID := createTestRepo(t, pool.Pool(), TestRepoOpts{})
	commitID := createTestCommit(t, pool.Pool(), TestCommitOpts{RepoID: repoID})
	id := insertLegacyScanReview(t, pool, repoID, commitID, uuid.New(), "1 day", false)
	_, err := pool.Pool().Exec(ctx, `UPDATE sync_metadata SET value = $2 WHERE key = $1`, legacyReviewScanKey, `{"Done":`)
	require.NoError(t, err)
	var logs bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previous) })
	require.NoError(t, pool.migrateLegacyReviews(ctx, false))
	require.NoError(t, pool.migrateLegacyReviews(ctx, false))
	assert.Equal(t, 1, strings.Count(logs.String(), "unreadable scan state; restarting full scan"))
	var output string
	require.NoError(t, pool.Pool().QueryRow(ctx, `SELECT record->>'output' FROM legacy_reviews WHERE uuid = $1`, id).Scan(&output))
	assert.Equal(t, "Preserve this historical text.", output)
}

func TestIntegrationLegacyReviewMigration(t *testing.T) { //nolint:paralleltest // shares the roborev schema in the PostgreSQL database at TEST_POSTGRES_URL
	env := newIntegrationEnv(t)
	pool := env.Pool
	ctx := t.Context()
	repoID := createTestRepo(t, pool.Pool(), TestRepoOpts{})
	commitID := createTestCommit(t, pool.Pool(), TestCommitOpts{RepoID: repoID})
	jobID := uuid.New()
	createTestJob(t, pool.Pool(), TestJobOpts{UUID: jobID, RepoID: repoID, CommitID: commitID})
	reviewID := uuid.New()
	_, err := pool.Pool().Exec(ctx, `INSERT INTO reviews (uuid, job_uuid, job_enqueued_at, job_source_machine_id, agent, prompt, output, updated_by_machine_id)
 VALUES ($1, $2, (SELECT enqueued_at FROM review_jobs WHERE uuid = $2), (SELECT source_machine_id FROM review_jobs WHERE uuid = $2), 'test', 'prompt', 'Legacy finding with missing details', $3)`, reviewID, jobID, defaultTestMachineID)
	require.NoError(t, err)
	// Connecting to an up-to-date schema leaves historical maintenance deferred.
	require.NoError(t, pool.EnsureSchema(ctx))
	var active int
	require.NoError(t, pool.Pool().QueryRow(ctx, `SELECT count(*) FROM reviews WHERE uuid = $1`, reviewID).Scan(&active))
	assert.Equal(t, 1, active)
	pulled, _, err := pool.PullReviews(ctx, uuid.New(), map[uuid.UUID]struct{}{jobID: {}}, "", 10)
	require.NoError(t, err)
	assert.Empty(t, pulled)
	require.NoError(t, pool.migrateLegacyReviews(ctx, false))
	var original, reason string
	require.NoError(t, pool.Pool().QueryRow(ctx, `SELECT record->>'output', migration_error FROM legacy_reviews WHERE uuid = $1`, reviewID).Scan(&original, &reason))
	assert.Equal(t, "Legacy finding with missing details", original)
	assert.Contains(t, reason, "AI conversion required")
	require.NoError(t, pool.Pool().QueryRow(ctx, `SELECT count(*) FROM reviews WHERE uuid = $1`, reviewID).Scan(&active))
	assert.Zero(t, active)
	require.NoError(t, pool.migrateLegacyReviews(ctx, false))
	raw := jsontext.Value(`{"schema_version":2,"summary":"Converted review.","verdict":"pass","findings":[]}`)
	require.NoError(t, pool.UpsertReview(ctx, SyncableReview{
		UUID: reviewID, JobUUID: jobID, JobEnqueuedAt: testJobEnqueuedAt(t, pool.Pool(), jobID), JobSourceMachineID: testJobSourceMachineID(t, pool.Pool(), jobID),
		Agent: "test", StructuredOutput: raw, UpdatedByMachineID: defaultTestMachineID, CreatedAt: time.Now(),
	}))
	var resolved bool
	var output string
	require.NoError(t, pool.Pool().QueryRow(ctx, `SELECT resolved_at IS NOT NULL FROM legacy_reviews WHERE uuid = $1`, reviewID).Scan(&resolved))
	assert.True(t, resolved)
	require.NoError(t, pool.Pool().QueryRow(ctx, `SELECT output FROM reviews WHERE uuid = $1`, reviewID).Scan(&output))
	assert.Empty(t, output)
}

func TestIntegrationLegacyReviewExplicitConversion(t *testing.T) { //nolint:paralleltest // shares the roborev schema in the PostgreSQL database at TEST_POSTGRES_URL
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
			_, err := pool.Pool().Exec(ctx, `INSERT INTO reviews (uuid, job_uuid, job_enqueued_at, job_source_machine_id, agent, prompt, output, updated_by_machine_id) VALUES ($1, $2, (SELECT enqueued_at FROM review_jobs WHERE uuid = $2), (SELECT source_machine_id FROM review_jobs WHERE uuid = $2), 'test', 'prompt', 'The member wrote prose that roborev cannot read back.', $3)`, memberReviewID, memberID, defaultTestMachineID)
			require.NoError(t, err)
			require.NoError(t, pool.migrateLegacyReviews(ctx, false))
			clean := jsontext.Value(`{"schema_version":2,"summary":"Current review.","verdict":"pass","findings":[]}`)
			require.NoError(t, pool.ResolveLegacyReview(ctx, memberReviewID, clean))
			_, err = pool.Pool().Exec(ctx, `INSERT INTO legacy_reviews (uuid, record, migration_error, resolved_at) SELECT $1, record, migration_error, resolved_at FROM legacy_reviews WHERE uuid = $2`, uuid.New(), memberReviewID)
			require.NoError(t, err)
		}
	}
	incoming := SyncableReview{UUID: reviewID, JobUUID: jobID, JobEnqueuedAt: testJobEnqueuedAt(t, pool.Pool(), jobID), JobSourceMachineID: testJobSourceMachineID(t, pool.Pool(), jobID), Agent: "test", Output: "Legacy finding", UpdatedByMachineID: defaultTestMachineID, CreatedAt: time.Now()}
	_, err = pool.Pool().Exec(ctx, `INSERT INTO reviews (uuid, job_uuid, job_enqueued_at, job_source_machine_id, agent, prompt, output, updated_by_machine_id) VALUES ($1, $2, (SELECT enqueued_at FROM review_jobs WHERE uuid = $2), (SELECT source_machine_id FROM review_jobs WHERE uuid = $2), 'test', 'original prompt', 'Legacy finding', $3)`, reviewID, jobID, defaultTestMachineID)
	require.NoError(t, err)
	require.NoError(t, pool.migrateLegacyReviews(ctx, false))
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
	pulled, _, err := pool.PullReviews(ctx, defaultTestMachineID, map[uuid.UUID]struct{}{jobID: {}}, "", 10)
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

func TestIntegrationLegacyReviewAutomaticConversion(t *testing.T) { //nolint:paralleltest // shares the roborev schema in the PostgreSQL database at TEST_POSTGRES_URL
	pool := openTestPgPool(t)
	ctx := t.Context()
	repoID := createTestRepo(t, pool.Pool(), TestRepoOpts{})
	commitID := createTestCommit(t, pool.Pool(), TestCommitOpts{RepoID: repoID})
	insert := func(markdown string, verdict, closed bool) uuid.UUID {
		reviewID := insertLegacyScanReview(t, pool, repoID, commitID, uuid.New(), "0 seconds", closed)
		_, err := pool.Pool().Exec(ctx, `UPDATE reviews SET prompt = 'original prompt', output = $2, verdict_bool = $3 WHERE uuid = $1`, reviewID, markdown, verdict)
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
	wantDocument := `{"schema_version":3,"summary":"The change adds a save routine.","findings":[
		{"severity":"high","kind":null,"topic":null,"confidence":null,"problem":"The write is not atomic.","fix":"Write to a temporary file and rename it.","file":null,"start_line":null,"end_line":null,"location":"store/save.go:88"},
		{"severity":"low","kind":null,"topic":null,"confidence":null,"problem":"The comment is stale.","fix":"Update the comment.","file":null,"start_line":null,"end_line":null,"location":"store/save.go:12"}]}`

	// The mirror upgrade converts what roborev can read back and archives the rest.
	upgraded := insert(legacyFindingsMarkdown, false, true)
	upgradedProse := insert(legacyProseMarkdown, false, false)
	require.NoError(t, pool.migrateLegacyReviews(ctx, false))
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

func TestIntegrationRestoreLegacyAndImport(t *testing.T) { //nolint:paralleltest // shares the roborev schema in the PostgreSQL database at TEST_POSTGRES_URL
	pool := openTestPgPool(t)
	ctx := t.Context()
	repoID := createTestRepo(t, pool.Pool(), TestRepoOpts{})
	commitID := createTestCommit(t, pool.Pool(), TestCommitOpts{RepoID: repoID})
	jobID, reviewID := uuid.New(), uuid.New()
	createTestJob(t, pool.Pool(), TestJobOpts{UUID: jobID, RepoID: repoID, CommitID: commitID})
	_, err := pool.Pool().Exec(ctx, `INSERT INTO reviews (uuid, job_uuid, job_enqueued_at, job_source_machine_id, agent, prompt, output, closed, verdict_bool, updated_by_machine_id)
 VALUES ($1, $2, (SELECT enqueued_at FROM review_jobs WHERE uuid = $2), (SELECT source_machine_id FROM review_jobs WHERE uuid = $2), 'test', 'prompt', 'The write loses data. Keep the old file until rename.', true, NULL, $3)`, reviewID, jobID, defaultTestMachineID)
	require.NoError(t, err)
	require.NoError(t, pool.migrateLegacyReviews(ctx, false))
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
	// A peer can reopen its cached legacy envelope without downgrading the
	// converted content, just as it can close it after conversion.
	require.NoError(t, pool.UpsertReview(ctx, SyncableReview{UUID: reviewID, JobUUID: jobID, JobEnqueuedAt: testJobEnqueuedAt(t, pool.Pool(), jobID), JobSourceMachineID: testJobSourceMachineID(t, pool.Pool(), jobID), Agent: "test", StructuredOutput: raw, Closed: false, CreatedAt: time.Now(), UpdatedByMachineID: defaultTestMachineID}))
	require.NoError(t, pool.Pool().QueryRow(ctx, `SELECT structured_output, closed FROM reviews WHERE uuid = $1`, reviewID).Scan(&raw, &closed))
	doc, err = structuredreview.Decode(raw)
	require.NoError(t, err)
	assert.Nil(t, doc.Legacy)
	assert.Equal(t, "Converted.", doc.Summary)
	assert.False(t, closed)
}

func TestIntegrationLegacyMigrationBatches(t *testing.T) { //nolint:paralleltest // shares the roborev schema in the PostgreSQL database at TEST_POSTGRES_URL
	env := newIntegrationEnv(t)
	pool, ctx := env.Pool, t.Context()
	repoID := createTestRepo(t, pool.Pool(), TestRepoOpts{})
	commitID := createTestCommit(t, pool.Pool(), TestCommitOpts{RepoID: repoID})
	for range 205 {
		insertLegacyScanReview(t, pool, repoID, commitID, uuid.New(), "0 seconds", true)
	}
	require.NoError(t, pool.migrateLegacyReviews(ctx, false))
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

func TestIntegrationLegacyMaintenance(t *testing.T) { //nolint:paralleltest // shares the PostgreSQL test schema
	for _, stopEarly := range []bool{false, true} {
		name := "retry and sync"
		if stopEarly {
			name = "stop during retry"
		}
		t.Run(name, func(t *testing.T) {
			env := newIntegrationEnv(t)
			pool, ctx := env.Pool, t.Context()
			repoID := createTestRepo(t, pool.Pool(), TestRepoOpts{})
			commitID := createTestCommit(t, pool.Pool(), TestCommitOpts{RepoID: repoID})
			// Leave fewer than a batch after the checkpoint, even if sync adds a review.
			for range 102 {
				insertLegacyScanReview(t, pool, repoID, commitID, uuid.New(), "0 seconds", false)
			}
			_, err := pool.Pool().Exec(ctx, `DELETE FROM sync_metadata WHERE key = $1`, legacyReviewScanKey)
			require.NoError(t, err)
			var checkpoint, earlier, locked uuid.UUID
			require.NoError(t, pool.Pool().QueryRow(ctx, `SELECT uuid FROM reviews ORDER BY uuid OFFSET 99 LIMIT 1`).Scan(&checkpoint))
			require.NoError(t, pool.Pool().QueryRow(ctx, `SELECT uuid FROM reviews ORDER BY uuid OFFSET 100 LIMIT 1`).Scan(&earlier))
			require.NoError(t, pool.Pool().QueryRow(ctx, `SELECT uuid FROM reviews ORDER BY uuid OFFSET 101 LIMIT 1`).Scan(&locked))
			tx, err := pool.Pool().Begin(ctx)
			require.NoError(t, err)
			t.Cleanup(func() { _ = tx.Rollback(context.WithoutCancel(ctx)) })
			_, err = tx.Exec(ctx, `SELECT uuid FROM reviews WHERE uuid = $1 FOR UPDATE`, locked)
			require.NoError(t, err)

			db := env.openDB("maintenance.db")
			worker := startSyncWorker(t, db, env.pgURL, "maintenance-test", "100ms")
			// PostgreSQL persists the checkpoint outside synctest's control.
			waitCondition(t, 10*time.Second, "saved maintenance checkpoint", func() (bool, error) {
				var stored string
				if err := pool.Pool().QueryRow(ctx, `SELECT value FROM sync_metadata WHERE key = $1`, legacyReviewScanKey).Scan(&stored); err != nil {
					return false, err
				}
				var state legacyReviewScanState
				if err := json.Unmarshal([]byte(stored), &state); err != nil {
					return false, err
				}
				return state.Run != nil && state.Run.After != nil && *state.Run.After == checkpoint, nil
			})
			// This pass must fail at the held row, rolling back locks on earlier rows.
			err = pool.migrateLegacyReviews(ctx, true)
			require.ErrorContains(t, err, "could not obtain lock")
			if stopEarly {
				done := make(chan struct{})
				go func() { worker.Stop(); close(done) }()
				// The worker and PostgreSQL run outside synctest's control.
				require.Eventually(t, func() bool {
					select {
					case <-done:
						return true
					default:
						return false
					}
				}, 10*time.Second, 50*time.Millisecond, "worker did not stop during maintenance retry")
				return
			}

			writeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			var jobID, owner uuid.UUID
			var enqueued time.Time
			require.NoError(t, pool.Pool().QueryRow(ctx, `SELECT job_uuid, job_enqueued_at, job_source_machine_id FROM reviews WHERE uuid = $1`, earlier).Scan(&jobID, &enqueued, &owner))
			err = pool.UpsertReview(writeCtx, SyncableReview{
				UUID: earlier, JobUUID: jobID, JobEnqueuedAt: enqueued, JobSourceMachineID: owner,
				Agent: "test", Closed: true, UpdatedByMachineID: defaultTestMachineID, CreatedAt: time.Now(),
				StructuredOutput: jsontext.Value(`{"schema_version":2,"summary":"Updated review.","verdict":"pass","findings":[]}`),
			})
			require.NoError(t, err)
			var closed bool
			require.NoError(t, pool.Pool().QueryRow(ctx, `SELECT closed FROM reviews WHERE uuid = $1`, earlier).Scan(&closed))
			assert.True(t, closed)
			repo, err := db.GetOrCreateRepo(env.TmpDir, "git@example.test:synthetic/repo.git")
			require.NoError(t, err)
			_, review := createCompletedReview(t, db, repo.ID, "abc123def456", "Author", "Subject", "prompt", "output")
			require.NotNil(t, review.UUID)
			// PostgreSQL receives scheduled sync writes outside synctest's control.
			waitCondition(t, 10*time.Second, "scheduled review push", func() (bool, error) {
				var exists bool
				err := pool.Pool().QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM reviews WHERE uuid = $1)`, *review.UUID).Scan(&exists)
				return exists, err
			})
			_, err = worker.SyncNow()
			require.NoError(t, err)
			var stored string
			require.NoError(t, pool.Pool().QueryRow(ctx, `SELECT value FROM sync_metadata WHERE key = $1`, legacyReviewScanKey).Scan(&stored))
			var state legacyReviewScanState
			require.NoError(t, json.Unmarshal([]byte(stored), &state))
			require.NotNil(t, state.Run)
			require.NotNil(t, state.Run.After)
			assert.Equal(t, checkpoint, *state.Run.After)

			require.NoError(t, tx.Rollback(ctx))
			// PostgreSQL persists completion outside synctest's control.
			waitCondition(t, 10*time.Second, "maintenance completed after retry", func() (bool, error) {
				var raw string
				if err := pool.Pool().QueryRow(ctx, `SELECT value FROM sync_metadata WHERE key = $1`, legacyReviewScanKey).Scan(&raw); err != nil {
					return false, err
				}
				var current legacyReviewScanState
				if err := json.Unmarshal([]byte(raw), &current); err != nil {
					return false, err
				}
				return current.Run == nil && current.Done != nil, nil
			})
			var archived bool
			require.NoError(t, pool.Pool().QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM legacy_reviews WHERE uuid = $1)`, locked).Scan(&archived))
			assert.True(t, archived)
		})
	}
}

func TestIntegrationLegacyMaintenancePassProgress(t *testing.T) { //nolint:paralleltest // shares the PostgreSQL test schema
	t.Run("structured scan checkpoint", func(t *testing.T) {
		env := newIntegrationEnv(t)
		pool, ctx := env.Pool, t.Context()
		repoID := createTestRepo(t, pool.Pool(), TestRepoOpts{})
		commitID := createTestCommit(t, pool.Pool(), TestCommitOpts{RepoID: repoID})
		for range 101 {
			insertLegacyScanReview(t, pool, repoID, commitID, uuid.New(), "0 seconds", false)
		}
		_, err := pool.Pool().Exec(ctx, `UPDATE reviews SET output = '', structured_output = '{"schema_version":2,"summary":"Review.","verdict":"pass","findings":[]}'`)
		require.NoError(t, err)
		_, err = pool.Pool().Exec(ctx, `DELETE FROM sync_metadata WHERE key = $1`, legacyReviewScanKey)
		require.NoError(t, err)
		var locked uuid.UUID
		require.NoError(t, pool.Pool().QueryRow(ctx, `SELECT uuid FROM reviews ORDER BY uuid DESC LIMIT 1`).Scan(&locked))
		tx, err := pool.Pool().Begin(ctx)
		require.NoError(t, err)
		t.Cleanup(func() { _ = tx.Rollback(context.WithoutCancel(ctx)) })
		_, err = tx.Exec(ctx, `SELECT 1 FROM reviews WHERE uuid = $1 FOR UPDATE`, locked)
		require.NoError(t, err)
		err = pool.legacyMaintenancePass(ctx)
		var retry *backoff.RetryAfterError
		require.ErrorAs(t, err, &retry)
		assert.Zero(t, retry.Duration)
	})

	t.Run("completed scan before blocked restore", func(t *testing.T) {
		env := newIntegrationEnv(t)
		pool, ctx := env.Pool, t.Context()
		repoID := createTestRepo(t, pool.Pool(), TestRepoOpts{})
		commitID := createTestCommit(t, pool.Pool(), TestCommitOpts{RepoID: repoID})
		blocked := uuid.MustParse("00000000-0000-0000-0000-000000000001")
		scanned := uuid.MustParse("ffffffff-ffff-ffff-ffff-ffffffffffff")
		insertLegacyScanReview(t, pool, repoID, commitID, blocked, "0 seconds", false)
		_, err := pool.Pool().Exec(ctx, `INSERT INTO legacy_reviews (uuid, record, migration_error) SELECT uuid, to_jsonb(r), 'Unstructured historical review' FROM reviews r WHERE uuid = $1`, blocked)
		require.NoError(t, err)
		_, err = pool.Pool().Exec(ctx, `DELETE FROM reviews WHERE uuid = $1`, blocked)
		require.NoError(t, err)
		insertLegacyScanReview(t, pool, repoID, commitID, scanned, "0 seconds", false)
		_, err = pool.Pool().Exec(ctx, `DELETE FROM sync_metadata WHERE key = $1`, legacyReviewScanKey)
		require.NoError(t, err)
		tx, err := pool.Pool().Begin(ctx)
		require.NoError(t, err)
		t.Cleanup(func() { _ = tx.Rollback(context.WithoutCancel(ctx)) })
		_, err = tx.Exec(ctx, `INSERT INTO reviews (uuid, job_uuid, agent, prompt, output, updated_by_machine_id) SELECT $1, (record->>'job_uuid')::uuid, 'test', 'prompt', '', $2 FROM legacy_reviews WHERE uuid = $1`, blocked, defaultTestMachineID)
		require.NoError(t, err)
		passCtx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		err = pool.legacyMaintenancePass(passCtx)
		var retry *backoff.RetryAfterError
		require.ErrorAs(t, err, &retry)
		assert.Zero(t, retry.Duration)
		var archived bool
		require.NoError(t, pool.Pool().QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM legacy_reviews WHERE uuid = $1)`, scanned).Scan(&archived))
		assert.True(t, archived)
		var stored string
		require.NoError(t, pool.Pool().QueryRow(ctx, `SELECT value FROM sync_metadata WHERE key = $1`, legacyReviewScanKey).Scan(&stored))
		var state legacyReviewScanState
		require.NoError(t, json.Unmarshal([]byte(stored), &state))
		assert.Nil(t, state.Run)
		assert.NotNil(t, state.Done)
	})

	t.Run("restore progress then no progress", func(t *testing.T) {
		env := newIntegrationEnv(t)
		pool, ctx := env.Pool, t.Context()
		repoID := createTestRepo(t, pool.Pool(), TestRepoOpts{})
		commitID := createTestCommit(t, pool.Pool(), TestCommitOpts{RepoID: repoID})
		ids := []uuid.UUID{uuid.MustParse("00000000-0000-0000-0000-000000000001"), uuid.MustParse("ffffffff-ffff-ffff-ffff-ffffffffffff")}
		for _, id := range ids {
			insertLegacyScanReview(t, pool, repoID, commitID, id, "0 seconds", false)
			_, err := pool.Pool().Exec(ctx, `INSERT INTO legacy_reviews (uuid, record, migration_error) SELECT uuid, to_jsonb(r), 'Unstructured historical review' FROM reviews r WHERE uuid = $1`, id)
			require.NoError(t, err)
			_, err = pool.Pool().Exec(ctx, `DELETE FROM reviews WHERE uuid = $1`, id)
			require.NoError(t, err)
		}
		tx, err := pool.Pool().Begin(ctx)
		require.NoError(t, err)
		t.Cleanup(func() { _ = tx.Rollback(context.WithoutCancel(ctx)) })
		_, err = tx.Exec(ctx, `INSERT INTO reviews (uuid, job_uuid, agent, prompt, output, updated_by_machine_id) SELECT $1, (record->>'job_uuid')::uuid, 'test', 'prompt', '', $2 FROM legacy_reviews WHERE uuid = $1`, ids[1], defaultTestMachineID)
		require.NoError(t, err)
		passCtx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		err = pool.legacyMaintenancePass(passCtx)
		var retry *backoff.RetryAfterError
		require.ErrorAs(t, err, &retry)
		assert.Zero(t, retry.Duration)
		var restored bool
		require.NoError(t, pool.Pool().QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM reviews WHERE uuid = $1)`, ids[0]).Scan(&restored))
		assert.True(t, restored)
		passCtx2, cancel2 := context.WithTimeout(ctx, time.Second)
		defer cancel2()
		err = pool.legacyMaintenancePass(passCtx2)
		require.Error(t, err)
		assert.NotErrorAs(t, err, &retry)
	})
}

func TestIntegrationMirrorArchivesOriginalBeforeSyncWrite(t *testing.T) { //nolint:paralleltest // shares the PostgreSQL test schema
	for _, tc := range []struct {
		name, document string
		resolved       bool
	}{
		{"structured single", `{"schema_version":2,"summary":"Updated review.","verdict":"pass","findings":[]}`, true},
		{"legacy single", `{"schema_version":0,"legacy":{"markdown":"Local text","recorded_verdict":null}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newIntegrationEnv(t)
			pool, ctx := env.Pool, t.Context()
			repoID := createTestRepo(t, pool.Pool(), TestRepoOpts{})
			commitID := createTestCommit(t, pool.Pool(), TestCommitOpts{RepoID: repoID})
			id := insertLegacyScanReview(t, pool, repoID, commitID, uuid.New(), "0 seconds", false)
			var job, owner uuid.UUID
			var enqueued time.Time
			require.NoError(t, pool.Pool().QueryRow(ctx, `SELECT job_uuid, job_enqueued_at, job_source_machine_id FROM reviews WHERE uuid = $1`, id).Scan(&job, &enqueued, &owner))
			review := SyncableReview{UUID: id, JobUUID: job, JobEnqueuedAt: enqueued, JobSourceMachineID: owner, Agent: "test", Closed: true, UpdatedByMachineID: defaultTestMachineID, CreatedAt: time.Now(), StructuredOutput: jsontext.Value(tc.document)}
			require.NoError(t, pool.UpsertReview(ctx, review))
			var output, prompt string
			var resolved bool
			require.NoError(t, pool.Pool().QueryRow(ctx, `SELECT record->>'output', record->>'prompt', resolved_at IS NOT NULL FROM legacy_reviews WHERE uuid = $1`, id).Scan(&output, &prompt, &resolved))
			assert.Equal(t, "Preserve this historical text.", output)
			assert.Equal(t, "prompt", prompt)
			assert.Equal(t, tc.resolved, resolved)
		})
	}
}

func TestIntegrationLegacyReviewConcurrentScan(t *testing.T) { //nolint:paralleltest // shares the PostgreSQL test schema
	env := newIntegrationEnv(t)
	pool, ctx := env.Pool, t.Context()
	repoID := createTestRepo(t, pool.Pool(), TestRepoOpts{})
	commitID := createTestCommit(t, pool.Pool(), TestCommitOpts{RepoID: repoID})
	ids := []uuid.UUID{uuid.MustParse("00000000-0000-0000-0000-000000000001"), uuid.MustParse("ffffffff-ffff-ffff-ffff-ffffffffffff")}
	var reviews []SyncableReview
	for i, id := range ids {
		insertLegacyScanReview(t, pool, repoID, commitID, id, "0 seconds", false)
		var job, owner uuid.UUID
		var enqueued time.Time
		require.NoError(t, pool.Pool().QueryRow(ctx, `SELECT job_uuid, job_enqueued_at, job_source_machine_id FROM reviews WHERE uuid = $1`, id).Scan(&job, &enqueued, &owner))
		document := jsontext.Value(`{"schema_version":2,"summary":"Updated review.","verdict":"pass","findings":[]}`)
		if i == 0 {
			document = jsontext.Value(`{"schema_version":0,"legacy":{"markdown":"Local text","recorded_verdict":null}}`)
		}
		reviews = append(reviews, SyncableReview{UUID: id, JobUUID: job, JobEnqueuedAt: enqueued, JobSourceMachineID: owner, Agent: "test", Closed: true, UpdatedByMachineID: defaultTestMachineID, CreatedAt: time.Now(), StructuredOutput: document})
	}
	_, err := pool.Pool().Exec(ctx, `DELETE FROM sync_metadata WHERE key = $1`, legacyReviewScanKey)
	require.NoError(t, err)
	tx, err := pool.Pool().Begin(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback(context.WithoutCancel(ctx)) })
	_, err = tx.Exec(ctx, `SELECT 1 FROM reviews WHERE uuid = $1 FOR UPDATE`, ids[1])
	require.NoError(t, err)
	write := make(chan error, 1)
	go func() { _, err := pool.BatchUpsertReviews(ctx, reviews); write <- err }()
	waitCondition(t, 10*time.Second, "batch waiting for second review", func() (bool, error) {
		var waiting bool
		err := pool.Pool().QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND query LIKE '%WITH locked_job AS MATERIALIZED%')`).Scan(&waiting)
		return waiting, err
	})
	scan := make(chan error, 1)
	go func() { scan <- pool.migrateLegacyReviews(ctx, false) }()
	waitCondition(t, 10*time.Second, "scan waiting for batch", func() (bool, error) {
		var waiting bool
		err := pool.Pool().QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND query LIKE '%SELECT r.uuid, r.output, r.structured_output%')`).Scan(&waiting)
		return waiting, err
	})
	require.NoError(t, tx.Rollback(ctx))
	require.NoError(t, <-write)
	require.NoError(t, <-scan)
	var count int
	require.NoError(t, pool.Pool().QueryRow(ctx, `SELECT count(*) FROM legacy_reviews WHERE record->>'output' = 'Preserve this historical text.' AND record->>'prompt' = 'prompt'`).Scan(&count))
	assert.Equal(t, 2, count)
}

func TestIntegrationLegacyResolveConcurrentUpload(t *testing.T) { //nolint:paralleltest // shares the PostgreSQL test schema
	env := newIntegrationEnv(t)
	pool, ctx := env.Pool, t.Context()
	repoID := createTestRepo(t, pool.Pool(), TestRepoOpts{})
	commitID := createTestCommit(t, pool.Pool(), TestCommitOpts{RepoID: repoID})
	id := insertLegacyScanReview(t, pool, repoID, commitID, uuid.New(), "0 seconds", false)
	var job, owner uuid.UUID
	var enqueued time.Time
	require.NoError(t, pool.Pool().QueryRow(ctx, `SELECT job_uuid, job_enqueued_at, job_source_machine_id FROM reviews WHERE uuid = $1`, id).Scan(&job, &enqueued, &owner))
	legacy := `{"schema_version":0,"legacy":{"markdown":"Local text","recorded_verdict":null}}`
	_, err := pool.Pool().Exec(ctx, `INSERT INTO legacy_reviews (uuid, record, migration_error) SELECT uuid, to_jsonb(r), 'Unstructured historical review' FROM reviews r WHERE uuid = $1`, id)
	require.NoError(t, err)
	_, err = pool.Pool().Exec(ctx, `UPDATE reviews SET output = '', structured_output = $2 WHERE uuid = $1`, id, legacy)
	require.NoError(t, err)
	raw := jsontext.Value(`{"schema_version":2,"summary":"Converted review.","verdict":"pass","findings":[]}`)
	tx, err := pool.Pool().Begin(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback(context.WithoutCancel(ctx)) })
	_, err = tx.Exec(ctx, `SELECT 1 FROM reviews WHERE uuid = $1 FOR UPDATE`, id)
	require.NoError(t, err)
	resolve := make(chan error, 1)
	upload := make(chan error, 1)
	go func() { resolve <- pool.ResolveLegacyReview(ctx, id, raw) }()
	// The prelock query must wait before conversion can lock the archive.
	waitCondition(t, 10*time.Second, "conversion waiting for review", func() (bool, error) {
		var waiting bool
		err := pool.Pool().QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND query LIKE '%SELECT 1 FROM reviews WHERE uuid%FOR UPDATE%')`).Scan(&waiting)
		return waiting, err
	})
	_, err = tx.Exec(ctx, `UPDATE legacy_reviews SET migration_error = migration_error WHERE uuid = $1`, id)
	require.NoError(t, err)
	go func() {
		upload <- pool.UpsertReview(ctx, SyncableReview{UUID: id, JobUUID: job, JobEnqueuedAt: enqueued, JobSourceMachineID: owner, Agent: "test", Closed: true, UpdatedByMachineID: defaultTestMachineID, CreatedAt: time.Now(), StructuredOutput: raw})
	}()
	waitCondition(t, 10*time.Second, "upload waiting for review", func() (bool, error) {
		var waiting bool
		err := pool.Pool().QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND query LIKE '%WITH locked_job AS MATERIALIZED%')`).Scan(&waiting)
		return waiting, err
	})
	require.NoError(t, tx.Rollback(ctx))
	require.NoError(t, <-resolve)
	require.NoError(t, <-upload)
}
