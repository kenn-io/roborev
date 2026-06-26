package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
)

func respondJSON(
	w http.ResponseWriter, status int, payload any,
) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(payload)
}

func executeReviewCmd(
	args ...string,
) (string, string, error) {
	var stdout, stderr bytes.Buffer

	cmd := reviewCmd()
	// Mirror main.go: the production root sets SilenceUsage in
	// PersistentPreRunE, which a standalone reviewCmd() bypasses.
	cmd.SilenceUsage = true
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return stdout.String(), stderr.String(), err
}

func setupTestEnvironment(
	t *testing.T,
) (*TestGitRepo, *http.ServeMux) {
	t.Helper()
	mux := http.NewServeMux()
	daemonFromHandler(t, mux)
	return newTestGitRepo(t), mux
}

type capturedEnqueue struct {
	GitRef    string `json:"git_ref"`
	Reasoning string `json:"reasoning"`
}

type repoCommitSpec struct {
	path    string
	content string
	message string
}

func setupRepoWithCommits(
	t *testing.T, repo *TestGitRepo, commits ...repoCommitSpec,
) []string {
	t.Helper()
	shas := make([]string, 0, len(commits))
	for _, commit := range commits {
		shas = append(shas, repo.CommitFile(
			commit.path, commit.content, commit.message,
		))
	}
	return shas
}

func mockEnqueue(
	t *testing.T, mux *http.ServeMux,
) <-chan capturedEnqueue {
	t.Helper()
	ch := make(chan capturedEnqueue, 1)
	mux.HandleFunc("/api/enqueue", func(
		w http.ResponseWriter, r *http.Request,
	) {
		var req capturedEnqueue
		err := json.NewDecoder(r.Body).Decode(&req)
		assert.NoError(t, err, "decode enqueue request")
		select {
		case ch <- req:
		default:
			assert.Condition(t, func() bool {
				return false
			}, "mockEnqueue: unexpected extra enqueue request")
			http.Error(w, "duplicate request", http.StatusConflict)
			return
		}
		respondJSON(w, http.StatusCreated, storage.ReviewJob{
			ID: 1, GitRef: req.GitRef, Agent: "test",
		})
	})
	return ch
}

func mockEnqueueQueued(
	mux *http.ServeMux, gitRef string,
) {
	mux.HandleFunc("/api/enqueue", func(
		w http.ResponseWriter, r *http.Request,
	) {
		respondJSON(w, http.StatusCreated, storage.ReviewJob{
			ID: 1, GitRef: gitRef, Agent: "test",
			Status: "queued",
		})
	})
}

func handleJobsDone(
	w http.ResponseWriter, _ *http.Request, status storage.JobStatus,
) {
	job := storage.ReviewJob{
		ID: 1, GitRef: "abc123", Agent: "test",
		Status: status,
	}
	respondJSON(w, http.StatusOK, map[string]any{
		"jobs": []storage.ReviewJob{job}, "has_more": false,
	})
}

func mockWaitableReview(
	t *testing.T, mux *http.ServeMux, output string,
) {
	t.Helper()
	mockEnqueueQueued(mux, "abc123")
	mux.HandleFunc("/api/jobs", func(
		w http.ResponseWriter, r *http.Request,
	) {
		handleJobsDone(w, r, storage.JobStatusDone)
	})
	mux.HandleFunc("/api/review", func(
		w http.ResponseWriter, r *http.Request,
	) {
		respondJSON(w, http.StatusOK, storage.Review{
			ID: 1, JobID: 1, Agent: "test", Output: output,
		})
	})
}

