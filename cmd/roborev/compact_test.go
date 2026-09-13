// ABOUTME: Unit tests for the compact command
// ABOUTME: Tests validation, prompt building, and helper functions
package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

func TestFilterReviewJobs(t *testing.T) {
	tests := []struct {
		name    string
		jobs    []storage.ReviewJob
		wantIDs []int64
	}{
		{
			name: "excludes_compact_and_task",
			jobs: []storage.ReviewJob{
				{ID: 1, JobType: "review"},
				{ID: 2, JobType: "compact"},
				{ID: 3, JobType: "range"},
				{ID: 4, JobType: "task"},
				{ID: 5, JobType: "dirty"},
			},
			wantIDs: []int64{1, 3, 5},
		},
		{
			name: "keeps_all_review_types",
			jobs: []storage.ReviewJob{
				{ID: 10, JobType: "review"},
				{ID: 11, JobType: "range"},
				{ID: 12, JobType: "dirty"},
				{ID: 13, JobType: "security"},
			},
			wantIDs: []int64{10, 11, 12, 13},
		},
		{
			name:    "empty_input",
			jobs:    []storage.ReviewJob{},
			wantIDs: nil,
		},
		{
			name: "all_excluded",
			jobs: []storage.ReviewJob{
				{ID: 1, JobType: "compact"},
				{ID: 2, JobType: "task"},
			},
			wantIDs: nil,
		},
		{
			name: "empty_job_type_kept",
			jobs: []storage.ReviewJob{
				{ID: 1, JobType: ""},
				{ID: 2, JobType: "compact"},
			},
			wantIDs: []int64{1},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := filterReviewJobs(tt.jobs)

			var gotIDs []int64
			for _, j := range got {
				gotIDs = append(gotIDs, j.ID)
			}

			assert.Equal(t, tt.wantIDs, gotIDs, "filterReviewJobs()")
		})
	}
}

func TestExtractJobIDs(t *testing.T) {
	tests := []struct {
		name    string
		reviews []jobReview
		want    []int64
	}{
		{
			name: "three_jobs",
			reviews: []jobReview{
				{jobID: 123},
				{jobID: 456},
				{jobID: 789},
			},
			want: []int64{123, 456, 789},
		},
		{
			name:    "empty",
			reviews: []jobReview{},
			want:    []int64{},
		},
		{
			name: "single_job",
			reviews: []jobReview{
				{jobID: 999},
			},
			want: []int64{999},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractJobIDs(tt.reviews)

			assert.Equal(t, tt.want, got, "extractJobIDs()")
		})
	}
}

func mockJobReview(id int64, ref, output string) jobReview {
	return jobReview{
		jobID: id,
		job:   &storage.ReviewJob{ID: id, GitRef: ref},
		review: &storage.Review{
			VerdictBool: testutil.ReviewFixtureVerdict(output), Output: output,
		},
	}
}

func TestBuildCompactPrompt(t *testing.T) {
	tests := []struct {
		name           string
		jobReviews     []jobReview
		branch         string
		wantContains   []string
		wantNotContain []string
	}{
		{
			name: "single_job_no_branch",
			jobReviews: []jobReview{
				mockJobReview(123, "abc123def456", "Finding 1: Issue in main.go"),
			},
			branch: "",
			wantContains: []string{
				"Verification and Consolidation Request",
				"1 open review",
				"Job 123",
				"Finding 1: Issue in main.go",
				"abc123d", // short SHA
				"Do not include any front matter",
				"Use the review output format above",
				"Every verified finding that still applies must be repeated",
				"Return the verified findings in the review JSON findings array",
				"may mention how many prior findings were dropped",
			},
			wantNotContain: []string{
				"If verified findings remain, format it exactly like this",
				"     - **Severity**: High/Medium/Low",
			},
		},
		{
			name: "multiple_jobs_with_branch",
			jobReviews: []jobReview{
				mockJobReview(123, "sha1", "Issue 1"),
				mockJobReview(124, "sha2", "Issue 2"),
			},
			branch: "main",
			wantContains: []string{
				"2 open reviews from branch main",
				"Job 123",
				"Job 124",
				"Issue 1",
				"Issue 2",
			},
		},
		{
			name: "all_branches",
			jobReviews: []jobReview{
				mockJobReview(100, "", "Finding"),
			},
			branch: "",
			wantContains: []string{
				"1 open review",
			},
			wantNotContain: []string{
				"from branch",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildCompactPrompt(t.Context(), tt.jobReviews, tt.branch, "", "codex")

			for _, want := range tt.wantContains {
				assert.Contains(t, got, want, "buildCompactPrompt() missing")
			}

			for _, notWant := range tt.wantNotContain {
				assert.NotContains(t, got, notWant, "buildCompactPrompt() should not contain")
			}
		})
	}
}

