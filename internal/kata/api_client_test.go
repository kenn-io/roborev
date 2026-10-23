package kata

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gen "go.kenn.io/kata/pkg/client/generated"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/kata/katafake"
)

const testAPIKey = "synthetic-kata-credential"

func apiClientFor(t *testing.T, f *katafake.Server, workdir string, mutate ...func(*config.KataIntegrationConfig)) *APIClient {
	t.Helper()
	cfg := config.KataIntegrationConfig{
		Enabled: true, Endpoint: f.URL(), APIKey: testAPIKey, DefaultProject: "reviews", Teammate: "roborev",
	}
	for _, m := range mutate {
		m(&cfg)
	}
	c, err := NewAPIClient(workdir, cfg)
	require.NoError(t, err)
	t.Cleanup(c.CloseIdleConnections)
	return c
}

func bindWorkspace(t *testing.T, project string) string {
	t.Helper()
	dir := t.TempDir()
	if project != "" {
		require.NoError(t, os.WriteFile(filepath.Join(dir, ".kata.toml"), []byte("[project]\nname = \""+project+"\"\n"), 0o600))
	}
	return dir
}

func TestNewAPIClientFailsExplicitlyOnABadConfiguration(t *testing.T) {
	_, err := NewAPIClient(t.TempDir(), config.KataIntegrationConfig{Enabled: true, Endpoint: "ftp://x"})
	require.ErrorIs(t, err, ErrInsecureEndpoint)

	_, err = NewAPIClient(t.TempDir(), config.KataIntegrationConfig{
		Enabled: true, Endpoint: "https://kata.example.com", APIKeyFile: filepath.Join(t.TempDir(), "missing"),
	})
	require.ErrorIs(t, err, ErrAuthenticationRequired, "an unreadable key file is a credential failure, never a silent anonymous client")

	_, err = NewAPIClient(t.TempDir(), config.KataIntegrationConfig{Endpoint: "https://kata.example.com"})
	require.Error(t, err, "a disabled configuration does not build an API client")
}

func TestAPIClientBinding(t *testing.T) {
	f := katafake.New(t)
	ctx := t.Context()

	t.Run("workspace binding wins over the default project", func(t *testing.T) {
		b, err := apiClientFor(t, f, bindWorkspace(t, "other")).Binding(ctx)
		require.NoError(t, err)
		assert.Equal(t, "other", b.Project)
	})
	t.Run("default project covers an unbound workspace", func(t *testing.T) {
		b, err := apiClientFor(t, f, bindWorkspace(t, "")).Binding(ctx)
		require.NoError(t, err)
		assert.Equal(t, "reviews", b.Project)
	})
	t.Run("no binding and no default", func(t *testing.T) {
		c := apiClientFor(t, f, bindWorkspace(t, ""), func(c *config.KataIntegrationConfig) { c.DefaultProject = "" })
		_, err := c.Binding(ctx)
		require.ErrorIs(t, err, ErrNoBinding)
	})
	t.Run("a broken binding file stops instead of using the default", func(t *testing.T) {
		dir := bindWorkspace(t, "")
		require.NoError(t, os.WriteFile(filepath.Join(dir, ".kata.local.toml"), []byte("not = = toml"), 0o600))
		_, err := apiClientFor(t, f, dir).Binding(ctx)
		require.Error(t, err)
		require.NotErrorIs(t, err, ErrNoBinding)
	})
	t.Run("a binding file is read without contacting Kata", func(t *testing.T) {
		assert.Empty(t, f.AllRequests())
	})
}

