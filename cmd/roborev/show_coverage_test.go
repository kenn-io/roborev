package main

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
)

func TestShowReportsStoredFileCoverage(t *testing.T) {
	zero, excluded := 0, 3
	t.Run("text", func(t *testing.T) {
		repo := newTestGitRepo(t)
		repo.CommitFile("file.txt", "content", "initial")
		chdir(t, repo.Dir)
		review := storage.Review{
			ID: 1, JobID: 42, Output: "PASS", Agent: "test",
			FileCoverage: &storage.ReviewFileCoverage{Reviewed: &zero, Excluded: &excluded},
		}
		mockReviewDaemon(t, review)
		output := runShowCmd(t, "--job", "42")
		assert.Contains(t, output, "Files: 0 files reviewed, 3 excluded")
	})
	t.Run("unknown", func(t *testing.T) {
		repo := newTestGitRepo(t)
		repo.CommitFile("file.txt", "content", "initial")
		chdir(t, repo.Dir)
		mockReviewDaemon(t, storage.Review{ID: 1, JobID: 42, Output: "PASS", Agent: "test"})
		output := runShowCmd(t, "--job", "42")
		assert.NotContains(t, output, "Files:")
	})
	t.Run("json", func(t *testing.T) {
		repo := newTestGitRepo(t)
		repo.CommitFile("file.txt", "content", "initial")
		chdir(t, repo.Dir)
		review := storage.Review{
			ID: 1, JobID: 42, Output: "PASS", Agent: "test",
			FileCoverage: &storage.ReviewFileCoverage{Reviewed: &zero, Excluded: &excluded},
		}
		mockReviewDaemon(t, review)
		jsonOutput := runShowCmd(t, "--job", "42", "--json")
		var decoded storage.Review
		require.NoError(t, json.Unmarshal([]byte(jsonOutput), &decoded))
		require.NotNil(t, decoded.FileCoverage)
		assert.Equal(t, 0, *decoded.FileCoverage.Reviewed)
	})
}
