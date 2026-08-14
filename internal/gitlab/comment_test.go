package gitlab

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gogitlab "gitlab.com/gitlab-org/api/client-go/v2"

	"go.kenn.io/roborev/internal/review"
)

const (
	testProject        = "group/subgroup/project"
	testProjectEscaped = "group%2Fsubgroup%2Fproject"
)

type noteAPIServer struct {
	t *testing.T

	wantAuth string

	notesByMR map[int][]*gogitlab.Note
	// notePages, when set, is served one page per entry with pagination
	// headers instead of notesByMR.
	notePages   [][]*gogitlab.Note
	listedPages []int
	// listCalls counts note listings, which distinguishes a retry the client
	// library performed from one recoverFailedCreate performed after re-listing.
	listCalls int

	listStatus   int
	createStatus int
	updateStatus int
	// createLandsAnyway stores the note even when createStatus is an error,
	// modelling a POST that applied but whose response failed.
	createLandsAnyway bool
	// concurrentNoteOnCreate adds a marker note with someone else's body during
	// the create, modelling another pipeline posting between the list and the
	// create. Unlike createLandsAnyway, this note is NOT ours.
	concurrentNoteOnCreate bool
	// recoverCreateStatus, when set, is the status for every create after the
	// first, so a transient failure followed by success can be modelled.
	recoverCreateStatus int

	// updateStatusByNote overrides updateStatus per note ID, so a pre-existing
	// note can reject updates while a newly landed one accepts them.
	updateStatusByNote map[int64]int

	createdBodies  []string
	updatedBodies  []string
	updatedNoteIDs []int64
	requestPaths   []string
}

func newNoteAPIServer(t *testing.T) *noteAPIServer {
	t.Helper()
	return &noteAPIServer{
		t:         t,
		notesByMR: make(map[int][]*gogitlab.Note),
	}
}

func (s *noteAPIServer) handler(w http.ResponseWriter, r *http.Request) {
	s.t.Helper()
	if s.wantAuth != "" {
		assert.Equal(s.t, s.wantAuth, r.Header.Get("PRIVATE-TOKEN"))
	}
	w.Header().Set("Content-Type", "application/json")

	// RawPath preserves the URL-encoded project path.
	path := r.URL.EscapedPath()
	s.requestPaths = append(s.requestPaths, path)
	path = strings.TrimPrefix(path, "/api/v4")
	notesPath := "/projects/" + testProjectEscaped + "/merge_requests/1/notes"

	switch {
	case r.Method == http.MethodGet && path == notesPath:
		s.writeNotes(w, r)
	case r.Method == http.MethodPost && path == notesPath:
		s.captureCreate(w, r)
	case r.Method == http.MethodPut && strings.HasPrefix(path, notesPath+"/"):
		s.captureUpdate(w, r)
	default:
		http.NotFound(w, r)
	}
}

// writeNotes serves the stubbed notes. When notePages is set, it serves one
// page per entry and advertises the next page through GitLab's pagination
// headers so the client's paging loop is exercised.
func (s *noteAPIServer) writeNotes(w http.ResponseWriter, r *http.Request) {
	s.listCalls++
	if s.listStatus != 0 {
		w.WriteHeader(s.listStatus)
		_, _ = w.Write([]byte(`{"message":"list failed"}`))
		return
	}
	if len(s.notePages) == 0 {
		assert.NoError(s.t, json.NewEncoder(w).Encode(s.notesByMR[1]))
		return
	}

	page := 1
	if raw := r.URL.Query().Get("page"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if !assert.NoError(s.t, err) {
			http.Error(w, "bad page", http.StatusBadRequest)
			return
		}
		page = parsed
	}
	s.listedPages = append(s.listedPages, page)
	if !assert.True(s.t, page >= 1 && page <= len(s.notePages),
		"page %d out of range", page) {
		http.Error(w, "page out of range", http.StatusBadRequest)
		return
	}

	w.Header().Set("X-Total-Pages", strconv.Itoa(len(s.notePages)))
	if page < len(s.notePages) {
		w.Header().Set("X-Next-Page", strconv.Itoa(page+1))
	}
	assert.NoError(s.t, json.NewEncoder(w).Encode(s.notePages[page-1]))
}

