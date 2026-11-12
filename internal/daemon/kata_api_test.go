package daemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/danielgtaylor/huma/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	katagen "go.kenn.io/kata/pkg/client/generated"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/kata"
	"go.kenn.io/roborev/internal/kata/katafake"
	"go.kenn.io/roborev/internal/mcpserver"
	"go.kenn.io/roborev/internal/prompt"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/internal/testutil"
	apiclient "go.kenn.io/roborev/pkg/client"
	"go.kenn.io/roborev/pkg/client/generated"
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
	local := fake.AddIssue(7, "aaa1", "In reviews", "the body", "bug")
	fake.AddIssue(8, "bbb2", "In other", "")
	when := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	link := katagen.CommentLink{UID: new("foreign-reply"), Handle: new("bbb2:abcdef"), Kind: "refute", Author: new("other-author"), Teammate: new("other-teammate"), IssueUID: new("other-issue"), IssueShortID: new("bbb2"), ProjectID: new(int64(8)), ProjectUID: new("other-project"), Body: new("foreign evidence"), CreatedAt: &when, EditedAt: &when, TargetEdited: new(true)}
	pending := katagen.CommentLink{UID: new("missing-target"), Kind: "reply", Status: new("pending")}
	local.Comments = []katagen.CommentOut{{UID: "local-root", IssueID: local.Out.ID, Author: "finder", Body: "finding", Backlinks: []katagen.CommentLink{link}, BacklinksTruncated: new(true)}, {UID: "local-reply", IssueID: local.Out.ID, Author: "reviewer", Body: "reply", ReplyToUID: new("missing-target"), ReplyKind: new("reply"), Reply: &pending}}

	w := kataGet(t, server, "/api/kata/issue", url.Values{"ref": {"aaa1"}})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	issue := decodeKata[KataIssue](t, w)
	assert.Equal(t, "reviews#aaa1", issue.QualifiedID)
	assert.Equal(t, "the body", issue.Body)
	wire := decodeKata[struct {
		Comments []katagen.CommentOut `json:"comments"`
	}](t, w)
	require.Len(t, wire.Comments, 2)
	assert.Equal(t, "local-root", wire.Comments[0].UID)
	assert.Equal(t, []katagen.CommentLink{link}, wire.Comments[0].Backlinks)
	assert.Equal(t, new(true), wire.Comments[0].BacklinksTruncated)
	assert.Equal(t, &pending, wire.Comments[1].Reply)
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
	link := katagen.CommentLink{UID: new("linked-reply"), Kind: "reply", Author: new("linked-author"), Teammate: new("linked-team"), IssueUID: new("linked-issue"), IssueShortID: new("other1"), ProjectUID: new("linked-project"), ProjectID: new(int64(8)), Body: new("cross-issue evidence"), CreatedAt: new(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))}
	issue.Comments = []katagen.CommentOut{{UID: "root-uid", IssueID: issue.Out.ID, Handle: new("c:root"), Body: marker, Author: "service", Backlinks: []katagen.CommentLink{link}, BacklinksTruncated: new(true)}}
	cfg := kataConnectionConfig(fake)
	// A synced machine has no local publication hook or marker.
	server := newServerWithLogs(tc.DB, cfg, "", newTestErrorLog(), newTestActivityLog())
	t.Cleanup(func() { _ = server.Close() })
	w := kataGet(t, server, "/api/kata/publication", url.Values{"job_id": {fmt.Sprint(job.ID)}})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	got := decodeKata[KataPublicationBody](t, w)
	assert.Equal(t, "complete", got.State)
	assert.Equal(t, "summary", got.Policy)
	publicationWire := decodeKata[katagen.ShowIssueResponseBody](t, w)
	require.Len(t, publicationWire.Comments, 1)
	assert.Equal(t, []katagen.CommentLink{link}, publicationWire.Comments[0].Backlinks)
	assert.Equal(t, new(true), publicationWire.Comments[0].BacklinksTruncated)
	require.Len(t, got.Roots, 1)
	assert.Equal(t, "service", got.Roots[0].Comment.Author)
	byUUID := kataGet(t, server, "/api/kata/publication", url.Values{"review_uuid": {record.ReviewUUID.String()}})
	require.Equal(t, http.StatusOK, byUUID.Code, byUUID.Body.String())
	assert.Equal(t, got, decodeKata[KataPublicationBody](t, byUUID))
	explicit := kataGet(t, server, "/api/kata/publication", url.Values{"issue": {issue.Out.UID}, "project": {"reviews"}})
	require.Equal(t, http.StatusOK, explicit.Code, explicit.Body.String())
	explicitBody := decodeKata[KataPublicationBody](t, explicit)
	assert.Equal(t, "unverified", explicitBody.State)
	assert.Equal(t, review.StructuredOutput, explicitBody.SourceDocument)
	// A replacement can carry a valid digest of its own body without matching
	// the original review. Its identity and attribution remain visible.
	replacement := "Finding 1 from a different review."
	replacementDigest := sha256.Sum256([]byte(replacement))
	issue.Comments[0].UID = "replacement-uid"
	issue.Comments[0].Body = fmt.Sprintf("%s\n\n<!-- roborev:v1 review=%s position=0 digest=%x -->", replacement, record.ReviewUUID, replacementDigest)
	issue.Comments = append(issue.Comments, katagen.CommentOut{UID: "descendant", IssueID: issue.Out.ID, ReplyToUID: new("replacement-uid"), Body: "claim"})
	w = kataGet(t, server, "/api/kata/publication", url.Values{"job_id": {fmt.Sprint(job.ID)}})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	got = decodeKata[KataPublicationBody](t, w)
	assert.Equal(t, "partial", got.State)
	require.Len(t, got.Roots, 1)
	assert.Equal(t, "edited", got.Roots[0].State)
	assert.Equal(t, "replacement-uid", got.Roots[0].Comment.UID)
	assert.Equal(t, "service", got.Roots[0].Comment.Author)
	require.Len(t, got.Comments, 2)
	assert.Equal(t, "unmapped", got.Comments[1].Lineage)
	issue.Comments = issue.Comments[:1]
	issue.Comments[0].UID = "root-uid"
	issue.Comments[0].Body = marker
	issue.Out.Metadata["roborev.completed_at"] = review.CreatedAt.In(time.FixedZone("offset", 3600)).Format(time.RFC3339Nano)
	w = kataGet(t, server, "/api/kata/publication", url.Values{"job_id": {fmt.Sprint(job.ID)}})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, "complete", decodeKata[KataPublicationBody](t, w).State)
	issue.Out.Metadata["roborev.version"] = 2
	w = kataGet(t, server, "/api/kata/publication", url.Values{"job_id": {fmt.Sprint(job.ID)}})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	invalid := decodeKata[KataPublicationBody](t, w)
	assert.Equal(t, "ambiguous", invalid.State)
	require.NotNil(t, invalid.Issue)
	assert.Equal(t, marker, invalid.Comments[0].Body)
	issue.Out.Metadata["roborev.version"] = 1
	// An explicit hook project wins over the default for an unmarked reader.
	other := fake.AddIssue(8, "other-issue", "findings", "body", "roborev")
	other.Out.Metadata = issue.Out.Metadata
	other.Comments = append([]katagen.CommentOut(nil), issue.Comments...)
	other.Comments[0].IssueID = other.Out.ID
	cfg.Hooks = []config.HookConfig{{Type: "kata", Event: "review.completed", Findings: "summary", Project: "other"}}
	w = kataGet(t, server, "/api/kata/publication", url.Values{"job_id": {fmt.Sprint(job.ID)}})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	got = decodeKata[KataPublicationBody](t, w)
	assert.Equal(t, "other", got.Project)
	assert.Equal(t, "complete", got.State)
	for _, selector := range []url.Values{{"job_id": {fmt.Sprint(job.ID)}, "project": {"reviews"}}, {"review_uuid": {record.ReviewUUID.String()}, "project": {"reviews"}}} {
		before := len(fake.AllRequests())
		w = kataGet(t, server, "/api/kata/publication", selector)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		assert.Equal(t, "reviews", decodeKata[KataPublicationBody](t, w).Project)
		for _, request := range fake.AllRequests()[before:] {
			assert.NotEqual(t, http.MethodPost, request.Method)
			assert.NotContains(t, request.Path, "/projects/8/issues")
		}
	}
	w = kataGet(t, server, "/api/kata/publication", url.Values{"job_id": {fmt.Sprint(job.ID)}})
	assert.Equal(t, "other", decodeKata[KataPublicationBody](t, w).Project)
	api := httptest.NewServer(server.httpServer.Handler)
	t.Cleanup(api.Close)
	query := mcpserver.KataContextQuery{ReviewUUID: record.ReviewUUID.String(), Project: "reviews"}
	direct, err := server.mcpBackend().GetKataContext(t.Context(), query)
	require.NoError(t, err)
	remote, err := mcpserver.NewHTTPBackend(api.URL, api.Client()).GetKataContext(t.Context(), query)
	require.NoError(t, err)
	assert.Equal(t, direct, remote)
	assert.Equal(t, "reviews", direct["project"])
	for _, result := range []map[string]any{direct, remote} {
		var publicationWire katagen.ShowIssueResponseBody
		encoded, marshalErr := json.Marshal(result)
		require.NoError(t, marshalErr)
		require.NoError(t, json.Unmarshal(encoded, &publicationWire))
		require.Len(t, publicationWire.Comments, 1)
		assert.Equal(t, []katagen.CommentLink{link}, publicationWire.Comments[0].Backlinks)
		assert.Equal(t, new(true), publicationWire.Comments[0].BacklinksTruncated)
	}
	cfg.Hooks = nil
	cfg.Integrations.Kata.DefaultProject = "other"
	w = kataGet(t, server, "/api/kata/publication", url.Values{"review_uuid": {record.ReviewUUID.String()}, "project": {"reviews"}})
	assert.Equal(t, "reviews", decodeKata[KataPublicationBody](t, w).Project)
	w = kataGet(t, server, "/api/kata/publication", url.Values{"job_id": {fmt.Sprint(job.ID)}})
	assert.Equal(t, "other", decodeKata[KataPublicationBody](t, w).Project)
	cfg.Integrations.Kata.DefaultProject = "reviews"
	cfg.Hooks = []config.HookConfig{{Type: "kata", Event: "review.completed", Findings: "summary", Project: "other"}}
	cfg.Hooks = append(cfg.Hooks, config.HookConfig{Type: "kata", Event: "review.completed", Findings: "summary", Project: "reviews"})
	w = kataGet(t, server, "/api/kata/publication", url.Values{"job_id": {fmt.Sprint(job.ID)}})
	assert.Contains(t, decodeKata[KataPublicationBody](t, w).Message, "ambiguous Kata publication projects")
	w = kataGet(t, server, "/api/kata/publication", url.Values{"job_id": {fmt.Sprint(job.ID)}, "project": {"reviews"}})
	assert.Equal(t, "reviews", decodeKata[KataPublicationBody](t, w).Project)

	// Sync may replace a document without changing the review UUID.
	cfg.Hooks = nil
	doc.Summary = "newer structured review"
	doc.Findings[0].Problem = "newer finding"
	rawB, err := json.Marshal(doc)
	require.NoError(t, err)
	_, err = tc.DB.ExecContext(t.Context(), `UPDATE reviews SET structured_output = ? WHERE job_id = ?`, string(rawB), job.ID)
	require.NoError(t, err)
	reviewB, err := tc.DB.GetReviewByJobID(job.ID)
	require.NoError(t, err)
	recordB, err := tc.DB.GetKataPublication(t.Context(), job.ID)
	require.NoError(t, err)
	assert.Equal(t, *review.UUID, *reviewB.UUID)
	assert.NotEqual(t, record.DocumentHash, recordB.DocumentHash)
	require.NoError(t, prompt.ValidateKataContextSource(reviewB, reviewB.StructuredOutput, recordB.DocumentHash))
	require.Error(t, prompt.ValidateKataContextSource(reviewB, reviewB.StructuredOutput, record.DocumentHash))
	require.Error(t, prompt.ValidateKataContextSource(reviewB, review.StructuredOutput, recordB.DocumentHash))
	issue.Out.Metadata["roborev.document_hash"] = recordB.DocumentHash
	issue.Comments = append(issue.Comments, katagen.CommentOut{UID: "new-reply", IssueID: issue.Out.ID, ReplyToUID: new("root-uid"), Author: "teammate", Body: "new version claim"})
	review.Job, reviewB.Job = job, job
	cache := prompt.NewKataContextCache(server.readKataPromptContext)
	beforeA := len(fake.AllRequests())
	selectedA, err := cache.Decorate(t.Context(), review)
	require.NoError(t, err)
	require.NotEmpty(t, review.Output)
	assert.Contains(t, selectedA.Output, review.Output)
	assert.Contains(t, selectedA.Output, "Kata discussion unavailable")
	assert.NotContains(t, selectedA.Output, "new version claim")
	afterA := len(fake.AllRequests())
	assert.Greater(t, afterA, beforeA)
	selectedB, err := cache.Decorate(t.Context(), reviewB)
	require.NoError(t, err)
	assert.Contains(t, selectedB.Output, reviewB.Output)
	assert.Contains(t, selectedB.Output, "new version claim")
	assert.Contains(t, selectedB.Output, "teammate")
	afterB := len(fake.AllRequests())
	assert.Greater(t, afterB, afterA)
	againA, err := cache.Decorate(t.Context(), review)
	require.NoError(t, err)
	againB, err := cache.Decorate(t.Context(), reviewB)
	require.NoError(t, err)
	assert.Equal(t, selectedA.Output, againA.Output)
	assert.Equal(t, selectedB.Output, againB.Output)
	assert.Len(t, fake.AllRequests(), afterB)
	_, err = tc.DB.ExecContext(t.Context(), `UPDATE reviews SET structured_output = ? WHERE job_id = ?`, string(raw), job.ID)
	require.NoError(t, err)
	issue.Out.Metadata["roborev.document_hash"] = record.DocumentHash
	issue.Comments = issue.Comments[:1]

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
	// Replace the selected review during the remote read, after UUID resolution.
	replacementID := uuid.New()
	changed := false
	fake.Override = func(_ http.ResponseWriter, r *http.Request) bool {
		if !changed && strings.Contains(r.URL.Path, "/issues") {
			changed = true
			_, updateErr := tc.DB.ExecContext(r.Context(), `UPDATE reviews SET uuid = ? WHERE job_id = ?`, replacementID.String(), job.ID)
			assert.NoError(t, updateErr)
		}
		return false
	}
	w = kataGet(t, server, "/api/kata/publication", url.Values{"review_uuid": {record.ReviewUUID.String()}})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.True(t, changed)
	displaced := decodeKata[KataPublicationBody](t, w)
	assert.Equal(t, "unavailable", displaced.State)
	assert.Empty(t, displaced.Comments)
	assert.Empty(t, displaced.SourceOutput)
	assert.Empty(t, displaced.SourceDocument)
	selected, err := server.readKataPromptContext(t.Context(), review)
	require.NoError(t, err)
	assert.Equal(t, "unavailable", selected.(KataPublicationBody).State)
	for _, req := range fake.AllRequests() {
		assert.NotEqual(t, http.MethodPost, req.Method)
	}
}

