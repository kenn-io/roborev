package daemon

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/storage"
)

func createTestGitRepo(t *testing.T) (string, func(args ...string)) {
	t.Helper()
	tmpDir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(tmpDir); err == nil {
		tmpDir = resolved
	}
	repoDir := filepath.Join(tmpDir, "repo")
	require.NoError(t, os.MkdirAll(repoDir, 0o755))
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v\n%s", args, out)
	}
	run("init")
	run("config", "user.email", "test@test.com")
	run("config", "user.name", "Test")
	testFile := filepath.Join(repoDir, "test.txt")
	require.NoError(t, os.WriteFile(testFile, []byte("test"), 0o644))
	run("add", ".")
	run("commit", "-m", "initial")
	return repoDir, run
}

func mockAPI(t *testing.T, handler http.HandlerFunc) *HTTPClient {
	t.Helper()
	s := httptest.NewServer(handler)
	t.Cleanup(s.Close)
	ep, err := ParseEndpoint(s.URL)
	require.NoError(t, err, "ParseEndpoint")
	return NewHTTPClient(ep)
}

func assertRequest(t *testing.T, r *http.Request, method, path string) {
	t.Helper()
	assert.Equal(t, method, r.Method)
	assert.Equal(t, path, r.URL.Path)
}

func assertQuery(t *testing.T, r *http.Request, key, expected string) {
	t.Helper()
	assert.Equal(t, expected, r.URL.Query().Get(key), "query param %s", key)
}

func decodeJSON(t *testing.T, r *http.Request, v any) {
	t.Helper()
	require.NoError(t, json.NewDecoder(r.Body).Decode(v), "decode request")
}

func writeTestJSON(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	require.NoError(t, json.NewEncoder(w).Encode(v), "encode response")
}

func TestHTTPClientAddComment(t *testing.T) {
	assert := assert.New(t)

	var received struct {
		JobID     int    `json:"job_id"`
		Commenter string `json:"commenter"`
		Comment   string `json:"comment"`
	}

	client := mockAPI(t, func(w http.ResponseWriter, r *http.Request) {
		assertRequest(t, r, http.MethodPost, "/api/comment")
		decodeJSON(t, r, &received)
		w.WriteHeader(http.StatusCreated)
	})
	require.NoError(t, client.AddComment(42, "test-agent", "Fixed the issue"), "AddComment failed")
	assert.Equal(42, received.JobID)
	assert.Equal("test-agent", received.Commenter)
	assert.Equal("Fixed the issue", received.Comment)
}

func TestHTTPClientGetAllCommentsForJobSkipsLegacyLookupForDirtyJob(t *testing.T) {
	assert := assert.New(t)
	var sawLegacy bool
	client := mockAPI(t, func(w http.ResponseWriter, r *http.Request) {
		assertRequest(t, r, http.MethodGet, "/api/comments")
		switch r.URL.RawQuery {
		case "job_id=11":
			writeTestJSON(t, w, struct {
				Responses []storage.Response `json:"responses"`
			}{
				Responses: []storage.Response{{Responder: "alice", Response: "job comment"}},
			})
		case "commit_id=42":
			sawLegacy = true
			writeTestJSON(t, w, struct {
				Responses []storage.Response `json:"responses"`
			}{
				Responses: []storage.Response{{Responder: "bob", Response: "base commit comment"}},
			})
		default:
			http.Error(w, "unexpected query: "+r.URL.RawQuery, http.StatusBadRequest)
		}
	})

	got, err := client.GetAllCommentsForJob(11, 42, "dirty")
	require.NoError(t, err)

	require.Len(t, got, 1)
	assert.Equal("alice", got[0].Responder)
	assert.False(sawLegacy, "dirty jobs must not fetch legacy comments for the base commit")
}

func TestHTTPClientMarkReviewClosed(t *testing.T) {
	assert := assert.New(t)

	var received struct {
		JobID  int  `json:"job_id"`
		Closed bool `json:"closed"`
	}

	client := mockAPI(t, func(w http.ResponseWriter, r *http.Request) {
		assertRequest(t, r, http.MethodPost, "/api/review/close")
		decodeJSON(t, r, &received)
		w.WriteHeader(http.StatusOK)
	})
	require.NoError(t, client.MarkReviewClosed(99), "MarkReviewClosed failed")
	assert.Equal(99, received.JobID)
	assert.True(received.Closed)
}

