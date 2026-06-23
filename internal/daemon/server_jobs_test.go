package daemon

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	gitpkg "go.kenn.io/roborev/internal/git"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testenv"
	"go.kenn.io/roborev/internal/testutil"
)

// listJobsResponse is the JSON shape returned by GET /api/jobs.
type listJobsResponse struct {
	Jobs    []storage.ReviewJob `json:"jobs"`
	HasMore bool                `json:"has_more"`
	Stats   storage.JobStats    `json:"stats"`
}

// fetchJobs calls GET /api/jobs via the mux, asserts HTTP 200,
// decodes the JSON body, and returns the parsed response.
func fetchJobs(t *testing.T, server *Server, query string) listJobsResponse {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/jobs?"+query, nil)
	w := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, "GET /api/jobs?%s: %s", query, w.Body.String())
	var resp listJobsResponse
	testutil.DecodeJSON(t, w, &resp)
	return resp
}

func TestHandleListJobsWithFilter(t *testing.T) {
	server, db, tmpDir := newTestServer(t)

	// Create repos and jobs
	repo1, _ := seedRepoWithJobs(t, db, filepath.Join(tmpDir, "repo1"), 3, "repo1")
	seedRepoWithJobs(t, db, filepath.Join(tmpDir, "repo2"), 2, "repo2")

	tests := []struct {
		name         string
		query        string
		wantCount    int
		wantRepoName string
	}{
		{"no filter returns all jobs", "", 5, ""},
		{"repo filter returns only matching jobs", "repo=" + url.QueryEscape(repo1.RootPath), 3, "repo1"},
		{"limit parameter works", "limit=2", 2, ""},
		{"limit=0 returns all jobs", "limit=0", 5, ""},
		{"repo filter with limit", "repo=" + url.QueryEscape(repo1.RootPath) + "&limit=2", 2, "repo1"},
		{"negative limit treated as unlimited", "limit=-1", 5, ""},
		{"very large limit capped to max", "limit=999999", 5, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := fetchJobs(t, server, tt.query)
			assert.Len(t, resp.Jobs, tt.wantCount, "job count")
			if tt.wantRepoName != "" {
				for _, job := range resp.Jobs {
					assert.Equal(t, tt.wantRepoName, job.RepoName, "RepoName")
				}
			}
		})
	}
}

func TestHandleListJobsMultiRepoFilter(t *testing.T) {
	assert := assert.New(t)
	server, db, tmpDir := newTestServer(t)

	repo1, _ := seedRepoWithJobs(t, db, filepath.Join(tmpDir, "repo1"), 3, "repo1")
	repo2, _ := seedRepoWithJobs(t, db, filepath.Join(tmpDir, "repo2"), 2, "repo2")
	seedRepoWithJobs(t, db, filepath.Join(tmpDir, "repo3"), 4, "repo3") // excluded

	multi := "repo=" + url.QueryEscape(repo1.RootPath) +
		"&repo=" + url.QueryEscape(repo2.RootPath)

	t.Run("repeated repo params scope server-side via IN clause", func(t *testing.T) {
		resp := fetchJobs(t, server, multi)
		assert.Len(resp.Jobs, 5, "only repo1 + repo2 jobs, repo3 excluded")
		for _, job := range resp.Jobs {
			assert.Contains([]string{"repo1", "repo2"}, job.RepoName)
		}
	})

	t.Run("multi-repo paginates instead of loading everything", func(t *testing.T) {
		// This is the crash fix: a display name spanning repos returns a
		// bounded page rather than every matching job in one response.
		resp := fetchJobs(t, server, multi+"&limit=2")
		assert.Len(resp.Jobs, 2, "respects the page limit")
		assert.True(resp.HasMore, "more jobs remain in the scoped set")
	})
}

func TestListJobsPagination(t *testing.T) {
	server, db, _ := newTestServer(t)

	// Create test repo and 10 jobs
	seedRepoWithJobs(t, db, "/test/repo", 10, "")

	t.Run("has_more true when more jobs exist", func(t *testing.T) {
		resp := fetchJobs(t, server, "limit=5")
		assert.Len(t, resp.Jobs, 5, "job count")
		assert.True(t, resp.HasMore, "expected has_more=true when more jobs exist")
	})

	t.Run("has_more false when no more jobs", func(t *testing.T) {
		resp := fetchJobs(t, server, "limit=50")
		assert.Len(t, resp.Jobs, 10, "job count")
		assert.False(t, resp.HasMore, "expected has_more=false when all jobs returned")
	})

	t.Run("offset skips jobs", func(t *testing.T) {
		result1 := fetchJobs(t, server, "limit=3&offset=0")
		result2 := fetchJobs(t, server, "limit=3&offset=3")

		// Ensure no overlap
		for _, j1 := range result1.Jobs {
			for _, j2 := range result2.Jobs {
				if j1.ID == j2.ID {
					assert.Condition(t, func() bool {
						return false
					}, "Job %d appears in both pages", j1.ID)
				}
			}
		}
	})

	t.Run("offset ignored when limit=0", func(t *testing.T) {
		resp := fetchJobs(t, server, "limit=0&offset=5")
		assert.Len(t, resp.Jobs, 10, "expected all 10 jobs (offset ignored with limit=0)")
	})
}

func TestListJobsWithGitRefFilter(t *testing.T) {
	server, db, _ := newTestServer(t)

	// Create repo and jobs with different git refs
	repo, _ := db.GetOrCreateRepo("/tmp/test-repo")
	refs := []string{"abc123", "def456", "abc123..def456"}
	for _, ref := range refs {
		commit, _ := db.GetOrCreateCommit(repo.ID, ref, "A", "S", time.Now())
		db.EnqueueJob(storage.EnqueueOpts{RepoID: repo.ID, CommitID: commit.ID, GitRef: ref, Agent: "codex"})
	}

	t.Run("git_ref filter returns matching job", func(t *testing.T) {
		resp := fetchJobs(t, server, "git_ref=abc123")
		assert.Len(t, resp.Jobs, 1, "job count")
		if len(resp.Jobs) > 0 {
			assert.Equal(t, "abc123", resp.Jobs[0].GitRef, "GitRef")
		}
	})

	t.Run("git_ref filter with no match returns empty", func(t *testing.T) {
		resp := fetchJobs(t, server, "git_ref=nonexistent")
		assert.Empty(t, resp.Jobs, "job count")
	})

	t.Run("git_ref filter with range ref", func(t *testing.T) {
		resp := fetchJobs(t, server, "git_ref="+url.QueryEscape("abc123..def456"))
		assert.Len(t, resp.Jobs, 1, "job count")
	})
}

func TestHandleListJobsClosedFilter(t *testing.T) {
	db := testutil.OpenTestDB(t)
	cfg := config.DefaultConfig()
	server := NewServer(db, cfg, "")

	repo, _ := db.GetOrCreateRepo("/tmp/repo-addr-filter")
	commit, _ := db.GetOrCreateCommit(repo.ID, "aaa", "A", "S", time.Now())
	job1, _ := db.EnqueueJob(storage.EnqueueOpts{RepoID: repo.ID, CommitID: commit.ID, GitRef: "aaa", Branch: "main", Agent: "codex"})
	db.ClaimJob("w")
	db.CompleteJob(job1.ID, "codex", "", "output1")

	commit2, _ := db.GetOrCreateCommit(repo.ID, "bbb", "A", "S2", time.Now())
	job2, _ := db.EnqueueJob(storage.EnqueueOpts{RepoID: repo.ID, CommitID: commit2.ID, GitRef: "bbb", Branch: "main", Agent: "codex"})
	db.ClaimJob("w")
	db.CompleteJob(job2.ID, "codex", "", "output2")
	db.MarkReviewClosedByJobID(job2.ID, true)

	t.Run("closed=false", func(t *testing.T) {
		resp := fetchJobs(t, server, "closed=false")
		assert.Len(t, resp.Jobs, 1, "expected 1 open job")
	})

	t.Run("branch filter", func(t *testing.T) {
		resp := fetchJobs(t, server, "branch=main")
		assert.Len(t, resp.Jobs, 2, "expected 2 jobs on main")
	})
}

func TestHandleEnqueueExcludedBranch(t *testing.T) {
	server, db, tmpDir := newTestServer(t)

	repoDir := filepath.Join(tmpDir, "testrepo")
	testutil.InitTestGitRepo(t, repoDir)

	// Switch to excluded branch
	checkoutCmd := exec.Command("git", "-C", repoDir, "checkout", "-b", "wip-feature")
	if out, err := checkoutCmd.CombinedOutput(); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "git checkout failed: %v\n%s", err, out)
	}

	// Create .roborev.toml with excluded_branches
	repoConfig := filepath.Join(repoDir, ".roborev.toml")
	configContent := `excluded_branches = ["wip-feature", "draft"]`
	if err := os.WriteFile(repoConfig, []byte(configContent), 0o644); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "Failed to write repo config: %v", err)
	}

	t.Run("enqueue on excluded branch returns skipped", func(t *testing.T) {
		reqData := EnqueueRequest{RepoPath: repoDir, GitRef: "HEAD", Agent: "test"}
		req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/enqueue", reqData)
		w := httptest.NewRecorder()

		server.httpServer.Handler.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			assert.Condition(t, func() bool {
				return false
			}, "Expected status 200 for skipped enqueue, got %d: %s", w.Code, w.Body.String())
		}

		var response struct {
			Skipped bool   `json:"skipped"`
			Reason  string `json:"reason"`
		}
		testutil.DecodeJSON(t, w, &response)

		if !response.Skipped {
			assert.Condition(t, func() bool {
				return false
			}, "Expected skipped=true")
		}
		if !strings.Contains(response.Reason, "wip-feature") {
			assert.Condition(t, func() bool {
				return false
			}, "Expected reason to mention branch name, got %q", response.Reason)
		}

		// Verify no job was created
		queued, _, _, _, _, _, _, _, _ := db.GetJobCounts()
		if queued != 0 {
			assert.Condition(t, func() bool {
				return false
			}, "Expected 0 queued jobs, got %d", queued)
		}
	})

	t.Run("enqueue on non-excluded branch succeeds", func(t *testing.T) {
		// Switch to a non-excluded branch
		checkoutCmd := exec.Command("git", "checkout", "-b", "feature-ok")
		checkoutCmd.Dir = repoDir
		if out, err := checkoutCmd.CombinedOutput(); err != nil {
			require.Condition(t, func() bool {
				return false
			}, "git checkout failed: %v\n%s", err, out)
		}

		reqData := EnqueueRequest{RepoPath: repoDir, GitRef: "HEAD", Agent: "test"}
		req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/enqueue", reqData)
		w := httptest.NewRecorder()

		server.httpServer.Handler.ServeHTTP(w, req)

		if w.Code != http.StatusCreated {
			assert.Condition(t, func() bool {
				return false
			}, "Expected status 201 for successful enqueue, got %d: %s", w.Code, w.Body.String())
		}

		// Verify job was created
		queued, _, _, _, _, _, _, _, _ := db.GetJobCounts()
		if queued != 1 {
			assert.Condition(t, func() bool {
				return false
			}, "Expected 1 queued job, got %d", queued)
		}
	})
}