func TestEnqueueCmdPositionalArg(t *testing.T) {
	t.Run("positional arg overrides default HEAD", func(t *testing.T) {
		repo, mux := setupTestEnvironment(t)
		reqCh := mockEnqueue(t, mux)

		shas := setupRepoWithCommits(t, repo,
			repoCommitSpec{"file1.txt", "first", "first commit"},
			repoCommitSpec{"file2.txt", "second", "second commit"},
		)

		shortFirstSHA := shas[0][:7]
		_, _, err := executeReviewCmd("--repo", repo.Dir, shortFirstSHA)
		require.NoError(t, err, "enqueue failed: %v")

		req := <-reqCh
		assert.Equal(t, req.GitRef, shortFirstSHA)
		assert.NotEqual(t, "HEAD", req.GitRef)
	})

	t.Run("sha flag works", func(t *testing.T) {
		repo, mux := setupTestEnvironment(t)
		reqCh := mockEnqueue(t, mux)

		shas := setupRepoWithCommits(t, repo,
			repoCommitSpec{"file1.txt", "first", "first commit"},
			repoCommitSpec{"file2.txt", "second", "second commit"},
		)

		shortFirstSHA := shas[0][:7]
		_, _, err := executeReviewCmd("--repo", repo.Dir, "--sha", shortFirstSHA)
		require.NoError(t, err, "enqueue failed: %v")

		req := <-reqCh
		assert.Equal(t, req.GitRef, shortFirstSHA)
	})

	t.Run("defaults to HEAD", func(t *testing.T) {
		repo, mux := setupTestEnvironment(t)
		reqCh := mockEnqueue(t, mux)

		setupRepoWithCommits(t, repo,
			repoCommitSpec{"file1.txt", "first", "first commit"},
		)

		_, _, err := executeReviewCmd("--repo", repo.Dir)
		require.NoError(t, err, "enqueue failed: %v")

		req := <-reqCh
		assert.Equal(t, "HEAD", req.GitRef)
	})
}

func TestEnqueueSkippedBranch(t *testing.T) {
	t.Run("skipped response prints message and exits successfully", func(t *testing.T) {
		repo, mux := setupTestEnvironment(t)
		mux.HandleFunc("/api/enqueue", func(w http.ResponseWriter, r *http.Request) {
			respondJSON(w, http.StatusOK, map[string]any{
				"skipped": true,
				"reason":  "branch \"wip\" is excluded from reviews",
			})
		})

		repo.CommitFile("file.txt", "content", "initial commit")

		stdout, _, err := executeReviewCmd("--repo", repo.Dir)
		require.NoError(t, err)

		assert.Contains(t, stdout, "Skipped")
	})

	t.Run("skipped response in quiet mode suppresses output", func(t *testing.T) {
		repo, mux := setupTestEnvironment(t)
		mux.HandleFunc("/api/enqueue", func(w http.ResponseWriter, r *http.Request) {
			respondJSON(w, http.StatusOK, map[string]any{
				"skipped": true,
				"reason":  "branch \"wip\" is excluded from reviews",
			})
		})

		repo.CommitFile("file.txt", "content", "initial commit")

		stdout, _, err := executeReviewCmd("--repo", repo.Dir, "--quiet")
		require.NoError(t, err)

		assert.Empty(t, stdout)
	})
}

func TestWaitQuietVerdictExitCode(t *testing.T) {
	setupFastPolling(t)

	t.Run("passing review exits 0 with no output", func(t *testing.T) {
		repo, mux := setupTestEnvironment(t)
		repo.CommitFile("file.txt", "content", "initial commit")
		mockWaitableReview(t, mux, "No issues found.")

		stdout, stderr, err := executeReviewCmd("--repo", repo.Dir, "--wait", "--quiet")

		require.NoError(t, err)
		assert.Empty(t, stdout)
		assert.Empty(t, stderr)
	})

	t.Run("failing review exits 1 with no output", func(t *testing.T) {
		repo, mux := setupTestEnvironment(t)
		repo.CommitFile("file.txt", "content", "initial commit")
		mockWaitableReview(t, mux,
			"Found 2 issues:\n1. Bug in foo.go\n2. Missing error handling",
		)

		stdout, stderr, err := executeReviewCmd("--repo", repo.Dir, "--wait", "--quiet")

		require.Error(t, err, "expected review command to fail in this case")
		var exitErr *exitError
		require.ErrorAs(t, err, &exitErr)
		require.Equal(t, 1, exitErr.code, "expected exit code 1")
		assert.Empty(t, stdout)
		assert.Empty(t, stderr)
	})
}

