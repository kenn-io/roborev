package daemon

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

func TestWorkerIsolatedReviewFailoverSurvivesCallerRemoval(t *testing.T) {
	for _, fixture := range []struct {
		name             string
		remove           bool
		disableIsolation bool
		err              error
		beforeClaim      bool
		retryExhausted   bool
	}{
		{name: "cooldown before checkout after caller removal", remove: true, beforeClaim: true},
		{name: "nonisolated cooldown after caller removal", remove: true, beforeClaim: true, disableIsolation: true},
		{name: "context limit after caller removal", remove: true, err: errors.New("context window exceeded")},
		{name: "retries exhausted after caller removal", remove: true, retryExhausted: true, err: errors.New("synthetic temporary failure")},
		{name: "intact", err: errors.New("quota exhausted, reset after 2h")},
		{name: "quota after caller removal", remove: true, err: errors.New("quota exhausted, reset after 2h")},
		{name: "unavailable after caller removal", remove: true, err: agent.MarkUnavailable(errors.New("synthetic startup failure"))},
		{name: "caller disables isolation", remove: true, disableIsolation: true, err: errors.New("quota exhausted, reset after 2h")},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			assert := assert.New(t)
			t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
			server, db, _ := newTestServer(t)
			server.configWatcher.Config().IsolateReviews = true
			if fixture.beforeClaim && fixture.disableIsolation {
				// With the caller already gone, the main/global setting governs this attempt.
				server.configWatcher.Config().IsolateReviews = false
			}
			repo := testutil.NewGitRepo(t)
			const backupConfig = "backup_agent = 'gemini'\nbackup_model = 'backup-model'\n"
			sha := repo.CommitFile(".roborev.toml", backupConfig, "backup configuration")
			caller := filepath.Join(t.TempDir(), "caller")
			repo.Run("worktree", "add", "--detach", caller, sha)
			canonical, err := filepath.EvalSymlinks(caller)
			require.NoError(t, err)
			caller = canonical
			if fixture.disableIsolation {
				require.NoError(t, os.WriteFile(filepath.Join(caller, ".roborev.toml"), []byte(backupConfig+"isolate_reviews = false\n"), 0o600))
			}
			primaryCalls, backupCalls := 0, 0
			register := func(a agent.Agent) {
				original, err := agent.Get(a.Name())
				require.NoError(t, err)
				agent.Register(a)
				t.Cleanup(func() { agent.Register(original) })
			}
			register(&agent.FakeAgent{NameStr: "codex", ReviewFn: func(ctx context.Context, path, ref, prompt string, out io.Writer) (string, error) {
				primaryCalls++
				if fixture.disableIsolation {
					require.Equal(t, caller, path)
				} else {
					require.NotEqual(t, caller, path)
					require.NotEqual(t, repo.Path(), path)
				}
				require.DirExists(t, path)
				if fixture.remove {
					repo.Run("worktree", "remove", "--force", caller)
				}
				if !fixture.disableIsolation {
					require.DirExists(t, path, "isolated review remains usable after caller removal")
				}
				return "", fixture.err
			}})
			register(&agent.FakeAgent{NameStr: "gemini", ReviewFn: func(ctx context.Context, path, ref, prompt string, out io.Writer) (string, error) {
				backupCalls++
				require.NotEqual(t, caller, path)
				require.DirExists(t, path)
				return string(testutil.ReviewFixtureJSON("No issues found.")), nil
			}})
			req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/enqueue", EnqueueRequest{RepoPath: caller, GitRef: "HEAD", Agent: "codex", Panel: "none"})
			w := httptest.NewRecorder()
			server.httpServer.Handler.ServeHTTP(w, req)
			require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
			var queued storage.ReviewJob
			testutil.DecodeJSON(t, w, &queued)
			require.Empty(t, queued.BackupAgent)
			require.Empty(t, queued.BackupModel)
			require.Empty(t, queued.PanelMemberConfigJSON)
			assert.Equal(caller, queued.WorktreePath)
			_, events := server.broadcaster.Subscribe("")
			if fixture.beforeClaim {
				repo.Run("worktree", "remove", "--force", caller)
				server.workerPool.cooldownAgent("codex", time.Now().Add(time.Hour))
			}
			if fixture.retryExhausted {
				_, err := db.Exec("UPDATE review_jobs SET retry_count=? WHERE id=?", maxRetries, queued.ID)
				require.NoError(t, err)
			}
			claimed, err := db.ClaimJob(testWorkerID)
			require.NoError(t, err)
			require.NotNil(t, claimed)
			server.workerPool.processJob(testWorkerID, claimed)
			updated, err := db.GetJobByID(queued.ID)
			require.NoError(t, err)
			expectedPrimaryCalls := 1
			if fixture.beforeClaim {
				expectedPrimaryCalls = 0
			}
			require.Equal(t, expectedPrimaryCalls, primaryCalls)
			if !fixture.beforeClaim {
				require.NotEmpty(t, events)
				started := <-events
				assert.Equal("review.started", started.Type)
				assert.Equal(caller, started.WorktreePath)
				assert.Equal(queued.RepoPath, started.Repo)
			}
			if fixture.disableIsolation {
				assert.Equal(storage.JobStatusFailed, updated.Status)
				assert.Equal("codex", updated.Agent)
				assert.Zero(backupCalls)
				assert.Equal(caller, updated.WorktreePath)
				require.NotEmpty(t, events)
				failed := <-events
				assert.Equal("review.failed", failed.Type)
				assert.Equal(queued.RepoPath, failed.Repo)
				assert.Empty(failed.WorktreePath)
				return
			}
			require.Equal(t, storage.JobStatusQueued, updated.Status, "configured live backup must be selected")
			assert.Empty(events, "failover does not emit a premature completion/failure")
			assert.Equal("gemini", updated.Agent)
			assert.Equal("backup-model", updated.Model)
			assert.Equal(caller, updated.WorktreePath)
			claimed, err = db.ClaimJob(testWorkerID)
			require.NoError(t, err)
			require.NotNil(t, claimed)
			server.workerPool.processJob(testWorkerID, claimed)
			completed, err := db.GetJobByID(queued.ID)
			require.NoError(t, err)
			assert.Equal(1, backupCalls)
			assert.Equal(storage.JobStatusDone, completed.Status)
			assert.Equal(caller, completed.WorktreePath)
			assert.Equal(queued.RepoPath, completed.RepoPath)
			assert.Equal(queued.Branch, completed.Branch)
			for _, eventType := range []string{"review.started", "review.completed"} {
				require.NotEmpty(t, events)
				event := <-events
				assert.Equal(eventType, event.Type)
				assert.Equal(queued.RepoPath, event.Repo)
				assert.Equal(queued.ID, event.JobID)
				worktreePath := caller
				if fixture.remove {
					worktreePath = ""
				}
				assert.Equal(worktreePath, event.WorktreePath)
			}
		})
	}
}
