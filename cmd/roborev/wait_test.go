package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"testing"

	"github.com/roborev-dev/roborev/internal/storage"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type mockConfig struct {
	Jobs        []storage.ReviewJob
	Review      *storage.Review
	JobsErr     int                   // Optional HTTP error code
	OnJobsQuery func(r *http.Request) // Optional spy callback
}

func newWaitMockHandler(cfg mockConfig) http.HandlerFunc {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/jobs", func(w http.ResponseWriter, r *http.Request) {
		if cfg.OnJobsQuery != nil {
			cfg.OnJobsQuery(r)
		}
		if cfg.JobsErr != 0 {
			w.WriteHeader(cfg.JobsErr)
			// For compatibility with existing tests expecting specific error body
			if cfg.JobsErr == http.StatusInternalServerError {
				w.Write([]byte("database locked"))
			}
			return
		}
		w.WriteHeader(http.StatusOK)
		// Ensure empty slice is encoded as [] not null if initialized but empty
		jobs := cfg.Jobs
		if jobs == nil {
			jobs = []storage.ReviewJob{}
		}
		json.NewEncoder(w).Encode(map[string]any{
			"jobs":     jobs,
			"has_more": false,
		})
	})
	mux.HandleFunc("/api/review", func(w http.ResponseWriter, r *http.Request) {
		if cfg.Review != nil {
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(cfg.Review)
		} else {
			w.WriteHeader(http.StatusNotFound)
		}
	})
	return mux.ServeHTTP
}

// requireExitCode asserts err is an *exitError with the expected code.
func requireExitCode(t *testing.T, err error, code int) {
	t.Helper()
	require.Error(t, err)
	var exitErr *exitError
	require.ErrorAs(t, err, &exitErr)
	assert.Equal(t, exitErr.code, code)
}

// waitEnv holds common test fixtures for wait command tests.
type waitEnv struct {
	repo *TestGitRepo
	sha  string
}

// newWaitEnv creates a git repo with a commit, starts a mock daemon,
// and chdirs into the repo. Cleanup is automatic via t.Cleanup.
func newWaitEnv(t *testing.T, handler http.Handler) *waitEnv {
	t.Helper()
	repo := newTestGitRepo(t)
	sha := repo.CommitFile("file.txt", "content", "initial commit")
	daemonFromHandler(t, handler)
	chdir(t, repo.Dir)
	return &waitEnv{repo: repo, sha: sha}
}

// runWait executes the wait command with the given args and returns
// captured stdout and the error. It mirrors main.go's setup by silencing
// the usage block for runtime errors (the production root sets this in
// PersistentPreRunE, which a freshly-constructed waitCmd() bypasses).
func runWait(t *testing.T, args ...string) (stdout string, err error) {
	t.Helper()
	var out bytes.Buffer
	cmd := waitCmd()
	cmd.SilenceUsage = true
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs(args)
	err = cmd.Execute()
	return out.String(), err
}

func TestWaitArgValidation(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{
			name:    "job flag without argument",
			args:    []string{"--job"},
			wantErr: "--job requires a job ID argument",
		},
		{
			name:    "sha and positional arg conflict",
			args:    []string{"--sha", "HEAD", "42"},
			wantErr: "cannot use both",
		},
		{
			name:    "invalid positional arg",
			args:    []string{"not-a-ref-or-number"},
			wantErr: "not a valid git ref or job ID",
		},
		{
			name:    "positional zero is invalid",
			args:    []string{"0"},
			wantErr: "not a valid git ref or job ID",
		},
		{
			name:    "invalid sha ref",
			args:    []string{"--sha", "not-a-valid-ref"},
			wantErr: "invalid git ref",
		},
		{
			name:    "job flag rejects non-positive ID",
			args:    []string{"--job", "0"},
			wantErr: "invalid job ID",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			newWaitEnv(t, newWaitMockHandler(mockConfig{}))
			_, err := runWait(t, tc.args...)
			assertErrorContains(t, err, tc.wantErr)
		})
	}
}

