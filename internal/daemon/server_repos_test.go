package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gitrepo "go.kenn.io/kit/git/repo"

	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
)

func TestHandleResolveRepo(t *testing.T) {
	server, db, _ := newTestServer(t)
	repo := testutil.NewTestRepo(t)
	identity := "https://github.com/test/resolve-repo.git"
	registeredRoot, err := gitrepo.MainRoot(context.Background(), repo.Root)
	require.NoError(t, err)
	stored, err := db.GetOrCreateRepo(registeredRoot, identity)
	require.NoError(t, err)

	t.Run("tracked repo returns repo metadata", func(t *testing.T) {
		req := httptest.NewRequest(
			http.MethodGet,
			"/api/repos/resolve?path="+url.QueryEscape(repo.Root),
			nil,
		)
		w := httptest.NewRecorder()

		server.httpServer.Handler.ServeHTTP(w, req)

		testutil.AssertStatusCode(t, w, http.StatusOK)

		var response struct {
			Tracked bool `json:"tracked"`
			Repo    *struct {
				RootPath string `json:"root_path"`
				Identity string `json:"identity"`
				Name     string `json:"name"`
			} `json:"repo,omitempty"`
		}
		testutil.DecodeJSON(t, w, &response)

		require.True(t, response.Tracked)
		require.NotNil(t, response.Repo)
		assert.Equal(t, stored.RootPath, response.Repo.RootPath)
		assert.Equal(t, identity, response.Repo.Identity)
		assert.Equal(t, stored.Name, response.Repo.Name)
	})

	t.Run("path inside tracked repo resolves to root", func(t *testing.T) {
		nestedPath := filepath.Join(stored.RootPath, "src", "pkg")
		require.NoError(t, os.MkdirAll(nestedPath, 0o755))
		req := httptest.NewRequest(
			http.MethodGet,
			"/api/repos/resolve?path="+url.QueryEscape(nestedPath),
			nil,
		)
		w := httptest.NewRecorder()

		server.httpServer.Handler.ServeHTTP(w, req)

		testutil.AssertStatusCode(t, w, http.StatusOK)

		var response struct {
			Tracked bool `json:"tracked"`
			Repo    *struct {
				RootPath string `json:"root_path"`
			} `json:"repo,omitempty"`
		}
		testutil.DecodeJSON(t, w, &response)

		require.True(t, response.Tracked)
		require.NotNil(t, response.Repo)
		assert.Equal(t, stored.RootPath, response.Repo.RootPath)
	})

	t.Run("unknown repo returns untracked", func(t *testing.T) {
		unknownPath := filepath.Join(t.TempDir(), "unknown-repo")
		req := httptest.NewRequest(
			http.MethodGet,
			"/api/repos/resolve?path="+url.QueryEscape(unknownPath),
			nil,
		)
		w := httptest.NewRecorder()

		server.httpServer.Handler.ServeHTTP(w, req)

		testutil.AssertStatusCode(t, w, http.StatusOK)

		var response struct {
			Tracked bool `json:"tracked"`
			Repo    any  `json:"repo,omitempty"`
		}
		testutil.DecodeJSON(t, w, &response)

		assert.False(t, response.Tracked)
		assert.Nil(t, response.Repo)
	})

	t.Run("missing path returns client error", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/repos/resolve", nil)
		w := httptest.NewRecorder()

		server.httpServer.Handler.ServeHTTP(w, req)

		testutil.AssertStatusCode(t, w, http.StatusBadRequest)
		assert.Contains(t, w.Body.String(), "path is required")
	})
}

