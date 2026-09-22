package storage

import (
	"encoding/json/jsontext"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRestoreLegacyHistoryAndImport(t *testing.T) {
	for _, archived := range []bool{false, true} {
		t.Run(map[bool]string{false: "direct upgrade", true: "already archived"}[archived], func(t *testing.T) {
			env := setupJobEnv(t, t.TempDir(), "legacy-head")
			legacy := seedLegacyMarkdownReview(t, env.db, env.repo.ID, "prose", "  The write loses data.\nKeep the old file until rename.\n", 0, true)
			unknown := seedLegacyMarkdownReview(t, env.db, env.repo.ID, "unknown", "Cannot infer a verdict from this historical text.", nil, false)
			convertible := seedLegacyMarkdownReview(t, env.db, env.repo.ID, "convertible", legacyFindingsMarkdown, 0, false)
			if archived {
				archiveAsOlderRelease(t, env.db)
			}
			var seq int
			var name, path string
			require.NoError(t, env.db.QueryRow("PRAGMA database_list").Scan(&seq, &name, &path))
			require.NoError(t, env.db.Close())
			db, err := Open(path)
			require.NoError(t, err)
			defer db.Close()
			review, err := db.GetReviewByJobIDWithFindingCounts(legacy.jobID)
			require.NoError(t, err)
			assert.True(t, review.Closed)
			assert.Equal(t, VerdictFail, review.Verdict())
			assert.Contains(t, review.Output, "Unstructured historical review")
			assert.Contains(t, review.Output, "  The write loses data.\nKeep the old file until rename.\n")
			assert.NotContains(t, review.Output, "No issues found")
			assert.Nil(t, review.Job.FindingCounts)
			u, err := db.GetReviewByJobID(unknown.jobID)
			require.NoError(t, err)
			assert.Equal(t, VerdictUnknown, u.Verdict())
			c, err := db.GetReviewByJobID(convertible.jobID)
			require.NoError(t, err)
			assert.NotContains(t, c.Output, "Unstructured historical review")
			require.NoError(t, db.restoreLegacyReviews())
			records, err := db.UnresolvedLegacyReviews()
			require.NoError(t, err)
			var archiveID int64
			for _, record := range records {
				if record.JobID == legacy.jobID {
					archiveID = record.ID
				}
			}
			require.NotZero(t, archiveID)
			require.Error(t, db.ResolveLegacyReview(archiveID, jsontext.Value(`{"summary":"invalid"}`)))
			unchanged, err := db.GetReviewByJobID(legacy.jobID)
			require.NoError(t, err)
			assert.Equal(t, review.Output, unchanged.Output)
			raw := jsontext.Value(`{"schema_version":1,"summary":"Converted.","findings":[{"severity":"high","problem":"The write loses data.","fix":"Keep the old file until rename.","location":null}]}`)
			require.NoError(t, db.ResolveLegacyReview(archiveID, raw))
			converted, err := db.GetReviewByJobID(legacy.jobID)
			require.NoError(t, err)
			assert.Equal(t, review.ID, converted.ID)
			assert.Equal(t, review.UUID, converted.UUID)
			assert.True(t, converted.Closed)
			assert.Contains(t, converted.Output, "The write loses data.")
			require.ErrorIs(t, db.ResolveLegacyReview(archiveID, raw), ErrReviewNotLegacy)
			require.ErrorIs(t, db.MigrateReview(converted.ID, raw), ErrReviewNotLegacy)
		})
	}
}

func TestLegacySyncCannotReplaceConvertedReview(t *testing.T) {
	env := setupJobEnv(t, t.TempDir(), "sync-head")
	incoming := PulledReview{
		UUID: testUUID("legacy-sync-document"), JobUUID: *env.job.UUID, Agent: "test", CreatedAt: time.Now(), UpdatedAt: time.Now(),
		StructuredOutput: jsontext.Value(`{"schema_version":0,"legacy":{"markdown":"The write loses data.","recorded_verdict":null}}`),
	}
	require.NoError(t, env.db.UpsertPulledReview(incoming))
	review, err := env.db.GetReviewByJobID(env.job.ID)
	require.NoError(t, err)
	assert.Equal(t, VerdictUnknown, review.Verdict())
	records, err := env.db.UnresolvedLegacyReviews()
	require.NoError(t, err)
	require.Len(t, records, 1)
	raw := jsontext.Value(`{"schema_version":1,"summary":"Converted.","findings":[]}`)
	require.NoError(t, env.db.ResolveLegacyReview(records[0].ID, raw))
	incoming.UpdatedAt = incoming.UpdatedAt.Add(time.Hour)
	require.NoError(t, env.db.UpsertPulledReview(incoming))
	converted, err := env.db.GetReviewByJobID(env.job.ID)
	require.NoError(t, err)
	assert.Equal(t, "Converted.", converted.StructuredOutput["summary"])
	assert.Equal(t, review.ID, converted.ID)
}

func TestMigrateLegacySynthesisValidatesSources(t *testing.T) {
	env := setupJobEnv(t, t.TempDir(), "synthesis-head")
	jobID := seedLegacyPanel(t, env, "legacy-source-check")
	_, err := env.db.Exec(`INSERT INTO reviews (job_id, agent, prompt, output, verdict_bool, uuid) VALUES (?, 'test', 'prompt', ?, 0, ?)`, jobID, legacyFindingsMarkdown, testUUID("legacy-source-check"))
	require.NoError(t, err)
	require.NoError(t, env.db.migrateLegacyReviews())
	require.NoError(t, env.db.restoreLegacyReviews())
	review, err := env.db.GetReviewByJobID(jobID)
	require.NoError(t, err)
	invalid := jsontext.Value(`{"schema_version":1,"summary":"Converted.","findings":[{"severity":"high","problem":"The write loses data.","fix":"Keep the old file.","location":null,"sources":[3]}]}`)
	require.ErrorContains(t, env.db.MigrateReview(review.ID, invalid), "only 2 reviews")
	unchanged, err := env.db.GetReviewByJobID(jobID)
	require.NoError(t, err)
	assert.Equal(t, review.Output, unchanged.Output)
	valid := jsontext.Value(`{"schema_version":1,"summary":"Converted.","findings":[{"severity":"high","problem":"The write loses data.","fix":"Keep the old file.","location":null,"sources":[2]}]}`)
	require.NoError(t, env.db.MigrateReview(review.ID, valid))
	converted, err := env.db.GetReviewByJobID(jobID)
	require.NoError(t, err)
	assert.Contains(t, converted.Output, "Reported by:** test (design)")
}

func TestRestoreSynthesisKeepsExistingFindings(t *testing.T) {
	env := setupJobEnv(t, t.TempDir(), "synthesis-existing-json")
	jobID := seedLegacyPanel(t, env, "existing-json")
	raw := `{"schema_version":2,"summary":"Existing assessment.","verdict":"fail","findings":[{"severity":"high","problem":"The write loses data.","fix":"Keep the old file.","location":null,"sources":[2]}]}`
	_, err := env.db.Exec(`INSERT INTO reviews (job_id, agent, prompt, output, structured_output, verdict_bool, uuid) VALUES (?, 'test', 'prompt', 'Older rendered text without machine-readable attribution.', ?, 0, ?)`, jobID, raw, testUUID("existing-json-review"))
	require.NoError(t, err)
	require.NoError(t, env.db.migrateLegacyReviews())
	require.NoError(t, env.db.restoreLegacyReviews())
	review, err := env.db.GetReviewByJobID(jobID)
	require.NoError(t, err)
	assert.NotContains(t, review.Output, "Unstructured historical review")
	assert.Contains(t, review.Output, "The write loses data.")
	assert.Contains(t, review.Output, "Reported by:** test (design)")
	var stored string
	require.NoError(t, env.db.QueryRow(`SELECT structured_output FROM reviews WHERE job_id = ?`, jobID).Scan(&stored))
	assert.JSONEq(t, `{"schema_version":2,"summary":"Existing assessment.","verdict":"fail","findings":[{"severity":"high","problem":"The write loses data.","fix":"Keep the old file.","location":null,"sources":[2]}],"source_labels":["test (security)","test (design)"]}`, stored)
	assert.Equal(t, "Existing assessment.", review.StructuredOutput["summary"])
	assert.Equal(t, VerdictFail, review.Verdict())
}
