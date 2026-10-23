package daemon

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/kata/katafake"
	"go.kenn.io/roborev/internal/testutil"
)

const kataTestCredential = "synthetic-kata-credential"

// kataAPIServer is a daemon whose Kata connection points at a fake Kata.
func kataAPIServer(t *testing.T) (*Server, *katafake.Server) {
	t.Helper()
	fake := katafake.New(t)
	server, _, _ := newTestServer(t)
	enableKataConnection(server, fake)
	return server, fake
}

func kataGet(t *testing.T, server *Server, path string, query url.Values) *httptest.ResponseRecorder {
	t.Helper()
	target := path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	w := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, target, nil))
	return w
}

func kataPost(t *testing.T, server *Server, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	server.httpServer.Handler.ServeHTTP(w, testutil.MakeJSONRequest(t, http.MethodPost, path, body))
	return w
}

func decodeKata[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var out T
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out), w.Body.String())
	return out
}

func TestKataStatusDisabledMakesNoRequest(t *testing.T) {
	fake := katafake.New(t)
	server, _, _ := newTestServer(t)
	server.configWatcher.Config().Integrations.Kata = config.KataIntegrationConfig{Endpoint: fake.URL(), DefaultProject: "reviews"}

	w := kataGet(t, server, "/api/kata/status", nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	status := decodeKata[KataStatusBody](t, w)
	assert.False(t, status.Enabled)
	assert.Equal(t, "disabled", status.State)
	assert.Empty(t, fake.AllRequests())
}

func TestKataStatusReportsTheConnection(t *testing.T) {
	server, fake := kataAPIServer(t)
	repo := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(repo, ".kata.local.toml"), []byte("[project]\nname = \"other\"\n"), 0o600))

	w := kataGet(t, server, "/api/kata/status", url.Values{"repo": {repo}})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	status := decodeKata[KataStatusBody](t, w)
	assert.True(t, status.Enabled)
	assert.Equal(t, "ready", status.State, status.Message)
	assert.Equal(t, "other", status.Project, "the repository's binding is the project in effect")
	assert.Equal(t, "reviews", status.DefaultProject)
	assert.Equal(t, "roborev", status.Teammate)
	assert.Equal(t, "v0.19.0", status.ServerVersion)
	assert.Equal(t, "0.28.0", status.APISchemaVersion)
	assert.True(t, status.SupportsReplies)
	assert.NotContains(t, w.Body.String(), kataTestCredential)
	assert.NotContains(t, w.Body.String(), fake.URL(), "the endpoint stays on the daemon")

	w = kataGet(t, server, "/api/kata/status", url.Values{"repo": {t.TempDir()}})
	assert.Equal(t, "reviews", decodeKata[KataStatusBody](t, w).Project, "an unbound repository uses default_project")

	w = kataGet(t, server, "/api/kata/status", url.Values{"project": {"ghost"}})
	status = decodeKata[KataStatusBody](t, w)
	assert.Equal(t, "project_not_found", status.State)
	assert.Equal(t, "ghost", status.Project)
}

func TestKataStatusStates(t *testing.T) {
	t.Run("old server", func(t *testing.T) {
		server, fake := kataAPIServer(t)
		fake.APISchema = "0.27.0"
		status := decodeKata[KataStatusBody](t, kataGet(t, server, "/api/kata/status", nil))
		assert.Equal(t, "ready", status.State)
		assert.False(t, status.SupportsReplies)
		fake.APISchema = "0.20.0"
		assert.Equal(t, "incompatible", decodeKata[KataStatusBody](t, kataGet(t, server, "/api/kata/status", nil)).State)
	})
	t.Run("credential rejected", func(t *testing.T) {
		server, fake := kataAPIServer(t)
		fake.Token = "a-different-credential"
		assert.Equal(t, "authentication_required", decodeKata[KataStatusBody](t, kataGet(t, server, "/api/kata/status", nil)).State)
	})
	t.Run("unreachable", func(t *testing.T) {
		server, _ := kataAPIServer(t)
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		addr := listener.Addr().String()
		require.NoError(t, listener.Close())
		server.configWatcher.Config().Integrations.Kata.Endpoint = "http://" + addr
		assert.Equal(t, "unreachable", decodeKata[KataStatusBody](t, kataGet(t, server, "/api/kata/status", nil)).State)
	})
	t.Run("misconfigured", func(t *testing.T) {
		server, _ := kataAPIServer(t)
		server.configWatcher.Config().Integrations.Kata.Endpoint = "http://kata.example.com"
		status := decodeKata[KataStatusBody](t, kataGet(t, server, "/api/kata/status", nil))
		assert.Equal(t, "incompatible", status.State)
		assert.True(t, status.Enabled)
	})
}

