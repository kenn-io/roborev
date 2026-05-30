package daemon

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

func TestHandleBatchJobs(t *testing.T) {
	server, db, _ := newTestServer(t)

	repo, err := db.GetOrCreateRepo("/tmp/repo", "")
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "GetOrCreateRepo failed: %v", err)
	}

	// Job 1: with review
	job1, err := db.EnqueueJob(storage.EnqueueOpts{RepoID: repo.ID, GitRef: "abc123", Agent: "test"})
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, err)
	}
	setJobStatus(t, db, job1.ID, storage.JobStatusRunning)
	if err := db.CompleteJob(job1.ID, "test", "p1", "o1"); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "CompleteJob failed for job1: %v", err)
	}

	// Job 2: no review
	job2, err := db.EnqueueJob(storage.EnqueueOpts{RepoID: repo.ID, GitRef: "def456", Agent: "test"})
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, err)
	}

	nonExistentJobID := int64(9999)

	t.Run("fetches jobs with and without reviews", func(t *testing.T) {
		reqBody := map[string][]int64{"job_ids": {job1.ID, job2.ID, nonExistentJobID}}
		req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/jobs/batch", reqBody)
		w := httptest.NewRecorder()

		server.httpServer.Handler.ServeHTTP(w, req)

		testutil.AssertStatusCode(t, w, http.StatusOK)

		var resp struct {
			Results map[int64]storage.JobWithReview `json:"results"`
		}
		testutil.DecodeJSON(t, w, &resp)

		if len(resp.Results) != 2 {
			require.Condition(t, func() bool {
				return false
			}, "Expected 2 results, got %d", len(resp.Results))
		}

		// Check job 1 (with review)
		res1, ok := resp.Results[job1.ID]
		if !ok {
			assert.Condition(t, func() bool {
				return false
			}, "Result for job %d not found", job1.ID)
		}
		if res1.Review == nil {
			assert.Condition(t, func() bool {
				return false
			}, "Expected review for job 1")
		} else if res1.Review.Output != "o1" {
			assert.Condition(t, func() bool {
				return false
			}, "Expected output 'o1', got %q", res1.Review.Output)
		}

		// Check job 2 (no review)
		res2, ok := resp.Results[job2.ID]
		if !ok {
			assert.Condition(t, func() bool {
				return false
			}, "Result for job %d not found", job2.ID)
		}
		if res2.Review != nil {
			assert.Condition(t, func() bool {
				return false
			}, "Expected nil review for job 2")
		}
	})

	t.Run("wrong method fails", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/jobs/batch", nil)
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)
		testutil.AssertStatusCode(t, w, http.StatusMethodNotAllowed)
	})

	t.Run("empty job_ids fails", func(t *testing.T) {
		req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/jobs/batch", map[string][]int64{"job_ids": {}})
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)
		testutil.AssertStatusCode(t, w, http.StatusBadRequest)
	})

	t.Run("missing job_ids fails", func(t *testing.T) {
		req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/jobs/batch", map[string]string{})
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)
		testutil.AssertStatusCode(t, w, http.StatusBadRequest)
	})

	t.Run("batch size limit enforced", func(t *testing.T) {
		ids := make([]int64, 101)
		for i := range ids {
			ids[i] = int64(i)
		}
		req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/jobs/batch", map[string][]int64{"job_ids": ids})
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)
		testutil.AssertStatusCode(t, w, http.StatusBadRequest)
		if !strings.Contains(w.Body.String(), "too many job IDs") {
			assert.Condition(t, func() bool {
				return false
			}, "Expected error message about batch size, got: %s", w.Body.String())
		}
	})
}