func TestAPIClientProjectPrecedenceOnCreate(t *testing.T) {
	tests := []struct {
		name                string
		hook, binding, dflt string
		wantPath            string
		wantErr             error
	}{
		{"hook project wins", "other", "reviews", "reviews", "/api/v1/projects/8/issues", nil},
		{"binding wins over default", "", "other", "reviews", "/api/v1/projects/8/issues", nil},
		{"default covers no binding", "", "", "other", "/api/v1/projects/8/issues", nil},
		{"hook project names no project", "ghost", "reviews", "reviews", "", ErrProjectNotFound},
		{"nothing selects a project", "", "", "", "", ErrNoBinding},
		{"default project must exist", "", "", "ghost", "", ErrProjectNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := katafake.New(t)
			c := apiClientFor(t, f, bindWorkspace(t, tt.binding), func(c *config.KataIntegrationConfig) { c.DefaultProject = tt.dflt })
			_, err := c.Create(t.Context(), CreateReq{Title: "t", Project: tt.hook, IdempotencyKey: "k"})
			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
				for _, r := range f.Requests() {
					assert.NotEqual(t, http.MethodPost, r.Method, "nothing is written when no project resolves")
				}
				return
			}
			require.NoError(t, err)
			var posted []string
			for _, r := range f.Requests() {
				if r.Method == http.MethodPost {
					posted = append(posted, r.Path)
				}
			}
			assert.Equal(t, []string{tt.wantPath}, posted)
		})
	}
}

func TestAPIClientSchemaGate(t *testing.T) {
	for _, tc := range []struct {
		schema  string
		wantErr bool
	}{
		{"0.21.0", false},
		{"0.28.0", false},
		{"v0.30.1", false},
		{"1.0.0", false},
		{"0.20.9", true},
		{"0.9.0", true},
		{"", true},
		{"banana", true},
		{"0.21", true},
	} {
		f := katafake.New(t)
		f.APISchema = tc.schema
		_, err := apiClientFor(t, f, bindWorkspace(t, "reviews")).List(t.Context(), ListOpts{})
		if tc.wantErr {
			require.ErrorIs(t, err, ErrIncompatible, "schema %q", tc.schema)
			assert.Empty(t, f.Requests(), "no data request runs against an incompatible server")
			continue
		}
		require.NoError(t, err, "schema %q", tc.schema)
	}
}

func TestAPIClientTreatsAMissingHealthEndpointAsIncompatible(t *testing.T) {
	srv := newStatusServer(t, http.StatusNotFound)
	c := apiClientFromURL(t, srv.URL)
	_, err := c.List(t.Context(), ListOpts{})
	require.ErrorIs(t, err, ErrIncompatible)
}

func FuzzCompatibleAPISchema(f *testing.F) {
	f.Add(uint8(0), uint8(21), uint8(0))
	f.Add(uint8(0), uint8(20), uint8(255))
	f.Add(uint8(1), uint8(0), uint8(0))
	f.Fuzz(func(t *testing.T, major, minor, patch uint8) {
		version := strconv.Itoa(int(major)) + "." + strconv.Itoa(int(minor)) + "." + strconv.Itoa(int(patch))
		want := major > 0 || minor >= 21
		require.Equal(t, want, compatibleAPISchema(version), version)
		require.Equal(t, want, compatibleAPISchema("v"+version), "v"+version)
		paginates := major > 0 || minor >= 27
		require.Equal(t, paginates, schemaAtLeast(version, PaginationAPISchemaVersion), version)
	})
}

func TestAPIClientProjectLookupSkipsInactiveAndCaches(t *testing.T) {
	f := katafake.New(t)
	f.Projects = append(f.Projects, gen.ProjectOut{ID: 9, Name: "retired", Active: false, UID: "P9"})
	deleted := time.Unix(1, 0).UTC()
	f.Projects = append(f.Projects, gen.ProjectOut{ID: 10, Name: "gone", Active: true, DeletedAt: &deleted, UID: "P10"})
	c := apiClientFor(t, f, bindWorkspace(t, "retired"))
	_, err := c.List(t.Context(), ListOpts{})
	require.ErrorIs(t, err, ErrProjectNotFound)

	c = apiClientFor(t, f, bindWorkspace(t, "gone"))
	_, err = c.List(t.Context(), ListOpts{})
	require.ErrorIs(t, err, ErrProjectNotFound)

	c = apiClientFor(t, f, bindWorkspace(t, "reviews"))
	for range 3 {
		_, err = c.List(t.Context(), ListOpts{})
		require.NoError(t, err)
	}
	var lookups int
	for _, r := range f.AllRequests() {
		if r.Path == "/api/v1/projects" {
			lookups++
		}
	}
	assert.Equal(t, 2+1, lookups, "a client resolves a project once")
}