func TestWaitForJobUnknownStatus(t *testing.T) {
	setupFastPolling(t)

	t.Run("unknown status exceeds max retries", func(t *testing.T) {
		repo, mux := setupTestEnvironment(t)
		setupRepoWithCommits(t, repo,
			repoCommitSpec{"file.txt", "content", "initial commit"},
		)

		callCount := 0
		mockEnqueueQueued(mux, "abc123")
		mux.HandleFunc("/api/jobs", func(w http.ResponseWriter, r *http.Request) {
			callCount++
			handleJobsDone(w, r, storage.JobStatus("future_status"))
		})

		_, _, err := executeReviewCmd("--repo", repo.Dir, "--wait", "--quiet")

		assertErrorContains(t, err, "unknown status")
		assertErrorContains(t, err, "daemon may be newer than CLI")

		assert.Equal(t, 10, callCount)
	})

	t.Run("counter resets on known status", func(t *testing.T) {
		repo, mux := setupTestEnvironment(t)
		setupRepoWithCommits(t, repo,
			repoCommitSpec{"file.txt", "content", "initial commit"},
		)

		poller := &mockJobPoller{}

		mockEnqueueQueued(mux, "abc123")
		mux.HandleFunc("/api/jobs", poller.HandleJobs)
		mux.HandleFunc("/api/review", func(w http.ResponseWriter, r *http.Request) {
			respondJSON(w, http.StatusOK, storage.Review{
				ID:     1,
				JobID:  1,
				Agent:  "test",
				Output: "No issues found. LGTM!",
			})
		})

		_, _, err := executeReviewCmd("--repo", repo.Dir, "--wait", "--quiet")

		require.NoError(t, err)
		assert.Equal(t, 12, poller.callCount)
	})
}

type mockJobPoller struct {
	callCount int
}

func (m *mockJobPoller) HandleJobs(
	w http.ResponseWriter, r *http.Request,
) {
	m.callCount++
	var status string
	switch {
	case m.callCount <= 5:
		status = "future_status"
	case m.callCount == 6:
		status = "running"
	case m.callCount <= 11:
		status = "future_status"
	default:
		status = "done"
	}
	handleJobsDone(w, r, storage.JobStatus(status))
}

func TestReviewFlagValidation(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want []string
	}{
		{
			"since and branch exclusive",
			[]string{"--since", "abc123", "--branch"},
			[]string{"cannot use --branch with --since"},
		},
		{
			"since and dirty exclusive",
			[]string{"--since", "abc123", "--dirty"},
			[]string{"cannot use --since with --dirty"},
		},
		{
			"since with positional args",
			[]string{"--since", "abc123", "def456"},
			[]string{"cannot specify commits with --since"},
		},
		{
			"invalid review type",
			[]string{"--type", "bogus"},
			[]string{`invalid --type "bogus"`, "security, design, lookahead"},
		},
		{
			"branch and dirty exclusive",
			[]string{"--branch", "--dirty"},
			[]string{"cannot use --branch with --dirty"},
		},
		{
			"branch with positional args",
			[]string{"--branch", "abc123"},
			[]string{
				"cannot specify commits with --branch",
				"--branch=<name>",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo, _ := setupTestEnvironment(t)
			_, _, err := executeReviewCmd(
				append([]string{"--repo", repo.Dir}, tt.args...)...,
			)
			for _, w := range tt.want {
				assertErrorContains(t, err, w)
			}
		})
	}
}

func TestReviewSinceFlag(t *testing.T) {
	t.Run("since with valid ref succeeds", func(t *testing.T) {
		repo, mux := setupTestEnvironment(t)
		reqCh := mockEnqueue(t, mux)

		shas := setupRepoWithCommits(t, repo,
			repoCommitSpec{"file1.txt", "first", "first commit"},
			repoCommitSpec{"file2.txt", "second", "second commit"},
		)

		_, _, err := executeReviewCmd("--repo", repo.Dir, "--since", shas[0][:7])
		require.NoError(t, err)

		req := <-reqCh
		assert.Contains(t, req.GitRef, shas[0])
		assert.True(t, strings.HasSuffix(req.GitRef, "..HEAD"))
	})

	t.Run("since with invalid ref fails", func(t *testing.T) {
		repo, _ := setupTestEnvironment(t)
		repo.CommitFile("file.txt", "content", "initial")

		_, _, err := executeReviewCmd("--repo", repo.Dir, "--since", "nonexistent123")
		assertErrorContains(t, err, "invalid --since commit")
	})

	t.Run("since with no commits ahead fails", func(t *testing.T) {
		repo, _ := setupTestEnvironment(t)
		repo.CommitFile("file.txt", "content", "initial")

		_, _, err := executeReviewCmd("--repo", repo.Dir, "--since", "HEAD")
		assertErrorContains(t, err, "no commits since")
	})
}

