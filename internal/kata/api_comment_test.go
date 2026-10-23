package kata

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gen "go.kenn.io/kata/pkg/client/generated"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/kata/katafake"
)

// evidence returns text of exactly n Unicode runes whose words are separated by
// single spaces. Each rune past the first word is multi-byte, so the byte length
// is well above n.
func evidence(n int) string {
	var b strings.Builder
	for i := range n {
		if i%6 == 5 {
			b.WriteByte(' ')
			continue
		}
		b.WriteRune('é')
	}
	return b.String()
}

func TestAPIClientExposesFullProjectAndIssueUIDs(t *testing.T) {
	f := katafake.New(t)
	issue := f.AddIssue(7, "abc4", "Task", "")
	c := apiClientFor(t, f, bindWorkspace(t, "reviews"))

	t.Run("binding resolves the project UID", func(t *testing.T) {
		b, err := c.ResolveBinding(t.Context())
		require.NoError(t, err)
		assert.Equal(t, "reviews", b.Project)
		assert.Equal(t, f.Projects[0].UID, b.ProjectUID)
	})
	t.Run("a binding without a reachable project fails", func(t *testing.T) {
		_, err := apiClientFor(t, f, bindWorkspace(t, "ghost")).ResolveBinding(t.Context())
		require.ErrorIs(t, err, ErrProjectNotFound)
		_, err = apiClientFor(t, f, bindWorkspace(t, ""), func(c *config.KataIntegrationConfig) { c.DefaultProject = "" }).ResolveBinding(t.Context())
		require.ErrorIs(t, err, ErrNoBinding)
	})
	t.Run("show", func(t *testing.T) {
		got, err := c.Show(t.Context(), "abc4")
		require.NoError(t, err)
		assert.Equal(t, issue.Out.UID, got.UID)
		assert.Equal(t, f.Projects[0].UID, got.ProjectUID)
	})
	t.Run("create", func(t *testing.T) {
		created, err := c.Create(t.Context(), CreateReq{Title: "t", IdempotencyKey: "uids"})
		require.NoError(t, err)
		assert.NotEmpty(t, created.IssueUID)
		assert.Equal(t, f.Projects[0].UID, created.ProjectUID)
		shown, err := c.Show(t.Context(), created.IssueUID)
		require.NoError(t, err)
		assert.Equal(t, created.IssueUID, shown.UID, "the UID create returned names the issue it created")
	})
}

func TestAPIClientShowReadsComments(t *testing.T) {
	f := katafake.New(t)
	issue := f.AddIssue(7, "abc4", "Task", "")
	c := apiClientFor(t, f, bindWorkspace(t, "reviews"))

	root, err := c.Comment(t.Context(), CommentReq{Issue: "abc4", Body: "root finding", Teammate: "roborev", IdempotencyKey: "root"})
	require.NoError(t, err)
	reply, err := c.Comment(t.Context(), CommentReq{
		Issue: "abc4", Body: evidence(40), Kind: CommentRefute, ReplyTo: root.UID,
		Teammate: "roborev-claude-code", IdempotencyKey: "reply",
	})
	require.NoError(t, err)
	edited := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
	issue.Comments[0].EditedAt = &edited

	got, err := c.Show(t.Context(), "abc4")
	require.NoError(t, err)
	require.Len(t, got.Comments, 2)
	assert.False(t, got.CommentsTruncated)

	first, second := got.Comments[0], got.Comments[1]
	assert := assert.New(t)
	assert.Equal(root.UID, first.UID)
	assert.True(strings.HasPrefix(first.Handle, "c:"), first.Handle)
	assert.Equal(issue.Out.UID, first.IssueUID)
	assert.Equal("root finding", first.Body)
	assert.Equal("roborev", first.Author)
	assert.Equal("roborev", first.Teammate)
	assert.Empty(first.ReplyTo)
	assert.Empty(first.Kind)
	require.NotNil(t, first.EditedAt)
	assert.True(edited.Equal(*first.EditedAt))
	assert.False(first.CreatedAt.IsZero())

	assert.Equal(reply.UID, second.UID)
	assert.Equal(root.UID, second.ReplyTo)
	assert.Equal(CommentRefute, second.Kind)
	assert.Equal("roborev-claude-code", second.Teammate)
	assert.Nil(second.EditedAt, "an unedited comment has no edit time")
}