func TestHandleRemap(t *testing.T) {
	server, db, tmpDir := newTestServer(t)

	// Set up a git repo so the remap route can resolve paths.
	repoDir := filepath.Join(tmpDir, "remap-repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		require.Condition(t, func() bool {
			return false
		}, err)
	}
	for _, args := range [][]string{
		{"init"},
		{"config", "user.email", "test@test.com"},
		{"config", "user.name", "Test"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		if out, err := cmd.CombinedOutput(); err != nil {
			require.Condition(t, func() bool {
				return false
			}, "git %v: %s: %v", args, out, err)
		}
	}

	// Resolve symlinks to get the canonical path
	// (macOS /var -> /private/var symlink)
	resolvedDir, err := filepath.EvalSymlinks(repoDir)
	if err != nil {
		resolvedDir = repoDir
	}

	repo, err := db.GetOrCreateRepo(resolvedDir)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, err)
	}
	commit, err := db.GetOrCreateCommit(repo.ID, "oldsha111", "Test", "old commit", time.Now())
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, err)
	}
	_, err = db.EnqueueJob(storage.EnqueueOpts{
		RepoID:   repo.ID,
		CommitID: commit.ID,
		GitRef:   "oldsha111",
		Agent:    "test",
		PatchID:  "patchXYZ",
	})
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, err)
	}

	t.Run("remap updates job", func(t *testing.T) {
		reqData := RemapRequest{
			RepoPath: repoDir,
			Mappings: []RemapMapping{
				{
					OldSHA:    "oldsha111",
					NewSHA:    "newsha222",
					PatchID:   "patchXYZ",
					Author:    "Test",
					Subject:   "new commit",
					Timestamp: time.Now().Format(time.RFC3339),
				},
			},
		}
		req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/remap", reqData)
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			require.Condition(t, func() bool {
				return false
			}, "expected 200, got %d: %s", w.Code, w.Body.String())
		}

		var result struct {
			Remapped int `json:"remapped"`
		}
		testutil.DecodeJSON(t, w, &result)
		if result.Remapped != 1 {
			assert.Condition(t, func() bool {
				return false
			}, "expected remapped=1, got %d", result.Remapped)
		}
	})

	t.Run("remap with non-git path returns 400", func(t *testing.T) {
		reqData := RemapRequest{
			RepoPath: "/nonexistent/repo",
			Mappings: []RemapMapping{
				{OldSHA: "a", NewSHA: "b", PatchID: "c", Author: "x", Subject: "y", Timestamp: time.Now().Format(time.RFC3339)},
			},
		}
		req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/remap", reqData)
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)

		if w.Code != http.StatusBadRequest {
			require.Condition(t, func() bool {
				return false
			}, "expected 400, got %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("remap with unregistered repo returns 404", func(t *testing.T) {
		// Create a valid git repo that is NOT registered in the DB
		unregistered := filepath.Join(tmpDir, "unregistered-repo")
		if err := os.MkdirAll(unregistered, 0o755); err != nil {
			require.Condition(t, func() bool {
				return false
			}, err)
		}
		cmd := exec.Command("git", "init")
		cmd.Dir = unregistered
		if out, err := cmd.CombinedOutput(); err != nil {
			require.Condition(t, func() bool {
				return false
			}, "git init: %s: %v", out, err)
		}

		reqData := RemapRequest{
			RepoPath: unregistered,
			Mappings: []RemapMapping{
				{OldSHA: "a", NewSHA: "b", PatchID: "c", Author: "x", Subject: "y", Timestamp: time.Now().Format(time.RFC3339)},
			},
		}
		req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/remap", reqData)
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)

		if w.Code != http.StatusNotFound {
			require.Condition(t, func() bool {
				return false
			}, "expected 404, got %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("remap with invalid timestamp returns 400", func(t *testing.T) {
		reqData := RemapRequest{
			RepoPath: repoDir,
			Mappings: []RemapMapping{
				{
					OldSHA:    "oldsha111",
					NewSHA:    "newsha333",
					PatchID:   "patchXYZ",
					Author:    "Test",
					Subject:   "bad ts",
					Timestamp: "not-a-timestamp",
				},
			},
		}
		req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/remap", reqData)
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)

		if w.Code != http.StatusBadRequest {
			require.Condition(t, func() bool {
				return false
			}, "expected 400, got %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("remap with empty repo_path returns 400", func(t *testing.T) {
		reqData := RemapRequest{
			RepoPath: "",
			Mappings: []RemapMapping{
				{
					OldSHA:    "a",
					NewSHA:    "b",
					PatchID:   "c",
					Author:    "x",
					Subject:   "y",
					Timestamp: time.Now().Format(time.RFC3339),
				},
			},
		}
		req := testutil.MakeJSONRequest(
			t, http.MethodPost, "/api/remap", reqData,
		)
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)

		if w.Code != http.StatusBadRequest {
			require.Condition(t, func() bool {
				return false
			}, "expected 400, got %d: %s",
				w.Code, w.Body.String())
		}
	})

	t.Run("remap rejects too many mappings", func(t *testing.T) {
		mappings := make([]RemapMapping, 1001)
		for i := range mappings {
			mappings[i] = RemapMapping{
				OldSHA: "a", NewSHA: "b", PatchID: "c",
				Author: "x", Subject: "y",
				Timestamp: time.Now().Format(time.RFC3339),
			}
		}
		reqData := RemapRequest{
			RepoPath: repoDir,
			Mappings: mappings,
		}
		req := testutil.MakeJSONRequest(
			t, http.MethodPost, "/api/remap", reqData,
		)
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)

		if w.Code != http.StatusBadRequest {
			require.Condition(t, func() bool {
				return false
			}, "expected 400, got %d: %s",
				w.Code, w.Body.String())
		}
	})

	t.Run("remap rejects oversized body", func(t *testing.T) {
		// Build a payload larger than 1MB.
		// JSON-encode repoDir so Windows backslashes are escaped.
		escapedDir, _ := json.Marshal(repoDir)
		body := []byte(`{"repo_path":` + string(escapedDir) + `,"mappings":[`)
		entry := []byte(`{"old_sha":"a","new_sha":"b","patch_id":"c","author":"x","subject":"y","timestamp":"2026-01-01T00:00:00Z"},`)
		for len(body) < 1<<20+1 {
			body = append(body, entry...)
		}
		body = append(body[:len(body)-1], []byte(`]}`)...)

		req := httptest.NewRequest(
			http.MethodPost, "/api/remap",
			bytes.NewReader(body),
		)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)

		if w.Code != http.StatusRequestEntityTooLarge {
			require.Condition(t, func() bool {
				return false
			}, "expected 413, got %d: %s",
				w.Code, w.Body.String())
		}
	})
}