func TestHTTPClientWaitForReviewUsesJobID(t *testing.T) {
	assert := assert.New(t)

	var reviewCalls atomic.Int32

	client := mockAPI(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/jobs" && r.Method == http.MethodGet:
			writeTestJSON(t, w, map[string]any{
				"jobs": []storage.ReviewJob{{ID: 1, Status: storage.JobStatusDone, GitRef: "commit1"}},
			})
			return
		case r.URL.Path == "/api/review" && r.Method == http.MethodGet:
			assertQuery(t, r, "job_id", "1")
			assertQuery(t, r, "sha", "")
			if reviewCalls.Add(1) == 1 {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			writeTestJSON(t, w, storage.Review{ID: 1, JobID: 1, Output: "Review complete"})
			return
		default:
			assert.Empty(t, fmt.Sprintf("%s %s", r.Method, r.URL.Path), "unexpected request")
			w.WriteHeader(http.StatusNotFound)
		}
	})
	client.SetPollInterval(1 * time.Millisecond)
	review, err := client.WaitForReview(1)
	require.NoError(t, err, "WaitForReview failed")
	assert.Equal("Review complete", review.Output)
	assert.GreaterOrEqual(reviewCalls.Load(), int32(2), "expected review to be retried after 404")
}

func TestFindJobForCommit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	mainRepo, runGit := createTestGitRepo(t)
	worktreeDir := filepath.Join(filepath.Dir(mainRepo), "worktree")
	runGit("worktree", "add", worktreeDir, "HEAD")

	sha := "abc123"

	tests := []struct {
		name           string
		queryRepo      string
		setupMock      func(t *testing.T) func(t *testing.T, w http.ResponseWriter, r *http.Request)
		expectedJobID  int64
		expectNotFound bool
	}{
		{
			name:      "worktree path normalized to main repo",
			queryRepo: worktreeDir,
			setupMock: func(t *testing.T) func(t *testing.T, w http.ResponseWriter, r *http.Request) {
				return func(t *testing.T, w http.ResponseWriter, r *http.Request) {
					assertRequest(t, r, http.MethodGet, "/api/jobs")

					repo := r.URL.Query().Get("repo")
					assert.NotEmpty(t, repo, "expected server to receive a repo path")
					assert.NotEqual(t, worktreeDir, repo, "expected server to NOT receive worktree path")

					normalizedReceived := repo
					if resolved, err := filepath.EvalSymlinks(repo); err == nil {
						normalizedReceived = resolved
					}

					if normalizedReceived == mainRepo {
						writeTestJSON(t, w, map[string]any{
							"jobs": []storage.ReviewJob{
								{ID: 1, GitRef: sha, RepoPath: mainRepo, Status: storage.JobStatusDone},
							},
						})
						return
					}
					writeTestJSON(t, w, map[string]any{"jobs": []storage.ReviewJob{}})
				}
			},
			expectedJobID: 1,
		},
		{
			name:      "main repo path works directly",
			queryRepo: mainRepo,
			setupMock: func(t *testing.T) func(t *testing.T, w http.ResponseWriter, r *http.Request) {
				return func(t *testing.T, w http.ResponseWriter, r *http.Request) {
					assertRequest(t, r, http.MethodGet, "/api/jobs")

					repo := r.URL.Query().Get("repo")
					normalizedReceived := repo
					if resolved, err := filepath.EvalSymlinks(repo); err == nil {
						normalizedReceived = resolved
					}

					if normalizedReceived == mainRepo {
						writeTestJSON(t, w, map[string]any{
							"jobs": []storage.ReviewJob{
								{ID: 1, GitRef: sha, RepoPath: mainRepo, Status: storage.JobStatusDone},
							},
						})
						return
					}
					writeTestJSON(t, w, map[string]any{"jobs": []storage.ReviewJob{}})
				}
			},
			expectedJobID: 1,
		},
		{
			name:      "fallback when primary query returns no results",
			queryRepo: mainRepo,
			setupMock: func(t *testing.T) func(t *testing.T, w http.ResponseWriter, r *http.Request) {
				var calls int
				t.Cleanup(func() {
					assert.Equal(t, 2, calls, "expected 2 calls")
				})
				return func(t *testing.T, w http.ResponseWriter, r *http.Request) {
					assertRequest(t, r, http.MethodGet, "/api/jobs")
					calls++

					repo := r.URL.Query().Get("repo")
					if calls == 1 {
						assert.NotEmpty(t, repo, "expected first call to have non-empty repo")

						writeTestJSON(t, w, map[string]any{"jobs": []storage.ReviewJob{}})
						return
					}

					if calls == 2 {
						assert.Empty(t, repo, "expected second call to have empty repo")

						writeTestJSON(t, w, map[string]any{
							"jobs": []storage.ReviewJob{
								{ID: 1, GitRef: sha, RepoPath: mainRepo, Status: storage.JobStatusDone},
							},
						})
						return
					}
					assert.Equal(t, 0, calls, "unexpected call")
				}
			},
			expectedJobID: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			handler := tc.setupMock(t)
			client := mockAPI(t, func(w http.ResponseWriter, r *http.Request) {
				handler(t, w, r)
			})
			job, err := client.FindJobForCommit(t.Context(), tc.queryRepo, sha)
			require.NoError(err, "FindJobForCommit failed")

			if tc.expectNotFound {
				assert.Nil(job)
			} else {
				require.NotNil(job, "expected to find job")
				assert.Equal(tc.expectedJobID, job.ID)
			}
		})
	}
}