func TestKataPromptContextWithoutDestination(t *testing.T) {
	fake := katafake.New(t)
	tc := newWorkerTestContext(t, 1)
	job := tc.createAndClaimJob(t, "unbound-review", "worker")
	doc := structuredreview.Document{SchemaVersion: structuredreview.SchemaVersion, Summary: "summary", Verdict: structuredreview.VerdictFail, Findings: []structuredreview.Finding{{Severity: "high", Problem: "problem", Fix: "fix"}}}
	raw, err := json.Marshal(doc)
	require.NoError(t, err)
	require.NoError(t, tc.DB.CompleteJobResult(job.ID, "test", storage.ReviewCompletion{StructuredOutput: raw, Verdict: storage.VerdictFail}))
	review, err := tc.DB.GetReviewByJobID(job.ID)
	require.NoError(t, err)
	review.Job = job
	cfg := kataConnectionConfig(fake)
	cfg.Integrations.Kata.DefaultProject = ""
	server := newServerWithLogs(tc.DB, cfg, "", newTestErrorLog(), newTestActivityLog())
	t.Cleanup(func() { _ = server.Close() })

	decorated, err := prompt.NewKataContextCache(server.readKataPromptContext).Decorate(t.Context(), review)
	require.NoError(t, err)
	assert.Equal(t, review.Output, decorated.Output)
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
	_, generation := server.configWatcher.kataSnapshot()
	base := KataResponseRequest{Project: "reviews", DestinationID: "wrong", Generation: generation, Teammate: "roborev", IssueUID: "issue-uid", TargetUID: "01AAAAAAAAAAAAAAAAAAABC123", Kind: "reply", Body: "text", IdempotencyKey: "key"}
	for _, tc := range []struct {
		name         string
		request      KataResponseRequest
		status       int
		message      string
		projectReads int
	}{
		{"missing fields", KataResponseRequest{}, http.StatusBadRequest, "missing destination or target", 0},
		{"invalid key", func() KataResponseRequest { r := base; r.IdempotencyKey = ""; return r }(), http.StatusBadRequest, "invalid idempotency key", 0},
		{"generation", func() KataResponseRequest { r := base; r.Generation++; return r }(), http.StatusConflict, "configuration changed", 0},
		{"teammate", func() KataResponseRequest { r := base; r.Teammate = "other"; return r }(), http.StatusConflict, "teammate changed", 0},
		{"destination", base, http.StatusConflict, "destination changed", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := len(fake.AllRequests())
			w := kataPost(t, server, "/api/kata/response", tc.request)
			assert.Equal(t, tc.status, w.Code, w.Body.String())
			assert.Contains(t, w.Body.String(), tc.message)
			var projectReads int
			for _, request := range fake.AllRequests()[before:] {
				assert.NotEqual(t, http.MethodPost, request.Method)
				if request.Method == http.MethodGet && request.Path == "/api/v1/projects" {
					projectReads++
				}
			}
			assert.Equal(t, tc.projectReads, projectReads)
		})
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
		saved := katagen.CommentOut{UID: fmt.Sprintf("01%024d", len(issue.Comments)+1), IssueID: issue.Out.ID, Handle: new("c:789012"), Body: request.Body, Author: "actual-author", Teammate: request.Teammate, ReplyToUID: request.ReplyTo, ReplyKind: new(string(*request.Kind))}
		issue.Comments = append(issue.Comments, saved)
		fake.Reply(w, map[string]any{"changed": true, "issue": map[string]any{"id": issue.Out.ID, "uid": issue.Out.UID, "project_id": 8}, "comment": map[string]any{"uid": saved.UID, "issue_id": issue.Out.ID, "body": saved.Body, "author": saved.Author, "teammate": *saved.Teammate, "reply_to_uid": *saved.ReplyToUID, "reply_kind": *saved.ReplyKind}})
		return true
	}
	read := kataGet(t, server, "/api/kata/issue", url.Values{"ref": {issue.Out.UID}, "project": {"other"}})
	require.Equal(t, http.StatusOK, read.Code, read.Body.String())
	full := decodeKata[KataIssue](t, read)
	for _, tc := range []struct {
		name    string
		mutate  func()
		status  int
		message string
	}{
		{"removed target", func() { issue.Comments = issue.Comments[:1] }, http.StatusNotFound, "target comment not found"},
		{"edited root", func() { issue.Comments[0].Body += " changed" }, http.StatusBadRequest, "complete finding thread"},
		{"missing ancestor", func() { issue.Comments[1].ReplyToUID = new("01CCCCCCCCCCCCCCCCCCABC123") }, http.StatusBadRequest, "missing comment ancestor"},
		{"reparented", func() {
			issue.Comments = append(issue.Comments, katagen.CommentOut{UID: "01CCCCCCCCCCCCCCCCCCABC123", IssueID: issue.Out.ID, Body: "other root", Author: "service"})
			issue.Comments[1].ReplyToUID = &issue.Comments[2].UID
		}, http.StatusBadRequest, "complete finding thread"},
		{"foreign ownership", func() { issue.Comments[1].IssueID++ }, http.StatusBadRequest, "foreign comment"},
		{"cycle", func() {
			issue.Comments[0].ReplyToUID = &issue.Comments[1].UID
			issue.Comments[0].ReplyKind = new("reply")
		}, http.StatusBadRequest, "cyclic comment ancestry"},
		{"unsupported envelope", func() { issue.Out.Metadata["roborev.version"] = 2 }, http.StatusBadRequest, "invalid publication envelope"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := append([]katagen.CommentOut(nil), issue.Comments...)
			tc.mutate()
			defer func() { issue.Comments = original; issue.Out.Metadata["roborev.version"] = 1 }()
			before := len(fake.AllRequests())
			result := kataPost(t, server, "/api/kata/response", KataResponseRequest{Project: full.Project, DestinationID: full.DestinationID, Generation: full.Generation, Teammate: full.ReplyTeammate, IssueUID: full.UID, TargetUID: prior.UID, Kind: "reply", Body: "evidence", IdempotencyKey: "Thread-Key"})
			assert.Equal(t, tc.status, result.Code, result.Body.String())
			assert.Contains(t, result.Body.String(), tc.message)
			for _, sent := range fake.AllRequests()[before:] {
				assert.NotEqual(t, http.MethodPost, sent.Method)
			}
		})
	}
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

	// Exercise the registered route with the exact JSON-v2 producer sizes.
	kind := katagen.Reply
	wire := katagen.CommentRequestBody{Actor: new(kata.WriteActor), Body: "", Kind: &kind, ReplyTo: &prior.UID, Teammate: new("roborev")}
	emptyWire, encodeErr := jsonv2.Marshal(wire, jsonv2.Deterministic(true))
	require.NoError(t, encodeErr)
	route := httptest.NewServer(server.Handler())
	t.Cleanup(route.Close)
	generatedClient, clientErr := apiclient.NewWithHTTPClient(route.URL, route.Client())
	require.NoError(t, clientErr)
	for _, tc := range []struct {
		name, content, encoding string
		producerSize            int
		accepted                bool
	}{
		{"producer N-1", strings.Repeat("x", kata.PublicationMaxRequestBytes-1-len(emptyWire)), "direct", kata.PublicationMaxRequestBytes - 1, true},
		{"producer N", strings.Repeat("x", kata.PublicationMaxRequestBytes-len(emptyWire)), "direct", kata.PublicationMaxRequestBytes, false},
		{"producer N+1", strings.Repeat("x", kata.PublicationMaxRequestBytes+1-len(emptyWire)), "direct", kata.PublicationMaxRequestBytes + 1, false},
		{"generated HTML escaping", strings.Repeat("<", 360000), "generated", 0, true},
		{"direct expanded JSON", strings.Repeat("x", 360000), "expanded", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wire.Body = tc.content
			encoded, encodeErr := jsonv2.Marshal(wire, jsonv2.Deterministic(true))
			require.NoError(t, encodeErr)
			if tc.producerSize != 0 {
				require.Len(t, encoded, tc.producerSize)
			}
			requestBody := KataResponseRequest{Project: full.Project, DestinationID: full.DestinationID, Generation: full.Generation, Teammate: full.ReplyTeammate, IssueUID: full.UID, TargetUID: prior.UID, Kind: "reply", Body: tc.content, IdempotencyKey: "Boundary-Key"}
			before := len(fake.AllRequests())
			var status int
			var reply KataReplyReceipt
			var wrapperBytes int64
			if tc.encoding == "generated" {
				input := generated.KataResponseRequest{Project: requestBody.Project, DestinationID: requestBody.DestinationID, Generation: int64(requestBody.Generation), Teammate: requestBody.Teammate, IssueUID: requestBody.IssueUID, TargetUID: requestBody.TargetUID, Kind: requestBody.Kind, Body: requestBody.Body, IdempotencyKey: requestBody.IdempotencyKey}
				got, callErr := generatedClient.KataResponse(t.Context(), &generated.KataResponseRequestOptions{Body: &input}, func(_ context.Context, r *http.Request) error { wrapperBytes = r.ContentLength; return nil })
				require.NoError(t, callErr)
				reply = KataReplyReceipt{UID: got.UID, Author: got.Author, Teammate: got.Teammate, Changed: got.Changed, Body: got.Body}
				status = http.StatusOK
			} else {
				raw, marshalErr := json.Marshal(requestBody)
				require.NoError(t, marshalErr)
				if tc.encoding == "expanded" {
					original := append([]byte(`"body":"`), bytes.Repeat([]byte("x"), len(tc.content))...)
					replacement := append([]byte(`"body":"`), bytes.Repeat([]byte(`\u0078`), len(tc.content))...)
					require.Contains(t, string(raw), string(original))
					raw = bytes.Replace(raw, original, replacement, 1)
				}
				wrapperBytes = int64(len(raw))
				r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/kata/response", bytes.NewReader(raw))
				r.Header.Set("Content-Type", "application/json")
				w := httptest.NewRecorder()
				server.Handler().ServeHTTP(w, r)
				status = w.Code
				if tc.accepted {
					reply = decodeKata[KataReplyReceipt](t, w)
				} else {
					assert.Contains(t, w.Body.String(), "request limit")
				}
			}
			if tc.encoding == "generated" || tc.encoding == "expanded" {
				assert.Greater(t, wrapperBytes, int64(2<<20))
			}
			if tc.producerSize == kata.PublicationMaxRequestBytes-1 {
				assert.Greater(t, wrapperBytes, int64(kata.PublicationMaxRequestBytes))
			}
			var posts []katafake.Request
			for _, sent := range fake.AllRequests()[before:] {
				if sent.Method == http.MethodPost {
					posts = append(posts, sent)
				}
			}
			if !tc.accepted {
				assert.Equal(t, http.StatusBadRequest, status)
				assert.Empty(t, posts)
				return
			}
			require.Equal(t, http.StatusOK, status)
			require.Len(t, posts, 1)
			assert.Equal(t, encoded, posts[0].Body)
			assert.Equal(t, requestBody.IdempotencyKey, posts[0].Header.Get("Idempotency-Key"))
			assert.Equal(t, tc.content, request.Body)
			require.NotNil(t, request.ReplyTo)
			assert.Equal(t, prior.UID, *request.ReplyTo)
			require.NotNil(t, request.Kind)
			assert.Equal(t, kind, *request.Kind)
			assert.Equal(t, "actual-author", reply.Author)
			assert.Equal(t, "roborev", reply.Teammate)
			assert.Equal(t, tc.content, reply.Body)
		})
	}
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
}

