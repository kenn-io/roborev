package kata

import (
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gen "go.kenn.io/kata/pkg/client/generated"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/kata/katafake"
	"go.kenn.io/roborev/internal/testenv"
	"go.kenn.io/roborev/internal/testutil"
)

func TestMain(m *testing.M) {
	os.Exit(testenv.RunIsolatedMain(m))
}

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

func namedWorkspace(t *testing.T, project string) string {
	t.Helper()
	dir := t.TempDir()
	if project != "" {
		require.NoError(t, os.WriteFile(filepath.Join(dir, ".kata.local.toml"), []byte("version = 1\n[project]\nname = \""+project+"\"\n"), 0o600))
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

	t.Run("no binding and no default", func(t *testing.T) {
		c := apiClientFor(t, f, namedWorkspace(t, ""), func(c *config.KataIntegrationConfig) { c.DefaultProject = "" })
		_, err := c.Binding(ctx)
		require.ErrorIs(t, err, ErrNoBinding)
	})
	t.Run("a broken binding file stops instead of using the default", func(t *testing.T) {
		dir := namedWorkspace(t, "")
		require.NoError(t, os.WriteFile(filepath.Join(dir, ".kata.local.toml"), []byte("not = = toml"), 0o600))
		_, err := apiClientFor(t, f, dir).Binding(ctx)
		require.Error(t, err)
		require.NotErrorIs(t, err, ErrNoBinding)
	})
}

func TestAPIClientProjectPrecedenceOnCreate(t *testing.T) {
	tests := []struct {
		project, wantPath string
		wantErr           error
	}{
		{"other", "/api/v1/projects/8/issues", nil},
		{"ghost", "", ErrProjectNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.project, func(t *testing.T) {
			f := katafake.New(t)
			c := apiClientFor(t, f, namedWorkspace(t, "reviews"))
			_, err := c.Create(t.Context(), CreateReq{Title: "t", Project: tt.project, IdempotencyKey: "k"})
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
		{"0.21.0-rc.1", true},
		{"0.27.0", false},
		{"0.28.0", false},
		{"v0.28.0", false},
		{"0.28.0+build.1", false},
		{"0.28.1", true},
		{"v0.30.1", true},
		{"1.0.0", true},
		{"99.0.0", true},
		{"0.20.9", true},
		{"0.9.0", true},
		{"", true},
		{"banana", true},
		{"0.21", true},
	} {
		f := katafake.New(t)
		f.APISchema = tc.schema
		_, err := apiClientFor(t, f, namedWorkspace(t, "reviews")).List(t.Context(), ListOpts{})
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

// FuzzCompatibleNumericAPISchema covers numeric release versions; the schema
// gate table owns malformed input, prereleases, and build metadata.
func FuzzCompatibleNumericAPISchema(f *testing.F) {
	f.Add(uint64(0), uint64(21), uint64(0))
	f.Add(uint64(0), uint64(20), uint64(255))
	f.Add(uint64(0), uint64(28), uint64(0))
	f.Add(uint64(0), uint64(28), uint64(1))
	f.Add(uint64(1), uint64(0), uint64(0))
	f.Fuzz(func(t *testing.T, major, minor, patch uint64) {
		version := strconv.FormatUint(major, 10) + "." + strconv.FormatUint(minor, 10) + "." + strconv.FormatUint(patch, 10)
		want := major == 0 && minor >= 21 && (minor < 28 || minor == 28 && patch == 0)
		require.Equal(t, want, compatibleAPISchema(version), version)
		require.Equal(t, want, compatibleAPISchema("v"+version), "v"+version)
	})
}

func TestAPIClientProjectLookupSkipsInactive(t *testing.T) {
	f := katafake.New(t)
	f.Projects = append(f.Projects, gen.ProjectOut{ID: 9, Name: "retired", Active: false, UID: "P9"})
	deleted := time.Unix(1, 0).UTC()
	f.Projects = append(f.Projects, gen.ProjectOut{ID: 10, Name: "gone", Active: true, DeletedAt: &deleted, UID: "P10"})
	c := apiClientFor(t, f, namedWorkspace(t, "retired"))
	_, err := c.List(t.Context(), ListOpts{})
	require.ErrorIs(t, err, ErrProjectNotFound)

	c = apiClientFor(t, f, namedWorkspace(t, "gone"))
	_, err = c.List(t.Context(), ListOpts{})
	require.ErrorIs(t, err, ErrProjectNotFound)
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
	c := apiClientFor(t, f, namedWorkspace(t, "reviews"))

	issues, err := c.List(t.Context(), ListOpts{})
	require.NoError(t, err)
	require.Len(t, issues, 1)
	assert.Equal(t, Issue{
		ShortID: "abc4", QualifiedID: "reviews#abc4", Title: "Title", Body: "Body", Status: "open", Labels: []string{"bug", "p1"},
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
	f.ListPages = map[string]gen.ListIssuesResponseBody{
		"":   {Issues: []gen.IssueOut{{ShortID: "a1", QualifiedID: "reviews#a1", Status: "open"}}, NextCursor: new("c1"), Truncated: true},
		"c1": {Issues: []gen.IssueOut{{ShortID: "a2", QualifiedID: "reviews#a2", Status: "open"}}, NextCursor: new("c2"), Truncated: true},
		"c2": {Issues: []gen.IssueOut{{ShortID: "a3", QualifiedID: "reviews#a3", Status: "open"}}, Complete: true},
	}
	c := apiClientFor(t, f, namedWorkspace(t, "reviews"))
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
		assert.Equal(t, "200", r.Query.Get("limit"), "without a limit Kata returns no cursor")
	}
	assert.Equal(t, []string{"", "c1", "c2"}, []string{reqs[1].Query.Get("cursor"), reqs[2].Query.Get("cursor"), reqs[3].Query.Get("cursor")})
}

func TestAPIClientListUnlimitedStopsOnARepeatedCursor(t *testing.T) {
	f := katafake.New(t)
	f.ListPages = map[string]gen.ListIssuesResponseBody{
		"":     {Issues: []gen.IssueOut{{ShortID: "a1", Status: "open"}}, NextCursor: new("same"), Truncated: true},
		"same": {Issues: []gen.IssueOut{{ShortID: "a2", Status: "open"}}, NextCursor: new("same"), Truncated: true},
	}
	c := apiClientFor(t, f, namedWorkspace(t, "reviews"))
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
		wantBody := ""
		if schema == "0.26.9" {
			wantBody = strings.Repeat("x", (16<<20)+1)
		}
		f.Override = func(w http.ResponseWriter, r *http.Request) bool {
			if r.URL.Path != "/api/v1/projects/7/issues" {
				return false
			}
			f.Reply(w, map[string]any{"issues": []gen.IssueOut{
				{ShortID: "a1", Status: "open", Body: wantBody}, {ShortID: "a2", Status: "open"}, {ShortID: "a3", Status: "open"},
			}})
			return true
		}
		c := apiClientFor(t, f, namedWorkspace(t, "reviews"))
		issues, err := c.List(t.Context(), ListOpts{Unlimited: true})
		require.NoError(t, err, schema)
		require.Len(t, issues, 3, schema)
		assert.Equal(t, wantBody, issues[0].Body, "legacy responses retain the complete body")
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

func TestAPIClientListRejectsInvalidPages(t *testing.T) {
	for _, tc := range []struct {
		name      string
		schema    string
		unlimited bool
		body      string
	}{
		{name: "unfinished final page", schema: "0.28.0", unlimited: true, body: `{"issues":[{"short_id":"a1","status":"open"}],"complete":false,"truncated":true}`},
		{name: "bounded list missing issues", schema: "0.28.0", body: `{}`},
		{name: "legacy list missing issues", schema: "0.26.9", unlimited: true, body: `{}`},
		{name: "paginated list missing issues", schema: "0.28.0", unlimited: true, body: `{"complete":true}`},
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
			c := apiClientFor(t, f, namedWorkspace(t, "reviews"))
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
	c := apiClientFor(t, f, namedWorkspace(t, "reviews"))

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

	byLowerUID, err := c.Show(t.Context(), strings.ToLower(issue.Out.UID))
	require.NoError(t, err, "commit-message refs arrive lowercased")
	assert.Equal(t, "abc4", byLowerUID.ShortID)

	qualified, err := c.Show(t.Context(), "other#zzz9")
	require.NoError(t, err)
	assert.Equal(t, "other#zzz9", qualified.QualifiedID, "a qualified ref selects its own project")
}

func TestAPIClientShowRejectsHostileRefsWithoutARequest(t *testing.T) {
	f := katafake.New(t)
	c := apiClientFor(t, f, namedWorkspace(t, "reviews"))
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
	c := apiClientFor(t, f, namedWorkspace(t, "reviews"))
	_, err := c.Show(t.Context(), "abc4")
	require.ErrorIs(t, err, ErrInvalidResponse)
}

func TestAPIClientCreateSendsWireFieldsAndRecordsAttribution(t *testing.T) {
	for _, principal := range []string{"", "service-reviewer"} {
		t.Run("principal="+principal, func(t *testing.T) {
			f := katafake.New(t)
			f.Principal = principal
			c := apiClientFor(t, f, namedWorkspace(t, "reviews"))
			priority := 0
			res, err := c.Create(t.Context(), CreateReq{
				Title: "Review failed", Body: "body", Labels: []string{"roborev", "review-failed"}, Priority: &priority,
				IdempotencyKey: "roborev:job:1:review.failed",
			})
			require.NoError(t, err)
			assert := assert.New(t)
			assert.NotEmpty(res.ShortID)
			assert.False(res.Reused)
			author := principal
			if author == "" {
				author = "roborev"
			}
			assert.Equal(author, res.Attribution.Author, "record Kata's actual author")
			post := lastPost(t, f)
			assert.Equal("roborev:job:1:review.failed", post.Header.Get("Idempotency-Key"))
			body := post.JSON(t)
			assert.Equal("roborev", body["actor"], "the requested actor never impersonates the credential's principal")
			assert.Equal("Review failed", body["title"])
			assert.Equal("body", body["body"])
			assert.Equal([]any{"roborev", "review-failed"}, body["labels"])
			assert.InDelta(0, body["priority"], 0, "a zero priority is sent")
			assert.NotContains(body, "force_new", "a look-alike is surfaced, as the CLI does")
			assert.Equal("Bearer "+testAPIKey, post.Header.Get("Authorization"))
		})
	}
}

func TestAPIClientAuthenticationFailureNeverEchoesTheKey(t *testing.T) {
	f := katafake.New(t)
	f.Token = "a-different-credential"
	c := apiClientFor(t, f, namedWorkspace(t, "reviews"))
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
		errc := make(chan error, 1)
		go func() { _, err := c.List(t.Context(), ListOpts{}); errc <- err }()
		synctest.Wait()
		assert.Empty(t, errc, "request returned before the configured deadline")
		require.ErrorIs(t, <-errc, ErrUnreachable)
	})
}

func lastPost(t *testing.T, f *katafake.Server) katafake.Request {
	t.Helper()
	var posts []katafake.Request
	for _, r := range f.Requests() {
		if r.Method == http.MethodPost {
			posts = append(posts, r)
		}
	}
	require.NotEmpty(t, posts, "no POST was sent")
	return posts[len(posts)-1]
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

// A reused client must not turn an earlier successful lookup into authority
// for a later request after the server or destination has changed.
func TestAPIClientRechecksCompatibilityAndProject(t *testing.T) {
	for _, operation := range []struct {
		name string
		run  func(*APIClient) error
	}{
		{"binding", func(c *APIClient) error { _, err := c.Binding(t.Context()); return err }},
		{"list", func(c *APIClient) error { _, err := c.List(t.Context(), ListOpts{}); return err }},
		{"show", func(c *APIClient) error { _, err := c.Show(t.Context(), "abc4"); return err }},
		{"create", func(c *APIClient) error {
			_, err := c.Create(t.Context(), CreateReq{Title: "review findings", IdempotencyKey: "retry"})
			return err
		}},
		{"evaluate", nil},
	} {
		for _, change := range []struct {
			name  string
			apply func(*katafake.Server)
			want  error
			state State
		}{
			{"renamed project", func(f *katafake.Server) { f.Projects[0].Name = "renamed" }, ErrProjectNotFound, StateProjectNotFound},
			{"incompatible health", func(f *katafake.Server) { f.APISchema = "0.20.0" }, ErrIncompatible, StateIncompatible},
		} {
			t.Run(operation.name+"/"+change.name, func(t *testing.T) {
				f := katafake.New(t)
				f.AddIssue(7, "abc4", "Task", "")
				c := apiClientFor(t, f, namedWorkspace(t, "reviews"))
				_, err := c.List(t.Context(), ListOpts{})
				require.NoError(t, err)
				before := len(f.AllRequests())
				change.apply(f)
				check := func(want error, state State) {
					if operation.run == nil {
						assert.Equal(t, state, c.EvaluateProject(t.Context(), "").State)
						return
					}
					require.ErrorIs(t, operation.run(c), want)
				}
				check(change.want, change.state)
				for _, r := range f.AllRequests()[before:] {
					assert.NotContains(t, r.Path, "/issues", "no issue operation can reuse stale authorization")
				}
				if errors.Is(change.want, ErrProjectNotFound) {
					f.Projects[0].Name = "reviews"
					check(nil, StateReady)
				}
			})
		}
	}
}

func FuzzAPIClientIdempotencyHeader(f *testing.F) {
	for _, key := range []string{"", "opaque:key/with?punctuation", "tab\tkey", "é", "\xff", "bad\nkey", "bad\x00key", "bad\x7fkey"} {
		f.Add(key)
	}
	f.Fuzz(func(t *testing.T, key string) {
		var posted []string
		client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			body := `{"api_schema_version":"0.28.0"}`
			switch r.URL.Path {
			case "/api/v1/projects":
				body = `{"projects":[{"id":7,"name":"reviews","active":true}]}`
			case "/api/v1/projects/7/issues":
				posted = append(posted, r.Header.Get("Idempotency-Key"))
				body = `{"issue":{"short_id":"abc4"}}`
			}
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
		})}
		c, err := NewAPIClient("", config.KataIntegrationConfig{
			Enabled: true, Endpoint: "https://kata.example.com",
		}, WithHTTPClient(client))
		require.NoError(t, err)
		t.Cleanup(c.CloseIdleConnections)
		_, err = c.Create(t.Context(), CreateReq{Title: "retry test", Project: "reviews", IdempotencyKey: key})
		valid := true
		for i := range len(key) {
			if key[i] < 0x20 && key[i] != '\t' || key[i] == 0x7f {
				valid = false
			}
		}
		if !valid {
			require.ErrorIs(t, err, ErrRequestRejected)
			require.Empty(t, posted, "invalid headers cause no write")
			return
		}
		require.NoError(t, err)
		require.Equal(t, []string{key}, posted, "valid opaque keys reach the transport unchanged")
	})
}

// Kata's DiscoverPaths stops the .kata.toml search at the nearest .git marker.
// An unbound nested Git workspace should therefore use the configured default.
func TestAPIClientBindingDoesNotEscapeGitWorkspace(t *testing.T) {
	for _, tc := range []struct{ name, dflt string }{
		{"directory", "other"},
		{"file", "other"},
		{"physical repository", "reviews"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := katafake.New(t)
			parent := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(parent, ".kata.toml"), []byte("version = 1\n[project]\nname = \"reviews\"\n"), 0o600))
			workspace := filepath.Join(parent, "nested")
			require.NoError(t, os.Mkdir(workspace, 0o700))
			if tc.name == "file" {
				require.NoError(t, os.WriteFile(filepath.Join(workspace, ".git"), []byte("gitdir: ../git-metadata\n"), 0o600))
			} else {
				require.NoError(t, os.Mkdir(filepath.Join(workspace, ".git"), 0o700))
			}
			if tc.name == "physical repository" {
				for dir, project := range map[string]string{parent: "reviews", workspace: "other"} {
					if dir == parent {
						require.NoError(t, os.Mkdir(filepath.Join(dir, ".git"), 0o700))
					}
					for _, filename := range []string{".kata.toml", ".kata.local.toml"} {
						require.NoError(t, os.WriteFile(filepath.Join(dir, filename), []byte("version = 1\n[project]\nname = \""+project+"\"\n"), 0o600))
					}
				}
				sub := filepath.Join(workspace, "sub")
				require.NoError(t, os.Mkdir(sub, 0o700))
				linked := filepath.Join(parent, "linked")
				err := os.Symlink(sub, linked)
				if runtime.GOOS == "windows" && errors.Is(err, syscall.Errno(1314)) { // ERROR_PRIVILEGE_NOT_HELD
					t.Skip("directory symlinks require Windows privileges or developer mode")
				}
				require.NoError(t, err)
				workspace = linked
			}
			c := apiClientFor(t, f, workspace, func(cfg *config.KataIntegrationConfig) { cfg.DefaultProject = tc.dflt })
			_, err := c.Create(t.Context(), CreateReq{Title: "Git-bounded workspace selection", IdempotencyKey: "review:git-boundary"})
			require.NoError(t, err)
			assert.Equal(t, "/api/v1/projects/8/issues", lastPost(t, f).Path, "the lexical ancestor cannot select the destination")
		})
	}
}

// Implicit workspace selection follows a registered alias, while explicit
// selection keeps its name. A resolver refusal must never trigger attachment
// or a fallback request, even when the old project name still exists.
func TestAPIClientRegisteredAliasSelection(t *testing.T) {
	const project = "version = 1\n[project]\n"
	const reviews = project + "name = \"reviews\"\n"
	const other = project + "name = \"other\"\n"
	for _, tc := range []struct {
		name, binding, local, project, schema, localName string
		status                                           int
		code                                             string
		wantErr                                          error
		alias                                            bool
	}{
		{name: "implicit canonical", binding: reviews, project: "other", schema: "0.21.0", alias: true},
		{name: "local override", binding: other, local: reviews, project: "reviews"},
		{name: "default", project: "other"},
		{name: "unknown alias", binding: reviews, status: 404, code: "project_not_initialized", wantErr: ErrProjectNotFound, alias: true},
		{name: "bootstrap refused", binding: reviews, status: 403, code: "bootstrap_token_write_forbidden", wantErr: ErrAuthenticationRequired, alias: true},
		{name: "issue scope refused", binding: reviews, status: 403, code: "scoped_operation_forbidden", wantErr: ErrAuthenticationRequired, alias: true},
		{name: "proxy actor absent", binding: reviews, status: 400, code: "actor_header_required", wantErr: ErrRequestRejected, alias: true},
		{name: "committed legacy identity", binding: project + "identity = \"code.example/team/reviews\"\n", project: "reviews", alias: true},
		{name: "committed trim", binding: project + "name = \"  reviews  \"\n", project: "other", localName: "reviews", alias: true},
		{name: "local trim", binding: other, local: project + "name = \"  reviews  \"\n", project: "reviews", localName: "reviews"},
		{name: "committed version unsupported", binding: "version = 2\n[project]\nname = \"reviews\"\n", wantErr: ErrIncompatible},
		{name: "local version unsupported", binding: reviews, local: "version = 2\n[project]\nname = \"reviews\"\n", wantErr: ErrIncompatible},
		{name: "committed empty after trim", binding: project + "name = \"  \"\n", wantErr: ErrIncompatible},
		{name: "committed control", binding: project + "name = \"rev\\niews\"\n", wantErr: ErrIncompatible},
		{name: "local control", binding: reviews, local: project + "name = \"rev\\niews\"\n", wantErr: ErrIncompatible},
		{name: "local empty after trim is optional", binding: reviews, local: project + "name = \"  \"\n", project: "other", alias: true},
		{name: "local server only keeps base", binding: reviews, local: "version = 1\n[server]\nurl = \"https://kata.example\"\n", project: "other", alias: true},
		{name: "local legacy identity is ignored", binding: reviews, local: project + "identity = \"code.example/team/other\"\n", project: "other", alias: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			f := katafake.New(t)
			if tc.schema != "" {
				f.APISchema = tc.schema
			}
			workspace := t.TempDir()
			for filename, contents := range map[string]string{".kata.toml": tc.binding, ".kata.local.toml": tc.local} {
				if contents != "" {
					require.NoError(t, os.WriteFile(filepath.Join(workspace, filename), []byte(contents), 0o600))
				}
			}
			f.Override = func(w http.ResponseWriter, r *http.Request) bool {
				if r.URL.Path != "/api/v1/projects/resolve" {
					return false
				}
				assert.Equal(http.MethodPost, r.Method)
				var body map[string]any
				assert.NoError(json.UnmarshalRead(r.Body, &body))
				assert.Equal(map[string]any{"alias": map[string]any{"identity": "local://" + workspace, "kind": "local"}}, body,
					"neither name nor start_path may authorize first-seen attachment")
				if tc.status != 0 {
					f.Fail(w, tc.status, tc.code, "synthetic resolver refusal")
				} else {
					canonical := f.Projects[1]
					if tc.project == "reviews" {
						canonical = f.Projects[0]
					}
					f.Reply(w, gen.ProjectResolveBody{Project: canonical, Alias: gen.ProjectAlias{
						AliasIdentity: "local://" + workspace, AliasKind: "local", ID: 1, ProjectID: canonical.ID, CreatedAt: time.Unix(0, 0).UTC(),
					}})
				}
				return true
			}
			if tc.localName != "" {
				binding, err := NewCLIClient(workspace).Binding(t.Context())
				require.NoError(t, err)
				assert.Equal(tc.localName, binding.Project)
			}
			c := apiClientFor(t, f, workspace, func(cfg *config.KataIntegrationConfig) { cfg.DefaultProject = "other" })
			_, err := c.Create(t.Context(), CreateReq{Title: "Alias-selected review"})
			require.ErrorIs(t, err, tc.wantErr)
			assert.Equal(tc.code, ErrorCode(err))
			if errors.Is(tc.wantErr, ErrIncompatible) {
				assert.Empty(f.AllRequests(), "invalid local input must not contact the server or select a default")
				return
			}
			var requests []string
			for _, r := range f.AllRequests() {
				requests = append(requests, r.Method+" "+r.Path)
			}
			wantRequests := []string{"GET /api/v1/health", "GET /api/v1/projects"}
			if tc.alias {
				wantRequests[1] = "POST /api/v1/projects/resolve"
			}
			if tc.wantErr == nil {
				path := map[string]string{"reviews": "/api/v1/projects/7/issues", "other": "/api/v1/projects/8/issues"}[tc.project]
				wantRequests = append(wantRequests, "POST "+path)
			}
			assert.Equal(wantRequests, requests, "one fresh selection, no fallback or writes after refusal")
			if tc.binding != "" {
				data, err := os.ReadFile(filepath.Join(workspace, ".kata.toml"))
				require.NoError(t, err)
				assert.Equal(tc.binding, string(data), "lookup never repairs the binding")
			}
		})
	}
}

