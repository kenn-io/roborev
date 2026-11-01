package git

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

func TestAgentSessionTrailers(t *testing.T) {
	message := "Subject\nAgent-Session: codex:01234567-89ab-4cde-8fab-0123456789ab\n\nsquashed\r\nAgent-Session:\tpi:session-1\t\r\nAgent-Session: pi:session-1\n Agent-Session: pi:indented\nAgent-Session: codex:not-a-uuid\nAgent-Session: Bad:id\n"
	assert.Equal(t, []AgentSession{{Agent: "codex", ID: "01234567-89ab-4cde-8fab-0123456789ab"}, {Agent: "pi", ID: "session-1"}}, ParseAgentSessions(message))
}

func TestAgentSessionTrailerOrderProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		ids := hegel.Draw(ht, hegel.Lists(hegel.Integers[uint64](0, ^uint64(0))))
		var lines []string
		var want []AgentSession
		seen := map[string]bool{}
		for _, id := range ids {
			raw := fmt.Sprintf("s-%x", id)
			lines = append(lines, "Agent-Session: pi:"+raw, "Agent-Session: pi:"+raw)
			if !seen[raw] {
				want = append(want, AgentSession{Agent: "pi", ID: raw})
				seen[raw] = true
			}
		}
		assert.Equal(ht, want, ParseAgentSessions(strings.Join(lines, "\r\n")))
	}, hegel.WithTestCases(2000))
}

func TestAgentSessionTrailerRejectsIndentedProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		text := hegel.Draw(ht, hegel.Text())
		// Every line is indented, so even arbitrary embedded trailer text is data.
		message := " " + strings.ReplaceAll(strings.ReplaceAll(text, "\r", "\n"), "\n", "\n ")
		assert.Empty(ht, ParseAgentSessions(message))
	}, hegel.WithTestCases(2000))
}

func TestGetCommitMessagePreservesTrailerLines(t *testing.T) {
	repo := NewTestRepoWithCommit(t)
	repo.CommitFile("raw.txt", "raw\n", "Subject\nAgent-Session: pi:first\n\n Agent-Session: pi:indented\n\nAgent-Session: pi:last")
	sha := repo.Run("rev-parse", "HEAD")
	message, err := GetCommitMessageCtx(t.Context(), repo.Dir, strings.TrimSpace(sha))
	require.NoError(t, err)
	assert.Contains(t, message, "\n Agent-Session: pi:indented\n")
	assert.Equal(t, []AgentSession{{Agent: "pi", ID: "first"}, {Agent: "pi", ID: "last"}}, ParseAgentSessions(message))
}