func TestKataResponseRejectsDestinationChangesAtBarrier(t *testing.T) {
	for _, scenario := range []string{"project UID", "reload"} {
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
			configBefore, _ := server.configWatcher.kataSnapshot()
			if scenario == "reload" {
				server.configWatcher.configPath = filepath.Join(t.TempDir(), "config.toml")
				require.NoError(t, config.SaveGlobalTo(server.configWatcher.configPath, server.configWatcher.Config()))
			}
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
				server.configWatcher.reloadConfig()
				reloaded, generation := server.configWatcher.kataSnapshot()
				assert.Equal(t, configBefore, reloaded)
				assert.Equal(t, full.Generation+1, generation)
			}
			close(resume)
			<-done
			assert.Equal(t, http.StatusConflict, w.Code, w.Body.String())
			if scenario == "project UID" {
				assert.Contains(t, w.Body.String(), "destination changed")
			} else {
				assert.Contains(t, w.Body.String(), "configuration changed")
			}
			for _, req := range fake.AllRequests() {
				assert.NotEqual(t, http.MethodPost, req.Method)
			}
		})
	}
}

func TestKataPublicationExplicitIssueWithoutLocalReview(t *testing.T) {
	fake := katafake.New(t)
	tc := newWorkerTestContext(t, 1)
	issue := fake.AddIssue(7, "standalone", "Discussion", "complete issue body")
	issue.Comments = []katagen.CommentOut{{UID: "orphan", IssueID: issue.Out.ID, Author: "reviewer", Body: "complete evidence", ReplyToUID: new("missing")}}
	server := newServerWithLogs(tc.DB, kataConnectionConfig(fake), "", newTestErrorLog(), newTestActivityLog())
	t.Cleanup(func() { _ = server.Close() })
	w := kataGet(t, server, "/api/kata/publication", url.Values{"issue": {issue.Out.UID}, "project": {"reviews"}})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	got := decodeKata[KataPublicationBody](t, w)
	assert.Equal(t, "unverified", got.State)
	require.NotNil(t, got.Issue)
	assert.Equal(t, "complete issue body", got.Issue.Body)
	require.Len(t, got.Comments, 1)
	assert.Equal(t, "complete evidence", got.Comments[0].Body)
	assert.Equal(t, "orphan", got.Comments[0].Lineage)
	assert.Equal(t, "unknown", got.Comments[0].History)
}

