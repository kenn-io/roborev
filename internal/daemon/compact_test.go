// ABOUTME: Unit tests for compact job metadata handling
// ABOUTME: Tests reading, deleting, and validating compact metadata files
package daemon

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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

func TestIsValidCompactOutput(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{"real_review", "No issues found.", true},
		{"empty", "", false},
		{"whitespace", "   \n  ", false},
		{"error_prefix", "Error: something broke", false},
		{"exception_prefix", "Exception: null pointer", false},
		{"traceback", "Traceback (most recent call last):", false},
		{"placeholder", "No review output generated", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, IsValidCompactOutput(tt.input), "IsValidCompactOutput result mismatch")
		})
	}
}

func TestCompactMetadataPath(t *testing.T) {
	tmpDir := setupTestEnv(t)

	jobID := int64(123)

	path := compactMetadataPath(jobID)

	expected := filepath.Join(tmpDir, "compact-123.json")
	assert.Equal(t, expected, path, "compactMetadataPath(123) path mismatch")
}