func TestHandleEnqueueExcludedCommitPattern(t *testing.T) {
	t.Parallel()
	server, db, tmpDir := newTestServer(t)

	repoDir := filepath.Join(tmpDir, "testrepo")
	testutil.InitTestGitRepo(t, repoDir)

	// Write repo config with excluded_commit_patterns
	repoConfig := filepath.Join(repoDir, ".roborev.toml")
	configContent := `excluded_commit_patterns = ["[skip review]", "[wip]"]`
	if err := os.WriteFile(repoConfig, []byte(configContent), 0o644); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "Failed to write repo config: %v", err)
	}

	// Create a commit whose message matches an exclusion pattern
	addExcluded := exec.Command("git", "-C", repoDir,
		"commit", "--allow-empty", "-m", "wip: checkpoint [skip review]")
	if out, err := addExcluded.CombinedOutput(); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "git commit failed: %v\n%s", err, out)
	}

	t.Run("matching commit returns skipped", func(t *testing.T) {
		reqData := EnqueueRequest{
			RepoPath: repoDir, GitRef: "HEAD", Agent: "test",
		}
		req := testutil.MakeJSONRequest(
			t, http.MethodPost, "/api/enqueue", reqData,
		)
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			assert.Condition(t, func() bool {
				return false
			}, "expected 200, got %d: %s",
				w.Code, w.Body.String())
		}

		var resp struct {
			Skipped bool   `json:"skipped"`
			Reason  string `json:"reason"`
		}
		testutil.DecodeJSON(t, w, &resp)

		if !resp.Skipped {
			assert.Condition(t, func() bool {
				return false
			}, "expected skipped=true")
		}
		if !strings.Contains(resp.Reason, "excluded") {
			assert.Condition(t, func() bool {
				return false
			}, "reason should mention excluded, got %q",
				resp.Reason)
		}

		// No job should have been created
		queued, _, _, _, _, _, _, _, _ := db.GetJobCounts()
		if queued != 0 {
			assert.Condition(t, func() bool {
				return false
			}, "expected 0 queued jobs, got %d", queued)
		}
	})

	// Create a commit that does NOT match any exclusion pattern
	addNormal := exec.Command("git", "-C", repoDir,
		"commit", "--allow-empty", "-m", "feat: add endpoint")
	if out, err := addNormal.CombinedOutput(); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "git commit failed: %v\n%s", err, out)
	}

	t.Run("non-matching commit enqueues normally", func(t *testing.T) {
		reqData := EnqueueRequest{
			RepoPath: repoDir, GitRef: "HEAD", Agent: "test",
		}
		req := testutil.MakeJSONRequest(
			t, http.MethodPost, "/api/enqueue", reqData,
		)
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)

		if w.Code != http.StatusCreated {
			assert.Condition(t, func() bool {
				return false
			}, "expected 201, got %d: %s",
				w.Code, w.Body.String())
		}

		queued, _, _, _, _, _, _, _, _ := db.GetJobCounts()
		if queued != 1 {
			assert.Condition(t, func() bool {
				return false
			}, "expected 1 queued job, got %d", queued)
		}
	})

	t.Run("range where all commits excluded returns skipped",
		func(t *testing.T) {
			// Create a branch with only excluded commits
			branchCmd := exec.Command("git", "-C", repoDir,
				"checkout", "-b", "all-excluded")
			if out, err := branchCmd.CombinedOutput(); err != nil {
				require.Condition(t, func() bool {
					return false
				}, "checkout failed: %v\n%s", err, out)
			}
			base := testutil.GetHeadSHA(t, repoDir)

			for i := range 2 {
				cmd := exec.Command("git", "-C", repoDir,
					"commit", "--allow-empty",
					"-m", fmt.Sprintf("[wip] checkpoint %d", i))
				if out, err := cmd.CombinedOutput(); err != nil {
					require.Condition(t, func() bool {
						return false
					}, "commit failed: %v\n%s", err, out)
				}
			}

			ref := base + "..HEAD"
			reqData := EnqueueRequest{
				RepoPath: repoDir, GitRef: ref, Agent: "test",
			}
			req := testutil.MakeJSONRequest(
				t, http.MethodPost, "/api/enqueue", reqData,
			)
			w := httptest.NewRecorder()
			server.httpServer.Handler.ServeHTTP(w, req)

			if w.Code != http.StatusOK {
				assert.Condition(t, func() bool {
					return false
				}, "expected 200, got %d: %s",
					w.Code, w.Body.String())
			}

			var resp struct {
				Skipped bool   `json:"skipped"`
				Reason  string `json:"reason"`
			}
			testutil.DecodeJSON(t, w, &resp)
			if !resp.Skipped {
				assert.Condition(t, func() bool {
					return false
				}, "expected skipped=true for all-excluded range")
			}
		})

	t.Run("range with mixed commits enqueues normally",
		func(t *testing.T) {
			branchCmd := exec.Command("git", "-C", repoDir,
				"checkout", "-b", "mixed-range")
			if out, err := branchCmd.CombinedOutput(); err != nil {
				require.Condition(t, func() bool {
					return false
				}, "checkout failed: %v\n%s", err, out)
			}
			base := testutil.GetHeadSHA(t, repoDir)

			cmds := [][]string{
				{"commit", "--allow-empty", "-m", "[wip] temp"},
				{"commit", "--allow-empty", "-m", "feat: real work"},
			}
			for _, args := range cmds {
				cmd := exec.Command("git", append(
					[]string{"-C", repoDir}, args...)...)
				if out, err := cmd.CombinedOutput(); err != nil {
					require.Condition(t, func() bool {
						return false
					}, "commit failed: %v\n%s", err, out)
				}
			}

			ref := base + "..HEAD"
			reqData := EnqueueRequest{
				RepoPath: repoDir, GitRef: ref, Agent: "test",
			}
			req := testutil.MakeJSONRequest(
				t, http.MethodPost, "/api/enqueue", reqData,
			)
			w := httptest.NewRecorder()
			server.httpServer.Handler.ServeHTTP(w, req)

			if w.Code != http.StatusCreated {
				assert.Condition(t, func() bool {
					return false
				}, "expected 201, got %d: %s",
					w.Code, w.Body.String())
			}
		})

	// This test corrupts a branch's parent chain, so it must run last
	// since the corrupt-range branch becomes unwalkable afterward.
	t.Run("range with corrupt mid-commit enqueues normally",
		func(t *testing.T) {
			// Build a synthetic tip whose `parent` line points at a SHA
			// that does not exist in the object store. GetRangeCommits
			// walks back from tip, can't load the fake parent, and
			// fails — same failure mode as a corrupted/missing object,
			// but independent of the on-disk object layout (which is
			// fragile on Windows due to packing and AV behavior).
			// (The allRead guard is additional defense for transient
			// I/O failures where GetRangeCommits succeeds but
			// individual GetCommitInfo calls fail — git object
			// corruption can't isolate those two calls.)
			branchCmd := exec.Command("git", "-C", repoDir,
				"checkout", "-b", "corrupt-range")
			if out, err := branchCmd.CombinedOutput(); err != nil {
				require.Condition(t, func() bool {
					return false
				}, "checkout failed: %v\n%s", err, out)
			}
			base := testutil.GetHeadSHA(t, repoDir)

			treeOut, err := exec.Command("git", "-C", repoDir,
				"rev-parse", "HEAD^{tree}").Output()
			require.NoError(t, err, "rev-parse tree")
			tree := strings.TrimSpace(string(treeOut))

			const fakeParent = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
			content := fmt.Sprintf(
				"tree %s\nparent %s\n"+
					"author test <t@example.com> 1700000000 +0000\n"+
					"committer test <t@example.com> 1700000000 +0000\n"+
					"\nbroken parent\n",
				tree, fakeParent)
			hashCmd := exec.Command("git", "-C", repoDir,
				"hash-object", "-w", "-t", "commit", "--stdin")
			hashCmd.Stdin = strings.NewReader(content)
			hashOut, err := hashCmd.Output()
			require.NoError(t, err, "hash-object")
			tip := strings.TrimSpace(string(hashOut))

			if out, err := exec.Command("git", "-C", repoDir,
				"update-ref", "refs/heads/corrupt-range", tip).
				CombinedOutput(); err != nil {
				require.NoError(t, err, "update-ref: %s", out)
			}

			// ResolveSHA succeeds for both endpoints (base and the
			// synthetic tip both exist as objects), but
			// GetRangeCommits fails because git can't load tip's
			// fake parent. The exclusion block is skipped and the
			// job is enqueued normally.
			ref := base + ".." + tip
			reqData := EnqueueRequest{
				RepoPath: repoDir, GitRef: ref, Agent: "test",
			}
			req := testutil.MakeJSONRequest(
				t, http.MethodPost, "/api/enqueue", reqData,
			)
			w := httptest.NewRecorder()
			server.httpServer.Handler.ServeHTTP(w, req)

			if w.Code != http.StatusCreated {
				assert.Condition(t, func() bool {
					return false
				}, "expected 201, got %d: %s",
					w.Code, w.Body.String())
			}
		})
}

func TestHandleEnqueueReusesPreviousBranchSessionWhenEnabled(t *testing.T) {
	server, db, tmpDir := newTestServer(t)

	repoDir := filepath.Join(tmpDir, "testrepo")
	testutil.InitTestGitRepo(t, repoDir)

	checkoutCmd := exec.Command("git", "-C", repoDir, "checkout", "-b", "feature/session")
	if out, err := checkoutCmd.CombinedOutput(); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "git checkout failed: %v\n%s", err, out)
	}
	if branch := gitpkg.GetCurrentBranch(repoDir); branch != "feature/session" {
		require.Condition(t, func() bool {
			return false
		}, "current branch = %q, want %q", branch, "feature/session")
	}

	reuseSessions := true
	server.configWatcher.Config().ReuseReviewSession = &reuseSessions

	repoRoot, err := gitpkg.GetMainRepoRoot(repoDir)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "GetMainRepoRoot failed: %v", err)
	}
	repo, err := db.GetOrCreateRepo(repoRoot)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "GetOrCreateRepo failed: %v", err)
	}

	sha := testutil.GetHeadSHA(t, repoDir)
	commit, err := db.GetOrCreateCommit(repo.ID, sha, "Author", "Subject", time.Now())
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "GetOrCreateCommit failed: %v", err)
	}

	prevJob, err := db.EnqueueJob(storage.EnqueueOpts{
		RepoID:     repo.ID,
		CommitID:   commit.ID,
		GitRef:     sha,
		Branch:     "feature/session",
		Agent:      "test",
		ReviewType: config.ReviewTypeDefault,
	})
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "EnqueueJob failed: %v", err)
	}
	if _, err := db.ClaimJob("test-worker"); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "ClaimJob failed: %v", err)
	}
	if err := db.CompleteJob(prevJob.ID, "test", "prompt", "No issues found."); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "CompleteJob failed: %v", err)
	}
	if _, err := db.Exec(`UPDATE review_jobs SET session_id = ? WHERE id = ?`, "session-123", prevJob.ID); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "failed to seed session_id: %v", err)
	}

	candidate, err := db.FindReusableSessionCandidate(repo.ID, "feature/session", "test", config.ReviewTypeDefault, "")
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "FindReusableSessionCandidate failed: %v", err)
	}
	require.NotNil(t, candidate, "expected reusable session candidate")
	assert.Equal(t, "session-123", candidate.SessionID, "candidate session_id")

	reused := server.findReusableSessionID(t.Context(), repoRoot, repo.ID, "feature/session", "test", config.ReviewTypeDefault, "", sha)
	if reused != "session-123" {
		require.Condition(t, func() bool {
			return false
		}, "findReusableSessionID() = %q, want %q", reused, "session-123")
	}

	reqData := EnqueueRequest{RepoPath: repoDir, GitRef: "HEAD", Branch: "feature/session", Agent: "test"}
	req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/enqueue", reqData)
	w := httptest.NewRecorder()

	server.httpServer.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		require.Condition(t, func() bool {
			return false
		}, "expected 201, got %d: %s", w.Code, w.Body.String())
	}

	var job storage.ReviewJob
	testutil.DecodeJSON(t, w, &job)
	if job.RepoID != repo.ID {
		require.Condition(t, func() bool {
			return false
		}, "repo_id = %d, want %d", job.RepoID, repo.ID)
	}
	if job.Branch != "feature/session" {
		require.Condition(t, func() bool {
			return false
		}, "branch = %q, want %q", job.Branch, "feature/session")
	}
	if job.Agent != "test" {
		require.Condition(t, func() bool {
			return false
		}, "agent = %q, want %q", job.Agent, "test")
	}

	if job.SessionID != "session-123" {
		require.Condition(t, func() bool {
			return false
		}, "session_id = %q, want %q", job.SessionID, "session-123")
	}

	stored, err := db.GetJobByID(job.ID)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "GetJobByID failed: %v", err)
	}
	if stored.SessionID != "session-123" {
		require.Condition(t, func() bool {
			return false
		}, "stored session_id = %q, want %q", stored.SessionID, "session-123")
	}
}