func TestKataIssuesListsTheDefaultProjectForTheCaller(t *testing.T) {
	marker := kataOnPathMarker(t)
	server, fake := kataAPIServer(t)
	fake.AddIssue(7, "aaa1", "In reviews", "body", "bug")
	fake.AddIssue(8, "bbb2", "In other", "")
	closed := fake.AddIssue(7, "ccc3", "Closed", "")
	closed.Out.Status = "closed"
	// The daemon's own directory carries a binding that must not be consulted.
	stray := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(stray, ".kata.toml"), []byte("[project]\nname = \"other\"\n"), 0o600))
	t.Chdir(stray)

	w := kataGet(t, server, "/api/kata/issues", nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	out := decodeKata[struct{ Issues []KataIssue }](t, w)
	require.Len(t, out.Issues, 1)
	assert.Equal(t, "aaa1", out.Issues[0].ShortID)
	assert.Equal(t, []string{"bug"}, out.Issues[0].Labels)

	w = kataGet(t, server, "/api/kata/issues", url.Values{"project": {"other"}, "status": {"all"}})
	out = decodeKata[struct{ Issues []KataIssue }](t, w)
	require.Len(t, out.Issues, 1)
	assert.Equal(t, "bbb2", out.Issues[0].ShortID)

	w = kataGet(t, server, "/api/kata/issues", url.Values{"unlimited": {"true"}, "status": {"closed"}})
	out = decodeKata[struct{ Issues []KataIssue }](t, w)
	require.Len(t, out.Issues, 1)
	assert.Equal(t, "ccc3", out.Issues[0].ShortID)
	assert.NoFileExists(t, marker)
}

func TestKataIssuesRejectsBadInput(t *testing.T) {
	server, fake := kataAPIServer(t)
	for name, tc := range map[string]struct {
		query url.Values
		want  int
	}{
		"status":  {url.Values{"status": {"stale"}}, http.StatusBadRequest},
		"project": {url.Values{"project": {"ghost"}}, http.StatusNotFound},
	} {
		w := kataGet(t, server, "/api/kata/issues", tc.query)
		assert.Equal(t, tc.want, w.Code, "%s: %s", name, w.Body.String())
	}
	for _, r := range fake.Requests() {
		assert.NotContains(t, r.Path, "/issues", "a rejected request reads nothing: %s", r.Path)
	}
}

func TestKataIssueShowsOneIssue(t *testing.T) {
	server, fake := kataAPIServer(t)
	fake.AddIssue(7, "aaa1", "In reviews", "the body", "bug")
	fake.AddIssue(8, "bbb2", "In other", "")

	w := kataGet(t, server, "/api/kata/issue", url.Values{"ref": {"aaa1"}})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	issue := decodeKata[KataIssue](t, w)
	assert.Equal(t, "reviews#aaa1", issue.QualifiedID)
	assert.Equal(t, "the body", issue.Body)

	issue = decodeKata[KataIssue](t, kataGet(t, server, "/api/kata/issue", url.Values{"ref": {"bbb2"}, "project": {"other"}}))
	assert.Equal(t, "other#bbb2", issue.QualifiedID)

	issue = decodeKata[KataIssue](t, kataGet(t, server, "/api/kata/issue", url.Values{"ref": {"other#bbb2"}}))
	assert.Equal(t, "other#bbb2", issue.QualifiedID, "a qualified ref selects its own project")

	assert.Equal(t, http.StatusNotFound, kataGet(t, server, "/api/kata/issue", url.Values{"ref": {"zzzz"}}).Code)
	for _, ref := range []string{"a/b", "../x", "other#", "#abc4"} {
		assert.Equal(t, http.StatusBadRequest, kataGet(t, server, "/api/kata/issue", url.Values{"ref": {ref}}).Code, ref)
	}
	assert.Equal(t, http.StatusUnprocessableEntity, kataGet(t, server, "/api/kata/issue", nil).Code, "ref is required")
}

