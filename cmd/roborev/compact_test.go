// ABOUTME: Unit tests for the compact command
// ABOUTME: Tests validation, prompt building, and helper functions
package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"uuid"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/daemon"
	"go.kenn.io/roborev/internal/review"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
	"go.kenn.io/roborev/pkg/structuredreview"
)

func TestFilterReviewJobs(t *testing.T) {
	tests := []struct {
		name    string
		jobs    []storage.ReviewJob
		wantIDs []int64
	}{
		{
			name: "excludes_non_review_jobs",
			jobs: []storage.ReviewJob{
				{ID: 1, JobType: "review"},
				{ID: 2, JobType: "compact"},
				{ID: 3, JobType: "range"},
				{ID: 4, JobType: "task"},
				{ID: 5, JobType: "dirty"},
				{ID: 6, JobType: storage.JobTypeGoalReview},
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
				{ID: 3, JobType: storage.JobTypeGoalReview},
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
	doc := structuredreview.Document{SchemaVersion: 3, Summary: "Original source", Verdict: "fail", Findings: []structuredreview.Finding{{Severity: "high", Problem: output, Fix: "Correct the reported behavior"}}}
	raw, _ := json.Marshal(doc)
	var fields storage.StructuredOutput
	_ = json.Unmarshal(raw, &fields)
	return jobReview{jobID: id, job: &storage.ReviewJob{ID: id, GitRef: ref}, review: &storage.Review{UUID: new(uuid.New()), Output: output, StructuredOutput: fields}}
}

func TestBuildCompactPrompt(t *testing.T) {
	sources := []jobReview{mockJobReview(123, "synthetic-ref", "Cross-tenant invoice lookup"), mockJobReview(124, "other-ref", "Missing cancellation")}
	got, err := buildCompactPrompt(t.Context(), sources, "main", "", "codex")
	require.NoError(t, err)
	manifest, err := buildCompactManifest(sources)
	require.NoError(t, err)
	require.NoError(t, review.ValidateCompactManifest(got, manifest))
	assert.Contains(t, got, "Cross-tenant invoice lookup")
	assert.Contains(t, got, "Missing cancellation")
	assert.Contains(t, got, "### F1")
	assert.Contains(t, got, "### F2")
}

func TestCompactRefusesUnparseableLegacyBeforeEnqueue(t *testing.T) {
	source := mockJobReview(123, "HEAD", "unparseable historical prose")
	source.review.StructuredOutput = nil
	_, err := buildCompactPrompt(t.Context(), []jobReview{source}, "main", "", "codex")
	require.ErrorContains(t, err, "source job 123 cannot be completely parsed")
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
				"Accounting for findings from 3 open reviews from branch main",
				"Original jobs: 123, 124, 125",
			},
		},
		{
			name:     "without_branch",
			jobCount: 2,
			branch:   "",
			jobIDs:   []int64{100, 200},
			wantContains: []string{
				"Accounting for findings from 2 open reviews",
				"Original jobs: 100, 200",
			},
		},
		{
			name:     "single_job",
			jobCount: 1,
			branch:   "feature",
			jobIDs:   []int64{999},
			wantContains: []string{
				"Accounting for findings from 1 open review from branch feature",
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

func TestBuildCompactPromptCarriesTrustedHistory(t *testing.T) {
	repo := testutil.NewTestRepoWithCommit(t)
	source := repo.CommitFile("guard.go", "package fixture\n// unchecked\n", "feat: original")
	repaired := repo.CommitFile("guard.go", "package fixture\n// tenant checked\n", "fix: check tenant")
	jr := mockJobReview(123, source, "Cross-tenant lookup")
	got, err := buildCompactPrompt(t.Context(), []jobReview{jr}, "main", repo.Root, "pi")
	require.NoError(t, err)
	assert.Contains(t, got, repaired)
	assert.Contains(t, got, "+// tenant checked")
}

func TestBuildCompactPromptFiltersHistoryWithConfiguredExcludes(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("ROBOREV_DATA_DIR", dataDir)
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "config.toml"), []byte("exclude_patterns = [\"global-secret.go\"]\n"), 0o600))

	repo := testutil.InitTestRepo(t)
	repo.CommitFile(".roborev.toml", "exclude_patterns = [\"repo-secret.go\"]\n", "config: exclude private paths")
	source := repo.CommitFiles(map[string]string{
		"global-secret.go": "package fixture\n// global baseline\n",
		"repo-secret.go":   "package fixture\n// repo baseline\n",
		"visible.go":       "package fixture\n// visible baseline\n",
	}, "feat: source files")
	repo.CommitFiles(map[string]string{
		"global-secret.go": "package fixture\n// GLOBAL_SECRET_CHANGE\n",
		"repo-secret.go":   "package fixture\n// REPO_SECRET_CHANGE\n",
		"visible.go":       "package fixture\n// visible change\n",
	}, "fix: update source files")
	got, err := buildCompactPrompt(t.Context(), []jobReview{mockJobReview(123, source, "Visible behavior")}, "main", repo.Root, "pi")
	require.NoError(t, err)
	assert.Contains(t, got, "visible.go")
	assert.Contains(t, got, "+// visible change")
	assert.NotContains(t, got, "global-secret.go")
	assert.NotContains(t, got, "repo-secret.go")
	assert.NotContains(t, got, "GLOBAL_SECRET_CHANGE")
	assert.NotContains(t, got, "REPO_SECRET_CHANGE")
}

func TestCompactEnqueuePreservesCallerCheckout(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	repo, worktree := setupWorktree(t)
	sourceSHA := repo.Run("rev-parse", "HEAD")
	mainSHA := repo.CommitFile("different.txt", "main checkout only\n", "chore: main change")
	require.NoError(t, os.WriteFile(filepath.Join(worktree, ".roborev.toml"), []byte("review_guidelines = \"Caller worktree guide\"\n"), 0o600))
	var received daemon.EnqueueRequest
	_ = newMockDaemonBuilder(t).WithHandler("/api/enqueue", func(w http.ResponseWriter, r *http.Request) {
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&received))
		w.WriteHeader(http.StatusCreated)
		writeJSON(w, storage.ReviewJob{ID: 900})
	}).Build()
	_, err := enqueueConsolidation(t.Context(), &cobra.Command{}, repo.Dir, []jobReview{mockJobReview(123, sourceSHA, "Original finding")}, "wt-branch", compactOptions{agentName: "test", quiet: true, worktreePath: worktree})
	require.NoError(t, err)
	assert.Equal(t, worktree, received.RepoPath, "server derives checkout binding and canonical repo identity from this path")
	assert.Contains(t, received.CustomPrompt, "Caller worktree guide")
	assert.Contains(t, received.CustomPrompt, "Captured current HEAD: "+sourceSHA)
	assert.NotContains(t, received.CustomPrompt, "Captured current HEAD: "+mainSHA)
}

