package storage

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLegacyReviewArchiveAndExplicitConversion(t *testing.T) {
	env := setupJobEnv(t, "/tmp/legacy-review", "legacy-head")
	claimJob(t, env.db, "worker")
	_, err := env.db.Exec(`INSERT INTO reviews (job_id, agent, prompt, output, uuid)
 VALUES (?, 'test', 'prompt', 'A finding without a severity or fix.', '11111111-1111-4111-8111-111111111111')`, env.job.ID)
	require.NoError(t, err)
	require.NoError(t, env.db.migrateLegacyReviews())
	records, err := env.db.UnresolvedLegacyReviews()
	require.NoError(t, err)
	require.Len(t, records, 1)
	assert.Contains(t, records[0].Output, "A finding without a severity or fix.")
	assert.Contains(t, records[0].Reason, "AI conversion required")
	_, err = env.db.GetReviewByJobID(env.job.ID)
	require.Error(t, err)
	require.NoError(t, env.db.migrateLegacyReviews())
	records, err = env.db.UnresolvedLegacyReviews()
	require.NoError(t, err)
	require.Len(t, records, 1)
	require.Error(t, env.db.ResolveLegacyReview(records[0].ID, json.RawMessage(`not JSON`)))
	raw := json.RawMessage(`{"schema_version":2,"summary":"Converted review.","verdict":"fail","findings":[{"severity":"high","problem":"The operation loses a record.","fix":"Keep the record until completion.","location":null}]}`)
	require.NoError(t, env.db.ResolveLegacyReview(records[0].ID, raw))
	got, err := env.db.GetReviewByJobID(env.job.ID)
	require.NoError(t, err)
	assert.Equal(t, VerdictFail, got.Verdict())
	assert.Contains(t, got.Output, "The operation loses a record.")
	var stored, original string
	require.NoError(t, env.db.QueryRow(`SELECT output FROM reviews WHERE job_id = ?`, env.job.ID).Scan(&stored))
	assert.Empty(t, stored)
	require.NoError(t, env.db.QueryRow(`SELECT output FROM legacy_reviews WHERE archive_id = ?`, records[0].ID).Scan(&original))
	assert.Equal(t, "A finding without a severity or fix.", original)
	remaining, err := env.db.UnresolvedLegacyReviews()
	require.NoError(t, err)
	assert.Empty(t, remaining)
}

func TestReviewWritesRequireJSON(t *testing.T) {
	env := setupJobEnv(t, "/tmp/json-review", "json-head")
	claimJob(t, env.db, "worker")
	require.ErrorContains(t, env.db.CompleteJob(env.job.ID, "test", "prompt", "No issues found."), "JSON document is required")
	raw := json.RawMessage(`{"schema_version":2,"summary":"Clean change.","verdict":"pass","findings":[]}`)
	require.NoError(t, env.db.CompleteJobResult(env.job.ID, "test", "prompt", ReviewCompletion{Output: "rendered Markdown must not be stored", StructuredOutput: raw}))
	var output string
	var document []byte
	require.NoError(t, env.db.QueryRow(`SELECT output, structured_output FROM reviews WHERE job_id = ?`, env.job.ID).Scan(&output, &document))
	assert.Empty(t, output)
	assert.JSONEq(t, string(raw), string(document))
	got, err := env.db.GetReviewByJobID(env.job.ID)
	require.NoError(t, err)
	assert.Contains(t, got.Output, "Clean change.")
	assert.Equal(t, VerdictPass, got.Verdict())
}

func TestLegacyReviewWithJSONUsesDocument(t *testing.T) {
	env := setupJobEnv(t, "/tmp/dual-review", "dual-head")
	raw := `{"schema_version":2,"summary":"Canonical summary.","verdict":"pass","findings":[]}`
	_, err := env.db.Exec(`INSERT INTO reviews (job_id, agent, prompt, output, structured_output, uuid)
 VALUES (?, 'test', 'prompt', 'Obsolete Markdown', ?, '22222222-2222-4222-8222-222222222222')`, env.job.ID, raw)
	require.NoError(t, err)
	require.NoError(t, env.db.migrateLegacyReviews())
	got, err := env.db.GetReviewByJobID(env.job.ID)
	require.NoError(t, err)
	assert.Contains(t, got.Output, "Canonical summary.")
	assert.NotContains(t, got.Output, "Obsolete Markdown")
	records, err := env.db.UnresolvedLegacyReviews()
	require.NoError(t, err)
	assert.Empty(t, records)
	var original string
	require.NoError(t, env.db.QueryRow(`SELECT output FROM legacy_reviews WHERE job_id = ?`, env.job.ID).Scan(&original))
	assert.Equal(t, "Obsolete Markdown", original)
}

