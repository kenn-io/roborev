package daemon

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

type commandTestAgent struct {
	name    string
	command string
}

func (a *commandTestAgent) Name() string { return a.name }

func (a *commandTestAgent) Review(ctx context.Context, repoPath, commitSHA, prompt string, output io.Writer) (string, error) {
	return "No issues found.", nil
}

func (a *commandTestAgent) WithReasoning(level agent.ReasoningLevel) agent.Agent {
	return a
}

func (a *commandTestAgent) WithAgentic(agentic bool) agent.Agent { return a }

func (a *commandTestAgent) WithModel(model string) agent.Agent { return a }

func (a *commandTestAgent) CommandLine() string { return a.command }

func (a *commandTestAgent) CommandName() string { return a.command }

func TestHandleStatus(t *testing.T) {
	server, db, _ := newTestServer(t)

	t.Run("returns status with version", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
		w := httptest.NewRecorder()

		server.httpServer.Handler.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			require.Condition(t, func() bool {
				return false
			}, "Expected status 200, got %d: %s", w.Code, w.Body.String())
		}

		var status storage.DaemonStatus
		testutil.DecodeJSON(t, w, &status)

		// Version should be set (non-empty)
		if status.Version == "" {
			assert.Condition(t, func() bool {
				return false
			}, "Expected Version to be set in status response")
		}
	})

	t.Run("wrong method fails", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/status", nil)
		w := httptest.NewRecorder()

		server.httpServer.Handler.ServeHTTP(w, req)

		if w.Code != http.StatusMethodNotAllowed {
			assert.Condition(t, func() bool {
				return false
			}, "Expected status 405 for POST, got %d", w.Code)
		}
	})

	t.Run("returns max_workers from pool not config", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
		w := httptest.NewRecorder()

		server.httpServer.Handler.ServeHTTP(w, req)

		var status storage.DaemonStatus
		testutil.DecodeJSON(t, w, &status)

		// MaxWorkers should match the pool size (config default), not a potentially reloaded config value
		expectedWorkers := config.DefaultConfig().MaxWorkers
		if status.MaxWorkers != expectedWorkers {
			assert.Condition(t, func() bool {
				return false
			}, "Expected MaxWorkers %d from pool, got %d", expectedWorkers, status.MaxWorkers)
		}
	})

	t.Run("includes queue paused state", func(t *testing.T) {
		require.NoError(t, db.SetQueuePaused(true))
		req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
		w := httptest.NewRecorder()

		server.httpServer.Handler.ServeHTTP(w, req)

		var status storage.DaemonStatus
		testutil.DecodeJSON(t, w, &status)
		assert.True(t, status.QueuePaused)
		require.NoError(t, db.SetQueuePaused(false))
	})

	t.Run("config_reloaded_at empty initially", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
		w := httptest.NewRecorder()

		server.httpServer.Handler.ServeHTTP(w, req)

		var status storage.DaemonStatus
		testutil.DecodeJSON(t, w, &status)

		// ConfigReloadedAt should be empty when no reload has occurred
		if status.ConfigReloadedAt != "" {
			assert.Condition(t, func() bool {
				return false
			}, "Expected ConfigReloadedAt to be empty initially, got %q", status.ConfigReloadedAt)
		}
	})
}

func TestHandleQueuePause(t *testing.T) {
	server, db, _ := newTestServer(t)

	req := httptest.NewRequest(http.MethodPost, "/api/queue/pause", nil)
	w := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	paused, err := db.IsQueuePaused()
	require.NoError(t, err)
	assert.True(t, paused)

	req = httptest.NewRequest(http.MethodPost, "/api/queue/unpause", nil)
	w = httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	paused, err = db.IsQueuePaused()
	require.NoError(t, err)
	assert.False(t, paused)
}

func TestHandlePing(t *testing.T) {
	server, _, _ := newTestServer(t)

	t.Run("returns daemon identity", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/ping", nil)
		w := httptest.NewRecorder()

		server.httpServer.Handler.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			require.Condition(t, func() bool {
				return false
			}, "Expected status 200, got %d: %s", w.Code, w.Body.String())
		}

		var ping PingInfo
		testutil.DecodeJSON(t, w, &ping)
		if ping.Service != daemonServiceName {
			require.Condition(t, func() bool {
				return false
			}, "Expected service %q, got %q", daemonServiceName, ping.Service)
		}
		if ping.Version == "" {
			require.Condition(t, func() bool {
				return false
			}, "Expected ping version to be set")
		}
		if ping.PID == 0 {
			require.Condition(t, func() bool {
				return false
			}, "Expected ping PID to be set")
		}
	})

	t.Run("wrong method fails", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/ping", nil)
		w := httptest.NewRecorder()

		server.httpServer.Handler.ServeHTTP(w, req)

		if w.Code != http.StatusMethodNotAllowed {
			require.Condition(t, func() bool {
				return false
			}, "Expected status 405, got %d", w.Code)
		}
	})
}