func TestFindReusableSessionIDUsesDirtyBaseCommit(t *testing.T) {
	server, db, _ := newTestServer(t)
	repo := testutil.InitTestRepo(t)
	repoRoot := repo.Path()
	baseSHA := repo.HeadSHA()

	reuseSessions := true
	server.configWatcher.Config().ReuseReviewSession = &reuseSessions

	storedRepo, err := db.GetOrCreateRepo(repoRoot)
	require.NoError(t, err)
	commit, err := db.GetOrCreateCommit(storedRepo.ID, baseSHA, "Author", "Subject", time.Now())
	require.NoError(t, err)

	const dirtyDiff = "diff --git a/base.txt b/base.txt\n" +
		"--- a/base.txt\n" +
		"+++ b/base.txt\n" +
		"@@ -1 +1 @@\n" +
		"-base\n" +
		"+dirty\n"
	prevJob, err := db.EnqueueJob(storage.EnqueueOpts{
		RepoID:      storedRepo.ID,
		CommitID:    commit.ID,
		GitRef:      "dirty",
		Branch:      "feature/session",
		Agent:       "test",
		ReviewType:  config.ReviewTypeDefault,
		JobType:     storage.JobTypeDirty,
		DiffContent: dirtyDiff,
	})
	require.NoError(t, err)
	claimed, err := db.ClaimJob("test-worker")
	require.NoError(t, err)
	require.NotNil(t, claimed)
	require.Equal(t, prevJob.ID, claimed.ID)
	require.NoError(t, db.CompleteJob(prevJob.ID, "test", "prompt", "No issues found."))
	_, err = db.Exec(`UPDATE review_jobs SET session_id = ? WHERE id = ?`, "session-dirty", prevJob.ID)
	require.NoError(t, err)

	reused := server.findReusableSessionID(
		t.Context(), repoRoot, storedRepo.ID, "feature/session", "test",
		config.ReviewTypeDefault, "", baseSHA,
	)

	assert.Equal(t, "session-dirty", reused)
}

func TestFindReusableSessionIDRejectsReusedBranchNameFromUnrelatedHistory(t *testing.T) {
	server, db, tmpDir := newTestServer(t)

	repoDir := filepath.Join(tmpDir, "testrepo")
	testutil.InitTestGitRepo(t, repoDir)

	checkoutCmd := exec.Command("git", "-C", repoDir, "checkout", "-b", "feature/session")
	if out, err := checkoutCmd.CombinedOutput(); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "git checkout failed: %v\n%s", err, out)
	}

	reuseSessions := true
	server.configWatcher.Config().ReuseReviewSession = &reuseSessions

	repoRoot, err := gitpkg.GetMainRepoRoot(repoDir)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "GetMainRepoRoot failed: %v", err)
	}
	repo, err := db.GetOrCreateRepo(repoRoot)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "GetOrCreateRepo failed: %v", err)
	}

	if out, err := exec.Command("git", "-C", repoDir, "checkout", "--orphan", "branch-reused").CombinedOutput(); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "git checkout --orphan failed: %v\n%s", err, out)
	}
	if out, err := exec.Command("git", "-C", repoDir, "rm", "-rf", ".").CombinedOutput(); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "git rm failed: %v\n%s", err, out)
	}
	unrelatedFile := filepath.Join(repoDir, "unrelated.txt")
	if err := os.WriteFile(unrelatedFile, []byte("unrelated\n"), 0o644); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "WriteFile failed: %v", err)
	}
	if out, err := exec.Command("git", "-C", repoDir, "add", "unrelated.txt").CombinedOutput(); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "git add failed: %v\n%s", err, out)
	}
	if out, err := exec.Command("git", "-C", repoDir, "commit", "-m", "unrelated history").CombinedOutput(); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "git commit failed: %v\n%s", err, out)
	}
	unrelatedSHA := testutil.GetHeadSHA(t, repoDir)

	prevCommit, err := db.GetOrCreateCommit(repo.ID, unrelatedSHA, "Author", "Subject", time.Now())
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "GetOrCreateCommit failed: %v", err)
	}
	prevJob, err := db.EnqueueJob(storage.EnqueueOpts{
		RepoID:     repo.ID,
		CommitID:   prevCommit.ID,
		GitRef:     unrelatedSHA,
		Branch:     "feature/session",
		Agent:      "test",
		ReviewType: config.ReviewTypeDefault,
	})
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "EnqueueJob failed: %v", err)
	}
	if _, err := db.ClaimJob("test-worker"); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "ClaimJob failed: %v", err)
	}
	if err := db.CompleteJob(prevJob.ID, "test", "prompt", "No issues found."); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "CompleteJob failed: %v", err)
	}
	if _, err := db.Exec(`UPDATE review_jobs SET session_id = ? WHERE id = ?`, "session-old", prevJob.ID); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "failed to seed session_id: %v", err)
	}

	if out, err := exec.Command("git", "-C", repoDir, "checkout", "feature/session").CombinedOutput(); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "git checkout feature/session failed: %v\n%s", err, out)
	}
	targetSHA := testutil.GetHeadSHA(t, repoDir)

	if got := server.findReusableSessionID(t.Context(), repoRoot, repo.ID, "feature/session", "test", config.ReviewTypeDefault, "", targetSHA); got != "" {
		require.Condition(t, func() bool {
			return false
		}, "findReusableSessionID() = %q, want empty for unrelated history", got)
	}
}

func TestFindReusableSessionIDRejectsCandidateThatIsTooOldOnBranch(t *testing.T) {
	t.Parallel()
	server, db, tmpDir := newTestServer(t)

	repoDir := filepath.Join(tmpDir, "testrepo")
	testutil.InitTestGitRepo(t, repoDir)

	checkoutCmd := exec.Command("git", "-C", repoDir, "checkout", "-b", "feature/session")
	if out, err := checkoutCmd.CombinedOutput(); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "git checkout failed: %v\n%s", err, out)
	}

	reuseSessions := true
	server.configWatcher.Config().ReuseReviewSession = &reuseSessions

	repoRoot, err := gitpkg.GetMainRepoRoot(repoDir)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "GetMainRepoRoot failed: %v", err)
	}
	repo, err := db.GetOrCreateRepo(repoRoot)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "GetOrCreateRepo failed: %v", err)
	}

	candidateSHA := testutil.GetHeadSHA(t, repoDir)
	candidateCommit, err := db.GetOrCreateCommit(repo.ID, candidateSHA, "Author", "Subject", time.Now())
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "GetOrCreateCommit failed: %v", err)
	}
	prevJob, err := db.EnqueueJob(storage.EnqueueOpts{
		RepoID:     repo.ID,
		CommitID:   candidateCommit.ID,
		GitRef:     candidateSHA,
		Branch:     "feature/session",
		Agent:      "test",
		ReviewType: config.ReviewTypeDefault,
	})
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "EnqueueJob failed: %v", err)
	}
	if _, err := db.ClaimJob("test-worker"); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "ClaimJob failed: %v", err)
	}
	if err := db.CompleteJob(prevJob.ID, "test", "prompt", "No issues found."); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "CompleteJob failed: %v", err)
	}
	if _, err := db.Exec(`UPDATE review_jobs SET session_id = ? WHERE id = ?`, "session-old", prevJob.ID); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "failed to seed session_id: %v", err)
	}

	for i := range 51 {
		nextFile := filepath.Join(repoDir, fmt.Sprintf("commit-%02d.txt", i))
		if err := os.WriteFile(nextFile, fmt.Appendf(nil, "%d\n", i), 0o644); err != nil {
			require.Condition(t, func() bool {
				return false
			}, "WriteFile failed: %v", err)
		}
		if out, err := exec.Command("git", "-C", repoDir, "add", filepath.Base(nextFile)).CombinedOutput(); err != nil {
			require.Condition(t, func() bool {
				return false
			}, "git add failed: %v\n%s", err, out)
		}
		if out, err := exec.Command("git", "-C", repoDir, "commit", "-m", fmt.Sprintf("commit %02d", i)).CombinedOutput(); err != nil {
			require.Condition(t, func() bool {
				return false
			}, "git commit failed: %v\n%s", err, out)
		}
	}
	targetSHA := testutil.GetHeadSHA(t, repoDir)

	if got := server.findReusableSessionID(t.Context(), repoRoot, repo.ID, "feature/session", "test", config.ReviewTypeDefault, "", targetSHA); got != "" {
		require.Condition(t, func() bool {
			return false
		}, "findReusableSessionID() = %q, want empty for old candidate", got)
	}
}

func TestFindReusableSessionIDFallsBackToOlderValidCandidate(t *testing.T) {
	server, db, tmpDir := newTestServer(t)

	repoDir := filepath.Join(tmpDir, "testrepo")
	testutil.InitTestGitRepo(t, repoDir)

	if out, err := exec.Command("git", "-C", repoDir, "checkout", "-b", "feature/session").CombinedOutput(); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "git checkout failed: %v\n%s", err, out)
	}

	reuseSessions := true
	server.configWatcher.Config().ReuseReviewSession = &reuseSessions

	repoRoot, err := gitpkg.GetMainRepoRoot(repoDir)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "GetMainRepoRoot failed: %v", err)
	}
	repo, err := db.GetOrCreateRepo(repoRoot)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "GetOrCreateRepo failed: %v", err)
	}

	validSHA := testutil.GetHeadSHA(t, repoDir)
	validCommit, err := db.GetOrCreateCommit(repo.ID, validSHA, "Author", "Subject", time.Now())
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "GetOrCreateCommit failed: %v", err)
	}
	validJob, err := db.EnqueueJob(storage.EnqueueOpts{
		RepoID:     repo.ID,
		CommitID:   validCommit.ID,
		GitRef:     validSHA,
		Branch:     "feature/session",
		Agent:      "test",
		ReviewType: config.ReviewTypeDefault,
	})
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "EnqueueJob failed: %v", err)
	}
	if _, err := db.ClaimJob("worker-valid"); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "ClaimJob failed: %v", err)
	}
	if err := db.CompleteJob(validJob.ID, "test", "prompt", "No issues found."); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "CompleteJob failed: %v", err)
	}
	if _, err := db.Exec(`UPDATE review_jobs SET session_id = ?, finished_at = datetime('now', '-1 minute') WHERE id = ?`, "session-valid", validJob.ID); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "failed to seed valid session_id: %v", err)
	}

	if out, err := exec.Command("git", "-C", repoDir, "checkout", "--orphan", "branch-reused").CombinedOutput(); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "git checkout --orphan failed: %v\n%s", err, out)
	}
	if out, err := exec.Command("git", "-C", repoDir, "rm", "-rf", ".").CombinedOutput(); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "git rm failed: %v\n%s", err, out)
	}
	unrelatedFile := filepath.Join(repoDir, "unrelated-newer.txt")
	if err := os.WriteFile(unrelatedFile, []byte("unrelated newer\n"), 0o644); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "WriteFile failed: %v", err)
	}
	if out, err := exec.Command("git", "-C", repoDir, "add", filepath.Base(unrelatedFile)).CombinedOutput(); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "git add failed: %v\n%s", err, out)
	}
	if out, err := exec.Command("git", "-C", repoDir, "commit", "-m", "new unrelated history").CombinedOutput(); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "git commit failed: %v\n%s", err, out)
	}
	invalidSHA := testutil.GetHeadSHA(t, repoDir)
	invalidCommit, err := db.GetOrCreateCommit(repo.ID, invalidSHA, "Author", "Subject", time.Now())
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "GetOrCreateCommit failed: %v", err)
	}
	invalidJob, err := db.EnqueueJob(storage.EnqueueOpts{
		RepoID:     repo.ID,
		CommitID:   invalidCommit.ID,
		GitRef:     invalidSHA,
		Branch:     "feature/session",
		Agent:      "test",
		ReviewType: config.ReviewTypeDefault,
	})
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "EnqueueJob failed: %v", err)
	}
	if _, err := db.ClaimJob("worker-invalid"); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "ClaimJob failed: %v", err)
	}
	if err := db.CompleteJob(invalidJob.ID, "test", "prompt", "No issues found."); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "CompleteJob failed: %v", err)
	}
	if _, err := db.Exec(`UPDATE review_jobs SET session_id = ?, finished_at = datetime('now') WHERE id = ?`, "session-invalid", invalidJob.ID); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "failed to seed invalid session_id: %v", err)
	}

	if out, err := exec.Command("git", "-C", repoDir, "checkout", "feature/session").CombinedOutput(); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "git checkout feature/session failed: %v\n%s", err, out)
	}
	targetSHA := testutil.GetHeadSHA(t, repoDir)

	if got := server.findReusableSessionID(t.Context(), repoRoot, repo.ID, "feature/session", "test", config.ReviewTypeDefault, "", targetSHA); got != "session-valid" {
		require.Condition(t, func() bool {
			return false
		}, "findReusableSessionID() = %q, want %q", got, "session-valid")
	}
}