func (s *noteAPIServer) captureCreate(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		Body string `json:"body"`
	}
	assert.NoError(s.t, json.NewDecoder(r.Body).Decode(&payload))
	s.createdBodies = append(s.createdBodies, payload.Body)

	// Resolved per request, like captureUpdate, so the stub carries no
	// ordering-dependent state and an unset createStatus stays distinguishable
	// from an explicit 201.
	status := s.createStatus
	if status == 0 {
		status = http.StatusCreated
	}
	if s.recoverCreateStatus != 0 && len(s.createdBodies) > 1 {
		status = s.recoverCreateStatus
	}

	if s.createLandsAnyway {
		// Model the case the retry hazard turns on: GitLab applied the note
		// and still answered with an error, so a later list returns it.
		s.notesByMR[1] = append(s.notesByMR[1],
			note(int64(700+len(s.createdBodies)), payload.Body, false))
	}
	if s.concurrentNoteOnCreate {
		s.notesByMR[1] = append(s.notesByMR[1],
			note(int64(800+len(s.createdBodies)),
				CommentMarker+"\nanother pipeline's newer review", false))
	}
	w.WriteHeader(status)
	assert.NoError(s.t, json.NewEncoder(w).Encode(&gogitlab.Note{
		ID:   999,
		Body: payload.Body,
	}))
}

func (s *noteAPIServer) captureUpdate(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		Body string `json:"body"`
	}
	assert.NoError(s.t, json.NewDecoder(r.Body).Decode(&payload))
	s.updatedBodies = append(s.updatedBodies, payload.Body)

	noteID := updatedNoteID(s.t, r.URL.EscapedPath())
	s.updatedNoteIDs = append(s.updatedNoteIDs, noteID)

	// Resolve the default per request rather than mutating updateStatus, so a
	// per-note override cannot depend on which note was updated first.
	status := s.updateStatus
	if status == 0 {
		status = http.StatusOK
	}
	if perNote, ok := s.updateStatusByNote[noteID]; ok {
		status = perNote
	}

	w.WriteHeader(status)
	assert.NoError(s.t, json.NewEncoder(w).Encode(&gogitlab.Note{
		ID:   noteID,
		Body: payload.Body,
	}))
}

// updatedNoteID pulls the note ID off the end of a note update path.
func updatedNoteID(t *testing.T, path string) int64 {
	t.Helper()
	_, last, ok := strings.Cut(path, "/notes/")
	if !ok {
		return 0
	}
	id, err := strconv.ParseInt(last, 10, 64)
	assert.NoError(t, err)
	return id
}

func newTestGitLabClient(t *testing.T, token string, server *httptest.Server) *Client {
	t.Helper()
	client, err := NewClient(token, WithBaseURL(server.URL), WithoutRetries())
	require.NoError(t, err)
	return client
}

func startNoteAPI(t *testing.T) (*noteAPIServer, *Client) {
	t.Helper()
	api := newNoteAPIServer(t)
	srv := httptest.NewServer(http.HandlerFunc(api.handler))
	t.Cleanup(srv.Close)
	return api, newTestGitLabClient(t, "", srv)
}

// startNoteAPIRetrying builds a client with the library's retry loop left on,
// the way production clients run. The note-create tests need it: with retries
// disabled globally there would be nothing to prove about the per-request
// no-retry option.
func startNoteAPIRetrying(t *testing.T) (*noteAPIServer, *Client) {
	t.Helper()
	api := newNoteAPIServer(t)
	srv := httptest.NewServer(http.HandlerFunc(api.handler))
	t.Cleanup(srv.Close)
	client, err := NewClient("", WithBaseURL(srv.URL))
	require.NoError(t, err)
	return api, client
}

func note(id int64, body string, system bool) *gogitlab.Note {
	return &gogitlab.Note{ID: id, Body: body, System: system}
}

func TestFindExistingMRComment_NoMatch(t *testing.T) {
	api, client := startNoteAPI(t)
	api.notesByMR[1] = []*gogitlab.Note{note(10, "ordinary note", false)}

	id, err := client.FindExistingMRComment(context.Background(), testProject, 1)
	require.NoError(t, err)
	assert.Zero(t, id)
}

func TestFindExistingMRComment_FoundNewestMatch(t *testing.T) {
	api, client := startNoteAPI(t)
	api.notesByMR[1] = []*gogitlab.Note{
		note(10, "ordinary note", false),
		note(20, CommentMarker+"\nold", false),
		note(30, CommentMarker+"\nnew", false),
	}

	id, err := client.FindExistingMRComment(context.Background(), testProject, 1)
	require.NoError(t, err)
	assert.Equal(t, int64(30), id)
}