func TestHandleListRepos(t *testing.T) {
	server, db, tmpDir := newTestServer(t)

	t.Run("empty database", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/repos", nil)
		w := httptest.NewRecorder()

		server.httpServer.Handler.ServeHTTP(w, req)

		testutil.AssertStatusCode(t, w, http.StatusOK)

		var response struct {
			Repos      []struct{ Name string } `json:"repos"`
			TotalCount int                     `json:"total_count"`
		}
		testutil.DecodeJSON(t, w, &response)

		if len(response.Repos) != 0 {
			assert.Condition(t, func() bool {
				return false
			}, "Expected 0 repos, got %d", len(response.Repos))
		}
		if response.TotalCount != 0 {
			assert.Condition(t, func() bool {
				return false
			}, "Expected total_count 0, got %d", response.TotalCount)
		}
	})

	// Create repos and jobs
	seedRepoWithJobs(t, db, filepath.Join(tmpDir, "repo1"), 3, "repo1")
	seedRepoWithJobs(t, db, filepath.Join(tmpDir, "repo2"), 2, "repo2")

	t.Run("repos with jobs", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/repos", nil)
		w := httptest.NewRecorder()

		server.httpServer.Handler.ServeHTTP(w, req)

		testutil.AssertStatusCode(t, w, http.StatusOK)

		var response struct {
			Repos []struct {
				Name  string `json:"name"`
				Count int    `json:"count"`
			} `json:"repos"`
			TotalCount int `json:"total_count"`
		}
		testutil.DecodeJSON(t, w, &response)

		if len(response.Repos) != 2 {
			assert.Condition(t, func() bool {
				return false
			}, "Expected 2 repos, got %d", len(response.Repos))
		}
		if response.TotalCount != 5 {
			assert.Condition(t, func() bool {
				return false
			}, "Expected total_count 5, got %d", response.TotalCount)
		}

		repoMap := make(map[string]int)
		for _, r := range response.Repos {
			repoMap[r.Name] = r.Count
		}
		if repoMap["repo1"] != 3 {
			assert.Condition(t, func() bool {
				return false
			}, "Expected repo1 count 3, got %d", repoMap["repo1"])
		}
		if repoMap["repo2"] != 2 {
			assert.Condition(t, func() bool {
				return false
			}, "Expected repo2 count 2, got %d", repoMap["repo2"])
		}
	})

	t.Run("repos include identity", func(t *testing.T) {
		repo, err := db.GetRepoByName("repo1")
		require.NoError(t, err)
		identity := "https://github.com/test/repo1.git"
		require.NoError(t, db.SetRepoIdentity(repo.ID, identity))

		req := httptest.NewRequest(http.MethodGet, "/api/repos", nil)
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)
		testutil.AssertStatusCode(t, w, http.StatusOK)

		var response struct {
			Repos []struct {
				Name     string `json:"name"`
				Identity string `json:"identity"`
			} `json:"repos"`
		}
		testutil.DecodeJSON(t, w, &response)

		var found bool
		for _, r := range response.Repos {
			if r.Name == "repo1" {
				found = true
				assert.Equal(t, identity, r.Identity)
			}
		}
		assert.True(t, found)
	})

	t.Run("wrong method fails", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/repos", nil)
		w := httptest.NewRecorder()

		server.httpServer.Handler.ServeHTTP(w, req)

		if w.Code != http.StatusMethodNotAllowed {
			assert.Condition(t, func() bool {
				return false
			}, "Expected status 405 for POST, got %d", w.Code)
		}
	})
}