func TestFindReusableSessionIDUsesConfigurableLookback(t *testing.T) {
	t.Parallel()
	server, db, tmpDir := newTestServer(t)

	repoDir := filepath.Join(tmpDir, "testrepo")
	testutil.InitTestGitRepo(t, repoDir)

	if out, err := exec.Command("git", "-C", repoDir, "checkout", "-b", "feature/session").CombinedOutput(); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "git checkout failed: %v\n%s", err, out)
	}

	reuseSessions := true
	server.configWatcher.Config().ReuseReviewSession = &reuseSessions
	server.configWatcher.Config().ReuseReviewSessionLookback = 0

	repoRoot, err := gitpkg.GetMainRepoRoot(repoDir)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "GetMainRepoRoot failed: %v", err)
	}
	repo, err := db.GetOrCreateRepo(repoRoot)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "GetOrCreateRepo failed: %v", err)
	}

	validSHA := testutil.GetHeadSHA(t, repoDir)
	validCommit, err := db.GetOrCreateCommit(repo.ID, validSHA, "Author", "Subject", time.Now())
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "GetOrCreateCommit failed: %v", err)
	}
	validJob, err := db.EnqueueJob(storage.EnqueueOpts{
		RepoID:     repo.ID,
		CommitID:   validCommit.ID,
		GitRef:     validSHA,
		Branch:     "feature/session",
		Agent:      "test",
		ReviewType: config.ReviewTypeDefault,
	})
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "EnqueueJob failed: %v", err)
	}
	if _, err := db.ClaimJob("worker-valid"); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "ClaimJob failed: %v", err)
	}
	if err := db.CompleteJob(validJob.ID, "test", "prompt", "No issues found."); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "CompleteJob failed: %v", err)
	}
	if _, err := db.Exec(`UPDATE review_jobs SET session_id = ?, finished_at = datetime('now', '-12 minutes') WHERE id = ?`, "session-valid", validJob.ID); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "failed to seed valid session_id: %v", err)
	}

	for i := range 11 {
		branchName := fmt.Sprintf("branch-reused-%02d", i)
		if out, err := exec.Command("git", "-C", repoDir, "checkout", "--orphan", branchName).CombinedOutput(); err != nil {
			require.Condition(t, func() bool {
				return false
			}, "git checkout --orphan failed: %v\n%s", err, out)
		}
		if out, err := exec.Command("git", "-C", repoDir, "rm", "-rf", ".").CombinedOutput(); err != nil {
			require.Condition(t, func() bool {
				return false
			}, "git rm failed: %v\n%s", err, out)
		}

		unrelatedFile := filepath.Join(repoDir, fmt.Sprintf("unrelated-%02d.txt", i))
		if err := os.WriteFile(unrelatedFile, fmt.Appendf(nil, "unrelated %02d\n", i), 0o644); err != nil {
			require.Condition(t, func() bool {
				return false
			}, "WriteFile failed: %v", err)
		}
		if out, err := exec.Command("git", "-C", repoDir, "add", filepath.Base(unrelatedFile)).CombinedOutput(); err != nil {
			require.Condition(t, func() bool {
				return false
			}, "git add failed: %v\n%s", err, out)
		}
		if out, err := exec.Command("git", "-C", repoDir, "commit", "-m", fmt.Sprintf("unrelated %02d", i)).CombinedOutput(); err != nil {
			require.Condition(t, func() bool {
				return false
			}, "git commit failed: %v\n%s", err, out)
		}

		invalidSHA := testutil.GetHeadSHA(t, repoDir)
		invalidCommit, err := db.GetOrCreateCommit(repo.ID, invalidSHA, "Author", "Subject", time.Now())
		if err != nil {
			require.Condition(t, func() bool {
				return false
			}, "GetOrCreateCommit failed: %v", err)
		}
		invalidJob, err := db.EnqueueJob(storage.EnqueueOpts{
			RepoID:     repo.ID,
			CommitID:   invalidCommit.ID,
			GitRef:     invalidSHA,
			Branch:     "feature/session",
			Agent:      "test",
			ReviewType: config.ReviewTypeDefault,
		})
		if err != nil {
			require.Condition(t, func() bool {
				return false
			}, "EnqueueJob failed: %v", err)
		}
		if _, err := db.ClaimJob(fmt.Sprintf("worker-invalid-%02d", i)); err != nil {
			require.Condition(t, func() bool {
				return false
			}, "ClaimJob failed: %v", err)
		}
		if err := db.CompleteJob(invalidJob.ID, "test", "prompt", "No issues found."); err != nil {
			require.Condition(t, func() bool {
				return false
			}, "CompleteJob failed: %v", err)
		}
		offsetMinutes := 11 - i
		if _, err := db.Exec(`UPDATE review_jobs SET session_id = ?, finished_at = datetime('now', ?) WHERE id = ?`, fmt.Sprintf("session-invalid-%02d", i), fmt.Sprintf("-%d minutes", offsetMinutes), invalidJob.ID); err != nil {
			require.Condition(t, func() bool {
				return false
			}, "failed to seed invalid session_id: %v", err)
		}
	}

	if out, err := exec.Command("git", "-C", repoDir, "checkout", "feature/session").CombinedOutput(); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "git checkout feature/session failed: %v\n%s", err, out)
	}
	targetSHA := testutil.GetHeadSHA(t, repoDir)

	if got := server.findReusableSessionID(t.Context(), repoRoot, repo.ID, "feature/session", "test", config.ReviewTypeDefault, "", targetSHA); got != "session-valid" {
		require.Condition(t, func() bool {
			return false
		}, "findReusableSessionID() with default lookback = %q, want %q", got, "session-valid")
	}

	server.configWatcher.Config().ReuseReviewSessionLookback = 10
	if got := server.findReusableSessionID(t.Context(), repoRoot, repo.ID, "feature/session", "test", config.ReviewTypeDefault, "", targetSHA); got != "" {
		require.Condition(t, func() bool {
			return false
		}, "findReusableSessionID() with capped lookback = %q, want empty", got)
	}

	server.configWatcher.Config().ReuseReviewSessionLookback = 12
	if got := server.findReusableSessionID(t.Context(), repoRoot, repo.ID, "feature/session", "test", config.ReviewTypeDefault, "", targetSHA); got != "session-valid" {
		require.Condition(t, func() bool {
			return false
		}, "findReusableSessionID() with expanded lookback = %q, want %q", got, "session-valid")
	}
}

func TestFindReusableSessionIDLookbackIgnoresUnusableRefs(t *testing.T) {
	server, db, tmpDir := newTestServer(t)

	repoDir := filepath.Join(tmpDir, "testrepo")
	testutil.InitTestGitRepo(t, repoDir)

	if out, err := exec.Command("git", "-C", repoDir, "checkout", "-b", "feature/session").CombinedOutput(); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "git checkout failed: %v\n%s", err, out)
	}

	reuseSessions := true
	server.configWatcher.Config().ReuseReviewSession = &reuseSessions
	server.configWatcher.Config().ReuseReviewSessionLookback = 1

	repoRoot, err := gitpkg.GetMainRepoRoot(repoDir)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "GetMainRepoRoot failed: %v", err)
	}
	repo, err := db.GetOrCreateRepo(repoRoot)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "GetOrCreateRepo failed: %v", err)
	}

	targetSHA := testutil.GetHeadSHA(t, repoDir)
	validCommit, err := db.GetOrCreateCommit(repo.ID, targetSHA, "Author", "Subject", time.Now())
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "GetOrCreateCommit failed: %v", err)
	}
	validJob, err := db.EnqueueJob(storage.EnqueueOpts{
		RepoID:     repo.ID,
		CommitID:   validCommit.ID,
		GitRef:     targetSHA,
		Branch:     "feature/session",
		Agent:      "test",
		ReviewType: config.ReviewTypeDefault,
	})
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "EnqueueJob failed: %v", err)
	}
	if _, err := db.ClaimJob("worker-valid"); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "ClaimJob failed: %v", err)
	}
	if err := db.CompleteJob(validJob.ID, "test", "prompt", "No issues found."); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "CompleteJob failed: %v", err)
	}
	if _, err := db.Exec(`UPDATE review_jobs SET session_id = ?, finished_at = datetime('now', '-2 minutes') WHERE id = ?`, "session-valid", validJob.ID); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "failed to seed valid session_id: %v", err)
	}

	dirtyJob, err := db.EnqueueJob(storage.EnqueueOpts{
		RepoID:     repo.ID,
		GitRef:     "dirty",
		Branch:     "feature/session",
		Agent:      "test",
		ReviewType: config.ReviewTypeDefault,
	})
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "EnqueueJob failed: %v", err)
	}
	if _, err := db.ClaimJob("worker-dirty"); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "ClaimJob failed: %v", err)
	}
	if err := db.CompleteJob(dirtyJob.ID, "test", "prompt", "No issues found."); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "CompleteJob failed: %v", err)
	}
	if _, err := db.Exec(`UPDATE review_jobs SET session_id = ?, finished_at = datetime('now') WHERE id = ?`, "session-dirty", dirtyJob.ID); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "failed to seed dirty session_id: %v", err)
	}

	malformedJob, err := db.EnqueueJob(storage.EnqueueOpts{
		RepoID:     repo.ID,
		CommitID:   validCommit.ID,
		GitRef:     targetSHA,
		Branch:     "feature/session",
		Agent:      "test",
		ReviewType: config.ReviewTypeDefault,
	})
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "EnqueueJob failed: %v", err)
	}
	if _, err := db.ClaimJob("worker-malformed"); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "ClaimJob failed: %v", err)
	}
	if err := db.CompleteJob(malformedJob.ID, "test", "prompt", "No issues found."); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "CompleteJob failed: %v", err)
	}
	if _, err := db.Exec(`UPDATE review_jobs SET git_ref = ?, session_id = ?, finished_at = datetime('now', '-1 minute') WHERE id = ?`, targetSHA+"..", "session-malformed", malformedJob.ID); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "failed to seed malformed session candidate: %v", err)
	}

	if got := server.findReusableSessionID(t.Context(), repoRoot, repo.ID, "feature/session", "test", config.ReviewTypeDefault, "", targetSHA); got != "session-valid" {
		require.Condition(t, func() bool {
			return false
		}, "findReusableSessionID() with unusable newer refs = %q, want %q", got, "session-valid")
	}
}

func TestFindReusableSessionIDSkipsInvalidStoredSessionID(t *testing.T) {
	server, db, tmpDir := newTestServer(t)

	repoDir := filepath.Join(tmpDir, "testrepo")
	testutil.InitTestGitRepo(t, repoDir)

	if out, err := exec.Command("git", "-C", repoDir, "checkout", "-b", "feature/session").CombinedOutput(); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "git checkout failed: %v\n%s", err, out)
	}

	reuseSessions := true
	server.configWatcher.Config().ReuseReviewSession = &reuseSessions
	server.configWatcher.Config().ReuseReviewSessionLookback = 1

	repoRoot, err := gitpkg.GetMainRepoRoot(repoDir)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "GetMainRepoRoot failed: %v", err)
	}
	repo, err := db.GetOrCreateRepo(repoRoot)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "GetOrCreateRepo failed: %v", err)
	}

	targetSHA := testutil.GetHeadSHA(t, repoDir)
	commit, err := db.GetOrCreateCommit(repo.ID, targetSHA, "Author", "Subject", time.Now())
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "GetOrCreateCommit failed: %v", err)
	}

	validJob, err := db.EnqueueJob(storage.EnqueueOpts{
		RepoID:     repo.ID,
		CommitID:   commit.ID,
		GitRef:     targetSHA,
		Branch:     "feature/session",
		Agent:      "test",
		ReviewType: config.ReviewTypeDefault,
	})
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "EnqueueJob failed: %v", err)
	}
	if _, err := db.ClaimJob("worker-valid"); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "ClaimJob failed: %v", err)
	}
	if err := db.CompleteJob(validJob.ID, "test", "prompt", "No issues found."); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "CompleteJob failed: %v", err)
	}
	if _, err := db.Exec(`UPDATE review_jobs SET session_id = ?, finished_at = datetime('now', '-1 minute') WHERE id = ?`, "session-valid", validJob.ID); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "failed to seed valid session_id: %v", err)
	}

	invalidJob, err := db.EnqueueJob(storage.EnqueueOpts{
		RepoID:     repo.ID,
		CommitID:   commit.ID,
		GitRef:     targetSHA,
		Branch:     "feature/session",
		Agent:      "test",
		ReviewType: config.ReviewTypeDefault,
	})
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "EnqueueJob failed: %v", err)
	}
	if _, err := db.ClaimJob("worker-invalid"); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "ClaimJob failed: %v", err)
	}
	if err := db.CompleteJob(invalidJob.ID, "test", "prompt", "No issues found."); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "CompleteJob failed: %v", err)
	}
	if _, err := db.Exec(`UPDATE review_jobs SET session_id = ?, finished_at = datetime('now') WHERE id = ?`, "-bad-session", invalidJob.ID); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "failed to seed invalid session_id: %v", err)
	}

	if got := server.findReusableSessionID(t.Context(), repoRoot, repo.ID, "feature/session", "test", config.ReviewTypeDefault, "", targetSHA); got != "session-valid" {
		require.Condition(t, func() bool {
			return false
		}, "findReusableSessionID() with invalid stored session_id = %q, want %q", got, "session-valid")
	}
}

