package daemon

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

func TestProjectPanelOverrideSurvivesAgentAutoDetection(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("minimal PATH setup uses POSIX symlink")
	}
	repo := testutil.NewTestRepoWithCommit(t)
	repo.AddRemote("origin", "https://example.com/team/project-a.git")
	cfg := config.DefaultConfig()
	cfg.Projects = map[string]config.ProjectConfig{
		"example.com/team/project-a": {ReviewModel: "project-model", OverridePanelModels: true},
	}
	cfg.CI.Panel = "panel-a"
	cfg.Review = config.ReviewConfig{
		Subagents: map[string]config.SubagentSpec{
			"security-check": {ReviewType: "security"},
		},
		Panels: map[string]config.PanelSpec{
			"panel-a": {Members: []string{"security-check"}, SynthesisAgent: "test"},
		},
	}
	cfg = cfg.ForRepo(repo.Path())
	members, _, err := config.ResolveCIPanel("panel-a", nil, cfg)
	require.NoError(t, err)
	require.Len(t, members, 1)
	gitPath, err := exec.LookPath("git")
	require.NoError(t, err)
	binDir := t.TempDir()
	require.NoError(t, os.Symlink(gitPath, filepath.Join(binDir, "git")))
	t.Setenv("PATH", binDir)
	agent.Register(&agent.FakeAgent{NameStr: "project-panel-auto"})
	t.Cleanup(func() { agent.Unregister("project-panel-auto") })

	t.Run("local", func(t *testing.T) {
		selected, model, _, _, err := resolvePanelMemberExecution(members[0], targetDescriptor{}, nil, cfg)
		require.NoError(t, err)
		assert.Equal(t, "project-panel-auto", selected)
		assert.Equal(t, "project-model", model)
	})
	t.Run("CI", func(t *testing.T) {
		p := &CIPoller{}
		selected, model, _, _, err := p.resolveCIPanelMemberExecution(nil, cfg, members[0])
		require.NoError(t, err)
		assert.Equal(t, "project-panel-auto", selected)
		assert.Equal(t, "project-model", model)
	})
}

func TestCIPollerProjectPanelModelOverride(t *testing.T) {
	for _, named := range []bool{false, true} {
		for _, override := range []bool{false, true} {
			t.Run(fmt.Sprintf("named=%t/override=%t", named, override), func(t *testing.T) {
				assert := assert.New(t)
				p, db, _, repo, cfg := newCIPanelGitHarness(t)
				repo.AddRemote("origin", "git@github.com:acme/api.git")
				cfg.CI.ReviewTypes = []string{"default", "security"}
				cfg.CI.Model = "pinned-model"
				cfg.CI.SynthesisModel = "pinned-synthesis"
				cfg.AutoDesignReview.Enabled = true
				cfg.DesignAgent = "test"
				project := config.ProjectConfig{ReviewModel: "project-model", OverridePanelModels: override}
				if override {
					project.SynthesisModel = "project-synthesis"
				}
				cfg.Projects = map[string]config.ProjectConfig{"github.com/acme/api": project}
				cfg.Review = config.ReviewConfig{
					Subagents: map[string]config.SubagentSpec{
						"general":        {Agent: "test", Model: "pinned-model", ReviewType: "default"},
						"security-check": {Agent: "test", Model: "pinned-model", ReviewType: "security"},
					},
					Panels: map[string]config.PanelSpec{
						"panel-a": {Members: []string{"general", "security-check"}, SynthesisAgent: "test", SynthesisModel: "pinned-synthesis"},
					},
				}
				if named {
					cfg.CI.Panel = "panel-a"
				}
				p.loadRepoConfigFn = func(string) (ciRepoConfigSource, error) {
					return ciRepoConfigSource{Config: &config.RepoConfig{}}, nil
				}
				base := repo.HeadSHA()
				head := repo.CommitFile("db/migrations/001_widgets.sql", "CREATE TABLE widgets(id INT);\n", "Add widgets table")
				p.mergeBaseFn = func(_, _, _ string) (string, error) { return base, nil }
				err := p.processPR(context.Background(), "acme/api", ghPR{
					Number: 1, HeadRefOid: head, BaseRefName: "main",
				}, cfg)
				require.NoError(t, err)
				panel, err := db.GetCIPanelByPRSHA("acme/api", 1, head)
				require.NoError(t, err)
				require.NotNil(t, panel)
				members, err := db.GetPanelMembers(panel.PanelRunUUID)
				require.NoError(t, err)
				require.Len(t, members, 3)
				want := "pinned-model"
				if override {
					want = "project-model"
				}
				for _, member := range members {
					assert.Equal(want, member.Model, member.ReviewType)
				}
				require.NotNil(t, panel.SynthesisJobID)
				synth, err := db.GetJobByID(*panel.SynthesisJobID)
				require.NoError(t, err)
				wantSynthesis := "pinned-synthesis"
				if override {
					wantSynthesis = "project-synthesis"
				}
				assert.Equal(wantSynthesis, synth.Model)
				assert.Equal("pinned-model", cfg.Review.Subagents["general"].Model)
			})
		}
	}
}

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