func TestWaitArgValidationWithoutDaemon(t *testing.T) {
	// Validation errors should be returned before contacting the daemon.
	// This test does NOT start a mock daemon.
	repo := newTestGitRepo(t)
	repo.CommitFile("file.txt", "content", "initial commit")
	chdir(t, repo.Dir)

	_, err := runWait(t, "--job", "0")
	assertErrorContains(t, err, "invalid job ID")

	_, err = runWait(t, "--sha", "not-a-valid-ref")
	assertErrorContains(t, err, "invalid git ref")
}

func TestWait_Scenarios(t *testing.T) {
	setupFastPolling(t)

	cases := []struct {
		name          string
		args          []string
		mock          mockConfig
		expectErr     bool
		expectErrCode int
		expectOut     string
	}{
		{
			name:          "No job for SHA",
			args:          []string{"--sha", "HEAD"},
			mock:          mockConfig{}, // No jobs
			expectErr:     true,
			expectErrCode: 1,
			expectOut:     "No job found",
		},
		{
			name:          "Quiet mode suppresses output",
			args:          []string{"--sha", "HEAD", "--quiet"},
			mock:          mockConfig{},
			expectErr:     true,
			expectErrCode: 1,
			expectOut:     "",
		},
		{
			name:          "Job ID not found",
			args:          []string{"--job", "99999"},
			mock:          mockConfig{},
			expectErr:     true,
			expectErrCode: 1,
			expectOut:     "No job found",
		},
		{
			name: "Passing Review",
			args: []string{"--sha", "HEAD", "--quiet"},
			mock: mockConfig{
				Jobs:   []storage.ReviewJob{{ID: 1, Agent: "test", Status: "done"}},
				Review: &storage.Review{ID: 1, JobID: 1, Agent: "test", Output: "No issues found."},
			},
			expectErr: false,
		},
		{
			name: "Failing Review",
			args: []string{"--sha", "HEAD", "--quiet"},
			mock: mockConfig{
				Jobs:   []storage.ReviewJob{{ID: 1, Agent: "test", Status: "done"}},
				Review: &storage.Review{ID: 1, JobID: 1, Agent: "test", Output: "Found 2 issues:\n1. Bug\n2. Missing check"},
			},
			expectErr:     true,
			expectErrCode: 1,
		},
		{
			name: "Positional Arg As Git Ref",
			args: []string{"HEAD", "--quiet"},
			mock: mockConfig{
				Jobs:   []storage.ReviewJob{{ID: 1, Agent: "test", Status: "done"}},
				Review: &storage.Review{ID: 1, JobID: 1, Agent: "test", Output: "No issues found."},
			},
			expectErr: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			newWaitEnv(t, newWaitMockHandler(tc.mock))
			stdout, err := runWait(t, tc.args...)

			if tc.expectErr {
				requireExitCode(t, err, tc.expectErrCode)
			} else if err != nil {
				require.NoError(t, err)
			}

			if tc.expectOut == "" {
				assert.Empty(t, stdout)
			} else {
				assert.Contains(t, stdout, tc.expectOut)
			}
		})
	}
}

func TestWaitJobIDPollingNon200Response(t *testing.T) {
	setupFastPolling(t)
	newWaitEnv(t, newWaitMockHandler(mockConfig{
		JobsErr: http.StatusInternalServerError,
	}))

	_, err := runWait(t, "--job", "42")
	assertErrorContains(t, err, "500")
}

