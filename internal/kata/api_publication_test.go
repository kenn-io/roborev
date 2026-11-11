package kata

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gen "go.kenn.io/kata/pkg/client/generated"

	"go.kenn.io/roborev/internal/kata/katafake"
	"go.kenn.io/roborev/pkg/structuredreview"
)

func publicationFixture(t *testing.T, policy string) *Publication {
	t.Helper()
	doc := structuredreview.Document{SchemaVersion: structuredreview.SchemaVersion, Summary: "private summary", SourceLabels: []string{"private source"}, Findings: []structuredreview.Finding{{Severity: "high", Problem: "private problem", Fix: "private fix", Location: "private/path.go:4", Sources: []int{1}}}}
	p, err := PreparePublication(uuid.MustParse("11111111-1111-4111-8111-111111111111"), strings.Repeat("a", 64), time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), doc, policy, "reviewer")
	require.NoError(t, err)
	return p
}

func TestPublicationRenderingPreflightAndFingerprint(t *testing.T) {
	t.Parallel()
	comments := publicationFixture(t, "comments")
	summary := publicationFixture(t, "summary")
	assert.Contains(t, comments.roots[0].Body, "private problem")
	assert.Contains(t, comments.roots[0].Body, "private fix")
	assert.Contains(t, comments.roots[0].Body, "private/path.go:4")
	assert.Contains(t, comments.roots[0].Body, "private source")
	issueJSON, err := encodeRequest(summary.issue)
	require.NoError(t, err)
	rootJSON, err := encodeRequest(summary.roots[0])
	require.NoError(t, err)
	for _, secret := range []string{"private summary", "private problem", "private fix", "private/path", "private source"} {
		assert.NotContains(t, string(issueJSON), secret)
		assert.NotContains(t, string(rootJSON), secret)
	}
	assert.Equal(t, comments.Fingerprint, publicationFixture(t, "comments").Fingerprint)
	assert.NotEqual(t, comments.Fingerprint, summary.Fingerprint)
	assert.Contains(t, comments.IssueKey(), comments.ReviewUUID.String())
	key, err := comments.RootKey(0)
	require.NoError(t, err)
	assert.Contains(t, key, comments.digests[0])
	require.True(t, comments.MatchesIssueEnvelope(comments.issue.Metadata))
	assert.False(t, summary.MatchesIssueEnvelope(comments.issue.Metadata))

	// JSON escaping makes the actual request larger than the finding text.
	doc := structuredreview.Document{SchemaVersion: structuredreview.SchemaVersion, Summary: "summary", Findings: []structuredreview.Finding{{Severity: "high", Problem: "first", Fix: "fix"}, {Severity: "high", Problem: strings.Repeat("\"", PublicationMaxRequestBytes/2), Fix: "fix"}}}
	_, err = PreparePublication(comments.ReviewUUID, strings.Repeat("a", 64), comments.CompletedAt, doc, "comments", "reviewer")
	require.ErrorIs(t, err, ErrRequestRejected)
	// Summary mode must not serialize even an oversized finding.
	_, err = PreparePublication(comments.ReviewUUID, strings.Repeat("a", 64), comments.CompletedAt, doc, "summary", "reviewer")
	require.NoError(t, err)
}

func TestPublicationRootStates(t *testing.T) {
	t.Parallel()
	p := publicationFixture(t, "comments")
	handle := "c:abc123"
	comment := gen.CommentOut{UID: "comment-uid", Handle: &handle, Body: p.roots[0].Body, Author: "actual-author"}
	roots := p.Roots(PublicationIssue{Comments: []gen.CommentOut{comment}})
	require.Len(t, roots, 1)
	assert.Equal(t, "complete", roots[0].State)
	assert.Equal(t, "actual-author", roots[0].Comment.Author)
	assert.Equal(t, "ambiguous", p.Roots(PublicationIssue{Comments: []gen.CommentOut{comment, comment}})[0].State)
	comment.Body += " edited"
	assert.Equal(t, "edited", p.Roots(PublicationIssue{Comments: []gen.CommentOut{comment}})[0].State)
	comment.Body = p.roots[0].Body
	comment.Handle = nil
	assert.Equal(t, "unavailable", p.Roots(PublicationIssue{Comments: []gen.CommentOut{comment}})[0].State)
	assert.Equal(t, "missing", p.Roots(PublicationIssue{})[0].State)
	assert.Equal(t, "truncated", p.Roots(PublicationIssue{Truncated: true})[0].State)
	comment.Handle = &handle
	assert.Equal(t, "truncated", p.Roots(PublicationIssue{Comments: []gen.CommentOut{comment}, Truncated: true})[0].State)
}