func TestReviewBranchFlag(t *testing.T) {
	t.Run("branch on default branch fails", func(t *testing.T) {
		repo, _ := setupTestEnvironment(t)

		repo.Run("symbolic-ref", "HEAD", "refs/heads/main")
		repo.CommitFile("file.txt", "content", "initial")

		_, _, err := executeReviewCmd("--repo", repo.Dir, "--branch")
		assertErrorContains(t, err, "already on main")
	})

	t.Run("branch with no commits fails", func(t *testing.T) {
		repo, _ := setupTestEnvironment(t)

		repo.Run("symbolic-ref", "HEAD", "refs/heads/main")
		repo.CommitFile("file.txt", "content", "initial")
		repo.Run("checkout", "-b", "feature")

		_, _, err := executeReviewCmd("--repo", repo.Dir, "--branch")
		assertErrorContains(t, err, "no commits on branch")
	})

	t.Run("branch review succeeds with commits", func(t *testing.T) {
		repo, mux := setupTestEnvironment(t)
		reqCh := mockEnqueue(t, mux)

		repo.Run("symbolic-ref", "HEAD", "refs/heads/main")
		repo.CommitFile("file.txt", "content", "initial")
		mainSHA := repo.Run("rev-parse", "HEAD")
		repo.Run("checkout", "-b", "feature")
		repo.CommitFile("feature.txt", "feature", "feature commit")

		_, _, err := executeReviewCmd("--repo", repo.Dir, "--branch")
		require.NoError(t, err)

		req := <-reqCh
		assert.Contains(t, req.GitRef, mainSHA)
		assert.True(t, strings.HasSuffix(req.GitRef, "..HEAD"))
	})

	t.Run("branch review uses branch base before url upstream", func(t *testing.T) {
		repo, mux := setupTestEnvironment(t)
		reqCh := mockEnqueue(t, mux)

		repo.Run("symbolic-ref", "HEAD", "refs/heads/main")
		repo.CommitFile("file.txt", "content", "initial")
		mainSHA := repo.Run("rev-parse", "HEAD")
		repo.Run("checkout", "-b", "feature")
		repo.Run("config", "branch.feature.remote", "https://example.com/fork.git")
		repo.Run("config", "branch.feature.merge", "refs/heads/feature")
		repo.Run("config", "branch.feature.base", "main")
		repo.CommitFile("feature.txt", "feature", "feature commit")

		_, _, err := executeReviewCmd("--repo", repo.Dir, "--branch")
		require.NoError(t, err)

		req := <-reqCh
		assert.Equal(t, mainSHA+"..HEAD", req.GitRef)
	})

	t.Run("branch review uses origin counterpart when local main is stale after rebase", func(t *testing.T) {
		// Regression: a feature branch rebased onto origin/main with a stale
		// local main pointing at an older commit must review only the new
		// branch commits — not the trunk commits that already landed on
		// origin/main between the local main pointer and the rebase target.
		repo, mux := setupTestEnvironment(t)
		reqCh := mockEnqueue(t, mux)

		repo.Run("symbolic-ref", "HEAD", "refs/heads/main")
		repo.CommitFile("base.txt", "base", "initial")
		staleMainSHA := repo.Run("rev-parse", "HEAD")
		// Two commits land on the trunk after local main was last updated.
		repo.CommitFile("trunk1.txt", "t1", "trunk advance 1")
		repo.CommitFile("trunk2.txt", "t2", "trunk advance 2")
		freshOriginSHA := repo.Run("rev-parse", "HEAD")
		// Rewind local main to simulate a stale checkout.
		repo.Run("update-ref", "refs/heads/main", staleMainSHA)
		repo.Run("remote", "add", "origin", "/dev/null")
		repo.Run("update-ref", "refs/remotes/origin/main", freshOriginSHA)
		// Feature branch was rebased onto origin/main.
		repo.Run("checkout", "-b", "feature", freshOriginSHA)
		repo.CommitFile("feature.txt", "f", "feature commit")
		repo.Run("config", "branch.feature.base", "main")

		_, _, err := executeReviewCmd("--repo", repo.Dir, "--branch")
		require.NoError(t, err)

		req := <-reqCh
		assert.Equal(t, freshOriginSHA+"..HEAD", req.GitRef,
			"range must start at origin/main, not stale local main")
	})
}