func TestWaitNumericFallbackToJobID(t *testing.T) {
	setupFastPolling(t)

	repo := newTestGitRepo(t)
	repo.CommitFile("file.txt", "content", "initial commit")

	// "42" is not a valid git ref, so the wait command should fall back to
	// treating it as a numeric job ID and query by id (not git_ref).
	var lastJobsQuery string
	mock := mockConfig{
		Jobs: []storage.ReviewJob{{ID: 42, GitRef: "abc", Agent: "test", Status: "done"}},
		Review: &storage.Review{
			ID: 1, JobID: 42, Agent: "test", Output: "No issues found.",
		},
		OnJobsQuery: func(r *http.Request) {
			lastJobsQuery = r.URL.RawQuery
		},
	}

	daemonFromHandler(t, newWaitMockHandler(mock))
	chdir(t, repo.Dir)

	_, err := runWait(t, "42", "--quiet")
	require.NoError(t, err)
	// The final poll should query by id=42, not git_ref=42
	assert.Contains(t, lastJobsQuery, "id=42")
}

func TestWaitMultipleJobIDs(t *testing.T) {
	setupFastPolling(t)

	// Mock that serves two jobs, both passing
	mock := mockConfig{
		Jobs: []storage.ReviewJob{
			{ID: 10, Agent: "test", Status: "done"},
			{ID: 20, Agent: "test", Status: "done"},
		},
		Review: &storage.Review{ID: 1, JobID: 10, Agent: "test", Output: "No issues found."},
	}
	newWaitEnv(t, newWaitMockHandler(mock))

	_, err := runWait(t, "--job", "10", "20")
	require.NoError(t, err)

}

func TestWaitMultipleJobIDsOneFails(t *testing.T) {
	setupFastPolling(t)

	// One job passes, one fails (has issues)
	handler := newMultiJobMockHandler(map[int64]mockJobResult{
		10: {
			job:    storage.ReviewJob{ID: 10, Agent: "test", Status: "done"},
			review: &storage.Review{ID: 1, JobID: 10, Agent: "test", Output: "No issues found."},
		},
		20: {
			job:    storage.ReviewJob{ID: 20, Agent: "test", Status: "done"},
			review: &storage.Review{ID: 2, JobID: 20, Agent: "test", Output: "Found 1 issue:\n1. Bug"},
		},
	})
	newWaitEnv(t, handler)

	_, err := runWait(t, "--job", "10", "20")
	requireExitCode(t, err, 1)
}

func TestWaitMultipleJobIDsOneFailsReportsOutput(t *testing.T) {
	setupFastPolling(t)

	// One job passes, one has issues. The failure message should
	// appear in stdout (not suppressed).
	handler := newMultiJobMockHandler(map[int64]mockJobResult{
		10: {
			job:    storage.ReviewJob{ID: 10, Agent: "test", Status: "done"},
			review: &storage.Review{ID: 1, JobID: 10, Agent: "test", Output: "No issues found."},
		},
		20: {
			job:    storage.ReviewJob{ID: 20, Agent: "test", Status: "done"},
			review: &storage.Review{ID: 2, JobID: 20, Agent: "test", Output: "Found 1 issue:\n1. Bug"},
		},
	})
	newWaitEnv(t, handler)

	stdout, err := runWait(t, "--job", "10", "20")
	requireExitCode(t, err, 1)
	assert.Contains(t, stdout, "Job 20: review has issues")
	// Passing job should not appear in output.
	assert.NotContains(t, stdout, "Job 10")
}

func TestWaitMultipleJobIDsNotFoundReportsOutput(t *testing.T) {
	setupFastPolling(t)

	// Job 10 passes, job 99 does not exist. The "no job found"
	// message should appear in stdout.
	handler := newMultiJobMockHandler(map[int64]mockJobResult{
		10: {
			job:    storage.ReviewJob{ID: 10, Agent: "test", Status: "done"},
			review: &storage.Review{ID: 1, JobID: 10, Agent: "test", Output: "No issues found."},
		},
		// 99 is absent — will produce ErrJobNotFound
	})
	newWaitEnv(t, handler)

	stdout, err := runWait(t, "--job", "10", "99")
	requireExitCode(t, err, 1)
	assert.Contains(t, stdout, "Job 99: no job found")
}