func TestAPIClientComment(t *testing.T) {
	f := katafake.New(t)
	f.AddIssue(7, "abc4", "T", "")
	c := apiClientFor(t, f, bindWorkspace(t, "reviews"))

	root, err := c.Comment(t.Context(), CommentReq{Issue: "abc4", Body: "finding", IdempotencyKey: "root"})
	require.NoError(t, err)
	assert.True(t, root.Changed)
	assert.NotEmpty(t, root.UID)

	req := CommentReq{
		Issue: "abc4", Body: evidence(60), Kind: CommentRefute, ReplyTo: root.UID,
		Teammate: "roborev-claude-code", IdempotencyKey: "reply",
	}
	reply, err := c.Comment(t.Context(), req)
	require.NoError(t, err)
	post := lastPost(t, f)
	body := post.JSON(t)
	assert.Equal(t, "refute", body["kind"])
	assert.Equal(t, root.UID, body["reply_to"])
	assert.Equal(t, "roborev-claude-code", body["teammate"])
	assert.Equal(t, "roborev", body["actor"])
	assert.NotContains(t, body, "force", "a duplicate reply is never forced")
	assert.Equal(t, "reply", post.Header.Get("Idempotency-Key"))
	assert.Equal(t, CommentRefute, reply.Kind)
	assert.Equal(t, root.UID, reply.ReplyTo)
	assert.Equal(t, "roborev-claude-code", reply.Attribution.Teammate)

	replayed, err := c.Comment(t.Context(), req)
	require.NoError(t, err, "a replay is a success")
	assert.False(t, replayed.Changed, "and reports changed=false")
	assert.Equal(t, reply.UID, replayed.UID)

	changed := req
	changed.Body = evidence(61)
	_, err = c.Comment(t.Context(), changed)
	require.ErrorIs(t, err, ErrIdempotencyMismatch)
	require.ErrorIs(t, err, ErrConflict, "the typed error is still a conflict")
	require.NotErrorIs(t, err, ErrDuplicateReply)
	assert.Equal(t, "idempotency_mismatch", ErrorCode(err))
}

func TestAPIClientCommentRecordsTheAttributionKataReturns(t *testing.T) {
	f := katafake.New(t)
	f.Principal = "svc-roborev"
	f.AddIssue(7, "abc4", "T", "")
	c := apiClientFor(t, f, bindWorkspace(t, "reviews"))
	res, err := c.Comment(t.Context(), CommentReq{Issue: "abc4", Body: "finding", Teammate: "roborev", IdempotencyKey: "k"})
	require.NoError(t, err)
	assert.Equal(t, "svc-roborev", res.Attribution.Author, "what Kata returns is what is recorded")
	assert.Equal(t, "roborev", lastPost(t, f).JSON(t)["actor"])
}

func TestAPIClientCommentDefaultsTheTeammate(t *testing.T) {
	f := katafake.New(t)
	f.AddIssue(7, "abc4", "T", "")
	c := apiClientFor(t, f, bindWorkspace(t, "reviews"), func(c *config.KataIntegrationConfig) { c.Teammate = "review-bot" })
	_, err := c.Comment(t.Context(), CommentReq{Issue: "abc4", Body: "b", IdempotencyKey: "k"})
	require.NoError(t, err)
	assert.Equal(t, "review-bot", lastPost(t, f).JSON(t)["teammate"])
}

func TestAPIClientCommentValidatesBeforeSending(t *testing.T) {
	f := katafake.New(t)
	f.AddIssue(7, "abc4", "T", "")
	c := apiClientFor(t, f, bindWorkspace(t, "reviews"))
	validReplyUID := "C0000000000000000000000001"
	for name, req := range map[string]CommentReq{
		"bad kind":              {Issue: "abc4", Body: "b", Kind: "approve", ReplyTo: validReplyUID, IdempotencyKey: "k"},
		"kind without a target": {Issue: "abc4", Body: "b", Kind: CommentReply, IdempotencyKey: "k"},
		"target without a kind": {Issue: "abc4", Body: "b", ReplyTo: validReplyUID, IdempotencyKey: "k"},
		"bad teammate":          {Issue: "abc4", Body: "b", Teammate: "-nope", IdempotencyKey: "k"},
		"empty body":            {Issue: "abc4", Body: "  ", IdempotencyKey: "k"},
		"no key":                {Issue: "abc4", Body: "b"},
		"hostile issue":         {Issue: "a/b", Body: "b", IdempotencyKey: "k"},
		"hostile target":        {Issue: "abc4", Body: "b", Kind: CommentReply, ReplyTo: "C1/../x", IdempotencyKey: "k"},
	} {
		_, err := c.Comment(t.Context(), req)
		require.ErrorIs(t, err, ErrRequestRejected, name)
	}
	assert.Empty(t, f.AllRequests())
}

