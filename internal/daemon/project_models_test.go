package daemon

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

func TestEnqueueProjectModelFromBareWorktrees(t *testing.T) {
	repo := testutil.NewTestRepoWithCommit(t)
	bare := filepath.Join(t.TempDir(), "project.git")
	repo.Run("clone", "--bare", repo.Path(), bare)
	repo.Run("-C", bare, "remote", "set-url", "origin", "ssh://git@example.com/team/project-a.git")
	cfg := config.DefaultConfig()
	cfg.DefaultAgent = "test"
	cfg.ReviewModel = "default-review"
	cfg.Projects = map[string]config.ProjectConfig{
		"example.com/team/project-a": {ReviewModel: "project-review", DisplayName: "Project A"},
	}
	db, _ := testutil.OpenTestDBWithDir(t)
	server := NewServer(db, cfg, "")
	t.Cleanup(func() { require.NoError(t, server.Close()) })

	for _, branch := range []string{"feature-a", "feature-b"} {
		wt := filepath.Join(t.TempDir(), branch)
		repo.Run("-C", bare, "worktree", "add", "-b", branch, wt)
		resolution, err := agent.ResolveWorkflowConfig("", wt, cfg, "review", "thorough")
		require.NoError(t, err)
		assert.Equal(t, "project-review", resolution.ModelForSelectedAgent("test", ""))
		for _, explicit := range []string{"", "cli-review"} {
			request := testutil.MakeJSONRequest(t, http.MethodPost, "/api/enqueue", EnqueueRequest{
				RepoPath: wt, GitRef: "HEAD", Agent: "test", Model: explicit,
			})
			response := httptest.NewRecorder()
			server.httpServer.Handler.ServeHTTP(response, request)
			require.Equal(t, http.StatusCreated, response.Code, response.Body.String())
			var job storage.ReviewJob
			testutil.DecodeJSON(t, response, &job)
			want := "project-review"
			if explicit != "" {
				want = explicit
			}
			assert.Equal(t, want, job.Model)
			stored, err := db.GetJobByID(job.ID)
			require.NoError(t, err)
			assert.Equal(t, want, stored.Model)
		}
		assert.Equal(t, "Project A", config.GetDisplayName(wt, cfg))
	}
	assert.Equal(t, "default-review", config.ResolveModelForWorkflowFromConfig("", nil, cfg, "review", "thorough"))
}