func TestWaitMultipleGenericErrors(t *testing.T) {
	setupFastPolling(t)

	cases := []struct {
		name    string
		jobs    map[int64]mockJobResult
		args    []string
		wantMsg string // substring expected in stdout for the failing job
	}{
		{
			name: "failed job reports error message",
			jobs: map[int64]mockJobResult{
				10: {
					job:    storage.ReviewJob{ID: 10, Agent: "test", Status: "done"},
					review: &storage.Review{ID: 1, JobID: 10, Agent: "test", Output: "No issues found."},
				},
				20: {
					job: storage.ReviewJob{ID: 20, Agent: "test", Status: "failed", Error: "agent crashed"},
				},
			},
			args:    []string{"--job", "10", "20"},
			wantMsg: "Job 20: review failed: agent crashed",
		},
		{
			name: "canceled job reports canceled",
			jobs: map[int64]mockJobResult{
				10: {
					job:    storage.ReviewJob{ID: 10, Agent: "test", Status: "done"},
					review: &storage.Review{ID: 1, JobID: 10, Agent: "test", Output: "No issues found."},
				},
				30: {
					job: storage.ReviewJob{ID: 30, Agent: "test", Status: "canceled"},
				},
			},
			args:    []string{"--job", "10", "30"},
			wantMsg: "Job 30: review was canceled",
		},
		{
			name: "review fetch error reports message",
			jobs: map[int64]mockJobResult{
				10: {
					job:    storage.ReviewJob{ID: 10, Agent: "test", Status: "done"},
					review: &storage.Review{ID: 1, JobID: 10, Agent: "test", Output: "No issues found."},
				},
				40: {
					job: storage.ReviewJob{ID: 40, Agent: "test", Status: "done"},
					// review is nil → /api/review returns 404
				},
			},
			args:    []string{"--job", "10", "40"},
			wantMsg: "Job 40: no review found",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			handler := newMultiJobMockHandler(tc.jobs)
			newWaitEnv(t, handler)

			stdout, err := runWait(t, tc.args...)
			requireExitCode(t, err, 1)
			assert.Contains(t, stdout, tc.wantMsg)
		})
	}
}

func TestWaitMultipleValidationBeforeDaemon(t *testing.T) {
	// Validation errors for --job should surface before contacting
	// the daemon. This test does NOT start a mock daemon.
	repo := newTestGitRepo(t)
	repo.CommitFile("file.txt", "content", "initial commit")
	chdir(t, repo.Dir)

	_, err := runWait(t, "--job", "10", "abc")
	assertErrorContains(t, err, "invalid job ID")

	_, err = runWait(t, "--job", "10", "0")
	assertErrorContains(t, err, "invalid job ID")
}

func TestWaitMultipleJobIDsValidation(t *testing.T) {
	// --sha is incompatible with multiple args
	repo := newTestGitRepo(t)
	repo.CommitFile("file.txt", "content", "initial commit")
	chdir(t, repo.Dir)

	_, err := runWait(t, "--sha", "HEAD", "10", "20")
	assertErrorContains(t, err, "cannot use both")
}

// mockJobResult holds per-job mock data for multi-job tests.
type mockJobResult struct {
	job    storage.ReviewJob
	review *storage.Review
}