func TestFindExistingMRComment_IgnoresSystemNotes(t *testing.T) {
	api, client := startNoteAPI(t)
	api.notesByMR[1] = []*gogitlab.Note{
		note(20, CommentMarker+"\nroborev", false),
		note(30, CommentMarker+"\nsystem echo", true),
	}

	id, err := client.FindExistingMRComment(context.Background(), testProject, 1)
	require.NoError(t, err)
	assert.Equal(t, int64(20), id)
}

func TestFindExistingMRComment_FollowsPagination(t *testing.T) {
	api, client := startNoteAPI(t)
	api.notePages = [][]*gogitlab.Note{
		{note(10, "ordinary note", false), note(20, CommentMarker+"\nold", false)},
		{note(30, "another note", false), note(40, CommentMarker+"\nnew", false)},
	}

	id, err := client.FindExistingMRComment(context.Background(), testProject, 1)
	require.NoError(t, err)
	assert.Equal(t, int64(40), id)
	assert.Equal(t, []int{1, 2}, api.listedPages)
}

func TestFindExistingMRComment_KeepsMatchFromEarlierPage(t *testing.T) {
	api, client := startNoteAPI(t)
	api.notePages = [][]*gogitlab.Note{
		{note(20, CommentMarker+"\nroborev", false)},
		{note(30, "unrelated note", false)},
	}

	id, err := client.FindExistingMRComment(context.Background(), testProject, 1)
	require.NoError(t, err)
	assert.Equal(t, int64(20), id)
	assert.Equal(t, []int{1, 2}, api.listedPages)
}

func TestFindExistingMRComment_Error(t *testing.T) {
	api, client := startNoteAPI(t)
	api.listStatus = http.StatusInternalServerError

	_, err := client.FindExistingMRComment(context.Background(), testProject, 1)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "list merge request notes")
}

func TestFindExistingMRComment_InvalidProject(t *testing.T) {
	_, client := startNoteAPI(t)

	_, err := client.FindExistingMRComment(context.Background(), "no-group", 1)
	require.ErrorContains(t, err, "invalid GitLab project")
}

func TestCreateMRComment_URLEncodesProjectPath(t *testing.T) {
	api, client := startNoteAPI(t)

	require.NoError(t, client.CreateMRComment(
		context.Background(), testProject, 1, "review body"))
	require.Len(t, api.createdBodies, 1)
	require.NotEmpty(t, api.requestPaths)
	assert.Contains(t, api.requestPaths[0], testProjectEscaped)
	assert.True(t, strings.HasPrefix(api.createdBodies[0], CommentMarker+"\n"))
	assert.Contains(t, api.createdBodies[0], "review body")
}

func TestUpsertMRComment_Create(t *testing.T) {
	api, client := startNoteAPI(t)

	require.NoError(t, client.UpsertMRComment(
		context.Background(), testProject, 1, "review body"))
	require.Len(t, api.createdBodies, 1)
	assert.Empty(t, api.updatedBodies)
	assert.True(t, strings.HasPrefix(api.createdBodies[0], CommentMarker+"\n"))
	// The marker must appear exactly once, even on the create path.
	assert.Equal(t, 1, strings.Count(api.createdBodies[0], CommentMarker))
}

func TestUpsertMRComment_Update(t *testing.T) {
	api, client := startNoteAPI(t)
	api.notesByMR[1] = []*gogitlab.Note{note(42, CommentMarker+"\nold", false)}

	require.NoError(t, client.UpsertMRComment(
		context.Background(), testProject, 1, "updated body"))
	require.Len(t, api.updatedBodies, 1)
	assert.Empty(t, api.createdBodies)
	assert.True(t, strings.HasPrefix(api.updatedBodies[0], CommentMarker+"\n"))
	assert.Contains(t, api.updatedBodies[0], "updated body")
}