func TestBuildCompactOutputPrefix(t *testing.T) {
	tests := []struct {
		name         string
		jobCount     int
		branch       string
		jobIDs       []int64
		wantContains []string
	}{
		{
			name:     "with_branch",
			jobCount: 3,
			branch:   "main",
			jobIDs:   []int64{123, 124, 125},
			wantContains: []string{
				"Verified and consolidated 3 open reviews from branch main",
				"Original jobs: 123, 124, 125",
			},
		},
		{
			name:     "without_branch",
			jobCount: 2,
			branch:   "",
			jobIDs:   []int64{100, 200},
			wantContains: []string{
				"Verified and consolidated 2 open reviews",
				"Original jobs: 100, 200",
			},
		},
		{
			name:     "single_job",
			jobCount: 1,
			branch:   "feature",
			jobIDs:   []int64{999},
			wantContains: []string{
				"Verified and consolidated 1 open review from branch feature",
				"Original jobs: 999",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildCompactOutputPrefix(tt.jobCount, tt.branch, tt.jobIDs)

			for _, want := range tt.wantContains {
				assert.Contains(t, got, want, "buildCompactOutputPrefix() missing")
			}
		})
	}
}

func TestWriteCompactMetadata(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("ROBOREV_DATA_DIR", tmpDir)

	tests := []struct {
		name           string
		consolidatedID int64
		sourceIDs      []int64
		expectFile     bool
	}{
		{
			name:           "write_valid_metadata",
			consolidatedID: 999,
			sourceIDs:      []int64{100, 200, 300},
			expectFile:     true,
		},
		{
			name:           "write_empty_source_ids",
			consolidatedID: 888,
			sourceIDs:      []int64{},
			expectFile:     false,
		},
		{
			name:           "write_single_source_id",
			consolidatedID: 777,
			sourceIDs:      []int64{42},
			expectFile:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := writeCompactMetadata(tt.consolidatedID, tt.sourceIDs)
			require.NoError(t, err, "writeCompactMetadata")

			path := filepath.Join(tmpDir, getCompactMetadataFilename(tt.consolidatedID))

			if !tt.expectFile {
				_, err := os.Stat(path)
				require.ErrorIs(t, err, os.ErrNotExist, "Expected os.ErrNotExist")
				return
			}

			data, err := os.ReadFile(path)
			require.NoError(t, err, "Failed to read metadata file")

			var metadata struct {
				SourceJobIDs []int64 `json:"source_job_ids"`
			}
			require.NoError(t, json.Unmarshal(data, &metadata), "Failed to parse metadata JSON")

			assert.Equal(t, tt.sourceIDs, metadata.SourceJobIDs, "SourceJobIDs")
		})
	}
}

func TestCompactWorktreeBranchResolution(t *testing.T) {
	var receivedRepo string
	_ = newMockDaemonBuilder(t).
		WithHandler("/api/jobs", func(w http.ResponseWriter, r *http.Request) {
			receivedRepo = r.URL.Query().Get("repo")
			writeJSON(w, map[string]any{
				"jobs":     []storage.ReviewJob{},
				"has_more": false,
			})
		}).
		Build()

	repo, worktreeDir := setupWorktree(t)
	chdir(t, worktreeDir)

	cmd := &cobra.Command{}
	var buf bytes.Buffer
	cmd.SetOut(&buf)

	opts := compactOptions{quiet: true}
	_ = runCompact(cmd, opts)

	require.NotEmpty(t, receivedRepo, "expected repo param to be sent")
	assert.Equal(t, repo.Dir, receivedRepo, "repo: want main repo path")
}
