package main

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

func TestLegacyReviewCommands(t *testing.T) {
	db, dir := testutil.OpenTestDBWithDir(t)
	repo, err := db.GetOrCreateRepo(filepath.Join(dir, "repo"))
	require.NoError(t, err)
	job := testutil.CreateCompletedReview(t, db, repo.ID, "test-head", "test", "No issues found.")
	_, err = db.Exec(`UPDATE reviews SET structured_output = NULL, output = 'Legacy review needing conversion' WHERE job_id = ?`, job.ID)
	require.NoError(t, err)
	require.NoError(t, db.Close())
	dbPath := filepath.Join(dir, "test.db")
	migrated, err := storage.Open(dbPath)
	require.NoError(t, err)
	require.NoError(t, migrated.Close())
	var out bytes.Buffer
	export := legacyReviewsCmd()
	export.SetOut(&out)
	export.SetArgs([]string{"--db", dbPath, "export"})
	require.NoError(t, export.Execute())
	var exported struct {
		Records []storage.LegacyReview `json:"records"`
	}
	require.NoError(t, json.Unmarshal(out.Bytes(), &exported))
	require.Len(t, exported.Records, 1)
	assert.Equal(t, "Legacy review needing conversion", exported.Records[0].Output)
	command := legacyReviewsCmd()
	command.SetIn(bytes.NewReader(testutil.ReviewFixtureJSON("Converted review.")))
	command.SetOut(&out)
	command.SetArgs([]string{"--db", dbPath, "import", "1"})
	require.NoError(t, command.Execute())
	check, err := storage.OpenReadOnly(dbPath)
	require.NoError(t, err)
	defer check.Close()
	review, err := check.GetReviewByJobID(job.ID)
	require.NoError(t, err)
	assert.Equal(t, "Converted review.", review.StructuredOutput["summary"])
}

func TestLegacyReviewsConvertCommand(t *testing.T) {
	db, dir := testutil.OpenTestDBWithDir(t)
	repo, err := db.GetOrCreateRepo(filepath.Join(dir, "repo"))
	require.NoError(t, err)
	convertible := testutil.CreateCompletedReview(t, db, repo.ID, "convertible-head", "test", "No issues found.")
	prose := testutil.CreateCompletedReview(t, db, repo.ID, "prose-head", "test", "No issues found.")
	// Archive both reviews as Markdown the way the first JSON release did,
	// before automatic conversion existed.
	for jobID, markdown := range map[int64]string{
		convertible.ID: "## Review Findings\n\n- **Severity**: Medium\n- **Location**: store/save.go:88\n" +
			"- **Problem**: The write is not atomic.\n- **Fix**: Rename a temporary file.\n\n## Summary\n\nThe change adds a save routine.",
		prose.ID: "The save routine looks risky. Consider a rename.",
	} {
		_, err = db.Exec(`INSERT INTO legacy_reviews (id, job_id, agent, prompt, output, created_at, closed, verdict_bool, uuid, updated_at, migration_error)
 SELECT id, job_id, agent, prompt, ?, created_at, closed, 0, uuid, updated_at, 'No valid review JSON document; AI conversion required'
 FROM reviews WHERE job_id = ?`, markdown, jobID)
		require.NoError(t, err)
		_, err = db.Exec(`DELETE FROM reviews WHERE job_id = ?`, jobID)
		require.NoError(t, err)
	}
	require.NoError(t, db.Close())
	dbPath := filepath.Join(dir, "test.db")

	run := func(args ...string) string {
		var out bytes.Buffer
		command := legacyReviewsCmd()
		command.SetOut(&out)
		command.SetArgs(append([]string{"--db", dbPath}, args...))
		require.NoError(t, command.Execute())
		return out.String()
	}

	assert.Equal(t, "Archived reviews needing conversion: 2\nWould convert: 1\nWould stay archived: 1\n  unrecognized_format: 1\n",
		run("convert", "--dry-run"))
	check, err := storage.OpenReadOnly(dbPath)
	require.NoError(t, err)
	_, err = check.GetReviewByJobID(convertible.ID)
	require.ErrorIs(t, err, storage.ErrLegacyReviewMigration, "a dry run restores nothing")
	require.NoError(t, check.Close())

	assert.Equal(t, "Archived reviews needing conversion: 2\nConverted and restored: 1\nLeft archived for export and import: 1\n  unrecognized_format: 1\n",
		run("convert"))
	assert.Equal(t, "Archived reviews needing conversion: 1\nConverted and restored: 0\nLeft archived for export and import: 1\n  unrecognized_format: 1\n",
		run("convert"), "a second run only sees what is still unresolved")

	check, err = storage.OpenReadOnly(dbPath)
	require.NoError(t, err)
	defer check.Close()
	review, err := check.GetReviewByJobID(convertible.ID)
	require.NoError(t, err)
	assert.Equal(t, "The change adds a save routine.", review.StructuredOutput["summary"])
	assert.Equal(t, storage.VerdictFail, review.Verdict())
	_, err = check.GetReviewByJobID(prose.ID)
	require.ErrorIs(t, err, storage.ErrLegacyReviewMigration)
}