func TestHandleGetReviewJobIDParsing(t *testing.T) {
	server, db, tmpDir := newTestServer(t)

	repoPath := filepath.Join(tmpDir, "repo-review-parse")
	repo, err := db.GetOrCreateRepo(repoPath)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "GetOrCreateRepo: %v", err)
	}
	commit, err := db.GetOrCreateCommit(
		repo.ID, "abc123", "msg", "Author", time.Now(),
	)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "GetOrCreateCommit: %v", err)
	}
	job, err := db.EnqueueJob(storage.EnqueueOpts{
		RepoID:   repo.ID,
		CommitID: commit.ID,
		GitRef:   "abc123",
		Branch:   "main",
		Agent:    "test",
	})
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "EnqueueJob: %v", err)
	}
	claimed, err := db.ClaimJob("test-worker")
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "ClaimJob: %v", err)
	}
	if claimed == nil || claimed.ID != job.ID {
		require.Condition(t, func() bool {
			return false
		}, "ClaimJob: expected job %d", job.ID)
	}
	if err := db.CompleteJob(
		job.ID, "test", "prompt", "review output",
	); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "CompleteJob: %v", err)
	}

	tests := []struct {
		name       string
		query      string
		wantStatus int
	}{
		{
			"valid job_id",
			fmt.Sprintf("job_id=%d", job.ID),
			http.StatusOK,
		},
		{
			"missing params",
			"",
			http.StatusBadRequest,
		},
		{
			"non-numeric job_id",
			"job_id=abc",
			http.StatusUnprocessableEntity,
		},
		{
			"partial numeric job_id",
			"job_id=10abc",
			http.StatusUnprocessableEntity,
		},
		{
			"not found job_id",
			"job_id=999999",
			http.StatusNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			url := "/api/review"
			if tt.query != "" {
				url += "?" + tt.query
			}
			req := httptest.NewRequest(http.MethodGet, url, nil)
			w := httptest.NewRecorder()

			server.httpServer.Handler.ServeHTTP(w, req)

			if w.Code != tt.wantStatus {
				assert.Condition(t, func() bool {
					return false
				}, "expected status %d, got %d: %s",
					tt.wantStatus, w.Code, w.Body.String())
			}
		})
	}
}

// fixJobRequest mirrors FixJobRequest for test payloads.
type fixJobRequest struct {
	ParentJobID int64  `json:"parent_job_id"`
	Prompt      string `json:"prompt,omitempty"`
	GitRef      string `json:"git_ref,omitempty"`
	StaleJobID  int64  `json:"stale_job_id,omitempty"`
}

type fixJobFixture struct {
	server    *Server
	db        *storage.DB
	tmpDir    string
	repoDir   string
	repo      *storage.Repo
	commit    *storage.Commit
	reviewJob *storage.ReviewJob
}

func assertHandlerStatus(
	t *testing.T, w *httptest.ResponseRecorder, want int,
) {
	t.Helper()
	require.Equalf(t, want, w.Code,
		"unexpected handler status, body: %s", w.Body.String(),
	)
}

func setupFixJobFixture(
	t *testing.T, repoName, gitRef string,
) *fixJobFixture {
	t.Helper()
	server, db, tmpDir := newTestServer(t)
	server.configWatcher.Config().FixAgent = "test"

	repoDir := filepath.Join(tmpDir, repoName)
	testutil.InitTestGitRepo(t, repoDir)

	repo, err := db.GetOrCreateRepo(repoDir)
	require.NoError(t, err, "GetOrCreateRepo failed")

	commit, err := db.GetOrCreateCommit(
		repo.ID, gitRef, "Author", "Subject", time.Now(),
	)
	require.NoError(t, err, "GetOrCreateCommit failed")

	reviewJob, err := db.EnqueueJob(storage.EnqueueOpts{
		RepoID:   repo.ID,
		CommitID: commit.ID,
		GitRef:   gitRef,
		Agent:    "test",
	})
	require.NoError(t, err, "EnqueueJob failed")

	_, err = db.ClaimJob("w1")
	require.NoError(t, err, "ClaimJob failed")
	require.NoError(t, db.CompleteJob(
		reviewJob.ID, "test", "prompt", "FAIL: issues found",
	), "CompleteJob failed")

	return &fixJobFixture{
		server:    server,
		db:        db,
		tmpDir:    tmpDir,
		repoDir:   repoDir,
		repo:      repo,
		commit:    commit,
		reviewJob: reviewJob,
	}
}