func TestAPIClientTypedRepliesNeedAServerThatStoresThem(t *testing.T) {
	f := katafake.New(t)
	f.APISchema = "0.27.0"
	f.AddIssue(7, "abc4", "T", "")
	c := apiClientFor(t, f, bindWorkspace(t, "reviews"))

	_, err := c.Comment(t.Context(), CommentReq{Issue: "abc4", Body: "b", Kind: CommentReply, ReplyTo: "C0000000000000000000000001", IdempotencyKey: "k"})
	require.ErrorIs(t, err, ErrRepliesUnsupported)
	for _, r := range f.Requests() {
		assert.NotContains(t, r.Path, "/comments", "an old server is never sent a reply it would store as a plain comment")
	}

	asked := len(f.Requests())
	_, err = c.Thread(t.Context(), ThreadReq{Issue: "abc4", Root: "C0000000000000000000000001"})
	require.ErrorIs(t, err, ErrRepliesUnsupported, "an old server would ignore the selection and answer with the whole issue")
	assert.Len(t, f.Requests(), asked, "and is not asked")

	_, err = c.Comment(t.Context(), CommentReq{Issue: "abc4", Body: "plain", IdempotencyKey: "k2"})
	require.NoError(t, err, "a plain comment works on any compatible server")
}

func TestAPIClientRefusesATypedReplyKataStoredAsAPlainComment(t *testing.T) {
	f := katafake.New(t)
	f.AddIssue(7, "abc4", "T", "")
	root, err := apiClientFor(t, f, bindWorkspace(t, "reviews")).Comment(t.Context(), CommentReq{Issue: "abc4", Body: "root", IdempotencyKey: "root"})
	require.NoError(t, err)
	// A server that ignores the reply fields answers with a plain comment.
	f.Override = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/comments") {
			return false
		}
		f.Reply(w, gen.CommentResponseBody{Changed: true, Comment: gen.Comment{UID: "C1", Author: "roborev", Body: "x"}})
		return true
	}
	c := apiClientFor(t, f, bindWorkspace(t, "reviews"))
	_, err = c.Comment(t.Context(), CommentReq{Issue: "abc4", Body: "ok", Kind: CommentReply, ReplyTo: root.UID, IdempotencyKey: "k"})
	require.ErrorIs(t, err, ErrInvalidResponse)
}

