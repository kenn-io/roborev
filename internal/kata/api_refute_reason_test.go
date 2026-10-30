package kata

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gen "go.kenn.io/kata/pkg/client/generated"

	"go.kenn.io/roborev/internal/kata/katafake"
)

func TestAPIClientNativeReasonReadbackAndEdits(t *testing.T) {
	for _, reason := range []string{"false-positive", "spurious", "risk-accepted", ""} {
		t.Run(reason, func(t *testing.T) {
			f := katafake.New(t)
			f.AddIssue(7, "abc4", "Task", "")
			evidence := strings.Repeat("界", 40)
			body := evidence
			state, wantReason := "absent", "unspecified"
			if reason != "" {
				body = "<!-- roborev-refute:v1 reason=" + reason + " -->\n" + evidence
				state, wantReason = "explicit", reason
			}
			edited := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
			f.Override = func(w http.ResponseWriter, r *http.Request) bool {
				if r.Method != http.MethodGet || r.URL.Path != "/api/v1/projects/7/issues/abc4" {
					return false
				}
				// This native server body has no local operation or publication state.
				f.Reply(w, gen.ShowIssueResponseBody{Issue: gen.ShowIssueOut{ShortID: "abc4", UID: "I1", Status: "open"}, Labels: []gen.IssueLabel{}, Links: []gen.LinkOut{}, Comments: []gen.CommentOut{{UID: "C0000000000000000000000001", Body: body, ReplyKind: new("refute"), ReplyToUID: new("C0000000000000000000000002"), Author: "native-author", Teammate: new("native-reviewer"), EditedAt: &edited}}})
				return true
			}
			client := apiClientFor(t, f, bindWorkspace(t, "reviews"))
			check := func(comments []Comment) {
				require.Len(t, comments, 1)
				c := comments[0]
				assert.Equal(t, body, c.Body)
				assert.Equal(t, CommentBody{Reason: wantReason, ReasonState: state, Evidence: evidence, EvidenceValid: true}, c.CommentBody)
				assert.Equal(t, "native-author", c.Author)
				assert.Equal(t, "native-reviewer", c.Teammate)
				assert.Equal(t, &edited, c.EditedAt)
			}
			shown, err := client.Show(t.Context(), "abc4")
			require.NoError(t, err)
			check(shown.Comments)
			thread, err := client.Thread(t.Context(), ThreadReq{Issue: "abc4", Root: "C0000000000000000000000001"})
			require.NoError(t, err)
			check(thread.Comments)
			body = "<!-- roborev-refute:v1 reason=unknown -->\n" + evidence
			shown, err = client.Show(t.Context(), "abc4")
			require.NoError(t, err)
			assert.Equal(t, body, shown.Comments[0].Body)
			assert.Equal(t, "invalid", shown.Comments[0].ReasonState)
			assert.False(t, shown.Comments[0].EvidenceValid)
			body = "<!-- roborev-refute:v1 reason=spurious -->\n"
			shown, err = client.Show(t.Context(), "abc4")
			require.NoError(t, err)
			assert.Equal(t, "explicit", shown.Comments[0].ReasonState)
			assert.Empty(t, shown.Comments[0].Evidence)
			assert.False(t, shown.Comments[0].EvidenceValid)
		})
	}
}

func TestAPIClientRejectsMarkerOnlyEvidenceBeforeNetwork(t *testing.T) {
	f := katafake.New(t)
	client := apiClientFor(t, f, bindWorkspace(t, "reviews"))
	for _, body := range []string{"<!-- roborev-refute:v1 reason=spurious -->\n", "<!-- roborev-refute:v2 reason=spurious -->\n" + strings.Repeat("界", 40)} {
		_, err := client.Comment(t.Context(), CommentReq{Issue: "abc4", Kind: CommentRefute, ReplyTo: "C0000000000000000000000001", Body: body, IdempotencyKey: "attempt"})
		require.Error(t, err)
	}
	assert.Empty(t, f.Requests())
}

func TestAPIClientReasonWriteReceiptAndNativeIdempotency(t *testing.T) {
	for _, reason := range []string{"false-positive", "spurious", "risk-accepted"} {
		t.Run(reason, func(t *testing.T) {
			f := katafake.New(t)
			f.AddIssue(7, "abc4", "Task", "")
			client := apiClientFor(t, f, bindWorkspace(t, "reviews"))
			root, err := client.Comment(t.Context(), CommentReq{Issue: "abc4", Body: "Finding root", IdempotencyKey: "root"})
			require.NoError(t, err)
			body := "<!-- roborev-refute:v1 reason=" + reason + " -->\n" + strings.Repeat("界", 40)
			req := CommentReq{Issue: "abc4", Kind: CommentRefute, ReplyTo: root.UID, Body: body, IdempotencyKey: "refute"}
			stored, err := client.Comment(t.Context(), req)
			require.NoError(t, err)
			assert.Equal(t, body, stored.Body)
			assert.Equal(t, reason, stored.Reason)
			assert.Equal(t, "explicit", stored.ReasonState)
			assert.Equal(t, strings.Repeat("界", 40), stored.Evidence)
			assert.True(t, stored.EvidenceValid)
			replay, err := client.Comment(t.Context(), req)
			require.NoError(t, err)
			assert.Equal(t, stored.UID, replay.UID)
			assert.False(t, replay.Changed)
			changed := "spurious"
			if reason == changed {
				changed = "false-positive"
			}
			req.Body = "<!-- roborev-refute:v1 reason=" + changed + " -->\n" + strings.Repeat("界", 40)
			_, err = client.Comment(t.Context(), req)
			require.ErrorIs(t, err, ErrIdempotencyMismatch)
			reader := apiClientFor(t, f, bindWorkspace(t, "reviews"))
			shown, err := reader.ShowComplete(t.Context(), "abc4")
			require.NoError(t, err)
			require.Len(t, shown.Comments, 2)
			assert.Equal(t, body, shown.Comments[1].Body)
			assert.Equal(t, reason, shown.Comments[1].Reason)
		})
	}
}