func TestUpsertMRComment_UpdateForbiddenOrMissingFallsBackToCreate(t *testing.T) {
	tests := []struct {
		name   string
		status int
	}{
		{"Forbidden", http.StatusForbidden},
		{"NotFound", http.StatusNotFound},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api, client := startNoteAPI(t)
			api.notesByMR[1] = []*gogitlab.Note{note(42, CommentMarker+"\nold", false)}
			api.updateStatus = tt.status

			require.NoError(t, client.UpsertMRComment(
				context.Background(), testProject, 1, "updated body"))
			require.Len(t, api.updatedBodies, 1)
			require.Len(t, api.createdBodies, 1)
			assert.Equal(t, 1, strings.Count(api.createdBodies[0], CommentMarker))
		})
	}
}

func TestUpsertMRComment_UpdateErrorReturnsError(t *testing.T) {
	api, client := startNoteAPI(t)
	api.notesByMR[1] = []*gogitlab.Note{note(42, CommentMarker+"\nold", false)}
	api.updateStatus = http.StatusInternalServerError

	err := client.UpsertMRComment(context.Background(), testProject, 1, "updated body")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "update note")
}

// A 5xx answer to the create POST must not be replayed by the client library:
// the note may already exist, and a replay would leave a duplicate that later
// upserts never clean up. Exactly one POST per create attempt is the guarantee.
func TestUpsertMRComment_CreateIsNotRetriedByTheLibrary(t *testing.T) {
	api, client := startNoteAPIRetrying(t)
	api.createStatus = http.StatusInternalServerError

	err := client.UpsertMRComment(context.Background(), testProject, 1, "review body")
	require.Error(t, err)
	// One original attempt plus one deliberate recovery attempt after the
	// list came back empty. The library's default RetryMax would make this 12.
	assert.Len(t, api.createdBodies, 2,
		"create must not be retried inside the client library")
}

// When the failed POST did apply, recovery must recognize its own body and do
// nothing further — no second POST, and no update of a note that is already
// exactly right.
func TestUpsertMRComment_RecoversWhenFailedCreateLanded(t *testing.T) {
	assert := assert.New(t)
	api, client := startNoteAPIRetrying(t)
	api.createStatus = http.StatusInternalServerError
	api.createLandsAnyway = true

	// Oversized on purpose: the prepared body then sits exactly at
	// MaxCommentLen, so a path that prepared it a second time shows up as a
	// duplicated marker and a body shifted by the 28 bytes that second marker
	// adds — and the body match this recovery relies on would miss. Two things
	// it does not show up as: a duplicated truncation suffix — re-preparing
	// pushes the old suffix past the cut, so it is discarded rather than doubled
	// — or a length change, since TruncateComment returns exactly MaxCommentLen
	// for an oversized all-ASCII body like this one (a multi-byte rune
	// straddling the cut would cost 1-3 bytes to TrimPartialRune). Re-escaping
	// is not observable at any size either: neutralizeQuickActions is idempotent
	// and this body has no slash.
	body := strings.Repeat("x", review.MaxCommentLen)
	prepared := prepareBody(body)
	require.Len(t, prepared, review.MaxCommentLen,
		"setup: the input must be large enough that preparing twice is observable")

	require.NoError(t, client.UpsertMRComment(
		context.Background(), testProject, 1, body))

	assert.Len(api.createdBodies, 1, "must not post a second note")
	assert.Empty(api.updatedBodies,
		"a note that already carries this exact body needs no write")

	// The landed note is the one the create posted, so checking it pins that the
	// body was prepared exactly once. Cheap properties first: a re-prepared body
	// trips the marker count with a readable message, so the full comparison of
	// two 60KB strings only runs when the difference is something else.
	got := api.createdBodies[0]
	require.Equal(t, 1, strings.Count(got, CommentMarker), "marker must not be prepended twice")
	require.Equal(t, 1, strings.Count(got, review.CommentTruncSuffix),
		"posted body must be the prepared, truncated one")
	assert.Equal(prepared, got)
}

// The concurrency case: another pipeline posts its own marker note between this
// job's list and its create, and this job's create then fails without landing.
// Recovery must not mistake that note for its own and overwrite a newer review
// with this older one — it must retry its own create instead.
func TestUpsertMRComment_RecoveryLeavesAConcurrentNoteAlone(t *testing.T) {
	assert := assert.New(t)
	api, client := startNoteAPIRetrying(t)
	api.createStatus = http.StatusInternalServerError
	api.concurrentNoteOnCreate = true
	api.recoverCreateStatus = http.StatusCreated

	require.NoError(t, client.UpsertMRComment(
		context.Background(), testProject, 1, "review body"))

	assert.Empty(api.updatedBodies,
		"the other pipeline's note must not be overwritten")
	require.Len(t, api.createdBodies, 2, "recovery must post its own note")
	assert.Equal(prepareBody("review body"), api.createdBodies[1])
}