func TestHandleListReposWithBranchFilter(t *testing.T) {
	server, db, tmpDir := newTestServer(t)

	// Create repos and jobs
	seedRepoWithJobs(t, db, filepath.Join(tmpDir, "repo1"), 3, "repo1")
	seedRepoWithJobs(t, db, filepath.Join(tmpDir, "repo2"), 2, "repo2")
	repo1, err := db.GetRepoByName("repo1")
	require.NoError(t, err)
	repo2, err := db.GetRepoByName("repo2")
	require.NoError(t, err)
	repo1Identity := "https://github.com/test/repo1.git"
	repo2Identity := "https://github.com/test/repo2.git"
	require.NoError(t, db.SetRepoIdentity(repo1.ID, repo1Identity))
	require.NoError(t, db.SetRepoIdentity(repo2.ID, repo2Identity))

	// Set branches: repo1 jobs 1,2 = main, job 3 = feature; repo2 jobs 4,5 = main
	setJobBranch(t, db, 1, "main")
	setJobBranch(t, db, 2, "main")
	setJobBranch(t, db, 4, "main")
	setJobBranch(t, db, 5, "main")
	setJobBranch(t, db, 3, "feature")

	type reposResponse struct {
		Repos []struct {
			Name     string `json:"name"`
			Identity string `json:"identity"`
		} `json:"repos"`
		TotalCount int `json:"total_count"`
	}

	t.Run("filter by main branch", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/repos?branch=main", nil)
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)
		testutil.AssertStatusCode(t, w, http.StatusOK)

		var response reposResponse
		testutil.DecodeJSON(t, w, &response)
		if len(response.Repos) != 2 {
			assert.Condition(t, func() bool {
				return false
			}, "Expected 2 repos with main branch, got %d", len(response.Repos))
		}
		if response.TotalCount != 4 {
			assert.Condition(t, func() bool {
				return false
			}, "Expected total_count 4, got %d", response.TotalCount)
		}
		identities := map[string]string{}
		for _, repo := range response.Repos {
			identities[repo.Name] = repo.Identity
		}
		assert.Equal(t, repo1Identity, identities["repo1"])
		assert.Equal(t, repo2Identity, identities["repo2"])
	})

	t.Run("filter by feature branch", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/repos?branch=feature", nil)
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)
		testutil.AssertStatusCode(t, w, http.StatusOK)

		var response reposResponse
		testutil.DecodeJSON(t, w, &response)
		if len(response.Repos) != 1 {
			assert.Condition(t, func() bool {
				return false
			}, "Expected 1 repo with feature branch, got %d", len(response.Repos))
		}
		if response.TotalCount != 1 {
			assert.Condition(t, func() bool {
				return false
			}, "Expected total_count 1, got %d", response.TotalCount)
		}
		require.Len(t, response.Repos, 1)
		assert.Equal(t, repo1Identity, response.Repos[0].Identity)
	})

	t.Run("nonexistent branch returns empty", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/repos?branch=nonexistent", nil)
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)
		testutil.AssertStatusCode(t, w, http.StatusOK)

		var response reposResponse
		testutil.DecodeJSON(t, w, &response)
		if response.TotalCount != 0 {
			assert.Condition(t, func() bool {
				return false
			}, "Expected total_count 0 for nonexistent branch, got %d", response.TotalCount)
		}
	})
}