func TestHandleCancelJob(t *testing.T) {
	tests := []struct {
		name       string
		setup      func(t *testing.T, server *Server, db *storage.DB, tmpDir string) int64 // returns job_id or 0
		request    func(t *testing.T, jobID int64) *http.Request                           // builds the request
		wantStatus int
		verify     func(t *testing.T, db *storage.DB, jobID int64) // optional post-cancel check
	}{
		{
			name: "cancel queued job",
			setup: func(t *testing.T, server *Server, db *storage.DB, tmpDir string) int64 {
				job := createTestJob(t, db, tmpDir, "cancelqueued", "test")
				return job.ID
			},
			request: func(t *testing.T, jobID int64) *http.Request {
				return testutil.MakeJSONRequest(t, http.MethodPost, "/api/job/cancel", CancelJobRequest{JobID: jobID})
			},
			wantStatus: http.StatusOK,
			verify: func(t *testing.T, db *storage.DB, jobID int64) {
				updated, err := db.GetJobByID(jobID)
				require.NoError(t, err, "GetJobByID failed")
				assert.Equal(t, storage.JobStatusCanceled, updated.Status)
			},
		},
		{
			name: "cancel already canceled job",
			setup: func(t *testing.T, server *Server, db *storage.DB, tmpDir string) int64 {
				job := createTestJob(t, db, tmpDir, "alreadycanceled", "test")
				// Cancel through the same server's handler to exercise
				// the full code path including workerPool side-effects.
				req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/job/cancel", CancelJobRequest{JobID: job.ID})
				w := httptest.NewRecorder()
				server.httpServer.Handler.ServeHTTP(w, req)
				require.Equal(t, http.StatusOK, w.Code, "first cancel should succeed")
				return job.ID
			},
			request: func(t *testing.T, jobID int64) *http.Request {
				return testutil.MakeJSONRequest(t, http.MethodPost, "/api/job/cancel", CancelJobRequest{JobID: jobID})
			},
			wantStatus: http.StatusNotFound,
		},
		{
			name: "cancel nonexistent job",
			setup: func(t *testing.T, _ *Server, db *storage.DB, tmpDir string) int64 {
				return 99999
			},
			request: func(t *testing.T, jobID int64) *http.Request {
				return testutil.MakeJSONRequest(t, http.MethodPost, "/api/job/cancel", CancelJobRequest{JobID: jobID})
			},
			wantStatus: http.StatusNotFound,
		},
		{
			name: "cancel with missing job_id",
			setup: func(t *testing.T, _ *Server, db *storage.DB, tmpDir string) int64 {
				return 0
			},
			request: func(t *testing.T, jobID int64) *http.Request {
				return testutil.MakeJSONRequest(t, http.MethodPost, "/api/job/cancel", map[string]any{})
			},
			wantStatus: http.StatusUnprocessableEntity,
		},
		{
			name: "cancel with wrong method",
			setup: func(t *testing.T, _ *Server, db *storage.DB, tmpDir string) int64 {
				return 0
			},
			request: func(t *testing.T, jobID int64) *http.Request {
				return httptest.NewRequest(http.MethodGet, "/api/job/cancel", nil)
			},
			wantStatus: http.StatusMethodNotAllowed,
		},
		{
			name: "cancel running job",
			setup: func(t *testing.T, _ *Server, db *storage.DB, tmpDir string) int64 {
				job := createTestJob(t, db, tmpDir, "cancelrunning", "test")
				_, err := db.ClaimJob("worker-1")
				require.NoError(t, err, "ClaimJob failed")
				return job.ID
			},
			request: func(t *testing.T, jobID int64) *http.Request {
				return testutil.MakeJSONRequest(t, http.MethodPost, "/api/job/cancel", CancelJobRequest{JobID: jobID})
			},
			wantStatus: http.StatusOK,
			verify: func(t *testing.T, db *storage.DB, jobID int64) {
				updated, err := db.GetJobByID(jobID)
				require.NoError(t, err, "GetJobByID failed")
				assert.Equal(t, storage.JobStatusCanceled, updated.Status)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server, db, tmpDir := newTestServer(t)
			jobID := tt.setup(t, server, db, tmpDir)

			req := tt.request(t, jobID)
			w := httptest.NewRecorder()

			server.httpServer.Handler.ServeHTTP(w, req)

			assert.Equal(t, tt.wantStatus, w.Code, "response body: %s", w.Body.String())

			if tt.verify != nil {
				tt.verify(t, db, jobID)
			}
		})
	}
}