// When nothing landed, recovery retries the create once so a genuinely
// transient failure still gets its note posted.
func TestUpsertMRComment_RetriesCreateOnceWhenNothingLanded(t *testing.T) {
	api, client := startNoteAPIRetrying(t)
	api.createStatus = http.StatusInternalServerError
	// Succeed on the recovery attempt.
	api.recoverCreateStatus = http.StatusCreated

	require.NoError(t, client.UpsertMRComment(
		context.Background(), testProject, 1, "review body"))
	assert.Len(t, api.createdBodies, 2)
	assert.Empty(t, api.updatedBodies)
}

// After an update fails with 403 the upsert falls through to a create. If that
// create fails too and nothing landed, recovery must not adopt the pre-existing
// note — updating it again fails the same way and buries the create error.
func TestUpsertMRComment_RecoveryIgnoresPreExistingNote(t *testing.T) {
	assert := assert.New(t)
	api, client := startNoteAPIRetrying(t)
	api.notesByMR[1] = []*gogitlab.Note{note(42, CommentMarker+"\nold", false)}
	api.updateStatus = http.StatusForbidden
	api.createStatus = http.StatusInternalServerError

	err := client.UpsertMRComment(context.Background(), testProject, 1, "review body")
	require.Error(t, err)
	// The create was retried once instead of re-updating note 42.
	assert.Len(api.createdBodies, 2, "recovery must retry the create")
	assert.Len(api.updatedBodies, 1, "note 42 must not be updated twice")
	assert.Contains(err.Error(), "create MR comment",
		"the create failure must survive in the returned error")
}

// A pre-existing marker note must not confuse the body match: when the failed
// create landed, recovery recognizes its own note and stops, leaving the older
// note untouched rather than posting a duplicate or writing again.
func TestUpsertMRComment_LandedNoteRecognizedDespitePreExistingOne(t *testing.T) {
	assert := assert.New(t)
	api, client := startNoteAPIRetrying(t)
	api.notesByMR[1] = []*gogitlab.Note{note(42, CommentMarker+"\nold", false)}
	// Note 42 rejects updates, which is what drives the create fallback.
	api.updateStatusByNote = map[int64]int{42: http.StatusForbidden}
	api.createStatus = http.StatusInternalServerError
	api.createLandsAnyway = true

	require.NoError(t, client.UpsertMRComment(
		context.Background(), testProject, 1, "review body"))

	assert.Len(api.createdBodies, 1, "the landed note must not be posted again")
	// Only the rejected update of note 42 — recovery writes nothing.
	assert.Equal([]int64{42}, api.updatedNoteIDs)
	// The landed note carries the prepared body, prepared exactly once. This
	// body is short and slash-free, so what that pins here is the marker; the
	// truncation half is covered by the oversized case in
	// TestUpsertMRComment_RecoversWhenFailedCreateLanded.
	assert.Equal(prepareBody("review body"), api.createdBodies[0])
}

// An authorization or not-found create will fail again for the same reason, so
// recovery must not retry it or relabel it as transient. A Guest-role or
// read_api-only token can list notes but not create them, which is exactly this
// shape. 404 covers the ErrNotFound sentinel the library returns instead of an
// *ErrorResponse. The transient 4xx statuses are covered by the sibling test
// below, which requires the opposite behavior.
func TestUpsertMRComment_PermanentFailureIsNotRetried(t *testing.T) {
	statuses := []struct {
		name   string
		status int
	}{
		{"Forbidden", http.StatusForbidden},
		{"NotFound", http.StatusNotFound},
	}

	for _, tt := range statuses {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			api, client := startNoteAPIRetrying(t)
			api.createStatus = tt.status

			err := client.UpsertMRComment(
				context.Background(), testProject, 1, "review body")
			require.Error(t, err)
			assert.Len(api.createdBodies, 1, "a %d create must not be retried", tt.status)
			assert.NotContains(err.Error(), "transient",
				"a permanent failure must not be reported as transient")
		})
	}
}