func TestLegacySynthesisRequiresKnownSources(t *testing.T) {
	env := setupJobEnv(t, "/tmp/legacy-synthesis", "synthesis-head")
	runID := testUUID("legacy-synthesis")
	_, err := env.db.Exec(`UPDATE review_jobs SET job_type = 'synthesis', panel_run_uuid = ?, panel_role = 'synthesis' WHERE id = ?`, runID, env.job.ID)
	require.NoError(t, err)
	_, err = env.db.EnqueueJob(EnqueueOpts{RepoID: env.repo.ID, GitRef: "synthesis-head", Agent: "test", PanelRunUUID: &runID, PanelRole: PanelRoleMember})
	require.NoError(t, err)
	raw := `{"schema_version":2,"summary":"Synthesis finding.","verdict":"fail","findings":[{"severity":"high","problem":"The operation loses a record.","fix":"Keep the record until completion.","location":null,"sources":[1]}]}`
	_, err = env.db.Exec(`INSERT INTO reviews (job_id, agent, prompt, output, structured_output, uuid) VALUES (?, 'test', 'prompt', 'Original synthesis', ?, ?)`, env.job.ID, raw, testUUID("synthesis-review"))
	require.NoError(t, err)
	require.NoError(t, env.db.migrateLegacyReviews())
	records, err := env.db.UnresolvedLegacyReviews()
	require.NoError(t, err)
	require.Len(t, records, 1)
	assert.Contains(t, records[0].Reason, "only 0 reviews")
	require.Len(t, records[0].Sources, 1)
	assert.Equal(t, "test", records[0].Sources[0].Agent)
	invalid := strings.Replace(raw, `"sources":[1]`, `"sources":[2]`, 1)
	require.ErrorContains(t, env.db.ResolveLegacyReview(records[0].ID, json.RawMessage(invalid)), "only 1 reviews")
	require.NoError(t, env.db.ResolveLegacyReview(records[0].ID, json.RawMessage(raw)))
	require.NoError(t, env.db.migrateLegacyReviews())
	got, err := env.db.GetReviewByJobID(env.job.ID)
	require.NoError(t, err)
	assert.Contains(t, got.Output, "test")
	var stored string
	require.NoError(t, env.db.QueryRow(`SELECT structured_output FROM reviews WHERE job_id = ?`, env.job.ID).Scan(&stored))
	assert.Contains(t, stored, `"source_labels":["test"]`)
}

func TestLegacyReviewResolvedBySync(t *testing.T) {
	env := setupJobEnv(t, "/tmp/legacy-sync", "sync-head")
	incoming := PulledReview{
		UUID: testUUID("legacy-sync-review"), JobUUID: *env.job.UUID,
		Agent: "test", Prompt: "prompt", Output: "Unstructured review.",
		UpdatedByMachineID: testUUID("remote-machine"), CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	require.NoError(t, env.db.UpsertPulledReview(incoming))
	_, err := env.db.GetReviewByJobID(env.job.ID)
	require.ErrorIs(t, err, ErrLegacyReviewMigration)
	incoming.StructuredOutput = json.RawMessage(`{"schema_version":2,"summary":"Converted elsewhere.","verdict":"pass","findings":[]}`)
	require.NoError(t, env.db.UpsertPulledReview(incoming))
	got, err := env.db.GetReviewByJobID(env.job.ID)
	require.NoError(t, err)
	assert.Contains(t, got.Output, "Converted elsewhere.")
	remaining, err := env.db.UnresolvedLegacyReviews()
	require.NoError(t, err)
	assert.Empty(t, remaining)
	var original string
	require.NoError(t, env.db.QueryRow(`SELECT output FROM legacy_reviews WHERE uuid = ?`, incoming.UUID).Scan(&original))
	assert.Equal(t, "Unstructured review.", original)
}