func TestReviewFastFlag(t *testing.T) {
	t.Run("fast flag sets reasoning to fast", func(t *testing.T) {
		repo, mux := setupTestEnvironment(t)
		reqCh := mockEnqueue(t, mux)

		repo.CommitFile("file.txt", "content", "initial")

		_, _, err := executeReviewCmd("--repo", repo.Dir, "--fast")
		require.NoError(t, err)

		req := <-reqCh
		assert.Equal(t, "fast", req.Reasoning)
	})

	t.Run("explicit reasoning takes precedence over fast", func(t *testing.T) {
		repo, mux := setupTestEnvironment(t)
		reqCh := mockEnqueue(t, mux)

		repo.CommitFile("file.txt", "content", "initial")

		_, _, err := executeReviewCmd("--repo", repo.Dir, "--fast", "--reasoning", "thorough")
		require.NoError(t, err)

		req := <-reqCh
		assert.Equal(t, "thorough", req.Reasoning)
	})
}

func TestReviewInvalidArgsNoSideEffects(t *testing.T) {
	repo, mux := setupTestEnvironment(t)

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		assert.Condition(t, func() bool {
			return false
		}, "unexpected request %s %s", r.Method, r.URL.Path)
	})

	hooksDir := filepath.Join(repo.Dir, ".git", "hooks")

	_, _, err := executeReviewCmd("--repo", repo.Dir, "--branch", "--dirty")
	assertErrorContains(t, err, "cannot use --branch with --dirty")

	for _, name := range []string{"post-commit", "post-rewrite"} {
		_, statErr := os.Stat(
			filepath.Join(hooksDir, name),
		)
		require.ErrorIs(t, statErr, os.ErrNotExist, "%s should not exist after invalid args", name)
	}
}

func writeRoborevConfig(t *testing.T, repo *TestGitRepo, content string) {
	t.Helper()
	err := os.WriteFile(
		filepath.Join(repo.Dir, ".roborev.toml"),
		[]byte(content), 0o644,
	)
	require.NoError(t, err, "write .roborev.toml: %v")
}

