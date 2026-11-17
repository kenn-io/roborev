package github

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	googlegithub "github.com/google/go-github/v93/github"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/review"
)

type commentAPIServer struct {
	t *testing.T

	wantAuth string

	issueCommentsByPR  map[int][]*googlegithub.IssueComment
	reviewsByPR        map[int][]*googlegithub.PullRequestReview
	inlineCommentsByPR map[int][]*googlegithub.PullRequestComment
	collaborators      []*googlegithub.User

	listCount       int
	listIssueStatus int
	createStatus    int
	patchStatus     int

	createdBodies []string
	patchedBodies []string
	patchedPaths  []string
}

func newCommentAPIServer(t *testing.T) *commentAPIServer {
	t.Helper()
	return &commentAPIServer{
		t:                  t,
		issueCommentsByPR:  make(map[int][]*googlegithub.IssueComment),
		reviewsByPR:        make(map[int][]*googlegithub.PullRequestReview),
		inlineCommentsByPR: make(map[int][]*googlegithub.PullRequestComment),
	}
}

func (s *commentAPIServer) handler(w http.ResponseWriter, r *http.Request) {
	s.t.Helper()
	if s.wantAuth != "" {
		assert.Equal(s.t, s.wantAuth, r.Header.Get("Authorization"))
	}
	w.Header().Set("Content-Type", "application/json")
	path := strings.TrimPrefix(r.URL.Path, "/api/v3")

	switch {
	case r.Method == http.MethodGet && path == "/repos/owner/repo/issues/1/comments":
		s.writeIssueComments(w, 1)
	case r.Method == http.MethodGet && path == "/repos/owner/repo/issues/17/comments":
		s.writeIssueComments(w, 17)
	case r.Method == http.MethodPost && path == "/repos/owner/repo/issues/1/comments":
		s.captureCreate(w, r)
	case r.Method == http.MethodPatch && strings.HasPrefix(path, "/repos/owner/repo/issues/comments/"):
		s.capturePatch(w, r)
	case r.Method == http.MethodGet && path == "/repos/owner/repo/pulls/17/reviews":
		assert.NoError(s.t, json.NewEncoder(w).Encode(s.reviewsByPR[17]))
	case r.Method == http.MethodGet && path == "/repos/owner/repo/pulls/17/comments":
		assert.NoError(s.t, json.NewEncoder(w).Encode(s.inlineCommentsByPR[17]))
	case r.Method == http.MethodGet && path == "/repos/owner/repo/collaborators":
		assert.NoError(s.t, json.NewEncoder(w).Encode(s.collaborators))
	default:
		http.NotFound(w, r)
	}
}

func (s *commentAPIServer) writeIssueComments(w http.ResponseWriter, prNumber int) {
	s.listCount++
	if s.listIssueStatus != 0 {
		w.WriteHeader(s.listIssueStatus)
		_, _ = w.Write([]byte(`{"message":"list failed"}`))
		return
	}
	assert.NoError(s.t, json.NewEncoder(w).Encode(s.issueCommentsByPR[prNumber]))
}

func mustParseTime(raw string) time.Time {
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		panic(err)
	}
	return parsed
}

func (s *commentAPIServer) captureCreate(w http.ResponseWriter, r *http.Request) {
	if s.createStatus == 0 {
		s.createStatus = http.StatusCreated
	}
	var payload struct {
		Body string `json:"body"`
	}
	assert.NoError(s.t, json.NewDecoder(r.Body).Decode(&payload))
	s.createdBodies = append(s.createdBodies, payload.Body)
	w.WriteHeader(s.createStatus)
	assert.NoError(s.t, json.NewEncoder(w).Encode(&googlegithub.IssueComment{
		ID:   ptr(int64(999)),
		Body: ptr(payload.Body),
	}))
}

func (s *commentAPIServer) capturePatch(w http.ResponseWriter, r *http.Request) {
	if s.patchStatus == 0 {
		s.patchStatus = http.StatusOK
	}
	var payload struct {
		Body string `json:"body"`
	}
	assert.NoError(s.t, json.NewDecoder(r.Body).Decode(&payload))
	s.patchedBodies = append(s.patchedBodies, payload.Body)
	s.patchedPaths = append(s.patchedPaths, r.URL.Path)
	status := s.patchStatus
	if strings.HasSuffix(r.URL.Path, "/20") {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	id, err := strconv.ParseInt(r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:], 10, 64)
	assert.NoError(s.t, err)
	assert.NoError(s.t, json.NewEncoder(w).Encode(&googlegithub.IssueComment{
		ID:   ptr(id),
		Body: ptr(payload.Body),
	}))
}

func newTestGitHubClient(t *testing.T, token string, server *httptest.Server) *Client {
	t.Helper()
	client, err := NewClient(token, WithBaseURL(server.URL+"/"))
	require.NoError(t, err)
	return client
}