// newMultiJobMockHandler creates a handler that serves different jobs by ID.
func newMultiJobMockHandler(jobs map[int64]mockJobResult) http.HandlerFunc {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/jobs", func(w http.ResponseWriter, r *http.Request) {
		idStr := r.URL.Query().Get("id")
		if idStr != "" {
			id, _ := strconv.ParseInt(idStr, 10, 64)
			if result, ok := jobs[id]; ok {
				w.WriteHeader(http.StatusOK)
				json.NewEncoder(w).Encode(map[string]any{
					"jobs":     []storage.ReviewJob{result.job},
					"has_more": false,
				})
				return
			}
		}
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]any{
			"jobs":     []storage.ReviewJob{},
			"has_more": false,
		})
	})
	mux.HandleFunc("/api/review", func(w http.ResponseWriter, r *http.Request) {
		jobIDStr := r.URL.Query().Get("job_id")
		if jobIDStr != "" {
			id, _ := strconv.ParseInt(jobIDStr, 10, 64)
			if result, ok := jobs[id]; ok && result.review != nil {
				w.WriteHeader(http.StatusOK)
				json.NewEncoder(w).Encode(result.review)
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
	})
	return mux.ServeHTTP
}

func TestWaitReviewFetchErrorExposesCauseInQuietMode(t *testing.T) {
	setupFastPolling(t)

	// Job completes but /api/review returns 404. In --quiet mode the
	// command exits with code 1, the usage block is suppressed, and
	// cobra prints the underlying cause via err.Error() so the user
	// (or the hook log) can still see what failed.
	mock := mockConfig{
		Jobs: []storage.ReviewJob{{ID: 1, Agent: "test", Status: "done"}},
		// Review is nil, so 404
	}
	newWaitEnv(t, newWaitMockHandler(mock))

	_, err := runWait(t, "--sha", "HEAD", "--quiet")
	requireExitCode(t, err, 1)
	assert.Contains(t, err.Error(), "review", "underlying cause should remain accessible")
}

func TestWaitLookupNon200Response(t *testing.T) {
	setupFastPolling(t)
	newWaitEnv(t, newWaitMockHandler(mockConfig{
		JobsErr: http.StatusInternalServerError,
	}))

	_, err := runWait(t, "--sha", "HEAD")
	assertErrorContains(t, err, "500 Internal Server Error")
}

// setupRepoWithWorktree creates a repo, a worktree, and commits in both.
// It returns the repo path, worktree path, main SHA, and worktree SHA.
func setupRepoWithWorktree(t *testing.T) (repoDir, wtDir, mainSHA, wtSHA string) {
	t.Helper()
	// Create main repo with initial commit
	repo := newTestGitRepo(t)
	mainSHA = repo.CommitFile("file.txt", "content", "initial on main")

	// Create a worktree on a new branch with a different commit
	wtDir = t.TempDir()
	os.Remove(wtDir) // git worktree add needs to create it
	repo.Run("worktree", "add", "-b", "wt-branch", wtDir)

	// Make a new commit in the worktree so HEAD differs from main
	wtRepo := &TestGitRepo{Dir: wtDir, t: t}
	wtSHA = wtRepo.CommitFile("wt-file.txt", "worktree content", "commit in worktree")

	assert.NotEqual(t, mainSHA, wtSHA)
	return repo.Dir, wtDir, mainSHA, wtSHA
}

func TestWaitWorktreeResolvesRefFromWorktreeAndRepoFromMain(t *testing.T) {
	setupFastPolling(t)

	repoDir, worktreeDir, mainSHA, wtSHA := setupRepoWithWorktree(t)

	var lookupQuery string
	mock := mockConfig{
		Jobs: []storage.ReviewJob{{ID: 1, GitRef: wtSHA, Agent: "test", Status: "done"}},
		Review: &storage.Review{
			ID: 1, JobID: 1, Agent: "test", Output: "No issues found.",
		},
		OnJobsQuery: func(r *http.Request) {
			// Capture only the lookup query (has git_ref), not the poll query (has id)
			if r.URL.Query().Get("git_ref") != "" {
				lookupQuery = r.URL.RawQuery
			}
		},
	}

	daemonFromHandler(t, newWaitMockHandler(mock))

	chdir(t, worktreeDir)

	_, err := runWait(t, "--sha", "HEAD", "--quiet")
	require.NoError(t, err)

	assert.NotEmpty(t, lookupQuery)

	// git_ref should be the worktree's HEAD (wtSHA), not the main repo's HEAD
	assert.Contains(t, lookupQuery, "git_ref="+url.QueryEscape(wtSHA))
	assert.NotContains(t, lookupQuery, "git_ref="+url.QueryEscape(mainSHA))

	// repo should be the main repo path, not the worktree path
	assert.NotContains(t, lookupQuery, url.QueryEscape(worktreeDir))
	assert.Contains(t, lookupQuery, url.QueryEscape(repoDir))
}