func TestPublicationSchemaAndProjectResolution(t *testing.T) {
	t.Parallel()
	f := katafake.New(t)
	c := apiClientFor(t, f, namedWorkspace(t, "reviews"))
	project, err := c.ResolvePublicationProject(t.Context(), "")
	require.NoError(t, err)
	assert.Equal(t, int64(7), project.ID)
	assert.Equal(t, "reviews", project.Name)
	assert.NotEmpty(t, project.UID)
	f.APISchema = "0.27.0"
	_, err = c.ResolvePublicationProject(t.Context(), "")
	require.ErrorIs(t, err, ErrIncompatible)
	_, err = c.Binding(t.Context())
	require.NoError(t, err, "legacy binding retains its older compatibility")
	endpoint, err := NormalizePublicationEndpoint("https://EXAMPLE.test:443/base/")
	require.NoError(t, err)
	assert.Equal(t, "https://example.test/base", endpoint)
	endpoint, err = NormalizePublicationEndpoint("unix:///tmp/example.sock")
	require.NoError(t, err)
	assert.Equal(t, "unix:///tmp/example.sock", endpoint)
}

func TestPublicationSearchPaginationAndCompleteShow(t *testing.T) {
	t.Parallel()
	f := katafake.New(t)
	c := apiClientFor(t, f, namedWorkspace(t, "reviews"))
	project, err := c.ResolvePublicationProject(t.Context(), "")
	require.NoError(t, err)
	p := publicationFixture(t, "comments")
	cursor := "next"
	f.ListPages = map[string]gen.ListIssuesResponseBody{
		"":     {Issues: []gen.IssueOut{{UID: "issue-1", Status: "closed", Metadata: p.issue.Metadata}}, NextCursor: &cursor},
		"next": {Issues: []gen.IssueOut{{UID: "issue-2", Status: "open", Metadata: p.issue.Metadata}}, Complete: true},
	}
	issues, err := c.SearchPublicationIssues(t.Context(), project, p.ReviewUUID)
	require.NoError(t, err)
	require.Len(t, issues, 2)
	assert.Equal(t, "closed", issues[0].Status)
	for _, req := range f.Requests() {
		if req.Path == "/api/v1/projects/7/issues" {
			assert.Equal(t, publicationReviewKey+"="+p.ReviewUUID.String(), req.Query.Get("meta"))
			assert.Empty(t, req.Query.Get("status"))
		}
	}
	f.ListPages["next"] = gen.ListIssuesResponseBody{Issues: []gen.IssueOut{}, NextCursor: &cursor}
	_, err = c.SearchPublicationIssues(t.Context(), project, p.ReviewUUID)
	require.ErrorIs(t, err, ErrInvalidResponse)
	f.ListPages["next"] = gen.ListIssuesResponseBody{Issues: []gen.IssueOut{}, Truncated: true, Complete: true}
	_, err = c.SearchPublicationIssues(t.Context(), project, p.ReviewUUID)
	require.ErrorIs(t, err, ErrInvalidResponse)

	issue := f.AddIssue(7, "x001", "title", "body")
	issue.Out.Metadata = p.issue.Metadata
	shown, err := c.ShowPublicationIssue(t.Context(), project, "x001")
	require.NoError(t, err)
	assert.False(t, shown.Truncated)
	assert.True(t, p.MatchesIssueEnvelope(shown.Issue.Metadata))
	f.Override = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/7/issues/x001" {
			f.Reply(w, map[string]any{"issue": map[string]any{"uid": issue.Out.UID, "short_id": "x001", "project_id": 7}, "comments_truncated": true})
			return true
		}
		return false
	}
	shown, err = c.ShowPublicationIssue(t.Context(), project, "x001")
	require.NoError(t, err)
	assert.True(t, shown.Truncated)
}