func TestHandleEnqueueBranchFallback(t *testing.T) {
	server, db, tmpDir := newTestServer(t)

	repoDir := filepath.Join(tmpDir, "testrepo")
	testutil.InitTestGitRepo(t, repoDir)

	// Switch to a named branch
	branchCmd := exec.Command("git", "-C", repoDir, "checkout", "-b", "my-feature")
	if out, err := branchCmd.CombinedOutput(); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "git checkout failed: %v\n%s", err, out)
	}

	// Enqueue with empty branch field
	reqData := EnqueueRequest{
		RepoPath: repoDir,
		GitRef:   "HEAD",
		Agent:    "test",
	}
	req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/enqueue", reqData)
	w := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		require.Condition(t, func() bool {
			return false
		}, "expected 201, got %d: %s", w.Code, w.Body.String())
	}

	var respJob storage.ReviewJob
	testutil.DecodeJSON(t, w, &respJob)

	// Verify the job has the detected branch, not empty
	job, err := db.GetJobByID(respJob.ID)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "GetJob: %v", err)
	}
	if job.Branch != "my-feature" {
		assert.Condition(t, func() bool {
			return false
		}, "expected branch %q, got %q", "my-feature", job.Branch)
	}
}

func TestHandleEnqueueBodySizeLimit(t *testing.T) {
	server, _, tmpDir := newTestServer(t)

	repoDir := filepath.Join(tmpDir, "testrepo")
	testutil.InitTestGitRepo(t, repoDir)

	t.Run("rejects oversized request body", func(t *testing.T) {
		// Create a request body larger than the default limit (200KB + 50KB overhead)
		largeDiff := strings.Repeat("a", 300*1024) // 300KB
		reqData := EnqueueRequest{
			RepoPath:    repoDir,
			GitRef:      "dirty",
			Agent:       "test",
			DiffContent: largeDiff,
		}
		req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/enqueue", reqData)
		w := httptest.NewRecorder()

		server.httpServer.Handler.ServeHTTP(w, req)

		if w.Code != http.StatusRequestEntityTooLarge {
			assert.Condition(t, func() bool {
				return false
			}, "Expected status 413, got %d: %s", w.Code, w.Body.String())
		}

		var response struct {
			Error string `json:"error"`
		}
		testutil.DecodeJSON(t, w, &response)

		if !strings.Contains(response.Error, "too large") {
			assert.Condition(t, func() bool {
				return false
			}, "Expected error about body size, got %q", response.Error)
		}
	})

	t.Run("rejects dirty review with empty diff_content", func(t *testing.T) {
		// git_ref="dirty" with empty diff_content should return a clear error
		reqData := EnqueueRequest{
			RepoPath: repoDir,
			GitRef:   "dirty",
			Agent:    "test",
			// diff_content intentionally omitted/empty
		}
		req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/enqueue", reqData)
		w := httptest.NewRecorder()

		server.httpServer.Handler.ServeHTTP(w, req)

		if w.Code != http.StatusBadRequest {
			assert.Condition(t, func() bool {
				return false
			}, "Expected status 400, got %d: %s", w.Code, w.Body.String())
		}

		var response struct {
			Error string `json:"error"`
		}
		testutil.DecodeJSON(t, w, &response)

		if !strings.Contains(response.Error, "diff_content required") {
			assert.Condition(t, func() bool {
				return false
			}, "Expected error about diff_content required, got %q", response.Error)
		}
	})

	t.Run("accepts valid size dirty request", func(t *testing.T) {
		// Create a valid-sized diff (under 200KB)
		validDiff := strings.Repeat("a", 100*1024) // 100KB
		reqData := EnqueueRequest{
			RepoPath:    repoDir,
			GitRef:      "dirty",
			Agent:       "test",
			DiffContent: validDiff,
		}
		req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/enqueue", reqData)
		w := httptest.NewRecorder()

		server.httpServer.Handler.ServeHTTP(w, req)

		if w.Code != http.StatusCreated {
			assert.Condition(t, func() bool {
				return false
			}, "Expected status 201, got %d: %s", w.Code, w.Body.String())
		}
	})
}

func TestHandleListJobsByID(t *testing.T) {
	server, db, tmpDir := newTestServer(t)

	// Create repos and jobs
	_, jobs := seedRepoWithJobs(t, db, filepath.Join(tmpDir, "testrepo"), 3, "repo1")
	job1ID := jobs[0].ID
	job2ID := jobs[1].ID
	job3ID := jobs[2].ID

	t.Run("fetches specific job by ID", func(t *testing.T) {
		// Request job 1 specifically
		req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/jobs?id=%d", job1ID), nil)
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			require.Condition(t, func() bool {
				return false
			}, "Expected 200, got %d: %s", w.Code, w.Body.String())
		}

		var response struct {
			Jobs    []storage.ReviewJob `json:"jobs"`
			HasMore bool                `json:"has_more"`
		}
		testutil.DecodeJSON(t, w, &response)

		if len(response.Jobs) != 1 {
			assert.Condition(t, func() bool {
				return false
			}, "Expected exactly 1 job, got %d", len(response.Jobs))
		}
		if response.Jobs[0].ID != job1ID {
			assert.Condition(t, func() bool {
				return false
			}, "Expected job ID %d, got %d", job1ID, response.Jobs[0].ID)
		}
	})

	t.Run("fetches middle job correctly", func(t *testing.T) {
		// Request job 2 specifically (the middle job)
		req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/jobs?id=%d", job2ID), nil)
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			require.Condition(t, func() bool {
				return false
			}, "Expected 200, got %d: %s", w.Code, w.Body.String())
		}

		var response struct {
			Jobs []storage.ReviewJob `json:"jobs"`
		}
		testutil.DecodeJSON(t, w, &response)

		if len(response.Jobs) != 1 {
			assert.Condition(t, func() bool {
				return false
			}, "Expected exactly 1 job, got %d", len(response.Jobs))
		}
		if response.Jobs[0].ID != job2ID {
			assert.Condition(t, func() bool {
				return false
			}, "Expected job ID %d, got %d", job2ID, response.Jobs[0].ID)
		}
	})

	t.Run("returns empty for non-existent job ID", func(t *testing.T) {
		// Request a job ID that doesn't exist
		req := httptest.NewRequest(http.MethodGet, "/api/jobs?id=99999", nil)
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			require.Condition(t, func() bool {
				return false
			}, "Expected 200, got %d: %s", w.Code, w.Body.String())
		}

		var response struct {
			Jobs []storage.ReviewJob `json:"jobs"`
		}
		testutil.DecodeJSON(t, w, &response)

		if len(response.Jobs) != 0 {
			assert.Condition(t, func() bool {
				return false
			}, "Expected 0 jobs for non-existent ID, got %d", len(response.Jobs))
		}
	})

	t.Run("returns error for invalid job ID", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/jobs?id=notanumber", nil)
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)

		assert.GreaterOrEqual(t, w.Code, 400,
			"expected client error for invalid id, got %d: %s",
			w.Code, w.Body.String())
	})

	t.Run("without id param returns all jobs", func(t *testing.T) {
		// Request without id param should return all jobs
		req := httptest.NewRequest(http.MethodGet, "/api/jobs", nil)
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			require.Condition(t, func() bool {
				return false
			}, "Expected 200, got %d: %s", w.Code, w.Body.String())
		}

		var response struct {
			Jobs []storage.ReviewJob `json:"jobs"`
		}
		testutil.DecodeJSON(t, w, &response)

		// Should have all 3 jobs
		if len(response.Jobs) != 3 {
			assert.Condition(t, func() bool {
				return false
			}, "Expected 3 jobs, got %d", len(response.Jobs))
		}

		// Verify all job IDs are present (order may vary due to same-second timestamps)
		foundIDs := make(map[int64]bool)
		for _, job := range response.Jobs {
			foundIDs[job.ID] = true
		}
		if !foundIDs[job1ID] || !foundIDs[job2ID] || !foundIDs[job3ID] {
			assert.Condition(t, func() bool {
				return false
			}, "Expected jobs %d, %d, %d but found %v", job1ID, job2ID, job3ID, foundIDs)
		}
	})
}

