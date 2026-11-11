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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	katagen "go.kenn.io/kata/pkg/client/generated"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/kata"
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
	var reads int
	for _, request := range fake.AllRequests() {
		if request.Method == http.MethodGet && request.Path == "/api/v1/projects/7/issues/aaa1" {
			reads++
		}
	}
	assert.Equal(t, 1, reads)

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
	issue.Comments = []katagen.CommentOut{{UID: "root-uid", Handle: new("c:root"), Body: "root text", Author: "actual-author"}, {UID: "reply-uid", Handle: new("c:reply"), Body: "reply text", Author: "responder", ReplyToUID: new("root-uid"), ReplyKind: new("reply")}}
	w := kataGet(t, server, "/api/kata/issue", url.Values{"ref": {"abc1"}})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	got := decodeKata[KataIssue](t, w)
	assert.Equal(t, issue.Out.UID, got.UID)
	assert.NotEmpty(t, got.ProjectUID)
	require.NotNil(t, got.CommentsComplete)
	assert.True(t, *got.CommentsComplete)
	require.Len(t, got.Comments, 2)
	assert.Equal(t, "actual-author", got.Comments[0].Author)
	assert.Equal(t, "reply text", got.Comments[1].Body)
	assert.Equal(t, "responder", got.Comments[1].Author)
	require.NotNil(t, got.Comments[1].ReplyToUID)
	assert.Equal(t, "root-uid", *got.Comments[1].ReplyToUID)
	require.NotNil(t, got.Comments[1].ReplyKind)
	assert.Equal(t, "reply", *got.Comments[1].ReplyKind)
}

func TestKataIssueHugeUntrustedFindingCount(t *testing.T) {
	server, fake := kataAPIServer(t)
	issue := fake.AddIssue(7, "large-count", "title", "body")
	count := 1 << 30
	issue.Out.Metadata = map[string]any{"roborev.version": 1, "roborev.review_uuid": testUUID("remote-review").String(), "roborev.document_hash": strings.Repeat("a", 64), "roborev.policy": "comments", "roborev.completed_at": time.Now().UTC().Format(time.RFC3339Nano), "roborev.finding_count": count}
	w := kataGet(t, server, "/api/kata/issue", url.Values{"ref": {"large-count"}})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	got := decodeKata[KataIssue](t, w)
	require.NotNil(t, got.Envelope)
	assert.Equal(t, count, got.Envelope.FindingCount)
	require.Len(t, got.Roots, 1)
	assert.Equal(t, "missing", got.Roots[0].State)
	require.NotNil(t, got.Roots[0].EndPosition)
	assert.Equal(t, count-1, *got.Roots[0].EndPosition)
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
	// A replacement can carry a valid digest of its own body without matching
	// the original review. Its identity and attribution remain visible.
	replacement := "Finding 1 from a different review."
	replacementDigest := sha256.Sum256([]byte(replacement))
	issue.Comments[0].UID = "replacement-uid"
	issue.Comments[0].Body = fmt.Sprintf("%s\n\n<!-- roborev:v1 review=%s position=0 digest=%x -->", replacement, record.ReviewUUID, replacementDigest)
	w = kataGet(t, server, "/api/kata/publication", url.Values{"job_id": {fmt.Sprint(job.ID)}})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	got = decodeKata[KataPublicationBody](t, w)
	assert.Equal(t, "partial", got.State)
	require.Len(t, got.Roots, 1)
	assert.Equal(t, "edited", got.Roots[0].State)
	assert.Equal(t, "replacement-uid", got.Roots[0].Comment.UID)
	assert.Equal(t, "service", got.Roots[0].Comment.Author)
	issue.Comments[0].UID = "root-uid"
	issue.Comments[0].Body = marker
	issue.Out.Metadata["roborev.completed_at"] = review.CreatedAt.In(time.FixedZone("offset", 3600)).Format(time.RFC3339Nano)
	w = kataGet(t, server, "/api/kata/publication", url.Values{"job_id": {fmt.Sprint(job.ID)}})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, "complete", decodeKata[KataPublicationBody](t, w).State)
	issue.Out.Metadata["roborev.version"] = 2
	w = kataGet(t, server, "/api/kata/publication", url.Values{"job_id": {fmt.Sprint(job.ID)}})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, "ambiguous", decodeKata[KataPublicationBody](t, w).State)
	issue.Out.Metadata["roborev.version"] = 1
	// An explicit hook project wins over the default for an unmarked reader.
	other := fake.AddIssue(8, "other-issue", "findings", "body", "roborev")
	other.Out.Metadata = issue.Out.Metadata
	other.Comments = issue.Comments
	cfg.Hooks = []config.HookConfig{{Type: "kata", Event: "review.completed", Findings: "summary", Project: "other"}}
	w = kataGet(t, server, "/api/kata/publication", url.Values{"job_id": {fmt.Sprint(job.ID)}})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	got = decodeKata[KataPublicationBody](t, w)
	assert.Equal(t, "other", got.Project)
	assert.Equal(t, "complete", got.State)

	// Expired manual create still performs read-only discovery, even with no hook.
	cfg.Hooks = nil
	completed := time.Now().UTC().Add(-8 * 24 * time.Hour).Truncate(time.Second)
	_, err = tc.DB.ExecContext(t.Context(), `UPDATE reviews SET created_at = ? WHERE job_id = ?`, completed.Format(time.RFC3339), job.ID)
	require.NoError(t, err)
	issue.Out.Metadata["roborev.completed_at"] = completed.Format(time.RFC3339Nano)
	before := len(fake.AllRequests())
	expired := kataPost(t, server, "/api/kata/publication", KataPublicationRequest{JobID: job.ID})
	require.Equal(t, http.StatusOK, expired.Code, expired.Body.String())
	expiredBody := decodeKata[KataPublicationBody](t, expired)
	assert.True(t, expiredBody.Expired)
	assert.Equal(t, "complete", expiredBody.State)
	assert.Contains(t, expiredBody.Message, "expired")
	for _, request := range fake.AllRequests()[before:] {
		assert.NotEqual(t, http.MethodPost, request.Method)
	}
	after, err := tc.DB.GetKataPublication(t.Context(), job.ID)
	require.NoError(t, err)
	assert.Nil(t, after.Marker)
	for _, req := range fake.AllRequests() {
		assert.NotEqual(t, http.MethodPost, req.Method)
	}
}