func TestPublicationWriteFailuresReachTargetAndPreserveServerErrors(t *testing.T) {
	t.Parallel()
	for _, root := range []bool{false, true} {
		t.Run(fmt.Sprint(root), func(t *testing.T) {
			f := katafake.New(t)
			c := apiClientFor(t, f, namedWorkspace(t, "reviews"))
			p := publicationFixture(t, "comments")
			project, err := c.ResolvePublicationProject(t.Context(), "")
			require.NoError(t, err)
			target := "/api/v1/projects/7/issues"
			if root {
				target += "/issue-uid/comments"
			}
			f.Override = func(w http.ResponseWriter, r *http.Request) bool {
				if r.Method == http.MethodPost && r.URL.Path == target {
					f.Fail(w, http.StatusConflict, "idempotency_mismatch", "synthetic replay conflict")
					return true
				}
				return false
			}
			if root {
				_, err = c.CreatePublicationRoot(t.Context(), project, "issue-uid", p, 0)
			} else {
				_, err = c.CreatePublicationIssue(t.Context(), project, p)
			}
			require.ErrorIs(t, err, ErrConflict)
			assert.Equal(t, "idempotency_mismatch", ErrorCode(err))
			var reached bool
			for _, req := range f.Requests() {
				if req.Method == http.MethodPost && req.Path == target {
					reached = true
					assert.NotEmpty(t, req.Header.Get("Idempotency-Key"))
				}
			}
			assert.True(t, reached)
		})
	}
}

func TestPublicationGuardedPostContext(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	f := katafake.New(t)
	c := apiClientFor(t, f, namedWorkspace(t, "reviews"))
	_, err := c.CreatePublicationIssue(ctx, PublicationProject{ID: 7, Name: "reviews", UID: "uid"}, publicationFixture(t, "summary"))
	require.ErrorIs(t, err, context.Canceled)
}

func TestPublicationPostsUsePreflightedBodiesAndServerAttribution(t *testing.T) {
	t.Parallel()
	f := katafake.New(t)
	c := apiClientFor(t, f, namedWorkspace(t, "reviews"))
	p := publicationFixture(t, "comments")
	project, err := c.ResolvePublicationProject(t.Context(), "")
	require.NoError(t, err)
	f.Override = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodPost {
			return false
		}
		switch r.URL.Path {
		case "/api/v1/projects/7/issues":
			f.Reply(w, map[string]any{"issue": map[string]any{"uid": "issue-uid", "project_id": 7, "metadata": p.issue.Metadata, "author": "server-user"}})
			return true
		case "/api/v1/projects/7/issues/issue-uid/comments":
			f.Reply(w, map[string]any{"issue": map[string]any{"uid": "issue-uid"}, "comment": map[string]any{"uid": "comment-uid", "body": p.roots[0].Body, "author": "server-user"}})
			return true
		}
		return false
	}
	created, err := c.CreatePublicationIssue(t.Context(), project, p)
	require.NoError(t, err)
	assert.Equal(t, "server-user", created.Issue.Author)
	root, err := c.CreatePublicationRoot(t.Context(), project, "issue-uid", p, 0)
	require.NoError(t, err)
	assert.Equal(t, "server-user", root.Comment.Author)
	issueBytes, err := encodeRequest(p.issue)
	require.NoError(t, err)
	rootBytes, err := encodeRequest(p.roots[0])
	require.NoError(t, err)
	var posts []katafake.Request
	for _, req := range f.Requests() {
		if req.Method == http.MethodPost {
			posts = append(posts, req)
		}
	}
	require.Len(t, posts, 2)
	assert.Equal(t, issueBytes, posts[0].Body)
	assert.Equal(t, rootBytes, posts[1].Body)
	assert.Equal(t, p.IssueKey(), posts[0].Header.Get("Idempotency-Key"))
	rootKey, err := p.RootKey(0)
	require.NoError(t, err)
	assert.Equal(t, rootKey, posts[1].Header.Get("Idempotency-Key"))
}

func TestPublicationOptionsFingerprint(t *testing.T) {
	t.Parallel()
	doc := structuredreview.Document{SchemaVersion: structuredreview.SchemaVersion, Summary: "summary", Findings: []structuredreview.Finding{{Severity: "high", Problem: "problem", Fix: "fix"}}}
	id := uuid.MustParse("11111111-1111-4111-8111-111111111111")
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	prepare := func(opts PublicationOptions) *Publication {
		p, err := PreparePublicationWithOptions(id, strings.Repeat("a", 64), at, doc, "comments", "reviewer", opts)
		require.NoError(t, err)
		return p
	}
	first := prepare(PublicationOptions{Labels: []string{"z", "a", "z"}, Priority: new(1)})
	equivalent := prepare(PublicationOptions{Labels: []string{"a", "z"}, Priority: new(1)})
	assert.Equal(t, first.Fingerprint, equivalent.Fingerprint)
	assert.NotEqual(t, first.Fingerprint, prepare(PublicationOptions{Labels: []string{"a", "z"}, Priority: new(2)}).Fingerprint)
	assert.NotEqual(t, first.Fingerprint, prepare(PublicationOptions{Labels: []string{"a"}, Priority: new(1)}).Fingerprint)
}
