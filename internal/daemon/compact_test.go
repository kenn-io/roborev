// ABOUTME: Unit tests for compact job metadata handling
// ABOUTME: Tests reading, deleting, and validating compact metadata files
package daemon

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
)

func TestCompactSourceClosureRefreshesSearch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		setupTestEnv(t)
		server := newSearchHTTPServer(t, nil)
		job, _ := completeSearchIntegrationReview(t, server.db)
		reconciler := newRecordingSearchReconciler()
		server.searchReconciler = reconciler
		server.startSearch(t.Context())
		defer server.stopSearch()
		subscriber, events := server.broadcaster.Subscribe("")
		defer server.broadcaster.Unsubscribe(subscriber)
		source, err := server.db.GetReviewByJobID(job.ID)
		require.NoError(t, err)
		require.NotNil(t, source.UUID)
		compact, err := server.db.EnqueueJob(storage.EnqueueOpts{RepoID: job.RepoID, GitRef: "compact", Agent: "test", JobType: storage.JobTypeCompact})
		require.NoError(t, err)
		claimed, err := server.db.ClaimJob("test")
		require.NoError(t, err)
		require.Equal(t, compact.ID, claimed.ID)
		ids, err := server.db.CompleteCompactJobResult(t.Context(), compact.ID, "test", storage.ReviewCompletion{StructuredOutput: []byte(`{"schema_version":3,"summary":"Compact","verdict":"pass","findings":[]}`)}, []storage.CompactSourceIdentity{{JobID: job.ID, ReviewUUID: *source.UUID}})
		require.NoError(t, err)
		require.Equal(t, []int64{job.ID}, ids)
		worker := &WorkerPool{db: server.db, broadcaster: server.broadcaster}
		worker.notifyCompactSourcesClosed(t.Context(), "test", ids)
		synctest.Wait()
		require.Len(t, reconciler.jobs, 1)
		assert.Equal(t, job.ID, <-reconciler.jobs)
		assert.Empty(t, reconciler.wakes)
		require.Len(t, events, 1)
		event := <-events
		assert.Equal(t, "review.closed", event.Type)
		assert.Equal(t, job.ID, event.JobID)
		assert.True(t, event.SuppressHooks, "automatic bookkeeping must not introduce hook execution")
	})
}

func setupTestEnv(t *testing.T) string {
	t.Helper()
	tmpDir := t.TempDir()
	t.Setenv("ROBOREV_DATA_DIR", tmpDir)
	return tmpDir
}

func TestReadCompactMetadata(t *testing.T) {
	tests := []struct {
		name     string
		jobID    int64
		mockFile []byte
		wantIDs  []int64
		wantErr  bool
	}{
		{
			name:     "valid_metadata",
			jobID:    123,
			mockFile: []byte(`{"source_job_ids":[100, 200, 300]}`),
			wantIDs:  []int64{100, 200, 300},
			wantErr:  false,
		},
		{
			name:     "missing_file",
			jobID:    999,
			mockFile: nil,
			wantIDs:  nil,
			wantErr:  true,
		},
		{
			name:     "invalid_json",
			jobID:    456,
			mockFile: []byte("{invalid json}"),
			wantIDs:  nil,
			wantErr:  true,
		},
		{
			name:     "empty_source_ids",
			jobID:    789,
			mockFile: []byte(`{"source_job_ids":[]}`),
			wantIDs:  []int64{},
			wantErr:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setupTestEnv(t)

			if tt.mockFile != nil {
				path := compactMetadataPath(tt.jobID)
				if err := os.WriteFile(path, tt.mockFile, 0o644); err != nil {
					require.NoError(t, err, "Setup failed: %v")
				}
			}

			got, err := ReadCompactMetadata(tt.jobID)
			if (err != nil) != tt.wantErr {
				assert.Equal(t, tt.wantErr, err != nil, "ReadCompactMetadata() error mismatch")
				return
			}

			if !tt.wantErr {
				if !slices.Equal(got.SourceJobIDs, tt.wantIDs) {
					assert.Equal(t, tt.wantIDs, got.SourceJobIDs, "ReadCompactMetadata() result mismatch")
				}
			}
		})
	}
}

func TestDeleteCompactMetadata(t *testing.T) {
	t.Run("delete_existing_file", func(t *testing.T) {
		setupTestEnv(t)
		jobID := int64(123)

		// Create a metadata file
		path := compactMetadataPath(jobID)
		if err := os.WriteFile(path, []byte(`{"source_job_ids":[1,2,3]}`), 0o644); err != nil {
			require.NoError(t, err, "Failed to write metadata file: %v")
		}

		// Delete it
		err := DeleteCompactMetadata(jobID)
		require.NoError(t, err, "DeleteCompactMetadata should succeed")

		// Verify it's gone
		_, err = os.Stat(path)
		assert.True(t, os.IsNotExist(err), "Metadata file should be deleted")
	})

	t.Run("delete_nonexistent_file", func(t *testing.T) {
		setupTestEnv(t)
		jobID := int64(999)

		// Try to delete non-existent file (should not error)
		err := DeleteCompactMetadata(jobID)
		require.NoError(t, err, "DeleteCompactMetadata should not error on missing file")
	})
}

func TestCompactMetadataPath(t *testing.T) {
	tmpDir := setupTestEnv(t)

	jobID := int64(123)

	path := compactMetadataPath(jobID)

	expected := filepath.Join(tmpDir, "compact-123.json")
	assert.Equal(t, expected, path, "compactMetadataPath(123) path mismatch")
}
