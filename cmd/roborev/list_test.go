//go:build integration

package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
)

type repoSetupResult struct {
	workingDir string
	repo       *TestGitRepo
	extraArgs  []string
}

type listTestCase struct {
	name          string
	args          []string
	handler       http.HandlerFunc
	repoSetup     func(t *testing.T) repoSetupResult
	check         func(t *testing.T, output string, query string, repo *TestGitRepo, wd string)
	wantOutput    []string
	notWantOutput []string
	wantError     string
	wantQuery     []string
	notWantQuery  []string
}

func assertListContainsAll(t *testing.T, name, subject string, wants []string) {
	t.Helper()
	for _, want := range wants {
		assert.Contains(t, subject, want, "expected %s to contain %q", name, want)
	}
}

func assertListNotContainsAny(t *testing.T, name, subject string, notWants []string) {
	t.Helper()
	for _, notWant := range notWants {
		assert.NotContains(t, subject, notWant, "expected %s NOT to contain %q", name, notWant)
	}
}

func TestListCommand(t *testing.T) {
	now := time.Now()
	started := now.Add(-10 * time.Second)
	finished := now.Add(-5 * time.Second)
	testJobs := []storage.ReviewJob{
		{
			ID:         1,
			GitRef:     "abc1234567890",
			RepoName:   "myrepo",
			Agent:      "test",
			Status:     storage.JobStatusDone,
			StartedAt:  &started,
			FinishedAt: &finished,
		},
		{
			ID:       2,
			GitRef:   "def4567890123",
			RepoName: "myrepo",
			Agent:    "codex",
			Status:   storage.JobStatusQueued,
		},
	}

	// Helper to create a handler that returns a specific list of jobs
	jobsHandler := func(jobs []storage.ReviewJob, hasMore bool) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/jobs" {
				json.NewEncoder(w).Encode(map[string]any{
					"jobs":     jobs,
					"has_more": hasMore,
				})
			}
		}
	}

	// Helper to create a handler that returns an error
	errorHandler := func(code int, body string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/jobs" {
				w.WriteHeader(code)
				w.Write([]byte(body))
			}
		}
	}

	tests := []listTestCase{
		{
			name:       "tabular output shows jobs",
			args:       []string{},
			handler:    jobsHandler(testJobs, false),
			wantOutput: []string{"abc1234", "myrepo", "test", "done"},
		},
		{
			name:    "json output passes through raw response",
			args:    []string{"--json"},
			handler: jobsHandler(testJobs, true),
			check: func(t *testing.T, output string, query string, repo *TestGitRepo, wd string) {
				var parsed []storage.ReviewJob
				require.NoError(t, json.Unmarshal([]byte(output), &parsed), "json output not valid JSON\noutput: %s", output)
				assert.Len(t, parsed, 2)
			},
		},
		{
			name:       "has_more shows hint in tabular mode",
			args:       []string{},
			handler:    jobsHandler(testJobs, true),
			wantOutput: []string{"more results available"},
		},
		{
			name:       "empty results shows message",
			args:       []string{},
			handler:    jobsHandler([]storage.ReviewJob{}, false),
			wantOutput: []string{"No jobs found"},
		},
		{
			name:      "non-2xx response returns error",
			args:      []string{},
			handler:   errorHandler(http.StatusInternalServerError, "database locked"),
			wantError: "500",
		},
		{
			name:      "non-2xx response returns error with --json",
			args:      []string{"--json"},
			handler:   errorHandler(http.StatusBadRequest, "invalid status filter"),
			wantError: "400",
		},
		{
			name:      "query params pass through correctly",
			args:      []string{"--status", "done", "--limit", "10"},
			handler:   jobsHandler([]storage.ReviewJob{}, false),
			wantQuery: []string{"status=done", "limit=10"},
		},
		{
			name:    "explicit --repo to non-git path sends no branch",
			args:    []string{"--repo", "/some/other/repo"},
			handler: jobsHandler([]storage.ReviewJob{}, false),
			wantQuery: []string{
				"repo=%2Fsome%2Fother%2Frepo",
			},
			notWantQuery: []string{"branch="},
		},
		{
			name: "explicit --repo to cwd repo still auto-resolves branch",
			repoSetup: func(t *testing.T) repoSetupResult {
				repo := newTestGitRepo(t)
				repo.CommitFile("file.txt", "content", "initial")
				// We want to pass repo.Dir as --repo arg
				return repoSetupResult{workingDir: repo.Dir, repo: repo, extraArgs: []string{"--repo", repo.Dir}}
			},
			handler:   jobsHandler([]storage.ReviewJob{}, false),
			wantQuery: []string{"branch="},
		},
		{
			name: "worktree sends main repo path as repo param",
			repoSetup: func(t *testing.T) repoSetupResult {
				repo := newTestGitRepo(t)
				repo.CommitFile("file.txt", "content", "initial")
				worktreeDir := t.TempDir()
				os.Remove(worktreeDir)
				repo.Run("worktree", "add", "-b", "wt-branch", worktreeDir)
				return repoSetupResult{workingDir: worktreeDir, repo: repo, extraArgs: nil}
			},
			handler: jobsHandler([]storage.ReviewJob{}, false),
			check: func(t *testing.T, output string, query string, repo *TestGitRepo, wd string) {
				assert.Contains(t, query, url.QueryEscape(repo.Dir), "expected main repo path in query")
				assert.Contains(t, query, "branch=wt-branch", "expected branch in query")
				assert.NotContains(t, query, url.QueryEscape(wd), "expected worktree path NOT in query")
			},
		},
		{
			name:      "--open sends closed=false query param",
			args:      []string{"--open"},
			handler:   jobsHandler([]storage.ReviewJob{}, false),
			wantQuery: []string{"closed=false"},
		},
		{
			name:      "--closed sends closed=true query param",
			args:      []string{"--closed"},
			handler:   jobsHandler([]storage.ReviewJob{}, false),
			wantQuery: []string{"closed=true"},
		},
		{
			name:      "--unaddressed alias sends closed=false query param",
			args:      []string{"--unaddressed"},
			handler:   jobsHandler([]storage.ReviewJob{}, false),
			wantQuery: []string{"closed=false"},
		},
		{
			name:      "--closed and --open conflict",
			args:      []string{"--closed", "--open"},
			handler:   jobsHandler([]storage.ReviewJob{}, false),
			wantError: "if any flags in the group",
		},
		{
			name:      "--closed and --unaddressed conflict",
			args:      []string{"--closed", "--unaddressed"},
			handler:   jobsHandler([]storage.ReviewJob{}, false),
			wantError: "if any flags in the group",
		},
		{
			name: "explicit --repo with worktree path normalizes to main repo",
			repoSetup: func(t *testing.T) repoSetupResult {
				repo := newTestGitRepo(t)
				repo.CommitFile("file.txt", "content", "initial")
				worktreeDir := t.TempDir()
				os.Remove(worktreeDir)
				repo.Run("worktree", "add", "-b", "wt-branch", worktreeDir)
				return repoSetupResult{workingDir: worktreeDir, repo: repo, extraArgs: []string{"--repo", worktreeDir}}
			},
			handler: jobsHandler([]storage.ReviewJob{}, false),
			check: func(t *testing.T, output string, query string, repo *TestGitRepo, wd string) {
				assert.Contains(t, query, url.QueryEscape(repo.Dir), "expected main repo path in query")
				assert.NotContains(t, query, url.QueryEscape(wd), "expected worktree path NOT in query")
			},
		},
		{
			name: "workspace mode suppresses auto-detected branch",
			repoSetup: func(t *testing.T) repoSetupResult {
				// Create a non-git parent dir (workspace mode)
				parent := t.TempDir()
				return repoSetupResult{
					workingDir: parent,
					repo:       nil,
					extraArgs:  nil,
				}
			},
			handler:      jobsHandler([]storage.ReviewJob{}, false),
			wantQuery:    []string{"repo_prefix="},
			notWantQuery: []string{"branch="},
		},
		{
			name: "workspace mode honors explicit --branch",
			repoSetup: func(t *testing.T) repoSetupResult {
				parent := t.TempDir()
				return repoSetupResult{
					workingDir: parent,
					repo:       nil,
					extraArgs:  []string{"--branch", "main"},
				}
			},
			handler:   jobsHandler([]storage.ReviewJob{}, false),
			wantQuery: []string{"repo_prefix=", "branch=main", "branch_include_empty=true"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			runListTest(t, tc)
		})
	}
}