func TestKataPublicationLegacyUUIDPreservesSource(t *testing.T) {
	fake := katafake.New(t)
	tc := newWorkerTestContext(t, 1)
	job := testutil.CreateCompletedReview(t, tc.DB, tc.Repo.ID, "legacy-review", "test", "original legacy findings")
	review, err := tc.DB.GetReviewByJobID(job.ID)
	require.NoError(t, err)
	require.NotNil(t, review.UUID)
	_, err = tc.DB.ExecContext(t.Context(), `UPDATE reviews SET structured_output = NULL, output = ? WHERE job_id = ?`, "original legacy findings", job.ID)
	require.NoError(t, err)
	server := newServerWithLogs(tc.DB, kataConnectionConfig(fake), "", newTestErrorLog(), newTestActivityLog())
	t.Cleanup(func() { _ = server.Close() })
	byJob := kataGet(t, server, "/api/kata/publication", url.Values{"job_id": {fmt.Sprint(job.ID)}})
	byUUID := kataGet(t, server, "/api/kata/publication", url.Values{"review_uuid": {review.UUID.String()}})
	require.Equal(t, http.StatusOK, byJob.Code, byJob.Body.String())
	require.Equal(t, http.StatusOK, byUUID.Code, byUUID.Body.String())
	got := decodeKata[KataPublicationBody](t, byUUID)
	assert.Equal(t, decodeKata[KataPublicationBody](t, byJob), got)
	assert.Equal(t, "unavailable", got.State)
	assert.Equal(t, "original legacy findings", got.SourceOutput)
}

