package kata

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"maps"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gen "go.kenn.io/kata/pkg/client/generated"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/kata/katafake"
	"go.kenn.io/roborev/pkg/structuredreview"
)

func publicationFixture(t *testing.T, policy string) *Publication {
	t.Helper()
	doc := structuredreview.Document{SchemaVersion: structuredreview.SchemaVersion, Summary: "private summary", SourceLabels: []string{"private source"}, Findings: []structuredreview.Finding{{Severity: "high", Problem: "private problem", Fix: "private fix", Location: "private/path.go:4", Sources: []int{1}}}}
	p, err := PreparePublicationWithOptions(uuid.MustParse("11111111-1111-4111-8111-111111111111"), strings.Repeat("a", 64), time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), doc, policy, "reviewer", PublicationOptions{})
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
	_, err = PreparePublicationWithOptions(comments.ReviewUUID, strings.Repeat("a", 64), comments.CompletedAt, doc, "comments", "reviewer", PublicationOptions{})
	require.ErrorIs(t, err, ErrRequestRejected)
	// Summary mode must not serialize even an oversized finding.
	_, err = PreparePublicationWithOptions(comments.ReviewUUID, strings.Repeat("a", 64), comments.CompletedAt, doc, "summary", "reviewer", PublicationOptions{})
	require.NoError(t, err)
}

func TestPublicationFindingPositions(t *testing.T) {
	doc := structuredreview.Document{SchemaVersion: structuredreview.SchemaVersion, Summary: "private summary", SourceLabels: []string{"reviewer"}, Findings: []structuredreview.Finding{
		{Severity: "high", Problem: "first problem", Fix: "first fix"},
		{Severity: "medium", Problem: "second problem", Fix: "second fix", Location: "file.go:12", Sources: []int{1}},
	}}
	id := uuid.MustParse("11111111-1111-4111-8111-111111111111")
	for _, policy := range []string{"comments", "summary"} {
		p, err := PreparePublicationWithOptions(id, strings.Repeat("a", 64), time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), doc, policy, "reviewer", PublicationOptions{})
		require.NoError(t, err)
		require.Len(t, p.roots, 2)
		if policy == "comments" {
			assert.Contains(t, p.roots[0].Body, "### 1. High")
			assert.Contains(t, p.roots[1].Body, "### 2. Medium")
			for _, content := range []string{"second problem", "second fix", "file.go:12", "reviewer"} {
				assert.Contains(t, p.roots[1].Body, content)
			}
		} else {
			assert.Contains(t, p.roots[0].Body, "Finding 1 from a roborev review.")
			assert.Contains(t, p.roots[1].Body, "Finding 2 from a roborev review.")
			assert.NotContains(t, p.roots[1].Body, "second problem")
		}
	}
}

func TestPublicationV1WireFormat(t *testing.T) {
	for _, tc := range []struct{ policy, text, digest, fingerprint string }{
		{"comments", "## Summary\n\nReview finding.\n\n## Findings\n\n### 1. High\n\n**Location:** private/path.go:4\n\n**Problem:** private problem\n\n**Fix:** private fix\n\n**Reported by:** private source\n", "1b90257dc41b56c574ad0bad6a45a244d63c48c02baaaf3c713182719067caae", "1be12e340183d64d5068ca910fdd125b89559fef1cf0cd48fbe0a3bbbd580585"},
		{"summary", "Finding 1 from a roborev review.", "ebb986cabac1ebbb091268e8f7b596661315b2ba0836dd699eaca7c336f5b566", "a9bc0b71b949611c192fbbf34e54a83a8f3b7f09fa374d50c5f5d8badf9ad014"},
	} {
		t.Run(tc.policy, func(t *testing.T) {
			p := publicationFixture(t, tc.policy)
			body := tc.text + "\n\n<!-- roborev:v1 review=11111111-1111-4111-8111-111111111111 position=0 digest=" + tc.digest + " -->"
			assert.Equal(t, body, p.roots[0].Body)
			issue, err := encodeRequest(p.issue)
			require.NoError(t, err)
			expectedIssue := `{"actor":"roborev","body":"A roborev review reported findings. Each finding has a separate comment.","force_new":true,"labels":["roborev"],"metadata":{"roborev.completed_at":"2026-01-02T03:04:05Z","roborev.document_hash":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","roborev.finding_count":1,"roborev.policy":"` + tc.policy + `","roborev.review_uuid":"11111111-1111-4111-8111-111111111111","roborev.version":1},"title":"Roborev review findings"}`
			assert.Equal(t, expectedIssue, string(issue))
			root, err := encodeRequest(p.roots[0])
			require.NoError(t, err)
			assert.Equal(t, `{"actor":"roborev","body":`+strconv.Quote(body)+`,"teammate":"reviewer"}`, string(root))
			assert.Equal(t, tc.fingerprint, p.Fingerprint)
			key, err := p.RootKey(0)
			require.NoError(t, err)
			assert.Equal(t, "roborev:review:11111111-1111-4111-8111-111111111111:0:"+tc.digest, key)
		})
	}
}