func TestHandleListReposWithPrefixFilter(t *testing.T) {
	server, db, tmpDir := newTestServer(t)

	// Create repos under a "workspace" parent and one outside it
	workspace := filepath.Join(tmpDir, "workspace")
	seedRepoWithJobs(t, db, filepath.Join(workspace, "repo1"), 3, "repo1")
	seedRepoWithJobs(t, db, filepath.Join(workspace, "repo2"), 2, "repo2")
	seedRepoWithJobs(t, db, filepath.Join(tmpDir, "other-repo"), 1, "other")

	type reposResponse struct {
		Repos      []struct{ Name string } `json:"repos"`
		TotalCount int                     `json:"total_count"`
	}

	t.Run("prefix returns only child repos", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/repos?prefix="+url.QueryEscape(workspace), nil)
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)
		testutil.AssertStatusCode(t, w, http.StatusOK)

		var response reposResponse
		testutil.DecodeJSON(t, w, &response)
		if len(response.Repos) != 2 {
			assert.Condition(t, func() bool {
				return false
			}, "Expected 2 repos under workspace, got %d", len(response.Repos))
		}
		if response.TotalCount != 5 {
			assert.Condition(t, func() bool {
				return false
			}, "Expected total_count 5, got %d", response.TotalCount)
		}
	})

	t.Run("prefix excludes non-matching repos", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/repos?prefix="+url.QueryEscape(filepath.Join(tmpDir, "nonexistent")), nil)
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)
		testutil.AssertStatusCode(t, w, http.StatusOK)

		var response reposResponse
		testutil.DecodeJSON(t, w, &response)
		if len(response.Repos) != 0 {
			assert.Condition(t, func() bool {
				return false
			}, "Expected 0 repos for nonexistent prefix, got %d", len(response.Repos))
		}
	})

	// Set branches on workspace repos: repo1 jobs 1,2=main, 3=feature; repo2 jobs 4,5=main
	setJobBranch(t, db, 1, "main")
	setJobBranch(t, db, 2, "main")
	setJobBranch(t, db, 3, "feature")
	setJobBranch(t, db, 4, "main")
	setJobBranch(t, db, 5, "main")

	t.Run("prefix + branch combined filter", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet,
			"/api/repos?prefix="+url.QueryEscape(workspace)+"&branch=main", nil)
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)
		testutil.AssertStatusCode(t, w, http.StatusOK)

		var response reposResponse
		testutil.DecodeJSON(t, w, &response)
		if len(response.Repos) != 2 {
			assert.Condition(t, func() bool {
				return false
			}, "Expected 2 repos with prefix+branch=main, got %d", len(response.Repos))
		}
		if response.TotalCount != 4 {
			assert.Condition(t, func() bool {
				return false
			}, "Expected total_count 4, got %d", response.TotalCount)
		}
	})

	t.Run("prefix + feature branch narrows results", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet,
			"/api/repos?prefix="+url.QueryEscape(workspace)+"&branch=feature", nil)
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)
		testutil.AssertStatusCode(t, w, http.StatusOK)

		var response reposResponse
		testutil.DecodeJSON(t, w, &response)
		if len(response.Repos) != 1 {
			assert.Condition(t, func() bool {
				return false
			}, "Expected 1 repo with prefix+branch=feature, got %d", len(response.Repos))
		}
		if response.TotalCount != 1 {
			assert.Condition(t, func() bool {
				return false
			}, "Expected total_count 1, got %d", response.TotalCount)
		}
	})

	t.Run("prefix with trailing slash is normalized", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet,
			"/api/repos?prefix="+url.QueryEscape(workspace+"/"), nil)
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)
		testutil.AssertStatusCode(t, w, http.StatusOK)

		var response reposResponse
		testutil.DecodeJSON(t, w, &response)
		if len(response.Repos) != 2 {
			assert.Condition(t, func() bool {
				return false
			}, "Expected 2 repos with trailing-slash prefix, got %d", len(response.Repos))
		}
	})

	t.Run("prefix with dot-dot is normalized", func(t *testing.T) {
		dotdotPrefix := workspace + "/../" + filepath.Base(workspace)
		req := httptest.NewRequest(http.MethodGet,
			"/api/repos?prefix="+url.QueryEscape(dotdotPrefix), nil)
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)
		testutil.AssertStatusCode(t, w, http.StatusOK)

		var response reposResponse
		testutil.DecodeJSON(t, w, &response)
		if len(response.Repos) != 2 {
			assert.Condition(t, func() bool {
				return false
			}, "Expected 2 repos with dot-dot prefix, got %d", len(response.Repos))
		}
	})
}

func TestHandleListReposSlashNormalization(t *testing.T) {
	server, db, tmpDir := newTestServer(t)

	// Store repos with forward-slash paths (matching ToSlash output)
	ws := filepath.ToSlash(tmpDir) + "/slash-ws"
	seedRepoWithJobs(t, db, ws+"/repo-x", 2, "rx")
	seedRepoWithJobs(t, db, ws+"/repo-y", 1, "ry")
	seedRepoWithJobs(t, db, filepath.ToSlash(tmpDir)+"/other-z", 1, "rz")

	type repoEntry struct {
		Name     string `json:"name"`
		RootPath string `json:"root_path"`
	}
	type reposResponse struct {
		Repos      []repoEntry `json:"repos"`
		TotalCount int         `json:"total_count"`
	}

	t.Run("forward-slash prefix matches stored paths", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet,
			"/api/repos?prefix="+url.QueryEscape(ws), nil)
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)
		testutil.AssertStatusCode(t, w, http.StatusOK)

		var response reposResponse
		testutil.DecodeJSON(t, w, &response)
		if len(response.Repos) != 2 {
			assert.Condition(t, func() bool {
				return false
			}, "Expected 2 repos with forward-slash prefix, got %d",
				len(response.Repos))
		}
		if response.TotalCount != 3 {
			assert.Condition(t, func() bool {
				return false
			}, "Expected total_count 3, got %d", response.TotalCount)
		}
		names := make(map[string]bool)
		for _, r := range response.Repos {
			names[r.Name] = true
		}
		if !names["repo-x"] || !names["repo-y"] {
			assert.Condition(t, func() bool {
				return false
			}, "Expected repo-x and repo-y, got %v", response.Repos)
		}
		if names["other-z"] {
			assert.Condition(t, func() bool {
				return false
			}, "other-z should not be in prefix-filtered results")
		}
	})
}