func TestKataInspectionReadsOversizedUnmarkedPublication(t *testing.T) {
	fake := katafake.New(t)
	tc := newWorkerTestContext(t, 1)
	job := tc.createAndClaimJob(t, "large-review", "worker")
	finding := structuredreview.Finding{Severity: "high", Problem: strings.Repeat("x", kata.PublicationMaxRequestBytes), Fix: "fix"}
	doc := structuredreview.Document{SchemaVersion: structuredreview.SchemaVersion, Summary: "summary", Verdict: structuredreview.VerdictFail, Findings: []structuredreview.Finding{finding}}
	raw, err := json.Marshal(doc)
	require.NoError(t, err)
	require.NoError(t, tc.DB.CompleteJobResult(job.ID, "test", storage.ReviewCompletion{StructuredOutput: raw, Verdict: storage.VerdictFail}))
	review, err := tc.DB.GetReviewByJobID(job.ID)
	require.NoError(t, err)
	record, err := tc.DB.GetKataPublication(t.Context(), job.ID)
	require.NoError(t, err)
	issue := fake.AddIssue(7, "large", "findings", "body")
	issue.Out.Metadata = map[string]any{"roborev.version": 1, "roborev.review_uuid": record.ReviewUUID.String(), "roborev.document_hash": record.DocumentHash, "roborev.policy": "comments", "roborev.completed_at": review.CreatedAt.UTC().Format(time.RFC3339Nano), "roborev.finding_count": 1}
	text := (structuredreview.Document{SchemaVersion: structuredreview.SchemaVersion, Summary: "Review finding.", Findings: []structuredreview.Finding{finding}}).MarkdownFrom("", 1)
	digest := sha256.Sum256([]byte(text))
	body := fmt.Sprintf("%s\n\n<!-- roborev:v1 review=%s position=0 digest=%x -->", text, record.ReviewUUID, digest)
	issue.Comments = []katagen.CommentOut{{UID: "root-uid", Handle: new("c:root"), Body: body, Author: "service"}}
	server := newServerWithLogs(tc.DB, kataConnectionConfig(fake), "", newTestErrorLog(), newTestActivityLog())
	t.Cleanup(func() { _ = server.Close() })
	w := kataGet(t, server, "/api/kata/publication", url.Values{"job_id": {fmt.Sprint(job.ID)}})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	got := decodeKata[KataPublicationBody](t, w)
	assert.Equal(t, "complete", got.State)
	_, err = kata.PreparePublicationWithOptions(record.ReviewUUID, record.DocumentHash, review.CreatedAt, doc, "comments", "reviewer", kata.PublicationOptions{})
	require.ErrorIs(t, err, kata.ErrRequestRejected)
	after, err := tc.DB.GetKataPublication(t.Context(), job.ID)
	require.NoError(t, err)
	assert.Nil(t, after.Marker)
	for _, request := range fake.AllRequests() {
		assert.NotEqual(t, http.MethodPost, request.Method)
	}
}