func TestHandleRerunJob(t *testing.T) {
	server, db, tmpDir := newTestServer(t)

	// Create a repo
	repo, err := db.GetOrCreateRepo(tmpDir)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "GetOrCreateRepo failed: %v", err)
	}

	t.Run("rerun failed job", func(t *testing.T) {
		commit, _ := db.GetOrCreateCommit(repo.ID, "rerun-failed", "Author", "Subject", time.Now())
		job, _ := db.EnqueueJob(storage.EnqueueOpts{RepoID: repo.ID, CommitID: commit.ID, GitRef: "rerun-failed", Agent: "test"})
		db.ClaimJob("worker-1")
		db.FailJob(job.ID, "", "some error")

		req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/job/rerun", RerunJobRequest{JobID: job.ID})
		w := httptest.NewRecorder()

		server.httpServer.Handler.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			assert.Condition(t, func() bool {
				return false
			}, "Expected status 200, got %d: %s", w.Code, w.Body.String())
		}

		updated, err := db.GetJobByID(job.ID)
		if err != nil {
			require.Condition(t, func() bool {
				return false
			}, "GetJobByID failed: %v", err)
		}
		if updated.Status != storage.JobStatusQueued {
			assert.Condition(t, func() bool {
				return false
			}, "Expected status 'queued', got '%s'", updated.Status)
		}
	})

	t.Run("rerun canceled job", func(t *testing.T) {
		commit, _ := db.GetOrCreateCommit(repo.ID, "rerun-canceled", "Author", "Subject", time.Now())
		job, _ := db.EnqueueJob(storage.EnqueueOpts{RepoID: repo.ID, CommitID: commit.ID, GitRef: "rerun-canceled", Agent: "test"})
		db.CancelJob(job.ID)

		req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/job/rerun", RerunJobRequest{JobID: job.ID})
		w := httptest.NewRecorder()

		server.httpServer.Handler.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			assert.Condition(t, func() bool {
				return false
			}, "Expected status 200, got %d: %s", w.Code, w.Body.String())
		}

		updated, err := db.GetJobByID(job.ID)
		if err != nil {
			require.Condition(t, func() bool {
				return false
			}, "GetJobByID failed: %v", err)
		}
		if updated.Status != storage.JobStatusQueued {
			assert.Condition(t, func() bool {
				return false
			}, "Expected status 'queued', got '%s'", updated.Status)
		}
	})

	t.Run("rerun done job", func(t *testing.T) {
		commit, _ := db.GetOrCreateCommit(repo.ID, "rerun-done", "Author", "Subject", time.Now())
		job, _ := db.EnqueueJob(storage.EnqueueOpts{RepoID: repo.ID, CommitID: commit.ID, GitRef: "rerun-done", Agent: "test"})
		// Claim and complete job
		var claimed *storage.ReviewJob
		for {
			claimed, _ = db.ClaimJob("worker-1")
			require.NotNil(t, claimed, "No job to claim")
			if claimed.ID == job.ID {
				break
			}
			db.CompleteJob(claimed.ID, "test", "prompt", "output")
		}
		db.CompleteJob(job.ID, "test", "prompt", "output")

		req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/job/rerun", RerunJobRequest{JobID: job.ID})
		w := httptest.NewRecorder()

		server.httpServer.Handler.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			assert.Condition(t, func() bool {
				return false
			}, "Expected status 200, got %d: %s", w.Code, w.Body.String())
		}

		updated, err := db.GetJobByID(job.ID)
		if err != nil {
			require.Condition(t, func() bool {
				return false
			}, "GetJobByID failed: %v", err)
		}
		if updated.Status != storage.JobStatusQueued {
			assert.Condition(t, func() bool {
				return false
			}, "Expected status 'queued', got '%s'", updated.Status)
		}
	})

	t.Run("rerun reevaluates implicit effective model", func(t *testing.T) {
		isolatedDB, isolatedDir := testutil.OpenTestDBWithDir(t)
		server := NewServer(isolatedDB, config.DefaultConfig(), "")
		agentName := "rerun-implicit-model"
		agent.Register(&commandTestAgent{name: agentName, command: "go"})
		t.Cleanup(func() {
			agent.Unregister(agentName)
		})

		repo, err := isolatedDB.GetOrCreateRepo(isolatedDir)
		require.NoError(t, err)
		commit, err := isolatedDB.GetOrCreateCommit(repo.ID, "rerun-implicit-model", "Author", "Subject", time.Now())
		require.NoError(t, err)
		job, err := isolatedDB.EnqueueJob(storage.EnqueueOpts{
			RepoID:   repo.ID,
			CommitID: commit.ID,
			GitRef:   "rerun-implicit-model",
			Agent:    agentName,
			Model:    "minimax-m2.5-free",
		})
		require.NoError(t, err)

		claimed, err := isolatedDB.ClaimJob("worker-1")
		require.NoError(t, err)
		require.NotNil(t, claimed)
		require.Equal(t, job.ID, claimed.ID)
		require.NoError(t, isolatedDB.CompleteJob(job.ID, agentName, "prompt", "output"))

		req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/job/rerun", RerunJobRequest{JobID: job.ID})
		w := httptest.NewRecorder()

		server.httpServer.Handler.ServeHTTP(w, req)
		testutil.AssertStatusCode(t, w, http.StatusOK)

		updated, err := isolatedDB.GetJobByID(job.ID)
		require.NoError(t, err)
		assert.Equal(t, storage.JobStatusQueued, updated.Status)
		assert.Empty(t, updated.Model, "rerun should recompute implicit model instead of preserving stale effective value")
	})

	t.Run("rerun queued job fails", func(t *testing.T) {
		commit, _ := db.GetOrCreateCommit(repo.ID, "rerun-queued", "Author", "Subject", time.Now())
		job, _ := db.EnqueueJob(storage.EnqueueOpts{RepoID: repo.ID, CommitID: commit.ID, GitRef: "rerun-queued", Agent: "test"})

		req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/job/rerun", RerunJobRequest{JobID: job.ID})
		w := httptest.NewRecorder()

		server.httpServer.Handler.ServeHTTP(w, req)

		if w.Code != http.StatusNotFound {
			assert.Condition(t, func() bool {
				return false
			}, "Expected status 404 for queued job, got %d", w.Code)
		}
	})

	t.Run("rerun with invalid worktree path fails", func(t *testing.T) {
		repoDir := filepath.Join(tmpDir, "rerun-invalid-worktree")
		testutil.InitTestGitRepo(t, repoDir)

		repo, err := db.GetOrCreateRepo(repoDir)
		require.NoError(t, err)
		commit, err := db.GetOrCreateCommit(repo.ID, "rerun-stale-worktree", "Author", "Subject", time.Now())
		require.NoError(t, err)
		job, err := db.EnqueueJob(storage.EnqueueOpts{
			RepoID:       repo.ID,
			CommitID:     commit.ID,
			GitRef:       "rerun-stale-worktree",
			Agent:        "test",
			WorktreePath: filepath.Join(tmpDir, "stale-worktree"),
		})
		require.NoError(t, err)

		for {
			claimed, err := db.ClaimJob("worker-stale-rerun")
			require.NoError(t, err)
			require.NotNil(t, claimed)
			if claimed.ID == job.ID {
				break
			}
			require.NoError(t, db.CompleteJob(claimed.ID, "test", "prompt", "output"))
		}
		require.NoError(t, db.CompleteJob(job.ID, "test", "prompt", "output"))

		req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/job/rerun", RerunJobRequest{JobID: job.ID})
		w := httptest.NewRecorder()

		server.httpServer.Handler.ServeHTTP(w, req)
		testutil.AssertStatusCode(t, w, http.StatusBadRequest)

		assert.Contains(t, w.Body.String(), "rerun job worktree path is stale or invalid")

		updated, err := db.GetJobByID(job.ID)
		require.NoError(t, err)
		assert.Equal(t, storage.JobStatusDone, updated.Status)
	})

	t.Run("rerun nonexistent job fails", func(t *testing.T) {
		req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/job/rerun", RerunJobRequest{JobID: 99999})
		w := httptest.NewRecorder()

		server.httpServer.Handler.ServeHTTP(w, req)

		if w.Code != http.StatusNotFound {
			assert.Condition(t, func() bool {
				return false
			}, "Expected status 404 for nonexistent job, got %d", w.Code)
		}
	})

	t.Run("rerun with missing job_id fails", func(t *testing.T) {
		req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/job/rerun", map[string]any{})
		w := httptest.NewRecorder()

		server.httpServer.Handler.ServeHTTP(w, req)

		if w.Code != http.StatusUnprocessableEntity {
			assert.Condition(t, func() bool {
				return false
			}, "Expected status 422 for missing job_id, got %d", w.Code)
		}
	})

	t.Run("rerun with invalid method fails", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/job/rerun", nil)
		w := httptest.NewRecorder()

		server.httpServer.Handler.ServeHTTP(w, req)

		if w.Code != http.StatusMethodNotAllowed {
			assert.Condition(t, func() bool {
				return false
			}, "Expected status 405 for GET, got %d", w.Code)
		}
	})
}