func TestHandleListBranches(t *testing.T) {
	server, db, tmpDir := newTestServer(t)

	// Create repos and jobs
	seedRepoWithJobs(t, db, filepath.Join(tmpDir, "repo1"), 3, "repo1")
	seedRepoWithJobs(t, db, filepath.Join(tmpDir, "repo2"), 2, "repo2")

	// Set branches: jobs 1,2,4 = main, job 3 = feature, job 5 = no branch
	setJobBranch(t, db, 1, "main")
	setJobBranch(t, db, 2, "main")
	setJobBranch(t, db, 4, "main")
	setJobBranch(t, db, 3, "feature")
	// job 5 left empty

	type branchesResponse struct {
		Branches       []json.RawMessage `json:"branches"`
		TotalCount     int               `json:"total_count"`
		NullsRemaining int               `json:"nulls_remaining"`
	}

	t.Run("list all branches", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/branches", nil)
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)
		testutil.AssertStatusCode(t, w, http.StatusOK)

		var response branchesResponse
		testutil.DecodeJSON(t, w, &response)
		if len(response.Branches) != 3 {
			assert.Condition(t, func() bool {
				return false
			}, "Expected 3 branches, got %d", len(response.Branches))
		}
		if response.TotalCount != 5 {
			assert.Condition(t, func() bool {
				return false
			}, "Expected total_count 5, got %d", response.TotalCount)
		}
		if response.NullsRemaining != 1 {
			assert.Condition(t, func() bool {
				return false
			}, "Expected nulls_remaining 1, got %d", response.NullsRemaining)
		}
	})

	t.Run("filter by repo", func(t *testing.T) {
		repoPath := filepath.Join(tmpDir, "repo1")
		req := httptest.NewRequest(http.MethodGet, "/api/branches?repo="+repoPath, nil)
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)
		testutil.AssertStatusCode(t, w, http.StatusOK)

		var response branchesResponse
		testutil.DecodeJSON(t, w, &response)
		if len(response.Branches) != 2 {
			assert.Condition(t, func() bool {
				return false
			}, "Expected 2 branches for repo1, got %d", len(response.Branches))
		}
		if response.TotalCount != 3 {
			assert.Condition(t, func() bool {
				return false
			}, "Expected total_count 3 for repo1, got %d", response.TotalCount)
		}
	})

	t.Run("filter by multiple repos", func(t *testing.T) {
		repo1Path := filepath.Join(tmpDir, "repo1")
		repo2Path := filepath.Join(tmpDir, "repo2")
		req := httptest.NewRequest(http.MethodGet,
			"/api/branches?repo="+url.QueryEscape(repo1Path)+"&repo="+url.QueryEscape(repo2Path), nil)
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)
		testutil.AssertStatusCode(t, w, http.StatusOK)

		var response branchesResponse
		testutil.DecodeJSON(t, w, &response)
		if len(response.Branches) != 3 {
			assert.Condition(t, func() bool {
				return false
			}, "Expected 3 branches for both repos, got %d", len(response.Branches))
		}
		if response.TotalCount != 5 {
			assert.Condition(t, func() bool {
				return false
			}, "Expected total_count 5 for both repos, got %d", response.TotalCount)
		}
	})

	t.Run("empty repo param treated as no filter", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/branches?repo=", nil)
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)
		testutil.AssertStatusCode(t, w, http.StatusOK)

		var response branchesResponse
		testutil.DecodeJSON(t, w, &response)
		if len(response.Branches) != 3 {
			assert.Condition(t, func() bool {
				return false
			}, "Expected 3 branches (empty repo = no filter), got %d", len(response.Branches))
		}
		if response.TotalCount != 5 {
			assert.Condition(t, func() bool {
				return false
			}, "Expected total_count 5 (empty repo = no filter), got %d", response.TotalCount)
		}
	})

	t.Run("wrong method fails", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/branches", nil)
		w := httptest.NewRecorder()

		server.httpServer.Handler.ServeHTTP(w, req)

		if w.Code != http.StatusMethodNotAllowed {
			assert.Condition(t, func() bool {
				return false
			}, "Expected status 405 for POST, got %d", w.Code)
		}
	})
}