// Git identity follows the producer's origin preference and normalization.
// Selecting by the stale file name, another remote, or an unnormalized URL
// must not direct a request at the old project.
func TestAPIClientRegisteredGitAlias(t *testing.T) {
	repo := testutil.NewTestRepo(t)
	repo.RunGit("remote", "add", "backup", "https://code.example/other/repo.git")
	repo.RunGit("remote", "add", "origin", "git@code.example:Team/Review%20Repo.git")
	repo.WriteFile(".kata.toml", "version = 1\n[project]\nname = \"reviews\"\n")
	f := katafake.New(t)
	f.Override = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/api/v1/projects/resolve" {
			return false
		}
		var body map[string]any
		assert.NoError(t, json.UnmarshalRead(r.Body, &body))
		assert.Equal(t, map[string]any{"alias": map[string]any{"identity": "code.example/Team/Review-Repo", "kind": "git"}}, body)
		f.Reply(w, gen.ProjectResolveBody{Project: f.Projects[1], Alias: gen.ProjectAlias{
			AliasIdentity: "code.example/Team/Review-Repo", AliasKind: "git", ID: 1, ProjectID: 8, CreatedAt: time.Unix(0, 0).UTC(),
		}})
		return true
	}
	b, err := apiClientFor(t, f, repo.Root).Binding(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "other", b.Project)
	reqs := f.Requests()
	require.Len(t, reqs, 1)
	assert.Equal(t, "/api/v1/projects/resolve", reqs[0].Path)
}
