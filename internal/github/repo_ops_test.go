package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	googlegithub "github.com/google/go-github/v88/github"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type repoAPIServer struct {
	t *testing.T

	orgRepos   []*googlegithub.Repository
	userRepos  []*googlegithub.Repository
	authRepos  []*googlegithub.Repository
	authStatus int
}

func (s *repoAPIServer) handler(w http.ResponseWriter, r *http.Request) {
	s.t.Helper()
	w.Header().Set("Content-Type", "application/json")
	path := strings.TrimPrefix(r.URL.Path, "/api/v3")

	switch {
	case r.Method == http.MethodGet && path == "/orgs/acme/repos":
		assert.NoError(s.t, json.NewEncoder(w).Encode(s.orgRepos))
	case r.Method == http.MethodGet && path == "/orgs/jane/repos":
		w.WriteHeader(http.StatusNotFound)
		assert.NoError(s.t, json.NewEncoder(w).Encode(map[string]any{"message": "not found"}))
	case r.Method == http.MethodGet && path == "/users/acme/repos":
		assert.NoError(s.t, json.NewEncoder(w).Encode(s.orgRepos))
	case r.Method == http.MethodGet && path == "/users/jane/repos":
		assert.NoError(s.t, json.NewEncoder(w).Encode(s.userRepos))
	case r.Method == http.MethodGet && path == "/user/repos":
		if s.authStatus != 0 {
			w.WriteHeader(s.authStatus)
			assert.NoError(s.t, json.NewEncoder(w).Encode(map[string]any{"message": "auth failed"}))
			return
		}
		assert.NoError(s.t, json.NewEncoder(w).Encode(s.authRepos))
	default:
		http.NotFound(w, r)
	}
}

func TestListOwnerRepos_FiltersArchivedAndFallsBackToAuthenticatedUser(t *testing.T) {
	api := &repoAPIServer{
		t: t,
		orgRepos: []*googlegithub.Repository{
			{FullName: ptr("acme/api"), Archived: ptr(false)},
			{FullName: ptr("acme/old"), Archived: ptr(true)},
		},
		userRepos: []*googlegithub.Repository{
			{
				FullName: ptr("jane/app"),
				Archived: ptr(false),
				Owner:    &googlegithub.User{Login: ptr("jane")},
			},
		},
		authRepos: []*googlegithub.Repository{
			{
				FullName: ptr("jane/private"),
				Archived: ptr(false),
				Owner:    &googlegithub.User{Login: ptr("jane")},
			},
			{
				FullName: ptr("other/nope"),
				Archived: ptr(false),
				Owner:    &googlegithub.User{Login: ptr("other")},
			},
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(api.handler))
	defer srv.Close()

	client, err := NewClient("", WithBaseURL(srv.URL+"/"))
	require.NoError(t, err)

	orgRepos, err := client.ListOwnerRepos(context.Background(), "acme", 1000)
	require.NoError(t, err)
	assert.Equal(t, []string{"acme/api"}, orgRepos)

	userRepos, err := client.ListOwnerRepos(context.Background(), "jane", 1000)
	require.NoError(t, err)
	assert.Equal(t, []string{"jane/app", "jane/private"}, userRepos)
}

func TestListOwnerRepos_KeepsPublicReposWhenAuthenticatedListingFails(t *testing.T) {
	api := &repoAPIServer{
		t: t,
		userRepos: []*googlegithub.Repository{
			{
				FullName: ptr("jane/app"),
				Archived: ptr(false),
				Owner:    &googlegithub.User{Login: ptr("jane")},
			},
		},
		authStatus: http.StatusUnauthorized,
	}
	srv := httptest.NewServer(http.HandlerFunc(api.handler))
	defer srv.Close()

	client, err := NewClient("", WithBaseURL(srv.URL+"/"))
	require.NoError(t, err)

	repos, err := client.ListOwnerRepos(context.Background(), "jane", 1000)
	require.NoError(t, err)
	assert.Equal(t, []string{"jane/app"}, repos)
}

func TestNewClient_DefaultHTTPTimeout(t *testing.T) {
	client, err := NewClient("")
	require.NoError(t, err)
	assert.Equal(t, defaultHTTPTimeout, client.api.Client().Timeout)
}

func TestNewClient_UsesCustomHTTPClient(t *testing.T) {
	customHTTPClient := &http.Client{Timeout: 5 * time.Second}

	client, err := NewClient("", WithHTTPClient(customHTTPClient))
	require.NoError(t, err)
	assert.Equal(t, customHTTPClient.Timeout, client.api.Client().Timeout)
}

func TestCloneURL_DoesNotEmbedToken(t *testing.T) {
	plain, err := CloneURL("owner/repo")
	require.NoError(t, err)
	assert.Equal(t, "https://github.com/owner/repo.git", plain)
}

func TestCloneURLForBase_UsesEnterpriseHost(t *testing.T) {
	plain, err := CloneURLForBase("owner/repo", "https://ghe.example.com/api/v3/")
	require.NoError(t, err)
	assert.Equal(t, "https://ghe.example.com/owner/repo.git", plain)
}

func TestHostnameFromAPIBaseURL(t *testing.T) {
	t.Setenv("GH_HOST", "")
	tests := []struct {
		name string
		url  string
		want string
	}{
		{"public api", "https://api.github.com/", "github.com"},
		{"enterprise", "https://ghe.example.com/api/v3/", "ghe.example.com"},
		{"enterprise no trailing slash", "https://ghe.corp.net/api/v3", "ghe.corp.net"},
		{"enterprise custom port", "https://ghe.example.com:8443/api/v3/", "ghe.example.com:8443"},
		{"empty falls back to default", "", "github.com"},
		{"invalid falls back to default", "://bad", "github.com"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, HostnameFromAPIBaseURL(tt.url))
		})
	}
}

func TestHostnameFromAPIBaseURL_RespectsGHHost(t *testing.T) {
	t.Setenv("GH_HOST", "ghe.fallback.com")
	assert.Equal(t, "ghe.fallback.com", HostnameFromAPIBaseURL(""))
}

func TestGitAuthEnv_UsesTransientExtraHeader(t *testing.T) {
	baseEnv := []string{"PATH=" + os.Getenv("PATH")}

	env := GitAuthEnv(baseEnv, "abc123")
	joined := strings.Join(env, "\n")

	assert.Contains(t, joined, "GIT_CONFIG_COUNT=1")
	assert.Contains(t, joined, "GIT_CONFIG_KEY_0=http.https://github.com/.extraheader")
	assert.Contains(t, joined, "AUTHORIZATION: basic ")
	assert.NotContains(t, joined, "https://x-access-token:abc123@github.com")
	assert.NotContains(t, joined, "abc123@github.com")
}

func TestGitAuthEnvForBase_PreservesExistingGitConfig(t *testing.T) {
	baseEnv := []string{
		"PATH=" + os.Getenv("PATH"),
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.sslCAInfo",
		"GIT_CONFIG_VALUE_0=/tmp/custom-ca.pem",
	}

	env := GitAuthEnvForBase(baseEnv, "abc123", "https://ghe.example.com/api/v3/")
	joined := strings.Join(env, "\n")

	assert.Contains(t, joined, "GIT_CONFIG_COUNT=2")
	assert.Contains(t, joined, "GIT_CONFIG_KEY_0=http.sslCAInfo")
	assert.Contains(t, joined, "GIT_CONFIG_VALUE_0=/tmp/custom-ca.pem")
	assert.Contains(t, joined, "GIT_CONFIG_KEY_1=http.https://ghe.example.com/.extraheader")
	assert.Contains(t, joined, "AUTHORIZATION: basic ")
}
