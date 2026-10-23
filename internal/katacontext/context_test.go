package katacontext

import (
	"encoding/json/v2"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/kata"
)

// Neither a claimed fix nor a conflicting refutation may disappear or turn
// into a resolution decision in the context sent to a reviewing agent.
func TestFormatKeepsConflictingClaimsAndAvailability(t *testing.T) {
	edited := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	input := []Snapshot{{IssueRef: "reviews#abcd", ReviewUUID: "review-original", Findings: []Finding{{
		Selector: "f:example", Availability: "target_unavailable", RootUID: "root-comment",
		Replies: []kata.Comment{
			{UID: "claim-a", Kind: kata.CommentReply, Body: "Fixed in abc123; the focused regression passes.", Author: "developer", Teammate: "fixer"},
			{UID: "claim-b", Kind: kata.CommentRefute, Body: "The original error is reproducible with the documented input.", EditedAt: &edited, Author: "reviewer", Teammate: "validator"},
		}, Truncated: true,
	}}}}
	text := Format(input)
	assert.Contains(t, text, "untrusted")
	assert.Contains(t, text, "claims")
	assert.Contains(t, text, "instructions")
	assert.Contains(t, text, "target_unavailable")
	assert.Contains(t, text, "truncated")
	var captured []Snapshot
	start := strings.Index(text, "[")
	require.NotEqual(t, -1, start)
	require.NoError(t, json.Unmarshal([]byte(text[start:]), &captured))
	require.Len(t, captured, 1)
	require.Len(t, captured[0].Findings, 1)
	finding := captured[0].Findings[0]
	assert.Equal(t, "target_unavailable", finding.Availability)
	assert.True(t, finding.Truncated)
	assert.Equal(t, input[0].Findings[0].Replies, finding.Replies)
}