// 429 and 408 are 4xx but must stay replayable: neither is a verdict on the
// request, so when one reaches recovery that last attempt is the difference
// between posting and dropping the comment. WithoutRetries is the fastest way to
// make the status escape to recovery rather than being retried in-call.
func TestUpsertMRComment_TransientFailureReachingRecoveryIsRetried(t *testing.T) {
	statuses := []struct {
		name   string
		status int
	}{
		{"TooManyRequests", http.StatusTooManyRequests},
		{"RequestTimeout", http.StatusRequestTimeout},
	}

	for _, tt := range statuses {
		t.Run(tt.name, func(t *testing.T) {
			api, client := startNoteAPI(t)
			api.createStatus = tt.status
			api.recoverCreateStatus = http.StatusCreated

			require.NoError(t, client.UpsertMRComment(
				context.Background(), testProject, 1, "review body"))
			assert.Len(t, api.createdBodies, 2,
				"recovery must still retry a transient create failure")
			// Two listings means the second POST came from recovery. Without
			// this, a library retry inside the create call would also produce
			// two POSTs and isPermanentFailure would never be consulted.
			assert.Equal(t, 2, api.listCalls,
				"the failure must escape to recoverFailedCreate")
		})
	}
}

// A rate-limited create is safe to replay: the limiter rejects it before the
// note is applied, so the library's retry must stay enabled for 429.
func TestUpsertMRComment_RateLimitedCreateIsRetried(t *testing.T) {
	api, client := startNoteAPIRetrying(t)
	api.createStatus = http.StatusTooManyRequests
	api.recoverCreateStatus = http.StatusCreated

	require.NoError(t, client.UpsertMRComment(
		context.Background(), testProject, 1, "review body"))
	assert.Len(t, api.createdBodies, 2)
	assert.Empty(t, api.updatedBodies)
	// Both POSTs came from one create call, so only the initial upsert lookup
	// listed notes. A second listing would mean the 429 escaped to
	// recoverFailedCreate instead of being retried by the library.
	assert.Equal(t, 1, api.listCalls, "the 429 must be retried inside the create call")
}

// WithoutRetries must still mean no retries. The per-request policy replaces
// the library's own, which is the only one that consults the flag, so the
// policy has to check it too — otherwise startNoteAPI's helper silently
// replays a 429 five times with real backoff.
func TestCreateMRComment_WithoutRetriesSuppressesRateLimitRetry(t *testing.T) {
	api, client := startNoteAPI(t)
	api.createStatus = http.StatusTooManyRequests

	err := client.CreateMRComment(context.Background(), testProject, 1, "review body")
	require.Error(t, err)
	assert.Len(t, api.createdBodies, 1, "WithoutRetries must not replay the 429")
}

func TestCreateMRComment_SendsPrivateTokenHeader(t *testing.T) {
	api := newNoteAPIServer(t)
	api.wantAuth = "secret-token"
	srv := httptest.NewServer(http.HandlerFunc(api.handler))
	defer srv.Close()

	client := newTestGitLabClient(t, "secret-token", srv)
	require.NoError(t, client.CreateMRComment(
		context.Background(), testProject, 1, "body"))
	require.Len(t, api.createdBodies, 1)
}

