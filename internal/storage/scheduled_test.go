package storage

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestScheduledAnalysisHistorySkipsInvalidAnalysisFiles(t *testing.T) {
	db, repo := setupDBAndRepo(t, "scheduled-history-invalid-files")

	valid, err := db.EnqueueJob(EnqueueOpts{
		RepoID:        repo.ID,
		Prompt:        "scheduled analysis",
		Agent:         "test",
		AnalysisType:  "complexity",
		AnalysisFiles: []string{"pkg/valid.go"},
	})
	require.NoError(t, err)

	malformed, err := db.EnqueueJob(EnqueueOpts{
		RepoID:       repo.ID,
		Prompt:       "malformed metadata",
		Agent:        "test",
		AnalysisType: "complexity",
	})
	require.NoError(t, err)
	_, err = db.Exec("UPDATE review_jobs SET analysis_files = ? WHERE id = ?", "not-json", malformed.ID)
	require.NoError(t, err)

	nonArray, err := db.EnqueueJob(EnqueueOpts{
		RepoID:       repo.ID,
		Prompt:       "non-array metadata",
		Agent:        "test",
		AnalysisType: "complexity",
	})
	require.NoError(t, err)
	_, err = db.Exec("UPDATE review_jobs SET analysis_files = ? WHERE id = ?", `{"file":"pkg/object.go"}`, nonArray.ID)
	require.NoError(t, err)

	for _, value := range []string{`["pkg/null.go", null]`, `["pkg/mixed.go", 1]`} {
		mixed, err := db.EnqueueJob(EnqueueOpts{
			RepoID:       repo.ID,
			Prompt:       "mixed metadata",
			Agent:        "test",
			AnalysisType: "complexity",
		})
		require.NoError(t, err)
		_, err = db.Exec("UPDATE review_jobs SET analysis_files = ? WHERE id = ?", value, mixed.ID)
		require.NoError(t, err)
	}

	history, err := db.ScheduledAnalysisHistoryForRepo(repo.ID)
	require.NoError(t, err)
	require.Len(t, history, 1)
	require.Len(t, history[ScheduledAnalysisKey{Path: "pkg/valid.go", Type: "complexity"}], 1)
	require.Equal(t, valid.ID, history[ScheduledAnalysisKey{Path: "pkg/valid.go", Type: "complexity"}][0].ID)
}