func TestWorkflowForJobFixType(t *testing.T) {
	assert := assert.New(t)
	assert.Equal("fix", workflowForJob(storage.JobTypeFix, config.ReviewTypeDefault))
	assert.Equal("fix", workflowForJob(storage.JobTypeCompact, config.ReviewTypeDefault))
	assert.Equal("review", workflowForJob(storage.JobTypeReview, config.ReviewTypeDefault))
	assert.Equal("security", workflowForJob(storage.JobTypeReview, "security"))
}

func TestResolveRerunModelProviderUsesWorktreeConfig(t *testing.T) {
	mainRepo := t.TempDir()
	testutil.InitTestGitRepo(t, mainRepo)
	worktreeRepo := filepath.Join(t.TempDir(), "worktree")
	worktreeAdd := exec.Command(
		"git", "-C", mainRepo, "worktree", "add", "--detach", worktreeRepo, "HEAD",
	)
	out, err := worktreeAdd.CombinedOutput()
	require.NoError(t, err, "git worktree add failed: %s", out)

	require.NoError(t, os.WriteFile(filepath.Join(mainRepo, ".roborev.toml"), []byte("review_model = \"main-model\"\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(worktreeRepo, ".roborev.toml"), []byte("review_model = \"worktree-model\"\n"), 0o644))

	job := &storage.ReviewJob{
		Agent:        "test",
		JobType:      storage.JobTypeReview,
		ReviewType:   config.ReviewTypeDefault,
		Reasoning:    "thorough",
		RepoPath:     mainRepo,
		WorktreePath: worktreeRepo,
	}

	model, provider, err := resolveRerunModelProvider(
		job, config.DefaultConfig(),
	)
	require.NoError(t, err)
	assert.Equal(t, "worktree-model", model)
	assert.Empty(t, provider)
}