func TestAPIClientCommentErrorsAreDistinct(t *testing.T) {
	f := katafake.New(t)
	open := f.AddIssue(7, "abc4", "Open", "")
	closed := f.AddIssue(7, "done", "Closed", "")
	c := apiClientFor(t, f, bindWorkspace(t, "reviews"))
	root, err := c.Comment(t.Context(), CommentReq{Issue: "abc4", Body: "finding", IdempotencyKey: "root"})
	require.NoError(t, err)
	closedRoot, err := c.Comment(t.Context(), CommentReq{Issue: "done", Body: "finding", IdempotencyKey: "closed-root"})
	require.NoError(t, err)
	closed.Out.Status = "closed"

	reply := func(key, teammate string) CommentReq {
		return CommentReq{Issue: "abc4", Body: evidence(50), Kind: CommentConfirm, ReplyTo: root.UID, Teammate: teammate, IdempotencyKey: key}
	}
	first, err := c.Comment(t.Context(), reply("first", "roborev-codex"))
	require.NoError(t, err)

	t.Run("duplicate reply names the existing comment", func(t *testing.T) {
		_, err := c.Comment(t.Context(), reply("second", "roborev-codex"))
		require.ErrorIs(t, err, ErrDuplicateReply)
		require.ErrorIs(t, err, ErrConflict)
		assert.Equal(t, "duplicate_reply", ErrorCode(err))
		assert.Contains(t, err.Error(), "duplicate_reply", "a rejection by Kata keeps Kata's own code and message")
		dup, ok := errors.AsType[*CommentError](err)
		require.True(t, ok)
		assert.Equal(t, first.UID, dup.ExistingUID)
		assert.NotContains(t, lastPost(t, f).JSON(t), "force")
		assert.Len(t, open.Comments, 2, "nothing was posted")
	})
	t.Run("closed issue", func(t *testing.T) {
		_, err := c.Comment(t.Context(), CommentReq{Issue: "done", Body: "x", Kind: CommentReply, ReplyTo: closedRoot.UID, IdempotencyKey: "closed"})
		require.ErrorIs(t, err, ErrIssueClosed)
		require.ErrorIs(t, err, ErrConflict)
		assert.Equal(t, "issue_closed", ErrorCode(err))
	})
	t.Run("missing target", func(t *testing.T) {
		_, err := c.Comment(t.Context(), CommentReq{Issue: "abc4", Body: "x", Kind: CommentReply, ReplyTo: "C0000000000000000000000999", IdempotencyKey: "missing"})
		require.ErrorIs(t, err, ErrCommentNotFound)
		require.ErrorIs(t, err, ErrNotFound)
		assert.Equal(t, "comment_not_found", ErrorCode(err))
	})
	t.Run("missing issue is not a missing comment", func(t *testing.T) {
		_, err := c.Comment(t.Context(), CommentReq{Issue: "nope", Body: "x", IdempotencyKey: "no-issue"})
		require.ErrorIs(t, err, ErrNotFound)
		require.NotErrorIs(t, err, ErrCommentNotFound)
	})
}

// Kata refuses a confirm or refute whose body has fewer than 40 Unicode runes
// after whitespace is normalized (internal/db/comment_reply_write.go:49 at
// v0.19.0). The check runs here first so a short message is rejected with its
// kind unchanged, and nothing is sent.
func TestEvidenceLengthCountsRunesAfterNormalizingWhitespace(t *testing.T) {
	tests := []struct {
		name string
		body string
		runs int
		ok   bool
	}{
		{"39 runes", evidence(39), 39, false},
		{"40 runes", evidence(40), 40, true},
		{"41 runes", evidence(41), 41, true},
		{"39 runes in more than 39 bytes", strings.Repeat("é", 39), 39, false},
		{"40 runes where spaces collapse", strings.Repeat("é", 20) + "  \t\n  " + strings.Repeat("é", 19), 40, true},
		{"41 raw runes collapse to 39", strings.Repeat("é", 19) + "    \n" + strings.Repeat("é", 19), 39, false},
		{"leading and trailing space does not count", "  " + evidence(39) + "\n\n", 39, false},
		{"only space", strings.Repeat(" \n\t", 40), 0, false},
		{"empty", "", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.runs, EvidenceRunes(tt.body))
			for _, kind := range []CommentKind{CommentConfirm, CommentRefute} {
				err := ValidateEvidence(kind, tt.body)
				if tt.ok {
					require.NoError(t, err, kind)
				} else {
					require.ErrorIs(t, err, ErrEvidenceTooShort, kind)
				}
			}
			for _, kind := range []CommentKind{CommentReply, CommentSupersede, ""} {
				require.NoError(t, ValidateEvidence(kind, tt.body), "only confirm and refute need evidence: %q", kind)
			}
		})
	}
}

