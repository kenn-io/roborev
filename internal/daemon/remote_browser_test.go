package daemon

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/storage"
)

func TestRemotePublishedBrowserRequiresIndependentLogin(t *testing.T) {
	s, remoteCfg, key := remoteFixture(t)
	allowed, err := s.db.GetOrCreateRepo("/synthetic/allowed")
	require.NoError(t, err)
	_, err = s.db.EnqueueJob(storage.EnqueueOpts{RepoID: allowed.ID, GitRef: "granted-history", Agent: "test"})
	require.NoError(t, err)
	denied, err := s.db.GetOrCreateRepo("/synthetic/denied")
	require.NoError(t, err)
	job, err := s.db.EnqueueJob(storage.EnqueueOpts{RepoID: denied.ID, GitRef: "outside-grant-history", Agent: "test"})
	require.NoError(t, err)
	remoteCfg.Keys[0].AllRepos = false
	remoteCfg.Keys[0].RepoIDs = []int64{allowed.ID}
	cfg := config.DefaultConfig()
	cfg.AuthKey, cfg.Remote = s.authKey, remoteCfg
	cfg.Web.Listen = "127.0.0.1:0"
	cfg.Web.PublicOrigin = "https://reviews.example.com"
	configPath := filepath.Join(t.TempDir(), "config.toml")
	var contents bytes.Buffer
	require.NoError(t, toml.NewEncoder(&contents).Encode(cfg))
	require.NoError(t, os.WriteFile(configPath, contents.Bytes(), 0o600))
	_, err = config.LoadGlobalFrom(configPath)
	require.ErrorContains(t, err, "web.auth_token")

	cfg.Web.AuthToken = testBrowserAuthToken
	require.NoError(t, config.SaveGlobalTo(configPath, cfg))
	cfg, err = config.LoadGlobalFrom(configPath)
	require.NoError(t, err)
	remote, err := s.newRemoteHandler(cfg.Remote)
	require.NoError(t, err)
	defer remote.Close()
	out := httptest.NewRecorder()
	remote.ServeHTTP(out, remoteRequest(t, key, fmt.Sprintf("/api/jobs?id=%d", job.ID)))
	require.Equal(t, http.StatusForbidden, out.Code)
	// The same scoped credential can read its granted repository.
	out = httptest.NewRecorder()
	remote.ServeHTTP(out, remoteRequest(t, key, "/api/jobs"))
	require.Equal(t, http.StatusOK, out.Code)
	require.Contains(t, out.Body.String(), "granted-history")
	require.NotContains(t, out.Body.String(), "outside-grant-history")

	s.allowWebCompilationStub = true
	browser, err := s.startBrowserServer(cfg.Web)
	require.NoError(t, err)
	for _, tc := range []struct {
		token  string
		status int
	}{
		{s.authKey, http.StatusUnauthorized},
		{testBrowserAuthToken, http.StatusOK},
	} {
		r, err := http.NewRequest(http.MethodPost, "http://"+browser.Address+"/api/ui/session/login", strings.NewReader(`{"token":"`+tc.token+`"}`))
		require.NoError(t, err)
		r.Host = "reviews.example.com"
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", cfg.Web.PublicOrigin)
		r.Header.Set("X-Forwarded-For", "192.0.2.1")
		resp, err := http.DefaultClient.Do(r)
		require.NoError(t, err)
		resp.Body.Close()
		require.Equal(t, tc.status, resp.StatusCode)
	}
}
