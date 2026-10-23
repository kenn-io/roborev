package kata

import (
	"encoding/json/v2"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	gen "go.kenn.io/kata/pkg/client/generated"

	"go.kenn.io/roborev/internal/kata/katafake"
)

// Losing the server's link status makes an unavailable target look like a
// complete discussion. Exercise Show and Thread through their HTTP boundary.
func TestAPIReadbackPreservesUnavailableLinks(t *testing.T) {
	f := katafake.New(t)
	f.AddIssue(7, "abc4", "Finding", "")
	uid, handle, status := "C0000000000000000000000002", "c:target", "unavailable"
	body, author, teammate := "Claim with evidence", "reviewer", "validator"
	verification := gen.CommentOutVerificationPending
	edited := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	yes := true
	root := gen.CommentOut{
		UID: "C0000000000000000000000001", Author: author, Body: "Finding root",
		Reply:              &gen.CommentLink{UID: &uid, Handle: &handle, Status: &status, Kind: "refute", TargetEdited: &yes},
		Backlinks:          []gen.CommentLink{{UID: &uid, Body: &body, Author: &author, Teammate: &teammate, Kind: "refute", EditedAt: &edited}},
		BacklinksTruncated: &yes, Verification: &verification,
	}
	f.Override = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/projects/7/issues/abc4" {
			return false
		}
		f.Reply(w, gen.ShowIssueResponseBody{
			Issue:    gen.ShowIssueOut{ShortID: "abc4", UID: "I1", Status: "open"},
			Comments: []gen.CommentOut{root}, CommentsTruncated: &yes,
		})
		return true
	}
	c := apiClientFor(t, f, bindWorkspace(t, "reviews"))
	issue, err := c.Show(t.Context(), "abc4")
	require.NoError(t, err)
	thread, err := c.Thread(t.Context(), ThreadReq{Issue: "abc4", Root: root.UID})
	require.NoError(t, err)
	for _, comments := range [][]Comment{issue.Comments, thread.Comments} {
		require.Len(t, comments, 1)
		// Decode the public representation without depending on the new fields
		// at compile time: the baseline must fail because it drops server data.
		raw, err := json.Marshal(comments[0])
		require.NoError(t, err)
		var got struct {
			Reply *struct {
				UID          string `json:"uid"`
				Handle       string `json:"handle"`
				Status       string `json:"status"`
				Kind         string `json:"kind"`
				TargetEdited bool   `json:"target_edited"`
			}
			Backlinks []struct {
				UID      string     `json:"uid"`
				Body     string     `json:"body"`
				Author   string     `json:"author"`
				Teammate string     `json:"teammate"`
				Kind     string     `json:"kind"`
				EditedAt *time.Time `json:"edited_at"`
			}
			BacklinksTruncated bool
			Verification       string
		}
		require.NoError(t, json.Unmarshal(raw, &got))
		require.NotNil(t, got.Reply, "unavailable reply target was discarded")
		assert.Equal(t, uid, got.Reply.UID)
		assert.Equal(t, status, got.Reply.Status)
		assert.True(t, got.Reply.TargetEdited)
		require.Len(t, got.Backlinks, 1)
		assert.Equal(t, body, got.Backlinks[0].Body)
		assert.Equal(t, author, got.Backlinks[0].Author)
		assert.Equal(t, teammate, got.Backlinks[0].Teammate)
		assert.Equal(t, "refute", got.Backlinks[0].Kind)
		assert.Equal(t, &edited, got.Backlinks[0].EditedAt)
		assert.True(t, got.BacklinksTruncated)
		assert.Equal(t, "pending", got.Verification)
	}
	assert.True(t, issue.CommentsTruncated)
	assert.True(t, thread.Truncated)
}