func TestNeutralizeQuickActions(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "Approve",
			body: "/approve",
			want: `\/approve`,
		},
		{
			name: "Close",
			body: "findings:\n/close\ndone",
			want: "findings:\n" + `\/close` + "\ndone",
		},
		{
			name: "CommandWithArgument",
			body: "/target_branch foo",
			want: `\/target_branch foo`,
		},
		{
			name: "MultipleCommands",
			body: "/approve\n/merge\n/unlabel ~bug",
			want: `\/approve` + "\n" + `\/merge` + "\n" + `\/unlabel ~bug`,
		},
		{
			name: "MidLineSlashUntouched",
			body: "a /b",
			want: "a /b",
		},
		{
			name: "URLUntouched",
			body: "https://example.com/approve",
			want: "https://example.com/approve",
		},
		{
			// GitLab anchors commands at the first column, so an
			// indented slash is never a command. Leaving it alone
			// keeps indented code samples intact.
			name: "IndentedSlashUntouched",
			body: "    /approve",
			want: "    /approve",
		},
		{
			// `/usr` is not a known quick action, so GitLab renders
			// it as text either way. Escaping it is harmless: the
			// rendered output still reads `/usr/bin/foo`.
			name: "AbsolutePathEscapedHarmlessly",
			body: "/usr/bin/foo",
			want: `\/usr/bin/foo`,
		},
		{
			name: "SlashWithoutLetterUntouched",
			body: "/2 spaces\n//comment\n/",
			want: "/2 spaces\n//comment\n/",
		},
		{
			// Code fences do not exempt a line. GitLab's extractor
			// skips code blocks, but only through a regex whose
			// branches this package refuses to mirror, so the
			// cosmetic escape is accepted inside fences too.
			name: "InsideCodeFenceEscaped",
			body: "text\n```\n/approve\n```\nmore",
			want: "text\n```\n" + `\/approve` + "\n```\nmore",
		},
		{
			name: "InsideLanguageFenceEscaped",
			body: "```sh\n/close\n```",
			want: "```sh\n" + `\/close` + "\n```",
		},
		{
			// An empty fence pair is not a code block for GitLab,
			// which shifts every later boundary by one line.
			// Unconditional escaping is unaffected by the shift.
			name: "AdjacentEmptyFencesEscaped",
			body: "```\n```\nx\n```\n/close\n```",
			want: "```\n```\nx\n```\n" + `\/close` + "\n```",
		},
		{
			// GitLab's HTML block branch can swallow a fence line,
			// moving the block boundary past the command.
			name: "HTMLWrappedFenceEscaped",
			body: "<div>\n```\n</div>\n/close\n```",
			want: "<div>\n```\n</div>\n" + `\/close` + "\n```",
		},
		{
			name: "QuoteWrappedFenceEscaped",
			body: ">>>\n```\n>>>\n/close\n```",
			want: ">>>\n```\n>>>\n" + `\/close` + "\n```",
		},
		{
			name: "UnterminatedFenceEscaped",
			body: "```\n/approve",
			want: "```\n" + `\/approve`,
		},
		{
			// GitLab's extractor strips carriage returns before
			// matching, so a leading CR does not protect the line.
			name: "LeadingCarriageReturnEscaped",
			body: "\r/approve",
			want: "\r" + `\/approve`,
		},
		{
			// GitLab drops every carriage return, so one wedged
			// between the slash and the command name does not
			// protect the line either.
			name: "InteriorCarriageReturnEscaped",
			body: "/\rapprove",
			want: `\/` + "\rapprove",
		},
		{
			name: "AlreadyEscapedNotDoubleEscaped",
			body: `\/approve`,
			want: `\/approve`,
		},
		{
			name: "Empty",
			body: "",
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := neutralizeQuickActions(tt.body)
			assert.Equal(t, tt.want, got)
			assert.Equal(t, got, neutralizeQuickActions(got),
				"neutralization must be idempotent")
		})
	}
}

func TestCreateMRComment_NeutralizesQuickActions(t *testing.T) {
	api, client := startNoteAPI(t)

	require.NoError(t, client.CreateMRComment(
		context.Background(), testProject, 1, "/approve\nreview body"))
	require.Len(t, api.createdBodies, 1)
	assert.Contains(t, api.createdBodies[0], `\/approve`)
	assert.NotContains(t, api.createdBodies[0], "\n/approve")
}

func TestUpsertMRComment_NeutralizesQuickActionsOnBothPaths(t *testing.T) {
	tests := []struct {
		name     string
		existing []*gogitlab.Note
		bodies   func(api *noteAPIServer) []string
	}{
		{
			name:   "Create",
			bodies: func(api *noteAPIServer) []string { return api.createdBodies },
		},
		{
			name:     "Update",
			existing: []*gogitlab.Note{note(42, CommentMarker+"\nold", false)},
			bodies:   func(api *noteAPIServer) []string { return api.updatedBodies },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api, client := startNoteAPI(t)
			api.notesByMR[1] = tt.existing

			require.NoError(t, client.UpsertMRComment(
				context.Background(), testProject, 1,
				"/approve\n/close\nfindings"))
			bodies := tt.bodies(api)
			require.Len(t, bodies, 1)
			assert.Contains(t, bodies[0], `\/approve`)
			assert.Contains(t, bodies[0], `\/close`)
			assert.NotContains(t, bodies[0], "\n/approve")
			assert.NotContains(t, bodies[0], "\n/close")
		})
	}
}