func TestHandleFixJobStaleValidation(t *testing.T) {
	fixture := setupFixJobFixture(t, "repo-fix-val", "fix-val-abc")
	server := fixture.server
	db := fixture.db
	tmpDir := fixture.tmpDir
	repo := fixture.repo
	commit := fixture.commit
	reviewJob := fixture.reviewJob

	t.Run("fix job inherits parent commit metadata", func(t *testing.T) {
		req := testutil.MakeJSONRequest(
			t, http.MethodPost, "/api/job/fix",
			fixJobRequest{ParentJobID: reviewJob.ID},
		)
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)
		assertHandlerStatus(t, w, http.StatusCreated)

		var fixJob storage.ReviewJob
		testutil.DecodeJSON(t, w, &fixJob)
		require.NotNil(t, fixJob.CommitID)
		require.Equal(t, commit.ID, *fixJob.CommitID)
		require.Equal(t, commit.Subject, fixJob.CommitSubject)

		stored, err := db.GetJobByID(fixJob.ID)
		require.NoError(t, err, "GetJobByID(%d)", fixJob.ID)
		require.NotNil(t, stored.CommitID)
		require.Equal(t, commit.ID, *stored.CommitID)
		require.Equal(t, commit.Subject, stored.CommitSubject)
	})

	t.Run("fix job uses configured fix reasoning", func(t *testing.T) {
		origReasoning := server.configWatcher.Config().FixReasoning
		server.configWatcher.Config().FixReasoning = "maximum"
		t.Cleanup(func() {
			server.configWatcher.Config().FixReasoning = origReasoning
		})

		req := testutil.MakeJSONRequest(
			t, http.MethodPost, "/api/job/fix",
			fixJobRequest{ParentJobID: reviewJob.ID},
		)
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)
		assertHandlerStatus(t, w, http.StatusCreated)

		var fixJob storage.ReviewJob
		testutil.DecodeJSON(t, w, &fixJob)
		require.Equal(t, "maximum", fixJob.Reasoning)

		stored, err := db.GetJobByID(fixJob.ID)
		require.NoError(t, err, "GetJobByID(%d)", fixJob.ID)
		require.Equal(t, "maximum", stored.Reasoning)
	})

	t.Run("fix job inherits parent worktree path", func(t *testing.T) {
		worktreePath := filepath.Join(tmpDir, "worktrees", "feature")
		worktreeAdd := exec.Command(
			"git", "-C", fixture.repoDir,
			"worktree", "add", "--detach", worktreePath, "HEAD",
		)
		out, err := worktreeAdd.CombinedOutput()
		require.NoError(t, err, "git worktree add failed: %s", out)

		// Enqueue a review with a worktree path
		wtJob, err := db.EnqueueJob(storage.EnqueueOpts{
			RepoID:       repo.ID,
			CommitID:     commit.ID,
			GitRef:       "fix-val-abc",
			Agent:        "test",
			WorktreePath: worktreePath,
		})
		require.NoError(t, err)

		// Claim jobs until we get ours (earlier subtests may leave queued jobs)
		for {
			claimed, claimErr := db.ClaimJob("w-wt-fix")
			require.NoError(t, claimErr)
			require.NotNil(t, claimed)
			if claimed.ID == wtJob.ID {
				break
			}
			db.CompleteJob(claimed.ID, "test", "prompt", "PASS")
		}
		require.NoError(t, db.CompleteJob(
			wtJob.ID, "test", "prompt", "FAIL: issues found",
		))

		req := testutil.MakeJSONRequest(
			t, http.MethodPost, "/api/job/fix",
			fixJobRequest{ParentJobID: wtJob.ID},
		)
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)
		assertHandlerStatus(t, w, http.StatusCreated)

		var fixJob storage.ReviewJob
		testutil.DecodeJSON(t, w, &fixJob)

		stored, err := db.GetJobByID(fixJob.ID)
		require.NoError(t, err)
		require.Equal(t, worktreePath, stored.WorktreePath)
	})

	t.Run("fix job uses worktree config when present", func(t *testing.T) {
		worktreePath := filepath.Join(tmpDir, "worktrees", "feature-config")
		worktreeAdd := exec.Command(
			"git", "-C", fixture.repoDir,
			"worktree", "add", "--detach", worktreePath, "HEAD",
		)
		out, err := worktreeAdd.CombinedOutput()
		require.NoError(t, err, "git worktree add failed: %s", out)
		require.NoError(t, os.WriteFile(
			filepath.Join(worktreePath, ".roborev.toml"),
			[]byte("fix_reasoning = \"maximum\"\nfix_model = \"worktree-model\"\n"),
			0o644,
		))

		wtJob, err := db.EnqueueJob(storage.EnqueueOpts{
			RepoID:       repo.ID,
			CommitID:     commit.ID,
			GitRef:       "fix-val-abc",
			Agent:        "test",
			WorktreePath: worktreePath,
		})
		require.NoError(t, err)

		for {
			claimed, claimErr := db.ClaimJob("w-wt-fix-config")
			require.NoError(t, claimErr)
			require.NotNil(t, claimed)
			if claimed.ID == wtJob.ID {
				break
			}
			require.NoError(t, db.CompleteJob(claimed.ID, "test", "prompt", "PASS"))
		}
		require.NoError(t, db.CompleteJob(
			wtJob.ID, "test", "prompt", "FAIL: issues found",
		))

		req := testutil.MakeJSONRequest(
			t, http.MethodPost, "/api/job/fix",
			fixJobRequest{ParentJobID: wtJob.ID},
		)
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)
		assertHandlerStatus(t, w, http.StatusCreated)

		var fixJob storage.ReviewJob
		testutil.DecodeJSON(t, w, &fixJob)
		require.Equal(t, "maximum", fixJob.Reasoning)
		require.Equal(t, "worktree-model", fixJob.Model)

		stored, err := db.GetJobByID(fixJob.ID)
		require.NoError(t, err)
		require.Equal(t, "maximum", stored.Reasoning)
		require.Equal(t, "worktree-model", stored.Model)
		require.Equal(t, worktreePath, stored.WorktreePath)
	})

	t.Run("fix job with invalid worktree path is rejected", func(t *testing.T) {
		stalePath := filepath.Join(tmpDir, "worktrees", "stale-config")
		require.NoError(t, os.MkdirAll(stalePath, 0o755))
		require.NoError(t, os.WriteFile(
			filepath.Join(stalePath, ".roborev.toml"),
			[]byte("fix_reasoning = \"maximum\"\nfix_model = \"stale-model\"\n"),
			0o644,
		))

		wtJob, err := db.EnqueueJob(storage.EnqueueOpts{
			RepoID:       repo.ID,
			CommitID:     commit.ID,
			GitRef:       "fix-val-abc",
			Agent:        "test",
			WorktreePath: stalePath,
		})
		require.NoError(t, err)

		for {
			claimed, claimErr := db.ClaimJob("w-wt-fix-stale")
			require.NoError(t, claimErr)
			require.NotNil(t, claimed)
			if claimed.ID == wtJob.ID {
				break
			}
			require.NoError(t, db.CompleteJob(claimed.ID, "test", "prompt", "PASS"))
		}
		require.NoError(t, db.CompleteJob(
			wtJob.ID, "test", "prompt", "FAIL: issues found",
		))

		req := testutil.MakeJSONRequest(
			t, http.MethodPost, "/api/job/fix",
			fixJobRequest{ParentJobID: wtJob.ID},
		)
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)
		assertHandlerStatus(t, w, http.StatusBadRequest)

		var resp ErrorResponse
		testutil.DecodeJSON(t, w, &resp)
		assert.Contains(t, resp.Error, "parent job worktree path is stale or invalid")
	})

	t.Run("custom prompt includes review context", func(t *testing.T) {
		req := testutil.MakeJSONRequest(
			t, http.MethodPost, "/api/job/fix",
			fixJobRequest{
				ParentJobID: reviewJob.ID,
				Prompt:      "Ignore the security concern, it's only a testing binary.",
			},
		)
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)
		assertHandlerStatus(t, w, http.StatusCreated)

		var fixJob storage.ReviewJob
		testutil.DecodeJSON(t, w, &fixJob)

		stored, err := db.GetJobByID(fixJob.ID)
		require.NoError(t, err, "GetJobByID(%d)", fixJob.ID)

		// The stored prompt must contain both the review output AND the custom instructions
		require.Contains(t, stored.Prompt, "FAIL: issues found")
		require.Contains(t, stored.Prompt, "Ignore the security concern")
	})

	t.Run("fix job as parent is rejected", func(t *testing.T) {
		// Create a fix job and try to use it as a parent
		fixJob, _ := db.EnqueueJob(storage.EnqueueOpts{
			RepoID:      repo.ID,
			CommitID:    commit.ID,
			GitRef:      "fix-val-abc",
			Agent:       "test",
			JobType:     storage.JobTypeFix,
			ParentJobID: reviewJob.ID,
		})
		db.ClaimJob("w-fix-parent")
		db.CompleteJob(fixJob.ID, "test", "prompt", "done")

		req := testutil.MakeJSONRequest(
			t, http.MethodPost, "/api/job/fix",
			fixJobRequest{ParentJobID: fixJob.ID},
		)
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)
		assertHandlerStatus(t, w, http.StatusBadRequest)
	})

	t.Run("stale job that is not a fix job is rejected", func(t *testing.T) {
		// reviewJob is a review, not a fix job
		req := testutil.MakeJSONRequest(
			t, http.MethodPost, "/api/job/fix",
			fixJobRequest{ParentJobID: reviewJob.ID, StaleJobID: reviewJob.ID},
		)
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)
		assertHandlerStatus(t, w, http.StatusBadRequest)
	})

	t.Run("stale job with wrong parent is rejected", func(t *testing.T) {
		// Create a second review + fix in the SAME repo, linked to the other review
		review2, _ := db.EnqueueJob(storage.EnqueueOpts{
			RepoID:   repo.ID,
			CommitID: commit.ID,
			GitRef:   "fix-val-def",
			Agent:    "test",
		})
		db.ClaimJob("w3")
		db.CompleteJob(review2.ID, "test", "prompt", "FAIL: other issues")

		wrongParentFix, _ := db.EnqueueJob(storage.EnqueueOpts{
			RepoID:      repo.ID,
			CommitID:    commit.ID,
			GitRef:      "fix-val-def",
			Agent:       "test",
			JobType:     storage.JobTypeFix,
			ParentJobID: review2.ID,
		})
		// Complete it so it has terminal status + patch
		db.ClaimJob("w4")
		db.CompleteJob(wrongParentFix.ID, "test", "prompt", "done")
		db.SaveJobPatch(wrongParentFix.ID, "--- a/f\n+++ b/f\n")

		req := testutil.MakeJSONRequest(
			t, http.MethodPost, "/api/job/fix",
			fixJobRequest{ParentJobID: reviewJob.ID, StaleJobID: wrongParentFix.ID},
		)
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)
		assertHandlerStatus(t, w, http.StatusBadRequest)
	})

	t.Run("stale job without patch is rejected", func(t *testing.T) {
		// Create a fix job linked to reviewJob but with no patch
		noPatchFix, _ := db.EnqueueJob(storage.EnqueueOpts{
			RepoID:      repo.ID,
			CommitID:    commit.ID,
			GitRef:      "fix-val-abc",
			Agent:       "test",
			JobType:     storage.JobTypeFix,
			ParentJobID: reviewJob.ID,
		})
		// Complete it (terminal status) but don't set a patch
		db.ClaimJob("w5")
		db.CompleteJob(noPatchFix.ID, "test", "prompt", "done but no diff")

		req := testutil.MakeJSONRequest(
			t, http.MethodPost, "/api/job/fix",
			fixJobRequest{ParentJobID: reviewJob.ID, StaleJobID: noPatchFix.ID},
		)
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)
		assertHandlerStatus(t, w, http.StatusBadRequest)
	})

	t.Run("non-terminal stale job statuses are rejected", func(t *testing.T) {
		for _, status := range []storage.JobStatus{
			storage.JobStatusQueued,
			storage.JobStatusRunning,
			storage.JobStatusFailed,
			storage.JobStatusCanceled,
		} {
			t.Run(string(status), func(t *testing.T) {
				fixJob, err := db.EnqueueJob(storage.EnqueueOpts{
					RepoID:      repo.ID,
					CommitID:    commit.ID,
					GitRef:      "fix-val-abc",
					Agent:       "test",
					JobType:     storage.JobTypeFix,
					ParentJobID: reviewJob.ID,
				})
				if err != nil {
					require.Condition(t, func() bool {
						return false
					}, "EnqueueJob: %v", err)
				}

				// Set status directly to avoid ClaimJob picking
				// a different queued job from an earlier iteration.
				if status != storage.JobStatusQueued {
					_, err = db.Exec(
						`UPDATE review_jobs SET status = ? WHERE id = ?`,
						string(status), fixJob.ID,
					)
					if err != nil {
						require.Condition(t, func() bool {
							return false
						}, "Set status to %s: %v", status, err)
					}
				}

				// Verify the job is in the expected state
				got, err := db.GetJobByID(fixJob.ID)
				if err != nil {
					require.Condition(t, func() bool {
						return false
					}, "GetJobByID: %v", err)
				}
				if got.Status != status {
					require.Condition(t, func() bool {
						return false
					}, "Expected job %d status %s, got %s",
						fixJob.ID, status, got.Status)
				}

				req := testutil.MakeJSONRequest(
					t, http.MethodPost, "/api/job/fix",
					fixJobRequest{ParentJobID: reviewJob.ID, StaleJobID: fixJob.ID},
				)
				w := httptest.NewRecorder()
				server.httpServer.Handler.ServeHTTP(w, req)
				assertHandlerStatus(t, w, http.StatusBadRequest)
			})
		}
	})

	t.Run("compact parent with empty git_ref uses branch as fallback", func(t *testing.T) {
		// Compact jobs are stored with an empty git_ref (the label is stored separately).
		// Fixing a compact job must not pass "" to worktree.Create — it should fall back
		// to the branch, then "HEAD".
		compactJob, _ := db.EnqueueJob(storage.EnqueueOpts{
			RepoID:  repo.ID,
			GitRef:  "", // compact jobs have no real git ref
			Branch:  "main",
			Agent:   "test",
			JobType: storage.JobTypeCompact,
			Prompt:  "consolidated findings",
			Label:   "compact-all-20240101-120000",
		})
		// Force to running so CompleteJob can transition it to done.
		setJobStatus(t, db, compactJob.ID, storage.JobStatusRunning)
		db.CompleteJob(compactJob.ID, "test", "consolidated findings", "FAIL: issues found")

		req := testutil.MakeJSONRequest(
			t, http.MethodPost, "/api/job/fix",
			fixJobRequest{ParentJobID: compactJob.ID},
		)
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)
		assertHandlerStatus(t, w, http.StatusCreated)

		var fixJob storage.ReviewJob
		testutil.DecodeJSON(t, w, &fixJob)

		if fixJob.GitRef == "" {
			assert.Condition(t, func() bool {
				return false
			}, "Fix job git_ref must not be empty for compact parent")
		}
		if fixJob.GitRef != "main" {
			assert.Condition(t, func() bool {
				return false
			}, "Expected fix job git_ref 'main' (branch fallback), got %q", fixJob.GitRef)
		}
	})

	t.Run("range parent uses branch instead of range ref for fix worktree", func(t *testing.T) {
		// Range git refs like "sha1..sha2" are not valid for git worktree add.
		// Fixing a range review should use the branch instead.
		rangeJob, _ := db.EnqueueJob(storage.EnqueueOpts{
			RepoID: repo.ID,
			GitRef: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa..bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			Branch: "feature/foo",
			Agent:  "test",
		})
		setJobStatus(t, db, rangeJob.ID, storage.JobStatusRunning)
		db.CompleteJob(rangeJob.ID, "test", "prompt", "FAIL: issues found")

		req := testutil.MakeJSONRequest(
			t, http.MethodPost, "/api/job/fix",
			fixJobRequest{ParentJobID: rangeJob.ID},
		)
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)
		assertHandlerStatus(t, w, http.StatusCreated)

		var fixJob storage.ReviewJob
		testutil.DecodeJSON(t, w, &fixJob)

		if strings.Contains(fixJob.GitRef, "..") {
			assert.Condition(t, func() bool {
				return false
			}, "Fix job git_ref must not be a range, got %q", fixJob.GitRef)
		}
		if fixJob.GitRef != "feature/foo" {
			assert.Condition(t, func() bool {
				return false
			}, "Expected fix job git_ref 'feature/foo' (branch fallback), got %q", fixJob.GitRef)
		}
	})

	t.Run("rejects git_ref starting with dash", func(t *testing.T) {
		req := testutil.MakeJSONRequest(
			t, http.MethodPost, "/api/job/fix",
			fixJobRequest{ParentJobID: reviewJob.ID, GitRef: "--option-injection"},
		)
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)
		assertHandlerStatus(t, w, http.StatusBadRequest)
	})

	t.Run("rejects git_ref with control chars", func(t *testing.T) {
		req := testutil.MakeJSONRequest(
			t, http.MethodPost, "/api/job/fix",
			fixJobRequest{ParentJobID: reviewJob.ID, GitRef: "main\x00injected"},
		)
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)
		assertHandlerStatus(t, w, http.StatusBadRequest)
	})

	t.Run("rejects whitespace-padded dash ref", func(t *testing.T) {
		req := testutil.MakeJSONRequest(
			t, http.MethodPost, "/api/job/fix",
			fixJobRequest{ParentJobID: reviewJob.ID, GitRef: " --option-injection"},
		)
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)
		assertHandlerStatus(t, w, http.StatusBadRequest)
	})

	t.Run("treats whitespace-only git_ref as empty", func(t *testing.T) {
		req := testutil.MakeJSONRequest(
			t, http.MethodPost, "/api/job/fix",
			fixJobRequest{ParentJobID: reviewJob.ID, GitRef: "   "},
		)
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)

		// Whitespace-only trims to empty, so it's treated as
		// no user-provided ref — the server falls through to
		// the parent ref/branch/HEAD resolution chain.
		assertHandlerStatus(t, w, http.StatusCreated)
	})

	t.Run("accepts valid git_ref from request", func(t *testing.T) {
		req := testutil.MakeJSONRequest(
			t, http.MethodPost, "/api/job/fix",
			fixJobRequest{ParentJobID: reviewJob.ID, GitRef: "feature/my-branch"},
		)
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)
		assertHandlerStatus(t, w, http.StatusCreated)

		var fixJob storage.ReviewJob
		testutil.DecodeJSON(t, w, &fixJob)
		if fixJob.GitRef != "feature/my-branch" {
			assert.Condition(t, func() bool {
				return false
			}, "Expected git_ref 'feature/my-branch', got %q", fixJob.GitRef)
		}
	})

	t.Run("stale job from different repo is rejected", func(t *testing.T) {
		// Create a fix job in a different repo
		repo2Dir := filepath.Join(tmpDir, "repo-fix-val-2")
		testutil.InitTestGitRepo(t, repo2Dir)
		repo2, _ := db.GetOrCreateRepo(repo2Dir)
		commit2, _ := db.GetOrCreateCommit(
			repo2.ID, "other-sha", "Author", "Subject", time.Now(),
		)
		otherReview, _ := db.EnqueueJob(storage.EnqueueOpts{
			RepoID:   repo2.ID,
			CommitID: commit2.ID,
			GitRef:   "other-sha",
			Agent:    "test",
		})
		db.ClaimJob("w2")
		db.CompleteJob(otherReview.ID, "test", "prompt", "FAIL")

		otherFix, _ := db.EnqueueJob(storage.EnqueueOpts{
			RepoID:      repo2.ID,
			CommitID:    commit2.ID,
			GitRef:      "other-sha",
			Agent:       "test",
			JobType:     storage.JobTypeFix,
			ParentJobID: otherReview.ID,
		})

		req := testutil.MakeJSONRequest(
			t, http.MethodPost, "/api/job/fix",
			fixJobRequest{ParentJobID: reviewJob.ID, StaleJobID: otherFix.ID},
		)
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)
		assertHandlerStatus(t, w, http.StatusBadRequest)
	})
}