func TestAPIClientRejectsShortEvidenceWithoutChangingTheKind(t *testing.T) {
	f := katafake.New(t)
	f.AddIssue(7, "abc4", "T", "")
	c := apiClientFor(t, f, bindWorkspace(t, "reviews"))
	root, err := c.Comment(t.Context(), CommentReq{Issue: "abc4", Body: "finding", IdempotencyKey: "root"})
	require.NoError(t, err)
	before := len(f.AllRequests())

	for _, kind := range []CommentKind{CommentConfirm, CommentRefute} {
		_, err := c.Comment(t.Context(), CommentReq{Issue: "abc4", Body: evidence(39), Kind: kind, ReplyTo: root.UID, IdempotencyKey: "short-" + string(kind)})
		require.ErrorIs(t, err, ErrEvidenceTooShort, kind)
		require.ErrorIs(t, err, ErrRequestRejected)
		assert.Contains(t, err.Error(), "a "+string(kind)+" needs at least 40 characters", "the message says what is missing")
		assert.Contains(t, err.Error(), "this has 39", "and how much there was")
	}
	assert.Len(t, f.AllRequests(), before, "a short message is rejected before anything is sent")

	for _, n := range []int{40, 41} {
		got, err := c.Comment(t.Context(), CommentReq{
			Issue: "abc4", Body: evidence(n), Kind: CommentConfirm, ReplyTo: root.UID,
			Teammate: fmt.Sprintf("t%d", n), IdempotencyKey: fmt.Sprintf("ok-%d", n),
		})
		require.NoError(t, err, n)
		assert.Equal(t, CommentConfirm, got.Kind)
	}
}

// FuzzEvidenceRunesIgnoresHowSpaceIsWritten builds text with a known number of
// runes after normalization and writes every space as a different run of
// Unicode whitespace, so the count must not move.
func FuzzEvidenceRunesIgnoresHowSpaceIsWritten(f *testing.F) {
	f.Add(uint8(40), uint8(3), uint8(0))
	f.Add(uint8(39), uint8(1), uint8(2))
	f.Add(uint8(41), uint8(7), uint8(5))
	spaces := []string{" ", "\t", "\n", "\r\n", "\u00a0", "\u2003", "\u3000", "  \n\t "}
	f.Fuzz(func(t *testing.T, n, padSeed, spaceSeed uint8) {
		want := int(n)
		// Every sixth rune is a space. The text must not end on one, because
		// trailing space is trimmed and the count would be one short.
		if want == 0 || want%6 == 0 {
			t.Skip()
		}
		var b strings.Builder
		b.WriteString(spaces[int(padSeed)%len(spaces)])
		for i := range want {
			if i%6 == 5 {
				b.WriteString(spaces[(int(spaceSeed)+i)%len(spaces)])
				continue
			}
			b.WriteRune('é')
		}
		b.WriteString(spaces[int(spaceSeed)%len(spaces)])
		require.Equal(t, want, EvidenceRunes(b.String()), "%q", b.String())
	})
}

func TestAPIClientThread(t *testing.T) {
	f := katafake.New(t)
	root := gen.CommentOut{UID: "C0000000000000000000000001", Body: "finding"}
	reply := gen.CommentOut{UID: "C0000000000000000000000002", Body: "reply", ReplyToUID: &root.UID}
	f.Override = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/projects/7/issues/abc4" {
			return false
		}
		f.Reply(w, gen.ShowIssueResponseBody{
			Issue:  gen.ShowIssueOut{ShortID: "abc4", UID: "I1", Status: "open"},
			Labels: []gen.IssueLabel{}, Links: []gen.LinkOut{}, Comments: []gen.CommentOut{root, reply},
		})
		return true
	}
	c := apiClientFor(t, f, bindWorkspace(t, "reviews"))

	got, err := c.Thread(t.Context(), ThreadReq{Issue: "abc4", Root: root.UID})
	require.NoError(t, err)
	assert.False(t, got.Truncated)
	assert.Equal(t, []Comment{
		{UID: root.UID, Body: root.Body}, {UID: reply.UID, Body: reply.Body, ReplyTo: root.UID},
	}, got.Comments)

	get := f.Requests()[len(f.Requests())-1]
	assert.Equal(t, http.MethodGet, get.Method)
	assert.Equal(t, url.Values{"thread": {root.UID}}, get.Query)
}

