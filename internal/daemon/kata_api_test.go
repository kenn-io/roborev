package daemon

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	katagen "go.kenn.io/kata/pkg/client/generated"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/kata/katafake"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
	"go.kenn.io/roborev/pkg/structuredreview"
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
	server.httpServer.Handler.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, target, nil))
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
	w := kataGet(t, server, "/api/kata/status", url.Values{"project": {"other"}})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	status := decodeKata[KataStatusBody](t, w)
	assert.True(t, status.Enabled)
	assert.Equal(t, "ready", status.State, status.Message)
	assert.Equal(t, "other", status.Project, "the requested project is in effect")
	assert.Equal(t, "reviews", status.DefaultProject)
	assert.Equal(t, "roborev", status.Teammate)
	assert.Equal(t, "v0.19.0", status.ServerVersion)
	assert.Equal(t, "0.28.0", status.APISchemaVersion)
	assert.True(t, status.SupportsReplies)
	assert.NotContains(t, w.Body.String(), kataTestCredential)
	assert.NotContains(t, w.Body.String(), fake.URL(), "the endpoint stays on the daemon")

	w = kataGet(t, server, "/api/kata/status", nil)
	assert.Equal(t, "reviews", decodeKata[KataStatusBody](t, w).Project, "an unbound repository uses default_project")

	w = kataGet(t, server, "/api/kata/status", url.Values{"project": {"ghost"}})
	status = decodeKata[KataStatusBody](t, w)
	assert.Equal(t, "project_not_found", status.State)
	assert.Equal(t, "ghost", status.Project)
}

func TestKataStatusResolvesWorkspaceAlias(t *testing.T) {
	server, fake := kataAPIServer(t)
	const identity = "code.example/Team/Review-Repo"
	fake.Override = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/api/v1/projects/resolve" {
			return false
		}
		fake.Reply(w, katagen.ProjectResolveBody{
			Project: fake.Projects[1],
			Alias: katagen.ProjectAlias{
				AliasIdentity: identity, AliasKind: "git", ID: 1, ProjectID: fake.Projects[1].ID,
				CreatedAt: time.Unix(0, 0).UTC(),
			},
		})
		return true
	}

	query := url.Values{
		"workspace_alias_identity": {identity},
		"workspace_alias_kind":     {"git"},
	}
	w := kataGet(t, server, "/api/kata/status", query)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	status := decodeKata[KataStatusBody](t, w)
	assert.Equal(t, "ready", status.State, status.Message)
	assert.Equal(t, "other", status.Project, "the alias resolves to the canonical project name")

	requests := fake.Requests()
	require.Len(t, requests, 1)
	assert.Equal(t, http.MethodPost, requests[0].Method)
	assert.Equal(t, "/api/v1/projects/resolve", requests[0].Path)
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
		listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
		require.NoError(t, err)
		addr := listener.Addr().String()
		require.NoError(t, listener.Close())
		server.configWatcher.Config().Integrations.Kata.Endpoint = "http://" + addr
		assert.Equal(t, "unreachable", decodeKata[KataStatusBody](t, kataGet(t, server, "/api/kata/status", nil)).State)
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
	require.NoError(t, os.WriteFile(filepath.Join(stray, ".kata.toml"), []byte("version = 1\n[project]\nname = \"other\"\n"), 0o600))
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

func TestKataWorkspaceAliasCannotBeCombinedWithProjectName(t *testing.T) {
	server, fake := kataAPIServer(t)
	query := url.Values{
		"project":                  {"reviews"},
		"workspace_alias_identity": {"code.example/team/repo"},
		"workspace_alias_kind":     {"git"},
	}
	w := kataGet(t, server, "/api/kata/issues", query)
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())

	assert.Empty(t, fake.AllRequests(), "ambiguous selection never reaches Kata")
}