func TestHandleEnqueuePromptJob(t *testing.T) {
	repoDir := t.TempDir()
	testutil.InitTestGitRepo(t, repoDir)

	server, _, _ := newTestServer(t)

	t.Run("enqueues prompt job successfully", func(t *testing.T) {
		reqData := EnqueueRequest{
			RepoPath:     repoDir,
			GitRef:       "prompt",
			Agent:        "test",
			CustomPrompt: "Explain this codebase",
		}
		req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/enqueue", reqData)
		w := httptest.NewRecorder()

		server.httpServer.Handler.ServeHTTP(w, req)

		if w.Code != http.StatusCreated {
			require.Condition(t, func() bool {
				return false
			}, "Expected 201, got %d: %s", w.Code, w.Body.String())
		}

		var job storage.ReviewJob
		testutil.DecodeJSON(t, w, &job)

		if job.GitRef != "prompt" {
			assert.Condition(t, func() bool {
				return false
			}, "Expected git_ref 'prompt', got '%s'", job.GitRef)
		}
		if job.Agent != "test" {
			assert.Condition(t, func() bool {
				return false
			}, "Expected agent 'test', got '%s'", job.Agent)
		}
		if job.Status != storage.JobStatusQueued {
			assert.Condition(t, func() bool {
				return false
			}, "Expected status 'queued', got '%s'", job.Status)
		}
	})

	t.Run("git_ref prompt without custom_prompt is treated as branch review", func(t *testing.T) {
		// With no custom_prompt, git_ref="prompt" is treated as trying to review
		// a branch/commit named "prompt" (not a prompt job). This allows reviewing
		// branches literally named "prompt" without collision.
		reqData := EnqueueRequest{
			RepoPath: repoDir,
			GitRef:   "prompt",
			Agent:    "test",
			// no custom_prompt - should try to resolve "prompt" as a git ref
		}
		req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/enqueue", reqData)
		w := httptest.NewRecorder()

		server.httpServer.Handler.ServeHTTP(w, req)

		// Should fail because there's no branch named "prompt", not because
		// custom_prompt is missing
		if w.Code != http.StatusBadRequest {
			assert.Condition(t, func() bool {
				return false
			}, "Expected 400 (invalid commit), got %d: %s", w.Code, w.Body.String())
		}

		if strings.Contains(w.Body.String(), "custom_prompt required") {
			assert.Condition(t, func() bool {
				return false
			}, "Should NOT require custom_prompt for git_ref=prompt, got: %s", w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "invalid commit") {
			assert.Condition(t, func() bool {
				return false
			}, "Expected 'invalid commit' error, got: %s", w.Body.String())
		}
	})

	t.Run("prompt job with reasoning level", func(t *testing.T) {
		reqData := EnqueueRequest{
			RepoPath:     repoDir,
			GitRef:       "prompt",
			Agent:        "test",
			Reasoning:    "fast",
			CustomPrompt: "Quick analysis",
		}
		req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/enqueue", reqData)
		w := httptest.NewRecorder()

		server.httpServer.Handler.ServeHTTP(w, req)

		if w.Code != http.StatusCreated {
			require.Condition(t, func() bool {
				return false
			}, "Expected 201, got %d: %s", w.Code, w.Body.String())
		}

		var job storage.ReviewJob
		testutil.DecodeJSON(t, w, &job)

		if job.Reasoning != "fast" {
			assert.Condition(t, func() bool {
				return false
			}, "Expected reasoning 'fast', got '%s'", job.Reasoning)
		}
	})

	t.Run("prompt job with agentic flag", func(t *testing.T) {
		reqData := EnqueueRequest{
			RepoPath:     repoDir,
			GitRef:       "prompt",
			Agent:        "test",
			CustomPrompt: "Fix all bugs",
			Agentic:      true,
		}
		req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/enqueue", reqData)
		w := httptest.NewRecorder()

		server.httpServer.Handler.ServeHTTP(w, req)

		if w.Code != http.StatusCreated {
			require.Condition(t, func() bool {
				return false
			}, "Expected 201, got %d: %s", w.Code, w.Body.String())
		}

		var job storage.ReviewJob
		testutil.DecodeJSON(t, w, &job)

		if !job.Agentic {
			assert.Condition(t, func() bool {
				return false
			}, "Expected Agentic to be true")
		}
	})

	t.Run("prompt job without agentic defaults to false", func(t *testing.T) {
		reqData := EnqueueRequest{
			RepoPath:     repoDir,
			GitRef:       "prompt",
			Agent:        "test",
			CustomPrompt: "Read-only review",
		}
		req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/enqueue", reqData)
		w := httptest.NewRecorder()

		server.httpServer.Handler.ServeHTTP(w, req)

		if w.Code != http.StatusCreated {
			require.Condition(t, func() bool {
				return false
			}, "Expected 201, got %d: %s", w.Code, w.Body.String())
		}

		var job storage.ReviewJob
		testutil.DecodeJSON(t, w, &job)

		if job.Agentic {
			assert.Condition(t, func() bool {
				return false
			}, "Expected Agentic to be false by default")
		}
	})
}

func TestHandleEnqueueAgentAvailability(t *testing.T) {
	// Shared read-only git repo created once (all subtests use different servers for DB isolation)
	repoDir := filepath.Join(t.TempDir(), "repo")
	testutil.InitTestGitRepo(t, repoDir)
	headSHA := testutil.GetHeadSHA(t, repoDir)

	// Create an isolated dir containing only a wrapper for git.
	// We can't just use git's parent dir because it may contain real agent
	// binaries (e.g. codex, claude) that would defeat the PATH isolation.
	// Symlinks don't work reliably on Windows, so we use wrapper scripts.
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

	mockScript := "#!/bin/sh\nexit 0\n"

	tests := []struct {
		name          string
		requestAgent  string
		defaultAgent  string
		backupAgent   string
		mockBinaries  []string // binary names to place in PATH
		expectedAgent string   // expected agent stored in job
		expectedCode  int      // expected HTTP status code
	}{
		{
			name:          "explicit test agent preserved",
			requestAgent:  "test",
			mockBinaries:  nil,
			expectedAgent: "test",
			expectedCode:  http.StatusCreated,
		},
		{
			name:         "unavailable explicit codex without backup returns 503",
			requestAgent: "codex",
			mockBinaries: []string{"claude"},
			expectedCode: http.StatusServiceUnavailable,
		},
		{
			name:         "default agent without backup returns 503 when unavailable",
			requestAgent: "",
			mockBinaries: []string{"claude"},
			expectedCode: http.StatusServiceUnavailable,
		},
		{
			name:          "configured backup used when default agent unavailable",
			requestAgent:  "",
			backupAgent:   "gemini",
			mockBinaries:  []string{"gemini"},
			expectedAgent: "gemini",
			expectedCode:  http.StatusCreated,
		},
		{
			name:          "explicit agent alias resolves to cursor",
			requestAgent:  "agent",
			mockBinaries:  []string{"agent"},
			expectedAgent: "cursor",
			expectedCode:  http.StatusCreated,
		},
		{
			name:          "default agent alias resolves to cursor",
			requestAgent:  "",
			defaultAgent:  "agent",
			mockBinaries:  []string{"agent"},
			expectedAgent: "cursor",
			expectedCode:  http.StatusCreated,
		},
		{
			name:          "explicit codex kept when available",
			requestAgent:  "codex",
			mockBinaries:  []string{"codex"},
			expectedAgent: "codex",
			expectedCode:  http.StatusCreated,
		},
		{
			name:         "default does not use hardcoded fallback when only kilo available",
			requestAgent: "",
			mockBinaries: []string{"kilo"},
			expectedCode: http.StatusServiceUnavailable,
		},
		{
			name:         "no agents available returns 503",
			requestAgent: "codex",
			mockBinaries: nil,
			expectedCode: http.StatusServiceUnavailable,
		},
		{
			name:         "unknown agent returns 400",
			requestAgent: "typo-agent",
			mockBinaries: nil,
			expectedCode: http.StatusBadRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Each subtest gets its own server/DB to avoid SHA dedup conflicts
			server, _, _ := newTestServer(t)
			if tt.defaultAgent != "" {
				server.configWatcher.Config().DefaultAgent = tt.defaultAgent
			}
			if tt.backupAgent != "" {
				server.configWatcher.Config().DefaultBackupAgent = tt.backupAgent
			}

			// Isolate PATH: only mock binaries + git (no real agent CLIs)
			origPath := os.Getenv("PATH")
			mockDir := t.TempDir()
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
			os.Setenv("PATH", mockDir+string(os.PathListSeparator)+gitOnlyDir)
			t.Cleanup(func() { os.Setenv("PATH", origPath) })

			reqData := EnqueueRequest{
				RepoPath:  repoDir,
				CommitSHA: headSHA,
			}
			if tt.requestAgent != "" {
				reqData.Agent = tt.requestAgent
			}
			req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/enqueue", reqData)
			w := httptest.NewRecorder()

			server.httpServer.Handler.ServeHTTP(w, req)

			if w.Code != tt.expectedCode {
				require.Condition(t, func() bool {
					return false
				}, "Expected status %d, got %d: %s", tt.expectedCode, w.Code, w.Body.String())
			}

			if tt.expectedCode != http.StatusCreated {
				return
			}

			var job storage.ReviewJob
			testutil.DecodeJSON(t, w, &job)

			if job.Agent != tt.expectedAgent {
				assert.Condition(t, func() bool {
					return false
				}, "Expected agent %q, got %q", tt.expectedAgent, job.Agent)
			}
		})
	}
}

func TestHandleEnqueueWorktreeGitDirIsolation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping worktree test on Windows due to path differences")
	}

	tmpDir := t.TempDir()

	// Create main repo with initial commit (commit A)
	mainRepo := filepath.Join(tmpDir, "main-repo")
	testutil.InitTestGitRepo(t, mainRepo)
	commitA := testutil.GetHeadSHA(t, mainRepo)

	// Create a worktree
	worktreeDir := filepath.Join(tmpDir, "worktree")
	wtCmd := exec.Command("git", "-C", mainRepo, "worktree", "add", "-b", "wt-branch", worktreeDir)
	if out, err := wtCmd.CombinedOutput(); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "git worktree add failed: %v\n%s", err, out)
	}

	// Make a new commit in the worktree so HEAD differs (commit B)
	wtFile := filepath.Join(worktreeDir, "worktree-file.txt")
	if err := os.WriteFile(wtFile, []byte("worktree content"), 0o644); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "write file: %v", err)
	}
	for _, args := range [][]string{
		{"git", "-C", worktreeDir, "add", "."},
		{"git", "-C", worktreeDir, "commit", "-m", "worktree commit"},
	} {
		cmd := exec.Command(args[0], args[1:]...)
		if out, err := cmd.CombinedOutput(); err != nil {
			require.Condition(t, func() bool {
				return false
			}, "%v failed: %v\n%s", args, err, out)
		}
	}
	commitB := testutil.GetHeadSHA(t, worktreeDir)

	require.NotEqual(t, commitA, commitB, "test setup error: commits A and B should differ")

	enqueue := func(t *testing.T) storage.ReviewJob {
		t.Helper()
		server, _, _ := newTestServer(t)
		reqData := EnqueueRequest{
			RepoPath: worktreeDir,
			GitRef:   "HEAD",
			Agent:    "test",
		}
		req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/enqueue", reqData)
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)
		if w.Code != http.StatusCreated {
			require.Condition(t, func() bool {
				return false
			}, "expected 201, got %d: %s", w.Code, w.Body.String())
		}
		var job storage.ReviewJob
		testutil.DecodeJSON(t, w, &job)
		return job
	}

	t.Run("leaked GIT_DIR is ignored", func(t *testing.T) {
		// Set GIT_DIR to the main repo's .git dir, simulating a post-commit hook.
		// t.Setenv restores the original value after the subtest.
		mainGitDir := filepath.Join(mainRepo, ".git")
		t.Setenv("GIT_DIR", mainGitDir)

		job := enqueue(t)

		// Kit's git runner strips inherited GIT_* variables, so the request
		// resolves HEAD from the worktree even when the process env is polluted.
		assert.Equal(t, commitB, job.GitRef)
	})

	t.Run("cleared GIT_DIR resolves correct commit", func(t *testing.T) {
		// Simulate the daemon startup fix: clear GIT_DIR before handling requests.
		// This is what daemonRunCmd() does with os.Unsetenv.
		t.Setenv("GIT_DIR", "")
		os.Unsetenv("GIT_DIR")

		job := enqueue(t)

		// Without GIT_DIR, git uses cmd.Dir correctly and resolves the worktree's HEAD.
		assert.Equal(t, commitB, job.GitRef)
	})
}

// TestHandleEnqueueRangeFromRootCommit verifies that a range review starting
// from the root commit (which has no parent) succeeds by falling back to the
// empty tree SHA.
func TestHandleEnqueueRangeFromRootCommit(t *testing.T) {
	repoDir := t.TempDir()
	testutil.InitTestGitRepo(t, repoDir)

	// Get the root commit SHA
	rootSHA, err := gitpkg.ResolveSHA(repoDir, "HEAD")
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "resolve root SHA: %v", err)
	}

	// Add a second commit so we have a range
	testFile := filepath.Join(repoDir, "second.txt")
	if err := os.WriteFile(testFile, []byte("second"), 0o644); err != nil {
		require.Condition(t, func() bool {
			return false
		}, err)
	}
	for _, args := range [][]string{
		{"git", "-C", repoDir, "add", "."},
		{"git", "-C", repoDir, "commit", "-m", "second"},
	} {
		cmd := exec.Command(args[0], args[1:]...)
		if out, err := cmd.CombinedOutput(); err != nil {
			require.Condition(t, func() bool {
				return false
			}, "%v failed: %v\n%s", args, err, out)
		}
	}
	endSHA, err := gitpkg.ResolveSHA(repoDir, "HEAD")
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "resolve end SHA: %v", err)
	}

	server, _, _ := newTestServer(t)

	// Send range starting from root commit's parent (rootSHA^..endSHA)
	// This is what the CLI sends for "roborev review <root> <end>"
	rangeRef := rootSHA + "^.." + endSHA
	reqData := EnqueueRequest{
		RepoPath: repoDir,
		GitRef:   rangeRef,
		Agent:    "test",
	}
	req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/enqueue", reqData)
	w := httptest.NewRecorder()

	server.httpServer.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		require.Condition(t, func() bool {
			return false
		}, "expected 201, got %d: %s", w.Code, w.Body.String())
	}

	var job storage.ReviewJob
	testutil.DecodeJSON(t, w, &job)

	// The stored range should use the empty tree SHA as the start
	expectedRef := gitpkg.EmptyTreeSHA + ".." + endSHA
	if job.GitRef != expectedRef {
		assert.Condition(t, func() bool {
			return false
		}, "expected git_ref %q, got %q", expectedRef, job.GitRef)
	}
}

// TestHandleEnqueueRangeNonCommitObjectRejects verifies that the root-commit
// fallback does not trigger for non-commit objects (e.g. blobs).
func TestHandleEnqueueRangeNonCommitObjectRejects(t *testing.T) {
	repoDir := t.TempDir()
	testutil.InitTestGitRepo(t, repoDir)

	endSHA, err := gitpkg.ResolveSHA(repoDir, "HEAD")
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "resolve HEAD: %v", err)
	}

	// Get a blob SHA (the test.txt file created by InitTestGitRepo)
	cmd := exec.Command("git", "-C", repoDir, "rev-parse", "HEAD:test.txt")
	out, err := cmd.Output()
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "get blob SHA: %v", err)
	}
	blobSHA := strings.TrimSpace(string(out))

	server, _, _ := newTestServer(t)

	// A blob^ should not fall back to EmptyTreeSHA — it should return 400
	rangeRef := blobSHA + "^.." + endSHA
	reqData := EnqueueRequest{
		RepoPath: repoDir,
		GitRef:   rangeRef,
		Agent:    "test",
	}
	req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/enqueue", reqData)
	w := httptest.NewRecorder()

	server.httpServer.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		assert.Condition(t, func() bool {
			return false
		}, "expected 400, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "invalid start commit") {
		assert.Condition(t, func() bool {
			return false
		}, "expected 'invalid start commit' error, got: %s", w.Body.String())
	}
}

