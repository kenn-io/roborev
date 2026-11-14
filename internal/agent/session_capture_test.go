package agent

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExtractSessionID(t *testing.T) {
	initCapture, err := os.ReadFile("testdata/antigravity-init-capture.jsonl")
	require.NoError(t, err)
	tests := []struct {
		name string
		line string
		want string
	}{
		{
			name: "claude init session_id",
			line: `{"type":"system","subtype":"init","session_id":"claude-session-123"}`,
			want: "claude-session-123",
		},
		{
			name: "camel sessionId",
			line: `{"type":"init","sessionId":"camel-session-456"}`,
			want: "camel-session-456",
		},
		{
			name: "pascal sessionID",
			line: `{"type":"text","sessionID":"pascal-session-999"}`,
			want: "pascal-session-999",
		},
		{
			name: "captured agy init conversation_id",
			line: string(initCapture),
			want: "...",
		},
		{
			name: "conversation_id outside an init event is ignored",
			line: `{"event":"step_update","conversation_id":"agy-step-789"}`,
			want: "",
		},
		{
			name: "pi session event id",
			line: `{"type":"session","id":"pi-session-321"}`,
			want: "pi-session-321",
		},
		{
			name: "codex thread started",
			line: `{"type":"thread.started","thread_id":"thread-789"}`,
			want: "thread-789",
		},
		{
			name: "other thread id ignored",
			line: `{"type":"turn.started","thread_id":"thread-789"}`,
			want: "",
		},
		{
			name: "plain text ignored",
			line: "not json",
			want: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, ExtractSessionID(tc.line))
		})
	}
}