func TestKataPublicationAuthenticationIsFatal(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			fake := katafake.New(t)
			fake.Override = func(w http.ResponseWriter, r *http.Request) bool {
				fake.Fail(w, status, "denied", "access denied")
				return true
			}
			tc := newWorkerTestContext(t, 1)
			server := newServerWithLogs(tc.DB, kataConnectionConfig(fake), "", newTestErrorLog(), newTestActivityLog())
			t.Cleanup(func() { _ = server.Close() })
			w := kataGet(t, server, "/api/kata/publication", url.Values{"issue": {"issue"}, "project": {"reviews"}})
			assert.Equal(t, status, w.Code, w.Body.String())
		})
	}
}

func TestKataContextMCPBackendParity(t *testing.T) {
	server, fake := kataAPIServer(t)
	issue := fake.AddIssue(7, "parity", "Discussion", "body")
	issue.Comments = []katagen.CommentOut{{UID: "comment", IssueID: issue.Out.ID, Author: "author", Body: "evidence"}}
	api := httptest.NewServer(server.httpServer.Handler)
	t.Cleanup(api.Close)
	query := mcpserver.KataContextQuery{Issue: issue.Out.UID, Project: "reviews"}
	inProcess, err := server.mcpBackend().GetKataContext(t.Context(), query)
	require.NoError(t, err)
	httpResult, err := mcpserver.NewHTTPBackend(api.URL, api.Client()).GetKataContext(t.Context(), query)
	require.NoError(t, err)
	assert.Equal(t, inProcess, httpResult)
	numeric := fake.AddIssue(8, "1234", "Numeric issue", "complete evidence")
	numeric.Comments = []katagen.CommentOut{{UID: "numeric-comment", IssueID: numeric.Out.ID, Author: "reviewer", Body: "conflicting evidence"}}
	for _, query := range []mcpserver.KataContextQuery{{Issue: "other#1234"}, {Issue: "1234", Project: "other"}, {Issue: "other#1234", Project: "other"}} {
		direct, err := server.mcpBackend().GetKataContext(t.Context(), query)
		require.NoError(t, err)
		remote, err := mcpserver.NewHTTPBackend(api.URL, api.Client()).GetKataContext(t.Context(), query)
		require.NoError(t, err)
		assert.Equal(t, direct, remote)
		assert.Equal(t, "other", direct["project"])
		assert.Equal(t, numeric.Out.UID, direct["issue_uid"])
		assert.Contains(t, fmt.Sprint(direct["comments"]), "conflicting evidence")
	}
	for _, query := range []url.Values{{"issue": {"other#1234"}, "project": {"reviews"}}, {"issue": {"other#"}}, {"issue": {"#1234"}}, {"issue": {"other#bad#1234"}}} {
		before := len(fake.AllRequests())
		w := kataGet(t, server, "/api/kata/publication", query)
		assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
		assert.Len(t, fake.AllRequests(), before)
	}
	conflict := mcpserver.KataContextQuery{Issue: "other#1234", Project: "reviews"}
	_, directConflict := server.mcpBackend().GetKataContext(t.Context(), conflict)
	_, remoteConflict := mcpserver.NewHTTPBackend(api.URL, api.Client()).GetKataContext(t.Context(), conflict)
	var directInvalid, remoteInvalid *mcpserver.Error
	require.ErrorAs(t, directConflict, &directInvalid)
	require.ErrorAs(t, remoteConflict, &remoteInvalid)
	assert.Equal(t, directInvalid.Code, remoteInvalid.Code)
	fake.Override = func(w http.ResponseWriter, r *http.Request) bool {
		fake.Fail(w, http.StatusUnauthorized, "unauthorized", "authentication required")
		return true
	}
	_, directErr := server.mcpBackend().GetKataContext(t.Context(), query)
	_, httpErr := mcpserver.NewHTTPBackend(api.URL, api.Client()).GetKataContext(t.Context(), query)
	var direct, remote *mcpserver.Error
	require.ErrorAs(t, directErr, &direct)
	require.ErrorAs(t, httpErr, &remote)
	assert.Equal(t, mcpserver.ErrorCodeAuthentication, direct.Code)
	assert.Equal(t, direct.Code, remote.Code)
}