func TestResolveRerunModelProviderRejectsInvalidWorktreeConfig(t *testing.T) {
	mainRepo := t.TempDir()
	stalePath := t.TempDir()

	require.NoError(t, os.WriteFile(filepath.Join(mainRepo, ".roborev.toml"), []byte("review_model = \"main-model\"\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(stalePath, ".roborev.toml"), []byte("review_model = \"stale-model\"\n"), 0o644))

	job := &storage.ReviewJob{
		Agent:        "test",
		JobType:      storage.JobTypeReview,
		ReviewType:   config.ReviewTypeDefault,
		Reasoning:    "thorough",
		RepoPath:     mainRepo,
		WorktreePath: stalePath,
	}

	model, provider, err := resolveRerunModelProvider(
		job, config.DefaultConfig(),
	)
	require.Error(t, err)
	require.ErrorContains(t, err, "rerun job worktree path is stale or invalid")
	assert.Empty(t, model)
	assert.Empty(t, provider)
}

func TestResolveRerunModelProviderRejectsInvalidWorktreeWithRequestedOverrides(t *testing.T) {
	mainRepo := t.TempDir()
	stalePath := t.TempDir()

	require.NoError(t, os.WriteFile(filepath.Join(mainRepo, ".roborev.toml"), []byte("review_model = \"main-model\"\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(stalePath, ".roborev.toml"), []byte("review_model = \"stale-model\"\n"), 0o644))

	job := &storage.ReviewJob{
		Agent:             "test",
		JobType:           storage.JobTypeReview,
		ReviewType:        config.ReviewTypeDefault,
		Reasoning:         "thorough",
		RepoPath:          mainRepo,
		WorktreePath:      stalePath,
		RequestedModel:    "requested-model",
		RequestedProvider: "anthropic",
	}

	model, provider, err := resolveRerunModelProvider(
		job, config.DefaultConfig(),
	)
	require.Error(t, err)
	require.ErrorContains(t, err, "rerun job worktree path is stale or invalid")
	assert.Empty(t, model)
	assert.Empty(t, provider)
}