func TestPublicationSourceInspectionCannotWrite(t *testing.T) {
	id := uuid.MustParse("11111111-1111-4111-8111-111111111111")
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	doc := structuredreview.Document{SchemaVersion: structuredreview.SchemaVersion, Summary: "summary", SourceLabels: []string{"source"}, Findings: []structuredreview.Finding{
		{Severity: "high", Problem: "first", Fix: "fix"},
		{Severity: "medium", Problem: strings.Repeat("x", PublicationMaxRequestBytes), Fix: "fix", Sources: []int{1}},
	}}
	project := PublicationProject{ID: 7, Name: "reviews", UID: "project-uid"}
	client := &APIClient{}
	for _, policy := range []string{"comments", "summary"} {
		source, err := PublicationFromSource(id, strings.Repeat("a", 64), at, doc, policy)
		require.NoError(t, err)
		assert.False(t, source.ready())
		_, err = client.CreatePublicationIssue(t.Context(), project, source)
		require.ErrorContains(t, err, "not preflighted")
		_, err = client.CreatePublicationRoot(t.Context(), project, "issue-uid", source, 1)
		require.ErrorContains(t, err, "not preflighted")
		text := "replacement"
		comment := gen.CommentOut{UID: "replacement-uid", Handle: new("c:replacement"), Author: "actual", Body: text + "\n\n" + publicationMarker(id, 1, publicationDigest([]byte(text)))}
		roots := source.Roots(PublicationIssue{Comments: []gen.CommentOut{comment}})
		require.Len(t, roots, 2)
		assert.Equal(t, "edited", roots[1].State)
		assert.Equal(t, "actual", roots[1].Comment.Author)
		if policy == "comments" {
			_, err = PreparePublicationWithOptions(id, strings.Repeat("a", 64), at, doc, policy, "reviewer", PublicationOptions{})
			require.ErrorIs(t, err, ErrRequestRejected)
		}
	}
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
	duplicate := comment
	duplicate.UID, duplicate.Handle = "other-comment-uid", new("c:other")
	assert.Equal(t, "ambiguous", p.Roots(PublicationIssue{Comments: []gen.CommentOut{comment, duplicate}})[0].State)
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

func TestReadRemotePublicationEnvelopeAndRoots(t *testing.T) {
	p := publicationFixture(t, "comments")
	envelope, err := ReadPublicationEnvelope(p.issue.Metadata)
	require.NoError(t, err)
	assert.Equal(t, p.ReviewUUID, envelope.ReviewUUID)
	assert.Equal(t, 1, envelope.FindingCount)
	comment := gen.CommentOut{UID: "comment-uid", Handle: new("c:one"), Body: p.roots[0].Body, Author: "actual-author"}
	roots := ReadPublicationRoots(envelope, PublicationIssue{Comments: []gen.CommentOut{comment}})
	require.Len(t, roots, 1)
	assert.Equal(t, "complete", roots[0].State)
	assert.Equal(t, "actual-author", roots[0].Comment.Author)
	comment.Body = "modified" + comment.Body
	assert.Equal(t, "edited", ReadPublicationRoots(envelope, PublicationIssue{Comments: []gen.CommentOut{comment}})[0].State)
	duplicate := comment
	duplicate.UID, duplicate.Handle = "other-comment-uid", new("c:other")
	assert.Equal(t, "ambiguous", ReadPublicationRoots(envelope, PublicationIssue{Comments: []gen.CommentOut{comment, duplicate}})[0].State)
	assert.Equal(t, "truncated", ReadPublicationRoots(envelope, PublicationIssue{Truncated: true})[0].State)
	comment.Body = "marker removed"
	now := time.Now()
	comment.EditedAt = &now
	assert.Equal(t, "ambiguous", ReadPublicationRoots(envelope, PublicationIssue{Comments: []gen.CommentOut{comment}})[0].State)
	assert.Equal(t, "missing", ReadPublicationRoots(envelope, PublicationIssue{})[0].State)

	// A remote count can be arbitrarily large; only observed positions and
	// missing ranges are materialized.
	envelope.FindingCount = int(^uint(0) >> 1)
	comment = gen.CommentOut{UID: "comment-uid", Handle: new("c:one"), Body: p.roots[0].Body}
	roots = ReadPublicationRoots(envelope, PublicationIssue{Comments: []gen.CommentOut{comment}})
	require.Len(t, roots, 2)
	assert.Equal(t, "complete", roots[0].State)
	assert.Equal(t, 1, roots[1].Position)
	require.NotNil(t, roots[1].EndPosition)
	assert.Equal(t, envelope.FindingCount-1, *roots[1].EndPosition)
	assert.Equal(t, "missing", roots[1].State)
	envelope.FindingCount = 3
	roots = ReadPublicationRoots(envelope, PublicationIssue{Comments: []gen.CommentOut{comment}})
	require.Len(t, roots, 2)
	assert.Equal(t, 2, *roots[1].EndPosition)
	assert.Equal(t, "truncated", ReadPublicationRoots(envelope, PublicationIssue{Comments: []gen.CommentOut{comment}, Truncated: true})[1].State)
	lastText := "last finding"
	last := gen.CommentOut{UID: "last", Handle: new("c:last"), Body: lastText + "\n\n" + publicationMarker(envelope.ReviewUUID, 2, publicationDigest([]byte(lastText)))}
	roots = ReadPublicationRoots(envelope, PublicationIssue{Comments: []gen.CommentOut{last}})
	require.Len(t, roots, 2)
	assert.Equal(t, 0, roots[0].Position)
	require.NotNil(t, roots[0].EndPosition)
	assert.Equal(t, 1, *roots[0].EndPosition)
	assert.Equal(t, "missing", roots[0].State)
	assert.Equal(t, 2, roots[1].Position)
	assert.Equal(t, "complete", roots[1].State)
	roots = ReadPublicationRoots(envelope, PublicationIssue{Comments: []gen.CommentOut{last, {Body: "marker removed"}}})
	assert.Equal(t, "ambiguous", roots[0].State)

	doc := structuredreview.Document{SchemaVersion: structuredreview.SchemaVersion, Summary: "summary", Findings: []structuredreview.Finding{{Severity: "high", Problem: "one", Fix: "fix"}, {Severity: "high", Problem: "two", Fix: "fix"}, {Severity: "high", Problem: "three", Fix: "fix"}}}
	source, err := PublicationFromSource(p.ReviewUUID, p.DocumentHash, p.CompletedAt, doc, "comments")
	require.NoError(t, err)
	localRoots := source.Roots(PublicationIssue{})
	require.Len(t, localRoots, 3)
	for i, root := range localRoots {
		assert.Equal(t, i, root.Position)
		assert.Equal(t, "missing", root.State)
		assert.Nil(t, root.EndPosition)
	}
	localRoots = source.Roots(PublicationIssue{Comments: []gen.CommentOut{{UID: "second", Handle: new("c:second"), Body: source.roots[1].Body}}})
	require.Len(t, localRoots, 3)
	assert.Equal(t, "missing", localRoots[0].State)
	assert.Equal(t, "complete", localRoots[1].State)
	assert.Equal(t, "missing", localRoots[2].State)

	for _, key := range []string{publicationVersionKey, publicationReviewKey, publicationHashKey, publicationPolicyKey, publicationTimeKey, publicationCountKey} {
		metadata := maps.Clone(p.issue.Metadata)
		delete(metadata, key)
		_, err := ReadPublicationEnvelope(metadata)
		require.Error(t, err, key)
	}
}

func TestPublicationProjectSelectionUsesFreshObject(t *testing.T) {
	for _, alias := range []bool{false, true} {
		t.Run(fmt.Sprintf("alias=%t", alias), func(t *testing.T) {
			f := katafake.New(t)
			var mode atomic.Int32
			f.Override = func(w http.ResponseWriter, r *http.Request) bool {
				if r.URL.Path != "/api/v1/projects" && r.URL.Path != "/api/v1/projects/resolve" {
					return false
				}
				project := f.Projects[0]
				switch mode.Load() {
				case 1:
					project.UID = "changed-uid"
				case 2:
					project.UID = ""
				case 3:
					project.Active = false
				case 4:
					now := time.Now()
					project.DeletedAt = &now
				}
				if r.URL.Path == "/api/v1/projects/resolve" {
					f.Reply(w, gen.ProjectResolveBody{Project: project})
				} else {
					f.Reply(w, gen.ListProjectsResponseBody{Projects: []gen.ProjectOut{project}})
				}
				return true
			}
			cfg := config.KataIntegrationConfig{Enabled: true, Endpoint: f.URL(), APIKey: testAPIKey, DefaultProject: "reviews"}
			opts := []APIOption{WithoutWorkspace()}
			if alias {
				opts = append(opts, WithWorkspaceAlias(ProjectAlias{Kind: "local", Identity: "synthetic"}))
			}
			c, err := NewAPIClient("", cfg, opts...)
			require.NoError(t, err)
			t.Cleanup(c.CloseIdleConnections)
			first, err := c.ResolvePublicationProject(t.Context(), "")
			require.NoError(t, err)
			assert.Equal(t, f.Projects[0].UID, first.UID)
			mode.Store(1)
			second, err := c.ResolvePublicationProject(t.Context(), "")
			require.NoError(t, err)
			assert.Equal(t, "changed-uid", second.UID)
			for _, value := range []int32{2, 3, 4} {
				mode.Store(value)
				_, err = c.ResolvePublicationProject(t.Context(), "")
				require.ErrorIs(t, err, ErrProjectNotFound)
			}
			var lists, resolves int
			for _, request := range f.Requests() {
				if request.Path == "/api/v1/projects" {
					lists++
				}
				if request.Path == "/api/v1/projects/resolve" {
					resolves++
				}
			}
			if alias {
				assert.Equal(t, 0, lists)
				assert.Equal(t, 5, resolves)
			} else {
				assert.Equal(t, 5, lists)
				assert.Equal(t, 0, resolves)
			}
		})
	}
}

func TestResolvePublicationTarget(t *testing.T) {
	p := publicationFixture(t, "comments")
	project := PublicationProject{ID: 7, Name: "reviews", UID: "project-uid"}
	root := gen.CommentOut{UID: "01AAAAAAAAAAAAAAAAAAABC123", Handle: new("c:abc123"), Body: p.roots[0].Body, IssueID: 10}
	reply := gen.CommentOut{UID: "01BBBBBBBBBBBBBBBBBBDEF456", Handle: new("c:def456"), Body: "reply", IssueID: 10, ReplyToUID: &root.UID, ReplyKind: new("reply")}
	issue := PublicationIssue{Issue: gen.ShowIssueOut{UID: "issue-uid", ID: 10, ProjectID: 7, Metadata: p.issue.Metadata}, Comments: []gen.CommentOut{root, reply}}
	selected, err := ResolvePublicationTarget(project, issue, "c:def456")
	require.NoError(t, err)
	assert.Equal(t, reply.UID, selected)
	selected, err = ResolvePublicationTarget(project, issue, root.UID)
	require.NoError(t, err)
	assert.Equal(t, root.UID, selected)
	for _, tc := range []struct {
		uids         []string
		target, want string
		wantErr      error
	}{
		{[]string{root.UID, reply.UID}, strings.ToLower(reply.UID), reply.UID, nil},
		{[]string{root.UID, reply.UID}, "C:dEf456", reply.UID, nil},
		{[]string{root.UID, reply.UID}, "c:ef456", "", ErrNotFound},
		{[]string{root.UID, reply.UID}, "missing", "", ErrNotFound},
		{[]string{root.UID, ""}, root.UID, "", ErrInvalidResponse},
		{[]string{root.UID, strings.ToLower(root.UID)}, root.UID, "", ErrInvalidResponse},
		{[]string{reply.UID, "01CCCCCCCCCCCCCCCCCCDEF456"}, "c:def456", "", ErrConflict},
	} {
		got, selectErr := SelectPublicationTarget(tc.uids, tc.target)
		assert.Equal(t, tc.want, got)
		if tc.wantErr != nil {
			require.ErrorIs(t, selectErr, tc.wantErr)
		} else {
			require.NoError(t, selectErr)
		}
	}
	cases := []struct {
		name   string
		mutate func(*PublicationIssue)
	}{
		{"truncated", func(i *PublicationIssue) { i.Truncated = true }},
		{"edited root", func(i *PublicationIssue) { i.Comments[0].Body += "changed" }},
		{"missing parent", func(i *PublicationIssue) { i.Comments[1].ReplyToUID = new("01CCCCCCCCCCCCCCCCCCABC123") }},
		{"ambiguous", func(i *PublicationIssue) {
			i.Comments = append(i.Comments, gen.CommentOut{UID: "01CCCCCCCCCCCCCCCCCCDEF456", IssueID: 10})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			copy := issue
			copy.Comments = append([]gen.CommentOut(nil), issue.Comments...)
			tc.mutate(&copy)
			_, err := ResolvePublicationTarget(project, copy, "c:def456")
			require.Error(t, err)
		})
	}
}

func TestValidateReplyKey(t *testing.T) {
	for _, key := range []string{"", "bad\nkey", "bad\x00key", " key", "key "} {
		require.Error(t, ValidateReplyKey(key))
	}
	for _, key := range []string{"CaseSensitive", "opaque:key/1", "MiXeD-Key"} {
		require.NoError(t, ValidateReplyKey(key))
	}
}

func TestTypedCommentKindsAndReceipts(t *testing.T) {
	const issueUID = "01AAAAAAAAAAAAAAAAAAABC123"
	const targetUID = "01BBBBBBBBBBBBBBBBBBDEF456"
	const commentUID = "01CCCCCCCCCCCCCCCCCC789012"
	for _, kind := range []string{"reply", "confirm", "refute", "supersede"} {
		t.Run(kind, func(t *testing.T) {
			f := katafake.New(t)
			c := apiClientFor(t, f, namedWorkspace(t, "reviews"))
			project, err := c.ResolvePublicationProject(t.Context(), "")
			require.NoError(t, err)
			body := strings.Repeat("界", 40) + "\n original"
			var sent gen.CommentRequestBody
			var decodeErr error
			f.Override = func(w http.ResponseWriter, r *http.Request) bool {
				if r.Method != http.MethodPost {
					return false
				}
				decodeErr = json.UnmarshalRead(r.Body, &sent)
				f.Reply(w, map[string]any{"changed": true, "issue": map[string]any{"uid": issueUID, "id": 11, "project_id": project.ID}, "comment": map[string]any{"uid": commentUID, "issue_id": 11, "body": body, "author": "author", "teammate": "reviewer", "reply_to_uid": targetUID, "reply_kind": kind}})
				return true
			}
			receipt, err := c.Comment(t.Context(), project, issueUID, targetUID, kind, body, "key", "reviewer")
			require.NoError(t, err)
			assert.Equal(t, "author", receipt.Comment.Author)
			assert.True(t, receipt.Changed)
			requests := f.Requests()
			require.NotEmpty(t, requests)
			assert.Equal(t, "key", requests[len(requests)-1].Header.Get("Idempotency-Key"))
			require.NoError(t, decodeErr)
			require.NotNil(t, sent.Kind)
			assert.Equal(t, kind, string(*sent.Kind))
			assert.Equal(t, body, sent.Body)
			require.NotNil(t, sent.ReplyTo)
			assert.Equal(t, targetUID, *sent.ReplyTo)
			assert.Nil(t, sent.Force)
		})
	}
	// Reject inconsistent receipts even when the server responds successfully.
	cases := []struct {
		name    string
		corrupt func(map[string]any, map[string]any)
	}{
		{"issue UID", func(i, _ map[string]any) { i["uid"] = "different" }},
		{"issue project ID", func(i, _ map[string]any) { i["project_id"] = 99 }},
		{"issue project UID", func(i, _ map[string]any) { i["project_uid"] = "different" }},
		{"issue ID", func(i, _ map[string]any) { i["id"] = 0 }},
		{"comment issue", func(_, c map[string]any) { c["issue_id"] = 12 }},
		{"comment UID", func(_, c map[string]any) { c["uid"] = "" }},
		{"author", func(_, c map[string]any) { c["author"] = "" }},
		{"teammate missing", func(_, c map[string]any) { delete(c, "teammate") }},
		{"teammate changed", func(_, c map[string]any) { c["teammate"] = "other" }},
		{"body", func(_, c map[string]any) { c["body"] = "changed" }},
		{"target missing", func(_, c map[string]any) { delete(c, "reply_to_uid") }},
		{"target changed", func(_, c map[string]any) { c["reply_to_uid"] = "other-target" }},
		{"kind missing", func(_, c map[string]any) { delete(c, "reply_kind") }},
		{"kind changed", func(_, c map[string]any) { c["reply_kind"] = "supersede" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := katafake.New(t)
			c := apiClientFor(t, f, namedWorkspace(t, "reviews"))
			project, err := c.ResolvePublicationProject(t.Context(), "")
			require.NoError(t, err)
			for _, changed := range []bool{true, false} {
				f.Override = func(w http.ResponseWriter, r *http.Request) bool {
					if r.Method != http.MethodPost {
						return false
					}
					issue := map[string]any{"uid": issueUID, "id": 11, "project_id": project.ID, "project_uid": project.UID}
					comment := map[string]any{"uid": commentUID, "issue_id": 11, "body": "evidence", "author": "actual", "teammate": "reviewer", "reply_to_uid": targetUID, "reply_kind": "reply"}
					tc.corrupt(issue, comment)
					f.Reply(w, map[string]any{"changed": changed, "issue": issue, "comment": comment})
					return true
				}
				_, err = c.Comment(t.Context(), project, issueUID, targetUID, "reply", "evidence", "opaque-key", "reviewer")
				require.ErrorIs(t, err, ErrInvalidResponse, "changed=%t", changed)
			}
		})
	}
}

func TestTypedCommentEditedReplay(t *testing.T) {
	f := katafake.New(t)
	c := apiClientFor(t, f, namedWorkspace(t, "reviews"))
	project, err := c.ResolvePublicationProject(t.Context(), "")
	require.NoError(t, err)
	f.Override = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodPost {
			return false
		}
		f.Reply(w, map[string]any{"changed": false, "issue": map[string]any{"uid": "issue-uid", "id": 11, "project_id": project.ID}, "comment": map[string]any{"uid": "reply-uid", "issue_id": 11, "body": "edited evidence", "edited_at": "2026-01-02T03:04:05Z", "author": "actual", "teammate": "reviewer", "reply_to_uid": "target-uid", "reply_kind": "reply"}})
		return true
	}
	receipt, err := c.Comment(t.Context(), project, "issue-uid", "target-uid", "reply", "original evidence", "opaque-key", "reviewer")
	require.NoError(t, err)
	assert.False(t, receipt.Changed)
	assert.Equal(t, "edited evidence", receipt.Comment.Body)
	assert.NotNil(t, receipt.Comment.EditedAt)
}
