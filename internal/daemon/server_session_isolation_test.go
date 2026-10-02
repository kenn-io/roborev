package daemon

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

func TestHandleEnqueueNeverReusesIsolatedSource(t *testing.T) {
	for _, dirty := range []bool{false, true} {
		name := "isolation disabled"
		if dirty {
			name = "dirty review while enabled"
		}
		t.Run(name, func(t *testing.T) {
			assert := assert.New(t)
			t.Setenv("ROBOREV_DATA_DIR", t.TempDir())
			server, db, _ := newTestServer(t)
			registered, err := agent.Get("test")
			require.NoError(t, err)
			require.IsType(t, &agent.TestAgent{}, registered)
			testAgent := registered.(*agent.TestAgent)
			priorCalls := len(testAgent.Calls())
			repo := testutil.NewGitRepo(t)
			sha := repo.CommitFile("file.txt", "base\n", "base")
			caller := filepath.Join(t.TempDir(), "caller")
			repo.Run("worktree", "add", "-b", "feature/session", caller, sha)
			cfg := server.configWatcher.Config()
			cfg.ReviewInWorktree = new(true)
			cfg.ReuseReviewSession = new(true)
			stored, err := db.GetOrCreateRepo(repo.Path())
			require.NoError(t, err)
			commit, err := db.GetOrCreateCommit(stored.ID, sha, "Author", "Subject", time.Now())
			require.NoError(t, err)
			source, err := db.EnqueueJob(storage.EnqueueOpts{
				RepoID: stored.ID, CommitID: commit.ID, GitRef: sha, Branch: "feature/session",
				Agent: "test", Reasoning: "thorough", ReviewType: config.ReviewTypeDefault,
				WorktreePath: caller,
			})
			require.NoError(t, err)
			claimed, err := db.ClaimJob(testWorkerID)
			require.NoError(t, err)
			server.workerPool.processJob(testWorkerID, claimed)
			completed, err := db.GetJobByID(source.ID)
			require.NoError(t, err)
			require.Equal(t, storage.JobStatusDone, completed.Status)
			require.NotEmpty(t, completed.SessionID)
			dirs, err := staleCIWorktreeDirs(ciWorktreeParentDir())
			require.NoError(t, err)
			assert.Empty(dirs, "the isolated source checkout has been removed")

			input := EnqueueRequest{RepoPath: caller, GitRef: "HEAD", Branch: "feature/session", Agent: "test", Panel: "none"}
			if dirty {
				input.GitRef = "dirty"
				input.DiffContent = "diff --git a/file.txt b/file.txt\n--- a/file.txt\n+++ b/file.txt\n@@ -1 +1 @@\n-base\n+changed\n"
				input.DirtyFiles = []string{"file.txt"}
			} else {
				cfg.ReviewInWorktree = new(false)
			}
			enqueue := func() storage.ReviewJob {
				w := httptest.NewRecorder()
				req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/enqueue", input)
				server.httpServer.Handler.ServeHTTP(w, req)
				require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
				var job storage.ReviewJob
				testutil.DecodeJSON(t, w, &job)
				return job
			}
			fresh := enqueue()
			assert.Empty(fresh.SessionID, "never inherit a session whose cwd was deleted")
			assert.Nil(fresh.ResumeSourceJobUUID)
			claimed, err = db.ClaimJob(testWorkerID)
			require.NoError(t, err)
			require.Equal(t, fresh.ID, claimed.ID)
			server.workerPool.processJob(testWorkerID, claimed)
			calls := testAgent.Calls()[priorCalls:]
			require.Len(t, calls, 2)
			assert.Empty(calls[1].SessionID, "the follow-on agent must start fresh")
			normal, err := db.GetJobByID(fresh.ID)
			require.NoError(t, err)
			require.Equal(t, storage.JobStatusDone, normal.Status)
			require.NotEmpty(t, normal.SessionID)
			assert.NotEqual(completed.SessionID, normal.SessionID)

			reused := enqueue()
			assert.Equal(normal.SessionID, reused.SessionID, "normal source sessions remain reusable")
			assert.Equal(normal.UUID, reused.ResumeSourceJobUUID)
		})
	}
}
