//go:build kata && !windows

package kata_test

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/kata"
	"go.kenn.io/roborev/internal/kata/katatest"
)

// evidenceOf returns text of exactly n runes, in words of five runes, so the
// byte length is above n and the normalized rune count is n.
func evidenceOf(n int) string {
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

// kataCLI runs the real kata binary in the disposable home and workspace.
func kataCLI(ctx context.Context, k katatest.RealKata, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "kata", args...)
	cmd.Dir, cmd.Env = k.Repo, k.Env
	return cmd.CombinedOutput()
}

// TestAPIClientCommentsE2E runs the comment API against a real Kata: UIDs,
// plain and typed comments, replay, each distinct rejection, the thread cap,
// and the evidence rule at 39, 40, and 41 runes.
func TestAPIClientCommentsE2E(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	k := katatest.StartReal(t, ctx)
	cfg := config.KataIntegrationConfig{
		Enabled: true, Endpoint: k.SocketEndpoint(t, ctx), DefaultProject: "e2etest", Teammate: "roborev",
	}
	c, err := kata.NewAPIClient(k.Repo, cfg)
	require.NoError(t, err)
	defer c.CloseIdleConnections()

	created, err := c.Create(ctx, kata.CreateReq{Title: "findings", IdempotencyKey: "e2e:comments:issue"})
	require.NoError(t, err)

	t.Run("UIDs", func(t *testing.T) {
		binding, err := c.ResolveBinding(ctx)
		require.NoError(t, err)
		assert.NotEmpty(t, binding.ProjectUID)
		assert.Equal(t, binding.ProjectUID, created.ProjectUID)
		shown, err := c.Show(ctx, created.ShortID)
		require.NoError(t, err)
		assert.Equal(t, created.IssueUID, shown.UID)
		assert.Equal(t, binding.ProjectUID, shown.ProjectUID)
	})

	root, err := c.Comment(ctx, kata.CommentReq{Issue: created.ShortID, Body: "root finding", IdempotencyKey: "e2e:comments:root"})
	require.NoError(t, err)
	assert.True(t, root.Changed)
	assert.Equal(t, "roborev", root.Attribution.Teammate)

	refute := kata.CommentReq{
		Issue: created.ShortID, Body: evidenceOf(60), Kind: kata.CommentRefute, ReplyTo: root.UID,
		Teammate: "roborev-claude-code", IdempotencyKey: "e2e:comments:refute",
	}
	t.Run("typed reply, replay, and mismatch", func(t *testing.T) {
		first, err := c.Comment(ctx, refute)
		require.NoError(t, err)
		assert.True(t, first.Changed)
		assert.Equal(t, kata.CommentRefute, first.Kind)
		assert.Equal(t, root.UID, first.ReplyTo)
		assert.Equal(t, "roborev-claude-code", first.Attribution.Teammate)

		replayed, err := c.Comment(ctx, refute)
		require.NoError(t, err)
		assert.False(t, replayed.Changed, "a replay is a success that reports changed=false")
		assert.Equal(t, first.UID, replayed.UID)

		changed := refute
		changed.Body = evidenceOf(61)
		_, err = c.Comment(ctx, changed)
		require.ErrorIs(t, err, kata.ErrIdempotencyMismatch)
		require.ErrorIs(t, err, kata.ErrConflict)

		// The same actor, teammate, kind, and target again, under a new key.
		again := refute
		again.IdempotencyKey = "e2e:comments:refute-again"
		_, err = c.Comment(ctx, again)
		require.ErrorIs(t, err, kata.ErrDuplicateReply)
		dup, ok := errors.AsType[*kata.CommentError](err)
		require.True(t, ok)
		assert.Equal(t, first.UID, dup.ExistingUID, "Kata names the comment that already holds the reply")
	})

	t.Run("comments are read back with their replies", func(t *testing.T) {
		shown, err := c.Show(ctx, created.ShortID)
		require.NoError(t, err)
		require.Len(t, shown.Comments, 2)
		assert.Equal(t, root.UID, shown.Comments[0].UID)
		assert.True(t, strings.HasPrefix(shown.Comments[0].Handle, "c:"), shown.Comments[0].Handle)
		assert.Equal(t, shown.UID, shown.Comments[0].IssueUID)
		assert.Equal(t, kata.CommentRefute, shown.Comments[1].Kind)
		assert.Equal(t, root.UID, shown.Comments[1].ReplyTo)
		assert.Nil(t, shown.Comments[1].EditedAt)
	})

	t.Run("missing target", func(t *testing.T) {
		_, err := c.Comment(ctx, kata.CommentReq{
			Issue: created.ShortID, Body: "x", Kind: kata.CommentReply, ReplyTo: "01J00000000000000000000000", IdempotencyKey: "e2e:comments:missing",
		})
		require.ErrorIs(t, err, kata.ErrCommentNotFound)
		_, err = c.Thread(ctx, kata.ThreadReq{Issue: created.ShortID, Root: "01J00000000000000000000000"})
		require.ErrorIs(t, err, kata.ErrCommentNotFound)
	})

	t.Run("evidence rule matches Kata at 39, 40, and 41 runes", func(t *testing.T) {
		// The client refuses short evidence itself. Ask the real kata CLI, which
		// sends the text as written, so the rule is checked against Kata and not
		// against the client's own copy of it.
		for _, tc := range []struct {
			runes int
			ok    bool
		}{{39, false}, {40, true}, {41, true}} {
			body := evidenceOf(tc.runes)
			require.Equal(t, tc.runes, kata.EvidenceRunes(body))
			out, err := kataCLI(ctx, k, "comment", created.ShortID, "--confirm", root.UID, "-m", body, "--teammate", fmt.Sprintf("cli-%d", tc.runes), "--as", "e2e-cli")
			if tc.ok {
				require.NoError(t, err, "%d runes: %s", tc.runes, out)
			} else {
				require.Error(t, err, "%d runes should be refused: %s", tc.runes, out)
			}
			assert.Equal(t, tc.ok, kata.ValidateEvidence(kata.CommentConfirm, body) == nil, "the client agrees at %d runes", tc.runes)
		}
	})

	t.Run("thread cap and truncation", func(t *testing.T) {
		for i := range 55 {
			_, err := c.Comment(ctx, kata.CommentReq{
				Issue: created.ShortID, Body: "reply", Kind: kata.CommentReply, ReplyTo: root.UID,
				Teammate: fmt.Sprintf("t%02d", i), IdempotencyKey: fmt.Sprintf("e2e:comments:r%02d", i),
			})
			require.NoError(t, err)
		}
		thread, err := c.Thread(ctx, kata.ThreadReq{Issue: created.ShortID, Root: root.UID})
		require.NoError(t, err)
		assert.True(t, thread.Truncated, "Kata says it cut the thread")
		assert.Len(t, thread.Comments, 50, "internal/commentref/graph.go:313-324 at v0.19.0")
		assert.Equal(t, root.UID, thread.Comments[0].UID)

		whole, err := c.Show(ctx, created.ShortID)
		require.NoError(t, err)
		assert.False(t, whole.CommentsTruncated)
		assert.Greater(t, len(whole.Comments), 50, "reading the issue is not capped")
	})

	t.Run("closed issue", func(t *testing.T) {
		out, err := kataCLI(ctx, k, "close", created.ShortID, "--done", "-m", "Closed by the end-to-end test after the replies above were verified.", "--test", "go test -tags kata ./internal/kata/", "--as", "e2e-cli")
		require.NoError(t, err, string(out))
		_, err = c.Comment(ctx, kata.CommentReq{
			Issue: created.ShortID, Body: "late", Kind: kata.CommentReply, ReplyTo: root.UID,
			Teammate: "after-close", IdempotencyKey: "e2e:comments:closed",
		})
		require.ErrorIs(t, err, kata.ErrIssueClosed)
		_, err = c.Comment(ctx, kata.CommentReq{Issue: created.ShortID, Body: "a plain note", IdempotencyKey: "e2e:comments:closed-note"})
		require.NoError(t, err, "a plain comment is allowed on a closed issue")
	})
}
