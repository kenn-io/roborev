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

	"go.kenn.io/roborev/internal/storage"
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
		{"no_verified_findings_remain", "No verified findings remain.", true},
		{"zero_findings", "0 findings.", true},
		{"zero_findings_remain", "0 findings remain.", true},
		{"zero_word_findings", "zero findings.", true},
		{"zero_word_findings_remain", "zero findings remain.", true},
		{"no_issues_with_dropped_verified_findings_summary", "No issues found.\nSummary: Dropped 4 previously reported verified findings because they no longer reproduce.", true},
		{
			"remaining_count_without_findings",
			`Verdict: Fail

## Compact Analysis

Verified and consolidated 6 open reviews from branch main

Original jobs: 46, 45, 44, 41, 40, 38

---

Done. All 6 reviews have been verified against the current codebase: 4 previously-reported issues are fixed, 5 verified findings remain (1 high, 2 medium, 2 low).`,
			false,
		},
		{
			"review_findings_with_actionable_entry",
			`## Review Findings

- **Severity**: High
- **Location**: cmd/roborev/compact.go:42
- **Problem**: Source jobs can be closed after a count-only compact result.
- **Fix**: Reject compact output that says findings remain without listing them.`,
			true,
		},
		{
			"review_findings_header_with_remaining_count_only",
			`Verdict: Fail

## Review Findings

5 verified findings remain.`,
			false,
		},
		{
			"ten_verified_findings_without_details",
			`Verdict: Fail

10 verified findings remain.`,
			false,
		},
		{
			"negated_resolved_phrase_with_remaining_count",
			"Not all findings have been resolved: 5 verified findings remain.",
			false,
		},
		{
			"dropped_zero_summary_with_remaining_count",
			"0 verified findings were dropped; 5 verified findings remain.",
			false,
		},
		{
			"remaining_count_with_file_line_without_findings",
			"5 verified findings remain: cmd/foo.go:42",
			false,
		},
		{
			"review_findings_with_explicit_block",
			`Verdict: Fail

## Verified Findings

### **Medium Severity**

#### 1. State leak
**Files:** cmd/roborev/compact.go:42
**Issue:** The compact job closes source jobs before preserving actionable findings.`,
			true,
		},
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

func TestCleanCompactOutputsParseAsPassVerdicts(t *testing.T) {
	outputs := []string{
		"All previous findings have been addressed.",
		"All findings have been resolved.",
		"No issues found.",
		"No verified findings remain.",
		"No findings remain.",
		"No remaining findings.",
		"0 findings.",
		"0 findings remain.",
		"0 verified findings.",
		"0 verified findings remain.",
		"zero findings.",
		"zero findings remain.",
		"zero verified findings.",
		"zero verified findings remain.",
	}

	for _, output := range outputs {
		t.Run(output, func(t *testing.T) {
			require.True(t, IsValidCompactOutput(output))
			assert.Equal(t, "P", storage.ParseVerdict(output))
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