// Thread preserves the comments and truncation flag returned by Kata. Reading
// an issue without a thread selection still returns its complete comments.
func TestAPIClientThreadReportsTruncation(t *testing.T) {
	f := katafake.New(t)
	f.AddIssue(7, "abc4", "T", "")
	root := gen.CommentOut{UID: "C0000000000000000000000001", Body: "root"}
	reply := gen.CommentOut{UID: "C0000000000000000000000002", Body: "reply"}
	other := gen.CommentOut{UID: "C0000000000000000000000003", Body: "another reply"}
	truncated := true
	f.Override = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/projects/7/issues/abc4" {
			return false
		}
		comments := []gen.CommentOut{root, reply, other}
		var commentsTruncated *bool
		if r.URL.Query().Get("thread") == root.UID {
			comments = []gen.CommentOut{root, reply}
			commentsTruncated = &truncated
		}
		f.Reply(w, gen.ShowIssueResponseBody{
			Issue:  gen.ShowIssueOut{ShortID: "abc4", UID: "I1", Status: "open"},
			Labels: []gen.IssueLabel{}, Links: []gen.LinkOut{}, Comments: comments,
			CommentsTruncated: commentsTruncated,
		})
		return true
	}
	c := apiClientFor(t, f, bindWorkspace(t, "reviews"))

	got, err := c.Thread(t.Context(), ThreadReq{Issue: "abc4", Root: root.UID})
	require.NoError(t, err)
	assert.True(t, got.Truncated)
	assert.Equal(t, []Comment{{UID: root.UID, Body: root.Body}, {UID: reply.UID, Body: reply.Body}}, got.Comments)

	whole, err := c.Show(t.Context(), "abc4")
	require.NoError(t, err)
	assert.Equal(t, []Comment{
		{UID: root.UID, Body: root.Body}, {UID: reply.UID, Body: reply.Body}, {UID: other.UID, Body: other.Body},
	}, whole.Comments)
	assert.False(t, whole.CommentsTruncated)
}

func TestAPIClientThreadErrors(t *testing.T) {
	f := katafake.New(t)
	f.AddIssue(7, "abc4", "T", "")
	f.Override = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/projects/7/issues/abc4" {
			return false
		}
		f.Fail(w, http.StatusNotFound, "comment_not_found", "comment not found")
		return true
	}
	c := apiClientFor(t, f, bindWorkspace(t, "reviews"))

	_, err := c.Thread(t.Context(), ThreadReq{Issue: "abc4", Root: "C0000000000000000000000999"})
	require.ErrorIs(t, err, ErrCommentNotFound)

	before := len(f.AllRequests())
	for name, req := range map[string]ThreadReq{
		"no root":      {Issue: "abc4"},
		"hostile root": {Issue: "abc4", Root: "a/b"},
		"no issue":     {Root: "C0000000000000000000000999"},
		"hostile ref":  {Issue: "a/b", Root: "C0000000000000000000000999"},
	} {
		_, err := c.Thread(t.Context(), req)
		require.ErrorIs(t, err, ErrRequestRejected, name)
	}
	assert.Len(t, f.AllRequests(), before)

	// An answer that leaves out the requested root is not a thread.
	f.Override = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodGet || !strings.Contains(r.URL.Path, "/issues/abc4") {
			return false
		}
		f.Reply(w, gen.ShowIssueResponseBody{
			Issue: gen.ShowIssueOut{ShortID: "abc4", UID: "I1", Status: "open"}, Labels: []gen.IssueLabel{}, Links: []gen.LinkOut{},
			Comments: []gen.CommentOut{{UID: "C0000000000000000000000777", Author: "x", Body: "y"}},
		})
		return true
	}
	_, err = c.Thread(t.Context(), ThreadReq{Issue: "abc4", Root: "C0000000000000000000000888"})
	require.ErrorIs(t, err, ErrInvalidResponse)
}

func TestCLIClientCannotReadOrWriteComments(t *testing.T) {
	c := NewCLIClient(t.TempDir())
	var runs int
	c.run = func(context.Context, *CLIClient, []string, io.Reader) ([]byte, error) {
		runs++
		return nil, nil
	}
	_, err := c.Comment(t.Context(), CommentReq{Issue: "abc4", Body: "b", Kind: CommentReply, ReplyTo: "C1", IdempotencyKey: "k"})
	require.ErrorIs(t, err, ErrCommentsUnsupported)
	_, err = c.Thread(t.Context(), ThreadReq{Issue: "abc4", Root: "C1"})
	require.ErrorIs(t, err, ErrCommentsUnsupported)
	assert.Zero(t, runs, "the kata CLI is not run for a comment operation")
}