func TestResolveRerunModelProviderPreservesRequestedOverridesOnParseableInvalidConfig(t *testing.T) {
	mainRepo := t.TempDir()

	require.NoError(t, os.WriteFile(filepath.Join(mainRepo, ".roborev.toml"), []byte("review_reasoning = \"bogus\"\n"), 0o644))

	job := &storage.ReviewJob{
		Agent:             "test",
		JobType:           storage.JobTypeReview,
		ReviewType:        config.ReviewTypeDefault,
		Reasoning:         "thorough",
		RepoPath:          mainRepo,
		RequestedModel:    "requested-model",
		RequestedProvider: "anthropic",
	}

	model, provider, err := resolveRerunModelProvider(
		job, config.DefaultConfig(),
	)
	require.NoError(t, err)
	assert.Equal(t, "requested-model", model)
	assert.Equal(t, "anthropic", provider)
}

func TestResolveRerunModelProviderRejectsMalformedConfigWithRequestedOverrides(t *testing.T) {
	mainRepo := t.TempDir()

	require.NoError(t, os.WriteFile(
		filepath.Join(mainRepo, ".roborev.toml"),
		[]byte("this is not valid toml [[["),
		0o644,
	))

	job := &storage.ReviewJob{
		Agent:             "test",
		JobType:           storage.JobTypeReview,
		ReviewType:        config.ReviewTypeDefault,
		Reasoning:         "thorough",
		RepoPath:          mainRepo,
		RequestedModel:    "requested-model",
		RequestedProvider: "anthropic",
	}

	model, provider, err := resolveRerunModelProvider(
		job, config.DefaultConfig(),
	)
	require.Error(t, err)
	require.ErrorContains(t, err, "resolve workflow config")
	assert.Empty(t, model)
	assert.Empty(t, provider)
}

func TestResolveRerunModelProviderRejectsInvalidAgentWithRequestedOverrides(t *testing.T) {
	mainRepo := t.TempDir()

	job := &storage.ReviewJob{
		Agent:             "missing-agent",
		JobType:           storage.JobTypeReview,
		ReviewType:        config.ReviewTypeDefault,
		Reasoning:         "thorough",
		RepoPath:          mainRepo,
		RequestedModel:    "requested-model",
		RequestedProvider: "anthropic",
	}

	model, provider, err := resolveRerunModelProvider(
		job, config.DefaultConfig(),
	)
	require.Error(t, err)
	require.ErrorContains(t, err, `invalid agent: unknown agent "missing-agent"`)
	assert.Empty(t, model)
	assert.Empty(t, provider)
}

func TestResolveRerunModelProviderAllowsUnavailablePrimaryWithBackup(t *testing.T) {
	t.Setenv("PATH", "")
	mainRepo := t.TempDir()

	job := &storage.ReviewJob{
		Agent:      "claude-code",
		JobType:    storage.JobTypeReview,
		ReviewType: config.ReviewTypeDefault,
		Reasoning:  "thorough",
		RepoPath:   mainRepo,
	}
	cfg := config.DefaultConfig()
	cfg.ReviewBackupAgent = "test"

	model, provider, err := resolveRerunModelProvider(job, cfg)

	require.NoError(t, err)
	assert.Empty(t, model)
	assert.Empty(t, provider)
}

func TestResolveRerunModelProviderAllowsUnavailablePrimaryWithStoredBackup(t *testing.T) {
	t.Setenv("PATH", "")
	mainRepo := t.TempDir()

	job := &storage.ReviewJob{
		Agent:       "claude-code",
		BackupAgent: "test",
		JobType:     storage.JobTypeReview,
		ReviewType:  config.ReviewTypeDefault,
		Reasoning:   "thorough",
		RepoPath:    mainRepo,
	}

	model, provider, err := resolveRerunModelProvider(job, config.DefaultConfig())

	require.NoError(t, err)
	assert.Empty(t, model)
	assert.Empty(t, provider)
}

