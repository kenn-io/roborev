package kata

import (
	"context"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReplyTypedRelation(t *testing.T) {
	for kind, flag := range map[string]string{"confirmed": "--confirm", "refuted": "--refute", "superseded": "--supersede"} {
		t.Run(kind, func(t *testing.T) {
			var got []string
			var body string
			c := &CLIClient{run: func(_ context.Context, _ *CLIClient, args []string, stdin io.Reader) ([]byte, error) {
				got = args
				b, err := io.ReadAll(stdin)
				require.NoError(t, err)
				body = string(b)
				return []byte(`{}`), nil
			}}
			err := c.Reply(context.Background(), ReplyReq{Issue: "abc4", Target: "c:abc123", Kind: kind, Body: "evidence", IdempotencyKey: "assessment-key"})
			require.NoError(t, err)
			assert.Equal(t, []string{"comment", "abc4", flag, "c:abc123", "--body-stdin", "--json", "--idempotency-key", "assessment-key"}, got)
			assert.Equal(t, "evidence", body)
		})
	}
	c, _ := stubClient("", ErrUnavailable)
	require.ErrorIs(t, c.Reply(context.Background(), ReplyReq{Issue: "abc4", Target: "c:abc123", Kind: "refuted", Body: "body"}), ErrUnavailable)
	called := false
	c = &CLIClient{run: func(context.Context, *CLIClient, []string, io.Reader) ([]byte, error) {
		called = true
		return []byte(`{}`), nil
	}}
	err := c.Reply(context.Background(), ReplyReq{
		Issue: "abc4", Target: "c:abc123", Kind: "unknown", Body: "evidence",
	})
	require.EqualError(t, err, `unsupported Kata reply kind "unknown"`)
	assert.False(t, called)
}