func TestCompactAPIKeepsCanonicalIdentityAndCallerCheckout(t *testing.T) {
	t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
	repo, caller := setupWorktree(t)
	sha := repo.Run("rev-parse", "HEAD")
	db := testutil.OpenTestDB(t)
	cfg := config.DefaultConfig()
	server := daemon.NewServer(db, cfg, "")
	t.Cleanup(func() { require.NoError(t, server.Close()) })
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	previous := serverAddr
	serverAddr = httpServer.URL
	t.Cleanup(func() { serverAddr = previous })
	id, err := enqueueConsolidation(t.Context(), &cobra.Command{}, repo.Dir, []jobReview{mockJobReview(123, sha, "Original finding")}, "wt-branch", compactOptions{agentName: "test", quiet: true, worktreePath: caller})
	require.NoError(t, err)
	stored, err := db.GetJobByID(id)
	require.NoError(t, err)
	canonicalRepo, err := filepath.EvalSymlinks(repo.Dir)
	require.NoError(t, err)
	storedRepo, err := filepath.EvalSymlinks(stored.RepoPath)
	require.NoError(t, err)
	assert.Equal(t, canonicalRepo, storedRepo)
	canonicalCaller, err := filepath.EvalSymlinks(caller)
	require.NoError(t, err)
	storedCaller, err := filepath.EvalSymlinks(stored.WorktreePath)
	require.NoError(t, err)
	assert.Equal(t, canonicalCaller, storedCaller)
}