func TestAPIClientRefreshesProjectsAfterACacheMiss(t *testing.T) {
	f := katafake.New(t)
	c := apiClientFor(t, f, bindWorkspace(t, "reviews"))

	_, err := c.projectID(t.Context(), "new-project")
	require.ErrorIs(t, err, ErrProjectNotFound)

	f.AddProject(12, "new-project")
	id, err := c.projectID(t.Context(), "new-project")
	require.NoError(t, err)
	assert.Equal(t, int64(12), id)

	var lookups int
	for _, r := range f.AllRequests() {
		if r.Path == "/api/v1/projects" {
			lookups++
		}
	}
	assert.Equal(t, 2, lookups, "a missing project is looked up again on a later call")
}

func TestAPIClientListMapsIssuesAndSendsFilters(t *testing.T) {
	f := katafake.New(t)
	issue := f.AddIssue(7, "abc4", "Title", "Body", "bug", "p1")
	issue.Out.Parent = &gen.LinkPeer{ShortID: "par1", Project: "reviews", QualifiedID: "reviews#par1", Status: "open"}
	issue.Out.Blocks = []gen.LinkPeer{{ShortID: "b1", Project: "other", QualifiedID: "other#b1", Status: "open"}}
	issue.Out.BlockedBy = []gen.LinkPeer{{ShortID: "b2", Project: "reviews", QualifiedID: "reviews#b2", Status: "closed"}}
	issue.Out.Related = []gen.LinkPeer{{ShortID: "r1", Project: "reviews", QualifiedID: "reviews#r1", Status: "open"}}
	closed := f.AddIssue(7, "def5", "Done", "", "x")
	closed.Out.Status = "closed"
	c := apiClientFor(t, f, bindWorkspace(t, "reviews"))

	issues, err := c.List(t.Context(), ListOpts{})
	require.NoError(t, err)
	require.Len(t, issues, 1)
	assert.Equal(t, Issue{
		ShortID: "abc4", QualifiedID: "reviews#abc4", UID: "I0000000000000000000000001", ProjectUID: "P0000000000000000000000007", Title: "Title", Body: "Body", Status: "open", Labels: []string{"bug", "p1"}, Metadata: map[string]any{}, Attribution: Attribution{Author: "someone"},
		Parent:    &LinkPeer{ShortID: "par1", Project: "reviews", QualifiedID: "reviews#par1", Status: "open"},
		Blocks:    []LinkPeer{{ShortID: "b1", Project: "other", QualifiedID: "other#b1", Status: "open"}},
		BlockedBy: []LinkPeer{{ShortID: "b2", Project: "reviews", QualifiedID: "reviews#b2", Status: "closed"}},
		Related:   []LinkPeer{{ShortID: "r1", Project: "reviews", QualifiedID: "reviews#r1", Status: "open"}},
	}, issues[0])
	last := f.Requests()[len(f.Requests())-1]
	assert.Equal(t, "open", last.Query.Get("status"), "the CLI client's default is open issues")
	assert.Equal(t, "200", last.Query.Get("limit"), "the CLI's default row limit")

	all, err := c.List(t.Context(), ListOpts{Status: "all"})
	require.NoError(t, err)
	assert.Len(t, all, 2)
	last = f.Requests()[len(f.Requests())-1]
	assert.True(t, last.Query.Has("status"), "an absent status could default to open")
	assert.Empty(t, last.Query.Get("status"), "all statuses is the empty filter")
}