// TestHandleAddCommentToJobStates tests that comments can be added to jobs
// in any state: queued, running, done, failed, and canceled.
func TestHandleAddCommentToJobStates(t *testing.T) {
	server, db, tmpDir := newTestServer(t)

	// Create repo and commit
	repo, err := db.GetOrCreateRepo(filepath.Join(tmpDir, "test-repo"))
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "GetOrCreateRepo failed: %v", err)
	}
	commit, err := db.GetOrCreateCommit(repo.ID, "abc123", "Author", "Test commit", time.Now())
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "GetOrCreateCommit failed: %v", err)
	}

	testCases := []struct {
		name   string
		status storage.JobStatus // empty string means keep as queued
	}{
		{"queued job", ""},
		{"running job", storage.JobStatusRunning},
		{"completed job", storage.JobStatusDone},
		{"failed job", storage.JobStatusFailed},
		{"canceled job", storage.JobStatusCanceled},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Create a job
			job, err := db.EnqueueJob(storage.EnqueueOpts{RepoID: repo.ID, CommitID: commit.ID, GitRef: "abc123", Agent: "test-agent"})
			if err != nil {
				require.Condition(t, func() bool {
					return false
				}, "EnqueueJob failed: %v", err)
			}

			// Set job to desired state
			if tc.status != "" {
				setJobStatus(t, db, job.ID, tc.status)
			}

			// Add comment via API
			reqData := AddCommentRequest{
				JobID:     job.ID,
				Commenter: "test-user",
				Comment:   "Test comment for " + tc.name,
			}
			req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/comment", reqData)
			w := httptest.NewRecorder()

			server.httpServer.Handler.ServeHTTP(w, req)

			if w.Code != http.StatusCreated {
				assert.Condition(t, func() bool {
					return false
				}, "Expected status 201, got %d: %s", w.Code, w.Body.String())
			}

			// Verify response contains the comment
			var resp storage.Response
			testutil.DecodeJSON(t, w, &resp)
			if resp.Responder != "test-user" {
				assert.Condition(t, func() bool {
					return false
				}, "Expected responder 'test-user', got %q", resp.Responder)
			}
		})
	}
}

// TestHandleAddCommentToNonExistentJob tests that adding a comment to a
// non-existent job returns 404.
func TestHandleAddCommentToNonExistentJob(t *testing.T) {
	server, _, _ := newTestServer(t)

	reqData := AddCommentRequest{
		JobID:     99999,
		Commenter: "test-user",
		Comment:   "This should fail",
	}
	req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/comment", reqData)
	w := httptest.NewRecorder()

	server.httpServer.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		assert.Condition(t, func() bool {
			return false
		}, "Expected status 404, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "job not found") {
		assert.Condition(t, func() bool {
			return false
		}, "Expected 'job not found' error, got: %s", w.Body.String())
	}
}

// TestHandleAddCommentWithoutReview tests that comments can be added to jobs
// that don't have a review yet (job exists but hasn't completed).
func TestHandleAddCommentWithoutReview(t *testing.T) {
	server, db, tmpDir := newTestServer(t)

	// Create repo, commit, and job (but NO review)
	job := createTestJob(t, db, filepath.Join(tmpDir, "test-repo"), "abc123", "test-agent")

	// Set job to running (no review exists yet)
	setJobStatus(t, db, job.ID, storage.JobStatusRunning)

	// Verify no review exists
	if _, err := db.GetReviewByJobID(job.ID); err == nil {
		require.Condition(t, func() bool {
			return false
		}, "Expected no review to exist for job")
	}

	// Add comment - should succeed even without a review
	reqData := AddCommentRequest{
		JobID:     job.ID,
		Commenter: "test-user",
		Comment:   "Comment on in-progress job without review",
	}
	req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/comment", reqData)
	w := httptest.NewRecorder()

	server.httpServer.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		assert.Condition(t, func() bool {
			return false
		}, "Expected status 201, got %d: %s", w.Code, w.Body.String())
	}

	// Verify comment was stored
	comments, err := db.GetCommentsForJob(job.ID)
	if err != nil {
		require.Condition(t, func() bool {
			return false
		}, "GetCommentsForJob failed: %v", err)
	}
	if len(comments) != 1 {
		require.Condition(t, func() bool {
			return false
		}, "Expected 1 comment, got %d", len(comments))
	}
	if comments[0].Response != "Comment on in-progress job without review" {
		assert.Condition(t, func() bool {
			return false
		}, "Unexpected comment: %q", comments[0].Response)
	}
}

func TestHandleCloseReview_BroadcastsEvent(t *testing.T) {
	assert := assert.New(t)
	server, db, tmpDir := newTestServer(t)

	// Create a completed job (which creates a review)
	job := createTestJob(t, db, tmpDir, "abc123", "test")
	claimed, err := db.ClaimJob("worker-1")
	require.NoError(t, err)
	require.Equal(t, job.ID, claimed.ID)
	require.NoError(t, db.CompleteJob(job.ID, "test", "prompt", "output"))

	// Subscribe to broadcaster before the close call
	_, eventCh := server.broadcaster.Subscribe("")

	req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/review/close", CloseReviewRequest{
		JobID:  job.ID,
		Closed: true,
	})
	w := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(w, req)

	assert.Equal(http.StatusOK, w.Code)

	// Verify event was broadcast with full metadata
	select {
	case event := <-eventCh:
		assert.Equal("review.closed", event.Type)
		assert.Equal(job.ID, event.JobID)
		assert.NotEmpty(event.Repo)
		assert.NotEmpty(event.RepoName)
		assert.Equal("abc123", event.SHA)
		assert.Equal("test", event.Agent)
	case <-time.After(time.Second):
		require.FailNow(t, "timed out waiting for review.closed event")
	}
}