func TestTryBranchReview(t *testing.T) {
	t.Run("returns false when no config", func(t *testing.T) {
		repo := newTestGitRepo(t)
		repo.Run("symbolic-ref", "HEAD", "refs/heads/main")
		repo.CommitFile("file.txt", "content", "initial")
		repo.Run("checkout", "-b", "feature")
		repo.CommitFile("feature.txt", "feature", "feature commit")

		_, ok := tryBranchReview(t.Context(), repo.Dir, "")
		assert.False(t, ok)
	})

	t.Run("returns false when config is commit", func(t *testing.T) {
		repo := newTestGitRepo(t)
		repo.Run("symbolic-ref", "HEAD", "refs/heads/main")
		repo.CommitFile("file.txt", "content", "initial")
		repo.Run("checkout", "-b", "feature")
		repo.CommitFile("feature.txt", "feature", "feature commit")
		writeRoborevConfig(t, repo, `post_commit_review = "commit"`)

		_, ok := tryBranchReview(t.Context(), repo.Dir, "")
		assert.False(t, ok)
	})

	t.Run("returns merge-base range when config is branch", func(t *testing.T) {
		repo := newTestGitRepo(t)
		repo.Run("symbolic-ref", "HEAD", "refs/heads/main")
		repo.CommitFile("file.txt", "content", "initial")
		mainSHA := repo.Run("rev-parse", "HEAD")
		repo.Run("checkout", "-b", "feature")
		repo.CommitFile("feature.txt", "feature", "feature commit")
		writeRoborevConfig(t, repo, `post_commit_review = "branch"`)

		ref, ok := tryBranchReview(t.Context(), repo.Dir, "")
		require.True(t, ok, "expected true with branch config")

		assert.Contains(t, ref, mainSHA)
		assert.True(t, strings.HasSuffix(ref, "..HEAD"))
	})

	t.Run("covers multiple commits on feature branch", func(t *testing.T) {
		repo := newTestGitRepo(t)
		repo.Run("symbolic-ref", "HEAD", "refs/heads/main")
		repo.CommitFile("file.txt", "content", "initial")
		mainSHA := repo.Run("rev-parse", "HEAD")
		repo.Run("checkout", "-b", "feature")
		repo.CommitFile("a.txt", "a", "first feature commit")
		repo.CommitFile("b.txt", "b", "second feature commit")
		repo.CommitFile("c.txt", "c", "third feature commit")
		writeRoborevConfig(t, repo, `post_commit_review = "branch"`)

		ref, ok := tryBranchReview(t.Context(), repo.Dir, "")
		require.True(t, ok, "expected true")

		want := mainSHA + "..HEAD"
		assert.Equal(t, want, ref)
	})

	t.Run("returns false on base branch", func(t *testing.T) {
		repo := newTestGitRepo(t)
		repo.Run("symbolic-ref", "HEAD", "refs/heads/main")
		repo.CommitFile("file.txt", "content", "initial")
		writeRoborevConfig(t, repo, `post_commit_review = "branch"`)

		_, ok := tryBranchReview(t.Context(), repo.Dir, "")
		assert.False(t, ok)
	})

	t.Run("uses baseBranch override", func(t *testing.T) {
		repo := newTestGitRepo(t)
		repo.Run("symbolic-ref", "HEAD", "refs/heads/develop")
		repo.CommitFile("file.txt", "content", "initial")
		developSHA := repo.Run("rev-parse", "HEAD")
		repo.Run("checkout", "-b", "feature")
		repo.CommitFile("feature.txt", "feature", "feature commit")
		writeRoborevConfig(t, repo, `post_commit_review = "branch"`)

		ref, ok := tryBranchReview(t.Context(), repo.Dir, "develop")
		require.True(t, ok, "expected true with baseBranch override")

		want := developSHA + "..HEAD"
		assert.Equal(t, want, ref)
	})

	t.Run("uses branch base before url upstream", func(t *testing.T) {
		repo := newTestGitRepo(t)
		repo.Run("symbolic-ref", "HEAD", "refs/heads/main")
		repo.CommitFile("file.txt", "content", "initial")
		mainSHA := repo.Run("rev-parse", "HEAD")
		repo.Run("checkout", "-b", "feature")
		repo.Run("config", "branch.feature.remote", "https://example.com/fork.git")
		repo.Run("config", "branch.feature.merge", "refs/heads/feature")
		repo.Run("config", "branch.feature.base", "main")
		repo.CommitFile("feature.txt", "feature", "feature commit")
		writeRoborevConfig(t, repo, `post_commit_review = "branch"`)

		ref, ok := tryBranchReview(t.Context(), repo.Dir, "")
		require.True(t, ok, "expected branch base config to enable branch review")
		assert.Equal(t, mainSHA+"..HEAD", ref)
	})

	t.Run("returns false on detached HEAD", func(t *testing.T) {
		repo := newTestGitRepo(t)
		repo.Run("symbolic-ref", "HEAD", "refs/heads/main")
		repo.CommitFile("file.txt", "content", "initial")
		sha := repo.Run("rev-parse", "HEAD")
		repo.Run("checkout", sha)
		writeRoborevConfig(t, repo, `post_commit_review = "branch"`)

		_, ok := tryBranchReview(t.Context(), repo.Dir, "")
		assert.False(t, ok)
	})

	t.Run("allows feature branch tracking its own remote counterpart", func(t *testing.T) {
		// Regression: using @{upstream} as base for a feature tracking
		// origin/feature would skip already-pushed feature commits, which
		// contradicts "--branch reviews all commits since trunk". With
		// UpstreamIsTrunk gating, origin/feature (not trunk-named) is
		// rejected and the review falls back to the repository's default
		// branch for the merge base.
		remote := newBareTestGitRepo(t)
		repo := newTestGitRepo(t)
		repo.Run("symbolic-ref", "HEAD", "refs/heads/main")
		repo.CommitFile("file.txt", "content", "initial")
		repo.Run("remote", "add", "origin", remote.Dir)
		repo.Run("push", "-u", "origin", "main")
		repo.Run("remote", "set-head", "origin", "main")
		mainSHA := repo.Run("rev-parse", "origin/main")
		repo.Run("checkout", "-b", "feature")
		repo.CommitFile("first.txt", "first", "first feature commit")
		repo.Run("push", "-u", "origin", "feature")
		repo.CommitFile("second.txt", "second", "unpushed commit")
		writeRoborevConfig(t, repo, `post_commit_review = "branch"`)

		ref, ok := tryBranchReview(t.Context(), repo.Dir, "")
		require.True(t, ok, "feature tracking origin/feature with unpushed commits must still enqueue a review")
		// Merge-base range runs from origin/main (trunk), covering BOTH the
		// pushed and unpushed feature commits — the documented --branch
		// semantic. Using @{u}=origin/feature would have skipped the pushed
		// one.
		assert.Equal(t, mainSHA+"..HEAD", ref)
	})

	t.Run("skips when upstream is configured but unresolvable", func(t *testing.T) {
		// Regression: when @{upstream} is configured but the remote-tracking
		// ref has not been fetched, tryBranchReview must skip rather than
		// silently fall back to origin/main or local main — that fallback
		// would enqueue a review against the wrong commit range in fork
		// workflows.
		repo := newTestGitRepo(t)
		repo.Run("symbolic-ref", "HEAD", "refs/heads/main")
		repo.CommitFile("file.txt", "content", "initial")
		repo.Run("checkout", "-b", "feature")
		repo.CommitFile("feature.txt", "feature", "feature commit")
		// Configure tracking against an upstream that never resolves locally.
		repo.Run("config", "branch.feature.remote", "upstream")
		repo.Run("config", "branch.feature.merge", "refs/heads/main")
		writeRoborevConfig(t, repo, `post_commit_review = "branch"`)

		ref, ok := tryBranchReview(t.Context(), repo.Dir, "")
		assert.False(t, ok, "must skip when upstream is configured but missing")
		assert.Empty(t, ref)
	})

	t.Run("blocks local main tracking non-origin upstream", func(t *testing.T) {
		// Regression: LocalBranchName only stripped "origin/", so
		// current="main" vs base="upstream/main" missed the guardrail
		// and produced a branch review on the base branch itself.
		remote := newBareTestGitRepo(t)
		repo := newTestGitRepo(t)
		repo.Run("symbolic-ref", "HEAD", "refs/heads/main")
		repo.CommitFile("file.txt", "content", "initial")
		repo.Run("remote", "add", "upstream", remote.Dir)
		repo.Run("push", "-u", "upstream", "main")
		writeRoborevConfig(t, repo, `post_commit_review = "branch"`)

		_, ok := tryBranchReview(t.Context(), repo.Dir, "")
		assert.False(t, ok, "local main tracking upstream/main must be treated as base branch")
	})

	t.Run("uses origin counterpart when local base is stale ancestor", func(t *testing.T) {
		// Mirrors the rebase-onto-origin/main scenario for the post-commit
		// hook path (tryBranchReview): branch.<feature>.base = main with a
		// stale local main must resolve to origin/main, not the rewound
		// pointer, so the hook enqueues only the real branch commits.
		repo := newTestGitRepo(t)
		repo.Run("symbolic-ref", "HEAD", "refs/heads/main")
		repo.CommitFile("base.txt", "base", "initial")
		staleMainSHA := repo.Run("rev-parse", "HEAD")
		repo.CommitFile("trunk.txt", "t", "trunk advance")
		freshOriginSHA := repo.Run("rev-parse", "HEAD")
		repo.Run("update-ref", "refs/heads/main", staleMainSHA)
		repo.Run("remote", "add", "origin", "/dev/null")
		repo.Run("update-ref", "refs/remotes/origin/main", freshOriginSHA)
		repo.Run("checkout", "-b", "feature", freshOriginSHA)
		repo.CommitFile("feature.txt", "f", "feature commit")
		repo.Run("config", "branch.feature.base", "main")
		writeRoborevConfig(t, repo, `post_commit_review = "branch"`)

		ref, ok := tryBranchReview(t.Context(), repo.Dir, "")
		require.True(t, ok, "expected branch review with stale local main")
		assert.Equal(t, freshOriginSHA+"..HEAD", ref,
			"range must start at origin/main, not stale local main")
	})

	t.Run("prefers branch upstream over default branch", func(t *testing.T) {
		// Reproduces the "single commit past upstream" bug: when the default
		// branch (e.g., origin/main) lags behind the tip the branch actually
		// diverged from (e.g., upstream/main), the review must use the
		// upstream tracking ref, not the default branch, so already-merged
		// commits are not re-reviewed.
		remote := newBareTestGitRepo(t)
		repo := newTestGitRepo(t)
		repo.Run("symbolic-ref", "HEAD", "refs/heads/main")
		repo.CommitFile("file.txt", "u1", "upstream c1")
		repo.CommitFile("file.txt", "u1\nu2", "upstream c2")
		repo.CommitFile("file.txt", "u1\nu2\nu3", "upstream c3")
		repo.Run("remote", "add", "upstream", remote.Dir)
		repo.Run("push", "-u", "upstream", "main")

		// Simulate origin lagging behind upstream by 2 commits.
		staleOrigin := newBareTestGitRepo(t)
		repo.Run("remote", "add", "origin", staleOrigin.Dir)
		repo.Run("push", "origin", "HEAD~2:refs/heads/main")
		repo.Run("fetch", "origin")
		repo.Run("remote", "set-head", "origin", "main")

		repo.Run("checkout", "-b", "feature", "--track", "upstream/main")
		repo.CommitFile("feature.txt", "only-new-commit", "feature commit")
		writeRoborevConfig(t, repo, `post_commit_review = "branch"`)

		ref, ok := tryBranchReview(t.Context(), repo.Dir, "")
		require.True(t, ok, "expected branch review to run")

		upstreamSHA := repo.Run("rev-parse", "upstream/main")
		want := upstreamSHA + "..HEAD"
		assert.Equal(t, want, ref,
			"review must compare against upstream/main, not origin/main")
	})
}