func TestAPIClientListUnlimitedFollowsCursors(t *testing.T) {
	f := katafake.New(t)
	more := "c2"
	last := ""
	f.ListPages = map[string]gen.ListIssuesResponseBody{
		"":   {Issues: []gen.IssueOut{{ShortID: "a1", QualifiedID: "reviews#a1", Status: "open"}}, NextCursor: new("c1")},
		"c1": {Issues: []gen.IssueOut{{ShortID: "a2", QualifiedID: "reviews#a2", Status: "open"}}, NextCursor: &more},
		"c2": {Issues: []gen.IssueOut{{ShortID: "a3", QualifiedID: "reviews#a3", Status: "open"}}, NextCursor: &last, Complete: true},
	}
	c := apiClientFor(t, f, bindWorkspace(t, "reviews"))
	issues, err := c.List(t.Context(), ListOpts{Unlimited: true})
	require.NoError(t, err)
	var ids []string
	for _, issue := range issues {
		ids = append(ids, issue.ShortID)
	}
	assert.Equal(t, []string{"a1", "a2", "a3"}, ids)
	reqs := f.Requests()
	require.Len(t, reqs, 4) // project lookup + three pages
	for _, r := range reqs[1:] {
		assert.Equal(t, "oldest", r.Query.Get("sort"), "cursors need a stable order")
		assert.False(t, r.Query.Has("limit"), "unlimited reads leave page sizing to Kata")
	}
	assert.Equal(t, []string{"", "c1", "c2"}, []string{reqs[1].Query.Get("cursor"), reqs[2].Query.Get("cursor"), reqs[3].Query.Get("cursor")})
}

func TestAPIClientListUnlimitedAcceptsLargeLegacyListing(t *testing.T) {
	f := katafake.New(t)
	f.APISchema = "0.26.9"
	const bodySize = (16 << 20) + 1
	wantBody := strings.Repeat("x", bodySize)
	f.AddIssue(7, "large1", "Large issue", wantBody)
	c := apiClientFor(t, f, bindWorkspace(t, "reviews"))

	issues, err := c.List(t.Context(), ListOpts{Unlimited: true})
	require.NoError(t, err)
	require.Len(t, issues, 1)
	assert.Equal(t, wantBody, issues[0].Body)
}

func TestAPIClientListUnlimitedStopsOnARepeatedCursor(t *testing.T) {
	f := katafake.New(t)
	loop := "same"
	f.ListPages = map[string]gen.ListIssuesResponseBody{
		"":     {Issues: []gen.IssueOut{{ShortID: "a1", Status: "open"}}, NextCursor: &loop},
		"same": {Issues: []gen.IssueOut{{ShortID: "a2", Status: "open"}}, NextCursor: &loop},
	}
	c := apiClientFor(t, f, bindWorkspace(t, "reviews"))
	_, err := c.List(t.Context(), ListOpts{Unlimited: true})
	require.ErrorIs(t, err, ErrInvalidResponse)
}

// Kata honors a list cursor from API schema 0.27.0. An older server ignores it
// and would answer every page request with the first page, so the complete
// graph is read in one request, which is what `kata list --limit 0` does.
func TestAPIClientListUnlimitedOnAServerWithoutCursorPagination(t *testing.T) {
	for _, schema := range []string{"0.21.0", "0.26.9"} {
		f := katafake.New(t)
		f.APISchema = schema
		f.ListPages = map[string]gen.ListIssuesResponseBody{
			"": {Issues: []gen.IssueOut{
				{ShortID: "a1", Status: "open"}, {ShortID: "a2", Status: "open"}, {ShortID: "a3", Status: "open"},
			}, Complete: true},
		}
		c := apiClientFor(t, f, bindWorkspace(t, "reviews"))
		issues, err := c.List(t.Context(), ListOpts{Unlimited: true})
		require.NoError(t, err, schema)
		assert.Len(t, issues, 3, schema)
		var lists []katafake.Request
		for _, r := range f.Requests() {
			if strings.HasSuffix(r.Path, "/issues") {
				lists = append(lists, r)
			}
		}
		require.Len(t, lists, 1, schema)
		for _, param := range []string{"limit", "cursor", "sort"} {
			assert.False(t, lists[0].Query.Has(param), "%s: %s would be ignored or truncate", schema, param)
		}
	}
}