func issueComment(id int64, body, login, userType, createdAt string) *googlegithub.IssueComment {
	comment := &googlegithub.IssueComment{
		ID:   ptr(id),
		Body: ptr(body),
		User: &googlegithub.User{
			Login: ptr(login),
			Type:  ptr(userType),
		},
	}
	if createdAt != "" {
		comment.CreatedAt = &googlegithub.Timestamp{Time: mustParseTime(createdAt)}
	}
	return comment
}

func reviewComment(body, login, userType, submittedAt string) *googlegithub.PullRequestReview {
	review := &googlegithub.PullRequestReview{
		Body: ptr(body),
		User: &googlegithub.User{
			Login: ptr(login),
			Type:  ptr(userType),
		},
	}
	if submittedAt != "" {
		review.SubmittedAt = &googlegithub.Timestamp{Time: mustParseTime(submittedAt)}
	}
	return review
}

func inlineComment(body, login, userType, createdAt, path string, line, originalLine int) *googlegithub.PullRequestComment {
	comment := &googlegithub.PullRequestComment{
		Body: ptr(body),
		Path: ptr(path),
		Line: ptr(line),
		User: &googlegithub.User{
			Login: ptr(login),
			Type:  ptr(userType),
		},
	}
	if originalLine > 0 {
		comment.OriginalLine = ptr(originalLine)
	}
	if createdAt != "" {
		comment.CreatedAt = &googlegithub.Timestamp{Time: mustParseTime(createdAt)}
	}
	return comment
}

func collaborator(login, roleName string) *googlegithub.User {
	return &googlegithub.User{
		Login:    ptr(login),
		RoleName: ptr(roleName),
	}
}

func TestFindExistingComment_NoMatch(t *testing.T) {
	api := newCommentAPIServer(t)
	srv := httptest.NewServer(http.HandlerFunc(api.handler))
	defer srv.Close()

	client := newTestGitHubClient(t, "", srv)
	id, err := client.FindExistingComment(t.Context(), "owner/repo", 1)
	require.NoError(t, err)
	assert.Zero(t, id)
}

func TestFindExistingComment_FoundNewestMatch(t *testing.T) {
	api := newCommentAPIServer(t)
	api.issueCommentsByPR[1] = []*googlegithub.IssueComment{
		issueComment(10, "ordinary comment", "alice", "User", ""),
		issueComment(20, CommentMarker+"\nold", "alice", "User", ""),
		issueComment(30, CommentMarker+"\nnew", "alice", "User", ""),
	}
	srv := httptest.NewServer(http.HandlerFunc(api.handler))
	defer srv.Close()

	client := newTestGitHubClient(t, "", srv)
	id, err := client.FindExistingComment(t.Context(), "owner/repo", 1)
	require.NoError(t, err)
	assert.Equal(t, int64(30), id)
}

func TestFindExistingComment_Error(t *testing.T) {
	api := newCommentAPIServer(t)
	api.listIssueStatus = http.StatusInternalServerError
	srv := httptest.NewServer(http.HandlerFunc(api.handler))
	defer srv.Close()

	client := newTestGitHubClient(t, "", srv)
	_, err := client.FindExistingComment(t.Context(), "owner/repo", 1)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "list issue comments")
}