func TestReviewIgnoresBranchConfig(t *testing.T) {
	repo, mux := setupTestEnvironment(t)
	reqCh := mockEnqueue(t, mux)

	repo.Run("symbolic-ref", "HEAD", "refs/heads/main")
	repo.CommitFile("file.txt", "content", "initial")
	repo.Run("checkout", "-b", "feature")
	repo.CommitFile("feature.txt", "feature", "feature commit")
	writeRoborevConfig(t, repo, `post_commit_review = "branch"`)

	_, _, err := executeReviewCmd("--repo", repo.Dir)
	require.NoError(t, err)

	req := <-reqCh
	assert.Equal(t, "HEAD", req.GitRef)
}

func TestReviewQuietIgnoresBranchConfig(t *testing.T) {
	repo, mux := setupTestEnvironment(t)
	reqCh := mockEnqueue(t, mux)

	repo.Run("symbolic-ref", "HEAD", "refs/heads/main")
	repo.CommitFile("file.txt", "content", "initial")
	repo.Run("checkout", "-b", "feature")
	repo.CommitFile("feature.txt", "feature", "feature commit")
	writeRoborevConfig(t, repo, `post_commit_review = "branch"`)

	_, _, err := executeReviewCmd("--repo", repo.Dir, "--quiet")
	require.NoError(t, err)

	req := <-reqCh
	assert.Equal(t, "HEAD", req.GitRef)
}