func TestKataPublicationUUIDWithoutLocalReview(t *testing.T) {
	fake := katafake.New(t)
	tc := newWorkerTestContext(t, 1)
	issue := fake.AddIssue(7, "remote-only", "Discussion", "body")
	id := "11111111-1111-4111-8111-111111111111"
	issue.Out.Metadata = map[string]any{"roborev.version": 1, "roborev.review_uuid": id, "roborev.document_hash": strings.Repeat("a", 64), "roborev.policy": "summary", "roborev.completed_at": "2026-01-02T03:04:05Z", "roborev.finding_count": 1}
	server := newServerWithLogs(tc.DB, kataConnectionConfig(fake), "", newTestErrorLog(), newTestActivityLog())
	t.Cleanup(func() { _ = server.Close() })
	w := kataGet(t, server, "/api/kata/publication", url.Values{"review_uuid": {id}, "project": {"reviews"}})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	got := decodeKata[KataPublicationBody](t, w)
	assert.Equal(t, "unverified", got.State)
	assert.Equal(t, id, got.ReviewUUID)
	assert.Equal(t, issue.Out.UID, got.IssueUID)
	require.NotNil(t, got.Issue)
	assert.Equal(t, "body", got.Issue.Body)
}

func TestKataContextCancellationClassification(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
	}{
		{context.Canceled, http.StatusRequestTimeout},
		{context.DeadlineExceeded, http.StatusGatewayTimeout},
	} {
		var status huma.StatusError
		require.ErrorAs(t, kataReadError(tc.err), &status)
		assert.Equal(t, tc.status, status.GetStatus())
	}
}

