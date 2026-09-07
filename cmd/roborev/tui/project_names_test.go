package tui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

func TestProjectDisplayNameGroupsBareWorktrees(t *testing.T) {
	assert := assert.New(t)
	repo := testutil.NewTestRepoWithCommit(t)
	bare := filepath.Join(t.TempDir(), "project.git")
	repo.Run("clone", "--bare", repo.Path(), bare)
	repo.Run("-C", bare, "remote", "set-url", "origin", "git@example.com:team/project-a.git")
	var repos []storage.RepoWithCount
	var paths []string
	for _, branch := range []string{"feature-a", "feature-b"} {
		wt := filepath.Join(t.TempDir(), branch)
		repo.Run("-C", bare, "worktree", "add", "-b", branch, wt)
		paths = append(paths, wt)
		repos = append(repos, storage.RepoWithCount{Name: branch, RootPath: wt, Count: 1})
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"repos": repos})
	}))
	t.Cleanup(server.Close)
	m := newModel(testEndpointFromURL(server.URL), withExternalIODisabled())
	m.globalCfg = &config.Config{Projects: map[string]config.ProjectConfig{
		"example.com/team/project-a": {DisplayName: "Project A"},
	}}
	msg := m.fetchRepos()()
	result, ok := msg.(reposMsg)
	require.True(t, ok, "unexpected message: %T", msg)
	require.Len(t, result.repos, 1)
	assert.Equal("Project A", result.repos[0].name)
	assert.ElementsMatch(paths, result.repos[0].rootPaths)
	assert.Equal(2, result.repos[0].count)
	names, ok := m.fetchRepoNames()().(repoNamesMsg)
	require.True(t, ok)
	assert.ElementsMatch(paths, names.names["Project A"])
}