func TestFindChildGitRepos(t *testing.T) {
	parent := t.TempDir()

	regularRepo := filepath.Join(parent, "regular")
	os.Mkdir(regularRepo, 0o755)
	os.Mkdir(filepath.Join(regularRepo, ".git"), 0o755)

	worktreeRepo := filepath.Join(parent, "worktree")
	os.Mkdir(worktreeRepo, 0o755)
	os.WriteFile(
		filepath.Join(worktreeRepo, ".git"),
		[]byte("gitdir: /some/main/.git/worktrees/wt"),
		0o644,
	)

	plainDir := filepath.Join(parent, "plain")
	os.Mkdir(plainDir, 0o755)

	hiddenDir := filepath.Join(parent, ".hidden")
	os.Mkdir(hiddenDir, 0o755)
	os.Mkdir(filepath.Join(hiddenDir, ".git"), 0o755)

	repos := findChildGitRepos(parent)

	assert.Len(t, repos, 2)

	found := make(map[string]bool)
	for _, r := range repos {
		found[r] = true
	}
	assert.True(t, found["regular"])
	assert.True(t, found["worktree"])
	assert.False(t, found["plain"])
	assert.False(t, found[".hidden"])
}

func TestFindChildGitReposHintPaths(t *testing.T) {
	parent := t.TempDir()

	repoDir := filepath.Join(parent, "my-repo")
	os.Mkdir(repoDir, 0o755)
	os.Mkdir(filepath.Join(repoDir, ".git"), 0o755)

	_, _, err := executeReviewCmd("--repo", parent)
	require.Error(t, err, "Expected error for non-git directory")

	errMsg := err.Error()
	expectedPath := filepath.Join(parent, "my-repo")
	assert.Contains(t, errMsg, expectedPath, "Hint should contain full path %q, got: %s", expectedPath, errMsg)
}