func TestHandleListJobsIDParsing(t *testing.T) {
	server, _, _ := newTestServer(t)
	for _, id := range []string{"abc", "10abc", "1.5"} {
		t.Run("invalid_id_"+id, func(t *testing.T) {
			rr := serveHuma(t, server, http.MethodGet,
				"/api/jobs?id="+id, nil)
			assert.GreaterOrEqual(t, rr.Code, 400,
				"expected client error for invalid id %q", id)
		})
	}
}

func TestHandleListJobsJobTypeFilter(t *testing.T) {
	server, db, tmpDir := newTestServer(t)

	repoDir := filepath.Join(tmpDir, "repo-jt")
	testutil.InitTestGitRepo(t, repoDir)
	repo, _ := db.GetOrCreateRepo(repoDir)
	commit, _ := db.GetOrCreateCommit(
		repo.ID, "jt-abc", "Author", "Subject", time.Now(),
	)

	// Create a review job
	reviewJob, _ := db.EnqueueJob(storage.EnqueueOpts{
		RepoID:   repo.ID,
		CommitID: commit.ID,
		GitRef:   "jt-abc",
		Agent:    "test",
	})

	// Create a fix job parented to the review
	fixJob, _ := db.EnqueueJob(storage.EnqueueOpts{
		RepoID:      repo.ID,
		CommitID:    commit.ID,
		GitRef:      "jt-abc",
		Agent:       "test",
		JobType:     storage.JobTypeFix,
		ParentJobID: reviewJob.ID,
	})
	_, err := db.Exec(
		`UPDATE review_jobs SET status = 'running' WHERE id IN (?, ?)`,
		reviewJob.ID, fixJob.ID,
	)
	require.NoError(t, err)
	require.NoError(t, db.CompleteJob(reviewJob.ID, "test", "prompt", "review done"))
	require.NoError(t, db.CompleteJob(fixJob.ID, "test", "prompt", "fix done"))

	t.Run("job_type=fix returns only fix jobs", func(t *testing.T) {
		req := httptest.NewRequest(
			http.MethodGet, "/api/jobs?job_type=fix", nil,
		)
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			require.Condition(t, func() bool {
				return false
			}, "Expected 200, got %d: %s", w.Code, w.Body.String())
		}

		var resp struct {
			Jobs  []storage.ReviewJob `json:"jobs"`
			Stats storage.JobStats    `json:"stats"`
		}
		testutil.DecodeJSON(t, w, &resp)

		if len(resp.Jobs) != 1 {
			require.Condition(t, func() bool {
				return false
			}, "Expected 1 fix job, got %d", len(resp.Jobs))
		}
		if resp.Jobs[0].JobType != storage.JobTypeFix {
			assert.Condition(t, func() bool {
				return false
			}, "Expected job_type 'fix', got %q", resp.Jobs[0].JobType)
		}
	})

	t.Run("no job_type returns all jobs", func(t *testing.T) {
		req := httptest.NewRequest(
			http.MethodGet, "/api/jobs", nil,
		)
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			require.Condition(t, func() bool {
				return false
			}, "Expected 200, got %d: %s", w.Code, w.Body.String())
		}

		var resp struct {
			Jobs  []storage.ReviewJob `json:"jobs"`
			Stats storage.JobStats    `json:"stats"`
		}
		testutil.DecodeJSON(t, w, &resp)

		if len(resp.Jobs) != 2 {
			assert.Condition(t, func() bool {
				return false
			}, "Expected 2 jobs total, got %d", len(resp.Jobs))
		}
	})

	t.Run("exclude_job_type=fix returns only non-fix jobs", func(t *testing.T) {
		req := httptest.NewRequest(
			http.MethodGet, "/api/jobs?exclude_job_type=fix", nil,
		)
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			require.Condition(t, func() bool {
				return false
			}, "Expected 200, got %d: %s", w.Code, w.Body.String())
		}

		var resp struct {
			Jobs  []storage.ReviewJob `json:"jobs"`
			Stats storage.JobStats    `json:"stats"`
		}
		testutil.DecodeJSON(t, w, &resp)

		if len(resp.Jobs) != 1 {
			require.Condition(t, func() bool {
				return false
			}, "Expected 1 non-fix job, got %d", len(resp.Jobs))
		}
		if resp.Jobs[0].JobType == storage.JobTypeFix {
			assert.Condition(t, func() bool {
				return false
			}, "Expected non-fix job, got fix")
		}
		assert.Equal(t, storage.JobStats{
			Done:   1,
			Closed: 0,
			Open:   1,
		}, resp.Stats)
	})

	t.Run("hide_classify_jobs=true hides classify and skipped rows", func(t *testing.T) {
		// Add a running classify row and a skipped design row alongside
		// the existing review + fix jobs. The (repo_id, commit_id,
		// review_type='design') partial unique index on auto_design rows
		// means each auto_design row needs a distinct commit.
		classifyCommit, err := db.GetOrCreateCommit(repo.ID, "jt-classify", "Author", "Subject", time.Now())
		require.NoError(t, err)
		_, err = db.Exec(`
			INSERT INTO review_jobs
			  (repo_id, commit_id, git_ref, agent, status, job_type, review_type, source, worker_id, started_at, enqueued_at, updated_at)
			VALUES (?, ?, 'jt-classify', 'test', 'running', 'classify', 'design', 'auto_design', 'w1', datetime('now'), datetime('now'), datetime('now'))
		`, repo.ID, classifyCommit.ID)
		require.NoError(t, err)

		skippedCommit, err := db.GetOrCreateCommit(repo.ID, "jt-skipped", "Author", "Subject", time.Now())
		require.NoError(t, err)
		_, err = db.Exec(`
			INSERT INTO review_jobs
			  (repo_id, commit_id, git_ref, agent, status, job_type, review_type, source, skip_reason, enqueued_at, finished_at, updated_at)
			VALUES (?, ?, 'jt-skipped', 'test', 'skipped', 'review', 'design', 'auto_design', 'trivial diff', datetime('now'), datetime('now'), datetime('now'))
		`, repo.ID, skippedCommit.ID)
		require.NoError(t, err)

		req := httptest.NewRequest(
			http.MethodGet, "/api/jobs?hide_classify_jobs=true", nil,
		)
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)
		testutil.AssertStatusCode(t, w, http.StatusOK)

		var resp struct {
			Jobs []storage.ReviewJob `json:"jobs"`
		}
		testutil.DecodeJSON(t, w, &resp)

		// Should see the original review + fix (2 jobs), not the classify
		// or skipped rows.
		assert.Len(t, resp.Jobs, 2, "expected only review and fix jobs")
		for _, j := range resp.Jobs {
			assert.NotEqual(t, storage.JobTypeClassify, j.JobType,
				"classify rows should be hidden")
			assert.NotEqual(t, storage.JobStatusSkipped, j.Status,
				"skipped rows should be hidden")
		}
	})
}

func TestHandleListJobsRepoPrefixFilter(t *testing.T) {
	server, db, tmpDir := newTestServer(t)

	// Create repos under a "workspace" parent and one outside it
	workspace := filepath.Join(tmpDir, "workspace")
	seedRepoWithJobs(t, db, filepath.Join(workspace, "repo-a"), 3, "repoA")
	seedRepoWithJobs(t, db, filepath.Join(workspace, "repo-b"), 2, "repoB")
	seedRepoWithJobs(t, db, filepath.Join(tmpDir, "outside-repo"), 1, "outside")

	t.Run("repo_prefix returns only child repos", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/jobs?repo_prefix="+url.QueryEscape(workspace), nil)
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)
		testutil.AssertStatusCode(t, w, http.StatusOK)

		var resp struct {
			Jobs  []storage.ReviewJob `json:"jobs"`
			Stats storage.JobStats    `json:"stats"`
		}
		testutil.DecodeJSON(t, w, &resp)

		if len(resp.Jobs) != 5 {
			assert.Condition(t, func() bool {
				return false
			}, "Expected 5 jobs under workspace prefix, got %d", len(resp.Jobs))
		}
		wsSlash := filepath.ToSlash(workspace) + "/"
		for _, j := range resp.Jobs {
			if !strings.HasPrefix(j.RepoPath, wsSlash) {
				assert.Condition(t, func() bool {
					return false
				}, "Job repo_path %q does not start with workspace prefix", j.RepoPath)
			}
		}
	})

	t.Run("repo_prefix does not match parent directory itself", func(t *testing.T) {
		// A repo AT the workspace path shouldn't match (must be a child)
		seedRepoWithJobs(t, db, workspace, 1, "exact")
		req := httptest.NewRequest(http.MethodGet, "/api/jobs?repo_prefix="+url.QueryEscape(workspace), nil)
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)
		testutil.AssertStatusCode(t, w, http.StatusOK)

		var resp struct {
			Jobs []storage.ReviewJob `json:"jobs"`
		}
		testutil.DecodeJSON(t, w, &resp)

		// Should still be 5 (not 6) - the exact workspace path match is excluded
		if len(resp.Jobs) != 5 {
			assert.Condition(t, func() bool {
				return false
			}, "Expected 5 jobs (excluding exact path match), got %d", len(resp.Jobs))
		}
	})

	t.Run("repo param takes precedence over repo_prefix", func(t *testing.T) {
		exactRepo := filepath.Join(workspace, "repo-a")
		req := httptest.NewRequest(http.MethodGet, "/api/jobs?repo="+url.QueryEscape(exactRepo)+"&repo_prefix="+url.QueryEscape(workspace), nil)
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)
		testutil.AssertStatusCode(t, w, http.StatusOK)

		var resp struct {
			Jobs []storage.ReviewJob `json:"jobs"`
		}
		testutil.DecodeJSON(t, w, &resp)

		if len(resp.Jobs) != 3 {
			assert.Condition(t, func() bool {
				return false
			}, "Expected 3 jobs for exact repo (repo takes precedence), got %d", len(resp.Jobs))
		}
	})

	t.Run("repo_prefix trailing slash is normalized", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/jobs?repo_prefix="+url.QueryEscape(workspace+"/"), nil)
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)
		testutil.AssertStatusCode(t, w, http.StatusOK)

		var resp struct {
			Jobs []storage.ReviewJob `json:"jobs"`
		}
		testutil.DecodeJSON(t, w, &resp)

		if len(resp.Jobs) != 5 {
			assert.Condition(t, func() bool {
				return false
			}, "Expected 5 jobs with trailing-slash prefix, got %d", len(resp.Jobs))
		}
	})

	t.Run("repo_prefix with dot-dot is normalized", func(t *testing.T) {
		// workspace/../workspace should normalize to workspace
		dotdotPrefix := workspace + "/../" + filepath.Base(workspace)
		req := httptest.NewRequest(http.MethodGet, "/api/jobs?repo_prefix="+url.QueryEscape(dotdotPrefix), nil)
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)
		testutil.AssertStatusCode(t, w, http.StatusOK)

		var resp struct {
			Jobs []storage.ReviewJob `json:"jobs"`
		}
		testutil.DecodeJSON(t, w, &resp)

		if len(resp.Jobs) != 5 {
			assert.Condition(t, func() bool {
				return false
			}, "Expected 5 jobs with dot-dot prefix, got %d", len(resp.Jobs))
		}
	})
}