func TestKataResponseRejectsInvalidRequestsBeforeWriting(t *testing.T) {
	server, fake := kataAPIServer(t)
	for _, request := range []KataResponseRequest{
		{},
		{Project: "reviews", DestinationID: "wrong", IssueUID: "issue-uid", TargetUID: "target-uid", Kind: "reply", Body: "text", IdempotencyKey: "key"},
		{Project: "reviews", DestinationID: "wrong", IssueUID: "issue-uid", TargetUID: "target-uid", Kind: "confirm", Body: "short", IdempotencyKey: "key"},
		{Project: "reviews", DestinationID: "wrong", IssueUID: "issue-uid", TargetUID: "target-uid", Kind: "reply", Body: "text", IdempotencyKey: "key", Generation: 10},
	} {
		w := kataPost(t, server, "/api/kata/response", request)
		assert.NotEqual(t, http.StatusOK, w.Code)
	}
	for _, request := range fake.AllRequests() {
		assert.NotEqual(t, http.MethodPost, request.Method)
	}
}

func TestKataResponseWritesSelectedReplyAndReadsItBack(t *testing.T) {
	server, fake := kataAPIServer(t)
	issue := fake.AddIssue(8, "a123", "findings", "body")
	reviewID := "11111111-1111-4111-8111-111111111111"
	issue.Out.Metadata = map[string]any{"roborev.version": 1, "roborev.review_uuid": reviewID, "roborev.document_hash": strings.Repeat("a", 64), "roborev.policy": "summary", "roborev.completed_at": "2026-01-02T03:04:05Z", "roborev.finding_count": 1}
	text := "Finding 1 from a roborev review."
	digest := sha256.Sum256([]byte(text))
	root := katagen.CommentOut{UID: "01AAAAAAAAAAAAAAAAAAABC123", IssueID: issue.Out.ID, Handle: new("c:abc123"), Body: fmt.Sprintf("%s\n\n<!-- roborev:v1 review=%s position=0 digest=%x -->", text, reviewID, digest), Author: "service"}
	prior := katagen.CommentOut{UID: "01BBBBBBBBBBBBBBBBBBDEF456", IssueID: issue.Out.ID, Handle: new("c:def456"), Body: "earlier reply", Author: "teammate", ReplyToUID: &root.UID, ReplyKind: new("reply")}
	issue.Comments = []katagen.CommentOut{root, prior}
	var request katagen.CommentRequestBody
	var requestErr error
	var requestKey string
	var rejectValidation bool
	fake.Override = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodPost {
			return false
		}
		requestErr = json.NewDecoder(r.Body).Decode(&request)
		requestKey = r.Header.Get("Idempotency-Key")
		if requestErr != nil || request.ReplyTo == nil || request.Kind == nil || request.Teammate == nil {
			w.WriteHeader(http.StatusBadRequest)
			return true
		}
		if rejectValidation {
			fake.Fail(w, http.StatusBadRequest, "validation", "synthetic evidence error")
			return true
		}
		saved := katagen.CommentOut{UID: "01CCCCCCCCCCCCCCCCCC789012", IssueID: issue.Out.ID, Handle: new("c:789012"), Body: request.Body, Author: "actual-author", Teammate: request.Teammate, ReplyToUID: request.ReplyTo, ReplyKind: new(string(*request.Kind))}
		issue.Comments = append(issue.Comments, saved)
		fake.Reply(w, map[string]any{"changed": true, "issue": map[string]any{"id": issue.Out.ID, "uid": issue.Out.UID, "project_id": 8}, "comment": map[string]any{"uid": saved.UID, "issue_id": issue.Out.ID, "body": saved.Body, "author": saved.Author, "teammate": *saved.Teammate, "reply_to_uid": *saved.ReplyToUID, "reply_kind": *saved.ReplyKind}})
		return true
	}
	read := kataGet(t, server, "/api/kata/issue", url.Values{"ref": {issue.Out.UID}, "project": {"other"}})
	require.Equal(t, http.StatusOK, read.Code, read.Body.String())
	full := decodeKata[KataIssue](t, read)
	rejectValidation = true
	beforeInvalid := len(fake.AllRequests())
	invalid := kataPost(t, server, "/api/kata/response", KataResponseRequest{Project: full.Project, DestinationID: full.DestinationID, Generation: full.Generation, Teammate: full.ReplyTeammate, IssueUID: full.UID, TargetUID: prior.UID, Kind: "confirm", Body: "short", IdempotencyKey: "Evidence-Key"})
	assert.Equal(t, http.StatusBadRequest, invalid.Code)
	assert.Contains(t, invalid.Body.String(), "validation")
	assert.Contains(t, invalid.Body.String(), "synthetic evidence error")
	assert.Equal(t, "short", request.Body)
	require.NotNil(t, request.Kind)
	assert.Equal(t, katagen.Confirm, *request.Kind)
	assert.Equal(t, "Evidence-Key", requestKey)
	assert.Nil(t, request.Force)
	var invalidPosts int
	for _, sent := range fake.AllRequests()[beforeInvalid:] {
		if sent.Method == http.MethodPost {
			invalidPosts++
		}
	}
	assert.Equal(t, 1, invalidPosts)
	assert.Len(t, issue.Comments, 2)
	rejectValidation = false
	body := strings.Repeat("e", 40)
	posted := kataPost(t, server, "/api/kata/response", KataResponseRequest{Project: full.Project, DestinationID: full.DestinationID, Generation: full.Generation, Teammate: full.ReplyTeammate, IssueUID: full.UID, TargetUID: prior.UID, Kind: "refute", Body: body, IdempotencyKey: "Exact-Key"})
	require.Equal(t, http.StatusOK, posted.Code, posted.Body.String())
	receipt := decodeKata[KataReplyReceipt](t, posted)
	require.NoError(t, requestErr)
	require.NotNil(t, request.ReplyTo)
	require.NotNil(t, request.Kind)
	assert.Equal(t, prior.UID, *request.ReplyTo)
	assert.Equal(t, katagen.Refute, *request.Kind)
	assert.Equal(t, "Exact-Key", requestKey)
	assert.Nil(t, request.Force)
	assert.Equal(t, "actual-author", receipt.Author)
	assert.Equal(t, "roborev", receipt.Teammate)
	assert.True(t, receipt.Changed)
	read = kataGet(t, server, "/api/kata/issue", url.Values{"ref": {issue.Out.UID}, "project": {"other"}})
	require.Equal(t, http.StatusOK, read.Code, read.Body.String())
	full = decodeKata[KataIssue](t, read)
	require.Len(t, full.Comments, 3)
	assert.Equal(t, body, full.Comments[2].Body)
	assert.Equal(t, prior.UID, *full.Comments[2].ReplyToUID)
	// The successful response may have been lost. A fresh read after a teammate
	// change must not turn the same key into a different producer request.
	server.configWatcher.cfgMu.Lock()
	server.configWatcher.cfg.Integrations.Kata.Teammate = "new-teammate"
	server.configWatcher.reloadCounter++
	server.configWatcher.cfgMu.Unlock()
	read = kataGet(t, server, "/api/kata/issue", url.Values{"ref": {issue.Out.UID}, "project": {"other"}})
	require.Equal(t, http.StatusOK, read.Code, read.Body.String())
	updated := decodeKata[KataIssue](t, read)
	beforeChangeRetry := len(fake.AllRequests())
	retry := kataPost(t, server, "/api/kata/response", KataResponseRequest{Project: updated.Project, DestinationID: updated.DestinationID, Generation: updated.Generation, Teammate: full.ReplyTeammate, IssueUID: updated.UID, TargetUID: prior.UID, Kind: "refute", Body: body, IdempotencyKey: "Exact-Key"})
	assert.Equal(t, http.StatusConflict, retry.Code, retry.Body.String())
	assert.Len(t, fake.AllRequests(), beforeChangeRetry)
	server.configWatcher.cfgMu.Lock()
	server.configWatcher.cfg.Integrations.Kata.Teammate = "roborev"
	server.configWatcher.reloadCounter++
	server.configWatcher.cfgMu.Unlock()
	read = kataGet(t, server, "/api/kata/issue", url.Values{"ref": {issue.Out.UID}, "project": {"other"}})
	require.Equal(t, http.StatusOK, read.Code, read.Body.String())
	full = decodeKata[KataIssue](t, read)
	// A reload after the full read but before the final write invalidates the tuple.
	fake.Override = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/issues/") {
			server.configWatcher.cfgMu.Lock()
			server.configWatcher.reloadCounter++
			server.configWatcher.cfgMu.Unlock()
		}
		return false
	}
	before := len(fake.AllRequests())
	stale := kataPost(t, server, "/api/kata/response", KataResponseRequest{Project: full.Project, DestinationID: full.DestinationID, Generation: full.Generation, Teammate: full.ReplyTeammate, IssueUID: full.UID, TargetUID: prior.UID, Kind: "reply", Body: "another reply", IdempotencyKey: "other-key"})
	assert.Equal(t, http.StatusConflict, stale.Code, stale.Body.String())
	for _, request := range fake.AllRequests()[before:] {
		assert.NotEqual(t, http.MethodPost, request.Method)
	}
}