func TestHandleRegisterRepo(t *testing.T) {
	t.Run("GET returns 405", func(t *testing.T) {
		server, _, _ := newTestServer(t)
		req := httptest.NewRequest(http.MethodGet, "/api/repos/register", nil)
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)
		if w.Code != http.StatusMethodNotAllowed {
			assert.Condition(t, func() bool {
				return false
			}, "Expected 405, got %d", w.Code)
		}
	})

	t.Run("empty body returns 400", func(t *testing.T) {
		server, _, _ := newTestServer(t)
		req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/repos/register", map[string]string{})
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			assert.Condition(t, func() bool {
				return false
			}, "Expected 400, got %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("non-git path returns 400", func(t *testing.T) {
		server, _, _ := newTestServer(t)
		plainDir := t.TempDir()
		req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/repos/register", map[string]string{
			"repo_path": plainDir,
		})
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			assert.Condition(t, func() bool {
				return false
			}, "Expected 400, got %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("valid git repo returns 200 and persists", func(t *testing.T) {
		server, db, tmpDir := newTestServer(t)
		repoDir := filepath.Join(tmpDir, "testrepo")
		testutil.InitTestGitRepo(t, repoDir)

		// Add a remote so identity resolves to something meaningful
		remoteCmd := exec.Command("git", "-C", repoDir, "remote", "add", "origin", "https://github.com/test/testrepo.git")
		if out, err := remoteCmd.CombinedOutput(); err != nil {
			require.Condition(t, func() bool {
				return false
			}, "git remote add failed: %v\n%s", err, out)
		}

		req := testutil.MakeJSONRequest(t, http.MethodPost, "/api/repos/register", map[string]string{
			"repo_path": repoDir,
		})
		w := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			require.Condition(t, func() bool {
				return false
			}, "Expected 200, got %d: %s", w.Code, w.Body.String())
		}

		var repo storage.Repo
		testutil.DecodeJSON(t, w, &repo)
		if repo.ID == 0 {
			assert.Condition(t, func() bool {
				return false
			}, "Expected non-zero repo ID")
		}
		if repo.Identity == "" {
			assert.Condition(t, func() bool {
				return false
			}, "Expected non-empty identity")
		}

		// Verify repo is in the DB
		repos, err := db.ListRepos()
		if err != nil {
			require.Condition(t, func() bool {
				return false
			}, "ListRepos: %v", err)
		}
		if len(repos) != 1 {
			require.Condition(t, func() bool {
				return false
			}, "Expected 1 repo in DB, got %d", len(repos))
		}
	})

	t.Run("idempotent", func(t *testing.T) {
		server, db, tmpDir := newTestServer(t)
		repoDir := filepath.Join(tmpDir, "testrepo")
		testutil.InitTestGitRepo(t, repoDir)

		body := map[string]string{"repo_path": repoDir}

		// First call
		req1 := testutil.MakeJSONRequest(t, http.MethodPost, "/api/repos/register", body)
		w1 := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w1, req1)
		if w1.Code != http.StatusOK {
			require.Condition(t, func() bool {
				return false
			}, "First call: expected 200, got %d: %s", w1.Code, w1.Body.String())
		}

		// Second call
		req2 := testutil.MakeJSONRequest(t, http.MethodPost, "/api/repos/register", body)
		w2 := httptest.NewRecorder()
		server.httpServer.Handler.ServeHTTP(w2, req2)
		if w2.Code != http.StatusOK {
			require.Condition(t, func() bool {
				return false
			}, "Second call: expected 200, got %d: %s", w2.Code, w2.Body.String())
		}

		// Still only one repo in DB
		repos, err := db.ListRepos()
		if err != nil {
			require.Condition(t, func() bool {
				return false
			}, "ListRepos: %v", err)
		}
		if len(repos) != 1 {
			require.Condition(t, func() bool {
				return false
			}, "Expected 1 repo in DB after two calls, got %d", len(repos))
		}
	})
}