func TestUpsertPRComment(t *testing.T) {
	for _, tc := range []struct {
		name                       string
		known, found               int64
		status                     int
		wantID                     int64
		paths                      []string
		lists                      int
		create, wantErr, oversized bool
	}{
		{name: "create", wantID: 999, lists: 1, create: true},
		{name: "update", found: 42, wantID: 42, paths: []string{"42"}, lists: 1},
		{name: "known", known: 42, wantID: 42, paths: []string{"42"}},
		{name: "forbidden", found: 42, status: 403, wantID: 999, paths: []string{"42"}, lists: 1, create: true},
		{name: "deleted", found: 42, status: 404, wantID: 999, paths: []string{"42"}, lists: 1, create: true},
		{name: "oversized forbidden", found: 42, status: 403, wantID: 999, paths: []string{"42"}, lists: 1, create: true, oversized: true},
		{name: "oversized deleted", found: 42, status: 404, wantID: 999, paths: []string{"42"}, lists: 1, create: true, oversized: true},
		{name: "transient", found: 42, status: 500, paths: []string{"42"}, lists: 1, wantErr: true},
		{name: "known deleted missing", known: 42, status: 404, wantID: 999, paths: []string{"42"}, lists: 1, create: true},
		{name: "known forbidden missing", known: 42, status: 403, wantID: 999, paths: []string{"42"}, lists: 1, create: true},
		{name: "known deleted alternate", known: 42, found: 20, status: 404, wantID: 20, paths: []string{"42", "20"}, lists: 1},
		{name: "known forbidden alternate", known: 42, found: 20, status: 403, wantID: 20, paths: []string{"42", "20"}, lists: 1},
		{name: "known forbidden same", known: 42, found: 42, status: 403, wantID: 999, paths: []string{"42"}, lists: 1, create: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := newCommentAPIServer(t)
			api.patchStatus = tc.status
			if tc.found != 0 {
				api.issueCommentsByPR[1] = []*googlegithub.IssueComment{issueComment(tc.found, CommentMarker, "alice", "User", "")}
			}
			server := httptest.NewServer(http.HandlerFunc(api.handler))
			defer server.Close()
			client := newTestGitHubClient(t, "", server)
			body := "body"
			if tc.oversized {
				body = strings.Repeat("x", review.MaxCommentLen+500)
			}
			id, err := client.UpsertPRComment(t.Context(), "owner/repo", 1, body, tc.known)
			if tc.wantErr {
				require.Error(t, err)
				assert.True(t, isGitHubStatus(err, http.StatusInternalServerError))
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tc.wantID, id)
			assert.Equal(t, tc.lists, api.listCount)
			require.Len(t, api.patchedPaths, len(tc.paths))
			for i, path := range tc.paths {
				assert.Equal(t, "/api/v3/repos/owner/repo/issues/comments/"+path, api.patchedPaths[i])
				if tc.oversized {
					assert.LessOrEqual(t, len(api.patchedBodies[i]), review.MaxCommentLen)
					assert.Equal(t, 1, strings.Count(api.patchedBodies[i], CommentMarker))
					assert.Equal(t, 1, strings.Count(api.patchedBodies[i], review.CommentTruncSuffix))
				} else {
					assert.Equal(t, CommentMarker+"\nbody", api.patchedBodies[i])
				}
			}
			if tc.create {
				require.Len(t, api.createdBodies, 1)
				if tc.oversized {
					assert.LessOrEqual(t, len(api.createdBodies[0]), review.MaxCommentLen)
					assert.Equal(t, 1, strings.Count(api.createdBodies[0], CommentMarker))
					assert.Equal(t, 1, strings.Count(api.createdBodies[0], review.CommentTruncSuffix))
				} else {
					assert.Equal(t, CommentMarker+"\nbody", api.createdBodies[0])
				}
			} else {
				assert.Empty(t, api.createdBodies)
			}
		})
	}
}

func TestCreatePRComment_TruncationUTF8Safe(t *testing.T) {
	api := newCommentAPIServer(t)
	srv := httptest.NewServer(http.HandlerFunc(api.handler))
	defer srv.Close()

	const truncSuffix = "\n\n...(truncated — comment exceeded size limit)"
	maxBody := review.MaxCommentLen - len(truncSuffix)
	markerOverhead := len(CommentMarker) + 1
	input := strings.Repeat("x", maxBody-markerOverhead-2) + "\U0001f600" + strings.Repeat("y", 100)

	client := newTestGitHubClient(t, "", srv)
	require.NoError(t, client.CreatePRComment(t.Context(), "owner/repo", 1, input))
	require.Len(t, api.createdBodies, 1)
	body := api.createdBodies[0]
	assert.True(t, strings.HasPrefix(body, CommentMarker))
	assert.Contains(t, body, "truncated")
	assert.LessOrEqual(t, len(body), review.MaxCommentLen)
	assert.True(t, utf8.ValidString(body))
}

func TestListPRDiscussionComments_FiltersAndSorts(t *testing.T) {
	api := newCommentAPIServer(t)
	api.issueCommentsByPR[17] = []*googlegithub.IssueComment{
		issueComment(1, "human issue comment", "alice", "User", "2026-03-24T14:00:00Z"),
		issueComment(2, "comment from bot", "dependabot[bot]", "Bot", "2026-03-24T15:00:00Z"),
		issueComment(3, CommentMarker+"\nroborev summary", "roborev-runner", "User", "2026-03-24T16:00:00Z"),
	}
	api.reviewsByPR[17] = []*googlegithub.PullRequestReview{
		reviewComment("review summary comment", "bob", "User", "2026-03-25T10:30:00Z"),
	}
	api.inlineCommentsByPR[17] = []*googlegithub.PullRequestComment{
		inlineComment("inline review comment", "carol", "User", "2026-03-26T09:15:00Z", "internal/daemon/ci_poller.go", 123, 0),
	}
	srv := httptest.NewServer(http.HandlerFunc(api.handler))
	defer srv.Close()

	client := newTestGitHubClient(t, "", srv)
	comments, err := client.ListPRDiscussionComments(t.Context(), "owner/repo", 17)
	require.NoError(t, err)
	require.Len(t, comments, 3)

	assert.Equal(t, "alice", comments[0].Author)
	assert.Equal(t, PRDiscussionSourceIssueComment, comments[0].Source)
	assert.Equal(t, "bob", comments[1].Author)
	assert.Equal(t, PRDiscussionSourceReview, comments[1].Source)
	assert.Equal(t, "carol", comments[2].Author)
	assert.Equal(t, PRDiscussionSourceReviewComment, comments[2].Source)
	assert.Equal(t, "internal/daemon/ci_poller.go", comments[2].Path)
	assert.Equal(t, 123, comments[2].Line)
}