// A page that ends without a cursor must be a complete one. A bounded result
// that says otherwise is an error, not a shorter graph.
func TestAPIClientListUnlimitedRefusesAnUnfinishedPage(t *testing.T) {
	f := katafake.New(t)
	f.ListPages = map[string]gen.ListIssuesResponseBody{
		"": {Issues: []gen.IssueOut{{ShortID: "a1", Status: "open"}}, Complete: false, Truncated: true},
	}
	c := apiClientFor(t, f, bindWorkspace(t, "reviews"))
	_, err := c.List(t.Context(), ListOpts{Unlimited: true})
	require.ErrorIs(t, err, ErrInvalidResponse)
}

func TestAPIClientListRejectsMissingOrNullIssues(t *testing.T) {
	for _, tc := range []struct {
		name      string
		schema    string
		unlimited bool
		body      string
	}{
		{name: "bounded list missing issues", schema: "0.28.0", body: `{}`},
		{name: "legacy list missing issues", schema: "0.26.9", unlimited: true, body: `{}`},
		{name: "legacy list null issues", schema: "0.26.9", unlimited: true, body: `{"issues":null}`},
		{name: "paginated list missing issues", schema: "0.28.0", unlimited: true, body: `{"complete":true}`},
		{name: "paginated list null issues", schema: "0.28.0", unlimited: true, body: `{"issues":null,"complete":true}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := katafake.New(t)
			f.APISchema = tc.schema
			f.Override = func(w http.ResponseWriter, r *http.Request) bool {
				if r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/issues") {
					return false
				}
				jsonHandler(tc.body)(w, r)
				return true
			}
			c := apiClientFor(t, f, bindWorkspace(t, "reviews"))
			_, err := c.List(t.Context(), ListOpts{Unlimited: tc.unlimited})
			require.ErrorIs(t, err, ErrInvalidResponse)
		})
	}
}

func TestAPIClientShow(t *testing.T) {
	f := katafake.New(t)
	issue := f.AddIssue(7, "abc4", "Task", "Do it", "bug")
	issue.Links = []gen.LinkOut{
		{Type: "blocks", From: gen.LinkPeer{ShortID: "x1", Project: "other", QualifiedID: "other#x1", Status: "open"}, To: gen.LinkPeer{ShortID: "abc4", Project: "reviews", QualifiedID: "reviews#abc4", Status: "open"}},
		{Type: "parent", From: gen.LinkPeer{ShortID: "abc4", Project: "reviews", QualifiedID: "reviews#abc4", Status: "open"}, To: gen.LinkPeer{ShortID: "p1", Project: "reviews", QualifiedID: "reviews#p1", Status: "open"}},
		{Type: "related", From: gen.LinkPeer{ShortID: "abc4", Project: "reviews", QualifiedID: "reviews#abc4", Status: "open"}, To: gen.LinkPeer{ShortID: "r1", Project: "reviews", QualifiedID: "reviews#r1", Status: "closed"}},
	}
	f.AddIssue(8, "zzz9", "Elsewhere", "")
	c := apiClientFor(t, f, bindWorkspace(t, "reviews"))

	got, err := c.Show(t.Context(), "abc4")
	require.NoError(t, err)
	assert.Equal(t, "reviews#abc4", got.QualifiedID)
	assert.Equal(t, "Do it", got.Body)
	assert.Equal(t, []string{"bug"}, got.Labels)
	require.NotNil(t, got.Parent)
	assert.Equal(t, "reviews#p1", got.Parent.QualifiedID)
	require.Len(t, got.BlockedBy, 1)
	assert.Equal(t, "other#x1", got.BlockedBy[0].QualifiedID)
	assert.Empty(t, got.Blocks)
	require.Len(t, got.Related, 1)

	byUID, err := c.Show(t.Context(), issue.Out.UID)
	require.NoError(t, err)
	assert.Equal(t, "abc4", byUID.ShortID)

	qualified, err := c.Show(t.Context(), "other#zzz9")
	require.NoError(t, err)
	assert.Equal(t, "other#zzz9", qualified.QualifiedID, "a qualified ref selects its own project")
}

func TestAPIClientShowRejectsHostileRefsWithoutARequest(t *testing.T) {
	f := katafake.New(t)
	c := apiClientFor(t, f, bindWorkspace(t, "reviews"))
	for _, ref := range []string{"", "a/b", "../x", "a?b", "other#", "#abc4", "x#a/b"} {
		_, err := c.Show(t.Context(), ref)
		require.ErrorIs(t, err, ErrRequestRejected, ref)
	}
	assert.Empty(t, f.AllRequests())
}

func TestAPIClientShowRefusesAnIssueThatIsNotTheOneAsked(t *testing.T) {
	f := katafake.New(t)
	f.AddIssue(7, "abc4", "Task", "")
	// Answer every issue read with a different issue than the one requested.
	f.Override = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodGet || !strings.HasPrefix(r.URL.Path, "/api/v1/projects/7/issues/") {
			return false
		}
		f.Reply(w, gen.ShowIssueResponseBody{Issue: gen.ShowIssueOut{ShortID: "zzzz", UID: "U1", Status: "open"}, Labels: []gen.IssueLabel{}, Links: []gen.LinkOut{}, Comments: []gen.CommentOut{}})
		return true
	}
	c := apiClientFor(t, f, bindWorkspace(t, "reviews"))
	_, err := c.Show(t.Context(), "abc4")
	require.ErrorIs(t, err, ErrInvalidResponse)
}

func TestAPIClientCreateSendsActorKeyAndMetadata(t *testing.T) {
	f := katafake.New(t)
	c := apiClientFor(t, f, bindWorkspace(t, "reviews"))
	priority := 0
	req := CreateReq{
		Title: "Review failed", Body: "body", Labels: []string{"roborev", "review-failed"}, Priority: &priority,
		IdempotencyKey: "roborev:job:1:review.failed", Metadata: map[string]any{"roborev.action": "a1"},
	}
	res, err := c.Create(t.Context(), req)
	require.NoError(t, err)
	assert.NotEmpty(t, res.ShortID)
	assert.False(t, res.Reused)

	post := lastPost(t, f)
	assert.Equal(t, "roborev:job:1:review.failed", post.Header.Get("Idempotency-Key"))
	body := post.JSON(t)
	assert.Equal(t, "roborev", body["actor"])
	assert.Equal(t, "Review failed", body["title"])
	assert.Equal(t, "body", body["body"])
	assert.Equal(t, []any{"roborev", "review-failed"}, body["labels"])
	assert.InDelta(t, 0, body["priority"], 0, "a zero priority is sent")
	assert.Equal(t, map[string]any{"roborev.action": "a1"}, body["metadata"])
	assert.NotContains(t, body, "force_new", "a look-alike is surfaced, as the CLI does")
	assert.Equal(t, "Bearer "+testAPIKey, post.Header.Get("Authorization"))

	again, err := c.Create(t.Context(), req)
	require.NoError(t, err)
	assert.True(t, again.Reused, "the same key reuses the issue")
	assert.Equal(t, res.ShortID, again.ShortID)

	changed := req
	changed.Title = "different"
	_, err = c.Create(t.Context(), changed)
	require.ErrorIs(t, err, ErrConflict)
	assert.Equal(t, "idempotency_mismatch", ErrorCode(err))
}

func TestAPIClientRecordsTheAttributionKataReturns(t *testing.T) {
	f := katafake.New(t)
	f.Principal = "svc-roborev"
	c := apiClientFor(t, f, bindWorkspace(t, "reviews"))
	res, err := c.Create(t.Context(), CreateReq{Title: "t", IdempotencyKey: "k"})
	require.NoError(t, err)
	assert.Equal(t, "svc-roborev", res.Attribution.Author, "what Kata returns is what is recorded")
	assert.Equal(t, "roborev", lastPost(t, f).JSON(t)["actor"], "roborev still asks to be roborev and never impersonates")
}

func TestAPIClientRejectsRedirectsWithoutLeakingTheCredential(t *testing.T) {
	var leaked []string
	other := katafake.New(t)
	other.Override = func(_ http.ResponseWriter, r *http.Request) bool {
		leaked = append(leaked, r.Header.Get("Authorization"))
		return false
	}
	redirecting := katafake.New(t)
	redirecting.Override = func(w http.ResponseWriter, r *http.Request) bool {
		http.Redirect(w, r, other.URL()+r.URL.Path, http.StatusTemporaryRedirect)
		return true
	}
	c := apiClientFor(t, redirecting, bindWorkspace(t, "reviews"))
	_, err := c.Create(t.Context(), CreateReq{Title: "t", IdempotencyKey: "k"})
	require.ErrorIs(t, err, ErrRedirect)
	assert.Empty(t, leaked)
	assert.NotContains(t, err.Error(), testAPIKey)
}

func TestAPIClientAuthenticationFailureNeverEchoesTheKey(t *testing.T) {
	f := katafake.New(t)
	f.Token = "a-different-credential"
	c := apiClientFor(t, f, bindWorkspace(t, "reviews"))
	_, err := c.List(t.Context(), ListOpts{})
	require.ErrorIs(t, err, ErrAuthenticationRequired)
	assert.NotContains(t, err.Error(), testAPIKey)
}

func TestAPIClientTimesOut(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		hung := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			<-r.Context().Done()
			return nil, r.Context().Err()
		})}
		cfg := config.KataIntegrationConfig{Enabled: true, Endpoint: "https://kata.example.com", APIKey: testAPIKey, DefaultProject: "reviews"}
		c, err := NewAPIClient(t.TempDir(), cfg, WithHTTPClient(hung), WithTimeout(2*time.Second))
		require.NoError(t, err)
		t.Cleanup(c.CloseIdleConnections)
		started := time.Now()
		errc := make(chan error, 1)
		go func() { _, err := c.List(context.Background(), ListOpts{}); errc <- err }()
		synctest.Wait()
		assert.Empty(t, errc, "request returned before the configured deadline")
		require.ErrorIs(t, <-errc, ErrUnreachable)
		assert.Equal(t, 2*time.Second, time.Since(started), "request returns at the configured deadline")
	})
}

func lastPost(t *testing.T, f *katafake.Server) katafake.Request {
	t.Helper()
	reqs := f.Requests()
	for _, r := range slices.Backward(reqs) {
		if r.Method == http.MethodPost {
			return r
		}
	}
	require.Fail(t, "no POST was sent")
	return katafake.Request{}
}

func newStatusServer(t *testing.T, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) }))
	t.Cleanup(srv.Close)
	return srv
}

func apiClientFromURL(t *testing.T, rawURL string) *APIClient {
	t.Helper()
	cfg := config.KataIntegrationConfig{Enabled: true, Endpoint: rawURL, APIKey: testAPIKey, DefaultProject: "reviews"}
	c, err := NewAPIClient(t.TempDir(), cfg)
	require.NoError(t, err)
	t.Cleanup(c.CloseIdleConnections)
	return c
}