func TestKataEndpointsRefuseWhenTheConnectionIsOffOrBroken(t *testing.T) {
	marker := kataOnPathMarker(t)
	server, _, _ := newTestServer(t)
	for _, call := range []func() *httptest.ResponseRecorder{
		func() *httptest.ResponseRecorder { return kataGet(t, server, "/api/kata/issues", nil) },
		func() *httptest.ResponseRecorder {
			return kataGet(t, server, "/api/kata/issue", url.Values{"ref": {"abc4"}})
		},
	} {
		w := call()
		assert.Equal(t, http.StatusConflict, w.Code, "disabled: %s", w.Body.String())
	}

	credentialPath := filepath.Join(t.TempDir(), "missing")
	server.configWatcher.Config().Integrations.Kata = config.KataIntegrationConfig{Enabled: true, Endpoint: "https://kata.example.com", APIKeyFile: credentialPath}
	for _, call := range []func() *httptest.ResponseRecorder{
		func() *httptest.ResponseRecorder { return kataGet(t, server, "/api/kata/issues", nil) },
		func() *httptest.ResponseRecorder {
			return kataGet(t, server, "/api/kata/issue", url.Values{"ref": {"abc4"}})
		},
	} {
		w := call()
		assert.Equal(t, http.StatusBadGateway, w.Code, "broken: %s", w.Body.String())
		assert.NotContains(t, w.Body.String(), kataTestCredential)
		assert.NotContains(t, w.Body.String(), credentialPath)
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
}

func TestKataPublicationRoutesRejectMissingReview(t *testing.T) {
	server, _ := kataAPIServer(t)
	get := kataGet(t, server, "/api/kata/publication", url.Values{"job_id": {"12345"}})
	assert.Equal(t, http.StatusNotFound, get.Code)
	post := kataPost(t, server, "/api/kata/publication", KataPublicationRequest{JobID: 12345, Findings: "comments"})
	assert.Equal(t, http.StatusNotFound, post.Code)
	invalid := kataPost(t, server, "/api/kata/publication", KataPublicationRequest{JobID: 12345, Findings: "unknown"})
	assert.Equal(t, http.StatusBadRequest, invalid.Code)
}

func TestKataIssueFullCommentsAndAttribution(t *testing.T) {
	server, fake := kataAPIServer(t)
	issue := fake.AddIssue(7, "abc1", "title", "body")
	issue.Comments = []katagen.CommentOut{{UID: "root-uid", Handle: new("c:root"), Body: "root text", Author: "actual-author"}, {UID: "reply-uid", Handle: new("c:reply"), Body: "reply text", Author: "responder", ReplyToUID: new("root-uid"), ReplyKind: new("answer")}}
	w := kataGet(t, server, "/api/kata/issue", url.Values{"ref": {"abc1"}})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	got := decodeKata[KataIssue](t, w)
	assert.Equal(t, issue.Out.UID, got.UID)
	assert.NotEmpty(t, got.ProjectUID)
	require.NotNil(t, got.CommentsComplete)
	assert.Equal(t, true, *got.CommentsComplete)
	require.Len(t, got.Comments, 2)
	assert.Equal(t, "actual-author", got.Comments[0].Author)
	assert.Equal(t, "reply text", got.Comments[1].Body)
	assert.Equal(t, "root-uid", *got.Comments[1].ReplyToUID)
}

func TestKataPublicationDiscoversUnmarkedReview(t *testing.T) {
	fake := katafake.New(t)
	tc := newWorkerTestContext(t, 1)
	job := tc.createAndClaimJob(t, "synced-review", "worker")
	doc := structuredreview.Document{SchemaVersion: structuredreview.SchemaVersion, Summary: "summary", Verdict: structuredreview.VerdictFail, Findings: []structuredreview.Finding{{Severity: "high", Problem: "problem", Fix: "fix"}}}
	raw, err := json.Marshal(doc)
	require.NoError(t, err)
	require.NoError(t, tc.DB.CompleteJobResult(job.ID, "test", storage.ReviewCompletion{StructuredOutput: raw, Verdict: storage.VerdictFail}))
	review, err := tc.DB.GetReviewByJobID(job.ID)
	require.NoError(t, err)
	record, err := tc.DB.GetKataPublication(t.Context(), job.ID)
	require.NoError(t, err)
	require.Nil(t, record.Marker)
	issue := fake.AddIssue(7, "synthetic", "findings", "body", "roborev")
	issue.Out.Metadata = map[string]any{"roborev.version": 1, "roborev.review_uuid": record.ReviewUUID.String(), "roborev.document_hash": record.DocumentHash, "roborev.policy": "summary", "roborev.completed_at": review.CreatedAt.UTC().Format(time.RFC3339Nano), "roborev.finding_count": 1}
	text := "Finding 1 from a roborev review."
	digest := sha256.Sum256([]byte(text))
	marker := fmt.Sprintf("%s\n\n<!-- roborev:v1 review=%s position=0 digest=%x -->", text, record.ReviewUUID, digest)
	issue.Comments = []katagen.CommentOut{{UID: "root-uid", Handle: new("c:root"), Body: marker, Author: "service"}}
	cfg := kataConnectionConfig(fake)
	// A synced machine has no local publication hook or marker.
	server := newServerWithLogs(tc.DB, cfg, "", newTestErrorLog(), newTestActivityLog())
	t.Cleanup(func() { _ = server.Close() })
	w := kataGet(t, server, "/api/kata/publication", url.Values{"job_id": {fmt.Sprint(job.ID)}})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	got := decodeKata[KataPublicationBody](t, w)
	assert.Equal(t, "complete", got.State)
	assert.Equal(t, "summary", got.Policy)
	require.Len(t, got.Roots, 1)
	assert.Equal(t, "service", got.Roots[0].Comment.Author)
	after, err := tc.DB.GetKataPublication(t.Context(), job.ID)
	require.NoError(t, err)
	assert.Nil(t, after.Marker)
	for _, req := range fake.AllRequests() {
		assert.NotEqual(t, http.MethodPost, req.Method)
	}
}
