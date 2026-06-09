package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	googlegithub "github.com/google/go-github/v88/github"
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

	listIssueStatus int
	createStatus    int
	patchStatus     int

	createdBodies []string
	patchedBodies []string
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
	w.WriteHeader(s.patchStatus)
	assert.NoError(s.t, json.NewEncoder(w).Encode(&googlegithub.IssueComment{
		ID:   ptr(int64(42)),
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
	id, err := client.FindExistingComment(context.Background(), "owner/repo", 1)
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
	id, err := client.FindExistingComment(context.Background(), "owner/repo", 1)
	require.NoError(t, err)
	assert.Equal(t, int64(30), id)
}

func TestFindExistingComment_Error(t *testing.T) {
	api := newCommentAPIServer(t)
	api.listIssueStatus = http.StatusInternalServerError
	srv := httptest.NewServer(http.HandlerFunc(api.handler))
	defer srv.Close()

	client := newTestGitHubClient(t, "", srv)
	_, err := client.FindExistingComment(context.Background(), "owner/repo", 1)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "list issue comments")
}

func TestUpsertPRComment_Create(t *testing.T) {
	api := newCommentAPIServer(t)
	srv := httptest.NewServer(http.HandlerFunc(api.handler))
	defer srv.Close()

	client := newTestGitHubClient(t, "", srv)
	require.NoError(t, client.UpsertPRComment(context.Background(), "owner/repo", 1, "review body"))
	require.Len(t, api.createdBodies, 1)
	assert.Empty(t, api.patchedBodies)
	assert.True(t, strings.HasPrefix(api.createdBodies[0], CommentMarker+"\n"))
}

func TestUpsertPRComment_Update(t *testing.T) {
	api := newCommentAPIServer(t)
	api.issueCommentsByPR[1] = []*googlegithub.IssueComment{
		issueComment(42, CommentMarker+"\nold", "alice", "User", ""),
	}
	srv := httptest.NewServer(http.HandlerFunc(api.handler))
	defer srv.Close()

	client := newTestGitHubClient(t, "", srv)
	require.NoError(t, client.UpsertPRComment(context.Background(), "owner/repo", 1, "updated body"))
	require.Len(t, api.patchedBodies, 1)
	assert.Empty(t, api.createdBodies)
	assert.True(t, strings.HasPrefix(api.patchedBodies[0], CommentMarker+"\n"))
}

func TestUpsertPRComment_Patch403FallsBackToCreate(t *testing.T) {
	api := newCommentAPIServer(t)
	api.issueCommentsByPR[1] = []*googlegithub.IssueComment{
		issueComment(42, CommentMarker+"\nold", "alice", "User", ""),
	}
	api.patchStatus = http.StatusForbidden
	srv := httptest.NewServer(http.HandlerFunc(api.handler))
	defer srv.Close()

	client := newTestGitHubClient(t, "", srv)
	require.NoError(t, client.UpsertPRComment(context.Background(), "owner/repo", 1, "updated body"))
	require.Len(t, api.patchedBodies, 1)
	require.Len(t, api.createdBodies, 1)
}

func TestUpsertPRComment_Patch404FallsBackToCreate(t *testing.T) {
	api := newCommentAPIServer(t)
	api.issueCommentsByPR[1] = []*googlegithub.IssueComment{
		issueComment(42, CommentMarker+"\nold", "alice", "User", ""),
	}
	api.patchStatus = http.StatusNotFound
	srv := httptest.NewServer(http.HandlerFunc(api.handler))
	defer srv.Close()

	client := newTestGitHubClient(t, "", srv)
	require.NoError(t, client.UpsertPRComment(context.Background(), "owner/repo", 1, "updated body"))
	require.Len(t, api.patchedBodies, 1)
	require.Len(t, api.createdBodies, 1)
}

func TestUpsertPRComment_PatchErrorReturnsError(t *testing.T) {
	api := newCommentAPIServer(t)
	api.issueCommentsByPR[1] = []*googlegithub.IssueComment{
		issueComment(42, CommentMarker+"\nold", "alice", "User", ""),
	}
	api.patchStatus = http.StatusInternalServerError
	srv := httptest.NewServer(http.HandlerFunc(api.handler))
	defer srv.Close()

	client := newTestGitHubClient(t, "", srv)
	err := client.UpsertPRComment(context.Background(), "owner/repo", 1, "updated body")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "patch comment")
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
	require.NoError(t, client.CreatePRComment(context.Background(), "owner/repo", 1, input))
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
	comments, err := client.ListPRDiscussionComments(context.Background(), "owner/repo", 17)
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
	comments, err := client.ListPRDiscussionComments(context.Background(), "owner/repo", 17)
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
	trusted, err := client.ListTrustedRepoCollaborators(context.Background(), "owner/repo")
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