func TestKataCreateFilesThroughTheDaemonCredential(t *testing.T) {
	server, fake := kataAPIServer(t)
	priority := 2
	req := KataCreateRequest{
		Title: "Review failed", Body: "details", Project: "other", Labels: []string{"roborev"},
		Priority: &priority, IdempotencyKey: "roborev:job:1",
	}
	w := kataPost(t, server, "/api/kata/issues", req)
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	created := decodeKata[KataCreateResponse](t, w)
	assert.NotEmpty(t, created.ShortID)
	assert.False(t, created.Reused)
	assert.Equal(t, "roborev", created.Author)

	var post katafake.Request
	for _, r := range fake.Requests() {
		if r.Method == http.MethodPost {
			post = r
		}
	}
	assert.Equal(t, "/api/v1/projects/8/issues", post.Path)
	assert.Equal(t, "roborev", post.JSON(t)["actor"])
	assert.Equal(t, "Bearer "+kataTestCredential, post.Header.Get("Authorization"))
	assert.Equal(t, "roborev:job:1", post.Header.Get("Idempotency-Key"))

	again := decodeKata[KataCreateResponse](t, kataPost(t, server, "/api/kata/issues", req))
	assert.True(t, again.Reused)
	assert.Equal(t, created.ShortID, again.ShortID)

	w = kataPost(t, server, "/api/kata/issues", KataCreateRequest{Body: "no title"})
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestKataEndpointsRefuseWhenTheConnectionIsOffOrBroken(t *testing.T) {
	marker := kataOnPathMarker(t)
	server, _, _ := newTestServer(t)
	for _, call := range []func() *httptest.ResponseRecorder{
		func() *httptest.ResponseRecorder { return kataGet(t, server, "/api/kata/issues", nil) },
		func() *httptest.ResponseRecorder {
			return kataGet(t, server, "/api/kata/issue", url.Values{"ref": {"abc4"}})
		},
		func() *httptest.ResponseRecorder {
			return kataPost(t, server, "/api/kata/issues", KataCreateRequest{Title: "t"})
		},
	} {
		w := call()
		assert.Equal(t, http.StatusConflict, w.Code, "disabled: %s", w.Body.String())
	}

	server.configWatcher.Config().Integrations.Kata = config.KataIntegrationConfig{Enabled: true, Endpoint: "ftp://kata.example.com", APIKey: kataTestCredential}
	for _, call := range []func() *httptest.ResponseRecorder{
		func() *httptest.ResponseRecorder { return kataGet(t, server, "/api/kata/issues", nil) },
		func() *httptest.ResponseRecorder {
			return kataGet(t, server, "/api/kata/issue", url.Values{"ref": {"abc4"}})
		},
		func() *httptest.ResponseRecorder {
			return kataPost(t, server, "/api/kata/issues", KataCreateRequest{Title: "t"})
		},
	} {
		w := call()
		assert.Equal(t, http.StatusBadGateway, w.Code, "broken: %s", w.Body.String())
		assert.NotContains(t, w.Body.String(), kataTestCredential)
	}
	assert.NoFileExists(t, marker, "the daemon never falls back to the kata CLI")
}

func TestKataEndpointsMapKataFailures(t *testing.T) {
	server, fake := kataAPIServer(t)
	fake.Token = "another-credential"
	w := kataGet(t, server, "/api/kata/issues", nil)
	assert.Equal(t, http.StatusBadGateway, w.Code, w.Body.String())
	assert.NotContains(t, w.Body.String(), kataTestCredential)
	assert.NotContains(t, w.Body.String(), "another-credential")

	fake.Token = ""
	fake.Override = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/api/v1/health" {
			return false
		}
		fake.Fail(w, http.StatusConflict, "idempotency_mismatch", "key reused")
		return true
	}
	w = kataPost(t, server, "/api/kata/issues", KataCreateRequest{Title: "t", Project: "reviews", IdempotencyKey: "k"})
	assert.Equal(t, http.StatusConflict, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "idempotency_mismatch", "Kata's error code survives the proxy")
}