func runListTest(t *testing.T, tc listTestCase) {
	// Setup mock daemon to capture query and handle request
	var capturedQuery string
	wrapperHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/jobs" {
			capturedQuery = r.URL.RawQuery
		}
		if tc.handler != nil {
			tc.handler(w, r)
		}
	})
	daemonFromHandler(t, wrapperHandler)

	// Setup repo and cwd
	var wd string
	var repo *TestGitRepo
	var extraArgs []string
	if tc.repoSetup != nil {
		res := tc.repoSetup(t)
		wd, repo, extraArgs = res.workingDir, res.repo, res.extraArgs
	} else {
		repo = newTestGitRepo(t)
		repo.CommitFile("file.txt", "content", "initial")
		wd = repo.Dir
	}
	chdir(t, wd)

	// Execute command
	var cmdErr error
	output := captureStdout(t, func() {
		cmd := listCmd()
		args := append(tc.args, extraArgs...)
		cmd.SetArgs(args)
		cmdErr = cmd.Execute()
	})

	// Verify error
	if tc.wantError != "" {
		require.Error(t, cmdErr, "expected error but got none")
		assert.Contains(t, cmdErr.Error(), tc.wantError)
	} else {
		require.NoError(t, cmdErr)
	}

	// Verify string output
	assertListContainsAll(t, "output", output, tc.wantOutput)
	assertListNotContainsAny(t, "output", output, tc.notWantOutput)

	// Verify query parameters
	assertListContainsAll(t, "query", capturedQuery, tc.wantQuery)
	assertListNotContainsAny(t, "query", capturedQuery, tc.notWantQuery)

	// Custom checks
	if tc.check != nil {
		tc.check(t, output, capturedQuery, repo, wd)
	}
}
