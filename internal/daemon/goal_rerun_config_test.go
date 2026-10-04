package daemon

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/goalreview"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

func TestGoalReviewRerunConfigPolicy(t *testing.T) {
	t.Parallel()
	server, db, _ := newTestServer(t)
	executable, err := os.Executable()
	require.NoError(t, err)
	server.configWatcher.Config().PiCmd = executable

	for _, tt := range []struct {
		name           string
		source         string
		worktreeConfig string
		removeWorktree bool
		wantModel      string
	}{
		{"watcher conflicting config", "goal_watch", "review_model = 'worktree-model'", false, "main-model"},
		{"watcher malformed config", "goal_watch", "[invalid", false, "main-model"},
		{"manual worktree config", "", "review_model = 'worktree-model'", false, "worktree-model"},
		{"gate worktree config", "goal_gate", "review_model = 'worktree-model'", false, "worktree-model"},
		{"manual removed worktree", "", "review_model = 'worktree-model'", true, "main-model"},
		{"gate removed worktree", "goal_gate", "review_model = 'worktree-model'", true, "main-model"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			repo := testutil.NewTestRepoWithCommit(t)
			worktree := filepath.Join(t.TempDir(), "worktree")
			repo.Run("worktree", "add", "--detach", worktree, "HEAD")
			repo.WriteFile(".roborev.toml", "review_model = 'main-model'\n")
			require.NoError(t, os.WriteFile(filepath.Join(worktree, ".roborev.toml"), []byte(tt.worktreeConfig), 0o600))
			if tt.removeWorktree {
				repo.Run("worktree", "remove", "--force", worktree)
			}
			storedRepo, err := db.GetOrCreateRepo(repo.Path())
			require.NoError(t, err)
			snapshot := goalreview.Snapshot{
				Source: "superpowers", Stage: "spec",
				Artifacts: []goalreview.Artifact{{Kind: "spec", Path: "spec.md", Content: "# Frozen goal\n"}},
			}
			frozenPrompt := goalreview.BuildPrompt(snapshot)
			job, err := db.EnqueueJob(storage.EnqueueOpts{
				RepoID: storedRepo.ID, GitRef: snapshot.ID(), Agent: "pi", Model: "original-model",
				ReviewType: config.ReviewTypeGoal, JobType: storage.JobTypeGoalReview,
				Source: tt.source, WorktreePath: worktree, Prompt: frozenPrompt, PromptPrebuilt: true,
			})
			require.NoError(t, err)
			require.NoError(t, db.CancelJob(job.ID))

			request := testutil.MakeJSONRequest(t, http.MethodPost, "/api/job/rerun", RerunJobRequest{JobID: job.ID})
			response := httptest.NewRecorder()
			server.httpServer.Handler.ServeHTTP(response, request)
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())

			rerun, err := db.GetJobByID(job.ID)
			require.NoError(t, err)
			assert := assert.New(t)
			assert.Equal(storage.JobStatusQueued, rerun.Status)
			assert.Equal(tt.wantModel, rerun.Model)
			assert.Equal(tt.source, rerun.Source)
			assert.Equal(snapshot.ID(), rerun.GitRef)
			assert.Equal(frozenPrompt, rerun.Prompt)
		})
	}
}