func TestKataResponseRejectsDestinationChangesAtBarrier(t *testing.T) {
	for _, scenario := range []string{"endpoint", "project UID", "reload", "disabled"} {
		t.Run(scenario, func(t *testing.T) {
			server, fake := kataAPIServer(t)
			issue := fake.AddIssue(7, "a123", "findings", "body")
			reviewID := "11111111-1111-4111-8111-111111111111"
			issue.Out.Metadata = map[string]any{"roborev.version": 1, "roborev.review_uuid": reviewID, "roborev.document_hash": strings.Repeat("a", 64), "roborev.policy": "summary", "roborev.completed_at": "2026-01-02T03:04:05Z", "roborev.finding_count": 1}
			text := "Finding 1 from a roborev review."
			digest := sha256.Sum256([]byte(text))
			target := "01AAAAAAAAAAAAAAAAAAABC123"
			issue.Comments = []katagen.CommentOut{{UID: target, IssueID: issue.Out.ID, Handle: new("c:abc123"), Body: fmt.Sprintf("%s\n\n<!-- roborev:v1 review=%s position=0 digest=%x -->", text, reviewID, digest), Author: "service"}}
			read := kataGet(t, server, "/api/kata/issue", url.Values{"ref": {issue.Out.UID}, "project": {"reviews"}})
			require.Equal(t, http.StatusOK, read.Code, read.Body.String())
			full := decodeKata[KataIssue](t, read)
			request := KataResponseRequest{Project: full.Project, DestinationID: full.DestinationID, Generation: full.Generation, Teammate: full.ReplyTeammate, IssueUID: full.UID, TargetUID: target, Kind: "reply", Body: "evidence", IdempotencyKey: "Exact-Key"}
			entered, resume := make(chan struct{}), make(chan struct{})
			var once sync.Once
			fake.Override = func(_ http.ResponseWriter, r *http.Request) bool {
				path := "/api/v1/projects/7/issues/" + issue.Out.UID
				if scenario == "project UID" {
					path = "/api/v1/projects"
				}
				if r.Method == http.MethodGet && r.URL.Path == path {
					once.Do(func() { close(entered); <-resume })
				}
				return false
			}
			w := httptest.NewRecorder()
			httpReq := testutil.MakeJSONRequest(t, http.MethodPost, "/api/kata/response", request)
			done := make(chan struct{})
			go func() { defer close(done); server.httpServer.Handler.ServeHTTP(w, httpReq) }()
			select {
			case <-entered:
			case <-done:
				close(resume)
				require.Equal(t, "barrier reached", "request completed before barrier", w.Body.String())
			case <-t.Context().Done():
				close(resume)
				<-done
				require.NoError(t, t.Context().Err())
			}
			if scenario == "project UID" {
				fake.Projects[0].UID = "P9999999999999999999999999"
			} else {
				server.configWatcher.cfgMu.Lock()
				switch scenario {
				case "endpoint":
					server.configWatcher.cfg.Integrations.Kata.Endpoint = "http://127.0.0.1:2"
				case "disabled":
					server.configWatcher.cfg.Integrations.Kata.Enabled = false
				}
				server.configWatcher.reloadCounter++
				server.configWatcher.cfgMu.Unlock()
			}
			close(resume)
			<-done
			assert.Equal(t, http.StatusConflict, w.Code, w.Body.String())
			for _, req := range fake.AllRequests() {
				assert.NotEqual(t, http.MethodPost, req.Method)
			}
		})
	}
}