func TestListPRDiscussionComments_AllowsMissingTimestamps(t *testing.T) {
	api := newCommentAPIServer(t)
	api.issueCommentsByPR[17] = []*googlegithub.IssueComment{
		issueComment(1, "issue without timestamp", "alice", "User", ""),
	}
	api.reviewsByPR[17] = []*googlegithub.PullRequestReview{
		reviewComment("review without timestamp", "bob", "User", ""),
	}
	api.inlineCommentsByPR[17] = []*googlegithub.PullRequestComment{
		inlineComment("inline without timestamp", "carol", "User", "", "internal/github/pr_discussion.go", 59, 0),
	}
	srv := httptest.NewServer(http.HandlerFunc(api.handler))
	defer srv.Close()

	client := newTestGitHubClient(t, "", srv)
	comments, err := client.ListPRDiscussionComments(t.Context(), "owner/repo", 17)
	require.NoError(t, err)
	require.Len(t, comments, 3)

	for _, comment := range comments {
		assert.True(t, comment.CreatedAt.IsZero())
	}
}

func TestListTrustedRepoCollaborators_FiltersToMaintainAndAdmin(t *testing.T) {
	api := newCommentAPIServer(t)
	api.collaborators = []*googlegithub.User{
		collaborator("alice", "admin"),
		collaborator("bob", "maintain"),
		collaborator("eve", "write"),
	}
	srv := httptest.NewServer(http.HandlerFunc(api.handler))
	defer srv.Close()

	client := newTestGitHubClient(t, "", srv)
	trusted, err := client.ListTrustedRepoCollaborators(t.Context(), "owner/repo")
	require.NoError(t, err)
	assert.Contains(t, trusted, "alice")
	assert.Contains(t, trusted, "bob")
	assert.NotContains(t, trusted, "eve")
}

func TestCloneURL(t *testing.T) {
	plain, err := CloneURL("owner/repo")
	require.NoError(t, err)
	assert.Equal(t, "https://github.com/owner/repo.git", plain)
}

// Silent cleanup must never create, including after an update loses its target.
func TestUpdateExistingPRComment(t *testing.T) {
	for _, tc := range []struct {
		name         string
		known, found int64
		status       int
		wantID       int64
		paths        []string
		lists        int
		wantErr      bool
	}{
		{name: "missing", lists: 1},
		{name: "updated", found: 42, wantID: 42, paths: []string{"42"}, lists: 1},
		{name: "deleted", found: 42, status: 404, paths: []string{"42"}, lists: 1},
		{name: "forbidden", found: 42, status: 403, paths: []string{"42"}, lists: 1, wantErr: true},
		{name: "transient", found: 42, status: 500, paths: []string{"42"}, lists: 1, wantErr: true},
		{name: "known", known: 42, wantID: 42, paths: []string{"42"}},
		{name: "known deleted missing", known: 42, status: 404, paths: []string{"42"}, lists: 1},
		{name: "known forbidden missing", known: 42, status: 403, paths: []string{"42"}, lists: 1},
		{name: "known deleted alternate", known: 42, found: 20, status: 404, wantID: 20, paths: []string{"42", "20"}, lists: 1},
		{name: "known forbidden alternate", known: 42, found: 20, status: 403, wantID: 20, paths: []string{"42", "20"}, lists: 1},
		{name: "known forbidden same", known: 42, found: 42, status: 403, paths: []string{"42"}, lists: 1, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := newCommentAPIServer(t)
			api.patchStatus = tc.status
			if tc.found != 0 {
				api.issueCommentsByPR[1] = []*googlegithub.IssueComment{issueComment(tc.found, CommentMarker, "alice", "User", "")}
			}
			server := httptest.NewServer(http.HandlerFunc(api.handler))
			defer server.Close()
			client := newTestGitHubClient(t, "token", server)
			id, err := client.UpdateExistingPRComment(t.Context(), "owner/repo", 1, "No findings.", tc.known)
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tc.wantID, id)
			assert.Equal(t, tc.lists, api.listCount)
			assert.Empty(t, api.createdBodies)
			require.Len(t, api.patchedPaths, len(tc.paths))
			for i, path := range tc.paths {
				assert.Equal(t, "/api/v3/repos/owner/repo/issues/comments/"+path, api.patchedPaths[i])
				assert.Equal(t, CommentMarker+"\nNo findings.", api.patchedBodies[i])
			}
		})
	}
}