func TestHandleCloseReview_BroadcastsReopenEvent(t *testing.T) {
	assert := assert.New(t)
	server, db, tmpDir := newTestServer(t)

	job := createTestJob(t, db, tmpDir, "reopen123", "test")
	claimed, err := db.ClaimJob("worker-1")
	require.NoError(t, err)
	require.Equal(t, job.ID, claimed.ID)
	require.NoError(t, db.CompleteJob(job.ID, "test", "prompt", "output"))

	// Close first, then reopen
	require.NoError(t, db.MarkReviewClosedByJobID(job.ID, true))

	_, eventCh := server.broadcaster.Subscribe("")

	req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/review/close", CloseReviewRequest{
		JobID:  job.ID,
		Closed: false,
	})
	w := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(w, req)

	assert.Equal(http.StatusOK, w.Code)

	select {
	case event := <-eventCh:
		assert.Equal("review.reopened", event.Type)
		assert.Equal(job.ID, event.JobID)
		assert.NotEmpty(event.Repo)
		assert.NotEmpty(event.RepoName)
		assert.Equal("reopen123", event.SHA)
		assert.Equal("test", event.Agent)
	case <-time.After(time.Second):
		require.FailNow(t, "timed out waiting for review.reopened event")
	}
}

func TestHandleCloseReview_RepoFilteredSubscriber(t *testing.T) {
	assert := assert.New(t)
	server, db, tmpDir := newTestServer(t)

	job := createTestJob(t, db, tmpDir, "filter123", "test")
	claimed, err := db.ClaimJob("worker-1")
	require.NoError(t, err)
	require.Equal(t, job.ID, claimed.ID)
	require.NoError(t, db.CompleteJob(job.ID, "test", "prompt", "output"))

	// Look up the normalized repo path used in the DB
	loaded, err := db.GetJobByID(job.ID)
	require.NoError(t, err)

	// Subscribe with repo filter — should receive the event
	_, filteredCh := server.broadcaster.Subscribe(loaded.RepoPath)
	// Subscribe with wrong repo — should NOT receive the event
	_, wrongCh := server.broadcaster.Subscribe("/nonexistent/repo")

	req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/review/close", CloseReviewRequest{
		JobID:  job.ID,
		Closed: true,
	})
	w := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(w, req)
	assert.Equal(http.StatusOK, w.Code)

	// Filtered subscriber receives the event
	select {
	case event := <-filteredCh:
		assert.Equal("review.closed", event.Type)
		assert.Equal(job.ID, event.JobID)
	case <-time.After(time.Second):
		require.FailNow(t, "repo-filtered subscriber did not receive review.closed")
	}

	// Wrong-repo subscriber does not receive the event
	select {
	case event := <-wrongCh:
		require.FailNow(t, "wrong-repo subscriber received event", "event: %v", event)
	case <-time.After(50 * time.Millisecond):
		// expected — no event
	}
}

func TestHandleEnqueue_BroadcastsEvent(t *testing.T) {
	assert := assert.New(t)
	server, _, tmpDir := newTestServer(t)

	repoDir := filepath.Join(tmpDir, "testrepo")
	testutil.InitTestGitRepo(t, repoDir)
	sha := testutil.GetHeadSHA(t, repoDir)

	_, eventCh := server.broadcaster.Subscribe("")

	req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/enqueue", EnqueueRequest{
		RepoPath: repoDir,
		GitRef:   sha,
		Agent:    "test",
	})
	w := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(w, req)

	assert.Equal(http.StatusCreated, w.Code)

	select {
	case event := <-eventCh:
		assert.Equal("job.enqueued", event.Type)
		// Repo path is resolved by git (symlinks, short names),
		// so compare non-empty rather than exact match.
		assert.NotEmpty(event.Repo)
		assert.Equal(sha, event.SHA)
		assert.Equal("test", event.Agent)
	case <-time.After(time.Second):
		require.FailNow(t, "timed out waiting for job.enqueued event")
	}
}

func TestHandleListCommentsJobIDParsing(t *testing.T) {
	server, _, _ := newTestServer(t)
	for _, id := range []string{"abc", "10abc", "1.5"} {
		t.Run("invalid_id_"+id, func(t *testing.T) {
			rr := serveHuma(t, server, http.MethodGet,
				"/api/comments?job_id="+id, nil)
			assert.GreaterOrEqual(t, rr.Code, 400,
				"expected client error for invalid id %q", id)
		})
	}
}