func TestKataEvidenceStoredForWorkerPlanAndImplementation(t *testing.T) {
	fake := katafake.New(t)
	tc := newWorkerTestContext(t, 1)
	require.NoError(t, os.WriteFile(filepath.Join(tc.TmpDir, ".roborev.toml"), []byte("max_prompt_size = 1000\n"), 0o600))
	job := tc.createAndClaimJob(t, testutil.GetHeadSHA(t, tc.TmpDir), "parent-worker")
	doc := structuredreview.Document{SchemaVersion: structuredreview.SchemaVersion, Summary: "summary", Verdict: structuredreview.VerdictFail, Findings: []structuredreview.Finding{{Severity: "high", Problem: "original finding", Fix: "fix"}}}
	raw, err := json.Marshal(doc)
	require.NoError(t, err)
	require.NoError(t, tc.DB.CompleteJobResult(job.ID, "test", storage.ReviewCompletion{Output: "High: original finding", StructuredOutput: raw, Verdict: storage.VerdictFail}))
	review, err := tc.DB.GetReviewByJobID(job.ID)
	require.NoError(t, err)
	record, err := tc.DB.GetKataPublication(t.Context(), job.ID)
	require.NoError(t, err)
	issue := fake.AddIssue(7, "stored", "findings", "body")
	issue.Out.Metadata = map[string]any{"roborev.version": 1, "roborev.review_uuid": record.ReviewUUID.String(), "roborev.document_hash": record.DocumentHash, "roborev.policy": "summary", "roborev.completed_at": review.CreatedAt.UTC().Format(time.RFC3339Nano), "roborev.finding_count": 1}
	evidence := strings.Repeat("conflicting current evidence ", 400)
	issue.Comments = []katagen.CommentOut{{UID: "orphan", IssueID: issue.Out.ID, Body: evidence, Author: "reviewer", ReplyToUID: new("missing")}}
	name := "worker-stored-kata-context"
	calls := 0
	var snapshots []string
	agent.RegisterForTest(t, &agent.FakeAgent{NameStr: name, ReviewFn: func(_ context.Context, path, _, p string, _ io.Writer) (string, error) {
		calls++
		if rest, ok := strings.CutPrefix(p, "Read the complete task prompt from "); ok {
			quoted, _, _ := strings.Cut(rest, " and carry out")
			file, err := strconv.Unquote(quoted)
			require.NoError(t, err)
			snapshots = append(snapshots, file)
			data, err := os.ReadFile(file)
			require.NoError(t, err)
			p = string(data)
		}
		assert.Contains(t, p, "original finding")
		assert.Contains(t, p, evidence)
		assert.Contains(t, p, "reviewer")
		if calls == 1 {
			return "Fix the finding", nil
		}
		require.NoError(t, os.WriteFile(filepath.Join(path, "fixed.txt"), []byte("fixed"), 0o600))
		return "Fixed", nil
	}})
	cfg := kataConnectionConfig(fake)
	cfg.FixAgent = name
	server := newServerWithLogs(tc.DB, cfg, "", newTestErrorLog(), newTestActivityLog())
	t.Cleanup(func() { _ = server.Close() })
	w := kataPost(t, server, "/api/job/fix", FixJobRequest{ParentJobID: job.ID, PlanFirst: true})
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	queued := decodeKata[storage.ReviewJob](t, w)
	reads := len(fake.AllRequests())
	require.Positive(t, reads)
	claimed, err := tc.DB.ClaimJob(testWorkerID)
	require.NoError(t, err)
	require.Equal(t, queued.ID, claimed.ID)
	server.workerPool.processJob(testWorkerID, claimed)
	assert.Equal(t, 2, calls)
	assert.Len(t, fake.AllRequests(), reads, "stored prompt execution must not re-read Kata")
	for _, file := range snapshots {
		_, err := os.Stat(file)
		require.ErrorIs(t, err, os.ErrNotExist)
	}
}