// TestUpsertMRComment_ReescapingIsStable guards the upsert loop: feeding an
// already-prepared body back through the API must not grow backslashes.
func TestUpsertMRComment_ReescapingIsStable(t *testing.T) {
	first := prepareBody("/approve\nfindings")
	second := prepareBody(strings.TrimPrefix(first, CommentMarker+"\n"))

	assert.Equal(t, first, second)
	assert.Equal(t, 1, strings.Count(second, `\/approve`))
	assert.NotContains(t, second, `\\/approve`)
}

// TestPrepareBody_TruncationCannotExposeQuickAction guards the interaction
// between escaping and truncation: because escaping ignores fences, cutting a
// closing fence away can never turn a code sample back into a live command.
func TestPrepareBody_TruncationCannotExposeQuickAction(t *testing.T) {
	assert := assert.New(t)

	filler := strings.Repeat("x\n", (review.MaxCommentLen-200)/2)
	input := filler + "```\n/approve\n" + strings.Repeat("y\n", 500) + "```\n"

	body := prepareBody(input)
	assert.LessOrEqual(len(body), review.MaxCommentLen)
	assert.Contains(body, "truncated")
	assert.Contains(body, `\/approve`)
	assert.NotContains(body, "\n/approve")
}

func TestCreateMRComment_TruncationUTF8Safe(t *testing.T) {
	api, client := startNoteAPI(t)

	const truncSuffix = "\n\n...(truncated — comment exceeded size limit)"
	maxBody := review.MaxCommentLen - len(truncSuffix)
	markerOverhead := len(CommentMarker) + 1
	input := strings.Repeat("x", maxBody-markerOverhead-2) +
		"\U0001f600" + strings.Repeat("y", 100)

	require.NoError(t, client.CreateMRComment(
		context.Background(), testProject, 1, input))
	require.Len(t, api.createdBodies, 1)
	body := api.createdBodies[0]
	assert.True(t, strings.HasPrefix(body, CommentMarker))
	assert.Contains(t, body, "truncated")
	assert.LessOrEqual(t, len(body), review.MaxCommentLen)
	assert.True(t, utf8.ValidString(body))
}

// MergeRequestHeadSHA is what binds a reviewed range to the merge request the
// note lands on, so it has to read the source-branch head GitLab reports.
func TestMergeRequestRefs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			assert.Contains(t, r.URL.EscapedPath(),
				"group%2Fproject/merge_requests/7")
			assert.NotContains(t, r.URL.EscapedPath(), "notes")
			w.Header().Set("Content-Type", "application/json")
			assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{
				"iid": 7,
				"sha": "feedface00000000000000000000000000000000",
				"diff_refs": map[string]any{
					"base_sha": "ba5e0000000000000000000000000000000000ba",
					"head_sha": "feedface00000000000000000000000000000000",
				},
			}))
		}))
	t.Cleanup(srv.Close)

	client, err := NewClient("token", WithBaseURL(srv.URL))
	require.NoError(t, err)

	refs, err := client.MergeRequestRefs(
		context.Background(), "group/project", 7)
	require.NoError(t, err)
	assert.Equal(t, "feedface00000000000000000000000000000000", refs.HeadSHA)
	assert.Equal(t, "ba5e0000000000000000000000000000000000ba", refs.BaseSHA,
		"the diff base is what bounds the range, so it must be reported too")
}

// GitLab recomputes diff_refs asynchronously, so a force push can leave the
// reported base describing the previous head. Pairing that base with the new
// head would compare a range against a diff that never existed.
func TestMergeRequestRefs_RejectsDiffRefsForAnotherHead(t *testing.T) {
	const head = "feedface00000000000000000000000000000000"
	const staleHead = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"

	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{
				"iid": 7,
				"sha": head,
				"diff_refs": map[string]any{
					"base_sha": "ba5e0000000000000000000000000000000000ba",
					"head_sha": staleHead,
				},
			}))
		}))
	t.Cleanup(srv.Close)

	client, err := NewClient("token", WithBaseURL(srv.URL))
	require.NoError(t, err)

	_, err = client.MergeRequestRefs(context.Background(), "group/project", 7)
	require.Error(t, err)
	assert.Contains(t, err.Error(), head)
	assert.Contains(t, err.Error(), staleHead)
}

func TestMergeRequestRefs_RejectsBadProject(t *testing.T) {
	client, err := NewClient("token")
	require.NoError(t, err)

	_, err = client.MergeRequestRefs(context.Background(), "nogroup", 7)
	require.ErrorContains(t, err, "invalid GitLab project")
}
