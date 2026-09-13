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