func TestHandleEnqueueAgentOverrideModel(t *testing.T) {
	t.Parallel()
	// When the requested agent differs from config default, the generic
	// default_model should be skipped. When they match (even via alias),
	// default_model should apply.

	repoDir := t.TempDir()
	testutil.InitTestGitRepo(t, repoDir)
	headSHA := testutil.GetHeadSHA(t, repoDir)

	tests := []struct {
		name         string
		defaultAgent string
		defaultModel string
		reqAgent     string
		reqModel     string
		wantModel    string
	}{
		{
			name:         "agent matches default: default_model applied",
			defaultAgent: "test",
			defaultModel: "gpt-5.4",
			reqAgent:     "test",
			wantModel:    "gpt-5.4",
		},
		{
			name:         "agent differs: default_model skipped",
			defaultAgent: "codex",
			defaultModel: "gpt-5.4",
			reqAgent:     "test",
			wantModel:    "",
		},
		{
			name:         "no agent override: default_model applied",
			defaultAgent: "test",
			defaultModel: "gpt-5.4",
			reqAgent:     "",
			wantModel:    "gpt-5.4",
		},
		{
			name:         "explicit model always used",
			defaultAgent: "codex",
			defaultModel: "gpt-5.4",
			reqAgent:     "test",
			reqModel:     "my-model",
			wantModel:    "my-model",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db, _ := testutil.OpenTestDBWithDir(t)
			cfg := config.DefaultConfig()
			cfg.DefaultAgent = tt.defaultAgent
			cfg.DefaultModel = tt.defaultModel
			server := NewServer(db, cfg, "")

			reqData := EnqueueRequest{
				RepoPath:  repoDir,
				CommitSHA: headSHA,
				Agent:     tt.reqAgent,
				Model:     tt.reqModel,
			}
			req := testutil.MakeJSONRequest(
				t, http.MethodPost, "/api/enqueue", reqData,
			)
			w := httptest.NewRecorder()
			server.httpServer.Handler.ServeHTTP(w, req)

			if w.Code != http.StatusCreated {
				require.Condition(t, func() bool {
					return false
				}, "expected 201, got %d: %s", w.Code, w.Body.String())
			}

			var job storage.ReviewJob
			testutil.DecodeJSON(t, w, &job)

			if job.Model != tt.wantModel {
				assert.Condition(t, func() bool {
					return false
				}, "model = %q, want %q", job.Model, tt.wantModel)
			}
			assert.Equal(t, tt.reqModel, job.RequestedModel)
		})
	}
}

func TestHandleEnqueueBackupAgentUsesBackupModel(t *testing.T) {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not installed")
	}
	binDir := t.TempDir()
	claudeName := "claude"
	gitName := "git"
	var gitWrapper []byte
	var claudeScript []byte
	if runtime.GOOS == "windows" {
		claudeName += ".bat"
		gitName += ".bat"
		claudeScript = []byte("@exit /b 0\r\n")
		gitWrapper = fmt.Appendf(nil, "@\"%s\" %%*\r\n", gitPath)
	} else {
		claudeScript = []byte("#!/bin/sh\nexit 0\n")
		gitWrapper = fmt.Appendf(nil, "#!/bin/sh\nexec %q \"$@\"\n", gitPath)
	}
	claudePath := filepath.Join(binDir, claudeName)
	if err := os.WriteFile(claudePath, claudeScript, 0o755); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "write fake claude: %v", err)
	}
	if err := os.WriteFile(filepath.Join(binDir, gitName), gitWrapper, 0o755); err != nil {
		require.Condition(t, func() bool {
			return false
		}, "write git wrapper: %v", err)
	}
	t.Setenv("PATH", binDir)

	repoDir := t.TempDir()
	testutil.InitTestGitRepo(t, repoDir)
	headSHA := testutil.GetHeadSHA(t, repoDir)

	db, _ := testutil.OpenTestDBWithDir(t)
	cfg := config.DefaultConfig()
	cfg.DefaultAgent = "claude-code"
	cfg.DefaultModel = "gpt-5.4"
	cfg.ReviewAgent = "codex"
	cfg.ReviewBackupAgent = "claude-code"
	cfg.ReviewBackupModel = "backup-sonnet"
	server := NewServer(db, cfg, "")

	reqData := EnqueueRequest{
		RepoPath:  repoDir,
		CommitSHA: headSHA,
	}
	req := testutil.MakeJSONRequest(
		t, http.MethodPost, "/api/enqueue", reqData,
	)
	w := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		require.Condition(t, func() bool {
			return false
		}, "expected 201, got %d: %s", w.Code, w.Body.String())
	}

	var job storage.ReviewJob
	testutil.DecodeJSON(t, w, &job)

	if job.Agent != "claude-code" {
		require.Condition(t, func() bool {
			return false
		}, "agent = %q, want %q", job.Agent, "claude-code")
	}
	if job.Model != "backup-sonnet" {
		require.Condition(t, func() bool {
			return false
		}, "model = %q, want %q", job.Model, "backup-sonnet")
	}
}

func TestHandleEnqueueCompactReasoning(t *testing.T) {
	// Compact jobs should use fix reasoning defaults ("standard"),
	// not review reasoning defaults ("thorough").

	repoDir := t.TempDir()
	testutil.InitTestGitRepo(t, repoDir)

	db, _ := testutil.OpenTestDBWithDir(t)
	cfg := config.DefaultConfig()
	server := NewServer(db, cfg, "")

	reqData := EnqueueRequest{
		RepoPath:     repoDir,
		GitRef:       "compact-test",
		Agent:        "test",
		CustomPrompt: "consolidation prompt",
		Agentic:      true,
		JobType:      "compact",
		// No explicit reasoning — should default to fix reasoning
	}
	req := testutil.MakeJSONRequest(
		t, http.MethodPost, "/api/enqueue", reqData,
	)
	w := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		require.Condition(t, func() bool {
			return false
		}, "expected 201, got %d: %s", w.Code, w.Body.String())
	}

	var job storage.ReviewJob
	testutil.DecodeJSON(t, w, &job)

	// Fix reasoning default is "standard", review default is "thorough"
	if job.Reasoning != "standard" {
		assert.Condition(t, func() bool {
			return false
		}, "compact job reasoning = %q, want %q (fix default)",
			job.Reasoning, "standard")
	}
}

func TestHandleEnqueueUsesConfiguredReviewReasoning(t *testing.T) {
	_ = testenv.SetDataDir(t)

	configPath := filepath.Join(t.TempDir(), "daemon-config.toml")
	err := os.WriteFile(
		configPath,
		[]byte(`review_reasoning = "maximum"`),
		0o644,
	)
	require.NoError(t, err)

	repoDir := t.TempDir()
	testutil.InitTestGitRepo(t, repoDir)

	db, _ := testutil.OpenTestDBWithDir(t)
	cfg, err := config.LoadGlobalFrom(configPath)
	require.NoError(t, err)
	server := NewServer(db, cfg, configPath)
	t.Cleanup(func() {
		require.NoError(t, server.Close())
	})

	reqData := EnqueueRequest{
		RepoPath: repoDir,
		GitRef:   "HEAD",
		Agent:    "test",
	}
	req := testutil.MakeJSONRequest(
		t, http.MethodPost, "/api/enqueue", reqData,
	)
	w := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		require.Condition(t, func() bool {
			return false
		}, "expected 201, got %d: %s", w.Code, w.Body.String())
	}

	var job storage.ReviewJob
	testutil.DecodeJSON(t, w, &job)
	assert.Equal(t, "maximum", job.Reasoning)
}

func TestHandleEnqueueRejectsMalformedRepoConfigWithExplicitReasoning(t *testing.T) {
	_ = testenv.SetDataDir(t)

	repoDir := t.TempDir()
	testutil.InitTestGitRepo(t, repoDir)
	err := os.WriteFile(
		filepath.Join(repoDir, ".roborev.toml"),
		[]byte("review_model = ["),
		0o644,
	)
	require.NoError(t, err)

	db, _ := testutil.OpenTestDBWithDir(t)
	server := NewServer(db, config.DefaultConfig(), "")
	t.Cleanup(func() {
		require.NoError(t, server.Close())
	})

	req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/enqueue", EnqueueRequest{
		RepoPath:  repoDir,
		GitRef:    "HEAD",
		Agent:     "test",
		Reasoning: "fast",
	})
	w := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(w, req)

	require.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "resolve workflow config:")
}

func TestHandleEnqueueUsesWorktreeConfigWhenPresent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping worktree test on Windows due to path differences")
	}

	_ = testenv.SetDataDir(t)

	tmpDir := t.TempDir()
	mainRepo := filepath.Join(tmpDir, "main-repo")
	testutil.InitTestGitRepo(t, mainRepo)

	worktreeDir := filepath.Join(tmpDir, "worktree")
	wtCmd := exec.Command(
		"git", "-C", mainRepo, "worktree", "add", "-b", "wt-branch",
		worktreeDir,
	)
	out, err := wtCmd.CombinedOutput()
	require.NoError(t, err, "git worktree add failed: %s", out)

	err = os.WriteFile(
		filepath.Join(mainRepo, ".roborev.toml"),
		[]byte("review_model = ["),
		0o644,
	)
	require.NoError(t, err)
	err = os.WriteFile(
		filepath.Join(worktreeDir, ".roborev.toml"),
		[]byte(`review_reasoning = "maximum"`),
		0o644,
	)
	require.NoError(t, err)

	db, _ := testutil.OpenTestDBWithDir(t)
	server := NewServer(db, config.DefaultConfig(), "")
	t.Cleanup(func() {
		require.NoError(t, server.Close())
	})

	req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/enqueue", EnqueueRequest{
		RepoPath: worktreeDir,
		GitRef:   "HEAD",
		Agent:    "test",
	})
	w := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(w, req)

	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())

	var job storage.ReviewJob
	testutil.DecodeJSON(t, w, &job)
	resolvedWorktreeDir, err := filepath.EvalSymlinks(worktreeDir)
	if err != nil {
		resolvedWorktreeDir = worktreeDir
	}
	assert.Equal(t, "maximum", job.Reasoning)
	assert.Equal(t, filepath.Clean(resolvedWorktreeDir), job.WorktreePath)
}

func TestHandleListJobsSlashNormalization(t *testing.T) {
	server, db, tmpDir := newTestServer(t)

	// Store repos with forward-slash paths (matching ToSlash output)
	ws := filepath.ToSlash(tmpDir) + "/slash-ws"
	seedRepoWithJobs(t, db, ws+"/repo-a", 2, "sa")
	seedRepoWithJobs(t, db, ws+"/repo-b", 1, "sb")
	seedRepoWithJobs(t, db, filepath.ToSlash(tmpDir)+"/other-c", 1, "sc")

	t.Run("forward-slash prefix matches stored paths", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet,
			"/api/jobs?repo_prefix="+url.QueryEscape(ws), nil)
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)
		testutil.AssertStatusCode(t, w, http.StatusOK)

		var resp struct {
			Jobs []storage.ReviewJob `json:"jobs"`
		}
		testutil.DecodeJSON(t, w, &resp)

		if len(resp.Jobs) != 3 {
			assert.Condition(t, func() bool {
				return false
			}, "Expected 3 jobs with forward-slash prefix, got %d",
				len(resp.Jobs))
		}
		for _, j := range resp.Jobs {
			if !strings.HasPrefix(j.RepoPath, ws+"/") {
				assert.Condition(t, func() bool {
					return false
				}, "Job %d repo_path %q should be under %s",
					j.ID, j.RepoPath, ws)
			}
		}
	})
}

func TestHandleEnqueueMinSeverity(t *testing.T) {
	t.Run("valid min_severity stored as canonical lowercase", func(t *testing.T) {
		server, db, tmpDir := newTestServer(t)
		testutil.InitTestGitRepo(t, tmpDir)

		reqData := EnqueueRequest{
			RepoPath:    tmpDir,
			GitRef:      "HEAD",
			Agent:       "test",
			MinSeverity: "HIGH",
		}
		req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/enqueue", reqData)
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)
		require.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())

		jobs, err := db.ListJobs("", "", 0, 0)
		require.NoError(t, err)
		require.Len(t, jobs, 1)
		assert.Equal(t, "high", jobs[0].MinSeverity)
	})

	t.Run("invalid min_severity returns 400", func(t *testing.T) {
		server, _, tmpDir := newTestServer(t)
		testutil.InitTestGitRepo(t, tmpDir)

		reqData := EnqueueRequest{
			RepoPath:    tmpDir,
			GitRef:      "HEAD",
			Agent:       "test",
			MinSeverity: "bogus",
		}
		req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/enqueue", reqData)
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)
		assert.Equal(t, http.StatusBadRequest, w.Code)
		assert.Contains(t, w.Body.String(), "invalid min_severity")
	})
}