func TestHandleFixJobAgentAvailability(t *testing.T) {
	// Create shared git repo + completed parent review job.
	gitPath, err := exec.LookPath("git")
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "git not found in PATH")
	}
	gitOnlyDir := t.TempDir()
	if runtime.GOOS == "windows" {
		wrapper := fmt.Sprintf("@\"%s\" %%*\r\n", gitPath)
		if err := os.WriteFile(filepath.Join(gitOnlyDir, "git.cmd"), []byte(wrapper), 0o755); err != nil {
			require.Condition(t, func() bool {
				return false
			}, err)
		}
	} else {
		wrapper := fmt.Sprintf("#!/bin/sh\nexec '%s' \"$@\"\n", gitPath)
		if err := os.WriteFile(filepath.Join(gitOnlyDir, "git"), []byte(wrapper), 0o755); err != nil {
			require.Condition(t, func() bool {
				return false
			}, err)
		}
	}

	tests := []struct {
		name         string
		fixAgent     string
		mockBinaries []string
		expectedCode int
	}{
		{
			name:         "unknown fix agent returns 400",
			fixAgent:     "typo-agent",
			mockBinaries: nil,
			expectedCode: http.StatusBadRequest,
		},
		{
			name:         "no agents available returns 503",
			fixAgent:     "codex",
			mockBinaries: nil,
			expectedCode: http.StatusServiceUnavailable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := setupFixJobFixture(t, "repo-fix-avail", "fix-avail-abc")
			server := fixture.server
			server.configWatcher.Config().FixAgent = tt.fixAgent
			reviewJob := fixture.reviewJob

			// Isolate PATH
			mockDir := t.TempDir()
			mockScript := "#!/bin/sh\nexit 0\n"
			for _, bin := range tt.mockBinaries {
				name := bin
				content := mockScript
				if runtime.GOOS == "windows" {
					name = bin + ".cmd"
					content = "@exit /b 0\r\n"
				}
				if err := os.WriteFile(filepath.Join(mockDir, name), []byte(content), 0o755); err != nil {
					require.Condition(t, func() bool {
						return false
					}, err)
				}
			}
			origPath := os.Getenv("PATH")
			os.Setenv("PATH", mockDir+string(os.PathListSeparator)+gitOnlyDir)
			t.Cleanup(func() { os.Setenv("PATH", origPath) })

			req := testutil.MakeJSONRequest(
				t, http.MethodPost, "/api/job/fix",
				fixJobRequest{ParentJobID: reviewJob.ID},
			)
			w := httptest.NewRecorder()
			server.httpServer.Handler.ServeHTTP(w, req)
			assertHandlerStatus(t, w, tt.expectedCode)
		})
	}
}