func TestFindPendingJobForRef(t *testing.T) {
	tests := []struct {
		name                 string
		mockResponses        map[string][]storage.ReviewJob
		expectJobID          int64
		expectStatus         storage.JobStatus
		expectNotFound       bool
		expectedRequestOrder []string
	}{
		{
			name: "returns running job",
			mockResponses: map[string][]storage.ReviewJob{
				"running": {{ID: 1, GitRef: "abc123..def456", Status: storage.JobStatusRunning}},
			},
			expectJobID:          1,
			expectStatus:         storage.JobStatusRunning,
			expectedRequestOrder: []string{"queued", "running"},
		},
		{
			name:                 "returns nil when no pending jobs",
			mockResponses:        map[string][]storage.ReviewJob{},
			expectNotFound:       true,
			expectedRequestOrder: []string{"queued", "running"},
		},
		{
			name: "returns queued job before checking running",
			mockResponses: map[string][]storage.ReviewJob{
				"queued": {{ID: 1, GitRef: "abc123..def456", Status: storage.JobStatusQueued}},
			},
			expectJobID:          1,
			expectStatus:         storage.JobStatusQueued,
			expectedRequestOrder: []string{"queued"},
		},
		{
			name: "queries both queued and running when needed",
			mockResponses: map[string][]storage.ReviewJob{
				"running": {{ID: 2, GitRef: "abc123..def456", Status: storage.JobStatusRunning}},
			},
			expectJobID:          2,
			expectStatus:         storage.JobStatusRunning,
			expectedRequestOrder: []string{"queued", "running"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			var requestedStatuses []string
			var mu sync.Mutex

			client := mockAPI(t, func(w http.ResponseWriter, r *http.Request) {
				assertRequest(t, r, http.MethodGet, "/api/jobs")

				assertQuery(t, r, "git_ref", "abc123..def456")

				expectedRepo, err := filepath.Abs("/test/repo")
				assert.NoError(err, "filepath.Abs failed: %v", err)

				assertQuery(t, r, "repo", expectedRepo)

				status := r.URL.Query().Get("status")
				mu.Lock()
				requestedStatuses = append(requestedStatuses, status)
				mu.Unlock()

				jobs, ok := tc.mockResponses[status]
				if !ok {
					jobs = []storage.ReviewJob{}
				}
				writeTestJSON(t, w, map[string]any{"jobs": jobs})
			})

			job, err := client.FindPendingJobForRef(t.Context(), "/test/repo", "abc123..def456")
			require.NoError(err, "FindPendingJobForRef failed")

			if tc.expectNotFound {
				assert.Nil(job)
			} else {
				require.NotNil(job, "expected to find job")
				assert.Equal(tc.expectJobID, job.ID)
				assert.Equal(tc.expectStatus, job.Status)

			}

			if assert.Len(requestedStatuses, len(tc.expectedRequestOrder), "request count") {
				for i, want := range tc.expectedRequestOrder {
					assert.Equal(want, requestedStatuses[i], "request %d status", i)
				}
			}
		})
	}
}