func TestKataFixEnqueueSkipsDisabledAndCIParents(t *testing.T) {
	for _, mode := range []string{"disabled", "ci-source", "ci-base"} {
		t.Run(mode, func(t *testing.T) {
			fixture := setupFixJobFixture(t, "synthetic-repo", "synthetic-ref")
			fake := katafake.New(t)
			enableKataConnection(fixture.server, fake)
			doc := structuredreview.Document{SchemaVersion: structuredreview.SchemaVersion, Summary: "summary", Verdict: structuredreview.VerdictFail, Findings: []structuredreview.Finding{{Severity: "high", Problem: "problem", Fix: "fix"}}}
			raw, err := json.Marshal(doc)
			require.NoError(t, err)
			_, err = fixture.db.ExecContext(t.Context(), "UPDATE reviews SET structured_output = ? WHERE job_id = ?", string(raw), fixture.reviewJob.ID)
			require.NoError(t, err)
			switch mode {
			case "disabled":
				fixture.server.configWatcher.Config().Integrations.Kata.Enabled = false
			case "ci-source":
				_, err = fixture.db.ExecContext(t.Context(), "UPDATE review_jobs SET source = ? WHERE id = ?", storage.JobSourceCI, fixture.reviewJob.ID)
			case "ci-base":
				_, err = fixture.db.ExecContext(t.Context(), "UPDATE review_jobs SET ci_base_branch = ? WHERE id = ?", "main", fixture.reviewJob.ID)
			}
			require.NoError(t, err)
			w := kataPost(t, fixture.server, "/api/job/fix", FixJobRequest{ParentJobID: fixture.reviewJob.ID, PlanFirst: true})
			require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
			assert.Empty(t, fake.AllRequests())
		})
	}
}