func TestHandleFixJobMinSeverity(t *testing.T) {
	t.Run("bakes severity instruction into stored prompt", func(t *testing.T) {
		fixture := setupFixJobFixture(t, "repo-fix-sev", "fix-sev-abc")
		server := fixture.server
		db := fixture.db

		// Write repo config with fix_min_severity
		require.NoError(t, os.WriteFile(
			filepath.Join(fixture.repoDir, ".roborev.toml"),
			[]byte("fix_min_severity = \"high\"\n"),
			0o644,
		))

		req := testutil.MakeJSONRequest(
			t, http.MethodPost, "/api/job/fix",
			fixJobRequest{ParentJobID: fixture.reviewJob.ID},
		)
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)
		assertHandlerStatus(t, w, http.StatusCreated)

		var fixJob storage.ReviewJob
		testutil.DecodeJSON(t, w, &fixJob)

		stored, err := db.GetJobByID(fixJob.ID)
		require.NoError(t, err)
		assert.Contains(t, stored.Prompt, "Severity filter:")
		assert.Contains(t, stored.Prompt, config.SeverityThresholdMarker)
	})

	t.Run("skips severity for task parent", func(t *testing.T) {
		server, db, tmpDir := newTestServer(t)
		server.configWatcher.Config().FixAgent = "test"

		repoDir := filepath.Join(tmpDir, "repo-fix-task")
		testutil.InitTestGitRepo(t, repoDir)

		// Write repo config with fix_min_severity
		require.NoError(t, os.WriteFile(
			filepath.Join(repoDir, ".roborev.toml"),
			[]byte("fix_min_severity = \"high\"\n"),
			0o644,
		))

		repo, err := db.GetOrCreateRepo(repoDir)
		require.NoError(t, err)

		// Enqueue a task job (no commit) with pre-stored prompt
		taskJob, err := db.EnqueueJob(storage.EnqueueOpts{
			RepoID:  repo.ID,
			GitRef:  "task-ref",
			Agent:   "test",
			JobType: storage.JobTypeTask,
			Prompt:  "analyze this",
			Label:   "analyze",
		})
		require.NoError(t, err)

		_, claimErr := db.ClaimJob("w-task")
		require.NoError(t, claimErr)
		require.NoError(t, db.CompleteJob(
			taskJob.ID, "test", "analyze this", "FAIL: issues found",
		))

		req := testutil.MakeJSONRequest(
			t, http.MethodPost, "/api/job/fix",
			fixJobRequest{ParentJobID: taskJob.ID},
		)
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)
		assertHandlerStatus(t, w, http.StatusCreated)

		var fixJob storage.ReviewJob
		testutil.DecodeJSON(t, w, &fixJob)

		stored, err := db.GetJobByID(fixJob.ID)
		require.NoError(t, err)
		assert.NotContains(t, stored.Prompt, "Severity filter:")
	})

	t.Run("rebase prompt has no severity instruction", func(t *testing.T) {
		fixture := setupFixJobFixture(t, "repo-fix-rebase-sev", "fix-rebase-abc")
		server := fixture.server
		db := fixture.db

		// Write repo config with fix_min_severity
		require.NoError(t, os.WriteFile(
			filepath.Join(fixture.repoDir, ".roborev.toml"),
			[]byte("fix_min_severity = \"high\"\n"),
			0o644,
		))

		// Create a stale fix job with a patch
		staleFix, err := db.EnqueueJob(storage.EnqueueOpts{
			RepoID:      fixture.repo.ID,
			CommitID:    fixture.commit.ID,
			GitRef:      "fix-rebase-abc",
			Agent:       "test",
			JobType:     storage.JobTypeFix,
			ParentJobID: fixture.reviewJob.ID,
		})
		require.NoError(t, err)

		// Drain queue until we claim stale fix
		for {
			claimed, claimErr := db.ClaimJob("w-rebase")
			require.NoError(t, claimErr)
			require.NotNil(t, claimed)
			if claimed.ID == staleFix.ID {
				break
			}
			db.CompleteJob(claimed.ID, "test", "prompt", "PASS")
		}

		patch := "diff --git a/foo.go b/foo.go\n--- a/foo.go\n+++ b/foo.go\n@@ -1 +1 @@\n-old\n+new\n"
		require.NoError(t, db.CompleteFixJob(
			staleFix.ID, "test", "prompt", "done", patch,
		))

		req := testutil.MakeJSONRequest(
			t, http.MethodPost, "/api/job/fix",
			fixJobRequest{
				ParentJobID: fixture.reviewJob.ID,
				StaleJobID:  staleFix.ID,
			},
		)
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)
		assertHandlerStatus(t, w, http.StatusCreated)

		var fixJob storage.ReviewJob
		testutil.DecodeJSON(t, w, &fixJob)

		stored, err := db.GetJobByID(fixJob.ID)
		require.NoError(t, err)
		assert.Contains(t, stored.Prompt, "Rebase Fix Request")
		assert.NotContains(t, stored.Prompt, "Severity filter:")
	})
}
